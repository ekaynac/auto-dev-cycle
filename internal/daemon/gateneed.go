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
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
)

// gateNeed is the gate requirement a review's input on gate node gateNode
// takes (P22a-6a′; I5, I13), derived from the log, never from the receipt
// it checks:
//   - the tree: the selected tree of accept, the gate's producer;
//   - the instrument and the environment: gate.Contract over the project's
//     gate in the image the gate's selected result was measured in
//     (gate.measured/v1), which must be the image the project pins;
//   - the demand: the key the recorded baseline's digest derives.
//
// A gate result with no recorded measurement, or one measured in an image
// the project no longer pins, serves no review: an error, the evaluation's.
func gateNeed(tx *eventstore.Tx, gateNode types.NodeID) (*receipt.Requirement, error) {
	g, err := graph.ViewOf(tx, gateNode)
	if err != nil || g.Node.Kind != types.NodeKindGate || g.State.Result == nil {
		return nil, errors.Join(err, fmt.Errorf("daemon: %s is no gate with a selected result", gateNode))
	}
	m, ok, err := gate.MeasurementOf(tx, *g.State.Result)
	if err != nil || !ok {
		return nil, errors.Join(err, fmt.Errorf("daemon: gate result %s has no recorded measurement", *g.State.Result))
	}
	prof, found, err := authority.ProjectProfile(tx, g.Project)
	if err != nil || !found {
		return nil, errors.Join(err, fmt.Errorf("daemon: project %s has no profile", g.Project))
	}
	req, err := requirementWith(tx, gateNode, m, *prof)
	return &req, err
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
		return receipt.Requirement{}, fmt.Errorf("daemon: gate result %s was measured in an image the project does not pin", m.Result)
	}
	var accept types.NodeID
	for _, in := range g.Node.Inputs {
		if src, ok := in.Source.Variant.(*types.ResultSource); ok && src.ResultKind == types.ResultKindTree {
			accept = src.Producer
		}
	}
	a, err := graph.ViewOf(tx, accept)
	if err != nil || a.State.Result == nil {
		return receipt.Requirement{}, errors.Join(err, fmt.Errorf("daemon: the gate's accept %s has no selected tree", accept))
	}
	tree, err := treeOf(tx, *a.State.Result)
	if err != nil {
		return receipt.Requirement{}, err
	}
	return gate.RequirementFor(prof.Gate, tree, m, gate.Subject{Project: g.Project, Revision: buildContractRevision})
}

// treeOf is the git tree a tree result holds, hex.
func treeOf(tx *eventstore.Tx, result types.ResultID) (string, error) {
	raw, ok, err := tx.Get(graph.ResultProjection, string(result))
	var rr types.ResultRecorded
	if err == nil && ok {
		err = json.Unmarshal(raw, &rr)
	}
	if err != nil || !ok || rr.Result.Kind != types.ResultKindTree || len(rr.Result.Value) == 0 || rr.Result.Value[0].Type != types.RefTypeGitTree {
		return "", errors.Join(err, fmt.Errorf("daemon: result %s holds no tree", result))
	}
	return strings.TrimPrefix(rr.Result.Value[0].ID, "git:"), nil
}
