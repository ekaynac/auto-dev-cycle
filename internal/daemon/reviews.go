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
//     candidate), the gate's result under the gate requirement the log
//     derives (gateNeed, P22a-6a′) and the requirement, each recorded
//     through graph.RecordReview (the registry derives its roster).
//
// The requirement is the controller's: claimed by the evaluator
// (graph.ClaimReview), its result committed by Require, its conclusions
// exactly its params. A review held reviewer_unavailable, or the
// requirement a live review waits on so held (its claim refused: its
// roster's family no longer the registry's), clears by itself (Unheld,
// each pass, the Unblock pattern): with the registry now (a restart's),
// the same roster independent clears the hold; another independent roster
// is recorded as the next revision of the review's own requirement and the
// review superseded onto it, in one command (graph.SupersedeOnto; Opus
// 22a-6a H1, re-check H1); a held requirement it moves off is retired with
// it (the supersession's fold). No independent roster: the hold stays. A
// roster that changes after the review's claim is caught at the commit
// (graph.reviewIndependent: the run records could_not_run/
// reviewer_not_independent and holds the review reviewer_unavailable,
// 22a-6c; Opus 22a-6a L2).

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

// Record records gate's review nodes (the file's comment): nothing when a
// review takes gate already, or no reviewer is registered.
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
	// Recorded already: a review takes gate (Opus 22a-6a′ L2: keyed by gate,
	// so a later round's gate is reviewed too). shortcut: a later gate's
	// review would record requirement revision 1 again, which the fold
	// refuses; 22a-6d's repair records the next round's review itself.
	for _, n := range nodes {
		if graph.TakesGate(n.Node, gate) {
			return nil, nil
		}
	}
	// The candidate the gate checked: its chain back, gate → accept → build
	// → plan, never the task's last node of each kind (Opus 22a-6a L1).
	accept, err := producerOf(tx, gate, types.ResultKindTree)
	if err != nil {
		return nil, err
	}
	build, err := producerOf(tx, accept, types.ResultKindPatch)
	if err != nil {
		return nil, err
	}
	plan, err := producerOf(tx, build, types.ResultKindPlan)
	if err != nil {
		return nil, err
	}
	roster, ok, err := r.roster(tx, g.Task, g.Project)
	if err != nil || !ok {
		return nil, err
	}
	req, err := r.requirement(g.Task, g.Project, roster, 1)
	if err != nil {
		return nil, err
	}
	review, err := reviewNode(g.Task, g.Project, plan, build, gate, req.Node.ID, roster)
	if err != nil {
		return nil, err
	}
	evs, err := graph.RecordReview(req, r.reg)
	if err != nil {
		return nil, err
	}
	more, err := graph.RecordReview(review, r.reg)
	return append(evs, more...), err
}

// reviewNode is the review of build's candidate (on plan), checked by gate,
// under requirement, rostered on roster: Record's and Repair's.
func reviewNode(task types.TaskID, project types.ProjectID, plan, build, gate, requirement types.NodeID, roster []types.RosterMember) (types.NodeRecorded, error) {
	id, err := ids.New("nod")
	if err != nil {
		return types.NodeRecorded{}, err
	}
	params, err := json.Marshal(types.ReviewNodeParams{Subject: reviewSubject, Roster: roster})
	on := func(name string, p types.NodeID, kind types.ResultKind) types.InputSpec {
		return types.InputSpec{Name: types.Name(name), Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: p, ResultKind: kind}}}
	}
	reviewer, read := types.Name(types.RegistryExecutorRoleReviewer), types.Name("review") // a model kind's role and capability (records/node)
	return types.NodeRecorded{Task: task, Project: project, Node: types.Node{ID: types.NodeID(id), Kind: types.NodeKindReview,
		DefinitionVersion: graph.TemplateVersion, Round: 1, Params: params,
		Inputs: []types.InputSpec{on("plan", plan, types.ResultKindPlan), on("patch", build, types.ResultKindPatch),
			on("gate", gate, types.ResultKindGateResult), on("required", requirement, types.ResultKindReviewRequirement)},
		Requires: types.NodeRequires{Role: &reviewer, Capability: &read, Resources: []types.ResourceGroupKind{types.ResourceGroupKindAccountQuota}}}}, err
}

