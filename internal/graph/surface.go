// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// M1Kinds are the node kinds M1(a)'s template holds: plan, build, accept,
// gate, and the review with its requirement. merge joins them only while a
// merge capability is active (P22b); every other kind's surface stays
// unreachable (00-slice-plan §1, D47; I15).
var M1Kinds = []types.NodeKind{types.NodeKindPlan, types.NodeKindBuild, types.NodeKindAccept, types.NodeKindGate,
	types.NodeKindReview, types.NodeKindReviewRequirement}

// CheckSurface is the deferred-surface guard (I15 admission, P22a) of a
// template: it refuses a node of a kind not allowed, and an Env holding
// instrument contracts (gate contracts live apart, P15; with none,
// ClaimEvidence fails closed). adcd's Projector refuses the same kinds
// when any node is recorded, a template's or not.
func CheckSurface(env Env, nodes []types.Node, allowed []types.NodeKind) error {
	if env.Contracts != nil {
		return fmt.Errorf("graph: deferred surface: the environment holds instrument contracts")
	}
	for _, n := range nodes {
		if !slices.Contains(allowed, n.Kind) {
			return fmt.Errorf("graph: deferred surface: node %s is a %s node", n.ID, n.Kind)
		}
	}
	return nil
}
