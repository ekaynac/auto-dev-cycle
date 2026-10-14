## Lead confirmation of #255 round 9 (Opus, 2026-10-14)

Result: NOT CONFIRMED. The only failure is check 2b. **The lead's disposition (below) accepts it as a stated residual.**

Checks 1–5 all pass. The result is NOT CONFIRMED only because of check 2b, an extra check the lead added during the run. In that check, all four probes are accepted **and** each one forges end to end. It is not a regression: 55c8732 accepts the same four exact sources. The round-9 alias fix is incomplete for an alias that is already in the base, in another package.

| Check | Result |
|---|---|
| **1. r8 E2E sources through the guard** (exact `pkg/` and `util/` files, with and without the module's go.mod) | **PASS.** e5 (C1, init) and e6 (C1, TestMain) block on "a write it cannot prove safe (os.Args)", "value … converted ([]string(os.Args))" and "os.Args assigned in init/TestMain". e1 (H1 generic) and e2 (H1 cross-package) block on "can forge go test2json results" (e2 is reported on `util`). |
| 1. e3 (reflection through `any`) | Accepted, with class-1 `reflect.*` warnings only. This is the accepted residual (#226 entry 82). |
| 1. e4 (M2) | Accepted with zero warnings. This is the accepted residual (#226 entry 82). |
| **2. C1 probes** | **PASS, all block.** `_ = append([]string(os.Args)[:1], "-h")` (also in TestMain), `p := &[]string(os.Args)[1]`, `set([]string(os.Args))`, and `args()` returning `os.Args` ("plain data returned (os.Args)"). |
| 2. Class 2 `copy([]string(os.Args), …)` | **PASS, blocks** in a helper (`os.Args assigned in reset`) and in a non-init test function (`os.Args assigned in TestZ`). |
| 2. H1 probes | **PASS, all block.** `mk(t).(TG[int])`, `case TG[int]:`, `type TG[P any] = TP` (both the assertion and the case), and the cross-package `util.TP` source when `util/u.go` is in the candidate. |
| 2. M1 probes | **PASS, all block.** `*util.PF.(*any) = forger{}` (and `p := util.PF.(*any)`), `copy([]any(util.S), …)`, `fill([]any(util.S))`, `q := &[]any(util.S)[0]`, `get()[0] = …`, `v := util.CAPS; v[0] = …`. |
| **2b. Cross-package alias already in the BASE** (`util/u.go` holds `type TP = *testing.T` and `type TG[P any] = *testing.T`, unchanged) | **FAIL.** All four probes are **accepted**, with class-1 `reflect.*` warnings only: the r8 H1 reflect source with `.(util.TP)`, `case util.TP:`, `mk(t).(util.TG[int])` and `case util.TG[int]:`. The fixture works: an own alias, or a direct `.(*testing.T)`, blocks in the same base, and a plain test is accepted. **End to end** (`go test -json`, `GOPROXY=off`, `Add` returning `a-b`): all four give `--- PASS: TestAdd`, the package passes, exit 0. The guard also accepts each exact source. 55c8732 accepts the same four sources, so this is not a regression. |
| **3. Controls** | **PASS, all unchanged.** `copy(os.Args[1:], …)` blocks (in init and in a helper). `util.Ch <- 1` from init is accepted. `int(other.Count)` from init is accepted, both with the suite's unresolved `other.Count` and with `var Count int32` declared. `loc := time.UTC` is accepted. `errors.New`, `filepath.Join` and `strings.Map(unicode.ToUpper)` from init are accepted with no warnings. The plain `t.Error` control (`e2e-r8/ctl`) fails end to end: `--- FAIL: TestAdd`, exit 1. |
| **4. Regression** | **PASS.** `go test ./internal/accept/` is ok. The replay accepts 6 of 10: 62eba92, c09ac49, e4b28c2, 75ddc12, d7efb84, d639cf8. Blocked: 3662e65 (vocab) 1, 3c2e5ee (harness) 25, 040c74f (testguard) 7, f45b400 4. Both head touches (graph/build.go, daemon/daemon.go) give 0 blocks and 0 warnings. |
| **5. Mutations** | **PASS.** "C1 root not through a conversion" turns 9 rows red: `C1 &[]string(os.Args)[1]`, `C1 a conversion read`, `C1 a converted os.Args anywhere`, `… in init`, `… in the hook`, `C1 a converted os.Args local`, `C1 append([]string(os.Args)[:1])`, `C1 set([]string(os.Args))`, `M1 copy([]any(util.S))`. "H1 an alias of a testing type allowed" turns `H1 another package's alias` red. Both match round 9's own results, and the files were restored. |

**Why 2b gets through.** `ptrTo` (`integrity_go.go:352-359`) and `alias` (`:319-326`) are purely syntactic, and `s.aliases` holds only the current package's TypeSpecs (`:529`). The new TypeSpec rule (`:410`) counts an alias only in a package the candidate changes. So `.(util.TP)` and `case util.TG[int]:` never resolve when `util` is unchanged. A possible fix: in the TypeAssertExpr and CaseClause checks, use go/types (`types.Unalias(info.TypeOf(x.Type))` is a pointer to `testing.T`, `B`, `F`, `M` or `TB`). When the asserted type is another package's alias that cannot be resolved, treat it as unverifiable.

**Cleanup.** Nothing was changed, committed or pushed. All temporary files and both scratch worktrees are removed.

### Lead disposition (2026-10-14)

Check 2b is accepted as a stated residual of P14b-init. It does not block the merge. The reasons:

- **The precondition is a testing-type alias that already exists in the base.** A candidate cannot create one: round 9 makes any new alias of a testing type forge in every package, and the r9 mutation "H1 an alias of a testing type allowed" turns red.
- **This repository has none.** `git grep` over `origin/main` finds no alias of `testing.T`/`B`/`F`/`M`/`TB` in any `.go` file, test or non-test. So the attack is latent: it needs a target repository that already holds such an alias.
- **The proper fix is real types.** It is recorded as #226 entry 82(e) for P14d, which is a gate for P22a. P14b stays inert until P22a.

This follows operator decision A: block the known forms a candidate can produce, and state the residual.
