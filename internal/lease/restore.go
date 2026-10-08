// SPDX-License-Identifier: Apache-2.0

package lease

// Evidence-target restores (05 "Evidence-target leases"; 13 [G5]; plan 15
// G8). A grant records the lease's evidence targets in the log (the
// lease.granted payload's targets), so a rebuild never reads configuration:
// a lease with one settles only through a verified restore. A restore's
// outcome is its own log fact (lease.restore_recorded), recorded while the
// lease restores, for its attempt, one of its targets and its current
// restore cycle. Each lease.restore_started opens a cycle; a
// lease.restore_failed, or a failed restore of a target, voids the
// successes recorded before it in the cycle. A lease.restore_verified
// names one of the cycle's standing successes, and every target the grant
// recorded must have one. The fold, which admission and replay share,
// refuses anything else. lease.restore_recorded is trusted controller
// input: in M0 only the fixtures record it; M1's restore runner must be
// its only writer (05).

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// RestoreRecorded is the event of one restore's outcome; its payload is a
// records/restore-evidence.
const (
	RestoreRecorded = "lease.restore_recorded/v1"
	RestoreSchema   = "records/restore-evidence"
)

// Projections: each lease's evidence targets, each restore by receipt, and
// each lease's restore cycle.
const (
	TargetProjection  = "lease_targets"
	RestoreProjection = "lease_restores"
	CycleProjection   = "lease_restore_cycles"
)

// cycle is a lease's current restore cycle and, per target, the receipt of
// its standing verified restore in that cycle.
type cycle struct {
	Number   int64                                     `json:"number"`
	Verified map[types.ResourceGroupID]types.ReceiptID `json:"verified"`
}

func cycleOf(tx *eventstore.Tx, id types.LeaseID) (cycle, error) {
	c := cycle{Verified: map[types.ResourceGroupID]types.ReceiptID{}}
	raw, ok, err := tx.Get(CycleProjection, string(id))
	if err == nil && ok {
		err = json.Unmarshal(raw, &c)
	}
	return c, err
}

func putCycle(tx *eventstore.Tx, id types.LeaseID, c cycle) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return tx.Put(CycleProjection, string(id), b)
}

// restoreCycle folds a target lease's restore transition into its cycle: a
// start opens the next one, a failure voids its standing successes.
func restoreCycle(tx *eventstore.Tx, transition string, id types.LeaseID) error {
	if transition != RestoreStarted && transition != RestoreFailed {
		return nil
	}
	c, err := cycleOf(tx, id)
	if err != nil {
		return err
	}
	if transition == RestoreStarted {
		c.Number++
	}
	c.Verified = map[types.ResourceGroupID]types.ReceiptID{}
	return putCycle(tx, id, c)
}

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

// restoreRecorded admits a restore's outcome: while its lease restores, for
// the lease's attempt and one of its targets, under a new receipt.
func restoreRecorded(tx *eventstore.Tx, ev eventstore.Event) error {
	var r types.RestoreEvidence
	if err := json.Unmarshal(ev.Payload, &r); err != nil {
		return err
	}
	l, _, ok, err := load[types.Lease](tx, LeaseProjection, string(r.Lease))
	targets, terr := targetsOf(tx, r.Lease)
	c, cerr := cycleOf(tx, r.Lease)
	if err = errors.Join(err, terr, cerr); err != nil || !ok || ev.Subject != string(r.Lease) {
		return errors.Join(err, fmt.Errorf("a restore is recorded under its lease %s", r.Lease))
	}
	switch {
	case l.State != types.LeaseStateRestoring && l.State != types.LeaseStateRestoreFailed:
		return fmt.Errorf("a restore is recorded while lease %s restores, not %s", r.Lease, l.State)
	case l.Attempt == nil || *l.Attempt != r.Attempt:
		return fmt.Errorf("the restore follows another attempt than lease %s's", r.Lease)
	case !slices.Contains(targets, r.Group):
		return fmt.Errorf("group %s is not an evidence target of lease %s", r.Group, r.Lease)
	case r.Cycle != c.Number:
		return fmt.Errorf("the restore is of cycle %d, lease %s is in cycle %d", r.Cycle, r.Lease, c.Number)
	}
	if _, used, err := tx.Get(RestoreProjection, string(r.Receipt)); err != nil || used {
		return errors.Join(err, fmt.Errorf("receipt %s is recorded", r.Receipt))
	}
	delete(c.Verified, r.Group) // a failure voids the target's earlier success
	if r.Verified {
		c.Verified[r.Group] = r.Receipt
	}
	return errors.Join(tx.Put(RestoreProjection, string(r.Receipt), ev.Payload), putCycle(tx, r.Lease, c))
}

// verified: lease.restore_verified names a standing verified restore of
// this lease in its current cycle, and every target the grant recorded has
// one; restoreRecorded bound each to the lease's attempt.
func verified(tx *eventstore.Tx, n types.Lease, targets []types.ResourceGroupID) error {
	if n.RestoreReceipt == nil {
		return fmt.Errorf("lease %s's restore_verified names no receipt", n.ID)
	}
	var r types.RestoreEvidence
	raw, ok, err := tx.Get(RestoreProjection, string(*n.RestoreReceipt))
	if err == nil && ok {
		err = json.Unmarshal(raw, &r)
	}
	c, cerr := cycleOf(tx, n.ID)
	// The cycle is the lease's own, so a standing receipt in it is this lease's.
	if err = errors.Join(err, cerr); err != nil || !ok || c.Verified[r.Group] != *n.RestoreReceipt {
		return errors.Join(err, fmt.Errorf("lease %s's restore receipt %s names no standing verified restore of this cycle", n.ID, *n.RestoreReceipt))
	}
	for _, t := range targets {
		if _, ok := c.Verified[t]; !ok {
			return fmt.Errorf("lease %s's target %s has no verified restore in this cycle", n.ID, t)
		}
	}
	return nil
}
