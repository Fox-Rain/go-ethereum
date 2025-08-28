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

/*
	TODO: Ethereum Virtual Machine으로 StateProcessor가 트랜잭션을 실행하라는 명령을 내리면 이 파일에 정의된
		  EVM객체가 실제 계산을 수행한다.

	EVM 구조체는 다음과 같이 구성된다.
	BlockContext, TXContext, StateDB (interface), 체인과 EVM관련된 설정,  Jump캐싱도구, precompile contract 목록
	그리고 실질적인 OPcode계산을 수행하는 EVMinterpreter 객체로 구성된다.

	또한 call, delegatecall, staticcall과 OPcode func도 구현되어있다.
*/

package vm

import (
	"errors"
	"math/big"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

type (
	// CanTransferFunc is the signature of a transfer guard function
	CanTransferFunc func(StateDB, common.Address, *uint256.Int) bool
	// TransferFunc is the signature of a transfer function
	TransferFunc func(StateDB, common.Address, common.Address, *uint256.Int)
	// GetHashFunc returns the n'th block hash in the blockchain
	// and is used by the BLOCKHASH EVM op code.
	GetHashFunc func(uint64) common.Hash
)

// TODO: 인자로 받은 주소 addr이 precompile address인지 확인하고 맞다면 해당 컨트랙트의 구현체를 반환한다.
// TODO: [Precompile Contract] : 일반적인 EVM바이트코드가 아닌 네이티브언어로 직접 구현되어서 내장된 고성능 컨트랙트
func (evm *EVM) precompile(addr common.Address) (PrecompiledContract, bool) {
	p, ok := evm.precompiles[addr]
	return p, ok
}

// BlockContext provides the EVM with auxiliary information. Once provided
// it shouldn't be modified.
// TODO: BlockContext Structor  :   EVM이 tx를 실행하는 데 필요한 모든 외부 '환경정보'를 담아 전달하는 데이터 컨테이너
// TODO: EVM은 가상 격리 환경이므로, 실행에 필요한 필수적인 정보를 주입해줘야한다. 이런 역할을 하는 구조체가 바로 이것
type BlockContext struct {
	// CanTransfer returns whether the account contains
	// sufficient ether to transfer the value
	CanTransfer CanTransferFunc		// 특정 계정이 지정된 금액의 ETH가 충분한지 확인하는 함수
	// Transfer transfers ether from one account to the other
	Transfer TransferFunc			// 계정 -> 계정으로 ETH를 전송하는 함수
	// GetHash returns the hash corresponding to n
	GetHash GetHashFunc				// BlockHash OPcode가 실행될때, 특정 블록 번호에 해당하는 과거 블록의 해시를 반환하는 함수

	// Block information	* 현재 블록의 값들
	Coinbase    common.Address // Provides information for COINBASE
	GasLimit    uint64         // Provides information for GASLIMIT
	BlockNumber *big.Int       // Provides information for NUMBER
	Time        uint64         // Provides information for TIME
	Difficulty  *big.Int       // Provides information for DIFFICULTY
	BaseFee     *big.Int       // Provides information for BASEFEE (0 if vm runs with NoBaseFee flag and 0 gas price)
	BlobBaseFee *big.Int       // Provides information for BLOBBASEFEE (0 if vm runs with NoBaseFee flag and 0 blob gas price)
	Random      *common.Hash   // Provides information for PREVRANDAO
}

// TxContext provides the EVM with information about a transaction.
// All fields can change between transactions.
// TODO: 현재 실행중인 단일 tx에 대한 구체적인 정보를 EVM에 제공하는 데이터 컨테이너
type TxContext struct {
	// Message information
	Origin       common.Address      // Provides information for ORIGIN
	GasPrice     *big.Int            // Provides information for GASPRICE (and is used to zero the basefee if NoBaseFee is set)
	BlobHashes   []common.Hash       // Provides information for BLOBHASH
	BlobFeeCap   *big.Int            // Is used to zero the blobbasefee if NoBaseFee is set
	AccessEvents *state.AccessEvents // Capture all state accesses for this tx
}






// EVM is the Ethereum Virtual Machine base object and provides
// the necessary tools to run a contract on the given state with
// the provided context. It should be noted that any error
// generated through any of the calls should be considered a
// revert-state-and-consume-all-gas operation, no checks on
// specific errors should ever be performed. The interpreter makes
// sure that any errors generated are to be considered faulty code.
//
// The EVM should never be reused and is not thread safe.
// TODO: EVM 핵심 객체로	트랜잭션을 처리하는데 필요한 모든 상태, 설정, 내부도구들을 담고 있다. 
// TODO: EVM객체는 일회성으로 보통 트랜잭션을 하나 처리하기위해서 생성되었다가 사라진다. + "스레드로부터 안전하지 않으므로 병렬이 아닌 하나씩 처리되어야한다."
type EVM struct {
	// Context provides auxiliary blockchain related information
	Context BlockContext		// BlockContext, TxContext
	TxContext

	// StateDB gives access to the underlying state
	StateDB StateDB				// world State에 접근할 수 있는 interface

	// depth is the current call stack
	depth int					// 현재 컨트랙트의 호출 깊이 EX. 함수 재귀호출에 cnt

	// chainConfig contains information about the current chain
	chainConfig *params.ChainConfig		// 체인전체에 대한 설정

	// chain rules contains the chain rules for the current epoch
	chainRules params.Rules				// 체인규칙

	// virtual machine configuration options used to initialise the evm
	Config Config						// EVM관련 설정

	// global (to this context) ethereum virtual machine used throughout
	// the execution of the tx
	interpreter *EVMInterpreter			// TODO: EVM OPcode를 하나씩 해석하고 실행하는 interpreter 객체  (*실질적인 연산을 수행)

	// abort is used to abort the EVM calling operations
	abort atomic.Bool					// 외부에서 EVM을 즉시 중단시키기위한 신호 플래그

	// callGasTemp holds the gas available for the current call. This is needed because the
	// available gas is calculated in gasCall* according to the 63/64 rule and later
	// applied in opCall*.
	callGasTemp uint64					// TODO: CALL과 같은 OPcode를 실행할 떄, 하위 컨텍스트에 전달할 가스를 임시로 저장하는 변수

	// precompiles holds the precompiled contracts for the current epoch
	precompiles map[common.Address]PrecompiledContract	// TODO: 활성화된 precompile Contract 목록

	// jumpDests stores results of JUMPDEST analysis.
	jumpDests JumpDestCache			// TODO: Jump목적지를 캐싱하여서 성능최적화하는 도구
}







// NewEVM constructs an EVM instance with the supplied block context, state
// database and several configs. It meant to be used throughout the entire
// state transition of a block, with the transaction context switched as
// needed by calling evm.SetTxContext.
// TODO: 블록처리를 시작할 때 한번만 호출되는 EVM객체를 생성하고 초기화하는 생성자 함수
func NewEVM(blockCtx BlockContext, statedb StateDB, chainConfig *params.ChainConfig, config Config) *EVM {
	// 필드 할당
	evm := &EVM{
		Context:     blockCtx,
		StateDB:     statedb,
		Config:      config,
		chainConfig: chainConfig,
		chainRules:  chainConfig.Rules(blockCtx.BlockNumber, blockCtx.Random != nil, blockCtx.Time),
		jumpDests:   newMapJumpDests(),
	}

	evm.precompiles = activePrecompiledContracts(evm.chainRules)	// precompile contract 로드
	evm.interpreter = NewEVMInterpreter(evm)	// interpreter 객체 생성
	return evm	// evm 객체 리턴
}


// SetPrecompiles sets the precompiled contracts for the EVM.
// This method is only used through RPC calls.
// It is not thread-safe.
// TODO: EVM의 precompile객체를 통쨰로 교체하는 함수 (개발자 test용도)
func (evm *EVM) SetPrecompiles(precompiles PrecompiledContracts) {
	evm.precompiles = precompiles
}

// SetJumpDestCache configures the analysis cache.
// TODO: EVM의 JUMPDEST캐시를 교체하는 함수 (역시 개발자 테스트 용)
func (evm *EVM) SetJumpDestCache(jumpDests JumpDestCache) {
	evm.jumpDests = jumpDests
}


// SetTxContext resets the EVM with a new transaction context.
// This is not threadsafe and should only be done very cautiously.
// EVM의 TxContext를 교체하는 함수
func (evm *EVM) SetTxContext(txCtx TxContext) {
	if evm.chainRules.IsEIP4762 {
		txCtx.AccessEvents = state.NewAccessEvents(evm.StateDB.PointCache())
	}
	evm.TxContext = txCtx
}

// Cancel cancels any running EVM operation. This may be called concurrently and
// it's safe to be called multiple times.
// EVM 실행을 중단하라는 신호를 보내는 함수
func (evm *EVM) Cancel() {
	evm.abort.Store(true)
}

// Cancelled returns true if Cancel has been called
// EVM 살행이 중단되었는지 확인하는 함수
func (evm *EVM) Cancelled() bool {
	return evm.abort.Load()
}

// Interpreter returns the current interpreter
// EVMInterpreter를 반환하는 함수
func (evm *EVM) Interpreter() *EVMInterpreter {
	return evm.interpreter
}

// 특정 call의 발신자가 system인지 확인하는 함수  즉, systemcall인지 체크하는 함수
func isSystemCall(caller common.Address) bool {
	return caller == params.SystemAddress
}



// Call executes the contract associated with the addr with the given input as
// parameters. It also handles any necessary value transfer required and takse
// the necessary steps to create accounts and reverses the state in case of an
// execution error or failed value transfer.
// TODO: 특정 스마트컨트랙트를 호출하여 코드를 실행하고, 필요한 ETH 전송, 실행결과를 처리하는 모든 과정을 담당하는 함수
// TODO: EVM OPcode가 실행될 때 내부적으로 이 함수가 호출된다.
func (evm *EVM) Call(caller common.Address, addr common.Address, input []byte, gas uint64, value *uint256.Int) (ret []byte, leftOverGas uint64, err error) {
	// Capture the tracer start/end events in debug mode
	if evm.Config.Tracer != nil {
		evm.captureBegin(evm.depth, CALL, caller, addr, input, gas, value.ToBig())
		defer func(startGas uint64) {
			evm.captureEnd(evm.depth, startGas, leftOverGas, ret, err)
		}(gas)
	}
	// Fail if we're trying to execute above the call depth limit
	// TODO: 현재 depth (재귀호출한 횟수)가 1024(한도)를 초과하는지 확인한다. (무한 재귀 공격을 방지)
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth
	}
	// Fail if we're trying to transfer more than the available balance
	// TODO: ETH value가 0 보다 크다면 잔고가 충분한지 먼저 확인
	if !value.IsZero() && !evm.Context.CanTransfer(evm.StateDB, caller, value) {
		return nil, gas, ErrInsufficientBalance
	}

	snapshot := evm.StateDB.Snapshot()	// TODO: 현재 상태 데이터에 대한 스냅샷을 저장 (만약 에러가 발생시 되돌리기 위함.)
	p, isPrecompile := evm.precompile(addr)

	if !evm.StateDB.Exist(addr) {
		if !isPrecompile && evm.chainRules.IsEIP4762 && !isSystemCall(caller) {
			// Add proof of absence to witness
			// At this point, the read costs have already been charged, either because this
			// is a direct tx call, in which case it's covered by the intrinsic gas, or because
			// of a CALL instruction, in which case BASIC_DATA has been added to the access
			// list in write mode. If there is enough gas paying for the addition of the code
			// hash leaf to the access list, then account creation will proceed unimpaired.
			// Thus, only pay for the creation of the code hash leaf here.
			wgas := evm.AccessEvents.CodeHashGas(addr, true, gas, false)
			if gas < wgas {
				evm.StateDB.RevertToSnapshot(snapshot)
				return nil, 0, ErrOutOfGas
			}
			gas -= wgas
		}

		if !isPrecompile && evm.chainRules.IsEIP158 && value.IsZero() {
			// Calling a non-existing account, don't do anything.
			return nil, gas, nil
		}
		evm.StateDB.CreateAccount(addr)
	}

	// TODO: value에 담긴 ETH만큼 caller에서 addr 게정으로 전송하여 상태를 변경
	evm.Context.Transfer(evm.StateDB, caller, addr, value)

	// TODO: 컨트랙트 실행	 호출된 주소가 프리컴파일 컨트랙트인지 일반 컨트랙트인지에 따라 다른 로직을 수행
	if isPrecompile {	// EVM 사용 x   내장된 함수 사용
		ret, gas, err = RunPrecompiledContract(p, input, gas, evm.Config.Tracer)
	} else {	// EVM 사용
		// Initialise a new contract and set the code that is to be used by the EVM.
		code := evm.resolveCode(addr)	// addr에 해당하는 스마트컨트랙트 바이트코드를 DB에서 가져온다.
		if len(code) == 0 {
			ret, err = nil, nil // gas is unchanged
		} else {
			// The contract is a scoped environment for this execution context only.
			// TODO: 이번 호출만을 위한 임시 컨트랙트 객체를 만들고 인터프리터를 통해서 바이트코드를 실행시킨다.
			contract := NewContract(caller, addr, value, gas, evm.jumpDests)
			contract.IsSystemCall = isSystemCall(caller)
			contract.SetCallCode(evm.resolveCodeHash(addr), code)
			ret, err = evm.interpreter.Run(contract, input, false)
			gas = contract.Gas	// 끝난뒤 남은 가스를 업데이트
		}
	}
	// When an error was returned by the EVM or when setting the creation code
	// above we revert to the snapshot and consume any gas remaining. Additionally,
	// when we're in homestead this also counts for code storage gas errors.
	// TODO: 에러 발생시 스냅샷으로 되돌린다. + 가스처리
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != ErrExecutionReverted {
			if evm.Config.Tracer != nil && evm.Config.Tracer.OnGasChange != nil {
				evm.Config.Tracer.OnGasChange(gas, 0, tracing.GasChangeCallFailedExecution)
			}

			gas = 0
		}
		// TODO: consider clearing up unused snapshots:
		//} else {
		//	evm.StateDB.DiscardSnapshot(snapshot)
	}
	return ret, gas, err	// 최종 결과 반환
}






