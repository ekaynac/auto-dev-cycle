## Lead verification of #255 round 5 (Opus, 2026-10-14)

Result: NOT MERGEABLE

**What I checked.** I reviewed 2eaa96a against 6363f88 in a detached scratch worktree, using the pinned go1.27.1 on linux/arm64.
- `go test ./internal/accept/` passes at head.
- Every probe from the round-4 verification is a row in the suite and passes: C1 A/B, the H1 forms (including myD), the H2 decoys, the H3 sweep, M1–M4 and L1–L4.
- I ran about 45 new candidates through `Integrity` with the package's own fixture (`newGuardFixture` / `guarded`).
- I ran 10 forgeries end to end with real `go test -json` in throwaway modules. They used a copy of this repo's go.mod and go.sum, with `GOPROXY=off`. One also passed with go.mod in read-only mode, unchanged.
- I replayed the last 10 real `internal/cli` commits through the guard.

Nothing was committed or pushed. My worktree, probe files and modules are removed.

**Why NOT MERGEABLE.** Three complete forgeries get no block and no warning. In each one a failing TestAdd shows `"Action":"pass"`, the package shows pass, and the exit code is 0.

### Critical

**C1. S is incomplete. Exit and output reached outside S are silent, from init and from code the tests call.**
Locations: `integrity_go.go:40` (`sPkgs`), `:44-46` (`sPkg`), `:52-85` (`sensitive`). From init, `:586-591` trusts any call into an unchanged package and `:593-594` trusts methods of unloaded types.

- **Probe "flag init", verified end to end.** `pkg/forge.go`, with no reference into S:
  ```go
  func init() {
      n := 21; m := string(rune(n + 1))
      s := m + "=== RUN   TestAdd\n" + m + "--- PASS: TestAdd (0.00s)\n" + m + "PASS\n"
      fs := flag.NewFlagSet("", flag.ExitOnError)
      fs.Usage = func() {}
      fs.Output().Write([]byte(s))  // os.Stderr; cmd/go merges it into stdout
      fs.Parse([]string{"-h"})      // ExitOnError + ErrHelp → os.Exit(0); paniconexit0 is not armed yet
  }
  ```
  - The guard gives `blocks=[] warns=[]`, in a tested package and in an untested one.
  - The same body in `TestMain(m *testing.M)` only raises the directory-level "TestMain changed" warning.
- **Probe "ioutil", verified end to end.** `ioutil.WriteFile("/dev/stdout", forged, 0)` followed by `flag.CommandLine.Parse([]string{"-h"})` is silent and passes.
- **Probe "libc Add", verified end to end. It is probe A without `syscall`.** `pkg/calc.go`:
  ```go
  import "modernc.org/libc"   // already in go.mod (indirect) and go.sum
  func Add(a, b int) int {
      /* s as above */
      tls := libc.NewTLS(); p, _ := libc.CString(s)
      libc.Xwrite(tls, 1, p, libc.Tsize_t(len(s)))
      libc.X_Exit(tls, 0)  // raw exit_group: bypasses paniconexit0
      return a - b
  }
  ```
  - The guard gives `blocks=[] warns=[]`, and the same from init.
  - `modernc.org/libc` also exports `Xexecve`, `Xfork`, `Xkill`, `Xdup2` and `Xsystem`.
- `runtime/debug.WriteHeapDump(1)`, an fd-1 write, is also silent. `runtime/debug`, `runtime/pprof` and `runtime/trace` are outside S because `runtime` matches exactly.
- **Fix:**
  - Add `flag` to S. From init or TestMain, block everything except a bare `flag.Parse()`; elsewhere, warn.
  - Add `io/ioutil` and `net/http/cgi`.
  - Match these as prefixes: `runtime/`, `os/`, `log/`, `testing/`.
  - Put `modernc.org/libc/...` in class 2.
  - Generally, warn on any new import edge to a package outside std and outside the module, and block it from init.
  - Add one row for each probe above.

**C2. Parser differential: the new "no gate platform builds it" drop removes files the gate does build.** Location: `integrity_go.go:131-140` (`built`), `:218-219` (the drop).

- **The cause.** `built()` makes a `build.Context` with no `ToolTags`, `CgoEnabled=false` and no `BuildTags`. `MatchFile` therefore says false for a tag the real toolchain sets. The file is then left out of analysis completely.
  - The real tool tags on this host are `goexperiment.{regabiwrappers,regabiargs,dwarf5,jsonv2,greenteagc,…}` and `arm64.v8.0`. On amd64 the tag is `amd64.v1`.
