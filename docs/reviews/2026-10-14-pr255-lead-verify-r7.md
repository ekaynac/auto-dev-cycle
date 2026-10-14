## Lead verification of #255 round 7 (Opus, 2026-10-14)

Result: NOT MERGEABLE

Two defects fall inside the promise, and I verified both end to end. A write from init through a pointer into another package's variable is never seen, which gives a silent forgery at init (Critical, F1). The C3 taint can still be laundered (High, F2).

**What I checked**
- **Setup.** f69cf84 in a detached scratch worktree, pinned go1.27.1 on linux/arm64. I read `git diff 9cd672b f69cf84`, the PR's "Round 7" section and the r4, r5 and r6 verifications.
- **Suite.** `go test ./internal/accept/` passes.
- **Earlier probes.** Every r4, r5 and r6 probe within the promise is already a row and passes. I also re-ran each r6 probe from its "also silent" list and its re-run list through `newGuardFixture` / `guarded`. All of these now block:
  - declared range-over-func, and `range (forger{}).All`;
  - `var f = g`, `cmpG[int]`, `[]func{cmpF}[0]`, `var mv = forger{}.Cmp`, `h.F`;
  - `errors.Is(e)`, `json.Marshal(any)`, `sort.Sort(Interface)`, `Sprint([]forger{{}})`;
  - `var v = util.V`, `var f = util.Forge`, the three H3 `W{X: …}` forms, P19;
  - flag untested / behind a helper / in a var initializer / `flag.Parse` in init, and two `flag.Parse` in the hook;
  - ioutil in a var, libc in a test-file init, `runtime/debug` in the hook;
  - the tool-tag, custom-tag, `+build` and untested drops;
  - fresh T in pkg_test, reflect through a var, promoted `*inner`, the `": "` test path.
- **New probes.** About 95 candidates went through the guard. 7 forgeries ran end to end with real `go test -json` in throwaway modules (`GOPROXY=off`). In each, a failing TestAdd (`Add` returns `a-b`) reported `"Action":"pass"`, the package passed and the exit code was 0. Each exact source was then fed back through the guard.
- **Cleanup.** Nothing was changed, committed or pushed. Both scratch worktrees are removed.

### Critical

**F1. A write through a pointer into another package's variable from init or the hook is invisible. An inert package's poisoned value then runs own code through the inert-root exception.**

Locations, all in `internal/accept/integrity_go.go`:
- `:727-738` (`store`): the write check is syntactic, on `qual(root(lhs))`, so `*p = …` has root `p`.
- `:734`: `&inertpkg.Var` and `&other.ALLCAPS` pass the read check.
- `:709-711`: an own callee's arguments are only `own()`'d, so `set(&flag.Usage)` and `set(&os.Args)` are unchecked.
- `:712-713`: `fmt.Sprint(errors.ErrUnsupported)` passes as "that package's code".
- `:369-377`: the class-2 "os.Args assigned" rule only sees assignments to `os.Args` itself.

Probes:
- **E2E, `pkg/forge.go`: `blocks=[] warns=[]` → pass, exit 0, and an always-failing `TestFails` vanishes.**
  ```go
  type perr struct{}
  func (perr) Error() string { /* FLAG with n:=21; string(rune(n+1)) */ return "" }
  func init() {
      p := &errors.ErrUnsupported
      *p = perr{}
      _ = fmt.Sprint(errors.ErrUnsupported) // fmt calls perr.Error()
  }
  ```
- **E2E, same payload via `func set(p *error) { *p = perr{} }; init(){ set(&errors.ErrUnsupported); … }`.** Silent; pass, exit 0.
- **E2E, `setU(&flag.Usage)` plus `setA(&os.Args)` with `copy((*p)[1:], []string{"-h"})`.** The Usage literal prints the forged lines through a fresh FlagSet's `Output()`, and testing's own `flag.Parse` then exits 0 before `-test.paniconexit0` is armed. The guard gives only the class-1 warning "os.Args in init"; pass, exit 0.
- **Also silent:**
  - the same in the TestMain hook (only "TestMain changed" warns);
  - the same in a test-file init, and in an untested package;
  - `p := &filepath.SkipDir; *p = perr{}; errors.Is(filepath.SkipDir, filepath.SkipAll)`;
  - `p := &util.FOO; *p = util.T{}`;
  - `set(&util.HOOK)`;
  - `copy(os.Args[1:], …)` directly (warns only).
