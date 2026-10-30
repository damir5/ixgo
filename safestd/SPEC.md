# Allocation charges for Go functions

`Context.MaxAlloc` counts memory that interpreted code allocates. A Go function that ixgo calls for the program, such as `strings.Repeat`, allocates outside that count. This file says which of those functions must charge `MaxAlloc` themselves, what they charge, and what stays uncovered. Package `safestd` implements it for the packages listed below.

## Rule

A Go function needs a wrapper when it can allocate memory that the program has not already paid for. Before the call, the wrapper charges an upper bound of the bytes the call returns or keeps. If the bound is loose, the wrapper refunds the difference after the call.

Temporary buffers that the call frees before it returns are not charged. They are a fixed multiple of the charged output; the multiples measured are below. `MaxHeapAlloc` covers them.

Each function falls into one of four classes:

| Class | Meaning | What the wrapper does |
|---|---|---|
| none | Allocates nothing, a fixed small amount, or returns part of an input (a substring or subslice). | No wrapper. |
| size | An argument sets the output size, so a small input gives a large output. | Charges the exact size or an upper bound before the call. |
| copy | Returns a new copy of an input. A loop that calls it on the same input makes a new copy each time, and only the first input was charged. | Charges the copy before the call. |
| growth | Grows a `bytes.Buffer`, a `strings.Builder` or a `dst` slice inside Go code. | Charges the new backing array when the capacity grew. |

A wrapper that charges after the call does not prevent the allocation. It stops a loop on the next charge. Only "size" and "copy" wrappers charge before the call.

## API

- `ixgo.Charge(n int64)` counts `n` bytes against `MaxAlloc`. When the total exceeds the limit, the program stops and the run returns `ixgo.ErrAllocLimit`. `recover()` in the program does not stop it.
- `ixgo.ChargeElems(n int, size uintptr)` charges `n*size` and treats an overflow as exceeding the limit.
- `ixgo.Refund(n int64)` returns bytes charged as an upper bound that the call did not keep.
- `ctx.RegisterExternal(name, fn)` installs a wrapper. `name` is `pkg.Func`, or `(*pkg.Type).Method` for a method. `fn` has the same signature as the original; a method takes its receiver as the first parameter.

The charge functions find the program through the goroutine. When a limit is set, ixgo records the program for the goroutine during each Go function call. Outside such a call they do nothing. Measured cost on an M3 Max, median of five runs: 1M calls to `strings.Index` took 188 ms without limits and 308 ms with `MaxSteps` and `MaxAlloc` set, about 120 ns per call. That includes the spot check below.

## Covered packages

`safestd.Packages` lists them: `bytes`, `cmp`, `encoding/base64`, `encoding/hex`, `encoding/json`, `errors`, `fmt`, `iter`, `maps`, `math`, `math/bits`, `slices`, `sort`, `strconv`, `strings`, `unicode`, `unicode/utf8`.

`slices`, `maps`, `cmp` and `iter` are generic. ixgo runs them from source, so the interpreter charges them like program code. The one exception is `maps.Clone`, which calls runtime code.

The tables list every function that needs a wrapper. All other exported functions and methods of these packages are in class "none": they compare, search, check, parse into fixed-size values, return substrings or subslices, or allocate a fixed small amount. This includes `strings.Lines`, `SplitSeq` and `FieldsSeq`, which yield substrings one at a time.

In the tables, `n` is the length of the main input in bytes.

### strings and bytes

| Function | Class | Charged before the call | Kept after |
|---|---|---|---|
| `Repeat(s, count)` | size | `len(s)*count`, overflow refused | same |
| `Join(elems, sep)` | size | sum of lengths plus `(len(elems)-1)*len(sep)` | same |
| `Replace`, `ReplaceAll` | size | `n + matches*(len(new)-len(old))`, matches from `Count` | same |
| `ToValidUTF8(s, rep)` | size | `n*(1+len(rep))` | result length |
| `NewReplacer` then `(*Replacer).Replace`, `WriteString` | size | `(n+1)*(longest replacement+1)` | result length |
| `(*Builder).Grow(k)`, `(*Buffer).Grow(k)` | size | `k` | plus the rest of the new array |
| `ToUpper`, `ToLower`, `ToTitle`, `Title`, `To*Special` | size | `3n` (an invalid byte becomes U+FFFD, 3 bytes) | result length |
| `Map(f, s)` | size | `4n` (each input byte at most one rune) | result length |
| `Split`, `SplitN`, `SplitAfter`, `SplitAfterN` | size | parts times 16 (24 for `bytes`), parts from `Count` | same |
| `Fields`, `FieldsFunc` | size | `(n/2+1)` times 16 (24 for `bytes`) | parts times 16 (24) |
| `bytes.Runes` | size | `4 * RuneCount` | same |
| `Clone`, `bytes.NewBufferString`, `(*Buffer).String` | copy | `n` | same |
| `(*Buffer).ReadBytes`, `ReadString` | copy | `Len()` | result length |
| `(*Builder).Write*`, `(*Buffer).Write*`, `(*Buffer).ReadFrom` | growth | none | new array |
| `(*Reader).WriteTo`, `(*Buffer).WriteTo` | growth | none | new array of the writer when it is a `bytes.Buffer` or `strings.Builder` |