// CallCode executes the contract associated with the addr with the given input
// as parameters. It also handles any necessary value transfer required and takes
// the necessary steps to create accounts and reverses the state in case of an
// execution error or failed value transfer.
//
// CallCode differs from Call in the sense that it executes the given address'
// code with the caller as context.
// 현재 사용안되는 코드 DelegateCall 사용
func (evm *EVM) CallCode(caller common.Address, addr common.Address, input []byte, gas uint64, value *uint256.Int) (ret []byte, leftOverGas uint64, err error) {
	// Invoke tracer hooks that signal entering/exiting a call frame
	if evm.Config.Tracer != nil {
		evm.captureBegin(evm.depth, CALLCODE, caller, addr, input, gas, value.ToBig())
		defer func(startGas uint64) {
			evm.captureEnd(evm.depth, startGas, leftOverGas, ret, err)
		}(gas)
	}
	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth
	}
	// Fail if we're trying to transfer more than the available balance
	// Note although it's noop to transfer X ether to caller itself. But
	// if caller doesn't have enough balance, it would be an error to allow
	// over-charging itself. So the check here is necessary.
	if !evm.Context.CanTransfer(evm.StateDB, caller, value) {
		return nil, gas, ErrInsufficientBalance
	}
	var snapshot = evm.StateDB.Snapshot()

	// It is allowed to call precompiles, even via delegatecall
	if p, isPrecompile := evm.precompile(addr); isPrecompile {
		ret, gas, err = RunPrecompiledContract(p, input, gas, evm.Config.Tracer)
	} else {
		// Initialise a new contract and set the code that is to be used by the EVM.
		// The contract is a scoped environment for this execution context only.
		contract := NewContract(caller, caller, value, gas, evm.jumpDests)
		contract.SetCallCode(evm.resolveCodeHash(addr), evm.resolveCode(addr))
		ret, err = evm.interpreter.Run(contract, input, false)
		gas = contract.Gas
	}
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != ErrExecutionReverted {
			if evm.Config.Tracer != nil && evm.Config.Tracer.OnGasChange != nil {
				evm.Config.Tracer.OnGasChange(gas, 0, tracing.GasChangeCallFailedExecution)
			}
			gas = 0
		}
	}
	return ret, gas, err
}


