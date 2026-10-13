// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	fakeforge "github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/forge"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// mergeNeed is the merge kind's requirement of a refreshed gate: a go-test
// receipt about tree under demand A; other inputs need none.
func mergeNeed(tree string) Requirements {
	return func(n types.Node, in types.InputSpec) (*receipt.Requirement, error) {
		if src, ok := in.Source.Variant.(*types.ResultSource); ok && n.Kind == types.NodeKindMerge && carriesConclusions(src.ResultKind) {
			return gateOn(tree, demandA)(n, in)
		}
		return nil, nil
	}
}

// A merge lands by compare-and-set on its generation's base: a base that
// moved before the apply gets no push and fails the effect, confirmed, so
// the next generation's merge is intended and lands.
func TestMergeCAS(t *testing.T) {
	g := openEffectStore(t)
	f := fakeforge.New()
	f.Move("main", "B1")
	gen1 := g.claimedMerge("main", "B1")
	op1 := g.mustOperate(gen1.Merge)
	if op1.Request.Expected != "B1" {
		t.Fatalf("the merge's request applies over %q", op1.Request.Expected)
	}
	f.Move("main", "B2") // after the intent, before the apply
	e, err := g.runEffect(op1, f)
	if !errors.As(err, new(ErrTargetMoved)) || e.State != eventstore.EffectFailedConfirmed || len(f.Pushed("main")) != 0 {
		t.Fatalf("a merge over a moved base: %+v (%v), pushed %v", e, err, f.Pushed("main"))
	}
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return nil, CompleteEffect(tx, gen1.Merge, g.blobs)
	}); err == nil {
		t.Fatal("a failed merge recorded a result")
	}
	spec := g.specOf(gen1)
	gen2, err := g.generate(spec, "B2")
	if err != nil || gen2.Number != 2 {
		t.Fatalf("the base move's generation: %+v (%v)", gen2, err)
	}
	g.rebased(gen2, g.ref(types.RefTypeGitTree, "T on B2"), g.patchOf(spec.Candidate))
	if _, err := g.carryOver(gen2.CarryOver); err != nil {
		t.Fatal(err)
	}
	g.must(evaluate(effectEnv, gen2.Merge))
	op2 := g.mustOperate(gen2.Merge)
	if e, err := g.runEffect(op2, f); err != nil || e.State != eventstore.EffectCompleted {
		t.Fatalf("the next generation's merge: %+v (%v)", e, err)
	}
	if head, _ := f.Holds("main"); head != string(op2.Request.Want) || len(f.Pushed("main")) != 1 {
		t.Fatalf("main holds %s, pushed %v", head, f.Pushed("main"))
	}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return nil, CompleteEffect(tx, gen2.Merge, g.blobs)
	})
}