### strconv and unicode/utf8

| Function | Class | Charged before the call | Kept after |
|---|---|---|---|
| `FormatFloat`, `AppendFloat` | size | `max(prec,0)+330` (`'f'` of 1e308 has 309 digits) | same, plus new array |
| `FormatComplex` | size | twice that plus 3 | same |
| `Quote`, `QuoteToASCII`, `QuoteToGraphic`, `AppendQuote*` | size | `4n+2` (`\x01` is 4 bytes per byte) | result length |
| `Unquote` | copy | `n` | result length |
| `AppendInt`, `AppendUint`, `AppendBool`, `AppendQuoteRune*`, `utf8.AppendRune` | growth | none | new array |

### fmt

| Function | Class | Charged before the call | Kept after |
|---|---|---|---|
| `Sprintf`, `Sprint`, `Sprintln`, `Errorf`, `Appendf`, `Append`, `Appendln` | size | output bound, see below | result length, or new array |
| `Printf`, `Print`, `Println` | size | output bound | none |
| `Fprintf`, `Fprint`, `Fprintln` | size, growth | output bound | new array of the writer when it is a `bytes.Buffer` or `strings.Builder` |
| `Sscan`, `Sscanf`, `Sscanln` | copy | `len(str)` | same |
| `Fscan`, `Fscanf`, `Fscanln` | copy | none | bytes consumed, for a `strings.Reader` or `bytes.Reader` |

The output bound is the format length, plus every width and precision in the format, plus a bound for each argument. fmt accepts widths and precisions up to 1e6 (`fmt.tooLarge`), so a `*` counts as 1e6. The argument bound walks the value:

- a string counts 6 bytes per byte;
- a byte slice counts 6 bytes per byte (`0x61, ` in `%#v`);
- a number counts 68 bytes (`%#b` of a uint64);
- a float counts 40 bytes, or `330+precision` when the format has an `f` or `F` verb;
- a container counts its type name, plus each element and its separator;
- zero-size elements such as `struct{}` count once per element. They cost no memory, so `make([]struct{}, 1<<30)` is free and prints 3 GB.

fmt never stops on a map or slice that contains itself. Go's fmt overflows the goroutine stack, and Go ends the process with `fatal error: stack overflow`, which `recover` cannot catch. The walk panics with `safestd.ErrCyclicValue` instead, as a normal program panic. It also refuses values nested deeper than 10000 levels with `safestd.ErrTooDeep`. Pointers below the top level are printed as addresses, so fmt does not follow them and the walk does not either.

### encoding/json, encoding/base64, encoding/hex, errors, maps

| Function | Class | Charged before the call | Kept after |
|---|---|---|---|
| `json.Marshal`, `(*Encoder).Encode` | size | output bound (the fmt walk, following pointers; json reports cycles itself) | result length, plus new array of the writer |
| `json.MarshalIndent`, `json.Indent`, `(*Encoder).Encode` after `SetIndent` | size | `len(compact) + lines*(prefix + depth*indent + 2)` from one pass over the compact form | same |
| `json.Unmarshal` | size | `64n` | same |
| `(*Decoder).Decode`, `(*Decoder).Token` | size | none | `64` per input byte read (charged late, see gaps) |
| `json.Compact`, `json.HTMLEscape` | growth | none | new array of `dst` |
| base64 `EncodeToString`, `DecodeString` | size | `EncodedLen(n)`, `DecodedLen(n)` | same |
| hex `EncodeToString`, `DecodeString`, `Dump` | size | `2n`, `n/2`, `(n/16+1)*79` | same |
| base64 and hex `AppendEncode`, `AppendDecode` | growth | none | new array |
| `errors.Join(errs...)` | copy | 16 per error | same |
| `maps.Clone(m)` | copy | `len(m)*(key size + value size + 1)` | same |

`json.Unmarshal` keeps up to 45 bytes per input byte. That was measured for `[{"a":0},...]` decoded into `any`. Other measured shapes: `[0,...]` 15.7, `[{},...]` 20.5, `[[],...]` 12.6, `["",...]` 9.9. The 64 per byte stays charged, so a 1 MB document uses 64 MB of `MaxAlloc`.

## Temporary buffers measured

These ratios are total bytes allocated per input byte, measured with `runtime.MemStats.TotalAlloc` on the worst input found. The wrappers charge only what the call returns or keeps; the rest is temporary.

