// SPDX-License-Identifier: Apache-2.0

package modelcall_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
)

const (
	config  = "../../fixtures/m1/config/"
	actor   = "act_01m3250v0aer9ss2097svmefj8"
	builder = types.ExecutorID("exe_01m4tm99haemqt8es2qfbwj8pa") // registry-stub.json: destination stub
	leaseID = types.LeaseID("lea_01m4tm99haemss9tvzd438vq8j")
	attempt = types.AttemptID("att_01m4tm99haemss9tvzd438vq8j")
	callID  = types.CallID("cal_01m4tm99haemss9tvzd438vq8j")
	rgB     = types.ResourceGroupID("rg_01m4tm99haemtbemfkdve21grg")
	rgA     = types.ResourceGroupID("rg_01m4tm99haemss9tvzd438vq8j")
	good    = `{"model":"stub-builder-1","messages":[{"role":"user","content":"hi"}]}`
)

var binding = gateway.Binding{Lease: leaseID, Attempt: attempt, Call: callID}

// world is a controller's state directory, opened and closed like adcd's.
type world struct {
	t    *testing.T
	dir  string
	lock *eventstore.Lock
	s    *eventstore.Store
}

func openWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, dir: filepath.Join(t.TempDir(), "state")}
	w.open()
	t.Cleanup(w.close)
	return w
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
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: modelcall.Kinds(), Blobs: blobs, Effects: modelcall.Effects(),
		Hooks: eventstore.Hooks{Identify: ok, Authorize: ok, Project: modelcall.Project}})
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

// setLease puts l in the lease projection, as internal/lease's projector
// keeps it (its folds are its own tests').
func (w *world) setLease(state types.LeaseState, running types.AttemptID) {
	w.t.Helper()
	l := map[string]any{"id": leaseID, "unit": "unt_01m3250vz2ezn81cgvbkcn17jd", "role": "builder", "executor": builder, "state": state,
		"reservations": []any{map[string]any{"group": rgB, "amount": 1000, "unit": "tokens"}, map[string]any{"group": rgA, "amount": 1, "unit": "requests"}},
		"priority":     0, "footprint": map[string]any{"context_tokens": 1, "expected_turns": 1, "gpu_seconds": nil}, "deadline": "2026-10-14T00:00:00Z",
		"fallback_step": 0, "placement_checks": map[string]any{}, "hold": nil, "attempt": running, "restore_receipt": nil, "settlement": nil}
	raw, err := json.Marshal(map[string]any{"revision": 1, "record": l})
	if err != nil {
		w.t.Fatal(err)
	}
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return nil, tx.Put(lease.LeaseProjection, string(leaseID), raw)
	}); err != nil {
		w.t.Fatal(err)
	}
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

