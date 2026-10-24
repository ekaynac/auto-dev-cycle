// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// TimesProjection: node id → its node.claimed and result.selected event
// times (the benchmark's stage wall time, §2.8; adc dev bench record, P25),
// read by the node's id, never by scanning the log. A node completed by
// reuse (node.reused) is stamped claimed and selected at its reuse's time,
// over any earlier row (it took no time of its own), and a released reuse
// clears its row, as it clears its claim and result (Opus #432 r2 L1).
const TimesProjection = "graph_node_times"

// ErrTimesMissing: a node holds a claim or a selected result, its evidence
// of having been claimed or selected, but no time of it: the projection is
// damaged (Astra #432 H2). Never read as a node not yet claimed.
var ErrTimesMissing = errors.New("graph: a node's claim or selection has no recorded time")

// Times are a node's controller event times: its first node.claimed, and
// its latest result.selected (both its latest node.reused's when reused); zero
// while it has none.
type Times struct {
	Claimed  time.Time
	Selected time.Time
}

// times is Times as stored: RFC 3339 event times, "" for none.
type times struct {
	Claimed  string `json:"claimed"`
	Selected string `json:"selected"`
}

// TimesOf is node's Times, checked against the node's own evidence: a
// node holding a claim has a claim time, and one holding a selected result
// a selection time; otherwise ErrTimesMissing (Astra #432 H2).
func TimesOf(tx *eventstore.Tx, node types.NodeID) (Times, error) {
	t, _, err := load[times](tx, TimesProjection, string(node))
	var out Times
	for _, f := range []struct {
		at  string
		dst *time.Time
	}{{t.Claimed, &out.Claimed}, {t.Selected, &out.Selected}} {
		if err == nil && f.at != "" {
			*f.dst, err = time.Parse(time.RFC3339Nano, f.at)
		}
	}
	if err != nil {
		return out, err
	}
	rec, err := loadNode(tx, node)
	_, claimed, cerr := load[claim](tx, ClaimProjection, string(node))
	switch {
	case err != nil || cerr != nil:
		return out, errors.Join(err, cerr)
	case claimed && out.Claimed.IsZero():
		return out, fmt.Errorf("%w: %s is claimed", ErrTimesMissing, node)
	case rec.State.Result != nil && out.Selected.IsZero():
		return out, fmt.Errorf("%w: %s has a selected result", ErrTimesMissing, node)
	}
	return out, nil
}

// stamp records ev's time on node: as its claim unless one is recorded
// (the first claim), else as its selection (the latest).
func stamp(tx *eventstore.Tx, ev eventstore.Event, node types.NodeID, claim bool) error {
	t, _, err := load[times](tx, TimesProjection, string(node))
	if err != nil {
		return err
	}
	if claim && t.Claimed == "" {
		t.Claimed = ev.At
	} else if !claim {
		t.Selected = ev.At
	}
	return put(tx, TimesProjection, string(node), t)
}

// reuseTimes records a reuse's times on node: claimed and selected at its
// node.reused (it took no time of its own), over any earlier row; release
// clears them, as it clears the reuse's claim and result (Opus #432 r2 L1).
func reuseTimes(tx *eventstore.Tx, ev eventstore.Event, node types.NodeID, released bool) error {
	if released {
		return tx.Delete(TimesProjection, string(node))
	}
	return put(tx, TimesProjection, string(node), times{Claimed: ev.At, Selected: ev.At})
}

// RailClearProjection: "<command>/<node>" → the rail signal of each
// budget_rail hold that command cleared on node, by its signal: a rail
// raise's (authority.Raise), whose stage is the cleared node's (§2.8; adc
// dev bench record, P25).
const RailClearProjection = "graph_rail_clears"

// RailCleared are the nodes whose budget_rail holds command, a raise of
// rail, cleared, in node id order. A row whose signal is not rail's is
// ErrIndexDamaged (Opus #432 r2 H1′).
func RailCleared(tx *eventstore.Tx, command, rail string) ([]types.NodeID, error) {
	keys, err := tx.Keys(RailClearProjection, command+"/")
	out := make([]types.NodeID, 0, len(keys))
	for _, k := range keys {
		signal, _, lerr := load[string](tx, RailClearProjection, k)
		if err = errors.Join(err, lerr); err != nil {
			return nil, err
		}
		if signal != RailSignal(rail) {
			return nil, fmt.Errorf("%w: %s cleared %s's hold, not %s's", ErrIndexDamaged, k, signal, RailSignal(rail))
		}
		out = append(out, types.NodeID(strings.TrimPrefix(k, command+"/")))
	}
	return out, err
}

// ErrIndexDamaged: an index names a record that is not the one it indexes
// (another attempt's result, another rail's clear): the projection is
// damaged, and the read through it fails (Opus #432 r2 H1′).
var ErrIndexDamaged = errors.New("graph: an index names a record not its own")

// railCleared records p, a hold cleared by its signal, when the signal is
// a rail's and the event a command's.
func railCleared(tx *eventstore.Tx, ev eventstore.Event, p *types.HoldCleared) error {
	c, ok := p.Cause.Variant.(*types.Signalled)
	if !ok || ev.Command == nil || !strings.HasPrefix(c.SignalInstance, RailSignal("")) {
		return nil
	}
	return put(tx, RailClearProjection, *ev.Command+"/"+string(p.Node), c.SignalInstance)
}
