// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Projections: each resource group's outstanding reservations (what
// granted leases reserved and have not yet returned) with its capacity
// revision, and every lease capacity granted (kept once the lease ends,
// so CheckHistory can tell a store whose grants it all projected).
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
// reservations at settlement or release (on_settle). An evidence target
// (on_restore_verified, 13) returns its slot at the settlement that only a
// verified restore reaches: the grant's payload records its targets, which
// admission checks against the groups and a rebuild reads from the log
// alone, and the lease projector holds such a lease to the restore table,
// so a failed restore keeps the target excluded, across restarts, until a
// restore is verified. A settlement moves what its uncertain calls owe from
// reservation to liability, which counts against the group's limit until
// their usage settles (liability.go). Broker balances wait for the ledger's
// affordability rule and window resets for their events; until then such a
// group is refused. Capacity must be wired from a store's first grant (CheckHistory),
// and its groups must still be the ones recorded grants name (CheckGroups).
// Compose it with Hook.
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
		case (g.Kind == types.ResourceGroupKindEvidenceTarget) != (g.ReleaseRule == types.ResourceGroupReleaseRuleOnRestoreVerified):
			return Reservations{}, fmt.Errorf("capacity: group %s returns %s: only an evidence target returns on a verified restore, and always", g.ID, g.ReleaseRule)
		case g.ReleaseRule != types.ResourceGroupReleaseRuleOnSettle && g.ReleaseRule != types.ResourceGroupReleaseRuleOnRestoreVerified:
			return Reservations{}, fmt.Errorf("capacity: group %s returns %s, which is not wired yet", g.ID, g.ReleaseRule)
		case r.groups[g.ID].ID != "":
			return Reservations{}, fmt.Errorf("capacity: group %s twice", g.ID)
		}
		r.groups[g.ID] = g
	}
	return r, nil
}

// granted is a lease capacity granted: the groups its grant recorded as
// evidence targets. A grant with none is `{}`, as before targets were
// wired, so an existing store's projection replays byte for byte.
type granted struct {
	Targets []types.ResourceGroupID `json:"targets,omitempty"`
}

// ErrGroupChanged is CheckGroups' refusal: a group a recorded grant
// reserved is gone, or changed kind.
type ErrGroupChanged struct {
	Group types.ResourceGroupID
	Lease string
	Gone  bool
}

func (e *ErrGroupChanged) Error() string {
	if e.Gone {
		return fmt.Sprintf("capacity: group %s, which lease %s reserved, is not configured under an existing store", e.Group, e.Lease)
	}
	return fmt.Sprintf("capacity: group %s changed kind under an existing store (lease %s)", e.Group, e.Lease)
}

// RestoreFree reports whether capacity granted the lease and it holds no
// evidence target, so it owes no restore (lease.Groups). The record is a
// fold over the log, written at the grant.
func (r Reservations) RestoreFree(tx *eventstore.Tx, id types.LeaseID) (bool, error) {
	g, ok, err := r.granted(tx, id)
	return ok && len(g.Targets) == 0, err
}

// CheckGroups refuses to run capacity over groups that no longer match a
// recorded grant: a group it names that is gone, or whose kind changed
// under it (an evidence target recorded as one, or the reverse). Call it,
// as CheckHistory, on the store's projections before admitting commands.
// The groups a lease reserved come from its own record.
func (r Reservations) CheckGroups(tx *eventstore.Tx) error {
	keys, err := tx.Keys(GrantedProjection, "")
	for _, id := range keys {
		g, _, gerr := r.granted(tx, types.LeaseID(id))
		var rec struct {
			Record types.Lease `json:"record"`
		}
		raw, _, lerr := tx.Get(lease.LeaseProjection, id)
		if err = errors.Join(gerr, lerr); err == nil {
			err = json.Unmarshal(raw, &rec)
		}
		if err != nil {
			return err
		}
		for _, res := range rec.Record.Reservations {
			cfg, ok := r.groups[res.Group]
			if !ok || slices.Contains(g.Targets, res.Group) != (cfg.Kind == types.ResourceGroupKindEvidenceTarget) {
				return &ErrGroupChanged{Group: res.Group, Lease: id, Gone: !ok}
			}
		}
	}
	return err
}

func (r Reservations) granted(tx *eventstore.Tx, id types.LeaseID) (granted, bool, error) {
	var g granted
	raw, ok, err := tx.Get(GrantedProjection, string(id))
	if err == nil && ok {
		err = json.Unmarshal(raw, &g)
	}
	return g, ok, err
}

// Hook composes the lease projector and r into one Project hook, event by
// event: the lease projector accepts each event, then r folds it, so a
// command's events project exactly as they would one at a time (a rebuild
// replays them singly). The lease projector asks r which leases owe no
// restore.
func Hook(p lease.Projector, r Reservations) func(*eventstore.Tx, []eventstore.Event) error {
	p.Groups = r
	return func(tx *eventstore.Tx, evs []eventstore.Event) error {
		for i := range evs {
			one := evs[i : i+1]
			if err := p.Project(tx, one); err != nil {
				return err
			}
			if err := r.Project(tx, one); err != nil {
				return err
			}
		}
		return nil
	}
}

