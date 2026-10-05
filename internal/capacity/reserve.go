// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Projections: each resource group's outstanding reservations (what
// granted leases reserved and have not yet returned) with its capacity
// revision, and the leases capacity granted that have not yet ended.
const (
	ReservedProjection = "capacity_reserved"
	GrantedProjection  = "capacity_granted"
)

// Exhausted refuses a grant one of whose groups cannot hold its reservation
// now: capacity_reserved_full when the known limit, less what is
// outstanding, is below it (the lease waits on capacity_released for a
// revision after Revision); quota_unknown when the group's limit is unknown
// (unknown capacity is never assumed).
type Exhausted struct {
	Lease    types.LeaseID
	Group    types.ResourceGroupID
	Revision int64 // the group's capacity revision the refusal read
	Reason   vocab.Reason
}

func (e *Exhausted) Error() string {
	return fmt.Sprintf("capacity: %s: lease %s does not fit group %s at capacity revision %d", e.Reason, e.Lease, e.Group, e.Revision)
}

// Reservations makes and returns a lease's reservations inside the writer
// transaction that records the lease's transition (docs/design/m0/05
// "Lease", 03 "The writer"): lease.granted reserves every group the lease
// names, all or none, refusing the command when any group lacks room;
// lease.settled and lease.released return them. The outstanding amounts are
// a fold over lease events, so a rebuild derives them from the log; the
// admission check reads the groups, which live outside the log, and runs
// only when an event is admitted, never on replay.
//
// Only what the log can account for is admitted. A group returns its
// reservations at settlement or release (on_settle). Broker balances wait
// for the ledger's affordability rule, window resets and evidence-target
// restores for their events, and an uncertain liability for its amount on
// the group's ledger; until then such a group or settlement is refused.
// Capacity must be wired from a store's first grant: a lease it granted
// holds no evidence target, which RestoreFree tells the lease projector.
type Reservations struct {
	groups map[types.ResourceGroupID]types.ResourceGroup
}

// NewReservations checks the groups and returns the hook over them.
func NewReservations(groups []types.ResourceGroup) (Reservations, error) {
	r := Reservations{groups: map[types.ResourceGroupID]types.ResourceGroup{}}
	for _, g := range groups {
		if err := CheckGroup(g); err != nil {
			return Reservations{}, err
		}
		switch {
		case g.Kind == types.ResourceGroupKindBrokerBalance:
			return Reservations{}, fmt.Errorf("capacity: broker balance %s is admitted by the ledger's affordability rule, which is not wired yet", g.ID)
		case g.ReleaseRule != types.ResourceGroupReleaseRuleOnSettle:
			// Evidence targets among them: on_restore_verified (13).
			return Reservations{}, fmt.Errorf("capacity: group %s returns %s, which is not wired yet", g.ID, g.ReleaseRule)
		case r.groups[g.ID].ID != "":
			return Reservations{}, fmt.Errorf("capacity: group %s twice", g.ID)
		}
		r.groups[g.ID] = g
	}
	return r, nil
}

// RestoreFree reports whether capacity granted the lease, which therefore
// holds no evidence target and owes no restore (lease.Groups). The record
// is a fold over the log, written at the grant and dropped when the lease
// ends.
func (r Reservations) RestoreFree(tx *eventstore.Tx, id types.LeaseID) (bool, error) {
	_, ok, err := tx.Get(GrantedProjection, string(id))
	return ok, err
}

// leaseEvent is the part of a lease event's payload the fold reads.
type leaseEvent struct {
	Transition string       `json:"transition"`
	Lease      *types.Lease `json:"lease"`
}

// Project is an event store Project hook; compose it after lease.Projector,
// which accepts the transition itself.
func (r Reservations) Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		var p leaseEvent
		switch ev.Kind {
		case lease.Granted + "/v1", lease.Settled + "/v1", lease.Released + "/v1":
		default:
			continue
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil || p.Lease == nil {
			return fmt.Errorf("capacity: event %s carries no lease (%v)", ev.ID, err)
		}
		var err error
		if p.Transition == lease.Granted {
			err = r.reserve(tx, *p.Lease)
		} else {
			err = r.release(tx, *p.Lease)
		}
		if err != nil {
			return fmt.Errorf("capacity: event %s: %w", ev.ID, err)
		}
	}
	return nil
}

