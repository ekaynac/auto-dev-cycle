// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	tskID  = "tsk_01m3250v9xft08ssr4ygq6g2np"
	tskB   = "tsk_01m3250vkwffc979nzjw0g2sf9"
	tskC   = "tsk_01m3250vm7f6ktw39dszqnyddr"
	prjB   = "prj_01m3250vm7f6ktw39dszqnyddr"
	nodNil = "nod_01m3250vnfeyrv67x6w0ddqr6a"
)

// graphStore is a store with the graph fold, and a selected plan result
// of task tskID that the nodes under test take as their input.
type graphStore struct {
	t       *testing.T
	s       *eventstore.Store
	plan    types.NodeID
	planRes types.ResultID
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
	g := graphStore{t: t, s: s}
	g.plan, g.planRes = g.planOf(tskID, prjID)
	return g
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

// planOf records, claims and completes a plan node of task in project, and
// returns it with its selected result.
func (g graphStore) planOf(task types.TaskID, project types.ProjectID) (types.NodeID, types.ResultID) {
	id, _ := ids.New("nod")
	r := types.NodeRecorded{Type: "node_recorded", Task: task, Project: project, Node: types.Node{ID: types.NodeID(id), Kind: types.NodeKindPlan,
		DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{}`), Inputs: []types.InputSpec{}, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}}
	n := g.record(r)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Claim(tx, n, map[string]Resolved{}, 1) })
	rid, _ := ids.New("res")
	res := types.Result{ID: types.ResultID(rid), Computation: g.state(n).Key.Key, Kind: types.ResultKindPlan, Value: []types.Ref{},
		Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Completed: true}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return RecordResult(tx, types.ResultRecorded{Node: n, Result: res})
	})
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, n, res.ID) })
	return n, res.ID
}

// newNode is a node of kind taking the store's plan result, in task tskID
// of prjID, under a fresh id.
func (g graphStore) newNode(kind types.NodeKind) types.NodeRecorded {
	return g.nodeOn(kind, g.plan)
}

func (g graphStore) nodeOn(kind types.NodeKind, plan types.NodeID) types.NodeRecorded {
	id, err := ids.New("nod")
	if err != nil {
		g.t.Fatal(err)
	}
	n := types.Node{ID: types.NodeID(id), Kind: kind, DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{"task":"x"}`),
		Inputs:   []types.InputSpec{{Name: "plan", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: plan, ResultKind: types.ResultKindPlan}}}},
		Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}
	return types.NodeRecorded{Type: "node_recorded", Node: n, Task: tskID, Project: prjID}
}

func (g graphStore) record(r types.NodeRecorded) types.NodeID {
	g.t.Helper()
	g.must(func(*eventstore.Tx) ([]eventstore.Event, error) { return Record(r) })
	return r.Node.ID
}

func claimWith(n types.NodeID, res types.ResultID) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return Claim(tx, n, map[string]Resolved{"plan": {Result: res}}, 1)
	}
}

func (g graphStore) claimOf(n types.NodeID) logic { return claimWith(n, g.planRes) }

func (g graphStore) state(n types.NodeID) types.NodeState {
	rec, _ := view[nodeRecord](g, NodeProjection, string(n))
	return rec.State
}

func (g graphStore) claimed(n types.NodeID) claim {
	c, _ := view[claim](g, ClaimProjection, string(n))
	return c
}

// result records a build result of n's key: completed and found, or not.
func (g graphStore) result(n types.NodeID, completed bool) types.Result {
	g.t.Helper()
	r := unrecorded(g.t, *g.state(n).Key)
	if !completed {
		r = must(Build(*g.state(n).Key, BuildFacts{Project: prjID}))
	}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return RecordResult(tx, types.ResultRecorded{Node: n, Result: r})
	})
	return r
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func (g graphStore) count(kind string) int {
	n := 0
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) error {
		evs, err := tx.EventsAfter(0, 1000)
		for _, e := range evs {
			if e.Kind == kind {
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
	a := g.record(g.newNode(types.NodeKindBuild))
	before := g.count(NodeClaimed)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { g.must(g.claimOf(a)) })
	}
	wg.Wait()
	if c := g.count(NodeClaimed) - before; c != 1 || g.state(a).State != types.NodeStateStateClaimed {
		t.Fatalf("%d claims, state %s", c, g.state(a).State)
	}
}