- **Direct `errors.ErrUnsupported = perr{}` blocks**, which is the control.

Fix:
- From init and the hook, block taking the address of another package's variable, or of any part of one: `&pkg.V`, `&pkg.V.f`, `&pkg.A[i]`. Inert and ALL_CAPS variables are included; read only by value.
- Block passing another package's slice, map or chan (they alias) to own callees and to `copy`, `append`, `clear` and `delete`.
- Count `&os.Args` and `os.Args` passed to `copy`/`append` or to own code as "os.Args assigned" (class 2) everywhere.
- Optionally restrict the inert-value exception to functions, constants and the plain-typed variables of inert packages.
- Add the three E2E sources as rows.

### High

**F2. C3 and the testing-value forms are still laundered. The taint is per syntax and per identifier, so the C3 promise ("now through taint tracking") does not hold.**

Locations, all in `integrity_go.go`:
- `:293-302`: `callee` returns nil for an `IndexExpr`/`IndexListExpr` Fun.
- `:391-399`: the C3 check covers direct calls only.
- `:497-508`: the taint covers only identifiers on the left-hand side.
- `:338-345`: `mentions` does not resolve own types, functions or fields whose declaration mentions testing.
- `:328-335`: `ptrTo` only sees a literal `*testing.X`.

Probes (each is `fake(X, decls)`):
- **E2E: `fresh2[int](t)`** with `func fresh2[Q any, P any](*P) *P { return new(P) }`. A partial explicit instantiation; `blocks=[] warns=[]` → pass, exit 0.
- **E2E: `fresh(zt())`** with `func zt() *testing.T { return nil }`. Silent → pass, exit 0.
- **E2E: `var f func(*testing.T) *testing.T = fresh; f(t)`.** Inference from the assignment; silent → pass, exit 0.
- **E2E: `reflect.New(reflect.TypeOf(TP(nil)).Elem()).Interface().(TP)`** with `type TP = *testing.T`. Only class-1 `reflect.*` warnings → pass, exit 0. This defeats "reflect reaching testing values" and the `*testing.X` type-assertion rule (H1).
- **Also silent in the guard:**
  - `apply(fresh, t)`, `fresh(H{}.T())`, `fresh(holder{}.t)`;
  - `h.t = t; fresh(h.t)`;
  - `fresh(ts[0])` and `fresh(<-c)` with the slice or chan as a parameter;
  - `type TM map[int]*testing.T; var m TM; fresh(m[0])`;
  - `fresh(TP(nil))`, `var x TP; fresh(x)`;
  - a type switch `case TP:`.
- **Caught:** a closure capture, a local chan or map, `type DP *testing.T` with `(*testing.T)(…)`, and plain reflect on `t`.

Fix:
- Make `callee` unwrap `IndexExpr` and `IndexListExpr`.
- Treat any non-call use of an own generic function as unverifiable in a file that mentions a testing type. That covers an assignment, an argument and a return.
- Compute "testing-bearing" own objects once, to a fixed point, and have `mentions` and `ptrTo` resolve identifiers and selectors through them:
  - an alias or defined type whose right-hand side mentions a testing type;
  - a function or method whose signature does;
  - a struct field whose type does;
  - a parameter or variable of any composite type that does.
- An alias to `*testing.X` should count as `*testing.X` in assertions, case clauses and conversions.
- The principled fix stays real import types (P14d).

### Medium

**M1. Values leave the package from init without the plain-data check, through a channel send or by being stored into another package's slice.** Locations: `integrity_go.go:631-671` has no `SendStmt` case, and builtin arguments are skipped at `:689-690`.
- Probes, all silent: `util.Ch <- forger{}`, `_ = append(util.S[:0], forger{})` and `copy(util.S, []any{forger{}})`.
- No std inert package exposes such a container, so this needs an unchanged in-module consumer. That is why it is not High.
- Fix: check a send's value as an argument when the channel is rooted at another package. Fold `copy` and `append` into another package's slice into F1's write rule.