| Call and input | Ratio |
|---|---|
| `json.Unmarshal` of `[{},{},...]` into `any` | 57.4 |
| `fmt.Sprintf("%#v", []byte)` | 37.7 |
| `json.Marshal` of a string of `\x01` | 33.3 |
| `fmt.Sprintf("%q", invalid UTF-8)` | 28.2 |
| `strconv.Quote` of `\x01` | 22.0 |
| `strings.Map` to U+10FFFF | 20.2 |
| `strings.ToUpper` of invalid UTF-8 | 11.4 |

## Spot check

When `MaxAlloc` is set, ixgo also compares the process heap growth with the charges. It does this every 256 steps, and after any native call slower than 100 µs, because allocating a lot takes time. If the heap grew by more than 8 times the charges in that window, plus 1 MB, plus 1 byte per nanosecond since the last check, ixgo charges the difference. The factor 8 covers the temporary buffers of wrapped calls; the largest measured was 6.3 times what the call keeps (`%#v` of a byte slice).

The spot check charges after the allocation, so it stops the program late: up to 256 steps late, or one slow call late. It is a heuristic in three ways:
- the heap counter covers the whole process, so a host that allocates faster than 1 byte per nanosecond while the program runs can be charged to the program;
- a program that charges a lot can allocate up to 8 times that uncharged;
- a native call that allocates without taking long enough to trigger the slow-call check waits for the next 256-step check.

Tests: a `Write` through an interface, a `base64.NewEncoder` stream and `errors.Join(...).Error()` on a long message each stop with `ErrAllocLimit`. With the check turned off, the same programs ran until the 110 s timeout.

## Not covered by a wrapper

These paths allocate without a wrapper. The spot check charges them late, and `MaxHeapAlloc` is the backstop.

1. **Interface calls on Go types.** `RegisterExternal` replaces static calls and function values. A call through an interface goes to the original method. For example, `var w io.Writer = &buf; w.Write(p)` grows `buf` without a charge.
2. **Writers and readers that Go code returns.** `base64.NewEncoder`, `hex.NewEncoder` and `hex.Dumper` write into their destination from Go code.
3. **Formatting methods.** A value with a `String`, `Error`, `Format`, `GoString`, `MarshalJSON` or `MarshalText` method counts as 256 bytes in the output bound. Such a method can return a longer string, for example the same long string for every element of a slice. `errors.Join(...).Error()` joins all messages the same way.
4. **json streams charge late.** `(*Decoder).Decode` and `Token` learn the input size only after reading, so one call can allocate up to 57 times its input before the charge.
5. **Other readers in `fmt.Fscan`.** Only `strings.Reader` and `bytes.Reader` report how much a scan consumed.
6. **Standard input.** `fmt.Scan`, `Scanf` and `Scanln` read the host's standard input.

## Excluded packages

ixgo registers packages for the whole process, so a host that registered `os` for its own use would also expose it to programs. `safestd.Restrict(ctx, allow...)` lets programs import only `safestd.Packages` plus the host's own packages in `allow`:

- It refuses `unsafe`, which `//go:linkname` also needs.
- It checks only the program's own imports. Dependencies of an allowed package pass, because `slices` and `maps` import `unsafe` in their source.
- It stops ixgo from loading packages from source files. Without that, an import of a package that is not registered, such as `container/list`, loads it from the Go installation and runs it.

| Package | Why it is excluded |
|---|---|
| `reflect` | `MakeSlice`, `MakeMapWithSize`, `New`, `Append` take sizes from arguments. |
| `math/big` | `Lsh(x, n)` and `Exp(x, y, nil)` take sizes from arguments; `Mul` in a loop grows uncharged. |
| `regexp` | `ReplaceAll*` with `$1` expands a match per reference; `FindAll*` returns many matches. |
| `bufio` | `NewReaderSize`, `NewWriterSize` and `Scanner.Buffer` take sizes from arguments. |
| `io`, `os`, `net/...` | `ReadAll` and `Copy` read without a bound, and the host's files and network are exposed. |
| `time` | `Format` needs a wrapper (output up to 3 times the layout). `AfterFunc` runs program code on a Go goroutine; ixgo now reports a panic there as the run's error, but the program can still sleep and start timers. |
| `strings.Builder` through `io.Writer` | See item 1 above. |

## Checks

`safestd_test.go` runs each case in a program with `MaxAlloc` at 64 MB and expects `ErrAllocLimit`. Each case was also run without `Register`. Each either finished without an error or reached the process heap cap. The size cases allocated 50 MB to 3 GB. The two json decoding cases have a 2 MB input; they fail only because the wrapper charges 64 bytes per input byte. The same file checks that a self-containing map or slice passed to fmt panics in the program, and that every wrapper returns what the library returns.
