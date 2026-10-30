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

// Package safestd replaces standard library functions that allocate inside
// Go code with versions that charge Context.MaxAlloc first. SPEC.md lists
// every function, the bound it charges, and what stays uncovered.
//
//	ctx := ixgo.NewContext(0)
//	ctx.MaxAlloc = 256 << 20
//	safestd.Register(ctx)
//	safestd.Restrict(ctx, "example.com/tools") // plus the host's packages
package safestd

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/goplus/ixgo"
	_ "github.com/goplus/ixgo/pkg/bytes"
	_ "github.com/goplus/ixgo/pkg/cmp"
	_ "github.com/goplus/ixgo/pkg/encoding/base64"
	_ "github.com/goplus/ixgo/pkg/encoding/hex"
	_ "github.com/goplus/ixgo/pkg/encoding/json"
	_ "github.com/goplus/ixgo/pkg/errors"
	_ "github.com/goplus/ixgo/pkg/fmt"
	_ "github.com/goplus/ixgo/pkg/iter"
	_ "github.com/goplus/ixgo/pkg/maps"
	_ "github.com/goplus/ixgo/pkg/math"
	_ "github.com/goplus/ixgo/pkg/math/bits"
	_ "github.com/goplus/ixgo/pkg/slices"
	_ "github.com/goplus/ixgo/pkg/sort"
	_ "github.com/goplus/ixgo/pkg/strconv"
	_ "github.com/goplus/ixgo/pkg/strings"
	_ "github.com/goplus/ixgo/pkg/unicode"
	_ "github.com/goplus/ixgo/pkg/unicode/utf8"
)

// Packages lists the import paths Register covers. Restrict lets programs
// import only these and the host's own packages.
var Packages = []string{
	"bytes", "cmp", "encoding/base64", "encoding/hex", "encoding/json", "errors",
	"fmt", "iter", "maps", "math", "math/bits", "slices", "sort", "strconv",
	"strings", "unicode", "unicode/utf8",
}

// mapsClone is the runtime map copy that ixgo links maps.Clone to.
var mapsClone = func() reflect.Value {
	pkg, _ := ixgo.LookupPackage("maps")
	return pkg.Funcs["clone"]
}()

// Register installs the wrapped functions on ctx.
func Register(ctx *ixgo.Context) {
	for name, fn := range funcs() {
		ctx.RegisterExternal(name, fn)
	}
}

const (
	sliceHeader  = 24 // bytes of a slice header
	stringHeader = 16 // bytes of a string header
)

// reserve charges an upper bound before a call and returns a function that
// refunds the part of it the call did not keep.
func reserve(bound int) func(kept int) {
	ixgo.Charge(int64(bound))
	return func(kept int) {
		if kept < bound {
			ixgo.Refund(int64(bound - kept))
		}
	}
}

// grew charges the new backing array when a call grew a buffer's capacity.
func grew(before, after int) { ixgo.Charge(int64(newArray(before, after))) }

// newArray is the size of the backing array a call allocated when it grew
// a slice from capacity before to after, or 0.
func newArray(before, after int) int {
	if after > before {
		return after
	}
	return 0
}

// grow charges n bytes before a Grow call and the rest of the new backing
// array after it.
func grow(n int, capacity func() int, call func()) {
	if n > 0 {
		ixgo.Charge(int64(n))
	}
	c := capacity()
	call()
	if after := capacity(); after > c {
		ixgo.Charge(int64(max(after-max(n, 0), 0)))
	}
}

// count is the number of non-overlapping matches of sep in s, as Replace and
// Split see them, limited to n when n >= 0.
func count(s, sep string, n int) int {
	c := strings.Count(s, sep)
	if n >= 0 && c > n {
		c = n
	}
	return c
}

func joinSize(n, total, sep int) int {
	if n == 0 {
		return 0
	}
	return total + (n-1)*sep
}

func replaceSize(s, old, new string, n int) int {
	return len(s) + count(s, old, n)*(len(new)-len(old))
}

