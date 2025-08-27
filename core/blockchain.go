// Copyright 2014 The go-ethereum Authors
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

// Package core implements the Ethereum consensus protocol.

/*
	TODO: 블록체인 데이터베이스를 직접 다루고, 체인 재구성과 같은 복잡한 로직을 처리하는 블록체인 데이터베이스 총괄 관리자 역할의 파일





*/

package core

import (
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/common/prque"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core/history"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/state/snapshot"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/internal/syncx"
	"github.com/ethereum/go-ethereum/internal/version"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/hashdb"
	"github.com/ethereum/go-ethereum/triedb/pathdb"
)

// Geth노드 성능/상태 측정용 변수 +  ERROR 객체 변수들 선언
var (

	// Geth 노드의 성능과 상태를 실시간으로 모니터링하기 위한 측정 변수들
	headBlockGauge          = metrics.NewRegisteredGauge("chain/head/block", nil)
	headHeaderGauge         = metrics.NewRegisteredGauge("chain/head/header", nil)
	headFastBlockGauge      = metrics.NewRegisteredGauge("chain/head/receipt", nil)
	headFinalizedBlockGauge = metrics.NewRegisteredGauge("chain/head/finalized", nil)
	headSafeBlockGauge      = metrics.NewRegisteredGauge("chain/head/safe", nil)

	chainInfoGauge   = metrics.NewRegisteredGaugeInfo("chain/info", nil)
	chainMgaspsMeter = metrics.NewRegisteredResettingTimer("chain/mgasps", nil)

	accountReadTimer   = metrics.NewRegisteredResettingTimer("chain/account/reads", nil)
	accountHashTimer   = metrics.NewRegisteredResettingTimer("chain/account/hashes", nil)
	accountUpdateTimer = metrics.NewRegisteredResettingTimer("chain/account/updates", nil)
	accountCommitTimer = metrics.NewRegisteredResettingTimer("chain/account/commits", nil)

	storageReadTimer   = metrics.NewRegisteredResettingTimer("chain/storage/reads", nil)
	storageUpdateTimer = metrics.NewRegisteredResettingTimer("chain/storage/updates", nil)
	storageCommitTimer = metrics.NewRegisteredResettingTimer("chain/storage/commits", nil)

	accountCacheHitMeter  = metrics.NewRegisteredMeter("chain/account/reads/cache/process/hit", nil)
	accountCacheMissMeter = metrics.NewRegisteredMeter("chain/account/reads/cache/process/miss", nil)
	storageCacheHitMeter  = metrics.NewRegisteredMeter("chain/storage/reads/cache/process/hit", nil)
	storageCacheMissMeter = metrics.NewRegisteredMeter("chain/storage/reads/cache/process/miss", nil)

	accountCacheHitPrefetchMeter  = metrics.NewRegisteredMeter("chain/account/reads/cache/prefetch/hit", nil)
	accountCacheMissPrefetchMeter = metrics.NewRegisteredMeter("chain/account/reads/cache/prefetch/miss", nil)
	storageCacheHitPrefetchMeter  = metrics.NewRegisteredMeter("chain/storage/reads/cache/prefetch/hit", nil)
	storageCacheMissPrefetchMeter = metrics.NewRegisteredMeter("chain/storage/reads/cache/prefetch/miss", nil)

	accountReadSingleTimer = metrics.NewRegisteredResettingTimer("chain/account/single/reads", nil)
	storageReadSingleTimer = metrics.NewRegisteredResettingTimer("chain/storage/single/reads", nil)

	snapshotCommitTimer = metrics.NewRegisteredResettingTimer("chain/snapshot/commits", nil)
	triedbCommitTimer   = metrics.NewRegisteredResettingTimer("chain/triedb/commits", nil)

	blockInsertTimer          = metrics.NewRegisteredResettingTimer("chain/inserts", nil)
	blockValidationTimer      = metrics.NewRegisteredResettingTimer("chain/validation", nil)
	blockCrossValidationTimer = metrics.NewRegisteredResettingTimer("chain/crossvalidation", nil)
	blockExecutionTimer       = metrics.NewRegisteredResettingTimer("chain/execution", nil)
	blockWriteTimer           = metrics.NewRegisteredResettingTimer("chain/write", nil)

	blockReorgMeter     = metrics.NewRegisteredMeter("chain/reorg/executes", nil)
	blockReorgAddMeter  = metrics.NewRegisteredMeter("chain/reorg/add", nil)
	blockReorgDropMeter = metrics.NewRegisteredMeter("chain/reorg/drop", nil)

	blockPrefetchExecuteTimer    = metrics.NewRegisteredResettingTimer("chain/prefetch/executes", nil)
	blockPrefetchInterruptMeter  = metrics.NewRegisteredMeter("chain/prefetch/interrupts", nil)
	blockPrefetchTxsInvalidMeter = metrics.NewRegisteredMeter("chain/prefetch/txs/invalid", nil)
	blockPrefetchTxsValidMeter   = metrics.NewRegisteredMeter("chain/prefetch/txs/valid", nil)


	// 여러 ERROR 객체들
	errInsertionInterrupted = errors.New("insertion is interrupted")
	errChainStopped         = errors.New("blockchain is stopped")
	errInvalidOldChain      = errors.New("invalid old chain")
	errInvalidNewChain      = errors.New("invalid new chain")
)

// 시간간격 변수 (3분)
var (
	forkReadyInterval = 3 * time.Minute
)

// TODO: 성능 최적화를 위한 Cache 크기, 데이터베이스의 구조 호환성을 관리하기 위한 버전 번호를 정의하는 상수들
//		 여기서의 캐싱은 하드웨어에 대한 캐시를 의미하는 것이 아니라 주 메모리 일부를 캐시처럼 사용
const (
	bodyCacheLimit     = 256		// 최대로 캐싱할 트랜잭션 목록 수
	blockCacheLimit    = 256		// 최대로 캐싱할 블록 수
	receiptsCacheLimit = 32			// 트랜잭션 receipt의 쵀대 캐싱할 수
	txLookupCacheLimit = 1024		// 특정 트랜잭션이 어떤 블록에 포함되어있는지 빠르게 찾기 위한 조회정보 (lock up)을 최대 몇개까지 캐싱할지

	// BlockChainVersion ensures that an incompatible database forces a resync from scratch.
	//
	// Changelog:
	//
	// - Version 4
	//   The following incompatible database changes were added:
	//   * the `BlockNumber`, `TxHash`, `TxIndex`, `BlockHash` and `Index` fields of log are deleted
	//   * the `Bloom` field of receipt is deleted
	//   * the `BlockIndex` and `TxIndex` fields of txlookup are deleted
	//
	// - Version 5
	//  The following incompatible database changes were added:
	//    * the `TxHash`, `GasCost`, and `ContractAddress` fields are no longer stored for a receipt
	//    * the `TxHash`, `GasCost`, and `ContractAddress` fields are computed by looking up the
	//      receipts' corresponding block
	//
	// - Version 6
	//  The following incompatible database changes were added:
	//    * Transaction lookup information stores the corresponding block number instead of block hash
	//
	// - Version 7
	//  The following incompatible database changes were added:
	//    * Use freezer as the ancient database to maintain all ancient data
	//
	// - Version 8
	//  The following incompatible database changes were added:
	//    * New scheme for contract code in order to separate the codes and trie nodes
	//
	// - Version 9
	//  The following incompatible database changes were added:
	//  * Total difficulty has been removed from both the key-value store and the ancient store.
	//  * The metadata structure of freezer is changed by adding 'flushOffset'
	BlockChainVersion uint64 = 9	// TODO: Geth가 사용하는 블록체인 DB  내부구조의 버전을 나타낸다.
)








// BlockChainConfig contains the configuration of the BlockChain object.
// TODO: Blockchain 객체를 생성하는데에 사용하는 모든 설정값 구조체
type BlockChainConfig struct {
	// Trie database related options
	TrieCleanLimit       int           // Memory allowance (MB) to use for caching trie nodes in memory
	TrieDirtyLimit       int           // Memory limit (MB) at which to start flushing dirty trie nodes to disk
	TrieTimeLimit        time.Duration // Time limit after which to flush the current in-memory trie to disk
	TrieNoAsyncFlush     bool          // Whether the asynchronous buffer flushing is disallowed
	TrieJournalDirectory string        // Directory path to the journal used for persisting trie data across node restarts

	Preimages    bool   // Whether to store preimage of trie key to the disk
	StateHistory uint64 // Number of blocks from head whose state histories are reserved.
	StateScheme  string // Scheme used to store ethereum states and merkle tree nodes on top
	ArchiveMode  bool   // Whether to enable the archive mode

	// State snapshot related options
	SnapshotLimit   int  // Memory allowance (MB) to use for caching snapshot entries in memory
	SnapshotNoBuild bool // Whether the background generation is allowed
	SnapshotWait    bool // Wait for snapshot construction on startup. TODO(karalabe): This is a dirty hack for testing, nuke it

	// This defines the cutoff block for history expiry.
	// Blocks before this number may be unavailable in the chain database.
	ChainHistoryMode history.HistoryMode

	// Misc options
	NoPrefetch bool            // Whether to disable heuristic state prefetching when processing blocks
	Overrides  *ChainOverrides // Optional chain config overrides
	VmConfig   vm.Config       // Config options for the EVM Interpreter

	// TxLookupLimit specifies the maximum number of blocks from head for which
	// transaction hashes will be indexed.
	//
	// If the value is zero, all transactions of the entire chain will be indexed.
	// If the value is -1, indexing is disabled.
	TxLookupLimit int64
}




/*
	TODO: 아래의 BlockchainConfig 관련된 함수들은 모두 "Builder Pattern"을 따라서 구현된다.
		  (체이닝을 통해서 객체의 멤버변수들을 초기화할 수 있도록)

		EX) DefaultConfig().WithArchive(true).WithStateScheme("test_scheme"). .... 
*/


// DefaultConfig returns the default config.
// Note the returned object is safe to modify!
// default BLockchainConfig 객체를 리턴하는 함수
func DefaultConfig() *BlockChainConfig {
	return &BlockChainConfig{
		TrieCleanLimit:   256,
		TrieDirtyLimit:   256,
		TrieTimeLimit:    5 * time.Minute,
		StateScheme:      rawdb.HashScheme,
		SnapshotLimit:    256,
		SnapshotWait:     true,
		ChainHistoryMode: history.KeepAll,
		// Transaction indexing is disabled by default.
		// This is appropriate for most unit tests.
		TxLookupLimit: -1,
	}
}


// WithArchive enables/disables archive mode on the config.
// BlockchainConfig 객체의 Archive Mode를 켜거나 끄는 helper 함수		*Archive Mode는 지금까지의 모든 데이터를 저장하는 모드 (full node)
func (cfg BlockChainConfig) WithArchive(on bool) *BlockChainConfig {
	cfg.ArchiveMode = on
	return &cfg
}


// WithStateScheme sets the state storage scheme on the config.
// BlockchainConfig 객체의 state 데이터를 어떤 규칙으로 DB에 저장할지 지정하는 helper 함수
func (cfg BlockChainConfig) WithStateScheme(scheme string) *BlockChainConfig {
	cfg.StateScheme = scheme
	return &cfg
}


// WithNoAsyncFlush enables/disables asynchronous buffer flushing mode on the config.
// BlochainConfig 객체에서 Trie데이터의 비동기 디스크 쓰기 기능을 켜거나 끄는 helper 함수 
func (cfg BlockChainConfig) WithNoAsyncFlush(on bool) *BlockChainConfig {
	cfg.TrieNoAsyncFlush = on
	return &cfg
}






// triedbConfig derives the configures for trie database.
// TODO: BlockchainConfig라는 범용 설정 객체를 => 실제 Trie DB를 초기화하는데 필요한 구체적잉ㄴ triedb.Config 객체로 변환하는 함수
func (cfg *BlockChainConfig) triedbConfig(isVerkle bool) *triedb.Config {
	config := &triedb.Config{
		Preimages: cfg.Preimages,
		IsVerkle:  isVerkle,
	}
	if cfg.StateScheme == rawdb.HashScheme {
		config.HashDB = &hashdb.Config{
			CleanCacheSize: cfg.TrieCleanLimit * 1024 * 1024,
		}
	}
	if cfg.StateScheme == rawdb.PathScheme {
		config.PathDB = &pathdb.Config{
			StateHistory:        cfg.StateHistory,
			EnableStateIndexing: cfg.ArchiveMode,
			TrieCleanSize:       cfg.TrieCleanLimit * 1024 * 1024,
			StateCleanSize:      cfg.SnapshotLimit * 1024 * 1024,
			JournalDirectory:    cfg.TrieJournalDirectory,

			// TODO(rjl493456442): The write buffer represents the memory limit used
			// for flushing both trie data and state data to disk. The config name
			// should be updated to eliminate the confusion.
			WriteBufferSize: cfg.TrieDirtyLimit * 1024 * 1024,
			NoAsyncFlush:    cfg.TrieNoAsyncFlush,
		}
	}
	return config
}









// txLookup is wrapper over transaction lookup along with the corresponding
// transaction object.
// TODO: txLookup 		특정 tx의 조회정보와 실제tx를 하나로 몪어주는 구조체
type txLookup struct {
	lookup      *rawdb.LegacyTxLookupEntry		// tx의 위치정보 : 해당 트랜잭션의 Block number, Block hash, Transaction index 같은 메타데이터가 들어있다.
	transaction *types.Transaction				// tx의 실제정보 : tx의 모든 내용을 포함한 객체
}


// BlockChain represents the canonical chain given a database with a genesis
// block. The Blockchain manages chain imports, reverts, chain reorganisations.
//
// Importing blocks in to the block chain happens according to the set of rules
// defined by the two stage Validator. Processing of blocks is done using the
// Processor which processes the included transaction. The validation of the state
// is done in the second part of the Validator. Failing results in aborting of
// the import.
//
// The BlockChain also helps in returning blocks from **any** chain included
// in the database as well as blocks that represents the canonical chain. It's
// important to note that GetBlock can return any block and does not need to be
// included in the canonical one where as GetBlockByNumber always represents the
// canonical chain.

// TODO: Blockchain Structure 		모든 데이터관리, 새로운 블록 검증, 체인 유지하는 모든 구성요소를 담고 있는 구조체
// TODO: 설정&DB,	이벤트를 알리기 위한 통신채널,	 체인의 상태를 가르키는 포인터 (헤더등을 가르킴),  성능 최적화를 위한 캐시,	 합의, 형식검증, 상태변경등의 역할을 하는 엔진등이 포함되어 있다.
// TODO: 구조는 일반적으로 Blockchain가 상위모듈, 그 하위의 합의, 검증과 관련된 하위 모듈을 갖는 구조이다. (이런 하위모듈의 인터페이스를 갖고있음)
type BlockChain struct {

	// TODO: 설정 및 DB
	chainConfig *params.ChainConfig // Chain & network configuration	합의규칙
	cfg         *BlockChainConfig   // Blockchain configuration			노드 운영과 관련된 설정

	db            ethdb.Database                   // low level DB 핸들		block, receipt 등 최종 데이터가 저장
	snaps         *snapshot.Tree                   // Snapshot tree for fast trie leaf access	스냅샷 데이터 관리 객체
	triegc        *prque.Prque[int64, common.Hash] // Priority queue mapping block numbers to tries to gc	오래된 상태 데이터를 삭제하기 위한 우선순위 큐
	gcproc        time.Duration                    // Accumulates canonical block processing for trie dumping	
	lastWrite     uint64                           // Last block when the state was flushed			
	flushInterval atomic.Int64                     // Time interval (processing time) after which to flush a state
	triedb        *triedb.Database                 // The database handler for maintaining trie nodes.			머클 패트리샤 트라이 노드를 저장하고 관리하는 DB
	statedb       *state.CachingDB                 // State database to reuse between imports (contains state cache)	상태 DB (여러 블록을 처리하는 동안 계정정보를 메모리에 캐싱하는 역할)
	txIndexer     *txIndexer                       // Transaction indexer, might be nil if not enabled			tx hash로 tx를 빠르게 찾을 수 있도록 돕는 인덱서


	// TODO: Blockchain 내부나 외부의 다른 모듈에게 중요한 이벤트를 알리기 위한 통신 채널 (체인 재구성, 새 블록, 최신 블록 변경 등..)
	hc               *HeaderChain
	rmLogsFeed       event.Feed
	chainFeed        event.Feed
	chainHeadFeed    event.Feed
	logsFeed         event.Feed
	blockProcFeed    event.Feed
	blockProcCounter int32
	scope            event.SubscriptionScope
	genesisBlock     *types.Block


	// This mutex synchronizes chain write operations.
	// Readers don't need to take it, they can just read the database.
	// TODO: 체인 상태 포인터  (블록체인의 현재 상태가 아디인지 체크하는 역할)
	chainmu *syncx.ClosableMutex

	currentBlock      atomic.Pointer[types.Header] // Current head of the chain		정식체인의 최신블록 헤더를 가르킨다.
	currentSnapBlock  atomic.Pointer[types.Header] // Current head of snap-sync		스냅샷 동기화중일때 현재까지 동기화된 최신블록 헤더
	currentFinalBlock atomic.Pointer[types.Header] // Latest (consensus) finalized block	pos합의에 따라 최종확정된 블록 헤더
	currentSafeBlock  atomic.Pointer[types.Header] // Latest (consensus) safe block			pos합의에 따라 안전하다고 간주되는 블록 헤더
	historyPrunePoint atomic.Pointer[history.PrunePoint]	// 가지치기 기준점   *이 이전으로는 데이터가 삭제됬을 수 있음을 알려줌

	// TODO: 성능 최적화를 위한 LRU Cache	(block body, receipt, block, txloopup 등의 정보를 저장하는 캐시)
	bodyCache     *lru.Cache[common.Hash, *types.Body]	
	bodyRLPCache  *lru.Cache[common.Hash, rlp.RawValue]
	receiptsCache *lru.Cache[common.Hash, []*types.Receipt] // Receipts cache with all fields derived
	blockCache    *lru.Cache[common.Hash, *types.Block]

	txLookupLock  sync.RWMutex
	txLookupCache *lru.Cache[common.Hash, txLookup]

	stopping      atomic.Bool // false if chain is running, true when stopped
	procInterrupt atomic.Bool // interrupt signaler for block processing


	// TODO: 핵심로직 & 인터페이스
	engine     consensus.Engine		// 합의 엔진
	validator  Validator // Block and state validator interface		블록의 구조와 내용이 규칙에 맞는지 검증하는 역할
	prefetcher statePrefetcher
	processor  Processor // Block transaction processor interface	블록에 포함된 모든 tx를 순차적으로 실행하여서 상태를 변경하는 역할
	logger     *tracing.Hooks

	lastForkReadyAlert time.Time // Last time there was a fork readiness print out
}



