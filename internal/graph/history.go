// SPDX-License-Identifier: Apache-2.0

package graph

// The graph's start-time history check and its holds as read (M1 P4a;
// docs/design/m1/00-slice-plan.md §5.1; 15 §3.1 "CheckHistory coverage
// moves to M1").
//
// Decisions:
//   - CheckHistory runs on a store's existing projections before any
//     replay, as capacity.CheckHistory does. A replay compares the graph
//     projections with the log, but the effects table is no projection:
//     the folds read it as it is now. So the graph's links to records
//     outside its projections are cross-checked here, against those
//     records: each operation's effect (its kind, target, request and
//     state), and the hold index. The lease projections are replayed, and
//     the supersession fold derives the attempt it stops, and the
//     result.recorded fold checks that a result's attempt ran on its
//     node's unit (units.go), so replay covers both.
//   - Effect states are monotone, and a node's key never moves while its
//     operation is live (an invalidation waits, settledOperation), so each
//     rule reads the same at any start of a correct history.
//   - A decode or storage error is the projections' being unreadable,
//     never a refusal of the history: only a missing record or a mismatch
//     is ErrHistory.

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// ErrHistory is CheckHistory's refusal of the history itself.
var ErrHistory = errors.New("graph: the graph projections disagree with the records they name")

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrHistory}, a...)...)
}

// CheckHistory refuses (ErrHistory) graph projections whose operations or
// holds disagree with the effect and hold records they name. Call it on the store's projections (a View) before any replay.
func CheckHistory(tx *eventstore.Tx) error {
	ops, err := tx.Keys(EffectProjection, "")
	if err != nil {
		return err
	}
	for _, k := range ops {
		if err := checkOperation(tx, types.NodeID(k)); err != nil {
			return err
		}
	}
	nodes, err := tx.Keys(NodeProjection, "")
	for _, k := range nodes {
		rec, lerr := loadNode(tx, types.NodeID(k))
		if err = errors.Join(err, lerr); err != nil {
			return err
		}
		// Checked from node to index only: a stale index entry is the
		// replay's to find, since graph_holds is a projection.
		for _, h := range rec.State.Holds {
			by, ok, err := load[heldBy](tx, HoldProjection, string(h.ID))
			if err != nil {
				return err
			}
			if !ok || by.Node != rec.Recorded.Node.ID {
				return refuse("hold %s of node %s is not indexed to it", h.ID, k)
			}
		}
	}
	return err
}

// checkOperation: node's operation names a recorded effect of the node's
// kind on its target; while live, the node is still at the operation's key,
// and claimed there, for exactly that request; a node completed at that
// key completed its effect.
func checkOperation(tx *eventstore.Tx, node types.NodeID) error {
	op, _, err := load[types.NodeOperated](tx, EffectProjection, string(node))
	if err != nil {
		return err
	}
	rec, ok, err := load[nodeRecord](tx, NodeProjection, string(node))
	if err != nil {
		return err
	}
	if !ok || rec.Recorded.Node.Effect == nil {
		return refuse("operation %s is of no effect node %s", op.Effect, node)
	}
	e, err := tx.LoadEffect(string(op.Effect))
	if errors.Is(err, eventstore.ErrNoEffect) {
		return refuse("node %s's operation names no effect (%s)", node, op.Effect)
	} else if err != nil {
		return err
	}
	target, err := effectTarget(rec.Recorded.Node)
	if err != nil {
		return err
	}
	if e.Kind != string(*rec.Recorded.Node.Effect) || e.Target != target {
		return refuse("effect %s is not of node %s's kind, on its target", e.ID, node)
	}
	live := e.State == eventstore.EffectPending || e.State == eventstore.EffectUncertain
	atKey := rec.State.Key != nil && *rec.State.Key == op.Key
	switch {
	case live && (!atKey || rec.State.State == types.NodeStateStateCompleted):
		return refuse("effect %s is %s, but node %s moved past its operation", e.ID, e.State, node)
	case atKey && rec.State.State == types.NodeStateStateCompleted && e.State != eventstore.EffectCompleted:
		return refuse("node %s completed, but its effect %s is %s", node, e.ID, e.State)
	case atKey && rec.State.State == types.NodeStateStateClaimed:
		r, _, err := effectRequest(tx, node)
		if err != nil {
			return err
		}
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		d, err := eventstore.ContentDigest(b)
		if err != nil {
			return err
		}
		if d != e.RequestDigest {
			return refuse("effect %s is not node %s's request", e.ID, node)
		}
	}
	return nil
}

// NodeHolds are a node's holds as recorded, with its task and project.
type NodeHolds struct {
	Node    types.NodeID
	Task    types.TaskID
	Project types.ProjectID
	Holds   []types.NodeHold
}

// Held is every node that holds a hold, in node id order.
func Held(tx *eventstore.Tx) ([]NodeHolds, error) {
	keys, err := tx.Keys(NodeProjection, "")
	var out []NodeHolds
	for _, k := range keys {
		rec, lerr := loadNode(tx, types.NodeID(k))
		if err = errors.Join(err, lerr); err != nil {
			return nil, err
		}
		if len(rec.State.Holds) > 0 {
			out = append(out, NodeHolds{Node: rec.Recorded.Node.ID, Task: rec.Recorded.Task, Project: rec.Recorded.Project, Holds: rec.State.Holds})
		}
	}
	return out, err
}

// NodeView is a node as recorded, with its task, project and state, and
// the outcome and reason of its selected result: nil when none is
// selected, or when the results projection does not hold it (State.Result
// tells which).
type NodeView struct {
	Node    types.Node
	Task    types.TaskID
	Project types.ProjectID
	State   types.NodeState
	Outcome *types.Outcome
	Reason  *types.ReasonCode
}

// TaskProject is the project task's first node pinned (graph_tasks);
// false: the task has no node.
func TaskProject(tx *eventstore.Tx, task types.TaskID) (types.ProjectID, bool, error) {
	return load[types.ProjectID](tx, TaskProjection, string(task))
}

// Nodes is every node, in node id order (the board, M1 P4b).
func Nodes(tx *eventstore.Tx) ([]NodeView, error) {
	keys, err := tx.Keys(NodeProjection, "")
	var out []NodeView
	for _, k := range keys {
		rec, lerr := loadNode(tx, types.NodeID(k))
		if err = errors.Join(err, lerr); err != nil {
			return nil, err
		}
		v := NodeView{Node: rec.Recorded.Node, Task: rec.Recorded.Task, Project: rec.Recorded.Project, State: rec.State}
		if id := rec.State.Result; id != nil {
			r, ok, err := load[types.ResultRecorded](tx, ResultProjection, string(*id))
			if err != nil {
				return nil, err
			}
			if ok {
				v.Outcome, v.Reason = &r.Result.Outcome, &r.Result.Reason
			}
		}
		out = append(out, v)
	}
	return out, err
}

// ProjectOf is node's project, as its record holds it.
func ProjectOf(tx *eventstore.Tx, node types.NodeID) (types.ProjectID, error) {
	rec, err := loadNode(tx, node)
	return rec.Recorded.Project, err
}
