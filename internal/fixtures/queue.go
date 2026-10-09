// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/forge"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/owedanswers"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// queueFix is a graph fixture with a reviewed candidate to merge onto main
// on the fake forge.
type queueFix struct {
	*graphFix
	forge *forge.Forge
	spec  graph.QueueSpec
}

func (q *queueFix) env() graph.Env {
	return graph.Env{Need: noNeed, Blobs: q.blobs, Authority: owedanswers.Authority{}}
}

// ref publishes content and seals a ref of typ to it.
func (q *queueFix) ref(typ types.RefType, content string) (types.Ref, error) {
	d, err := q.blobs.Put(strings.NewReader(content), "")
	if err != nil {
		return types.Ref{}, err
	}
	return knowledge.Seal(types.Ref{Type: typ, ID: "fixture-" + content, Scope: types.RefScope{Project: ptrTo(gProject)},
		Lineage: []string{}, Restrictions: []types.Restriction{}, ContentDigest: types.Digest(d)})
}

// openQueue opens the store in dir with a candidate whose patch is patch's
// blob, and its review.
func openQueue(ctx context.Context, dir, patch string) (*queueFix, types.Ref, func() error, error) {
	g, closeFn, err := openGraphWith(ctx, dir, eventstore.EffectKinds{"merge": {}}) // reconciled first, never blindly retried
	if err != nil {
		return nil, types.Ref{}, nil, err
	}
	q := &queueFix{graphFix: g, forge: forge.New(), spec: graph.QueueSpec{Task: gTask, Project: gProject, Branch: "main"}}
	p, err := q.ref(types.RefTypeBlob, patch)
	if err == nil {
		q.spec.Candidate, _, err = g.complete(types.NodeKindBuild, json.RawMessage(`{}`), []types.InputSpec{}, map[string]graph.Resolved{}, types.ResultKindPatch, []types.Ref{p}, nil)
	}
	if err == nil {
		var res types.ResultID
		st, _, serr := g.node(q.spec.Candidate)
		if err = serr; err == nil && st.Result != nil {
			res = *st.Result
		}
		// The review took the candidate's patch (13: its verdict is G7b-2's).
		q.spec.Review, _, err = g.complete(types.NodeKindReview, soleReviewer, []types.InputSpec{{Name: "patch",
			Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: q.spec.Candidate, ResultKind: types.ResultKindPatch}}}},
			map[string]graph.Resolved{"patch": {Result: res}}, types.ResultKindReview, []types.Ref{}, nil)
	}
	if err != nil {
		return nil, types.Ref{}, nil, errors.Join(err, closeFn())
	}
	return q, p, closeFn, nil
}

// generate starts the generation the forge's base head calls for, if any:
// the step of its command, and the generation (none: number 0).
func (q *queueFix) generate(o *observe) (gen graph.Generation, err error) {
	head, version := q.forge.Observe(q.spec.Branch)
	return q.generateAt(o, graph.Observation{Head: head, Version: version})
}

// generateAt is generate on obs, an observation made earlier.
func (q *queueFix) generateAt(o *observe, obs graph.Observation) (gen graph.Generation, err error) {
	err = q.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, g, err := graph.Generate(tx, q.spec, obs)
		gen = g
		return evs, err
	})
	if !errors.As(err, new(graph.ErrStaleObservation)) {
		err = o.step(err)
	}
	return gen, err
}

// rebase claims gen's rebase node and commits its result holding value.
func (q *queueFix) rebase(gen graph.Generation, value ...types.Ref) error {
	err := q.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Evaluate(tx, []types.NodeID{gen.Rebase}, q.env())
	})
	st, _, nerr := q.node(gen.Rebase)
	id, ierr := ids.New("res")
	if err = errors.Join(err, nerr, ierr); err != nil || st.Key == nil {
		return errors.Join(err, errors.New("the rebase is not claimed"))
	}
	r := types.Result{ID: types.ResultID(id), Computation: st.Key.Key, Kind: types.ResultKindTree, Value: value,
		Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Completed: true}
	if len(value) == 3 { // conflicts found, and resolved
		r.Outcome, r.Reason, r.Detail = types.OutcomeFound, vocab.ReasonCompletedWithFindings, "conflicts resolved"
	}
	return q.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return nil, graph.Commit(tx, gen.Rebase, r, q.env())
	})
}

