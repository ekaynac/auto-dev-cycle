// SPDX-License-Identifier: Apache-2.0

package api

// The holds read (14 9e; 15 D2; M1 P4a): node holds (records/node-hold)
// and lease holds (lease.waiting and lease.restore_failed, 05), reconciled
// into one shape.
//
// Decisions:
//   - A node hold is sent as recorded: its id, owner, since and paging
//     history. Its project is its node's, re-read by the node's id.
//   - A lease records only the hold and its signal instance (05), so a
//     lease hold has no id, since or concrete owner yet (null), and no
//     pages. Its owner is the hold's registry owner; a hold the registry no
//     longer lists is the operator's. A lease records no project, so its
//     hold is installation-wide: read only under an every-project read.
//     TODO(P8a): once attempts are bound to nodes, a lease hold's project
//     is derivable from Lease.Unit (its node), so a project-scoped reader
//     can see it.

import (
	"encoding/json"
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// holds are every hold in force: node holds in node id order, then lease
// holds in lease id order.
var holds = Resource{
	Domain: "hold",
	Schema: "api/hold.json",
	List: func(tx *eventstore.Tx, _ ReadAt) ([]any, error) {
		nodes, err := graph.Held(tx)
		if err != nil {
			return nil, err
		}
		out := []any{}
		for _, n := range nodes {
			for _, h := range n.Holds {
				pages := make([]types.HoldReadPagesItem, 0, len(h.Pages))
				for _, p := range h.Pages {
					pages = append(pages, types.HoldReadPagesItem(p))
				}
				out = append(out, types.HoldRead{ID: &h.ID, Held: types.HoldReadHeldNode, Subject: types.TypeID(n.Node), Project: &n.Project, Hold: h.Hold,
					Reason: h.Reason, SignalInstance: h.SignalInstance, Owner: h.Owner, OwnerRef: h.OwnerRef, Since: &h.Since, Pages: pages})
			}
		}
		leases, err := leaseHolds(tx)
		return append(out, leases...), err
	},
	Project: func(tx *eventstore.Tx, rec any) (*types.ProjectID, error) {
		h, ok := rec.(types.HoldRead)
		switch {
		case !ok:
			return nil, fmt.Errorf("api: a hold record of type %T", rec)
		case h.Held == types.HoldReadHeldLease:
			return nil, nil
		}
		project, err := graph.ProjectOf(tx, types.NodeID(h.Subject))
		return &project, err
	},
	Fields: []string{"id", "held", "subject", "project", "hold", "reason", "signal_instance", "owner", "owner_ref", "since", "pages"},
}

// leaseHolds are the holds of the leases waiting or restore_failed.
func leaseHolds(tx *eventstore.Tx) ([]any, error) {
	keys, err := tx.Keys(lease.LeaseProjection, "")
	if err != nil {
		return nil, err
	}
	var reg *vocab.Registry
	out := []any{}
	for _, k := range keys {
		raw, _, err := tx.Get(lease.LeaseProjection, k)
		if err != nil {
			return nil, err
		}
		var stored struct {
			Record types.Lease `json:"record"`
		}
		if err := json.Unmarshal(raw, &stored); err != nil {
			return nil, fmt.Errorf("api: lease record %s is damaged: %w", k, err)
		}
		l := stored.Record
		if l.Hold == nil {
			continue
		}
		if reg == nil {
			if reg, err = vocab.Load(); err != nil {
				return nil, err
			}
		}
		owner := types.OwnerOperator
		if entry, ok := reg.Holds[l.Hold.Hold]; ok {
			owner = entry.Owner
		}
		out = append(out, types.HoldRead{Held: types.HoldReadHeldLease, Subject: types.TypeID(l.ID), Hold: l.Hold.Hold, SignalInstance: l.Hold.SignalInstance,
			Owner: owner, Pages: []types.HoldReadPagesItem{}})
	}
	return out, nil
}
