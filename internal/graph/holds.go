// SPDX-License-Identifier: Apache-2.0

package graph

// Node holds (02 "Hold reasons and resume signals"; 13 "Evaluation",
// [G2], supersession; plan 15 G3-3).
//
// Decisions:
//   - A hold has an id no other hold had. It is raised at its event's time,
//     by its registry owner, with no page. A completed node holds nothing:
//     records/node-state has no held state with a selected result. A held node keeps its key and missing
//     inputs; once it holds nothing it is claimed, waiting or runnable
//     again, as they say.
//   - A could_not_run result retries on its signal (13 [G2]): Retry raises
//     the hold its reason names in the registry, with the reason, so the
//     node is held until the signal clears it. A reason that names no hold
//     leaves the retry to the rails (05, 12).
//   - A page is recorded at its event's time, for the hold's signal
//     instance, to the hold's owner or to the registry's escalate_to, which
//     then owns it: escalation never restarts (13 supersession).
//   - A transfer moves a hold, with its id, owner and pages, to another
//     node of the task (to the same node, it would be held twice, which
//     records/node-state refuses).
//   - An invalidation clears a held node's holds; the events keep their
//     history.

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// Raise is the event raising hold on node.
func Raise(tx *eventstore.Tx, node types.NodeID, hold types.NodeHold) ([]eventstore.Event, error) {
	return onNode(tx, node, HoldRaised, string(node), &types.HoldRaised{Type: "hold_raised", Node: node, Hold: hold})
}

// Retry is the event holding node on the signal of its could_not_run
// result: the hold the result's reason names, with that reason; hold
// carries the rest (id, signal instance, owner).
func Retry(tx *eventstore.Tx, node types.NodeID, result types.ResultID, hold types.NodeHold) ([]eventstore.Event, error) {
	r, ok, err := load[types.ResultRecorded](tx, ResultProjection, string(result))
	reg, rerr := registry()
	if err = errors.Join(err, rerr); err != nil || !ok || r.Node != node || r.Result.Outcome != types.OutcomeCouldNotRun {
		return nil, errors.Join(err, fmt.Errorf("graph: %s is no could_not_run result of %s", result, node))
	}
	entry := reg.Reasons[r.Result.Reason.String()]
	if entry.Hold == nil {
		return nil, fmt.Errorf("graph: reason %s names no hold; the rails decide its retry", r.Result.Reason)
	}
	hold.Hold, hold.Reason = string(*entry.Hold), &r.Result.Reason
	return Raise(tx, node, hold)
}

// Page is the event recording a page of node's hold.
func Page(tx *eventstore.Tx, node types.NodeID, hold types.HoldID, owner types.Owner, ownerRef *string, channel *types.Name) ([]eventstore.Event, error) {
	return onNode(tx, node, HoldPaged, string(node), &types.HoldPaged{Type: "hold_paged", Node: node, Hold: hold, Owner: owner, OwnerRef: ownerRef, Channel: channel})
}

// Clear is the event clearing node's hold.
func Clear(tx *eventstore.Tx, node types.NodeID, hold types.HoldID) ([]eventstore.Event, error) {
	return onNode(tx, node, HoldCleared, string(node), &types.HoldCleared{Type: "hold_cleared", Node: node, Hold: hold})
}

// Transfer is the event moving hold from one node to another of its task.
func Transfer(tx *eventstore.Tx, from, to types.NodeID, hold types.HoldID) ([]eventstore.Event, error) {
	return onNode(tx, to, HoldTransferred, string(to), &types.HoldTransferred{Type: "hold_transferred", From: from, To: to, Hold: hold})
}

// settle is a node's state once it holds nothing: claimed with a key,
// waiting with missing inputs, runnable otherwise.
func settle(st *types.NodeState) {
	switch {
	case st.Key != nil:
		st.State = types.NodeStateStateClaimed
	case len(st.WaitingOn) > 0:
		st.State = types.NodeStateStateWaiting
	default:
		st.State = types.NodeStateStateRunnable
	}
}

// sameTime: a and b are one instant.
func sameTime(a, b string) bool {
	x, err1 := time.Parse(time.RFC3339Nano, a)
	y, err2 := time.Parse(time.RFC3339Nano, b)
	return err1 == nil && err2 == nil && x.Equal(y)
}

