// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/owedanswers"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

func reuseOf(n types.NodeID, res types.ResultID) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return Reuse(tx, n, withRequired(tx, n, map[string]Resolved{"plan": {Result: res}}), owedanswers.Authority{})
	}
}

// A memoizable instance refused its claim completes by reuse of the
// holder's selected result, and a dependant takes it as its producer's.
// The reused node is not invalidated on its own; the holder's invalidation
// releases it in the same command, so the dependants of both wait and
// nothing serves the old result. A rebuild reaches the same state.
func TestReuse(t *testing.T) {
	g := openStore(t)
	holder, r := g.gated(g.receiptBlob(treeSubject(treeA, demandA), nil))
	b := g.record(g.nodeLikeGate(holder))
	var held ErrKeyClaimed
	if err := g.do(g.claimOf(b)); !errors.As(err, &held) || held.Node != holder {
		t.Fatalf("b's claim: %v", err)
	}
	g.must(reuseOf(b, g.planRes))
	st := g.state(b)
	if st.State != types.NodeStateStateCompleted || *st.Result != r.ID || *st.Key != *g.state(holder).Key || len(g.claimed(b).Inputs) != 1 {
		t.Fatalf("reused: %+v, claim %+v", st, g.claimed(b))
	}
	onHolder, onB := g.reviewOf(holder), g.reviewOf(b)
	for _, d := range []types.NodeID{onHolder, onB} {
		if res, miss := g.resolve(d, gateOn(treeA, demandA), at); len(miss) != 0 || res["gate"].Result != r.ID {
			t.Fatalf("a dependant: %v %+v", res, miss)
		}
	}
	var reused ErrReused
	if err := g.do(invalidate(b)); !errors.As(err, &reused) || reused.Holder != holder {
		t.Fatalf("b invalidated on its own: %v", err)
	}
	g.must(invalidate(holder))
	if st := g.state(b); st.State != types.NodeStateStateWaiting || st.Key != nil || st.Result != nil || g.count(ReuseReleased) != 1 {
		t.Fatalf("b after its holder's invalidation: %+v", st)
	}
	if _, claimed := view[claim](g, ClaimProjection, string(b)); claimed {
		t.Fatal("b keeps its reuse's claim")
	}
	for _, d := range []types.NodeID{onHolder, onB} {
		if !g.waits(d, gateOn(treeA, demandA)) {
			t.Errorf("a dependant resolves the invalidated result")
		}
		if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return claimAny(tx, d, map[string]Resolved{"gate": {Result: r.ID}}, 0)
		}); err == nil {
			t.Error("a dependant claims the invalidated result")
		}
	}
	// At the new generation b reuses again, and the holder's next
	// invalidation releases it again.
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return claimAny(tx, b, map[string]Resolved{"plan": {Result: g.planRes}}, 0)
	}); !errors.As(err, &held) || held.Node != holder {
		t.Fatalf("b's claim at generation 2: %v", err)
	}
	r2 := g.selectGate(holder)
	g.must(reuseOf(b, g.planRes))
	if st := g.state(b); *st.Result != r2.ID || st.Key.Generation != 2 {
		t.Fatalf("b's reuse at generation 2: %+v", st)
	}
	g.must(invalidate(holder))
	if st := g.state(b); st.State != types.NodeStateStateWaiting || g.count(ReuseReleased) != 2 || g.state(holder).Key.Generation != 3 {
		t.Fatalf("the second release: %+v", st)
	}
	diffs, err := g.s.RebuildProjections(context.Background())
	if err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

func invalidate(n types.NodeID) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Invalidate(tx, n) }
}

// selectGate records and selects a result of n's kind and current key,
// holding a fresh receipt.
func (g graphStore) selectGate(n types.NodeID) types.Result {
	g.t.Helper()
	r := unrecorded(g.t, *g.state(n).Key)
	d := g.receiptBlob(treeSubject(treeA, demandA), nil)
	if g.recordOf(n).Node.Kind == types.NodeKindEvidence { // an evidence node holds its own instrument's receipt
		d = g.walkReceipt(walkContract())
	}
	r.Kind, r.Outcome, r.Reason, r.Detail, r.Conclusions = produces[g.recordOf(n).Node.Kind][0], types.OutcomeEmpty, vocab.ReasonCompletedClean, "", ptr(g.conclusions[d])
	r.Value = []types.Ref{g.sealed(types.RefTypeReceipt, rcpID, d)}
	rr := g.published(n, r)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return recordResult(tx, rr, g.blobs) })
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return selectResult(tx, n, r.ID) })
	return r
}