// The merge consumes the review's verdict: it waits on an attested review
// that is no_go, and on one whose attested result is no longer its
// review's.
func TestMergeVerdict(t *testing.T) {
	g := openEffectStore(t)
	patch := g.ref(types.RefTypeBlob, "P")
	cand := g.candidate(patch)
	review := g.consumerOf(cand, types.ResultKindPatch)
	g.must(evaluate(effectEnv, review))
	nogo := g.reviewResult(review)
	nogo.Outcome, nogo.Reason, nogo.Detail = types.OutcomeFound, vocab.ReasonCompletedWithFindings, "a blocking finding"
	g.must(commit(review, nogo, effectEnv))
	gen, err := g.generate(QueueSpec{Task: tskID, Project: prjID, Branch: "main", Candidate: cand, Review: review}, "B1")
	if err != nil {
		t.Fatal(err)
	}
	g.rebased(gen, g.ref(types.RefTypeGitTree, "T"), patch)
	if r, err := g.carryOver(gen.CarryOver); err != nil || r.Outcome != types.OutcomeEmpty {
		t.Fatalf("the carry_over attests the review, not its verdict: %+v (%v)", r, err)
	}
	waits := func(why string) {
		t.Helper()
		g.must(evaluate(effectEnv, gen.Merge))
		res, miss := g.resolve(gen.Merge, noRequirement, at)
		if st := g.state(gen.Merge); st.State != types.NodeStateStateWaiting || res["carry_over"].Result != "" || len(miss) != 1 || !strings.Contains(miss[0].Detail, why) {
			t.Fatalf("the merge is %s, missing %+v", st.State, miss)
		}
	}
	waits("no_go")
	g.must(invalidate(review))
	g.must(commit(review, g.reviewResult(review), effectEnv))
	waits("no longer its review's")
	// The re-run review's go result starts a generation of its own on the
	// unmoved base, whose merge goes.
	again, err := g.generate(QueueSpec{Task: tskID, Project: prjID, Branch: "main", Candidate: cand, Review: review}, "B1")
	if err != nil || again.Number != 2 || again.Verdict != *g.state(review).Result {
		t.Fatalf("the re-run review's generation: %+v (%v)", again, err)
	}
	g.rebased(again, g.ref(types.RefTypeGitTree, "T"), patch)
	if _, err := g.carryOver(again.CarryOver); err != nil {
		t.Fatal(err)
	}
	g.must(evaluate(effectEnv, again.Merge))
	if st := g.state(again.Merge); st.State != types.NodeStateStateClaimed {
		t.Fatalf("the re-run review's merge is %s", st.State)
	}
	if same, err := g.generate(QueueSpec{Task: tskID, Project: prjID, Branch: "main", Candidate: cand, Review: review}, "B1"); err != nil || same.Number != 0 {
		t.Fatalf("the same review result again started %+v (%v)", same, err)
	}
}

// A merge whose push landed before a crash, and whose base was then moved
// over, is uncertain, not failed: the forge cannot show nothing was applied.
func TestMergeLandedThenMoved(t *testing.T) {
	g := openEffectStore(t)
	f := fakeforge.New()
	f.Move("main", "B1")
	gen := g.claimedMerge("main", "B1")
	op := g.mustOperate(gen.Merge)
	if landed, err := f.Apply("main", op.Effect, string(op.Request.Want), "B1"); err != nil || !landed { // the push, before the crash
		t.Fatal(landed, err)
	}
	f.Move("main", "B2") // someone else's push over it
	e, err := g.runEffect(op, f)
	if !errors.As(err, new(ErrTargetMoved)) || e.State != eventstore.EffectUncertain || len(f.Pushed("main")) != 1 {
		t.Fatalf("a landed merge moved over: %+v (%v)", e, err)
	}
}

