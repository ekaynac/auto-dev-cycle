## Lead verification of #255 round 8 (Opus, 2026-10-14)

Result: NOT MERGEABLE

Two defects fall inside items 1–5, and both were confirmed end to end. First, `os.Args` can still be rewritten from init or from the TestMain hook when the write goes through a type conversion (Critical, C1). Second, the alias fix misses generic aliases and aliases declared in another package (High, H1). The `tval` fix (item 5) is correct. The regression run matches the PR's figures.

**What I checked**
- **Setup.** 55c8732 in a detached scratch worktree, pinned go1.27.1. I read `git diff f69cf84 55c8732`, the PR's "Round 8" section and the r7 verification.
- **Suite.** `go test ./internal/accept/` passes.
- **Probes.** About 80 went through `newGuardFixture` / `guarded` (in temporary `zz_*_test.go` files, now deleted).
- **End to end.** 6 forgeries ran with real `go test -json` in throwaway modules (`GOPROXY=off`), with `Add` returning `a-b`. Each exact source was then fed back through the guard. A control with a plain `t.Error` fails with exit 1.
- **Cleanup.** Nothing was changed, committed or pushed. The worktree is removed.

### Critical

**C1. A type conversion hides an `os.Args` write from both the init/hook check and the class-2 rule. This is r7's F1 FLAG path again: every test vanishes, the package passes, exit 0.** (items 1 and 4)

Locations, all in `internal/accept/integrity_go.go`:
- `:808-828` (`root`): it stops at a conversion `CallExpr` (and at a `TypeAssertExpr` or any call).
- `:740-742` (`call`): a conversion returns `""` without checking its argument.
- `:679-727` (`reach`): there is no `ReturnStmt` case.
- `:381-384` and `:423-432` (`count`): the class-2 rule uses the same `root`.

Probes:
- **E2E, `pkg/forge.go`:** `func init() { copy([]string(os.Args)[1:], []string{"-h"}) }`
  - `go test -json`: usage is printed, the package reports `pass`, exit 0, and both `TestAdd` and an always-failing `TestFails` are gone.
  - Guard: accepted, `blocks=[]`, only the class-1 warning "os.Args in init".
- **E2E, `pkg/main_test.go`:** `func TestMain(m *testing.M) { copy([]string(os.Args)[1:], []string{"-h"}); os.Exit(m.Run()) }`
  - Same result: pass, exit 0.
  - Guard: accepted; it warns only "TestMain changed" and "os.Args in TestMain".
- **Also silent from init:**
  - `_ = append([]string(os.Args)[:1], "-h")`;
  - `p := &[]string(os.Args)[1]; *p = "-h"`;
  - `set([]string(os.Args))` into own code;
  - `func args() []string { return os.Args }; copy(args()[1:], …)` (an own function returns the variable).
- **Silent anywhere** (class 1 only, not class 2): `copy([]string(os.Args), …)`.
- **Control.** `copy(os.Args[1:], …)` blocks.

Fix:
- Make `root` see through conversions (a `CallExpr` whose `Fun` `IsType()`) and through `TypeAssertExpr`.
- In `call`, check a conversion's argument as a read (the `store` rhs rule) instead of returning `""`.
- Add a `ReturnStmt` case to `reach` that checks results like `store`'s rhs: another package's non-inert, non-plain variable returned from code reached by init is unverifiable.
- Add the two E2E sources and the four variants as rows.

### High

**H1. The alias fix misses generic aliases and aliases declared in another package.** (items 3 and 4: "alias resolution", reported twice; both reports are confirmed)

Locations: `integrity_go.go:320-326`: `alias` follows identifiers only, and `aliases` holds only this package's `TypeSpec`s. `ptrTo` at `:352-359` depends on it.

Probes (`mk` is `func mk(v any) any { return reflect.New(reflect.TypeOf(v).Elem()).Interface() }`):
- **E2E, generic alias:** `type TG[P any] = *testing.T` with `fake("mk(t).(TG[int])")`.
  - Pass, exit 0. Guard: accepted, class-1 `reflect.*` warnings only.
  - Also silent: `case TG[int]:` in a type switch, and `type TG[P any] = TP`.
- **E2E, cross-package alias: r7's own E2E source with the alias moved.** `util/u.go` declares `type TP = *testing.T`; the test runs `reflect.New(reflect.TypeOf(util.TP(nil)).Elem()).Interface().(util.TP)`. Pass, exit 0. Guard: accepted, class-1 warnings only. The reflect check does not fire because `util.TP` does not resolve.
- **These block correctly:**
  - `.(TP)`, an alias of an alias (`TQ = TP`), an alias declared in another file;
  - `case TP:`, the conversion `TP(…)`;
  - r7's direct-reflect source with an own generic alias (caught by the reflect check).

Fix: count any alias declaration whose right-hand side mentions a testing type as forged, in every package. That covers plain, generic and exported aliases at once, and `forged` already blocks in untested packages (`integrity.go:253`). Also unwrap `IndexExpr`/`IndexListExpr` in `alias`.

### Medium

