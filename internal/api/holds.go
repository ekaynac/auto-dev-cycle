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
//     longer lists is the operator's.
//   - A lease's unit is a node (M1 P8a, graph.Units), so its hold's project
//     is its node's, re-read by the lease's id, and a project-scoped reader
//     sees it. A lease whose unit no recorded node binds has no project: its
//     hold is installation-wide, read only under an every-project read.

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
	List: func(tx *eventstore.Tx) ([]any, error) {
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
			l, err := leaseRecord(tx, string(h.Subject))
			if err != nil {
				return nil, err
			}
			return unitProject(tx, l.Unit)
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
		l, err := leaseRecord(tx, k)
		if err != nil {
			return nil, err
		}
		if l.Hold == nil {
			continue
		}
		project, err := unitProject(tx, l.Unit)
		if err != nil {
			return nil, err
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
		out = append(out, types.HoldRead{Held: types.HoldReadHeldLease, Subject: types.TypeID(l.ID), Project: project, Hold: l.Hold.Hold,
			SignalInstance: l.Hold.SignalInstance, Owner: owner, Pages: []types.HoldReadPagesItem{}})
	}
	return out, nil
}

// leaseRecord is lease id's projected record.
func leaseRecord(tx *eventstore.Tx, id string) (types.Lease, error) {
	var stored struct {
		Record types.Lease `json:"record"`
	}
	raw, ok, err := tx.Get(lease.LeaseProjection, id)
	switch {
	case err != nil:
		return types.Lease{}, err
	case !ok:
		return types.Lease{}, fmt.Errorf("api: no lease %s", id)
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return types.Lease{}, fmt.Errorf("api: lease record %s is damaged: %w", id, err)
	}
	return stored.Record, nil
}

// unitProject is the project of unit's node, nil for a unit no recorded
// node binds.
func unitProject(tx *eventstore.Tx, unit types.UnitID) (*types.ProjectID, error) {
	project, _, ok, err := graph.Units{}.ProjectTask(tx, unit)
	if err != nil || !ok {
		return nil, err
	}
	return &project, nil
}