// DelegateCall executes the contract associated with the addr with the given input
// as parameters. It reverses the state in case of an execution error.
//
// DelegateCall differs from CallCode in the sense that it executes the given address'
// code with the caller as context and the caller is set to the caller of the caller.
// TODO: DelegateCall    addr주소에있는 컨트랙트 코드를 실행하되, 모든 상태변경은 caller의 context에서 실행되도록하는 호출방식 함수
// TODO: 즉 call한 함수자체를 call한 주체에게 복사붙여넣기 해서 수행하는 것 처럼 동작한다.
func (evm *EVM) DelegateCall(originCaller common.Address, caller common.Address, addr common.Address, input []byte, gas uint64, value *uint256.Int) (ret []byte, leftOverGas uint64, err error) {
	// Invoke tracer hooks that signal entering/exiting a call frame
	if evm.Config.Tracer != nil {
		// DELEGATECALL inherits value from parent call
		evm.captureBegin(evm.depth, DELEGATECALL, caller, addr, input, gas, value.ToBig())
		defer func(startGas uint64) {
			evm.captureEnd(evm.depth, startGas, leftOverGas, ret, err)
		}(gas)
	}
	// Fail if we're trying to execute above the call depth limit
	// 깊이 확인 (재귀스택)
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth
	}
	var snapshot = evm.StateDB.Snapshot()	// 에러시 상태를 되돌리기 위한 스냅샷

	// It is allowed to call precompiles, even via delegatecall
	if p, isPrecompile := evm.precompile(addr); isPrecompile {
		ret, gas, err = RunPrecompiledContract(p, input, gas, evm.Config.Tracer)
	} else {
		// Initialise a new contract and make initialise the delegate values
		//
		// Note: The value refers to the original value from the parent call.
		contract := NewContract(originCaller, caller, value, gas, evm.jumpDests)	// TODO: NewContract를 호출할떄, 첫번째 인자로 caller가 아닌 originCaller로 전달한다.
		contract.SetCallCode(evm.resolveCodeHash(addr), evm.resolveCode(addr))
		ret, err = evm.interpreter.Run(contract, input, false)
		gas = contract.Gas
	}
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != ErrExecutionReverted {
			if evm.Config.Tracer != nil && evm.Config.Tracer.OnGasChange != nil {
				evm.Config.Tracer.OnGasChange(gas, 0, tracing.GasChangeCallFailedExecution)
			}
			gas = 0
		}
	}
	return ret, gas, err
}



