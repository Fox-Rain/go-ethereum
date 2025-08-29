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
	TODO: EVM Interpreter의 핵심 구현체로, 모든 Opcode를 포함하여 바이트코드를 순서대로 읽고 실행하는 메인루프를 담고있다.


*/

package vm

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/holiman/uint256"
)

// Config are the configuration options for the Interpreter
// TODO: 인터프리터 설정
type Config struct {
	Tracer                  *tracing.Hooks
	NoBaseFee               bool  // Forces the EIP-1559 baseFee to 0 (needed for 0 price calls)
	EnablePreimageRecording bool  // Enables recording of SHA3/keccak preimages
	ExtraEips               []int // Additional EIPS that are to be enabled

	StatelessSelfValidation bool // Generate execution witnesses and self-check against them (testing purpose)
}

// ScopeContext contains the things that are per-call, such as stack and memory,
// but not transients like pc and gas
// TODO: 단일 컨트랙트 호출 (call) 동안 유지되는 작업환경을 담고있는 컨테이너
type ScopeContext struct {
	Memory   *Memory		// 해당 호출이 사용되는 임시 메모리 공간
	Stack    *Stack			// 해당 호출에서 사용되는 스택
	Contract *Contract		// 현재 실행중인 컨트랙트 객체에 대한 참조
}


// MemoryData returns the underlying memory slice. Callers must not modify the contents
// of the returned data.
// 현재실행 스코프의 메모리에 데이터를 리턴하는 함수
func (ctx *ScopeContext) MemoryData() []byte {
	if ctx.Memory == nil {
		return nil
	}
	return ctx.Memory.Data()
}

// StackData returns the stack data. Callers must not modify the contents
// of the returned data.
// 현재실행 스코프의 스택에 데이터를 리턴하는 함수
func (ctx *ScopeContext) StackData() []uint256.Int {
	if ctx.Stack == nil {
		return nil
	}
	return ctx.Stack.Data()
}

// Caller returns the current caller.
// 현재 실행중인 컨트랙트의 caller 리턴함수
func (ctx *ScopeContext) Caller() common.Address {
	return ctx.Contract.Caller()
}

// Address returns the address where this scope of execution is taking place.
// 현재 컨트랙트 주소 리턴함수
func (ctx *ScopeContext) Address() common.Address {
	return ctx.Contract.Address()
}

// CallValue returns the value supplied with this call.
// 이번호출과 함께 전송된 이더양 리턴함수
func (ctx *ScopeContext) CallValue() *uint256.Int {
	return ctx.Contract.Value()
}

// CallInput returns the input/calldata with this call. Callers must not modify
// the contents of the returned data.
// 현재 컨트랙트호출에 사용된 입력데이터를 리턴함수
func (ctx *ScopeContext) CallInput() []byte {
	return ctx.Contract.Input
}

// ContractCode returns the code of the contract being executed.
// 현재 컨트랙트의 EVM bytecode를 리턴하는 함수
func (ctx *ScopeContext) ContractCode() []byte {
	return ctx.Contract.Code
}






// EVMInterpreter represents an EVM interpreter
// TODO: EVM Interpreter 핵심 구조체
type EVMInterpreter struct {
	evm   *EVM			// EVM객체 포인터 (call, delegatecall과 같은 opcode는 evm을 통해 수행)
	table *JumpTable	// TODO: OPcode와 실제 로직을 연결하는 점프 테이블

	// TODO: SHA3(KECCAK256) OPcode를 실행할 때 사용되는 해시 계산기 객체와 결과 버퍼
	hasher    crypto.KeccakState // Keccak256 hasher instance shared across opcodes
	hasherBuf common.Hash        // Keccak256 hasher result array shared across opcodes

	readOnly   bool   // Whether to throw on stateful modifications    읽기전용인지 플래그
	returnData []byte // Last CALL's return data for subsequent reuse	TODO: sub-call의 반환데이터를 임시저장하는 공간
}



