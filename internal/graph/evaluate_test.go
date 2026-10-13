// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/owedanswers"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	ownB = "own_01m3250vnfeyrv67x6w0ddqr6a"
	actB = "act_01m3250vnfeyrv67x6w0ddqr6a"
)

// owedAnswer sets owed's recorded state at revision 2 by edit, keeping
// what edit leaves.
func owedAnswer(owed types.OwedInputID, edit func(*owedanswers.Answer)) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		a := owedanswers.Answer{Owed: owed, Project: prjID, Owner: actID, Revision: 2}
		raw, ok, err := tx.Get(owedanswers.Projection, string(owed))
		if err == nil && ok {
			err = json.Unmarshal(raw, &a)
		}
		if err != nil {
			return nil, err
		}
		edit(&a)
		return owedanswers.Record(a)
	}
}

// answer sets owed's answer (nil: unanswered or revoked), waiting on owner;
// who asked for it stays.
func answer(owed types.OwedInputID, owner types.ActorID, approval *types.ApprovalID) logic {
	return owedAnswer(owed, func(a *owedanswers.Answer) { a.Owner, a.Approval = owner, approval })
}

// gatesOn is gateOn for gate result inputs; the others need no receipt.
func gatesOn(tree string, demand types.Digest) Requirements {
	return func(n types.Node, in types.InputSpec) (*receipt.Requirement, error) {
		if src, ok := in.Source.Variant.(*types.ResultSource); !ok || src.ResultKind != types.ResultKindGateResult {
			return nil, nil
		}
		return gateOn(tree, demand)(n, in)
	}
}

func (g graphStore) env(need Requirements) Env {
	return Env{Need: need, Blobs: g.blobs, Authority: owedanswers.Authority{}, Families: reviewers}
}

func evaluate(env Env, goals ...types.NodeID) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Evaluate(tx, goals, env) }
}

func commit(n types.NodeID, r types.Result, env Env) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) { return nil, Commit(tx, n, r, env) }
}

// asking records a review, under a key of its own, with inputs. An answer
// is a successor's input: given answers, it records a round 1 review with
// the other inputs that asks for each owed input, and returns its round 2
// successor declaring the answers.
func (g graphStore) asking(inputs ...types.InputSpec) types.NodeID {
	ask := g.newNode(types.NodeKindReview)
	ask.Node.Params = reviewOf(types.Name(ask.Node.ID), exeA)
	ask.Node.Inputs = []types.InputSpec{requiredOn(g.defaultRequirement())}
	var answers []types.InputSpec
	for _, in := range inputs {
		if a, ok := in.Source.Variant.(*types.AnswerSource); ok {
			answers = append(answers, in)
			g.must(owedAnswer(a.OwedInput, func(*owedanswers.Answer) {})) // recorded, if it was not
		} else {
			ask.Node.Inputs = append(ask.Node.Inputs, in)
		}
	}
	n := g.record(ask)
	if len(answers) == 0 {
		return n
	}
	for _, in := range answers {
		g.must(owedAnswer(in.Source.Variant.(*types.AnswerSource).OwedInput, func(a *owedanswers.Answer) { a.Asker = n }))
	}
	next, err := g.successor(n, answers...)
	if err != nil {
		g.t.Fatal(err)
	}
	return next
}

func owes(name string, owed types.OwedInputID) types.InputSpec {
	return types.InputSpec{Name: types.Name(name), Source: types.InputSpecSource{Variant: &types.AnswerSource{Type: "answer", OwedInput: owed, Revision: 2}}}
}

func takes(name string, producer types.NodeID) types.InputSpec {
	return types.InputSpec{Name: types.Name(name), Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: producer, ResultKind: types.ResultKindGateResult}}}
}

// gateResult is a clean gate result of n's key holding a fresh receipt.
func (g graphStore) gateResult(n types.NodeID, until *string) types.Result {
	d := g.receiptBlob(treeSubject(treeA, demandA), until)
	r := unrecorded(g.t, *g.state(n).Key)
	r.Kind, r.Outcome, r.Reason, r.Detail, r.Conclusions = types.ResultKindGateResult, types.OutcomeEmpty, vocab.ReasonCompletedClean, "", ptr(g.conclusions[d])
	r.Value = []types.Ref{g.sealed(types.RefTypeReceipt, rcpID, d)}
	return r
}

// reviewResult is a clean review result of n's key.
func (g graphStore) reviewResult(n types.NodeID) types.Result {
	r := unrecorded(g.t, *g.state(n).Key)
	r.Kind, r.Outcome, r.Reason, r.Detail = types.ResultKindReview, types.OutcomeEmpty, vocab.ReasonCompletedClean, ""
	return r
}