// NewBlockChain returns a fully initialised block chain using information
// available in the database. It initialises the default Ethereum Validator
// and Processor.
// TODO: Blokchain 객체를 생성하는 함수
// TODO: DB핸들러 생성, 객체 변수 초기화, 제네시스블록 등록, 체인상태 로딩 및 자동 복구
func NewBlockChain(db ethdb.Database, genesis *Genesis, engine consensus.Engine, cfg *BlockChainConfig) (*BlockChain, error) {
	
	if cfg == nil {
		cfg = DefaultConfig()
	}
	// Open trie database with provided config
	enableVerkle, err := EnableVerkleAtGenesis(db, genesis)
	if err != nil {
		return nil, err
	}

	triedb := triedb.NewDatabase(db, cfg.triedbConfig(enableVerkle)) // TODO: config를 바탕으로 state를 저장하는 merkle patricia tree를 관리할 DB handler를 생성

	// Write the supplied genesis to the database if it has not been initialized
	// yet. The corresponding chain config will be returned, either from the
	// provided genesis or from the locally stored configuration if the genesis
	// has already been initialized.
	// TODO: genesis block 처리 (등록) + 체인정보 출력
	chainConfig, genesisHash, compatErr, err := SetupGenesisBlockWithOverride(db, triedb, genesis, cfg.Overrides)
	if err != nil {
		return nil, err
	}
	log.Info("")
	log.Info(strings.Repeat("-", 153))
	for _, line := range strings.Split(chainConfig.Description(), "\n") {
		log.Info(line)
	}
	log.Info(strings.Repeat("-", 153))
	log.Info("")


	// TODO: Blockchain 객체 기본 생성 및 초기화
	bc := &BlockChain{
		chainConfig:   chainConfig,
		cfg:           cfg,
		db:            db,
		triedb:        triedb,
		triegc:        prque.New[int64, common.Hash](nil),
		chainmu:       syncx.NewClosableMutex(),
		bodyCache:     lru.NewCache[common.Hash, *types.Body](bodyCacheLimit),
		bodyRLPCache:  lru.NewCache[common.Hash, rlp.RawValue](bodyCacheLimit),
		receiptsCache: lru.NewCache[common.Hash, []*types.Receipt](receiptsCacheLimit),
		blockCache:    lru.NewCache[common.Hash, *types.Block](blockCacheLimit),
		txLookupCache: lru.NewCache[common.Hash, txLookup](txLookupCacheLimit),
		engine:        engine,
		logger:        cfg.VmConfig.Tracer,
	}

	bc.hc, err = NewHeaderChain(db, chainConfig, engine, bc.insertStopped)
	if err != nil {
		return nil, err
	}
	bc.flushInterval.Store(int64(cfg.TrieTimeLimit))
	bc.statedb = state.NewDatabase(bc.triedb, nil)
	bc.validator = NewBlockValidator(chainConfig, bc)
	bc.prefetcher = newStatePrefetcher(chainConfig, bc.hc)
	bc.processor = NewStateProcessor(chainConfig, bc.hc)

	genesisHeader := bc.GetHeaderByNumber(0)
	if genesisHeader == nil {
		return nil, ErrNoGenesis
	}
	bc.genesisBlock = types.NewBlockWithHeader(genesisHeader)

	bc.currentBlock.Store(nil)
	bc.currentSnapBlock.Store(nil)
	bc.currentFinalBlock.Store(nil)
	bc.currentSafeBlock.Store(nil)

	// Update chain info data metrics
	chainInfoGauge.Update(metrics.GaugeInfoValue{"chain_id": bc.chainConfig.ChainID.String()})

	// If Geth is initialized with an external ancient store, re-initialize the
	// missing chain indexes and chain flags. This procedure can survive crash
	// and can be resumed in next restart since chain flags are updated in last step.
	if bc.empty() {
		rawdb.InitDatabaseFromFreezer(bc.db)
	}

	// TODO: 체인 상태 로딩 및 자동 복구
	// Load blockchain states from disk
	if err := bc.loadLastState(); err != nil {
		return nil, err
	}
	// Make sure the state associated with the block is available, or log out
	// if there is no available state, waiting for state sync.
	head := bc.CurrentBlock()
	if !bc.HasState(head.Root) {
		if head.Number.Uint64() == 0 {
			// The genesis state is missing, which is only possible in the path-based
			// scheme. This situation occurs when the initial state sync is not finished
			// yet, or the chain head is rewound below the pivot point. In both scenarios,
			// there is no possible recovery approach except for rerunning a snap sync.
			// Do nothing here until the state syncer picks it up.
			log.Info("Genesis state is missing, wait state sync")
		} else {
			// Head state is missing, before the state recovery, find out the disk
			// layer point of snapshot(if it's enabled). Make sure the rewound point
			// is lower than disk layer.
			//
			// Note it's unnecessary in path mode which always keep trie data and
			// state data consistent.
			var diskRoot common.Hash
			if bc.cfg.SnapshotLimit > 0 && bc.cfg.StateScheme == rawdb.HashScheme {
				diskRoot = rawdb.ReadSnapshotRoot(bc.db)
			}
			if diskRoot != (common.Hash{}) {
				log.Warn("Head state missing, repairing", "number", head.Number, "hash", head.Hash(), "snaproot", diskRoot)

				snapDisk, err := bc.setHeadBeyondRoot(head.Number.Uint64(), 0, diskRoot, true)
				if err != nil {
					return nil, err
				}
				// Chain rewound, persist old snapshot number to indicate recovery procedure
				if snapDisk != 0 {
					rawdb.WriteSnapshotRecoveryNumber(bc.db, snapDisk)
				}
			} else {
				log.Warn("Head state missing, repairing", "number", head.Number, "hash", head.Hash())
				if _, err := bc.setHeadBeyondRoot(head.Number.Uint64(), 0, common.Hash{}, true); err != nil {
					return nil, err
				}
			}
		}
	}
	// Ensure that a previous crash in SetHead doesn't leave extra ancients
	if frozen, err := bc.db.Ancients(); err == nil && frozen > 0 {
		var (
			needRewind bool
			low        uint64
		)
		// The head full block may be rolled back to a very low height due to
		// blockchain repair. If the head full block is even lower than the ancient
		// chain, truncate the ancient store.
		fullBlock := bc.CurrentBlock()
		if fullBlock != nil && fullBlock.Hash() != bc.genesisBlock.Hash() && fullBlock.Number.Uint64() < frozen-1 {
			needRewind = true
			low = fullBlock.Number.Uint64()
		}
		// In snap sync, it may happen that ancient data has been written to the
		// ancient store, but the LastFastBlock has not been updated, truncate the
		// extra data here.
		snapBlock := bc.CurrentSnapBlock()
		if snapBlock != nil && snapBlock.Number.Uint64() < frozen-1 {
			needRewind = true
			if snapBlock.Number.Uint64() < low || low == 0 {
				low = snapBlock.Number.Uint64()
			}
		}
		if needRewind {
			log.Error("Truncating ancient chain", "from", bc.CurrentHeader().Number.Uint64(), "to", low)
			if err := bc.SetHead(low); err != nil {
				return nil, err
			}
		}
	}


	// TODO: 최종 준비 및 반환
	// The first thing the node will do is reconstruct the verification data for
	// the head block (ethash cache or clique voting snapshot). Might as well do
	// it in advance.
	bc.engine.VerifyHeader(bc, bc.CurrentHeader())

	if bc.logger != nil && bc.logger.OnBlockchainInit != nil {
		bc.logger.OnBlockchainInit(chainConfig)
	}
	if bc.logger != nil && bc.logger.OnGenesisBlock != nil {
		if block := bc.CurrentBlock(); block.Number.Uint64() == 0 {
			alloc, err := getGenesisState(bc.db, block.Hash())
			if err != nil {
				return nil, fmt.Errorf("failed to get genesis state: %w", err)
			}
			if alloc == nil {
				return nil, errors.New("live blockchain tracer requires genesis alloc to be set")
			}
			bc.logger.OnGenesisBlock(bc.genesisBlock, alloc)
		}
	}
	bc.setupSnapshot()

	// Rewind the chain in case of an incompatible config upgrade.
	if compatErr != nil {
		log.Warn("Rewinding chain to upgrade configuration", "err", compatErr)
		if compatErr.RewindToTime > 0 {
			bc.SetHeadWithTimestamp(compatErr.RewindToTime)
		} else {
			bc.SetHead(compatErr.RewindToBlock)
		}
		rawdb.WriteChainConfig(db, genesisHash, chainConfig)
	}

	// Start tx indexer if it's enabled.
	if bc.cfg.TxLookupLimit >= 0 {
		bc.txIndexer = newTxIndexer(uint64(bc.cfg.TxLookupLimit), bc)
	}
	return bc, nil
}



// TODO: 스냅샷 동기화에 사용되는 스냅샷 트리를 초기화하고 설정 + Blockchain 구조체에 할당하는 역할
func (bc *BlockChain) setupSnapshot() {
	// Short circuit if the chain is established with path scheme, as the
	// state snapshot has been integrated into path database natively.
	// 현재 노드가 pathScheme를 사용한다면 그냥 즉시 종료  *PathScheme는 snapshot기능이 DB에 이미 존재하므로 필요없기에
	if bc.cfg.StateScheme == rawdb.PathScheme {
		return
	}

	// Load any existing snapshot, regenerating it if loading failed
	// 스냅샷 동기화 허용 여부체크 + 복구모드 확인 뒤에 스냅샷객체를 생성해서 블록체인 구조체에 할당
	if bc.cfg.SnapshotLimit > 0 {
		// If the chain was rewound past the snapshot persistent layer (causing
		// a recovery block number to be persisted to disk), check if we're still
		// in recovery mode and in that case, don't invalidate the snapshot on a
		// head mismatch.
		var recover bool
		head := bc.CurrentBlock()
		if layer := rawdb.ReadSnapshotRecoveryNumber(bc.db); layer != nil && *layer >= head.Number.Uint64() {
			log.Warn("Enabling snapshot recovery", "chainhead", head.Number, "diskbase", *layer)
			recover = true
		}
		// snapshot 객체 생성 + 그 객체를 Blockchain 객체에 할당
		snapconfig := snapshot.Config{
			CacheSize:  bc.cfg.SnapshotLimit,
			Recovery:   recover,
			NoBuild:    bc.cfg.SnapshotNoBuild,
			AsyncBuild: !bc.cfg.SnapshotWait,
		}
		bc.snaps, _ = snapshot.New(snapconfig, bc.db, bc.triedb, head.Root)

		// Re-initialize the state database with snapshot
		// state DB 재초기화
		bc.statedb = state.NewDatabase(bc.triedb, bc.snaps)
	}
}



// empty returns an indicator whether the blockchain is empty.
// Note, it's a special case that we connect a non-empty ancient
// database with an empty node, so that we can plugin the ancient
// into node seamlessly.

//블록체인 DB가 비어있는지 (genesis block only)인지 확인하는 함수
func (bc *BlockChain) empty() bool {
	genesis := bc.genesisBlock.Hash()	// genesis block's hash
	for _, hash := range []common.Hash{rawdb.ReadHeadBlockHash(bc.db), rawdb.ReadHeadHeaderHash(bc.db), rawdb.ReadHeadFastBlockHash(bc.db)} {
		if hash != genesis {
			return false  
		}
	}
	return true
}



// loadLastState loads the last known chain state from the database. This method
// assumes that the chain manager mutex is held.

// TODO: Geth노드가 다시 시작될떄, 디스크에 저장된 마지막 블록체인 상태를 읽어와 메모리에 올리는 함수
// TODO: DB에 있는 최신블록 해시, 헤더, 블록 전체를 가져오고 관련된 변수등을 모두 갱신한다.
// TODO: 여기서 판단하는 것은 이 데이터가 깨졌는지만 판단한다. 전체적으로 최신상태인지는 상관하지 않음
func (bc *BlockChain) loadLastState() error {
	// Restore the last known head block
	head := rawdb.ReadHeadBlockHash(bc.db)				// TODO: DB에서 최신블록의 해시를 읽어온다.
	if head == (common.Hash{}) {						// 만약 해시가 비어있다면 손상된 것으로 간주하고, 체인을 제네시스상태로 리셋
		// Corrupt or empty database, init from scratch
		log.Warn("Empty database, resetting chain")
		return bc.Reset()
	}
	headHeader := bc.GetHeaderByHash(head)				// TODO: 읽어온 해시로 해당 블록의 헤더를 가져온다.
	if headHeader == nil {								// 해시가 없다면 역시 손상된것으로 간주 제네시스상태로 초기화
		// Corrupt or empty database, init from scratch
		log.Warn("Head header missing, resetting chain", "hash", head)
		return bc.Reset()
	}


	// TODO: 트랜잭션 목록도 포함된 전체 블록 데이터를 가져온다.  마찬가지로 블록 못가져온다면 제네시스로 리셋
	var headBlock *types.Block
	if cmp := headHeader.Number.Cmp(new(big.Int)); cmp == 1 {
		// Make sure the entire head block is available.
		headBlock = bc.GetBlockByHash(head)
	} else if cmp == 0 {
		// On a pruned node the block body might not be available. But a pruned
		// block should never be the head block. The only exception is when, as
		// a last resort, chain is reset to genesis.
		headBlock = bc.genesisBlock
	}
	if headBlock == nil {
		// Corrupt or empty database, init from scratch
		log.Warn("Head block missing, resetting chain", "hash", head)
		return bc.Reset()
	}

	// Everything seems to be fine, set as the head block
	// TODO: 모든 검증을 통과하면 이 헤더를 최신블록으로 메모리에 설정
	bc.currentBlock.Store(headHeader)
	headBlockGauge.Update(int64(headBlock.NumberU64()))	// 모니터링을 위한 최신블록 번호 메트릭도 업데이트

	// Restore the last known head header
	// TODO: 헤더체인의 최신 헤더 해시를 읽어오고 읽어온 헤더를 헤더체인의 현재 헤더로 설정
	if head := rawdb.ReadHeadHeaderHash(bc.db); head != (common.Hash{}) {
		if header := bc.GetHeaderByHash(head); header != nil {
			headHeader = header
		}
	}
	bc.hc.SetCurrentHeader(headHeader)

	// Initialize history pruning.
	// TODO: 로드한 최신 블록 번호 기준으로 가지치기를 결정하는 메커니즘을 초기화
	latest := max(headBlock.NumberU64(), headHeader.Number.Uint64())
	if err := bc.initializeHistoryPruning(latest); err != nil {
		return err
	}

	// Restore the last known head snap block
	// TODO: 최신 스냅 블록 포인터를 갱신
	bc.currentSnapBlock.Store(headBlock.Header())
	headFastBlockGauge.Update(int64(headBlock.NumberU64()))

	if head := rawdb.ReadHeadFastBlockHash(bc.db); head != (common.Hash{}) {
		if block := bc.GetBlockByHash(head); block != nil {
			bc.currentSnapBlock.Store(block.Header())
			headFastBlockGauge.Update(int64(block.NumberU64()))
		}
	}

	// Restore the last known finalized block and safe block
	// Note: the safe block is not stored on disk and it is set to the last
	// known finalized block on startup
	// TODO: DB에서 마지막으로 최종 확정된 블록해시를 읽어온다. 읽어온것을 최종확정블록과 안전한 블록 포인터에 모두 설정
	if head := rawdb.ReadFinalizedBlockHash(bc.db); head != (common.Hash{}) {
		if block := bc.GetBlockByHash(head); block != nil {
			bc.currentFinalBlock.Store(block.Header())
			headFinalizedBlockGauge.Update(int64(block.NumberU64()))
			bc.currentSafeBlock.Store(block.Header())
			headSafeBlockGauge.Update(int64(block.NumberU64()))
		}
	}

	// Issue a status log for the user
	// TODO: 상태로그 출력
	var (
		currentSnapBlock  = bc.CurrentSnapBlock()
		currentFinalBlock = bc.CurrentFinalBlock()
	)
	if headHeader.Hash() != headBlock.Hash() {
		log.Info("Loaded most recent local header", "number", headHeader.Number, "hash", headHeader.Hash(), "age", common.PrettyAge(time.Unix(int64(headHeader.Time), 0)))
	}
	log.Info("Loaded most recent local block", "number", headBlock.Number(), "hash", headBlock.Hash(), "age", common.PrettyAge(time.Unix(int64(headBlock.Time()), 0)))
	if headBlock.Hash() != currentSnapBlock.Hash() {
		log.Info("Loaded most recent local snap block", "number", currentSnapBlock.Number, "hash", currentSnapBlock.Hash(), "age", common.PrettyAge(time.Unix(int64(currentSnapBlock.Time), 0)))
	}
	if currentFinalBlock != nil {
		log.Info("Loaded most recent local finalized block", "number", currentFinalBlock.Number, "hash", currentFinalBlock.Hash(), "age", common.PrettyAge(time.Unix(int64(currentFinalBlock.Time), 0)))
	}
	if pivot := rawdb.ReadLastPivotNumber(bc.db); pivot != nil {
		log.Info("Loaded last snap-sync pivot marker", "number", *pivot)
	}
	if pruning := bc.historyPrunePoint.Load(); pruning != nil {
		log.Info("Chain history is pruned", "earliest", pruning.BlockNumber, "hash", pruning.BlockHash)
	}
	return nil
}



