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
//   - its charge (P22a-5c-2c; Astra #335 L3; slice plan §4.3), with its
//     tokens: the settled calls' usage at the lease's registry price
//     (Lease.Price, ledger.PriceMicros), unknown (null), never zero, when
//     the lease records no price or a settled call's usage is unknown,
//     saturated or inconsistent; the calls not settled yet listed apart,
//     their count and what the USD rail counts of them (dispatch.Cost:
//     while the lease is live, its reservation or its usage so far, the
//     larger, at the higher token rate; once ended, the liability its
//     settlement kept), null with no price or past the contract's
//     integers. A reported cost is never read (#226 entry 62).
//   - bench, the task's benchmark facts (P25; taskbench.go), only in the
//     one task's read, GET /v1/tasks/{task} (ReadTask): never in the list,
//     which would carry every task's ledger in one response, past the
//     client's response bound (Opus #430 H1). Null in the list.
//
// Its work is bounded by what the board reads and the task's own nodes:
// the first attempt is read from each node's first-attempt index
// (lease.FirstOn), and only for a task whose board and attempts the reader
// reads, never by scanning the installation's attempts (Astra #335 M1).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/dispatch"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
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
		byTask := map[types.TaskID][]graph.NodeView{}
		for _, n := range nodes {
			byTask[n.Task] = append(byTask[n.Task], n)
		}
		out := []any{}
		for _, id := range ids {
			read, ok, err := taskRead(tx, at, id, byTask[id], false)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, read)
			}
		}
		return out, nil
	},
	Project: func(tx *eventstore.Tx, rec any) (*types.ProjectID, error) {
		r, _ := rec.(types.TaskRead)
		_, _, project, err := boardTask(tx, r.ID)
		return project, err
	},
	Fields: []string{"id", "project", "card", "first_attempt", "bench"},
}

// TaskPath is the one task's read: the tasks resource's list holding that
// task's record alone, its benchmark facts included (P25; Opus #430 H1).
// Its one query parameter, without=bench, leaves the facts out (null), so
// a poll (adc task wait) never pays for the task's ledger (Opus #432 L1).
//
// The bound (stated, Opus #432 L1): the facts cost about 1.2 KB a model
// call (its measurements and ids), so one task's read passes the client's
// 1 MiB response bound past about 850 calls in that one task. At (a) a
// task's calls are bounded by its rounds and turns rails, far below that;
// past it, adc task show and adc dev bench record fail, never truncate,
// and adc task wait (without=bench) still reads.
const TaskPath = "/v1/tasks/{task}"

// WithoutBench is TaskPath's query that leaves the benchmark facts out.
const WithoutBench = "without=bench"

// ReadTask is task id's record as the one task's read gives it, read at
// at: its card, its first attempt and, with bench, its benchmark facts
// (under both the attempt and model_call read kinds); false when the
// caller reads no such task. The task's board scope is checked first, so
// nothing of a task the caller does not read is read, nor its damage
// shown (Opus #432 L2). Only that task's nodes, leases and calls are
// read, and a record of it that cannot be read fails this read alone
// (Opus #430 M1).
func ReadTask(tx *eventstore.Tx, at ReadAt, id types.TaskID, bench bool) (types.TaskRead, bool, error) {
	t, recorded, _, err := boardTask(tx, id)
	if err != nil || !recorded || !at.Scope.ReadsRecord(boardDomain, &t.Project) {
		return types.TaskRead{}, false, err
	}
	nodes, err := graph.TaskNodes(tx, id)
	if err != nil {
		return types.TaskRead{}, false, err
	}
	return taskRead(tx, at, id, nodes, bench)
}

// taskRead is task id's record over its nodes, with its benchmark facts
// when bench; false for a task not recorded or whose board the caller does
// not read.
func taskRead(tx *eventstore.Tx, at ReadAt, id types.TaskID, nodes []graph.NodeView, bench bool) (types.TaskRead, bool, error) {
	t, recorded, _, err := boardTask(tx, id)
	if err != nil || !recorded || !at.Scope.ReadsRecord(boardDomain, &t.Project) { // a damaged record is the board's to show
		return types.TaskRead{}, false, err
	}
	read := types.TaskRead{ID: id, Project: t.Project}
	if ending, err := endingOf(tx, t); err != nil { // one task's unreadable record (Mythos chunk 5a L4)
		read.Card = unplaced(id, &t.Project, types.BoardUnplacedCardReasonDamagedRecord)
	} else {
		read.Card = boardCard(t, nodes, at.Now, at.Scope.ReadsRecord(holdDomain, &t.Project), ending, stalledOf(at, t))
	}
	if at.Scope.ReadsRecord(attemptDomain, &t.Project) {
		f, ok, err := firstAttempt(tx, nodes)
		if err == nil && ok {
			read.FirstAttempt, err = metrics(tx, t, f, at.Scope.ReadsRecord(modelCallDomain, &t.Project))
		}
		if err == nil && bench && at.Scope.ReadsRecord(modelCallDomain, &t.Project) {
			read.Bench, err = benchOf(tx, t, nodes)
		}
		if err != nil {
			return types.TaskRead{}, false, err
		}
	}
	return read, true, nil
}