// Evaluation claims producers first, each node once; a dependant waits
// structurally, recording nothing, until its producer completes.
func TestEvaluateProducersFirst(t *testing.T) {
	g := openStore(t)
	gate := g.newNode(types.NodeKindGate)
	gate.Node.Params = json.RawMessage(`{"node":"` + string(gate.Node.ID) + `"}`)
	gn := g.record(gate)
	a := g.reviewOf(gn)
	env := g.env(gatesOn(treeA, demandA))
	g.must(evaluate(env, a, a))
	if g.state(gn).State != types.NodeStateStateClaimed || g.count(NodeClaimed) != 3 || g.state(a).State != types.NodeStateStateWaiting {
		t.Fatalf("the first evaluation: gate %+v, review %+v", g.state(gn), g.state(a))
	}
	g.must(evaluate(env, a)) // the gate runs
	g.must(commit(gn, g.gateResult(gn, nil), env))
	g.must(evaluate(env, a))
	if g.state(a).State != types.NodeStateStateClaimed || g.count(NodeClaimed) != 4 { // the plan's, the requirement's, the gate's and the review's
		t.Fatalf("the dependant: %+v", g.state(a))
	}
}

// One command claims a memoizable key once; every other instance reuses a
// completed holder, in the same evaluation, also after a release.
func TestEvaluateOneClaimPerKey(t *testing.T) {
	g := openStore(t)
	first := g.record(g.newNode(types.NodeKindGate))
	second, third := g.record(g.nodeLikeGate(first)), g.record(g.nodeLikeGate(first))
	env := g.env(gatesOn(treeA, demandA))
	g.must(evaluate(env, first, second, third))
	if g.state(first).State != types.NodeStateStateClaimed || g.state(second).State != types.NodeStateStateWaiting || g.count(NodeClaimed) != 2 {
		t.Fatalf("one key three times: %+v %+v", g.state(first), g.state(second))
	}
	g.must(evaluate(env, second, third)) // the holder runs: wait
	reusedBoth := func(r types.Result) {
		t.Helper()
		g.must(evaluate(env, second, third))
		for _, n := range []types.NodeID{second, third} {
			if st := g.state(n); st.State != types.NodeStateStateCompleted || *st.Result != r.ID {
				t.Fatalf("the reuse: %+v", st)
			}
		}
	}
	reusedBoth(g.selectGate(first))
	g.must(invalidate(first)) // releases both
	reusedBoth(g.selectGate(first))
}