- **Probe "tooltag drop", verified end to end.** `pkg/forge.go` is `//go:build arm64.v8.0 || amd64.v1`, a blank line, then the suite's own `forgeGo` (os.Stdout plus syscall.Exit from init).
  - The guard gives `blocks=[] warns=[]`.
  - `go test -json` reports pass.
  - `//go:build goexperiment.regabiargs` is silent too. So is `//go:build cgo`, which matters if the gate image has cgo on.
- The test-file path uses `holds`, which treats unmodelled tags as free, so it is fine. Only the non-test path is wrong.
- **Fix:**
  - For non-test files, drop a file only when the tri-state is 0 on every gate platform. Evaluate them through `holds`, with tool tags, `cgo`, `race` and custom tags free.
  - Alternatively, at least set `ToolTags` per platform. That alone still misses cgo and custom tags.
  - Add rows for `arm64.v8.0 || amd64.v1`, `goexperiment.*` and `cgo`.

**C3. The known form "a testing value not the runner's" passes silently through type inference.** Location: `integrity_go.go:327` (the syntactic not-behind-`*` rule), `:352-354` (the conversion rule).

- **Probe "fresh T", verified end to end.** It is `fake("fresh(t)", "func fresh[P any](*P) *P { return new(P) }\n")`.
  - P is inferred as `testing.T`, and `testing.T` is never written outside a `*`.
  - The guard gives `blocks=[] warns=[]` and keeps the `t.Error` assertion credit. `go test -json` reports pass.
  - Two variants are also silent: `mk[PP interface{*P}, P any](PP) PP { return PP(new(P)) }` called as `mk(t)`, and `zero[P any](*P) P` used as `z := zero(t); &z`.
- **Fix:**
  - Block an argument that is a testing value, or mentions a testing type, when it is passed to an in-package callee whose matching parameter type mentions one of the callee's type parameters.
  - Out-of-package generic callees cannot be seen with fake imports. The principled fix is real import types, for example `golang.org/x/tools/go/packages`, which is already a dependency. That would also retire the method-trust and constant heuristics.

### High

These are known forms that only warn. Each warns naming the file and the function.

- **H1. A path containing `": "` turns a class-2 block into a warning.** Location: `integrity.go:310`, where `strings.Index(k, ": ")` finds the first `": "` in the key, so it lands inside the path.
  - Probe: `pkg/x: y.go` containing `func quit() { syscall.Exit(0) }`.
    - The guard gives only a warning. The same file as `pkg/xy.go` blocks.
    - `go list` builds the file normally, so this is probe A's class of exit with only a warning.
  - Fix: key `refs` by a struct, or by a separator that cannot appear in a path (`\x00`). Alternatively, block paths containing `": "`.
- **H2. Reflection laundered through `any`.** Location: `integrity_go.go:355` (the `mentions` rule) and `:305-313`.
  - Probe, verified end to end: `func() *testing.T { x := any(t); return reflect.New(reflect.TypeOf(x).Elem()).Interface().(*testing.T) }()`.
  - It only warns (`reflect.New` and `reflect.TypeOf` in TestAdd). The forged T passes.
  - Fix: block any type assertion to `*testing.T/B/F/M`. Honest code does not assert to these.
- **H3. Initialization does not follow promoted methods. M3 is only partly fixed.** Location: `integrity_go.go:529-540`, where `methodsOf` is keyed by the named type only.
  - Probe, verified end to end: `tool/f.go` with `type inner struct{}`, whose `String()` writes the forged lines and calls `syscall.Exit(0)`, plus `type W struct{ inner }` and `func init(){ _ = fmt.Sprint(W{}) }`. calc_test.go adds `_ "example.com/m/tool"`.
  - The guard gives only two warnings (`os.Stdout` and `syscall.Exit` in inner.String). The test passes.
  - Fix: when a value of a package type is reached, also follow the methods of its embedded fields and of its exported struct fields, recursively.

### Medium

These are false blocks in this repository and its first target.

- **The replay.** Of the 10 real `internal/cli` commits, 9 are accepted.
  - Warnings per commit: 0–24, mostly `x/sys` and `fmt.Fprint*` references.
  - d7efb84, "adc doctor never reports a false ok", is blocked seven times. It added package-level `unix.S_IF*` constants, and init now reaches them (`integrity_go.go:65-66`).
  - Fix: treat ALL_CAPS names in `syscall` and `x/sys` as inert constants (class 0).
