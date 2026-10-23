## External review of chunk 5b (split), K1 and K2a (Mythos, 2026-10-23)

Verdict: GO

Read-only review of K2a (`git diff 3182f50..HEAD`, head 716b8de) and K1 (`git diff 2f71548 232d305`, merged as #409). No tests were run. At review time #422's `make ci` check was still pending; `T0 fixtures well-formed` had passed.

### Answers to the five questions

1. **Kept bits enforced on adcd's production path.** Spot-checked seven, each from the bit back to the code adcd wires under `launch` and to a row that depends on that code:
   - I12 Consumption: `internal/graph/template.go:85` wires the gate on accept's tree only; `internal/daemon/gate.go:238-251` refuses any input that is not a tree result recorded with no attempt (`errContent`), and `gate.go:128-135` rebuilds accept's tree from the patch at its base and refuses a tree that does not match. Rows `TestAGateOverARefusedAcceptIsHeld`, `TestAGateRefusesATreeThatDoesNotRebuild` (new, `internal/daemon/gate_test.go:322-359`), `TestCandidateTemplate`.
   - I12 Admission (Go): `internal/daemon/accept.go:136-137` runs `accept.Accept` with `accept.Integrity` over the sandboxed lister. A lister that ran and failed is the guard's block (`TestAFailedGoListRefusesTheCandidate`, `accept_test.go:187`). A listing that did not run is `accept.ErrGuardRun`, a bounded retry (`run.go:893`), never a green.
   - I10 Consumption: `internal/daemon/place.go:280-292` records the intent in the launch command after `dispatch.Launch` and refuses it unless `registered` (`place.go:228-241`) matches the lease's executor; the fold walks it (`internal/intent/recorded.go:114`). The three named rows exist in `internal/daemon/intent_test.go`.
   - I10 Effect: `internal/daemon/daemon.go:248` builds the one gateway at every start through `newEgress` (`egress.go:68`), before the runner and the loop.
   - I17 Admission: `internal/daemon/adapter.go:102-123` resolves the pinned image under `resolveTimeout`, checks `CheckManifest`/`CheckPrepared` then `CheckAdapter` against the installed release before anything is prepared; adcd has no host adapter path (`wiring.go:148`).
   - K1, I16 Admission: `internal/graph/holds.go:134` refuses a retry whose reason names no hold; `holds_test.go:277` now asserts that refusal, not the fold's.
   - K1, I8 Fencing: `dispatch.Launch` (`internal/dispatch/launch.go:128-151`) starts or refuses only at the lease revision the launch read (`lease.StartNext(tx, id, expected, …)`).

   Every test the new inventory rows name exists under the name given (grep over `internal/`).

2. **Overstatement of main's code.** None that matters. Three wording points are Low (below).

3. **Refused-accept hold.** Visible and terminal. A refused accept records a completed `failed` result (`protected_path`, `unsafe_output` or `test_integrity_violation`) with no tree. The gate claimed on it is held in one command, `source_invalid_config` / `runner: failed`, owner operator (`gate.go:107` → `run.go:901-911`), with no gate result and no run. `runner.cleared` returns false for `runner: failed` (`run.go:958`), so `Unblocked` never clears it; the loop offers it (`reconcile.go:229-230`) and leaves it. The terminal nature and the recovery (revoke the task's approval / `task.cancel`) are stated in `holdOn`'s comment (`run.go:873-875`), not in the K2a inventory row. Low L1.

4. **Can either PR activate launch or change a run path?** No. `launch.Missing()` is exactly I5 Admission and I21 Admission, Fold, Effect (`capability_test.go:173-177`); `TestTable` still requires the active set to be intake, answers, admission. Runtime readers of the table use only `Active()`: `wiring.go:92` (`Evaluates`), `wiring.go:219` (`task.raise_rail` handler), `wiring.go:294` (`model_call` registration), `matrix.go:56`, `config.go:258`. `capability.Kept(r)` is read only by the `adc capabilities` view (`internal/cli/capabilities.go:64`). K2a's non-test code change is the `kept` map alone; K1's likewise. The only observable runtime change is the capabilities view showing the rows kept.

5. **Regression against main.** None found. K2a adds two tests, flips nine bits, and rewrites inventory rows; no production path changes. `TestKeptIsTheInventory` (`capability_test.go:51`) stays consistent: every flipped bit's per-invariant Status cell now reads `kept`, I12 Effect and I17 Fold stay `n/a`, and the row count stays 88.

Recurring classes checked: the new terminal hold is by design and stated (L1); `gating` returns a store read error as an error, never as "no tree" (`gate.go:241-248`); a lister that did not run is a retry, not a refusal (`run.go:893`). The accepted residual stands as the promise states it: a host fault inside a listing that ran is the candidate's refusal (entry 148), K2b's.

### Findings

**Critical:** none.
**High:** none.
**Medium:** none.

**Low**

- **L1. The terminal hold's recovery is not stated in the row.** `docs/design/m1/01-invariant-inventory.md:217` (I12 consumption) and `:215` (I12 admission, "a visible hold, below"). Input: a candidate that edits a protected path. The gate node ends held `source_invalid_config` / `runner: failed` with an empty `Pages` and no detail (`run.go:910`), and nothing clears it (`run.go:958`). The operator sees the signal on the gate and must read accept's selected result for the reason. Fix: add one sentence to the I12 consumption row: "the hold never clears (`runner.cleared`); the recovery is `task.cancel`, which abandons the task, as `holdOn` states". In a later PR, carry the cause in the hold's detail.
- **L2. The mutation scripts the rows cite are not in the repository.** `01-invariant-inventory.md:197` names `muts-k2.py`, `:217` names `muts-k2a.py` (K1's `:136` names `muts-k1.py`). Input: a reader of the tree cannot re-run "each fails with its check removed". Precedent: Opus re-ran `muts-k1.py` on the lead's copy (#409 review). Fix: commit them under `scripts/muts/` or state where they live.
- **L3. "The Python profile … holds on its profile" (`01-invariant-inventory.md:66`) conflates two paths.** A changed `.py` file is refused at accept, the candidate's `failed/test_integrity_violation` (`internal/accept/integrity.go:222`), after which the gate is the terminal hold of L1. The profile hold is the gate's (`gate.ErrSpec` → `errConfig`, `gate.go:144-146`) and the image check's. Fix: "refused at accept, fail closed; its gate holds on the profile". Same row: "22a-5d" in the I17 summary cell lacks its P prefix.

The review is also saved at ``.
