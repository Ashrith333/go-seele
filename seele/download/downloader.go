/**
*  @file
*  @copyright defined in go-seele/LICENSE
 */

package downloader

import (
	"errors"
	"math/big"
	"sync"
	"time"

	"github.com/seeleteam/go-seele/common"
	"github.com/seeleteam/go-seele/core"
	"github.com/seeleteam/go-seele/core/types"
	"github.com/seeleteam/go-seele/event"
	"github.com/seeleteam/go-seele/log"
	"github.com/seeleteam/go-seele/p2p"
)

const (
	GetBlockHeadersMsg uint16 = 8
	BlockHeadersMsg    uint16 = 9
	GetBlocksMsg       uint16 = 10
	BlocksPreMsg       uint16 = 11 // is sent before BlockMsg, containing block numbers of BlockMsg.
	BlocksMsg          uint16 = 12
)

var (
	MaxBlockFetch  = 128 // Amount of blocks to be fetched per retrieval request
	MaxHeaderFetch = 256 // Amount of block headers to be fetched per retrieval request

	MaxForkAncestry = 90000       // Maximum chain reorganisation
	peerIdleTime    = time.Second // peer's wait time for next turn if no task now

	MaxMessageLength = 8 * 1024 * 1024
	statusNone       = 1 // no sync session
	statusPreparing  = 2 // sync session is preparing
	statusFetching   = 3 // sync session is downloading
	statusCleaning   = 4 // sync session is cleaning
)

var (
	errHashNotMatch        = errors.New("Hash not match")
	errInvalidAncestor     = errors.New("Ancestor is invalid")
	errInvalidPacketRecved = errors.New("Invalid packet received")
	errIsSynchronising     = errors.New("Is synchronising")
	errMaxForkAncestor     = errors.New("Can not find ancestor when reached MaxForkAncestry")
	errPeerNotFound        = errors.New("Peer not found")
	errSyncErr             = errors.New("Err occurs when syncing")
)

// Downloader sync block chain with remote peer
type Downloader struct {
	cancelCh   chan struct{}        // Cancel current synchronising session
	masterPeer string               // Identifier of the best peer
	peers      map[string]*peerConn // peers map. peerID=>peer

	syncStatus int
	tm         *taskMgr

	chain     *core.Blockchain
	sessionWG sync.WaitGroup
	log       *log.SeeleLog
	lock      sync.RWMutex
}

// NewDownloader create Downloader
func NewDownloader(chain *core.Blockchain) *Downloader {
	d := &Downloader{
		peers:      make(map[string]*peerConn),
		chain:      chain,
		syncStatus: statusNone,
	}
	d.log = log.GetLogger("download", common.PrintLog)
	return d
}

// Synchronise try to sync with remote peer.
func (d *Downloader) Synchronise(id string, head common.Hash, td *big.Int, localTD *big.Int) error {
	// Make sure only one routine can pass at once
	d.lock.Lock()
	if d.syncStatus != statusNone {
		d.lock.Unlock()
		return errIsSynchronising
	}
	d.syncStatus = statusPreparing
	d.cancelCh = make(chan struct{})
	d.masterPeer = id
	p, ok := d.peers[id]
	if !ok {
		close(d.cancelCh)
		d.syncStatus = statusNone
		d.lock.Unlock()
		return errPeerNotFound
	}
	d.lock.Unlock()

	err := d.doSynchronise(p, head, td, localTD)
	d.lock.Lock()
	d.syncStatus = statusNone
	d.sessionWG.Wait()
	d.cancelCh = nil
	d.lock.Unlock()
	return err
}

func (d *Downloader) doSynchronise(conn *peerConn, head common.Hash, td *big.Int, localTD *big.Int) (err error) {
	event.BlockDownloaderEventManager.Fire(event.DownloaderStartEvent)
	defer func() {
		if err != nil {
			event.BlockDownloaderEventManager.Fire(event.DownloaderFailedEvent)
		} else {
			event.BlockDownloaderEventManager.Fire(event.DownloaderDoneEvent)
		}
	}()
	d.log.Debug("Downloader.doSynchronise start")
	latest, err := d.fetchHeight(conn)
	if err != nil {
		return err
	}
	height := latest.Height

	ancestor, err := d.findCommonAncestorHeight(conn, height)
	if err != nil {
		return err
	}
	d.log.Debug("Downloader.findCommonAncestorHeight start, ancestor=%d", ancestor)
	tm := newTaskMgr(d, d.masterPeer, ancestor+1, height)
	d.tm = tm
	d.lock.Lock()
	d.syncStatus = statusFetching
	for _, c := range d.peers {
		_, peerTD := c.peer.Head()
		if localTD.Cmp(peerTD) >= 0 {
			continue
		}
		d.sessionWG.Add(1)

		go d.peerDownload(c, tm)
	}
	d.lock.Unlock()
	d.sessionWG.Wait()

	d.lock.Lock()
	d.syncStatus = statusCleaning
	d.lock.Unlock()
	tm.close()
	d.tm = nil
	d.log.Info("downloader.doSynchronise quit!")

	if tm.isDone() {
		return nil
	}

	return errSyncErr
}

