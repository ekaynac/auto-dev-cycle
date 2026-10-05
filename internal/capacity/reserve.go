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

// ReservedProjection holds each resource group's outstanding reservations:
// what granted leases reserved and have not yet returned.
const ReservedProjection = "capacity_reserved"

// Exhausted refuses a grant one of whose groups cannot hold its reservation
// now: quota_exhausted when the known limit, less what is outstanding, is
// below it; quota_unknown when the group's limit is unknown (unknown
// capacity is never assumed). Both wait on quota_reset (02).
type Exhausted struct {
	Lease  types.LeaseID
	Group  types.ResourceGroupID
	Reason vocab.Reason
}

func (e *Exhausted) Error() string {
	return fmt.Sprintf("capacity: %s: lease %s does not fit group %s", e.Reason, e.Lease, e.Group)
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
			return Reservations{}, fmt.Errorf("capacity: group %s returns %s, which is not wired yet", g.ID, g.ReleaseRule)
		case r.groups[g.ID].ID != "":
			return Reservations{}, fmt.Errorf("capacity: group %s twice", g.ID)
		}
		r.groups[g.ID] = g
	}
	return r, nil
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

// reserve admits every reservation of l against its group, then records
// them all: a refusal rolls back the whole command.
func (r Reservations) reserve(tx *eventstore.Tx, l types.Lease) error {
	seen := map[types.ResourceGroupID]bool{}
	next := map[types.ResourceGroupID]int64{}
	for _, res := range l.Reservations {
		if seen[res.Group] {
			return fmt.Errorf("lease %s reserves group %s twice", l.ID, res.Group)
		}
		seen[res.Group] = true
		cur, err := Outstanding(tx, res.Group)
		if err != nil {
			return err
		}
		if int64(res.Amount) > maxAmount-cur {
			return fmt.Errorf("group %s's reservations overflow", res.Group)
		}
		if !tx.Replaying() {
			if err := r.admit(l.ID, res, cur); err != nil {
				return err
			}
		}
		next[res.Group] = cur + int64(res.Amount)
	}
	return putAll(tx, next)
}

// admit refuses a reservation its group cannot hold with cur outstanding.
func (r Reservations) admit(id types.LeaseID, res types.LeaseReservationsItem, cur int64) error {
	g, ok := r.groups[res.Group]
	switch {
	case !ok:
		return fmt.Errorf("lease %s reserves unknown group %s", id, res.Group)
	case res.Unit != g.CapacityModel.Unit:
		return fmt.Errorf("lease %s reserves %s of group %s, counted in %s", id, res.Unit, res.Group, g.CapacityModel.Unit)
	case g.CapacityModel.Limit == nil:
		return &Exhausted{Lease: id, Group: res.Group, Reason: vocab.ReasonQuotaUnknown}
	}
	if mutation.Is(mutation.ReservationsUncounted) {
		cur = 0
	}
	if int64(res.Amount) > int64(*g.CapacityModel.Limit)-cur {
		return &Exhausted{Lease: id, Group: res.Group, Reason: vocab.ReasonQuotaExhausted}
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
	next := map[types.ResourceGroupID]int64{}
	for _, res := range l.Reservations {
		cur, err := Outstanding(tx, res.Group)
		if err != nil {
			return err
		}
		if cur < int64(res.Amount) {
			return fmt.Errorf("lease %s returns more of group %s than is outstanding", l.ID, res.Group)
		}
		next[res.Group] = cur - int64(res.Amount)
	}
	return putAll(tx, next)
}

const maxAmount = 9007199254740991

// Outstanding is what the group has reserved and not yet returned.
func Outstanding(tx *eventstore.Tx, group types.ResourceGroupID) (int64, error) {
	raw, ok, err := tx.Get(ReservedProjection, string(group))
	if err != nil || !ok {
		return 0, err
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("capacity: group %s's reservations: %w", group, err)
	}
	return n, nil
}

func putAll(tx *eventstore.Tx, next map[types.ResourceGroupID]int64) error {
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
