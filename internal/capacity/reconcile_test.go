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
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.u, q.found, q.err
}

func (q *query) times() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.asked
}

func (q *query) answer(u modelcall.Usage) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.u = u
}

// later is a clock past every floor's grace.
func later() time.Time { return time.Now().Add(modelcall.FloorGrace + time.Minute) }

// reconciler settles w's effects, querying the stub route with q (none when nil), at a clock past every floor's
// grace.
func (w *world) reconciler(q *query) *modelcall.Reconciler {
	qs := map[types.Slug]modelcall.Query{}
	if q != nil {
		qs["stub"] = q
	}
	return &modelcall.Reconciler{Store: w.s, Actor: testActor, Queries: qs, Now: later}
}

func (w *world) effect(r running) eventstore.Effect {
	w.t.Helper()
	e, err := w.s.LoadEffect(context.Background(), modelcall.EffectOf(r.call))
	if err != nil {
		w.t.Fatal(err)
	}
	return e
}

// standing is r's call's standing in the ledger, with its provider reconciliations.
func (w *world) standing(r running) (ledger.Standing, int) {
	w.t.Helper()
	var st ledger.Standing
	n := 0
	w.view(func(tx *eventstore.Tx) error {
		ms, err := ledger.Measurements(tx, r.call)
		for _, m := range ms {
			if m.Kind == types.UsageMeasurementKindProviderReconciliation {
				n++
			}
		}
		st = ledger.StandingOf(ms, r.call)
		return err
	})
	return st, n
}

// floor records the gateway's floor of r's call's request seq, as modelcall.Recorder does before a send.
func (w *world) floor(r running, seq, in, out int64) {
	w.t.Helper()
	if err := (modelcall.Recorder{Store: w.s, Actor: testActor}).Count(context.Background(),
		gateway.Binding{Lease: r.lease, Attempt: r.attempt, Call: r.call}, seq, gateway.Count{In: in, Out: out}); err != nil {
		w.t.Fatal(err)
	}
}

type result struct {
	e   eventstore.Effect
	err error
}

// F (P20): two runners on one pending effect. The call's one request kept its floor (its usage unread), so its run
// end leaves it uncertain, and the first runner asks the provider. While it waits for the answer, a Reconcile does
// nothing (ErrRunning), and a second Ended waits for it instead of losing the run end: one question, one
// reconciliation, one settlement.
func TestTwoRunnersOnOnePendingEffect(t *testing.T) {
	w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
	r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
	w.sendThroughGateway(r, `{"choices":[]}`)
	q := &query{u: modelcall.Usage{In: 30, Out: 20}, found: true, entered: make(chan bool, 3), release: make(chan bool)}
	var once sync.Once
	unblock := func() { once.Do(func() { close(q.release) }) }
	t.Cleanup(unblock)
	rc := w.reconciler(q)
	first, third := make(chan result, 1), make(chan result, 1)
	go func() { e, err := rc.Ended(context.Background(), r.call); first <- result{e, err} }()
	select {
	case <-q.entered:
	case got := <-first:
		t.Fatalf("the first runner settled %+v, %v without asking the provider", got.e, got.err)
	}
	second := make(chan result, 1)
	go func() { e, err := rc.Reconcile(context.Background(), r.call); second <- result{e, err} }()
	select {
	case got := <-second:
		if !errors.Is(got.err, modelcall.ErrRunning) {
			t.Fatalf("a second runner returned %+v, %v; want ErrRunning", got.e, got.err)
		}
	case <-q.entered:
		t.Fatal("a second runner asked the provider too")
	}
	go func() { e, err := rc.Ended(context.Background(), r.call); third <- result{e, err} }()
	select {
	case got := <-third:
		t.Fatalf("a run end did not wait for the runner: %+v, %v", got.e, got.err)
	case <-q.entered:
		t.Fatal("a run end asked the provider too")
	case <-time.After(200 * time.Millisecond):
	}
	unblock()
	for _, c := range []chan result{first, third} {
		if got := <-c; got.err != nil || got.e.State != eventstore.EffectCompleted || got.e.LastObservation != modelcall.ObservedQueried {
			t.Fatalf("a run end settled %+v, %v", got.e, got.err)
		}
	}
	if _, n := w.standing(r); q.times() != 1 || n != 1 {
		t.Fatalf("asked %d times, %d reconciliations recorded; want one each", q.times(), n)
	}
	w.stop(r)
	if s := w.settleDerived(r); len(s.Calls) != 1 || w.liable(quota) != 0 {
		t.Fatalf("settlement %+v, liable %d; want the call settled", s, w.liable(quota))
	}
	w.replays()
}