// Repair records the next round of node's candidate (P22a-6d; 13
// "Successor rounds"), node a completed review whose verdict is no_go or
// unreviewable, or a completed accept that refused the candidate
// (graph.AcceptRefused; #226 entry 164; graph.Repair checks either), in one
// command:
//   - the build's successor, round n+1, on its findings and its candidate
//     (graph.Repair); its builder sees the findings view only
//     (graph.Context; intent.Build);
//   - its accept and gate (graph.Checked, the template's);
//   - and their review, recorded now, before that gate completes (Opus
//     22a-6c L1): it takes the requirement node's review takes, on its
//     roster, so no requirement revision is recorded again. A roster the
//     registry no longer derives is recorded all the same
//     (graph.RecordRoundReview): the review is held reviewer_unavailable at
//     its claim, and Unheld clears it or supersedes it onto an independent
//     roster, so the task is never left stuck unheld (Main, 6d).
//
// An accept's refusal is repaired whether review is active or not: the
// next round's review is recorded only when the refused round has one
// (roundReview), on its requirement and roster.
//
// The task's round rail (rounds_per_task) holds the successor before its
// claim (graph's roundRail).
func (r reviews) Repair(tx *eventstore.Tx, node types.NodeID) ([]eventstore.Event, error) {
	v, err := graph.ViewOf(tx, node)
	if err != nil || v.Node.Kind != types.NodeKindReview && v.Node.Kind != types.NodeKindAccept {
		return nil, errors.Join(err, fmt.Errorf("daemon: node %s is no review nor accept", node))
	}
	build, err := producerOf(tx, node, types.ResultKindPatch)
	if err != nil {
		return nil, err
	}
	evs, err := graph.Repair(tx, build, node, r.env.Authority)
	if err != nil {
		return nil, err
	}
	next := types.NodeID(evs[len(evs)-1].Subject) // Successor's node.recorded is its last event
	checked, err := graph.Checked(v.Task, v.Project, next)
	if err != nil {
		return nil, err
	}
	for _, c := range checked {
		more, err := graph.Record(c)
		if err != nil {
			return nil, err
		}
		evs = append(evs, more...)
	}
	reviewed := node
	if v.Node.Kind == types.NodeKindAccept {
		if reviewed, err = roundReview(tx, v.Task, node); err != nil || reviewed == "" {
			return evs, err // no review of the refused round (review inactive): none of the next
		}
	}
	plan, err := producerOf(tx, reviewed, types.ResultKindPlan)
	required, rerr := producerOf(tx, reviewed, types.ResultKindReviewRequirement)
	if err = errors.Join(err, rerr); err != nil {
		return nil, err
	}
	req, err := graph.ViewOf(tx, required)
	if err != nil {
		return nil, err
	}
	p, err := req.Node.ParamsAsReviewRequirement()
	if err != nil {
		return nil, err
	}
	review, err := reviewNode(v.Task, v.Project, plan, next, checked[1].Node.ID, required, p.Roster)
	if err != nil {
		return nil, err
	}
	more, err := graph.RecordRoundReview(tx, review, r.reg) // a roster the registry no longer gives: held at its claim, never a stalled repair
	return append(evs, more...), err
}