// initializeHistoryPruning sets bc.historyPrunePoint.
// TODO: config에 맞는 블록 히스토리 보존 정책에 맞추어 노드의 "가지치기 기준점"을 초기화하고 유효성을 검사하는 함수 (NewBlockChain 생성자에서 호출된다.) 가지치기를 수행하진 않음
func (bc *BlockChain) initializeHistoryPruning(latest uint64) error {
	freezerTail, _ := bc.db.Tail()		// TODO: Freezer DB를확인하여서 가장 오래된 블럭번호를 가져온다.

	// TODO: chainHistoryMode에 따라 분기 처리한다.
	switch bc.cfg.ChainHistoryMode {
	case history.KeepAll:		// 모든 히스토리 보존 모드  (정상적일 경우 freezeTail == 0)
		if freezerTail == 0 {
			return nil
		}
		// The database was pruned somehow, so we need to figure out if it's a known
		// configuration or an error.
		predefinedPoint := history.PrunePoints[bc.genesisBlock.Hash()]
		if predefinedPoint == nil || freezerTail != predefinedPoint.BlockNumber {
			log.Error("Chain history database is pruned with unknown configuration", "tail", freezerTail)
			return errors.New("unexpected database tail")
		}
		bc.historyPrunePoint.Store(predefinedPoint)
		return nil

	case history.KeepPostMerge:		// 머지 이후 데이터만 보존 모드 
		if freezerTail == 0 && latest != 0 {
			// This is the case where a user is trying to run with --history.chain
			// postmerge directly on an existing DB. We could just trigger the pruning
			// here, but it'd be a bit dangerous since they may not have intended this
			// action to happen. So just tell them how to do it.
			log.Error(fmt.Sprintf("Chain history mode is configured as %q, but database is not pruned.", bc.cfg.ChainHistoryMode.String()))
			log.Error(fmt.Sprintf("Run 'geth prune-history' to prune pre-merge history."))
			return errors.New("history pruning requested via configuration")
		}
		predefinedPoint := history.PrunePoints[bc.genesisBlock.Hash()]
		if predefinedPoint == nil {
			log.Error("Chain history pruning is not supported for this network", "genesis", bc.genesisBlock.Hash())
			return errors.New("history pruning requested for unknown network")
		} else if freezerTail > 0 && freezerTail != predefinedPoint.BlockNumber {
			log.Error("Chain history database is pruned to unknown block", "tail", freezerTail)
			return errors.New("unexpected database tail")
		}
		bc.historyPrunePoint.Store(predefinedPoint)
		return nil

	default:
		return fmt.Errorf("invalid history mode: %d", bc.cfg.ChainHistoryMode)
	}
}



// SetHead rewinds the local chain to a new head. Depending on whether the node
// was snap synced or full synced and in which state, the method will try to
// delete minimal data from disk whilst retaining chain consistency.
// TODO: 현재의 최신 블록을 사용자가 지정한 과거의 특정 블록 번호로 강제로 되돌리는 함수  (ex ) hardfork
func (bc *BlockChain) SetHead(head uint64) error {
	if _, err := bc.setHeadBeyondRoot(head, 0, common.Hash{}, false); err != nil {
		return err
	}
	// Send chain head event to update the transaction pool
	header := bc.CurrentBlock()
	if block := bc.GetBlock(header.Hash(), header.Number.Uint64()); block == nil {
		// In a pruned node the genesis block will not exist in the freezer.
		// It should not happen that we set head to any other pruned block.
		if header.Number.Uint64() > 0 {
			// This should never happen. In practice, previously currentBlock
			// contained the entire block whereas now only a "marker", so there
			// is an ever so slight chance for a race we should handle.
			log.Error("Current block not found in database", "block", header.Number, "hash", header.Hash())
			return fmt.Errorf("current block missing: #%d [%x..]", header.Number, header.Hash().Bytes()[:4])
		}
	}
	bc.chainHeadFeed.Send(ChainHeadEvent{Header: header})	// 상태변경을 다른 모듈들에게 알림
	return nil
}





// SetHeadWithTimestamp rewinds the local chain to a new head that has at max
// the given timestamp. Depending on whether the node was snap synced or full
// synced and in which state, the method will try to delete minimal data from
// disk whilst retaining chain consistency.
// TODO: 지정된 타임스탬프의 블록 혹은 가장 그시간 이전에 최신 블록으로 체인을 강제로 되돌리는 함수
func (bc *BlockChain) SetHeadWithTimestamp(timestamp uint64) error {
	if _, err := bc.setHeadBeyondRoot(0, timestamp, common.Hash{}, false); err != nil {
		return err
	}
	// Send chain head event to update the transaction pool
	header := bc.CurrentBlock()
	if block := bc.GetBlock(header.Hash(), header.Number.Uint64()); block == nil {
		// In a pruned node the genesis block will not exist in the freezer.
		// It should not happen that we set head to any other pruned block.
		if header.Number.Uint64() > 0 {
			// This should never happen. In practice, previously currentBlock
			// contained the entire block whereas now only a "marker", so there
			// is an ever so slight chance for a race we should handle.
			log.Error("Current block not found in database", "block", header.Number, "hash", header.Hash())
			return fmt.Errorf("current block missing: #%d [%x..]", header.Number, header.Hash().Bytes()[:4])
		}
	}
	bc.chainHeadFeed.Send(ChainHeadEvent{Header: header})
	return nil
}




// SetFinalized sets the finalized block.
// TODO: 최종확정된 블록을 설정하고 기록하는 함수 (POS)    Blockchain.currentFinalBlocK 갱신 + 블록해시 DB에 저장
func (bc *BlockChain) SetFinalized(header *types.Header) {
	bc.currentFinalBlock.Store(header)	// blockchain 객체의 currentFinalBlock을 갱신
	// block header가 유효하다면 확정블록의 해시를 DB에 저장, 유효하지않다면 윙에서 갱신한 것도 비우고 종료
	if header != nil {
		rawdb.WriteFinalizedBlockHash(bc.db, header.Hash())
		headFinalizedBlockGauge.Update(int64(header.Number.Uint64()))
	} else {
		rawdb.WriteFinalizedBlockHash(bc.db, common.Hash{})
		headFinalizedBlockGauge.Update(0)
	}
}

// SetSafe sets the safe block.
// TODO: 세이프 블록을 설정하고 기록하는 함수 (POS)	 Blockchain.currentSafeBlock
func (bc *BlockChain) SetSafe(header *types.Header) {
	bc.currentSafeBlock.Store(header)
	if header != nil {
		headSafeBlockGauge.Update(int64(header.Number.Uint64()))
	} else {
		headSafeBlockGauge.Update(0)
	}
}



// rewindHashHead implements the logic of rewindHead in the context of hash scheme.
// TODO: DB에서 불일치가 발생시, 블록체인을 되감아서 유효상태인 가장 최신 블록을 찾고 그것을 최신블록으로 갱신하는 함수
func (bc *BlockChain) rewindHashHead(head *types.Header, root common.Hash) (*types.Header, uint64) {
	var (
		limit      uint64                             // 어디까지 되감을지 한계점
		beyondRoot = root == common.Hash{}            // Flag whether we're beyond the requested root (no root, always true)
		pivot      = rawdb.ReadLastPivotNumber(bc.db) // 스냅동기화 기준점
		rootNumber uint64                             // Associated block number of requested root

		start  = time.Now() // Timestamp the rewinding is restarted
		logged = time.Now() // Timestamp last progress log was printed
	)
	// The oldest block to be searched is determined by the pivot block or a constant
	// searching threshold. The rationale behind this is as follows:
	//
	// - Snap sync is selected if the pivot block is available. The earliest available
	//   state is the pivot block itself, so there is no sense in going further back.
	//
	// - Full sync is selected if the pivot block does not exist. The hash database
	//   periodically flushes the state to disk, and the used searching threshold is
	//   considered sufficient to find a persistent state, even for the testnet. It
	//   might be not enough for a chain that is nearly empty. In the worst case,
	//   the entire chain is reset to genesis, and snap sync is re-enabled on top,
	//   which is still acceptable.

	// 스냅동기화기록이 있다면 거기까지만 한계설정 (어차피 그 이후는 없기에), 풀동기화의 경우는 약9만블록
	if pivot != nil {
		limit = *pivot
	} else if head.Number.Uint64() > params.FullImmutabilityThreshold {
		limit = head.Number.Uint64() - params.FullImmutabilityThreshold
	}


	for {
		logger := log.Trace
		if time.Since(logged) > time.Second*8 {
			logged = time.Now()
			logger = log.Info
		}
		logger("Block state missing, rewinding further", "number", head.Number, "hash", head.Hash(), "elapsed", common.PrettyDuration(time.Since(start)))

		// If a root threshold was requested but not yet crossed, check
		if !beyondRoot && head.Root == root {
			beyondRoot, rootNumber = true, head.Number.Uint64()
		}
		// If search limit is reached, return the genesis block as the
		// new chain head.
		// TODO: 만약 한계지점까지도 유효한 상태를 찾지못하면 genesis block으로 리셋
		if head.Number.Uint64() < limit {
			log.Info("Rewinding limit reached, resetting to genesis", "number", head.Number, "hash", head.Hash(), "limit", limit)
			return bc.genesisBlock.Header(), rootNumber
		}

		// If the associated state is not reachable, continue searching
		// backwards until an available state is found.
		// TODO: 유효성 상태 존재 여부 검사
		if !bc.HasState(head.Root) {
			// If the chain is gapped in the middle, return the genesis
			// block as the new chain head.
			parent := bc.GetHeader(head.ParentHash, head.Number.Uint64()-1)
			if parent == nil {
				log.Error("Missing block in the middle, resetting to genesis", "number", head.Number.Uint64()-1, "hash", head.ParentHash)
				return bc.genesisBlock.Header(), rootNumber
			}
			head = parent

			// If the genesis block is reached, stop searching.
			if head.Number.Uint64() == 0 {
				log.Info("Genesis block reached", "number", head.Number, "hash", head.Hash())
				return head, rootNumber
			}
			continue // keep rewinding
		}
		// Once the available state is found, ensure that the requested root
		// has already been crossed. If not, continue rewinding.
		// TODO: 유효한 상태 발견시 이것을 최신블록으로 설정하고 리턴 후 종료
		if beyondRoot || head.Number.Uint64() == 0 {
			log.Info("Rewound to block with state", "number", head.Number, "hash", head.Hash())
			return head, rootNumber
		}
		log.Debug("Skipping block with threshold state", "number", head.Number, "hash", head.Hash(), "root", head.Root)
		head = bc.GetHeader(head.ParentHash, head.Number.Uint64()-1) // Keep rewinding
	}
}



// rewindPathHead implements the logic of rewindHead in the context of path scheme.
// TODO: PathScheme DB 구조에서 블록체인을 뒤로 감아 유효한상태를 갖는 가장 최신 블록을 찾는 자동복구 함수
func (bc *BlockChain) rewindPathHead(head *types.Header, root common.Hash) (*types.Header, uint64) {
	var (
		pivot      = rawdb.ReadLastPivotNumber(bc.db) // 스냅 동기화 기준점
		rootNumber uint64                             // 찾고자하는 특정 상태 루트

		// BeyondRoot represents whether the requested root is already
		// crossed. The flag value is set to true if the root is empty.
		beyondRoot = root == common.Hash{}			// 루트를 지났는지 여부를 체크하는 변수

		// noState represents if the target state requested for search
		// is unavailable and impossible to be recovered.
		noState = !bc.HasState(root) && !bc.stateRecoverable(root)

		start  = time.Now() // Timestamp the rewinding is restarted
		logged = time.Now() // Timestamp last progress log was printed
	)
	// Rewind the head block tag until an available state is found.
	for {
		logger := log.Trace
		if time.Since(logged) > time.Second*8 {
			logged = time.Now()
			logger = log.Info
		}
		logger("Block state missing, rewinding further", "number", head.Number, "hash", head.Hash(), "elapsed", common.PrettyDuration(time.Since(start)))

		// If a root threshold was requested but not yet crossed, check
		if !beyondRoot && head.Root == root {
			beyondRoot, rootNumber = true, head.Number.Uint64()
		}
		// If the root threshold hasn't been crossed but the available
		// state is reached, quickly determine if the target state is
		// possible to be reached or not.
		if !beyondRoot && noState && bc.HasState(head.Root) {
			beyondRoot = true
			log.Info("Disable the search for unattainable state", "root", root)
		}
		// Check if the associated state is available or recoverable if
		// the requested root has already been crossed.
		if beyondRoot && (bc.HasState(head.Root) || bc.stateRecoverable(head.Root)) {
			break
		}
		// If pivot block is reached, return the genesis block as the
		// new chain head. Theoretically there must be a persistent
		// state before or at the pivot block, prevent endless rewinding
		// towards the genesis just in case.
		if pivot != nil && *pivot >= head.Number.Uint64() {
			log.Info("Pivot block reached, resetting to genesis", "number", head.Number, "hash", head.Hash())
			return bc.genesisBlock.Header(), rootNumber
		}
		// If the chain is gapped in the middle, return the genesis
		// block as the new chain head
		parent := bc.GetHeader(head.ParentHash, head.Number.Uint64()-1) // Keep rewinding
		if parent == nil {
			log.Error("Missing block in the middle, resetting to genesis", "number", head.Number.Uint64()-1, "hash", head.ParentHash)
			return bc.genesisBlock.Header(), rootNumber
		}
		head = parent

		// If the genesis block is reached, stop searching.
		if head.Number.Uint64() == 0 {
			log.Info("Genesis block reached", "number", head.Number, "hash", head.Hash())
			return head, rootNumber
		}
	}
	// Recover if the target state if it's not available yet.
	if !bc.HasState(head.Root) {
		if err := bc.triedb.Recover(head.Root); err != nil {
			log.Crit("Failed to rollback state", "err", err)
		}
	}
	log.Info("Rewound to block with state", "number", head.Number, "hash", head.Hash())
	return head, rootNumber
}