// F (P20): an uncertain call is never released at zero. Its run ends with a floor alone (usage unread, 60 tokens,
// below the 400-token reservation), and its lease settles it as a liability. With no query on its route, its floor
// younger than the grace (its request possibly in flight), no usage at the provider, or a failed query, the effect
// stays uncertain, nothing is recorded, and the liability stays 400; asked again, nothing changes. Only the
// provider's answer releases it: the floor, unlike a count, is no bound on it.
func TestAnUncertainCallIsNeverReleasedAtZero(t *testing.T) {
	for name, c := range map[string]struct {
		q     *query
		young bool
		why   string
		fail  bool
	}{
		"no query on the route":      {nil, false, modelcall.ObservedNoQuery, false},
		"a floor still young":        {&query{found: true}, true, modelcall.ObservedYoung, false},
		"no usage at the provider":   {&query{}, false, modelcall.ObservedNotFound, false},
		"a failed query":             {&query{found: true, err: errors.New("unreachable")}, false, modelcall.ObservedFailed, true},
		"a negative answer, refused": {&query{u: modelcall.Usage{In: -1}, found: true}, false, modelcall.ObservedFailed, true},
	} {
		t.Run(name, func(t *testing.T) {
			w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
			r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
			w.floor(r, 0, 50, 10)
			w.stop(r)
			w.settleDerived(r)
			rc := w.reconciler(c.q)
			if c.young {
				rc.Now = time.Now
			}
			for i := range 2 {
				e, err := rc.Ended(context.Background(), r.call)
				if (err != nil) != c.fail || e.State != eventstore.EffectUncertain || e.LastObservation != c.why {
					t.Fatalf("run %d: %+v, %v; want uncertain: %s", i, e, err, c.why)
				}
			}
			if _, n := w.standing(r); n != 0 || w.liable(quota) != 400 {
				t.Fatalf("%d reconciliations, liable %d; want none, 400", n, w.liable(quota))
			}
			if c.q != nil && c.young && c.q.times() != 0 {
				t.Fatalf("asked %d times", c.q.times())
			}
			w.replays()
			// The provider's answer releases it, late: the lease settled already.
			e, err := w.reconciler(&query{u: modelcall.Usage{In: 30, Out: 20}, found: true}).Reconcile(context.Background(), r.call)
			if err != nil || e.State != eventstore.EffectCompleted || w.liable(quota) != 0 {
				t.Fatalf("after the answer: %+v, %v, liable %d", e, err, w.liable(quota))
			}
			w.replays()
		})
	}
}

