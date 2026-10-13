// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
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

// observed numbers the observations generate makes, newest last.
var observed int64

// generate is generateAt head, newer than every observation before it.
func (g graphStore) generate(spec QueueSpec, head string) (Generation, error) {
	observed++
	return g.generateAt(spec, Observation{Head: head, Version: observed})
}

func (g graphStore) generateAt(spec QueueSpec, obs Observation) (Generation, error) {
	var gen Generation
	err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, gn, err := Generate(tx, spec, obs)
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

// claimedMerge is the first generation of a reviewed candidate on base,
// whose carry_over attested and whose merge is claimed.
func (g graphStore) claimedMerge(branch, base string) Generation {
	g.t.Helper()
	patch := g.ref(types.RefTypeBlob, "P on "+branch)
	gen, err := g.generate(g.queueSpec(branch, patch), base)
	if err != nil {
		g.t.Fatal(err)
	}
	g.rebased(gen, g.ref(types.RefTypeGitTree, "T on "+base), patch)
	if r, err := g.carryOver(gen.CarryOver); err != nil || r.Outcome != types.OutcomeEmpty {
		g.t.Fatalf("the carry_over: %+v (%v)", r, err)
	}
	g.must(evaluate(effectEnv, gen.Merge))
	if st := g.state(gen.Merge); st.State != types.NodeStateStateClaimed {
		g.t.Fatalf("the merge is %s", st.State)
	}
	return gen
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
	if cur, _ := view[Generation](g, GenerationProjection, generationKey(tskID, "main")); !reflect.DeepEqual(cur, gen1) {
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

	cand := *g.state(spec.Candidate).Result
	pinned, _ := view[Generation](g, GenerationProjection, generationKey(tskID, "main"))
	rebase := func(params string) logic {
		rec := g.newNode(types.NodeKindRebase)
		rec.Node.Inputs[0].Name, rec.Node.Inputs[0].Source.Variant = "candidate", &types.ResultSource{Type: "result", Producer: spec.Candidate, ResultKind: types.ResultKindPatch}
		if !strings.Contains(params, `"review"`) {
			params = strings.Replace(params, `"generation"`, `"review":"`+string(pinned.Review)+`","review_result":"`+string(pinned.Verdict)+`","requirement":"`+string(pinned.Requirement)+`","generation"`, 1)
		}
		params = strings.Replace(params, `"generation"`, `"version":1000,"candidate":"`+string(cand)+`","generation"`, 1)
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
		"no review":           `{"branch":"main","base":"B3","review":"","requirement":"` + string(pinned.Requirement) + `","generation":3}`,
		"no requirement":      `{"branch":"main","base":"B3","review":"` + string(spec.Review) + `","requirement":"","generation":3}`,
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
	// A raw rebase pins the integration as its generation's review: only
	// the role refuses it then.
	pinning := substitute(gen.Rebase, `{"branch":"main","base":"B1","version":`+strconv.FormatInt(gen.Version, 10)+`,"candidate":"`+string(gen.Candidate)+`","review":"`+string(integration)+
		`","review_result":"`+string(ir.ID)+`","requirement":"`+string(gen.Requirement)+`","generation":2}`, nil)
	g.must(commit(pinning, g.rebaseResult(Generation{Rebase: pinning}, tree, p), effectEnv))
	if err := g.do(func(*eventstore.Tx) ([]eventstore.Event, error) {
		rec := g.recordOf(gen.CarryOver)
		rec.Node.ID = types.NodeID(must(ids.New("nod")))
		return Record(rec)
	}); err == nil {
		t.Fatal("a carry_over of an earlier generation's rebase was recorded")
	}
	forged := substitute(gen.CarryOver, "", map[string]source{"review": {integration, types.ResultKindTree}, "rebase": {pinning, types.ResultKindTree}})
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
	rebase := substitute(gen.Rebase, `{"branch":"dev","base":"B1","version":1,"candidate":"`+string(ir.ID)+`","review":"`+string(treeReview)+
		`","review_result":"`+string(*g.state(treeReview).Result)+`","requirement":"`+string(gen.Requirement)+`","generation":1}`, map[string]source{"candidate": {integration, types.ResultKindTree}})
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
	gen := g.claimedMerge("main", "B1")
	pkg, merge := gen.CarryOver, gen.Merge
	pres, _ := view[types.ResultRecorded](g, ResultProjection, string(*g.state(pkg).Result))
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
	if err := g.do(raw(merge, result(pres.Result.Value...), g.blobs)); err == nil {
		t.Fatal("a result of a pending effect folded")
	}
	f := fakeforge.New()
	f.Move("main", "B1") // the generation's base
	if _, err := g.runEffect(op, f); err != nil {
		t.Fatal(err)
	}
	g.must(invalidate(pkg)) // after the apply: no re-validation at completion
	if err := g.do(invalidate(merge)); err == nil || !strings.Contains(err.Error(), "its result is recorded") {
		t.Fatalf("a completed merge with no result was invalidated: %v", err)
	}
	if err := g.do(commit(merge, result(pres.Result.Value...), effectEnv)); err == nil || !strings.Contains(err.Error(), "only by CompleteEffect") {
		t.Fatalf("Commit of an effect node: %v", err)
	}
	found := result(pres.Result.Value...)
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
	if st.State != types.NodeStateStateCompleted || res.Result.Kind != types.ResultKindMergeResult || !sameRefs(res.Result.Value, pres.Result.Value) {
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

// A roster change after carry-over was selected makes the merge wait: the
// merge's intent re-validates against the subject's selected requirement
// revision and is refused; the next generation's carry_over demands the
// replacement review, and its merge waits. Once the replacement review is
// made under the new revision, a generation attests it and its merge is
// claimed and intended.
func TestMergeRequirement(t *testing.T) {
	g := openEffectStore(t)
	gen1 := g.claimedMerge("main", "B1")
	spec := g.specOf(gen1)
	replacement := g.rosterChange(spec.Review, exeB)
	var stale ErrRevalidated
	if _, err := g.operate(gen1.Merge); !errors.As(err, &stale) || stale.Reason != vocab.ReasonStaleExpectedState || stale.Input != "carry_over" && stale.Input != "requirement" || g.count(NodeOperated) != 0 { // the verdict sees the superseded review first
		t.Fatalf("the merge's intent after a roster change: %v", err)
	}
	patch := g.patchOf(spec.Candidate)
	gen2, err := g.generate(spec, "B2")
	if err != nil {
		t.Fatal(err)
	}
	g.must(evaluate(effectEnv, gen2.Rebase))
	g.must(commit(gen2.Rebase, g.rebaseResult(gen2, g.ref(types.RefTypeGitTree, "T on B2"), patch), effectEnv))
	g.must(evaluate(effectEnv, gen2.Merge)) // the superseded review serves no carry_over
	if co, m := g.state(gen2.CarryOver), g.state(gen2.Merge); co.State != types.NodeStateStateWaiting || m.State != types.NodeStateStateWaiting {
		t.Fatalf("the carry_over of a superseded review is %s, its merge %s", co.State, m.State)
	}
	// The replacement review, made under revision 2.
	spec.Review = replacement
	g.must(evaluate(effectEnv, spec.Review))
	g.must(commit(spec.Review, g.reviewResult(spec.Review), effectEnv))
	gen3, err := g.generate(spec, "B3")
	if err != nil {
		t.Fatal(err)
	}
	g.rebased(gen3, g.ref(types.RefTypeGitTree, "T on B3"), patch)
	if r, err := g.carryOver(gen3.CarryOver); err != nil || r.Outcome != types.OutcomeEmpty {
		t.Fatalf("the carry_over of the replacement: %+v (%v)", r, err)
	}
	g.must(evaluate(effectEnv, gen3.Merge))
	if op := g.mustOperate(gen3.Merge); g.effect(op.Effect).State != eventstore.EffectPending {
		t.Fatalf("the replacement's merge: %+v", g.effect(op.Effect))
	}
	if diffs, err := g.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// rosterChange supersedes review, of the candidate in tskID, by one of
// exes, under its subject's next requirement revision (G6b-1), and
// returns the replacement.
func (g graphStore) rosterChange(review types.NodeID, exes ...types.ExecutorID) types.NodeID {
	g.t.Helper()
	cur, _ := view[revision](g, RequirementProjection, requirementIndex(tskID, "candidate"))
	req := g.requirementNode(cur.Revision+1, "1", exes...)
	n := g.record(req)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return claimAny(tx, n, map[string]Resolved{}, 1) })
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Supersede(tx, review, n, reviewers) })
	r := unrecorded(g.t, *g.state(n).Key)
	r.Kind, r.Value, r.Conclusions, r.Outcome, r.Reason, r.Detail = types.ResultKindReviewRequirement, []types.Ref{}, &req.Node.Params, types.OutcomeFound, vocab.ReasonCompletedWithFindings, "required"
	g.must(raw(n, r, g.blobs))
	repl, _ := view[types.NodeID](g, SuccessorProjection, string(review))
	return repl
}

// specOf is the queue spec gen was generated for.
func (g graphStore) specOf(gen Generation) QueueSpec {
	co := g.recordOf(gen.CarryOver).Node
	return QueueSpec{Task: tskID, Project: prjID, Branch: "main", Candidate: producerOf(co, "candidate"), Review: producerOf(co, "review")}
}

// patchOf is the one ref of n's selected patch.
func (g graphStore) patchOf(n types.NodeID) types.Ref {
	r, _ := view[types.ResultRecorded](g, ResultProjection, string(*g.state(n).Result))
	return r.Result.Value[0]
}

// A merge is one per generation, on its current carry_over and that
// carry_over's requirement, at its base; a generation needs its review's
// selected requirement.
func TestMergeRecorded(t *testing.T) {
	g := openEffectStore(t)
	gen := g.claimedMerge("main", "B1")
	other, _, err := g.completeRequirement(g.requirementNode(1, "1", exeA, exeC), nil)
	if err == nil {
		t.Fatal("another requirement at the selected revision")
	}
	second := g.recordOf(gen.Merge)
	second.Node.ID = types.NodeID(must(ids.New("nod")))
	if err := g.do(forge(NodeRecorded, string(second.Node.ID), prjID, &second)); err == nil {
		t.Error("a second merge: recorded")
	}
	// A generation recorded without its merge, so each refusal is its own.
	spec := g.specOf(gen)
	spec.Branch = "dev"
	var bare Generation
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, gn, err := Generate(tx, spec, Observation{Head: "B1", Version: 1})
		bare = gn
		return evs[:len(evs)-1], err // the merge's node.recorded comes last
	})
	rec := g.recordOf(gen.Merge)
	rec.Node.Params = json.RawMessage(`{"target":"dev","base":"B1","generation":1}`)
	rec.Node.Inputs = slices.Clone(rec.Node.Inputs)
	rec.Node.Inputs[0].Source.Variant = &types.ResultSource{Type: "result", Producer: bare.CarryOver, ResultKind: types.ResultKindCarryOverAttestation}
	params := func(js string) func(*types.NodeRecorded) {
		return func(r *types.NodeRecorded) { r.Node.Params = json.RawMessage(js) }
	}
	for name, change := range map[string]func(*types.NodeRecorded){
		"another generation's carry_over": func(r *types.NodeRecorded) {
			r.Node.Inputs[0].Source.Variant = &types.ResultSource{Type: "result", Producer: gen.CarryOver, ResultKind: types.ResultKindCarryOverAttestation}
		},
		"another generation":     params(`{"target":"dev","base":"B1","generation":2}`),
		"another base":           params(`{"target":"dev","base":"B0","generation":1}`),
		"no generation":          params(`{"target":"rel","base":"B1","generation":1}`),
		"params of another kind": params(`{"target":"dev","base":"B1","generation":1,"x":1}`),
		"no requirement":         func(r *types.NodeRecorded) { r.Node.Inputs = r.Node.Inputs[:1] },
		"another requirement": func(r *types.NodeRecorded) {
			r.Node.Inputs[1].Source.Variant = &types.ResultSource{Type: "result", Producer: other, ResultKind: types.ResultKindReviewRequirement}
		},
		"the carry_over twice": func(r *types.NodeRecorded) { r.Node.Inputs[1] = r.Node.Inputs[0] },
		"a renamed input":      func(r *types.NodeRecorded) { r.Node.Inputs[1].Name = "required" },
		"an extra input":       func(r *types.NodeRecorded) { r.Node.Inputs = append(r.Node.Inputs, g.planInput("plan")) },
	} {
		r := rec
		r.Node.ID, r.Node.Inputs = types.NodeID(must(ids.New("nod"))), slices.Clone(rec.Node.Inputs)
		change(&r)
		if err := g.do(forge(NodeRecorded, string(r.Node.ID), prjID, &r)); err == nil {
			t.Errorf("%s: recorded", name)
		}
	}
	rec.Node.ID = types.NodeID(must(ids.New("nod")))
	g.must(forge(NodeRecorded, string(rec.Node.ID), prjID, &rec))
	if cur, _ := view[Generation](g, GenerationProjection, generationKey(tskID, "dev")); cur.Merge != rec.Node.ID {
		t.Fatalf("the generation's merge is %s, want %s", cur.Merge, rec.Node.ID)
	}
	nobody := QueueSpec{Task: tskID, Project: prjID, Branch: "other", Candidate: spec.Candidate, Review: g.plan}
	if _, err := g.generate(nobody, "B1"); err == nil || !strings.Contains(err.Error(), "no selected review requirement") {
		t.Fatalf("a generation of a review with no requirement: %v", err)
	}
}

