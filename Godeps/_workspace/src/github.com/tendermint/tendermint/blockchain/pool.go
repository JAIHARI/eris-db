package blockchain

import (
	"math"
	"sync"
	"time"

	flow "github.com/eris-ltd/eris-db/Godeps/_workspace/src/code.google.com/p/mxk/go1/flowcontrol"
	. "github.com/eris-ltd/eris-db/Godeps/_workspace/src/github.com/tendermint/tendermint/common"
	"github.com/eris-ltd/eris-db/Godeps/_workspace/src/github.com/tendermint/tendermint/types"
)

const (
	requestIntervalMS         = 250
	maxTotalRequesters        = 300
	maxPendingRequests        = maxTotalRequesters
	maxPendingRequestsPerPeer = 75
	peerTimeoutSeconds        = 15
	minRecvRate               = 10240 // 10Kb/s
)

/*
	Peers self report their heights when we join the block pool.
	Starting from our latest pool.height, we request blocks
	in sequence from peers that reported higher heights than ours.
	Every so often we ask peers what height they're on so we can keep going.

	Requests are continuously made for blocks of heigher heights until
	the limits. If most of the requests have no available peers, and we
	are not at peer limits, we can probably switch to consensus reactor
*/

type BlockPool struct {
	QuitService
	startTime time.Time

	// block requests
	mtx        sync.Mutex
	requesters map[int]*bpRequester
	height     int   // the lowest key in requesters.
	numPending int32 // number of requests pending assignment or block response

	// peers
	peersMtx sync.Mutex
	peers    map[string]*bpPeer

	requestsCh chan<- BlockRequest
	timeoutsCh chan<- string
}

func NewBlockPool(start int, requestsCh chan<- BlockRequest, timeoutsCh chan<- string) *BlockPool {
	bp := &BlockPool{
		peers: make(map[string]*bpPeer),

		requesters: make(map[int]*bpRequester),
		height:     start,
		numPending: 0,

		requestsCh: requestsCh,
		timeoutsCh: timeoutsCh,
	}
	bp.QuitService = *NewQuitService(log, "BlockPool", bp)
	return bp
}

func (pool *BlockPool) OnStart() error {
	pool.QuitService.OnStart()
	go pool.makeRequestersRoutine()
	pool.startTime = time.Now()
	return nil
}

func (pool *BlockPool) OnStop() {
	pool.QuitService.OnStop()
}

// Run spawns requesters as needed.
func (pool *BlockPool) makeRequestersRoutine() {
	for {
		if !pool.IsRunning() {
			break
		}
		_, numPending := pool.GetStatus()
		if numPending >= maxPendingRequests {
			// sleep for a bit.
			time.Sleep(requestIntervalMS * time.Millisecond)
			// check for timed out peers
			pool.removeTimedoutPeers()
		} else if len(pool.requesters) >= maxTotalRequesters {
			// sleep for a bit.
			time.Sleep(requestIntervalMS * time.Millisecond)
			// check for timed out peers
			pool.removeTimedoutPeers()
		} else {
			// request for more blocks.
			pool.makeNextRequester()
		}
	}
}

func (pool *BlockPool) removeTimedoutPeers() {
	for _, peer := range pool.peers {
		if !peer.didTimeout && peer.numPending > 0 {
			curRate := peer.recvMonitor.Status().CurRate
			// XXX remove curRate != 0
			if curRate != 0 && curRate < minRecvRate {
				pool.sendTimeout(peer.id)
				log.Warn("SendTimeout", "peer", peer.id, "reason", "curRate too low")
				peer.didTimeout = true
			}
		}
		if peer.didTimeout {
			pool.peersMtx.Lock() // Lock
			pool.removePeer(peer.id)
			pool.peersMtx.Unlock()
		}
	}
}

func (pool *BlockPool) GetStatus() (height int, numPending int32) {
	pool.mtx.Lock() // Lock
	defer pool.mtx.Unlock()

	return pool.height, pool.numPending
}

// TODO: relax conditions, prevent abuse.
func (pool *BlockPool) IsCaughtUp() bool {
	pool.mtx.Lock()
	height := pool.height
	pool.mtx.Unlock()

	pool.peersMtx.Lock()
	numPeers := len(pool.peers)
	maxPeerHeight := 0
	for _, peer := range pool.peers {
		maxPeerHeight = MaxInt(maxPeerHeight, peer.height)
	}
	pool.peersMtx.Unlock()

	return numPeers >= 3 && (height > 0 || time.Now().Sub(pool.startTime) > 30*time.Second) && (maxPeerHeight == 0 || height == maxPeerHeight)
}

// We need to see the second block's Validation to validate the first block.
// So we peek two blocks at a time.
func (pool *BlockPool) PeekTwoBlocks() (first *types.Block, second *types.Block) {
	pool.mtx.Lock() // Lock
	defer pool.mtx.Unlock()

	if r := pool.requesters[pool.height]; r != nil {
		first = r.getBlock()
	}
	if r := pool.requesters[pool.height+1]; r != nil {
		second = r.getBlock()
	}
	return
}

