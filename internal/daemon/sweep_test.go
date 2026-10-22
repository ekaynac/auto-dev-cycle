// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// launched is a run world whose build was placed and launched with no run
// behind it (as a restart leaves it): its attempt running, its lease
// active.
func launched(t *testing.T) (*runWorld, types.Attempt) {
	t.Helper()
	rw := newRunWorld(t, true, builder(nil, false, nil))
	rw.x.Runner = nil
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	var ls types.Lease
	live := false
	for i := 0; i < 10 && !live; i++ {
		rw.passesFor(t, 1, 0)
		rw.build = templateNodes(rw.w, rw.task)[types.NodeKindBuild]
		ls, live = leaseOn(rw.w, rw.build)
	}
	if !live {
		t.Fatal("the build was not placed")
	}
	aid := types.AttemptID(newID(t, "att"))
	if miss, err := rw.r.place.launch(context.Background(), rw.w.s, rw.x.System, rw.build, ls.ID, aid, emptyBase, nil); err != nil || miss != nil {
		t.Fatalf("the launch: %v %v", miss, err)
	}
	a, _ := rw.attemptOf(t)
	return rw, a
}

// P22a-5c-2c (I8): an attempt the restart left finishing (its result
// submitted, the run cut before it recorded it) is ended at the start
// sweep: sealed, could_not_run/ambiguous_state recorded with no attempt,
// its lease revoked and settled; the build stays claimed, so the loop
// matches it again.
func TestAFinishingAttemptIsEndedAtStart(t *testing.T) {
	rw, a := launched(t)
	if err := rw.w.try(rw.w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.SubmitResult(tx, a.ID, a.FencingToken, types.SubmittedResult{Call: types.CallID(newID(t, "cal")), Outcome: types.OutcomeEmpty,
			Reason: vocab.ReasonCompletedClean})
	}); err != nil {
		t.Fatal(err)
	}
	removeStale = func(*runner, context.Context) ([]string, error) { return nil, nil }
	t.Cleanup(func() {
		removeStale = func(r *runner, ctx context.Context) ([]string, error) {
			return r.box.RemoveStale(ctx, func(string) bool { return false })
		}
	})
	if err := rw.r.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, l := rw.attemptOf(t)
	got := resultsOn(t, rw.w, rw.build)
	if a.Status != types.AttemptStatusSealed || l.State != types.LeaseStateSettled || len(got) != 1 || got[0].Attempt != nil ||
		got[0].Result.Outcome != types.OutcomeCouldNotRun || got[0].Result.Reason != vocab.ReasonAmbiguousState {
		t.Fatalf("the finishing attempt after the sweep: %s, lease %s, results %+v", a.Status, l.State, got)
	}
	if st := nodesOf(rw.w, rw.task)[types.NodeKindBuild].State; st.State != types.NodeStateStateClaimed {
		t.Errorf("the build after its attempt was ended: %s", st.State)
	}
	if err := rw.r.endLeft(context.Background()); err != nil {
		t.Errorf("a second sweep: %v", err)
	}
}

