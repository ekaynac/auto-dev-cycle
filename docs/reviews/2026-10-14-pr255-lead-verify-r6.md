## Lead verification of #255 round 6 (Opus, 2026-10-14)

Result: NOT MERGEABLE

**What I checked.**
- **Setup.** I reviewed 9cd672b against 2eaa96a in a detached scratch worktree, with the pinned go1.27.1 on linux/arm64. I read the diff, the PR's "Round 6" section and both earlier verifications.
- **Suite.** `go test ./internal/accept/` passes. Every probe from the round-4 and round-5 verifications is now a row and passes.
- **Re-run variants (all of these block):**
  - flag in an untested package, behind a helper, in a var initializer, as `CommandLine.Parse`, and as `flag.Parse` in init;
  - two `flag.Parse()` calls in the hook;
  - ioutil in a var or an untested package;
  - libc from a test-file init;
  - runtime/debug in a var or in the hook;
  - tool-tag, custom-tag, `+build` and `cgo` drops, in test files and in untested packages;
  - fresh T in `pkg_test`;
  - reflect.New through a var;
  - promoted `*inner`;
  - the `": "` path in a test file.
- **Expected and out of scope.** "libc Add" and M4 remain as expected: libc Add is silent (outside init, P14b-out), and M4 now warns.
- **New work.** About 45 new candidates went through `newGuardFixture` / `guarded`. 9 forgeries were run end to end with `go test -json` in throwaway modules (`GOPROXY=off`). In each, a failing TestAdd reported `"Action":"pass"`, the package passed, and the exit code was 0. Each exact source was then fed back through the guard.
- Nothing was changed, committed or pushed. My worktree is removed; probe modules remain only in my scratchpad.

**Common payload.** "FLAG" below means the round-5 body: `fs := flag.NewFlagSet("", flag.ExitOnError); fs.Usage = func(){}; fs.Output().Write(forged); fs.Parse([]string{"-h"})`. Outside init it touches nothing in S, so in any package it is silent unless init reach follows it.

### Critical

**C1. Initialization and TestMain still run code the allowlist never sees. The forgeries are complete, with no block and no warning.**

Locations, in `internal/accept/integrity_go.go`:
- `:648-649`: a FuncLit is walked only if `live`.
- `:664-667`: a Func identifier is followed only if `live`.
- `:676-686`: liveness is given only to a bare identifier or selector argument.
- `:619-636`: `followType` follows only a named own-package type, so it misses `any`, slices, maps, arrays, struct literals and `any` fields.
- `:674-679`: the arguments of builtins, `panic` included, are skipped.
- `:658-661`: a read alone passes.
- There is no `RangeStmt` case.

Each probe below is `pkg/forge.go` in the tested package. The same files under `tool/` (untested) are also silent.

| Probe | Guard | Real `go test -json` |
|---|---|---|
| P1: `func init(){ for range func(func() bool){ FLAG } {} }` (range-over-func runs its operand) | `blocks=[] warns=[]` | pass, exit 0 |
| P3: `var f = func(r rune) rune { FLAG; return r }; func init(){ _ = strings.Map(f, "a") }` | silent | pass, exit 0 |
| P7: `type forger struct{}`, `String()` = FLAG; `var v any = forger{}; func init(){ _ = fmt.Sprint(v) }` | silent | pass, exit 0 |
| P9: `type perr struct{}`, `Error()` = FLAG; `func init(){ panic(perr{}) }` (the runtime calls `Error()` before it prints the panic) | silent | pass, exit 0 |
| P16: `util/u.go`'s `(*T).String()` = FLAG with `var V T`; `pkg`: `func init(){ _ = fmt.Sprint(&util.V) }` (`&` defeats the "value passed on" rule) | silent | pass, exit 0 |
| P18: `pkg/main_test.go` with `var cb = func(r rune) rune { FLAG; return r }` and `TestMain(m){ _ = strings.Map(cb,"a"); os.Exit(m.Run()) }` | only the "TestMain changed" warning | pass, exit 0 |