// F (P20, Opus on #258 H1): a found zero never settles a call below the gateway's count. The gateway counted the
// response's usage (300 in, 200 out); the provider then answers zero for the call. The call settles at the count,
// flagged as below it, so its charge is the count; Reconcile asks again, and a later answer supersedes the first.
func TestAFoundZeroNeverSettlesUnderTheCount(t *testing.T) {
	const usage = `{"choices":[],"usage":{"prompt_tokens":300,"completion_tokens":200}}`
	w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
	r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
	w.sendThroughGateway(r, usage)
	q := &query{found: true}
	rc := w.reconciler(q)
	if e, err := rc.Ended(context.Background(), r.call); err != nil || e.State != eventstore.EffectCompleted {
		t.Fatalf("run end: %+v, %v", e, err)
	}
	if st, n := w.standing(r); n != 1 || !st.Settled || !st.Below || !st.Inconsistent || st.Usage.In < 300 || st.Usage.Out < 200 {
		t.Fatalf("after a zero answer: %+v (usage %+v), %d reconciliations; want settled at the count, flagged", st, st.Usage, n)
	}
	q.answer(modelcall.Usage{In: 320, Out: 210})
	if e, err := rc.Reconcile(context.Background(), r.call); err != nil || e.State != eventstore.EffectCompleted || q.times() != 2 {
		t.Fatalf("asked again: %+v, %v, asked %d", e, err, q.times())
	}
	if st, n := w.standing(r); n != 2 || !st.Settled || st.Below || st.Inconsistent || st.Usage.In != 320 || st.Usage.Out != 210 {
		t.Fatalf("after the later answer: %+v (usage %+v), %d reconciliations", st, st.Usage, n)
	}
	w.replays()
}