// carry claims gen's carry_over node and commits its result: the step.
func (q *queueFix) carry(o *observe, gen graph.Generation) (r types.Result, err error) {
	err = q.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Evaluate(tx, []types.NodeID{gen.CarryOver}, q.env())
	})
	if err == nil {
		err = q.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			var cerr error
			r, cerr = graph.CarryOver(tx, gen.CarryOver, q.env())
			return nil, cerr
		})
	}
	if err == nil {
		o.steps = append(o.steps, Step{vocab.Outcome(r.Outcome), r.Reason.String()})
	}
	return r, err
}

// baseMoveStartsAGeneration: a base move mid-generation starts a new
// generation (13 "Merge-queue generations"). Generation 1 pins B1 and its
// rebase completes; the base moves to B2 outside the controller, so the
// template records generation 2 on B2; B2 again starts nothing. Generation
// 1's results stay, history.
func baseMoveStartsAGeneration(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	q, patch, closeFn, err := openQueue(ctx, dir, "patch P")
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	var o observe
	q.forge.Move("main", "B1")
	gen1, err := q.generate(&o)
	tree, terr := q.ref(types.RefTypeGitTree, "tree on B1")
	if err = errors.Join(err, terr); err == nil {
		err = q.rebase(gen1, tree, patch)
	}
	if err != nil {
		return nil, nil, err
	}
	q.forge.Move("main", "B2") // mid-generation, outside the controller
	gen2, err := q.generate(&o)
	var gen3 graph.Generation
	if err == nil {
		gen3, err = q.generate(&o)
	}
	st, _, nerr := q.node(gen1.Rebase)
	if err = errors.Join(err, nerr); err != nil {
		return nil, nil, err
	}
	o.check(gen1.Number == 1 && gen1.Base == "B1", "the first generation is %d on %s", gen1.Number, gen1.Base)
	o.check(gen2.Number == 2 && gen2.Base == "B2" && gen2.Rebase != gen1.Rebase, "the base move started generation %d on %q, want 2 on B2", gen2.Number, gen2.Base)
	o.check(gen3.Number == 0, "the same base started generation %d", gen3.Number)
	o.check(st.State == types.NodeStateStateCompleted, "generation 1's rebase is %s, not completed history", st.State)
	return o.steps, o.violations, nil
}

// carryOverNeedsEqualPatch: carry-over applies only with an equal patch
// identity (13 "Merge-queue generations"). On B1 the rebased patch is the
// reviewed one: an attestation. On B2 it differs, and on B3 it is equal but
// the rebase resolved conflicts: each demands a delta review.
func carryOverNeedsEqualPatch(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	q, patch, closeFn, err := openQueue(ctx, dir, "patch P")
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	var o observe
	for _, c := range []struct{ base, patch, resolution string }{{"B1", "", ""}, {"B2", "patch P rebased differently", ""}, {"B3", "", "resolution"}} {
		q.forge.Move("main", c.base)
		gen, err := q.generate(&observe{}) // the steps are the carry-overs
		tree, terr := q.ref(types.RefTypeGitTree, "tree on "+c.base)
		value := []types.Ref{tree, patch}
		if c.patch != "" {
			value[1], terr = q.ref(types.RefTypeBlob, c.patch)
		}
		if c.resolution != "" {
			var res types.Ref
			res, terr = q.ref(types.RefTypeBlob, c.resolution+" on "+c.base)
			value = append(value, res)
		}
		var r types.Result
		if err = errors.Join(err, terr); err == nil {
			err = q.rebase(gen, value...)
		}
		if err == nil {
			r, err = q.carry(&o, gen)
		}
		if err != nil {
			return nil, nil, err
		}
		if r.Outcome == types.OutcomeEmpty {
			o.check(len(r.Value) == 1 && r.Value[0].ContentDigest == tree.ContentDigest, "the attestation on %s does not bind its tree", c.base)
		}
	}
	return o.steps, o.violations, nil
}