// A memoizable key is claimed once per task: the second instance is told
// who holds it and is left as it was. Build instances never share.
func TestKeyClaimsByKind(t *testing.T) {
	g := openStore(t)
	a, b := g.record(g.newNode(types.NodeKindGate)), g.record(g.newNode(types.NodeKindGate))
	g.must(g.claimOf(a))
	var held ErrKeyClaimed
	if err := g.do(g.claimOf(b)); !errors.As(err, &held) || held.Node != a {
		t.Fatalf("the second gate's claim: %v", err)
	}
	if st := g.state(b); st.State != types.NodeStateStateWaiting || st.Key != nil {
		t.Fatalf("the refused gate is %+v", st)
	}
	x, y := g.record(g.newNode(types.NodeKindBuild)), g.record(g.newNode(types.NodeKindBuild))
	g.must(g.claimOf(x))
	g.must(g.claimOf(y))
	if *g.state(x).Key != *g.state(y).Key || g.state(y).State != types.NodeStateStateClaimed {
		t.Fatal("two builds of one key did not both claim")
	}
	c, d := g.record(g.newNode(types.NodeKindEvidence)), g.record(g.newNode(types.NodeKindEvidence))
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		e1, err1 := g.claimOf(c)(tx)
		e2, err2 := g.claimOf(d)(tx)
		return append(e1, e2...), errors.Join(err1, err2)
	}); err == nil {
		t.Fatal("two claims of one key in one command")
	}
}

// Claims never span tasks or projects, and an input is a result of its
// declared producer in the node's project.
func TestClaimsAreScoped(t *testing.T) {
	g := openStore(t)
	a := g.record(g.newNode(types.NodeKindGate))
	otherTask := g.newNode(types.NodeKindGate)
	otherTask.Task = tskB
	b := g.record(otherTask)
	planB, resB := g.planOf(tskC, prjB)
	otherProject := g.nodeOn(types.NodeKindGate, planB)
	otherProject.Task, otherProject.Project = tskC, prjB
	c := g.record(otherProject)
	g.must(g.claimOf(a))
	g.must(g.claimOf(b))
	g.must(claimWith(c, resB))
	if *g.state(a).Key != *g.state(b).Key {
		t.Fatal("a and b do not share a key")
	}
	borrower := g.nodeOn(types.NodeKindBuild, g.plan) // in prjB, on prjID's plan
	borrower.Task, borrower.Project = tskC, prjB
	d := g.record(borrower)
	wrongProducer := g.newNode(types.NodeKindBuild)
	wrongProducer.Node.Inputs[0].Source.Variant.(*types.ResultSource).Producer = a
	e := g.record(wrongProducer)
	keyD := must(Key(borrower.Node, map[string]Resolved{"plan": {Result: g.planRes}}, 1))
	intruder := g.newNode(types.NodeKindGate)
	intruder.Project = prjB
	// P1: a producer's could_not_run result, never selected; P2: a result
	// of a producer's invalidated generation; P9: a result of another kind.
	retry := g.newNode(types.NodeKindPlan)
	retry.Node.Inputs = []types.InputSpec{}
	p1 := g.record(retry)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Claim(tx, p1, map[string]Resolved{}, 1) })
	unselected := must(Build(*g.state(p1).Key, BuildFacts{Project: prjID}))
	unselected.Kind = types.ResultKindPlan
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return RecordResult(tx, types.ResultRecorded{Node: p1, Result: unselected})
	})
	dropped, droppedRes := g.planOf(tskID, prjID)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Invalidate(tx, dropped) })
	builder := g.record(g.newNode(types.NodeKindBuild))
	g.must(g.claimOf(builder))
	patch := g.result(builder, true)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, builder, patch.ID) })
	// A producer's completed result that it did not select.
	twice := g.newNode(types.NodeKindPlan)
	twice.Node.Inputs, twice.Node.Params = []types.InputSpec{}, []byte(`{"twice":true}`)
	p5 := g.record(twice)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Claim(tx, p5, map[string]Resolved{}, 1) })
	planResult := func() types.Result {
		r := unrecorded(t, *g.state(p5).Key)
		r.Kind, r.Value, r.Outcome, r.Reason, r.Detail = types.ResultKindPlan, []types.Ref{}, types.OutcomeEmpty, vocab.ReasonCompletedClean, ""
		g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return RecordResult(tx, types.ResultRecorded{Node: p5, Result: r})
		})
		return r
	}
	chosen, passed := planResult(), planResult()
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, p5, chosen.ID) })
	on := func(producer types.NodeID, res types.ResultID) logic {
		return claimWith(g.record(g.nodeOn(types.NodeKindBuild, producer)), res)
	}
	for name, fn := range map[string]logic{
		"P1: an unselected could_not_run result":  on(p1, unselected.ID),
		"an unselected completed result":          on(p5, passed.ID),
		"P2: a result of an invalidated producer": on(dropped, droppedRes),
		"P9: a result of another kind":            on(builder, patch.ID),
		"another project's result as an input":    claimWith(d, g.planRes),
		"a result of another producer":            g.claimOf(e),
		"an unrecorded result":                    claimWith(g.record(g.newNode(types.NodeKindBuild)), resA),
		"a claim of another project's node here":  forge(NodeClaimed, string(d), prjID, &types.NodeClaimed{Type: "node_claimed", Node: d, Key: keyD, Inputs: g.claimed(a).Inputs}),
		"a node of the task in another project":   forge(NodeRecorded, string(intruder.Node.ID), prjB, &intruder),
	} {
		if err := g.do(fn); err == nil {
			t.Errorf("%s was admitted", name)
		}
	}
}