// NewEVMInterpreter returns a new instance of the Interpreter.
// TODO: EVM Interpreter 생성하는 함수
func NewEVMInterpreter(evm *EVM) *EVMInterpreter {
	// If jump table was not initialised we set the default one.
	// TODO: evm.chainRules를 확인하여, 현재 실행중인 블록이 어떤 하드포크를 따르는지 판단 + 해당 하드포크에 사용되는 OPcode를 담는 JumpTable을 할당
	var table *JumpTable
	switch {
	case evm.chainRules.IsOsaka:
		table = &osakaInstructionSet
	case evm.chainRules.IsVerkle:
		// TODO replace with proper instruction set when fork is specified
		table = &verkleInstructionSet
	case evm.chainRules.IsPrague:
		table = &pragueInstructionSet
	case evm.chainRules.IsCancun:
		table = &cancunInstructionSet
	case evm.chainRules.IsShanghai:
		table = &shanghaiInstructionSet
	case evm.chainRules.IsMerge:
		table = &mergeInstructionSet
	case evm.chainRules.IsLondon:
		table = &londonInstructionSet
	case evm.chainRules.IsBerlin:
		table = &berlinInstructionSet
	case evm.chainRules.IsIstanbul:
		table = &istanbulInstructionSet
	case evm.chainRules.IsConstantinople:
		table = &constantinopleInstructionSet
	case evm.chainRules.IsByzantium:
		table = &byzantiumInstructionSet
	case evm.chainRules.IsEIP158:
		table = &spuriousDragonInstructionSet
	case evm.chainRules.IsEIP150:
		table = &tangerineWhistleInstructionSet
	case evm.chainRules.IsHomestead:
		table = &homesteadInstructionSet
	default:
		table = &frontierInstructionSet
	}
	var extraEips []int
	if len(evm.Config.ExtraEips) > 0 {
		// Deep-copy jumptable to prevent modification of opcodes in other tables
		table = copyJumpTable(table)
	}
	// 추가적인 EIp있다면 활성화
	for _, eip := range evm.Config.ExtraEips {
		if err := EnableEIP(eip, table); err != nil {
			// Disable it, so caller can check if it's activated or not
			log.Error("EIP activation failed", "eip", eip, "error", err)
		} else {
			extraEips = append(extraEips, eip)
		}
	}
	evm.Config.ExtraEips = extraEips	// evmInterpreter 객체 생성
	return &EVMInterpreter{evm: evm, table: table, hasher: crypto.NewKeccakState()}
}

