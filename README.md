# Goose: a subset of Go with a semantics in Coq

[![CI](https://github.com/goose-lang/goose/actions/workflows/ci.yml/badge.svg)](https://github.com/goose-lang/goose/actions/workflows/ci.yml)
[![](https://godoc.org/github.com/goose-lang/goose?status.svg)](https://godoc.org/github.com/goose-lang/goose)

Goose is a subset of Go equipped with a semantics in Coq, as well as translator to automatically convert Go programs written in Go to a model in Coq. The model plugs into [Perennial](https://github.com/mit-pdos/perennial) for carrying out verification of concurrent storage systems.

Goose includes a reasonable subset of Go, enough to write serious concurrent programs. The translator and semantics are trusted; you can view the process as giving a semantics to Go. The semantics is given via a translation to GooseLang, an untyped lambda calculus with references and concurrency, including some special support to work for Go.

## Demo: an example conversion

To give a flavor of what goose does, let's look at an example:

File `append_log.go`:

```go
// Append-only, sequential, crash-safe log.
//
// The main interesting feature is that the log supports multi-block atomic
// appends, which are implemented by atomically updating an on-disk header with
// the number of valid blocks in the log.
package append_log

import (
	"sync"

	"github.com/tchajed/marshal"

	"github.com/goose-lang/goose/machine/disk"
)

type Log struct {
	m      *sync.Mutex
	sz     uint64
	diskSz uint64
}

func (log *Log) mkHdr() disk.Block {
	enc := marshal.NewEnc(disk.BlockSize)
	enc.PutInt(log.sz)
	enc.PutInt(log.diskSz)
	return enc.Finish()
}

func (log *Log) writeHdr() {
	disk.Write(0, log.mkHdr())
}

func Open() *Log {
	hdr := disk.Read(0)
	dec := marshal.NewDec(hdr)
	sz := dec.GetInt()
	diskSz := dec.GetInt()
	return &Log{m: new(sync.Mutex), sz: sz, diskSz: diskSz}
}
```

Goose output:

```coq
(* autogenerated from append_log *)
From Perennial.goose_lang Require Import prelude.
From Perennial.goose_lang Require Import ffi.disk_prelude.

From Goose Require github_com.tchajed.marshal.

(* Append-only, sequential, crash-safe log.

   The main interesting feature is that the log supports multi-block atomic
   appends, which are implemented by atomically updating an on-disk header with
   the number of valid blocks in the log. *)

Module Log.
  Definition S := struct.decl [
    "m" :: lockRefT;
    "sz" :: uint64T;
    "diskSz" :: uint64T
  ].
End Log.

Definition Log__mkHdr: val :=
  rec: "Log__mkHdr" "log" :=
    let: "enc" := marshal.NewEnc disk.BlockSize in
    marshal.Enc__PutInt "enc" (struct.loadF Log.S "sz" "log");;
    marshal.Enc__PutInt "enc" (struct.loadF Log.S "diskSz" "log");;
    marshal.Enc__Finish "enc".

Definition Log__writeHdr: val :=
  rec: "Log__writeHdr" "log" :=
    disk.Write #0 (Log__mkHdr "log").

Definition Open: val :=
  rec: "Open" <> :=
    let: "hdr" := disk.Read #0 in
    let: "dec" := marshal.NewDec "hdr" in
    let: "sz" := marshal.Dec__GetInt "dec" in
    let: "diskSz" := marshal.Dec__GetInt "dec" in
    struct.new Log.S [
      "m" ::= lock.new #();
      "sz" ::= "sz";
      "diskSz" ::= "diskSz"
    ].
```

## Go support

Goose doesn't support arbitrary Go code; it uses a carefully-selected subset of Go that is still idiomatic Go (for the most part) while being easy to translate. See the [implementation design doc](docs/implementation.md) for more details.

There are three aspects of the Go model worth mentioning here:

- Assignments are not supported, only bindings. This imposes a "linearity" restriction where each variable is constant after being first defined, which aligns well with how Perennial code works. Mutable variables can still be emulated using pointers and heap allocation.
- Goose supports a few common control flow patterns so code can be written with early returns and some loops.
- Many operations are carefully modeled as being non-linearizable in Perennial, although this area is subtle and hard to reconcile with Go's documented memory model.

## Related approaches

Goose solves the problem of using Perennial to verify runnable systems while ensuring some connection between the verification and the code. How else do verification projects get executable code?

_Extraction_ is a popular approach, especially in Coq. Extraction is much like compilation in that Coq code is translated to OCaml or Haskell, relying on many similarities between the languages. (Side note: the reason it's called extraction is that it also does _proof erasure_, where proofs mixed into Coq code are removed so they don't have to be run.) To get side effects in extracted code the verification engineers write an interpreter in the target language that runs the verified part, using the power of the target language to interact with the outside world (eg, via the console, filesystem, or network stack).

A big disadvantage of extraction is performance. The source code is several steps away from the extracted code and interpreter. This distance makes it hard to debug performance problems and reduces developers' control over what the running code does.

Another approach is to use a deeply-embedded language that closely represents an efficient imperative language, such as C. Then one can write code directly in this language (eg, Cogent), produce it as part of proof-producing synthesis (eg, Fiat Cryptography), import it from normal source code (eg, `clightgen` for VST). Regardless of how the code in the language is produced, this approach requires a model of the syntax and semantics of the language at a fairly low-level, including modeled function calls, variable bindings, and local stacks, not to mention concurrency and side effects.

Directly modeling a low-level, imperative language (and especially writing in it) gives a great deal of control, which is good for performance. This approach is also well-suited to end-to-end verification: at first, one can pretty-print the imperative language and use any compiler, but eventually one can shift to a verified compiler and push the assumptions down the software stack.

## Running goose

Goose requires Go 1.22+.

You can install goose with either `go install github.com/goose-lang/goose/cmd/goose@latest` or from a clone of this repo with `go install ./cmd/goose`. These install
the `goose` binary to `~/go/bin`.

To run `goose` on one of the internal examples and update the Coq output in
Perennial, run (for the append_log example):

```
goose -out $perennial/src/goose_lang/examples/append_log.v \
  ./internal/examples/append_log
```

where `$perennial` is the path to a clone of [Perennial](https://github.com/mit-pdos/perennial).

## Developing goose

The bulk of goose is implemented in `goose.go` (which translates Go) and
`internal/coq/coq.go` (which has structs to represent Coq and GooseLang code,
and code to convert these to strings).

Goose has integration tests that translate example code and compare against a
"gold" file in the repo. Changes to these gold files are hand-audited when
they are initially created, and then the test ensures that previously-working
code continues to translate correctly. To update the gold files after
modifying the tests or fixing goose, run `go test -update-gold` (and manually
review the diff before committing).

The tests include `internal/examples/unittest`, a collection of small
examples intended for wide coverage, as well as some real programs in other
directories within `internal/examples`.

The Coq output is separately tested by building it within the [Perennial
](https://github.com/mit-pdos/perennial) source tree.

Additionally, the `internal/examples/semantics` package contains a set of
semantic tests for checking that translated programs preserve their semantic
meaning. The testing infrastructure requires test functions to have the form
`func test*() bool` and return true (that is, they begin with "test", take no
arguments, and return a bool expected to always be true). Any helper functions
should _not_ follow this pattern.

The `cmd/test_gen/main.go` file generates the required test files from these
functions. The files are:

- A `.go` file which uses the Go `testing` package. This is for confirming that
  tests actually do always return true, as intended. Note that the use of the
  testing package means that this file must end in `_test.go`. It should go in
  the `semantics/` directory.
- A `.v` file that runs using Perennial's semantic interpreter. This file
  should go in Perennial's `goose_lang/interpreter/` directory and can have any name.

To generate these files, the executable takes a required flag of either `-go` or
`-coq` which specifies the file type, an optional `-out` argument specifying
where to write the output, and finally the path to the semantics package. For
example, `go run ./cmd/test_gen -coq -out ~/code/perennial/src/goose_lang/interpreter/generated_test.v ./internal/examples/semantics` generates the Coq file of semantics tests.
(You'll probably need to adjust the path to Perennial.) To re-generate the Go
test file you can just run `go generate ./...`.

### Running tests

Use a tool such as [act](https://github.com/nektos/act) to run the CI tests locally.
To automatically fix all formatting issues and update the generated files, run `make fix`.
If the 'gold' files need updating, run `go test -update-gold`.