// StaticCall executes the contract associated with the addr with the given input
// as parameters while disallowing any modifications to the state during the call.
// Opcodes that attempt to perform such modifications will result in exceptions
// instead of performing the modifications.
// TODO: addr주소의 컨트랙트코드를 실행하되, 실행중에 블록체인의 상태변경하려는 모든 시도는 금지하는 읽기전용 모드 함수
func (evm *EVM) StaticCall(caller common.Address, addr common.Address, input []byte, gas uint64) (ret []byte, leftOverGas uint64, err error) {
	// Invoke tracer hooks that signal entering/exiting a call frame
	if evm.Config.Tracer != nil {
		evm.captureBegin(evm.depth, STATICCALL, caller, addr, input, gas, nil)
		defer func(startGas uint64) {
			evm.captureEnd(evm.depth, startGas, leftOverGas, ret, err)
		}(gas)
	}
	// Fail if we're trying to execute above the call depth limit
	// depth 확인
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth
	}
	// We take a snapshot here. This is a bit counter-intuitive, and could probably be skipped.
	// However, even a staticcall is considered a 'touch'. On mainnet, static calls were introduced
	// after all empty accounts were deleted, so this is not required. However, if we omit this,
	// then certain tests start failing; stRevertTest/RevertPrecompiledTouchExactOOG.json.
	// We could change this, but for now it's left for legacy reasons
	var snapshot = evm.StateDB.Snapshot()	// 예러시 복구를 위한 snapshot

	// We do an AddBalance of zero here, just in order to trigger a touch.
	// This doesn't matter on Mainnet, where all empties are gone at the time of Byzantium,
	// but is the correct thing to do and matters on other networks, in tests, and potential
	// future scenarios
	evm.StateDB.AddBalance(addr, new(uint256.Int), tracing.BalanceChangeTouchAccount)

	if p, isPrecompile := evm.precompile(addr); isPrecompile {
		ret, gas, err = RunPrecompiledContract(p, input, gas, evm.Config.Tracer)
	} else {
		// Initialise a new contract and set the code that is to be used by the EVM.
		// The contract is a scoped environment for this execution context only.
		contract := NewContract(caller, addr, new(uint256.Int), gas, evm.jumpDests)
		contract.SetCallCode(evm.resolveCodeHash(addr), evm.resolveCode(addr))

		// When an error was returned by the EVM or when setting the creation code
		// above we revert to the snapshot and consume any gas remaining. Additionally
		// when we're in Homestead this also counts for code storage gas errors.
		ret, err = evm.interpreter.Run(contract, input, true)	// TODO: 마지막 인자가 true == 인터프리터에게 읽기전용을 알리는 것
		gas = contract.Gas
	}
	// TODO: 변경을하려거나 에러가 발생시 스냅샷으로 되돌린다.
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != ErrExecutionReverted {
			if evm.Config.Tracer != nil && evm.Config.Tracer.OnGasChange != nil {
				evm.Config.Tracer.OnGasChange(gas, 0, tracing.GasChangeCallFailedExecution)
			}

			gas = 0
		}
	}
	return ret, gas, err
}