// splitBound is the bytes of the slice that Split returns.
func splitBound(s, sep string, n int) int {
	if n == 0 {
		return 0
	}
	parts := count(s, sep, -1) + 1
	if sep == "" {
		parts = utf8.RuneCountInString(s)
	}
	if n > 0 && parts > n {
		parts = n
	}
	return parts * stringHeader
}

func funcs() map[string]any {
	var replacers sync.Map // *strings.Replacer -> longest replacement
	var encoders sync.Map  // *json.Encoder -> *encoder
	return map[string]any{
		// strings: output size set by an argument, charged exactly.
		"strings.Repeat": func(s string, n int) string {
			if n > 0 {
				ixgo.ChargeElems(n, uintptr(len(s)))
			}
			return strings.Repeat(s, n)
		},
		"strings.Join": func(elems []string, sep string) string {
			total := 0
			for _, e := range elems {
				total += len(e)
			}
			ixgo.Charge(int64(joinSize(len(elems), total, len(sep))))
			return strings.Join(elems, sep)
		},
		"strings.Replace": func(s, old, new string, n int) string {
			ixgo.Charge(int64(replaceSize(s, old, new, n)))
			return strings.Replace(s, old, new, n)
		},
		"strings.ReplaceAll": func(s, old, new string) string {
			ixgo.Charge(int64(replaceSize(s, old, new, -1)))
			return strings.ReplaceAll(s, old, new)
		},
		"strings.ToValidUTF8": func(s, rep string) string {
			done := reserve(len(s) * (1 + len(rep)))
			r := strings.ToValidUTF8(s, rep)
			done(len(r))
			return r
		},
		"strings.NewReplacer": func(oldnew ...string) *strings.Replacer {
			r := strings.NewReplacer(oldnew...)
			longest := 0
			for i := 1; i < len(oldnew); i += 2 {
				longest = max(longest, len(oldnew[i]))
			}
			replacers.Store(r, longest)
			return r
		},
		"(*strings.Replacer).Replace": func(r *strings.Replacer, s string) string {
			longest, _ := replacers.Load(r)
			done := reserve((len(s) + 1) * (longest.(int) + 1))
			out := r.Replace(s)
			done(len(out))
			return out
		},
		"(*strings.Replacer).WriteString": func(r *strings.Replacer, w io.Writer, s string) (int, error) {
			longest, _ := replacers.Load(r)
			done := reserve((len(s) + 1) * (longest.(int) + 1))
			defer chargeGrowth(w)()
			n, err := r.WriteString(w, s)
			done(n)
			return n, err
		},
		"(*strings.Builder).Grow": func(b *strings.Builder, n int) { grow(n, b.Cap, func() { b.Grow(n) }) },
		"(*strings.Builder).Write": func(b *strings.Builder, p []byte) (int, error) {
			c := b.Cap()
			defer func() { grew(c, b.Cap()) }()
			return b.Write(p)
		},
		"(*strings.Builder).WriteString": func(b *strings.Builder, s string) (int, error) {
			c := b.Cap()
			defer func() { grew(c, b.Cap()) }()
			return b.WriteString(s)
		},
		"(*strings.Builder).WriteByte": func(b *strings.Builder, c byte) error {
			n := b.Cap()
			defer func() { grew(n, b.Cap()) }()
			return b.WriteByte(c)
		},
		"(*strings.Builder).WriteRune": func(b *strings.Builder, r rune) (int, error) {
			c := b.Cap()
			defer func() { grew(c, b.Cap()) }()
			return b.WriteRune(r)
		},
		"strings.Clone": func(s string) string { ixgo.Charge(int64(len(s))); return strings.Clone(s) },

		// strings: output a fixed multiple of the input at most.
		"strings.ToUpper":        caseString(strings.ToUpper),
		"strings.ToLower":        caseString(strings.ToLower),
		"strings.ToTitle":        caseString(strings.ToTitle),
		"strings.Title":          caseString(strings.Title), //nolint:staticcheck // still in the library
		"strings.ToUpperSpecial": caseSpecialString(strings.ToUpperSpecial),
		"strings.ToLowerSpecial": caseSpecialString(strings.ToLowerSpecial),
		"strings.ToTitleSpecial": caseSpecialString(strings.ToTitleSpecial),
		"strings.Map": func(f func(rune) rune, s string) string {
			done := reserve(4 * len(s))
			r := strings.Map(f, s)
			done(len(r))
			return r
		},
		"strings.Split": func(s, sep string) []string { ixgo.Charge(int64(splitBound(s, sep, -1))); return strings.Split(s, sep) },
		"strings.SplitN": func(s, sep string, n int) []string {
			ixgo.Charge(int64(splitBound(s, sep, n)))
			return strings.SplitN(s, sep, n)
		},
		"strings.SplitAfter": func(s, sep string) []string {
			ixgo.Charge(int64(splitBound(s, sep, -1)))
			return strings.SplitAfter(s, sep)
		},
		"strings.SplitAfterN": func(s, sep string, n int) []string {
			ixgo.Charge(int64(splitBound(s, sep, n)))
			return strings.SplitAfterN(s, sep, n)
		},
		"strings.Fields": func(s string) []string {
			done := reserve((len(s)/2 + 1) * stringHeader)
			r := strings.Fields(s)
			done(len(r) * stringHeader)
			return r
		},
		"strings.FieldsFunc": func(s string, f func(rune) bool) []string {
			done := reserve((len(s)/2 + 1) * stringHeader)
			r := strings.FieldsFunc(s, f)
			done(len(r) * stringHeader)
			return r
		},

		// bytes: the same rules as strings.
		"bytes.Repeat": func(b []byte, n int) []byte {
			if n > 0 {
				ixgo.ChargeElems(n, uintptr(len(b)))
			}
			return bytes.Repeat(b, n)
		},
		"bytes.Join": func(s [][]byte, sep []byte) []byte {
			total := 0
			for _, e := range s {
				total += len(e)
			}
			ixgo.Charge(int64(joinSize(len(s), total, len(sep))))
			return bytes.Join(s, sep)
		},
		"bytes.Replace": func(s, old, new []byte, n int) []byte {
			ixgo.Charge(int64(replaceSize(string(s), string(old), string(new), n)))
			return bytes.Replace(s, old, new, n)
		},
		"bytes.ReplaceAll": func(s, old, new []byte) []byte {
			ixgo.Charge(int64(replaceSize(string(s), string(old), string(new), -1)))
			return bytes.ReplaceAll(s, old, new)
		},
		"bytes.ToValidUTF8": func(s, rep []byte) []byte {
			done := reserve(len(s) * (1 + len(rep)))
			r := bytes.ToValidUTF8(s, rep)
			done(len(r))
			return r
		},
		"bytes.ToUpper":        caseBytes(bytes.ToUpper),
		"bytes.ToLower":        caseBytes(bytes.ToLower),
		"bytes.ToTitle":        caseBytes(bytes.ToTitle),
		"bytes.Title":          caseBytes(bytes.Title), //nolint:staticcheck // still in the library
		"bytes.ToUpperSpecial": caseSpecialBytes(bytes.ToUpperSpecial),
		"bytes.ToLowerSpecial": caseSpecialBytes(bytes.ToLowerSpecial),
		"bytes.ToTitleSpecial": caseSpecialBytes(bytes.ToTitleSpecial),
		"bytes.Map": func(f func(rune) rune, s []byte) []byte {
			done := reserve(4 * len(s))
			r := bytes.Map(f, s)
			done(len(r))
			return r
		},
		"bytes.Runes": func(s []byte) []rune { ixgo.ChargeElems(utf8.RuneCount(s), 4); return bytes.Runes(s) },
		"bytes.Clone": func(b []byte) []byte { ixgo.Charge(int64(len(b))); return bytes.Clone(b) },
		"bytes.Split": func(s, sep []byte) [][]byte {
			ixgo.Charge(int64(splitBound(string(s), string(sep), -1) / stringHeader * sliceHeader))
			return bytes.Split(s, sep)
		},
		"bytes.SplitN": func(s, sep []byte, n int) [][]byte {
			ixgo.Charge(int64(splitBound(string(s), string(sep), n) / stringHeader * sliceHeader))
			return bytes.SplitN(s, sep, n)
		},
		"bytes.SplitAfter": func(s, sep []byte) [][]byte {
			ixgo.Charge(int64(splitBound(string(s), string(sep), -1) / stringHeader * sliceHeader))
			return bytes.SplitAfter(s, sep)
		},
		"bytes.SplitAfterN": func(s, sep []byte, n int) [][]byte {
			ixgo.Charge(int64(splitBound(string(s), string(sep), n) / stringHeader * sliceHeader))
			return bytes.SplitAfterN(s, sep, n)
		},
		"bytes.FieldsFunc": func(s []byte, f func(rune) bool) [][]byte {
			done := reserve((len(s)/2 + 1) * sliceHeader)
			r := bytes.FieldsFunc(s, f)
			done(len(r) * sliceHeader)
			return r
		},
		"bytes.Fields": func(s []byte) [][]byte {
			done := reserve((len(s)/2 + 1) * sliceHeader)
			r := bytes.Fields(s)
			done(len(r) * sliceHeader)
			return r
		},
		"bytes.NewBufferString": func(s string) *bytes.Buffer {
			ixgo.Charge(int64(len(s)))
			return bytes.NewBufferString(s)
		},
		"(*bytes.Buffer).Grow": func(b *bytes.Buffer, n int) { grow(n, b.Cap, func() { b.Grow(n) }) },
		"(*bytes.Buffer).Write": func(b *bytes.Buffer, p []byte) (int, error) {
			c := b.Cap()
			defer func() { grew(c, b.Cap()) }()
			return b.Write(p)
		},
		"(*bytes.Buffer).WriteString": func(b *bytes.Buffer, s string) (int, error) {
			c := b.Cap()
			defer func() { grew(c, b.Cap()) }()
			return b.WriteString(s)
		},
		"(*bytes.Buffer).WriteByte": func(b *bytes.Buffer, c byte) error {
			n := b.Cap()
			defer func() { grew(n, b.Cap()) }()
			return b.WriteByte(c)
		},
		"(*bytes.Buffer).WriteRune": func(b *bytes.Buffer, r rune) (int, error) {
			c := b.Cap()
			defer func() { grew(c, b.Cap()) }()
			return b.WriteRune(r)
		},
		"(*bytes.Buffer).ReadFrom": func(b *bytes.Buffer, r io.Reader) (int64, error) {
			c := b.Cap()
			defer func() { grew(c, b.Cap()) }()
			return b.ReadFrom(r)
		},
		"(*bytes.Buffer).WriteTo": func(b *bytes.Buffer, w io.Writer) (int64, error) {
			defer chargeGrowth(w)()
			return b.WriteTo(w)
		},
		"(*bytes.Reader).WriteTo": func(r *bytes.Reader, w io.Writer) (int64, error) {
			defer chargeGrowth(w)()
			return r.WriteTo(w)
		},
		"(*strings.Reader).WriteTo": func(r *strings.Reader, w io.Writer) (int64, error) {
			defer chargeGrowth(w)()
			return r.WriteTo(w)
		},
		"(*bytes.Buffer).ReadBytes": func(b *bytes.Buffer, delim byte) ([]byte, error) {
			done := reserve(b.Len())
			r, err := b.ReadBytes(delim)
			done(len(r))
			return r, err
		},
		"(*bytes.Buffer).ReadString": func(b *bytes.Buffer, delim byte) (string, error) {
			done := reserve(b.Len())
			r, err := b.ReadString(delim)
			done(len(r))
			return r, err
		},
		"(*bytes.Buffer).String": func(b *bytes.Buffer) string { ixgo.Charge(int64(b.Len())); return b.String() },

		// strconv: precision is an argument; quoting is at most 10 bytes per
		// input byte (\U0010ffff for a 4-byte rune is 2.5, \x01 is 4).
		"strconv.FormatFloat": func(f float64, fmt byte, prec, bitSize int) string {
			ixgo.Charge(int64(floatBound(prec)))
			return strconv.FormatFloat(f, fmt, prec, bitSize)
		},
		"strconv.AppendFloat": func(dst []byte, f float64, fmt byte, prec, bitSize int) []byte {
			c := cap(dst)
			ixgo.Charge(int64(floatBound(prec)))
			r := strconv.AppendFloat(dst, f, fmt, prec, bitSize)
			grew(c, cap(r))
			return r
		},
		"strconv.FormatComplex": func(c complex128, fmt byte, prec, bitSize int) string {
			ixgo.Charge(int64(2*floatBound(prec) + 3))
			return strconv.FormatComplex(c, fmt, prec, bitSize)
		},
		"strconv.AppendInt": func(dst []byte, i int64, base int) []byte {
			c := cap(dst)
			r := strconv.AppendInt(dst, i, base)
			grew(c, cap(r))
			return r
		},
		"strconv.AppendUint": func(dst []byte, i uint64, base int) []byte {
			c := cap(dst)
			r := strconv.AppendUint(dst, i, base)
			grew(c, cap(r))
			return r
		},
		"strconv.AppendBool":               appendGrowth(strconv.AppendBool),
		"strconv.AppendQuoteRune":          appendGrowth(strconv.AppendQuoteRune),
		"strconv.AppendQuoteRuneToASCII":   appendGrowth(strconv.AppendQuoteRuneToASCII),
		"strconv.AppendQuoteRuneToGraphic": appendGrowth(strconv.AppendQuoteRuneToGraphic),
		"unicode/utf8.AppendRune":          appendGrowth(utf8.AppendRune),
		"strconv.Unquote": func(s string) (string, error) {
			done := reserve(len(s))
			r, err := strconv.Unquote(s)
			done(len(r))
			return r, err
		},
		"strconv.Quote":                quote(strconv.Quote),
		"strconv.QuoteToASCII":         quote(strconv.QuoteToASCII),
		"strconv.QuoteToGraphic":       quote(strconv.QuoteToGraphic),
		"strconv.AppendQuote":          appendQuote(strconv.AppendQuote),
		"strconv.AppendQuoteToASCII":   appendQuote(strconv.AppendQuoteToASCII),
		"strconv.AppendQuoteToGraphic": appendQuote(strconv.AppendQuoteToGraphic),

		// encoding: output length is a formula of the input length.
		"(*encoding/base64.Encoding).EncodeToString": func(e *base64.Encoding, src []byte) string {
			ixgo.Charge(int64(e.EncodedLen(len(src))))
			return e.EncodeToString(src)
		},
		"(*encoding/base64.Encoding).DecodeString": func(e *base64.Encoding, s string) ([]byte, error) {
			ixgo.Charge(int64(e.DecodedLen(len(s))))
			return e.DecodeString(s)
		},
		"(*encoding/base64.Encoding).AppendEncode": func(e *base64.Encoding, dst, src []byte) []byte {
			c := cap(dst)
			r := e.AppendEncode(dst, src)
			grew(c, cap(r))
			return r
		},
		"(*encoding/base64.Encoding).AppendDecode": func(e *base64.Encoding, dst, src []byte) ([]byte, error) {
			c := cap(dst)
			r, err := e.AppendDecode(dst, src)
			grew(c, cap(r))
			return r, err
		},
		"encoding/hex.EncodeToString": func(src []byte) string { ixgo.Charge(int64(2 * len(src))); return hex.EncodeToString(src) },
		"encoding/hex.DecodeString":   func(s string) ([]byte, error) { ixgo.Charge(int64(len(s) / 2)); return hex.DecodeString(s) },
		"encoding/hex.Dump": func(data []byte) string {
			ixgo.Charge(int64((len(data)/16 + 1) * 79))
			return hex.Dump(data)
		},
		"encoding/hex.AppendEncode": func(dst, src []byte) []byte {
			c := cap(dst)
			r := hex.AppendEncode(dst, src)
			grew(c, cap(r))
			return r
		},
		"encoding/hex.AppendDecode": func(dst, src []byte) ([]byte, error) {
			c := cap(dst)
			r, err := hex.AppendDecode(dst, src)
			grew(c, cap(r))
			return r, err
		},

		// encoding/json: the output bound comes from walking the value.
		"encoding/json.Marshal": func(v any) ([]byte, error) {
			done := reserve(jsonBound(v))
			b, err := json.Marshal(v)
			done(len(b))
			return b, err
		},
		"encoding/json.MarshalIndent": func(v any, prefix, indent string) ([]byte, error) {
			done := reserve(jsonBound(v))
			b, err := json.Marshal(v)
			done(len(b))
			if err != nil {
				return nil, err
			}
			var out bytes.Buffer
			ixgo.Charge(int64(indentBound(b, len(prefix), len(indent))))
			if err := json.Indent(&out, b, prefix, indent); err != nil {
				return nil, err
			}
			return out.Bytes(), nil
		},
		"encoding/json.Indent": func(dst *bytes.Buffer, src []byte, prefix, indent string) error {
			ixgo.Charge(int64(indentBound(src, len(prefix), len(indent))))
			return json.Indent(dst, src, prefix, indent)
		},
		"encoding/json.HTMLEscape": func(dst *bytes.Buffer, src []byte) {
			defer chargeGrowth(dst)()
			json.HTMLEscape(dst, src)
		},
		"encoding/json.Compact": func(dst *bytes.Buffer, src []byte) error {
			c := dst.Cap()
			defer func() { grew(c, dst.Cap()) }()
			return json.Compact(dst, src)
		},
		// Decoded values keep up to 45 bytes per input byte (measured for
		// [{"a":0},...] into any), so 64 per byte is charged and kept.
		"encoding/json.Unmarshal": func(data []byte, v any) error {
			ixgo.ChargeElems(len(data), 64)
			return json.Unmarshal(data, v)
		},
		"encoding/json.NewEncoder": func(w io.Writer) *json.Encoder {
			e := json.NewEncoder(w)
			encoders.Store(e, &encoder{w: w})
			return e
		},
		"(*encoding/json.Encoder).SetIndent": func(e *json.Encoder, prefix, indent string) {
			if v, ok := encoders.Load(e); ok {
				v.(*encoder).prefix, v.(*encoder).indent = len(prefix), len(indent)
			}
			e.SetIndent(prefix, indent)
		},
		"(*encoding/json.Encoder).Encode": func(e *json.Encoder, v any) error {
			bound := jsonBound(v)
			if st, ok := encoders.Load(e); ok {
				st := st.(*encoder)
				if st.prefix+st.indent > 0 {
					// Indented size needs the compact form first.
					done := reserve(bound)
					b, err := json.Marshal(v)
					done(0)
					if err != nil {
						return err
					}
					bound = indentBound(b, st.prefix, st.indent)
				}
				defer chargeGrowth(st.w)()
			}
			defer reserve(bound)(0)
			return e.Encode(v)
		},
		// The input size is known only after the call, so this charges late.
		"(*encoding/json.Decoder).Decode": func(d *json.Decoder, v any) error {
			off := d.InputOffset()
			defer func() { ixgo.ChargeElems(int(d.InputOffset()-off), 64) }()
			return d.Decode(v)
		},
		"(*encoding/json.Decoder).Token": func(d *json.Decoder) (json.Token, error) {
			off := d.InputOffset()
			defer func() { ixgo.ChargeElems(int(d.InputOffset()-off), 64) }()
			return d.Token()
		},

		// fmt: walks the arguments for an output bound, and refuses values
		// that contain themselves, which overflow the Go stack in fmt.
		"fmt.Sprintf": func(format string, a ...any) string {
			done := reserve(fmtBound(format, a))
			s := fmt.Sprintf(format, a...)
			done(len(s))
			return s
		},
		"fmt.Sprint": func(a ...any) string {
			done := reserve(fmtBound("", a))
			s := fmt.Sprint(a...)
			done(len(s))
			return s
		},
		"fmt.Sprintln": func(a ...any) string {
			done := reserve(fmtBound("", a))
			s := fmt.Sprintln(a...)
			done(len(s))
			return s
		},
		"fmt.Errorf": func(format string, a ...any) error {
			done := reserve(fmtBound(format, a))
			err := fmt.Errorf(format, a...)
			done(len(err.Error()))
			return err
		},
		"fmt.Appendf": func(b []byte, format string, a ...any) []byte {
			done := reserve(fmtBound(format, a))
			c := cap(b)
			r := fmt.Appendf(b, format, a...)
			done(newArray(c, cap(r)))
			return r
		},
		"fmt.Append": func(b []byte, a ...any) []byte {
			done := reserve(fmtBound("", a))
			c := cap(b)
			r := fmt.Append(b, a...)
			done(newArray(c, cap(r)))
			return r
		},
		"fmt.Appendln": func(b []byte, a ...any) []byte {
			done := reserve(fmtBound("", a))
			c := cap(b)
			r := fmt.Appendln(b, a...)
			done(newArray(c, cap(r)))
			return r
		},
		"fmt.Printf": func(format string, a ...any) (int, error) {
			defer reserve(fmtBound(format, a))(0)
			return fmt.Printf(format, a...)
		},
		"fmt.Print": func(a ...any) (int, error) {
			defer reserve(fmtBound("", a))(0)
			return fmt.Print(a...)
		},
		"fmt.Println": func(a ...any) (int, error) {
			defer reserve(fmtBound("", a))(0)
			return fmt.Println(a...)
		},
		"fmt.Fprintf": func(w io.Writer, format string, a ...any) (int, error) {
			defer reserve(fmtBound(format, a))(0)
			defer chargeGrowth(w)()
			return fmt.Fprintf(w, format, a...)
		},
		"fmt.Fprint": func(w io.Writer, a ...any) (int, error) {
			defer reserve(fmtBound("", a))(0)
			defer chargeGrowth(w)()
			return fmt.Fprint(w, a...)
		},
		"fmt.Fprintln": func(w io.Writer, a ...any) (int, error) {
			defer reserve(fmtBound("", a))(0)
			defer chargeGrowth(w)()
			return fmt.Fprintln(w, a...)
		},

		// Scanning copies tokens out of the input; each call on the same input
		// copies again.
		"fmt.Sscan": func(str string, a ...any) (int, error) {
			ixgo.Charge(int64(len(str)))
			return fmt.Sscan(str, a...)
		},
		"fmt.Sscanf": func(str, format string, a ...any) (int, error) {
			ixgo.Charge(int64(len(str)))
			return fmt.Sscanf(str, format, a...)
		},
		"fmt.Sscanln": func(str string, a ...any) (int, error) {
			ixgo.Charge(int64(len(str)))
			return fmt.Sscanln(str, a...)
		},
		"fmt.Fscan": func(r io.Reader, a ...any) (int, error) {
			defer chargeRead(r)()
			return fmt.Fscan(r, a...)
		},
		"fmt.Fscanf": func(r io.Reader, format string, a ...any) (int, error) {
			defer chargeRead(r)()
			return fmt.Fscanf(r, format, a...)
		},
		"fmt.Fscanln": func(r io.Reader, a ...any) (int, error) {
			defer chargeRead(r)()
			return fmt.Fscanln(r, a...)
		},

		"errors.Join": func(errs ...error) error {
			ixgo.ChargeElems(len(errs), stringHeader)
			return errors.Join(errs...)
		},

		// maps.Clone copies the map in runtime code.
		"maps.clone": func(m any) any {
			v := reflect.ValueOf(m)
			t := v.Type()
			ixgo.ChargeElems(v.Len(), t.Key().Size()+t.Elem().Size()+1)
			return mapsClone.Call([]reflect.Value{v})[0].Interface()
		},
	}
}

