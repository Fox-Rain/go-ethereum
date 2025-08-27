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
	TODO: Blockchain 객체의 지시를 받아 블록에 포함된 모든 tx를 EVM에서 실행시키고 그 결과로 발생하는 상태 변경을 계산하는 역할을 한다.
		  추가적으로 비콘블록에 있어 실행계층에서 접근못하는 데이터나, 실행계층에 있어 비콘블록에서 접근못하는 데이터들을 system call을 통해
		  올려주거나 내려주는 로직도 담고 있다.
*/

package core

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// StateProcessor is a basic Processor, which takes care of transitioning
// state from one point to another.
//
// StateProcessor implements Processor.
// TODO: StateProcessor
type StateProcessor struct {
	config *params.ChainConfig // 체인정보 설정 (*이를 참고하여서 tx를 어떤 규칙을 적용하여 처리할지 결정)
	chain  *HeaderChain        // 해더 체인 객체  (블록 처리 중 부모 블록의 해시같은 과거 정보가필용할 때 사용)
}

// NewStateProcessor initialises a new StateProcessor.
// TODO: StateProcessor 객체를 생성하는 함수
func NewStateProcessor(config *params.ChainConfig, chain *HeaderChain) *StateProcessor {
	return &StateProcessor{
		config: config,
		chain:  chain,
	}
}

// Process processes the state changes according to the Ethereum rules by running
// the transaction messages using the statedb and applying any rewards to both
// the processor (coinbase) and any included uncles.
//
// Process returns the receipts and logs accumulated during the process and
// returns the amount of gas that was used in the process. If any of the
// transactions failed to execute due to insufficient gas it will return an error.
// TODO: Process		모든 tx를 실제로 실행하고, 그 결과로 발생하는 모든 상태변경을 계산하는 핵심 실행 엔진 함수
func (p *StateProcessor) Process(block *types.Block, statedb *state.StateDB, cfg vm.Config) (*ProcessResult, error) {
	var (
		receipts    types.Receipts				// 수행후 반환할 Receipts 목록
		usedGas     = new(uint64)				// 사용된 총 가스
		header      = block.Header()			// 블록 헤더
		blockHash   = block.Hash()				// 블록 해시
		blockNumber = block.Number()			// 블록 넘버
		allLogs     []*types.Log				// 모든 이벤트 로그
		gp          = new(GasPool).AddGas(block.GasLimit())		// 가스한도내에서 tx들이 가스를 사용하는지 추적하는 계량기 변수
	)

	// Mutate the block and state according to any hard-fork specs
	// DAO관련 하드포크 블록이라면 따로 처리
	if p.config.DAOForkSupport && p.config.DAOForkBlock != nil && p.config.DAOForkBlock.Cmp(block.Number()) == 0 {
		misc.ApplyDAOHardFork(statedb)
	}
	var (
		context vm.BlockContext
		signer  = types.MakeSigner(p.config, header.Number, header.Time)
	)

	// Apply pre-execution system calls.
	var tracingStateDB = vm.StateDB(statedb)
	if hooks := cfg.Tracer; hooks != nil {
		tracingStateDB = state.NewHookedState(statedb, hooks)
	}
	context = NewEVMBlockContext(header, p.chain, nil)			// TODO: EVM실행시 참조할 실행환경 정보 설정
	evm := vm.NewEVM(context, tracingStateDB, p.config, cfg)	// TODO: 이번 블록처리에 사용될 EVM 객체 생성

	if beaconRoot := block.BeaconRoot(); beaconRoot != nil {
		ProcessBeaconBlockRoot(*beaconRoot, evm)				// 비콘블록의 루트해시를 실행계층의 컨트랙트로 내려서 저장
	}
	if p.config.IsPrague(block.Number(), block.Time()) || p.config.IsVerkle(block.Number(), block.Time()) {
		ProcessParentBlockHash(block.ParentHash(), evm)
	}

	// Iterate over and process the individual transactions
	// TODO: transactions execute 
	for i, tx := range block.Transactions() {
		msg, err := TransactionToMessage(tx, signer, header.BaseFee)	// TODO: EVM이 이해할 수 있는 msg 객체로 변환 + 이 과정에서 서명을 검증하여 발신자 주소 복구
		if err != nil {
			return nil, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}
		statedb.SetTxContext(tx.Hash(), i)

		receipt, err := ApplyTransactionWithEVM(msg, gp, statedb, blockNumber, blockHash, context.Time, tx, usedGas, evm) // TODO: tx실행이 일어나는 부분으로, EVM을 통해 msg를 실행하고 그 결과로 발생한 상태변경을 statedb에 적용하며 receipt를 생성한다.
		if err != nil {
			return nil, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}
		receipts = append(receipts, receipt)		// TODO: 생성된 영수증과 그 안의 로그들을 최종 결과 목록에 추가
		allLogs = append(allLogs, receipt.Logs...)
	}
	// Read requests if Prague is enabled.
	// TODO: 후처리 작업
	var requests [][]byte
	if p.config.IsPrague(block.Number(), block.Time()) {
		requests = [][]byte{}
		// EIP-6110
		if err := ParseDepositLogs(&requests, allLogs, p.config); err != nil {
			return nil, err
		}
		// EIP-7002
		if err := ProcessWithdrawalQueue(&requests, evm); err != nil {
			return nil, err
		}
		// EIP-7251
		if err := ProcessConsolidationQueue(&requests, evm); err != nil {
			return nil, err
		}
	}

	// Finalize the block, applying any consensus engine specific extras (e.g. block rewards)
	// TODO: 모든 tx실행이 끝나고 합의 엔진의 Finalize 함수를 호출 => 블록생성관련 보상지급등과 같은 최종 마무리 수행
	p.chain.engine.Finalize(p.chain, header, tracingStateDB, block.Body())

	// TODO: 결과 반환
	return &ProcessResult{
		Receipts: receipts,
		Requests: requests,
		Logs:     allLogs,
		GasUsed:  *usedGas,
	}, nil
}