These are also silent in the guard (not run end to end):
- the declared range-over-func `for range seq {}`, and `for range (forger{}).All {}`;
- `var f = g` (a declared func), then `strings.Map(f, …)`;
- `sort.Slice(xs, storedLess)`;
- `slices.SortFunc(xs, cmpG[int])`, a generic instantiation;
- `slices.SortFunc(xs, []func(a, b int) int{cmpF}[0])`;
- `var mv = forger{}.Cmp`, then passing `mv`;
- `strings.Map(h.F, …)` with a func field;
- `errors.Is(e, …)` with `var e error = perr{}` (it runs `Is`);
- `json.Marshal(v)` with `v any` (it runs `MarshalJSON`);
- `sort.Sort(s)` with `var s sort.Interface = bad{}`;
- `fmt.Sprint([]forger{{}})`;
- `var v = util.V; fmt.Sprint(v)`;
- `var f = util.Forge; strings.Map(f, "a")`;
- the H3 variants `W{X: []inner{…}}`, `W{X: map[int]inner{…}}` and `W{X: any(inner{})}`: these only warn, through the `os.Stdout` reference in `inner.String`.

With an `os.Stdout` + `syscall.Exit` payload, the tested package still blocks, because class 2 applies everywhere. An untested package that a test imports only warns (P19). FLAG, ioutil and libc payloads are silent everywhere.