// Lease is the gateway's lease reader over the store.
func (w *world) Lease(ctx context.Context, id types.LeaseID) (l types.Lease, ok bool, err error) {
	err = w.s.View(ctx, func(tx *eventstore.Tx) error {
		l, _, ok, err = lease.LeaseOf(tx, id)
		return err
	})
	return l, ok, err
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
	g, err := gateway.New(gateway.Config{Registry: r, Leases: w, Secrets: secrets{}, Intents: in})
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
	w.setLease(types.LeaseStateActive, attempt)
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
			b[0].Effect.ID != modelcall.EffectOf(callID) || b[0].Effect.Kind != modelcall.Kind || b[0].Effect.Target != "stub" {
			t.Fatalf("bound at send %d: %+v", i, b)
		}
	}
	if diffs, err := w.s.RebuildProjections(context.Background()); err != nil {
		t.Fatal(err)
	} else {
		for _, d := range diffs {
			if d.Projection != lease.LeaseProjection { // the test's own seed
				t.Fatalf("the binding is not a fold of the log: %+v", d)
			}
		}
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
	w.setLease(types.LeaseStateActive, attempt)
	up := newUpstream(t, nil)
	g, token := newGateway(t, w, up, crashing{modelcall.Recorder{Store: w.s, Actor: actor}, w})
	if rec := send(g, token); rec.Code == http.StatusOK || up.count() != 0 {
		t.Fatalf("the crashed send: %d, upstream saw %d", rec.Code, up.count())
	}

	w.open() // the restart, as adcd's start runs it
	if n, err := w.s.MarkPendingUncertain(context.Background(), actor); err != nil || n != 1 {
		t.Fatalf("marked %d: %v", n, err)
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

// Intend binds a running lease's attempt only, and a call once.
func TestIntendChecksTheLeaseAndBindsOnce(t *testing.T) {
	w := openWorld(t)
	intend := func(b gateway.Binding) error {
		return w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return modelcall.Intend(tx, b, "stub") })
	}
	if err := intend(binding); !errors.Is(err, modelcall.ErrNotRunning) {
		t.Fatalf("no lease: %v", err)
	}
	w.setLease(types.LeaseStateSettling, attempt)
	if err := intend(binding); !errors.Is(err, modelcall.ErrNotRunning) {
		t.Fatalf("a settling lease: %v", err)
	}
	w.setLease(types.LeaseStateActive, "att_01m4tm99haemss9tvzd438vq8k")
	if err := intend(binding); !errors.Is(err, modelcall.ErrNotRunning) {
		t.Fatalf("another attempt: %v", err)
	}
	w.setLease(types.LeaseStateActive, attempt)
	if err := intend(gateway.Binding{Lease: leaseID, Attempt: attempt, Call: "msr_01m4tm99haemss9tvzd438vq8j"}); err == nil {
		t.Fatal("a call id of another prefix")
	}
	if err := intend(binding); err != nil {
		t.Fatal(err)
	}
	var evs []eventstore.Event
	if err := w.do(func(tx *eventstore.Tx) (e []eventstore.Event, err error) {
		evs, err = modelcall.Intend(tx, binding, "stub")
		return evs, err
	}); err != nil || len(evs) != 0 {
		t.Fatalf("a pending call intended again: %v, %d events", err, len(evs))
	}
	if err := intend(gateway.Binding{Lease: leaseID, Attempt: attempt, Call: callID}); err != nil {
		t.Fatal(err)
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

// The kind's request is the binding, naming the intent's target, with its
// groups sorted; a second binding event for one call is refused on fold.
func TestTheRequestAndTheFold(t *testing.T) {
	w := openWorld(t)
	req := func(dest string, groups ...types.ResourceGroupID) json.RawMessage {
		raw, _ := json.Marshal(types.ModelCall{Call: callID, Attempt: attempt, Lease: leaseID, Executor: builder, Destination: dest, Groups: groups})
		return raw
	}
	for name, c := range map[string]struct {
		target string
		req    json.RawMessage
	}{
		"another target":    {"other", req("stub", rgA)},
		"unsorted groups":   {"stub", req("stub", rgB, rgA)},
		"not a binding":     {"stub", json.RawMessage(`{"call":"x"}`)},
		"duplicated groups": {"stub", req("stub", rgA, rgA)},
	} {
		if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return nil, tx.IntendEffect(modelcall.EffectOf(callID), modelcall.Kind, c.target, c.req)
		}); err == nil {
			t.Errorf("%s: intended", name)
		}
	}
	ev := func() eventstore.Event {
		id, _ := ids.New("evt")
		return eventstore.Event{ID: id, Kind: modelcall.Intended, Subject: string(callID), Payload: req("stub", rgA)}
	}
	if err := w.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return []eventstore.Event{ev()}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := w.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return []eventstore.Event{ev()}, nil }); err == nil {
		t.Fatal("a call bound twice")
	}
	other := ev()
	other.Subject = "cal_01m4tm99haemss9tvzd438vq8k"
	if err := w.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return []eventstore.Event{other}, nil }); err == nil {
		t.Fatal("a binding under another call's subject")
	}
}
