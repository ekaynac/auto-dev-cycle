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
// review of build's candidate (it declares build's result as an input), of
// its task, whose selected result is no_go (found); a go review, or one not
// selected, repairs nothing. The kinds follow from the results taken: only
// a build records a patch, and only a review a review result. Rails on
// rounds are P17's.
func Repair(tx *eventstore.Tx, build, review types.NodeID, auth Authority) ([]eventstore.Event, error) {
	b, err := loadNode(tx, build)
	r, rerr := loadNode(tx, review)
	if err = errors.Join(err, rerr); err != nil {
		return nil, err
	}
	takesBuild := slices.ContainsFunc(r.Recorded.Node.Inputs, func(in types.InputSpec) bool {
		src, ok := in.Source.Variant.(*types.ResultSource)
		return ok && src.Producer == build
	})
	if r.Recorded.Task != b.Recorded.Task || !takesBuild {
		return nil, fmt.Errorf("graph: %s does not review %s's candidate", review, build)
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
		}
	}
	n := b.Recorded.Node.Round
	return Successor(tx, build, []types.InputSpec{
		{Name: types.Name(fmt.Sprintf("findings-%d", n)), Source: types.InputSpecSource{Variant: findings}},
		{Name: types.Name(fmt.Sprintf("candidate-%d", n)), Source: types.InputSpecSource{Variant: candidate}},
	}, auth)
}
