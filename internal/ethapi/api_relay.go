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

package ethapi

import (
	"context"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
)

// TxRelayAPI offers transaction relay related RPC methods
type TxRelayAPI struct {
	b Backend
}

// NewTxRelayAPI creates a new relay API instance.
func NewTxRelayAPI(b Backend) *TxRelayAPI {
	return &TxRelayAPI{b}
}

type ExecutionContext struct {
	State    *state.StateDB
	Context  context.Context
	EVM      *vm.EVM
	Message  types.Message
	GasPool  *core.GasPool
	GasPrice *big.Int
	Gas      uint64
	Sender   common.Address
	VmError  func() error
	Cancel   context.CancelFunc
}

func (s *TxRelayAPI) newExecutionContext(ctx context.Context, args TxRelayCheckArgs, blockNr rpc.BlockNumber, vmCfg vm.Config, timeout time.Duration) (*ExecutionContext, error) {
	state, header, err := s.b.StateAndHeaderByNumber(ctx, blockNr)
	if state == nil || err != nil {
		return nil, err
	}
	// Set sender address or use a default if none specified
	addr := args.From
	if addr == (common.Address{}) {
		if wallets := s.b.AccountManager().Wallets(); len(wallets) > 0 {
			if accounts := wallets[0].Accounts(); len(accounts) > 0 {
				addr = accounts[0].Address
			}
		}
	}

	// Set default gas & gas price if none were set
	gas, gasPrice := uint64(args.Gas), args.GasPrice.ToInt()
	if gas == 0 {
		gas = math.MaxUint64 / 2
	}
	if gasPrice.Sign() == 0 {
		gasPrice = new(big.Int).SetUint64(defaultGasPrice)
	}

	// Setup gas pool
	gp := new(core.GasPool).AddGas(math.MaxUint64)

	// Create new call message
	msg := types.NewMessage(addr, args.To, 0, args.Value.ToInt(), gas, gasPrice, args.Data, false)

	// Setup context so it may be cancelled the call has completed
	// or, in case of unmetered gas, setup a context with a timeout.
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}

	// Get a new instance of the EVM.
	evm, vmError, err := s.b.GetEVM(ctx, msg, state, header, vmCfg)
	if err != nil {
		cancel()
		return nil, err
	}

	return &ExecutionContext{
		state,
		ctx,
		evm,
		msg,
		gp,
		gasPrice,
		gas,
		addr,
		vmError,
		cancel,
	}, nil
}

// TxRelayCheckArgs represents the arguments for a relay call.
type TxRelayCheckArgs struct {
	From     common.Address  `json:"from"`
	To       *common.Address `json:"to"`
	Gas      hexutil.Uint64  `json:"gas"`
	GasPrice hexutil.Big     `json:"gasPrice"`
	Value    hexutil.Big     `json:"value"`
	Data     hexutil.Bytes   `json:"data"`
	Token    common.Address  `json:"token"`
}

// TODO method comment
func (s *TxRelayAPI) CheckTransaction(ctx context.Context, args TxRelayCheckArgs, blockNr rpc.BlockNumber) (map[string]interface{}, error) {
	defer func(start time.Time) { log.Debug("Executing EVM call finished", "runtime", time.Since(start)) }(time.Now())

	execContext, err := s.newExecutionContext(ctx, args, blockNr, vm.Config{}, 5*time.Second)
	if err != nil {
		return nil, err
	}

	// Make sure the context is cancelled when the call has completed
	// this makes sure resources are cleaned up.
	defer execContext.Cancel()

	// Wait for the context to be done and cancel the evm. Even if the
	// EVM has finished, cancelling may be done (repeatedly)
	go func() {
		<-execContext.Context.Done()
		execContext.EVM.Cancel()
	}()

	initialTokenBalance := s.getTokenBalance(execContext, execContext.Sender, args.Token)
	initialBalance := execContext.State.GetBalance(execContext.Sender)

	result, usedGas, _, err := core.ApplyMessage(execContext.EVM, execContext.Message, execContext.GasPool)
	if err := execContext.VmError(); err != nil {
		return nil, err
	}

	finalTokenBalance := s.getTokenBalance(execContext, execContext.Sender, args.Token)

	fields := map[string]interface{}{
		"result":           (hexutil.Bytes)(result),
		"gasUsed":          hexutil.Uint64(usedGas),
		"ethBalanceDiff":   (*hexutil.Big)(new(big.Int).Sub(execContext.State.GetBalance(execContext.Sender), initialBalance)),
		"tokenBalanceDiff": (*hexutil.Big)(new(big.Int).Sub(finalTokenBalance, initialTokenBalance)),
	}
	return fields, err
}

func (s *TxRelayAPI) getTokenBalance(execContext *ExecutionContext, addr common.Address, token common.Address) *big.Int {
	if token == (common.Address{}) {
		return new(big.Int)
	}
	checkTokenData := append(common.Hex2Bytes("70a08231000000000000000000000000"), addr.Bytes()...)
	checkTokenMsg := types.NewMessage(addr, &token, 0, new(big.Int), math.MaxUint64/2, execContext.GasPrice, checkTokenData, false)
	hexBalance, _, _, _ := core.ApplyMessage(execContext.EVM, checkTokenMsg, execContext.GasPool)
	return new(big.Int).SetBytes(hexBalance)
}

// TODO method comment
func (s *TxRelayAPI) ExecuteCode(ctx context.Context, address common.Address, code hexutil.Bytes, args TxRelayCheckArgs, blockNr rpc.BlockNumber) (hexutil.Bytes, error) {
	defer func(start time.Time) { log.Debug("Executing EVM call finished", "runtime", time.Since(start)) }(time.Now())

	execContext, err := s.newExecutionContext(ctx, args, blockNr, vm.Config{}, 5*time.Second)
	if err != nil {
		return (hexutil.Bytes)(nil), err
	}

	// Make sure the context is cancelled when the call has completed
	// this makes sure resources are cleaned up.
	defer execContext.Cancel()

	// Wait for the context to be done and cancel the evm. Even if the
	// EVM has finished, cancelling may be done (repeatedly)
	go func() {
		<-execContext.Context.Done()
		execContext.EVM.Cancel()
	}()

	execContext.State.SetCode(address, code)

	result, _, _, err := core.ApplyMessage(execContext.EVM, execContext.Message, execContext.GasPool)
	if err := execContext.VmError(); err != nil {
		return (hexutil.Bytes)(nil), err
	}

	return (hexutil.Bytes)(result), err
}