**M1. "Incomplete check" (background flag): confirmed for in-module containers.** Same root cause as C1. All of these are silent from init:
- `*util.PF.(*any) = forger{}` and `p := util.PF.(*any); *p = …`;
- `copy([]any(util.S), …)`, `fill([]any(util.S))`, `q := &[]any(util.S)[0]`;
- `func get() []any { return util.S }; get()[0] = …`;
- `v := util.CAPS; v[0] = …`: the ALL_CAPS read exception at `:800` lets a slice alias into a local, and a write through that local is never seen.

This stays Medium for r7 M1's reason: it needs an unchanged in-module consumer, and no std inert package exports such a container (enumeration below). Fix: C1's `root`/return fix, plus one of two ALL_CAPS changes: apply the exception only when the value is plain-typed, or treat a local initialized from another package's variable as rooted there.

**M2. Generics as values: the file restriction is evaded through a testing-bearing own signature.** It depends on P14d's residual, so I do not count it. Lead's call.
- **Location:** `:575` and `:594`; the count only applies when the file itself mentions testing.
- **E2E:** `pkg/g.go` holds `func init() { Reg(fresh) }` and `fresh`. The test file has `var G func(*testing.T) *testing.T`, `func Reg(f func(*testing.T) *testing.T) { G = f }` and `fake("G(t)")`. Pass, exit 0. Guard: accepted with **zero warnings**.
- **Also silent:** a defined type in another file (`var G FT = fresh`), and a struct field (`var H = HT{F: fresh}`).
- **Cheap fix:** count an own generic used as a value without all its type arguments explicit, in any file of a package that mentions testing. Replay it first to measure false blocks.

### Low

**L1. "os.Args mutation" (background flag): confirmed only through C1.** The forms named in the flag are not a forgery path.
- `os.Args[1] = "-h"` and `os.Args = os.Args[:1]` are class 2 and block.
- In init and the hook, `sort.Strings(os.Args)`, `slices.Reverse(os.Args)`, `a := os.Args; a[1] = "-h"` and `f := run; f(os.Args)` all block.
- Elsewhere these four forms are class 1 only (`:425-427` drops the arguments of a function-value call). Code there runs after testing has parsed its flags, so they forge nothing. Fix (optional): count `os.Args` passed to any call not known to be read-only, or read into a local, as assigned.

### Items that hold
- **Item 1:**
  - These block from init: `&pkg.V` stored in a struct field, in a slice or in a package var and then dereferenced; `util.V.Set(…)` (unresolved); `reflect` with and without `&`; `unsafe` (the import flag); `util.AV.Store` (unresolved) and `atomic.StorePointer`; `fmt.Sscan(&util.FOO)` and `fmt.Sscan(&errors.ErrUnsupported)`; `util.V.G = …` through a promoted field; a range over `util.P` ("a range it cannot follow"), in the hook too.
  - `io.EOF`, `io.Discard`, `os.Stdout` and `http.DefaultClient` are not in inert packages. Passing them, taking their address, and handing them to own code all block.
  - **Excluding by name misses nothing on go1.27.1.** I enumerated every exported var of the inert std packages with go/importer. Every interface-typed one is named `Err*`/`Skip*`. The non-plain rest are concrete: `base64`/`base32.*Encoding`, `binary.*Endian`, `jsontext` tokens, `time.UTC`/`Local`, `unicode.TurkishCase`/`AzeriCase`. Optional: pin the list with a test that enumerates at the pinned toolchain.
  - `sink(errors.ErrUnsupported)` into own code is accepted. That is by design: the value is passed, not its address.
- **Item 2 (M1):** these block: a `select` send, a send of `errors.ErrUnsupported`, `util.S = append(util.S, …)`, `append(util.S[0:0:1], …)`. A plain send (`util.Ch <- 1`) is accepted, which is the control.
- **Item 3:** these block: `fresh2[int](t)`; generics assigned, partially instantiated as a value, stored in a struct or a map, or returned; a method value of a generic type (by the unstarred rule).
- **Item 5: confirmed, nothing is lost.** go/types records a nil object for only three identifiers:
  - a blank in a plain assignment (`_ = t`);
  - the type-switch symbolic variable (`y := x.(type)`);
  - an undeclared name (code that does not compile).

  None of these names a value. Uses in a clause body resolve to the `Implicits` objects, which `tvals` never held, before or after the fix. Every parameter name, including `_` and those in func types, interface methods and fields, and every `:=` or `var` name has a non-nil `Defs`.
- **Item 6:** `go test ./internal/accept/` passes. The replay accepts 6 of 10: 62eba92, c09ac49, e4b28c2, 75ddc12, d7efb84 and d639cf8. The four blocked commits are 3662e65 (vocab) 1, harness 25, f45b400 4 and testguard 7. Both head touches give 0/0, and no round-8 reason appears among the blocks.

### Not in items 1–5 (not counted)
The C3 type-assertion check is defeated whenever reflection launders a testing value through an `any` parameter: E2E `y := mk(t).(interface{ Error(...any) }); y.Error("add")` passes with exit 0, and the guard gives class-1 warnings only. This is older than round 8. Fixing H1 alone does not close it. I suggest routing it to P14d, or adding a rule that reflection which allocates (`reflect.New`, `NewAt`, `Zero`, `Make*`) of a type not known statically is unverifiable in a package with tests.

**Files** (at 55c8732): `internal/accept/integrity_go.go`, `internal/accept/integrity.go`.
