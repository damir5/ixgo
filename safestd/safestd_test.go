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
	"errors"
	"strings"
	"testing"

	"github.com/goplus/ixgo"
	"github.com/goplus/ixgo/safestd"
)

const maxAlloc = 64 << 20

func run(t *testing.T, imports, body string) error {
	t.Helper()
	ctx := ixgo.NewContext(0)
	ctx.MaxAlloc = maxAlloc
	safestd.Register(ctx)
	src := "package main\n\nimport (" + imports + ")\n\nfunc main() {\n" + body + "\n}\n"
	_, err := ctx.RunFile("main.go", src, nil)
	return err
}

// One call with a small input must not allocate more than the budget.
func TestAmplifyingCallsStop(t *testing.T) {
	cases := map[string][2]string{
		"strings.Repeat":         {`"strings"`, `println(len(strings.Repeat("x", 1<<30)))`},
		"Repeat as a value":      {`"strings"`, `f := strings.Repeat; println(len(f("x", 1<<30)))`},
		"Repeat as an argument":  {`"strings"`, `apply := func(f func(string, int) string) string { return f("x", 1<<30) }; println(len(apply(strings.Repeat)))`},
		"Repeat in a goroutine":  {`"strings"`, `done := make(chan int); go func() { done <- len(strings.Repeat("x", 1<<30)) }(); println(<-done)`},
		"Repeat after recover":   {`"strings"`, `defer func() { recover() }(); println(len(strings.Repeat("x", 1<<30)))`},
		"strings.Join separator": {`"strings"`, `parts := make([]string, 1<<20); println(len(strings.Join(parts, strings.Repeat("x", 1<<10))))`},
		"strings.ReplaceAll":     {`"strings"`, `s := strings.Repeat("a", 1<<20); println(len(strings.ReplaceAll(s, "a", strings.Repeat("b", 1<<10))))`},
		"strings.Replacer":       {`"strings"`, `r := strings.NewReplacer("a", strings.Repeat("b", 1<<10)); println(len(r.Replace(strings.Repeat("a", 1<<20))))`},
		"Builder.Grow":           {`"strings"`, `var b strings.Builder; b.Grow(1 << 30); println(b.Cap())`},
		"Builder in a loop":      {`"strings"`, `var b strings.Builder; s := strings.Repeat("x", 1<<16); for { b.WriteString(s) }`},
		"bytes.Repeat":           {`"bytes"`, `println(len(bytes.Repeat([]byte("x"), 1<<30)))`},
		"Buffer.Grow":            {`"bytes"`, `var b bytes.Buffer; b.Grow(1 << 30); println(b.Cap())`},
		"Fprint into a Buffer":   {`"bytes"; "fmt"`, `var b bytes.Buffer; s := string(make([]byte, 1<<16)); for { fmt.Fprint(&b, s) }`},
		"fmt widths":             {`"fmt"; "strings"`, `args := make([]any, 1000); for i := range args { args[i] = 1 }; println(len(fmt.Sprintf(strings.Repeat("%999999d", 1000), args...)))`},
		"fmt zero-size elements": {`"fmt"`, `println(len(fmt.Sprint(make([]struct{}, 1<<30))))`},
		"strconv.FormatFloat":    {`"strconv"`, `println(len(strconv.FormatFloat(1, 'f', 1<<30, 64)))`},
		"json.MarshalIndent":     {`"encoding/json"; "strings"`, `b, _ := json.MarshalIndent(make([]int, 1<<20), "", strings.Repeat(" ", 1<<10)); println(len(b))`},
		"json.Unmarshal":         {`"encoding/json"; "strings"`, `var v any; println(json.Unmarshal([]byte("["+strings.Repeat("0,", 1<<20)+"0]"), &v))`},
		"Reader.WriteTo":         {`"bytes"; "strings"`, `var b bytes.Buffer; s := strings.Repeat("x", 1<<16); for { strings.NewReader(s).WriteTo(&b) }`},
		"json.Encoder":           {`"bytes"; "encoding/json"; "strings"`, `var b bytes.Buffer; e := json.NewEncoder(&b); s := strings.Repeat("x", 1<<16); for { e.Encode(s) }`},
		"json.Decoder":           {`"encoding/json"; "strings"`, `d := json.NewDecoder(strings.NewReader("["+strings.Repeat("0,", 1<<20)+"0]")); var v any; println(d.Decode(&v))`},
		"AppendInt growing dst":  {`"strconv"`, `b := make([]byte, 40<<20); b = strconv.AppendInt(b, 1, 10); println(cap(b))`},
		"Encoder.SetIndent":      {`"bytes"; "encoding/json"; "strings"`, `var b bytes.Buffer; e := json.NewEncoder(&b); e.SetIndent("", strings.Repeat(" ", 1<<10)); println(e.Encode(make([]int, 1<<20)))`},
		"Unquote copies":         {`"strconv"; "strings"`, `q := strconv.Quote(strings.Repeat("x", 1<<20) + "\n"); var keep []string; for { s, _ := strconv.Unquote(q); keep = append(keep, s) }`},
		"Sscan copies":           {`"fmt"; "strings"`, `in := strings.Repeat("x", 1<<20); var keep []string; for { var s string; fmt.Sscan(in, &s); keep = append(keep, s) }`},
		"maps.Clone":             {`"maps"`, `m := map[int]int{}; for i := 0; i < 1<<21; i++ { m[i] = i }; println(len(maps.Clone(m)))`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if err := run(t, c[0], c[1]); !errors.Is(err, ixgo.ErrAllocLimit) {
				t.Fatalf("got %v, want ErrAllocLimit", err)
			}
		})
	}
}

