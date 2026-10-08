// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

const (
	tskID  = "tsk_01m3250v9xft08ssr4ygq6g2np"
	tskB   = "tsk_01m3250vkwffc979nzjw0g2sf9"
	tskC   = "tsk_01m3250vm7f6ktw39dszqnyddr"
	prjB   = "prj_01m3250vm7f6ktw39dszqnyddr"
	nodNil = "nod_01m3250vnfeyrv67x6w0ddqr6a"
)

type graphStore struct {
	t *testing.T
	s *eventstore.Store
}

func openStore(t *testing.T) graphStore {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	lock, err := eventstore.AcquireLock(state)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	none := func(*eventstore.Tx, eventstore.Command) error { return nil }
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: Kinds(), Blobs: blobs,
		Hooks: eventstore.Hooks{Identify: none, Authorize: none, Project: eventstore.ProjectEach(Project)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); lock.Release() })
	return graphStore{t, s}
}

type logic func(tx *eventstore.Tx) ([]eventstore.Event, error)

// do submits one command whose logic is fn.
func (g graphStore) do(fn logic) error {
	cmd, err := ids.New("cmd")
	if err != nil {
		g.t.Fatal(err)
	}
	actor, _ := ids.New("act")
	_, err = g.s.SubmitApplied(context.Background(), eventstore.Command{ID: cmd, Actor: actor, Action: "graph.test", IdempotencyKey: cmd, Input: []byte(`{}`)},
		eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := fn(tx)
			return evs, json.RawMessage(`{}`), err
		}))
	return err
}

func (g graphStore) must(fn logic) {
	g.t.Helper()
	if err := g.do(fn); err != nil {
		g.t.Fatal(err)
	}
}

func view[T any](g graphStore, projection, key string) (T, bool) {
	var v T
	var ok bool
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) (err error) { v, ok, err = load[T](tx, projection, key); return err }); err != nil {
		g.t.Fatal(err)
	}
	return v, ok
}

