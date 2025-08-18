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

	node.node

	노드의 틀과 생명주기를 관리하는 최상위 구조체
	디스크 경로, 로그설정, IPC / HTTP / WEBsocekt서버, DB, p2p network, 개별 서비스등록등을 모두 통합하는 객체이다.

	노드 시작,종료, RPC endpoint열고 닫기,  DB열고 닫기 등의 메서드들이 들어있다.
*/

package node

import (
	crand "crypto/rand"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gofrs/flock"
)

// Node is a container on which services can be registered.
// Geth의 핵심 구조체 Node 하나의 실행 프로세스가 운영되기 위한 모든 구성요소를 담고 있다.
type Node struct {

	// field
	eventmux      *event.TypeMux			// 내부 이벤트를 처리하는 중앙 이벤트 버스
	config        *Config					// 전체 노드의 설정값을 갖고있는 구조체 node.config  (노드의 config이다. cmd의 config 아님.)
	accman        *accounts.Manager			// 키스토어 및 계정 관리 담당
	log           log.Logger				// 로그 출력용
	keyDir        string        // key store directory  키스토어 파일 경로
	keyDirTemp    bool          // If true, key directory will be removed by Stop 임시 키 디렉토리 여부
	dirLock       *flock.Flock  // prevents concurrent use of instance directory TODO: geth 인스턴스가 해당 디렉토리를 독점하도록 lock 을 거는 용도
	stop          chan struct{} // Channel to wait for termination notifications 	노드가 종료될 때 다른 모듈에게 알리는 신호 채널
	server        *p2p.Server   // Currently running P2P networking layer			p2p 네트워크 서버 객체
	startStopLock sync.Mutex    // Start/Stop are protected by an additional lock   star(), stop() 호출을 동시성을 안전하게 보호 하는 역할
	state         int           // Tracks state of node lifecycle					현재 노드 상태 (pre start, running, stopped)


	// * 라이프사이클 & 서비스 관련된 field
	lock          sync.Mutex  // 전체 구조체 내부 공유 자원 보호용
	lifecycles    []Lifecycle // All registered backends, services, and auxiliary services that have a lifecycle		노드에 등록된 모든 백엔드/서비스의 생명주기 목록
	rpcAPIs       []rpc.API   // List of APIs currently provided by the node	이 노드가 외부에 제공하는 RPC API 목록
	
	// * RPC 서버 관련 field
	http          *httpServer //	http 기반 RPC서버 및 인증 버전
	ws            *httpServer //	webSocket RPC 서버 및 인증 버전
	httpAuth      *httpServer //	webSocket RPC 서버 및 인증 버전
	wsAuth        *httpServer //	webSocket RPC 서버 및 인증 버전
	ipc           *ipcServer  // Stores information about the ipc http server 	IPC 방식의 RPC 서버
	inprocHandler *rpc.Server // In-process RPC request handler to process the API requests		프로세스 내부에서 API 요청 처리하는 RPC 서버

	// * dataBase field
	databases map[*closeTrackingDB]struct{} // All open databases		현재 열려 있는 모든 DB 핸들 추적. 종료시 닫기 위함.
}

// Node.node의 실행 상태를 추적하기 위한 상수들
const (
	initializingState = iota	// 0 : 노드가 생성중 (시작 전)
	runningState				// 1 : 노드가 실행중 (start)
	closedState					// 2 : 노드가 종료되어 리소스도 정리된 상태 (stop 이후)
)