// create creates a new contract using code as deployment code.
// TODO: 새로운 스마트컨트랙트를 배포(생성)하는 함수
func (evm *EVM) create(caller common.Address, code []byte, gas uint64, value *uint256.Int, address common.Address, typ OpCode) (ret []byte, createAddress common.Address, leftOverGas uint64, err error) {
	if evm.Config.Tracer != nil {
		evm.captureBegin(evm.depth, typ, caller, address, code, gas, value.ToBig())
		defer func(startGas uint64) {
			evm.captureEnd(evm.depth, startGas, leftOverGas, ret, err)
		}(gas)
	}
	// Depth check execution. Fail if we're trying to execute above the
	// depth 확인
	if evm.depth > int(params.CallCreateDepth) {
		return nil, common.Address{}, gas, ErrDepth
	}
	if !evm.Context.CanTransfer(evm.StateDB, caller, value) {
		return nil, common.Address{}, gas, ErrInsufficientBalance
	}
	nonce := evm.StateDB.GetNonce(caller)
	if nonce+1 < nonce {
		return nil, common.Address{}, gas, ErrNonceUintOverflow
	}
	evm.StateDB.SetNonce(caller, nonce+1, tracing.NonceChangeContractCreator)

	// Charge the contract creation init gas in verkle mode
	if evm.chainRules.IsEIP4762 {
		statelessGas := evm.AccessEvents.ContractCreatePreCheckGas(address, gas)
		if statelessGas > gas {
			return nil, common.Address{}, 0, ErrOutOfGas
		}
		if evm.Config.Tracer != nil && evm.Config.Tracer.OnGasChange != nil {
			evm.Config.Tracer.OnGasChange(gas, gas-statelessGas, tracing.GasChangeWitnessContractCollisionCheck)
		}
		gas = gas - statelessGas
	}

	// We add this to the access list _before_ taking a snapshot. Even if the
	// creation fails, the access-list change should not be rolled back.
	if evm.chainRules.IsEIP2929 {
		evm.StateDB.AddAddressToAccessList(address)
	}
	// Ensure there's no existing contract already at the designated address.
	// Account is regarded as existent if any of these three conditions is met:
	// - the nonce is non-zero
	// - the code is non-empty
	// - the storage is non-empty
	contractHash := evm.StateDB.GetCodeHash(address)
	storageRoot := evm.StateDB.GetStorageRoot(address)
	if evm.StateDB.GetNonce(address) != 0 ||
		(contractHash != (common.Hash{}) && contractHash != types.EmptyCodeHash) || // non-empty code
		(storageRoot != (common.Hash{}) && storageRoot != types.EmptyRootHash) { // non-empty storage
		if evm.Config.Tracer != nil && evm.Config.Tracer.OnGasChange != nil {
			evm.Config.Tracer.OnGasChange(gas, 0, tracing.GasChangeCallFailedExecution)
		}
		return nil, common.Address{}, 0, ErrContractAddressCollision
	}
	// Create a new account on the state only if the object was not present.
	// It might be possible the contract code is deployed to a pre-existent
	// account with non-zero balance.
	snapshot := evm.StateDB.Snapshot()
	if !evm.StateDB.Exist(address) {
		evm.StateDB.CreateAccount(address)
	}
	// CreateContract means that regardless of whether the account previously existed
	// in the state trie or not, it _now_ becomes created as a _contract_ account.
	// This is performed _prior_ to executing the initcode,  since the initcode
	// acts inside that account.
	evm.StateDB.CreateContract(address)

	if evm.chainRules.IsEIP158 {
		evm.StateDB.SetNonce(address, 1, tracing.NonceChangeNewContract)
	}
	// Charge the contract creation init gas in verkle mode
	if evm.chainRules.IsEIP4762 {
		consumed, wanted := evm.AccessEvents.ContractCreateInitGas(address, gas)
		if consumed < wanted {
			return nil, common.Address{}, 0, ErrOutOfGas
		}
		if evm.Config.Tracer != nil && evm.Config.Tracer.OnGasChange != nil {
			evm.Config.Tracer.OnGasChange(gas, gas-consumed, tracing.GasChangeWitnessContractInit)
		}
		gas = gas - consumed
	}
	evm.Context.Transfer(evm.StateDB, caller, address, value)

	// Initialise a new contract and set the code that is to be used by the EVM.
	// The contract is a scoped environment for this execution context only.
	contract := NewContract(caller, address, value, gas, evm.jumpDests)

	// Explicitly set the code to a null hash to prevent caching of jump analysis
	// for the initialization code.
	contract.SetCallCode(common.Hash{}, code)
	contract.IsDeployment = true

	ret, err = evm.initNewContract(contract, address)
	if err != nil && (evm.chainRules.IsHomestead || err != ErrCodeStoreOutOfGas) {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != ErrExecutionReverted {
			contract.UseGas(contract.Gas, evm.Config.Tracer, tracing.GasChangeCallFailedExecution)
		}
	}
	return ret, address, contract.Gas, err
}




