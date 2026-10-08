// SPDX-License-Identifier: Apache-2.0

package graph

// Graph events and their projections (13 "Evaluation", plan 15 G3-1).
//
// Decisions:
//   - One claim per computation key, decided in the writer transaction: a
//     claim reads the claims projection and either appends node.claimed or
//     joins the claim already there, appending nothing. The writer runs one
//     transaction at a time, so two demands of one key never both claim.
//   - The fold recomputes the key from the recorded node and the claim's
//     inputs (Key) and refuses a claim whose key differs, so a forged or
//     replayed claim cannot bind a node to another computation.
//   - A result is recorded only for a claimed key, and selected only once,
//     only if completed and found, empty or failed (13 [G2]).
//   - The fold reads only the events and these projections, so a rebuild
//     reaches the same state. The claim keeps the command that made it
//     (ev.Command), as grant lineages do.

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// The graph event kinds and projections.
const (
	NodeRecorded   = "node.recorded/v1"
	NodeClaimed    = "node.claimed/v1"
	ResultRecorded = "result.recorded/v1"
	ResultSelected = "result.selected/v1"
	EventSchema    = "records/graph-event"

	NodeProjection   = "graph_nodes"   // node id → nodeRecord
	ClaimProjection  = "graph_claims"  // computation key → claim
	ResultProjection = "graph_results" // result id → the result.recorded payload
)

// Kinds registers the graph event kinds.
func Kinds() eventstore.Kinds {
	k := eventstore.Kinds{}
	for _, n := range []string{NodeRecorded, NodeClaimed, ResultRecorded, ResultSelected} {
		k[n] = eventstore.Kind{Schema: EventSchema}
	}
	return k
}

type nodeRecord struct {
	Recorded types.NodeRecorded `json:"recorded"`
	State    types.NodeState    `json:"state"`
}

type claim struct {
	Node    types.NodeID `json:"node"`
	Command *string      `json:"command"`
}

func load[T any](tx *eventstore.Tx, projection, key string) (T, bool, error) {
	var v T
	raw, ok, err := tx.Get(projection, key)
	if err != nil || !ok {
		return v, false, err
	}
	return v, true, json.Unmarshal(raw, &v)
}

func put(tx *eventstore.Tx, projection, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return tx.Put(projection, key, b)
}

func event(kind, subject string, project types.ProjectID, payload types.GraphEventVariant) ([]eventstore.Event, error) {
	b, err := json.Marshal(types.GraphEvent{Variant: payload})
	id, ierr := ids.New("evt")
	p := string(project)
	return []eventstore.Event{{ID: id, Kind: kind, Subject: subject, Project: &p, Payload: b}}, errors.Join(err, ierr)
}

// Record is the event that adds r's node to its task's graph.
func Record(r types.NodeRecorded) ([]eventstore.Event, error) {
	r.Type = "node_recorded"
	return event(NodeRecorded, string(r.Node.ID), r.Project, &r)
}

// Claim claims node's computation key at generation from its resolved
// inputs. It returns the claiming node: node itself with node.claimed, or,
// when the key is already claimed, that claim's node and no event (a
// duplicate demand joins it).
func Claim(tx *eventstore.Tx, node types.NodeID, resolved map[string]Resolved, generation int64) (types.NodeID, []eventstore.Event, error) {
	rec, ok, err := load[nodeRecord](tx, NodeProjection, string(node))
	if err != nil || !ok {
		return "", nil, errors.Join(err, fmt.Errorf("graph: no node %s", node))
	}
	key, err := Key(rec.Recorded.Node, resolved, generation)
	if err != nil {
		return "", nil, err
	}
	if c, ok, err := load[claim](tx, ClaimProjection, string(key.Key)); err != nil || ok {
		return c.Node, nil, err
	}
	c := types.NodeClaimed{Type: "node_claimed", Node: node, Key: key, Inputs: []types.NodeClaimedInputsItem{}}
	for _, in := range rec.Recorded.Node.Inputs {
		r := resolved[in.Name]
		item := types.NodeClaimedInputsItem{Name: in.Name}
		if r.Result != "" {
			item.Result = &r.Result
		} else {
			item.Approval, item.Revision = &r.Approval, &r.Revision
		}
		c.Inputs = append(c.Inputs, item)
	}
	evs, err := event(NodeClaimed, string(node), rec.Recorded.Project, &c)
	return node, evs, err
}

// RecordResult is the event recording r, a result of node's claimed key.
func RecordResult(tx *eventstore.Tx, r types.ResultRecorded) ([]eventstore.Event, error) {
	rec, ok, err := load[nodeRecord](tx, NodeProjection, string(r.Node))
	if err != nil || !ok {
		return nil, errors.Join(err, fmt.Errorf("graph: no node %s", r.Node))
	}
	r.Type = "result_recorded"
	return event(ResultRecorded, string(r.Result.ID), rec.Recorded.Project, &r)
}

// Select is the event selecting result for node's key.
func Select(tx *eventstore.Tx, node types.NodeID, result types.ResultID) ([]eventstore.Event, error) {
	rec, ok, err := load[nodeRecord](tx, NodeProjection, string(node))
	if err != nil || !ok {
		return nil, errors.Join(err, fmt.Errorf("graph: no node %s", node))
	}
	return event(ResultSelected, string(result), rec.Recorded.Project, &types.ResultSelected{Type: "result_selected", Node: node, Result: result})
}