// rewindHead searches the available states in the database and returns the associated
// block as the new head block.
//
// If the given root is not empty, then the rewind should attempt to pass the specified
// state root and return the associated block number as well. If the root, typically
// representing the state corresponding to snapshot disk layer, is deemed impassable,
// then block number zero is returned, indicating that snapshot recovery is disabled
// and the whole snapshot should be auto-generated in case of head mismatch.

// TODO:  blockchain DB scheme에 따라 적절한 rewind 함수를 호출해주는 Dispatcher
// TODO:  즉, 위의 rewindHashHead		rewindPathHead 함수 중 어떤 걸 호출할지 결정한다.
func (bc *BlockChain) rewindHead(head *types.Header, root common.Hash) (*types.Header, uint64) {
	if bc.triedb.Scheme() == rawdb.PathScheme {
		return bc.rewindPathHead(head, root)
	}
	return bc.rewindHashHead(head, root)
}




// setHeadBeyondRoot rewinds the local chain to a new head with the extra condition
// that the rewind must pass the specified state root. This method is meant to be
// used when rewinding with snapshots enabled to ensure that we go back further than
// persistent disk layer. Depending on whether the node was snap synced or full, and
// in which state, the method will try to delete minimal data from disk whilst
// retaining chain consistency.
//
// The method also works in timestamp mode if `head == 0` but `time != 0`. In that
// case blocks are rolled back until the new head becomes older or equal to the
// requested time. If both `head` and `time` is 0, the chain is rewound to genesis.
//
// The method returns the block number where the requested root cap was found.

// TODO: 체인의 최신블록을 과거로 되돌리는 것을 넘어서, 모든 데이터 정합성을 맞추고 불필요한 데이터를 삭제하며 모든 상태를 업데이트하는 함수
func (bc *BlockChain) setHeadBeyondRoot(head uint64, time uint64, root common.Hash, repair bool) (uint64, error) {
	if !bc.chainmu.TryLock() {
		return 0, errChainStopped
	}
	defer bc.chainmu.Unlock()

	var (
		// Track the block number of the requested root hash
		rootNumber uint64 // (no root == always 0)

		// Retrieve the last pivot block to short circuit rollbacks beyond it
		// and the current freezer limit to start nuking it's underflown.
		pivot = rawdb.ReadLastPivotNumber(bc.db)
	)
	updateFn := func(db ethdb.KeyValueWriter, header *types.Header) (*types.Header, bool) {
		// Rewind the blockchain, ensuring we don't end up with a stateless head
		// block. Note, depth equality is permitted to allow using SetHead as a
		// chain reparation mechanism without deleting any data!
		if currentBlock := bc.CurrentBlock(); currentBlock != nil && header.Number.Uint64() <= currentBlock.Number.Uint64() {
			var newHeadBlock *types.Header
			newHeadBlock, rootNumber = bc.rewindHead(header, root)
			rawdb.WriteHeadBlockHash(db, newHeadBlock.Hash())

			// Degrade the chain markers if they are explicitly reverted.
			// In theory we should update all in-memory markers in the
			// last step, however the direction of SetHead is from high
			// to low, so it's safe to update in-memory markers directly.
			bc.currentBlock.Store(newHeadBlock)
			headBlockGauge.Update(int64(newHeadBlock.Number.Uint64()))

			// The head state is missing, which is only possible in the path-based
			// scheme. This situation occurs when the chain head is rewound below
			// the pivot point. In this scenario, there is no possible recovery
			// approach except for rerunning a snap sync. Do nothing here until the
			// state syncer picks it up.
			if !bc.HasState(newHeadBlock.Root) {
				if newHeadBlock.Number.Uint64() != 0 {
					log.Crit("Chain is stateless at a non-genesis block")
				}
				log.Info("Chain is stateless, wait state sync", "number", newHeadBlock.Number, "hash", newHeadBlock.Hash())
			}
		}
		// Rewind the snap block in a simpleton way to the target head
		if currentSnapBlock := bc.CurrentSnapBlock(); currentSnapBlock != nil && header.Number.Uint64() < currentSnapBlock.Number.Uint64() {
			newHeadSnapBlock := bc.GetBlock(header.Hash(), header.Number.Uint64())
			// If either blocks reached nil, reset to the genesis state
			if newHeadSnapBlock == nil {
				newHeadSnapBlock = bc.genesisBlock
			}
			rawdb.WriteHeadFastBlockHash(db, newHeadSnapBlock.Hash())

			// Degrade the chain markers if they are explicitly reverted.
			// In theory we should update all in-memory markers in the
			// last step, however the direction of SetHead is from high
			// to low, so it's safe the update in-memory markers directly.
			bc.currentSnapBlock.Store(newHeadSnapBlock.Header())
			headFastBlockGauge.Update(int64(newHeadSnapBlock.NumberU64()))
		}
		var (
			headHeader = bc.CurrentBlock()
			headNumber = headHeader.Number.Uint64()
		)
		// If setHead underflown the freezer threshold and the block processing
		// intent afterwards is full block importing, delete the chain segment
		// between the stateful-block and the sethead target.
		var wipe bool
		frozen, _ := bc.db.Ancients()
		if headNumber+1 < frozen {
			wipe = pivot == nil || headNumber >= *pivot
		}
		return headHeader, wipe // Only force wipe if full synced
	}
	// Rewind the header chain, deleting all block bodies until then
	delFn := func(db ethdb.KeyValueWriter, hash common.Hash, num uint64) {
		// Ignore the error here since light client won't hit this path
		frozen, _ := bc.db.Ancients()
		if num+1 <= frozen {
			// The chain segment, such as the block header, canonical hash,
			// body, and receipt, will be removed from the ancient store
			// in one go.
			//
			// The hash-to-number mapping in the key-value store will be
			// removed by the hc.SetHead function.
		} else {
			// Remove the associated body and receipts from the key-value store.
			// The header, hash-to-number mapping, and canonical hash will be
			// removed by the hc.SetHead function.
			rawdb.DeleteBody(db, hash, num)
			rawdb.DeleteReceipts(db, hash, num)
		}
		// Todo(rjl493456442) txlookup, log index, etc
	}
	// If SetHead was only called as a chain reparation method, try to skip
	// touching the header chain altogether, unless the freezer is broken
	if repair {
		if target, force := updateFn(bc.db, bc.CurrentBlock()); force {
			bc.hc.SetHead(target.Number.Uint64(), nil, delFn)
		}
	} else {
		// Rewind the chain to the requested head and keep going backwards until a
		// block with a state is found or snap sync pivot is passed
		if time > 0 {
			log.Warn("Rewinding blockchain to timestamp", "target", time)
			bc.hc.SetHeadWithTimestamp(time, updateFn, delFn)
		} else {
			log.Warn("Rewinding blockchain to block", "target", head)
			bc.hc.SetHead(head, updateFn, delFn)
		}
	}
	// Clear out any stale content from the caches
	bc.bodyCache.Purge()
	bc.bodyRLPCache.Purge()
	bc.receiptsCache.Purge()
	bc.blockCache.Purge()
	bc.txLookupCache.Purge()

	// Clear safe block, finalized block if needed
	if safe := bc.CurrentSafeBlock(); safe != nil && head < safe.Number.Uint64() {
		log.Warn("SetHead invalidated safe block")
		bc.SetSafe(nil)
	}
	if finalized := bc.CurrentFinalBlock(); finalized != nil && head < finalized.Number.Uint64() {
		log.Error("SetHead invalidated finalized block")
		bc.SetFinalized(nil)
	}
	return rootNumber, bc.loadLastState()
}



// SnapSyncCommitHead sets the current head block to the one defined by the hash
// irrelevant what the chain contents were prior.
// TODO: snapshot sync가 완료된 후, 다운로드한 최신 블록을 현재 체인의 최신블록으로 지정하는 함수
func (bc *BlockChain) SnapSyncCommitHead(hash common.Hash) error {
	// Make sure that both the block as well at its state trie exists
	block := bc.GetBlockByHash(hash)
	if block == nil {
		return fmt.Errorf("non existent block [%x..]", hash[:4])
	}
	// Reset the trie database with the fresh snap synced state.
	root := block.Root()
	if bc.triedb.Scheme() == rawdb.PathScheme {
		if err := bc.triedb.Enable(root); err != nil {
			return err
		}
	}
	if !bc.HasState(root) {
		return fmt.Errorf("non existent state [%x..]", root[:4])
	}
	// If all checks out, manually set the head block.
	if !bc.chainmu.TryLock() {
		return errChainStopped
	}
	bc.currentBlock.Store(block.Header())
	headBlockGauge.Update(int64(block.NumberU64()))
	bc.chainmu.Unlock()

	// Destroy any existing state snapshot and regenerate it in the background,
	// also resuming the normal maintenance of any previously paused snapshot.
	if bc.snaps != nil {
		bc.snaps.Rebuild(root)
	}
	log.Info("Committed new head block", "number", block.Number(), "hash", hash)
	return nil
}



// Reset purges the entire blockchain, restoring it to its genesis state.
// TODO: 모든 데이터를 삭제하고, genesis block 상태로 초기화하는 함수
func (bc *BlockChain) Reset() error {
	return bc.ResetWithGenesisBlock(bc.genesisBlock)	// call ResetWithGenesisBlock
}



// ResetWithGenesisBlock purges the entire blockchain, restoring it to the
// specified genesis state.
// TODO: genesis block 상태로 돌리는 로직
func (bc *BlockChain) ResetWithGenesisBlock(genesis *types.Block) error {
	// Dump the entire block chain and purge the caches
	if err := bc.SetHead(0); err != nil {
		return err
	}
	if !bc.chainmu.TryLock() {
		return errChainStopped
	}
	defer bc.chainmu.Unlock()

	// Prepare the genesis block and reinitialise the chain
	batch := bc.db.NewBatch()
	rawdb.WriteBlock(batch, genesis)
	if err := batch.Write(); err != nil {
		log.Crit("Failed to write genesis block", "err", err)
	}
	bc.writeHeadBlock(genesis)

	// Last update all in-memory chain markers
	bc.genesisBlock = genesis
	bc.currentBlock.Store(bc.genesisBlock.Header())
	headBlockGauge.Update(int64(bc.genesisBlock.NumberU64()))
	bc.hc.SetGenesis(bc.genesisBlock.Header())
	bc.hc.SetCurrentHeader(bc.genesisBlock.Header())
	bc.currentSnapBlock.Store(bc.genesisBlock.Header())
	headFastBlockGauge.Update(int64(bc.genesisBlock.NumberU64()))

	// Reset history pruning status.
	return bc.initializeHistoryPruning(0)
}



// Export writes the active chain to the given writer.
// TODO: 전체 블록체인 데이터를 특정 출력 스트림으로 내보내는 함수
func (bc *BlockChain) Export(w io.Writer) error {
	return bc.ExportN(w, uint64(0), bc.CurrentBlock().Number.Uint64())
}



// ExportN writes a subset of the active chain to the given writer.
// TODO: 블록체인의 지정된 범위만큼의 데이터를 특정 스트림에 내보내는 함수
func (bc *BlockChain) ExportN(w io.Writer, first uint64, last uint64) error {
	if first > last {
		return fmt.Errorf("export failed: first (%d) is greater than last (%d)", first, last)
	}
	log.Info("Exporting batch of blocks", "count", last-first+1)

	var (
		parentHash common.Hash
		start      = time.Now()
		reported   = time.Now()
	)
	for nr := first; nr <= last; nr++ {
		block := bc.GetBlockByNumber(nr)
		if block == nil {
			return fmt.Errorf("export failed on #%d: not found", nr)
		}
		if nr > first && block.ParentHash() != parentHash {
			return errors.New("export failed: chain reorg during export")
		}
		parentHash = block.Hash()
		if err := block.EncodeRLP(w); err != nil {
			return err
		}
		if time.Since(reported) >= statsReportLimit {
			log.Info("Exporting blocks", "exported", block.NumberU64()-first, "elapsed", common.PrettyDuration(time.Since(start)))
			reported = time.Now()
		}
	}
	return nil
}



// writeHeadBlock injects a new head block into the current block chain. This method
// assumes that the block is indeed a true head. It will also reset the head
// header and the head snap sync block to this very same block if they are older
// or if they are on a different side chain.
//
// Note, this function assumes that the `mu` mutex is held!
// TODO: 새롭게 검증된 블록을 공식적인 최신블록 (head)로 저장하는 함수
func (bc *BlockChain) writeHeadBlock(block *types.Block) {
	// Add the block to the canonical chain number scheme and mark as the head
	batch := bc.db.NewBatch()
	rawdb.WriteHeadHeaderHash(batch, block.Hash())
	rawdb.WriteHeadFastBlockHash(batch, block.Hash())
	rawdb.WriteCanonicalHash(batch, block.Hash(), block.NumberU64())
	rawdb.WriteTxLookupEntriesByBlock(batch, block)
	rawdb.WriteHeadBlockHash(batch, block.Hash())

	// Flush the whole batch into the disk, exit the node if failed
	if err := batch.Write(); err != nil {
		log.Crit("Failed to update chain indexes and markers", "err", err)
	}
	// Update all in-memory chain markers in the last step
	bc.hc.SetCurrentHeader(block.Header())

	bc.currentSnapBlock.Store(block.Header())
	headFastBlockGauge.Update(int64(block.NumberU64()))

	bc.currentBlock.Store(block.Header())
	headBlockGauge.Update(int64(block.NumberU64()))
}




// stopWithoutSaving stops the blockchain service. If any imports are currently in progress
// it will abort them using the procInterrupt. This method stops all running
// goroutines, but does not do all the post-stop work of persisting data.
// OBS! It is generally recommended to use the Stop method!
// This method has been exposed to allow tests to stop the blockchain while simulating
// a crash.
// TODO: 정상적인 종료절차를 건너뛰고, 강제 중지시키는 함수
func (bc *BlockChain) stopWithoutSaving() {
	if !bc.stopping.CompareAndSwap(false, true) {
		return
	}
	// Signal shutdown tx indexer.
	if bc.txIndexer != nil {
		bc.txIndexer.close()
	}
	// Unsubscribe all subscriptions registered from blockchain.
	bc.scope.Close()

	// Signal shutdown to all goroutines.
	bc.InterruptInsert(true)

	// Now wait for all chain modifications to end and persistent goroutines to exit.
	//
	// Note: Close waits for the mutex to become available, i.e. any running chain
	// modification will have exited when Close returns. Since we also called StopInsert,
	// the mutex should become available quickly. It cannot be taken again after Close has
	// returned.
	bc.chainmu.Close()
}



// Stop stops the blockchain service. If any imports are currently in progress
// it will abort them using the procInterrupt.
// TODO: Blockchain service를 정상적이게 종료시키는 함수 ( + 디스크에 데이터 저장 + 자원 해제 후처리작업까지)
func (bc *BlockChain) Stop() {
	bc.stopWithoutSaving()	// 모든 작업 중지

	// Ensure that the entirety of the state snapshot is journaled to disk.
	// 최신 상태를 디스크에 저장 + 자원 해제 및 정리
	var snapBase common.Hash
	if bc.snaps != nil {
		var err error
		if snapBase, err = bc.snaps.Journal(bc.CurrentBlock().Root); err != nil {
			log.Error("Failed to journal state snapshot", "err", err)
		}
		bc.snaps.Release()
	}
	if bc.triedb.Scheme() == rawdb.PathScheme {
		// Ensure that the in-memory trie nodes are journaled to disk properly.
		if err := bc.triedb.Journal(bc.CurrentBlock().Root); err != nil {
			log.Info("Failed to journal in-memory trie nodes", "err", err)
		}
	} else {
		// Ensure the state of a recent block is also stored to disk before exiting.
		// We're writing three different states to catch different restart scenarios:
		//  - HEAD:     So we don't need to reprocess any blocks in the general case
		//  - HEAD-1:   So we don't do large reorgs if our HEAD becomes an uncle
		//  - HEAD-127: So we have a hard limit on the number of blocks reexecuted
		if !bc.cfg.ArchiveMode {
			triedb := bc.triedb

			for _, offset := range []uint64{0, 1, state.TriesInMemory - 1} {
				if number := bc.CurrentBlock().Number.Uint64(); number > offset {
					recent := bc.GetBlockByNumber(number - offset)

					log.Info("Writing cached state to disk", "block", recent.Number(), "hash", recent.Hash(), "root", recent.Root())
					if err := triedb.Commit(recent.Root(), true); err != nil {
						log.Error("Failed to commit recent state trie", "err", err)
					}
				}
			}
			if snapBase != (common.Hash{}) {
				log.Info("Writing snapshot state to disk", "root", snapBase)
				if err := triedb.Commit(snapBase, true); err != nil {
					log.Error("Failed to commit recent state trie", "err", err)
				}
			}
			for !bc.triegc.Empty() {
				triedb.Dereference(bc.triegc.PopItem())
			}
			if _, nodes, _ := triedb.Size(); nodes != 0 { // all memory is contained within the nodes return for hashdb
				log.Error("Dangling trie nodes after full cleanup")
			}
		}
	}
	// Allow tracers to clean-up and release resources.
	if bc.logger != nil && bc.logger.OnClose != nil {
		bc.logger.OnClose()
	}
	// Close the trie database, release all the held resources as the last step.
	if err := bc.triedb.Close(); err != nil {
		log.Error("Failed to close trie database", "err", err)
	}
	log.Info("Blockchain stopped")
}