// A missing answer raises awaiting_input once per owed input, owned by its
// own grant holder, also behind a missing producer; an answer clears its own
// hold only, and the node is claimed once both are answered. A claimed node
// is not evaluated again; a node with another hold is not claimed. A
// rebuild agrees.
func TestEvaluateAwaitsAnswers(t *testing.T) {
	g := openStore(t)
	g.must(answer(ownID, actID, nil))
	g.must(answer(ownB, actB, nil))
	pending := g.record(g.newNode(types.NodeKindGate))
	n := g.asking(takes("gate", pending), owes("ruling", ownID), owes("data", ownB))
	env := g.env(gatesOn(treeA, demandA))
	g.must(evaluate(env, n))
	g.must(evaluate(env, n))
	holds := g.state(n).Holds
	if len(holds) != 2 || g.count(HoldCleared) != 0 || holds[0].SignalInstance != ownID+"@2" || *holds[0].OwnerRef != actID || *holds[1].OwnerRef != actB {
		t.Fatalf("the waits: %+v", holds)
	}
	g.must(answer(ownID, actID, ptr(types.ApprovalID(aprA))))
	g.must(evaluate(env, n))
	if st := g.state(n); len(st.Holds) != 1 || st.Holds[0].ID != holds[1].ID {
		t.Fatalf("one answer: %+v", st.Holds)
	}
	g.must(answer(ownB, actB, ptr(types.ApprovalID(aprB))))
	g.selectGate(pending)
	g.must(evaluate(env, n))
	if st := g.state(n); st.State != types.NodeStateStateClaimed || len(st.Holds) != 0 {
		t.Fatalf("the answered node: %+v", st)
	}
	g.must(answer(ownID, actID, nil))
	g.must(evaluate(env, n))
	if len(g.state(n).Holds) != 0 {
		t.Fatal("a claimed node awaits its answer again")
	}
	other := g.asking(owes("data", ownB))
	g.must(raised(other, newHold(t, "budget_rail", types.OwnerOperator, nil), nil))
	g.must(evaluate(env, other))
	if st := g.state(other); st.State != types.NodeStateStateHeld {
		t.Fatalf("a node with another hold: %+v", st)
	}
	if diffs, err := g.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// Commit re-validates in its transaction, in declared order: a revoked or
// replaced answer, a receipt that no longer applies and a producer
// re-selected since the claim each refuse the result; otherwise it records
// and selects, and only records a could_not_run one. It runs only in a
// command, on a claimed node.
func TestCommitRevalidates(t *testing.T) {
	g := openStore(t)
	g.must(answer(ownID, actID, ptr(types.ApprovalID(aprA))))
	g.must(answer(ownB, actID, ptr(types.ApprovalID(aprB))))
	gate, _ := g.gated(g.receiptBlob(treeSubject(treeA, demandA), nil))
	both := g.asking(takes("gate", gate), owes("ruling", ownID))
	asked := g.asking(owes("ruling", ownB))
	env := g.env(gatesOn(treeA, demandA))
	g.must(evaluate(env, both, asked))
	var stale ErrRevalidated
	for name, c := range map[string]struct {
		node   types.NodeID
		before logic
		env    Env
		input  types.Name
		reason vocab.Reason
	}{
		"a revoked answer":     {asked, answer(ownB, actID, nil), env, "ruling", vocab.ReasonAuthorityRevoked},
		"another approval":     {asked, answer(ownB, actID, ptr(types.ApprovalID(aprC))), env, "ruling", vocab.ReasonAuthorityRevoked},
		"another demand":       {both, answer(ownID, actID, ptr(types.ApprovalID(aprA))), g.env(gatesOn(treeA, demandB)), "gate", vocab.ReasonEvidenceInapplicable},
		"two inputs that fail": {both, answer(ownID, actID, nil), g.env(gatesOn(treeA, demandB)), "gate", vocab.ReasonEvidenceInapplicable},
	} {
		g.must(c.before)
		for range 5 { // declared order, every time
			if err := g.do(commit(c.node, g.reviewResult(c.node), c.env)); !errors.As(err, &stale) || stale.Reason != c.reason || stale.Input != c.input {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
	g.must(answer(ownB, actID, ptr(types.ApprovalID(aprB))))
	failed := g.reviewResult(asked)
	failed.Outcome, failed.Reason, failed.Detail, failed.Completed = types.OutcomeCouldNotRun, vocab.ReasonReceiptIncomplete, "no receipt", false
	if err := g.do(commit(asked, failed, env)); err != nil || g.state(asked).State != types.NodeStateStateClaimed {
		t.Fatalf("a could_not_run commit: %v %+v", err, g.state(asked))
	}
	r := g.reviewResult(asked)
	if err := g.do(commit(asked, r, env)); err != nil || *g.state(asked).Result != r.ID {
		t.Fatalf("the commit: %v %+v", err, g.state(asked))
	}
	g.must(invalidate(gate))
	g.selectGate(gate) // selected again, at the next key
	if err := g.do(commit(both, g.reviewResult(both), env)); !errors.As(err, &stale) || stale.Reason != vocab.ReasonStaleExpectedState {
		t.Errorf("a producer selected again since the claim: %v", err)
	}
	if err := g.do(commit(g.record(g.newNode(types.NodeKindBuild)), g.reviewResult(both), env)); err == nil || !strings.Contains(err.Error(), "is not claimed") {
		t.Errorf("a commit of a node with no claim: %v", err)
	}
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) error { return Commit(tx, both, g.reviewResult(both), env) }); err == nil || !strings.Contains(err.Error(), "a commit runs in a command") {
		t.Error("a commit from a view")
	}
}

// Result events built before a producer's invalidation and submitted after
// it are refused by the folds: the claim's inputs are no longer selected.
func TestBufferedResultRefused(t *testing.T) {
	g := openStore(t)
	gate, _ := g.gated(g.receiptBlob(treeSubject(treeA, demandA), nil))
	review := g.reviewOf(gate)
	g.must(evaluate(g.env(gatesOn(treeA, demandA)), review))
	r := g.reviewResult(review)
	var buffered []eventstore.Event
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := recordResult(tx, types.ResultRecorded{Node: review, Result: r}, g.blobs)
		buffered = evs
		return nil, err
	})
	g.must(invalidate(gate))
	if err := g.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return buffered, nil }); err == nil || !strings.Contains(err.Error(), "graph: event") {
		t.Fatalf("a buffered result after the invalidation: %v", err)
	}
	g.selectGate(gate)
	if err := g.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return buffered, nil }); err == nil {
		t.Fatal("a buffered result after the producer was selected again")
	}
	// A selection built while the claim held, submitted after the producer's
	// invalidation, is refused too.
	review = g.reviewOf(gate)
	g.must(evaluate(g.env(gatesOn(treeA, demandA)), review))
	r = g.reviewResult(review)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return recordResult(tx, types.ResultRecorded{Node: review, Result: r}, g.blobs)
	})
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := selectResult(tx, review, r.ID)
		buffered = evs
		return nil, err
	})
	g.must(invalidate(gate))
	if err := g.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return buffered, nil }); err == nil || !strings.Contains(err.Error(), "graph: event") {
		t.Fatalf("a buffered selection after the invalidation: %v", err)
	}
}