// New creates a new P2P node, ready for protocol registration.
// TODO: *Node를 초기화해서 생성하는 엔트리 포인트이다.   node.config를 기반으로 노드 한개의 인스턴스를 초기화하고 start호출전에 모든 준비를 마친 객체를 반환한다.
func New(conf *Config) (*Node, error) {
	// Copy config and resolve the datadir so future changes to the current
	// working directory don't affect the node.
	confCopy := *conf		
	conf = &confCopy		// 외부에서 받은 config를  직접 수정하기 위해 복사본을 사용		* 외부에서 같은 config 사용시 부작용이 있을 수 있음
	
	if conf.DataDir != "" {
		absdatadir, err := filepath.Abs(conf.DataDir)
		if err != nil {
			return nil, err
		}
		conf.DataDir = absdatadir
	}
	
	// 로거가 생성되지 않았다면 기본 로그 객체 생성
	if conf.Logger == nil {
		conf.Logger = log.New()
	}

	// Ensure that the instance name doesn't cause weird conflicts with
	// other files in the data directory.
	// IPC 소켓 충돌이나 파일 시스템  문제를 방지하기 위해서 conf.Name 제약 조건을 검사한다.
	if strings.ContainsAny(conf.Name, `/\`) {
		return nil, errors.New(`Config.Name must not contain '/' or '\'`)
	}
	if conf.Name == datadirDefaultKeyStore {
		return nil, errors.New(`Config.Name cannot be "` + datadirDefaultKeyStore + `"`)
	}
	if strings.HasSuffix(conf.Name, ".ipc") {
		return nil, errors.New(`Config.Name cannot end in ".ipc"`)
	}

	// RPC 요청을 처리할 in-process RPC 서버 객체를 생성
	server := rpc.NewServer()
	server.SetBatchLimits(conf.BatchRequestLimit, conf.BatchResponseMaxSize)

	// TODO: 노드 인스턴스 생성 ( 내부의 구성요소를 초기화하는 과정)
	node := &Node{
		config:        conf,
		inprocHandler: server,
		eventmux:      new(event.TypeMux),
		log:           conf.Logger,
		stop:          make(chan struct{}),
		server:        &p2p.Server{Config: conf.P2P},
		databases:     make(map[*closeTrackingDB]struct{}),
	}

	// Register built-in APIs.		geth가 기본적으로 제공하는 내부 Api 등록
	node.rpcAPIs = append(node.rpcAPIs, node.apis()...)

	// TODO: Acquire the instance directory lock.	 데이터 디렉토리 락 획득 + 키스토어 설정
	// 	    * 디렉토리마다 락을 만드는 것은 여러 인스턴스가 동시에 열 경우 데이터 손상 위험이 있기 때문
	if err := node.openDataDir(); err != nil {
		return nil, err
	}
	keyDir, isEphem, err := conf.GetKeyStoreDir()
	if err != nil {
		return nil, err
	}
	node.keyDir = keyDir
	node.keyDirTemp = isEphem


	// Creates an empty AccountManager with no backends. Callers (e.g. cmd/geth)
	// are required to add the backends later on.
	// TODO: 계정 관리자 생성   빈 계정 관리자 생성
	//  	 
	node.accman = accounts.NewManager(nil)

	// Initialize the p2p server. This creates the node key and discovery databases.
	// p2p 서버 설정 초기화
	node.server.Config.PrivateKey = node.config.NodeKey()
	node.server.Config.Name = node.config.NodeName()
	node.server.Config.Logger = node.log
	node.config.checkLegacyFiles()
	if node.server.Config.NodeDatabase == "" {
		node.server.Config.NodeDatabase = node.config.NodeDB()
	}

	// Check HTTP/WS prefixes are valid.		RPC 경로 유효성 체크  
	if err := validatePrefix("HTTP", conf.HTTPPathPrefix); err != nil {
		return nil, err
	}
	if err := validatePrefix("WebSocket", conf.WSPathPrefix); err != nil {
		return nil, err
	}

	// Configure RPC servers.		RPC 서버 생성  실제 객체를 생성 이 객체들이 외부로부터 RPC 요청을 받게된다.
	node.http = newHTTPServer(node.log, conf.HTTPTimeouts)
	node.httpAuth = newHTTPServer(node.log, conf.HTTPTimeouts)
	node.ws = newHTTPServer(node.log, rpc.DefaultHTTPTimeouts)
	node.wsAuth = newHTTPServer(node.log, rpc.DefaultHTTPTimeouts)
	node.ipc = newIPCServer(node.log, conf.IPCEndpoint())

	return node, nil		// 노드를 리턴
}

// Start starts all registered lifecycles, RPC services and p2p networking.
// Node can only be started once.
// TODO: Node.Start() 노드를 한번만 실행가능하게 하고, 내부적으로 RPC, P2P, lifeCycle service를 초기화하고 실행
// 	     노드 중복호출을 방지하면서, 노드상태 확인 + 전이, 모든 서비스 start, 서버열기등의 역할을 한다.
func (n *Node) Start() error {
	n.startStopLock.Lock()				// ** start에 대해 lock 노드 중복 실행 방지
	defer n.startStopLock.Unlock()

	// 상태 체크 및 전이: runningState이면 실행중이니까 err, closedState면 종료노드니까 err, runningState로 변경해서 이제 실행중 상태로 전환
	n.lock.Lock()
	switch n.state {
	case runningState:
		n.lock.Unlock()
		return ErrNodeRunning
	case closedState:
		n.lock.Unlock()
		return ErrNodeStopped
	}

	// TODO: RPC 및 네트워크 엔드포인트 시작
	n.state = runningState
	// open networking and RPC endpoints	
	err := n.openEndpoints()	// HTTP, WebSocket, IPC 서버를 열고 내부적으로 소켓열기
	
	// 모든 서비스를 잠금 없이 안전하게 복사해둔다. 
	lifecycles := make([]Lifecycle, len(n.lifecycles))
	copy(lifecycles, n.lifecycles)
	n.lock.Unlock()

	// Check if endpoint startup failed.  엔드포인트 열기에 실패했다면 즉시 클린업 후 종료
	if err != nil {
		n.doClose(nil)
		return err
	}
	// Start all registered lifecycles.	 TODO: 모든 구성요소의 .Start()를 순차적 호출  EX) Ethereum.Start(), Admin.Start() ...  모듈들을 실행함.
	var started []Lifecycle
	for _, lifecycle := range lifecycles {
		if err = lifecycle.Start(); err != nil {	
			break
		}
		started = append(started, lifecycle)
	}
	// Check if any lifecycle failed to start.
	// TODO:  ** 라이프 사이클 중 어떤것들이던 실패시 전체 노드 종료
	if err != nil {
		n.stopServices(started)
		n.doClose(nil)
	}
	return err
}