// Task serves TaskPath: the tasks list holding task's record, or none when
// the caller reads no such task (never an existence oracle).
func (h Reads) Task(w http.ResponseWriter, r *http.Request) {
	id := types.TaskID(r.PathValue("task"))
	if ids.Check(string(id), "tsk") != nil {
		http.Error(w, "not a task id", http.StatusBadRequest)
		return
	}
	bench := r.URL.RawQuery == ""
	if !bench && r.URL.RawQuery != WithoutBench {
		http.Error(w, "the one task's read takes only "+WithoutBench, http.StatusBadRequest)
		return
	}
	r = r.Clone(r.Context()) // view takes no query: the mask is read here
	r.URL.RawQuery = ""
	h.view(w, r, func(tx *eventstore.Tx, _ identity.Principal, _ types.Actor, at ReadAt) (any, error) {
		if at.Scope.Empty() {
			return nil, errNotGranted
		}
		out := types.ReadList{Resource: tasksName, Outcome: types.ReadListOutcomeEmpty, Items: []json.RawMessage{}}
		rec, ok, err := ReadTask(tx, at, id, bench)
		if err != nil || !ok {
			return out, err
		}
		item, err := allowed(rec, tasks.Fields)
		if err != nil {
			return nil, err
		}
		out.Items, out.Outcome = append(out.Items, item), types.ReadListOutcomeFound
		return out, nil
	})
}

// tasksName is the tasks resource's name (Registered).
const tasksName = "tasks"

// started is an attempt, its node and its start time.
type started struct {
	attempt types.Attempt
	node    types.NodeID
	at      time.Time
}

// firstAttempt is a task's first attempt: of its nodes (all in its project:
// the node fold refuses another), each node's first (lease.FirstOn), the
// earliest start, then the smallest id; false before one starts.
func firstAttempt(tx *eventstore.Tx, nodes []graph.NodeView) (started, bool, error) {
	var f started
	seen := false
	for _, n := range nodes {
		id, ok, err := lease.FirstOn(tx, graph.UnitOf(n.Node.ID))
		if err != nil || !ok {
			if err != nil {
				return f, false, err
			}
			continue
		}
		a, _, known, aerr := lease.AttemptOf(tx, id)
		at, began, serr := lease.StartedAt(tx, id)
		if err := errors.Join(aerr, serr); err != nil {
			return f, false, err
		}
		if !known || !began {
			return f, false, fmt.Errorf("api: node %s's first attempt %s has no record or no start", n.Node.ID, id)
		}
		if !seen || at.Before(f.at) || at.Equal(f.at) && a.ID < f.attempt.ID {
			f, seen = started{attempt: a, node: n.Node.ID, at: at}, true
		}
	}
	return f, seen, nil
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
	l, _, _, err := lease.LeaseOf(tx, f.attempt.Lease)
	if err != nil {
		return nil, err
	}
	slices.Sort(calls)
	var settled int64
	settledKnown := l.Price != nil
	for _, c := range calls {
		ms, err := ledger.Measurements(tx, c)
		if err != nil {
			return nil, err
		}
		s := ledger.StandingOf(ms, c)
		if m.Charge == nil {
			m.Charge = &types.Charge{}
		}
		if !s.Settled {
			m.Charge.OutstandingCalls++
		}
		priced := int64(0)
		known := s.Usage != nil && !s.Saturated && !s.Inconsistent && l.Price != nil
		if known {
			if priced, err = ledger.PriceMicros(*s.Usage, *l.Price); err != nil {
				known = false
			}
		}
		if s.Settled {
			settled, settledKnown = settled+priced, settledKnown && known
		}
		if s.Usage == nil {
			continue
		}
		if m.Tokens == nil {
			m.Tokens = &types.Tokens{}
		}
		m.Tokens.In, m.Tokens.Out = m.Tokens.In+s.Usage.In, m.Tokens.Out+s.Usage.Out
	}
	if m.Charge != nil {
		if settledKnown {
			m.Charge.SettledUsdMicros = &settled
		}
		if m.Charge.OutstandingCalls > 0 && l.Price != nil && len(calls) == 1 { // the rail's own figure (Opus 22a-5c-2c M1): one call a lease
			outstanding, err := dispatch.Cost(tx, l)
			if err != nil {
				return nil, err
			}
			if outstanding != dispatch.Unaffordable {
				m.Charge.OutstandingUsdMicros = &outstanding
			}
		}
	}
	return m, nil
}
