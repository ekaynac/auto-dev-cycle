// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/recovery"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// cutStore is a store a controller died in, mid-call: a claimed node whose
// attempt runs under its lease, with one model call intended.
func cutStore(t *testing.T) (dir string, cfg Config, a types.AttemptID, l types.LeaseID, call types.CallID) {
	t.Helper()
	dir = stateDir(t)
	w := seed(t, dir, daemonProjection(t, tokens(10)))
	sys := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
	sys.CreatedBy = sys.ID
	w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(sys) })
	_, admitted := admittedTask(t, w, sys.ID) // a claim takes an admitted task (P22a-3)
	n := types.Node{ID: types.NodeID(newID(t, "nod")), Kind: types.NodeKindPlan, DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{}`),
		Inputs: []types.InputSpec{}, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}
	w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Record(types.NodeRecorded{Task: admitted, Project: graphProject, Node: n})
	})
	w.do(sys.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Claim(tx, n.ID, map[string]graph.Resolved{}, 1)
	})
	ls := w.lease(4)
	ls.Unit = graph.UnitOf(n.ID)
	w.do(sys.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, ls) })
	w.do(sys.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, ls.ID, 1, lease.LeaseEvent{Kind: lease.Granted}, w.holds)
	})
	a, call = types.AttemptID(newID(t, "att")), types.CallID(newID(t, "cal"))
	w.do(sys.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.StartAttempt(tx, ls.ID, 2, a, types.AttemptBase{Tree: "git:4b825dc642cb6eb9a060e54bf8d69288fbee4904", ContractRevision: "1"})
	})
	w.do(sys.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return modelcall.Intend(tx, gateway.Binding{Lease: ls.ID, Attempt: a, Call: call}, "stub")
	})
	w.done()
	return dir, Config{StateDir: dir, Groups: []types.ResourceGroup{tokens(10)}}, a, ls.ID, call
}

// cutState is the attempt, its lease and the call's effect, read from the store.
func cutState(t *testing.T, d *Daemon, a types.AttemptID, l types.LeaseID, call types.CallID) (types.Attempt, types.Lease, eventstore.Effect) {
	t.Helper()
	var at types.Attempt
	var ls types.Lease
	if err := d.store.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		at, _, _, err = lease.AttemptAt(tx, a)
		if err == nil {
			ls, _, _, err = lease.LeaseOf(tx, l)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e, err := d.store.LoadEffect(context.Background(), modelcall.EffectOf(call))
	if err != nil {
		t.Fatal(err)
	}
	return at, ls, e
}

// minSeq is the least seq of the events where (an SQL condition) selects,
// 0 with none.
func minSeq(t *testing.T, dir, where string) int {
	t.Helper()
	return count(t, dir, `SELECT COALESCE(MIN(seq), 0) FROM events WHERE `+where)
}

// P22a-1 (#226 entries 64, 70, 81): a start marks the restart (a fresh
// store's too), then recovers the cut attempt before anything else: lost
// and sealed, its lease settled with the call, uncertain, kept as a
// liability, which the model calls' bindings (projected in adcd) give. In
// the log, the restart rule's transition comes first, then attempt.lost,
// then whatever else the start appends, if anything (Opus #297 M2, L5).
// This store has an operator, since a claim takes an admitted task
// (P22a-3), so the bootstrap appends nothing, and on every store with a cut
// attempt the log cannot show recovery before the bootstrap: the types keep
// that order (recoveredStore, which openBootstrap takes). A second start appends no event; it records one
// recovery.lose command with none (Opus #297 L4).
func TestStartRecoversACutAttempt(t *testing.T) {
	fresh := restart(t, stateDir(t))
	if !fresh.store.RestartMarked() {
		t.Error("a fresh store is not marked")
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
	dir, cfg, a, l, call := cutStore(t)
	before := lastSeq(t, dir)
	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	at, ls, e := cutState(t, d, a, l, call)
	if !d.store.RestartMarked() || at.Status != types.AttemptStatusSealed || at.LostReason == nil || *at.LostReason != types.AttemptLostReasonValueControllerRestart ||
		ls.State != types.LeaseStateSettled || ls.Settlement == nil || !slices.Contains(ls.Settlement.Liabilities, call) || e.State != eventstore.EffectUncertain {
		t.Fatalf("after the start: attempt %+v, lease %s %+v, effect %s", at, ls.State, ls.Settlement, e.State)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	after := fmt.Sprintf("seq > %d AND ", before)
	uncertain, lost := minSeq(t, dir, after+`kind = 'effect.uncertain/v1'`), minSeq(t, dir, after+`kind = 'attempt.lost/v1'`)
	other := minSeq(t, dir, after+`kind NOT LIKE 'effect.%' AND kind NOT LIKE 'attempt.%' AND kind NOT LIKE 'lease.%'`)
	if uncertain == 0 || lost <= uncertain || other != 0 && other <= lost {
		t.Errorf("the start's order: the restart rule at %d, attempt.lost at %d, the first other event at %d", uncertain, lost, other)
	}
	head := lastSeq(t, dir)
	d, err = Start(context.Background(), cfg)
	if err == nil {
		err = d.Close()
	}
	if err != nil || lastSeq(t, dir) != head {
		t.Errorf("a second start: %v, the log from %d to %d", err, head, lastSeq(t, dir))
	}
}

// A recovery that leaves an attempt running refuses the start, as an
// ambiguous state when the store reads (Opus #297 L3); one that fails
// after closing every running attempt only notes it, with its cause
// (Opus #297 M1), and the start goes on (#226 entry 81).
func TestStartRecoveryFailures(t *testing.T) {
	t.Cleanup(func() { runRecovery = func(ctx context.Context, r recovery.Recovery) error { return r.Run(ctx) } })
	_, cfg, _, _, _ := cutStore(t)
	runRecovery = func(context.Context, recovery.Recovery) error { return errors.New("injected") }
	refused(t, cfg.StateDir, cfg, vocab.CouldNotRun, vocab.ReasonAmbiguousState, "did not close every running attempt")
	runRecovery = func(ctx context.Context, r recovery.Recovery) error {
		return errors.Join(r.Run(ctx), errors.New("injected"))
	}
	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.note, "restart recovery left work for the next start (it resumes there): injected") {
		t.Errorf("the note: %q", d.note)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

// failOnce is a publisher whose first Put fails.
type failOnce struct {
	lease.Publisher
	failed *bool
}

func (f failOnce) Put(r io.Reader, declared string) (string, error) {
	if !*f.failed {
		*f.failed = true
		return "", fmt.Errorf("injected: %w", syscall.EACCES)
	}
	return f.Publisher.Put(r, declared)
}

// A seal that fails once (Opus #297 L5): the start notes it and goes on,
// the attempt lost and its lease not settled; the next start resumes the
// recovery there, to a sealed attempt and a settled lease keeping the
// call's liability.
func TestStartResumesRecovery(t *testing.T) {
	t.Cleanup(func() { runRecovery = func(ctx context.Context, r recovery.Recovery) error { return r.Run(ctx) } })
	_, cfg, a, l, call := cutStore(t)
	failed := false
	runRecovery = func(ctx context.Context, r recovery.Recovery) error {
		r.Blobs = failOnce{r.Blobs, &failed}
		return r.Run(ctx)
	}
	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	at, ls, _ := cutState(t, d, a, l, call)
	if !failed || !strings.Contains(d.note, "permission denied") || at.Status != types.AttemptStatusLost || ls.State == types.LeaseStateSettled {
		t.Errorf("the first start: note %q, attempt %s, lease %s", d.note, at.Status, ls.State)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if d, err = Start(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	at, ls, _ = cutState(t, d, a, l, call)
	if d.note != "" && strings.Contains(d.note, "restart recovery") || at.Status != types.AttemptStatusSealed || ls.State != types.LeaseStateSettled ||
		ls.Settlement == nil || !slices.Contains(ls.Settlement.Liabilities, call) {
		t.Errorf("the next start: note %q, attempt %s, lease %s %+v", d.note, at.Status, ls.State, ls.Settlement)
	}
}

// The controller's own commands are the system actor's alone: each is a
// system action, and none is a registered action a principal could be
// authorized for.
func TestSystemActions(t *testing.T) {
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{recovery.ActionLose, string(recovery.Revoke), string(recovery.Seal), string(recovery.Settle), recovery.ActionRefuse,
		modelcall.Action, modelcall.Counted, "model_call.reconcile", reconcile.AdmitAction, reconcile.AbandonAction, reconcile.EvaluateAction} {
		if !slices.Contains(systemActions, a) {
			t.Errorf("%s is not a system action", a)
		}
		if _, registered := reg.Actions[a]; registered {
			t.Errorf("%s is a registered action", a)
		}
	}
}