// Close stops the Node and releases resources acquired in
// Node constructor New.
// node.Node를 종료하는 함수		 상황에 따라서 여러 lifecycle stop
func (n *Node) Close() error {
	n.startStopLock.Lock()			// close중에 close 방지를 위한 lock
	defer n.startStopLock.Unlock()

	n.lock.Lock()
	state := n.state
	n.lock.Unlock()
	switch state {
	case initializingState:					// start()한적 없으면 내부 리소스만 close
		// The node was never started.
		return n.doClose(nil)
	case runningState:						// 실행중이면 모든 등록된 lifecycle.stop()으로 모두 정지
		// The node was started, release resources acquired by Start().
		var errs []error
		if err := n.stopServices(n.lifecycles); err != nil {
			errs = append(errs, err)
		}
		return n.doClose(errs)
	case closedState:						// 이미 닫힌 상태라면 err
		return ErrNodeStopped
	default:								// 이상한 상태인 경우 panic
		panic(fmt.Sprintf("node is in unknown state %d", state))
	}
}

// doClose releases resources acquired by New(), collecting errors.
// geth의 Close() method에서 호출되는 것으로 남아있는 리소스를 모두 정리하고, 파일 및 디렉토리를 제거하며, 종료결과를 리턴하는 함수
func (n *Node) doClose(errs []error) error {
	// Close databases. This needs the lock because it needs to
	// synchronize with OpenDatabase*.
	// 락을 걸고 LevelDB등 모든 DB 핸들을 안전하게 닫는다.
	n.lock.Lock()
	n.state = closedState
	errs = append(errs, n.closeDatabases()...)		// closeDatabases <--- db bandle 닫기
	n.lock.Unlock()

	// 계정관리자가 내부적으로 연 DB나 파일 핸들을 닫는다.
	if err := n.accman.Close(); err != nil {
		errs = append(errs, err)
	}
	// 일시적 키 디렉토리도 닫음 -> 전체삭제
	if n.keyDirTemp {
		if err := os.RemoveAll(n.keyDir); err != nil {
			errs = append(errs, err)
		}
	}

	// Release instance directory lock.		인스턴스 디렉토리 락 해제 *락파일 제거 등
	n.closeDataDir()

	// Unblock n.Wait.	 종료신호를 보낸다.
	close(n.stop)

	// Report any errors that might have occurred.		에러가 있는지 체크하고 에러있다면 반환하고 아니면 그대로 정상종료
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		return fmt.Errorf("%v", errs)
	}
}

// openEndpoints starts all network and RPC endpoints.
// TODO: 외부와 연결 가능한 모든 엔드포인트 (p2p network, RPC server)를 시작하는 함수  Node.Start() 안에서 호출되며, 이로인해 네트워크 상에 작동하게 된다.
//  peer를 찾고, p2p연결 + 메세지 교환 채널 생성을하고,  RPC 소켓을 연다.   *** RPC != p2p
func (n *Node) openEndpoints() error {
	// start networking endpoints
	n.log.Info("Starting peer-to-peer node", "instance", n.server.Name)	
	
	// TODO: peer discovery 시작 + 노드간 연결 수락 및 다이얼 + p2p 메세지 교환 채널 생성을 수행한다.    n.server.Start()
	if err := n.server.Start(); err != nil {
		return convertFileLockError(err)
	}
	// start RPC endpoints
	// startRPC()는 IPC, HTTP, WebSocket 등 다양한 RPC 엔드포인트를 연다.
	err := n.startRPC()
	if err != nil {
		n.stopRPC()
		n.server.Stop()
	}
	return err
}



