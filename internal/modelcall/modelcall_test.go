// SPDX-License-Identifier: Apache-2.0

package modelcall_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	config  = "../../fixtures/m1/config/"
	actor   = "act_01m3250v0aer9ss2097svmefj8"
	builder = types.ExecutorID("exe_01m4tm99haemqt8es2qfbwj8pa") // registry-stub.json: destination stub
	leaseID = types.LeaseID("lea_01m4tm99haemss9tvzd438vq8j")
	attempt = types.AttemptID("att_01m4tm99haemss9tvzd438vq8j")
	unit    = types.UnitID("unt_01m3250vz2ezn81cgvbkcn17jd")
	callID  = types.CallID("cal_01m4tm99haemss9tvzd438vq8j")
	rgB     = types.ResourceGroupID("rg_01m4tm99haemtbemfkdve21grg")
	rgA     = types.ResourceGroupID("rg_01m4tm99haemss9tvzd438vq8j")
	good    = `{"model":"stub-builder-1","messages":[{"role":"user","content":"hi"}]}`
)

var binding = gateway.Binding{Lease: leaseID, Attempt: attempt, Call: callID}

// world is a controller's state directory, opened and closed like adcd's.
type world struct {
	t     *testing.T
	dir   string
	lock  *eventstore.Lock
	s     *eventstore.Store
	holds map[string]types.Hold
}

func openWorld(t *testing.T) *world {
	t.Helper()
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, dir: filepath.Join(t.TempDir(), "state"), holds: reg.Holds}
	w.open()
	t.Cleanup(w.close)
	w.restart()
	return w
}

// restart is adcd's start rule: every pending effect becomes uncertain.
func (w *world) restart() int {
	w.t.Helper()
	n, err := w.s.MarkPendingUncertain(context.Background(), actor)
	if err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *world) open() {
	w.t.Helper()
	lock, err := eventstore.AcquireLock(w.dir)
	if err != nil {
		w.t.Fatal(err)
	}
	blobs, err := blobstore.Open(w.dir)
	if err != nil {
		w.t.Fatal(err)
	}
	ok := func(*eventstore.Tx, eventstore.Command) error { return nil }
	kinds := lease.Kinds()
	maps.Copy(kinds, modelcall.Kinds())
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: kinds, Blobs: blobs, Effects: modelcall.Effects(),
		Hooks: eventstore.Hooks{Identify: ok, Authorize: ok, Project: eventstore.ProjectEach(
			lease.Projector{Blobs: blobs, Holds: w.holds, Units: units{}}.Project, modelcall.Project)}})
	if err != nil {
		w.t.Fatal(err)
	}
	w.lock, w.s = lock, s
}

// close is the process ending: what is committed stays, nothing else.
func (w *world) close() {
	if w.s != nil {
		w.s.Close()
		w.lock.Release()
		w.s = nil
	}
}

func (w *world) do(fn func(tx *eventstore.Tx) ([]eventstore.Event, error)) error {
	w.t.Helper()
	cmd, err := ids.New("cmd")
	if err != nil {
		w.t.Fatal(err)
	}
	_, err = w.s.SubmitApplied(context.Background(), eventstore.Command{ID: cmd, Actor: actor, Action: "test.command", IdempotencyKey: cmd, Input: []byte(`{}`)},
		eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := fn(tx)
			return evs, json.RawMessage(`{}`), err
		}))
	return err
}

// units binds every unit to one project and task, and lets it start.
type units struct{}

func (units) ProjectTask(*eventstore.Tx, types.UnitID) (types.ProjectID, types.TaskID, bool, error) {
	return "prj_01m3250vkwffc979nzjw0g2sf9", "tsk_01m3250v9xft08ssr4ygq6g2np", true, nil
}

func (units) Startable(*eventstore.Tx, types.UnitID) error { return nil }

func (w *world) must(fn func(tx *eventstore.Tx) ([]eventstore.Event, error)) {
	w.t.Helper()
	if err := w.do(fn); err != nil {
		w.t.Fatal(err)
	}
}