// An expired receipt is renewed by evaluation, through the holder of its
// key, and the dependants claim once the fresher result is selected.
func TestEvaluateRenews(t *testing.T) {
	g := openStore(t)
	holder, _ := g.gated(g.receiptBlob(treeSubject(treeA, demandA), &past))
	b := g.record(g.nodeLikeGate(holder))
	g.must(reuseOf(b, g.planRes))
	review, onHolder := g.reviewOf(b), g.reviewOf(holder)
	env := g.env(gatesOn(treeA, demandA))
	g.must(evaluate(env, review, onHolder)) // through the holder, once for both
	if g.count(NodeRenewed) != 1 || g.state(holder).State != types.NodeStateStateClaimed || g.state(review).State != types.NodeStateStateWaiting {
		t.Fatalf("the renewal: %+v %+v", g.state(holder), g.state(review))
	}
	g.must(evaluate(env, review)) // the holder runs
	g.must(commit(holder, g.gateResult(holder, &future), env))
	g.must(evaluate(env, review))
	if g.state(b).State != types.NodeStateStateCompleted {
		t.Fatalf("the reuse after the renewal: %+v", g.state(b))
	}
	g.must(evaluate(env, review))
	if g.state(review).State != types.NodeStateStateClaimed || g.count(NodeRenewed) != 1 {
		t.Fatalf("the dependant after the renewal: %+v", g.state(review))
	}
}

// failingAuthority cannot answer, though it names the owner.
type failingAuthority struct{ owedanswers.Authority }

func (failingAuthority) Answer(*eventstore.Tx, types.NodeRecorded, types.OwedInputID, int64) (types.ApprovalID, bool, error) {
	return "", false, errors.New("the authority is unavailable")
}

func (failingAuthority) Owner(*eventstore.Tx, types.NodeRecorded, types.OwedInputID) (types.ActorID, error) {
	return actID, nil
}

// A node named twice claims once, whatever its kind; an authority that
// cannot answer fails the evaluation.
func TestEvaluateOnce(t *testing.T) {
	g := openStore(t)
	build := g.record(g.newNode(types.NodeKindBuild))
	g.must(evaluate(g.env(noRequirement), build, build))
	if g.state(build).State != types.NodeStateStateClaimed {
		t.Fatalf("the build: %+v", g.state(build))
	}
	if err := g.do(evaluate(Env{Need: noRequirement, Blobs: g.blobs, Authority: failingAuthority{}}, g.asking(owes("ruling", ownID)))); err == nil {
		t.Error("an authority error was ignored")
	}
}

// A producer renewed in this command is a structural wait, even for an
// input that needs no receipt: a review result holding an expiring
// receipt, consumed by a review.
func TestEvaluateWaitsOnARenewal(t *testing.T) {
	g := openStore(t)
	producer, _ := g.completedWith(types.NodeKindReview, types.ResultKindReview,
		[]types.Ref{g.sealed(types.RefTypeReceipt, rcpID, g.receiptBlob(treeSubject(treeA, demandA), &past))})
	like := g.newNode(types.NodeKindReview)
	like.Node.Params = g.newNodeLike(producer).Params
	reuser := g.record(like)
	g.must(reuseOf(reuser, g.planRes))
	consumer, onReuser := g.consumerOf(producer, types.ResultKindReview), g.consumerOf(reuser, types.ResultKindReview)
	g.must(evaluate(g.env(noRequirement), onReuser, consumer))
	if g.count(NodeRenewed) != 1 || g.state(consumer).State != types.NodeStateStateWaiting || g.state(onReuser).State != types.NodeStateStateWaiting {
		t.Fatalf("the consumers of a renewal: %+v %+v", g.state(consumer), g.state(onReuser))
	}
}