// stopServices terminates running services, RPC and p2p networking.
// It is the inverse of Start.
// 노드 중지 로직 중 하나로, 노드가 정리될 떄 실행중인 모든 구성요소 lifecycle, RPC, P2P를 안전하게 종료하는 역할
func (n *Node) stopServices(running []Lifecycle) error {
	n.stopRPC()			// 1. RPC 서버 종료

	// Stop running lifecycles in reverse order.	2. lifeCycle 객체를 역순으로 하나씩 종료
	// TODO: 역순으로 종료하는 이유: 의존성 문제를 방지하기 위함이다.  A가 B를 사용한다면, B부터 종료해서는 안된다. EX) EVM -> Txpool -> Chain -> DB
	failure := &StopError{Services: make(map[reflect.Type]error)}
	for i := len(running) - 1; i >= 0; i-- {
		if err := running[i].Stop(); err != nil {
			failure.Services[reflect.TypeOf(running[i])] = err
		}
	}

	// Stop p2p networking.		3. p2p server 종료
	n.server.Stop()

	if len(failure.Services) > 0 {
		return failure
	}
	return nil
}



// 노드가 사용하는 데이터 디렉토리를 안전하게 준비하고, 동시접근을 막기위한 락을 거는 역할을 한다.
func (n *Node) openDataDir() error {
	// DataDir이 비어있다면, 디스크가 필요없는 일회성 인스턴스이므로 그냥 통과한다.
	if n.config.DataDir == "" {
		return nil // ephemeral
	}

	// 데이터를 저장할 경로를 생성 + 디렉토리가 없다면 생성한다.  
	instdir := filepath.Join(n.config.DataDir, n.config.name())
	if err := os.MkdirAll(instdir, 0700); err != nil {
		return err
	}
	// Lock the instance directory to prevent concurrent use by another instance as well as
	// accidental use of the instance directory as a database.
	// 파일 락 객체를 생성한다.
	n.dirLock = flock.New(filepath.Join(instdir, "LOCK"))

	if locked, err := n.dirLock.TryLock(); err != nil {
		return err
	} else if !locked {
		return ErrDatadirUsed
	}
	return nil
}


// 노드가 종료될 때, 열려있던 인스턴스 디렉토리의 락을 해제하는 역할을 하는 함수
func (n *Node) closeDataDir() {
	// Release instance directory lock.
	if n.dirLock != nil && n.dirLock.Locked() {
		n.dirLock.Unlock()
		n.dirLock = nil
	}
}



// ObtainJWTSecret loads the jwt-secret from the provided config. If the file is not
// present, it generates a new secret and stores to the given location.
// TODO: JWT 인증에 사용할 32bytes secret key를 파일에 로드하거나, 없으면 새로 생성하는 함수이다.
// TODO: JWT는 RPC를 위해서 
func ObtainJWTSecret(fileName string) ([]byte, error) {
	// try reading from file
	// 파일에서 JWT secret을 불러오도록 시도 -> 정상적으로 읽힌다면 hex인코딩된 JWT 문자열을 디코딩해서 길이가 정확히 32bytes인지 확인.
	if data, err := os.ReadFile(fileName); err == nil {
		jwtSecret := common.FromHex(strings.TrimSpace(string(data)))
		if len(jwtSecret) == 32 {
			log.Info("Loaded JWT secret file", "path", fileName, "crc32", fmt.Sprintf("%#x", crc32.ChecksumIEEE(jwtSecret)))
			return jwtSecret, nil
		}
		log.Error("Invalid JWT secret", "path", fileName, "length", len(jwtSecret))
		return nil, errors.New("invalid JWT secret")
	}

	// Need to generate one
	// 파일이 없거나 읽기 실패시 새로 생성한다.
	jwtSecret := make([]byte, 32)
	crand.Read(jwtSecret)
	
	// if we're in --dev mode, don't bother saving, just show it
	// --dev 모드라면 저장하지 않고 출력, 정상모드라면 파일로 저장한다. (저장완료 후 반환)
	if fileName == "" {
		log.Info("Generated ephemeral JWT secret", "secret", hexutil.Encode(jwtSecret))
		return jwtSecret, nil
	}
	if err := os.WriteFile(fileName, []byte(hexutil.Encode(jwtSecret)), 0600); err != nil {
		return nil, err
	}
	log.Info("Generated JWT secret", "path", fileName)
	return jwtSecret, nil
}

// obtainJWTSecret loads the jwt-secret, either from the provided config,
// or from the default location. If neither of those are present, it generates
// a new secret and stores to the default location.
// JWT secret key를 확보하는 헬퍼 함수		
func (n *Node) obtainJWTSecret(cliParam string) ([]byte, error) {
	// geth명령어로 jwt secret eky 경로를 설정했는지 확인 없다면 기본 경로를 사용한다.
	fileName := cliParam
	if len(fileName) == 0 {
		// no path provided, use default
		fileName = n.ResolvePath(datadirJWTKey)
	}

	return ObtainJWTSecret(fileName)	// 위의 obtainJWTSecret함수를 통해서 읽거나 새로 생성한다.
}