// start records the lease, reserving on groups in order, granted and
// running attempt, through internal/lease's commands and projector.
func (w *world) start(groups ...types.ResourceGroupID) {
	w.t.Helper()
	w.startOn(leaseID, unit, attempt, groups...)
}

// startOn is start for another lease, unit and attempt.
func (w *world) startOn(leaseID types.LeaseID, unit types.UnitID, attempt types.AttemptID, groups ...types.ResourceGroupID) {
	w.t.Helper()
	ok := types.PlacementCheck{Outcome: types.Outcome(vocab.Empty), Reason: vocab.ReasonCompletedClean}
	l := types.Lease{ID: leaseID, Unit: unit, Role: "builder", Executor: builder, State: types.LeaseStateRequested,
		Reservations: []types.LeaseReservationsItem{}, Footprint: types.LeaseFootprint{ContextTokens: 1000, ExpectedTurns: 5},
		Deadline: "2026-10-14T14:00:00Z", PlacementChecks: types.LeasePlacementChecks{DataFloor: ok, Destinations: ok, Capability: ok,
			Independence: ok, PaidAuthority: ok, Budget: ok, FallbackScope: ok}}
	for _, g := range groups {
		l.Reservations = append(l.Reservations, types.LeaseReservationsItem{Group: g, Amount: 1000, Unit: types.CapacityUnitTokens})
	}
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, l) })
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, leaseID, 1, lease.LeaseEvent{Kind: lease.Granted}, w.holds)
	})
	base := types.AttemptBase{Tree: types.GitID("git:" + strings.Repeat("c", 40)), ContractRevision: "7"}
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.StartAttempt(tx, leaseID, 2, attempt, base)
	})
}

// stop commits a stop of the running attempt, as graph.SupersedeMoving
// does: the attempt is stopping, and the lease stays active on it.
func (w *world) stop() {
	w.t.Helper()
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.StopRunning(tx, attempt, types.AttemptStopReasonValuePreempt)
	})
}

// reserve rewrites the lease's reservations in its projection, outside its
// events: only to show what a binding keeps once made.
func (w *world) reserve(groups ...types.ResourceGroupID) {
	w.t.Helper()
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		l, rev, _, err := lease.LeaseOf(tx, leaseID)
		if err != nil {
			return nil, err
		}
		l.Reservations = nil
		for _, g := range groups {
			l.Reservations = append(l.Reservations, types.LeaseReservationsItem{Group: g, Amount: 1, Unit: types.CapacityUnitTokens})
		}
		raw, err := json.Marshal(map[string]any{"revision": rev, "record": l})
		if err != nil {
			return nil, err
		}
		return nil, tx.Put(lease.LeaseProjection, string(leaseID), raw)
	})
}

func (w *world) view(fn func(tx *eventstore.Tx) error) {
	w.t.Helper()
	if err := w.s.View(context.Background(), fn); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) ofLease() []modelcall.Bound {
	w.t.Helper()
	var out []modelcall.Bound
	w.view(func(tx *eventstore.Tx) (err error) {
		out, err = modelcall.OfLease(tx, leaseID)
		return err
	})
	return out
}

type secrets struct{}

func (secrets) Credential(types.Slug) (gateway.Credential, bool, error) {
	return gateway.NewCredential("fake-credential-0123456789"), true, nil // a test value
}

// upstream is a loopback provider counting what reached it.
type upstream struct {
	*httptest.Server
	mu sync.Mutex
	n  int
}

func newUpstream(t *testing.T, seen func()) *upstream {
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		u.mu.Lock()
		u.n++
		u.mu.Unlock()
		if seen != nil {
			seen()
		}
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.n
}

// newGateway is a gateway over the stub registry pointed at up, recording
// intents through in, with binding bound.
func newGateway(t *testing.T, w *world, up *upstream, in gateway.Intents) (*gateway.Gateway, string) {
	return newGatewayWith(t, w, up, in, modelcall.Reader{Store: w.s})
}