// A carry_over of a reused review attests through the review's result
// holder: its claim is the one read.
func TestCarryOverReusedReview(t *testing.T) {
	g := openEffectStore(t)
	patch := g.ref(types.RefTypeBlob, "P")
	spec := g.queueSpec("main", patch)
	like := g.recordOf(spec.Review)
	like.Node.ID = types.NodeID(must(ids.New("nod")))
	reused := g.record(like)
	if err := g.do(evaluate(g.env(noRequirement), reused)); err != nil || g.state(reused).State != types.NodeStateStateCompleted {
		t.Fatalf("the reuse: %v %+v", err, g.state(reused))
	}
	spec.Review = reused
	gen, err := g.generate(spec, "B1")
	if err != nil {
		t.Fatal(err)
	}
	g.rebased(gen, g.ref(types.RefTypeGitTree, "T"), patch)
	if r, err := g.carryOver(gen.CarryOver); err != nil || r.Outcome != types.OutcomeEmpty {
		t.Fatalf("the carry_over of a reused review: %+v (%v)", r, err)
	}
	if diffs, err := g.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// A carry_over attests only a review made under its requirement: a review
// of the old revision that was not superseded (another review of the
// subject was) gets the demand for the replacement, and the merge of that
// demand waits.
func TestCarryOverRequirement(t *testing.T) {
	g := openEffectStore(t)
	patch := g.ref(types.RefTypeBlob, "P")
	spec := g.queueSpec("main", patch)
	other := g.nodeOn(types.NodeKindReview, spec.Candidate)
	other.Node.Inputs[0].Source.Variant.(*types.ResultSource).ResultKind = types.ResultKindPatch
	other.Node.Inputs = append(other.Node.Inputs, g.planInput("again"))
	kept := g.record(other) // another review of revision 1, of its own key
	g.must(evaluate(effectEnv, kept))
	g.must(commit(kept, g.reviewResult(kept), effectEnv))
	g.rosterChange(spec.Review, exeB)
	spec.Review = kept
	gen, err := g.generate(spec, "B1")
	if err != nil {
		t.Fatal(err)
	}
	g.rebased(gen, g.ref(types.RefTypeGitTree, "T"), patch)
	r, err := g.carryOver(gen.CarryOver)
	if err != nil || r.Outcome != types.OutcomeFound || r.Reason != vocab.ReasonReviewInputChanged || !strings.Contains(r.Detail, "replacement review") {
		t.Fatalf("the carry_over of a review of the old revision: %+v (%v)", r, err)
	}
	g.must(evaluate(effectEnv, gen.Merge))
	if st := g.state(gen.Merge); st.State != types.NodeStateStateWaiting {
		t.Fatalf("the merge of a demand for the replacement is %s", st.State)
	}
}

// While a merge's operation is live, nothing it consumed moves: no
// supersession moves its subject's revision, whichever review it replaces;
// its requirement, carry_over and carried review are not invalidated, by
// command or raw event. Once the operation settles, they move.
func TestMergeInputsFenced(t *testing.T) {
	g := openEffectStore(t)
	f := fakeforge.New()
	f.Move("main", "B1")
	gen := g.claimedMerge("main", "B1")
	spec := g.specOf(gen)
	// Another review of the subject, unclaimed by any carry_over.
	other := g.nodeOn(types.NodeKindReview, spec.Candidate)
	other.Node.Inputs[0].Source.Variant.(*types.ResultSource).ResultKind = types.ResultKindPatch
	other.Node.Inputs = append(other.Node.Inputs, g.planInput("again"))
	otherReview := g.record(other)
	op := g.mustOperate(gen.Merge)
	cur, _ := view[revision](g, RequirementProjection, requirementIndex(tskID, "candidate"))
	next := g.record(g.requirementNode(cur.Revision+1, "1", exeB))
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return claimAny(tx, next, map[string]Resolved{}, 1)
	})
	fenced := func(what string, err error, fold bool) {
		t.Helper()
		var live ErrMergeLive
		if !errors.As(err, &live) || live.Merge != gen.Merge || fold != strings.Contains(err.Error(), "graph: event") {
			t.Errorf("%s: %v (want a fence, by the %s)", what, err, map[bool]string{true: "fold", false: "builder"}[fold])
		}
	}
	for name, review := range map[string]types.NodeID{"the carried review": spec.Review, "another review of the subject": otherReview} {
		fenced(name+" superseded", g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return Supersede(tx, review, next, reviewers)
		}), false)
		fenced(name+" superseded by a raw event", g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			rec, err := loadNode(tx, review)
			n := rec.Recorded.Node
			n.ID, n.Params, n.Inputs = types.NodeID(must(ids.New("nod"))), reviewParams(exeB), slices.Clone(n.Inputs)
			for i, in := range n.Inputs {
				if src, ok := in.Source.Variant.(*types.ResultSource); ok && src.ResultKind == types.ResultKindReviewRequirement {
					n.Inputs[i].Source.Variant = &types.ResultSource{Type: "result", Producer: next, ResultKind: types.ResultKindReviewRequirement}
				}
			}
			evs, eerr := event(ReviewSuperseded, string(review), prjID, &types.ReviewSuperseded{Type: "review_superseded", Predecessor: review, Replacement: n,
				StoppedAttempts: []types.AttemptID{}, TransferredHolds: []types.HoldID{}, RetiredHolds: []types.RetiredHold{}})
			return evs, errors.Join(err, eerr)
		}), true)
	}
	for name, n := range map[string]types.NodeID{"the requirement": producerOf(g.recordOf(gen.Merge).Node, "requirement"), "the carry_over": gen.CarryOver,
		"the carried review": spec.Review, "the rebase": gen.Rebase} {
		fenced(name+" invalidated", g.do(invalidate(n)), false)
		fenced(name+" invalidated by a raw event", g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			c, _, err := load[claim](tx, ClaimProjection, string(n))
			rec, rerr := loadNode(tx, n)
			key, kerr := Key(rec.Recorded.Node, resolutions(c.Inputs), c.Key.Generation+1)
			if err = errors.Join(err, rerr, kerr); err != nil {
				return nil, err
			}
			return event(NodeInvalidated, string(n), prjID, &types.NodeInvalidated{Type: "node_invalidated", Node: n, Key: key})
		}), true)
	}
	if e, err := g.runEffect(op, f); err != nil || e.State != eventstore.EffectCompleted || len(f.Pushed("main")) != 1 {
		t.Fatalf("the fenced merge: %+v (%v)", e, err)
	}
	// Settled: the review may be superseded, and the requirement re-run.
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return nil, CompleteEffect(tx, gen.Merge, g.blobs)
	})
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return Supersede(tx, otherReview, next, reviewers)
	})
}