// A log whose holder's invalidation released no reuse serves nothing
// through the reused node: its key is of a stale generation, so its
// dependants wait, a claim of its result is refused, and it is not
// invalidated on its own.
func TestStaleReuseServesNothing(t *testing.T) {
	g := openStore(t)
	holder, r := g.gated(g.receiptBlob(treeSubject(treeA, demandA), nil))
	b := g.record(g.nodeLikeGate(holder))
	g.must(reuseOf(b, g.planRes))
	onB := g.reviewOf(b)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := Invalidate(tx, holder)
		return evs[:1], err // no release
	})
	if st := g.state(b); st.State != types.NodeStateStateCompleted || !g.waits(onB, gateOn(treeA, demandA)) {
		t.Errorf("a stale reuse serves its result: %+v", st)
	}
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return claimAny(tx, onB, map[string]Resolved{"gate": {Result: r.ID}}, 0)
	}); err == nil {
		t.Error("a dependant claims a stale reuse's result")
	}
	var reused ErrReused
	if err := g.do(invalidate(b)); !errors.As(err, &reused) || reused.Holder != "" || !strings.Contains(err.Error(), "holder was invalidated") {
		t.Errorf("a stale reuse invalidated: %v", err)
	}
}

// nodeLikeGate is another gate with holder's params and inputs: its key.
func (g graphStore) nodeLikeGate(holder types.NodeID) types.NodeRecorded {
	r := g.newNode(types.NodeKindGate)
	r.Node.Params = g.newNodeLike(holder).Params
	return r
}

// reviewNodeOn is a review taking gate's result as its "gate" input.
func (g graphStore) reviewNodeOn(gate types.NodeID) types.NodeRecorded {
	r := g.nodeOn(types.NodeKindReview, gate)
	r.Node.Inputs[0].Name = "gate"
	r.Node.Inputs[0].Source.Variant.(*types.ResultSource).ResultKind = types.ResultKindGateResult
	return r
}

// The fold refuses a reuse its state does not derive.
func TestReuseRefusals(t *testing.T) {
	g := openStore(t)
	holder, _ := g.gated("")
	key := *g.state(holder).Key
	in := g.claimed(holder).Inputs
	b := g.record(g.nodeLikeGate(holder))
	held := g.record(g.nodeLikeGate(holder))
	g.must(raised(held, awaitingInput(t), nil))
	build := g.record(g.newNode(types.NodeKindBuild))
	busy := g.record(g.newNode(types.NodeKindGate)) // holds its key, claimed, not completed
	g.must(g.claimOf(busy))
	waiter := g.record(g.newNode(types.NodeKindGate))
	other, _ := g.gated("") // another key
	elsewhere := g.nodeLikeGate(holder)
	elsewhere.Task = tskB
	far := g.record(elsewhere)
	reuse := func(subject string, project types.ProjectID, node, holder types.NodeID, k types.ComputationKey, items []types.ClaimedInput) logic {
		return forge(NodeReused, subject, project, &types.NodeReused{Type: "node_reused", Node: node, Holder: holder, Key: k, Inputs: items})
	}
	k2 := must(Key(g.newNodeLike(b), resolutions(in), 2))
	bk := must(Key(g.newNodeLike(build), resolutions(in), 1))
	for name, fn := range map[string]logic{
		"a reuse under another subject":    reuse(string(holder), prjID, b, holder, key, in),
		"a reuse under node.claimed":       forge(NodeClaimed, string(b), prjID, &types.NodeReused{Type: "node_reused", Node: b, Holder: holder, Key: key, Inputs: in}),
		"a reuse in another project":       reuse(string(b), prjB, b, holder, key, in),
		"a reuse of a held instance":       reuse(string(held), prjID, held, holder, key, in),
		"a reuse of a completed node":      reuse(string(holder), prjID, holder, holder, key, in),
		"a reuse of a build":               reuse(string(build), prjID, build, holder, bk, in),
		"a reuse at another generation":    reuse(string(b), prjID, b, holder, k2, in),
		"a reuse from other inputs":        reuse(string(b), prjID, b, holder, key, []types.ClaimedInput{{Name: "plan", Result: ptr(types.ResultID(resB))}}),
		"a reuse with no inputs":           reuse(string(b), prjID, b, holder, key, []types.ClaimedInput{}),
		"a reuse of a holder not holding":  reuse(string(b), prjID, b, other, key, in),
		"a reuse of an unfinished holder":  reuse(string(waiter), prjID, waiter, busy, *g.state(busy).Key, in),
		"a reuse of another task's holder": reuse(string(far), prjID, far, holder, key, in),
	} {
		if err := g.do(fn); err == nil || !strings.Contains(err.Error(), "graph: event") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := g.do(reuseOf(far, g.planRes)); err == nil {
		t.Error("a reuse with no holder in the task")
	}
	// The inputs must still be selected: after the plan's invalidation, its
	// old result no longer serves a reuse, though the key still matches.
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Invalidate(tx, g.plan) })
	if err := g.do(reuse(string(b), prjID, b, holder, key, in)); err == nil || !strings.Contains(err.Error(), "graph: event") {
		t.Errorf("a reuse of unselected inputs: %v", err)
	}
}