// newGatewayWith is newGateway with attempts as its attempt reader.
func newGatewayWith(t *testing.T, w *world, up *upstream, in gateway.Intents, attempts gateway.Attempts) (*gateway.Gateway, string) {
	t.Helper()
	doc, err := registry.Load(config + "registry-stub.json")
	if err != nil {
		t.Fatal(err)
	}
	for i := range doc.Executors {
		if strings.HasPrefix(doc.Executors[i].Destination.Endpoint, "http://127.0.0.1:") {
			doc.Executors[i].Destination.Endpoint = up.URL + "/v1"
		}
	}
	var groups []types.ResourceGroup
	raw, err := os.ReadFile(config + "groups.json")
	if err == nil {
		err = json.Unmarshal(raw, &groups)
	}
	if err != nil {
		t.Fatal(err)
	}
	r, err := registry.New(doc, groups)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gateway.New(gateway.Config{Registry: r, Leases: modelcall.Reader{Store: w.s}, Attempts: attempts, Secrets: secrets{}, Intents: in})
	if err != nil {
		t.Fatal(err)
	}
	token, err := g.Bind(binding)
	if err != nil {
		t.Fatal(err)
	}
	return g, token
}

func send(g *gateway.Gateway, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(good))
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, r)
	return rec
}

// When the upstream sees the call's first request, the call is bound and
// its effect pending, committed: call → attempt → lease → the lease's
// groups, sorted. A second request of the call adds no intent.
func TestTheIntentIsCommittedBeforeTheGatewaySends(t *testing.T) {
	w := openWorld(t)
	w.start(rgB, rgA)
	var atSend [][]modelcall.Bound
	up := newUpstream(t, func() { atSend = append(atSend, w.ofLease()) })
	g, token := newGateway(t, w, up, modelcall.Recorder{Store: w.s, Actor: actor})
	for range 2 {
		if rec := send(g, token); rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	}
	want := types.ModelCall{Call: callID, Attempt: attempt, Lease: leaseID, Executor: builder, Destination: "stub", Groups: []types.ResourceGroupID{rgA, rgB}}
	if len(atSend) != 2 {
		t.Fatalf("%d sends", len(atSend))
	}
	for i, b := range atSend {
		if len(b) != 1 || !same(b[0].Call, want) || b[0].Effect.State != eventstore.EffectPending ||
			b[0].Effect.ID != modelcall.EffectOf(callID) || b[0].Effect.Kind != modelcall.Kind || b[0].Effect.Target != string(callID) {
			t.Fatalf("bound at send %d: %+v", i, b)
		}
	}
	if diffs, err := w.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("the binding is not a fold of the log: %v %+v", err, diffs)
	}
}

func same(a, b types.ModelCall) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// crashing commits the intent, then the controller dies before the send:
// the request's context ends and the store closes with nothing else
// written.
type crashing struct {
	modelcall.Recorder
	w *world
}

func (c crashing) Intend(ctx context.Context, b gateway.Binding, destination types.Slug) error {
	if err := c.Recorder.Intend(ctx, b, destination); err != nil {
		return err
	}
	c.w.close()
	return errors.New("killed")
}

// F (P12a): a crash between the intent and the send. After the restart,
// recovery reads the intent: the call stays bound to its attempt, lease and
// groups, its effect uncertain, so its charge is never lost. It is never
// sent again: a new gateway refuses the call, and the store refuses to
// retry it.
func TestACrashBetweenIntentAndSend(t *testing.T) {
	w := openWorld(t)
	w.start(rgB, rgA)
	up := newUpstream(t, nil)
	g, token := newGateway(t, w, up, crashing{modelcall.Recorder{Store: w.s, Actor: actor}, w})
	if rec := send(g, token); rec.Code == http.StatusOK || up.count() != 0 {
		t.Fatalf("the crashed send: %d, upstream saw %d", rec.Code, up.count())
	}

	w.open() // the restart, as adcd's start runs it
	if n := w.restart(); n != 1 {
		t.Fatalf("marked %d", n)
	}
	bound := w.ofLease()
	want := types.ModelCall{Call: callID, Attempt: attempt, Lease: leaseID, Executor: builder, Destination: "stub", Groups: []types.ResourceGroupID{rgA, rgB}}
	if len(bound) != 1 || !same(bound[0].Call, want) || bound[0].Effect.State != eventstore.EffectUncertain {
		t.Fatalf("recovery read %+v", bound)
	}

	g2, token2 := newGateway(t, w, up, modelcall.Recorder{Store: w.s, Actor: actor})
	if rec := send(g2, token2); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "intent_unrecorded") || up.count() != 0 {
		t.Fatalf("the call re-sent after its crash: %d %s, upstream saw %d", rec.Code, rec.Body, up.count())
	}
	if _, err := w.s.RecordEffect(context.Background(), actor, modelcall.EffectOf(callID), eventstore.Transition{Kind: "retried"}); !errors.Is(err, eventstore.ErrEffectTransition) {
		t.Fatalf("an uncertain model_call retried: %v", err)
	}
	if b := w.ofLease(); len(b) != 1 || b[0].Effect.State != eventstore.EffectUncertain {
		t.Fatalf("after the refusal: %+v", b)
	}
}

