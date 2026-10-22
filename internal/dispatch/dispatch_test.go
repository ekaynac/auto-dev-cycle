// SPDX-License-Identifier: Apache-2.0

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	config   = "../../fixtures/m1/config/"
	project  = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sf9")
	other    = types.ProjectID("prj_01m3250vm7f6ktw39dszqnyddr")
	task     = types.TaskID("tsk_01m3250v9xft08ssr4ygq6g2np")
	builder  = types.ExecutorID("exe_01m4tm99haemqt8es2qfbwj8pa") // registry-stub.json
	reviewer = types.ExecutorID("exe_01m4tm99haemr8ajyf09v4ytzb")
	quota    = types.ResourceGroupID("rg_01m4tm99haemss9tvzd438vq8j") // theirs, 2,000,000 tokens
	actor    = types.ActorID("act_01m3250v0aer9ss2097svmefj8")
	image    = "registry.example/adc/py-mini@sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// stub is the stub registry over the fixture groups, edited by edit.
func stub(t *testing.T, edit func(*types.ExecutorRegistry)) (registry.Registry, []types.ResourceGroup) {
	t.Helper()
	doc, err := registry.Load(config + "registry-stub.json")
	if err != nil {
		t.Fatal(err)
	}
	var gs []types.ResourceGroup
	raw, err := os.ReadFile(config + "groups.json")
	if err == nil {
		err = json.Unmarshal(raw, &gs)
	}
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(&doc)
	}
	reg, err := registry.New(doc, gs)
	if err != nil {
		t.Fatal(err)
	}
	return reg, gs
}

type env struct {
	t     *testing.T
	s     *eventstore.Store
	blobs *blobstore.Store
}

// unrailed are the count rails' defaults and USD rails no test reaches:
// the rails a test that is not about them passes (P17b: no rails, or no
// USD rail, fail closed for a priced executor).
var unrailed graph.Rails = func(*eventstore.Tx, types.TaskID) (graph.Caps, error) {
	c := graph.DefaultCaps
	c.PerTaskMicros, c.PerDayMicros = 1<<50, 1<<50
	return c, nil
}

// clock is the test stores' clock: now, unless a test moves it.
var clock = time.Now

// open is a store with the lease fold over capacity, the usage ledger and
// the model calls' bindings, and the checked graph fold (graph.Projector
// over authority.Answers), as adcd composes them.
func open(t *testing.T, groups []types.ResourceGroup) env {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	lock := must(eventstore.AcquireLock(state))
	blobs := must(blobstore.Open(state))
	r := must(capacity.NewReservations(groups))
	kinds := graph.Kinds()
	for _, ks := range []eventstore.Kinds{lease.Kinds(), ledger.Kinds(), modelcall.Kinds()} {
		maps.Copy(kinds, ks)
	}
	none := func(*eventstore.Tx, eventstore.Command) error { return nil }
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: kinds, Blobs: blobs, Effects: modelcall.Effects(),
		Now: func() time.Time { return clock() },
		Hooks: eventstore.Hooks{Identify: none, Authorize: none, Project: eventstore.ProjectEach(
			capacity.Hook(lease.Projector{Blobs: blobs, Units: graph.Units{}}, r), ledger.Project, modelcall.Project,
			graph.Projector{Answers: authority.Answers{}}.Project)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); lock.Release() })
	return env{t: t, s: s, blobs: blobs}
}

func (e env) do(fn func(*eventstore.Tx) ([]eventstore.Event, error)) error {
	cmd := must(ids.New("cmd"))
	_, err := e.s.SubmitApplied(context.Background(), eventstore.Command{ID: cmd, Actor: string(actor), Action: "test.command", IdempotencyKey: cmd, Input: []byte(`{}`)},
		eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := fn(tx)
			return evs, json.RawMessage(`{}`), err
		}))
	return err
}

func (e env) must(fn func(*eventstore.Tx) ([]eventstore.Event, error)) {
	e.t.Helper()
	if err := e.do(fn); err != nil {
		e.t.Fatal(err)
	}
}

func (e env) view(fn func(*eventstore.Tx) error) {
	e.t.Helper()
	if err := e.s.View(context.Background(), fn); err != nil {
		e.t.Fatal(err)
	}
}

