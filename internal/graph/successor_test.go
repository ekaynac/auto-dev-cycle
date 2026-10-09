// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/owedanswers"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const aprC = "apr_01m3250vm7f6ktw39dszqnyddr"

// asked records owed as asked by asker, answered by approval (nil: not yet).
func asked(owed types.OwedInputID, asker types.NodeID, approval *types.ApprovalID) logic {
	return owedAnswer(owed, func(a *owedanswers.Answer) { a.Asker, a.Approval = asker, approval })
}

// planInput is an input named name taking the store's plan result.
func (g graphStore) planInput(name types.Name) types.InputSpec {
	return types.InputSpec{Name: name, Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: g.plan, ResultKind: types.ResultKindPlan}}}
}

// successor records pred's successor with added, returning its id.
func (g graphStore) successor(pred types.NodeID, added ...types.InputSpec) (types.NodeID, error) {
	var id types.NodeID
	err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := Successor(tx, pred, added, owedanswers.Authority{})
		if len(evs) > 0 {
			id = types.NodeID(evs[len(evs)-1].Subject) // node.recorded comes last
		}
		return evs, err
	})
	return id, err
}

// A successor is the one round n+1 of its predecessor, with its definition,
// params, requirements, effect and inputs, and the new ones; it answers only
// what its lineage owed, at the current revision, and may move an inherited
// answer to a newer revision. The answer re-keys it; the review that asked
// never takes it.
func TestSuccessor(t *testing.T) {
	g := openStore(t)
	gate, _ := g.gated(g.receiptBlob(treeSubject(treeA, demandA), nil))
	review := g.reviewOf(gate)
	g.must(asked(ownID, review, ptr(types.ApprovalID(aprA))))
	g.must(asked(ownB, g.plan, ptr(types.ApprovalID(aprB)))) // owed by another node
	var notOwed ErrNotOwed
	for name, in := range map[string][]types.InputSpec{
		"an answer owed by another node": {owes("data", ownB)},
		"an answer at another revision":  {{Name: "ruling", Source: types.InputSpecSource{Variant: &types.AnswerSource{Type: "answer", OwedInput: ownID, Revision: 3}}}},
	} {
		if _, err := g.successor(review, in...); !errors.As(err, &notOwed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, in := range map[string][]types.InputSpec{
		"an inherited name":  {takes("gate", gate)},
		"a name added twice": {takes("x", gate), g.planInput("x")},
	} {
		if _, err := g.successor(review, in...); err == nil || !strings.Contains(err.Error(), "twice") {
			t.Errorf("%s: %v", name, err)
		}
	}
	next, err := g.successor(review, owes("ruling", ownID))
	if err != nil {
		t.Fatal(err)
	}
	pred, succ := g.recordOf(review), g.recordOf(next)
	if succ.Node.Round != 2 || *succ.Predecessor != review || succ.Node.Kind != pred.Node.Kind || string(succ.Node.Params) != string(pred.Node.Params) ||
		succ.Node.DefinitionVersion != pred.Node.DefinitionVersion || !reflect.DeepEqual(succ.Node.Requires, pred.Node.Requires) || succ.Node.Effect != pred.Node.Effect ||
		len(succ.Node.Inputs) != 3 || !reflect.DeepEqual(succ.Node.Inputs[:2], pred.Node.Inputs) || succ.Node.Inputs[2].Name != "ruling" {
		t.Fatalf("the successor: %+v", succ)
	}
	if _, err := g.successor(review, g.planInput("repair")); err == nil || !strings.Contains(err.Error(), "has a successor") {
		t.Errorf("a fork: %v", err)
	}
	env := g.env(gatesOn(treeA, demandA))
	g.must(evaluate(env, review, next))
	if *g.state(review).Key == *g.state(next).Key || len(g.claimed(review).Inputs) != 2 {
		t.Fatal("the answer did not re-key the successor, or reached the review that asked")
	}
	third, err := g.successor(next, g.planInput("repair")) // an inherited answer, given
	if err != nil || g.recordOf(third).Node.Round != 3 {
		t.Fatalf("a third round: %v", err)
	}
	// The question changes: a later round answers the new revision of what
	// round 1 asked, in place of the inherited answer.
	g.must(owedAnswer(ownID, func(a *owedanswers.Answer) { a.Revision, a.Approval = 3, ptr(types.ApprovalID(aprC)) }))
	newer := types.InputSpec{Name: "ruling", Source: types.InputSpecSource{Variant: &types.AnswerSource{Type: "answer", OwedInput: ownID, Revision: 3}}}
	fourth, err := g.successor(third, newer)
	if err != nil || !reflect.DeepEqual(g.recordOf(fourth).Node.Inputs[2], newer) {
		t.Fatalf("a newer revision: %v", err)
	}
	g.must(evaluate(env, fourth))
	if g.state(fourth).State != types.NodeStateStateClaimed {
		t.Fatalf("the round answering the newer revision: %+v", g.state(fourth))
	}
	// An inherited answer moves only to a newer revision of its own owed
	// input: not to another owed input's, nor back to an older revision.
	const ownC = "own_01m3250vm7f6ktw39dszqnyddr"
	g.must(owedAnswer(ownC, func(a *owedanswers.Answer) {
		a.Asker, a.Revision, a.Approval = review, 4, ptr(types.ApprovalID("apr_01m3250vm7f6ktw39dszqnyda"))
	}))
	if _, err := g.successor(fourth, types.InputSpec{Name: "ruling", Source: types.InputSpecSource{Variant: &types.AnswerSource{Type: "answer", OwedInput: ownC, Revision: 4}}}); err == nil {
		t.Error("an inherited answer moved to another owed input")
	}
	older := g.recordOf(fourth)
	older.Node.ID, older.Node.Round, older.Predecessor = g.newNode(types.NodeKindReview).Node.ID, older.Node.Round+1, &fourth
	older.Node.Inputs = slices.Clone(older.Node.Inputs)
	older.Node.Inputs[2] = owes("ruling", ownID) // revision 2
	if err := g.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return recordAny(older) }); err == nil {
		t.Error("an inherited answer moved back to an older revision")
	}
	if diffs, err := g.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// The node.recorded fold refuses an answer on a first round, a successor
// that changes its predecessor's definition, params, requirements, effect
// or inputs, and a fork; an answer its lineage never asked for, recorded
// directly or inherited, never resolves.
func TestSuccessorLineage(t *testing.T) {
	g := openStore(t)
	review := g.reviewOf(g.plan)
	g.must(asked(ownID, review, nil))                        // what the lineage owes
	g.must(asked(ownB, g.plan, ptr(types.ApprovalID(aprB)))) // another node's
	first := g.newNode(types.NodeKindReview)
	first.Node.Inputs = append(first.Node.Inputs, owes("data", ownB))
	recordSuccessor := func(edit func(*types.Node)) (types.NodeID, error) {
		n := g.recordOf(review)
		n.Node.ID, n.Node.Round, n.Predecessor = g.newNode(types.NodeKindReview).Node.ID, 2, &review
		edit(&n.Node)
		return n.Node.ID, g.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return recordAny(n) })
	}
	refused := func(_ types.NodeID, err error) error { return err }
	for name, err := range map[string]error{
		"an answer on a first round":   g.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return recordAny(first) }),
		"another definition version":   refused(recordSuccessor(func(n *types.Node) { n.DefinitionVersion = "2" })),
		"other params":                 refused(recordSuccessor(func(n *types.Node) { n.Params = reviewParams(exeB) })),
		"other requirements":           refused(recordSuccessor(func(n *types.Node) { n.Requires.Resources = []types.ResourceGroupKind{"evidence_target"} })),
		"inputs not the predecessor's": refused(recordSuccessor(func(n *types.Node) { n.Inputs = n.Inputs[1:] })),
	} {
		if err == nil || !strings.Contains(err.Error(), "graph: event") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A record node's effect is notification or none: a successor keeps it.
	notify := types.Name("notification")
	rec := g.record(g.newNode(types.NodeKindRecord))
	quiet := g.recordOf(rec)
	quiet.Node.ID, quiet.Node.Round, quiet.Predecessor, quiet.Node.Effect = g.newNode(types.NodeKindRecord).Node.ID, 2, &rec, &notify
	if err := g.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return Record(quiet) }); err == nil || !strings.Contains(err.Error(), "graph: event") {
		t.Errorf("an effect: %v", err)
	}
	foreign, err := recordSuccessor(func(n *types.Node) { n.Inputs = append(n.Inputs, owes("data", ownB)) })
	if err != nil {
		t.Fatal(err)
	}
	if err := refused(recordSuccessor(func(*types.Node) {})); err == nil || !strings.Contains(err.Error(), "has a successor") {
		t.Errorf("a fork: %v", err)
	}
	var notOwed ErrNotOwed
	if err := g.do(evaluate(g.env(noRequirement), foreign)); !errors.As(err, &notOwed) {
		t.Errorf("a foreign answer recorded directly: %v", err)
	}
	third, err := g.successor(foreign, g.planInput("x"))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.do(evaluate(g.env(noRequirement), third)); !errors.As(err, &notOwed) {
		t.Errorf("an inherited foreign answer: %v", err)
	}
	if diffs, err := g.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// A successor is refused while its predecessor holds anything but a wait