// ApplyTransactionWithEVM attempts to apply a transaction to the given state database
// and uses the input parameters for its environment similar to ApplyTransaction. However,
// this method takes an already created EVM instance as input.
// TODO: 준비된 EVM를 사용하여서 "단일" tx를 실행하고 그 결과를 처리하는 유틸리티 함수
func ApplyTransactionWithEVM(msg *Message, gp *GasPool, statedb *state.StateDB, blockNumber *big.Int, blockHash common.Hash, blockTime uint64, tx *types.Transaction, usedGas *uint64, evm *vm.EVM) (receipt *types.Receipt, err error) {
	if hooks := evm.Config.Tracer; hooks != nil {
		if hooks.OnTxStart != nil {
			hooks.OnTxStart(evm.GetVMContext(), tx, msg.From)
		}
		if hooks.OnTxEnd != nil {
			defer func() { hooks.OnTxEnd(receipt, err) }()
		}
	}
	// Apply the transaction to the current state (included in the env).
	// TODO: 트랜잭션 실행
	result, err := ApplyMessage(evm, msg, gp)	// EVM에서 msg 실행 + 결과는 receipt에 담김
	if err != nil {
		return nil, err
	}
	// Update the state with pending changes.
	// TODO: StateDB 상태 업데이트 및 가스 정산
	var root []byte
	if evm.ChainConfig().IsByzantium(blockNumber) {
		evm.StateDB.Finalise(true)
	} else {
		root = statedb.IntermediateRoot(evm.ChainConfig().IsEIP158(blockNumber)).Bytes()
	}
	*usedGas += result.UsedGas

	// Merge the tx-local access event into the "block-local" one, in order to collect
	// all values, so that the witness can be built.
	if statedb.Database().TrieDB().IsVerkle() {
		statedb.AccessEvents().Merge(evm.AccessEvents)
	}
	return MakeReceipt(evm, result, statedb, blockNumber, blockHash, blockTime, tx, *usedGas, root), nil
}





// MakeReceipt generates the receipt object for a transaction given its execution result.
// TODO: tx수행이 끝나고, 모든 결과를 종합하여 Receipt를 만드는 함수   (tx1개당 receipt도 1개)
func MakeReceipt(evm *vm.EVM, result *ExecutionResult, statedb *state.StateDB, blockNumber *big.Int, blockHash common.Hash, blockTime uint64, tx *types.Transaction, usedGas uint64, root []byte) *types.Receipt {
	// Create a new receipt for the transaction, storing the intermediate root and gas used
	// by the tx.
	receipt := &types.Receipt{Type: tx.Type(), PostState: root, CumulativeGasUsed: usedGas}
	if result.Failed() {
		receipt.Status = types.ReceiptStatusFailed
	} else {
		receipt.Status = types.ReceiptStatusSuccessful
	}
	receipt.TxHash = tx.Hash()
	receipt.GasUsed = result.UsedGas

	if tx.Type() == types.BlobTxType {
		receipt.BlobGasUsed = uint64(len(tx.BlobHashes()) * params.BlobTxBlobGasPerBlob)
		receipt.BlobGasPrice = evm.Context.BlobBaseFee
	}

	// If the transaction created a contract, store the creation address in the receipt.
	if tx.To() == nil {
		receipt.ContractAddress = crypto.CreateAddress(evm.TxContext.Origin, tx.Nonce())
	}

	// Set the receipt logs and create the bloom filter.
	receipt.Logs = statedb.GetLogs(tx.Hash(), blockNumber.Uint64(), blockHash, blockTime)
	receipt.Bloom = types.CreateBloom(receipt)
	receipt.BlockHash = blockHash
	receipt.BlockNumber = blockNumber
	receipt.TransactionIndex = uint(statedb.TxIndex())
	return receipt
}