// Run loops and evaluates the contract's code with the given input data and returns
// the return byte-slice and an error if one occurred.
//
// It's important to note that any errors returned by the interpreter should be
// considered a revert-and-consume-all-gas operation except for
// ErrExecutionReverted which means revert-and-keep-gas-left.
func (in *EVMInterpreter) Run(contract *Contract, input []byte, readOnly bool) (ret []byte, err error) {
	// Increment the call depth which is restricted to 1024
	in.evm.depth++				// 컨트랙트 호출 깊이를 1 증가시킨다.
	defer func() { in.evm.depth-- }()		// defer를 통행서 함수가 종료될 떄, 1이 감소하도록 보장

	// Make sure the readOnly is only set if we aren't in readOnly yet.
	// This also makes sure that the readOnly flag isn't removed for child calls.
	// STATICCALL에 의한 readOnly일 경우 readOnly flag를 true로 설정
	if readOnly && !in.readOnly {
		in.readOnly = true
		defer func() { in.readOnly = false }()
	}

	// Reset the previous call's return data. It's unimportant to preserve the old buffer
	// as every returning call will return new data anyway.
	in.returnData = nil		// 혹시모를 이전 하위 호출 sub-call의 데이터가 남아있을 수 있으므로 지우기

	// Don't bother with the execution if there's no code.
	if len(contract.Code) == 0 {
		return nil, nil
	}


	// TODO: Local Variable 선언 	
	var (
		op          OpCode     				 // current opcode  현재 옵코드
		jumpTable   *JumpTable = in.table	 // 점프테이블
		mem                    = NewMemory() // bound memory  이번 호출에 사용될 메모리
		stack                  = newstack()  // local stack   이번 호출에 사용될 스택
		callContext            = &ScopeContext{
			Memory:   mem,
			Stack:    stack,
			Contract: contract,
		}
		// For optimisation reason we're using uint64 as the program counter.
		// It's theoretically possible to go above 2^64. The YP defines the PC
		// to be uint256. Practically much less so feasible.
		pc   = uint64(0) // program counter	 PC
		cost uint64
		// copies used by tracer
		pcCopy  uint64 // needed for the deferred EVMLogger
		gasCopy uint64 // for EVMLogger to log gas remaining before execution
		logged  bool   // deferred EVMLogger should ignore already logged steps
		res     []byte // result of the opcode execution function
		debug   = in.evm.Config.Tracer != nil
	)
	// Don't move this deferred function, it's placed before the OnOpcode-deferred method,
	// so that it gets executed _after_: the OnOpcode needs the stacks before
	// they are returned to the pools
	// TODO: 자원 반환 예약: 사용했던 스택, 메모리 객체를 재사용을위한 "풀"에 반환하도록 예약한다.   할당/해제에 드는 비용을 절약하기 위한 방식
	defer func() {
		returnStack(stack)
		mem.Free()
	}()
	contract.Input = input

	if debug {
		defer func() { // this deferred method handles exit-with-error
			if err == nil {
				return
			}
			if !logged && in.evm.Config.Tracer.OnOpcode != nil {
				in.evm.Config.Tracer.OnOpcode(pcCopy, byte(op), gasCopy, cost, callContext, in.returnData, in.evm.depth, VMErrorFromErr(err))
			}
			if logged && in.evm.Config.Tracer.OnFault != nil {
				in.evm.Config.Tracer.OnFault(pcCopy, byte(op), gasCopy, cost, callContext, in.evm.depth, VMErrorFromErr(err))
			}
		}()
	}
	// The Interpreter main run loop (contextual). This loop runs until either an
	// explicit STOP, RETURN or SELFDESTRUCT is executed, an error occurred during
	// the execution of one of the operations or until the done flag is set by the
	// parent context.
	// TODO: Main Loop
	_ = jumpTable[0] // nil-check the jumpTable out of the loop
	for {
		if debug {
			// Capture pre-execution values for tracing.
			logged, pcCopy, gasCopy = false, pc, contract.Gas
		}

		if in.evm.chainRules.IsEIP4762 && !contract.IsDeployment && !contract.IsSystemCall {
			// if the PC ends up in a new "chunk" of verkleized code, charge the
			// associated costs.
			contractAddr := contract.Address()
			consumed, wanted := in.evm.TxContext.AccessEvents.CodeChunksRangeGas(contractAddr, pc, 1, uint64(len(contract.Code)), false, contract.Gas)
			contract.UseGas(consumed, in.evm.Config.Tracer, tracing.GasChangeWitnessCodeChunk)
			if consumed < wanted {
				return nil, ErrOutOfGas
			}
		}

		// Get the operation from the jump table and validate the stack to ensure there are
		// enough stack items available to perform the operation.
		op = contract.GetOp(pc)		// TODO: program Counter가 가르키는 위치의 bytecode를 익어와 현재 실행할 OP로 지정
		operation := jumpTable[op]	// TODO: 점프테이블에서 해당 op에 대한 정보를 가져온다.
		cost = operation.constantGas // For tracing	
		// Validate stack   스택 검증
		if sLen := stack.len(); sLen < operation.minStack {
			return nil, &ErrStackUnderflow{stackLen: sLen, required: operation.minStack}
		} else if sLen > operation.maxStack {
			return nil, &ErrStackOverflow{stackLen: sLen, limit: operation.maxStack}
		}
		// for tracing: this gas consumption event is emitted below in the debug section.
		// TODO: Opcode를 실행할 수 있는 가스가 존재하는지 확인하고 + 차감
		if contract.Gas < cost {
			return nil, ErrOutOfGas
		} else {
			contract.Gas -= cost
		}

		// All ops with a dynamic memory usage also has a dynamic gas cost.
		var memorySize uint64
		if operation.dynamicGas != nil {
			// calculate the new memory size and expand the memory to fit
			// the operation
			// Memory check needs to be done prior to evaluating the dynamic gas portion,
			// to detect calculation overflows
			if operation.memorySize != nil {
				memSize, overflow := operation.memorySize(stack)
				if overflow {
					return nil, ErrGasUintOverflow
				}
				// memory is expanded in words of 32 bytes. Gas
				// is also calculated in words.
				if memorySize, overflow = math.SafeMul(toWordSize(memSize), 32); overflow {
					return nil, ErrGasUintOverflow
				}
			}
			// Consume the gas and return an error if not enough gas is available.
			// cost is explicitly set so that the capture state defer method can get the proper cost
			var dynamicCost uint64
			dynamicCost, err = operation.dynamicGas(in.evm, contract, stack, mem, memorySize)
			cost += dynamicCost // for tracing
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrOutOfGas, err)
			}
			// for tracing: this gas consumption event is emitted below in the debug section.
			if contract.Gas < dynamicCost {
				return nil, ErrOutOfGas
			} else {
				contract.Gas -= dynamicCost
			}
		}

		// Do tracing before potential memory expansion
		if debug {
			if in.evm.Config.Tracer.OnGasChange != nil {
				in.evm.Config.Tracer.OnGasChange(gasCopy, gasCopy-cost, tracing.GasChangeCallOpCode)
			}
			if in.evm.Config.Tracer.OnOpcode != nil {
				in.evm.Config.Tracer.OnOpcode(pc, byte(op), gasCopy, cost, callContext, in.returnData, in.evm.depth, VMErrorFromErr(err))
				logged = true
			}
		}

		// TODO: 현재 OPcode가 추가적인 메모리를 요구한다면, mem.Resize를 호출하여 메모리 공간을 동적으로 확장
		if memorySize > 0 {
			mem.Resize(memorySize)
		}

		// execute the operation
		// TODO: OPcode를 실행  [*점프테이블에거 가져온 operation의 execute 함수를 호출하여서 실제로 로직을 실행한다.]
		res, err = operation.execute(&pc, in, callContext)
		
		// TODO: 실행시 ERROR 발생시 루프탈출
		if err != nil {
			break
		}
		pc++	// TODO: 성공적으로 수행시, pc +1 을 통해서 다음 바이트 코드를 가르키도록 한다.
	}


	// TODO: 종료 및 결과 반환.		ERROR or SUCCESS에 따라서 결과를 반환한다.
	if err == errStopToken {
		err = nil // clear stop token error
	}

	return res, err
}