// for an answer it declares and the authority has given, which it clears in
// the same command; an inherited answer not given yet refuses the round.
func TestSuccessorHolds(t *testing.T) {
	g := openStore(t)
	held := g.reviewOf(g.plan)
	rail := newHold(t, "budget_rail", types.OwnerOperator, nil)
	g.must(raised(held, rail, nil))
	if _, err := g.successor(held, g.planInput("x")); err == nil || !strings.Contains(err.Error(), "G6b") {
		t.Errorf("a held predecessor: %v", err)
	}
	// The fold refuses it too, recorded directly, and retires only an
	// awaiting_input wait as revised.
	direct := g.recordOf(held)
	direct.Node.ID, direct.Node.Round, direct.Predecessor = g.newNode(types.NodeKindReview).Node.ID, 2, &held
	direct.Node.Inputs = append(slices.Clone(direct.Node.Inputs), g.planInput("x"))
	if err := g.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return recordAny(direct) }); err == nil || !strings.Contains(err.Error(), "G6b") {
		t.Errorf("a held predecessor's successor recorded directly: %v", err)
	}
	if err := g.do(forge(HoldCleared, string(held), prjID, &types.HoldCleared{Type: "hold_cleared", Node: held, Hold: rail.ID,
		Cause: types.HoldClearedCause{Variant: &types.Retired{Type: "retired", Reason: types.RetiredReasonRevised}}})); err == nil || !strings.Contains(err.Error(), "graph: event") {
		t.Errorf("a budget rail retired as revised: %v", err)
	}
	g.must(answer(ownID, actID, nil))
	waiting := g.asking(owes("ruling", ownID))
	g.must(evaluate(g.env(noRequirement), waiting))
	if h := g.state(waiting).Holds; len(h) != 1 || h[0].Hold != "awaiting_input" {
		t.Fatalf("the wait: %+v", h)
	}
	if _, err := g.successor(waiting, g.planInput("x")); err == nil || !strings.Contains(err.Error(), "not given yet") {
		t.Errorf("an inherited answer not given: %v", err)
	}
	g.must(answer(ownID, actID, ptr(types.ApprovalID(aprA))))
	cleared := g.count(HoldCleared)
	if _, err := g.successor(waiting, g.planInput("x")); err != nil || len(g.state(waiting).Holds) != 0 || g.count(HoldCleared) != cleared+1 {
		t.Fatalf("a successor answering the wait: %v %+v", err, g.state(waiting))
	}
	if diffs, err := g.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// The controller context is the claimed inputs' refs, in declared order:
// a result's value, and the Ref to the exact answer the claim fixed. The
// review that asked never sees the answer; an unclaimed node, a claim
// naming no recorded result and a claimed input resolved to nothing have
// no context.
func TestContext(t *testing.T) {
	g := openStore(t)
	gate, r := g.gated(g.receiptBlob(treeSubject(treeA, demandA), nil))
	review := g.reviewOf(gate)
	g.must(asked(ownID, review, ptr(types.ApprovalID(aprA))))
	g.must(asked(ownB, g.plan, ptr(types.ApprovalID(aprB)))) // another owed input's answer
	next, err := g.successor(review, owes("ruling", ownID))
	if err != nil {
		t.Fatal(err)
	}
	g.must(evaluate(g.env(gatesOn(treeA, demandA)), review, next))
	context := func(n types.NodeID) ([]types.Ref, error) {
		var refs []types.Ref
		err := g.s.View(context.Background(), func(tx *eventstore.Tx) error {
			c, err := Context(tx, n, Env{Authority: owedanswers.Authority{}})
			refs = c.Included
			return err
		})
		return refs, err
	}
	asking, err := context(review)
	if err != nil || !reflect.DeepEqual(asking, r.Value) {
		t.Fatalf("the asking review's context: %v %+v", err, asking)
	}
	art, _ := json.Marshal(owedanswers.Artifact{Approval: aprA, Owed: ownID, Revision: 2, Project: prjID})
	want := g.sealed(types.RefTypeApproval, aprA, types.Digest(must(eventstore.ContentDigest(art))))
	answered, err := context(next)
	if err != nil || !reflect.DeepEqual(answered, append(append([]types.Ref{}, r.Value...), want)) {
		t.Fatalf("the successor's context: %v %+v", err, answered)
	}
	if _, err := context(g.record(g.newNode(types.NodeKindBuild))); err == nil {
		t.Error("an unclaimed node's context")
	}
	for name, in := range map[string]types.ClaimedInput{
		"no recorded result":  {Name: "gate", Result: ptr(types.ResultID(resB))},
		"resolved to nothing": {Name: "gate"},
	} {
		n := g.reviewOf(gate)
		g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return nil, put(tx, ClaimProjection, string(n), claim{Key: *g.state(review).Key, Inputs: []types.ClaimedInput{in}})
		})
		if _, err := context(n); err == nil {
			t.Errorf("a claim with %s", name)
		}
	}
}