// ApplyTransaction attempts to apply a transaction to the given state database
// and uses the input parameters for its environment. It returns the receipt
// for the transaction, gas used and an error if the transaction failed,
// indicating the block was invalid.
// TODO: 트랜잭션 실행을 위한 사전 준비작업을 하고 + 실제 실행은 ApplyTransactionWithEVM 에게 위임하는 Wrapper 함수  (준비 + 실행하는 함수)
func ApplyTransaction(evm *vm.EVM, gp *GasPool, statedb *state.StateDB, header *types.Header, tx *types.Transaction, usedGas *uint64) (*types.Receipt, error) {
	msg, err := TransactionToMessage(tx, types.MakeSigner(evm.ChainConfig(), header.Number, header.Time), header.BaseFee)	// tx -> msg
	if err != nil {
		return nil, err
	}
	// Create a new context to be used in the EVM environment
	return ApplyTransactionWithEVM(msg, gp, statedb, header.Number, header.Hash(), header.Time, tx, usedGas, evm)	// TODO ApplyTransactionWithEVM을 호출
}



// ProcessBeaconBlockRoot applies the EIP-4788 system call to the beacon block root
// contract. This method is exported to be used in tests.
// TODO: 합의계층의 비콘블록의 루트해시를 실행계층에 있는 스마트컨트랙트에 기록하는 시스템 콜
// TODO: 이렇게 불편하게 과정을 거치는 이유: 실행계층의 스마트컨트랙트는 합의계층의 데이터를 직접 읽을 수 없기 때문에 아래로 내려서 저장해주는 것   { EIP-4788 }
// TODO: 검증자들은 비콘블록에 대한 정보에 접근이 가능하나, 일반적인 dapp의 경우는 스마트컨트랙트에 대해서만 접근이 가능하기에, 이들이 사용할 수 있도록 데이터를 아래로 내려서 저장해주는 것  * 
func ProcessBeaconBlockRoot(beaconRoot common.Hash, evm *vm.EVM) {
	if tracer := evm.Config.Tracer; tracer != nil {
		onSystemCallStart(tracer, evm.GetVMContext())
		if tracer.OnSystemCallEnd != nil {
			defer tracer.OnSystemCallEnd()
		}
	}

	// TODO: "transaction이 아닌" 시스템 자체에서 발생하는 특별한 메세지를 생성
	msg := &Message{
		From:      params.SystemAddress,
		GasLimit:  30_000_000,
		GasPrice:  common.Big0,
		GasFeeCap: common.Big0,
		GasTipCap: common.Big0,
		To:        &params.BeaconRootsAddress,
		Data:      beaconRoot[:],
	}

	// TODO: EVM 실행
	evm.SetTxContext(NewEVMTxContext(msg))
	evm.StateDB.AddAddressToAccessList(params.BeaconRootsAddress)
	_, _, _ = evm.Call(msg.From, *msg.To, msg.Data, 30_000_000, common.U2560)
	evm.StateDB.Finalise(true)
}



// ProcessParentBlockHash stores the parent block hash in the history storage contract
// as per EIP-2935/7709.
// TODO: 이전블록의 해시 prevHash를 History Storage라는 특별한 스마트컨트랙트에 기록하는 시스템 콜   { EIP-2935 }
// TODO: prev hash는 execute payload에 저장되므로, 물론 접근이 가능하지만 최신 256개의 블록에 대해서만 접근 가능하다는 제한이 있다. 이를 해결하기 위해서 특별한 컨트랙트를 만들어서 더 많은 해시르 저장하기 위함이다.
func ProcessParentBlockHash(prevHash common.Hash, evm *vm.EVM) {
	if tracer := evm.Config.Tracer; tracer != nil {
		onSystemCallStart(tracer, evm.GetVMContext())
		if tracer.OnSystemCallEnd != nil {
			defer tracer.OnSystemCallEnd()
		}
	}
	msg := &Message{
		From:      params.SystemAddress,
		GasLimit:  30_000_000,
		GasPrice:  common.Big0,
		GasFeeCap: common.Big0,
		GasTipCap: common.Big0,
		To:        &params.HistoryStorageAddress,
		Data:      prevHash.Bytes(),
	}
	evm.SetTxContext(NewEVMTxContext(msg))
	evm.StateDB.AddAddressToAccessList(params.HistoryStorageAddress)
	_, _, err := evm.Call(msg.From, *msg.To, msg.Data, 30_000_000, common.U2560)
	if err != nil {
		panic(err)
	}
	if evm.StateDB.AccessEvents() != nil {
		evm.StateDB.AccessEvents().Merge(evm.AccessEvents)
	}
	evm.StateDB.Finalise(true)
}