// node records a node of kind, a plan or, rostered by reviewer, a
// review_requirement, and claims it unless unclaimed.
func (e env) node(kind types.NodeKind, reg registry.Registry, unclaimed bool) types.NodeID {
	e.t.Helper()
	r := types.NodeRecorded{Type: "node_recorded", Task: task, Project: project, Node: types.Node{ID: types.NodeID(must(ids.New("nod"))),
		Kind: kind, DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{}`), Inputs: []types.InputSpec{},
		Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}}
	if kind == types.NodeKindReviewRequirement {
		roster := must(graph.Roster([]types.ExecutorID{reviewer}, reg))
		r.Node.Params = must(json.Marshal(types.ReviewRequirement{Subject: "candidate", Roster: roster, DefinitionVersion: "1", Revision: 1}))
		e.must(func(*eventstore.Tx) ([]eventstore.Event, error) { return graph.RecordReview(r, reg) })
		if !unclaimed {
			e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return graph.ClaimReview(tx, r.Node.ID, map[string]graph.Resolved{}, 1, reg)
			})
		}
		return r.Node.ID
	}
	e.must(func(*eventstore.Tx) ([]eventstore.Event, error) { return graph.Record(r) })
	if !unclaimed {
		e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Claim(tx, r.Node.ID, map[string]graph.Resolved{}, 1)
		})
	}
	return r.Node.ID
}

func card(n types.NodeID) Card {
	return Card{Node: n, Project: project, Image: image, Deadline: "2026-10-04T14:00:00Z",
		Policy: types.ProjectProfileDataPolicy{Floor: "contracted", Routing: "verified", AllowedDestinations: []types.Slug{"stub"}}}
}

func (e env) match(d Dispatcher, c Card, exe types.ExecutorID) (types.LeaseID, error) {
	var id types.LeaseID
	err := e.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, l, err := d.Match(tx, c, exe)
		id = l
		return evs, err
	})
	return id, err
}

// launch launches lease id (granted: revision 2) with what d observes.
func (e env) launch(d Dispatcher, id types.LeaseID, c Card, exe types.ExecutorID) (types.AttemptID, *Miss) {
	e.t.Helper()
	return e.launchWith(d, id, c, d.Observe(context.Background(), c, exe))
}

// launchWith launches lease id with the readiness ready.
func (e env) launchWith(d Dispatcher, id types.LeaseID, c Card, ready Readiness) (types.AttemptID, *Miss) {
	e.t.Helper()
	a, miss, err := e.tryLaunch(d, id, c, ready)
	if err != nil {
		e.t.Fatal(err)
	}
	return a, miss
}

func (e env) tryLaunch(d Dispatcher, id types.LeaseID, c Card, ready Readiness) (types.AttemptID, *Miss, error) {
	a := types.AttemptID(must(ids.New("att")))
	var miss *Miss
	err := e.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, m, err := d.Launch(tx, id, 2, c, a, types.AttemptBase{Tree: "git:4b825dc642cb6eb9a060e54bf8d69288fbee4904", ContractRevision: "1"}, ready)
		miss = m
		return evs, err
	})
	return a, miss, err
}

type images struct{ err error }

func (i images) Resolve(_ context.Context, ref string) (sandbox.Image, error) {
	return sandbox.Image{Pinned: ref}, i.err
}

type creds bool

func (c creds) Holds(context.Context, types.Slug, types.Slug) (bool, error) { return bool(c), nil }

// state is lease id's state, the quota group's outstanding reservation and
// node's current attempt.
func (e env) state(id types.LeaseID, node types.NodeID) (types.Lease, int64, *types.Attempt) {
	e.t.Helper()
	var l types.Lease
	var out int64
	var cur *types.Attempt
	e.view(func(tx *eventstore.Tx) error {
		var err error
		if l, _, _, err = lease.LeaseOf(tx, id); err != nil {
			return err
		}
		if out, err = capacity.Outstanding(tx, quota); err != nil {
			return err
		}
		a, ok, err := lease.CurrentAttempt(tx, graph.UnitOf(node))
		if err == nil && ok {
			at, _, _, aerr := lease.AttemptOf(tx, a)
			cur, err = &at, aerr
		}
		return err
	})
	return l, out, cur
}

// A match records the lease with the placement it decided on, reserving
// its group; a ready launch starts the node's first attempt at token 1,
// then, under a second lease, its successor at token 2 (fencing).
func TestMatchAndLaunch(t *testing.T) {
	reg, gs := stub(t, nil)
	e := open(t, gs)
	d := Dispatcher{Rails: unrailed, Registry: reg, Images: images{}, Credentials: creds(true)}
	n := e.node(types.NodeKindPlan, reg, false)
	id, err := e.match(d, card(n), builder)
	if err != nil {
		t.Fatal(err)
	}
	l, out, cur := e.state(id, n)
	entry, _ := reg.Executor(builder)
	if l.State != types.LeaseStateGranted || l.Role != "builder" || out != 1000 || cur != nil ||
		l.PlacementChecks.Destinations.Detail != "executor "+string(builder)+" "+digest(entry) ||
		l.PlacementChecks.Capability.Detail != "image "+image {
		t.Fatalf("matched lease %+v, outstanding %d, attempt %v", l, out, cur)
	}
	first, miss := e.launch(d, id, card(n), builder)
	if l, _, cur = e.state(id, n); miss != nil || l.State != types.LeaseStateActive || cur == nil || cur.ID != first || cur.FencingToken != 1 {
		t.Fatalf("launch: miss %v, lease %s, attempt %+v", miss, l.State, cur)
	}
	e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.LoseRunning(tx, first, types.AttemptLostReasonValueAgentDisconnect)
	})
	e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.SealMinimal(tx, first, 2, graph.Units{}, e.blobs, nil, actor)
	})
	// While the first lease is live, the node is not matched again.
	if _, err := e.match(d, card(n), builder); err == nil || !strings.Contains(err.Error(), "already has live lease") {
		t.Fatalf("a second match on a live lease: %v", err)
	}
	e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, id, 3, lease.LeaseEvent{Kind: lease.Revoked, Reason: "test"}, nil)
	})
	// F (P21, X3 step 4 before step 5): while the first lease settles, its
	// reservation not yet returned or moved to its liability, the node is not
	// placed again.
	if _, err := e.match(d, card(n), builder); err == nil || !strings.Contains(err.Error(), "has not settled") {
		t.Fatalf("a match while the last lease settles: %v", err)
	}
	e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, id, 4, lease.LeaseEvent{Kind: lease.Settled,
			Settlement: &types.LeaseSettlementValue{Calls: []types.CallID{}, Liabilities: []types.CallID{}}}, nil)
	})
	second, err := e.match(d, card(n), builder)
	if err != nil {
		t.Fatal(err)
	}
	next, miss := e.launch(d, second, card(n), builder)
	if _, _, cur = e.state(second, n); miss != nil || cur == nil || cur.ID != next || cur.FencingToken != 2 || cur.Supersedes == nil || *cur.Supersedes != first {
		t.Fatalf("successor: miss %v, attempt %+v", miss, cur)
	}
}

// Every placement refusal comes before any reservation (#226 entries 39
// and 47): no lease is recorded and nothing is reserved.
func TestMatchRefusesBeforeReserving(t *testing.T) {
	reg, gs := stub(t, nil)
	e := open(t, gs)
	d := Dispatcher{Rails: unrailed, Registry: reg}
	plan, req := e.node(types.NodeKindPlan, reg, false), e.node(types.NodeKindReviewRequirement, reg, false)
	unclaimed := e.node(types.NodeKindPlan, reg, true)
	for name, c := range map[string]struct {
		card  Card
		exe   types.ExecutorID
		check string // the blocking check; "" for an error before placement
		held  string // the source_invalid_config hold's signal (Opus 22a-5c-1 M1)
	}{
		"a destination the project does not allow": {edit(card(plan), func(c *Card) { c.Policy.AllowedDestinations = []types.Slug{"other"} }), builder, "data_floor", PlacementSignal("profile " + string(project))},
		"a floor the destination is below":         {edit(card(plan), func(c *Card) { c.Policy.Floor = "own" }), builder, "data_floor", PlacementSignal("profile " + string(project))},
		"an unregistered executor":                 {card(plan), "exe_01m4tm99haemrs8zjs56wjm467", "data_floor", PlacementSignal("executor exe_01m4tm99haemrs8zjs56wjm467")},
		"a reviewer on a builder's node":           {card(plan), reviewer, "independence", PlacementSignal("executor " + string(reviewer))},
		"a builder on a rostered node":             {card(req), builder, "independence", PlacementSignal("executor " + string(builder))},
		"no pinned image":                          {edit(card(plan), func(c *Card) { c.Image = "" }), builder, "capability", PlacementSignal("profile " + string(project))},
		"another project's card":                   {edit(card(plan), func(c *Card) { c.Project = other }), builder, "", ""},
		"an unclaimed node":                        {card(unclaimed), builder, "", ""},
	} {
		_, err := e.match(d, c.card, c.exe)
		var r *Refused
		if errors.As(err, &r) != (c.check != "") || (r != nil && r.Check != c.check) || err == nil {
			t.Errorf("%s: %v, want refused by %q", name, err, c.check)
		}
		if r != nil && (r.Hold == nil || r.Hold.Hold != "source_invalid_config" || r.Hold.SignalInstance != c.held || r.Hold.Owner != types.OwnerOperator) {
			t.Errorf("%s: the hold %+v, want source_invalid_config on %q", name, r.Hold, c.held)
		}
	}
	// #226 entry 103: a direct API with no footprint_prior is refused
	// precondition_failed, its node held source_invalid_config naming the
	// executor; there is no default number.
	unsized, _ := stub(t, func(r *types.ExecutorRegistry) { r.Executors[0].FootprintPrior = nil })
	var r *Refused
	if _, err := e.match(Dispatcher{Rails: unrailed, Registry: unsized}, card(plan), builder); !errors.As(err, &r) || r.Check != "footprint_prior" ||
		r.Reason != vocab.ReasonPreconditionFailed || r.Hold == nil || r.Hold.Hold != "source_invalid_config" || r.Hold.SignalInstance != PlacementSignal("executor "+string(builder)) {
		t.Errorf("no footprint_prior: %v (hold %+v)", err, r)
	}
	// An attempt_cap past its group's limit is configuration (Opus 22a-5c-1
	// M1): refused precondition_failed, held source_invalid_config naming the
	// executor, nothing requested.
	big, _ := stub(t, func(r *types.ExecutorRegistry) { r.Executors[0].FootprintPrior.ContextTokens = 1_500_000 })
	if _, err := e.match(Dispatcher{Rails: unrailed, Registry: big}, card(plan), builder); !errors.As(err, &r) || r.Check != "capacity" ||
		r.Reason != vocab.ReasonPreconditionFailed || r.Hold == nil || r.Hold.SignalInstance != PlacementSignal("executor "+string(builder)) {
		t.Errorf("an attempt_cap over the group's limit: %v", err)
	}
	e.view(func(tx *eventstore.Tx) error {
		keys, err := tx.Keys(lease.LeaseProjection, "")
		out, oerr := capacity.Outstanding(tx, quota)
		if len(keys) != 0 || out != 0 {
			t.Errorf("refused matches left leases %v, %d reserved", keys, out)
		}
		return errors.Join(err, oerr)
	})
}

func edit(c Card, fn func(*Card)) Card { fn(&c); return c }

// missed asserts a refused launch: the lease released with its
// reservation returned, no attempt started (no rail spent), and the node
// held on hold naming signal (none when hold is "").
func (e env) missed(id types.LeaseID, n types.NodeID, miss *Miss, hold, signal string) {
	e.t.Helper()
	l, out, cur := e.state(id, n)
	if miss == nil || l.State != types.LeaseStateReleased || out != 0 || cur != nil {
		e.t.Fatalf("miss %v: lease %s, outstanding %d, attempt %v", miss, l.State, out, cur)
	}
	if (miss.Hold == nil) != (hold == "") || (miss.Hold != nil && (miss.Hold.Hold != hold || miss.Hold.SignalInstance != signal)) {
		e.t.Fatalf("miss %+v holds %+v, want %s on %q", miss, miss.Hold, hold, signal)
	}
	e.view(func(tx *eventstore.Tx) error {
		evs, err := tx.EventsAfter(0, 1000)
		var kinds []string
		for _, ev := range evs {
			if ev.Kind == lease.AttemptStarted+"/v1" && ev.Payload != nil && strings.Contains(string(ev.Payload), string(l.ID)) {
				e.t.Errorf("an attempt started under the refused lease")
			}
			if ev.Subject == string(id) || (ev.Subject == string(n) && ev.Kind == graph.HoldRaised) {
				kinds = append(kinds, ev.Kind)
			}
		}
		want := "lease.requested/v1 lease.granted/v1 lease.start_refused/v1 lease.released/v1"
		if hold != "" {
			want += " " + graph.HoldRaised
		}
		if got := strings.Join(kinds, " "); got != want {
			e.t.Errorf("events %s, want %s", got, want)
		}
		return err
	})
}

// F: a missing credential consuming a rail. The launch is refused, the
// lease released, the node held on source_invalid_config naming the
// destination, and no attempt starts; also with no gateway at all (P11's
// seam unimplemented). A missing image holds on wrong_owner naming it.
func TestLaunchReadiness(t *testing.T) {
	reg, gs := stub(t, nil)
	for name, c := range map[string]struct {
		d            Dispatcher
		hold, signal string
	}{
		"the gateway holds no credential": {Dispatcher{Rails: unrailed, Registry: reg, Images: images{}, Credentials: creds(false)}, "source_invalid_config", PlacementSignal("destination stub")},
		"no gateway (P11 unbuilt)":        {Dispatcher{Rails: unrailed, Registry: reg, Images: images{}}, "source_invalid_config", PlacementSignal("destination stub")},
		"the image is absent":             {Dispatcher{Rails: unrailed, Registry: reg, Images: images{sandbox.ErrImage}, Credentials: creds(true)}, "wrong_owner", "image " + image},
		"no image store":                  {Dispatcher{Rails: unrailed, Registry: reg, Credentials: creds(true)}, "wrong_owner", "image " + image},
	} {
		t.Run(name, func(t *testing.T) {
			e := open(t, gs)
			n := e.node(types.NodeKindPlan, reg, false)
			id, err := e.match(c.d, card(n), builder)
			if err != nil {
				t.Fatal(err)
			}
			_, miss := e.launch(c.d, id, card(n), builder)
			e.missed(id, n, miss, c.hold, c.signal)
		})
	}
}

// F: a registry change between claim and start (13:106). The lease was
// placed under one registry; adcd restarts with another before the
// launch, which re-derives the placement and refuses it.
func TestLaunchRechecksRegistry(t *testing.T) {
	reg, gs := stub(t, nil)
	moved, _ := stub(t, func(doc *types.ExecutorRegistry) {
		for i := range doc.Executors {
			doc.Executors[i].Destination.Endpoint = "http://127.0.0.1:18081/v1"
		}
	})
	renamed, _ := stub(t, func(doc *types.ExecutorRegistry) { doc.Executors[1].Family = "stub-reviewer-2" })
	for name, c := range map[string]struct {
		kind  types.NodeKind
		exe   types.ExecutorID
		after Dispatcher
	}{
		"the destination moved":          {types.NodeKindPlan, builder, Dispatcher{Rails: unrailed, Registry: moved, Images: images{}, Credentials: creds(true)}},
		"the roster member's family":     {types.NodeKindReviewRequirement, reviewer, Dispatcher{Rails: unrailed, Registry: renamed, Images: images{}, Credentials: creds(true)}},
		"a builder's family, not placed": {types.NodeKindPlan, builder, Dispatcher{Rails: unrailed, Registry: renamed, Images: images{}, Credentials: creds(true)}},
	} {
		t.Run(name, func(t *testing.T) {
			e := open(t, gs)
			n := e.node(c.kind, reg, false)
			id, err := e.match(Dispatcher{Rails: unrailed, Registry: reg}, card(n), c.exe)
			if err != nil {
				t.Fatal(err)
			}
			_, miss := e.launch(c.after, id, card(n), c.exe)
			if name == "a builder's family, not placed" { // the builder's entry is unchanged: it launches
				if l, _, _ := e.state(id, n); miss != nil || l.State != types.LeaseStateActive {
					t.Fatalf("an unrelated registry change refused the launch: %v", miss)
				}
				return
			}
			if c.kind == types.NodeKindReviewRequirement { // as graph.Evaluate holds it
				e.missed(id, n, miss, "reviewer_unavailable", "registry:"+string(c.exe))
				return
			}
			e.missed(id, n, miss, "source_invalid_config", PlacementSignal("executor "+string(c.exe)))
		})
	}
	// A node held between the grant and the launch is no longer claimed:
	// the launch is refused and the lease released, with no second hold.
	e := open(t, gs)
	d := Dispatcher{Rails: unrailed, Registry: reg, Images: images{}, Credentials: creds(true)}
	n := e.node(types.NodeKindPlan, reg, false)
	id, err := e.match(d, card(n), builder)
	if err != nil {
		t.Fatal(err)
	}
	ref := "fix"
	e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Raise(tx, n, types.NodeHold{ID: types.HoldID(must(ids.New("hld"))), Hold: "wrong_owner", SignalInstance: "fix@1",
			Owner: types.OwnerNamedOwner, OwnerRef: &ref, Pages: []types.NodeHoldPagesItem{}}, nil)
	})
	_, miss := e.launch(d, id, card(n), builder)
	if l, out, cur := e.state(id, n); miss == nil || miss.Hold != nil || l.State != types.LeaseStateReleased || out != 0 || cur != nil {
		t.Fatalf("held node: miss %+v, lease %s, outstanding %d, attempt %v", miss, l.State, out, cur)
	}
}

// The launch fails closed (pre-review H1): a readiness never observed, or
// observed for another executor, launches nothing, nor does a dispatcher
// with no image store, or no gateway for a credentialed destination, even
// with a good observation in hand.
func TestLaunchFailsClosed(t *testing.T) {
	reg, gs := stub(t, nil)
	full := Dispatcher{Rails: unrailed, Registry: reg, Images: images{}, Credentials: creds(true)}
	ctx := context.Background()
	// Observations made under another registry, for the same executor and
	// image, differing in one binding each; the launch runs under reg, the
	// registry the lease was matched under, so its placement still holds
	// and only the binding check can refuse (Astra, #246 finding 3).
	dest, _ := stub(t, func(doc *types.ExecutorRegistry) {
		for i := range doc.Executors {
			doc.Executors[i].Destination.ID = "stub2"
		}
	})
	cred, _ := stub(t, func(doc *types.ExecutorRegistry) { *doc.Executors[0].CredentialRef = "other" })
	for name, c := range map[string]struct {
		d            Dispatcher
		ready        func(Card) Readiness
		hold, signal string
	}{
		"no dispatcher, no readiness":      {Dispatcher{}, func(Card) Readiness { return Readiness{} }, "source_invalid_config", PlacementSignal("executor " + string(builder))},
		"no readiness":                     {full, func(Card) Readiness { return Readiness{} }, "", ""},
		"a readiness for another executor": {full, func(c Card) Readiness { return full.Observe(ctx, c, reviewer) }, "", ""},
		"a readiness for another image":    {full, func(c Card) Readiness { c.Image += "x"; return full.Observe(ctx, c, builder) }, "", ""},
		"a readiness for another destination": {full, func(c Card) Readiness {
			return Dispatcher{Rails: unrailed, Registry: dest, Images: images{}, Credentials: creds(true)}.Observe(ctx, c, builder)
		}, "", ""},
		"a readiness for another credential": {full, func(c Card) Readiness {
			return Dispatcher{Rails: unrailed, Registry: cred, Images: images{}, Credentials: creds(true)}.Observe(ctx, c, builder)
		}, "", ""},
		"no image store, a good readiness": {Dispatcher{Rails: unrailed, Registry: reg, Credentials: creds(true)}, func(c Card) Readiness { return full.Observe(ctx, c, builder) }, "wrong_owner", "image " + image},
		"no gateway, a good readiness":     {Dispatcher{Rails: unrailed, Registry: reg, Images: images{}}, func(c Card) Readiness { return full.Observe(ctx, c, builder) }, "source_invalid_config", PlacementSignal("destination stub")},
	} {
		t.Run(name, func(t *testing.T) {
			e := open(t, gs)
			n := e.node(types.NodeKindPlan, reg, false)
			id, err := e.match(full, card(n), builder)
			if err != nil {
				t.Fatal(err)
			}
			_, miss := e.launchWith(c.d, id, card(n), c.ready(card(n)))
			e.missed(id, n, miss, c.hold, c.signal)
		})
	}
}

// Two matches of one node (pre-review M1): the second is refused and
// reserves nothing.
func TestMatchOncePerUnit(t *testing.T) {
	reg, gs := stub(t, nil)
	e := open(t, gs)
	d := Dispatcher{Rails: unrailed, Registry: reg}
	n := e.node(types.NodeKindPlan, reg, false)
	id, err := e.match(d, card(n), builder)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.match(d, card(n), builder); err == nil {
		t.Fatal("a second match of a placed node")
	}
	e.view(func(tx *eventstore.Tx) error {
		keys, err := tx.Keys(lease.LeaseProjection, "")
		out, oerr := capacity.Outstanding(tx, quota)
		if len(keys) != 1 || keys[0] != string(id) || out != 1000 {
			t.Errorf("leases %v, %d reserved; want only %s's 1000", keys, out, id)
		}
		return errors.Join(err, oerr)
	})
}

// Only a refusal of the node is a miss (pre-review M3): a node record the
// store cannot decode is an error, and the lease stays granted. A node of
// another project than the card's is refused and released (L4).
func TestLaunchErrorsAndProjects(t *testing.T) {
	reg, gs := stub(t, nil)
	d := Dispatcher{Rails: unrailed, Registry: reg, Images: images{}, Credentials: creds(true)}
	e := open(t, gs)
	n := e.node(types.NodeKindPlan, reg, false)
	id, err := e.match(d, card(n), builder)
	if err != nil {
		t.Fatal(err)
	}
	e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return nil, tx.Put(graph.NodeProjection, string(n), json.RawMessage(`{"recorded":5}`))
	})
	if _, miss, err := e.tryLaunch(d, id, card(n), d.Observe(context.Background(), card(n), builder)); err == nil || miss != nil {
		t.Fatalf("an undecodable node: miss %v, error %v", miss, err)
	}
	if l, out, _ := e.state(id, n); l.State != types.LeaseStateGranted || out != 1000 {
		t.Fatalf("after a store error: lease %s, outstanding %d", l.State, out)
	}

	e = open(t, gs)
	n = e.node(types.NodeKindPlan, reg, false)
	if id, err = e.match(d, card(n), builder); err != nil {
		t.Fatal(err)
	}
	moved := edit(card(n), func(c *Card) { c.Project = other })
	_, miss := e.launchWith(d, id, moved, d.Observe(context.Background(), moved, builder))
	e.missed(id, n, miss, "", "")
}

// A refusal's reason fits lease-event's 1024 bytes (pre-review L1).
// credSet holds exactly the named credentials.
type credSet map[types.Slug]bool

func (c credSet) Holds(_ context.Context, _, credential types.Slug) (bool, error) {
	return c[credential], nil
}

// Astra's sequence (#246 finding 1): observe credential A, change the
// registry document's credential to B, build a registry from it, match
// under it, and launch with the old observation. The observation still
// names A, which the gateway checked, so the launch is refused and the
// lease released, with no attempt.
func TestReadinessSnapshotsCredential(t *testing.T) {
	doc, err := registry.Load(config + "registry-stub.json")
	if err != nil {
		t.Fatal(err)
	}
	_, gs := stub(t, nil)
	regA, err := registry.New(doc, gs)
	if err != nil {
		t.Fatal(err)
	}
	gateway := credSet{"stub": true} // A only
	e := open(t, gs)
	n := e.node(types.NodeKindPlan, regA, false)
	observed := Dispatcher{Rails: unrailed, Registry: regA, Images: images{}, Credentials: gateway}.Observe(context.Background(), card(n), builder)

	*doc.Executors[0].CredentialRef = "other" // B, through the document's pointer
	regB, err := registry.New(doc, gs)
	if err != nil {
		t.Fatal(err)
	}
	dB := Dispatcher{Rails: unrailed, Registry: regB, Images: images{}, Credentials: gateway}
	id, err := e.match(dB, card(n), builder)
	if err != nil {
		t.Fatal(err)
	}
	_, miss := e.launchWith(dB, id, card(n), observed)
	e.missed(id, n, miss, "", "")
}

// A missing image whose reference is longer than a hold's signal holds
// (#246 finding 2): the signal is the reference's digest, so the hold is
// raised and the refusal, release and hold all commit.
func TestLaunchMissingImagePastSignal(t *testing.T) {
	reg, gs := stub(t, nil)
	d := Dispatcher{Rails: unrailed, Registry: reg, Images: images{sandbox.ErrImage}, Credentials: creds(true)}
	// Past the signal's 1024 characters; and past the placement detail's
	// 4096, which only the capability check's bounded detail lets match
	// (Astra, #246 round 2).
	for _, size := range []int{1100, 4200} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			long := "registry.example/adc/" + strings.Repeat("a", size) + "@sha256:" + strings.Repeat("0", 64)
			e := open(t, gs)
			n := e.node(types.NodeKindPlan, reg, false)
			c := edit(card(n), func(c *Card) { c.Image = long })
			id, err := e.match(d, c, builder)
			if err != nil {
				t.Fatal(err)
			}
			signal := imageSignal(long)
			if len(signal) > maxSignal || !strings.HasPrefix(signal, "image sha256:") {
				t.Fatalf("signal %q", signal)
			}
			if l, _, _ := e.state(id, n); l.PlacementChecks.Capability.Detail != signal {
				t.Fatalf("capability detail %q, want %q", l.PlacementChecks.Capability.Detail, signal)
			}
			_, miss := e.launch(d, id, c, builder)
			e.missed(id, n, miss, "wrong_owner", signal)
		})
	}
}

func TestReasonFits(t *testing.T) {
	long := strings.Repeat("é", 600) // 1200 bytes
	if r := reason(long); len(r) > 1024 || !strings.HasSuffix(r, "...") || !utf8.ValidString(r) {
		t.Fatalf("reason of %d bytes: %d bytes, valid %v", len(long), len(r), utf8.ValidString(r))
	}
	if reason("short") != "short" {
		t.Fatal("a short reason changed")
	}
}

// end loses attempt a under lease id, seals it, and settles the lease, so
// its node may be placed again.
func (e env) end(id types.LeaseID, a types.AttemptID) {
	e.t.Helper()
	e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.LoseRunning(tx, a, types.AttemptLostReasonValueAgentDisconnect)
	})
	e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.SealMinimal(tx, a, 2, graph.Units{}, e.blobs, nil, actor)
	})
	e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, id, 3, lease.LeaseEvent{Kind: lease.Revoked, Reason: "test"}, nil)
	})
	e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, id, 4, lease.LeaseEvent{Kind: lease.Settled,
			Settlement: &types.LeaseSettlementValue{Calls: []types.CallID{}, Liabilities: []types.CallID{}}}, nil)
	})
}

// The attempt rail (P17a; fault 1: "a counter reset across an
// invalidation"): a round's attempts are counted through the node's
// invalidations; the launch past its rail is refused, starts nothing, and
// holds the node on budget_rail (rail:attempts_per_round), owned by the
// operator; a refused launch counts against no rail.
func TestLaunchAttemptRail(t *testing.T) {
	reg, gs := stub(t, nil)
	e := open(t, gs)
	d := Dispatcher{Registry: reg, Images: images{}, Credentials: creds(true),
		Rails: func(*eventstore.Tx, types.TaskID) (graph.Caps, error) {
			return graph.Caps{AttemptsPerRound: 2, RoundsPerTask: 5, MergeOperationsPerGeneration: 2, PerTaskMicros: 1 << 50, PerDayMicros: 1 << 50}, nil
		}}
	n := e.node(types.NodeKindPlan, reg, false)
	for i := range 2 {
		id, err := e.match(d, card(n), builder)
		if err != nil {
			t.Fatal(err)
		}
		a, miss := e.launch(d, id, card(n), builder)
		if miss != nil {
			t.Fatalf("attempt %d: %v", i+1, miss)
		}
		e.end(id, a)
		if i == 0 {
			e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Invalidate(tx, n) })
		}
	}
	id, err := e.match(d, card(n), builder)
	if err != nil {
		t.Fatal(err)
	}
	_, miss := e.launch(d, id, card(n), builder)
	l, out, cur := e.state(id, n)
	if miss == nil || miss.Hold == nil || miss.Hold.Hold != "budget_rail" || miss.Hold.SignalInstance != graph.RailSignal(graph.RailAttempts) ||
		miss.Hold.Owner != types.OwnerOperator || l.State != types.LeaseStateReleased || out != 0 {
		t.Fatalf("a third attempt over a rail of 2: %v, lease %s, %d reserved", miss, l.State, out)
	}
	if cur == nil || cur.FencingToken != 2 {
		t.Errorf("the refused launch started an attempt: %+v", cur)
	}
	e.view(func(tx *eventstore.Tx) error {
		raw, _, err := tx.Get(graph.NodeProjection, string(n))
		if !strings.Contains(string(raw), `"hold":"budget_rail"`) || !strings.Contains(string(raw), `"state":"held"`) {
			t.Errorf("the node is not held: %s", raw)
		}
		return err
	})
}