// Intend binds a running lease's attempt only, with at least one group,
// and a call once.
func TestIntendChecksTheLeaseAndBindsOnce(t *testing.T) {
	w := openWorld(t)
	intend := func(b gateway.Binding) error {
		return w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return modelcall.Intend(tx, b, "stub") })
	}
	if err := intend(binding); !errors.Is(err, modelcall.ErrNotRunning) {
		t.Fatalf("no lease: %v", err)
	}
	w.start(rgB, rgA)
	if err := intend(gateway.Binding{Lease: leaseID, Attempt: "att_01m4tm99haemss9tvzd438vq8k", Call: callID}); !errors.Is(err, modelcall.ErrNotRunning) {
		t.Fatalf("another attempt: %v", err)
	}
	w.reserve()
	if err := intend(binding); !errors.Is(err, modelcall.ErrNoGroups) {
		t.Fatalf("a lease with no reservation: %v", err)
	}
	w.reserve(rgB, rgA)
	if err := intend(gateway.Binding{Lease: leaseID, Attempt: attempt, Call: "msr_01m4tm99haemss9tvzd438vq8j"}); err == nil {
		t.Fatal("a call id of another prefix")
	}
	if err := intend(binding); err != nil {
		t.Fatal(err)
	}
	// L3: a pending call is the same binding whatever its lease reserves now.
	w.reserve(rgA)
	var evs []eventstore.Event
	if err := w.do(func(tx *eventstore.Tx) (e []eventstore.Event, err error) {
		evs, err = modelcall.Intend(tx, binding, "stub")
		return evs, err
	}); err != nil || len(evs) != 0 {
		t.Fatalf("a pending call intended again: %v, %d events", err, len(evs))
	}
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return modelcall.Intend(tx, binding, "other") }); !errors.Is(err, modelcall.ErrBound) {
		t.Fatalf("a pending call bound again to another destination: %v", err)
	}
	if _, err := w.s.RecordEffect(context.Background(), actor, modelcall.EffectOf(callID), eventstore.Transition{Kind: "observed"}); err != nil {
		t.Fatal(err)
	}
	if err := intend(binding); !errors.Is(err, modelcall.ErrBound) {
		t.Fatalf("a completed call intended again: %v", err)
	}
}