// initNewContract runs a new contract's creation code, performs checks on the
// resulting code that is to be deployed, and consumes necessary gas.
// TODO: EVM.create내부에서 호출되는 함수로, initcode를 실행하고, 그 결과로 나온 runtimecode를 검증한뒤, DB에 저장하는 함수
func (evm *EVM) initNewContract(contract *Contract, address common.Address) ([]byte, error) {
	ret, err := evm.interpreter.Run(contract, nil, false)	// intertpreter를 통해서, contract의 생성코드를 실행 -> ret에 runtime code가 담기게된다.
	if err != nil {
		return ret, err
	}

	// Check whether the max code size has been exceeded, assign err if the case.
	// TODO: Runtime Code 검증
	if evm.chainRules.IsEIP158 && len(ret) > params.MaxCodeSize {	// 배포될 코드의 크기가 제한을 초과하는지 확인
		return ret, ErrMaxCodeSizeExceeded
	}

	// Reject code starting with 0xEF if EIP-3541 is enabled.
	if len(ret) >= 1 && ret[0] == 0xEF && evm.chainRules.IsLondon {		// 0xEF바이트로 시작하지 않는지 확인
		return ret, ErrInvalidCode
	}

	// 코드저장에 필요한 가스비 계산 + 차감
	if !evm.chainRules.IsEIP4762 {
		createDataGas := uint64(len(ret)) * params.CreateDataGas
		if !contract.UseGas(createDataGas, evm.Config.Tracer, tracing.GasChangeCallCodeStorage) {
			return ret, ErrCodeStoreOutOfGas
		}
	} else {
		consumed, wanted := evm.AccessEvents.CodeChunksRangeGas(address, 0, uint64(len(ret)), uint64(len(ret)), true, contract.Gas)
		contract.UseGas(consumed, evm.Config.Tracer, tracing.GasChangeWitnessCodeChunk)
		if len(ret) > 0 && (consumed < wanted) {
			return ret, ErrCodeStoreOutOfGas
		}
	}

	evm.StateDB.SetCode(address, ret)	// 최종 코드를 stateDB에 저장한다.
	return ret, nil
}


