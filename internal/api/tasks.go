// SPDX-License-Identifier: Apache-2.0

package api

// The task read (M1 slice plan §2.3 assertion 5; P22a-7, W18): GET
// /v1/tasks, one record per recorded task the caller reads the board of,
// in task id order, read under the board read kind.
//   - card is the task's board card, as GET /v1/board derives it.
//   - first_attempt is the earliest attempt started on any of the task's
//     nodes in its own project (by its start time, then its id), null until
//     one starts, and null unless the reader also reads attempts in the
//     project (the attempt read kind, as adc events gates attempt.*: Opus
//     #320 M1). It holds the whole seconds from the task's approval to that
//     start; the attempt's status; the outcome and reason of the result
//     that attempt itself recorded (never another attempt's, nor one
//     recorded with no attempt: Opus #320 H1), null until it records one;
//     and the input and output tokens of the model calls bound to its
//     lease, as the call ledger stands them, null with no call and null
//     unless the reader also reads model calls there (model_call).
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

// The read kinds of a task read's attempt and model-call fields.
const (
	attemptDomain   types.Name = "attempt"
	modelCallDomain types.Name = "model_call"
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
		recs, projects := map[types.TaskID]authority.TaskRecord{}, map[types.TaskID]types.ProjectID{}
		for _, id := range ids {
			t, recorded, _, err := boardTask(tx, id)
			if err != nil {
				return nil, err
			}
			if recorded { // a damaged record is the board's to show
				recs[id], projects[id] = t, t.Project
			}
		}
		firsts, err := firstAttempts(tx, byNode, projects)
		if err != nil {
			return nil, err
		}
		out := []any{}
		for _, id := range ids {
			t, recorded := recs[id]
			if !recorded || !at.Scope.ReadsRecord(boardDomain, &t.Project) {
				continue
			}
			read := types.TaskRead{ID: id, Project: t.Project, Card: boardCard(t, byTask[id], at.Now, at.Scope.ReadsRecord(holdDomain, &t.Project))}
			if f, ok := firsts[id]; ok && at.Scope.ReadsRecord(attemptDomain, &t.Project) {
				if read.FirstAttempt, err = metrics(tx, t, f, at.Scope.ReadsRecord(modelCallDomain, &t.Project)); err != nil {
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
// one of byNode's nodes in the task's own project (projects): the earliest
// start, then the smallest id.
func firstAttempts(tx *eventstore.Tx, byNode map[types.NodeID]graph.NodeView, projects map[types.TaskID]types.ProjectID) (map[types.TaskID]started, error) {
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
		if project, has := projects[n.Task]; !ok || !has || !began || !unit || !known || n.Project != project {
			continue
		}
		if f, seen := out[n.Task]; !seen || at.Before(f.at) || at.Equal(f.at) && a.ID < f.attempt.ID {
			out[n.Task] = started{attempt: a, node: node, at: at}
		}
	}
	return out, err
}

// metrics is task t's first-attempt metrics from f; tokens: with the
// tokens of its calls.
func metrics(tx *eventstore.Tx, t authority.TaskRecord, f started, tokens bool) (*types.FirstAttempt, error) {
	m := &types.FirstAttempt{Attempt: f.attempt.ID, Node: f.node, StartedAt: types.Time(f.at.UTC().Format(time.RFC3339Nano)),
		Status: string(f.attempt.Status)}
	if r, ok, err := graph.AttemptResult(tx, f.attempt.ID); err != nil {
		return nil, err
	} else if ok {
		m.Outcome, m.Reason = &r.Outcome, &r.Reason
	}
	if a := t.Approval; a != nil {
		approved, err := time.Parse(time.RFC3339Nano, string(a.At))
		if err != nil {
			return nil, err
		}
		s := int64(max(f.at.Sub(approved), 0) / time.Second)
		m.SecondsAfterApproval = &s
	}
	if !tokens {
		return m, nil
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
