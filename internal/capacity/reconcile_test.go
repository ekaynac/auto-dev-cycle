// SPDX-License-Identifier: Apache-2.0

package capacity_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
)

// The model_call reconciler (P20, its model_call half), over a store composed as P22a will compose adcd's.

// query is a route's usage query answering u, found and err, and counting what it is asked. With entered set, it
// signals each question there and waits for release.
type query struct {
	u       modelcall.Usage
	found   bool
	err     error
	entered chan bool
	release chan bool

	mu    sync.Mutex
	asked int
}

func (q *query) Usage(context.Context, types.ModelCall) (modelcall.Usage, bool, error) {
	q.mu.Lock()
	q.asked++
	q.mu.Unlock()
	if q.entered != nil {
		q.entered <- true
		<-q.release
	}
	return q.u, q.found, q.err
}

func (q *query) times() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.asked
}

// serving says whether every call has a request being served.
type serving bool

func (s serving) Serving(types.CallID) bool { return bool(s) }

// reconciler settles w's effects, querying the stub route with q (none when nil).
func (w *world) reconciler(q *query, s modelcall.Serving) *modelcall.Reconciler {
	qs := map[types.Slug]modelcall.Query{}
	if q != nil {
		qs["stub"] = q
	}
	return &modelcall.Reconciler{Store: w.s, Actor: testActor, Queries: qs, Gateway: s}
}

func (w *world) effect(r running) eventstore.Effect {
	w.t.Helper()
	e, err := w.s.LoadEffect(context.Background(), modelcall.EffectOf(r.call))
	if err != nil {
		w.t.Fatal(err)
	}
	return e
}

// reconciliations counts r's call's provider reconciliations.
func (w *world) reconciliations(r running) int {
	w.t.Helper()
	n := 0
	w.view(func(tx *eventstore.Tx) error {
		ms, err := ledger.Measurements(tx, r.call)
		for _, m := range ms {
			if m.Kind == types.UsageMeasurementKindProviderReconciliation {
				n++
			}
		}
		return err
	})
	return n
}

// F (P20): two runners on one pending effect. The call's one request kept its floor (its usage unread), so its run
// end leaves it uncertain, and the first runner asks the provider. A second runner, while the first waits for the
// answer, does nothing: one question, one reconciliation, one settlement.
func TestTwoRunnersOnOnePendingEffect(t *testing.T) {
	w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
	r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
	w.sendThroughGateway(r, `{"choices":[]}`)
	q := &query{u: modelcall.Usage{In: 30, Out: 20}, found: true, entered: make(chan bool, 2), release: make(chan bool)}
	var once sync.Once
	unblock := func() { once.Do(func() { close(q.release) }) }
	t.Cleanup(unblock)
	rc := w.reconciler(q, serving(false))
	type result struct {
		e   eventstore.Effect
		err error
	}
	first, second := make(chan result, 1), make(chan result, 1)
	go func() { e, err := rc.Ended(context.Background(), r.call); first <- result{e, err} }()
	select {
	case <-q.entered:
	case got := <-first:
		t.Fatalf("the first runner settled %+v, %v without asking the provider", got.e, got.err)
	}
	go func() { e, err := rc.Ended(context.Background(), r.call); second <- result{e, err} }()
	select {
	case got := <-second:
		if !errors.Is(got.err, modelcall.ErrRunning) {
			t.Errorf("the second runner returned %+v, %v; want ErrRunning", got.e, got.err)
		}
	case <-q.entered:
		t.Error("the second runner asked the provider too")
	case <-time.After(10 * time.Second):
		t.Error("the second runner neither returned nor asked")
	}
	unblock()
	if got := <-first; got.err != nil || got.e.State != eventstore.EffectCompleted || got.e.LastObservation != modelcall.ObservedQueried {
		t.Fatalf("the first runner settled %+v, %v", got.e, got.err)
	}
	if n, m := q.times(), w.reconciliations(r); n != 1 || m != 1 {
		t.Fatalf("asked %d times, %d reconciliations recorded; want one each", n, m)
	}
	w.stop(r)
	if s := w.settleDerived(r); len(s.Calls) != 1 || w.liable(quota) != 0 {
		t.Fatalf("settlement %+v, liable %d; want the call settled", s, w.liable(quota))
	}
	w.replays()
}

