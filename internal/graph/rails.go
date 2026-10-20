// SPDX-License-Identifier: Apache-2.0

package graph

// The count rails (M1 P17a; I16; slice plan §6 L3): attempts per round,
// rounds per task and merge operations per generation.
//
// Decisions:
//   - A count is derived from records an invalidation never rewrites, so
//     no invalidation resets it: a round's attempts are its unit's fencing
//     token (dispatch.Launch); a build's round is its node's Round; a merge
//     node's operations are the effects intended for each of its keys,
//     every generation of its claim (Operate).
//   - Reaching a rail holds the node on budget_rail, owned by the operator,
//     its signal the rail's (rail:<name>), instead of going on: the
//     evaluator before claiming a build past its rounds, Operate before
//     intending a merge past its operations, dispatch.Launch before
//     starting an attempt past its round's. A refused launch starts no
//     attempt, so it counts against no rail.
//   - A task's caps are its project profile's, absent ones the defaults,
//     raised for the task by task.raise_rail (authority.Rails), which
//     clears the task's holds on that rail (ClearRail).

import (
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The count rails, by their profile and task.raise_rail names.
const (
	RailAttempts = "attempts_per_round"
	RailRounds   = "rounds_per_task"
	RailMergeOps = "merge_operations_per_generation"
)

// Caps are a task's count rails in force.
type Caps struct{ AttemptsPerRound, RoundsPerTask, MergeOperationsPerGeneration int64 }

// DefaultCaps are the caps of a profile that names none (the operator's,
// 2026-10-20).
var DefaultCaps = Caps{AttemptsPerRound: 3, RoundsPerTask: 5, MergeOperationsPerGeneration: 2}

// CapsOf are r's count rails, a default for each one absent.
func CapsOf(r types.ProjectProfileBudgetRails) Caps {
	c := DefaultCaps
	for _, f := range []struct {
		v  *int64
		to *int64
	}{{r.AttemptsPerRound, &c.AttemptsPerRound}, {r.RoundsPerTask, &c.RoundsPerTask}, {r.MergeOperationsPerGeneration, &c.MergeOperationsPerGeneration}} {
		if f.v != nil {
			*f.to = *f.v
		}
	}
	return c
}

// Of is c's cap on rail; false for a rail that is not a count rail.
func (c Caps) Of(rail string) (int64, bool) {
	switch rail {
	case RailAttempts:
		return c.AttemptsPerRound, true
	case RailRounds:
		return c.RoundsPerTask, true
	case RailMergeOps:
		return c.MergeOperationsPerGeneration, true
	}
	return 0, false
}

// Rails are a task's caps in force in tx; nil is DefaultCaps for every
// task.
type Rails func(tx *eventstore.Tx, task types.TaskID) (Caps, error)

func (r Rails) of(tx *eventstore.Tx, task types.TaskID) (Caps, error) {
	if r == nil {
		return DefaultCaps, nil
	}
	return r(tx, task)
}

// RailSignal is the signal of a budget_rail hold on rail.
func RailSignal(rail string) string { return "rail:" + rail }

// RailHold is a budget_rail hold on rail, as Raise takes it.
func RailHold(rail string) (types.NodeHold, error) {
	id, err := ids.New("hld")
	return types.NodeHold{ID: types.HoldID(id), Hold: vocab.HoldBudgetRail, SignalInstance: RailSignal(rail), Owner: types.OwnerOperator,
		Pages: []types.NodeHoldPagesItem{}}, err
}

// TaskOf is node's task.
func TaskOf(tx *eventstore.Tx, node types.NodeID) (types.TaskID, error) {
	rec, err := loadNode(tx, node)
	return rec.Recorded.Task, err
}

// RoundAttempts are the attempts started on node's round, which its
// attempt rail counts (P17a): its unit's fencing token (every attempt,
// through every invalidation of the node), added up with those of the
// nodes it replaces, since a superseded review's replacement keeps its
// round (13:173).
func RoundAttempts(tx *eventstore.Tx, node types.NodeID) (int64, error) {
	started := int64(0)
	for id := &node; id != nil; {
		rec, err := loadNode(tx, *id)
		if err != nil {
			return 0, err
		}
		cur, ok, err := lease.CurrentAttempt(tx, UnitOf(*id))
		if err != nil {
			return 0, err
		}
		if ok {
			a, _, _, err := lease.AttemptOf(tx, cur)
			if err != nil {
				return 0, err
			}
			started += a.FencingToken
		}
		id = rec.Replaces
	}
	return started, nil
}

// railHeld: rec already holds budget_rail on rail.
func railHeld(rec nodeRecord, rail string) bool {
	for _, h := range rec.State.Holds {
		if h.Hold == vocab.HoldBudgetRail && h.SignalInstance == RailSignal(rail) {
			return true
		}
	}
	return false
}

// roundRail holds rec, a build not yet claimed, on its task's round rail
// when its round is past it; held: it is (or already was).
func (e *evaluation) roundRail(rec nodeRecord) (held bool, err error) {
	if rec.Recorded.Node.Kind != types.NodeKindBuild {
		return false, nil
	}
	caps, err := e.env.Rails.of(e.tx, rec.Recorded.Task)
	if err != nil || rec.Recorded.Node.Round <= caps.RoundsPerTask {
		return false, err
	}
	if railHeld(rec, RailRounds) {
		return true, nil
	}
	h, err := RailHold(RailRounds)
	if err == nil {
		var evs []eventstore.Event
		evs, err = Raise(e.tx, rec.Recorded.Node.ID, h, nil)
		e.evs = append(e.evs, evs...)
	}
	return true, err
}

// ClearRail is the events clearing every budget_rail hold on rail of
// task's nodes, on its signal: task.raise_rail's.
func ClearRail(tx *eventstore.Tx, task types.TaskID, rail string) ([]eventstore.Event, error) {
	if _, ok := DefaultCaps.Of(rail); !ok {
		return nil, fmt.Errorf("graph: %s is no count rail", rail)
	}
	keys, err := tx.Keys(NodeProjection, "")
	if err != nil {
		return nil, err
	}
	var out []eventstore.Event
	for _, k := range keys {
		rec, err := loadNode(tx, types.NodeID(k))
		if err != nil {
			return nil, err
		}
		if rec.Recorded.Task != task {
			continue
		}
		for _, h := range rec.State.Holds {
			if h.Hold == vocab.HoldBudgetRail && h.SignalInstance == RailSignal(rail) {
				evs, err := Clear(tx, rec.Recorded.Node.ID, h.ID, h.SignalInstance)
				if err != nil {
					return nil, err
				}
				out = append(out, evs...)
			}
		}
	}
	return out, nil
}