// Equivalence is the review's own decision on a declared gate or evidence
// edge: the review stands on a claim that used used, fresh is its producer's
// selected result, and both receipts conclude the same, about the same
// subject, by the same instrument, in the same project, under the same
// restrictions.
func TestEquivalent(t *testing.T) {
	g := openStore(t)
	gate, usedResult := g.gated(g.receiptBlob(treeSubject(treeA, demandA), nil))
	used := usedResult.ID
	review := g.reviewOf(gate)
	env := g.env(gatesOn(treeA, demandA))
	// An integration ran on the gate too (a carry_over is the queue's, G7b);
	// another review is claimed, not run.
	carry := g.newNode(types.NodeKindIntegration)
	carry.Node.Inputs[0].Name, carry.Node.Inputs[0].Source.Variant.(*types.ResultSource).ResultKind = "gate", types.ResultKindGateResult
	carry.Node.Inputs[0].Source.Variant.(*types.ResultSource).Producer = gate
	carried := g.record(carry)
	pendingNode := g.nodeOn(types.NodeKindReview, gate)
	pendingNode.Node.Params, pendingNode.Node.Inputs[0].Name = reviewOf(types.Name(pendingNode.Node.ID), exeB), "gate"
	pendingNode.Node.Inputs[0].Source.Variant.(*types.ResultSource).ResultKind = types.ResultKindGateResult
	pending := g.record(pendingNode)
	g.must(evaluate(env, review, carried, pending))
	g.must(commit(review, g.reviewResult(review), env))
	attestation := unrecorded(t, *g.state(carried).Key)
	attestation.Kind, attestation.Outcome, attestation.Reason, attestation.Detail = types.ResultKindTree, types.OutcomeEmpty, vocab.ReasonCompletedClean, ""
	attestation.Value = []types.Ref{g.sealed(types.RefTypeGitTree, "git:"+strings.Repeat("c", 40), "sha256:"+types.Digest(strings.Repeat("c", 64)))}
	g.must(commit(carried, attestation, env))
	equivalent := func(n types.NodeID, name types.Name, u, f types.ResultID) (got bool, err error) {
		err = g.s.View(context.Background(), func(tx *eventstore.Tx) (err error) {
			got, err = Equivalent(tx, n, name, u, f, g.blobs)
			return err
		})
		return got, err
	}
	// reselect makes the gate's next result the one holding doc's receipt,
	// edited.
	// A nil doc holds no receipt.
	reselect := func(doc map[string]any, edit func(*types.Ref)) types.ResultID {
		g.must(invalidate(gate))
		r := unrecorded(t, *g.state(gate).Key)
		r.Kind, r.Outcome, r.Reason, r.Detail, r.Conclusions, r.Value = types.ResultKindGateResult, types.OutcomeEmpty, vocab.ReasonCompletedClean, "", ptr(conclusions), []types.Ref{}
		if doc != nil {
			d := g.derived(doc)
			ref := g.sealed(types.RefTypeReceipt, rcpID, d)
			if edit != nil {
				edit(&ref)
				ref = must(knowledge.Seal(ref))
			}
			r.Conclusions, r.Value = ptr(g.conclusions[d]), []types.Ref{ref}
		}
		g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return recordResult(tx, types.ResultRecorded{Node: gate, Result: r}, g.blobs)
		})
		g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return selectResult(tx, gate, r.ID) })
		return r.ID
	}
	again := receiptDoc(treeSubject(treeA, demandA), nil)
	failing := receiptDoc(treeSubject(treeA, demandA), nil)
	failing["predicates"] = []any{map[string]any{"name": "tests", "outcome": "found", "reason": "completed_with_findings", "detail": "1 failed", "evidence": []any{}}}
	newer := receiptDoc(treeSubject(treeA, demandA), nil)
	newer["instrument"] = map[string]any{"name": "go-test", "version": "2.0.0", "digest": instrumentDigest}
	restricted := []types.Restriction{{Project: prjID, PolicyDigest: instrumentDigest, Floor: "own", Routing: "verified", AllowedDestinations: []string{}}}
	var earlier types.ResultID
	for _, c := range []struct {
		name string
		doc  map[string]any
		edit func(*types.Ref)
		want bool
	}{
		{"the same conclusions", again, nil, true},
		{"a changed step outcome", failing, nil, false},
		{"another subject", receiptDoc(treeSubject(treeB, demandA), nil), nil, false},
		{"changed restrictions", again, func(r *types.Ref) { r.Restrictions = restricted }, false},
		{"another instrument", newer, nil, false},
		{"another project", again, func(r *types.Ref) { r.Scope.Project = ptr(types.ProjectID(prjB)) }, false},
	} {
		earlier = reselect(c.doc, c.edit)
		if got, err := equivalent(review, "gate", used, earlier); err != nil || got != c.want {
			t.Errorf("%s: %v %v", c.name, got, err)
		}
	}
	fresh := reselect(again, nil)
	reviewed, rr := g.completedWith(types.NodeKindReview, types.ResultKindReview, []types.Ref{g.sealed(types.RefTypeReceipt, rcpID, g.receiptBlob(treeSubject(treeA, demandA), nil))})
	onReview := g.consumerOf(reviewed, types.ResultKindReview)
	g.must(evaluate(g.env(noRequirement), onReview))
	g.must(commit(onReview, g.reviewResult(onReview), g.env(noRequirement)))
	refused := func(name string, n types.NodeID, in types.Name, u, f types.ResultID) {
		t.Helper()
		if _, err := equivalent(n, in, u, f); err == nil {
			t.Errorf("%s gave a decision", name)
		}
	}
	refused("a review's plan input", g.record(g.newNode(types.NodeKindReview)), "plan", used, fresh)
	refused("an integration that used the result", carried, "gate", used, fresh)
	refused("a review's review input", onReview, "gate", rr.ID, rr.ID)
	refused("an undeclared input", review, "ruling", used, fresh)
	refused("a review that never ran", g.reviewOf(gate), "gate", used, fresh)
	refused("a review claimed, not run", pending, "gate", used, fresh)
	refused("a result the review did not use", review, "gate", fresh, fresh)
	refused("a result no longer selected", review, "gate", used, earlier)
	refused("a result with no receipt", review, "gate", used, reselect(nil, nil))
	g.must(invalidate(gate))
	cnr := unrecorded(t, *g.state(gate).Key)
	cnr.Kind, cnr.Outcome, cnr.Reason, cnr.Detail, cnr.Completed, cnr.Value = types.ResultKindGateResult, types.OutcomeCouldNotRun, vocab.ReasonPreconditionFailed, "probe", false, []types.Ref{}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return recordResult(tx, types.ResultRecorded{Node: gate, Result: cnr}, g.blobs)
	})
	refused("an incomplete result", review, "gate", used, cnr.ID)
	g.must(invalidate(review))
	if _, err := equivalent(review, "gate", used, fresh); err == nil {
		t.Error("an invalidated review gave a decision")
	}
}

