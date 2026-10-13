// SPDX-License-Identifier: Apache-2.0

package api

// The read-only board (docs/plan/01 §2.2 "Board and validated pull", D51;
// M1 P4b): one card per task, a projection of the task and graph records
// in the read's snapshot. Nobody writes it, and nothing on it is stored.
//
// Decisions:
//   - A Ready task's column is its approval's: ready while its spec's
//     approval is in force, else awaiting_approval (none, or revoked).
//   - An admitted task's (authority.TaskAdmitted, written by P22a) column
//     is its current (unsuperseded) nodes': running while a plan or build
//     node is claimed or held; else verifying while an accept, gate or
//     review node is; else done_candidate when a review node completed go
//     (outcome empty: no blocking finding; merge is inactive in M1(a));
//     else ready, with the admitted badge. A held node keeps its stage's
//     column and carries its hold badges.
//   - A column is never guessed: a task state, a node kind outside M1(a)'s
//     template, or a node whose task is unrecorded, not admitted or in
//     another project fails the read.
//   - A card exposes references only: never the spec's requirement, its
//     attachments' names or digests, nor the spec digest (a digest over
//     them, as the proposals read withholds its binding digest).
//   - Lease holds stay on GET /v1/holds: no lease is bound to a node
//     before P8a.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
)

// stages is the column each node kind of M1(a)'s template moves its
// task's card to while claimed or held ("": none, a controller node).
var stages = map[types.NodeKind]types.BoardCardColumn{
	types.NodeKindPlan: types.BoardCardColumnRunning, types.NodeKindBuild: types.BoardCardColumnRunning,
	types.NodeKindAccept: types.BoardCardColumnVerifying, types.NodeKindGate: types.BoardCardColumnVerifying,
	types.NodeKindReview: types.BoardCardColumnVerifying, types.NodeKindReviewRequirement: "",
}

// board is every task's card, in task id order.
var board = Resource{
	Domain: "board",
	Schema: "api/board.json",
	List: func(tx *eventstore.Tx, now time.Time) ([]any, error) {
		keys, err := tx.Keys(authority.TaskProjection, "")
		if err != nil {
			return nil, err
		}
		nodes, err := graph.Nodes(tx)
		if err != nil {
			return nil, err
		}
		byTask := map[types.TaskID][]graph.NodeView{}
		for _, n := range nodes {
			byTask[n.Task] = append(byTask[n.Task], n)
		}
		out := []any{}
		for _, k := range keys {
			if strings.Contains(k, "/") { // the ready/ and approver/ indexes
				continue
			}
			t, err := taskRecord(tx, types.TaskID(k))
			if err != nil {
				return nil, err
			}
			card, err := boardCard(t, byTask[t.ID], now)
			if err != nil {
				return nil, err
			}
			delete(byTask, t.ID)
			out = append(out, card)
		}
		for task, ns := range byTask {
			return nil, underivable(task, "node %s's task is not recorded", ns[0].Node.ID)
		}
		return out, nil
	},
	Project: func(tx *eventstore.Tx, rec any) (*types.ProjectID, error) {
		c, ok := rec.(types.BoardCard)
		if !ok {
			return nil, fmt.Errorf("api: a board record of type %T", rec)
		}
		t, err := taskRecord(tx, c.ID)
		return &t.Project, err
	},
	Fields: []string{"id", "project", "title", "column", "admitted", "approval", "created_at", "nodes"},
}

// taskRecord is task id's record; none is damage.
func taskRecord(tx *eventstore.Tx, id types.TaskID) (authority.TaskRecord, error) {
	var t authority.TaskRecord
	raw, ok, err := tx.Get(authority.TaskProjection, id)
	if err == nil && !ok {
		err = fmt.Errorf("api: no task record %s", id)
	}
	if err != nil {
		return t, err
	}
	if err := json.Unmarshal(raw, &t); err != nil || t.ID != id {
		return t, fmt.Errorf("api: task record %s is damaged: %v", id, err)
	}
	return t, nil
}

func underivable(task types.TaskID, format string, a ...any) error {
	return fmt.Errorf("api: board: task %s's column is not derivable from its records: "+format, append([]any{task}, a...)...)
}