// F (P20, Opus on #258 H2): a restarted reconciler, whose new gateway serves nothing, never settles out a request
// still held upstream. While the request's floor is younger than the grace, the provider is not asked. Asked past it
// (here, a request held longer than the gateway's deadline would allow), its answer leaves the request out; when the
// request's count lands, the figure rises to it.
func TestARestartedReconcilerWaitsForAYoungFloor(t *testing.T) {
	const usage = `{"choices":[],"usage":{"prompt_tokens":3000,"completion_tokens":1000}}`
	w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
	r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
	at, release := make(chan bool), make(chan bool)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	g, token := w.overGateway(r, func(rw http.ResponseWriter, _ *http.Request) {
		at <- true
		<-release
		_, _ = io.WriteString(rw, usage)
	}, nil)
	t.Cleanup(unblock) // before the upstream closes, even on a failure
	done := make(chan bool)
	go func() { send(t, g, token); close(done) }()
	<-at
	q := &query{u: modelcall.Usage{In: 10}, found: true}
	restarted := w.reconciler(q)
	restarted.Now = time.Now
	if e, err := restarted.Ended(context.Background(), r.call); err != nil || e.State != eventstore.EffectUncertain ||
		e.LastObservation != modelcall.ObservedYoung || q.times() != 0 {
		t.Fatalf("with a young floor: %+v, %v, asked %d", e, err, q.times())
	}
	restarted.Now = later
	if e, err := restarted.Reconcile(context.Background(), r.call); err != nil || e.State != eventstore.EffectCompleted || q.times() != 1 {
		t.Fatalf("past the grace: %+v, %v, asked %d", e, err, q.times())
	}
	unblock()
	<-done
	if st, _ := w.standing(r); !st.Settled || !st.Below || st.Usage.In < 3000 || st.Usage.Out < 1000 {
		t.Fatalf("after the request's count: %+v (usage %+v); want at least its count", st, st.Usage)
	}
	w.replays()
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
	rc := w.reconciler(q)
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

// Opus on #258, M2: the transition to completed rechecks the standing in its own transaction. A floor recorded
// after the run end's read (here, on the Check's own path, as if the gateway committed it then) keeps the effect from
// completing on a stale settlement: it is uncertain instead.
func TestARunEndRechecksTheStanding(t *testing.T) {
	const usage = `{"choices":[],"usage":{"prompt_tokens":300,"completion_tokens":200}}`
	w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(2000)))
	r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
	w.sendThroughGateway(r, usage)
	if err := w.measure(r, types.UsageMeasurementKindFinal, 0, 300, 200); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	rc := w.reconciler(nil)
	rc.Now = func() time.Time { // read inside the run's view, after its snapshot
		once.Do(func() { w.floor(r, 7, 5, 5) })
		return later()
	}
	if e, err := rc.Ended(context.Background(), r.call); err != nil || e.State != eventstore.EffectUncertain || e.LastObservation != modelcall.ObservedOpen {
		t.Fatalf("a stale settlement: %+v, %v", e, err)
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
	rc := w.reconciler(q)
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

// In process, a request being served has a young floor and no count: the run end leaves the effect uncertain and
// asks nothing. Once the request is answered and counted, nothing is floor-only, and the provider is asked at once.
func TestNoQueryWhileARequestIsServed(t *testing.T) {
	w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
	r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
	at, release := make(chan bool), make(chan bool)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	g, token := w.overGateway(r, func(rw http.ResponseWriter, _ *http.Request) {
		at <- true
		<-release
		_, _ = io.WriteString(rw, `{"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":10}}`)
	}, nil)
	t.Cleanup(unblock) // before the upstream closes, even on a failure
	done := make(chan bool)
	go func() { send(t, g, token); close(done) }()
	<-at
	q := &query{u: modelcall.Usage{In: 30, Out: 20}, found: true}
	rc := w.reconciler(q)
	rc.Now = time.Now
	if e, err := rc.Ended(context.Background(), r.call); err != nil || e.State != eventstore.EffectUncertain ||
		e.LastObservation != modelcall.ObservedYoung || q.times() != 0 {
		t.Fatalf("while served: %+v, %v, asked %d", e, err, q.times())
	}
	unblock()
	<-done
	if e, err := rc.Reconcile(context.Background(), r.call); err != nil || e.State != eventstore.EffectCompleted || q.times() != 1 {
		t.Fatalf("once answered: %+v, %v, asked %d", e, err, q.times())
	}
	w.replays()
}

// Opus on #258, M1 and M2: two Reconcilers on one store, against P22a's one-per-store rule. One asks and waits; the
// other asks, records and completes. The first then records nothing (its sequence is taken), so one reconciliation
// stands and no observation claims a second.
func TestTwoReconcilersRecordOnce(t *testing.T) {
	w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
	r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
	w.sendThroughGateway(r, `{"choices":[]}`)
	slow := &query{u: modelcall.Usage{In: 40, Out: 20}, found: true, entered: make(chan bool, 1), release: make(chan bool)}
	var once sync.Once
	unblock := func() { once.Do(func() { close(slow.release) }) }
	t.Cleanup(unblock)
	first := make(chan result, 1)
	go func() {
		e, err := w.reconciler(slow).Ended(context.Background(), r.call)
		first <- result{e, err}
	}()
	<-slow.entered
	if e, err := w.reconciler(&query{u: modelcall.Usage{In: 30, Out: 20}, found: true}).Reconcile(context.Background(), r.call); err != nil || e.State != eventstore.EffectCompleted {
		t.Fatalf("the other reconciler: %+v, %v", e, err)
	}
	unblock()
	got := <-first
	if st, n := w.standing(r); n != 1 || st.Usage.In != 30 || w.effect(r).LastObservation != modelcall.ObservedQueried {
		t.Fatalf("%d reconciliations at %+v, first runner %+v, %v", n, st.Usage, got.e, got.err)
	}
	w.replays()
}

// L1 (Opus on #258): a provider reconciliation is recorded only by the reconciler's command.
func TestOnlyTheReconcilerRecordsAReconciliation(t *testing.T) {
	w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
	r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
	m := types.UsageMeasurement{ID: types.MeasurementID(newID(t, "msr")), Call: r.call, Attempt: r.attempt, Lease: r.lease,
		Kind: types.UsageMeasurementKindProviderReconciliation, Cost: types.UsageMeasurementCost{Currency: "USD", Basis: types.UsageMeasurementCostBasisCash},
		SourceOfTruth: types.UsageMeasurementSourceOfTruthProviderUsage}
	refused(t, w.do(func(*eventstore.Tx) ([]eventstore.Event, error) {
		ev, err := ledger.Measure(m)
		return []eventstore.Event{ev}, err
	}), "recorded only by model_call.reconcile")
	if _, n := w.standing(r); n != 0 {
		t.Fatalf("%d reconciliations recorded", n)
	}
}