// Pop the first block at pool.height
// It must have been validated by 'second'.Validation from PeekTwoBlocks().
func (pool *BlockPool) PopRequest() {
	pool.mtx.Lock() // Lock
	defer pool.mtx.Unlock()

	if r := pool.requesters[pool.height]; r != nil {
		/*  The block can disappear at any time, due to removePeer().
		if r := pool.requesters[pool.height]; r == nil || r.block == nil {
			PanicSanity("PopRequest() requires a valid block")
		}
		*/
		r.Stop()
		delete(pool.requesters, pool.height)
		pool.height++
	} else {
		PanicSanity(Fmt("Expected requester to pop, got nothing at height %v", pool.height))
	}
}

// Invalidates the block at pool.height,
// Remove the peer and redo request from others.
func (pool *BlockPool) RedoRequest(height int) {
	pool.mtx.Lock() // Lock
	defer pool.mtx.Unlock()

	request := pool.requesters[height]
	if request.block == nil {
		PanicSanity("Expected block to be non-nil")
	}
	// RemovePeer will redo all requesters associated with this peer.
	// TODO: record this malfeasance
	pool.RemovePeer(request.peerID) // Lock on peersMtx.
}

// TODO: ensure that blocks come in order for each peer.
func (pool *BlockPool) AddBlock(peerID string, block *types.Block, blockSize int) {
	pool.mtx.Lock() // Lock
	defer pool.mtx.Unlock()

	requester := pool.requesters[block.Height]
	if requester == nil {
		return
	}

	if requester.setBlock(block, peerID) {
		pool.numPending--
		peer := pool.getPeer(peerID)
		peer.decrPending(blockSize)
	} else {
		// Bad peer?
	}
}

// Sets the peer's alleged blockchain height.
func (pool *BlockPool) SetPeerHeight(peerID string, height int) {
	pool.peersMtx.Lock() // Lock
	defer pool.peersMtx.Unlock()

	peer := pool.peers[peerID]
	if peer != nil {
		peer.height = height
	} else {
		peer = newBPPeer(pool, peerID, height)
		pool.peers[peerID] = peer
	}
}

func (pool *BlockPool) RemovePeer(peerID string) {
	pool.peersMtx.Lock() // Lock
	defer pool.peersMtx.Unlock()

	pool.removePeer(peerID)
}

func (pool *BlockPool) removePeer(peerID string) {
	for _, requester := range pool.requesters {
		if requester.getPeerID() == peerID {
			pool.numPending++
			go requester.redo() // pick another peer and ...
		}
	}
	delete(pool.peers, peerID)
}

func (pool *BlockPool) getPeer(peerID string) *bpPeer {
	pool.peersMtx.Lock() // Lock
	defer pool.peersMtx.Unlock()

	peer := pool.peers[peerID]
	return peer
}

// Pick an available peer with at least the given minHeight.
// If no peers are available, returns nil.
func (pool *BlockPool) pickIncrAvailablePeer(minHeight int) *bpPeer {
	pool.peersMtx.Lock()
	defer pool.peersMtx.Unlock()

	for _, peer := range pool.peers {
		if peer.isBad() {
			pool.removePeer(peer.id)
			continue
		} else {
		}
		if peer.numPending >= maxPendingRequestsPerPeer {
			continue
		}
		if peer.height < minHeight {
			continue
		}
		peer.incrPending()
		return peer
	}
	return nil
}

func (pool *BlockPool) makeNextRequester() {
	pool.mtx.Lock() // Lock
	defer pool.mtx.Unlock()

	nextHeight := pool.height + len(pool.requesters)
	request := newBPRequester(pool, nextHeight)

	pool.requesters[nextHeight] = request
	pool.numPending++

	request.Start()
}

func (pool *BlockPool) sendRequest(height int, peerID string) {
	if !pool.IsRunning() {
		return
	}
	pool.requestsCh <- BlockRequest{height, peerID}
}

func (pool *BlockPool) sendTimeout(peerID string) {
	if !pool.IsRunning() {
		return
	}
	pool.timeoutsCh <- peerID
}

func (pool *BlockPool) debug() string {
	pool.mtx.Lock() // Lock
	defer pool.mtx.Unlock()

	str := ""
	for h := pool.height; h < pool.height+len(pool.requesters); h++ {
		if pool.requesters[h] == nil {
			str += Fmt("H(%v):X ", h)
		} else {
			str += Fmt("H(%v):", h)
			str += Fmt("B?(%v) ", pool.requesters[h].block != nil)
		}
	}
	return str
}

//-------------------------------------

type bpPeer struct {
	pool        *BlockPool
	id          string
	height      int
	numPending  int32
	recvMonitor *flow.Monitor
	timeout     *time.Timer
	didTimeout  bool
}