// A pending effect that applies nothing fails, confirmed, only when its
// target proves it never landed; an apply that landed (a push whose
// observation was lost, then moved over), or a target that cannot tell,
// leaves it uncertain, its target still fenced.
func TestUnappliedNeedsLandedProof(t *testing.T) {
	f := fakeforge.New()
	f.Move("main", "B1")
	if _, err := f.Apply("main", "eff_landed", "W", "B1"); err != nil {
		t.Fatal(err)
	}
	f.Move("main", "B2")
	for name, c := range map[string]struct {
		target EffectTarget
		effect string
		want   string
	}{
		"never landed": {f, "eff_other", "failed"},
		"landed":       {f, "eff_landed", "uncertain"},
		"cannot tell":  {blindTarget{f}, "eff_other", "uncertain"},
	} {
		var got eventstore.Transition
		record := func(tr eventstore.Transition) (eventstore.Effect, error) { got = tr; return eventstore.Effect{}, nil }
		cause := errors.New("stale")
		if _, err := unapplied(record, c.target, Operation{Effect: c.effect, Request: EffectRequest{Target: "main"}}, "inputs moved", cause); !errors.Is(err, cause) || got.Kind != c.want {
			t.Errorf("%s: %s (%v), want %s", name, got.Kind, err, c.want)
		}
	}
}

