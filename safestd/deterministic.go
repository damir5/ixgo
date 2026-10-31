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
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/goplus/ixgo"
	_ "github.com/goplus/ixgo/pkg/math/rand/v2"
	_ "github.com/goplus/ixgo/pkg/time"
)

// DeterministicPackages are the imports Restrict also allows once
// Deterministic has set up a Context.
var DeterministicPackages = []string{"time", "math/rand/v2"}

var (
	errTimer   = errors.New("safestd: timers are not available in a deterministic run")
	errZone    = errors.New("safestd: time zones other than UTC are not available in a deterministic run")
	errAddress = errors.New("safestd: printing a memory address is not allowed in a deterministic run")
)

// Clock is the time a deterministic program reads. It starts at the run's
// start and moves only when the host advances it, to the time recorded with
// each tool result, or when the program sleeps; it never goes back.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// Now returns the clock's time, in UTC and without a monotonic reading.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock to t when t is later.
func (c *Clock) Advance(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t = t.UTC().Round(0); t.After(c.now) {
		c.now = t
	}
}

// Deterministic sets ctx up so that a program run again with the same start,
// seed and tool results makes the same calls and gives the same output:
// ixgo's Context.Deterministic (maps in key order, no goroutines or
// multi-case selects), time from the returned Clock in UTC, no timers, the
// top-level math/rand/v2 functions drawn from seed, and no memory addresses
// in fmt output. Call it after Register and before Restrict.
func Deterministic(ctx *ixgo.Context, start time.Time, seed uint64) *Clock {
	ctx.Deterministic = true
	c := &Clock{now: start.UTC().Round(0)}
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	utc := time.UTC
	ctx.RegisterExternal("time.Local", &utc)
	timer := func() { panic(errTimer) }
	for name, fn := range map[string]any{
		"time.Now":          c.Now,
		"time.Since":        func(t time.Time) time.Duration { return c.Now().Sub(t) },
		"time.Until":        func(t time.Time) time.Duration { return t.Sub(c.Now()) },
		"time.Sleep":        func(d time.Duration) { c.Advance(c.Now().Add(d)) },
		"time.Unix":         func(sec, nsec int64) time.Time { return time.Unix(sec, nsec).UTC() },
		"time.UnixMilli":    func(msec int64) time.Time { return time.UnixMilli(msec).UTC() },
		"time.UnixMicro":    func(usec int64) time.Time { return time.UnixMicro(usec).UTC() },
		"(time.Time).Local": func(t time.Time) time.Time { return t.UTC() },
		"(time.Time).Format": func(t time.Time, layout string) string {
			done := reserve(formatBound(t, layout))
			out := t.Format(layout)
			done(len(out))
			return out
		},
		"(time.Time).AppendFormat": func(t time.Time, b []byte, layout string) []byte {
			before := cap(b)
			b = t.AppendFormat(b, layout)
			grew(before, cap(b))
			return b
		},
		"time.LoadLocation":        loadUTC,
		"time.After":               func(time.Duration) <-chan time.Time { timer(); return nil },
		"time.Tick":                func(time.Duration) <-chan time.Time { timer(); return nil },
		"time.NewTimer":            func(time.Duration) *time.Timer { timer(); return nil },
		"time.NewTicker":           func(time.Duration) *time.Ticker { timer(); return nil },
		"time.AfterFunc":           func(time.Duration, func()) *time.Timer { timer(); return nil },
		"math/rand/v2.ExpFloat64":  r.ExpFloat64,
		"math/rand/v2.Float32":     r.Float32,
		"math/rand/v2.Float64":     r.Float64,
		"math/rand/v2.Int":         r.Int,
		"math/rand/v2.Int32":       r.Int32,
		"math/rand/v2.Int32N":      r.Int32N,
		"math/rand/v2.Int64":       r.Int64,
		"math/rand/v2.Int64N":      r.Int64N,
		"math/rand/v2.IntN":        r.IntN,
		"math/rand/v2.NormFloat64": r.NormFloat64,
		"math/rand/v2.Perm":        func(n int) []int { ixgo.ChargeElems(n, 8); return r.Perm(n) },
		"math/rand/v2.Shuffle":     r.Shuffle,
		"math/rand/v2.Uint":        r.Uint,
		"math/rand/v2.Uint32":      r.Uint32,
		"math/rand/v2.Uint32N":     r.Uint32N,
		"math/rand/v2.Uint64":      r.Uint64,
		"math/rand/v2.Uint64N":     r.Uint64N,
		"math/rand/v2.UintN":       r.UintN,
	} {
		ctx.RegisterExternal(name, fn)
	}
	return c
}

// formatBound bounds Time.Format: no layout element is shorter than a
// third of what it prints, except the zone name, which can be any length.
func formatBound(t time.Time, layout string) int {
	zone, _ := t.Zone()
	return 3*len(layout) + strings.Count(layout, "MST")*len(zone)
}

func loadUTC(name string) (*time.Location, error) {
	if name == "" || name == "UTC" {
		return time.UTC, nil
	}
	return nil, errZone
}

// refuseAddresses panics when fmt would print a memory address for format
// and args: the %p verb, or a pointer, channel, function or unsafe pointer
// that fmt prints as an address. A value with an Error or String method is
// printed by that method, so it is not walked.
//
// ponytail: refuses every pointer below the top level, including some that a
// verb such as %s would not print as an address.
func refuseAddresses(format string, args []any) {
	if !ixgo.InDeterministicRun() {
		return
	}
	if hasVerb(format, 'p') {
		panic(errAddress)
	}
	for _, a := range args {
		if prints := addressIn(reflect.ValueOf(a), 0); prints {
			panic(fmt.Errorf("%w: %T", errAddress, a))
		}
	}
}

// hasVerb reports whether format has the verb v, with any flags, width or
// precision before it.
func hasVerb(format string, v byte) bool {
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		for i < len(format) && strings.IndexByte("+-# 0123456789.*[]", format[i]) >= 0 {
			i++
		}
		if i < len(format) && format[i] == v {
			return true
		}
	}
	return false
}

func addressIn(v reflect.Value, depth int) bool {
	if !v.IsValid() || depth > 32 {
		return false
	}
	if v.CanInterface() {
		switch v.Interface().(type) {
		case error, fmt.Stringer:
			return false
		}
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return false
		}
		if e := v.Elem().Kind(); depth == 0 && (e == reflect.Struct || e == reflect.Array || e == reflect.Slice || e == reflect.Map) {
			return addressIn(v.Elem(), depth+1)
		}
		return true
	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return !v.IsNil()
	case reflect.Interface:
		return addressIn(v.Elem(), depth)
	case reflect.Struct:
		for i := range v.NumField() {
			if addressIn(v.Field(i), depth+1) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if addressIn(v.Index(i), depth+1) {
				return true
			}
		}
	case reflect.Map:
		it := v.MapRange()
		for it.Next() {
			if addressIn(it.Key(), depth+1) || addressIn(it.Value(), depth+1) {
				return true
			}
		}
	}
	return false
}