// roundReview is the review, not superseded, of task that takes the gate
// over accept's tree (graph.TakesGate); "" when there is none.
func roundReview(tx *eventstore.Tx, task types.TaskID, accept types.NodeID) (types.NodeID, error) {
	nodes, err := graph.TaskNodes(tx, task)
	if err != nil {
		return "", err
	}
	for _, g := range nodes {
		if g.Node.Kind != types.NodeKindGate {
			continue
		}
		if tree, err := producerOf(tx, g.Node.ID, types.ResultKindTree); err != nil || tree != accept {
			continue // a gate takes one tree, its accept's (Opus #410 L2)
		}
		for _, n := range nodes {
			if graph.TakesGate(n.Node, g.Node.ID) && n.State.State != types.NodeStateStateSuperseded {
				return n.Node.ID, nil
			}
		}
	}
	return "", nil
}

// producerOf is the producer of node's one input of kind.
func producerOf(tx *eventstore.Tx, node types.NodeID, kind types.ResultKind) (types.NodeID, error) {
	v, err := graph.ViewOf(tx, node)
	if err != nil {
		return "", err
	}
	for _, in := range v.Node.Inputs {
		if src, ok := in.Source.Variant.(*types.ResultSource); ok && src.ResultKind == kind {
			return src.Producer, nil
		}
	}
	return "", fmt.Errorf("daemon: node %s takes no %s", node, kind)
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

// Unheld re-derives node's roster, a review or the requirement a review
// waits on held reviewer_unavailable (hold, on signal), from the registry
// now (the file's comment).
func (r reviews) Unheld(tx *eventstore.Tx, node types.NodeID, hold types.HoldID, signal string) ([]eventstore.Event, error) {
	v, err := graph.ViewOf(tx, node)
	if err != nil || v.State.State == types.NodeStateStateSuperseded {
		return nil, err // superseded (a requirement retired) by an action of this pass: nothing to re-derive
	}
	review, roster := node, []types.RosterMember(nil)
	switch v.Node.Kind {
	case types.NodeKindReview:
		p, err := v.Node.ParamsAsReviewNodeParams()
		if err != nil {
			return nil, err
		}
		roster = p.Roster
	case types.NodeKindReviewRequirement: // the claim refused it: its roster's family is no longer the registry's
		p, err := v.Node.ParamsAsReviewRequirement()
		if err == nil {
			review, err = waitingOn(tx, v.Task, node)
		}
		if err != nil {
			return nil, err
		}
		roster = p.Roster
	default:
		return nil, fmt.Errorf("daemon: node %s is no review or review requirement", node)
	}
	now, ok, err := r.roster(tx, v.Task, v.Project)
	if err != nil || !ok || len(now) != 1 {
		return nil, err
	}
	if independent, err := r.independent(tx, v.Task, now[0].Executor); err != nil || !independent {
		return nil, err // no independent reviewer yet: the hold stays
	}
	if slices.Equal(now, roster) {
		return graph.Clear(tx, node, hold, signal)
	}
	rv, err := graph.ViewOf(tx, review)
	if err != nil {
		return nil, err
	}
	was, err := heldRequirement(tx, rv.Node)
	if err != nil {
		return nil, err
	}
	req, err := r.requirement(v.Task, v.Project, now, was.Revision+1) // its own revision's next, never the task's highest (Opus 22a-6a H1)
	if err != nil {
		return nil, err
	}
	return graph.SupersedeOnto(tx, review, req, r.reg) // one command: no requirement revision is recorded without its review
}

// waitingOn is the review of task that is not superseded and takes
// requirement.
func waitingOn(tx *eventstore.Tx, task types.TaskID, requirement types.NodeID) (types.NodeID, error) {
	nodes, err := graph.TaskNodes(tx, task)
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		if n.Node.Kind != types.NodeKindReview || n.State.State == types.NodeStateStateSuperseded {
			continue
		}
		for _, in := range n.Node.Inputs {
			if src, ok := in.Source.Variant.(*types.ResultSource); ok && src.ResultKind == types.ResultKindReviewRequirement && src.Producer == requirement {
				return n.Node.ID, nil
			}
		}
	}
	return "", fmt.Errorf("daemon: no review takes requirement %s", requirement)
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