// blindTarget cannot tell what landed.
type blindTarget struct{ *fakeforge.Forge }

func (blindTarget) Landed(string, string) (bool, error) { return false, errors.New("no push log") }

// A requirement is matched by what it requires, not by the result that
// recorded it: a review that took an identical requirement node's result,
// or the requirement's result before it was re-run, still carries over.
func TestCarryOverSameRequirement(t *testing.T) {
	for _, rerun := range []bool{false, true} {
		g := openEffectStore(t)
		patch := g.ref(types.RefTypeBlob, "P")
		cand := g.candidate(patch)
		first := g.defaultRequirement()
		review := g.consumerOf(cand, types.ResultKindPatch)
		if !rerun { // an identical requirement node, selected again, binds the review
			twin, _, err := g.completeRequirement(g.requirementNode(1, "1", exeA), nil)
			if err != nil {
				t.Fatal(err)
			}
			if cur, _ := view[revision](g, RequirementProjection, requirementIndex(tskID, "candidate")); cur.Node != first {
				t.Fatalf("an identical requirement moved the revision's node to %s", cur.Node)
			}
			rec := g.recordOf(review)
			rec.Node.ID, rec.Node.Inputs = types.NodeID(must(ids.New("nod"))), slices.Clone(rec.Node.Inputs)
			rec.Node.Inputs[1] = requiredOn(twin)
			g.must(func(*eventstore.Tx) ([]eventstore.Event, error) { return recordAny(rec) })
			review = rec.Node.ID
		}
		g.must(evaluate(effectEnv, review))
		g.must(commit(review, g.reviewResult(review), effectEnv))
		if rerun { // the requirement runs again after the review took it
			g.must(invalidate(first))
			r := unrecorded(t, *g.state(first).Key)
			r.Kind, r.Value, r.Conclusions, r.Outcome, r.Reason, r.Detail = types.ResultKindReviewRequirement, []types.Ref{}, ptr(g.recordOf(first).Node.Params), types.OutcomeFound, vocab.ReasonCompletedWithFindings, "required"
			g.must(raw(first, r, g.blobs))
		}
		gen, err := g.generate(QueueSpec{Task: tskID, Project: prjID, Branch: "main", Candidate: cand, Review: review}, "B1")
		if err != nil {
			t.Fatal(err)
		}
		g.rebased(gen, g.ref(types.RefTypeGitTree, "T"), patch)
		if r, err := g.carryOver(gen.CarryOver); err != nil || r.Outcome != types.OutcomeEmpty {
			t.Fatalf("rerun %t: the carry_over: %+v (%v)", rerun, r, err)
		}
	}
}