// Create creates a new contract using code as deployment code.
// TODO: 컨트랙트 주소를 계산하여 만들어준뒤 + 위의 EVM.create함수에게 실제 생성 작업은 위임하는 함수
func (evm *EVM) Create(caller common.Address, code []byte, gas uint64, value *uint256.Int) (ret []byte, contractAddr common.Address, leftOverGas uint64, err error) {
	contractAddr = crypto.CreateAddress(caller, evm.StateDB.GetNonce(caller))
	return evm.create(caller, code, gas, value, contractAddr, CREATE)
}

// Create2 creates a new contract using code as deployment code.
//
// The different between Create2 with Create is Create2 uses keccak256(0xff ++ msg.sender ++ salt ++ keccak256(init_code))[12:]
// instead of the usual sender-and-nonce-hash as the address where the contract is initialized at.
// TODO: 미리예측이 가능한 주소에 새로운 스마트컨트랙트를 배포하는 생성 함수  [EIP-1014	]
func (evm *EVM) Create2(caller common.Address, code []byte, gas uint64, endowment *uint256.Int, salt *uint256.Int) (ret []byte, contractAddr common.Address, leftOverGas uint64, err error) {
	inithash := crypto.HashData(evm.interpreter.hasher, code)	// 생성코드 해시 계산
	contractAddr = crypto.CreateAddress2(caller, salt.Bytes32(), inithash[:])	// addr 생성
	return evm.create(caller, code, gas, endowment, contractAddr, CREATE2)		// EVM.create에게 생성(배포)과정은 위임
}



