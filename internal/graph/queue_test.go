// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/effecttarget"
	fakeforge "github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/forge"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// candidate is a completed build whose patch holds refs.
func (g graphStore) candidate(refs ...types.Ref) types.NodeID {
	n, _ := g.completedWith(types.NodeKindBuild, types.ResultKindPatch, append([]types.Ref{}, refs...))
	return n
}

// reviewOn is a completed review that took producer's patch.
func (g graphStore) reviewOn(producer types.NodeID) types.NodeID {
	g.t.Helper()
	review := g.consumerOf(producer, types.ResultKindPatch)
	g.must(evaluate(effectEnv, review))
	g.must(commit(review, g.reviewResult(review), effectEnv))
	return review
}

// queueSpec is a candidate whose patch holds refs, and its review.
func (g graphStore) queueSpec(branch string, refs ...types.Ref) QueueSpec {
	cand := g.candidate(refs...)
	return QueueSpec{Task: tskID, Project: prjID, Branch: branch, Candidate: cand, Review: g.reviewOn(cand)}
}

func (g graphStore) generate(spec QueueSpec, head string) (Generation, error) {
	var gen Generation
	err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, gn, err := Generate(tx, spec, head)
		gen = gn
		return evs, err
	})
	return gen, err
}

// ref is a sealed ref of typ to a new blob of content.
func (g graphStore) ref(typ types.RefType, content string) types.Ref {
	return g.sealed(typ, content, g.blob(content))
}

// rebaseResult is gen's rebase result holding value: clean, or found when
// it holds a resolution.
func (g graphStore) rebaseResult(gen Generation, value ...types.Ref) types.Result {
	r := unrecorded(g.t, *g.state(gen.Rebase).Key)
	r.Kind, r.Value, r.Outcome, r.Reason, r.Detail = types.ResultKindTree, value, types.OutcomeEmpty, vocab.ReasonCompletedClean, ""
	if len(value) == 3 {
		r.Outcome, r.Reason, r.Detail = types.OutcomeFound, vocab.ReasonCompletedWithFindings, "conflicts resolved"
	}
	return r
}

// rebased claims gen's rebase, commits its result holding value, and claims
// its carry_over.
func (g graphStore) rebased(gen Generation, value ...types.Ref) {
	g.t.Helper()
	g.must(evaluate(effectEnv, gen.Rebase))
	g.must(commit(gen.Rebase, g.rebaseResult(gen, value...), effectEnv))
	g.must(evaluate(effectEnv, gen.CarryOver))
}

func (g graphStore) carryOver(n types.NodeID) (r types.Result, err error) {
	err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		var cerr error
		r, cerr = CarryOver(tx, n, effectEnv)
		return nil, cerr
	})
	return r, err
}

// raw records and selects r for n by events alone, as no builder would.
func raw(n types.NodeID, r types.Result, blobs receipt.Blobs) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := recordResult(tx, types.ResultRecorded{Node: n, Result: r}, blobs)
		sel, serr := selectResult(tx, n, r.ID)
		if err == nil {
			err = serr
		}
		return append(evs, sel...), err
	}
}