// newNode is a node of kind with one plan input, in task tskID of prjID,
// under a fresh id.
func newNode(t *testing.T, kind types.NodeKind) types.NodeRecorded {
	id, err := ids.New("nod")
	if err != nil {
		t.Fatal(err)
	}
	n := types.Node{ID: types.NodeID(id), Kind: kind, DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{"task":"x"}`),
		Inputs:   []types.InputSpec{{Name: "plan", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: planID, ResultKind: types.ResultKindPlan}}}},
		Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}
	return types.NodeRecorded{Type: "node_recorded", Node: n, Task: tskID, Project: prjID}
}

func (g graphStore) record(r types.NodeRecorded) types.NodeID {
	g.t.Helper()
	g.must(func(*eventstore.Tx) ([]eventstore.Event, error) { return Record(r) })
	return r.Node.ID
}

func claimOf(n types.NodeID) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return Claim(tx, n, map[string]Resolved{"plan": {Result: resA}}, 1)
	}
}

func (g graphStore) state(n types.NodeID) types.NodeState {
	rec, _ := view[nodeRecord](g, NodeProjection, string(n))
	return rec.State
}

func (g graphStore) claims() int {
	n := 0
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) error {
		evs, err := tx.EventsAfter(0, 1000)
		for _, e := range evs {
			if e.Kind == NodeClaimed {
				n++
			}
		}
		return err
	}); err != nil {
		g.t.Fatal(err)
	}
	return n
}

// Duplicate demands of one node instance, at once, make one claim (13 [G10]).
func TestRepeatDemandsJoinOneClaim(t *testing.T) {
	g := openStore(t)
	a := g.record(newNode(t, types.NodeKindBuild))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { g.must(claimOf(a)) })
	}
	wg.Wait()
	if c := g.claims(); c != 1 || g.state(a).State != types.NodeStateStateClaimed {
		t.Fatalf("%d claims, state %s", c, g.state(a).State)
	}
}

// A memoizable key is claimed once per task: the second instance is told
// who holds it and is left as it was. Build instances never share.
func TestKeyClaimsByKind(t *testing.T) {
	g := openStore(t)
	a, b := g.record(newNode(t, types.NodeKindGate)), g.record(newNode(t, types.NodeKindGate))
	g.must(claimOf(a))
	var held ErrKeyClaimed
	if err := g.do(claimOf(b)); !errors.As(err, &held) || held.Node != a {
		t.Fatalf("the second gate's claim: %v", err)
	}
	if st := g.state(b); st.State != types.NodeStateStateWaiting || st.Key != nil {
		t.Fatalf("the refused gate is %+v", st)
	}
	x, y := g.record(newNode(t, types.NodeKindBuild)), g.record(newNode(t, types.NodeKindBuild))
	g.must(claimOf(x))
	g.must(claimOf(y))
	if *g.state(x).Key != *g.state(y).Key || g.state(y).State != types.NodeStateStateClaimed {
		t.Fatal("two builds of one key did not both claim")
	}
	// Two claims of one memoizable key in one command fail.
	c, d := g.record(newNode(t, types.NodeKindEvidence)), g.record(newNode(t, types.NodeKindEvidence))
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		e1, err1 := claimOf(c)(tx)
		e2, err2 := claimOf(d)(tx)
		return append(e1, e2...), errors.Join(err1, err2)
	}); err == nil {
		t.Fatal("two claims of one key in one command")
	}
}

// Claims never span tasks or projects: same-key memoizable nodes of other
// tasks claim their own, and no event moves a node or a task across.
func TestClaimsAreTaskScoped(t *testing.T) {
	g := openStore(t)
	a := g.record(newNode(t, types.NodeKindGate))
	otherTask, otherProject := newNode(t, types.NodeKindGate), newNode(t, types.NodeKindGate)
	otherTask.Task = tskB
	otherProject.Task, otherProject.Project = tskC, prjB
	b, c := g.record(otherTask), g.record(otherProject)
	for _, n := range []types.NodeID{a, b, c} {
		g.must(claimOf(n))
	}
	if *g.state(a).Key != *g.state(c).Key {
		t.Fatal("the nodes do not share a key")
	}
	later := newNode(t, types.NodeKindBuild) // unclaimed, in the other project
	later.Task, later.Project = tskC, prjB
	d := g.record(later)
	keyD, err := Key(later.Node, map[string]Resolved{"plan": {Result: resA}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	intruder := newNode(t, types.NodeKindGate)
	intruder.Project = prjB
	for name, fn := range map[string]logic{
		"a claim of another project's node here": forge(NodeClaimed, string(d), prjID, &types.NodeClaimed{Type: "node_claimed", Node: d, Key: keyD, Inputs: g.claimed(a).Inputs}),
		"a node of the task in another project":  forge(NodeRecorded, string(intruder.Node.ID), prjB, &intruder),
	} {
		if err := g.do(fn); err == nil {
			t.Errorf("%s was admitted", name)
		}
	}
}

func (g graphStore) claimed(n types.NodeID) claim {
	c, _ := view[claim](g, ClaimProjection, string(n))
	return c
}

func forge(kind, subject string, project types.ProjectID, v types.GraphEventVariant) logic {
	return func(*eventstore.Tx) ([]eventstore.Event, error) { return event(kind, subject, project, v) }
}

// The projections a rebuild reaches equal the live ones, with a successor
// and an answer input in a claim; a claim keeps its resolution and command.
func TestRebuildReachesTheSameGraph(t *testing.T) {
	g := openStore(t)
	a := g.record(newNode(t, types.NodeKindBuild))
	succ := newNode(t, types.NodeKindBuild)
	succ.Predecessor, succ.Node.Round = &a, 2
	succ.Node.Inputs = append(succ.Node.Inputs, types.InputSpec{Name: "ruling", Source: types.InputSpecSource{Variant: &types.AnswerSource{Type: "answer", OwedInput: ownID, Revision: 2}}})
	b := g.record(succ)
	g.must(claimOf(a))
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return Claim(tx, b, map[string]Resolved{"plan": {Result: resA}, "ruling": {Approval: aprA, Revision: 2}}, 1)
	})
	c := g.claimed(b)
	if c.Key != *g.state(b).Key || len(c.Inputs) != 2 || *c.Inputs[1].Approval != aprA || *c.Inputs[1].Revision != 2 || c.Command == nil || !strings.HasPrefix(*c.Command, "cmd_") {
		t.Fatalf("the claim %+v", c)
	}
	diffs, err := g.s.RebuildProjections(context.Background())
	if err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// Other kinds pass through the fold untouched.
func TestOtherKindsPassThrough(t *testing.T) {
	g := openStore(t)
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) error {
		return Project(tx, []eventstore.Event{{Kind: "lease.granted/v1", Payload: json.RawMessage(`not json`)}})
	}); err != nil {
		t.Fatal(err)
	}
}

// The fold refuses events its state does not derive: forged or replayed.
func TestFoldRefusals(t *testing.T) {
	g := openStore(t)
	aRec := newNode(t, types.NodeKindBuild)
	a := g.record(aRec)
	g.must(claimOf(a))
	key := *g.state(a).Key
	other := g.record(newNode(t, types.NodeKindBuild)) // the same key, unclaimed
	in := []types.NodeClaimedInputsItem{{Name: "plan", Result: ptr(types.ResultID(resA))}}
	claimOther := func(items []types.NodeClaimedInputsItem) *types.NodeClaimed {
		return &types.NodeClaimed{Type: "node_claimed", Node: other, Key: key, Inputs: items}
	}
	// c is claimed with a key of its own.
	cRec := newNode(t, types.NodeKindBuild)
	cRec.Node.Params = json.RawMessage(`{"task":"c"}`)
	c := g.record(cRec)
	g.must(claimOf(c))
	key2, err := Key(aRec.Node, map[string]Resolved{"plan": {Result: resA}}, 2)
	if err != nil {
		t.Fatal(err)
	}
	with := func(r types.NodeRecorded, f func(*types.NodeRecorded)) *types.NodeRecorded { f(&r); return &r }
	fresh := func() types.NodeRecorded { return newNode(t, types.NodeKindBuild) }
	foreignPred := fresh()
	foreignPred.Task, foreignPred.Project = tskC, prjB
	fp := g.record(foreignPred)
	gate := g.record(newNode(t, types.NodeKindGate))
	g.must(claimOf(gate))
	for name, fn := range map[string]logic{
		"a node recorded twice":        forge(NodeRecorded, string(a), prjID, &aRec),
		"a node under another subject": forge(NodeRecorded, string(other), prjID, with(fresh(), func(*types.NodeRecorded) {})),
		"a node in another project": func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			r := fresh()
			return event(NodeRecorded, string(r.Node.ID), prjB, &r)
		},
		"an unknown predecessor": func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			r := with(fresh(), func(r *types.NodeRecorded) { r.Predecessor, r.Node.Round = ptr(types.NodeID(nodNil)), 2 })
			return event(NodeRecorded, string(r.Node.ID), prjID, r)
		},
		"a predecessor of another task": func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			r := with(fresh(), func(r *types.NodeRecorded) { r.Task, r.Predecessor, r.Node.Round = tskB, &a, 2 })
			return event(NodeRecorded, string(r.Node.ID), prjID, r)
		},
		"a predecessor in another project": func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			r := with(fresh(), func(r *types.NodeRecorded) { r.Predecessor, r.Node.Round = &fp, 2 })
			return event(NodeRecorded, string(r.Node.ID), prjID, r)
		},
		"a predecessor of another kind": func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			r := with(fresh(), func(r *types.NodeRecorded) { r.Predecessor, r.Node.Round = &gate, 2 })
			return event(NodeRecorded, string(r.Node.ID), prjID, r)
		},
		"a successor of the same round": func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			r := with(fresh(), func(r *types.NodeRecorded) { r.Predecessor = &a })
			return event(NodeRecorded, string(r.Node.ID), prjID, r)
		},
		"a first node of round 2": func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			r := with(fresh(), func(r *types.NodeRecorded) { r.Node.Round = 2 })
			return event(NodeRecorded, string(r.Node.ID), prjID, r)
		},
		"a recorded node under node.claimed": func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			r := fresh()
			return event(NodeClaimed, string(r.Node.ID), prjID, &r)
		},
		"a claim under node.recorded":      forge(NodeRecorded, string(other), prjID, claimOther(in)),
		"a claim under another subject":    forge(NodeClaimed, string(c), prjID, claimOther(in)),
		"a claim in another project":       forge(NodeClaimed, string(other), prjB, claimOther(in)),
		"a claim whose key is not its own": forge(NodeClaimed, string(other), prjID, &types.NodeClaimed{Type: "node_claimed", Node: other, Key: types.ComputationKey{Key: diff, Generation: 1}, Inputs: in}),
		"a claim from other inputs":        forge(NodeClaimed, string(other), prjID, claimOther([]types.NodeClaimedInputsItem{{Name: "plan", Result: ptr(types.ResultID(resB))}})),
		"a claim of a claimed node":        forge(NodeClaimed, string(a), prjID, &types.NodeClaimed{Type: "node_claimed", Node: a, Key: key2, Inputs: in}),
		"a claim with no inputs":           forge(NodeClaimed, string(other), prjID, claimOther([]types.NodeClaimedInputsItem{})),
		"a claim with an input twice":      forge(NodeClaimed, string(other), prjID, claimOther(append(in, in[0]))),
		"a claim with an undeclared input": forge(NodeClaimed, string(other), prjID, claimOther(append(in, types.NodeClaimedInputsItem{Name: "extra", Result: ptr(types.ResultID(resB))}))),
		"a claim input with both shapes":   forge(NodeClaimed, string(other), prjID, claimOther([]types.NodeClaimedInputsItem{{Name: "plan", Result: ptr(types.ResultID(resA)), Approval: ptr(types.ApprovalID(aprA)), Revision: ptr(int64(2))}})),
		"a claimed gate key claimed again in the task": func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			r := newNode(t, types.NodeKindGate)
			evs, err := Record(r)
			evc, err2 := event(NodeClaimed, string(r.Node.ID), prjID, &types.NodeClaimed{Type: "node_claimed", Node: r.Node.ID, Key: *g.state(gate).Key, Inputs: in})
			return append(evs, evc...), errors.Join(err, err2)
		},
	} {
		if err := g.do(fn); err == nil || !strings.Contains(err.Error(), "graph: event") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// Every projected node state is a valid records/node-state.
func TestInvalidNodeStatesAreNotStored(t *testing.T) {
	g := openStore(t)
	r := newNode(t, types.NodeKindBuild)
	bad := types.NodeState{Node: r.Node.ID, State: types.NodeStateStateHeld, WaitingOn: []types.Name{}, Holds: []types.NodeHold{}}
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return nil, putNode(tx, nodeRecord{Recorded: r, State: bad})
	}); err == nil {
		t.Fatal("a held state with no hold was stored")
	}
}
