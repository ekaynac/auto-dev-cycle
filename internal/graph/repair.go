// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// Repair is the events recording round n+1 of build, a repair (13
// "Successor rounds": "round n is a new build node whose inputs include
// round n−1's review findings and candidate tree"). It is build's
// Successor, whose new inputs are review's selected result (its
// findings), named findings-<n>, and build's own selected patch (the
// candidate), named candidate-<n>, n build's round. review must be a
// review of build's candidate, of its task: its claim fixed build's
// selected patch as an input (not an earlier patch: the build re-ran
// since), and its selected result is no_go (found); a go review, or one
// not selected, repairs nothing. The kinds follow
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
		case src == findings && sel.Result.Outcome != types.OutcomeFound:
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
