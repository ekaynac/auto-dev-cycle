// SPDX-License-Identifier: Apache-2.0

package graph

// Results, selection and invalidation (13 [G1], [G2]; plan 15 G3-2). Hold
// events follow in G3-3.
//
// Decisions:
//   - A result is recorded by a claimed node, for its current key, of a
//     kind its node kind produces, once. RecordResult runs the writer's
//     Check; the fold runs CheckRead, so a log replays whatever reason the
//     registry deprecates later. A result for an old key that arrives
//     after an invalidation is refused; its attempt record keeps the facts.
//   - A selection takes one of the node's results answering its current
//     key and not could_not_run, and completes the node: selection is per
//     node (13 [G1]; one per key for the memoizable kinds).
//   - An invalidation moves a claimed, held or completed node to the next
//     generation of the same inputs: a new key and no selected result. For
//     a memoizable kind the task's current generation of the node's
//     identity (its inputs, without a generation) moves with it, the new
//     key is claimed in the task and the old one is left stale, so a later
//     claim of it is ErrKeyStale. Results of the old key stay, history,
//     never selected again.

import (
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// produces binds a node kind to the result kinds it records, deny by
// default: a kind without a row records nothing until its PR adds the row
// (accept, closure, deploy, record, phase_gate). runtime_verify records a
// gate_result until its own kind is settled with the facts work (G8).
var produces = map[types.NodeKind][]types.ResultKind{
	types.NodeKindPlan:          {types.ResultKindPlan},
	types.NodeKindBuild:         {types.ResultKindPatch, types.ResultKindPartialPatch},
	types.NodeKindGate:          {types.ResultKindGateResult},
	types.NodeKindRuntimeVerify: {types.ResultKindGateResult},
	types.NodeKindIntegration:   {types.ResultKindTree, types.ResultKindPackage},
	types.NodeKindEvidence:      {types.ResultKindEvidence},
	types.NodeKindReview:        {types.ResultKindReview},
	types.NodeKindCarryOver:     {types.ResultKindCarryOverAttestation},
	types.NodeKindMerge:         {types.ResultKindMergeResult},
}

// RecordResult is the event recording r, a valid result of node's claimed key.
func RecordResult(tx *eventstore.Tx, r types.ResultRecorded) ([]eventstore.Event, error) {
	rec, err := loadNode(tx, r.Node)
	if err == nil {
		err = Check(r.Result)
	}
	if err != nil {
		return nil, err
	}
	r.Type = "result_recorded"
	return event(ResultRecorded, string(r.Result.ID), rec.Recorded.Project, &r)
}

// Select is the event selecting result for node.
func Select(tx *eventstore.Tx, node types.NodeID, result types.ResultID) ([]eventstore.Event, error) {
	return onNode(tx, node, ResultSelected, string(result), &types.ResultSelected{Type: "result_selected", Node: node, Result: result})
}

// Invalidate is the event moving node's claim to the next generation. A
// memoizable key of that generation held by another node of the task is
// ErrKeyClaimed.
func Invalidate(tx *eventstore.Tx, node types.NodeID) ([]eventstore.Event, error) {
	c, ok, err := load[claim](tx, ClaimProjection, string(node))
	if err != nil || !ok {
		return nil, errors.Join(err, fmt.Errorf("graph: %s is not claimed", node))
	}
	rec, err := loadNode(tx, node)
	if err != nil {
		return nil, err
	}
	key, err := Key(rec.Recorded.Node, resolutions(c.Inputs), c.Key.Generation+1)
	if err != nil {
		return nil, err
	}
	if slices.Contains(memoizable, rec.Recorded.Node.Kind) {
		if holder, taken, err := load[types.NodeID](tx, KeyProjection, keyIndex(rec.Recorded.Task, key.Key)); err != nil {
			return nil, err
		} else if taken {
			return nil, ErrKeyClaimed{holder}
		}
	}
	return onNode(tx, node, NodeInvalidated, string(node), &types.NodeInvalidated{Type: "node_invalidated", Node: node, Key: key})
}

func onNode(tx *eventstore.Tx, node types.NodeID, kind, subject string, v types.GraphEventVariant) ([]eventstore.Event, error) {
	rec, err := loadNode(tx, node)
	if err != nil {
		return nil, err
	}
	return event(kind, subject, rec.Recorded.Project, v)
}

func resultRecorded(tx *eventstore.Tx, ev eventstore.Event, p *types.ResultRecorded) error {
	rec, err := nodeOf(tx, ev, p.Node)
	if err != nil || ev.Kind != ResultRecorded || ev.Subject != string(p.Result.ID) {
		return errors.Join(err, errors.New("a result is recorded under its id"))
	}
	if rec.State.State != types.NodeStateStateClaimed || rec.State.Key.Key != p.Result.Computation {
		return errors.New("the result does not answer its claimed node's key")
	}
	if !slices.Contains(produces[rec.Recorded.Node.Kind], p.Result.Kind) {
		return fmt.Errorf("a %s node does not record a %s result", rec.Recorded.Node.Kind, p.Result.Kind)
	}
	if err := CheckRead(p.Result); err != nil {
		return err
	}
	if _, exists, err := load[types.ResultRecorded](tx, ResultProjection, string(p.Result.ID)); err != nil || exists {
		return errors.Join(err, errors.New("the result is recorded"))
	}
	return put(tx, ResultProjection, string(p.Result.ID), p)
}

func selected(tx *eventstore.Tx, ev eventstore.Event, p *types.ResultSelected) error {
	rec, err := nodeOf(tx, ev, p.Node)
	if err != nil || ev.Kind != ResultSelected || ev.Subject != string(p.Result) {
		return errors.Join(err, errors.New("a selection is under its result"))
	}
	r, _, err := load[types.ResultRecorded](tx, ResultProjection, string(p.Result))
	if err != nil || r.Node != p.Node { // an unrecorded result names no node
		return errors.Join(err, errors.New("the result is not one of the node's"))
	}
	if rec.State.State != types.NodeStateStateClaimed || rec.State.Key.Key != r.Result.Computation {
		return fmt.Errorf("a %s node selects no result of another key", rec.State.State)
	}
	// records/result makes every incomplete result could_not_run, and
	// CheckRead refuses a completed one.
	if r.Result.Outcome == types.OutcomeCouldNotRun {
		return errors.New("only a completed found, empty or failed result is selected")
	}
	rec.State.State, rec.State.Result = types.NodeStateStateCompleted, &p.Result
	return putNode(tx, rec)
}

func invalidated(tx *eventstore.Tx, ev eventstore.Event, p *types.NodeInvalidated) error {
	rec, err := nodeOf(tx, ev, p.Node)
	if err != nil || ev.Kind != NodeInvalidated || ev.Subject != string(p.Node) {
		return errors.Join(err, errors.New("an invalidation is under its node"))
	}
	if s := rec.State.State; s != types.NodeStateStateClaimed && s != types.NodeStateStateCompleted && s != types.NodeStateStateHeld {
		return fmt.Errorf("a %s node is not invalidated: only a claimed, held or completed one", s)
	}
	c, ok, err := load[claim](tx, ClaimProjection, string(p.Node))
	if err != nil || !ok {
		return errors.Join(err, errors.New("an unclaimed node is not invalidated"))
	}
	resolved := resolutions(c.Inputs)
	key, kerr := Key(rec.Recorded.Node, resolved, c.Key.Generation+1)
	if err = errors.Join(err, kerr); err != nil || key != p.Key {
		return errors.Join(err, errors.New("the new key is not the next generation's"))
	}
	if slices.Contains(memoizable, rec.Recorded.Node.Kind) {
		identity, err := Key(rec.Recorded.Node, resolved, 1)
		if err != nil {
			return err
		}
		if err := errors.Join(put(tx, GenProjection, keyIndex(rec.Recorded.Task, identity.Key), key.Generation),
			put(tx, KeyProjection, keyIndex(rec.Recorded.Task, c.Key.Key), types.NodeID(""))); err != nil {
			return err
		}
		if err := claimKey(tx, rec, resolved, key); err != nil {
			return err
		}
	}
	c.Key = key
	if err := put(tx, ClaimProjection, string(p.Node), c); err != nil {
		return err
	}
	// A held node's holds end with its generation; their events keep them.
	rec.State.State, rec.State.Key, rec.State.Result, rec.State.Holds = types.NodeStateStateClaimed, &key, nil, []types.NodeHold{}
	return putNode(tx, rec)
}
