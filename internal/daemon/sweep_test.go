// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
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

// P22a-5c-2c: a run directory kept for an unverified end is torn down only
// when the sweep removed its attempt's container; another kept one, and a
// run's own, stay.
func TestAKeptDirectoryIsTornDownOnlyOnceItsContainerIsRemoved(t *testing.T) {
	root := t.TempDir()
	r := &runner{box: sandbox.Host{Root: root}}
	mk := func(name, attempt string) string {
		dir := filepath.Join(root, "runs", name)
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
	if err := r.tearDownKept([]string{removed}); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]bool{gone: false, stays: true, own: true} {
		if _, err := os.Stat(dir); (err == nil) != want {
			t.Errorf("%s exists %v, want %v", filepath.Base(dir), err == nil, want)
		}
	}
}

// P22a-5c-2c: the sweep removes the stale containers, then refuses the
// output a restart-cut attempt left in its exchange root (stale_attempt).
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
	if err := rw.r.sweep(context.Background()); err != nil {
		t.Fatal(err)
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