// fetchHeight gets the latest head of peer
func (d *Downloader) fetchHeight(conn *peerConn) (*types.BlockHeader, error) {
	head, _ := conn.peer.Head()
	go conn.peer.RequestHeadersByHashOrNumber(head, 0, 1, false)
	msg, err := conn.waitMsg(BlockHeadersMsg, d.cancelCh)
	if err != nil {
		return nil, err
	}
	var headers []types.BlockHeader
	if err := common.Deserialize(msg.Payload, &headers); err != nil {
		return nil, err
	}
	if len(headers) != 1 {
		return nil, errInvalidPacketRecved
	}
	if headers[0].Hash() != head {
		return nil, errHashNotMatch
	}
	return &headers[0], nil
}

// findCommonAncestorHeight finds the common ancestor height
func (d *Downloader) findCommonAncestorHeight(conn *peerConn, height uint64) (uint64, error) {
	// Get the top height
	block, _ := d.chain.CurrentBlock()
	localHeight := block.Header.Height
	var top uint64
	if localHeight <= height {
		top = localHeight
	} else {
		top = height
	}

	if top == 0 {
		return top, nil
	}

	// get maximum chain reorganisation
	var maxFetchAncestry int
	if top >= uint64(MaxForkAncestry) {
		maxFetchAncestry = MaxForkAncestry
	} else {
		maxFetchAncestry = int(top) + 1
	}

	// Compare the peer and local block head hash and return the ancestor height
	var cmpCount = 0
	for {
		localTop := top - uint64(cmpCount)
		var fetchCount = 0
		if (maxFetchAncestry - cmpCount) >= MaxHeaderFetch {
			fetchCount = MaxHeaderFetch
		} else {
			fetchCount = maxFetchAncestry - cmpCount
		}
		if fetchCount == 0 {
			return 0, errMaxForkAncestor
		}

		// Get peer block headers
		go conn.peer.RequestHeadersByHashOrNumber(common.EmptyHash, localTop, fetchCount, true)
		msg, err := conn.waitMsg(BlockHeadersMsg, d.cancelCh)
		if err != nil {
			return 0, err
		}

		var headers []types.BlockHeader
		if err := common.Deserialize(msg.Payload, &headers); err != nil {
			return 0, err
		}

		if len(headers) == 0 {
			return 0, errInvalidAncestor
		}
		cmpCount += len(headers)

		// Is ancenstor found
		for i := 0; i < len(headers); i++ {
			cmpHeight := headers[i].Height
			localHash, err := d.chain.GetStore().GetBlockHash(cmpHeight)
			if err != nil {
				return 0, err
			}
			if localHash == headers[i].Hash() {
				return cmpHeight, nil
			}
		}
	}
}

// RegisterPeer add peer to download routine
func (d *Downloader) RegisterPeer(peerID string, peer Peer) {
	d.lock.Lock()
	defer d.lock.Unlock()
	newConn := newPeerConn(peer, peerID)
	d.peers[peerID] = newConn

	if d.syncStatus == statusFetching {
		d.sessionWG.Add(1)
		go d.peerDownload(newConn, d.tm)
	}
}

// UnRegisterPeer remove peer from download routine
func (d *Downloader) UnRegisterPeer(peerID string) {
	d.lock.Lock()
	defer d.lock.Unlock()
	if peerConn, ok := d.peers[peerID]; ok {
		peerConn.close()
		delete(d.peers, peerID)
	}
}

// DeliverMsg called by seeleprotocol to deliver recved msg from network
func (d *Downloader) DeliverMsg(peerID string, msg *p2p.Message) {
	d.lock.Lock()
	peerConn, ok := d.peers[peerID]
	d.lock.Unlock()
	if !ok {
		return
	}
	peerConn.deliverMsg(msg.Code, msg)
	return
}