func holdRaised(tx *eventstore.Tx, ev eventstore.Event, p *types.HoldRaised) error {
	rec, err := nodeOf(tx, ev, p.Node)
	if err != nil || ev.Kind != HoldRaised || ev.Subject != string(p.Node) {
		return errors.Join(err, errors.New("a hold is raised under its node"))
	}
	reg, err := registry()
	if err != nil {
		return err
	}
	h := p.Hold
	entry, registered := reg.Holds[h.Hold]
	if !registered || !sameTime(string(h.Since), ev.At) || len(h.Pages) != 0 || h.Owner != entry.Owner {
		return errors.New("a hold is a registered hold, raised at its event's time by its registry owner, with no page")
	}
	if h.Reason != nil {
		if r := reg.Reasons[h.Reason.String()]; r.Hold == nil || string(*r.Hold) != h.Hold {
			return fmt.Errorf("reason %s does not name hold %s", h.Reason, h.Hold)
		}
	}
	if _, used, err := load[types.NodeID](tx, HoldProjection, string(h.ID)); err != nil || used {
		return errors.Join(err, errors.New("the hold id is taken"))
	}
	rec.State.State, rec.State.Holds = types.NodeStateStateHeld, append(rec.State.Holds, h)
	if err := put(tx, HoldProjection, string(h.ID), p.Node); err != nil {
		return err
	}
	return putNode(tx, rec)
}

func holdPaged(tx *eventstore.Tx, ev eventstore.Event, p *types.HoldPaged) error {
	rec, err := nodeOf(tx, ev, p.Node)
	reg, rerr := registry()
	if err = errors.Join(err, rerr); err != nil || ev.Kind != HoldPaged || ev.Subject != string(p.Node) {
		return errors.Join(err, errors.New("a page is under its node"))
	}
	i := slices.IndexFunc(rec.State.Holds, func(h types.NodeHold) bool { return h.ID == p.Hold })
	if i < 0 {
		return fmt.Errorf("node %s does not hold %s", p.Node, p.Hold)
	}
	h := &rec.State.Holds[i]
	escalate := reg.Holds[h.Hold].EscalateTo
	switch {
	case p.Owner == h.Owner && (p.OwnerRef == nil) == (h.OwnerRef == nil) && (p.OwnerRef == nil || *p.OwnerRef == *h.OwnerRef):
	case escalate != nil && p.Owner == *escalate:
		h.Owner, h.OwnerRef = p.Owner, p.OwnerRef
	default:
		return errors.New("a page goes to the hold's owner or to the registry's escalate_to")
	}
	h.Pages = append(h.Pages, types.NodeHoldPagesItem{At: types.Time(ev.At), Owner: p.Owner, OwnerRef: p.OwnerRef, Channel: p.Channel, SignalInstance: h.SignalInstance})
	return putNode(tx, rec)
}

// takeHold removes hold id from rec and returns it.
func takeHold(rec *nodeRecord, id types.HoldID) (types.NodeHold, error) {
	i := slices.IndexFunc(rec.State.Holds, func(h types.NodeHold) bool { return h.ID == id })
	if i < 0 {
		return types.NodeHold{}, fmt.Errorf("node %s does not hold %s", rec.Recorded.Node.ID, id)
	}
	h := rec.State.Holds[i]
	rec.State.Holds = slices.Delete(rec.State.Holds, i, i+1)
	if len(rec.State.Holds) == 0 {
		settle(&rec.State)
	}
	return h, nil
}

func holdCleared(tx *eventstore.Tx, ev eventstore.Event, p *types.HoldCleared) error {
	rec, err := nodeOf(tx, ev, p.Node)
	if err != nil || ev.Kind != HoldCleared || ev.Subject != string(p.Node) {
		return errors.Join(err, errors.New("a hold is cleared under its node"))
	}
	if _, err := takeHold(&rec, p.Hold); err != nil {
		return err
	}
	return putNode(tx, rec)
}

func holdTransferred(tx *eventstore.Tx, ev eventstore.Event, p *types.HoldTransferred) error {
	from, err := nodeOf(tx, ev, p.From)
	to, err2 := nodeOf(tx, ev, p.To)
	if err = errors.Join(err, err2); err != nil || ev.Kind != HoldTransferred || ev.Subject != string(p.To) || from.Recorded.Task != to.Recorded.Task {
		return errors.Join(err, errors.New("a hold moves under its new node, to another node of its task"))
	}
	h, err := takeHold(&from, p.Hold)
	if err != nil {
		return err
	}
	to.State.State, to.State.Holds = types.NodeStateStateHeld, append(to.State.Holds, h)
	if err := errors.Join(putNode(tx, from), put(tx, HoldProjection, string(p.Hold), p.To)); err != nil {
		return err
	}
	return putNode(tx, to)
}
