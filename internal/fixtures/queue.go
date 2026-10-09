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
// revision 1 and its merge is claimed; the roster changes (the review is
// superseded under revision 2); the merge's intent re-validates against
// the subject's selected revision and is refused, so nothing is pushed.
// The base moves: generation 2's carry_over of the superseded review, and
// its merge, wait for the replacement.
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
	if _, err = q.supersede(q.spec.Review, byB); errors.Join(err, rerr) != nil { // the roster change
		return nil, nil, errors.Join(err, rerr)
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
	co2, _, cerr2 := q.node(gen2.CarryOver)
	err = errors.Join(err, cerr2)
	o.check(co2.State == types.NodeStateStateWaiting, "generation 2's carry_over of the superseded review is %s", co2.State)
	st2, _, serr := q.node(gen2.Merge)
	operated, cerr := q.count(graph.NodeOperated)
	if err = errors.Join(err, serr, cerr); err != nil {
		return nil, nil, err
	}
	o.check(st2.State == types.NodeStateStateWaiting, "generation 2's merge is %s: it waits for the replacement review", st2.State)
	o.check(operated == 0 && len(q.forge.Pushed("main")) == 0, "%d operations were intended and %d pushes landed after the roster change", operated, len(q.forge.Pushed("main")))
	return o.steps, o.violations, nil
}
