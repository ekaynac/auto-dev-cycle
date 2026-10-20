// SPDX-License-Identifier: Apache-2.0

package api

// The task read (M1 slice plan §2.3 assertion 5; P22a-7, W18): GET
// /v1/tasks, one record per recorded task the caller reads the board of,
// in task id order, read under the board read kind.
//   - card is the task's board card, as GET /v1/board derives it.
//   - first_attempt is the earliest attempt started on any of the task's
//     nodes (by its start time, then its id), null until one starts: the
//     whole seconds from the task's approval to that start; the attempt's
//     status; the outcome and reason of its node's selected result, null
//     until the node has one; and the input and output tokens of the model
//     calls bound to its lease, as the call ledger stands them.
//   - The charge is not here yet: it is priced at the lease's declared
//     price (P17b), which joins it.

import (
	"errors"
	"slices"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

var tasks = Resource{
	Domain: boardDomain,
	Schema: "api/task.json",
	List: func(tx *eventstore.Tx, at ReadAt) ([]any, error) {
		ids, err := authority.TaskIDs(tx)
		nodes, nerr := graph.Nodes(tx)
		if err = errors.Join(err, nerr); err != nil {
			return nil, err
		}
		byTask, byNode := map[types.TaskID][]graph.NodeView{}, map[types.NodeID]graph.NodeView{}
		for _, n := range nodes {
			byTask[n.Task], byNode[n.Node.ID] = append(byTask[n.Task], n), n
		}
		firsts, err := firstAttempts(tx, byNode)
		if err != nil {
			return nil, err
		}
		out := []any{}
		for _, id := range ids {
			t, recorded, project, err := boardTask(tx, id)
			if err != nil {
				return nil, err
			}
			if !recorded || !at.Scope.ReadsRecord(boardDomain, project) {
				continue // a damaged record is the board's to show
			}
			read := types.TaskRead{ID: id, Project: t.Project, Card: boardCard(t, byTask[id], at.Now, at.Scope.ReadsRecord(holdDomain, project))}
			if f, ok := firsts[id]; ok {
				if read.FirstAttempt, err = metrics(tx, t, f, byNode[types.NodeID(f.node)]); err != nil {
					return nil, err
				}
			}
			out = append(out, read)
		}
		return out, nil
	},
	Project: func(tx *eventstore.Tx, rec any) (*types.ProjectID, error) {
		r, _ := rec.(types.TaskRead)
		_, _, project, err := boardTask(tx, r.ID)
		return project, err
	},
	Fields: []string{"id", "project", "card", "first_attempt"},
}

// started is an attempt, its node and its start time.
type started struct {
	attempt types.Attempt
	node    types.NodeID
	at      time.Time
}

// firstAttempts is each task's first attempt among every attempt started on
// one of byNode's nodes: the earliest start, then the smallest id.
func firstAttempts(tx *eventstore.Tx, byNode map[types.NodeID]graph.NodeView) (map[types.TaskID]started, error) {
	keys, err := tx.Keys(lease.AttemptProjection, "")
	out := map[types.TaskID]started{}
	for _, k := range keys {
		a, _, ok, aerr := lease.AttemptOf(tx, types.AttemptID(k))
		at, began, serr := lease.StartedAt(tx, types.AttemptID(k))
		if err = errors.Join(err, aerr, serr); err != nil {
			return nil, err
		}
		node, unit := graph.NodeOf(a.Unit)
		n, known := byNode[node]
		if !ok || !began || !unit || !known {
			continue
		}
		s := started{attempt: a, node: node, at: at}
		if f, seen := out[n.Task]; !seen || at.Before(f.at) || at.Equal(f.at) && a.ID < f.attempt.ID {
			out[n.Task] = s
		}
	}
	return out, err
}

// metrics is task t's first-attempt metrics from f, its node n.
func metrics(tx *eventstore.Tx, t authority.TaskRecord, f started, n graph.NodeView) (*types.FirstAttempt, error) {
	m := &types.FirstAttempt{Attempt: f.attempt.ID, Node: f.node, StartedAt: types.Time(f.at.UTC().Format(time.RFC3339Nano)),
		Status: string(f.attempt.Status), Outcome: n.Outcome, Reason: n.Reason}
	if a := t.Approval; a != nil {
		approved, err := time.Parse(time.RFC3339Nano, string(a.At))
		if err != nil {
			return nil, err
		}
		s := int64(max(f.at.Sub(approved), 0) / time.Second)
		m.SecondsAfterApproval = &s
	}
	calls, err := ledger.BoundTo(tx, f.attempt.Lease)
	if err != nil {
		return nil, err
	}
	slices.Sort(calls)
	for _, c := range calls {
		ms, err := ledger.Measurements(tx, c)
		if err != nil {
			return nil, err
		}
		s := ledger.StandingOf(ms, c)
		if s.Usage == nil {
			continue
		}
		if m.Tokens == nil {
			m.Tokens = &types.Tokens{}
		}
		m.Tokens.In, m.Tokens.Out = m.Tokens.In+s.Usage.In, m.Tokens.Out+s.Usage.Out
	}
	return m, nil
}
