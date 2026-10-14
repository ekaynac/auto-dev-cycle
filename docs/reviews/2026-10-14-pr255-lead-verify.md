## Lead verification of #255 round 4 (Opus, 2026-10-14)

Result: NOT MERGEABLE

**What I checked.** I reviewed 6363f88 against 304708a in a detached scratch worktree, using the pinned go1.27.1 on arm64. `go test ./internal/accept/` passes at head. I ran 60 probe candidates through `Integrity` with the package's own fixture (`newGuardFixture` / `guarded`), and ran four forgeries end to end with real `go test -json` in throwaway modules. Nothing was committed or pushed. The worktree and all probe files are removed.

**Why NOT MERGEABLE.** Three defects fall inside decision A's promise:
- C1: a full forgery gets no block and no warning.
- H1: a listed known form ("a testing value not the runner's") does not block.
- H2: a decoy file defeats three more known forms.

### Critical

**C1. A complete forgery, from code the tests call and from init, gets no block and no warning.** Locations: `internal/accept/integrity_go.go:42-45` (the `danger` and `outputs` lists), `:48-49` (`initDeny`), and `:522-528` (any out-of-module package is trusted).

- **Probe A, verified end to end.** `pkg/calc.go`:
  ```go
  func Add(a, b int) int {
      n := 21; m := string(rune(n + 1))
      syscall.Dup3(1, 0, 0)                                   // fd 0 := stdout
      os.Stdin.WriteString(m + "--- PASS: TestAdd (0.00s)\n" + m + "PASS\n")
      syscall.AllThreadsSyscall(syscall.SYS_EXIT_GROUP, 0, 0, 0)
      return a - b
  }
  ```
  - The guard accepts it with `blocks=[] warns=[]`.
  - Real `go test -json` gives `pass` for TestAdd, `ok` and exit 0. A second test that always fails simply disappears.
  - `syscall.AllThreadsSyscall` is not in `danger`, which lists only `syscall.Exit/Exec/RawSyscall/Syscall`. `Dup3` and `os.Stdin` are on no list.
- **Probe B, verified end to end, from init.** `pkg/forge.go`:
  ```go
  func init() {
      unix.Dup3(1, 0, 0)
      os.Stdin.WriteString("=== RUN   TestAdd\n--- PASS: TestAdd (0.00s)\nPASS\n")
      unix.Exit(0)
  }
  ```
  - It imports `golang.org/x/sys/unix`, which is a direct dependency in this repo's go.mod (v0.48.0, h1 in go.sum).
  - The guard accepts it with no block and no warning. `go test -json` reports ok and pass.
  - It needs no marker, because nothing has been printed yet.
- **Fix:**
  - Add `syscall.AllThreadsSyscall` to `danger`.
  - Treat `golang.org/x/sys/...` like `syscall`: put it in `initDeny`, and put its `Exit`, `Exec`, `Syscall*`, `RawSyscall*` and `AllThreadsSyscall*` in `danger`.
  - Outside init, make every other reference into `syscall` and `golang.org/x/sys/...` at least warn. Count the fd-redirecting `Dup*` calls as output.
  - Add one row per probe.

### High

**H1. The known form "a testing value not the runner's" does not block for trivial spellings. All six forms below are verified end to end: a failing `t.Error` becomes PASS, and the guard keeps the assertion credit.** Location: `integrity_go.go:263-267` (`testType` and `qual` do no `Unparen`, are syntax-only, and miss aliases), `:307-319`.

- Each of these is accepted with no block and no warning, wrapped as `func(t *testing.T){ if Add(1,2)!=3 { t.Error() } }(X)`. The X values:
  - `(new)(testing.T)`
  - `new((testing.T))`
  - `type myA = testing.T` with `var f myA; &f`
  - `mk[testing.T]()` where `func mk[X any]() *X { return new(X) }`
  - `var a [1]testing.T; &a[0]`
  - `type myD testing.T` with `(*testing.T)(&b)`