func newBPPeer(pool *BlockPool, peerID string, height int) *bpPeer {
	peer := &bpPeer{
		pool:       pool,
		id:         peerID,
		height:     height,
		numPending: 0,
	}
	return peer
}

func (peer *bpPeer) resetMonitor() {
	peer.recvMonitor = flow.New(time.Second, time.Second*40)
	var initialValue = float64(minRecvRate) * math.E
	peer.recvMonitor.SetREMA(initialValue)
}

func (peer *bpPeer) resetTimeout() {
	if peer.timeout == nil {
		peer.timeout = time.AfterFunc(time.Second*peerTimeoutSeconds, peer.onTimeout)
	} else {
		peer.timeout.Reset(time.Second * peerTimeoutSeconds)
	}
}

func (peer *bpPeer) incrPending() {
	if peer.numPending == 0 {
		peer.resetMonitor()
		peer.resetTimeout()
	}
	peer.numPending++
}

func (peer *bpPeer) decrPending(recvSize int) {
	peer.numPending--
	if peer.numPending == 0 {
		peer.timeout.Stop()
	} else {
		peer.recvMonitor.Update(recvSize)
		peer.resetTimeout()
	}
}

func (peer *bpPeer) onTimeout() {
	peer.pool.sendTimeout(peer.id)
	log.Warn("SendTimeout", "peer", peer.id, "reason", "onTimeout")
	peer.didTimeout = true
}

func (peer *bpPeer) isBad() bool {
	return peer.didTimeout
}

//-------------------------------------

type bpRequester struct {
	QuitService
	pool       *BlockPool
	height     int
	gotBlockCh chan struct{}
	redoCh     chan struct{}

	mtx    sync.Mutex
	peerID string
	block  *types.Block
}

func newBPRequester(pool *BlockPool, height int) *bpRequester {
	bpr := &bpRequester{
		pool:       pool,
		height:     height,
		gotBlockCh: make(chan struct{}),
		redoCh:     make(chan struct{}),

		peerID: "",
		block:  nil,
	}
	bpr.QuitService = *NewQuitService(nil, "bpRequester", bpr)
	return bpr
}

func (bpr *bpRequester) OnStart() error {
	bpr.QuitService.OnStart()
	go bpr.requestRoutine()
	return nil
}

// Returns true if the peer matches
func (bpr *bpRequester) setBlock(block *types.Block, peerID string) bool {
	bpr.mtx.Lock()
	if bpr.block != nil || bpr.peerID != peerID {
		bpr.mtx.Unlock()
		return false
	}
	bpr.block = block
	bpr.mtx.Unlock()

	bpr.gotBlockCh <- struct{}{}
	return true
}

func (bpr *bpRequester) getBlock() *types.Block {
	bpr.mtx.Lock()
	defer bpr.mtx.Unlock()
	return bpr.block
}

func (bpr *bpRequester) getPeerID() string {
	bpr.mtx.Lock()
	defer bpr.mtx.Unlock()
	return bpr.peerID
}

func (bpr *bpRequester) reset() {
	bpr.mtx.Lock()
	bpr.peerID = ""
	bpr.block = nil
	bpr.mtx.Unlock()
}

// Tells bpRequester to pick another peer and try again.
// NOTE: blocking
func (bpr *bpRequester) redo() {
	bpr.redoCh <- struct{}{}
}

// Responsible for making more requests as necessary
// Returns only when a block is found (e.g. AddBlock() is called)
func (bpr *bpRequester) requestRoutine() {
OUTER_LOOP:
	for {
		// Pick a peer to send request to.
		var peer *bpPeer = nil
	PICK_PEER_LOOP:
		for {
			if !bpr.IsRunning() || !bpr.pool.IsRunning() {
				return
			}
			peer = bpr.pool.pickIncrAvailablePeer(bpr.height)
			if peer == nil {
				//log.Info("No peers available", "height", height)
				time.Sleep(requestIntervalMS * time.Millisecond)
				continue PICK_PEER_LOOP
			}
			break PICK_PEER_LOOP
		}
		bpr.mtx.Lock()
		bpr.peerID = peer.id
		bpr.mtx.Unlock()

		// Send request and wait.
		bpr.pool.sendRequest(bpr.height, peer.id)
		select {
		case <-bpr.pool.Quit:
			bpr.Stop()
			return
		case <-bpr.Quit:
			return
		case <-bpr.redoCh:
			bpr.reset()
			continue OUTER_LOOP // When peer is removed
		case <-bpr.gotBlockCh:
			// We got the block, now see if it's good.
			select {
			case <-bpr.pool.Quit:
				bpr.Stop()
				return
			case <-bpr.Quit:
				return
			case <-bpr.redoCh:
				bpr.reset()
				continue OUTER_LOOP
			}
		}
	}
}

//-------------------------------------

type BlockRequest struct {
	Height int
	PeerID string
}