// ErrGrantedBeforeWiring is CheckHistory's refusal of the history itself;
// any other error from it means the projections could not be read.
var ErrGrantedBeforeWiring = errors.New("capacity: a lease was granted before capacity was wired")

// CheckHistory refuses to run capacity over a store with a lease granted
// before capacity was wired (ErrGrantedBeforeWiring): the log cannot say
// that capacity admitted that grant, so a rebuild would read it as one
// capacity granted. Call it on the store's projections (a View) before
// admitting commands. A lease still requested or waiting was never granted.
func CheckHistory(tx *eventstore.Tx) error {
	keys, err := tx.Keys(lease.LeaseProjection, "")
	if err != nil {
		return err
	}
	for _, id := range keys {
		raw, _, err := tx.Get(lease.LeaseProjection, id)
		if err != nil {
			return err
		}
		var rec struct {
			Record struct {
				State types.LeaseState `json:"state"`
			} `json:"record"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("capacity: lease %s: %w", id, err)
		}
		if s := rec.Record.State; s == types.LeaseStateRequested || s == types.LeaseStateWaiting {
			continue
		}
		_, ok, err := tx.Get(GrantedProjection, id)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: lease %s; capacity runs only from a store's first grant", ErrGrantedBeforeWiring, id)
		}
	}
	return nil
}

// leaseEvent is the part of a lease event's payload the fold reads.
type leaseEvent struct {
	Transition string                  `json:"transition"`
	Lease      *types.Lease            `json:"lease"`
	Targets    []types.ResourceGroupID `json:"targets"`
}

// Project is an event store Project hook; compose it after lease.Projector,
// which accepts the transition itself.
func (r Reservations) Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		var p leaseEvent
		switch ev.Kind {
		case ledger.Measured:
			var m types.UsageMeasurement
			err := json.Unmarshal(ev.Payload, &m)
			if err == nil {
				err = lateUsage(tx, m)
			}
			if err != nil {
				return fmt.Errorf("capacity: event %s: %w", ev.ID, err)
			}
			continue
		case lease.Granted + "/v1", lease.Settled + "/v1", lease.Released + "/v1":
		case lease.RestoreFailed + "/v1":
			if !mutation.Is(mutation.RestoreFailureReleases) {
				continue
			}
		default:
			continue
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil || p.Lease == nil {
			return fmt.Errorf("capacity: event %s carries no lease (%v)", ev.ID, err)
		}
		var err error
		if p.Transition == lease.Granted {
			err = r.reserve(tx, *p.Lease, p.Targets)
		} else {
			err = r.release(tx, *p.Lease)
		}
		if err != nil {
			return fmt.Errorf("capacity: event %s: %w", ev.ID, err)
		}
	}
	return nil
}

// tally is a group's record: what is outstanding, its capacity revision,
// which every grant, return and change of a liability advances, and what
// settled leases' uncertain calls owe it (liability.go).
type tally struct {
	Reserved int64 `json:"reserved"`
	Revision int64 `json:"revision"`
	Liable   int64 `json:"liable,omitempty"` // omitted at zero, so a store from before replays byte for byte
}

// reserve admits every reservation of l against its group, then records
// them all: a refusal rolls back the whole command.
func (r Reservations) reserve(tx *eventstore.Tx, l types.Lease, targets []types.ResourceGroupID) error {
	next, rec := map[types.ResourceGroupID]tally{}, granted{Targets: targets}
	var want []types.ResourceGroupID // the targets the groups name
	for _, res := range l.Reservations {
		if r.groups[res.Group].Kind == types.ResourceGroupKindEvidenceTarget {
			want = append(want, res.Group)
		}
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
		next[res.Group] = tally{cur.Reserved + int64(res.Amount), cur.Revision + 1, cur.Liable}
	}
	if !tx.Replaying() && !slices.Equal(slices.Sorted(slices.Values(want)), slices.Sorted(slices.Values(targets))) {
		return fmt.Errorf("lease %s's grant names evidence targets %v, its groups %v", l.ID, targets, want)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := tx.Put(GrantedProjection, string(l.ID), raw); err != nil {
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
	held := cur.Reserved + cur.Liable
	if mutation.Is(mutation.ReservationsUncounted) {
		held = 0
	}
	if int64(res.Amount) > int64(*g.CapacityModel.Limit)-held {
		return &Exhausted{Lease: id, Group: res.Group, Revision: cur.Revision, Reason: vocab.ReasonCapacityReservedFull}
	}
	return nil
}

// release returns l's reservations at settlement or release. A settlement
// moves what its uncertain calls owe onto the groups (settle).
func (r Reservations) release(tx *eventstore.Tx, l types.Lease) error {
	if _, ok, err := tx.Get(GrantedProjection, string(l.ID)); err != nil || !ok {
		return errors.Join(err, fmt.Errorf("lease %s was not granted by capacity", l.ID))
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
		next[res.Group] = tally{cur.Reserved - int64(res.Amount), cur.Revision + 1, cur.Liable}
	}
	if l.Settlement != nil {
		return settle(tx, l, next)
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
