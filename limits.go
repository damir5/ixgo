/*
 * Copyright (c) 2026 The GoPlus Authors (goplus.org). All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package ixgo

import (
	"errors"
	"go/types"
	"runtime/metrics"
	"sync/atomic"

	"golang.org/x/tools/go/ssa"
)

// Errors returned when a program exceeds a limit set on its Context.
var (
	ErrStepLimit      = errors.New("ixgo: step limit exceeded")
	ErrAllocLimit     = errors.New("ixgo: allocation limit exceeded")
	ErrHeapAllocLimit = errors.New("ixgo: process heap allocation limit exceeded")
	ErrCallDepthLimit = errors.New("ixgo: call depth limit exceeded")
)

// limitPanic unwinds the interpreted program when a limit is exceeded.
// recover() in the program does not stop it.
type limitPanic struct{ err error }

// heapSampleSteps is how many steps pass between reads of the process
// allocation counter. One read costs a few hundred nanoseconds.
const heapSampleSteps = 256

type limits struct {
	on           bool
	maxSteps     int64
	maxAlloc     int64
	maxHeapAlloc uint64
	maxDepth     int32
	steps        atomic.Int64
	charged      atomic.Int64
	heapBase     uint64
	err          atomic.Pointer[error]
}

var heapAllocSample = []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}}

func heapAllocated() uint64 {
	s := []metrics.Sample{heapAllocSample[0]}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

func (l *limits) init(ctx *Context) {
	l.maxSteps = ctx.MaxSteps
	l.maxAlloc = ctx.MaxAlloc
	l.maxHeapAlloc = ctx.MaxHeapAlloc
	l.maxDepth = ctx.MaxCallDepth
	l.on = l.maxSteps > 0 || l.maxAlloc > 0 || l.maxHeapAlloc > 0 || l.maxDepth > 0
	if l.maxHeapAlloc > 0 {
		l.heapBase = heapAllocated()
	}
}

// exceed records the first exceeded limit, stops the program and unwinds it.
func (i *Interp) exceed(err error) {
	i.lim.err.CompareAndSwap(nil, &err)
	i.Abort()
	panic(limitPanic{*i.lim.err.Load()})
}

// limitErr returns the limit the program exceeded, or nil.
func (i *Interp) limitErr() error {
	if p := i.lim.err.Load(); p != nil {
		return *p
	}
	return nil
}

// step counts one branch or call against MaxSteps and samples the process
// heap against MaxHeapAlloc.
func (i *Interp) step() {
	n := i.lim.steps.Add(1)
	if i.lim.maxSteps > 0 && n > i.lim.maxSteps {
		i.exceed(ErrStepLimit)
	}
	if i.lim.maxHeapAlloc > 0 && n%heapSampleSteps == 0 && heapAllocated()-i.lim.heapBase > i.lim.maxHeapAlloc {
		i.exceed(ErrHeapAllocLimit)
	}
	if atomic.LoadInt32(&i.exited) == 1 && i.limitErr() != nil {
		panic(limitPanic{i.limitErr()})
	}
}

// charge counts n bytes against MaxAlloc before an allocation is made.
func (i *Interp) charge(n int64) {
	if i.lim.maxAlloc > 0 && (n < 0 || i.lim.charged.Add(n) > i.lim.maxAlloc) {
		i.exceed(ErrAllocLimit)
	}
}

// chargeElems charges n elements of size bytes each, refusing overflow.
func (i *Interp) chargeElems(n int, size uintptr) {
	if i.lim.maxAlloc <= 0 {
		return
	}
	if n < 0 || (size != 0 && uint64(n) > uint64(i.lim.maxAlloc)/uint64(size)) {
		i.exceed(ErrAllocLimit)
	}
	i.charge(int64(n) * int64(size))
}

// Steps returns the number of branches and calls the program has executed.
// It is counted only when a limit is set on the Context.
func (i *Interp) Steps() int64 { return i.lim.steps.Load() }

// Charged returns the bytes charged against MaxAlloc.
func (i *Interp) Charged() int64 { return i.lim.charged.Load() }

// enterCall counts a call as a step and checks MaxCallDepth.
func (i *Interp) enterCall(caller, fr *frame) {
	fr.depth = caller.depth + 1
	if i.lim.maxDepth > 0 && fr.depth > i.lim.maxDepth {
		i.exceed(ErrCallDepthLimit)
	}
	i.step()
}

// vm0Size is the key plus element size of the map a MapUpdate writes.
func vm0Size(interp *Interp, instr *ssa.MapUpdate) uintptr {
	t := interp.preToType(instr.Map.Type())
	return t.Key().Size() + t.Elem().Size()
}

// convertBytesPer returns the bytes a conversion allocates per element of
// its operand, or 0 when it does not copy: string to []byte or []rune, and
// []byte or []rune to string.
func convertBytesPer(instr *ssa.Convert) uintptr {
	isStr := func(t types.Type) bool {
		b, ok := t.Underlying().(*types.Basic)
		return ok && b.Info()&types.IsString != 0
	}
	elem := func(t types.Type) (types.BasicKind, bool) {
		if s, ok := t.Underlying().(*types.Slice); ok {
			if b, ok := s.Elem().Underlying().(*types.Basic); ok {
				return b.Kind(), true
			}
		}
		return 0, false
	}
	from, to := instr.X.Type(), instr.Type()
	switch {
	case isStr(from):
		switch k, ok := elem(to); {
		case ok && k == types.Byte:
			return 1
		case ok && k == types.Rune:
			return 4
		}
	case isStr(to):
		switch k, ok := elem(from); {
		case ok && k == types.Byte:
			return 1
		case ok && k == types.Rune:
			return 4 // utf8.UTFMax
		}
	}
	return 0
}

// isBackEdge reports whether instr is a branch to a block at or before its
// own block. Every loop and goto cycle passes through such a branch.
func isBackEdge(b *ssa.BasicBlock, instr ssa.Instruction) bool {
	switch instr.(type) {
	case *ssa.Jump, *ssa.If:
		for _, s := range b.Succs {
			if s.Index <= b.Index {
				return true
			}
		}
	}
	return false
}

// countStep counts fn as a step before running it.
func countStep(interp *Interp, fn func(*frame)) func(*frame) {
	return func(fr *frame) {
		interp.step()
		fn(fr)
	}
}