// resolveCode returns the code associated with the provided account. After
// Prague, it can also resolve code pointed to by a delegation designator.
// TODO: 주어진 주소 addr에 해당하는 EVM bytecode를 가져오는 함수
func (evm *EVM) resolveCode(addr common.Address) []byte {
	code := evm.StateDB.GetCode(addr)
	if !evm.chainRules.IsPrague {
		return code
	}
	if target, ok := types.ParseDelegation(code); ok {
		// Note we only follow one level of delegation.
		return evm.StateDB.GetCode(target)
	}
	return code
}



// resolveCodeHash returns the code hash associated with the provided address.
// After Prague, it can also resolve code hash of the account pointed to by a
// delegation designator. Although this is not accessible in the EVM it is used
// internally to associate jumpdest analysis to code.
// TODO: 주어진 주소 addr의 코드의 해시를 반환하는 함수
func (evm *EVM) resolveCodeHash(addr common.Address) common.Hash {
	if evm.chainRules.IsPrague {
		code := evm.StateDB.GetCode(addr)
		if target, ok := types.ParseDelegation(code); ok {
			// Note we only follow one level of delegation.
			return evm.StateDB.GetCodeHash(target)
		}
	}
	return evm.StateDB.GetCodeHash(addr)
}







// ChainConfig returns the environment's chain configuration
func (evm *EVM) ChainConfig() *params.ChainConfig { return evm.chainConfig }

// EVM 작업이 시작되기전의 상태를 기록하는 함수
func (evm *EVM) captureBegin(depth int, typ OpCode, from common.Address, to common.Address, input []byte, startGas uint64, value *big.Int) {
	tracer := evm.Config.Tracer
	if tracer.OnEnter != nil {
		tracer.OnEnter(depth, byte(typ), from, to, input, startGas, value)
	}
	if tracer.OnGasChange != nil {
		tracer.OnGasChange(0, startGas, tracing.GasChangeCallInitialBalance)
	}
}

// EVM 작업이 모두 끝난 후 상태를 기록하는 함수
func (evm *EVM) captureEnd(depth int, startGas uint64, leftOverGas uint64, ret []byte, err error) {
	tracer := evm.Config.Tracer
	if leftOverGas != 0 && tracer.OnGasChange != nil {
		tracer.OnGasChange(leftOverGas, 0, tracing.GasChangeCallLeftOverReturned)
	}
	var reverted bool
	if err != nil {
		reverted = true
	}
	if !evm.chainRules.IsHomestead && errors.Is(err, ErrCodeStoreOutOfGas) {
		reverted = false
	}
	if tracer.OnExit != nil {
		tracer.OnExit(depth, ret, startGas-leftOverGas, VMErrorFromErr(err), reverted)
	}
}



// GetVMContext provides context about the block being executed as well as state
// to the tracers.
// 추적에 필요한 핵심정보들을 모아서 WMContext라는 객체로 만들어 전달하는 함수
func (evm *EVM) GetVMContext() *tracing.VMContext {
	return &tracing.VMContext{
		Coinbase:    evm.Context.Coinbase,
		BlockNumber: evm.Context.BlockNumber,
		Time:        evm.Context.Time,
		Random:      evm.Context.Random,
		BaseFee:     evm.Context.BaseFee,
		StateDB:     evm.StateDB,
	}
}
