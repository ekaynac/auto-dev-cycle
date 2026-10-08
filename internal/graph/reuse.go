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
//     records them as its claim, so the reused node can be invalidated like
//     any claimed one. It never claims the key and never runs.

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
	if err != nil || !taken { // a key of the current generation is never stale
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
	// The key names the kind, so its holder is of the same kind; a holder
	// with no selected result leaves a completed state with none, which
	// records/node-state refuses.
	if err = errors.Join(err, herr); err != nil || !held || holder != p.Holder || *h.State.Key != key {
		return errors.Join(err, errors.New("the holder is the node that holds the key in the task"))
	}
	if err := put(tx, ClaimProjection, string(p.Node), claim{Key: key, Inputs: p.Inputs, Command: ev.Command}); err != nil {
		return err
	}
	rec.State.State, rec.State.WaitingOn, rec.State.Key, rec.State.Result = types.NodeStateStateCompleted, []types.Name{}, &key, h.State.Result
	return putNode(tx, rec)
}