// A generation refreshes its checks on the prospective tree: the merge
// takes each check's result, whose receipt must be about the tree the
// rebase holds, and a check of another generation's rebase is refused.
func TestRefreshedChecks(t *testing.T) {
	g := openEffectStore(t)
	patch := g.ref(types.RefTypeBlob, "P")
	spec := g.queueSpec("main", patch)
	spec.Checks = []QueueCheck{{Name: "tests", Kind: types.NodeKindGate, DefinitionVersion: "1", Params: json.RawMessage(`{"suite":"go"}`),
		Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}}
	gen, err := g.generate(spec, "B1")
	if err != nil || len(gen.Checks) != 1 {
		t.Fatalf("the generation: %+v (%v)", gen, err)
	}
	mrec := g.recordOf(gen.Merge).Node
	if len(mrec.Inputs) != 3 || mrec.Inputs[2].Name != "tests" || producerOf(mrec, "tests") != gen.Checks[0] || producerOf(g.recordOf(gen.Checks[0]).Node, "tree") != gen.CarryOver {
		t.Fatalf("the merge takes %+v", mrec.Inputs)
	}
	env := effectEnv
	env.Blobs, env.Need = g.blobs, mergeNeed(treeA)
	g.rebased(gen, g.sealed(types.RefTypeGitTree, treeA, g.blob("prospective")), patch)
	if _, err := g.carryOver(gen.CarryOver); err != nil {
		t.Fatal(err)
	}
	check := gen.Checks[0]
	g.must(evaluate(env, check))
	g.must(commit(check, g.gateResult(check, nil), env))
	// A receipt about tree A serves only a generation whose tree is A: the
	// tree the attestation holds, which the merge applies, even once the
	// rebase ran again onto another tree.
	g.must(invalidate(gen.Rebase))
	g.must(commit(gen.Rebase, g.rebaseResult(gen, g.sealed(types.RefTypeGitTree, treeB, g.blob("rebased again")), patch), effectEnv))
	if _, miss := g.resolve(gen.Merge, mergeNeed(treeA), at); len(miss) != 0 {
		t.Fatalf("the refreshed receipt: %+v", miss)
	}
	g.must(evaluate(env, gen.Merge))
	if st := g.state(gen.Merge); st.State != types.NodeStateStateClaimed {
		t.Fatalf("the merge is %s", st.State)
	}
	var op Operation
	g.must(func(tx *eventstore.Tx) (_ []eventstore.Event, err error) {
		op, err = Operate(tx, gen.Merge, env)
		return nil, err
	})
	if op.Request.Want == "" || op.Request.Expected != "B1" || len(op.Request.Inputs) != 3 {
		t.Fatalf("the merge's request: %+v", op.Request)
	}

	other := spec
	other.Branch = "dev"
	odd, err := g.generate(other, "B1")
	if err != nil {
		t.Fatal(err)
	}
	g.rebased(odd, g.sealed(types.RefTypeGitTree, treeB, g.blob("another")), patch)
	if _, err := g.carryOver(odd.CarryOver); err != nil {
		t.Fatal(err)
	}
	g.must(evaluate(env, odd.Checks[0]))
	g.must(commit(odd.Checks[0], g.gateResult(odd.Checks[0], nil), env)) // about tree A, not B
	if _, miss := g.resolve(odd.Merge, mergeNeed(treeA), at); len(miss) != 1 || miss[0].Refusal == nil || !strings.Contains(miss[0].Detail, "prospective tree") {
		t.Fatalf("a receipt about another tree: %+v", miss)
	}
	g.must(evaluate(env, odd.Merge))
	if st := g.state(odd.Merge); st.State != types.NodeStateStateWaiting {
		t.Fatalf("the merge of another tree's receipt is %s", st.State)
	}

	// A check of an earlier generation's rebase, or after the merge, is
	// refused; a check of any other tree is not the queue's.
	late := g.recordOf(gen.Checks[0])
	late.Node.ID = types.NodeID("nod_01m4e289fveqtbx5mgt0y0559z")
	if err := g.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return Record(late) }); err == nil {
		t.Error("a check after its generation's merge was recorded")
	}
	if _, err := g.generate(spec, "B2"); err != nil {
		t.Fatal(err)
	}
	if err := g.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return Record(late) }); err == nil {
		t.Error("a check of an earlier generation's rebase was recorded")
	}
	bad := spec
	bad.Branch, bad.Checks = "rel", []QueueCheck{{Name: "build", Kind: types.NodeKindBuild, DefinitionVersion: "1", Params: json.RawMessage(`{}`)}}
	if _, err := g.generate(bad, "B1"); err == nil || !strings.Contains(err.Error(), "not a build") {
		t.Errorf("a build check: %v", err)
	}
	if diffs, err := g.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// A replacement review starts a generation of its own on an unmoved base,
