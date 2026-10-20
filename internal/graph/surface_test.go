// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// TestCheckSurface (P22a-2, W8; I15 admission): a template of M1's kinds
// passes; one holding any deferred kind, or an Env with instrument
// contracts, is refused.
func TestCheckSurface(t *testing.T) {
	node := func(k types.NodeKind) types.Node { return types.Node{ID: types.NodeID("nod_" + string(k)), Kind: k} }
	m1 := []types.Node{node(types.NodeKindPlan), node(types.NodeKindBuild), node(types.NodeKindAccept), node(types.NodeKindGate),
		node(types.NodeKindReview), node(types.NodeKindReviewRequirement), node(types.NodeKindMerge)}
	if err := CheckSurface(Env{}, m1); err != nil {
		t.Fatal(err)
	}
	for _, k := range []types.NodeKind{types.NodeKindEvidence, types.NodeKindDeploy, types.NodeKindRuntimeVerify, types.NodeKindIntegration} {
		if err := CheckSurface(Env{}, append(m1, node(k))); err == nil || !strings.Contains(err.Error(), string(k)) {
			t.Errorf("a template with a %s node: %v", k, err)
		}
	}
	contracts := Contracts(func(types.InstrumentRef) (types.InstrumentContract, error) { return types.InstrumentContract{}, nil })
	if err := CheckSurface(Env{Contracts: contracts}, m1); err == nil || !strings.Contains(err.Error(), "instrument contracts") {
		t.Errorf("an Env with contracts: %v", err)
	}
}