// Generations follow base moves on a forge, per branch; the fold admits
// only the next generation, on another base.
func TestGenerations(t *testing.T) {
	g := openEffectStore(t)
	f := fakeforge.New()
	spec := g.queueSpec("main", g.ref(types.RefTypeBlob, "P"))
	head := func() string { h, _ := f.Holds("main"); return h }
	f.Move("main", "B1")
	gen1, err := g.generate(spec, head())
	if err != nil || gen1.Number != 1 || gen1.Base != "B1" {
		t.Fatalf("the first generation: %+v (%v)", gen1, err)
	}
	if cur, _ := view[Generation](g, GenerationProjection, generationKey(tskID, "main")); cur != gen1 {
		t.Fatalf("the projection holds %+v, want %+v", cur, gen1)
	}
	if again, err := g.generate(spec, head()); err != nil || again.Number != 0 {
		t.Fatalf("an unmoved base started %+v (%v)", again, err)
	}
	if _, err := g.generate(spec, ""); err == nil || !strings.Contains(err.Error(), "no base head") {
		t.Fatalf("an empty head: %v", err)
	}
	other := spec
	other.Branch = "release"
	if gen, err := g.generate(other, "B1"); err != nil || gen.Number != 1 {
		t.Fatalf("another branch's first generation: %+v (%v)", gen, err)
	}
	other.Branch = ""
	if _, err := g.generate(other, "B1"); err == nil {
		t.Fatal("a generation on no branch was recorded")
	}
	f.Move("main", "B2")
	if gen2, err := g.generate(spec, head()); err != nil || gen2.Number != 2 || gen2.Base != "B2" {
		t.Fatalf("a base move: %+v (%v)", gen2, err)
	}

	rebase := func(params string) logic {
		rec := g.newNode(types.NodeKindRebase)
		rec.Node.Params = json.RawMessage(params)
		return func(*eventstore.Tx) ([]eventstore.Event, error) { return Record(rec) }
	}
	for name, params := range map[string]string{
		"the same base":       `{"branch":"main","base":"B2","generation":3}`,
		"a gap":               `{"branch":"main","base":"B4","generation":4}`,
		"a repeat":            `{"branch":"main","base":"B3","generation":2}`,
		"a first again":       `{"branch":"main","base":"B3","generation":1}`,
		"a new branch at two": `{"branch":"dev","base":"B1","generation":2}`,
		"no base":             `{"branch":"main","base":"","generation":3}`,
		"another param":       `{"branch":"main","base":"B3","generation":3,"x":1}`,
	} {
		if err := g.do(rebase(params)); err == nil {
			t.Errorf("%s: recorded", name)
		}
	}
	g.must(rebase(`{"branch":"main","base":"B3","generation":3}`))
}

// A carry_over attests only a clean rebase of the reviewed patch with the
// candidate's identity.
func TestCarryOver(t *testing.T) {
	g := openEffectStore(t)
	p := g.ref(types.RefTypeBlob, "P")
	spec := g.queueSpec("main", p)
	unreviewed := spec
	unreviewed.Review = g.reviewOn(g.candidate(g.ref(types.RefTypeBlob, "P elsewhere")))
	// The candidate is rebuilt to P after its review took an earlier patch.
	rebuilt := g.candidate(g.ref(types.RefTypeBlob, "P earlier"))
	stale := QueueSpec{Task: tskID, Project: prjID, Candidate: rebuilt, Review: g.reviewOn(rebuilt)}
	g.must(invalidate(rebuilt))
	again := unrecorded(t, *g.state(rebuilt).Key)
	again.Value, again.Outcome, again.Reason, again.Detail = []types.Ref{p}, types.OutcomeEmpty, vocab.ReasonCompletedClean, ""
	g.must(commit(rebuilt, again, effectEnv))
	for i, c := range []struct {
		spec   QueueSpec
		value  []types.Ref
		attest bool
	}{
		{spec, []types.Ref{g.ref(types.RefTypeGitTree, "T1"), p}, true},
		{spec, []types.Ref{g.ref(types.RefTypeGitTree, "T2"), g.ref(types.RefTypeBlob, "P2")}, false},
		{spec, []types.Ref{g.ref(types.RefTypeGitTree, "T3"), p, g.ref(types.RefTypeBlob, "R")}, false},
		{unreviewed, []types.Ref{g.ref(types.RefTypeGitTree, "T4"), p}, false},
		{stale, []types.Ref{g.ref(types.RefTypeGitTree, "T5"), p}, false},
	} {
		c.spec.Branch = "b" + string(rune('1'+i))
		gen, err := g.generate(c.spec, "B")
		if err != nil {
			t.Fatal(err)
		}
		g.rebased(gen, c.value...)
		r, err := g.carryOver(gen.CarryOver)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.Outcome == types.OutcomeEmpty && r.Reason == vocab.ReasonCompletedClean && len(r.Value) == 1 && r.Value[0].ContentDigest == c.value[0].ContentDigest; got != c.attest {
			t.Errorf("case %d: attested %t: %+v", i, got, r)
		}
		if !c.attest && (r.Outcome != types.OutcomeFound || r.Reason != vocab.ReasonReviewInputChanged) {
			t.Errorf("case %d: %s/%s, want found/review_input_changed", i, r.Outcome, r.Reason)
		}
		if st := g.state(gen.CarryOver); st.State != types.NodeStateStateCompleted {
			t.Errorf("case %d: the carry_over is %s", i, st.State)
		}
	}
}

