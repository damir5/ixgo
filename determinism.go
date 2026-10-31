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
	"fmt"
	"reflect"

	"github.com/goplus/ixgo/internal/keyorder"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// ErrNondeterministic is returned, wrapped, before a deterministic run
// starts when the program contains a go statement or a select with more
// than one channel case.
var ErrNondeterministic = errors.New("ixgo: not allowed in a deterministic run")

// checkDeterministic refuses what makes a run take another path when it runs
// again: goroutines, which the Go scheduler orders, and a select with several
// channel cases, where Go picks a ready case at random.
func checkDeterministic(prog *ssa.Program, pkgs []*ssa.Package) error {
	interpreted := map[*ssa.Package]bool{}
	for _, p := range pkgs {
		interpreted[p] = true
	}
	for fn := range ssautil.AllFunctions(prog) {
		pkg := fn.Pkg
		if pkg == nil && fn.Origin() != nil {
			pkg = fn.Origin().Pkg
		}
		if !interpreted[pkg] {
			continue
		}
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				switch instr := instr.(type) {
				case *ssa.Go:
					return fmt.Errorf("%v: go statement: %w", prog.Fset.Position(instr.Pos()), ErrNondeterministic)
				case *ssa.Select:
					if len(instr.States) > 1 {
						return fmt.Errorf("%v: select with %d channel cases: %w", prog.Fset.Position(instr.Pos()), len(instr.States), ErrNondeterministic)
					}
				}
			}
		}
	}
	return nil
}

// sortedMapIter ranges over a map in key order. Like Go's own range, it
// skips keys deleted before they are reached and reads each value when the
// key is reached; keys added during the loop are not reached.
type sortedMapIter struct {
	m       reflect.Value
	entries []keyorder.KeyValue
}

func newSortedMapIter(interp *Interp, m reflect.Value) *sortedMapIter {
	interp.chargeElems(m.Len(), 2*reflect.TypeFor[reflect.Value]().Size())
	return &sortedMapIter{m: m, entries: keyorder.Sort(m)}
}

func (it *sortedMapIter) next() tuple {
	for len(it.entries) > 0 {
		e := it.entries[0]
		it.entries = it.entries[1:]
		v := it.m.MapIndex(e.Key)
		if !v.IsValid() {
			if e.Key.Equal(e.Key) {
				continue // deleted
			}
			v = e.Value // a NaN key cannot be looked up
		}
		return []value{true, e.Key.Interface(), v.Interface()}
	}
	return []value{false, nil, nil}
}

// InDeterministicRun reports whether the native function calling it runs for
// a program whose Context has Deterministic set.
func InDeterministicRun() bool {
	i := nativeInterp()
	return i != nil && i.ctx.Deterministic
}