// whose carry_over attests it and whose merge goes.
func TestReplacementStartsAGeneration(t *testing.T) {
	g := openEffectStore(t)
	gen1 := g.claimedMerge("main", "B1")
	spec := g.specOf(gen1)
	if again, err := g.generate(spec, "B1"); err != nil || again.Number != 0 {
		t.Fatalf("the same review on the same base: %+v (%v)", again, err)
	}
	replacement := g.rosterChange(spec.Review, exeB)
	gen2, err := g.generate(spec, "B1") // the requirement moved: the superseded review is carried, and waited on
	if err != nil || gen2.Number != 2 {
		t.Fatalf("a requirement change: %+v (%v)", gen2, err)
	}
	spec.Review = replacement
	g.must(evaluate(effectEnv, spec.Review))
	g.must(commit(spec.Review, g.reviewResult(spec.Review), effectEnv))
	gen3, err := g.generate(spec, "B1")
	if err != nil || gen3.Number != 3 || gen3.Base != "B1" || gen3.Review != spec.Review {
		t.Fatalf("the replacement's generation: %+v (%v)", gen3, err)
	}
	g.rebased(gen3, g.ref(types.RefTypeGitTree, "T3"), g.patchOf(spec.Candidate))
	if r, err := g.carryOver(gen3.CarryOver); err != nil || r.Outcome != types.OutcomeEmpty {
		t.Fatalf("the replacement's carry_over: %+v (%v)", r, err)
	}
	g.must(evaluate(effectEnv, gen3.Merge))
	if st := g.state(gen3.Merge); st.State != types.NodeStateStateClaimed {
		t.Fatalf("the replacement's merge is %s", st.State)
	}
}

// The queue's folds hold a generation together: a carry_over takes the
// review and requirement its rebase pins; a check takes its current
// generation's rebase; a merge takes each check's result as its kind.
func TestGenerationFolds(t *testing.T) {
	g := openEffectStore(t)
	gen := g.claimedMerge("main", "B1")
	spec := g.specOf(gen)
	spec.Branch = "dev"
	spec.Checks = []QueueCheck{{Name: "tests", Kind: types.NodeKindGate, DefinitionVersion: "1", Params: json.RawMessage(`{"suite":"go"}`),
		Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}}
	// Two generations of dev, the second recorded without its merge.
	first, err := g.generate(spec, "B1")
	if err != nil {
		t.Fatal(err)
	}
	var bare Generation
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, gn, err := Generate(tx, spec, Observation{Head: "B2", Version: 1 << 40})
		bare = gn
		return evs[:len(evs)-1], err // the merge's node.recorded comes last
	})
	record := func(n types.NodeRecorded) error {
		n.Node.ID = types.NodeID(must(ids.New("nod")))
		return g.do(forge(NodeRecorded, string(n.Node.ID), prjID, &n))
	}
	swap := func(n types.NodeRecorded, name string, producer types.NodeID, kind types.ResultKind) types.NodeRecorded {
		n.Node.Inputs = slices.Clone(n.Node.Inputs)
		for i, in := range n.Node.Inputs {
			if in.Name == types.Name(name) {
				n.Node.Inputs[i].Source.Variant = &types.ResultSource{Type: "result", Producer: producer, ResultKind: kind}
			}
		}
		return n
	}
	carry := g.recordOf(bare.CarryOver)
	other, _, _ := g.completeRequirement(g.requirementNode(1, "1", exeA, exeC), nil) // recorded, never selected
	for name, n := range map[string]types.NodeRecorded{
		"another review":      swap(carry, "review", g.plan, types.ResultKindReview),
		"another requirement": swap(carry, "requirement", other, types.ResultKindReviewRequirement),
		"an earlier rebase":   swap(carry, "rebase", first.Rebase, types.ResultKindTree),
		"an earlier check":    g.recordOf(first.Checks[0]),
	} {
		if err := record(n); err == nil {
			t.Errorf("%s: recorded", name)
		}
	}
	merge := g.recordOf(first.Merge)
	merge.Node.Params = json.RawMessage(`{"target":"dev","base":"B2","generation":2}`)
	merge = swap(merge, "carry_over", bare.CarryOver, types.ResultKindCarryOverAttestation)
	merge = swap(merge, "tests", bare.Checks[0], types.ResultKindGateResult)
	if err := record(swap(merge, "tests", bare.Checks[0], types.ResultKindEvidence)); err == nil {
		t.Error("a merge taking its check's result as another kind: recorded")
	}
	if err := record(merge); err != nil {
		t.Fatalf("the generation's merge: %v", err)
	}
}