// startRPC is a helper method to configure all the various RPC endpoints during node
// startup. It's not meant to be called at any time afterwards as it makes certain
// assumptions about the state of the node.
// TODO: 노드 설정값을 읽어 열어도되는 API를 고른뒤, 필요한 HTTP/WS/IPC/Auth 서버들을 준비하고 보안자원정책을 붙여 순차적으로 실행하는 함수
func (n *Node) startRPC() error {

	// 같은 프로세스 내부에서 RPC를 직접 호출할 수 있도록 등록
	if err := n.startInProc(n.rpcAPIs); err != nil {
		return err
	}
	// Configure IPC.
	if n.ipc.endpoint != "" {
		if err := n.ipc.start(n.rpcAPIs); err != nil {
			return err
		}
	}

	// server모음, api 모음
	var (
		servers           []*httpServer		// 나중에 한번에 start하려고 모은 리스트
		openAPIs, allAPIs = n.getAPIs()		// 비인증 모듈 api, 인증 포함 전체 모듈 api
	)

	// 배치 요청 / 응답 크기 제한 설정 dos방지
	rpcConfig := rpcEndpointConfig{
		batchItemLimit:         n.config.BatchRequestLimit,
		batchResponseSizeLimit: n.config.BatchResponseMaxSize,
	}

	// HTTP초기화
	initHttp := func(server *httpServer, port int) error {
		if err := server.setListenAddr(n.config.HTTPHost, port); err != nil {
			return err
		}
		if err := server.enableRPC(openAPIs, httpConfig{
			CorsAllowedOrigins: n.config.HTTPCors,
			Vhosts:             n.config.HTTPVirtualHosts,
			Modules:            n.config.HTTPModules,
			prefix:             n.config.HTTPPathPrefix,
			rpcEndpointConfig:  rpcConfig,
		}); err != nil {
			return err
		}
		servers = append(servers, server)
		return nil
	}

	// WS 초기화
	initWS := func(port int) error {
		server := n.wsServerForPort(port, false)
		if err := server.setListenAddr(n.config.WSHost, port); err != nil {
			return err
		}
		if err := server.enableWS(openAPIs, wsConfig{
			Modules:           n.config.WSModules,
			Origins:           n.config.WSOrigins,
			prefix:            n.config.WSPathPrefix,
			rpcEndpointConfig: rpcConfig,
		}); err != nil {
			return err
		}
		servers = append(servers, server)
		return nil
	}

	// Auth HTTP/WS 초기화
	initAuth := func(port int, secret []byte) error {
		// Enable auth via HTTP
		server := n.httpAuth
		if err := server.setListenAddr(n.config.AuthAddr, port); err != nil {
			return err
		}
		sharedConfig := rpcEndpointConfig{
			jwtSecret:              secret,
			batchItemLimit:         engineAPIBatchItemLimit,
			batchResponseSizeLimit: engineAPIBatchResponseSizeLimit,
			httpBodyLimit:          engineAPIBodyLimit,
		}
		err := server.enableRPC(allAPIs, httpConfig{
			CorsAllowedOrigins: DefaultAuthCors,
			Vhosts:             n.config.AuthVirtualHosts,
			Modules:            DefaultAuthModules,
			prefix:             DefaultAuthPrefix,
			rpcEndpointConfig:  sharedConfig,
		})
		if err != nil {
			return err
		}
		servers = append(servers, server)

		// Enable auth via WS
		server = n.wsServerForPort(port, true)
		if err := server.setListenAddr(n.config.AuthAddr, port); err != nil {
			return err
		}
		if err := server.enableWS(allAPIs, wsConfig{
			Modules:           DefaultAuthModules,
			Origins:           DefaultAuthOrigins,
			prefix:            DefaultAuthPrefix,
			rpcEndpointConfig: sharedConfig,
		}); err != nil {
			return err
		}
		servers = append(servers, server)
		return nil
	}

	// Set up HTTP.
	if n.config.HTTPHost != "" {
		// Configure legacy unauthenticated HTTP.
		if err := initHttp(n.http, n.config.HTTPPort); err != nil {
			return err
		}
	}
	// Configure WebSocket.
	if n.config.WSHost != "" {
		// legacy unauthenticated
		if err := initWS(n.config.WSPort); err != nil {
			return err
		}
	}
	// Configure authenticated API
	if len(openAPIs) != len(allAPIs) {
		jwtSecret, err := n.obtainJWTSecret(n.config.JWTSecret)
		if err != nil {
			return err
		}
		if err := initAuth(n.config.AuthPort, jwtSecret); err != nil {
			return err
		}
	}
	// Start the servers	모든 서버 시작.
	for _, server := range servers {
		if err := server.start(); err != nil {
			return err
		}
	}
	return nil
}



