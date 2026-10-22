// SPDX-License-Identifier: Apache-2.0

package daemon

// The review nodes, recorded late (P22a-6a; lead/p22a6a-design.md): a
// review's roster is part of its identity (13; graph/roster.go), so the
// candidate template stops at the gate, and the loop records the task's
// review once its gate completed (reconcile.RecordReview), with the roster
// the registry derives then:
//   - one reviewer, the first in id order registered as a reviewer, admitted
//     by the project's data policy, and of a family none of the task's
//     contributors has (graph.Contributors); none such: the first admitted
//     reviewer (else the first reviewer), which graph.ClaimReview refuses as
//     not independent, so the evaluator holds the review
//     reviewer_unavailable; no reviewer at all: nothing recorded (the
//     --registry obligation keeps review inactive there: Config.Validate);
//   - a review_requirement node (no inputs; its params its subject, roster,
//     definition version and revision), and a review node on the plan (the
//     requirement review.Prompt renders), the build's patch (the
//     candidate) and the requirement, each recorded through
//     graph.RecordReview (the registry derives its roster). The gate's
//     result is no input yet: a gate_result input needs its receipt
//     requirement (graph.Resolve, I13), which adcd cannot derive yet; the
//     gate's completion is the recording's precondition (stated; the gate
//     input is the lead's decision, open).
//
// The requirement is the controller's: claimed by the evaluator
// (graph.ClaimReview), its result committed by Require, its conclusions
// exactly its params. A review held reviewer_unavailable clears by itself
// (Unheld, each pass, the Unblock pattern): with the registry now
// (a restart's), the same roster independent clears the hold; another
// independent roster is recorded as the next revision of the review's own
// requirement and the review superseded onto it, in one command
// (graph.SupersedeOnto; Opus 22a-6a H1). A roster that changes after the
// claim is caught at the commit (graph.reviewIndependent, 22a-6c; stated,
// Opus 22a-6a L2).

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// reviewSubject is the subject of a task's review: its candidate.
const reviewSubject = types.Name("candidate")

// reviews is adcd's reconcile.Reviews, over its registry and Env.
type reviews struct {
	reg registry.Registry
	env graph.Env
}

var _ reconcile.Reviews = reviews{}

// Record records gate's task's review nodes (the file's comment): nothing
// when the task has one already, or no reviewer is registered.
func (r reviews) Record(tx *eventstore.Tx, gate types.NodeID) ([]eventstore.Event, error) {
	g, err := graph.ViewOf(tx, gate)
	if err != nil {
		return nil, err
	}
	if g.Node.Kind != types.NodeKindGate || g.State.State != types.NodeStateStateCompleted {
		return nil, fmt.Errorf("daemon: node %s is no completed gate", gate)
	}
	nodes, err := graph.TaskNodes(tx, g.Task)
	if err != nil {
		return nil, err
	}
	producer := map[types.NodeKind]types.NodeID{}
	for _, n := range nodes {
		if n.Node.Kind == types.NodeKindReview || n.Node.Kind == types.NodeKindReviewRequirement {
			return nil, nil // recorded already
		}
		if n.State.State != types.NodeStateStateSuperseded {
			producer[n.Node.Kind] = n.Node.ID
		}
	}
	roster, ok, err := r.roster(tx, g.Task, g.Project)
	if err != nil || !ok {
		return nil, err
	}
	req, err := r.requirement(g.Task, g.Project, roster, 1)
	if err != nil {
		return nil, err
	}
	id, err := ids.New("nod")
	if err != nil {
		return nil, err
	}
	params, err := json.Marshal(types.ReviewNodeParams{Subject: reviewSubject, Roster: roster})
	if err != nil {
		return nil, err
	}
	on := func(name string, p types.NodeID, kind types.ResultKind) types.InputSpec {
		return types.InputSpec{Name: types.Name(name), Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: p, ResultKind: kind}}}
	}
	reviewer, read := types.Name(types.RegistryExecutorRoleReviewer), types.Name("review") // a model kind's role and capability (records/node)
	review := types.NodeRecorded{Task: g.Task, Project: g.Project, Node: types.Node{ID: types.NodeID(id), Kind: types.NodeKindReview,
		DefinitionVersion: graph.TemplateVersion, Round: 1, Params: params,
		Inputs: []types.InputSpec{on("plan", producer[types.NodeKindPlan], types.ResultKindPlan), on("patch", producer[types.NodeKindBuild], types.ResultKindPatch),
			on("required", req.Node.ID, types.ResultKindReviewRequirement)},
		Requires: types.NodeRequires{Role: &reviewer, Capability: &read, Resources: []types.ResourceGroupKind{types.ResourceGroupKindAccountQuota}}}}
	evs, err := graph.RecordReview(req, r.reg)
	if err != nil {
		return nil, err
	}
	more, err := graph.RecordReview(review, r.reg)
	return append(evs, more...), err
}