// rawInvalidation is node.invalidated for n alone, with none of the reuse
// releases its builder emits: a stale alias keeps its old result.
func rawInvalidation(n types.NodeID) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		c, _, err := load[claim](tx, ClaimProjection, string(n))
		rec, rerr := loadNode(tx, n)
		key, kerr := Key(rec.Recorded.Node, resolutions(c.Inputs), c.Key.Generation+1)
		if err = errors.Join(err, rerr, kerr); err != nil {
			return nil, err
		}
		return event(NodeInvalidated, string(n), prjID, &types.NodeInvalidated{Type: "node_invalidated", Node: n, Key: key})
	}
}

// A carried reused review whose holder was invalidated, its release never
// recorded, is not go: the merge's resolution, intent and the replayed log
// all refuse it; while the merge's operation is live, the holder is not
// invalidated at all.
func TestMergeStaleReuse(t *testing.T) {
	g := openEffectStore(t)
	f := fakeforge.New()
	f.Move("main", "B1")
	patch := g.ref(types.RefTypeBlob, "P")
	spec := g.queueSpec("main", patch)
	like := g.recordOf(spec.Review)
	like.Node.ID = types.NodeID(must(ids.New("nod")))
	alias := g.record(like)
	g.must(evaluate(g.env(noRequirement), alias))
	holder := spec.Review
	spec.Review = alias
	attested := func(base string) Generation {
		t.Helper()
		f.Move("main", base)
		gen, err := g.generate(spec, base)
		if err != nil {
			t.Fatal(err)
		}
		g.rebased(gen, g.ref(types.RefTypeGitTree, "T on "+base), patch)
		if r, err := g.carryOver(gen.CarryOver); err != nil || r.Outcome != types.OutcomeEmpty {
			t.Fatalf("the carry_over: %+v (%v)", r, err)
		}
		g.must(evaluate(effectEnv, gen.Merge))
		return gen
	}
	// Live: the holder is fenced, even by a raw event.
	live := attested("B1")
	op := g.mustOperate(live.Merge)
	if err := g.do(rawInvalidation(holder)); !errors.As(err, new(ErrMergeLive)) {
		t.Fatalf("the holder of a live merge's review invalidated: %v", err)
	}
	if e, err := g.runEffect(op, f); err != nil || e.State != eventstore.EffectCompleted {
		t.Fatalf("the live merge: %+v (%v)", e, err)
	}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return nil, CompleteEffect(tx, live.Merge, g.blobs)
	})
	// Claimed, not intended: the holder is invalidated without releases.
	head, _ := f.Holds("main")
	gen := attested(head)
	g.must(rawInvalidation(holder))
	if st := g.state(alias); st.State != types.NodeStateStateCompleted {
		t.Fatalf("the stale alias is %s", st.State)
	}
	if _, miss := g.resolve(gen.Merge, noRequirement, at); len(miss) != 1 || !strings.Contains(miss[0].Detail, "serves nothing") {
		t.Fatalf("the merge of a stale reuse resolves: %+v", miss)
	}
	var stale ErrRevalidated
	if _, err := g.operate(gen.Merge); !errors.As(err, &stale) || stale.Input != "carry_over" {
		t.Fatalf("the intent of a merge of a stale reuse: %v", err)
	}
	if diffs, err := g.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// A carry_over claims and records only the review result its generation