// A carry_over whose reused review's holder ran again after the claim is
// refused as a commit is, ErrRevalidated (stale_expected_state), not by a
// role error: the carry_over is invalidated, and claims the fresh result.
func TestCarryOverReusedHolderRerun(t *testing.T) {
	g := openEffectStore(t)
	patch := g.ref(types.RefTypeBlob, "P")
	spec := g.queueSpec("main", patch)
	like := g.recordOf(spec.Review)
	like.Node.ID = types.NodeID(must(ids.New("nod")))
	reused := g.record(like)
	g.must(evaluate(g.env(noRequirement), reused))
	holder := spec.Review
	spec.Review = reused
	gen, err := g.generate(spec, "B1")
	if err != nil {
		t.Fatal(err)
	}
	g.rebased(gen, g.ref(types.RefTypeGitTree, "T"), patch)
	g.must(invalidate(holder)) // releases the reuse
	g.must(commit(holder, g.reviewResult(holder), effectEnv))
	var stale ErrRevalidated
	if _, err := g.carryOver(gen.CarryOver); !errors.As(err, &stale) || stale.Reason != vocab.ReasonStaleExpectedState {
		t.Fatalf("the carry_over of a re-run holder: %v", err)
	}
}

// An uncertain merge fences its inputs as a pending one does; a merge of
// another task fences nothing of this one.
func TestMergeFenceScope(t *testing.T) {
	g := openEffectStore(t)
	gen := g.claimedMerge("main", "B1")
	g.mustOperate(gen.Merge)
	g.uncertain()
	if err := g.do(invalidate(gen.CarryOver)); !errors.As(err, new(ErrMergeLive)) {
		t.Fatalf("an input of an uncertain merge invalidated: %v", err)
	}
	// Task B's subject candidate moves, under task A's live merge.
	plan, planRes := g.planOf(tskB, prjB)
	req := g.requirementNode(1, "1", exeA)
	req.Task, req.Project = tskB, prjB
	first, res, err := g.completeRequirement(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	review := g.nodeOn(types.NodeKindReview, plan)
	review.Task, review.Project = tskB, prjB
	review.Node.Inputs[1] = requiredOn(first)
	rid := g.record(review)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return claimAny(tx, rid, map[string]Resolved{"plan": {Result: planRes}, "required": {Result: res}}, 0)
	})
	next := g.requirementNode(2, "1", exeB)
	next.Task, next.Project = tskB, prjB
	n := g.record(next)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return claimAny(tx, n, map[string]Resolved{}, 1) })
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Supersede(tx, rid, n, reviewers) })
}