// staleObservationStartsNothing: an observation older than the current
// generation's starts no generation (13 "Merge-queue generations"). A
// caller observes A and pauses; another observes B and starts generation
// 2; the delayed observation of A is refused, while a genuine return to A,
// observed later, starts generation 3.
func staleObservationStartsNothing(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	q, _, closeFn, err := openQueue(ctx, dir, "patch P")
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	var o observe
	q.forge.Move("main", "A")
	head, version := q.forge.Observe("main")
	atA := graph.Observation{Head: head, Version: version}
	gen1, err := q.generateAt(&o, atA)
	q.forge.Move("main", "B")
	var gen2, stale, gen3 graph.Generation
	if err == nil {
		gen2, err = q.generate(&o)
	}
	if err == nil {
		stale, err = q.generateAt(&observe{}, atA)
	}
	staleErr := err
	q.forge.Move("main", "A")
	if errors.As(staleErr, new(graph.ErrStaleObservation)) || staleErr == nil {
		gen3, err = q.generate(&o)
	}
	if err != nil {
		return nil, nil, err
	}
	o.check(gen1.Number == 1 && gen2.Number == 2, "A then B started generations %d and %d", gen1.Number, gen2.Number)
	o.check(staleErr != nil && stale.Number == 0, "the delayed observation of A started generation %d", stale.Number)
	o.check(gen3.Base == "A" && gen3.Number == map[bool]int64{true: 3, false: 4}[staleErr != nil], "the genuine return to A started generation %d", gen3.Number)
	return o.steps, o.violations, nil
}

// rosterChangeMakesMergeWait: a roster change after carry-over was
// selected makes the merge wait (13 "Merge-queue generations"; plan 15
// G7b). Generation 1's carry_over attests the review under requirement
// revision 1 and its merge is claimed; the roster changes (another review
// of the subject is superseded under revision 2, so the carried review
// stays completed and go, G7b-2b's verdict); the merge's intent
// re-validates against the subject's selected revision and is refused, so
// nothing is pushed. The base moves: generation 2's carry_over demands the
// replacement review, and its merge waits.
func rosterChangeMakesMergeWait(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	q, patch, closeFn, err := openQueue(ctx, dir, "patch P")
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	var o observe
	evaluate := func(n types.NodeID) error {
		return q.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Evaluate(tx, []types.NodeID{n}, q.env())
		})
	}
	attest := func(base string) (graph.Generation, error) {
		q.forge.Move("main", base)
		gen, err := q.generate(&observe{})
		tree, terr := q.ref(types.RefTypeGitTree, "tree on "+base)
		if err = errors.Join(err, terr); err == nil {
			err = q.rebase(gen, tree, patch)
		}
		if st, _, serr := q.node(gen.CarryOver); errors.Join(err, serr, evaluate(gen.CarryOver)) == nil && st.Key == nil {
			if st, _, _ = q.node(gen.CarryOver); st.Key != nil { // its review serves it
				_, err = q.carry(&o, gen)
			}
		}
		if err == nil {
			err = evaluate(gen.Merge)
		}
		return gen, err
	}
	gen1, err := attest("B1")
	st, _, serr := q.node(gen1.Merge)
	if err = errors.Join(err, serr); err != nil {
		return nil, nil, err
	}
	o.check(st.State == types.NodeStateStateClaimed, "generation 1's merge is %s, not claimed on its attestation", st.State)
	byB, rerr := rosterOf(exeB)
	var cand types.ResultID
	if st, _, serr := q.node(q.spec.Candidate); errors.Join(rerr, serr) == nil && st.Result != nil {
		cand = *st.Result
	}
	other, _, err := q.complete(types.NodeKindReview, soleReviewer, append(on("patch", q.spec.Candidate, types.ResultKindPatch), on("plan", q.plan, types.ResultKindPlan)...),
		map[string]graph.Resolved{"patch": {Result: cand}, "plan": {Result: q.planRes}}, types.ResultKindReview, []types.Ref{}, nil)
	if err == nil {
		_, err = q.supersede(other, byB) // the roster change
	}
	if err = errors.Join(err, rerr); err != nil {
		return nil, nil, err
	}
	err = q.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		_, err := graph.Operate(tx, gen1.Merge, q.env())
		return nil, err
	})
	var refused graph.ErrRevalidated
	switch {
	case errors.As(err, &refused):
		o.steps = append(o.steps, Step{vocab.Failed, refused.Reason.String()})
	case err == nil:
		o.steps = append(o.steps, Step{vocab.Empty, vocab.ReasonCompletedClean.String()})
	default:
		return nil, nil, err
	}
	gen2, err := attest("B2")
	st2, _, serr := q.node(gen2.Merge)
	operated, cerr := q.count(graph.NodeOperated)
	if err = errors.Join(err, serr, cerr); err != nil {
		return nil, nil, err
	}
	o.check(st2.State == types.NodeStateStateWaiting, "generation 2's merge is %s: it waits for the replacement review", st2.State)
	o.check(operated == 0 && len(q.forge.Pushed("main")) == 0, "%d operations were intended and %d pushes landed after the roster change", operated, len(q.forge.Pushed("main")))
	return o.steps, o.violations, nil
}