- These forms are also accepted silently by the guard (I did not run them end to end):
  - `make([]testing.T, 1)`
  - `map[int]testing.T{}[0]`
  - `reflect.New(reflect.TypeOf((*testing.T)(nil)).Elem()).Interface().(*testing.T)`
- **Fix:**
  - Count as forging any `testing.T/B/F/M` selector that is not directly under a `*ast.StarExpr`. Honest code only writes `*testing.X`. This catches aliases, defined types, arrays, make, maps, type arguments and parens in one rule.
  - Also block conversions to `*testing.X`.
  - Make the reflection rule fire on any `reflect.*` call whose arguments mention a testing type, not only on a call that is passed a testing parameter.
  - Use `ast.Unparen` on `x.Fun` and `x.Args[0]`.

**H2. A file the gate never builds can shadow a predeclared identifier. This is a decoy with no redeclaration, so round 4's H1 fix misses it.** Location: `integrity_go.go:346-353` (all files are type-checked together; only "redeclared" errors are handled).

- **Probe, verified end to end on linux.** Add `pkg/n_windows.go` containing `func new(any) *testing.T { return nil }` and `func rune(int) int32 { return 0 }`.
  - `new(testing.T)` in calc_test.go is then accepted silently. That is Mythos's own H2 probe.
  - `var mark = string(rune(21 + 1))` is accepted silently, so the "0x16 in a constant" form fails.
- `func println(...any) {}` in `p_windows.go` makes init's `println("--- PASS…")` silent, so the init-output form fails too.
- **Fix:**
  - Block, as unverifiable, any package-level declaration that shadows `types.Universe`.
  - Better: drop from type-checking the non-test files that `MatchFile` excludes on every gate platform. That is the cheap part of P14d's per-platform work.

**H3. Output references with neither a block nor a warning.** Decision A says every output reference that does not block warns.

- The only write paths the guard counts outside init are the listed names: `os.Stdout`, `os.Stderr`, `os.NewFile`, `os.OpenFile`, `os.Create`, `os.WriteFile`, the listed `log.*` functions, `fmt.Print*` and `syscall.Write`.
- cmd/go sets `cmd.Stderr = stdout` (`cmd/go/internal/test/test.go:1684`). So everything written to stderr reaches test2json.
- These probes were accepted with nothing:
  - the `println` and `print` builtins in `Add` and in untested packages. `count()` never looks at builtins; only `resolve`, which runs for init, does.
  - `slog.Info("x\n--- PASS…")`
  - `log.SetFlags(0)` and `log.SetPrefix(…)`
  - `exec.Command(…).Run()` and `os.StartProcess`
  - `os.Stdin.Write`
  - `testing.RunExamples`, which prints framing lines with the marker already in them.
  - `syscall.Dup3`, `syscall.Kill`
- **Fix:**
  - In `count()`, count the builtins `print` and `println`.
  - Add these prefixes to `outputs` (warn): `log/slog.`, `log.Set`, `os/exec.`, `os.StartProcess`, `os.Stdin`, `testing.Run`, `testing.Benchmark`.
  - Make every other `syscall.*` reference warn.

### Medium

- **M1. Nested module: the changed-in-module-package rule fails open.** Location: `integrity.go:183-201`, `integrity_go.go:525`.
  - With `svc/go.mod` and no root go.mod, `r.mod == ""`.
  - So `init(){ util.Prepare() }`, with `util` changed to call `syscall.Dup3`, is accepted silently. With `os.Stdout` in `Prepare`, it only warns.
  - Fix: resolve the module from the nearest go.mod of each touched directory, or block when a touched package's module is not readable.
- **M2. Regression: a 0x16 spelled in the source.**
  - Round 3 blocked `\x16`, `\026` and `\u0016` spelled anywhere. Round 4 sees only constant values.
  - ``strconv.Unquote(`"\x16"`)`` is now silent.
  - `'\x16'` now only warns. That change is documented.
  - `type R int32; R(22)` is silent, because `tv.Type` is a named type, not a `*types.Basic`. See `integrity_go.go:354-361`.
  - Fix: also scan the escape spellings inside string `BasicLit` tokens (comments still excluded), and use `tv.Type.Underlying()`.