// tally is a group's record: what is outstanding, and its capacity
// revision, which every grant and return advances.
type tally struct {
	Reserved int64 `json:"reserved"`
	Revision int64 `json:"revision"`
}

// reserve admits every reservation of l against its group, then records
// them all: a refusal rolls back the whole command.
func (r Reservations) reserve(tx *eventstore.Tx, l types.Lease) error {
	next := map[types.ResourceGroupID]tally{}
	for _, res := range l.Reservations {
		if _, dup := next[res.Group]; dup {
			return fmt.Errorf("lease %s reserves group %s twice", l.ID, res.Group)
		}
		cur, err := load(tx, res.Group)
		if err != nil {
			return err
		}
		if int64(res.Amount) > maxAmount-cur.Reserved {
			return fmt.Errorf("group %s's reservations overflow", res.Group)
		}
		if !tx.Replaying() {
			if err := r.admit(l.ID, res, cur); err != nil {
				return err
			}
		}
		next[res.Group] = tally{cur.Reserved + int64(res.Amount), cur.Revision + 1}
	}
	if err := tx.Put(GrantedProjection, string(l.ID), json.RawMessage(`{}`)); err != nil {
		return err
	}
	return putAll(tx, next)
}

// admit refuses a reservation its group cannot hold with cur outstanding.
func (r Reservations) admit(id types.LeaseID, res types.LeaseReservationsItem, cur tally) error {
	g, ok := r.groups[res.Group]
	switch {
	case !ok:
		return fmt.Errorf("lease %s reserves unknown group %s", id, res.Group)
	case res.Unit != g.CapacityModel.Unit:
		return fmt.Errorf("lease %s reserves %s of group %s, counted in %s", id, res.Unit, res.Group, g.CapacityModel.Unit)
	case g.CapacityModel.Limit == nil:
		return &Exhausted{Lease: id, Group: res.Group, Revision: cur.Revision, Reason: vocab.ReasonQuotaUnknown}
	}
	held := cur.Reserved
	if mutation.Is(mutation.ReservationsUncounted) {
		held = 0
	}
	if int64(res.Amount) > int64(*g.CapacityModel.Limit)-held {
		return &Exhausted{Lease: id, Group: res.Group, Revision: cur.Revision, Reason: vocab.ReasonCapacityReservedFull}
	}
	return nil
}

// release returns l's reservations at settlement or release. A settlement
// with an uncertain liability is refused: the liability must stay on the
// group's ledger at its amount, which the log does not carry yet.
func (r Reservations) release(tx *eventstore.Tx, l types.Lease) error {
	if len(l.Reservations) > 0 && l.Settlement != nil && len(l.Settlement.Liabilities) > 0 {
		return fmt.Errorf("lease %s settles with uncertain liabilities, which the group ledger cannot hold yet", l.ID)
	}
	next := map[types.ResourceGroupID]tally{}
	for _, res := range l.Reservations {
		cur, err := load(tx, res.Group)
		if err != nil {
			return err
		}
		if cur.Reserved < int64(res.Amount) {
			return fmt.Errorf("lease %s returns more of group %s than is outstanding", l.ID, res.Group)
		}
		next[res.Group] = tally{cur.Reserved - int64(res.Amount), cur.Revision + 1}
	}
	if err := tx.Delete(GrantedProjection, string(l.ID)); err != nil {
		return err
	}
	return putAll(tx, next)
}

const maxAmount = 9007199254740991

func load(tx *eventstore.Tx, group types.ResourceGroupID) (tally, error) {
	var t tally
	raw, ok, err := tx.Get(ReservedProjection, string(group))
	if err != nil || !ok {
		return t, err
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return t, fmt.Errorf("capacity: group %s's reservations: %w", group, err)
	}
	return t, nil
}

// Outstanding is what the group has reserved and not yet returned.
func Outstanding(tx *eventstore.Tx, group types.ResourceGroupID) (int64, error) {
	t, err := load(tx, group)
	return t.Reserved, err
}

func putAll(tx *eventstore.Tx, next map[types.ResourceGroupID]tally) error {
	for _, g := range slices.Sorted(maps.Keys(next)) {
		raw, err := json.Marshal(next[g])
		if err != nil {
			return err
		}
		if err := tx.Put(ReservedProjection, string(g), raw); err != nil {
			return err
		}
	}
	return nil
}