// InterruptInsert interrupts all insertion methods, causing them to return
// errInsertionInterrupted as soon as possible, or resume the chain insertion
// if required.
// TODO: 블록 삽입 작업을 즉시 중단 or 재개하라고 신호를 보내는 스위치 함수
func (bc *BlockChain) InterruptInsert(on bool) {
	if on {									// on 인 경우 삽입을 즉시 중단
		bc.procInterrupt.Store(true)
	} else {								// off 인 경우 삽입으 재개
		bc.procInterrupt.Store(false)
	}
}


// insertStopped returns true after StopInsert has been called.
// TODO: 블록삽입 작업이 현재 중단된 상태인지 확인하는 함수  	ture or false
func (bc *BlockChain) insertStopped() bool {
	return bc.procInterrupt.Load()
}



// WriteStatus status of write
type WriteStatus byte

const (
	NonStatTy WriteStatus = iota	// 상태없음을 의미
	CanonStatTy						// 1 	정식상태를 의미
	SideStatTy						// 2	사이드상태를 의미	 해당블록이 유효하긴하나, 일시적 포크로 인한 사이드체인에 속해있음을 의미한다.
)

// InsertReceiptChain inserts a batch of blocks along with their receipts into
// the database. Unlike InsertChain, this function does not verify the state root
// in the blocks. It is used exclusively for snap sync. All the inserted blocks
// will be regarded as canonical, chain reorg is not supported.
//
// The optional ancientLimit can also be specified and chain segment before that
// will be directly stored in the ancient, getting rid of the chain migration.

// TODO: Snap sync 전용으로 사용되는 블록 삽입 함수
// TODO: 프로토콜을 통해 받은 블록과 영수중 데이터 묶음을 ancient와 최신 데이터로 나누어 freezer / LevelDB에 나누어 저장하는 역할을 한다.
func (bc *BlockChain) InsertReceiptChain(blockChain types.Blocks, receiptChain []rlp.RawValue, ancientLimit uint64) (int, error) {
	// Verify the supplied headers before insertion without lock
	var headers []*types.Header
	for _, block := range blockChain {
		headers = append(headers, block.Header())
		// Here we also validate that blob transactions in the block do not
		// contain a sidecar. While the sidecar does not affect the block hash
		// or tx hash, sending blobs within a block is not allowed.
		for txIndex, tx := range block.Transactions() {
			if tx.Type() == types.BlobTxType && tx.BlobTxSidecar() != nil {
				return 0, fmt.Errorf("block #%d contains unexpected blob sidecar in tx at index %d", block.NumberU64(), txIndex)
			}
		}
	}
	if n, err := bc.hc.ValidateHeaderChain(headers); err != nil {
		return n, err
	}
	// Hold the mutation lock
	if !bc.chainmu.TryLock() {
		return 0, errChainStopped
	}
	defer bc.chainmu.Unlock()

	var (
		stats = struct{ processed, ignored int32 }{}
		start = time.Now()
		size  = int64(0)
	)
	// updateHead updates the head header and head snap block flags.
	updateHead := func(header *types.Header) error {
		batch := bc.db.NewBatch()
		hash := header.Hash()
		rawdb.WriteHeadHeaderHash(batch, hash)
		rawdb.WriteHeadFastBlockHash(batch, hash)
		if err := batch.Write(); err != nil {
			return err
		}
		bc.hc.currentHeader.Store(header)
		bc.currentSnapBlock.Store(header)
		headHeaderGauge.Update(header.Number.Int64())
		headFastBlockGauge.Update(header.Number.Int64())
		return nil
	}
	// writeAncient writes blockchain and corresponding receipt chain into ancient store.
	//
	// this function only accepts canonical chain data. All side chain will be reverted
	// eventually.
	writeAncient := func(blockChain types.Blocks, receiptChain []rlp.RawValue) (int, error) {
		// Ensure genesis is in the ancient store
		if blockChain[0].NumberU64() == 1 {
			if frozen, _ := bc.db.Ancients(); frozen == 0 {
				writeSize, err := rawdb.WriteAncientBlocks(bc.db, []*types.Block{bc.genesisBlock}, []rlp.RawValue{rlp.EmptyList})
				if err != nil {
					log.Error("Error writing genesis to ancients", "err", err)
					return 0, err
				}
				size += writeSize
				log.Info("Wrote genesis to ancients")
			}
		}
		// Write all chain data to ancients.
		writeSize, err := rawdb.WriteAncientBlocks(bc.db, blockChain, receiptChain)
		if err != nil {
			log.Error("Error importing chain data to ancients", "err", err)
			return 0, err
		}
		size += writeSize

		// Sync the ancient store explicitly to ensure all data has been flushed to disk.
		if err := bc.db.SyncAncient(); err != nil {
			return 0, err
		}
		// Write hash to number mappings
		batch := bc.db.NewBatch()
		for _, block := range blockChain {
			rawdb.WriteHeaderNumber(batch, block.Hash(), block.NumberU64())
		}
		if err := batch.Write(); err != nil {
			return 0, err
		}
		// Update the current snap block because all block data is now present in DB.
		if err := updateHead(blockChain[len(blockChain)-1].Header()); err != nil {
			return 0, err
		}
		stats.processed += int32(len(blockChain))
		return 0, nil
	}

	// writeLive writes the blockchain and corresponding receipt chain to the active store.
	//
	// Notably, in different snap sync cycles, the supplied chain may partially reorganize
	// existing local chain segments (reorg around the chain tip). The reorganized part
	// will be included in the provided chain segment, and stale canonical markers will be
	// silently rewritten. Therefore, no explicit reorg logic is needed.
	writeLive := func(blockChain types.Blocks, receiptChain []rlp.RawValue) (int, error) {
		var (
			skipPresenceCheck = false
			batch             = bc.db.NewBatch()
		)
		for i, block := range blockChain {
			// Short circuit insertion if shutting down or processing failed
			if bc.insertStopped() {
				return 0, errInsertionInterrupted
			}
			if !skipPresenceCheck {
				// Ignore if the entire data is already known
				if bc.HasBlock(block.Hash(), block.NumberU64()) {
					stats.ignored++
					continue
				} else {
					// If block N is not present, neither are the later blocks.
					// This should be true, but if we are mistaken, the shortcut
					// here will only cause overwriting of some existing data
					skipPresenceCheck = true
				}
			}
			// Write all the data out into the database
			rawdb.WriteCanonicalHash(batch, block.Hash(), block.NumberU64())
			rawdb.WriteBlock(batch, block)
			rawdb.WriteRawReceipts(batch, block.Hash(), block.NumberU64(), receiptChain[i])

			// Write everything belongs to the blocks into the database. So that
			// we can ensure all components of body is completed(body, receipts)
			// except transaction indexes(will be created once sync is finished).
			if batch.ValueSize() >= ethdb.IdealBatchSize {
				if err := batch.Write(); err != nil {
					return 0, err
				}
				size += int64(batch.ValueSize())
				batch.Reset()
			}
			stats.processed++
		}
		// Write everything belongs to the blocks into the database. So that
		// we can ensure all components of body is completed(body, receipts,
		// tx indexes)
		if batch.ValueSize() > 0 {
			size += int64(batch.ValueSize())
			if err := batch.Write(); err != nil {
				return 0, err
			}
		}
		if err := updateHead(blockChain[len(blockChain)-1].Header()); err != nil {
			return 0, err
		}
		return 0, nil
	}

	// Split the supplied blocks into two groups, according to the
	// given ancient limit.
	index := sort.Search(len(blockChain), func(i int) bool {
		return blockChain[i].NumberU64() >= ancientLimit
	})
	if index > 0 {
		if n, err := writeAncient(blockChain[:index], receiptChain[:index]); err != nil {
			if err == errInsertionInterrupted {
				return 0, nil
			}
			return n, err
		}
	}
	if index != len(blockChain) {
		if n, err := writeLive(blockChain[index:], receiptChain[index:]); err != nil {
			if err == errInsertionInterrupted {
				return 0, nil
			}
			return n, err
		}
	}
	var (
		head    = blockChain[len(blockChain)-1]
		context = []interface{}{
			"count", stats.processed, "elapsed", common.PrettyDuration(time.Since(start)),
			"number", head.Number(), "hash", head.Hash(), "age", common.PrettyAge(time.Unix(int64(head.Time()), 0)),
			"size", common.StorageSize(size),
		}
	)
	if stats.ignored > 0 {
		context = append(context, []interface{}{"ignored", stats.ignored}...)
	}
	log.Debug("Imported new block receipts", context...)
	return 0, nil
}



// writeBlockWithoutState writes only the block and its metadata to the database,
// but does not write any state. This is used to construct competing side forks
// up to the point where they exceed the canonical total difficulty.
// TODO: state를 건드리지 않고, 블록데이터 (header + body) + metaData를 DB에 저장하는 함수
// TODO: 즉, 아직 체인에 정식적으로 포함될지 불확실한 블록을 "임시로 저장"하는 역할
// TODO: cosmos SDK와 다르게 ethereum의 경우는 임시DB를 사용하지 않고 메인에 저장후, 필요없다면 취소하는 방식이다.
func (bc *BlockChain) writeBlockWithoutState(block *types.Block) (err error) {
	if bc.insertStopped() {
		return errInsertionInterrupted
	}
	batch := bc.db.NewBatch()
	rawdb.WriteBlock(batch, block)	// TODO: 입력으로 받은 block 객체를 DB에 기록 (단, 상태를 변경하는 작업은 제외함)
	if err := batch.Write(); err != nil {
		log.Crit("Failed to write block into disk", "err", err)
	}
	return nil
}



// writeKnownBlock updates the head block flag with a known block
// and introduces chain reorg if necessary.
// TODO: 새로운 블록을 블록체인의 최신 블록 (head)로 지정하는 역할을 하는 함수 (fork 발생하였다면 재구성까지 자동 )
func (bc *BlockChain) writeKnownBlock(block *types.Block) error {
	current := bc.CurrentBlock()	// 현재 노드가 가장 최신이라고 생각하는 블록을 가져온다.

	// TODO: 새로 들어온 블록의 부모해시가 현재블록의 해시와 다른지 체크하고, 만약 같다면 그대로 확정 / 다르다면 fork가 발생한 것이므로 재구성 로직을 수행한다.
	if block.ParentHash() != current.Hash() {
		if err := bc.reorg(current, block.Header()); err != nil {
			return err
		}
	}
	bc.writeHeadBlock(block)	// 최신블록으로 확정
	return nil
}



// writeBlockWithState writes block, metadata and corresponding state data to the
// database.
// TODO: 완전히 검증된 블록과 그로 인한 상태 변경 내역 (RAM에 위치)을 모두 DB에 기록하는 함수
// TODO: 블록 처리의 마지막 단계로 모든 계산이 끝난 결과를 디스크에 영구적으로 저장하는 역할
func (bc *BlockChain) writeBlockWithState(block *types.Block, receipts []*types.Receipt, statedb *state.StateDB) error {
	
	// TODO: 이 블록의 부모블록이 DB에 존재하는지 확인
	if !bc.HasHeader(block.ParentHash(), block.NumberU64()-1) {
		return consensus.ErrUnknownAncestor
	}
	// Irrelevant of the canonical status, write the block itself to the database.
	//
	// Note all the components of block(hash->number map, header, body, receipts)
	// should be written atomically. BlockBatch is used for containing all components.
	// TODO: block, receipt, state를 batch에 추가하고 기록을 시작한다.
	blockBatch := bc.db.NewBatch()	
	rawdb.WriteBlock(blockBatch, block)
	rawdb.WriteReceipts(blockBatch, block.Hash(), block.NumberU64(), receipts)
	rawdb.WritePreimages(blockBatch, statedb.Preimages())
	if err := blockBatch.Write(); err != nil {
		log.Crit("Failed to write block into disk", "err", err)
	}

	// Commit all cached state changes into underlying memory database.
	// TODO: RAM에만 저장되어있던 모든 상태 변경값을 내부DB에 최종적으로 확정하고 stateRoot 해시값으을 리턴한다.
	root, err := statedb.Commit(block.NumberU64(), bc.chainConfig.IsEIP158(block.Number()), bc.chainConfig.IsCancun(block.Number(), block.Time()))
	if err != nil {
		return err
	}

	// If node is running in path mode, skip explicit gc operation
	// which is unnecessary in this mode.
	// HasScheme를 사용하는 구형의 노드에 대한 가비지 컬랙션과 디스크 플러시   * PathScheme의 경우 수행 x
	if bc.triedb.Scheme() == rawdb.PathScheme {
		return nil
	}
	// If we're running an archive node, always flush
	if bc.cfg.ArchiveMode {
		return bc.triedb.Commit(root, false)
	}
	// Full but not archive node, do proper garbage collection
	bc.triedb.Reference(root, common.Hash{}) // metadata reference to keep trie alive
	bc.triegc.Push(root, -int64(block.NumberU64()))

	// Flush limits are not considered for the first TriesInMemory blocks.
	current := block.NumberU64()
	if current <= state.TriesInMemory {
		return nil
	}
	// If we exceeded our memory allowance, flush matured singleton nodes to disk
	var (
		_, nodes, imgs = bc.triedb.Size() // all memory is contained within the nodes return for hashdb
		limit          = common.StorageSize(bc.cfg.TrieDirtyLimit) * 1024 * 1024
	)
	if nodes > limit || imgs > 4*1024*1024 {
		bc.triedb.Cap(limit - ethdb.IdealBatchSize)
	}
	// Find the next state trie we need to commit
	chosen := current - state.TriesInMemory
	flushInterval := time.Duration(bc.flushInterval.Load())
	// If we exceeded time allowance, flush an entire trie to disk
	if bc.gcproc > flushInterval {
		// If the header is missing (canonical chain behind), we're reorging a low
		// diff sidechain. Suspend committing until this operation is completed.
		header := bc.GetHeaderByNumber(chosen)
		if header == nil {
			log.Warn("Reorg in progress, trie commit postponed", "number", chosen)
		} else {
			// If we're exceeding limits but haven't reached a large enough memory gap,
			// warn the user that the system is becoming unstable.
			if chosen < bc.lastWrite+state.TriesInMemory && bc.gcproc >= 2*flushInterval {
				log.Info("State in memory for too long, committing", "time", bc.gcproc, "allowance", flushInterval, "optimum", float64(chosen-bc.lastWrite)/state.TriesInMemory)
			}
			// Flush an entire trie and restart the counters
			bc.triedb.Commit(header.Root, true)
			bc.lastWrite = chosen
			bc.gcproc = 0
		}
	}
	// Garbage collect anything below our required write retention
	for !bc.triegc.Empty() {
		root, number := bc.triegc.Pop()
		if uint64(-number) > chosen {
			bc.triegc.Push(root, number)
			break
		}
		bc.triedb.Dereference(root)
	}
	return nil
}


// writeBlockAndSetHead is the internal implementation of WriteBlockAndSetHead.
// This function expects the chain mutex to be held.
// TODO: 완전히 검증된 새로운 블록을 블록체인에 삽입하고, 이블록을 새로운 최신 블록 (head)로 확정하는 모든 과정을 총괄하는 함수
func (bc *BlockChain) writeBlockAndSetHead(block *types.Block, receipts []*types.Receipt, logs []*types.Log, state *state.StateDB, emitHeadEvent bool) (status WriteStatus, err error) {
	// TODO: 위의 함수 writeBlockWithState를 호출하여서 block,receipt, state 변경 내역을 모두 DB에 저장
	if err := bc.writeBlockWithState(block, receipts, state); err != nil {
		return NonStatTy, err
	}
	currentBlock := bc.CurrentBlock()

	// Reorganise the chain if the parent is not the head block
	// TODO: 체인재구성 검사 및 실행
	if block.ParentHash() != currentBlock.Hash() {
		if err := bc.reorg(currentBlock, block.Header()); err != nil {
			return NonStatTy, err
		}
	}

	// Set new head.
	// TODO: 최신블록 (Head) 업데이트
	bc.writeHeadBlock(block)


	// TODO: 이벤트 알림 전파 	새로운 블록 추가와 최신블록 변경등에 대한 이벤트를 발생시킨다.
	bc.chainFeed.Send(ChainEvent{Header: block.Header()})
	if len(logs) > 0 {
		bc.logsFeed.Send(logs)
	}
	// In theory, we should fire a ChainHeadEvent when we inject
	// a canonical block, but sometimes we can insert a batch of
	// canonical blocks. Avoid firing too many ChainHeadEvents,
	// we will fire an accumulated ChainHeadEvent and disable fire
	// event here.
	if emitHeadEvent {
		bc.chainHeadFeed.Send(ChainHeadEvent{Header: block.Header()})
	}
	return CanonStatTy, nil
}