// The kind's request is the binding of the call the intent targets, with
// its groups sorted; the fold admits a binding only with its own command's
// intent of the same request, once per call.
func TestTheRequestAndTheFold(t *testing.T) {
	w := openWorld(t)
	other := types.CallID("cal_01m4tm99haemss9tvzd438vq8k")
	req := func(call types.CallID, groups ...types.ResourceGroupID) json.RawMessage {
		raw, _ := json.Marshal(types.ModelCall{Call: call, Attempt: attempt, Lease: leaseID, Executor: builder, Destination: "stub", Groups: groups})
		return raw
	}
	for name, c := range map[string]struct {
		target string
		req    json.RawMessage
	}{
		"the destination as target": {"stub", req(callID, rgA)},
		"another call's target":     {string(other), req(callID, rgA)},
		"unsorted groups":           {string(callID), req(callID, rgB, rgA)},
		"not a binding":             {string(callID), json.RawMessage(`{"call":"x"}`)},
		"duplicated groups":         {string(callID), req(callID, rgA, rgA)},
		"no group":                  {string(callID), req(callID)},
	} {
		if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return nil, tx.IntendEffect(modelcall.EffectOf(callID), modelcall.Kind, c.target, c.req)
		}); err == nil {
			t.Errorf("%s: intended", name)
		}
	}
	ev := func(call types.CallID, payload json.RawMessage) eventstore.Event {
		id, _ := ids.New("evt")
		return eventstore.Event{ID: id, Kind: modelcall.Intended, Subject: string(call), Payload: payload}
	}
	bind := func(intend, payload json.RawMessage) error {
		return w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			if intend != nil {
				if err := tx.IntendEffect(modelcall.EffectOf(callID), modelcall.Kind, string(callID), intend); err != nil {
					return nil, err
				}
			}
			return []eventstore.Event{ev(callID, payload)}, nil
		})
	}
	if err := bind(nil, req(callID, rgA)); err == nil {
		t.Fatal("an orphan binding")
	}
	if err := bind(req(callID, rgA), req(callID, rgB)); err == nil {
		t.Fatal("a binding that is not its intent's request")
	}
	if err := bind(req(callID, rgA), req(callID, rgA)); err != nil {
		t.Fatal(err)
	}
	if err := bind(nil, req(callID, rgA)); err == nil {
		t.Fatal("a call bound twice")
	}
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return nil, tx.IntendEffect(modelcall.EffectOf(other), modelcall.Kind, string(other), req(other, rgA))
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.do(func(*eventstore.Tx) ([]eventstore.Event, error) {
		return []eventstore.Event{ev(other, req(other, rgA))}, nil
	}); err == nil {
		t.Fatal("a binding by another command than its intent's")
	}
	if diffs, err := w.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("replay: %v %+v", err, diffs)
	}
}

// M2: the Recorder runs only on a store where the restart rule has run, so
// a pending call it finds was intended since this open.
func TestTheRecorderNeedsTheRestartRule(t *testing.T) {
	w := openWorld(t)
	w.start(rgB, rgA)
	w.close()
	w.open() // no restart rule
	up := newUpstream(t, nil)
	g, token := newGateway(t, w, up, modelcall.Recorder{Store: w.s, Actor: actor})
	if rec := send(g, token); rec.Code != http.StatusServiceUnavailable || up.count() != 0 || len(w.ofLease()) != 0 {
		t.Fatalf("intended before the restart rule: %d, upstream saw %d", rec.Code, up.count())
	}
	w.restart()
	if rec := send(g, token); rec.Code != http.StatusOK || up.count() != 1 {
		t.Fatalf("after the restart rule: %d %s", rec.Code, rec.Body)
	}
}

// H1: a call bound under one binding is never sent under another. The
// gateway refuses a second binding of a bound call; once the first is
// unbound, the new binding names an attempt its lease does not run, and
// is refused before egress (the Recorder's own refusal of a rebinding is
// TestIntendChecksTheLeaseAndBindsOnce's).
func TestACallIsSentUnderOneBindingOnly(t *testing.T) {
	w := openWorld(t)
	w.start(rgB, rgA)
	up := newUpstream(t, nil)
	g, token := newGateway(t, w, up, modelcall.Recorder{Store: w.s, Actor: actor})
	if rec := send(g, token); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	a2 := types.AttemptID("att_01m4tm99haemss9tvzd438vq8k")
	moved := gateway.Binding{Lease: leaseID, Attempt: a2, Call: callID}
	if _, err := g.Bind(moved); err == nil {
		t.Fatal("a bound call bound again under another attempt")
	}
	g.Unbind(token)
	token2, err := g.Bind(moved)
	if err != nil {
		t.Fatal(err)
	}
	if rec := send(g, token2); rec.Code == http.StatusOK || up.count() != 1 || len(w.ofLease()) != 1 {
		t.Fatalf("the call sent under another attempt: %d %s, upstream saw %d", rec.Code, rec.Body, up.count())
	}
}