// WS를 어느 서버 인스턴스에 붙일지 결정하는 함수
func (n *Node) wsServerForPort(port int, authenticated bool) *httpServer {
	httpServer, wsServer := n.http, n.ws
	if authenticated {
		httpServer, wsServer = n.httpAuth, n.wsAuth
	}
	if n.config.HTTPHost == "" || httpServer.port == port {
		return httpServer
	}
	return wsServer
}


// 노드에 열어둔 모든 RPC endpoint를 역순으로 shutdown하는 함수
func (n *Node) stopRPC() {
	n.http.stop()
	n.ws.stop()
	n.httpAuth.stop()
	n.wsAuth.stop()
	n.ipc.stop()
	n.stopInProc()
}

// startInProc registers all RPC APIs on the inproc server.
// in-proc RPC handler에 API들을 등록하는 초기화 루틴 함수
func (n *Node) startInProc(apis []rpc.API) error {
	for _, api := range apis {
		if err := n.inprocHandler.RegisterName(api.Namespace, api.Service); err != nil {
			return err
		}
	}
	return nil
}

// stopInProc terminates the in-process RPC endpoint.
// 내부에서만 접근가능한 RPC 서버를 종료하는 함수
func (n *Node) stopInProc() {
	n.inprocHandler.Stop()
}

// Wait blocks until the node is closed.
// n.stop 채녈에서 값이 들어오거나 닫힐때까지 기다린다
func (n *Node) Wait() {
	<-n.stop
}

// RegisterLifecycle registers the given Lifecycle on the node.
// 주어진 lifeCycle을 노드에 등록하는 함수		(모듈마다 lifeCycle(인터페이스) 존재하고 이것들을 노드에 붙인다.)
func (n *Node) RegisterLifecycle(lifecycle Lifecycle) {
	n.lock.Lock()				// 락 잡고 시작
	defer n.lock.Unlock()

	// initializingState 일때만 수행가능하도록, 노드 초기화때만 붙일 수 있게
	if n.state != initializingState {
		panic("can't register lifecycle on running/stopped node")
	}	
	// lifeCycle이 이미 있는지 확인
	if slices.Contains(n.lifecycles, lifecycle) {
		panic(fmt.Sprintf("attempt to register lifecycle %T more than once", lifecycle))
	}

	n.lifecycles = append(n.lifecycles, lifecycle)	// 노드에 lifeCycle을 등록
}


// RegisterProtocols adds backend's protocols to the node's p2p server.
// P2P 통신 프로토콜을 등록하는 함수
func (n *Node) RegisterProtocols(protocols []p2p.Protocol) {
	n.lock.Lock()			// 락
	defer n.lock.Unlock()

	// initializaingState인 경우만
	if n.state != initializingState {
		panic("can't register protocols on running/stopped node")
	}

	// p2p 프로토콜 목록을 노드에 p2p서버에 추가한다.
	n.server.Protocols = append(n.server.Protocols, protocols...)
}




// RegisterAPIs registers the APIs a service provides on the node.
// RPC API를 등록하는 함수
func (n *Node) RegisterAPIs(apis []rpc.API) {
	n.lock.Lock()
	defer n.lock.Unlock()

	// 초기화할때만,
	if n.state != initializingState {
		panic("can't register APIs on running/stopped node")
	}
	
	// rpcAPI 등록
	n.rpcAPIs = append(n.rpcAPIs, apis...)
}



// getAPIs return two sets of APIs, both the ones that do not require
// authentication, and the complete set
// Geth노드에 등록된 AIP들을 분류하여서 반환하는 함수		(authenticated / unauthenticated 로 나뉜다.)
func (n *Node) getAPIs() (unauthenticated, all []rpc.API) {
	for _, api := range n.rpcAPIs {
		if !api.Authenticated {
			unauthenticated = append(unauthenticated, api)
		}
	}
	return unauthenticated, n.rpcAPIs
}



// RegisterHandler mounts a handler on the given path on the canonical HTTP server.
//
// The name of the handler is shown in a log message when the HTTP server starts
// and should be a descriptive term for the service provided by the handler.
// HTTP요청을 처리하는 핸들러를 등록하는 함수
func (n *Node) RegisterHandler(name, path string, handler http.Handler) {
	n.lock.Lock()
	defer n.lock.Unlock()

	if n.state != initializingState {
		panic("can't register HTTP handler on running/stopped node")
	}

	// path에 handler를 연결한다.  즉, 외부에서 특정 path로 요청이 들어오면 handler가 요청을 받아서 처리한다.
	n.http.mux.Handle(path, handler)
	n.http.handlerNames[path] = name
}