// fmt overflows the Go stack on a value that contains itself, which kills
// the host process; the wrapper must turn it into a program panic.
func TestCyclicValueIsAProgramPanic(t *testing.T) {
	for name, body := range map[string]string{
		"map":   `m := map[string]any{}; m["m"] = m; fmt.Println(m)`,
		"slice": `s := make([]any, 1); s[0] = s; println(fmt.Sprint(s))`,
	} {
		t.Run(name, func(t *testing.T) {
			err := run(t, `"fmt"`, body)
			if err == nil || !strings.Contains(err.Error(), safestd.ErrCyclicValue.Error()) {
				t.Fatalf("got %v, want a panic with %v", err, safestd.ErrCyclicValue)
			}
		})
	}
}

// The wrapped functions must return what the library returns.
func TestWrappedFunctionsKeepResults(t *testing.T) {
	err := run(t, `"bytes"; "encoding/base64"; "encoding/hex"; "errors"; "encoding/json"; "fmt"; "maps"; "slices"; "strconv"; "strings"; "unicode"`, `
	check := func(got, want string) {
		if got != want {
			panic(fmt.Sprintf("got %q, want %q", got, want))
		}
	}
	check(strings.Repeat("ab", 3), "ababab")
	check(strings.Join([]string{"a", "b", "c"}, ", "), "a, b, c")
	check(strings.Replace("aaaa", "a", "bb", 2), "bbbbaa")
	check(strings.ReplaceAll("abc", "", "-"), "-a-b-c-")
	check(strings.NewReplacer("<", "&lt;", ">", "&gt;").Replace("<b>"), "&lt;b&gt;")
	check(strings.ToUpper("straße"), "STRAßE")
	check(strings.ToUpper("\xff"), "\uFFFD")
	check(strings.Map(func(r rune) rune { return r + 1 }, "abc"), "bcd")
	check(fmt.Sprint(strings.Split("a,b,c", ",")), "[a b c]")
	check(fmt.Sprint(strings.SplitN("a,b,c", ",", 2)), "[a b,c]")
	check(fmt.Sprint(strings.Split("abc", "")), "[a b c]")
	check(fmt.Sprint(strings.Fields(" a  b ")), "[a b]")
	var sb strings.Builder
	sb.Grow(4)
	sb.WriteString("ab")
	sb.WriteByte('c')
	sb.WriteRune('d')
	fmt.Fprintf(&sb, "%03d", 7)
	check(sb.String(), "abcd007")
	check(string(bytes.Repeat([]byte("ab"), 2)), "abab")
	check(string(bytes.ReplaceAll([]byte("aXa"), []byte("a"), []byte("bb"))), "bbXbb")
	check(fmt.Sprint(len(bytes.Split([]byte("a,b"), []byte(",")))), "2")
	var buf bytes.Buffer
	buf.WriteString("x")
	buf.Write([]byte("y"))
	check(buf.String(), "xy")
	check(strconv.FormatFloat(1.5, 'f', 3, 64), "1.500")
	check(strconv.Quote("a\x01"), "\"a\\x01\"")
	check(string(strconv.AppendQuote([]byte("q:"), "z")), "q:\"z\"")
	check(base64.StdEncoding.EncodeToString([]byte("hi")), "aGk=")
	d, _ := base64.StdEncoding.DecodeString("aGk=")
	check(string(d), "hi")
	check(hex.EncodeToString([]byte{1, 255}), "01ff")
	j, _ := json.Marshal(map[string]any{"a": []int{1, 2}, "b": "<"})
	check(string(j), "{\"a\":[1,2],\"b\":\"\\u003c\"}")
	ji, _ := json.MarshalIndent([]int{1}, "", "  ")
	check(string(ji), "[\n  1\n]")
	var eb bytes.Buffer
	enc := json.NewEncoder(&eb)
	enc.SetIndent(">", "\t")
	enc.Encode(map[string]int{"a": 1})
	check(eb.String(), "{\n>\t\"a\": 1\n>}\n")
	check(string(strconv.AppendInt(nil, -42, 10)), "-42")
	uq, _ := strconv.Unquote("\"a\\tb\"")
	check(uq, "a\tb")
	var w1, w2 string
	fmt.Sscan("x y", &w1, &w2)
	check(w1+w2, "xy")
	fr := strings.NewReader("p q")
	fmt.Fscan(fr, &w1)
	fmt.Fscan(fr, &w2)
	check(w1+w2, "pq")
	var rb bytes.Buffer
	rb.WriteString("l1\nl2")
	line, _ := rb.ReadString('\n')
	check(line, "l1\n")
	check(errors.Join(errors.New("a"), errors.New("b")).Error(), "a\nb")
	check(strings.ToUpperSpecial(unicode.TurkishCase, "i"), "\u0130")
	var v map[string]any
	if err := json.Unmarshal(j, &v); err != nil {
		panic(err)
	}
	check(fmt.Sprint(v["a"]), "[1 2]")
	check(fmt.Sprintf("%5.2f|%-4s|%*d|%[1]v", 3.14159, "x", 3, 7), " 3.14|x   |  7|3.14159")
	check(fmt.Errorf("e%d", 1).Error(), "e1")
	check(string(fmt.Appendf([]byte("a"), "%d", 2)), "a2")
	m := maps.Clone(map[string]int{"k": 1})
	check(fmt.Sprint(m), "map[k:1]")
	check(fmt.Sprint(slices.Repeat([]int{1}, 3)), "[1 1 1]")
	type node struct{ next *node }
	n := &node{}
	n.next = n
	check(fmt.Sprintf("%T", n), "*main.node")
`)
	if err != nil {
		t.Fatal(err)
	}
}