// A queue generation takes only its own task's nodes: a candidate or a
// review of another task of the project is refused, by Generate and by
// the rebase and carry_over folds, so the task-scoped fences see every
// live merge.
func TestQueueTaskLocal(t *testing.T) {
	g := openEffectStore(t)
	patch := g.ref(types.RefTypeBlob, "P")
	spec := g.queueSpec("main", patch)
	inC := func(kind types.NodeKind, producer types.NodeID, rk types.ResultKind) types.NodeID {
		rec := g.nodeOn(kind, producer)
		rec.Task = tskC
		rec.Node.Inputs[0].Source.Variant.(*types.ResultSource).ResultKind = rk
		return g.record(rec)
	}
	// Task C's review of A's candidate, and C's own candidate.
	foreignReview := inC(types.NodeKindReview, spec.Candidate, types.ResultKindPatch)
	g.must(evaluate(effectEnv, foreignReview))
	g.must(commit(foreignReview, g.reviewResult(foreignReview), effectEnv))
	foreignCand := inC(types.NodeKindBuild, g.plan, types.ResultKindPlan)
	g.must(evaluate(effectEnv, foreignCand))
	r := unrecorded(t, *g.state(foreignCand).Key)
	r.Value = []types.Ref{patch}
	g.must(commit(foreignCand, r, effectEnv))
	for name, s := range map[string]QueueSpec{
		"a foreign review":    {Task: tskID, Project: prjID, Branch: "main", Candidate: spec.Candidate, Review: foreignReview},
		"a foreign candidate": {Task: tskID, Project: prjID, Branch: "main", Candidate: foreignCand, Review: spec.Review},
	} {
		if _, err := g.generate(s, "B1"); err == nil || !strings.Contains(err.Error(), "own task") || strings.Contains(err.Error(), "graph: event") {
			t.Errorf("Generate with %s, refused by the builder: %v", name, err)
		}
	}
	gen, err := g.generate(spec, "B1")
	if err != nil {
		t.Fatal(err)
	}
	raw := func(n types.NodeRecorded, edit func(*types.NodeRecorded)) error {
		n.Node.ID, n.Node.Inputs = types.NodeID(must(ids.New("nod"))), slices.Clone(n.Node.Inputs)
		edit(&n)
		return g.do(forge(NodeRecorded, string(n.Node.ID), prjID, &n))
	}
	swap := func(name string, producer types.NodeID, kind types.ResultKind) func(*types.NodeRecorded) {
		return func(n *types.NodeRecorded) {
			for i, in := range n.Node.Inputs {
				if in.Name == types.Name(name) {
					n.Node.Inputs[i].Source.Variant = &types.ResultSource{Type: "result", Producer: producer, ResultKind: kind}
				}
			}
		}
	}
	if err := raw(g.recordOf(gen.CarryOver), swap("review", foreignReview, types.ResultKindReview)); err == nil || !strings.Contains(err.Error(), "own task") {
		t.Errorf("a raw carry_over of a foreign review: %v", err)
	}
	if err := raw(g.recordOf(gen.Rebase), func(n *types.NodeRecorded) {
		swap("candidate", foreignCand, types.ResultKindPatch)(n)
		n.Node.Params = json.RawMessage(`{"branch":"main","base":"B2","version":` + strconv.FormatInt(gen.Version+1, 10) + `,"candidate":"` + string(r.ID) + `","review":"` + string(gen.Review) +
			`","review_result":"` + string(gen.Verdict) + `","requirement":"` + string(gen.Requirement) + `","generation":2}`)
	}); err == nil || !strings.Contains(err.Error(), "own task") {
		t.Errorf("a raw rebase of a foreign candidate: %v", err)
	}
}
