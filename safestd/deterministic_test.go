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

package safestd_test

import (
	"bytes"
	"errors"
	"runtime/metrics"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goplus/ixgo"
	"github.com/goplus/ixgo/safestd"
)

var start = time.Date(2026, 10, 31, 12, 0, 0, 0, time.FixedZone("CET", 3600))

// runDet runs body once in a deterministic Context and returns what it
// printed with println.
func runDet(t *testing.T, seed uint64, imports, body string) (string, error) {
	t.Helper()
	ctx := ixgo.NewContext(0)
	ctx.MaxAlloc = maxAlloc
	safestd.Register(ctx)
	clock := safestd.Deterministic(ctx, start, seed)
	safestd.Restrict(ctx)
	ctx.RegisterExternal("main.advance", func(sec int) { clock.Advance(clock.Now().Add(time.Duration(sec) * time.Second)) })
	var out bytes.Buffer
	ctx.SetPrintOutput(&out)
	src := "package main\n\nimport (" + imports + ")\n\nfunc advance(sec int)\n\n" + body + "\n"
	_, err := ctx.RunFile("main.go", src, nil)
	return out.String(), err
}

const sameEveryRun = `func main() {
	m := map[string]int{}
	for i := 0; i < 50; i++ { m[fmt.Sprint("k", i)] = i }
	var keys []string
	for k := range m { keys = append(keys, k) }
	println(strings.Join(keys, ","))
	println(strings.Join(slices.Collect(maps.Keys(m)), ","))
	println(rand.IntN(1000000), rand.Float64(), fmt.Sprint(rand.Perm(5)))
	t0 := time.Now()
	time.Sleep(90 * time.Second)
	advance(30)
	println(t0.Format(time.RFC3339), time.Since(t0).String(), time.Now().Location().String(), time.Local.String())
}`

const detImports = `"fmt"; "maps"; "math/rand/v2"; "slices"; "strings"; "time"`

func TestDeterministicRunRepeats(t *testing.T) {
	first, err := runDet(t, 7, detImports, sameEveryRun)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		again, err := runDet(t, 7, detImports, sameEveryRun)
		if err != nil || again != first {
			t.Fatalf("run differs or failed (%v):\n%s\nwant:\n%s", err, again, first)
		}
	}
	lines := strings.Split(first, "\n")
	keys := strings.Split(lines[0], ",")
	if !slices.IsSorted(keys) || lines[1] != lines[0] {
		t.Fatalf("map order is not key order:\n%s\n%s", lines[0], lines[1])
	}
	if !strings.HasPrefix(lines[3], "2026-10-31T11:00:00Z 2m0s UTC UTC") {
		t.Fatalf("time: %s", lines[3])
	}
	other, _ := runDet(t, 8, detImports, sameEveryRun)
	if strings.Split(other, "\n")[2] == lines[2] {
		t.Fatal("another seed gave the same random numbers")
	}
	t.Logf("\n%s", first)
}

func TestDeterministicRunRefuses(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"go statement":       {`func main() { go println(1) }`, ixgo.ErrNondeterministic.Error()},
		"two-case select":    {`func main() { a, b := make(chan int, 1), make(chan int, 1); a <- 1; b <- 1; select { case <-a: case <-b: } }`, ixgo.ErrNondeterministic.Error()},
		"timer":              {`func main() { <-time.After(time.Second) }`, "timers are not available"},
		"%p":                 {`func main() { x := 1; println(fmt.Sprintf("%+p", &x)) }`, "memory address"},
		"pointer to int":     {`func main() { x := 1; println(fmt.Sprint(&x)) }`, "memory address"},
		"pointer field":      {`type T struct{ P *int }; func main() { x := 1; println(fmt.Sprint(T{&x})) }`, "memory address"},
		"pointer map key":    {`func main() { a, b := 1, 2; for k := range map[*int]bool{&a: true, &b: true} { _ = k } }`, "have no fixed order"},
		"other time zone":    {`func main() { if _, err := time.LoadLocation("Europe/Zagreb"); err != nil { panic(err) } }`, "time zones other than UTC"},
		"one-case select ok": {`func main() { a := make(chan int, 1); select { case <-a: default: }; _ = time.Now; _ = fmt.Sprint }`, ""},
		"struct pointer ok":  {`type T struct{ N int }; func main() { println(fmt.Sprint(&T{1}), fmt.Sprintf("%%p")); _ = time.Now }`, ""},
		"error value ok":     {`func main() { println(fmt.Sprint(fmt.Errorf("x"))); _ = time.Now }`, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := runDet(t, 1, `"fmt"; "strings"; "time"`, "var _, _, _ = fmt.Sprint, strings.Repeat, time.Now\n"+c.body)
			if c.want == "" && err != nil {
				t.Fatalf("got %v, want no error", err)
			}
			if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("got %v, want an error with %q", err, c.want)
			}
		})
	}
}

// Time.Format is charged before it allocates: the program stops without
// the host allocating the output. A zone name is the input that multiplies;
// a layout prints at most a few times its own length.
func TestTimeFormatIsCharged(t *testing.T) {
	cases := map[string]string{
		"long zone name": `func main() { z := time.FixedZone(strings.Repeat("Z", 1<<20), 0); println(len(time.Now().In(z).Format(strings.Repeat("MST", 1000)))) }`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			a := heapAllocs()
			_, err := runDet(t, 1, `"strings"; "time"`, body)
			grew := (heapAllocs() - a) >> 20
			if !errors.Is(err, ixgo.ErrAllocLimit) || grew > 512 {
				t.Fatalf("got %v after %d MiB of host allocation, want ErrAllocLimit before the output is allocated", err, grew)
			}
			t.Logf("%v after %d MiB of host allocation", err, grew)
		})
	}
}

func heapAllocs() uint64 {
	s := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// Range skips a key deleted before it is reached and reads the value the
// key has when it is reached, as Go's own range does.
func TestSortedRangeFollowsChanges(t *testing.T) {
	out, err := runDet(t, 1, `"fmt"`, `func main() {
	m := map[int]int{1: 10, 2: 20, 3: 30}
	s := ""
	for k, v := range m {
		if k == 1 { delete(m, 2); m[3] = 33 }
		s += fmt.Sprint(k, "=", v, " ")
	}
	println(s)
}`)
	if err != nil || out != "1=10 3=33 \n" {
		t.Fatalf("got %q, %v", out, err)
	}
}

// Without Deterministic, programs keep goroutines and Go's map order.
func TestDeterministicIsOptIn(t *testing.T) {
	err := run(t, `"fmt"`, `done := make(chan bool); go func() { done <- true }(); <-done; fmt.Sprint(1)`)
	if err != nil && errors.Is(err, ixgo.ErrNondeterministic) {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
}