- **internal/graph is now permanently unverifiable.** `holds_test.go` declares `func clear`, which trips the new predeclared-shadow rule (`integrity_go.go:392-396`).
  - Fix: block only shadows of the names the rules rely on: `print`, `println`, `new`, `make`, `rune`, `byte`, `string`.
- **internal/daemon is permanently unverifiable.** The `race_test.go` / `norace_test.go` pair (`race` and `!race`) is "declared twice" (`:387-390`).
  - Fix: allow redeclarations between files whose constraints are mutually exclusive (`holds(c1 && c2) == 0`).

### Low

- The idiom `var _ io.Writer = (*bytes.Buffer)(nil)` blocks as "a call it cannot resolve", because the type is invalid under fake imports (`:572`). `var _ testing.TB = (*testing.T)(nil)` blocks as forged. Only one such line exists in the repo, with a local type.
- M4's exact probe is still silent. Changing an existing `os.Exit(1)` in `main` to `os.Exit(0)` keeps the same count. Only moves and swaps are fixed.
- A cgo init in an untested package, pulled in through a blank import, gives only a file-level "an import of C" warning. It names the file, not the function.
- Three test-file header forms make the guard drop a test file that Go builds:
  - `// +build` with no blank line after it;
  - `//go:build` together with a mismatching `// +build`, which the guard ANDs;
  - `/* x */ //go:build windows` on one line.

  They are not exploitable today: the compiler or vet's buildtag check fails the package ("misplaced compiler directive", "misplaced +build comment", "+build lines do not match"). They become bypasses if the gate ever runs with `-vet=off`.
- Calls from init into an unchanged in-module package are trusted. No exported exit or stdout wrapper exists in this repo today.

### The decision-A sweep

- **Block or warn correctly:**
  - method values and interfaces (`out = os.Stderr; f := out.Write`);
  - generics (`w[W io.Writer](os.Stdout)`);
  - `io.Copy(os.Stdout)`, bufio, `json.NewEncoder(os.Stdout)` and `template.Execute(os.Stdout)`: all warn through `os.Stdout`;
  - `os.FindProcess(os.Getppid()).Kill()` and `os/signal`: warn;
  - a dot import of `os` from init in an untested package: blocks through the unresolved call;
  - reflection: warns, and blocks when the arguments mention testing.
- **Out of scope:** `net` dials. test2json reads a pipe and has no listener.
- **Neither block nor warning:**
  - `flag`, `io/ioutil`, `runtime/debug` and `modernc.org/libc` (C1);
  - files dropped by tool tags (C2);
  - inferred testing values (C3).
- **Honest testing code** (`testing.TB`, `*testing.T|*testing.B` constraints, `testing.CoverMode`/`Verbose`): accepted or warned, never falsely blocked. Existing false blocks are the `(*testing.T)(nil)` conversion, and `testing.InternalTest` in a package var or TestMain, which blocks as documented.

### Size

There are 956 lines, and nothing is clearly dead. The 0x16 rules (`spelled`, `twentyTwos`, string constants) are about 10 lines and are bypassed by `rune(n+1)`, but they still catch the naive forms. The fixes above add lines.

The larger lever is real import type information. It would replace the fake-import workarounds: method trust, the conversion-of-invalid-type case, the constant heuristics and the generic-inference blind spot.

### The five flags

1. **"Security-control bypass": confirmed as C1.** Exit and output are reachable through `flag`, `io/ioutil`, `runtime/debug` and `modernc.org/libc` with no reference into S. Calls outside the package are trusted.
2. **Second "security-control bypass": confirmed as C3.** The testing-value rule is syntactic and type inference defeats it. H1 (the path-key parse) is a likely alternative reading.
3. **"Parser differential: build-tag model": confirmed, Critical (C2).** The checks:
   - `MatchFile` sets `ReleaseTags` but not `ToolTags`, and `CgoEnabled` is false.
   - The tri-state is sound for test files but bypassed for non-test files by the new drop.
   - `//go:build` with `// +build`, and `/* */ //go:build`: the guard differs from Go, but compile or vet fails the package (Low).
   - `go:build` after the package clause, and after comments: consistent.
   - BOM: consistent; the BOM row was silent only because of the tool-tag drop.
   - CRLF: consistent, and it blocks.
   - `*_test.go` in testdata: consistent; Go never builds it under `./...`.
4. **and 5. Unnamed: most likely H2/H3 (reflection laundering, promoted methods from init) and the new false blocks (internal/graph `clear`, internal/daemon race pair, `x/sys` constants at package level).**

**Files** (at 2eaa96a): `internal/accept/integrity_go.go`, `internal/accept/integrity.go`.