func forge(kind, subject string, project types.ProjectID, v types.GraphEventVariant) logic {
	return func(*eventstore.Tx) ([]eventstore.Event, error) { return event(kind, subject, project, v) }
}

// The life of a node: results, selection, an invalidation and a successor
// with an answer input; a rebuild reaches the same projections.
func TestNodeLifecycleRebuilds(t *testing.T) {
	g := openStore(t)
	a := g.record(g.newNode(types.NodeKindBuild))
	succ := g.newNode(types.NodeKindBuild)
	succ.Predecessor, succ.Node.Round = &a, 2
	succ.Node.Inputs = append(succ.Node.Inputs, types.InputSpec{Name: "ruling", Source: types.InputSpecSource{Variant: &types.AnswerSource{Type: "answer", OwedInput: ownID, Revision: 2}}})
	b := g.record(succ)
	g.must(g.claimOf(a))
	old := g.result(a, true)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, a, old.ID) })
	if st := g.state(a); st.State != types.NodeStateStateCompleted || *st.Result != old.ID {
		t.Fatalf("%+v", st)
	}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Invalidate(tx, a) })
	st := g.state(a)
	if st.State != types.NodeStateStateClaimed || st.Result != nil || st.Key.Generation != 2 || st.Key.Key == old.Computation {
		t.Fatalf("after the invalidation: %+v", st)
	}
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, a, old.ID) }); err == nil {
		t.Fatal("the old generation's result was selected again")
	}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Invalidate(tx, a) })
	if st := g.state(a); st.Key.Generation != 3 || g.claimed(a).Key != *st.Key {
		t.Fatalf("after a second invalidation: %+v, claim %+v", st, g.claimed(a))
	}
	fresh := g.result(a, true)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, a, fresh.ID) })
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return Claim(tx, b, map[string]Resolved{"plan": {Result: g.planRes}, "ruling": {Approval: aprA, Revision: 2}}, 1)
	})
	if c := g.claimed(b); len(c.Inputs) != 2 || *c.Inputs[1].Approval != aprA || c.Command == nil || !strings.HasPrefix(*c.Command, "cmd_") {
		t.Fatalf("the claim %+v", c)
	}
	diffs, err := g.s.RebuildProjections(context.Background())
	if err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// An invalidated memoizable node claims its new key in the task.
func TestInvalidationClaimsTheNewKey(t *testing.T) {
	g := openStore(t)
	a, b := g.record(g.newNode(types.NodeKindGate)), g.record(g.newNode(types.NodeKindGate))
	g.must(g.claimOf(a))
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Invalidate(tx, a) })
	if holder, ok := view[types.NodeID](g, KeyProjection, keyIndex(tskID, g.state(a).Key.Key)); !ok || holder != a {
		t.Fatal("the new key is not claimed")
	}
	// P4: the old key is stale, not held: a claim of it is ErrKeyStale.
	if holder, ok := view[types.NodeID](g, KeyProjection, keyIndex(tskID, must(Key(g.newNodeLike(a), map[string]Resolved{"plan": {Result: g.planRes}}, 1)).Key)); !ok || holder != "" {
		t.Fatalf("the old key is held by %q", holder)
	}
	var stale ErrKeyStale
	if err := g.do(g.claimOf(b)); !errors.As(err, &stale) || stale.Current != 2 {
		t.Fatalf("a claim at the old generation: %v", err)
	}
	// P3: no claim skips to another generation; at the current one, the
	// holder is named.
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return Claim(tx, b, map[string]Resolved{"plan": {Result: g.planRes}}, 5)
	}); !errors.As(err, &stale) {
		t.Fatalf("a claim at generation 5: %v", err)
	}
	var held ErrKeyClaimed
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return Claim(tx, b, map[string]Resolved{"plan": {Result: g.planRes}}, 2)
	}); !errors.As(err, &held) || held.Node != a {
		t.Fatalf("a claim at the current generation: %v", err)
	}
	g3 := must(Key(g.newNodeLike(b), map[string]Resolved{"plan": {Result: g.planRes}}, 3))
	if err := g.do(forge(NodeClaimed, string(b), prjID, &types.NodeClaimed{Type: "node_claimed", Node: b, Key: g3, Inputs: g.claimed(a).Inputs})); err == nil {
		t.Fatal("a forged claim at generation 3 was admitted")
	}
	// A replayed invalidation is refused.
	if err := g.do(forge(NodeInvalidated, string(a), prjID, &types.NodeInvalidated{Type: "node_invalidated", Node: a, Key: *g.state(a).Key})); err == nil {
		t.Fatal("a replayed invalidation was admitted")
	}
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return event(NodeClaimed, string(b), prjID, &types.NodeClaimed{Type: "node_claimed", Node: b, Key: *g.state(a).Key, Inputs: g.claimed(a).Inputs})
	}); err == nil {
		t.Fatal("another gate claimed the invalidated gate's new key")
	}
}

