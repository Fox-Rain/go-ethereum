// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

/*
	p2p network를 통해 다른 이더리움 노드로부터 수신한 프로토콜 메시지들을 처리하는 역할
	메시지 종류를 식별하고, 해당 메세지를 처리할 모듈에 전달하는 Router & dispatcher 역할을 수행한다.
	(* 주의: 이는 RPC요청 (dapp등의 요청)과는 다른 노드와 노드끼리의 통신을 의미한다.)
	블록체인 동기화, 트랜잭션 관리 및 전파 등의 역할을 수행한다.

	TODO: Node - to - Node Only
		  노드간의 p2p연결 자체를 하지는 않는다. (네트워크 연결은 p2p.server에서 수행)

	TODO: 다른 노드의 메세지를 handler.go의 ProtocolManager라는 구조체를 통해서 단일 진입점으로 받는다.
		  이후 handleMsg 메서드를 통해서 다른 내부의 기능으로 분배한다.






*/

package eth

import (
	"errors"
	"maps"
	"math"
	"math/big"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/eth/fetcher"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// 노드간 통신에 관련된 설정값
const (
	// txChanSize is the size of channel listening to NewTxsEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096		// NewTxsEvent를 수신하는 채널의 버퍼크기  (멤풀에 tx추가시 울리는 알림을 임시로 저장할 수 있는 공간 크기)

	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	chainHeadChanSize = 128	// chainHeadEvent 알림을 수신하는 채널의 버퍼크기 (새 블록이 채굴되었다는 알림)

	// txMaxBroadcastSize is the max size of a transaction that will be broadcasted.
	// All transactions with a higher size will be announced and need to be fetched
	// by the peer.
	txMaxBroadcastSize = 4096	// 이 값을 경계로 이보다 작으면 tx를 모두 브로드캐스팅, 아니면 일단 해시값만 브로드캐스팅
								// *해시값만 먼저 브로드캐스팅하고 필요시 (중복이 아닐때)만 요청하는 식으로
)



// TODO: 동기화 진행 상태를 묻는 챌린지 메세지를 보내고, 상대방 피어로부터 응답이 오기까지 기다리는 최대 시간		이를 통해 블록을 받아올떄 느린 노드 혹은 악의적인 노드를 거른다.
var syncChallengeTimeout = 15 * time.Second // Time allowance for a node to reply to the sync progress challenge





// txPool defines the methods needed from a transaction pool implementation to
// support all the operations needed by the Ethereum chain protocols.
// TODO: transaction pool interface
/*
	* txpool은 다른곳에 정의되는데 인터페이스가 여기있는 이유
	handler.go의 독립성:  내가 정의한 인터페이스 메서드 제공만해라 이것

	이렇게 의존성 역전원칙을 따르면, 상위모듈인 handler는 하위 모듈에 대해 의존하지않음.
*/
type txPool interface {
	// Has returns an indicator whether txpool has a transaction
	// cached with the given hash.
	Has(hash common.Hash) bool		// common.Hash인 해시값을 갖는 tx가 풀안에 존재하는지 확인하는 메서드

	// Get retrieves the transaction from local txpool with given
	// tx hash.
	Get(hash common.Hash) *types.Transaction	// common.Hash에 해당하는 tx를 리턴

	// GetRLP retrieves the RLP-encoded transaction from local txpool
	// with given tx hash.
	GetRLP(hash common.Hash) []byte		// common.Hash에 해당하는 트랜잭션을 RLP로 인코딩한 원시데이터 리턴

	// GetMetadata returns the transaction type and transaction size with the
	// given transaction hash.
	GetMetadata(hash common.Hash) *txpool.TxMetadata	// 해당 tx의 메타데이터 리턴

	// Add should add the given transactions to the pool.
	Add(txs []*types.Transaction, sync bool) []error	// 하나 이상의 tx를 풀에 추가하는 메서드

	// Pending should return pending transactions.
	// The slice should be modifiable by the caller.
	Pending(filter txpool.PendingFilter) map[common.Address][]*txpool.LazyTransaction	// 풀안에 댇기중인 tx들을 리턴

	// SubscribeTransactions subscribes to new transaction events. The subscriber
	// can decide whether to receive notifications only for newly seen transactions
	// or also for reorged out ones.
	SubscribeTransactions(ch chan<- core.NewTxsEvent, reorgs bool) event.Subscription	// txpool을 구독하는 메서드 (tx가 풀에 추가되면 제공된 채널로 알림이 전송된다.)
}





// handlerConfig is the collection of initialization parameters to create a full
// node network handler.
// TODO: protocolManager를 생성하는데 필요한 모든 초기화 파라미터를 모아둔 config 구조체
type handlerConfig struct {
	NodeID         enode.ID               // P2P node ID used for tx propagation topology	현재 노드의 p2p ID
	Database       ethdb.Database         // Database for direct sync insertions			DB에 직접 접근할 수 있는 핸들
	Chain          *core.BlockChain       // Blockchain to serve data from					블록체인 객체에 대한 포인터
	TxPool         txPool                 // Transaction pool to propagate from				txpool interface
	Network        uint64                 // Network identifier to advertise				네트워크 식별자
	Sync           ethconfig.SyncMode     // Whether to snap or full sync					동기화 모드
	BloomCache     uint64                 // Megabytes to alloc for snap sync bloom			볼룸필터 캐시 메모리 크기
	EventMux       *event.TypeMux         // Legacy event mux, deprecate for `feed`			이벤트 버스 *현재안씀
	RequiredBlocks map[uint64]common.Hash // Hard coded map of required block hashes for sync challenges	특정 블록 번호와 그에 해당하는 공식 해시값을 하드코딩해놓은 map       TODO: 동기화과정에서 보안을 강화하기 위한 싱크 챌린지에 사용함
}



// TODO: handler 구조체로      p2p연결 유지 메세지 주고받기 트랜잭션 전파 동기화등의 모든 로직을 담당함
type handler struct {
	nodeID    enode.ID		// 노드의 고유 p2p ID
	networkID uint64		// 네트워크 ID      *메인넷인지 테스트넷인지 등

	snapSync atomic.Bool // Flag whether snap sync is enabled (gets disabled if we already have blocks)
	synced   atomic.Bool // Flag whether we're considered synchronised (enables transaction processing)

	database ethdb.Database
	txpool   txPool
	chain    *core.BlockChain	// 블록상태를 읽고 처리하는 등 체인로직을 담고 있는 객체
	maxPeers int				// 연결을 허용할 최대 peer 수

	downloader *downloader.Downloader	// 대규모 블록 동기화를 담당하는 객체에 대한 포인터
	txFetcher  *fetcher.TxFetcher		// 다른 노드로부터 tx hash만 받았을 떄, 해당 tx의 전체 데이터를 요청하고 가져오는 객체
	peers      *peerSet					// 연결된 피어 관리 객체

	eventMux   *event.TypeMux
	txsCh      chan core.NewTxsEvent	// txpool에 tx 추가됨을 알리는 채널
	txsSub     event.Subscription		// 채널을 통해 이벤트 구독을하는 객체
	blockRange *blockRangeState

	requiredBlocks map[uint64]common.Hash

	// channels for fetcher, syncer, txsyncLoop
	quitSync chan struct{}

	wg sync.WaitGroup

	handlerStartCh chan struct{}
	handlerDoneCh  chan struct{}
}




// newHandler returns a handler for all Ethereum chain management protocol.
// TODO: protocol handler를 생성하고 초기화하는 함수 handlerConfig 설정값을 받아서 생성하고 객체 리턴
// 		 동기화모드 설정, 다운로더 설정, 등... 하고 handler 객체 반환
func newHandler(config *handlerConfig) (*handler, error) {
	// Create the protocol manager with the base fields
	if config.EventMux == nil {
		config.EventMux = new(event.TypeMux) // Nicety initialization for tests
	}

	// handlerConfig에 맞게 객체 생성
	h := &handler{
		nodeID:         config.NodeID,
		networkID:      config.Network,
		eventMux:       config.EventMux,
		database:       config.Database,
		txpool:         config.TxPool,
		chain:          config.Chain,
		peers:          newPeerSet(),
		requiredBlocks: config.RequiredBlocks,
		quitSync:       make(chan struct{}),
		handlerDoneCh:  make(chan struct{}),
		handlerStartCh: make(chan struct{}),
	}

	// TODO: config를 통해서 요청한 동기화 모드와 현재 DB상태를 비교하여서, 최종적으로 사용할 동기화 모드를 결정항고 설정
	if config.Sync == ethconfig.FullSync {
		// The database seems empty as the current block is the genesis. Yet the snap
		// block is ahead, so snap sync was enabled for this node at a certain point.
		// The scenarios where this can happen is
		// * if the user manually (or via a bad block) rolled back a snap sync node
		//   below the sync point.
		// * the last snap sync is not finished while user specifies a full sync this
		//   time. But we don't have any recent state for full sync.
		// In these cases however it's safe to reenable snap sync.
		fullBlock, snapBlock := h.chain.CurrentBlock(), h.chain.CurrentSnapBlock()
		if fullBlock.Number.Uint64() == 0 && snapBlock.Number.Uint64() > 0 {
			h.snapSync.Store(true)
			log.Warn("Switch sync mode from full sync to snap sync", "reason", "snap sync incomplete")
		} else if !h.chain.HasState(fullBlock.Root) {
			h.snapSync.Store(true)
			log.Warn("Switch sync mode from full sync to snap sync", "reason", "head state missing")
		}
	} else {
		head := h.chain.CurrentBlock()
		if head.Number.Uint64() > 0 && h.chain.HasState(head.Root) {
			// Print warning log if database is not empty to run snap sync.
			log.Warn("Switch sync mode from snap sync to full sync", "reason", "snap sync complete")
		} else {
			// If snap sync was requested and our database is empty, grant it
			h.snapSync.Store(true)
			log.Info("Enabled snap sync", "head", head.Number, "hash", head.Hash())
		}
	}
	// If snap sync is requested but snapshots are disabled, fail loudly
	if h.snapSync.Load() && (config.Chain.Snapshots() == nil && config.Chain.TrieDB().Scheme() == rawdb.HashScheme) {
		return nil, errors.New("snap sync not supported with snapshots disabled")
	}


	// 대규모 블록 동기화를 담당하는 downloader 객체를 생성
	// Construct the downloader (long sync)
	h.downloader = downloader.New(config.Database, h.eventMux, h.chain, h.removePeer, h.enableSyncedFeatures)

	// 특정 트랜잭션의 전체 데이터를 가져오는 txFetcher 객체를 생성
	fetchTx := func(peer string, hashes []common.Hash) error {
		p := h.peers.peer(peer)
		if p == nil {
			return errors.New("unknown peer")
		}
		return p.RequestTxs(hashes)
	}
	addTxs := func(txs []*types.Transaction) []error {
		return h.txpool.Add(txs, false)
	}
	h.txFetcher = fetcher.NewTxFetcher(h.txpool.Has, addTxs, fetchTx, h.removePeer)
	
	// 객체반환
	return h, nil
}



// protoTracker tracks the number of active protocol handlers.
// TODO: 현재 활성화되어 실행중인 프로토콜 핸들러의 개수를 추적하는 백그라운드 카운터
// 		 handler는 1개고 그 안에서 연결된 피어마다 고루틴을 각각 실행시키는 구조
func (h *handler) protoTracker() {
	defer h.wg.Done()
	var active int
	for {
		select {
		case <-h.handlerStartCh:
			active++
		case <-h.handlerDoneCh:
			active--
		case <-h.quitSync:
			// Wait for all active handlers to finish.
			for ; active > 0; active-- {
				<-h.handlerDoneCh
			}
			return
		}
	}
}


// incHandlers signals to increment the number of active handlers if not
// quitting.
// 새로운 peer handler가 시작되면 protoTracker에게 활성 핸들러수를 1 증가시키라는 신호를 보내는 역할을 하는 함수
func (h *handler) incHandlers() bool {
	select {
	case h.handlerStartCh <- struct{}{}:
		return true
	case <-h.quitSync:
		return false
	}
}


// decHandlers signals to decrement the number of active handlers.
// peer handler가 하나 종료될때 protoTracker에게 활성 핸들러수를 1 감소시키라는 신호를 보내는 함수
func (h *handler) decHandlers() {
	h.handlerDoneCh <- struct{}{}
}



// runEthPeer registers an eth peer into the joint eth/snap peerset, adds it to
// various subsystems and starts handling messages.
// TODO: 새로 연결된 peer와의 전체 통신 세션을 설정하고 관리하는 함수
// 		 새로 연결된 peer와 핸드쉐이크, peer검증, txpool의 tx 공유 등 (peer를 찾는 로직은 x)
func (h *handler) runEthPeer(peer *eth.Peer, handler eth.Handler) error {
	
	//  활성 핸들러 카운터 +1
	if !h.incHandlers() {
		return p2p.DiscQuitting
	}
	defer h.decHandlers()

	// If the peer has a `snap` extension, wait for it to connect so we can have
	// a uniform initialization/teardown mechanism
	// snap 프로토콜을 peer가 지원하는지 확인
	snap, err := h.peers.waitSnapExtension(peer)
	if err != nil {
		peer.Log().Error("Snapshot extension barrier failed", "err", err)
		return err
	}

	// Execute the Ethereum handshake
	// 이더리움 핸드세이크 수행
	if err := peer.Handshake(h.networkID, h.chain, h.blockRange.currentRange()); err != nil {
		peer.Log().Debug("Ethereum handshake failed", "err", err)
		return err
	}

	// 스냅 동기화 모드일 때, 전체 피어 슬롯의 일부를 snap프로토콜을 지원하는 피어를 위해 남겨두는 정책을 수행
	reject := false // reserved peer slots
	if h.snapSync.Load() {
		if snap == nil {
			// If we are running snap-sync, we want to reserve roughly half the peer
			// slots for peers supporting the snap protocol.
			// The logic here is; we only allow up to 5 more non-snap peers than snap-peers.
			if all, snp := h.peers.len(), h.peers.snapLen(); all-snp > snp+5 {
				reject = true
			}
		}
	}
	// Ignore maxPeers if this is a trusted peer
	// 신뢰하는 피어가 아니라면, 현재 연결된 피어수가 최대치를 초과했는지 확인한다.
	if !peer.Peer.Info().Network.Trusted {
		if reject || h.peers.len() >= h.maxPeers {
			return p2p.DiscTooManyPeers
		}
	}
	peer.Log().Debug("Ethereum peer connected", "name", peer.Name())

	// Register the peer locally
	// 위의 검증을 통과한 peer를 handler의 내부 피어 관리 목록 (peerSet)에 공식적으로 등록한다.
	if err := h.peers.registerPeer(peer, snap); err != nil {
		peer.Log().Error("Ethereum peer registration failed", "err", err)
		return err
	}
	defer h.unregisterPeer(peer.ID())

	p := h.peers.peer(peer.ID())
	if p == nil {
		return errors.New("peer dropped during handling")
	}
	
	// Register the peer in the downloader. If the downloader considers it banned, we disconnect
	// peer를 블록 동기화를 담당하는 downloader에 등록한다. 
	if err := h.downloader.RegisterPeer(peer.ID(), peer.Version(), peer); err != nil {
		peer.Log().Error("Failed to register peer in eth syncer", "err", err)
		return err
	}
	if snap != nil {
		if err := h.downloader.SnapSyncer.Register(snap); err != nil {
			peer.Log().Error("Failed to register peer in snap syncer", "err", err)
			return err
		}
	}
	// Propagate existing transactions. new transactions appearing
	// after this will be sent via broadcasts.
	// 이 노드의 txpool에 있는 tx들을 새로운 피어에게 공유한다.
	h.syncTransactions(peer)

	// Create a notification channel for pending requests if the peer goes down
	dead := make(chan struct{})
	defer close(dead)

	// If we have any explicit peer required block hashes, request them
	// 하드코딩된 블록들의 해시값을 상대 peer가 올바르게 가지고있는지 확인하는 보안 검증 절차이다.
	for number, hash := range h.requiredBlocks {
		resCh := make(chan *eth.Response)

		req, err := peer.RequestHeadersByNumber(number, 1, 0, false, resCh)
		if err != nil {
			return err
		}
		go func(number uint64, hash common.Hash, req *eth.Request) {
			// Ensure the request gets cancelled in case of error/drop
			defer req.Close()

			timeout := time.NewTimer(syncChallengeTimeout)
			defer timeout.Stop()

			select {
			case res := <-resCh:
				headers := ([]*types.Header)(*res.Res.(*eth.BlockHeadersRequest))
				if len(headers) == 0 {
					// Required blocks are allowed to be missing if the remote
					// node is not yet synced
					res.Done <- nil
					return
				}
				// Validate the header and either drop the peer or continue
				if len(headers) > 1 {
					res.Done <- errors.New("too many headers in required block response")
					return
				}
				if headers[0].Number.Uint64() != number || headers[0].Hash() != hash {
					peer.Log().Info("Required block mismatch, dropping peer", "number", number, "hash", headers[0].Hash(), "want", hash)
					res.Done <- errors.New("required block mismatch")
					return
				}
				peer.Log().Debug("Peer required block verified", "number", number, "hash", hash)
				res.Done <- nil
			case <-timeout.C:
				peer.Log().Warn("Required block challenge timed out, dropping", "addr", peer.RemoteAddr(), "type", peer.Name())
				h.removePeer(peer.ID())
			}
		}(number, hash, req)
	}
	// Handle incoming messages until the connection is torn down
	return handler(peer)
}




// runSnapExtension registers a `snap` peer into the joint eth/snap peerset and
// starts handling inbound messages. As `snap` is only a satellite protocol to
// `eth`, all subsystem registrations and lifecycle management will be done by
// the main `eth` handler to prevent strange races.
// TODO: 이미 eth protocol로 연결된 peer가 snap protocol도 지원하는 경우, 이 snap 통신 세션을 드옭하고 메세지 처리를 시작하는 함
func (h *handler) runSnapExtension(peer *snap.Peer, handler snap.Handler) error {
	if !h.incHandlers() {
		return p2p.DiscQuitting
	}
	defer h.decHandlers()

	if err := h.peers.registerSnapExtension(peer); err != nil {
		if metrics.Enabled() {
			if peer.Inbound() {
				snap.IngressRegistrationErrorMeter.Mark(1)
			} else {
				snap.EgressRegistrationErrorMeter.Mark(1)
			}
		}
		peer.Log().Debug("Snapshot extension registration failed", "err", err)
		return err
	}
	return handler(peer)
}



// removePeer requests disconnection of a peer.
// peer 제거하는 함수  제거 절차 시작
func (h *handler) removePeer(id string) {
	peer := h.peers.peer(id)
	if peer != nil {
		peer.Peer.Disconnect(p2p.DiscUselessPeer)
	}
}



// unregisterPeer removes a peer from the downloader, fetchers and main peer set.
// 이미 연결이 끊어진 피어에 대해 모든 내부 기록을 제거하는 함수
func (h *handler) unregisterPeer(id string) {
	// Create a custom logger to avoid printing the entire id
	// custom logger 8자리에 대해서만 로깅
	var logger log.Logger
	if len(id) < 16 {
		// Tests use short IDs, don't choke on them
		logger = log.New("peer", id)
	} else {
		logger = log.New("peer", id[:8])
	}

	// Abort if the peer does not exist
	peer := h.peers.peer(id)
	if peer == nil {
		logger.Warn("Ethereum peer removal failed", "err", errPeerNotRegistered)
		return
	}
	// Remove the `eth` peer if it exists
	logger.Debug("Removing Ethereum peer", "snap", peer.snapExt != nil)

	// Remove the `snap` extension if it exists
	// peer가 등록되어있던 하위 모델에서 모두 제거하도록 요청
	if peer.snapExt != nil {
		h.downloader.SnapSyncer.Unregister(id)
	}
	h.downloader.UnregisterPeer(id)
	h.txFetcher.Drop(id)

	if err := h.peers.unregisterPeer(id); err != nil {
		logger.Error("Ethereum peer removal failed", "err", err)
	}
}



// TODO: handler는 새로운 트랜잭션과 블록정보를 다른 피어들에게 전파하고 피어 핸들러의 상태를 추적할 준비를 마치는 함수
func (h *handler) Start(maxPeers int) {
	h.maxPeers = maxPeers		// 인자로 받은 최대 peer를 handler 내부에 저장

	// broadcast and announce transactions (only new ones, not resurrected ones)
	h.wg.Add(1)				// count ++
	h.txsCh = make(chan core.NewTxsEvent, txChanSize)			// txpool로부터 새 tx에 대한 알람을 받을 채널생성
	h.txsSub = h.txpool.SubscribeTransactions(h.txsCh, false)	// txpool에 구독을 신청
	go h.txBroadcastLoop()		// 채널을 감시하다가 새 tx이벤트가 도착하면 연결된 피어들에게 전파하는 고루틴

	// broadcast block range
	h.wg.Add(1)				// count ++
	h.blockRange = newBlockRangeState(h.chain, h.eventMux)		// 현재 블록체인의 상태 (최신번호)를 추적하는 객체 생성
	go h.blockRangeLoop(h.blockRange)							// 위의 값을 주기적으로 피어들에게 알리는 고루틴


	// start sync handlers	   다른 피어로부터 tx hash만 받았을 경우, 해당 트랜잭션의 전체 데이터를 요청하는 txFetcher
	h.txFetcher.Start()

	// start peer handler tracker
	h.wg.Add(1)			// count++
	go h.protoTracker()	// 현재 활성화된 피어 핸들러의 개수를 계속해서 추적하는 고루틴
}



// TODO: handler의 모든 활동을 안전하게 종료시키는 함수	(백그라운드 작업 중지, 전체 종료신호 발송, 모든 피어 연결 종료)
func (h *handler) Stop() {
	h.txsSub.Unsubscribe() // quits txBroadcastLoop
	h.blockRange.stop()
	h.txFetcher.Stop()
	h.downloader.Terminate()

	// Quit chainSync and txsync64.
	// After this is done, no new peers will be accepted.
	close(h.quitSync)

	// Disconnect existing sessions.
	// This also closes the gate for any new registrations on the peer set.
	// sessions which are already established but not added to h.peers yet
	// will exit when they try to register.
	h.peers.close()
	h.wg.Wait()

	log.Info("Ethereum protocol stopped")
}



// BroadcastTransactions will propagate a batch of transactions
// - To a square root of all peers for non-blob transactions
// - And, separately, as announcements to all peers which are not known to
// already have the given transaction.
/*	
	TODO: BroadcastTransaction   
	handler가 txpool로 부터 받은 새로운 트랜잭션 몪음을 p2p 네트워크 상의 다른 피어들에게 효율적으로 전파하는 역할

	* 이때 무작정 보내는 것이 아니라 tx의 종류와 크기에 따라 다른 전략을 사용한다.
	1) 직접전파 =>   작은 트랜잭션의 경우, 피어중 일부를 택해서 무작위로 전체 데이터를 보낸다.
	2) 해시알림 =>   큰 트랜잭션의 경우, 이런 트랜잭션이 있다는 해시값만 알려준다. 이 알림을 보고 필요한 피어들만 개별적으로 요청하는 구조

*/
func (h *handler) BroadcastTransactions(txs types.Transactions) {
	var (
		blobTxs  int // Number of blob transactions to announce only
		largeTxs int // Number of large transactions to announce only

		directCount int // Number of transactions sent directly to peers (duplicates included)
		annCount    int // Number of transactions announced across all peers (duplicates included)

		txset = make(map[*ethPeer][]common.Hash) // Set peer->hash to transfer directly	 * 직접 전파용 tx를 담을 map
		annos = make(map[*ethPeer][]common.Hash) // Set peer->hash to announce			 * 해시 알람용 tx를 담을 map
	)
	// Broadcast transactions to a batch of peers not knowing about it
	// 직접 보낼 peer 수를 sqrt(n) 계산
	direct := big.NewInt(int64(math.Sqrt(float64(h.peers.len())))) // Approximate number of peers to broadcast to
	if direct.BitLen() == 0 {
		direct = big.NewInt(1)
	}
	total := new(big.Int).Exp(direct, big.NewInt(2), nil) // Stabilise total peer count a bit based on sqrt peers


	// 암호화 관련 변수
	var (
		signer = types.LatestSigner(h.chain.Config()) // Don't care about chain status, we just need *a* sender
		hasher = crypto.NewKeccakState()
		hash   = make([]byte, 32)
	)

	// TODO: 트랜잭션 분류
	for _, tx := range txs {
		var maybeDirect bool
		switch {
		case tx.Type() == types.BlobTxType:		// 블롭트랜잭션일떄
			blobTxs++
		case tx.Size() > txMaxBroadcastSize:	// 큰 tx
			largeTxs++
		default:								// 일반 tx
			maybeDirect = true
		}
		// Send the transaction (if it's small enough) directly to a subset of
		// the peers that have not received it yet, ensuring that the flow of
		// transactions is grouped by account to (try and) avoid nonce gaps.
		//
		// To do this, we hash the local enode IW with together with a peer's
		// enode ID together with the transaction sender and broadcast if
		// `sha(self, peer, sender) mod peers < sqrt(peers)`.

		// TODO: 현재 트랜잭션에 대해 그것을 아직 가지고있지 않은 모든 피어를 순회한다.
		for _, peer := range h.peers.peersWithoutTransaction(tx.Hash()) {
			var broadcast bool

			// 직접 전송 후보 트랜잭션일 떄만, 아래의 추첨 로직을 실행
			if maybeDirect {
				hasher.Reset()
				hasher.Write(h.nodeID.Bytes())
				hasher.Write(peer.Node().ID().Bytes())

				from, _ := types.Sender(signer, tx) // Ignore error, we only use the addr as a propagation target splitter
				hasher.Write(from.Bytes())

				hasher.Read(hash)
				if new(big.Int).Mod(new(big.Int).SetBytes(hash), total).Cmp(direct) < 0 {
					broadcast = true
				}
			}
			// 피어순회히다 위의 로직을 통해 담청되면 직접전송    비당첨시 해시알림에 추가
			if broadcast {
				txset[peer] = append(txset[peer], tx.Hash())

			} else {
				annos[peer] = append(annos[peer], tx.Hash())
			}
		}
	}

	// 위의 분류에 맞추어 전송
	for peer, hashes := range txset {
		directCount += len(hashes)
		peer.AsyncSendTransactions(hashes)
	}
	for peer, hashes := range annos {
		annCount += len(hashes)
		peer.AsyncSendPooledTransactionHashes(hashes)
	}

	// 로그 출력
	log.Debug("Distributed transactions", "plaintxs", len(txs)-blobTxs-largeTxs, "blobtxs", blobTxs, "largetxs", largeTxs,
		"bcastpeers", len(txset), "bcastcount", directCount, "annpeers", len(annos), "anncount", annCount)
}




// txBroadcastLoop announces new transactions to connected peers.
// TODO: txpool에 event를 계속 기다리면서 알림이 오면 바로 위의 함수 BroadcastTransaction을 통해서 전파하는 고루틴함수
func (h *handler) txBroadcastLoop() {
	defer h.wg.Done()
	for {
		select {
		case event := <-h.txsCh:
			h.BroadcastTransactions(event.Txs)
		case <-h.txsSub.Err():
			return
		}
	}
}



// enableSyncedFeatures enables the post-sync functionalities when the initial
// sync is finished.
// TODO: 노드의 초기 동기화가 모두 완료됬을때 호출되어서 완전환 동기화 상태로 전환하는 함수
func (h *handler) enableSyncedFeatures() {
	// Mark the local node as synced.
	// 동기화 완료 상태로 전환
	h.synced.Store(true)

	// If we were running snap sync and it finished, disable doing another
	// round on next sync cycle
	// 스냅동기화모드였는지 확인하고 이후부터는 이제 풀 동기화 모드로 변경
	if h.snapSync.Load() {
		log.Info("Snap sync complete, auto disabling")
		h.snapSync.Store(false)
	}
}











// blockRangeState holds the state of the block range update broadcasting mechanism.
// TODO: blockRangeState	현재노드의 최신 블록범위를 다른 피어들에게 효율적으로 알리는 메커니즘의 상태를 관리하는 객체
type blockRangeState struct {
	prev    eth.BlockRangeUpdatePacket					// 이전에 피어들에게 마지막으로 브로드캐스팅했던 블록 범위
	next    atomic.Pointer[eth.BlockRangeUpdatePacket]	// 다음에 피어들에게 브로드캐스팅해야하는 범위
	headCh  chan core.ChainHeadEvent					// 최신 블록이 변경됬다는 이벤트를 수신하는 채널
	headSub event.Subscription							// 채널을 통해 구독하는 객체
	syncSub *event.TypeMuxSubscription					// downloader로 부터 동기화 상태변경에 대한 이벤트를 구독하는 객체
}



// TODO: blockRangeState 객체를 생성하고 초기화한 뒤 리턴하는 생성자 함수
func newBlockRangeState(chain *core.BlockChain, typeMux *event.TypeMux) *blockRangeState {
	headCh := make(chan core.ChainHeadEvent, chainHeadChanSize)		// 최신블록 생성 이벤트 수신 채널 생성
	headSub := chain.SubscribeChainHeadEvent(headCh)				// 구독
	syncSub := typeMux.Subscribe(downloader.StartEvent{}, downloader.DoneEvent{}, downloader.FailedEvent{})	// downloader관련되 이벤트 구독을 신청
	
	// blockRangeState 생성
	st := &blockRangeState{
		headCh:  headCh,
		headSub: headSub,
		syncSub: syncSub,
	}

	// 초기상태 설정
	st.update(chain, chain.CurrentBlock())	
	st.prev = *st.next.Load()
	return st
}



// blockRangeBroadcastLoop announces changes in locally-available block range to peers.
// The range to announce is the range that is available in the store, so it's not just
// about imported blocks.
// TODO: 백그라운드에서 실행되면서 로컬 체인의 상태변경을 감지하고 그 변경내용을 브로드캐스팅하는 알림 루프
func (h *handler) blockRangeLoop(st *blockRangeState) {
	defer h.wg.Done()

	for {
		select {
		case ev := <-st.syncSub.Chan():		// 동기화 상태변경 이벤트 수신
			if ev == nil {
				continue
			}
			if _, ok := ev.Data.(downloader.StartEvent); ok && h.snapSync.Load() {
				h.blockRangeWhileSnapSyncing(st)
			}	
		case <-st.headCh:					// 새 블록 이벤트 수신
			st.update(h.chain, h.chain.CurrentBlock())
			if st.shouldSend() {
				h.broadcastBlockRange(st)
			}
		case <-st.headSub.Err():			// 구독종료 이벤트 수신
			return
		}
	}
}




// blockRangeWhileSnapSyncing announces block range updates during snap sync.
// Here we poll the CurrentSnapBlock on a timer and announce updates to it.
// TODO: 스냅동기화가 진행될때만 실행되는 블록 범위 전파 루프
func (h *handler) blockRangeWhileSnapSyncing(st *blockRangeState) {
	tick := time.NewTicker(1 * time.Minute)
	defer tick.Stop()

	for {
		select {
		case <-tick.C:		// 1분마다 주기적으로 스냅동기화 진행 기준으로 blockRangeState 업데이트 + 업데이트된 정보가 이전과 달라져서 전파가 필요한지 체크 -> 필요하면 다른 노드에게 스냅동기화 상황을 브로드캐스팅
			st.update(h.chain, h.chain.CurrentSnapBlock())
			if st.shouldSend() {
				h.broadcastBlockRange(st)
			}
		// back to processing head block updates when sync is done
		case ev := <-st.syncSub.Chan():	// 동기화 완료/실패여부 감지 후 그에 맞는 로직처리
			if ev == nil {
				continue
			}
			switch ev.Data.(type) {
			case downloader.FailedEvent, downloader.DoneEvent:
				return
			}
		// ignore head updates, but exit when the subscription ends
		// 이벤트나 구독 종료를 처리
		case <-st.headCh:
		case <-st.headSub.Err():
			return
		}
	}
}



// broadcastBlockRange sends a range update when one is due.
// TODO: blockRangeState에 준비된 최신블록범위 정보를 모든 피어에게 전파하는 함수
func (h *handler) broadcastBlockRange(state *blockRangeState) {

	// 전송대상 피어목록 복사
	h.peers.lock.Lock()
	peerlist := slices.Collect(maps.Values(h.peers.peers))
	h.peers.lock.Unlock()
	if len(peerlist) == 0 {
		return
	}
	// 전송 메세지 준비
	msg := state.currentRange()
	log.Debug("Sending BlockRangeUpdate", "peers", len(peerlist), "earliest", msg.EarliestBlock, "latest", msg.LatestBlock)
	
	// 브로드캐스팅 수행
	for _, p := range peerlist {
		p.SendBlockRangeUpdate(msg)
	}

	// TODO: blockRangeState의 prev를 next와 동일하게 업데이트	*이래야 최신정보 보냄을 알 수 있고 새 블록이 생성되기전에 다시 보낼 필요가 없어진다.
	state.prev = *state.next.Load()
}



// update assigns the values of the next block range update from the chain.
// TODO: blockRangeState의 다음에 전파할 블록범위 정보인 next를 최신 체인으로 갱신하는 함수
func (st *blockRangeState) update(chain *core.BlockChain, latest *types.Header) {
	earliest, _ := chain.HistoryPruningCutoff()
	st.next.Store(&eth.BlockRangeUpdatePacket{
		EarliestBlock:   min(latest.Number.Uint64(), earliest),
		LatestBlock:     latest.Number.Uint64(),
		LatestBlockHash: latest.Hash(),
	})
}




// shouldSend decides whether it is time to send a block range update. We don't want to
// send these updates constantly, so they will usually only be sent every 32 blocks.
// However, there is a special case: if the range would move back, i.e. due to SetHead, we
// want to send it immediately.
/*
	TODO: 새로운 블록범위의 정보를 다른 피어에게 '즉시' 브로드캐스팅해야하는지 여부를 정하는 함수

	*True 알려야함
	1. 블록체인이 이전상태로 돌아간 경우    next.LatestBlock < st.prev.LatestBlock
		다음에 보낸 블록번호가 더 작다는 것은 블록체인의 재구성이 발생하여서 더 짧은 체인으로 후퇴함을 의미한다. 즉, 이례적인 이벤트이므로 즉시 피어에게 알려야한다.
	2. 마지막 정보를 보낸시점으로부터 최소 32개의 블록이 추가된 경우 (새로운 블록생성마다 매번알리는건 낭비므로 ) 

*/
func (st *blockRangeState) shouldSend() bool {
	next := st.next.Load()
	return next.LatestBlock < st.prev.LatestBlock ||
		next.LatestBlock-st.prev.LatestBlock >= 32
}



// blockRangeState가 구독하던걸 모두 정리하는 함수
func (st *blockRangeState) stop() {
	st.syncSub.Unsubscribe()
	st.headSub.Unsubscribe()
}

// currentRange returns the current block range.
// This is safe to call from any goroutine.
// blockRangeState의 최신블록 범위 정보를 리턴하는 함수      next
func (st *blockRangeState) currentRange() eth.BlockRangeUpdatePacket {
	return *st.next.Load()
}