// A rebase result's refs are typed and its conflicts explicit: truncated,
// reordered or mistyped, it is refused when recorded.
func TestRebaseShape(t *testing.T) {
	g := openEffectStore(t)
	p, tree, res := g.ref(types.RefTypeBlob, "P"), g.ref(types.RefTypeGitTree, "T"), g.ref(types.RefTypeBlob, "R")
	spec := g.queueSpec("main", p)
	gen, _ := g.generate(spec, "B1")
	g.must(evaluate(effectEnv, gen.Rebase))
	truncated := g.rebaseResult(gen, tree, p, res)
	truncated.Value = truncated.Value[:2] // found, without its resolution
	for name, r := range map[string]types.Result{
		"reordered":          g.rebaseResult(gen, p, tree),
		"a blob for a tree":  g.rebaseResult(gen, p, p),
		"a tree for a patch": g.rebaseResult(gen, tree, tree),
		"one ref":            g.rebaseResult(gen, tree),
		"truncated":          truncated,
		"a failed rebase": func() types.Result {
			r := g.rebaseResult(gen, tree, p, res)
			r.Outcome, r.Reason = types.OutcomeFailed, vocab.ReasonAuthorityRevoked
			return r
		}(),
		"clean with three": func() types.Result {
			r := g.rebaseResult(gen, tree, p, res)
			r.Outcome, r.Reason, r.Detail = types.OutcomeEmpty, vocab.ReasonCompletedClean, ""
			return r
		}(),
	} {
		if err := g.do(commit(gen.Rebase, r, effectEnv)); err == nil || !strings.Contains(err.Error(), "a rebase holds its git tree") {
			t.Errorf("%s: %v", name, err)
		}
	}
	two := g.queueSpec("dev", p, g.ref(types.RefTypeBlob, "Q"))
	gen2, _ := g.generate(two, "B1")
	g.must(evaluate(effectEnv, gen2.Rebase))
	if err := g.do(commit(gen2.Rebase, g.rebaseResult(gen2, tree, p), effectEnv)); err == nil {
		t.Error("a rebase of a candidate of two refs was recorded")
	}
}

// What a carry_over records is its inputs' decision, whoever records it.
func TestCarryOverInTheFold(t *testing.T) {
	g := openEffectStore(t)
	p, tree := g.ref(types.RefTypeBlob, "P"), g.ref(types.RefTypeGitTree, "T")
	spec := g.queueSpec("main", p)
	gen, _ := g.generate(spec, "B1")
	g.rebased(gen, tree, g.ref(types.RefTypeBlob, "P changed"))
	attest := unrecorded(t, *g.state(gen.CarryOver).Key)
	attest.Kind, attest.Value, attest.Outcome, attest.Reason, attest.Detail = types.ResultKindCarryOverAttestation, []types.Ref{tree}, types.OutcomeEmpty, vocab.ReasonCompletedClean, ""
	if err := g.do(commit(gen.CarryOver, attest, effectEnv)); err == nil || !strings.Contains(err.Error(), "only by CompleteEffect or CarryOver") {
		t.Fatalf("Commit of a carry_over: %v", err)
	}
	if err := g.do(raw(gen.CarryOver, attest, g.blobs)); err == nil || !strings.Contains(err.Error(), "not its inputs'") {
		t.Fatalf("a forged attestation over a changed patch: %v", err)
	}
	demand := attest
	demand.Value, demand.Outcome, demand.Reason, demand.Detail = []types.Ref{}, types.OutcomeFound, vocab.ReasonReviewInputChanged, "forged"
	g.must(raw(gen.CarryOver, demand, g.blobs)) // the decision, whatever the detail

	gen2, _ := g.generate(spec, "B2")
	g.rebased(gen2, tree, p)
	demand2 := unrecorded(t, *g.state(gen2.CarryOver).Key)
	demand2.Kind, demand2.Value, demand2.Outcome, demand2.Reason = types.ResultKindCarryOverAttestation, []types.Ref{}, types.OutcomeFound, vocab.ReasonReviewInputChanged
	if err := g.do(raw(gen2.CarryOver, demand2, g.blobs)); err == nil {
		t.Fatal("a demand where the inputs attest was folded")
	}
	other := demand2
	other.Value, other.Outcome, other.Reason, other.Detail = []types.Ref{g.ref(types.RefTypeGitTree, "T other")}, types.OutcomeEmpty, vocab.ReasonCompletedClean, ""
	if err := g.do(raw(gen2.CarryOver, other, g.blobs)); err == nil {
		t.Fatal("an attestation of another tree was folded")
	}
}