// F (P20): an uncertain call is never released at zero. Its run ends unsettled (one delta of 60 tokens, below the
// 400-token reservation), and its lease settles it as a liability. With no query on its route, a request still
// served, no usage at the provider, or a failed query, the effect stays uncertain, nothing is recorded, and the
// liability stays 400; asked again, nothing changes. Only the provider's answer releases it.
func TestAnUncertainCallIsNeverReleasedAtZero(t *testing.T) {
	zero := &query{found: true} // the provider would say zero: asked only past the check that keeps it out
	for name, c := range map[string]struct {
		q    *query
		s    serving
		why  string
		fail bool
	}{
		"no query on the route":      {nil, false, modelcall.ObservedNoQuery, false},
		"a request still served":     {zero, true, modelcall.ObservedServing, false},
		"no usage at the provider":   {&query{}, false, modelcall.ObservedNotFound, false},
		"a failed query":             {&query{found: true, err: errors.New("unreachable")}, false, modelcall.ObservedFailed, true},
		"a negative answer, refused": {&query{u: modelcall.Usage{In: -1}, found: true}, false, modelcall.ObservedFailed, true},
	} {
		t.Run(name, func(t *testing.T) {
			w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
			r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
			if err := w.measure(r, types.UsageMeasurementKindDelta, 0, 50, 10); err != nil {
				t.Fatal(err)
			}
			w.stop(r)
			w.settleDerived(r)
			rc := w.reconciler(c.q, c.s)
			for i := range 2 {
				e, err := rc.Ended(context.Background(), r.call)
				if (err != nil) != c.fail || e.State != eventstore.EffectUncertain || e.LastObservation != c.why {
					t.Fatalf("run %d: %+v, %v; want uncertain: %s", i, e, err, c.why)
				}
			}
			if n, l := w.reconciliations(r), w.liable(quota); n != 0 || l != 400 {
				t.Fatalf("%d reconciliations, liable %d; want none, 400", n, l)
			}
			w.replays()
			// The provider's answer releases it, late: the lease settled already.
			e, err := w.reconciler(&query{u: modelcall.Usage{In: 30, Out: 20}, found: true}, serving(false)).Reconcile(context.Background(), r.call)
			if err != nil || e.State != eventstore.EffectCompleted || w.liable(quota) != 0 {
				t.Fatalf("after the answer: %+v, %v, liable %d", e, err, w.liable(quota))
			}
			w.replays()
		})
	}
}

// At run end, a call whose usage settles completes with no question; one whose usage does not is uncertain, then
// reconciled. A pending effect is left alone by Reconcile: its run goes on.
func TestRunEndMovesTheEffectOutOfPending(t *testing.T) {
	const usage = `{"choices":[],"usage":{"prompt_tokens":300,"completion_tokens":200}}`
	w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(2000)))
	settled, open := w.running(reserve(quota, 400, types.CapacityUnitTokens)), w.running(reserve(quota, 400, types.CapacityUnitTokens))
	w.sendThroughGateway(settled, usage)
	if err := w.measure(settled, types.UsageMeasurementKindFinal, 0, 300, 200); err != nil {
		t.Fatal(err)
	}
	w.sendThroughGateway(open, usage) // counted, but no final
	q := &query{u: modelcall.Usage{In: 300, Out: 200}, found: true}
	rc := w.reconciler(q, serving(false))
	if e, err := rc.Reconcile(context.Background(), open.call); err != nil || e.State != eventstore.EffectPending || q.times() != 0 {
		t.Fatalf("a running call was settled: %+v, %v", e, err)
	}
	if e, err := rc.Ended(context.Background(), settled.call); err != nil || e.State != eventstore.EffectCompleted ||
		e.LastObservation != modelcall.ObservedSettled || q.times() != 0 {
		t.Fatalf("the settled call: %+v, %v, asked %d", e, err, q.times())
	}
	if e, err := rc.Ended(context.Background(), open.call); err != nil || e.State != eventstore.EffectCompleted ||
		e.LastObservation != modelcall.ObservedQueried || q.times() != 1 {
		t.Fatalf("the open call: %+v, %v, asked %d", e, err, q.times())
	}
	w.replays()
}