// Cancel cancels current session.
func (d *Downloader) Cancel() {
	d.lock.Lock()
	defer d.lock.Unlock()
	if d.cancelCh != nil {
		select {
		case <-d.cancelCh:
		default:
			close(d.cancelCh)
		}
	}
}

// Terminate close Downloader, cannot called anymore.
func (d *Downloader) Terminate() {
	d.Cancel()
	d.sessionWG.Wait()
	// TODO release variables if needed
}

// peerDownload peer download routine
func (d *Downloader) peerDownload(conn *peerConn, tm *taskMgr) {
	defer d.sessionWG.Done()
	d.log.Debug("Downloader.peerDownload start")
	bMaster := (conn.peerID == d.masterPeer)
	peerID := conn.peerID
	var err error
outLoop:
	for !tm.isDone() {
		hasReqData := false
		if startNo, amount := tm.getReqHeaderInfo(conn); amount > 0 {
			d.log.Debug("tm.getReqHeaderInfo. %d %d", startNo, amount)
			hasReqData = true
			if err = conn.peer.RequestHeadersByHashOrNumber(common.Hash{}, startNo, amount, false); err != nil {
				d.log.Info("RequestHeadersByHashOrNumber err!")
				break
			}
			msg, err := conn.waitMsg(BlockHeadersMsg, d.cancelCh)
			if err != nil {
				d.log.Info("peerDownload waitMsg BlockHeadersMsg err! %s", err)
				break
			}
			var headers []*types.BlockHeader
			if err = common.Deserialize(msg.Payload, &headers); err != nil {
				d.log.Info("peerDownload Deserialize err! %s", err)
				break
			}

			if err = tm.deliverHeaderMsg(peerID, headers); err != nil {
				d.log.Info("peerDownload deliverHeaderMsg err! %s", err)
				break
			}
		}

		if startNo, amount := tm.getReqBlocks(conn); amount > 0 {
			d.log.Debug("download.peerdown getReqBlocks startNo=%d amount=%d", startNo, amount)
			hasReqData = true
			if err = conn.peer.RequestBlocksByHashOrNumber(common.Hash{}, startNo, amount); err != nil {
				d.log.Info("RequestBlocksByHashOrNumber err!")
				break
			}

			msg, err := conn.waitMsg(BlocksPreMsg, d.cancelCh)
			if err != nil {
				d.log.Info("peerDownload waitMsg BlocksPreMsg err! %s", err)
				break
			}

			var blockNums []uint64
			if err = common.Deserialize(msg.Payload, &blockNums); err != nil {
				d.log.Info("peerDownload Deserialize err! %s", err)
				break
			}
			tm.deliverBlockPreMsg(peerID, blockNums)

			msg, err = conn.waitMsg(BlocksMsg, d.cancelCh)
			if err != nil {
				d.log.Info("peerDownload waitMsg BlocksMsg err! %s", err)
				break
			}

			var blocks []*types.Block
			if err = common.Deserialize(msg.Payload, &blocks); err != nil {
				d.log.Info("peerDownload Deserialize err! %s", err)
				break
			}
			tm.deliverBlockMsg(peerID, blocks)
		}
		if hasReqData {
			continue
		}

	outFor:
		for {
			select {
			case <-d.cancelCh:
				break outLoop
			case <-conn.quitCh:
				break outLoop
			case <-time.After(peerIdleTime):
				d.log.Debug("peerDownload peerIdleTime timeout")
				break outFor
			}
		}
	}

	tm.onPeerQuit(peerID)
	if bMaster {
		d.Cancel()
	}
	d.log.Debug("Downloader.peerDownload end")
}

// processBlocks writes blocks to the blockchain.
func (d *Downloader) processBlocks(headInfos []*masterHeadInfo) {

	for _, h := range headInfos {
		d.log.Debug("%d %s <- %s ", h.block.Header.Height, h.block.HeaderHash.ToHex(), h.block.Header.PreviousBlockHash.ToHex())
	}

	for _, h := range headInfos {
		d.log.Debug("d.processBlock %d", h.block.Header.Height)
		if err := d.chain.WriteBlock(h.block); err != nil && err != core.ErrBlockAlreadyExist {
			d.log.Error("downloader processBlocks err. %s", err)
			d.Cancel()
			break
		}
		h.status = taskStatusProcessed
	}
}