// InsertChain attempts to insert the given batch of blocks in to the canonical
// chain or, otherwise, create a fork. If an error is returned it will return
// the index number of the failing block as well an error describing what went
// wrong. After insertion is done, all accumulated events will be fired.
// TODO: 외부에서 받은 블록 묶음을 검증한뒤, 정식체인에 삽입하거나 새로운 fork를 생성하는 함수
// TODO: 블록처리의 첫 관문 함수로, 입력데이터의 유효성 확인 + 안전한 삽입을 시작하는 단계
func (bc *BlockChain) InsertChain(chain types.Blocks) (int, error) {
	// Sanity check that we have something meaningful to import
	if len(chain) == 0 {
		return 0, nil
	}

	// Do a sanity check that the provided chain is actually ordered and linked.
	for i := 1; i < len(chain); i++ {
		block, prev := chain[i], chain[i-1]
		if block.NumberU64() != prev.NumberU64()+1 || block.ParentHash() != prev.Hash() {
			log.Error("Non contiguous block insert",
				"number", block.Number(),
				"hash", block.Hash(),
				"parent", block.ParentHash(),
				"prevnumber", prev.Number(),
				"prevhash", prev.Hash(),
			)
			return 0, fmt.Errorf("non contiguous insert: item %d is #%d [%x..], item %d is #%d [%x..] (parent [%x..])", i-1, prev.NumberU64(),
				prev.Hash().Bytes()[:4], i, block.NumberU64(), block.Hash().Bytes()[:4], block.ParentHash().Bytes()[:4])
		}
	}
	// Pre-checks passed, start the full block imports
	if !bc.chainmu.TryLock() {
		return 0, errChainStopped
	}
	defer bc.chainmu.Unlock()

	// TODO: call insertChain
	_, n, err := bc.insertChain(chain, true, false) // No witness collection for mass inserts (would get super large)
	return n, err
}



// insertChain is the internal implementation of InsertChain, which assumes that
// 1) chains are contiguous, and 2) The chain mutex is held.
//
// This method is split out so that import batches that require re-injecting
// historical blocks can do so without releasing the lock, which could lead to
// racey behaviour. If a sidechain import is in progress, and the historic state
// is imported, but then new canon-head is added before the actual sidechain
// completes, then the historic state could be pruned again

// TODO: 위의 InsertChain의 실제 핵심 구현체이다. 입력으로 받은 블록 묶음의 유효성을 검증하고, tx를 수행하며, DB에 저장하는 중심역할을 한다.
func (bc *BlockChain) insertChain(chain types.Blocks, setHead bool, makeWitness bool) (*stateless.Witness, int, error) {
	// If the chain is terminating, don't even bother starting up.
	if bc.insertStopped() {
		return nil, 0, nil
	}

	// blockProcCounter라는 카운터를사용하여서 현재 몇개의 블록이 삽입중인지 추적합니다.
	if atomic.AddInt32(&bc.blockProcCounter, 1) == 1 {
		bc.blockProcFeed.Send(true)
	}
	defer func() {
		if atomic.AddInt32(&bc.blockProcCounter, -1) == 0 {
			bc.blockProcFeed.Send(false)
		}
	}()

	// Start a parallel signature recovery (signer will fluke on fork transition, minimal perf loss)
	// TODO: 블록에 포함된 tx를 서명을 병렬적으로 복구하는 작업 
	SenderCacher().RecoverFromBlocks(types.MakeSigner(bc.chainConfig, chain[0].Number(), chain[0].Time()), chain)
	

	
	var (
		stats     = insertStats{startTime: mclock.Now()}
		lastCanon *types.Block
	)
	// Fire a single chain head event if we've progressed the chain
	defer func() {
		if lastCanon != nil && bc.CurrentBlock().Hash() == lastCanon.Hash() {
			bc.chainHeadFeed.Send(ChainHeadEvent{Header: lastCanon.Header()})
		}
	}()
	// Start the parallel header verifier
	headers := make([]*types.Header, len(chain))
	for i, block := range chain {
		headers[i] = block.Header()
	}

	// TODO: 블록 헤더의 유효성 검사도 병렬적으로 실행하여서 속도를 높인다.
	abort, results := bc.engine.VerifyHeaders(bc, headers)
	defer close(abort)

	// Peek the error for the first block to decide the directing import logic
	// TODO: 입력으로 받은 블록 묶음을 순회하면서 유효성 검사 결과를 제공하는 이터레이터를 생성
	it := newInsertIterator(chain, results, bc.validator)
	block, err := it.next()

	// Left-trim all the known blocks that don't need to build snapshot
	// 기존블럭 건너뛰기
	if bc.skipBlock(err, it) {
		// First block (and state) is known
		//   1. We did a roll-back, and should now do a re-import
		//   2. The block is stored as a sidechain, and is lying about it's stateroot, and passes a stateroot
		//      from the canonical chain, which has not been verified.
		// Skip all known blocks that are behind us.
		current := bc.CurrentBlock()
		for block != nil && bc.skipBlock(err, it) {
			if block.NumberU64() > current.Number.Uint64() || bc.GetCanonicalHash(block.NumberU64()) != block.Hash() {
				break
			}
			log.Debug("Ignoring already known block", "number", block.Number(), "hash", block.Hash())
			stats.ignored++

			block, err = it.next()
		}
		// The remaining blocks are still known blocks, the only scenario here is:
		// During the snap sync, the pivot point is already submitted but rollback
		// happens. Then node resets the head full block to a lower height via `rollback`
		// and leaves a few known blocks in the database.
		//
		// When node runs a snap sync again, it can re-import a batch of known blocks via
		// `insertChain` while a part of them have higher total difficulty than current
		// head full block(new pivot point).

		// TODO: 매안 블록처리 루프
		for block != nil && bc.skipBlock(err, it) {
			log.Debug("Writing previously known block", "number", block.Number(), "hash", block.Hash())
			if err := bc.writeKnownBlock(block); err != nil {
				return nil, it.index, err
			}
			lastCanon = block

			block, err = it.next()
		}
		// Falls through to the block import
	}
	switch {
	// First block is pruned
	case errors.Is(err, consensus.ErrPrunedAncestor):
		if setHead {
			// First block is pruned, insert as sidechain and reorg only if TD grows enough
			log.Debug("Pruned ancestor, inserting as sidechain", "number", block.Number(), "hash", block.Hash())
			return bc.insertSideChain(block, it, makeWitness)
		} else {
			// We're post-merge and the parent is pruned, try to recover the parent state
			log.Debug("Pruned ancestor", "number", block.Number(), "hash", block.Hash())
			_, err := bc.recoverAncestors(block, makeWitness)
			return nil, it.index, err
		}
	// Some other error(except ErrKnownBlock) occurred, abort.
	// ErrKnownBlock is allowed here since some known blocks
	// still need re-execution to generate snapshots that are missing
	case err != nil && !errors.Is(err, ErrKnownBlock):
		stats.ignored += len(it.chain)
		bc.reportBlock(block, nil, err)
		return nil, it.index, err
	}
	// Track the singleton witness from this chain insertion (if any)
	var witness *stateless.Witness

	for ; block != nil && err == nil || errors.Is(err, ErrKnownBlock); block, err = it.next() {
		// If the chain is terminating, stop processing blocks
		if bc.insertStopped() {
			log.Debug("Abort during block processing")
			break
		}
		// If the block is known (in the middle of the chain), it's a special case for
		// Clique blocks where they can share state among each other, so importing an
		// older block might complete the state of the subsequent one. In this case,
		// just skip the block (we already validated it once fully (and crashed), since
		// its header and body was already in the database). But if the corresponding
		// snapshot layer is missing, forcibly rerun the execution to build it.
		if bc.skipBlock(err, it) {
			logger := log.Debug
			if bc.chainConfig.Clique == nil {
				logger = log.Warn
			}
			logger("Inserted known block", "number", block.Number(), "hash", block.Hash(),
				"uncles", len(block.Uncles()), "txs", len(block.Transactions()), "gas", block.GasUsed(),
				"root", block.Root())

			// Special case. Commit the empty receipt slice if we meet the known
			// block in the middle. It can only happen in the clique chain. Whenever
			// we insert blocks via `insertSideChain`, we only commit `td`, `header`
			// and `body` if it's non-existent. Since we don't have receipts without
			// reexecution, so nothing to commit. But if the sidechain will be adopted
			// as the canonical chain eventually, it needs to be reexecuted for missing
			// state, but if it's this special case here(skip reexecution) we will lose
			// the empty receipt entry.
			if len(block.Transactions()) == 0 {
				rawdb.WriteReceipts(bc.db, block.Hash(), block.NumberU64(), nil)
			} else {
				log.Error("Please file an issue, skip known block execution without receipt",
					"hash", block.Hash(), "number", block.NumberU64())
			}
			if err := bc.writeKnownBlock(block); err != nil {
				return nil, it.index, err
			}
			stats.processed++
			if bc.logger != nil && bc.logger.OnSkippedBlock != nil {
				bc.logger.OnSkippedBlock(tracing.BlockEvent{
					Block:     block,
					Finalized: bc.CurrentFinalBlock(),
					Safe:      bc.CurrentSafeBlock(),
				})
			}
			// We can assume that logs are empty here, since the only way for consecutive
			// Clique blocks to have the same state is if there are no transactions.
			lastCanon = block
			continue
		}
		// Retrieve the parent block and it's state to execute on top
		parent := it.previous()
		if parent == nil {
			parent = bc.GetHeader(block.ParentHash(), block.NumberU64()-1)
		}
		// The traced section of block import.
		start := time.Now()
		res, err := bc.processBlock(parent.Root, block, setHead, makeWitness && len(chain) == 1)
		if err != nil {
			return nil, it.index, err
		}
		// Report the import stats before returning the various results
		stats.processed++
		stats.usedGas += res.usedGas
		witness = res.witness

		var snapDiffItems, snapBufItems common.StorageSize
		if bc.snaps != nil {
			snapDiffItems, snapBufItems = bc.snaps.Size()
		}
		trieDiffNodes, trieBufNodes, _ := bc.triedb.Size()
		stats.report(chain, it.index, snapDiffItems, snapBufItems, trieDiffNodes, trieBufNodes, setHead)

		// Print confirmation that a future fork is scheduled, but not yet active.
		bc.logForkReadiness(block)

		if !setHead {
			// After merge we expect few side chains. Simply count
			// all blocks the CL gives us for GC processing time
			bc.gcproc += res.procTime
			return witness, it.index, nil // Direct block insertion of a single block
		}
		switch res.status {
		case CanonStatTy:
			log.Debug("Inserted new block", "number", block.Number(), "hash", block.Hash(),
				"uncles", len(block.Uncles()), "txs", len(block.Transactions()), "gas", block.GasUsed(),
				"elapsed", common.PrettyDuration(time.Since(start)),
				"root", block.Root())

			lastCanon = block

			// Only count canonical blocks for GC processing time
			bc.gcproc += res.procTime

		case SideStatTy:
			log.Debug("Inserted forked block", "number", block.Number(), "hash", block.Hash(),
				"diff", block.Difficulty(), "elapsed", common.PrettyDuration(time.Since(start)),
				"txs", len(block.Transactions()), "gas", block.GasUsed(), "uncles", len(block.Uncles()),
				"root", block.Root())

		default:
			// This in theory is impossible, but lets be nice to our future selves and leave
			// a log, instead of trying to track down blocks imports that don't emit logs.
			log.Warn("Inserted block with unknown status", "number", block.Number(), "hash", block.Hash(),
				"diff", block.Difficulty(), "elapsed", common.PrettyDuration(time.Since(start)),
				"txs", len(block.Transactions()), "gas", block.GasUsed(), "uncles", len(block.Uncles()),
				"root", block.Root())
		}
	}

	stats.ignored += it.remaining()
	return witness, it.index, err
}







// blockProcessingResult is a summary of block processing
// used for updating the stats.
// TODO: 하나의 블록을 처리한 결과에 대한 요약정보를 담는 컨테이너		*노드의 성능 통계 업데이트 
type blockProcessingResult struct {
	usedGas  uint64					// 소모된 총 가스량
	procTime time.Duration			// 블록을 처리하는데 걸린 총 시간
	status   WriteStatus			// 처리된 블록의 최종상태 *정식체인인지 사이드체인인지
	witness  *stateless.Witness		// tx의 유효성을 검증하는데 사용할 수 있는 암호학적 증거에 대한 포인터
}



