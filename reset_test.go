package ixgo_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goplus/ixgo"
)

const resetSrc = `package main

type counter struct{ n int }

func (c *counter) add() int { c.n++; return c.n }

var c counter
var plain int
var start = 10

func Count() int { plain++; start++; return c.add()*100 + plain*10 + start - 11 }

func Work() int { s := 0; for i := 0; i < 3000; i++ { s += i }; return s }

func GoBoom() { done := make(chan bool); go func() { defer close(done); panic("boom") }(); <-done }

func GoStep() { done := make(chan bool); go func() { defer close(done); for {} }(); <-done }

var never = make(chan bool)

func Leak() { go func() { <-never }() }

func main() {}
`

func resetInterp(t *testing.T, setup func(*ixgo.Context)) *ixgo.Interp {
	t.Helper()
	ctx := ixgo.NewContext(ixgo.SupportMultipleInterp)
	setup(ctx)
	pkg, err := ctx.LoadFile("main.go", resetSrc)
	if err != nil {
		t.Fatal(err)
	}
	in, err := ctx.NewInterp(pkg)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func call(t *testing.T, in *ixgo.Interp, name string) (any, error) {
	t.Helper()
	if err := in.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := in.RunFunc("init"); err != nil {
		t.Fatal(err)
	}
	r, err := in.RunFunc(name)
	// A goroutine records its panic after its deferred calls ran.
	for end := time.Now().Add(time.Second); in.Goroutines() > 0 && time.Now().Before(end); {
		time.Sleep(time.Millisecond)
	}
	if err == nil {
		err = in.Err()
	}
	return r, err
}

// After Reset and init, an interpreter runs its program as a new one would:
// package variables, also of a type with methods and one without an
// initial value, start again, and the step limit counts from zero.
func TestResetRunsTheProgramAgain(t *testing.T) {
	in := resetInterp(t, steps(5000))
	for k := range 3 {
		if r, err := call(t, in, "Count"); err != nil || r != 110 {
			t.Fatalf("Count %d = %v, %v; want 110", k, r, err)
		}
		if _, err := call(t, in, "Work"); err != nil {
			t.Fatalf("Work %d: %v", k, err)
		}
	}
}

// A goroutine that panics or exceeds a limit while RunFunc runs the program
// stops it, and Err gives the cause; Reset clears it.
func TestErrGivesTheFailureOfAGoroutine(t *testing.T) {
	in := resetInterp(t, steps(1e5))
	if _, err := call(t, in, "GoBoom"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("GoBoom: %v; want the panic", err)
	}
	if _, err := call(t, in, "GoStep"); !errors.Is(err, ixgo.ErrStepLimit) {
		t.Fatalf("GoStep: %v; want ErrStepLimit", err)
	}
	if r, err := call(t, in, "Count"); err != nil || r != 110 {
		t.Fatalf("Count after the failures = %v, %v", r, err)
	}
}

// Reset refuses while a goroutine of the program still runs.
func TestResetRefusesWhileAGoroutineRuns(t *testing.T) {
	in := resetInterp(t, steps(1e5))
	if _, err := call(t, in, "Leak"); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(time.Second); in.Goroutines() == 0 && time.Now().Before(end); {
		time.Sleep(time.Millisecond)
	}
	if n := in.Goroutines(); n != 1 {
		t.Fatalf("Goroutines = %d, want 1", n)
	}
	if err := in.Reset(); err == nil {
		t.Fatal("Reset with a goroutine waiting on a channel: no error")
	}
}
