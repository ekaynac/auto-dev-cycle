// SPDX-License-Identifier: Apache-2.0

package graph

// Renewal (13 [G1], [G2] freshness; plan 15 G4a-2).
//
// Decisions:
//   - Renewal is the one exception to "a key selects once": once the receipt
//     of a memoizable node's selected result has passed its valid_until at
//     the event's time, node.renewed reopens the same key, claimed with no
//     selected result, so a new attempt records and selects a fresher result.
//     The generation, the claim and its command stay: counters do not reset,
//     and the old selection stays in the log as history.
//   - A renewal leaves a selection boundary (graph_renewals): the node then
//     selects only a result recorded after the renewal, never the renewed
//     result or another recorded before it, and a result renews once. Its
//     receipt must be produced at or after the renewal: RecordResult and the
//     resolver refuse an older one (the fold reads no blob).
//   - The fold checks expiry against the event's time, never wall time, and
//     only from the validity the result bound when it was recorded
//     (RecordResult reads it from the receipt blob); the event must name that
//     validity and the result's one receipt Ref. The fold reads no blob: a
//     result.recorded event with a forged binding is refused by the resolver
//     and by Renew, which re-read the blob, so it never serves.
//   - A reused node is renewed through its holder (ErrReused). The holder's
//     renewal releases the nodes bound to the key in the same command, as an
//     invalidation does; they reuse again once the holder completes.
//   - A could_not_run result needs no renewal: it is never selected, and it
//     retries on its signal (Retry, holds.go) under the same key.

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
)

// Renew is the event reopening node's key once its selected result's
// receipt has expired, then one releasing each node bound to the key.
func Renew(tx *eventstore.Tx, node types.NodeID, blobs receipt.Blobs) ([]eventstore.Event, error) {
	rec, err := loadNode(tx, node)
	if err != nil {
		return nil, err
	}
	if rec.State.State != types.NodeStateStateCompleted {
		return nil, fmt.Errorf("graph: %s is not completed", node)
	}
	if err := inputsFenced(tx, node); err != nil {
		return nil, err
	}
	// Only a memoizable node holds its key in the task's index.
	if holder, _, err := load[types.NodeID](tx, KeyProjection, keyIndex(rec.Recorded.Task, rec.State.Key.Key)); err != nil || holder != node {
		return nil, errors.Join(err, ErrReused{holder})
	}
	r, _, err := load[types.ResultRecorded](tx, ResultProjection, string(*rec.State.Result))
	if err != nil {
		return nil, err
	}
	ref, rcpt, err := receiptOf(r, blobs)
	if err != nil {
		return nil, err
	}
	if rcpt.ValidUntil == nil {
		return nil, fmt.Errorf("graph: %s's receipt does not expire", node)
	}
	if expired, err := expiredAt(*rcpt.ValidUntil, tx.Now().UTC().Format(time.RFC3339Nano)); err != nil || !expired {
		return nil, errors.Join(err, fmt.Errorf("graph: %s's receipt is fresh until %s", node, *rcpt.ValidUntil))
	}
	if mutation.Is(mutation.RenewalInvalidates) {
		return Invalidate(tx, node)
	}
	evs, err := event(NodeRenewed, string(node), rec.Recorded.Project, &types.NodeRenewed{Type: "node_renewed", Node: node, Key: *rec.State.Key,
		Result: *rec.State.Result, Receipt: ref.ContentDigest, ValidUntil: *rcpt.ValidUntil})
	release, rerr := releases(tx, rec.Recorded.Task, *rec.State.Key)
	return append(evs, release...), errors.Join(err, rerr)
}

// expiredAt: a receipt valid until until has expired at at.
func expiredAt(until types.Time, at string) (bool, error) {
	u, err1 := time.Parse(time.RFC3339Nano, string(until))
	t, err2 := time.Parse(time.RFC3339Nano, at)
	return !t.Before(u), errors.Join(err1, err2)
}

func renewed(tx *eventstore.Tx, ev eventstore.Event, p *types.NodeRenewed) error {
	rec, err := nodeOf(tx, ev, p.Node)
	if err != nil || ev.Kind != NodeRenewed || ev.Subject != string(p.Node) {
		return errors.Join(err, errors.New("a renewal is under its node"))
	}
	if rec.State.State != types.NodeStateStateCompleted || *rec.State.Key != p.Key || *rec.State.Result != p.Result {
		return errors.New("a renewal is of a completed node's key and selected result")
	}
	if err := inputsFenced(tx, p.Node); err != nil {
		return err
	}
	holder, _, err := load[types.NodeID](tx, KeyProjection, keyIndex(rec.Recorded.Task, p.Key.Key))
	// Only a memoizable node holds a key in the index, and only its current key.
	if err != nil || holder != p.Node {
		return errors.Join(err, errors.New("a renewal is of the memoizable node that holds the key"))
	}
	r, _, err := load[types.ResultRecorded](tx, ResultProjection, string(p.Result))
	ref, rerr := receiptRef(r.Result)
	if err = errors.Join(err, rerr); err != nil || ref.ContentDigest != p.Receipt ||
		r.ReceiptValidUntil == nil || !sameTime(string(*r.ReceiptValidUntil), string(p.ValidUntil)) {
		return errors.Join(err, errors.New("a renewal names the selected result's receipt and the validity it bound"))
	}
	if expired, err := expiredAt(p.ValidUntil, ev.At); err != nil || !expired {
		return errors.Join(err, errors.New("a renewal comes once the receipt has expired"))
	}
	ren, _, err := load[renewal](tx, RenewalProjection, string(p.Node))
	if err != nil || slices.Contains(ren.Renewed, p.Result) {
		return errors.Join(err, errors.New("a result renews once"))
	}
	if err := put(tx, RenewalProjection, string(p.Node), renewal{Renewed: append(ren.Renewed, p.Result), After: []types.ResultID{}, At: types.Time(ev.At)}); err != nil {
		return err
	}
	rec.State.Result = nil
	settle(&rec.State)
	return putNode(tx, rec)
}

// renewal is a node's selection boundary: the results it renewed, and those
// recorded since its last renewal, the only ones it may select.
type renewal struct {
	Renewed []types.ResultID `json:"renewed"`
	After   []types.ResultID `json:"after"`
	At      types.Time       `json:"at"` // the last renewal's event time
}