// Project is the eventstore.Hooks.Project fold for the graph projections;
// other kinds pass through.
func Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		if !slices.Contains([]string{NodeRecorded, NodeClaimed, ResultRecorded, ResultSelected}, ev.Kind) {
			continue
		}
		var g types.GraphEvent
		err := json.Unmarshal(ev.Payload, &g)
		if err == nil {
			err = fold(tx, ev, g.Variant)
		}
		if err != nil {
			return fmt.Errorf("graph: event %s (%s): %w", ev.ID, ev.Kind, err)
		}
	}
	return nil
}

func fold(tx *eventstore.Tx, ev eventstore.Event, v types.GraphEventVariant) error {
	switch p := v.(type) {
	case *types.NodeRecorded:
		return recorded(tx, ev, p)
	case *types.NodeClaimed:
		return claimed(tx, ev, p)
	case *types.ResultRecorded:
		return resultRecorded(tx, ev, p)
	case *types.ResultSelected:
		return selected(tx, ev, p)
	}
	return errors.New("no payload")
}

// nodeOf loads the node an event names, which must be in the event's project.
func nodeOf(tx *eventstore.Tx, ev eventstore.Event, id types.NodeID) (nodeRecord, error) {
	rec, ok, err := load[nodeRecord](tx, NodeProjection, string(id))
	if err == nil && (!ok || ev.Project == nil || *ev.Project != string(rec.Recorded.Project)) {
		err = fmt.Errorf("node %s is not recorded in the event's project", id)
	}
	return rec, err
}

func recorded(tx *eventstore.Tx, ev eventstore.Event, p *types.NodeRecorded) error {
	n := p.Node
	if ev.Kind != NodeRecorded || ev.Subject != string(n.ID) || ev.Project == nil || *ev.Project != string(p.Project) {
		return errors.New("a node is recorded under its id, in its project")
	}
	if _, exists, err := load[nodeRecord](tx, NodeProjection, string(n.ID)); err != nil || exists {
		return errors.Join(err, errors.New("the node is recorded"))
	}
	if p.Predecessor != nil {
		pred, err := nodeOf(tx, ev, *p.Predecessor)
		if err != nil || pred.Recorded.Task != p.Task {
			return errors.Join(err, errors.New("the predecessor is not a node of the task"))
		}
	}
	st := types.NodeState{Node: n.ID, State: types.NodeStateStateRunnable, WaitingOn: []types.Name{}, Holds: []types.NodeHold{}}
	for _, in := range n.Inputs {
		st.State, st.WaitingOn = types.NodeStateStateWaiting, append(st.WaitingOn, in.Name)
	}
	return put(tx, NodeProjection, string(n.ID), nodeRecord{Recorded: *p, State: st})
}

func claimed(tx *eventstore.Tx, ev eventstore.Event, p *types.NodeClaimed) error {
	rec, err := nodeOf(tx, ev, p.Node)
	if err != nil || ev.Kind != NodeClaimed || ev.Subject != string(p.Node) {
		return errors.Join(err, errors.New("a claim is under its node"))
	}
	if s := rec.State.State; s != types.NodeStateStateWaiting && s != types.NodeStateStateRunnable {
		return fmt.Errorf("a %s node is not claimed", s)
	}
	if _, taken, err := load[claim](tx, ClaimProjection, string(p.Key.Key)); err != nil || taken {
		return errors.Join(err, errors.New("the key is claimed"))
	}
	resolved := map[string]Resolved{}
	for _, in := range p.Inputs {
		r := Resolved{}
		if in.Result != nil {
			r.Result = *in.Result
		}
		if in.Approval != nil && in.Revision != nil {
			r.Approval, r.Revision = *in.Approval, *in.Revision
		}
		resolved[in.Name] = r
	}
	key, err := Key(rec.Recorded.Node, resolved, p.Key.Generation)
	if err != nil || key != p.Key {
		return errors.Join(err, errors.New("the claim's key is not its node's key from its inputs"))
	}
	rec.State.State, rec.State.WaitingOn, rec.State.Key = types.NodeStateStateClaimed, []types.Name{}, &key
	if err := put(tx, ClaimProjection, string(key.Key), claim{Node: p.Node, Command: ev.Command}); err != nil {
		return err
	}
	return put(tx, NodeProjection, string(p.Node), rec)
}

func resultRecorded(tx *eventstore.Tx, ev eventstore.Event, p *types.ResultRecorded) error {
	rec, err := nodeOf(tx, ev, p.Node)
	if err != nil || ev.Kind != ResultRecorded || ev.Subject != string(p.Result.ID) {
		return errors.Join(err, errors.New("a result is recorded under its id"))
	}
	if rec.State.Key == nil || rec.State.Key.Key != p.Result.Computation {
		return errors.New("the result does not answer its node's claimed key")
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
	r, ok, err := load[types.ResultRecorded](tx, ResultProjection, string(p.Result))
	if err != nil || !ok || r.Node != p.Node {
		return errors.Join(err, errors.New("the result is not one of the node's"))
	}
	if rec.State.State != types.NodeStateStateClaimed {
		return fmt.Errorf("a %s node selects no result", rec.State.State)
	}
	// records/result makes every incomplete result could_not_run.
	if r.Result.Outcome == types.OutcomeCouldNotRun {
		return errors.New("only a completed found, empty or failed result is selected")
	}
	rec.State.State, rec.State.Result = types.NodeStateStateCompleted, &p.Result
	return put(tx, NodeProjection, string(p.Node), rec)
}
