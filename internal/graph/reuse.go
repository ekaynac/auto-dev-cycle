// SPDX-License-Identifier: Apache-2.0

package graph

// The reuse binding (13 [G10], memoization; plan 15 G4a).
//
// Decisions:
//   - A memoizable instance refused a claim (ErrKeyClaimed) completes by
//     reuse: node.reused sets its key and its result from the holder's
//     selected result. The holder is the node that holds the key in the
//     task now, of the same kind, completed, at the identity's current
//     generation; so a reuse never reads a stale or unselected result.
//   - The instance must be waiting or runnable: a held instance waits for
//     its hold first, and a claimed or completed one has its own result.
//   - The fold re-derives the instance's inputs and key as for a claim, and
//     records them as its claim (for the commit-time re-validation, 13
//     [G4]), and the instance among the key's reusers. It never claims the
//     key and never runs.
//   - A reused node is never invalidated on its own (ErrReused): the
//     holder's invalidation releases it in the same command
//     (node.reuse_released), which drops its claim and result and returns
//     it to waiting, so nothing serves the invalidated result through it.

import (
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// Reuse is the event binding node to the result of the node that holds its
// key in its task, from node's resolved inputs.
func Reuse(tx *eventstore.Tx, node types.NodeID, resolved map[string]Resolved) ([]eventstore.Event, error) {
	rec, err := loadNode(tx, node)
	if err != nil {
		return nil, err
	}
	cur, err := current(tx, rec, resolved)
	if err != nil {
		return nil, err
	}
	key, err := Key(rec.Recorded.Node, resolved, cur)
	if err != nil {
		return nil, err
	}
	holder, taken, err := load[types.NodeID](tx, KeyProjection, keyIndex(rec.Recorded.Task, key.Key))
	if err != nil || !taken || holder == "" {
		return nil, errors.Join(err, fmt.Errorf("graph: no node holds %s's key in its task", node))
	}
	r := types.NodeReused{Type: "node_reused", Node: node, Holder: holder, Key: key, Inputs: items(rec.Recorded.Node, resolved)}
	return event(NodeReused, string(node), rec.Recorded.Project, &r)
}

func reused(tx *eventstore.Tx, ev eventstore.Event, p *types.NodeReused) error {
	rec, err := nodeOf(tx, ev, p.Node)
	if err != nil || ev.Kind != NodeReused || ev.Subject != string(p.Node) {
		return errors.Join(err, errors.New("a reuse is under its node"))
	}
	if s := rec.State.State; s != types.NodeStateStateWaiting && s != types.NodeStateStateRunnable {
		return fmt.Errorf("a %s node is not reused", s)
	}
	resolved := resolutions(p.Inputs)
	if !reflect.DeepEqual(items(rec.Recorded.Node, resolved), p.Inputs) {
		return errors.New("the reuse's inputs are not one resolution per declared input, in order")
	}
	if err := selectedInputs(tx, rec, resolved); err != nil {
		return err
	}
	cur, err := current(tx, rec, resolved)
	key, kerr := Key(rec.Recorded.Node, resolved, cur)
	if err = errors.Join(err, kerr); err != nil || key != p.Key || !slices.Contains(memoizable, rec.Recorded.Node.Kind) {
		return errors.Join(err, errors.New("a reuse is of a memoizable node's key at the current generation"))
	}
	holder, held, err := load[types.NodeID](tx, KeyProjection, keyIndex(rec.Recorded.Task, key.Key))
	h, herr := nodeOf(tx, ev, p.Holder)
	// The key names the kind, so its holder is of the same kind.
	if err = errors.Join(err, herr); err != nil || !held || holder != p.Holder || *h.State.Key != key ||
		h.State.State != types.NodeStateStateCompleted || h.State.Result == nil {
		return errors.Join(err, errors.New("the holder is the completed node that holds the key in the task"))
	}
	index := keyIndex(rec.Recorded.Task, key.Key)
	reusers, _, err := load[[]types.NodeID](tx, ReuseProjection, index)
	if err = errors.Join(err, put(tx, ReuseProjection, index, append(reusers, p.Node)),
		put(tx, ClaimProjection, string(p.Node), claim{Key: key, Inputs: p.Inputs, Command: ev.Command})); err != nil {
		return err
	}
	rec.State.State, rec.State.WaitingOn, rec.State.Key, rec.State.Result = types.NodeStateStateCompleted, []types.Name{}, &key, h.State.Result
	return putNode(tx, rec)
}

// reuseReleased drops a reuse whose key's holder was invalidated or is
// renewing: the node waits for its inputs again, with its holds.
func reuseReleased(tx *eventstore.Tx, ev eventstore.Event, p *types.NodeReuseReleased) error {
	rec, err := nodeOf(tx, ev, p.Node)
	if err != nil || ev.Kind != ReuseReleased || ev.Subject != string(p.Node) {
		return errors.Join(err, errors.New("a reuse is released under its node"))
	}
	index := keyIndex(rec.Recorded.Task, p.Key.Key)
	reusers, _, err := load[[]types.NodeID](tx, ReuseProjection, index)
	holder, _, herr := load[types.NodeID](tx, KeyProjection, index)
	at := slices.Index(reusers, p.Node)
	if holder != "" && herr == nil {
		h, lerr := loadNode(tx, holder)
		if herr = lerr; lerr == nil && h.State.State == types.NodeStateStateCompleted {
			at = -1
		}
	}
	if err = errors.Join(err, herr); err != nil || at < 0 {
		return errors.Join(err, errors.New("a reuse is released only once the holder of its key is invalidated or renewing"))
	}
	if err := errors.Join(put(tx, ReuseProjection, index, slices.Delete(reusers, at, at+1)), tx.Delete(ClaimProjection, string(p.Node))); err != nil {
		return err
	}
	rec.State.WaitingOn, rec.State.Key, rec.State.Result = waitingOn(rec.Recorded.Node), nil, nil
	settle(&rec.State)
	return putNode(tx, rec)
}