### Low and notes

- **Replay.** I ran the lead's `p14b-replay_test.go.txt` at f69cf84 and at 9cd672b.
  - 6 of 10 pass (62eba92, c09ac49, e4b28c2, 75ddc12, d7efb84, d639cf8), and both head touches give 0/0, as stated.
  - The 4 blocked commits are the same as in round 6, in the same packages: vocab 1, harness 25, testguard 7, f45b400 4.
  - The reasons are not quite "their round-6 reasons". harness went 18→25 and testguard 5→7, with new plain-data blocks: `strings.Cut`, `filepath.Join`/`Dir`, `json.Unmarshal`, `fmt.Fprintln`/`Errorf`, `errors.Is`, `bufio.NewScanner`, `os.Chmod`/`MkdirAll`, plus a range over `os.Environ()`. harness's `os.Stdin`/`os.Stderr` and testguard's `io/fs.ErrNotExist` blocks are gone.
  - Every new reason is in the D55 ledger the PR documents ("a call into another package whose arguments are of unloaded types", "a range over a value of an unloaded type"). harness also keeps the round-1 "a skip added outside a test" and a class-2 `os/exec.Command` in a test, unchanged since round 6.
  - Fix (wording only): say the blocks gained documented D55 reasons, not that they kept round-6 reasons.
- **Receiver as argument under fake imports fails closed (confirmed).** All of these block as "a call it cannot resolve":
  - `time.Now().String()`, `strings.NewReplacer(…).Replace`, `regexp.MustCompile(…).ReplaceAllStringFunc`;
  - `util.FOO.String()`, a promoted method of an embedded `*strings.Builder`;
  - a type-parameter constraint method `x.M()`, an own interface method, a method expression `forger.String(…)`.

### The lead's focus list
- **(a) Plain-data check: sound.** These all block:
  - a type-parameter argument, `Box[forger]`, a generic alias;
  - `[1]func(){…}`, `chan any`;
  - methods defined only in a `_test.go`;
  - an alias, an embedded own interface, an other-package struct;
  - `&forger{}`, a pointer-receiver-only type;
  - `id(&util.V)` (invalid through a generic), `id(forger{})`, an unexported `forger` field.

  `type U forger` with no methods is accepted, which is correct: nothing runs.
- **(b) Inert-root exception: refuted only through F1.**
  - Without a write, these all block: `sort.Slice` with a literal, `NewReplacer.Replace`, `ReplaceAllStringFunc`, `time.AfterFunc`, `fmt.Errorf("%w", own)`, `errors.Join(own)`, `json.Marshal` of a struct with an `any` field, and `strings.Map(slices.Clone(fs)[0])` (the inner call is checked).
  - `errors.New`, `time.UTC`, `filepath.Join` and `strings.Map(unicode.ToUpper)` are accepted, as intended.
  - The only zero-argument callers among the inert std functions are `AfterFunc` and `Once*`.
- **(c) C3: refuted (F2).** Reflect blocks unless an alias launders it.
- **(d) ALL_CAPS:** `util.FOO` blocks when passed, called or used as a receiver. `v := util.FOO` is accepted, but any use of `v` blocks. `&util.FOO` is an F1 source.
- **(e) Ranges:** fail closed. A method value is followed. `seq[int]` and `mk()` block as "a range it cannot follow".
- **(f) `sync.Once*` lazy: holds.**
  - A stored OnceFunc that is called blocks, and so does one passed to own code and called there.
  - `sync.OnceFunc(lit)()` blocks.
  - `fmt.Sprint(sync.OnceFunc(f))` is accepted and does not run `f`.
- **(g) Package variables: holds.**
  - `var _ = forge()` is walked and blocks.
  - `var x = sort.Slice` is accepted unused, and blocks when called.
  - The only gap is F1's pointer write: `var p = &errors.ErrUnsupported` behaves like the local case.

**Files** (at f69cf84): `internal/accept/integrity_go.go`, `internal/accept/integrity.go`.
