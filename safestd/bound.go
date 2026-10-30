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

package safestd

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// Errors a wrapped function panics with instead of calling the library.
var (
	ErrCyclicValue = errors.New("safestd: value contains itself")
	ErrTooDeep     = errors.New("safestd: value nested too deeply")
)

// maxDepth is the deepest value printBound walks. fmt and encoding/json
// recurse once per level on the Go stack.
const maxDepth = 10000

// fmtMaxPad is the largest width or precision fmt accepts (fmt.tooLarge).
const fmtMaxPad = 1e6

// methodBound counts a value whose type formats itself. ponytail: a fixed
// guess; such a method can return more, and MaxHeapAlloc must catch that.
const methodBound = 256

var (
	typStringer      = reflect.TypeFor[fmt.Stringer]()
	typFormatter     = reflect.TypeFor[fmt.Formatter]()
	typGoStringer    = reflect.TypeFor[fmt.GoStringer]()
	typError         = reflect.TypeFor[error]()
	typJSONMarshaler = reflect.TypeFor[json.Marshaler]()
	typTextMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
)

func formatsItself(t reflect.Type) bool {
	for _, m := range []reflect.Type{typStringer, typFormatter, typGoStringer, typError, typJSONMarshaler, typTextMarshaler} {
		if t.Implements(m) {
			return true
		}
	}
	return false
}

// visit is the data of a map, slice or pointer; a slice that holds itself
// repeats all three.
type visit struct {
	p uintptr
	t reflect.Type
	n int
}

type walker struct {
	json  bool           // follow every pointer and leave cycles to json's own check
	float int            // bytes for one float: more when a %f verb sets a precision
	path  map[visit]bool // maps, slices and pointers on the current path
}

// jsonBound is an upper bound of the bytes encoding/json writes for v. It
// panics with ErrTooDeep.
func jsonBound(v any) int {
	w := walker{json: true, float: 40, path: map[visit]bool{}}
	return w.size(reflect.ValueOf(v), 0)
}

// fmtBound is an upper bound of the bytes fmt writes for a format and its
// arguments with any verbs: the format text, every width and precision it
// can apply, and each argument. It panics with ErrCyclicValue or ErrTooDeep.
func fmtBound(format string, args []any) int {
	pad, float := fmtPads(format)
	w := walker{float: float, path: map[visit]bool{}}
	n := len(format) + pad + 2*len(args)
	for _, a := range args {
		n += w.size(reflect.ValueOf(a), 0)
	}
	return n
}

// fmtPads sums the widths and precisions in format, counting a '*' as the
// largest fmt accepts, and returns the float size its verbs allow.
func fmtPads(format string) (pad, float int) {
	float = 40
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		prec := 0
		for i++; i < len(format); i++ {
			c := format[i]
			switch {
			case c == '*':
				pad += fmtMaxPad
			case c >= '0' && c <= '9':
				n := 0
				for ; i < len(format) && format[i] >= '0' && format[i] <= '9'; i++ {
					n = min(n*10+int(format[i]-'0'), fmtMaxPad)
				}
				i--
				pad += n
				prec = n
			case c == '.' || c == '+' || c == '-' || c == '#' || c == ' ' || c == '[' || c == ']':
			default:
				if c == 'f' || c == 'F' {
					// %f of 1e308 has 309 integer digits.
					float = max(float, 330+prec)
				}
				goto next
			}
		}
	next:
	}
	return pad, float
}

func (w *walker) size(v reflect.Value, depth int) int {
	if depth > maxDepth {
		panic(ErrTooDeep)
	}
	if !v.IsValid() {
		return 5 // <nil>
	}
	t := v.Type()
	name := len(t.String())
	if formatsItself(t) {
		return methodBound + name
	}
	switch t.Kind() {
	case reflect.Bool:
		return 5
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return 68 // %#b of a uint64
	case reflect.Float32, reflect.Float64:
		return w.float
	case reflect.Complex64, reflect.Complex128:
		return 2*w.float + 4
	case reflect.String:
		return 6*v.Len() + 2 // \u0001 in JSON
	case reflect.Interface:
		if v.IsNil() {
			return 5 + name
		}
		return name + w.size(v.Elem(), depth+1)
	case reflect.Pointer:
		if v.IsNil() {
			return 5 + name
		}
		if !w.json && depth > 0 {
			return 40 + name // fmt prints an inner pointer as an address
		}
		if w.cyclic(v.Pointer(), t, 1) {
			return 0
		}
		defer w.leave(v.Pointer(), t, 1)
		return name + 4 + w.size(v.Elem(), depth+1)
	case reflect.Map:
		if v.IsNil() {
			return 5 + name
		}
		if w.cyclic(v.Pointer(), t, 0) {
			return 0
		}
		defer w.leave(v.Pointer(), t, 0)
		n := name + 2
		for it := v.MapRange(); it.Next(); {
			n += w.size(it.Key(), depth+1) + w.size(it.Value(), depth+1) + 4
		}
		return n
	case reflect.Slice, reflect.Array:
		if t.Kind() == reflect.Slice {
			if v.IsNil() {
				return 5 + name
			}
			if w.cyclic(v.Pointer(), t, v.Len()) {
				return 0
			}
			defer w.leave(v.Pointer(), t, v.Len())
		}
		elem := t.Elem()
		per := len(elem.String()) + 2
		switch elem.Kind() {
		case reflect.Uint8:
			return name + 2 + 6*v.Len() // 0x61, for %#v
		case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
			reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
			if !formatsItself(elem) && v.Len() > 0 {
				return name + 2 + v.Len()*(per+w.size(v.Index(0), depth+1))
			}
		}
		if elem.Size() == 0 && v.Len() > 0 {
			// Zero-size elements cost no memory, so their count is not
			// charged anywhere else; each prints the same way.
			return name + 2 + v.Len()*(per+w.size(v.Index(0), depth+1))
		}
		n := name + 2
		for i := 0; i < v.Len(); i++ {
			n += per + w.size(v.Index(i), depth+1)
		}
		return n
	case reflect.Struct:
		n := name + 2
		for i := 0; i < v.NumField(); i++ {
			n += len(t.Field(i).Name) + 4 + w.size(v.Field(i), depth+1)
		}
		return n
	}
	return 40 + name // chan, func, unsafe.Pointer: an address
}

// cyclic marks the data at p as on the current path. When it already is,
// json reports the cycle itself, so cyclic returns true; for fmt it panics
// with ErrCyclicValue.
func (w *walker) cyclic(p uintptr, t reflect.Type, n int) bool {
	key := visit{p, t, n}
	if w.path[key] {
		if w.json {
			return true
		}
		panic(ErrCyclicValue)
	}
	w.path[key] = true
	return false
}

func (w *walker) leave(p uintptr, t reflect.Type, n int) { delete(w.path, visit{p, t, n}) }