// pins: a review re-run after the generation was recorded is a new
// generation's, by command and by raw event.
func TestCarryOverPinnedReview(t *testing.T) {
	g := openEffectStore(t)
	patch := g.ref(types.RefTypeBlob, "P")
	spec := g.queueSpec("main", patch)
	gen, err := g.generate(spec, "B1")
	if err != nil {
		t.Fatal(err)
	}
	g.must(evaluate(effectEnv, gen.Rebase))
	g.must(commit(gen.Rebase, g.rebaseResult(gen, g.ref(types.RefTypeGitTree, "T"), patch), effectEnv))
	g.must(invalidate(spec.Review))
	g.must(commit(spec.Review, g.reviewResult(spec.Review), effectEnv))
	if err := g.do(evaluate(effectEnv, gen.CarryOver)); err == nil || !strings.Contains(err.Error(), "pins review result") || g.state(gen.CarryOver).Key != nil {
		t.Fatalf("a carry_over of a re-run review claimed: %v", err)
	}
	raw := g.recordOf(gen.CarryOver)
	raw.Node.ID = types.NodeID(must(ids.New("nod")))
	if err := g.do(forge(NodeRecorded, string(raw.Node.ID), prjID, &raw)); err == nil || !strings.Contains(err.Error(), "its result") {
		t.Fatalf("a raw carry_over of a re-run review recorded: %v", err)
	}
	next, err := g.generate(spec, "B1")
	if err != nil || next.Number != 2 || next.Verdict != *g.state(spec.Review).Result {
		t.Fatalf("the re-run review's generation: %+v (%v)", next, err)
	}
	g.rebased(next, g.ref(types.RefTypeGitTree, "T"), patch)
	if r, err := g.carryOver(next.CarryOver); err != nil || r.Outcome != types.OutcomeEmpty {
		t.Fatalf("its carry_over: %+v (%v)", r, err)
	}
}

// A refreshed evidence check takes the attestation's one tree as its
// subject: it is claimed under its contract and caps, and completes.
func TestRefreshedEvidence(t *testing.T) {
	g := openEffectStore(t)
	patch := g.ref(types.RefTypeBlob, "P")
	spec := g.queueSpec("main", patch)
	spec.Checks = []QueueCheck{{Name: "walk", Kind: types.NodeKindEvidence, DefinitionVersion: "1", Params: evidenceCaps("refresh", "tree", 1, 10),
		Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{types.ResourceGroupKindEvidenceTarget}}}}
	gen, err := g.generate(spec, "B1")
	if err != nil {
		t.Fatal(err)
	}
	g.rebased(gen, g.ref(types.RefTypeGitTree, "T"), patch)
	env := withContracts(effectEnv, contractsOf(walkContract()))
	walk := gen.Checks[0]
	g.must(evaluate(env, walk)) // waits: no attestation yet
	if st := g.state(walk); st.State != types.NodeStateStateWaiting {
		t.Fatalf("a check before its attestation is %s", st.State)
	}
	if _, err := g.carryOver(gen.CarryOver); err != nil {
		t.Fatal(err)
	}
	g.must(evaluate(env, walk))
	if st := g.state(walk); st.State != types.NodeStateStateClaimed {
		t.Fatalf("the refreshed walk is %s", st.State)
	}
	attempt := g.attemptOn(walk)
	g.must(func(tx *eventstore.Tx) (_ []eventstore.Event, err error) {
		r := g.evidence(walk, nil)
		r.Value = []types.Ref{g.sealed(types.RefTypeBlob, "trace", g.blob("trace"))}
		_, err = CommitEvidence(tx, walk, r, attempt, isolatedProof(attempt), env)
		return nil, err
	})
	if st := g.state(walk); st.State != types.NodeStateStateCompleted {
		t.Fatalf("the refreshed walk completed as %s", st.State)
	}
}

// A refreshed check of a carry_over that demands a delta review waits: it
// has no prospective tree to check.
func TestRefreshedCheckWaitsOnDemand(t *testing.T) {
	g := openEffectStore(t)
	patch := g.ref(types.RefTypeBlob, "P")
	spec := g.queueSpec("main", patch)
	spec.Checks = []QueueCheck{{Name: "tests", Kind: types.NodeKindGate, DefinitionVersion: "1", Params: json.RawMessage(`{"suite":"go"}`),
		Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}}
	gen, err := g.generate(spec, "B1")
	if err != nil {
		t.Fatal(err)
	}
	g.rebased(gen, g.ref(types.RefTypeGitTree, "T"), g.ref(types.RefTypeBlob, "P rebased otherwise"))
	if r, err := g.carryOver(gen.CarryOver); err != nil || r.Outcome != types.OutcomeFound {
		t.Fatalf("the carry_over: %+v (%v)", r, err)
	}
	g.must(evaluate(effectEnv, gen.Checks[0]))
	if st := g.state(gen.Checks[0]); st.State != types.NodeStateStateWaiting {
		t.Fatalf("a check of a delta-review demand is %s", st.State)
	}
}

