// SPDX-License-Identifier: Apache-2.0

package graph

// Attempts bound to nodes (M1 P8a; docs/design/m1/00-slice-plan.md §5.2;
// 13 "The caller names the attempts until M1 binds attempts to nodes").
//
// Decisions:
//   - A lease's unit is a node: the unit's id is its node's, under the unit
//     prefix (UnitOf, NodeOf), so a lease, and each attempt under it, names
//     its node with no record of its own, and a rebuild derives the binding
//     from the log as it is.
//   - Units is the lease projector's binding (lease.Units): a unit's project
//     and task are its node's, and an attempt starts only on a claimed
//     node. A superseded or completed node starts none, so no attempt runs
//     for a node its supersession has already replaced.
//   - A node has one current attempt (the lease's unit_attempts), so the
//     attempts a supersession stops are derived from it, never named by
//     the caller (Supersede); the fold refuses one that leaves it running.
//   - A result recorded with its attempt is that attempt's node's
//     (attemptBound): the result.recorded fold refuses an attempt never
//     started, or started on another node's unit, on replay as at admission.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

const (
	nodePrefix = "nod_"
	unitPrefix = "unt_"
)

// UnitOf is node's unit: its id under the unit prefix.
func UnitOf(node types.NodeID) types.UnitID {
	return types.UnitID(unitPrefix + strings.TrimPrefix(string(node), nodePrefix))
}

// NodeOf is unit's node, false for an id that is not a unit's.
func NodeOf(unit types.UnitID) (types.NodeID, bool) {
	suffix, ok := strings.CutPrefix(string(unit), unitPrefix)
	return types.NodeID(nodePrefix + suffix), ok && suffix != ""
}

// Units binds each unit to its node (lease.Units).
type Units struct{}

var _ lease.Units = Units{}

func unitNode(tx *eventstore.Tx, unit types.UnitID) (nodeRecord, bool, error) {
	node, ok := NodeOf(unit)
	if !ok {
		return nodeRecord{}, false, nil
	}
	return load[nodeRecord](tx, NodeProjection, string(node))
}

// ProjectTask is unit's node's project and task, false when unit is no
// recorded node's.
func (Units) ProjectTask(tx *eventstore.Tx, unit types.UnitID) (types.ProjectID, types.TaskID, bool, error) {
	rec, ok, err := unitNode(tx, unit)
	if err != nil || !ok {
		return "", "", false, err
	}
	return rec.Recorded.Project, rec.Recorded.Task, true, nil
}

// Startable refuses an attempt starting on unit unless its node is
// recorded and claimed.
func (Units) Startable(tx *eventstore.Tx, unit types.UnitID) error {
	rec, ok, err := unitNode(tx, unit)
	switch {
	case err != nil:
		return err
	case !ok:
		return fmt.Errorf("graph: unit %s is no recorded node's: an attempt runs for a node", unit)
	case rec.State.State != types.NodeStateStateClaimed:
		return fmt.Errorf("graph: node %s is %s: an attempt starts only on a claimed node", rec.Recorded.Node.ID, rec.State.State)
	}
	return nil
}

// runningAttempt is node's current attempt when it is still running.
func runningAttempt(tx *eventstore.Tx, node types.NodeID) (types.Attempt, bool, error) {
	id, ok, err := lease.CurrentAttempt(tx, UnitOf(node))
	if err != nil || !ok {
		return types.Attempt{}, false, err
	}
	a, _, found, err := lease.AttemptOf(tx, id)
	if err != nil || !found {
		return types.Attempt{}, false, errors.Join(err, fmt.Errorf("graph: node %s's attempt %s is missing", node, id))
	}
	return a, a.Status == types.AttemptStatusRunning, nil
}

// attemptBound: attempt was started, on node's unit.
func attemptBound(tx *eventstore.Tx, node types.NodeID, attempt types.AttemptID) error {
	a, _, ok, err := lease.AttemptOf(tx, attempt)
	switch {
	case err != nil:
		return err
	case !ok:
		return fmt.Errorf("attempt %s was never started", attempt)
	case a.Unit != UnitOf(node):
		return fmt.Errorf("attempt %s ran for unit %s, not node %s's", attempt, a.Unit, node)
	}
	return nil
}