// Astra H1: a stop leaves the lease active on a stopping attempt. Neither a
// token whose intent is cached nor a first request on a new token sends
// anything after it, and the Recorder refuses the stopped attempt.
func TestAStoppedAttemptSendsNothing(t *testing.T) {
	w := openWorld(t)
	w.start(rgB, rgA)
	up := newUpstream(t, nil)
	g, token := newGateway(t, w, up, modelcall.Recorder{Store: w.s, Actor: actor})
	if rec := send(g, token); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	w.stop()
	if l, ok, err := (modelcall.Reader{Store: w.s}).Lease(context.Background(), leaseID); err != nil || !ok || l.State != types.LeaseStateActive {
		t.Fatalf("the stop left the lease %v: %v", l.State, err)
	}
	if rec := send(g, token); rec.Code == http.StatusOK || up.count() != 1 {
		t.Fatalf("a cached token sent after the stop: %d, upstream saw %d", rec.Code, up.count())
	}
	other := gateway.Binding{Lease: leaseID, Attempt: attempt, Call: "cal_01m4tm99haemss9tvzd438vq8k"}
	fresh, err := g.Bind(other)
	if err != nil {
		t.Fatal(err)
	}
	if rec := send(g, fresh); rec.Code == http.StatusOK || up.count() != 1 || len(w.ofLease()) != 1 {
		t.Fatalf("a first request sent after the stop: %d, upstream saw %d", rec.Code, up.count())
	}
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return modelcall.Intend(tx, other, "stub") }); !errors.Is(err, modelcall.ErrNotRunning) {
		t.Fatalf("the Recorder bound a stopped attempt: %v", err)
	}
}

// Astra round 2 M1: a call is never rebound to another eligible attempt
// and lease. A second attempt, running and current under another lease and
// unit at the same destination, binds the existing call: Intend refuses
// it, the gateway sends nothing, and the first binding stays.
func TestACallIsNeverRebound(t *testing.T) {
	w := openWorld(t)
	w.start(rgB, rgA)
	const (
		lease2   = types.LeaseID("lea_01m4tm99haemss9tvzd438vq8k")
		unit2    = types.UnitID("unt_01m3250vz2ezn81cgvbkcn17jk")
		attempt2 = types.AttemptID("att_01m4tm99haemss9tvzd438vq8k")
	)
	w.startOn(lease2, unit2, attempt2, rgA)
	up := newUpstream(t, nil)
	g, token := newGateway(t, w, up, modelcall.Recorder{Store: w.s, Actor: actor})
	if rec := send(g, token); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	first := w.ofLease()
	g.Unbind(token)
	moved := gateway.Binding{Lease: lease2, Attempt: attempt2, Call: callID}
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return modelcall.Intend(tx, moved, "stub") }); !errors.Is(err, modelcall.ErrBound) {
		t.Fatalf("the call rebound to another running attempt and lease: %v", err)
	}
	token2, err := g.Bind(moved)
	if err != nil {
		t.Fatal(err)
	}
	if rec := send(g, token2); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "intent_unrecorded") || up.count() != 1 {
		t.Fatalf("the call sent under another attempt and lease: %d %s, upstream saw %d", rec.Code, rec.Body, up.count())
	}
	var after modelcall.Bound
	w.view(func(tx *eventstore.Tx) (err error) {
		after.Call, _, err = modelcall.Of(tx, callID)
		return err
	})
	if len(first) != 1 || !same(after.Call, first[0].Call) {
		t.Fatalf("the binding changed: %+v, then %+v", first, after.Call)
	}
}

// admitting signals each attempt check the gateway makes.
type admitting struct {
	gateway.Attempts
	checked chan struct{}
}

func (a admitting) Attempt(ctx context.Context, id types.AttemptID) (types.Attempt, bool, bool, error) {
	v, current, ok, err := a.Attempts.Attempt(ctx, id)
	a.checked <- struct{}{}
	return v, current, ok, err
}