// A carried reused review whose holder was renewed by a raw event, its
// releases never recorded, is not go: the renewal keeps the generation,
// so the alias stays completed on its old result, but the holder no
// longer selects it. The merge does not resolve, before or after the
// holder selects a no_go result, its intent is refused, and the log
// replays the same.
func TestMergeRawRenewal(t *testing.T) {
	g := openEffectStore(t)
	patch := g.ref(types.RefTypeBlob, "P")
	cand := g.candidate(patch)
	holder := g.consumerOf(cand, types.ResultKindPatch)
	g.must(evaluate(effectEnv, holder))
	// Valid while the generation is built, expired by the raw renewal (the
	// evaluator renews an expired holder itself, with its releases).
	until := time.Now().UTC().Add(1500 * time.Millisecond).Format(time.RFC3339Nano)
	d := g.receiptBlob(treeSubject(treeA, demandA), &until)
	expiring := g.reviewResult(holder)
	expiring.Value = []types.Ref{g.sealed(types.RefTypeReceipt, rcpID, d)}
	withBlobs := effectEnv
	withBlobs.Blobs = g.blobs
	g.must(commit(holder, expiring, withBlobs))
	like := g.recordOf(holder)
	like.Node.ID = types.NodeID(must(ids.New("nod")))
	alias := g.record(like)
	g.must(evaluate(g.env(noRequirement), alias))
	if st := g.state(alias); st.State != types.NodeStateStateCompleted || *st.Result != expiring.ID {
		t.Fatalf("the alias: %+v", st)
	}
	gen, err := g.generate(QueueSpec{Task: tskID, Project: prjID, Branch: "main", Candidate: cand, Review: alias}, "B1")
	if err != nil {
		t.Fatal(err)
	}
	g.rebased(gen, g.ref(types.RefTypeGitTree, "T"), patch)
	if r, err := g.carryOver(gen.CarryOver); err != nil || r.Outcome != types.OutcomeEmpty {
		t.Fatalf("the carry_over: %+v (%v)", r, err)
	}
	g.must(evaluate(effectEnv, gen.Merge))
	// The holder's renewal, without the alias's release.
	time.Sleep(time.Until(must(time.Parse(time.RFC3339Nano, until))) + 50*time.Millisecond)
	g.must(forge(NodeRenewed, string(holder), prjID, &types.NodeRenewed{Type: "node_renewed", Node: holder, Key: *g.state(holder).Key,
		Result: expiring.ID, Receipt: d, ValidUntil: types.Time(until)}))
	if st := g.state(alias); st.State != types.NodeStateStateCompleted || *st.Result != expiring.ID {
		t.Fatalf("the unreleased alias: %+v", st)
	}
	stale := func(when string) {
		t.Helper()
		if _, miss := g.resolve(gen.Merge, noRequirement, at); len(miss) != 1 || !strings.Contains(miss[0].Detail, "holder") {
			t.Fatalf("%s: the merge of a renewed holder's alias resolves: %+v", when, miss)
		}
		var refused ErrRevalidated
		if _, err := g.operate(gen.Merge); !errors.As(err, &refused) || refused.Input != "carry_over" {
			t.Fatalf("%s: the intent: %v", when, err)
		}
	}
	stale("renewed")
	nogo := g.reviewResult(holder)
	nogo.Outcome, nogo.Reason, nogo.Detail = types.OutcomeFound, vocab.ReasonCompletedWithFindings, "a blocking finding"
	g.must(commit(holder, nogo, effectEnv))
	stale("no_go selected")
	if diffs, err := g.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
	stale("replayed")
}
