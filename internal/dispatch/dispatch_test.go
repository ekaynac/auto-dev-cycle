// SPDX-License-Identifier: Apache-2.0

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
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

// open is a store with the lease fold over capacity and the graph fold,
// as adcd composes them.
func open(t *testing.T, groups []types.ResourceGroup) env {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	lock := must(eventstore.AcquireLock(state))
	blobs := must(blobstore.Open(state))
	r := must(capacity.NewReservations(groups))
	kinds := graph.Kinds()
	for k, v := range lease.Kinds() {
		kinds[k] = v
	}
	none := func(*eventstore.Tx, eventstore.Command) error { return nil }
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: kinds, Blobs: blobs,
		Hooks: eventstore.Hooks{Identify: none, Authorize: none, Project: eventstore.ProjectEach(
			capacity.Hook(lease.Projector{Blobs: blobs, Units: graph.Units{}}, r), graph.Project)}})
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
	return Card{Node: n, Project: project, Image: image, Tokens: 1000, Deadline: "2026-10-04T14:00:00Z",
		Policy:    types.ProjectProfileDataPolicy{Floor: "contracted", Routing: "verified", AllowedDestinations: []types.Slug{"stub"}},
		Footprint: types.LeaseFootprint{ContextTokens: 1, ExpectedTurns: 1}}
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
	a, ready := types.AttemptID(must(ids.New("att"))), d.Observe(context.Background(), c, exe)
	var miss *Miss
	e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, m, err := d.Launch(tx, id, 2, c, a, types.AttemptBase{Tree: "git:4b825dc642cb6eb9a060e54bf8d69288fbee4904", ContractRevision: "1"}, ready)
		miss = m
		return evs, err
	})
	return a, miss
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
	d := Dispatcher{Registry: reg, Images: images{}, Credentials: creds(true)}
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
	d := Dispatcher{Registry: reg}
	plan, req := e.node(types.NodeKindPlan, reg, false), e.node(types.NodeKindReviewRequirement, reg, false)
	unclaimed := e.node(types.NodeKindPlan, reg, true)
	for name, c := range map[string]struct {
		card  Card
		exe   types.ExecutorID
		check string // the blocking check; "" for an error before placement
	}{
		"a destination the project does not allow": {edit(card(plan), func(c *Card) { c.Policy.AllowedDestinations = []types.Slug{"other"} }), builder, "data_floor"},
		"a floor the destination is below":         {edit(card(plan), func(c *Card) { c.Policy.Floor = "own" }), builder, "data_floor"},
		"an unregistered executor":                 {card(plan), "exe_01m4tm99haemrs8zjs56wjm467", "data_floor"},
		"a reviewer on a builder's node":           {card(plan), reviewer, "independence"},
		"a builder on a rostered node":             {card(req), builder, "independence"},
		"no pinned image":                          {edit(card(plan), func(c *Card) { c.Image = "" }), builder, "capability"},
		"another project's card":                   {edit(card(plan), func(c *Card) { c.Project = other }), builder, ""},
		"an unclaimed node":                        {card(unclaimed), builder, ""},
	} {
		_, err := e.match(d, c.card, c.exe)
		var r *Refused
		if errors.As(err, &r) != (c.check != "") || (r != nil && r.Check != c.check) || err == nil {
			t.Errorf("%s: %v, want refused by %q", name, err, c.check)
		}
	}
	// Capacity refuses the whole command: the request goes with the grant.
	var full *capacity.Exhausted
	if _, err := e.match(d, edit(card(plan), func(c *Card) { c.Tokens = 3000000 }), builder); !errors.As(err, &full) || full.Reason != vocab.ReasonCapacityReservedFull {
		t.Errorf("over the group's limit: %v", err)
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
		"the gateway holds no credential": {Dispatcher{Registry: reg, Images: images{}, Credentials: creds(false)}, "source_invalid_config", "destination stub"},
		"no gateway (P11 unbuilt)":        {Dispatcher{Registry: reg, Images: images{}}, "source_invalid_config", "destination stub"},
		"the image is absent":             {Dispatcher{Registry: reg, Images: images{sandbox.ErrImage}, Credentials: creds(true)}, "wrong_owner", "image " + image},
		"no image store":                  {Dispatcher{Registry: reg, Credentials: creds(true)}, "wrong_owner", "image " + image},
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
		"the destination moved":          {types.NodeKindPlan, builder, Dispatcher{Registry: moved, Images: images{}, Credentials: creds(true)}},
		"the roster member's family":     {types.NodeKindReviewRequirement, reviewer, Dispatcher{Registry: renamed, Images: images{}, Credentials: creds(true)}},
		"a builder's family, not placed": {types.NodeKindPlan, builder, Dispatcher{Registry: renamed, Images: images{}, Credentials: creds(true)}},
	} {
		t.Run(name, func(t *testing.T) {
			e := open(t, gs)
			n := e.node(c.kind, reg, false)
			id, err := e.match(Dispatcher{Registry: reg}, card(n), c.exe)
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
			e.missed(id, n, miss, "source_invalid_config", "executor "+string(c.exe))
		})
	}
	// A node held between the grant and the launch is no longer claimed:
	// the launch is refused and the lease released, with no second hold.
	e := open(t, gs)
	d := Dispatcher{Registry: reg, Images: images{}, Credentials: creds(true)}
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