// failingOwed cannot tell what a node owes.
type failingOwed struct{ owedanswers.Authority }

func (failingOwed) Owed(*eventstore.Tx, types.NodeRecorded) ([]types.AnswerSource, error) {
	return []types.AnswerSource{{Type: "answer", OwedInput: ownID, Revision: 2}}, errors.New("the authority is unavailable") // an error with a list
}

// A successor's answer is refused when the authority cannot tell what the
// predecessor owed.
func TestSuccessorAuthorityError(t *testing.T) {
	g := openStore(t)
	review := g.asking(takes("gate", g.plan))
	g.must(asked(ownID, review, nil))
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return Successor(tx, review, []types.InputSpec{owes("ruling", ownID)}, failingOwed{})
	}); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("an authority error: %v", err)
	}
}

// answerAt answers owed at revision under name.
func answerAt(name string, owed types.OwedInputID, revision int64) types.InputSpec {
	return types.InputSpec{Name: types.Name(name), Source: types.InputSpecSource{Variant: &types.AnswerSource{Type: "answer", OwedInput: owed, Revision: revision}}}
}

// An owed input revised while a round awaits its answer leaves that round
// waiting, never failing the command: an unrelated goal still progresses,
// a round declaring the old revision raises no hold, and one claimed on it
// is refused at commit and in its context as authority_revoked. The
// successor answering the new revision retires the old wait as revised,
// before it is recorded, and claims.
func TestSuccessorRevisionBump(t *testing.T) {
	const ownD, aprD = types.OwedInputID("own_01m3250vm7f6ktw39dszqnyddr"), types.ApprovalID("apr_01m3250vm7f6ktw39dszqnyda")
	g := openStore(t)
	env := g.env(noRequirement)
	review := g.asking(g.planInput("plan"))
	g.must(asked(ownID, review, nil)) // revision 2, unanswered
	next, err := g.successor(review, owes("ruling", ownID))
	if err != nil {
		t.Fatal(err)
	}
	g.must(evaluate(env, next))
	wait := g.state(next).Holds
	if len(wait) != 1 || wait[0].SignalInstance != string(ownID)+"@2" {
		t.Fatalf("the wait: %+v", wait)
	}
	running := g.asking(g.planInput("plan")) // a round claimed on revision 2
	g.must(asked(ownB, running, ptr(types.ApprovalID(aprB))))
	onB, err := g.successor(running, owes("ruling", ownB))
	if err != nil {
		t.Fatal(err)
	}
	g.must(evaluate(env, onB))
	quiet := g.asking(g.planInput("plan")) // a round not evaluated yet
	g.must(asked(ownD, quiet, nil))
	onD, err := g.successor(quiet, owes("ruling", ownD))
	if err != nil {
		t.Fatal(err)
	}
	// Every question is revised.
	g.must(owedAnswer(ownID, func(a *owedanswers.Answer) { a.Revision, a.Approval = 3, ptr(types.ApprovalID(aprC)) }))
	g.must(owedAnswer(ownB, func(a *owedanswers.Answer) { a.Revision, a.Approval = 3, ptr(aprD) }))
	g.must(owedAnswer(ownD, func(a *owedanswers.Answer) { a.Revision = 3 }))
	other := g.asking(g.planInput("plan")) // an unrelated goal
	raisedBefore := g.count(HoldRaised)
	g.must(evaluate(env, next, onD, other))
	if g.state(other).State != types.NodeStateStateClaimed {
		t.Errorf("the unrelated goal: %+v", g.state(other))
	}
	if h := g.state(next).Holds; len(h) != 1 || h[0].ID != wait[0].ID || g.state(onD).State != types.NodeStateStateWaiting || g.count(HoldRaised) != raisedBefore {
		t.Errorf("a stale answer is a structural wait: %+v %+v", g.state(next), g.state(onD))
	}
	var revoked ErrRevalidated
	if err := g.do(commit(onB, g.reviewResult(onB), env)); !errors.As(err, &revoked) || revoked.Reason != vocab.ReasonAuthorityRevoked {
		t.Errorf("a commit on a stale answer: %v", err)
	}
	if _, err := g.contextOf(onB); !errors.As(err, &revoked) || revoked.Reason != vocab.ReasonAuthorityRevoked {
		t.Errorf("the context of a stale answer: %v", err)
	}
	cleared := g.count(HoldCleared)
	third, err := g.successor(next, answerAt("ruling", ownID, 3))
	if err != nil || len(g.state(next).Holds) != 0 || g.count(HoldCleared) != cleared+1 {
		t.Fatalf("the round answering revision 3: %v %+v", err, g.state(next))
	}
	evs := g.events(HoldCleared)
	var retired types.HoldCleared
	if err := json.Unmarshal(evs[len(evs)-1].Payload, &retired); err != nil {
		t.Fatal(err)
	}
	if c, ok := retired.Cause.Variant.(*types.Retired); !ok || c.Reason != types.RetiredReasonRevised || retired.Hold != wait[0].ID {
		t.Errorf("the old wait's release: %+v", retired)
	}
	g.must(evaluate(env, third))
	if g.state(third).State != types.NodeStateStateClaimed {
		t.Errorf("the round on revision 3: %+v", g.state(third))
	}
	if diffs, err := g.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// contextOf is n's controller context.
func (g graphStore) contextOf(n types.NodeID) ([]types.Ref, error) {
	var refs []types.Ref
	err := g.s.View(context.Background(), func(tx *eventstore.Tx) error {
		c, err := Context(tx, n, Env{Authority: owedanswers.Authority{}})
		refs = c.Included
		return err
	})
	return refs, err
}

// A ruling the lineage did not ask for never reaches the context, even of
// a node claimed through Claim, which does not check the lineage.
func TestContextLineage(t *testing.T) {
	g := openStore(t)
	g.must(asked(ownB, g.plan, ptr(types.ApprovalID(aprB)))) // owed by the plan
	asker := g.asking(g.planInput("plan"))
	g.must(asked(ownID, asker, nil)) // the lineage owes something else
	stray := g.recordOf(asker)
	stray.Node.ID, stray.Node.Round, stray.Predecessor = g.newNode(types.NodeKindReview).Node.ID, 2, &asker
	stray.Node.Inputs = append(slices.Clone(stray.Node.Inputs), answerAt("ruling", ownB, 2))
	n := g.record(stray)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return claimAny(tx, n, map[string]Resolved{"plan": {Result: g.planRes}, "ruling": {Approval: aprB, Revision: 2}}, 0)
	})
	var notOwed ErrNotOwed
	if refs, err := g.contextOf(n); !errors.As(err, &notOwed) || refs != nil {
		t.Errorf("a foreign ruling's context: %v %+v", err, refs)
	}
}