// encoder is what Encode needs from an Encoder's private fields.
type encoder struct {
	w              io.Writer
	prefix, indent int
}

// appendGrowth charges what an append-style function grew dst by.
func appendGrowth[T any](f func([]byte, T) []byte) func([]byte, T) []byte {
	return func(dst []byte, v T) []byte {
		c := cap(dst)
		r := f(dst, v)
		grew(c, cap(r))
		return r
	}
}

func caseSpecialString(f func(unicode.SpecialCase, string) string) func(unicode.SpecialCase, string) string {
	return func(c unicode.SpecialCase, s string) string {
		done := reserve(3 * len(s))
		r := f(c, s)
		done(len(r))
		return r
	}
}

func caseSpecialBytes(f func(unicode.SpecialCase, []byte) []byte) func(unicode.SpecialCase, []byte) []byte {
	return func(c unicode.SpecialCase, s []byte) []byte {
		done := reserve(3 * len(s))
		r := f(c, s)
		done(len(r))
		return r
	}
}

func caseString(f func(string) string) func(string) string {
	// An invalid byte becomes U+FFFD, three bytes; no case change grows more.
	return func(s string) string { done := reserve(3 * len(s)); r := f(s); done(len(r)); return r }
}

func caseBytes(f func([]byte) []byte) func([]byte) []byte {
	return func(s []byte) []byte { done := reserve(3 * len(s)); r := f(s); done(len(r)); return r }
}