**Fix.** The principle: no value whose behaviour the walk did not follow may reach a place that runs it.
- In reached code, treat `for range X` with a func-typed X as a call of X: follow it, or count it unresolved.
- Give `panic`'s argument the same handling as a passed value.
- For each argument passed on, and for `panic` and range operands:
  - follow a FuncLit and an own function or method identifier wherever it sits in the argument (inside an index, a composite or a conversion), not only when it is the bare argument;
  - count as unresolved any argument whose type is a func, interface or invalid type and that is not one of those forms (a stored function, `any`, `error` or `sort.Interface` value, or another package's value read into a local).
- Make `followType` recurse through pointer, slice, array, map, chan, struct and field types, and through own types that appear in reached composite literals.
- Keep "stored literals are not run" only for stores whose values never flow into a call, argument, range or panic in reached code. Today's `stored functions` row and `session_test.go`'s `stubs` map stay accepted.
- Add P1, P3, P7, P9, P16 and P18 as rows, plus one stored-callback row through `sort.Slice` or `filepath.WalkDir`.

### High

**H1. The `qual` import-name fallback can be poisoned. H2 (a type assertion to `*testing.T`) then passes with only warnings.** Location: `integrity_go.go:270-281`, where the fallback uses a package-wide `s.imports`; the map is built at `:494-498`, last file wins.

- Probe, verified end to end: the round-5 H2 row (`laundered by any`) plus `pkg/zz.go` = `import testing "example.com/m/util"; var _ = testing.V`.
  - The type `*testing.T` is unrecorded, because the operand is invalid under fake imports. It falls back to `example.com/m/util.T`.
  - The guard gives two class-1 warnings (`reflect.New`, `reflect.TypeOf`) and no block.
  - `go test -json` reports pass. An import whose path base is `testing` (`example.com/m/x/testing`) works the same way.
- I found no unrecorded identifier in a position that runs code. The fallback only misnames type positions, but that is enough to defeat H2.
- Fix:
  - Fail closed: for an unrecorded `X.Sel` in a type assertion, type-switch case or conversion, treat `Sel ∈ {T, B, F, M, TB}` as a testing type whatever X is.
  - Alternatively, resolve through the file's own imports only (a per-`*ast.File` map, not package-wide) and only when no local shadows the name.
  - Add the poisoned row.

**H2. C3 (a testing value made by inference) is bypassed by a local copy.** Locations: `integrity_go.go:504-515`, where `tvals` holds only function parameters, and `:334-363` (`generic`/`mentions`).

- Probe, verified end to end: `fake("func() *testing.T { u := t; return fresh(u) }()", "func fresh[P any](*P) *P { return new(P) }")`. The guard gives `blocks=[] warns=[]` and keeps the assertion credit. `go test -json` reports pass.
- `fresh(u)` with a package-level `var u *testing.T` is silent too.
- Fix:
  - Taint through `:=`, `=` and var declarations whose right-hand side or declared type mentions a testing value or type. Include package-level vars of `*testing.X` type.
  - Better: record `types.Info.Instances` and block an in-package generic instance whose type arguments are invalid or are testing types.
  - Add both rows.

### Medium

**M1. (f) Two process-start and dup spellings only warn in a tested package.** Location: `integrity_go.go:90-95`.
- `(&exec.Cmd{Path: "/bin/true"}).Run()` warns as `os/exec.Cmd`, because only `Command*` is a process start.
- `unix.FcntlInt(1, unix.F_DUPFD, 0)` is a dup and only warns.
- `exec.CommandContext`, `os.StartProcess`, `syscall.ForkExec` and `syscall.RawSyscall(SYS_FCNTL, …, F_DUPFD)` all block.
- Neither spelling forges alone, because every in-scope exit in a tested package blocks. So this is Medium, not High.
- Fix: make `os/exec.Cmd` and `Fcntl*` class 2. A `Fcntl*` call with non-dup commands is rare.

### Low and notes

- **Replay.** None of the four blocked commits shows the guard is too broad for this repository.
  - All four TestMain hooks in the repo dispatch a helper process: `cmd/adc`, `adapterhost`, `adapterhost/session` and `harness`. Their files changed 2–4 times in 681 commits.
  - `vocab.go`'s init changed 5 times, and `redact.go` 2 times.
  - So 3c2e5ee- and 3662e65-style false blocks will recur, but rarely. f45b400 is a round-1 rule. 040c74f is `os` reads at init.
  - One side effect: `cmd/adc`'s `os.Exit(run())` makes the hook reach follow all of `run()`. Growth in `cmd/adc/main.go` will block.
- **Size.** 1,051 lines (294 + 757). Nothing is clearly dead. The `imports` fallback (about 8 lines) should change per H1. The C1 fix adds lines but can drop the `live` bookkeeping for literals.

### Disposition of the deviations

- **(a) "A read alone passes": confirmed, Critical (C1).**
  - Range-over-func runs a value that is only read.
  - A read of another package's value or function into a local passes, and then reaches `fmt` / `strings.Map` (P16, `util.V`, `util.Forge`).
  - Method values, `any` and interface values, and generic instantiations read and then passed all run unseen.
  - Refuted for one case: a read of another package's var initializer. That package's init is unchanged, or analysed itself when touched.
- **(b) `sync.Once*` is lazy: refuted.** `sync.OnceValue(f)()` blocks as an unresolved call. `once.Do(func(){…})` blocks: `Do` is a method of an unloaded type and its literal is walked. A stored `OnceFunc` called from a literal blocks.
- **(c) "Referenced or stored functions are not run": confirmed, Critical (C1).** `strings.Map`, `sort.Slice`, `slices.SortFunc`, `filepath.WalkDir`, `errors.Is`, `json.Marshal` and `fmt.Sprint` all run stored values.
- **(d) The `qual` fallback: confirmed, High (H1).**
- **(e) `path` / `filepath` inert.** A `WalkDir` literal callback is walked and blocks. A stored callback is silent; that is C1, not specific to `filepath`.
- **(f) Process start.** `CommandContext`, `os.StartProcess` and `ForkExec` block. `exec.Cmd{}` and `FcntlInt(F_DUPFD)` warn only (M1).

### The three background flags (all in `integrity_go.go`)

1. **"Authorization bypass (guard logic)", first: confirmed as C1.** Init and TestMain forgeries pass with no block: range-over-func, stored or referenced functions passed to inert callers, implicit `String`/`Error`/`Is`/`MarshalJSON` through `any`, composite types and `panic`, and `&other.V`. Verified end to end.
2. **"Authorization bypass (guard logic)", second: confirmed as H2.** A C3 testing value through a local copy or package var, verified end to end. H1 is the other plausible reading.
3. **"Allowlist semantic escape": confirmed.** Functions on the allowlist (`fmt.Sprint*`, `strings.Map`, `sort.*`, `slices.SortFunc`, `errors.Is`, `encoding/json`, `filepath.WalkDir`) run callbacks and methods the walk never follows (C1). The `qual` fallback lets an import alias rename `testing` (H1).

**Files** (at 9cd672b): `internal/accept/integrity_go.go`, `internal/accept/integrity.go`.
