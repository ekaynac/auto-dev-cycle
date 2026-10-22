// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// Opus 22a-6c M1: TakesGate is the one rule for "a review takes gate g": a
// review on gate g takes g, never another gate, and a node that is no
// review takes none.
func TestTakesGate(t *testing.T) {
	on := func(kind types.NodeKind, gate types.NodeID) types.Node {
		return types.Node{Kind: kind, Inputs: []types.InputSpec{{Name: "gate", Source: types.InputSpecSource{Variant: &types.ResultSource{
			Producer: gate, ResultKind: types.ResultKindGateResult}}}}}
	}
	if !TakesGate(on(types.NodeKindReview, "g1"), "g1") || TakesGate(on(types.NodeKindReview, "g1"), "g2") || TakesGate(on(types.NodeKindMerge, "g1"), "g1") {
		t.Error("TakesGate")
	}
}
