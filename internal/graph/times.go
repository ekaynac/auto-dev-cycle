// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// TimesProjection: node id → its node.claimed and result.selected event
// times (the benchmark's stage wall time, §2.8; adc dev bench record, P25),
// read by the node's id, never by scanning the log.
const TimesProjection = "graph_node_times"

// Times are a node's controller event times: its first node.claimed, and
// its latest result.selected; zero while it has none.
type Times struct {
	Claimed  time.Time
	Selected time.Time
}

// times is Times as stored: RFC 3339 event times, "" for none.
type times struct {
	Claimed  string `json:"claimed"`
	Selected string `json:"selected"`
}

// TimesOf is node's Times.
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
	return out, err
}

// stamp records ev's time on node: as its claim unless one is recorded
// (the first claim), else as its selection (the latest).
func stamp(tx *eventstore.Tx, ev eventstore.Event, node types.NodeID, claim bool) error {
	t, _, err := load[times](tx, TimesProjection, string(node))
	if err != nil {
		return err
	}
	switch {
	case claim && t.Claimed != "":
		return nil
	case claim:
		t.Claimed = ev.At
	default:
		t.Selected = ev.At
	}
	return put(tx, TimesProjection, string(node), t)
}

// RailClearProjection: "<command>/<node>" → the rail signal of each
// budget_rail hold that command cleared on node, by its signal: a rail
// raise's (authority.Raise), whose stage is the cleared node's (§2.8; adc
// dev bench record, P25).
const RailClearProjection = "graph_rail_clears"

// RailCleared are the nodes whose budget_rail holds command cleared, in
// node id order.
func RailCleared(tx *eventstore.Tx, command string) ([]types.NodeID, error) {
	keys, err := tx.Keys(RailClearProjection, command+"/")
	out := make([]types.NodeID, 0, len(keys))
	for _, k := range keys {
		out = append(out, types.NodeID(strings.TrimPrefix(k, command+"/")))
	}
	return out, err
}

// railCleared records p, a hold cleared by its signal, when the signal is
// a rail's and the event a command's.
func railCleared(tx *eventstore.Tx, ev eventstore.Event, p *types.HoldCleared) error {
	c, ok := p.Cause.Variant.(*types.Signalled)
	if !ok || ev.Command == nil || !strings.HasPrefix(c.SignalInstance, RailSignal("")) {
		return nil
	}
	return put(tx, RailClearProjection, *ev.Command+"/"+string(p.Node), c.SignalInstance)
}
