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
	"reflect"
	"runtime/metrics"
	"sync"
	"sync/atomic"
	"time"

	"github.com/visualfc/gid"
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
	spot         sync.Mutex // guards the last* fields
	lastTime     int64
	lastHeap     uint64
	lastCharged  int64
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
	l.on = l.maxSteps > 0 || l.maxAlloc > 0 || l.maxHeapAlloc > 0 || l.maxDepth > 0 || ctx.Deterministic // InDeterministicRun needs the native binding
	if l.maxHeapAlloc > 0 || l.maxAlloc > 0 {
		l.heapBase = heapAllocated()
		l.lastTime, l.lastHeap = time.Now().UnixNano(), l.heapBase
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
	if (i.lim.maxHeapAlloc > 0 || i.lim.maxAlloc > 0) && n%heapSampleSteps == 0 {
		heap := heapAllocated()
		if i.lim.maxHeapAlloc > 0 && heap-i.lim.heapBase > i.lim.maxHeapAlloc {
			i.exceed(ErrHeapAllocLimit)
		}
		if i.lim.maxAlloc > 0 {
			i.spotCheck(heap)
		}
	}
	if atomic.LoadInt32(&i.exited) == 1 && i.limitErr() != nil {
		panic(limitPanic{i.limitErr()})
	}
}

// Spot check constants. Wrapped Go functions charge what they return or keep,
// and their temporary buffers measured at most 6.3 times that, so heap growth
// up to spotFactor times the charges is expected. spotSlack and spotRate
// cover the interpreter's own allocations and other goroutines of the host.
const (
	spotFactor = 8
	spotSlack  = 1 << 20 // bytes per check
	spotRate   = 1       // bytes per nanosecond between checks
	// A native call slower than this is checked when it returns. Clearing
	// memory runs at about 10 GB/s, so this is about 1 MB of allocation.
	spotCallTime = 100 * time.Microsecond
)

// spotCheck charges heap growth since the last check that the charges in the
// same window do not explain: allocations in Go code that no wrapper covers,
// such as an interface call on a bytes.Buffer. It charges late, at most
// heapSampleSteps steps after the allocation. ponytail: the heap counter is
// process-wide, so a host allocating more than spotRate while the program
// runs can be charged to it; per-call measurement is the upgrade.
func (i *Interp) spotCheck(heap uint64) {
	l := &i.lim
	if !l.spot.TryLock() {
		return
	}
	now, charged := time.Now().UnixNano(), l.charged.Load()
	grown := int64(heap - l.lastHeap)
	explained := spotFactor*(charged-l.lastCharged) + spotSlack + spotRate*(now-l.lastTime)
	l.lastTime, l.lastHeap, l.lastCharged = now, heap, charged
	if excess := grown - explained; excess > 0 {
		l.lastCharged += excess
		l.spot.Unlock()
		i.charge(excess)
		return
	}
	l.spot.Unlock()
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

// natives maps a goroutine id to the Interp whose native call runs on that
// goroutine, so registered Go functions can charge with Charge.
var natives sync.Map // uint64 -> *Interp

// bindNative records i as the Interp calling native code on this goroutine
// and returns a function that restores the previous record.
func (i *Interp) bindNative() func() {
	id := gid.Get()
	prev, had := natives.Swap(id, i)
	start := time.Now()
	return func() {
		if had {
			natives.Store(id, prev)
		} else {
			natives.Delete(id)
		}
		// Allocating a lot takes time, so a slow call gets a spot check.
		if i.lim.maxAlloc > 0 && time.Since(start) > spotCallTime {
			i.spotCheck(heapAllocated())
		}
	}
}

func nativeInterp() *Interp {
	if v, ok := natives.Load(gid.Get()); ok {
		return v.(*Interp)
	}
	return nil
}

// foreign reports whether limits are on and Go code is calling into the
// program on a goroutine that is not running one of the program's native
// calls, such as a time.AfterFunc callback.
func (i *Interp) foreign() bool {
	return i.lim.on && nativeInterp() != i
}

// recoverForeign stops the program when program code that Go code runs on its
// own goroutine panics, instead of letting the panic end the host process.
// The run returns the limit error, or the panic as for a go statement.
func (i *Interp) recoverForeign() {
	e := recover()
	if e == nil {
		return
	}
	if lp, ok := e.(limitPanic); ok {
		i.lim.err.CompareAndSwap(nil, &lp.err)
	}
	select {
	case i.cherror <- PanicError{Value: e}:
	default:
	}
	i.Abort()
}

func zeroResults(typ reflect.Type) []reflect.Value {
	results := make([]reflect.Value, typ.NumOut())
	for k := range results {
		results[k] = reflect.Zero(typ.Out(k))
	}
	return results
}

// Charge counts n bytes against MaxAlloc of the program that called the
// running Go function. A Go function registered for interpreted programs
// calls it before it allocates memory the program controls. When the limit
// is exceeded, Charge stops the program and unwinds it with ErrAllocLimit.
// Outside a call from a limited program, Charge does nothing.
func Charge(n int64) {
	if i := nativeInterp(); i != nil {
		i.charge(n)
	}
}

// ChargeElems is Charge for n elements of size bytes each. It treats a
// product that overflows as exceeding the limit.
func ChargeElems(n int, size uintptr) {
	if i := nativeInterp(); i != nil {
		i.chargeElems(n, size)
	}
}

// Refund returns n bytes charged with Charge, for a function that charged
// an upper bound and kept less.
func Refund(n int64) {
	if i := nativeInterp(); i != nil && i.lim.maxAlloc > 0 {
		i.lim.charged.Add(-n)
	}
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