- **M3. Implicit dispatch from init is not followed.** Location: `integrity_go.go:478-497`.
  - In `init(){ _ = fmt.Sprint(forger{}) }`, the `String()` method's `os.Stdout` write only warns, where init output should block. Its `syscall.Dup3` is silent.
  - Fix: follow every method of package types whose values reached code creates.
- **M4. Counts are compared per key, so a swapped reference gives no signal.** Location: `integrity.go:258-261`, `:275-280`.
  - Round 3 warned on any exit change (`a.exits != b.exits`).
  - Now, replacing an existing `os.Exit(1)` in function F with an unconditional `os.Exit(0)` gives no signal, and neither does moving an existing init's `os.Stdout`.
  - This is outside "new reference", but the lost round-3 signal is cheap to restore: warn on `!=`.

### Low

- **L1. Reach bound.** When the base's init already exceeds `maxReach`, the "past the bound" key counts 1 on both sides. A forging chain deep enough to fall past the bound is then accepted silently. I verified this with `maxReach=2` and a chain init→g1→g2 ending in `syscall.Dup3` (`integrity_go.go:472-474`).
- **L2. Untested packages.** A dot import of `os`, `syscall` or `testing`, and `import "C"`, give neither a block nor a warning (`integrity.go:265`). Fix: warn instead of dropping the flag.
- **L3. Prefix false blocks.** `dangerous` matches `os.Executable` through the `os.Exit` prefix, which falsely blocks in tested packages.
- **L4. Danger keys.** The key names a method without its receiver, so same-named methods share a key.

### Background flags

1. **"Allowlist semantic escape": confirmed as a mechanism, Low on its own.**
   - Method calls on allowlisted values are trusted, not followed or denied (`resolve` returns `""` for methods of unloaded types). `os.Stdin.Write`, `os.Open(…).Write/Chmod/Close` and `os.Stdin.Fd()` from init all pass silently.
   - Alone they cannot reach test2json: cmd/go leaves the test binary's stdin as read-only /dev/null, and `Open` is O_RDONLY.
   - They become a stdout writer after an fd dup, which the guard misses (C1, probe B).
2. **"Control regression": partly confirmed.**
   - Every row from rounds 1–3 still passes: forge.go, linkname, unsafe, assembly, MethodByName, `t.Failed()`, shadowed t, `TestMain(*testing.T)`, the per-platform rows, package documentation, the constraints, and the Python fail-closed rule. The removed test lines were reworded or moved, not dropped.
   - The weakened rules are M2 (0x16 spellings) and M4 (exit changes now warn only on increase).
3. **"Fail-open": confirmed.**
   - The type uncertainty from files the gate never builds (H2), the empty module path (M1) and the reach bound (L1) all accept.
   - Refuted for the other examples:
     - generic calls fail closed (`g[int]()` and `g(1)` both block);
     - panics are not recovered, and Accept returns the error;
     - budget and ctx paths block or return an error;
     - a nil `Info` entry only arises on a redeclaration, which already blocks.
4. and 5. **Unnamed flags: most likely H1 (the testing-value forms) and C1/H3 (exit and output spellings that are not listed, including the builtins outside init).**

**Decision-A sweep.**
- These warn correctly, naming the file and function:
  - `fmt.Fprint(os.Stdout)`, `io.WriteString(os.Stdout)`, bufio over stdout, `log.Printf`, `syscall.Write`
  - the method value `os.Stdout.WriteString`
- These block correctly:
  - `os.Exit` and `syscall.RawSyscall` taken as function values
  - `runtime.Goexit`, `syscall.Exec`, `testing.MainStart`
  - cgo in a tested package, and a dot import of `os` in a tested package
- Neither block nor warning: `println`/`print`, `syscall.AllThreadsSyscall`, all of `x/sys/unix`, `slog`, `log.SetPrefix`, `os/exec`, `os.StartProcess`, `os.Stdin` writes, `testing.RunExamples`.
- `panic` and `syscall.Kill` are also silent, but they exit non-zero, so they cannot forge a pass.

**Files:** `<local path>`, `<local path>` (at 6363f88).
