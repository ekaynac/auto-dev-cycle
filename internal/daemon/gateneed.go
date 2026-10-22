// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gate"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// gateNeed is the gate requirement a review's input on gate node gateNode
// takes (P22a-6a′; I5, I13), derived from the log, never from the receipt
// it checks:
//   - the tree: the one the gate's claim took (accept's result it ran on;
//     Opus 22a-6a′ L1);
//   - the instrument and the environment: gate.Contract over the project's
//     gate in the image the gate's selected result was measured in
//     (gate.measured/v1), which must be the image the project pins;
//   - the demand: the key the recorded baseline's digest derives, of a
//     baseline measured on the build attempt's base (Opus L4).
//
// One that cannot be derived is a *receipt.Refusal, so the review waits and
// the rest of the task evaluates (Opus M1): no recorded measurement,
// receipt_incomplete; an image the project no longer pins,
// instrument_changed; a baseline of another base, evidence_inapplicable.
func gateNeed(tx *eventstore.Tx, gateNode types.NodeID) (*receipt.Requirement, error) {
	g, err := graph.ViewOf(tx, gateNode)
	if err != nil || g.Node.Kind != types.NodeKindGate || g.State.Result == nil {
		return nil, errors.Join(err, fmt.Errorf("daemon: %s is no gate with a selected result", gateNode))
	}
	m, ok, err := gate.MeasurementOf(tx, *g.State.Result)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &receipt.Refusal{Reason: vocab.ReasonReceiptIncomplete, Detail: fmt.Sprintf("gate result %s has no recorded measurement", *g.State.Result)}
	}
	prof, found, err := authority.ProjectProfile(tx, g.Project)
	if err != nil || !found {
		return nil, errors.Join(err, fmt.Errorf("daemon: project %s has no profile", g.Project))
	}
	req, err := requirementWith(tx, gateNode, m, *prof)
	if err != nil {
		return nil, err
	}
	return &req, nil
}

// requirementWith is gateNeed's requirement from m, the gate's selected
// result's measurement, under prof, its project's profile.
func requirementWith(tx *eventstore.Tx, gateNode types.NodeID, m types.GateMeasured, prof types.ProjectProfile) (receipt.Requirement, error) {
	g, err := graph.ViewOf(tx, gateNode)
	if err != nil {
		return receipt.Requirement{}, err
	}
	pinned := false
	for _, ref := range prof.Toolchain.Images {
		pinned = pinned || string(ref) == m.Image.Pinned
	}
	if !pinned || len(prof.Toolchain.Images) != 1 {
		return receipt.Requirement{}, &receipt.Refusal{Reason: vocab.ReasonInstrumentChanged,
			Detail: fmt.Sprintf("gate result %s was measured in an image the project does not pin", m.Result)}
	}
	tree, err := claimedResult(tx, gateNode, types.ResultKindTree)
	if err != nil {
		return receipt.Requirement{}, err
	}
	at, err := buildAttempt(tx, tree.Node)
	if err != nil {
		return receipt.Requirement{}, err
	}
	if at.Base.Tree != m.BaseTree {
		return receipt.Requirement{}, &receipt.Refusal{Reason: vocab.ReasonEvidenceInapplicable,
			Detail: fmt.Sprintf("gate result %s's baseline was measured on %s, not the build attempt's base %s", m.Result, m.BaseTree, at.Base.Tree)}
	}
	hex, err := treeOf(tree)
	if err != nil {
		return receipt.Requirement{}, err
	}
	return gate.RequirementFor(prof.Gate, hex, m, gate.Subject{Project: g.Project, Revision: buildContractRevision})
}

// claimedResult is the one result of kind node's claim took.
func claimedResult(tx *eventstore.Tx, node types.NodeID, kind types.ResultKind) (types.ResultRecorded, error) {
	ins, _, err := graph.ClaimInputs(tx, node)
	if err != nil {
		return types.ResultRecorded{}, err
	}
	var rr types.ResultRecorded
	if len(ins) != 1 || ins[0].Result == nil {
		return rr, fmt.Errorf("daemon: node %s's claim took %d inputs, not one result", node, len(ins))
	}
	raw, ok, err := tx.Get(graph.ResultProjection, string(*ins[0].Result))
	if err == nil && ok {
		err = json.Unmarshal(raw, &rr)
	}
	if err != nil || !ok || rr.Result.Kind != kind {
		return rr, errors.Join(err, fmt.Errorf("daemon: node %s's claimed input is no %s", node, kind))
	}
	return rr, nil
}

// buildAttempt is the build attempt whose patch accept node's claim took.
func buildAttempt(tx *eventstore.Tx, accept types.NodeID) (types.Attempt, error) {
	patch, err := claimedResult(tx, accept, types.ResultKindPatch)
	if err != nil {
		return types.Attempt{}, err
	}
	if patch.Attempt == nil {
		return types.Attempt{}, fmt.Errorf("daemon: accept %s's patch is no attempt's", accept)
	}
	at, _, found, err := lease.AttemptAt(tx, *patch.Attempt)
	if err != nil || !found {
		return types.Attempt{}, errors.Join(err, fmt.Errorf("daemon: the build's attempt %s is not recorded", *patch.Attempt))
	}
	return at, nil
}

// treeOf is the git tree rr, a tree result, holds, hex.
func treeOf(rr types.ResultRecorded) (string, error) {
	if rr.Result.Kind != types.ResultKindTree || len(rr.Result.Value) == 0 || rr.Result.Value[0].Type != types.RefTypeGitTree {
		return "", fmt.Errorf("daemon: result %s holds no tree", rr.Result.ID)
	}
	return strings.TrimPrefix(rr.Result.Value[0].ID, "git:"), nil
}