// processBlock executes and validates the given block. If there was no error
// it writes the block and associated state to database.
// TODO: 새로운 블록이 들어왔을 때, 해당블록의 모든 tx를 실행하고 결과를 검증한뒤, 최종적으로 DB에 기록하는 모든 과정을 총괄하는 핵심 함수
func (bc *BlockChain) processBlock(parentRoot common.Hash, block *types.Block, setHead bool, makeWitness bool) (_ *blockProcessingResult, blockEndErr error) {
	var (
		err       error
		startTime = time.Now()
		statedb   *state.StateDB
		interrupt atomic.Bool
	)
	defer interrupt.Store(true) // terminate the prefetch at the end

	// TODO: prefetching 여부 정하기 + stateDB 생성
	if bc.cfg.NoPrefetch {
		statedb, err = state.New(parentRoot, bc.statedb)
		if err != nil {
			return nil, err
		}
	} else {
		// If prefetching is enabled, run that against the current state to pre-cache
		// transactions and probabilistically some of the account/storage trie nodes.
		//
		// Note: the main processor and prefetcher share the same reader with a local
		// cache for mitigating the overhead of state access.
		prefetch, process, err := bc.statedb.ReadersWithCacheStats(parentRoot)
		if err != nil {
			return nil, err
		}
		throwaway, err := state.NewWithReader(parentRoot, bc.statedb, prefetch)
		if err != nil {
			return nil, err
		}
		statedb, err = state.NewWithReader(parentRoot, bc.statedb, process)
		if err != nil {
			return nil, err
		}
		// Upload the statistics of reader at the end
		defer func() {
			stats := prefetch.GetStats()
			accountCacheHitPrefetchMeter.Mark(stats.AccountHit)
			accountCacheMissPrefetchMeter.Mark(stats.AccountMiss)
			storageCacheHitPrefetchMeter.Mark(stats.StorageHit)
			storageCacheMissPrefetchMeter.Mark(stats.StorageMiss)
			stats = process.GetStats()
			accountCacheHitMeter.Mark(stats.AccountHit)
			accountCacheMissMeter.Mark(stats.AccountMiss)
			storageCacheHitMeter.Mark(stats.StorageHit)
			storageCacheMissMeter.Mark(stats.StorageMiss)
		}()

		go func(start time.Time, throwaway *state.StateDB, block *types.Block) {
			// Disable tracing for prefetcher executions.
			vmCfg := bc.cfg.VmConfig
			vmCfg.Tracer = nil
			bc.prefetcher.Prefetch(block, throwaway, vmCfg, &interrupt)

			blockPrefetchExecuteTimer.Update(time.Since(start))
			if interrupt.Load() {
				blockPrefetchInterruptMeter.Mark(1)
			}
		}(time.Now(), throwaway, block)
	}



	// If we are past Byzantium, enable prefetching to pull in trie node paths
	// while processing transactions. Before Byzantium the prefetcher is mostly
	// useless due to the intermediate root hashing after each transaction.
	// TODO: witness 생성 및 logger 설정
	var witness *stateless.Witness
	if bc.chainConfig.IsByzantium(block.Number()) {
		// Generate witnesses either if we're self-testing, or if it's the
		// only block being inserted. A bit crude, but witnesses are huge,
		// so we refuse to make an entire chain of them.
		if bc.cfg.VmConfig.StatelessSelfValidation || makeWitness {
			witness, err = stateless.NewWitness(block.Header(), bc)
			if err != nil {
				return nil, err
			}
		}
		statedb.StartPrefetcher("chain", witness)
		defer statedb.StopPrefetcher()
	}

	if bc.logger != nil && bc.logger.OnBlockStart != nil {
		bc.logger.OnBlockStart(tracing.BlockEvent{
			Block:     block,
			Finalized: bc.CurrentFinalBlock(),
			Safe:      bc.CurrentSafeBlock(),
		})
	}
	if bc.logger != nil && bc.logger.OnBlockEnd != nil {
		defer func() {
			bc.logger.OnBlockEnd(blockEndErr)
		}()
	}


	// Process block using the parent state as reference point
	// TODO: 블록 실행 및 상태검증 시작
	pstart := time.Now()
	// TODO: processor 객체는 이 블록에 모든 tx를 EVM에서 순서대로 실행한다. 이 과정에서 메모리에 있는 stateDB가 갱신된다.
	res, err := bc.processor.Process(block, statedb, bc.cfg.VmConfig)	
	if err != nil {
		bc.reportBlock(block, res, err)
		return nil, err
	}
	ptime := time.Since(pstart)

	vstart := time.Now()
	// TODO: tx실행이 끝난뒤, 최종검증 수행  (processor가 만든 stateRoot, receiptsRootrk 블록헤더와 정확히 일치하는지 비교한다.)
	if err := bc.validator.ValidateState(block, statedb, res, false); err != nil {
		bc.reportBlock(block, res, err)
		return nil, err
	}
	vtime := time.Since(vstart)

	// If witnesses was generated and stateless self-validation requested, do
	// that now. Self validation should *never* run in production, it's more of
	// a tight integration to enable running *all* consensus tests through the
	// witness builder/runner, which would otherwise be impossible due to the
	// various invalid chain states/behaviors being contained in those tests.
	xvstart := time.Now()
	if witness := statedb.Witness(); witness != nil && bc.cfg.VmConfig.StatelessSelfValidation {
		log.Warn("Running stateless self-validation", "block", block.Number(), "hash", block.Hash())

		// Remove critical computed fields from the block to force true recalculation
		context := block.Header()
		context.Root = common.Hash{}
		context.ReceiptHash = common.Hash{}

		task := types.NewBlockWithHeader(context).WithBody(*block.Body())

		// Run the stateless self-cross-validation
		crossStateRoot, crossReceiptRoot, err := ExecuteStateless(bc.chainConfig, bc.cfg.VmConfig, task, witness)
		if err != nil {
			return nil, fmt.Errorf("stateless self-validation failed: %v", err)
		}
		if crossStateRoot != block.Root() {
			return nil, fmt.Errorf("stateless self-validation root mismatch (cross: %x local: %x)", crossStateRoot, block.Root())
		}
		if crossReceiptRoot != block.ReceiptHash() {
			return nil, fmt.Errorf("stateless self-validation receipt root mismatch (cross: %x local: %x)", crossReceiptRoot, block.ReceiptHash())
		}
	}
	xvtime := time.Since(xvstart)
	proctime := time.Since(startTime) // processing + validation + cross validation

	// Update the metrics touched during block processing and validation
	accountReadTimer.Update(statedb.AccountReads) // Account reads are complete(in processing)
	storageReadTimer.Update(statedb.StorageReads) // Storage reads are complete(in processing)
	if statedb.AccountLoaded != 0 {
		accountReadSingleTimer.Update(statedb.AccountReads / time.Duration(statedb.AccountLoaded))
	}
	if statedb.StorageLoaded != 0 {
		storageReadSingleTimer.Update(statedb.StorageReads / time.Duration(statedb.StorageLoaded))
	}
	accountUpdateTimer.Update(statedb.AccountUpdates)                                 // Account updates are complete(in validation)
	storageUpdateTimer.Update(statedb.StorageUpdates)                                 // Storage updates are complete(in validation)
	accountHashTimer.Update(statedb.AccountHashes)                                    // Account hashes are complete(in validation)
	triehash := statedb.AccountHashes                                                 // The time spent on tries hashing
	trieUpdate := statedb.AccountUpdates + statedb.StorageUpdates                     // The time spent on tries update
	blockExecutionTimer.Update(ptime - (statedb.AccountReads + statedb.StorageReads)) // The time spent on EVM processing
	blockValidationTimer.Update(vtime - (triehash + trieUpdate))                      // The time spent on block validation
	blockCrossValidationTimer.Update(xvtime)                                          // The time spent on stateless cross validation

	// Write the block to the chain and get the status.
	// TODO: DB에 최종적으로 기록
	var (
		wstart = time.Now()
		status WriteStatus
	)
	if !setHead {
		// Don't set the head, only insert the block
		err = bc.writeBlockWithState(block, res.Receipts, statedb)
	} else {
		status, err = bc.writeBlockAndSetHead(block, res.Receipts, res.Logs, statedb, false)
	}
	if err != nil {
		return nil, err
	}

	// Update the metrics touched during block commit
	// TODO: 성능통계 기록 및 반환
	accountCommitTimer.Update(statedb.AccountCommits)   // Account commits are complete, we can mark them
	storageCommitTimer.Update(statedb.StorageCommits)   // Storage commits are complete, we can mark them
	snapshotCommitTimer.Update(statedb.SnapshotCommits) // Snapshot commits are complete, we can mark them
	triedbCommitTimer.Update(statedb.TrieDBCommits)     // Trie database commits are complete, we can mark them

	blockWriteTimer.Update(time.Since(wstart) - max(statedb.AccountCommits, statedb.StorageCommits) /* concurrent */ - statedb.SnapshotCommits - statedb.TrieDBCommits)
	elapsed := time.Since(startTime) + 1 // prevent zero division
	blockInsertTimer.Update(elapsed)

	// TODO(rjl493456442) generalize the ResettingTimer
	mgasps := float64(res.GasUsed) * 1000 / float64(elapsed)
	chainMgaspsMeter.Update(time.Duration(mgasps))

	return &blockProcessingResult{
		usedGas:  res.GasUsed,
		procTime: proctime,
		status:   status,
		witness:  witness,
	}, nil
}









// insertSideChain is called when an import batch hits upon a pruned ancestor
// error, which happens when a sidechain with a sufficiently old fork-block is
// found.
//
// The method writes all (header-and-body-valid) blocks to disk, then tries to
// switch over to the new chain if the TD exceeded the current chain.
// insertSideChain is only used pre-merge.
// TODO: 오래된 블록에서 분기된 '사이트체인'을 처리하기위한 블록 삽입 함수
// TODO: insertChain가 블록을 처리하다가, 특정블록의 부모에 대해 state를 찾을 수 없을때 이 함수가 호출된다.
func (bc *BlockChain) insertSideChain(block *types.Block, it *insertIterator, makeWitness bool) (*stateless.Witness, int, error) {
	var current = bc.CurrentBlock()

	// The first sidechain block error is already verified to be ErrPrunedAncestor.
	// Since we don't import them here, we expect ErrUnknownAncestor for the remaining
	// ones. Any other errors means that the block is invalid, and should not be written
	// to disk.
	err := consensus.ErrPrunedAncestor
	for ; block != nil && errors.Is(err, consensus.ErrPrunedAncestor); block, err = it.next() {
		// Check the canonical state root for that number
		if number := block.NumberU64(); current.Number.Uint64() >= number {
			canonical := bc.GetBlockByNumber(number)
			if canonical != nil && canonical.Hash() == block.Hash() {
				// Not a sidechain block, this is a re-import of a canon block which has it's state pruned
				continue
			}
			if canonical != nil && canonical.Root() == block.Root() {
				// This is most likely a shadow-state attack. When a fork is imported into the
				// database, and it eventually reaches a block height which is not pruned, we
				// just found that the state already exist! This means that the sidechain block
				// refers to a state which already exists in our canon chain.
				//
				// If left unchecked, we would now proceed importing the blocks, without actually
				// having verified the state of the previous blocks.
				log.Warn("Sidechain ghost-state attack detected", "number", block.NumberU64(), "sideroot", block.Root(), "canonroot", canonical.Root())

				// If someone legitimately side-mines blocks, they would still be imported as usual. However,
				// we cannot risk writing unverified blocks to disk when they obviously target the pruning
				// mechanism.
				return nil, it.index, errors.New("sidechain ghost-state attack")
			}
		}
		if !bc.HasBlock(block.Hash(), block.NumberU64()) {
			start := time.Now()
			if err := bc.writeBlockWithoutState(block); err != nil {
				return nil, it.index, err
			}
			log.Debug("Injected sidechain block", "number", block.Number(), "hash", block.Hash(),
				"diff", block.Difficulty(), "elapsed", common.PrettyDuration(time.Since(start)),
				"txs", len(block.Transactions()), "gas", block.GasUsed(), "uncles", len(block.Uncles()),
				"root", block.Root())
		}
	}
	// Gather all the sidechain hashes (full blocks may be memory heavy)
	var (
		hashes  []common.Hash
		numbers []uint64
	)
	parent := it.previous()
	for parent != nil && !bc.HasState(parent.Root) {
		if bc.stateRecoverable(parent.Root) {
			if err := bc.triedb.Recover(parent.Root); err != nil {
				return nil, 0, err
			}
			break
		}
		hashes = append(hashes, parent.Hash())
		numbers = append(numbers, parent.Number.Uint64())

		parent = bc.GetHeader(parent.ParentHash, parent.Number.Uint64()-1)
	}
	if parent == nil {
		return nil, it.index, errors.New("missing parent")
	}
	// Import all the pruned blocks to make the state available
	var (
		blocks []*types.Block
		memory uint64
	)
	for i := len(hashes) - 1; i >= 0; i-- {
		// Append the next block to our batch
		block := bc.GetBlock(hashes[i], numbers[i])

		blocks = append(blocks, block)
		memory += block.Size()

		// If memory use grew too large, import and continue. Sadly we need to discard
		// all raised events and logs from notifications since we're too heavy on the
		// memory here.
		if len(blocks) >= 2048 || memory > 64*1024*1024 {
			log.Info("Importing heavy sidechain segment", "blocks", len(blocks), "start", blocks[0].NumberU64(), "end", block.NumberU64())
			if _, _, err := bc.insertChain(blocks, true, false); err != nil {
				return nil, 0, err
			}
			blocks, memory = blocks[:0], 0

			// If the chain is terminating, stop processing blocks
			if bc.insertStopped() {
				log.Debug("Abort during blocks processing")
				return nil, 0, nil
			}
		}
	}
	if len(blocks) > 0 {
		log.Info("Importing sidechain segment", "start", blocks[0].NumberU64(), "end", blocks[len(blocks)-1].NumberU64())
		return bc.insertChain(blocks, true, makeWitness)
	}
	return nil, 0, nil
}



// recoverAncestors finds the closest ancestor with available state and re-execute
// all the ancestor blocks since that.
// recoverAncestors is only used post-merge.
// We return the hash of the latest block that we could correctly validate.
// TODO: 삭제된 과거기록 복구를 위해서 state가 없는 조상 블록을 순서대로 다시 실행하는 함수
func (bc *BlockChain) recoverAncestors(block *types.Block, makeWitness bool) (common.Hash, error) {
	// Gather all the sidechain hashes (full blocks may be memory heavy)
	var (
		hashes  []common.Hash
		numbers []uint64
		parent  = block
	)
	for parent != nil && !bc.HasState(parent.Root()) {
		if bc.stateRecoverable(parent.Root()) {
			if err := bc.triedb.Recover(parent.Root()); err != nil {
				return common.Hash{}, err
			}
			break
		}
		hashes = append(hashes, parent.Hash())
		numbers = append(numbers, parent.NumberU64())
		parent = bc.GetBlock(parent.ParentHash(), parent.NumberU64()-1)

		// If the chain is terminating, stop iteration
		if bc.insertStopped() {
			log.Debug("Abort during blocks iteration")
			return common.Hash{}, errInsertionInterrupted
		}
	}
	if parent == nil {
		return common.Hash{}, errors.New("missing parent")
	}
	// Import all the pruned blocks to make the state available
	for i := len(hashes) - 1; i >= 0; i-- {
		// If the chain is terminating, stop processing blocks
		if bc.insertStopped() {
			log.Debug("Abort during blocks processing")
			return common.Hash{}, errInsertionInterrupted
		}
		var b *types.Block
		if i == 0 {
			b = block
		} else {
			b = bc.GetBlock(hashes[i], numbers[i])
		}
		if _, _, err := bc.insertChain(types.Blocks{b}, false, makeWitness && i == 0); err != nil {
			return b.ParentHash(), err
		}
	}
	return block.Hash(), nil
}



// collectLogs collects the logs that were generated or removed during the
// processing of a block. These logs are later announced as deleted or reborn.
// TODO: 특정블록에 포함된 모든 tx이 실행되면서 생성된 eventlog를 수집하는 함수
func (bc *BlockChain) collectLogs(b *types.Block, removed bool) []*types.Log {
	var blobGasPrice *big.Int
	if b.ExcessBlobGas() != nil {
		blobGasPrice = eip4844.CalcBlobFee(bc.chainConfig, b.Header())
	}
	receipts := rawdb.ReadRawReceipts(bc.db, b.Hash(), b.NumberU64())
	if err := receipts.DeriveFields(bc.chainConfig, b.Hash(), b.NumberU64(), b.Time(), b.BaseFee(), blobGasPrice, b.Transactions()); err != nil {
		log.Error("Failed to derive block receipts fields", "hash", b.Hash(), "number", b.NumberU64(), "err", err)
	}
	var logs []*types.Log
	for _, receipt := range receipts {
		for _, log := range receipt.Logs {
			if removed {
				log.Removed = true
			}
			logs = append(logs, log)
		}
	}
	return logs
}



