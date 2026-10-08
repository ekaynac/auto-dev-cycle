// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"encoding/json"
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
	tskID = "tsk_01m3250v9xft08ssr4ygq6g2np"
	prjB  = "prj_01m3250vm7f6ktw39dszqnyddr"
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

// do submits one command whose logic is fn.
func (g graphStore) do(fn func(tx *eventstore.Tx) ([]eventstore.Event, error)) error {
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

func (g graphStore) must(fn func(tx *eventstore.Tx) ([]eventstore.Event, error)) {
	g.t.Helper()
	if err := g.do(fn); err != nil {
		g.t.Fatal(err)
	}
}

// build is a build node with one plan input, under a fresh id.
func build(t *testing.T) types.NodeRecorded {
	id, err := ids.New("nod")
	if err != nil {
		t.Fatal(err)
	}
	n := types.Node{ID: types.NodeID(id), Kind: types.NodeKindBuild, DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{"task":"x"}`),
		Inputs:   []types.InputSpec{{Name: "plan", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: planID, ResultKind: types.ResultKindPlan}}}},
		Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}
	return types.NodeRecorded{Node: n, Task: tskID, Project: prjID}
}

func (g graphStore) record(r types.NodeRecorded) types.NodeID {
	g.must(func(*eventstore.Tx) ([]eventstore.Event, error) { return Record(r) })
	return r.Node.ID
}

func (g graphStore) claim(n types.NodeID) (got types.NodeID) {
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		var evs []eventstore.Event
		var err error
		got, evs, err = Claim(tx, n, map[string]Resolved{"plan": {Result: resA}}, 1)
		return evs, err
	})
	return got
}

func (g graphStore) state(n types.NodeID) types.NodeState {
	var rec nodeRecord
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		rec, _, err = load[nodeRecord](tx, NodeProjection, string(n))
		return err
	}); err != nil {
		g.t.Fatal(err)
	}
	return rec.State
}

// result is a completed patch result of n's claimed key.
func (g graphStore) result(n types.NodeID, completed bool) types.Result {
	st := g.state(n)
	f := BuildFacts{Submitted: completed, Reported: types.OutcomeFound, Diff: diff, Project: prjID}
	r, err := Build(*st.Key, f)
	if err != nil {
		g.t.Fatal(err)
	}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return RecordResult(tx, types.ResultRecorded{Node: n, Result: r})
	})
	return r
}

// Duplicate demands share one claim: many claims of one key, at once, from
// distinct nodes, make one node.claimed and all name its node.
func TestConcurrentClaimsShareOne(t *testing.T) {
	g := openStore(t)
	var nodes []types.NodeID
	for range 8 {
		nodes = append(nodes, g.record(build(t)))
	}
	got := make([]types.NodeID, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Go(func() { got[i] = g.claim(n) })
	}
	wg.Wait()
	claimed := 0
	for i, n := range nodes {
		if got[i] != got[0] {
			t.Fatalf("claims name %s and %s", got[0], got[i])
		}
		switch st := g.state(n); {
		case st.State == types.NodeStateStateClaimed:
			claimed++
		case st.State != types.NodeStateStateWaiting || len(st.WaitingOn) != 1 || st.WaitingOn[0] != "plan" || st.Key != nil:
			t.Fatalf("a joining node is %+v, want waiting on plan", st)
		}
	}
	if claimed != 1 || g.state(got[0]).State != types.NodeStateStateClaimed {
		t.Fatalf("%d claimed nodes, want the one claims name", claimed)
	}
	// The same node demanded again joins its own claim.
	if again := g.claim(got[0]); again != got[0] {
		t.Fatalf("a repeated demand names %s", again)
	}
}

func (g graphStore) nodeOf(n types.NodeID) types.Node {
	var rec nodeRecord
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		rec, _, err = load[nodeRecord](tx, NodeProjection, string(n))
		return err
	}); err != nil {
		g.t.Fatal(err)
	}
	return rec.Recorded.Node
}

// The projections a rebuild reaches equal the live ones.
func TestRebuildReachesTheSameGraph(t *testing.T) {
	g := openStore(t)
	a := g.record(build(t))
	succ := build(t)
	succ.Predecessor = &a
	b := g.record(succ)
	g.claim(a)
	g.claim(b)
	r := g.result(a, true)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, a, r.ID) })
	if st := g.state(a); st.State != types.NodeStateStateCompleted || *st.Result != r.ID {
		t.Fatalf("%+v", st)
	}
	var c claim
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		c, _, err = load[claim](tx, ClaimProjection, string(g.state(a).Key.Key))
		return err
	}); err != nil || c.Node != a || c.Command == nil || !strings.HasPrefix(*c.Command, "cmd_") {
		t.Fatalf("the claim %+v does not name its node and command: %v", c, err)
	}
	diffs, err := g.s.RebuildProjections(context.Background())
	if err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// The fold refuses events its state does not derive: forged or replayed.
func TestFoldRefusals(t *testing.T) {
	g := openStore(t)
	a := g.record(build(t))
	g.claim(a)
	failed := g.result(a, false) // could_not_run
	other := g.record(build(t))  // the same key, never claimed
	recA := build(t)
	recA.Node.ID = a
	forge := func(kind, subject string, project types.ProjectID, v types.GraphEventVariant) func(*eventstore.Tx) ([]eventstore.Event, error) {
		return func(*eventstore.Tx) ([]eventstore.Event, error) { return event(kind, subject, project, v) }
	}
	key := *g.state(a).Key
	in := []types.NodeClaimedInputsItem{{Name: "plan", Result: ptr(types.ResultID(resA))}}
	inB := []types.NodeClaimedInputsItem{{Name: "plan", Result: ptr(types.ResultID(resB))}}
	ok := unrecorded(t, key)
	elsewhere, orphan := build(t), build(t)
	elsewhere.Type, orphan.Type = "node_recorded", "node_recorded"
	orphan.Predecessor = ptr(types.NodeID("nod_01m3250vnfeyrv67x6w0ddqr6a"))
	odd := unrecorded(t, key)
	odd.Completed, odd.Outcome, odd.Reason, odd.Detail = true, types.OutcomeCouldNotRun, failed.Reason, "a gate that could not load its baseline"
	g.must(func(*eventstore.Tx) ([]eventstore.Event, error) {
		return event(ResultRecorded, string(odd.ID), prjID, &types.ResultRecorded{Type: "result_recorded", Node: a, Result: odd})
	})
	// fresh and c have keys of their own: fresh is unclaimed, c claimed;
	// good is a selectable result of a.
	freshRec, cRec := build(t), build(t)
	freshRec.Node.Params, cRec.Node.Params = json.RawMessage(`{"task":"fresh"}`), json.RawMessage(`{"task":"c"}`)
	fresh, c := g.record(freshRec), g.record(cRec)
	g.claim(c)
	keyF, err := Key(freshRec.Node, map[string]Resolved{"plan": {Result: resA}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	claimF := &types.NodeClaimed{Type: "node_claimed", Node: fresh, Key: keyF, Inputs: []types.NodeClaimedInputsItem{{Name: "plan", Result: ptr(types.ResultID(resA))}}}
	good := g.result(a, true)
	stranger := build(t)
	stranger.Type, stranger.Task, stranger.Predecessor = "node_recorded", "tsk_01m3250vkwffc979nzjw0g2sf9", &a
	key2, err := Key(g.nodeOf(a), map[string]Resolved{"plan": {Result: resA}}, 2)
	if err != nil {
		t.Fatal(err)
	}
	for name, fn := range map[string]func(*eventstore.Tx) ([]eventstore.Event, error){
		"a node recorded twice":                  forge(NodeRecorded, string(a), prjID, &types.NodeRecorded{Type: "node_recorded", Node: recA.Node, Task: tskID, Project: prjID}),
		"a node under another subject":           forge(NodeRecorded, string(other), prjID, &types.NodeRecorded{Type: "node_recorded", Node: build(t).Node, Task: tskID, Project: prjID}),
		"a node in another project":              forge(NodeRecorded, string(elsewhere.Node.ID), prjB, &elsewhere),
		"a predecessor of another task":          forge(NodeRecorded, string(stranger.Node.ID), prjID, &stranger),
		"a claimed node claimed again":           forge(NodeClaimed, string(a), prjID, &types.NodeClaimed{Type: "node_claimed", Node: a, Key: key2, Inputs: in}),
		"a recorded node under node.claimed":     forge(NodeClaimed, string(elsewhere.Node.ID), prjID, &elsewhere),
		"a claim under node.recorded":            forge(NodeRecorded, string(other), prjID, &types.NodeClaimed{Type: "node_claimed", Node: other, Key: key, Inputs: in}),
		"a result under result.selected":         forge(ResultSelected, string(ok.ID), prjID, &types.ResultRecorded{Type: "result_recorded", Node: a, Result: ok}),
		"a selection under result.recorded":      forge(ResultRecorded, string(failed.ID), prjID, &types.ResultSelected{Type: "result_selected", Node: a, Result: failed.ID}),
		"a completed could_not_run selected":     forge(ResultSelected, string(odd.ID), prjID, &types.ResultSelected{Type: "result_selected", Node: a, Result: odd.ID}),
		"a fresh claim under node.recorded":      forge(NodeRecorded, string(fresh), prjID, claimF),
		"a fresh claim under another subject":    forge(NodeClaimed, string(other), prjID, claimF),
		"a fresh claim in another project":       forge(NodeClaimed, string(fresh), prjB, claimF),
		"a good selection under result.recorded": forge(ResultRecorded, string(good.ID), prjID, &types.ResultSelected{Type: "result_selected", Node: a, Result: good.ID}),
		"a good selection under another subject": forge(ResultSelected, string(a), prjID, &types.ResultSelected{Type: "result_selected", Node: a, Result: good.ID}),
		"a's result selected for c":              forge(ResultSelected, string(good.ID), prjID, &types.ResultSelected{Type: "result_selected", Node: c, Result: good.ID}),
		"an unknown predecessor":                 forge(NodeRecorded, string(orphan.Node.ID), prjID, &orphan),
		"a second claim of a key":                forge(NodeClaimed, string(other), prjID, &types.NodeClaimed{Type: "node_claimed", Node: other, Key: key, Inputs: in}),
		"a claim whose key is not its own":       forge(NodeClaimed, string(other), prjID, &types.NodeClaimed{Type: "node_claimed", Node: other, Key: types.ComputationKey{Key: diff, Generation: 1}, Inputs: in}),
		"a claim from other inputs":              forge(NodeClaimed, string(other), prjID, &types.NodeClaimed{Type: "node_claimed", Node: other, Key: key, Inputs: inB}),
		"a claim in another project":             forge(NodeClaimed, string(other), prjB, &types.NodeClaimed{Type: "node_claimed", Node: other, Key: key, Inputs: in}),
		"a claim of a claimed node":              forge(NodeClaimed, string(a), prjID, &types.NodeClaimed{Type: "node_claimed", Node: a, Key: key, Inputs: in}),
		"a result of an unclaimed node":          forge(ResultRecorded, string(ok.ID), prjID, &types.ResultRecorded{Type: "result_recorded", Node: other, Result: ok}),
		"a result of another key":                forge(ResultRecorded, string(ok.ID), prjID, &types.ResultRecorded{Type: "result_recorded", Node: a, Result: withKey(ok, diff)}),
		"a result recorded twice":                forge(ResultRecorded, string(failed.ID), prjID, &types.ResultRecorded{Type: "result_recorded", Node: a, Result: failed}),
		"a result under another subject":         forge(ResultRecorded, string(a), prjID, &types.ResultRecorded{Type: "result_recorded", Node: a, Result: ok}),
		"a could_not_run selected":               forge(ResultSelected, string(failed.ID), prjID, &types.ResultSelected{Type: "result_selected", Node: a, Result: failed.ID}),
		"an unrecorded result selected":          forge(ResultSelected, string(ok.ID), prjID, &types.ResultSelected{Type: "result_selected", Node: a, Result: ok.ID}),
		"a result selected for another node":     forge(ResultSelected, string(failed.ID), prjID, &types.ResultSelected{Type: "result_selected", Node: other, Result: failed.ID}),
		"a selection under another subject":      forge(ResultSelected, string(a), prjID, &types.ResultSelected{Type: "result_selected", Node: a, Result: failed.ID}),
	} {
		if err := g.do(fn); err == nil || !strings.Contains(err.Error(), "graph: event") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A selected result is the node's one: a second selection is refused.
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, a, good.ID) })
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, a, good.ID) }); err == nil {
		t.Error("a second selection was admitted")
	}
}

// unrecorded is a completed result of key, not yet recorded.
func unrecorded(t *testing.T, key types.ComputationKey) types.Result {
	r, err := Build(key, BuildFacts{Submitted: true, Reported: types.OutcomeFound, Diff: diff, Project: prjID})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func withKey(r types.Result, key types.Digest) types.Result {
	r.Computation = key
	return r
}

func ptr[T any](v T) *T { return &v }
