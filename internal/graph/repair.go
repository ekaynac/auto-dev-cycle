// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Unreviewable is whether r, a review's result, is the verdict that its
// candidate gives no review context: failed/unsafe_output, the
// candidate's fault (adcd's runner records it with no attempt).
func Unreviewable(r types.Result) bool {
	return r.Kind == types.ResultKindReview && r.Outcome == types.OutcomeFailed && r.Reason == vocab.ReasonUnsafeOutput
}

// RepairVerdict is whether a review's selected result, of outcome and
// reason, asks for a repair: no_go (found), or Unreviewable. The one rule,
// Repair's and the loop's (reconcile.Observe; Opus 22a-6d M1).
func RepairVerdict(outcome types.Outcome, reason types.ReasonCode) bool {
	return outcome == types.OutcomeFound || Unreviewable(types.Result{Kind: types.ResultKindReview, Outcome: outcome, Reason: reason})
}

// LastBuildRound is the last build round of nodes, a task's: the highest
// round of its build nodes, superseded ones included (a repair's build is
// the next round; P22a-6d), 0 with none: OfLastRound's.
func LastBuildRound(nodes []NodeView) int64 {
	last := int64(0)
	for _, n := range nodes {
		if n.Node.Kind == types.NodeKindBuild {
			last = max(last, n.Node.Round)
		}
	}
	return last
}

// OfLastRound reports whether review, a review node of nodes (a task's),
// takes the candidate of the task's last build round: its patch input is
// a build node's of LastBuildRound (a repair's accept, gate and review are
// recorded in round 1 too; only the build's round counts). The one rule
// for "the last round's review": Repairable's (reconcile.Observe) and
// CandidateGo's.
func OfLastRound(nodes []NodeView, review types.Node) bool {
	last := LastBuildRound(nodes)
	return last > 0 && slices.ContainsFunc(review.Inputs, func(in types.InputSpec) bool {
		src, ok := in.Source.Variant.(*types.ResultSource)
		return ok && src.ResultKind == types.ResultKindPatch && slices.ContainsFunc(nodes, func(n NodeView) bool {
			return n.Node.ID == src.Producer && n.Node.Kind == types.NodeKindBuild && n.Node.Round == last
		})
	})
}

// CandidateGo reports whether nodes, a task's, decide its candidate: a
// review of its last build round's candidate (OfLastRound) completed go
// (empty). It is the one rule for the board's done_candidate column and
// for the loop's task.done as a candidate (reconcile.Observe; 22a-7): a
// later round's build, or a NO-GO in the last round, is never a candidate.
func CandidateGo(nodes []NodeView) bool {
	return slices.ContainsFunc(nodes, func(n NodeView) bool {
		return n.Node.Kind == types.NodeKindReview && n.State.State == types.NodeStateStateCompleted && n.Outcome != nil &&
			*n.Outcome == types.OutcomeEmpty && OfLastRound(nodes, n.Node)
	})
}

// Repair is the events recording round n+1 of build, a repair (13
// "Successor rounds": "round n is a new build node whose inputs include
// round n−1's review findings and candidate tree"). It is build's
// Successor, whose new inputs are review's selected result (its
// findings), named findings-<n>, and build's own selected patch (the
// candidate), named candidate-<n>, n build's round. review must be a
// review of build's candidate, of its task: its claim fixed build's
// selected patch as an input (not an earlier patch: the build re-ran
// since), and its selected result is no_go (found), or the verdict that
// the candidate gives no review context (failed/unsafe_output: adcd's
// unreviewable; P22a-6d, Opus 22a-6c L4); a go review, or one not
// selected, repairs nothing. The kinds follow
// from the results taken: only a build records a patch, and only a review
// a review result. Rails on rounds are P17's.
func Repair(tx *eventstore.Tx, build, review types.NodeID, auth Authority) ([]eventstore.Event, error) {
	b, err := loadNode(tx, build)
	r, rerr := loadNode(tx, review)
	if err = errors.Join(err, rerr); err != nil {
		return nil, err
	}
	if r.Recorded.Task != b.Recorded.Task {
		return nil, fmt.Errorf("graph: %s is of another task than %s", review, build)
	}
	c, ok, err := load[claim](tx, ClaimProjection, string(review))
	if err != nil || !ok {
		return nil, errors.Join(err, fmt.Errorf("graph: %s is not claimed", review))
	}
	findings := &types.ResultSource{Type: "result", Producer: review, ResultKind: types.ResultKindReview}
	candidate := &types.ResultSource{Type: "result", Producer: build, ResultKind: types.ResultKindPatch}
	for _, src := range []*types.ResultSource{findings, candidate} {
		sel, why, err := selectedOf(tx, b, src)
		switch {
		case err != nil:
			return nil, err
		case why != "":
			return nil, fmt.Errorf("graph: no repair of %s: %s", build, why)
		case src == findings && !RepairVerdict(sel.Result.Outcome, sel.Result.Reason):
			return nil, fmt.Errorf("graph: no repair of %s: %s's verdict is not no_go (%s)", build, review, sel.Result.Outcome)
		case src == candidate && !slices.ContainsFunc(c.Inputs, func(in types.ClaimedInput) bool { return in.Result != nil && *in.Result == sel.Result.ID }):
			// The review's claim fixed another patch, or none of build's
			// (Astra #291 M2): its findings are not about this candidate.
			return nil, fmt.Errorf("graph: no repair of %s: %s did not review its selected patch %s", build, review, sel.Result.ID)
		}
	}
	n := b.Recorded.Node.Round
	return Successor(tx, build, []types.InputSpec{
		{Name: types.Name(fmt.Sprintf("findings-%d", n)), Source: types.InputSpecSource{Variant: findings}},
		{Name: types.Name(fmt.Sprintf("candidate-%d", n)), Source: types.InputSpecSource{Variant: candidate}},
	}, auth)
}