// reorg takes two blocks, an old chain and a new chain and will reconstruct the
// blocks and inserts them to be part of the new canonical chain and accumulates
// potential missing transactions and post an event about them.
//
// Note the new head block won't be processed here, callers need to handle it
// externally.
// TODO: 이더리움 노드가 fort를 해결하는 함수    현재보다 더 신뢰도 높은 체인이 나타냈을 때 수행되는 체인 재구성 작업
func (bc *BlockChain) reorg(oldHead *types.Header, newHead *types.Header) error {
	var (
		newChain    []*types.Header
		oldChain    []*types.Header
		commonBlock *types.Header
	)

	// Reduce the longer chain to the same number as the shorter one
	if oldHead.Number.Uint64() > newHead.Number.Uint64() {
		// Old chain is longer, gather all transactions and logs as deleted ones
		for ; oldHead != nil && oldHead.Number.Uint64() != newHead.Number.Uint64(); oldHead = bc.GetHeader(oldHead.ParentHash, oldHead.Number.Uint64()-1) {
			oldChain = append(oldChain, oldHead)
		}
	} else {
		// New chain is longer, stash all blocks away for subsequent insertion
		for ; newHead != nil && newHead.Number.Uint64() != oldHead.Number.Uint64(); newHead = bc.GetHeader(newHead.ParentHash, newHead.Number.Uint64()-1) {
			newChain = append(newChain, newHead)
		}
	}
	if oldHead == nil {
		return errInvalidOldChain
	}
	if newHead == nil {
		return errInvalidNewChain
	}
	// Both sides of the reorg are at the same number, reduce both until the common
	// ancestor is found
	for {
		// If the common ancestor was found, bail out
		if oldHead.Hash() == newHead.Hash() {
			commonBlock = oldHead
			break
		}
		// Remove an old block as well as stash away a new block
		oldChain = append(oldChain, oldHead)
		newChain = append(newChain, newHead)

		// Step back with both chains
		oldHead = bc.GetHeader(oldHead.ParentHash, oldHead.Number.Uint64()-1)
		if oldHead == nil {
			return errInvalidOldChain
		}
		newHead = bc.GetHeader(newHead.ParentHash, newHead.Number.Uint64()-1)
		if newHead == nil {
			return errInvalidNewChain
		}
	}
	// Ensure the user sees large reorgs
	if len(oldChain) > 0 && len(newChain) > 0 {
		logFn := log.Info
		msg := "Chain reorg detected"
		if len(oldChain) > 63 {
			msg = "Large chain reorg detected"
			logFn = log.Warn
		}
		logFn(msg, "number", commonBlock.Number, "hash", commonBlock.Hash(),
			"drop", len(oldChain), "dropfrom", oldChain[0].Hash(), "add", len(newChain), "addfrom", newChain[0].Hash())
		blockReorgAddMeter.Mark(int64(len(newChain)))
		blockReorgDropMeter.Mark(int64(len(oldChain)))
		blockReorgMeter.Mark(1)
	} else if len(newChain) > 0 {
		// Special case happens in the post merge stage that current head is
		// the ancestor of new head while these two blocks are not consecutive
		log.Info("Extend chain", "add", len(newChain), "number", newChain[0].Number, "hash", newChain[0].Hash())
		blockReorgAddMeter.Mark(int64(len(newChain)))
	} else {
		// len(newChain) == 0 && len(oldChain) > 0
		// rewind the canonical chain to a lower point.
		log.Error("Impossible reorg, please file an issue", "oldnum", oldHead.Number, "oldhash", oldHead.Hash(), "oldblocks", len(oldChain), "newnum", newHead.Number, "newhash", newHead.Hash(), "newblocks", len(newChain))
	}
	// Acquire the tx-lookup lock before mutation. This step is essential
	// as the txlookups should be changed atomically, and all subsequent
	// reads should be blocked until the mutation is complete.
	bc.txLookupLock.Lock()

	// Reorg can be executed, start reducing the chain's old blocks and appending
	// the new blocks
	var (
		deletedTxs []common.Hash
		rebirthTxs []common.Hash

		deletedLogs []*types.Log
		rebirthLogs []*types.Log
	)
	// Deleted log emission on the API uses forward order, which is borked, but
	// we'll leave it in for legacy reasons.
	//
	// TODO(karalabe): This should be nuked out, no idea how, deprecate some APIs?
	{
		for i := len(oldChain) - 1; i >= 0; i-- {
			block := bc.GetBlock(oldChain[i].Hash(), oldChain[i].Number.Uint64())
			if block == nil {
				return errInvalidOldChain // Corrupt database, mostly here to avoid weird panics
			}
			if logs := bc.collectLogs(block, true); len(logs) > 0 {
				deletedLogs = append(deletedLogs, logs...)
			}
			if len(deletedLogs) > 512 {
				bc.rmLogsFeed.Send(RemovedLogsEvent{deletedLogs})
				deletedLogs = nil
			}
		}
		if len(deletedLogs) > 0 {
			bc.rmLogsFeed.Send(RemovedLogsEvent{deletedLogs})
		}
	}
	// Undo old blocks in reverse order
	for i := 0; i < len(oldChain); i++ {
		// Collect all the deleted transactions
		block := bc.GetBlock(oldChain[i].Hash(), oldChain[i].Number.Uint64())
		if block == nil {
			return errInvalidOldChain // Corrupt database, mostly here to avoid weird panics
		}
		for _, tx := range block.Transactions() {
			deletedTxs = append(deletedTxs, tx.Hash())
		}
		// Collect deleted logs and emit them for new integrations
		if logs := bc.collectLogs(block, true); len(logs) > 0 {
			// Emit revertals latest first, older then
			slices.Reverse(logs)

			// TODO(karalabe): Hook into the reverse emission part
		}
	}
	// Apply new blocks in forward order
	for i := len(newChain) - 1; i >= 1; i-- {
		// Collect all the included transactions
		block := bc.GetBlock(newChain[i].Hash(), newChain[i].Number.Uint64())
		if block == nil {
			return errInvalidNewChain // Corrupt database, mostly here to avoid weird panics
		}
		for _, tx := range block.Transactions() {
			rebirthTxs = append(rebirthTxs, tx.Hash())
		}
		// Collect inserted logs and emit them
		if logs := bc.collectLogs(block, false); len(logs) > 0 {
			rebirthLogs = append(rebirthLogs, logs...)
		}
		if len(rebirthLogs) > 512 {
			bc.logsFeed.Send(rebirthLogs)
			rebirthLogs = nil
		}
		// Update the head block
		bc.writeHeadBlock(block)
	}
	if len(rebirthLogs) > 0 {
		bc.logsFeed.Send(rebirthLogs)
	}
	// Delete useless indexes right now which includes the non-canonical
	// transaction indexes, canonical chain indexes which above the head.
	batch := bc.db.NewBatch()
	for _, tx := range types.HashDifference(deletedTxs, rebirthTxs) {
		rawdb.DeleteTxLookupEntry(batch, tx)
	}
	// Delete all hash markers that are not part of the new canonical chain.
	// Because the reorg function does not handle new chain head, all hash
	// markers greater than or equal to new chain head should be deleted.
	number := commonBlock.Number
	if len(newChain) > 1 {
		number = newChain[1].Number
	}
	for i := number.Uint64() + 1; ; i++ {
		hash := rawdb.ReadCanonicalHash(bc.db, i)
		if hash == (common.Hash{}) {
			break
		}
		rawdb.DeleteCanonicalHash(batch, i)
	}
	if err := batch.Write(); err != nil {
		log.Crit("Failed to delete useless indexes", "err", err)
	}
	// Reset the tx lookup cache to clear stale txlookup cache.
	bc.txLookupCache.Purge()

	// Release the tx-lookup lock after mutation.
	bc.txLookupLock.Unlock()

	return nil
}




// InsertBlockWithoutSetHead executes the block, runs the necessary verification
// upon it and then persist the block and the associate state into the database.
// The key difference between the InsertChain is it won't do the canonical chain
// updating. It relies on the additional SetCanonical call to finalize the entire
// procedure.
// TODO: 새블록을 검증하고 DB에 저장, 단, 정식체인의 최신블록(head)로 설정하지는 않는 특수한 함수
func (bc *BlockChain) InsertBlockWithoutSetHead(block *types.Block, makeWitness bool) (*stateless.Witness, error) {
	if !bc.chainmu.TryLock() {
		return nil, errChainStopped
	}
	defer bc.chainmu.Unlock()

	witness, _, err := bc.insertChain(types.Blocks{block}, false, makeWitness)
	return witness, err
}



// SetCanonical rewinds the chain to set the new head block as the specified
// block. It's possible that the state of the new head is missing, and it will
// be recovered in this function as well.
// TODO: 지정된 블록을 정식체인의 새로운 최신블록(head)로 확정하는 최종단계의 함수 (이 과정에서 필요한 상태복구 + 체인 재구성을 모두 처리함)
func (bc *BlockChain) SetCanonical(head *types.Block) (common.Hash, error) {
	if !bc.chainmu.TryLock() {
		return common.Hash{}, errChainStopped
	}
	defer bc.chainmu.Unlock()

	// Re-execute the reorged chain in case the head state is missing.
	// TODO: 새로운 head 블록에 해당하는 상태데이터다 디스크에 존재하는지 확인 => 없다면 상태복구 로직 실행
	if !bc.HasState(head.Root()) {
		if latestValidHash, err := bc.recoverAncestors(head, false); err != nil {
			return latestValidHash, err
		}
		log.Info("Recovered head state", "number", head.Number(), "hash", head.Hash())
	}
	// Run the reorg if necessary and set the given block as new head.
	// TODO: 체인 분기 여부 확인 => 분기가 감지되면 재구성 작업 수행
	start := time.Now()
	if head.ParentHash() != bc.CurrentBlock().Hash() {
		if err := bc.reorg(bc.CurrentBlock(), head.Header()); err != nil {
			return common.Hash{}, err
		}
	}
	
	// TODO: 최신블록 확정
	bc.writeHeadBlock(head)

	// Emit events
	logs := bc.collectLogs(head, false)
	bc.chainFeed.Send(ChainEvent{Header: head.Header()})
	if len(logs) > 0 {
		bc.logsFeed.Send(logs)
	}
	bc.chainHeadFeed.Send(ChainHeadEvent{Header: head.Header()})

	context := []interface{}{
		"number", head.Number(),
		"hash", head.Hash(),
		"root", head.Root(),
		"elapsed", time.Since(start),
	}
	if timestamp := time.Unix(int64(head.Time()), 0); time.Since(timestamp) > time.Minute {
		context = append(context, []interface{}{"age", common.PrettyAge(timestamp)}...)
	}
	log.Info("Chain head was updated", context...)
	return head.Hash(), nil
}




// skipBlock returns 'true', if the block being imported can be skipped over, meaning
// that the block does not need to be processed but can be considered already fully 'done'.
// TODO: 블록을 삽입하는 과정에서 이미 DB에 존재하는 특정블록의 처리를 안전하게 건너 뛸 수 있는지 여부를 결정하는 함수
// TODO: EX 채인 재구성과같은 일이 일어날 경우
func (bc *BlockChain) skipBlock(err error, it *insertIterator) bool {
	// We can only ever bypass processing if the only error returned by the validator
	// is ErrKnownBlock, which means all checks passed, but we already have the block
	// and state.
	if !errors.Is(err, ErrKnownBlock) {
		return false
	}
	// If we're not using snapshots, we can skip this, since we have both block
	// and (trie-) state
	if bc.snaps == nil {
		return true
	}
	var (
		header     = it.current() // header can't be nil
		parentRoot common.Hash
	)
	// If we also have the snapshot-state, we can skip the processing.
	if bc.snaps.Snapshot(header.Root) != nil {
		return true
	}
	// In this case, we have the trie-state but not snapshot-state. If the parent
	// snapshot-state exists, we need to process this in order to not get a gap
	// in the snapshot layers.
	// Resolve parent block
	if parent := it.previous(); parent != nil {
		parentRoot = parent.Root
	} else if parent = bc.GetHeaderByHash(header.ParentHash); parent != nil {
		parentRoot = parent.Root
	}
	if parentRoot == (common.Hash{}) {
		return false // Theoretically impossible case
	}
	// Parent is also missing snapshot: we can skip this. Otherwise process.
	if bc.snaps.Snapshot(parentRoot) == nil {
		return true
	}
	return false
}



// reportBlock logs a bad block error.
// TODO: 블록처리 또는 검증과정에서 에러가 발생한 불량블럭에 대한 정보를 기록학 로그를 남기는 함수
func (bc *BlockChain) reportBlock(block *types.Block, res *ProcessResult, err error) {
	var receipts types.Receipts
	if res != nil {
		receipts = res.Receipts
	}
	rawdb.WriteBadBlock(bc.db, block)
	log.Error(summarizeBadBlock(block, receipts, bc.Config(), err))
}



// logForkReadiness will write a log when a future fork is scheduled, but not
// active. This is useful so operators know their client is ready for the fork.
// TODO: 예정된 하드포크가 다가올때, 노드운영자에게 하드포크를 할 준비가 되었다는 안내를 주기적으로 출력하는 함수
func (bc *BlockChain) logForkReadiness(block *types.Block) {
	config := bc.Config()
	current, last := config.LatestFork(block.Time()), config.LatestFork(math.MaxUint64)

	// Short circuit if the timestamp of the last fork is undefined,
	// or if the network has already passed the last configured fork.
	t := config.Timestamp(last)
	if t == nil || current >= last {
		return
	}
	at := time.Unix(int64(*t), 0)

	// Only log if:
	// - Current time is before the fork activation time
	// - Enough time has passed since last alert
	now := time.Now()
	if now.Before(at) && now.After(bc.lastForkReadyAlert.Add(forkReadyInterval)) {
		log.Info("Ready for fork activation", "fork", last, "date", at.Format(time.RFC822),
			"remaining", time.Until(at).Round(time.Second), "timestamp", at.Unix())
		bc.lastForkReadyAlert = time.Now()
	}
}



// summarizeBadBlock returns a string summarizing the bad block and other
// relevant information.
// 유효성 검사에 실패한 블량블록에 대한 모든 관련 정보를 사람이 읽기 좋은 형식의 문자열로 요약하여 리턴하는 함수
func summarizeBadBlock(block *types.Block, receipts []*types.Receipt, config *params.ChainConfig, err error) string {
	var receiptString string
	for i, receipt := range receipts {
		receiptString += fmt.Sprintf("\n  %d: cumulative: %v gas: %v contract: %v status: %v tx: %v logs: %v bloom: %x state: %x",
			i, receipt.CumulativeGasUsed, receipt.GasUsed, receipt.ContractAddress.Hex(),
			receipt.Status, receipt.TxHash.Hex(), receipt.Logs, receipt.Bloom, receipt.PostState)
	}
	version, vcs := version.Info()
	platform := fmt.Sprintf("%s %s %s %s", version, runtime.Version(), runtime.GOARCH, runtime.GOOS)
	if vcs != "" {
		vcs = fmt.Sprintf("\nVCS: %s", vcs)
	}
	return fmt.Sprintf(`
########## BAD BLOCK #########
Block: %v (%#x)
Error: %v
Platform: %v%v
Chain config: %#v
Receipts: %v
##############################
`, block.Number(), block.Hash(), err, platform, vcs, config, receiptString)
}



// InsertHeaderChain attempts to insert the given header chain in to the local
// chain, possibly creating a reorg. If an error is returned, it will return the
// index number of the failing header as well an error describing what went wrong.
// TODO: 외부에서 받은 블록헤더 묶음을 검증한뒤, 로컬 헤더 체인에 삽입하는 함수 *필요하다면 체인 재구성도 수행
func (bc *BlockChain) InsertHeaderChain(chain []*types.Header) (int, error) {
	if len(chain) == 0 {
		return 0, nil
	}
	start := time.Now()
	if i, err := bc.hc.ValidateHeaderChain(chain); err != nil {
		return i, err
	}
	if !bc.chainmu.TryLock() {
		return 0, errChainStopped
	}
	defer bc.chainmu.Unlock()

	_, err := bc.hc.InsertHeaderChain(chain, start)
	return 0, err
}



// InsertHeadersBeforeCutoff inserts the given headers into the ancient store
// as they are claimed older than the configured chain cutoff point. All the
// inserted headers are regarded as canonical and chain reorg is not supported.
// TODO: 오래된 블록 헤더묶음을 freezer에 직접 기록하는 특정 목적의 함수 (주로 동기화 과정에서 대량의 과거 헤더 데이터를 효율적으로 저장하기 위해 사용)
func (bc *BlockChain) InsertHeadersBeforeCutoff(headers []*types.Header) (int, error) {
	if len(headers) == 0 {
		return 0, nil
	}
	// TODO(rjl493456442): Headers before the configured cutoff have already
	// been verified by the hash of cutoff header. Theoretically, header validation
	// could be skipped here.
	if n, err := bc.hc.ValidateHeaderChain(headers); err != nil {
		return n, err
	}
	if !bc.chainmu.TryLock() {
		return 0, errChainStopped
	}
	defer bc.chainmu.Unlock()

	// Initialize the ancient store with genesis block if it's empty.
	var (
		frozen, _ = bc.db.Ancients()
		first     = headers[0].Number.Uint64()
	)
	if first == 1 && frozen == 0 {
		_, err := rawdb.WriteAncientBlocks(bc.db, []*types.Block{bc.genesisBlock}, []rlp.RawValue{rlp.EmptyList})
		if err != nil {
			log.Error("Error writing genesis to ancients", "err", err)
			return 0, err
		}
		log.Info("Wrote genesis to ancient store")
	} else if frozen != first {
		return 0, fmt.Errorf("headers are gapped with the ancient store, first: %d, ancient: %d", first, frozen)
	}

	// Write headers to the ancient store, with block bodies and receipts set to nil
	// to ensure consistency across tables in the freezer.
	_, err := rawdb.WriteAncientHeaderChain(bc.db, headers)
	if err != nil {
		return 0, err
	}
	// Sync the ancient store explicitly to ensure all data has been flushed to disk.
	if err := bc.db.SyncAncient(); err != nil {
		return 0, err
	}
	// Write hash to number mappings
	batch := bc.db.NewBatch()
	for _, header := range headers {
		rawdb.WriteHeaderNumber(batch, header.Hash(), header.Number.Uint64())
	}
	// Write head header and head snap block flags
	last := headers[len(headers)-1]
	rawdb.WriteHeadHeaderHash(batch, last.Hash())
	rawdb.WriteHeadFastBlockHash(batch, last.Hash())
	if err := batch.Write(); err != nil {
		return 0, err
	}
	// Truncate the useless chain segment (zero bodies and receipts) in the
	// ancient store.
	if _, err := bc.db.TruncateTail(last.Number.Uint64() + 1); err != nil {
		return 0, err
	}
	// Last step update all in-memory markers
	bc.hc.currentHeader.Store(last)
	bc.currentSnapBlock.Store(last)
	headHeaderGauge.Update(last.Number.Int64())
	headFastBlockGauge.Update(last.Number.Int64())
	return 0, nil
}


// SetBlockValidatorAndProcessorForTesting sets the current validator and processor.
// This method can be used to force an invalid blockchain to be verified for tests.
// This method is unsafe and should only be used before block import starts.
func (bc *BlockChain) SetBlockValidatorAndProcessorForTesting(v Validator, p Processor) {
	bc.validator = v
	bc.processor = p
}

// SetTrieFlushInterval configures how often in-memory tries are persisted to disk.
// The interval is in terms of block processing time, not wall clock.
// It is thread-safe and can be called repeatedly without side effects.
func (bc *BlockChain) SetTrieFlushInterval(interval time.Duration) {
	bc.flushInterval.Store(int64(interval))
}

// GetTrieFlushInterval gets the in-memory tries flushAlloc interval
func (bc *BlockChain) GetTrieFlushInterval() time.Duration {
	return time.Duration(bc.flushInterval.Load())
}