func quote(f func(string) string) func(string) string {
	return func(s string) string { done := reserve(4*len(s) + 2); r := f(s); done(len(r)); return r }
}

func appendQuote(f func([]byte, string) []byte) func([]byte, string) []byte {
	return func(dst []byte, s string) []byte {
		c := cap(dst)
		done := reserve(4*len(s) + 2 + len(dst))
		r := f(dst, s)
		done(newArray(c, cap(r)))
		return r
	}
}

// floatBound is the longest FormatFloat output for prec: 'f' of 1e308 has
// 309 integer digits, plus sign, point and exponent.
func floatBound(prec int) int { return max(prec, 0) + 330 }

// chargeGrowth charges what a buffer written by fmt.Fprint gained.
func chargeGrowth(w io.Writer) func() {
	switch b := w.(type) {
	case *bytes.Buffer:
		c := b.Cap()
		return func() { grew(c, b.Cap()) }
	case *strings.Builder:
		c := b.Cap()
		return func() { grew(c, b.Cap()) }
	}
	return func() {}
}

// chargeRead charges the bytes a scan consumed from a strings or bytes
// Reader. ponytail: other readers are not counted; see SPEC.md.
func chargeRead(r io.Reader) func() {
	type lener interface{ Len() int }
	if l, ok := r.(lener); ok {
		n := l.Len()
		return func() { ixgo.Charge(int64(max(n-l.Len(), 0))) }
	}
	return func() {}
}

// indentBound is the most bytes json.Indent writes for src: each value and
// closing bracket can start a line with the prefix and its depth of indents.
func indentBound(src []byte, prefix, indent int) int {
	lines, depth, deepest, inString, escaped := 1, 0, 0, false, false
	for _, c := range src {
		switch {
		case inString:
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{' || c == '[':
			depth++
			deepest = max(deepest, depth)
			lines += 2
		case c == '}' || c == ']':
			depth--
		case c == ',':
			lines++
		}
	}
	return len(src) + lines*(1+prefix+deepest*indent+1)
}
