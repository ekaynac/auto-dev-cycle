// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// DeferredKinds are the node kinds no M1 template holds: their surfaces
// stay unreachable (00-slice-plan §1, D47; I15).
var DeferredKinds = []types.NodeKind{types.NodeKindEvidence, types.NodeKindDeploy, types.NodeKindRuntimeVerify, types.NodeKindIntegration}

// CheckSurface is the deferred-surface guard (I15 admission, P22a): every
// template adcd records goes through it, with the Env it evaluates under.
// It refuses a node of a deferred kind, and an Env holding instrument
// contracts (gate contracts live apart, P15; with none, ClaimEvidence fails
// closed).
func CheckSurface(env Env, nodes []types.Node) error {
	if env.Contracts != nil {
		return fmt.Errorf("graph: deferred surface: the environment holds instrument contracts")
	}
	for _, n := range nodes {
		if slices.Contains(DeferredKinds, n.Kind) {
			return fmt.Errorf("graph: deferred surface: node %s is a %s node", n.ID, n.Kind)
		}
	}
	return nil
}
