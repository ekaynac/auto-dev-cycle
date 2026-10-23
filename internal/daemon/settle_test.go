// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
)

// providerSettled records, on the store at dir (no daemon on it), a
// provider reconciliation of call: its usage settles.
func providerSettled(t *testing.T, dir string, a types.AttemptID, l types.LeaseID, call types.CallID, also ...func(*seeder)) {
	t.Helper()
	w := seed(t, dir, daemonProjection(t, tokens(10)))
	for _, f := range also {
		f(w)
	}
	m := types.UsageMeasurement{ID: types.MeasurementID(newID(t, "msr")), Call: call, Attempt: a, Lease: l,
		Kind: types.UsageMeasurementKindProviderReconciliation, Tokens: types.UsageMeasurementTokens{In: 3, Out: 1},
		Cost:          types.UsageMeasurementCost{Currency: "USD", Basis: types.UsageMeasurementCostBasisCash, Estimate: true},
		SourceOfTruth: types.UsageMeasurementSourceOfTruthProviderUsage}
	ev, err := ledger.Measure(m)
	cmd := newID(t, "cmd")
	if err == nil {
		_, err = w.s.SubmitApplied(context.Background(), eventstore.Command{ID: cmd, Actor: newID(t, "act"), Action: modelcall.Reconciled, IdempotencyKey: cmd, Input: ev.Payload},
			eventstore.Applies(func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				return []eventstore.Event{ev}, json.RawMessage(`{}`), nil
			}))
	}
	if err != nil {
		t.Fatal(err)
	}
	w.done()
}

// passOnce runs one pass of a loop whose only executor part is e (the
// loop's timer runs the same Pass), then waits for the settlement it
// started.
func passOnce(t *testing.T, d *Daemon, e *egress) {
	t.Helper()
	l := reconcile.NewLoop(reconcile.Executor{Store: d.store, System: d.system, Settler: e}, reconcile.Gates{}, time.Hour, io.Discard)
	if _, err := l.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.wait()
}