// P22a-5c-2c (Opus 22a-5c-2c M3): the run directories and clones a crash
// left. With RemoveStale's listing complete, every run directory goes,
// kept or not, but never the exchange or socket directories, and the
// clones go too. With it incomplete, only a kept directory whose attempt
// it removed goes; another kept one, and an unkept one, stay.
func TestTheSweepTearsDownWhatACrashLeft(t *testing.T) {
	for name, complete := range map[string]bool{"a complete listing": true, "an incomplete listing": false} {
		t.Run(name, func(t *testing.T) {
			root, state := t.TempDir(), t.TempDir()
			runs := filepath.Join(root, "runs")
			r := &runner{box: sandbox.Host{Root: root}, host: &adapterhost.Host{Root: filepath.Join(runs, "exchange")}, sockets: filepath.Join(runs, "gateway"),
				clones: filepath.Join(state, "clones")}
			mk := func(name, attempt string) string {
				dir := filepath.Join(runs, name)
				if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o700); err != nil {
					t.Fatal(err)
				}
				if attempt != "" {
					if err := os.WriteFile(filepath.Join(dir, keptName), []byte(attempt), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return dir
			}
			removed, kept := newID(t, "att"), newID(t, "att")
			gone, stays, own := mk("a1", removed), mk("a2", kept), mk("a3", "")
			exch, socks := mk("exchange", ""), mk("gateway", "")
			clone := filepath.Join(r.clones, "att_x", "a.go")
			if err := os.MkdirAll(filepath.Dir(clone), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(clone, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := errors.Join(r.tearDownRuns([]string{removed}, complete), r.removeClones()); err != nil {
				t.Fatal(err)
			}
			want := map[string]bool{gone: false, stays: !complete, own: !complete, exch: true, socks: true, filepath.Dir(clone): false}
			for dir, w := range want {
				if _, err := os.Stat(dir); (err == nil) != w {
					t.Errorf("%s exists %v, want %v", filepath.Base(dir), err == nil, w)
				}
			}
		})
	}
}

// P22a-5c-2c: the sweep removes the stale containers, the run directory
// and the clone a crash left (its listing complete: Opus 22a-5c-2c M3),
// then refuses the output a restart-cut attempt left in its exchange root
// (stale_attempt).
func TestTheStartSweepRefusesAnOrphansOutput(t *testing.T) {
	rw, a := launched(t)
	if _, err := recoverAtStart(context.Background(), rw.w.s, rw.x.System, rw.w.blobs, false); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(rw.r.host.Root, adapterhost.ExchangeName(a.ID), "out")
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(out), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "late.txt"), []byte("late"), 0o600); err != nil {
		t.Fatal(err)
	}
	var swept []string
	removeStale = func(*runner, context.Context) ([]string, error) { swept = append(swept, "swept"); return nil, nil }
	t.Cleanup(func() {
		removeStale = func(r *runner, ctx context.Context) ([]string, error) {
			return r.box.RemoveStale(ctx, func(string) bool { return false })
		}
	})
	left := []string{filepath.Join(rw.r.box.Root, "runs", "a1"), filepath.Join(rw.r.clones, "att_left")}
	for _, d := range left {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := rw.r.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, d := range left {
		if _, err := os.Stat(d); err == nil {
			t.Errorf("the sweep left %s", d)
		}
	}
	var refused bool
	rw.w.view(func(tx *eventstore.Tx) error {
		_, ok, err := tx.Get(lease.RefusalProjection, string(a.ID))
		refused = ok
		return err
	})
	if !slices.Equal(swept, []string{"swept"}) || !refused {
		t.Errorf("the sweep: removed stale %v, the orphan's output refused %v", swept, refused)
	}
	if _, err := os.Stat(filepath.Dir(out)); err == nil {
		t.Error("the orphan's exchange root was left")
	}
}

// Opus 22a-5c-2c M2: one adcd a sandbox root: a second lock on it is
// refused controller_already_running while the first is held, and taken
// once it is released.
func TestOneAdcdASandboxRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sbx")
	unlock, err := lockSandbox(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = lockSandbox(root)
	startErr(t, err, vocab.CouldNotRun, vocab.ReasonControllerAlreadyRunning)
	unlock()
	again, err := lockSandbox(root)
	if err != nil {
		t.Fatalf("the lock after its release: %v", err)
	}
	again()
}

// unsealedRun is a run world whose build ran, recorded its patch and never
// sealed (its seal's command failing), as a crash between the two leaves
// it: the attempt finishing, its lease active. The seal works again after.
func unsealedRun(t *testing.T) (*runWorld, types.Attempt, types.Result) {
	t.Helper()
	sealWith = func(*eventstore.Tx, types.AttemptID, int64, lease.Units, lease.Publisher, *types.Digest, []string, types.ActorID) ([]eventstore.Event, error) {
		return nil, errors.New("the seal's blob could not be published")
	}
	t.Cleanup(func() { sealWith = lease.SealWith })
	rw := newRunWorld(t, true, builder(writes(map[string]string{"a.go": "package a\n\nconst X = 1\n"}), false, nil))
	rw.untilRun(t)
	sealWith = lease.SealWith
	removeStale = func(*runner, context.Context) ([]string, error) { return nil, nil }
	t.Cleanup(func() {
		removeStale = func(r *runner, ctx context.Context) ([]string, error) {
			return r.box.RemoveStale(ctx, func(string) bool { return false })
		}
	})
	a, l := rw.attemptOf(t)
	var res types.Result
	var byAttempt bool
	rw.w.view(func(tx *eventstore.Tx) (err error) {
		res, byAttempt, err = graph.AttemptResult(tx, a.ID)
		return err
	})
	if a.Status != types.AttemptStatusFinishing || l.State != types.LeaseStateActive || !byAttempt || len(res.Value) == 0 {
		t.Fatalf("the unsealed run: %s, lease %s, its result %v %+v", a.Status, l.State, byAttempt, res)
	}
	return rw, a, res
}

// Opus 22a-5c-2c M4: a finishing attempt with its own patch result is
// sealed by the sweep with that patch as its diff (the sealed record's
// partial_diff is the patch's content digest), completed with its result,
// and its lease completed and settled.
func TestTheSweepSealsAFinishingAttemptsPatch(t *testing.T) {
	rw, _, res := unsealedRun(t)
	if err := rw.r.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, l := rw.attemptOf(t)
	if a.Checkpoint == nil || a.Status != types.AttemptStatusCompleted || l.State != types.LeaseStateSettled {
		t.Fatalf("the finishing attempt after the sweep: %s (checkpoint %v), lease %s", a.Status, a.Checkpoint, l.State)
	}
	b, err := blobOf(t, rw.w, *a.Checkpoint)
	var rec types.AttemptRecord
	if err == nil {
		err = json.Unmarshal(b, &rec)
	}
	if err != nil || rec.PartialDiff == nil || *rec.PartialDiff != res.Value[0].ContentDigest {
		t.Errorf("the sealed record's partial diff %v, the result's patch %s (%v)", rec.PartialDiff, res.Value[0].ContentDigest, err)
	}
}

// Opus 22a-5c-2c M4: a completed attempt whose lease was left active (the
// run cut before its lease's end) has its lease completed and settled by
// the sweep.
func TestTheSweepEndsACompletedAttemptsLease(t *testing.T) {
	rw, a, res := unsealedRun(t)
	diff := res.Value[0].ContentDigest
	if err := rw.r.command(context.Background(), rw.r.sealStep(a.ID, &diff, nil)); err != nil {
		t.Fatal(err)
	}
	if err := rw.w.try(rw.w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		_, rev, _, err := lease.AttemptAt(tx, a.ID)
		if err != nil {
			return nil, err
		}
		return lease.ChangeAttempt(tx, a.ID, rev, lease.AttemptEvent{Kind: lease.AttemptDone,
			Completion: &types.AttemptCompletionValue{Outcome: res.Outcome, Reason: types.ReasonCode(res.Reason)}})
	}); err != nil {
		t.Fatal(err)
	}
	if a, l := rw.attemptOf(t); a.Status != types.AttemptStatusCompleted || l.State != types.LeaseStateActive {
		t.Fatalf("before the sweep: %s, lease %s", a.Status, l.State)
	}
	if err := rw.r.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, l := rw.attemptOf(t); l.State != types.LeaseStateSettled {
		t.Errorf("the completed attempt's lease after the sweep: %s", l.State)
	}
}

// Opus 22a-5c-2c L2: Reseal forgets a node no longer waiting: its clock and
// its tries go, so a node that waits again starts its clock again.
func TestResealForgetsANodeNoLongerWaiting(t *testing.T) {
	resealAfter = time.Hour
	t.Cleanup(func() { resealAfter = 2 * time.Minute })
	r := &runner{}
	a, b := types.NodeID(newID(t, "nod")), types.NodeID(newID(t, "nod"))
	r.Reseal([]types.NodeID{a})
	r.resealTries = map[types.NodeID]int{a: 2}
	r.Reseal([]types.NodeID{b})
	if _, ok := r.waiting[a]; ok || r.resealTries[a] != 0 {
		t.Errorf("a node no longer waiting is remembered: %v, %d tries", r.waiting, r.resealTries[a])
	}
	if _, ok := r.waiting[b]; !ok {
		t.Error("the waiting node is not")
	}
}