// Attach creates an RPC client attached to an in-process API handler.
// TODO: 외부 프로세스가 아닌, 노드 내부의 다른 구성요소가 노드와 통신할 수 있도록 RPC 클라이언트를 생성하고 리턴하는 함수
func (n *Node) Attach() *rpc.Client {
	return rpc.DialInProc(n.inprocHandler)
}


// RPCHandler returns the in-process RPC request handler.
// TODO: 객체의 내부 RPC 요청 핸들러를 반환하는 함수		 inprocHandler (= RPC 서버 객체) 리턴
func (n *Node) RPCHandler() (*rpc.Server, error) {
	n.lock.Lock()
	defer n.lock.Unlock()

	if n.state == closedState {
		return nil, ErrNodeStopped
	}
	return n.inprocHandler, nil
}


// Config returns the configuration of node.
// 노드의 config 설정파일을 리턴하는 함수
func (n *Node) Config() *Config {
	return n.config
}



// Server retrieves the currently running P2P network layer. This method is meant
// only to inspect fields of the currently running server. Callers should not
// start or stop the returned server.
// 노드의 p2p 서버 객체를 리턴하는 함수
func (n *Node) Server() *p2p.Server {
	n.lock.Lock()
	defer n.lock.Unlock()

	return n.server
}



// DataDir retrieves the current datadir used by the protocol stack.
// Deprecated: No files should be stored in this directory, use InstanceDir instead.
// 현재 노드가 사용하는 데이터 dir 경로를 리턴하는 함수
func (n *Node) DataDir() string {
	return n.config.DataDir
}



// InstanceDir retrieves the instance directory used by the protocol stack.
// InstanceDir은 여러 노드를 한 컴퓨터에서 실행할때 문제가 발생할 수 있는 dataDir을 발전시킨 것으로 노드 인스턴스마다 고유한 디렉토리 경로를 반환하는 함수
func (n *Node) InstanceDir() string {
	return n.config.instanceDir()
}


// KeyStoreDir retrieves the key directory
// keyStore 디렉토리 경로 반환 함수
func (n *Node) KeyStoreDir() string {
	return n.keyDir
}

// AccountManager retrieves the account manager used by the protocol stack.
// 노드가 사용하는 계정관리자 account Manager을 리턴하는 함수
func (n *Node) AccountManager() *accounts.Manager {
	return n.accman
}

// IPCEndpoint retrieves the current IPC endpoint used by the protocol stack.
// IPC (inter-process communication) 통신에 사용되는 엔드포인트 주소를 문자열로 반환하는 함수
func (n *Node) IPCEndpoint() string {
	return n.ipc.endpoint
}

// HTTPEndpoint returns the URL of the HTTP server. Note that this URL does not
// contain the JSON-RPC path prefix set by HTTPPathPrefix.
// HTTP 통신에 사용되는 앤드포인트 주소를 반환하는 함수
func (n *Node) HTTPEndpoint() string {
	return "http://" + n.http.listenAddr()
}

// WSEndpoint returns the current JSON-RPC over WebSocket endpoint.
// webSocket 엔드포인트 주소를 반환하는 함수
func (n *Node) WSEndpoint() string {
	if n.http.wsAllowed() {
		return "ws://" + n.http.listenAddr() + n.http.wsConfig.prefix
	}
	return "ws://" + n.ws.listenAddr() + n.ws.wsConfig.prefix
}

// HTTPAuthEndpoint returns the URL of the authenticated HTTP server.
// 인증이 필요한 HTTP서버의 주소를 반환하는 함수			* ex dev 전용    노드관리 계정잠금 등 민감한 작업
func (n *Node) HTTPAuthEndpoint() string {
	return "http://" + n.httpAuth.listenAddr()
}

// WSAuthEndpoint returns the current authenticated JSON-RPC over WebSocket endpoint.
// 인증이 필요한 websocket의 주소를 반환하는 함수
func (n *Node) WSAuthEndpoint() string {
	if n.httpAuth.wsAllowed() {
		return "ws://" + n.httpAuth.listenAddr() + n.httpAuth.wsConfig.prefix
	}
	return "ws://" + n.wsAuth.listenAddr() + n.wsAuth.wsConfig.prefix
}



// EventMux retrieves the event multiplexer used by all the network services in
// the current protocol stack.
// eventmux를 리턴하는 함수		*eventmux는 여러 이벤트가 발생하면 이를 저장해두는 곳   1:n 구조로 내부 모듈과 통신에 용이
func (n *Node) EventMux() *event.TypeMux {
	return n.eventmux
}