// effectOf is call's effect on d's store.
func effectOf(t *testing.T, s *eventstore.Store, call types.CallID) eventstore.Effect {
	t.Helper()
	e, err := s.LoadEffect(context.Background(), modelcall.EffectOf(call))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// failingQuery is a usage query that fails.
type failingQuery struct{}

func (failingQuery) Usage(context.Context, types.ModelCall) (modelcall.Usage, bool, error) {
	return modelcall.Usage{}, false, errors.New("the provider's usage endpoint answered 503")
}

// Opus #409 M1 (A6): the model calls' settlement driver. A call a restart
// left uncertain whose usage has since settled (a provider
// reconciliation) is settled by the next start, after recovery and before
// the loop's first pass (adcd's ready); on a store started without it, by
// a pass. One whose usage does not settle stays uncertain and keeps its
// liability (nothing released at zero); its tries back off (one per due
// retry, however many passes), and a failing usage query is logged once
// while its cause holds. A pending call of an ended attempt (its end's
// settlement failed) is settled by the Reconciler's end rule (Ended); one
// whose attempt is lost but not sealed is left to its end.
func TestTheSettlementDriver(t *testing.T) {
	t.Run("a settled call, at the next start", func(t *testing.T) {
		dir, cfg, a, l, call := cutStore(t)
		providerSettled(t, dir, a, l, call)
		cfg.reconcileEvery = time.Hour
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		var at eventstore.Effect
		go func() {
			done <- Run(ctx, cfg, io.Discard, func(d *Daemon) { at = effectOf(t, d.store, call); cancel() }) // before the loop's first pass
		}()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if at.State != eventstore.EffectCompleted {
			t.Fatalf("the restart-left call at adcd's ready: %s (%q)", at.State, at.LastObservation)
		}
	})
	t.Run("a settled call, on a pass", func(t *testing.T) {
		dir, cfg, a, l, call := cutStore(t)
		providerSettled(t, dir, a, l, call)
		d, err := Start(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Close() })
		e, err := newEgress(d, registry.Registry{}, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := effectOf(t, d.store, call); got.State != eventstore.EffectUncertain {
			t.Fatalf("after the start: %s", got.State)
		}
		passOnce(t, d, e)
		if got := effectOf(t, d.store, call); got.State != eventstore.EffectCompleted {
			t.Fatalf("after a pass: %s (%q)", got.State, got.LastObservation)
		}
	})
	t.Run("an unsettled call keeps its liability", func(t *testing.T) {
		_, cfg, _, l, call := cutStore(t)
		d, err := Start(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Close() })
		log := &logBuffer{}
		e, err := newEgress(d, registry.Registry{}, false)
		if err != nil {
			t.Fatal(err)
		}
		e.log = log
		liable := func() (int64, types.Lease) {
			var n int64
			var ls types.Lease
			if err := d.store.View(context.Background(), func(tx *eventstore.Tx) (err error) {
				if n, err = capacity.Liable(tx, cfg.Groups[0].ID); err == nil {
					ls, _, _, err = lease.LeaseOf(tx, l)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return n, ls
		}
		before, _ := liable()
		tries := func() int {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.retries[call].tries
		}
		due := func() {
			e.mu.Lock()
			r := e.retries[call]
			r.next = time.Now()
			e.retries[call] = r
			e.mu.Unlock()
		}
		passOnce(t, d, e)
		passOnce(t, d, e) // within the backoff: not tried
		after, ls := liable()
		if got := effectOf(t, d.store, call); got.State != eventstore.EffectUncertain || got.LastObservation != modelcall.ObservedNoQuery || before == 0 || after != before ||
			ls.Settlement == nil || !slices.Contains(ls.Settlement.Liabilities, call) || tries() != 1 {
			t.Fatalf("the unsettled call: %s (%q), liable %d then %d, settlement %+v, %d tries", got.State, got.LastObservation, before, after, ls.Settlement, tries())
		}
		e.recon.Queries = map[types.Slug]modelcall.Query{"stub": failingQuery{}}
		due()
		passOnce(t, d, e)
		due()
		passOnce(t, d, e)
		if n := strings.Count(log.String(), "could not be settled"); n != 1 || tries() != 3 {
			t.Errorf("two failed queries: %d tries, logged %d times: %s", tries(), n, log.String())
		}
		if got := effectOf(t, d.store, call); got.State != eventstore.EffectUncertain {
			t.Errorf("after the failed queries: %s", got.State)
		}
		if after, _ := liable(); after != before {
			t.Errorf("liable %d after the failed queries, %d before", after, before)
		}
	})
	t.Run("adcd's loop runs it", func(t *testing.T) {
		e := &egress{}
		if l := reconciler(&Daemon{}, registry.Registry{}, nil, e, nil, 0, io.Discard); l.Exec.Settler != reconcile.Settler(e) {
			t.Errorf("adcd's loop settles through %v, want its egress", l.Exec.Settler)
		}
	})
	t.Run("a pending call of an ended attempt", func(t *testing.T) {
		dir, _, a, l, call := cutStore(t)
		sys := types.ActorID(newID(t, "act"))
		providerSettled(t, dir, a, l, call, func(w *seeder) {
			w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.LoseRunning(tx, a, types.AttemptLostReasonValueAgentDisconnect)
			})
		})
		w := seed(t, dir, daemonProjection(t, tokens(10)))
		t.Cleanup(w.done)
		e := &egress{recon: &modelcall.Reconciler{Store: w.s, Actor: string(sys), Clock: &modelcall.Clock{}}}
		if err := e.settleEnded(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := effectOf(t, w.s, call); got.State != eventstore.EffectPending {
			t.Fatalf("the call of an attempt lost, not sealed (its end may still settle it): %s", got.State)
		}
		w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			_, rev, _, err := lease.AttemptAt(tx, a)
			if err != nil {
				return nil, err
			}
			return lease.SealMinimal(tx, a, rev, graph.Units{Tasks: authority.Tasks{}}, w.blobs, nil, sys)
		})
		if err := e.settleEnded(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := effectOf(t, w.s, call); got.State != eventstore.EffectCompleted {
			t.Fatalf("the pending call of an ended attempt: %s (%q)", got.State, got.LastObservation)
		}
	})
}
