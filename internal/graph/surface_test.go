// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// TestCheckSurface (P22a-2, W8; I15 admission; Opus #298 L1): a template of
// M1's kinds passes; one holding any other kind (the four §1 names, and
// the rest: closure, record, phase_gate, rebase, carry_over, and merge
// unless allowed), or an Env with instrument contracts, is refused.
func TestCheckSurface(t *testing.T) {
	node := func(k types.NodeKind) types.Node { return types.Node{ID: types.NodeID("nod_" + string(k)), Kind: k} }
	var m1 []types.Node
	for _, k := range M1Kinds {
		m1 = append(m1, node(k))
	}
	if err := CheckSurface(Env{}, m1, M1Kinds); err != nil {
		t.Fatal(err)
	}
	for _, k := range []types.NodeKind{types.NodeKindEvidence, types.NodeKindDeploy, types.NodeKindRuntimeVerify, types.NodeKindIntegration,
		types.NodeKindClosure, types.NodeKindRecord, types.NodeKindPhaseGate, types.NodeKindRebase, types.NodeKindCarryOver, types.NodeKindMerge} {
		if err := CheckSurface(Env{}, append(m1, node(k)), M1Kinds); err == nil || !strings.Contains(err.Error(), string(k)) {
			t.Errorf("a template with a %s node: %v", k, err)
		}
	}
	if err := CheckSurface(Env{}, append(m1, node(types.NodeKindMerge)), append(M1Kinds, types.NodeKindMerge)); err != nil {
		t.Errorf("merge, allowed: %v", err)
	}
	contracts := Contracts(func(types.InstrumentRef) (types.InstrumentContract, error) { return types.InstrumentContract{}, nil })
	if err := CheckSurface(Env{Contracts: contracts}, m1, M1Kinds); err == nil || !strings.Contains(err.Error(), "instrument contracts") {
		t.Errorf("an Env with contracts: %v", err)
	}
}