// attested starts the generation the forge's head calls for, rebases it
// cleanly onto the tree on base and commits its carry_over: its step.
func (q *queueFix) attested(o *observe, base string, patch types.Ref) (graph.Generation, error) {
	gen, err := q.generate(&observe{})
	tree, terr := q.ref(types.RefTypeGitTree, "tree on "+base)
	if err = errors.Join(err, terr); err == nil && gen.Number == 0 {
		return gen, nil
	}
	if err == nil {
		err = q.rebase(gen, tree, patch)
	}
	if err == nil {
		_, err = q.carry(o, gen)
	}
	return gen, err
}

// merge claims gen's merge, intends its operation and runs it on the
// forge: the effect's state ("": the merge is not claimed). A completed
// merge records its result.
func (q *queueFix) merge(gen graph.Generation) (eventstore.EffectState, error) {
	err := q.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Evaluate(tx, []types.NodeID{gen.Merge}, q.env())
	})
	st, _, serr := q.node(gen.Merge)
	if err = errors.Join(err, serr); err != nil || st.State != types.NodeStateStateClaimed {
		return "", err
	}
	var op graph.Operation
	if err := q.do(func(tx *eventstore.Tx) (_ []eventstore.Event, err error) {
		op, err = graph.Operate(tx, gen.Merge, q.env())
		return nil, err
	}); err != nil {
		return "", err
	}
	e, err := graph.RunEffect(q.ctx, q.s, q.actor, op, q.forge)
	if errors.As(err, new(graph.ErrTargetMoved)) {
		err = nil // the effect's state says what happened
	}
	if err == nil && e.State == eventstore.EffectCompleted {
		err = q.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return nil, graph.CompleteEffect(tx, gen.Merge, q.blobs)
		})
	}
	return e.State, err
}

// mergeStep is the step of a merge that ended in state: completed, failed
// over a moved base, or waiting for its review.
func mergeStep(state eventstore.EffectState) Step {
	switch state {
	case eventstore.EffectCompleted:
		return Step{vocab.Empty, vocab.ReasonCompletedClean.String()}
	case eventstore.EffectFailedConfirmed:
		return Step{vocab.Failed, vocab.ReasonStaleExpectedState.String()}
	}
	return Step{vocab.Found, vocab.ReasonReviewInputChanged.String()}
}