// reviewWith is a review of its own key taking producer's result as a kind
// result.
func (g graphStore) reviewWith(producer types.NodeID, kind types.ResultKind) types.NodeID {
	r := g.nodeOn(types.NodeKindReview, producer)
	r.Node.Inputs[0].Source.Variant.(*types.ResultSource).ResultKind, r.Node.Inputs[0].Name = kind, "gate"
	r.Node.Params = reviewOf(types.Name(r.Node.ID), exeA) // a review of its own key
	return g.record(r)
}

// A result built while its gate input's receipt was fresh and submitted
// after it expired is refused by the fold, from the validity the input
// bound: recorded late, or recorded in time and selected late. A review
// input's receipt, which no requirement reads, does not expire a result.
func TestBufferedResultAfterExpiry(t *testing.T) {
	at := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := at.Add(time.Hour).Format(time.RFC3339Nano)
	g := openStoreAt(t, func() time.Time { return at })
	gate, _ := g.gated(g.receiptBlob(treeSubject(treeA, demandA), &until))
	review, _ := g.completedWith(types.NodeKindReview, types.ResultKindReview,
		[]types.Ref{g.sealed(types.RefTypeReceipt, rcpID, g.receiptBlob(treeSubject(treeA, demandA), &until))})
	recorded, late, free := g.reviewWith(gate, types.ResultKindGateResult), g.reviewWith(gate, types.ResultKindGateResult), g.reviewWith(review, types.ResultKindReview)
	g.must(evaluate(g.env(gatesOn(treeA, demandA)), recorded, late, free))
	in := g.reviewResult(recorded)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return recordResult(tx, types.ResultRecorded{Node: recorded, Result: in}, g.blobs)
	})
	at = at.Add(2 * time.Hour)
	for name, fn := range map[string]logic{
		"recorded": func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return recordResult(tx, types.ResultRecorded{Node: late, Result: g.reviewResult(late)}, g.blobs)
		},
		"selected": func(tx *eventstore.Tx) ([]eventstore.Event, error) { return selectResult(tx, recorded, in.ID) },
	} {
		if err := g.do(fn); err == nil || !strings.Contains(err.Error(), "expired before the result") {
			t.Errorf("a result %s after its input expired: %v", name, err)
		}
	}
	if err := g.do(commit(free, g.reviewResult(free), g.env(noRequirement))); err != nil || g.state(free).State != types.NodeStateStateCompleted {
		t.Fatalf("a review input's expired receipt: %v, %+v", err, g.state(free))
	}
}

// The fold reads the time a result was recorded, not the wall clock or the
// command's: a store clock before a validity that wall time has passed
// commits, and rebuilds, a result. The fold also refuses a claim whose
// input result is not recorded.
func TestFoldClock(t *testing.T) {
	at := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC) // before past
	g := openStoreAt(t, func() time.Time { return at })
	gate, _ := g.gated(g.receiptBlob(treeSubject(treeA, demandA), &past))
	consumer := g.reviewOf(gate)
	env := g.env(gatesOn(treeA, demandA))
	g.must(evaluate(env, consumer))
	r, input := g.reviewResult(consumer), string(*g.state(gate).Result)
	rollback := errors.New("rolled back")
	var late, unrecorded error
	_ = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { // as a rebuild folds an event
		evs, err := recordResult(tx, types.ResultRecorded{Node: consumer, Result: r}, g.blobs)
		if err == nil {
			evs[0].At = future
			late = ProjectM0(tx, evs)
		}
		return nil, errors.Join(err, rollback)
	})
	_ = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		rec, err := loadNode(tx, consumer)
		if err == nil {
			err = tx.Delete(ResultProjection, input)
		}
		if err == nil {
			unrecorded = claimHolds(tx, rec, past)
		}
		return nil, errors.Join(err, rollback)
	})
	if late == nil || !strings.Contains(late.Error(), "expired before the result") || unrecorded == nil || !strings.Contains(unrecorded.Error(), "is not recorded") {
		t.Fatalf("a result folded after its input expired: %v; an unrecorded input: %v", late, unrecorded)
	}
	g.must(commit(consumer, r, env))
	if diffs, err := g.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 || g.state(consumer).State != types.NodeStateStateCompleted {
		t.Fatalf("the rebuild: %v, %+v, %+v", err, diffs, g.state(consumer))
	}
}