// The fold refuses a reuse naming another reuser as its holder,
// a reused node's own invalidation, and a release its state does not
// derive: of a node not bound to the key, before the holder's invalidation
// or twice.
func TestReuseReleaseRefusals(t *testing.T) {
	g := openStore(t)
	holder, _ := g.gated("")
	key := *g.state(holder).Key
	in := g.claimed(holder).Inputs
	k2 := must(Key(g.newNodeLike(holder), resolutions(in), 2))
	b := g.record(g.nodeLikeGate(holder))
	reuse := func(subject string, project types.ProjectID, node, holder types.NodeID, k types.ComputationKey, items []types.ClaimedInput) logic {
		return forge(NodeReused, subject, project, &types.NodeReused{Type: "node_reused", Node: node, Holder: holder, Key: k, Inputs: items})
	}
	// A reuse names the holder, never another node bound to its key.
	g.must(reuseOf(b, g.planRes))
	c := g.record(g.nodeLikeGate(holder))
	release := func(subject string, project types.ProjectID, node types.NodeID, k types.ComputationKey) logic {
		return forge(ReuseReleased, subject, project, &types.NodeReuseReleased{Type: "node_reuse_released", Node: node, Key: k})
	}
	for name, fn := range map[string]logic{
		"a reuse naming another reuser":     reuse(string(c), prjID, c, b, key, in),
		"a reused node's invalidation":      forge(NodeInvalidated, string(b), prjID, &types.NodeInvalidated{Type: "node_invalidated", Node: b, Key: k2}),
		"a release before the invalidation": release(string(b), prjID, b, key),
		"a release of the holder":           release(string(holder), prjID, holder, key),
	} {
		if err := g.do(fn); err == nil || !strings.Contains(err.Error(), "graph: event") {
			t.Errorf("%s: %v", name, err)
		}
	}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := Invalidate(tx, holder)
		return evs[:1], err // no release
	})
	for name, fn := range map[string]logic{
		"a release under another subject": release(string(holder), prjID, b, key),
		"a release under node.reused":     forge(NodeReused, string(b), prjID, &types.NodeReuseReleased{Type: "node_reuse_released", Node: b, Key: key}),
		"a release in another project":    release(string(b), prjB, b, key),
		"a release of a node not reusing": release(string(c), prjID, c, key),
		"a release of another key":        release(string(b), prjID, b, k2),
	} {
		if err := g.do(fn); err == nil || !strings.Contains(err.Error(), "graph: event") {
			t.Errorf("%s: %v", name, err)
		}
	}
	g.must(release(string(b), prjID, b, key))
	if err := g.do(release(string(b), prjID, b, key)); err == nil {
		t.Error("a reuse released twice")
	}
}

func (g graphStore) recordOf(n types.NodeID) types.NodeRecorded {
	rec, _ := view[nodeRecord](g, NodeProjection, string(n))
	return rec.Recorded
}

// timesOf is n's TimesOf, read in a view.
func (g graphStore) timesOf(n types.NodeID) (Times, error) {
	var t Times
	var terr error
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) error { t, terr = TimesOf(tx, n); return nil }); err != nil {
		g.t.Fatal(err)
	}
	return t, terr
}

// Astra #432 H2: a node's times are checked against its own evidence. A
// node completed by reuse is stamped claimed and selected at its reuse (it
// took no time of its own), and reads; the holder, claimed and selected,
// reads both. With the holder's times row gone (damage), its claim and
// selected result are evidence of times it lacks: ErrTimesMissing, never
// read as a node never claimed.
func TestNodeTimesAgainstTheirEvidence(t *testing.T) {
	g := openStore(t)
	holder, _ := g.gated(g.receiptBlob(treeSubject(treeA, demandA), nil))
	b := g.record(g.nodeLikeGate(holder))
	var held ErrKeyClaimed
	if err := g.do(g.claimOf(b)); !errors.As(err, &held) {
		t.Fatalf("b's claim: %v", err)
	}
	g.must(reuseOf(b, g.planRes))
	if tb, err := g.timesOf(b); err != nil || tb.Claimed.IsZero() || !tb.Claimed.Equal(tb.Selected) {
		t.Errorf("the reused node's times: %+v %v", tb, err)
	}
	if th, err := g.timesOf(holder); err != nil || th.Claimed.IsZero() || th.Selected.IsZero() {
		t.Errorf("the holder's times: %+v %v", th, err)
	}
	for _, half := range []string{`{"claimed":"","selected":"2026-10-24T00:00:00Z"}`, `{"claimed":"2026-10-24T00:00:00Z","selected":""}`, ""} {
		g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			if half == "" {
				return nil, tx.Delete(TimesProjection, string(holder))
			}
			return nil, tx.Put(TimesProjection, string(holder), json.RawMessage(half))
		})
		if _, err := g.timesOf(holder); !errors.Is(err, ErrTimesMissing) {
			t.Errorf("the holder's times %q: %v", half, err)
		}
	}
}