func (g graphStore) newNodeLike(n types.NodeID) types.Node {
	rec, _ := view[nodeRecord](g, NodeProjection, string(n))
	return rec.Recorded.Node
}

// Every node kind records exactly the result kinds of its row, and a kind
// without one records none.
func TestProducesTable(t *testing.T) {
	want := map[types.NodeKind][]types.ResultKind{
		"plan": {"plan"}, "build": {"patch", "partial_patch"}, "gate": {"gate_result"}, "runtime_verify": {"gate_result"},
		"integration": {"tree", "package"}, "evidence": {"evidence"}, "review": {"review"}, "carry_over": {"carry_over_attestation"},
		"merge": {"merge_result"}, "accept": nil, "closure": nil, "deploy": nil, "record": nil, "phase_gate": nil,
	}
	results := []types.ResultKind{"plan", "patch", "partial_patch", "tree", "package", "gate_result", "review", "evidence", "carry_over_attestation", "merge_result"}
	for nk, kinds := range want {
		for _, rk := range results {
			if got := slices.Contains(produces[nk], rk); got != slices.Contains(kinds, rk) {
				t.Errorf("%s records %s: %t", nk, rk, got)
			}
		}
	}
	if len(want) != 14 {
		t.Fatal("not every node kind is in the table")
	}
}

// A reason deprecated after a result was recorded still replays: the fold
// reads results as a reader (CheckRead), while a writer may not emit it.
func TestDeprecatedReasonsReplay(t *testing.T) {
	g := openStore(t)
	a := g.record(g.newNode(types.NodeKindBuild))
	g.must(g.claimOf(a))
	r := g.result(a, true)
	copy, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	entry := copy.Reasons[r.Reason.String()]
	since := "0.2.0"
	entry.Deprecated = &since
	copy.Reasons[r.Reason.String()] = entry
	registry = func() (*vocab.Registry, error) { return copy, nil }
	t.Cleanup(func() { registry = vocab.Load })
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return RecordResult(tx, types.ResultRecorded{Node: a, Result: unrecorded(t, *g.state(a).Key)})
	}); err == nil {
		t.Fatal("a writer recorded a deprecated reason")
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

// Every projected node state is a valid records/node-state.
func TestInvalidNodeStatesAreNotStored(t *testing.T) {
	g := openStore(t)
	r := g.newNode(types.NodeKindBuild)
	bad := types.NodeState{Node: r.Node.ID, State: types.NodeStateStateHeld, WaitingOn: []types.Name{}, Holds: []types.NodeHold{}}
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return nil, putNode(tx, nodeRecord{Recorded: r, State: bad})
	}); err == nil {
		t.Fatal("a held state with no hold was stored")
	}
}

func unrecorded(t *testing.T, key types.ComputationKey) types.Result {
	r, err := Build(key, BuildFacts{Submitted: true, Reported: types.OutcomeFound, Diff: diff, Project: prjID})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func withKey(r types.Result, key types.Digest) types.Result { r.Computation = key; return r }

func withKind(r types.Result, k types.ResultKind) types.Result { r.Kind = k; return r }

func withReason(r types.Result, reason vocab.Reason) types.Result { r.Reason = reason; return r }

func ptr[T any](v T) *T { return &v }