// #226 entry 59: usage after the run end is expected. Once the effect has left pending, the gateway sends nothing
// more for the call (its floor is refused, so the upstream never sees the request), but a late count of a request
// already sent, and a late adapter report, are recorded. A late count that unsettles the completed call keeps it a
// liability until the provider's answer releases it; the effect stays completed.
func TestLateUsageAfterTheRunEnd(t *testing.T) {
	const usage = `{"choices":[],"usage":{"prompt_tokens":300,"completion_tokens":200}}`
	w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(2000)))
	r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
	var reached int
	var mu sync.Mutex
	g, token := w.overGateway(r, func(rw http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		reached++
		mu.Unlock()
		_, _ = io.WriteString(rw, usage)
	}, nil)
	send(t, g, token)
	if err := w.measure(r, types.UsageMeasurementKindFinal, 0, 300, 200); err != nil {
		t.Fatal(err)
	}
	q := &query{u: modelcall.Usage{In: 900, Out: 600}, found: true}
	rc := w.reconciler(q, g)
	if e, err := rc.Ended(context.Background(), r.call); err != nil || e.State != eventstore.EffectCompleted {
		t.Fatalf("run end: %+v, %v", e, err)
	}
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(asked))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	if g.ServeHTTP(rec, req); rec.Code != http.StatusServiceUnavailable || reached != 1 {
		t.Fatalf("a request after the run end: %d, the upstream reached %d times", rec.Code, reached)
	}
	b := gateway.Binding{Lease: r.lease, Attempt: r.attempt, Call: r.call}
	if err := (modelcall.Recorder{Store: w.s, Actor: testActor}).Count(context.Background(), b, 0, gateway.Count{In: 600, Out: 400, Observed: true}); err != nil {
		t.Fatalf("a late count of a request sent: %v", err)
	}
	if err := w.measure(r, types.UsageMeasurementKindDelta, 0, 10, 10); err != nil {
		t.Fatalf("a late adapter report: %v", err)
	}
	w.stop(r)
	if s := w.settleDerived(r); len(s.Liabilities) != 1 || w.liable(quota) != 1000 {
		t.Fatalf("settlement %+v, liable %d; want the late count as a liability", s, w.liable(quota))
	}
	if e, err := rc.Reconcile(context.Background(), r.call); err != nil || e.State != eventstore.EffectCompleted || q.times() != 1 || w.liable(quota) != 0 {
		t.Fatalf("the answer: %+v, %v, asked %d, liable %d", e, err, q.times(), w.liable(quota))
	}
	w.replays()
}

// The provider is asked only once no request of the call is served: while the gateway serves one, the run end
// leaves the effect uncertain and asks nothing; once it is answered, the provider is asked.
func TestNoQueryWhileARequestIsServed(t *testing.T) {
	w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
	r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
	at, release := make(chan bool), make(chan bool)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	g, token := w.overGateway(r, func(rw http.ResponseWriter, _ *http.Request) {
		at <- true
		<-release
		_, _ = io.WriteString(rw, `{"choices":[]}`)
	}, nil)
	t.Cleanup(unblock) // before the upstream closes, even on a failure
	done := make(chan bool)
	go func() { send(t, g, token); close(done) }()
	<-at
	q := &query{u: modelcall.Usage{In: 30, Out: 20}, found: true}
	rc := w.reconciler(q, g)
	if e, err := rc.Ended(context.Background(), r.call); err != nil || e.State != eventstore.EffectUncertain ||
		e.LastObservation != modelcall.ObservedServing || q.times() != 0 {
		t.Fatalf("while served: %+v, %v, asked %d", e, err, q.times())
	}
	unblock()
	<-done
	if e, err := rc.Reconcile(context.Background(), r.call); err != nil || e.State != eventstore.EffectCompleted || q.times() != 1 {
		t.Fatalf("once answered: %+v, %v, asked %d", e, err, q.times())
	}
	w.replays()
}