// ProcessWithdrawalQueue calls the EIP-7002 withdrawal queue contract.
// It returns the opaque request data returned by the contract.
// TODO: 합의계층에서 처리된 검증자의 인출요청을 실행계층에 전달하고 처리하는 시스템 콜
func ProcessWithdrawalQueue(requests *[][]byte, evm *vm.EVM) error {
	return processRequestsSystemCall(requests, evm, 0x01, params.WithdrawalQueueAddress)
}

// ProcessConsolidationQueue calls the EIP-7251 consolidation queue contract.
// It returns the opaque request data returned by the contract.
// TODO: 검증자가 운영하는 주체가 보상을 받아 늘어난 잔액을 자신의 유효잔액에 통합하는 시스템 콜
func ProcessConsolidationQueue(requests *[][]byte, evm *vm.EVM) error {
	return processRequestsSystemCall(requests, evm, 0x02, params.ConsolidationQueueAddress)
}



// TODO: 위의 두 함수의 System call을 실행하기 위한 공통로직을 담은 내부 헬퍼 함수
func processRequestsSystemCall(requests *[][]byte, evm *vm.EVM, requestType byte, addr common.Address) error {
	if tracer := evm.Config.Tracer; tracer != nil {
		onSystemCallStart(tracer, evm.GetVMContext())
		if tracer.OnSystemCallEnd != nil {
			defer tracer.OnSystemCallEnd()
		}
	}
	
	// tx가 아닌 system msg 생성
	msg := &Message{
		From:      params.SystemAddress,
		GasLimit:  30_000_000,
		GasPrice:  common.Big0,
		GasFeeCap: common.Big0,
		GasTipCap: common.Big0,
		To:        &addr,
	}
	evm.SetTxContext(NewEVMTxContext(msg))
	evm.StateDB.AddAddressToAccessList(addr)
	ret, _, err := evm.Call(msg.From, *msg.To, msg.Data, 30_000_000, common.U2560)
	evm.StateDB.Finalise(true)
	if err != nil {
		return fmt.Errorf("system call failed to execute: %v", err)
	}
	if len(ret) == 0 {
		return nil // skip empty output
	}
	// Append prefixed requestsData to the requests list.
	requestsData := make([]byte, len(ret)+1)
	requestsData[0] = requestType
	copy(requestsData[1:], ret)
	*requests = append(*requests, requestsData)
	return nil
}



// TODO: 예치 컨트랙트에서 DepositEvent라는 이벤트가 발생할 때 생성되는 고유한 토픽 해시값이다. 이를 통해서 로그를 필터링함
var depositTopic = common.HexToHash("0x649bbc62d0e31342afea4e5cd82d4049e7e1ee912fc0889aa790803be39038c5")

// ParseDepositLogs extracts the EIP-6110 deposit values from logs emitted by
// BeaconDepositContract.
// TODO: 블록에 포함된 모든 로그중에서 새로운 검증자의 예치와 관련된 로그만 찾아내어 예치요청 데이터로 변환하는 함수  {EIP-6110}
// TODO: 이는 실행계층에서 발생한 예치를 합의계층에서도 알 수 있도록 데이터를 변환기 위함이다.
func ParseDepositLogs(requests *[][]byte, logs []*types.Log, config *params.ChainConfig) error {
	deposits := make([]byte, 1) // note: first byte is 0x00 (== deposit request type)
	for _, log := range logs {
		if log.Address == config.DepositContractAddress && len(log.Topics) > 0 && log.Topics[0] == depositTopic {
			request, err := types.DepositLogToRequest(log.Data)
			if err != nil {
				return fmt.Errorf("unable to parse deposit data: %v", err)
			}
			deposits = append(deposits, request...)
		}
	}
	if len(deposits) > 1 {
		*requests = append(*requests, deposits)
	}
	return nil
}


// 디버깅 및 분석을 위한 추적기의 시스템 콜 시작 훅 함수를 호출하는 역할의 헬퍼 함수
func onSystemCallStart(tracer *tracing.Hooks, ctx *tracing.VMContext) {
	if tracer.OnSystemCallStartV2 != nil {
		tracer.OnSystemCallStartV2(ctx)
	} else if tracer.OnSystemCallStart != nil {
		tracer.OnSystemCallStart()
	}
}