// baseMoveFailsTheMergeCAS: if the base moves before the merge commits,
// the compare-and-set fails and a new generation starts from the new base
// (13 "Merge-queue generations"). Generation 1's merge is intended over
// B1; the base moves to B2 outside the controller before the apply, so
// nothing is pushed and its effect fails, confirmed; generation 2 merges
// over B2.
func baseMoveFailsTheMergeCAS(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	q, patch, closeFn, err := openQueue(ctx, dir, "patch P")
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	var o observe
	q.forge.Move("main", "B1")
	gen1, err := q.attested(&observe{}, "B1", patch)
	var state eventstore.EffectState
	if err == nil {
		err = q.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Evaluate(tx, []types.NodeID{gen1.Merge}, q.env())
		})
	}
	var op graph.Operation
	if err == nil {
		err = o.step(q.do(func(tx *eventstore.Tx) (_ []eventstore.Event, err error) {
			op, err = graph.Operate(tx, gen1.Merge, q.env())
			return nil, err
		}))
	}
	if err != nil {
		return nil, nil, err
	}
	q.forge.Move("main", "B2") // after the intent, before the apply
	e, err := graph.RunEffect(ctx, q.s, q.actor, op, q.forge)
	if err != nil && !errors.As(err, new(graph.ErrTargetMoved)) {
		return nil, nil, err
	}
	o.steps = append(o.steps, mergeStep(e.State))
	gen2, err := q.attested(&observe{}, "B2", patch)
	if err == nil {
		state, err = q.merge(gen2)
	}
	st1, _, serr := q.node(gen1.Merge)
	if err = errors.Join(err, serr); err != nil {
		return nil, nil, err
	}
	o.steps = append(o.steps, mergeStep(state))
	head, _ := q.forge.Holds("main")
	o.check(gen2.Number == 2 && gen2.Base == "B2", "the base move started generation %d on %s", gen2.Number, gen2.Base)
	o.check(len(q.forge.Pushed("main")) == map[bool]int{true: 1, false: 2}[e.State != eventstore.EffectCompleted], "the forge took pushes %v", q.forge.Pushed("main"))
	o.check(head != "B2", "main still holds B2: the next generation did not merge")
	o.check(st1.Result == nil, "generation 1's merge recorded a result")
	return o.steps, o.violations, nil
}

// replacementReviewStartsAGeneration: the merge waits for the replacement
// review after a roster change, and goes with it (13 "Merge-queue
// generations"; "Replacement is a recorded supersession"). Generation 1
// attests the review under requirement revision 1; the roster changes and
// the replacement review is made under revision 2; on the unmoved base, a
// generation of its own carries it over and merges.
func replacementReviewStartsAGeneration(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	q, patch, closeFn, err := openQueue(ctx, dir, "patch P")
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	var o observe
	q.forge.Move("main", "B1")
	gen1, err := q.attested(&o, "B1", patch)
	byB, rerr := rosterOf(exeB)
	if err = errors.Join(err, rerr); err == nil {
		q.spec.Review, err = q.supersede(q.spec.Review, byB) // the roster change: the replacement, under revision 2
	}
	var cand types.ResultID
	if st, _, serr := q.node(q.spec.Candidate); errors.Join(err, serr) == nil && st.Result != nil {
		cand = *st.Result
	}
	if err == nil { // the replacement runs
		err = q.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.ClaimReview(tx, q.spec.Review, q.withRequired(q.spec.Review, map[string]graph.Resolved{"patch": {Result: cand}}), 0, reviewers)
		})
	}
	if err == nil {
		_, err = q.selectResult(q.spec.Review, types.ResultKindReview, []types.Ref{}, nil)
	}
	var gen2 graph.Generation
	var state eventstore.EffectState
	if err == nil {
		gen2, err = q.attested(&observe{}, "B1", patch)
	}
	if err == nil && gen2.Number != 0 {
		state, err = q.merge(gen2)
	}
	st1, _, serr := q.node(gen1.Merge)
	if err = errors.Join(err, serr); err != nil {
		return nil, nil, err
	}
	o.steps = append(o.steps, mergeStep(state))
	o.check(st1.Result == nil, "generation 1's merge, of the replaced review, recorded a result")
	o.check(gen2.Number == 0 || gen2.Base == "B1" && gen2.Review == q.spec.Review, "the replacement's generation is %+v", gen2)
	return o.steps, o.violations, nil
}
