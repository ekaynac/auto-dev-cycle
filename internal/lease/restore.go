// SPDX-License-Identifier: Apache-2.0

package lease

// Evidence-target leases (05 "Evidence-target leases"; 13 [G5]; plan 15
// G8). A grant records the lease's evidence targets in the log (the
// lease.granted payload's targets), so a rebuild never reads configuration:
// a lease with one settles only through a verified restore.

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// TargetProjection holds each lease's evidence targets.
const TargetProjection = "lease_targets"

// granted records l's evidence targets: reserved groups, each once.
func granted(tx *eventstore.Tx, l types.Lease, targets []types.ResourceGroupID) error {
	for i, g := range targets {
		if slices.Contains(targets[:i], g) || !slices.ContainsFunc(l.Reservations, func(r types.LeaseReservationsItem) bool { return r.Group == g }) {
			return fmt.Errorf("lease %s's target %s is not one of its reserved groups", l.ID, g)
		}
	}
	b, err := json.Marshal(targets)
	if err != nil {
		return err
	}
	return tx.Put(TargetProjection, string(l.ID), b)
}

func targetsOf(tx *eventstore.Tx, id types.LeaseID) ([]types.ResourceGroupID, error) {
	var t []types.ResourceGroupID
	raw, ok, err := tx.Get(TargetProjection, string(id))
	if err == nil && ok {
		err = json.Unmarshal(raw, &t)
	}
	return t, err
}