// Astra round 2 L1, the residual, documented: eligibility is checked when a
// request is admitted, before its body is read. A request admitted before
// a stop, whose body arrives after it, still sends when its token's intent
// is cached; a first request so admitted is refused by the Recorder, which
// checks again in its transaction. Every request admitted before the stop
// is in this window, however many; P22a bounds them (#226 entry 57), and
// their late usage is accepted (#226 entry 59).
func TestRequestsAdmittedBeforeAStop(t *testing.T) {
	w := openWorld(t)
	w.start(rgB, rgA)
	up := newUpstream(t, nil)
	at := admitting{modelcall.Reader{Store: w.s}, make(chan struct{}, 4)}
	g, token := newGatewayWith(t, w, up, modelcall.Recorder{Store: w.s, Actor: actor}, at)
	if rec := send(g, token); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	<-at.checked
	fresh, err := g.Bind(gateway.Binding{Lease: leaseID, Attempt: attempt, Call: "cal_01m4tm99haemss9tvzd438vq8k"})
	if err != nil {
		t.Fatal(err)
	}
	type held struct {
		body *io.PipeWriter
		rec  *httptest.ResponseRecorder
		done chan struct{}
	}
	admit := func(tk string) held {
		pr, pw := io.Pipe()
		h := held{pw, httptest.NewRecorder(), make(chan struct{})}
		r := httptest.NewRequest(http.MethodPost, "/chat/completions", pr)
		r.Header.Set("Authorization", "Bearer "+tk)
		go func() { g.ServeHTTP(h.rec, r); close(h.done) }()
		<-at.checked // admitted: the attempt was running; the body is not read yet
		return h
	}
	cached, first := admit(token), admit(fresh)
	w.stop()
	for _, h := range []held{cached, first} {
		_, _ = io.WriteString(h.body, good)
		_ = h.body.Close()
		<-h.done
	}
	if cached.rec.Code != http.StatusOK || up.count() != 2 {
		t.Fatalf("the admitted request on a cached token: %d, upstream saw %d", cached.rec.Code, up.count())
	}
	if first.rec.Code != http.StatusServiceUnavailable || len(w.ofLease()) != 1 {
		t.Fatalf("the admitted first request: %d %s", first.rec.Code, first.rec.Body)
	}
	if rec := send(g, token); rec.Code == http.StatusOK || up.count() != 2 {
		t.Fatalf("a request admitted after the stop: %d, upstream saw %d", rec.Code, up.count())
	}
}

// Opus pre-review of #253, M2 and L3: a call binds only to a lease whose
// every reservation is positive, so an unmeasured call's liability always
// has its reservation to stand for it; and a lease, which runs one attempt,
// binds one call, so that reservation is the call's.
func TestIntendNeedsAPositiveReservationAndOneCall(t *testing.T) {
	w := openWorld(t)
	intend := func(b gateway.Binding) error {
		return w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return modelcall.Intend(tx, b, "stub") })
	}
	w.start(rgB, rgA)
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		l, rev, _, err := lease.LeaseOf(tx, leaseID)
		if err != nil {
			return nil, err
		}
		l.Reservations = slices.Clone(l.Reservations)
		l.Reservations[1].Amount = 0
		raw, err := json.Marshal(map[string]any{"revision": rev, "record": l})
		if err != nil {
			return nil, err
		}
		return nil, tx.Put(lease.LeaseProjection, string(leaseID), raw)
	})
	if err := intend(binding); !errors.Is(err, modelcall.ErrNoPositiveReservation) {
		t.Fatalf("a zero reservation: %v", err)
	}
	w.reserve(rgB, rgA)
	if err := intend(binding); err != nil {
		t.Fatal(err)
	}
	if err := intend(gateway.Binding{Lease: leaseID, Attempt: attempt, Call: "cal_01m4tm99haemss9tvzd438vq8k"}); !errors.Is(err, modelcall.ErrSecondCall) {
		t.Fatalf("a second call on the lease: %v", err)
	}
	if err := intend(binding); err != nil {
		t.Fatalf("the bound call, intended again while pending: %v", err)
	}
}