// boardCard is t's card over its nodes (node id order), its hold badges
// aged at now.
func boardCard(t authority.TaskRecord, nodes []graph.NodeView, now time.Time) (types.BoardCard, error) {
	c := types.BoardCard{ID: t.ID, Project: t.Project, Title: t.Spec.Title, Admitted: t.State == authority.TaskAdmitted,
		Approval: types.BoardCardApprovalNone, CreatedAt: t.CreatedAt, Nodes: []types.BoardCardNodesItem{}}
	if a := t.Approval; a != nil {
		if a.Status != authority.ApprovalInForce && a.Status != authority.ApprovalRevoked {
			return c, underivable(t.ID, "approval status %q", a.Status)
		}
		c.Approval = types.BoardCardApproval(a.Status)
	}
	switch {
	case t.State == authority.TaskReady && len(nodes) > 0:
		return c, underivable(t.ID, "a task not admitted has node %s", nodes[0].Node.ID)
	case t.State == authority.TaskReady && c.Approval == types.BoardCardApprovalInForce:
		c.Column = types.BoardCardColumnReady
		return c, nil
	case t.State == authority.TaskReady:
		c.Column = types.BoardCardColumnAwaitingApproval
		return c, nil
	case !c.Admitted:
		return c, underivable(t.ID, "task state %q", t.State)
	}
	active := map[types.BoardCardColumn]bool{}
	for _, n := range nodes {
		stage, known := stages[n.Node.Kind]
		if !known || n.Project != t.Project {
			return c, underivable(t.ID, "node %s is a %s node in project %s", n.Node.ID, n.Node.Kind, n.Project)
		}
		card, err := nodeCard(t.ID, n, now)
		if err != nil {
			return c, err
		}
		c.Nodes = append(c.Nodes, card)
		switch s := n.State.State; {
		case s == types.NodeStateStateClaimed || s == types.NodeStateStateHeld:
			active[stage] = true
		case s == types.NodeStateStateCompleted && n.Node.Kind == types.NodeKindReview && n.Outcome != nil && *n.Outcome == types.OutcomeEmpty:
			active[types.BoardCardColumnDoneCandidate] = true
		}
	}
	c.Column = types.BoardCardColumnReady
	for _, col := range []types.BoardCardColumn{types.BoardCardColumnRunning, types.BoardCardColumnVerifying, types.BoardCardColumnDoneCandidate} {
		if active[col] {
			c.Column = col
			break
		}
	}
	return c, nil
}

// nodeCard is n's card: a badge per hold (hold, signal, owner, age at now)
// and per structural wait (the input, and its producer or owed input).
func nodeCard(task types.TaskID, n graph.NodeView, now time.Time) (types.BoardCardNodesItem, error) {
	c := types.BoardCardNodesItem{ID: n.Node.ID, Kind: types.BoardCardNodesItemKind(n.Node.Kind), Round: n.Node.Round,
		State: types.BoardCardNodesItemState(n.State.State), Holds: []types.BoardCardNodesItemHoldsItem{}, Waiting: []types.BoardCardNodesItemWaitingItem{}}
	for _, h := range n.State.Holds {
		since, err := time.Parse(time.RFC3339Nano, string(h.Since))
		if err != nil {
			return c, underivable(task, "hold %s's since %q", h.ID, h.Since)
		}
		c.Holds = append(c.Holds, types.BoardCardNodesItemHoldsItem{ID: h.ID, Hold: h.Hold, SignalInstance: h.SignalInstance, Owner: h.Owner,
			OwnerRef: h.OwnerRef, Since: h.Since, AgeSeconds: max(0, int64(now.Sub(since)/time.Second))})
	}
	for _, name := range n.State.WaitingOn {
		w := types.BoardCardNodesItemWaitingItem{Input: name}
		for _, in := range n.Node.Inputs {
			switch src := in.Source.Variant.(type) {
			case *types.ResultSource:
				if in.Name == name {
					w.Producer = &src.Producer
				}
			case *types.AnswerSource:
				if in.Name == name {
					w.OwedInput = &src.OwedInput
				}
			}
		}
		if w.Producer == nil && w.OwedInput == nil {
			return c, underivable(task, "node %s waits on %s, which it does not declare", n.Node.ID, name)
		}
		c.Waiting = append(c.Waiting, w)
	}
	return c, nil
}
