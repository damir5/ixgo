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

package ixgo_test

import (
	"errors"
	"testing"

	"github.com/goplus/ixgo"
)

const testMaxAlloc = 64 << 20

func runLimited(t *testing.T, body string, setup func(*ixgo.Context)) error {
	t.Helper()
	ctx := ixgo.NewContext(0)
	setup(ctx)
	src := "package main\n\n" + body + "\n"
	_, err := ctx.RunFile("main.go", src, nil)
	return err
}

func steps(n int64) func(*ixgo.Context) {
	return func(ctx *ixgo.Context) { ctx.MaxSteps = n }
}

func alloc(n int64) func(*ixgo.Context) {
	return func(ctx *ixgo.Context) { ctx.MaxAlloc = n }
}

// A runaway program must stop with an error, wherever its loop runs.
func TestStepLimit(t *testing.T) {
	cases := map[string]string{
		"loop in main":     `func main() { x := 0; for { x++ } }`,
		"loop in callee":   `func spin() int { x := 0; for { x++; if x < 0 { return x } } }; func main() { println(spin()) }`,
		"goto":             `func main() { x := 0; again: x++; goto again }`,
		"mutual recursion": `func a(n int) int { return b(n + 1) }; func b(n int) int { return a(n + 1) }; func main() { println(a(0)) }`,
		"in a goroutine": `func main() {
	done := make(chan bool)
	go func() { x := 0; for { x++ }; done <- true }()
	<-done
}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := ixgo.NewContext(0)
			ctx.MaxSteps = 1e6
			ctx.MaxCallDepth = 1e5
			_, err := ctx.RunFile("main.go", "package main\n\n"+body+"\n", nil)
			if !errors.Is(err, ixgo.ErrStepLimit) && !errors.Is(err, ixgo.ErrCallDepthLimit) {
				t.Fatalf("got %v, want a step or depth limit error", err)
			}
		})
	}
}

// Unbounded recursion must stop before it overflows the Go stack.
func TestCallDepthLimit(t *testing.T) {
	err := runLimited(t, `func f(n int) int { return f(n+1) + 1 }; func main() { println(f(0)) }`,
		func(ctx *ixgo.Context) { ctx.MaxCallDepth = 1000 })
	if !errors.Is(err, ixgo.ErrCallDepthLimit) {
		t.Fatalf("got %v, want ErrCallDepthLimit", err)
	}
}

// recover() in the program must not swallow a limit.
func TestLimitNotRecoverable(t *testing.T) {
	err := runLimited(t, `func spin() { defer func() { recover() }(); for {} }
func main() { for { spin() } }`, steps(1e5))
	if !errors.Is(err, ixgo.ErrStepLimit) {
		t.Fatalf("got %v, want ErrStepLimit", err)
	}
}

// Each operation that can allocate a lot in one step must be charged before
// it allocates.
func TestAllocLimit(t *testing.T) {
	cases := map[string]string{
		"append":          `func main() { var s [][]byte; for { s = append(s, make([]byte, 1<<20)) } }`,
		"string doubling": `func main() { s := "x"; for { s = s + s } }`,
		"constant make":   `func main() { b := make([]byte, 1<<34); println(len(b)) }`,
		"dynamic make":    `func main() { n := 1; for i := 0; i < 34; i++ { n *= 2 }; b := make([]byte, n); println(len(b)) }`,
		"map fill":        `func main() { m := map[int][64]byte{}; for i := 0; ; i++ { m[i] = [64]byte{} } }`,
		"map reserve":     `func main() { n := 1 << 30; m := make(map[int]int, n); println(len(m)) }`,
		"chan buffer":     `func main() { n := 1 << 30; c := make(chan int64, n); println(cap(c)) }`,
		"conversion": `func main() {
	s := "xxxxxxxxxxxxxxxx"
	for len(s) < 1<<20 { s += s }
	var keep [][]byte
	for { keep = append(keep, []byte(s)) }
}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			err := runLimited(t, body, alloc(testMaxAlloc))
			if !errors.Is(err, ixgo.ErrAllocLimit) {
				t.Fatalf("got %v, want ErrAllocLimit", err)
			}
		})
	}
}

// Growth that is not charged (here: boxing in a long loop) is caught by the
// process heap sample.
func TestHeapAllocLimit(t *testing.T) {
	err := runLimited(t, `type T struct{ a [256]byte }
func keep(v interface{}) interface{} { return v }
func main() { for { _ = keep(T{}) } }`,
		func(ctx *ixgo.Context) { ctx.MaxHeapAlloc = 8 << 20 })
	if !errors.Is(err, ixgo.ErrHeapAllocLimit) {
		t.Fatalf("got %v, want ErrHeapAllocLimit", err)
	}
}

// A normal program runs unchanged under limits.
func TestLimitsAllowNormalProgram(t *testing.T) {
	err := runLimited(t, `import "strings"
func main() {
	var parts []string
	m := map[string]int{}
	for i := 0; i < 1000; i++ {
		parts = append(parts, "x")
		m[strings.Repeat("k", i%10)] = i
	}
	if len(strings.Join(parts, "")) != 1000 || len(m) != 10 {
		panic("wrong result")
	}
}`, func(ctx *ixgo.Context) {
		ctx.MaxSteps, ctx.MaxAlloc, ctx.MaxHeapAlloc, ctx.MaxCallDepth = 1e6, testMaxAlloc, 4*testMaxAlloc, 1000
	})
	if err != nil {
		t.Fatal(err)
	}
}

func BenchmarkLoopNoLimits(b *testing.B) { benchLoop(b, nil) }

func BenchmarkLoopWithLimits(b *testing.B) {
	benchLoop(b, func(ctx *ixgo.Context) { ctx.MaxSteps, ctx.MaxAlloc = 1e12, 1 << 40 })
}

func benchLoop(b *testing.B, setup func(*ixgo.Context)) {
	src := `package main

func add(a, b int) int { return a + b }

func main() {
	t := 0
	for i := 0; i < 1000000; i++ {
		t = add(t, i)
	}
	if t != 499999500000 {
		panic(t)
	}
}
`
	for b.Loop() {
		ctx := ixgo.NewContext(0)
		if setup != nil {
			setup(ctx)
		}
		if _, err := ctx.RunFile("main.go", src, nil); err != nil {
			b.Fatal(err)
		}
	}
}