// requirement is task's review_requirement node at revision, of roster.
func (r reviews) requirement(task types.TaskID, project types.ProjectID, roster []types.RosterMember, revision int64) (types.NodeRecorded, error) {
	id, err := ids.New("nod")
	if err != nil {
		return types.NodeRecorded{}, err
	}
	params, err := json.Marshal(types.ReviewRequirement{Subject: reviewSubject, Roster: roster, DefinitionVersion: graph.TemplateVersion, Revision: revision})
	return types.NodeRecorded{Task: task, Project: project, Node: types.Node{ID: types.NodeID(id), Kind: types.NodeKindReviewRequirement,
		DefinitionVersion: graph.TemplateVersion, Round: 1, Params: params, Inputs: []types.InputSpec{},
		Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}}, err
}

// roster is task's review roster now (the file's comment), and whether it
// is independent of the task's contributors; false with no reviewer
// registered.
func (r reviews) roster(tx *eventstore.Tx, task types.TaskID, project types.ProjectID) ([]types.RosterMember, bool, error) {
	reviewers := r.reg.Executors(types.RegistryExecutorRoleReviewer)
	if len(reviewers) == 0 {
		return nil, false, nil
	}
	prof, found, err := authority.ProjectProfile(tx, project)
	if err != nil {
		return nil, false, err
	}
	var dp types.ProjectProfileDataPolicy
	if found {
		dp = prof.DataPolicy
	}
	pick, admitted := reviewers[0], false
	for _, e := range reviewers {
		if r.reg.Admits(dp, e) == nil {
			if !admitted {
				pick, admitted = e, true
			}
			if ok, err := r.independent(tx, task, e); err != nil || ok {
				roster, rerr := graph.Roster([]types.ExecutorID{e}, r.reg)
				return roster, true, errors.Join(err, rerr)
			}
		}
	}
	roster, err := graph.Roster([]types.ExecutorID{pick}, r.reg)
	return roster, true, err
}

// independent reports whether reviewer e's family is known and none of
// task's contributors' (P16b), all of them known: a contributor of an
// unknown family leaves no reviewer independent of it, as ClaimReview has it.
func (r reviews) independent(tx *eventstore.Tx, task types.TaskID, e types.ExecutorID) (bool, error) {
	contributors, err := graph.Contributors(tx, task, r.reg)
	family := r.reg.Family(e)
	return err == nil && family != receipt.UnknownFamily && !slices.Contains(contributors, receipt.UnknownFamily) && !slices.Contains(contributors, family), err
}

// Require commits node's result, a claimed review requirement's: its
// params, as its conclusions (graph.recordsRequirement).
func (r reviews) Require(tx *eventstore.Tx, node types.NodeID) error {
	v, err := graph.ViewOf(tx, node)
	if err != nil || v.Node.Kind != types.NodeKindReviewRequirement || v.State.State != types.NodeStateStateClaimed || v.State.Key == nil {
		return errors.Join(err, fmt.Errorf("daemon: node %s is no claimed review requirement", node))
	}
	id, err := ids.New("res")
	if err != nil {
		return err
	}
	conclusions := v.Node.Params
	return graph.Commit(tx, node, types.Result{ID: types.ResultID(id), Computation: v.State.Key.Key, Kind: types.ResultKindReviewRequirement,
		Value: []types.Ref{}, Conclusions: &conclusions, Outcome: types.OutcomeFound, Reason: vocab.ReasonCompletedWithFindings, Detail: "required",
		Completed: true}, r.env) // found: a result carrying conclusions (records/result)
}

// Unheld re-derives node's roster, a review held reviewer_unavailable
// (hold, on signal), from the registry now (the file's comment).
func (r reviews) Unheld(tx *eventstore.Tx, node types.NodeID, hold types.HoldID, signal string) ([]eventstore.Event, error) {
	v, err := graph.ViewOf(tx, node)
	if err != nil || v.Node.Kind != types.NodeKindReview {
		return nil, errors.Join(err, fmt.Errorf("daemon: node %s is no review", node))
	}
	p, err := v.Node.ParamsAsReviewNodeParams()
	if err != nil {
		return nil, err
	}
	now, ok, err := r.roster(tx, v.Task, v.Project)
	if err != nil || !ok || len(now) != 1 {
		return nil, err
	}
	if independent, err := r.independent(tx, v.Task, now[0].Executor); err != nil || !independent {
		return nil, err // no independent reviewer yet: the hold stays
	}
	if slices.Equal(now, p.Roster) {
		return graph.Clear(tx, node, hold, signal)
	}
	was, err := heldRequirement(tx, v.Node)
	if err != nil {
		return nil, err
	}
	req, err := r.requirement(v.Task, v.Project, now, was.Revision+1) // its own revision's next, never the task's highest (Opus 22a-6a H1)
	if err != nil {
		return nil, err
	}
	return graph.SupersedeOnto(tx, node, req, r.reg) // one command: no requirement revision is recorded without its review
}

// heldRequirement is the requirement review takes.
func heldRequirement(tx *eventstore.Tx, review types.Node) (types.ReviewRequirement, error) {
	for _, in := range review.Inputs {
		if src, ok := in.Source.Variant.(*types.ResultSource); ok && src.ResultKind == types.ResultKindReviewRequirement {
			n, err := graph.ViewOf(tx, src.Producer)
			if err != nil {
				return types.ReviewRequirement{}, err
			}
			return n.Node.ParamsAsReviewRequirement()
		}
	}
	return types.ReviewRequirement{}, fmt.Errorf("daemon: review %s takes no requirement", review.ID)
}
