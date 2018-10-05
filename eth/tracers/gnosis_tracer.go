// Copyright 2017 The go-ethereum Authors
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

package tracers

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
)

type Call struct {
	op    vm.OpCode
	to    common.Address
	from  common.Address
	input []byte
	value *big.Int
	err   error
	calls []Call
}

func (c Call) String() string {
	return fmt.Sprintf("%s: %s -> %s", c.op, c.from.Hex(), c.to.Hex())
}

type GnosisTracer struct {
	callstack    []Call
	maxDepth     int
	masterCopies []common.Address
}

// CaptureStart implements the Tracer interface to initialize the tracing operation.
func (gt *GnosisTracer) CaptureStart(from common.Address, to common.Address, create bool, input []byte, gas uint64, value *big.Int) error {
	call := Call{
		op:    vm.CALL,
		from:  from,
		to:    to,
		value: value,
		calls: []Call{},
	}
	if create {
		call.op = vm.CREATE
	}
	gt.callstack = []Call{call}

	return nil
}

func NewGnosisTracer() *GnosisTracer {
	tracer := &GnosisTracer{
		masterCopies: []common.Address{common.HexToAddress("0x44e7f5855a77fe1793a96be8a1c9c3eaf47e9d09")},
	}
	return tracer
}

// CaptureState implements the Tracer interface to trace a single step of VM execution.
func (gt *GnosisTracer) CaptureState(env *vm.EVM, pc uint64, op vm.OpCode, gas, cost uint64, memory *vm.Memory, stack *vm.Stack, contract *vm.Contract, depth int, err error) error {

	if depth > gt.maxDepth {
		gt.maxDepth = depth
	}

	if err != nil {
		gt.CaptureFault(env, pc, op, gas, cost, memory, stack, contract, depth, err)
		return nil
	}

	// If a new method invocation is being done, add to the call stack
	if op == vm.REVERT {
		gt.callstack[len(gt.callstack)-1].err = errors.New("execution reverted")
		return nil
	}

	if op == vm.CREATE || op == vm.CREATE2 {
		call := Call{
			op:    op,
			from:  contract.Address(),
			to:    contract.Address(),
			calls: []Call{},
		}
		gt.callstack = append(gt.callstack, call)
		return nil
	}

	if op == vm.SELFDESTRUCT {
		// TODO: Ignored
		return nil
	}

	if op == vm.CALL || op == vm.CALLCODE || op == vm.DELEGATECALL || op == vm.STATICCALL {

		call := Call{
			op:   op,
			from: contract.Address(),
			to:   common.BytesToAddress(stack.Data()[len(stack.Data())-2].Bytes()),
			//value: value,
			calls: []Call{},
		}
		gt.callstack = append(gt.callstack, call)
		return nil
	}

	callcount := len(gt.callstack)
	if depth == callcount-1 {
		call := gt.callstack[callcount-1]
		gt.callstack = gt.callstack[:callcount-1]
		gt.callstack[callcount-2].calls = append(gt.callstack[callcount-2].calls, call)
	}
	return nil
}

// CaptureFault implements the Tracer interface to trace an execution fault
// while running an opcode.
func (gt *GnosisTracer) CaptureFault(env *vm.EVM, pc uint64, op vm.OpCode, gas, cost uint64, memory *vm.Memory, stack *vm.Stack, contract *vm.Contract, depth int, err error) error {
	callcount := len(gt.callstack)
	call := gt.callstack[callcount-1]
	if call.err != nil {
		return nil
	}
	call.err = err
	if callcount > 1 {
		gt.callstack = gt.callstack[:callcount-1]
		gt.callstack[callcount-2].calls = append(gt.callstack[callcount-2].calls, call)
	}
	return nil
}

func (gt *GnosisTracer) isSafeTx(call Call) bool {
	if call.op == vm.DELEGATECALL {
		for _, masterCopy := range gt.masterCopies {
			if bytes.Compare(masterCopy.Bytes(), call.to.Bytes()) == 0 {
				return true
			}
		}
	}
	return false
}

func (gt *GnosisTracer) checkCalls(blockNumber *big.Int, time *big.Int, calls []Call) {
	for _, call := range calls {
		if gt.isSafeTx(call) {
			log.Info("Traced tx", "Safe Tx", fmt.Sprintf("%v", call))
		}
		gt.checkCalls(blockNumber, time, call.calls)
	}
}

func (gt *GnosisTracer) outputResult(blockNumber *big.Int, time *big.Int, depth int, calls []Call) {
	if depth > 1 {
		gt.checkCalls(blockNumber, time, calls)
	}
}

// CaptureEnd is called after the call finishes to finalize the tracing.
func (gt *GnosisTracer) CaptureEnd(env *vm.EVM, output []byte, gasUsed uint64, t time.Duration, err error) error {
	go gt.outputResult(env.BlockNumber, env.Time, gt.maxDepth, gt.callstack)
	return nil
}