func TestCarryOverRefusals(t *testing.T) {
	g := openEffectStore(t)
	p := g.ref(types.RefTypeBlob, "P")
	spec := g.queueSpec("main", p)
	unclaimed, _ := g.generate(spec, "B1")

	// A carry_over is recorded only on its current generation's rebase of
	// its own candidate.
	other := g.queueSpec("dev", g.ref(types.RefTypeBlob, "Q"))
	otherGen, _ := g.generate(other, "B1")
	integration, _ := g.completedWith(types.NodeKindIntegration, types.ResultKindTree, []types.Ref{g.ref(types.RefTypeGitTree, "T"), p})
	carry := func(rebase types.NodeID, name string) logic {
		rec := g.recordOf(unclaimed.CarryOver)
		rec.Node.ID = types.NodeID("nod_01m4e289fveqtbx5mgt0y0559" + name)
		rec.Node.Inputs = slices.Clone(rec.Node.Inputs)
		for i, in := range rec.Node.Inputs {
			if in.Name == "rebase" {
				rec.Node.Inputs[i].Source.Variant = &types.ResultSource{Type: "result", Producer: rebase, ResultKind: types.ResultKindTree}
			}
		}
		if rebase == "" { // no rebase input at all
			rec.Node.Inputs = slices.DeleteFunc(rec.Node.Inputs, func(in types.InputSpec) bool { return in.Name == "rebase" })
		}
		return func(*eventstore.Tx) ([]eventstore.Event, error) { return Record(rec) }
	}
	gen2, _ := g.generate(spec, "B2")
	for name, c := range map[string]struct {
		rebase types.NodeID
		want   string
	}{
		"a": {integration, "a rebase node of its own candidate"},
		"b": {otherGen.Rebase, "a rebase node of its own candidate"},
		"c": {unclaimed.Rebase, "its current generation's rebase"},
		"d": {"", "a rebase node of its own candidate"},
	} {
		if err := g.do(carry(c.rebase, name)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("case %s: %v", name, err)
		}
	}
	if cur, _ := view[Generation](g, GenerationProjection, generationKey(tskID, "main")); cur.CarryOver != gen2.CarryOver {
		t.Errorf("the generation keeps carry_over %s, want %s", cur.CarryOver, gen2.CarryOver)
	}
	for name, n := range map[string]types.NodeID{"an unclaimed carry_over": unclaimed.CarryOver, "a plan node": g.plan} {
		if _, err := g.carryOver(n); err == nil || !strings.Contains(err.Error(), "is not a claimed carry_over node") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A carry_over's inputs hold their roles in the fold: a raw carry_over
// whose review is an integration's tree over the candidate's patch, or
// whose candidate is a tree, attests nothing, by command, by raw
// events or replayed.
func TestCarryOverRoles(t *testing.T) {
	g := openEffectStore(t)
	p, tree := g.ref(types.RefTypeBlob, "P"), g.ref(types.RefTypeGitTree, "T")
	spec := g.queueSpec("main", p)
	gen, _ := g.generate(spec, "B1")
	g.must(evaluate(effectEnv, gen.Rebase))
	g.must(commit(gen.Rebase, g.rebaseResult(gen, tree, p), effectEnv))
	// An integration that took the candidate's patch, completed with a tree.
	irec := g.nodeOn(types.NodeKindIntegration, spec.Candidate)
	irec.Node.Inputs[0].Name = "patch"
	irec.Node.Inputs[0].Source.Variant.(*types.ResultSource).ResultKind = types.ResultKindPatch
	integration := g.record(irec)
	g.must(evaluate(effectEnv, integration))
	ir := unrecorded(t, *g.state(integration).Key)
	ir.Kind, ir.Value, ir.Outcome, ir.Reason, ir.Detail = types.ResultKindTree, []types.Ref{tree}, types.OutcomeEmpty, vocab.ReasonCompletedClean, ""
	g.must(commit(integration, ir, effectEnv))
	// substitute records and claims a node like tmpl, under a fresh id,
	// whose inputs named in with are those producers' results of their kinds.
	type source struct {
		producer types.NodeID
		kind     types.ResultKind
	}
	substitute := func(tmpl types.NodeID, params string, with map[string]source) types.NodeID {
		rec := g.recordOf(tmpl)
		id, _ := ids.New("nod")
		rec.Node.ID, rec.Node.Inputs = types.NodeID(id), slices.Clone(rec.Node.Inputs)
		if params != "" {
			rec.Node.Params = json.RawMessage(params)
		}
		for i, in := range rec.Node.Inputs {
			if src, ok := with[in.Name]; ok {
				rec.Node.Inputs[i].Source.Variant = &types.ResultSource{Type: "result", Producer: src.producer, ResultKind: src.kind}
			}
		}
		n := g.record(rec)
		g.must(evaluate(effectEnv, n))
		return n
	}
	forged := substitute(gen.CarryOver, "", map[string]source{"review": {integration, types.ResultKindTree}})
	if _, err := g.carryOver(forged); err == nil || !strings.Contains(err.Error(), "a carry_over's review is a review result of its review producer") {
		t.Fatalf("CarryOver over an integration's tree as the review: %v", err)
	}
	attest := unrecorded(t, *g.state(forged).Key)
	attest.Kind, attest.Value, attest.Outcome, attest.Reason, attest.Detail = types.ResultKindCarryOverAttestation, []types.Ref{tree}, types.OutcomeEmpty, vocab.ReasonCompletedClean, ""
	if err := g.do(raw(forged, attest, g.blobs)); err == nil || !strings.Contains(err.Error(), "a carry_over's review") {
		t.Fatalf("a raw attestation over an integration's tree as the review: %v", err)
	}
	// Replayed: the same events, through the fold a rebuild replays.
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := raw(forged, attest, g.blobs)(tx)
		if err != nil {
			return nil, err
		}
		return nil, Project(tx, evs)
	}); err == nil || !strings.Contains(err.Error(), "a carry_over's review") {
		t.Fatalf("a replayed attestation over an integration's tree as the review: %v", err)
	}
	if st := g.state(forged); st.State != types.NodeStateStateClaimed || st.Result != nil {
		t.Fatalf("the forged carry_over is %s with %v", st.State, st.Result)
	}

	// The integration's tree as the candidate of a raw rebase, reviewed as a
	// tree: the rebased "patch" is the tree, so only the role refuses it.
	treeReview := g.consumerOf(integration, types.ResultKindTree)
	g.must(evaluate(effectEnv, treeReview))
	g.must(commit(treeReview, g.reviewResult(treeReview), effectEnv))
	rebase := substitute(gen.Rebase, `{"branch":"dev","base":"B1","generation":1}`, map[string]source{"candidate": {integration, types.ResultKindTree}})
	rr := unrecorded(t, *g.state(rebase).Key)
	rr.Kind, rr.Value, rr.Outcome, rr.Reason, rr.Detail = types.ResultKindTree, []types.Ref{g.ref(types.RefTypeGitTree, "T2"), tree}, types.OutcomeEmpty, vocab.ReasonCompletedClean, ""
	g.must(commit(rebase, rr, effectEnv))
	treeCarry := substitute(gen.CarryOver, "", map[string]source{"candidate": {integration, types.ResultKindTree}, "rebase": {rebase, types.ResultKindTree},
		"review": {treeReview, types.ResultKindReview}})
	if _, err := g.carryOver(treeCarry); err == nil || !strings.Contains(err.Error(), "a carry_over's candidate is a patch result") {
		t.Fatalf("CarryOver over an integration's tree as the candidate: %v", err)
	}

	diffs, err := g.s.RebuildProjections(context.Background())
	if err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// An effect node records its result once its operation's effect completed,
// holding what the effect applied; its inputs were re-validated at the
// intent, not again.
func TestCompleteEffect(t *testing.T) {
	g := openEffectStore(t)
	pkg, pres := g.pkg("M")
	merge := g.effectOn(pkg, types.NodeKindMerge, "merge", `{"target":"main"}`, true)
	complete := func(n types.NodeID) error {
		return g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return nil, CompleteEffect(tx, n, g.blobs) })
	}
	if err := complete(merge); err == nil {
		t.Fatal("an effect node with no operation completed")
	}
	op := g.mustOperate(merge)
	if err := complete(merge); err == nil {
		t.Fatal("an effect node completed while its effect is pending")
	}
	result := func(value ...types.Ref) types.Result {
		r := unrecorded(t, *g.state(merge).Key)
		r.Kind, r.Value, r.Outcome, r.Reason, r.Detail = types.ResultKindMergeResult, append([]types.Ref{}, value...), types.OutcomeEmpty, vocab.ReasonCompletedClean, ""
		return r
	}
	if err := g.do(raw(merge, result(pres.Value...), g.blobs)); err == nil {
		t.Fatal("a result of a pending effect folded")
	}
	g.must(invalidate(pkg)) // after the intent: no re-validation at completion
	if _, err := g.runEffect(op, effecttarget.New()); err != nil {
		t.Fatal(err)
	}
	if err := g.do(invalidate(merge)); err == nil || !strings.Contains(err.Error(), "its result is recorded") {
		t.Fatalf("a completed merge with no result was invalidated: %v", err)
	}
	if err := g.do(commit(merge, result(pres.Value...), effectEnv)); err == nil || !strings.Contains(err.Error(), "only by CompleteEffect") {
		t.Fatalf("Commit of an effect node: %v", err)
	}
	found := result(pres.Value...)
	found.Outcome, found.Reason, found.Detail = types.OutcomeFound, vocab.ReasonCompletedWithFindings, "forged"
	for name, r := range map[string]types.Result{
		"an unrelated ref": result(g.ref(types.RefTypeGitTree, "never pushed")),
		"no ref":           result(),
		"a found outcome":  found,
	} {
		if err := g.do(raw(merge, r, g.blobs)); err == nil {
			t.Errorf("%s: folded", name)
		}
	}
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) error { return CompleteEffect(tx, merge, g.blobs) }); err == nil {
		t.Fatal("an effect node completed outside a command")
	}
	if err := complete(merge); err != nil {
		t.Fatal(err)
	}
	st := g.state(merge)
	res, _ := view[types.ResultRecorded](g, ResultProjection, string(*st.Result))
	if st.State != types.NodeStateStateCompleted || res.Result.Kind != types.ResultKindMergeResult || !sameRefs(res.Result.Value, pres.Value) {
		t.Fatalf("the merge is %s with %+v", st.State, res.Result)
	}
	// The next generation of a completed merge has no operation at its key.
	g.must(invalidate(merge))
	if err := complete(merge); err == nil {
		t.Fatal("an effect node completed at a key its operation is not of")
	}

	// A completed deploy still records no result.
	d := g.deployTo("A", "prod")
	dop := g.mustOperate(d)
	if _, err := g.runEffect(dop, effecttarget.New()); err != nil {
		t.Fatal(err)
	}
	for _, n := range []types.NodeID{d, g.plan} {
		if err := complete(n); err == nil || !strings.Contains(err.Error(), "records no effect result") {
			t.Fatalf("%s completed: %v", n, err)
		}
	}
}