// OpenDatabase opens an existing database with the given name (or creates one if no
// previous can be found) from within the node's instance directory. If the node has no
// data directory, an in-memory database is returned.
// TODO: 데이터베이스를 열거나 생성하는 함수    (name으로 저장된 db를 리턴 또는 이름의 db없으면 새로 생성함)
func (n *Node) OpenDatabaseWithOptions(name string, opt DatabaseOptions) (ethdb.Database, error) {
	n.lock.Lock()
	defer n.lock.Unlock()

	// 노드가 닫힌상태면 그냥 종료
	if n.state == closedState {
		return nil, ErrNodeStopped
	}
	var db ethdb.Database
	var err error

	// config의 db경로를 설정하지 않았다면 또는 db설정했다면
	if n.config.DataDir == "" {
		db, _ = rawdb.Open(memorydb.New(), rawdb.OpenOptions{
			MetricsNamespace: opt.MetricsNamespace,
			ReadOnly:         opt.ReadOnly,
		})
	} else {
		opt.AncientsDirectory = n.ResolveAncient(name, opt.AncientsDirectory)
		db, err = openDatabase(internalOpenOptions{
			directory:       n.ResolvePath(name),
			dbEngine:        n.config.DBEngine,
			DatabaseOptions: opt,
		})
	}
	if err == nil {
		db = n.wrapDatabase(db)
	}
	return db, err
}



// OpenDatabase opens an existing database with the given name (or creates one if no
// previous can be found) from within the node's instance directory.
// If the node has no data directory, an in-memory database is returned.
// Deprecated: use OpenDatabaseWithOptions instead.
// 현재 사용안되는 db여는 함수
func (n *Node) OpenDatabase(name string, cache, handles int, namespace string, readonly bool) (ethdb.Database, error) {
	return n.OpenDatabaseWithOptions(name, DatabaseOptions{
		MetricsNamespace: namespace,
		Cache:            cache,
		Handles:          handles,
		ReadOnly:         readonly,
	})
}



// OpenDatabaseWithFreezer opens an existing database with the given name (or
// creates one if no previous can be found) from within the node's data directory.
// If the node has no data directory, an in-memory database is returned.
// Deprecated: use OpenDatabaseWithOptions instead.
// 현재사용안되는 db여는 함수
func (n *Node) OpenDatabaseWithFreezer(name string, cache, handles int, ancient string, namespace string, readonly bool) (ethdb.Database, error) {
	return n.OpenDatabaseWithOptions(name, DatabaseOptions{
		AncientsDirectory: n.ResolveAncient(name, ancient),
		MetricsNamespace:  namespace,
		Cache:             cache,
		Handles:           handles,
		ReadOnly:          readonly,
	})
}



// ResolvePath returns the absolute path of a resource in the instance directory.
// 노드 객체 디렉터리 내에 있는 리소스의 절대경로를 리턴하는 함수이다.
func (n *Node) ResolvePath(x string) string {
	return n.config.ResolvePath(x)
}



// ResolveAncient returns the absolute path of the root ancient directory.
// ancient 디렉토리의 최종 절대 경로를 결정하는 함수	(ancient == old blocks, old states...)
func (n *Node) ResolveAncient(name string, ancient string) string {
	switch {
	case ancient == "":
		ancient = filepath.Join(n.ResolvePath(name), "ancient")
	case !filepath.IsAbs(ancient):
		ancient = n.ResolvePath(ancient)
	}
	return ancient
}




// TODO: 2중 db 닫기를 막기위한 과정들 
// closeTrackingDB wraps the Close method of a database. When the database is closed by the
// service, the wrapper removes it from the node's database map. This ensures that Node
// won't auto-close the database if it is closed by the service that opened it.
type closeTrackingDB struct {
	ethdb.Database
	n *Node
}

func (db *closeTrackingDB) Close() error {
	db.n.lock.Lock()
	delete(db.n.databases, db)
	db.n.lock.Unlock()
	return db.Database.Close()
}

// wrapDatabase ensures the database will be auto-closed when Node is closed.
func (n *Node) wrapDatabase(db ethdb.Database) ethdb.Database {
	wrapper := &closeTrackingDB{db, n}
	n.databases[wrapper] = struct{}{}
	return wrapper
}

// closeDatabases closes all open databases.
func (n *Node) closeDatabases() (errors []error) {
	for db := range n.databases {
		delete(n.databases, db)
		if err := db.Database.Close(); err != nil {
			errors = append(errors, err)
		}
	}
	return errors
}
