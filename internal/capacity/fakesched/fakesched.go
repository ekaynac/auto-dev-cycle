// SPDX-License-Identifier: Apache-2.0

// Package fakesched is the deterministic fake scheduler of docs/design/m0/05
// ("What M0 delivers"): it grants leases over resource groups through the
// lease state machine, all reservations or none, in a fixed order, so tests
// can drive a lease population to its end and check the invariants. The
// real scheduler's policy is M2.
package fakesched

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

type group struct {
	g        types.ResourceGroup
	reserved int64
}

// Scheduler holds resource groups and leases in memory.
type Scheduler struct {
	groups map[types.ResourceGroupID]*group
	leases map[types.LeaseID]types.Lease
	holds  map[string]types.Hold
}

// New checks the groups (capacity.CheckGroup) and returns an empty
// scheduler over them.
func New(groups []types.ResourceGroup, holds map[string]types.Hold) (*Scheduler, error) {
	s := &Scheduler{groups: map[types.ResourceGroupID]*group{}, leases: map[types.LeaseID]types.Lease{}, holds: holds}
	for _, g := range groups {
		if err := capacity.CheckGroup(g); err != nil {
			return nil, err
		}
		if _, dup := s.groups[g.ID]; dup {
			return nil, fmt.Errorf("fakesched: group %s twice", g.ID)
		}
		s.groups[g.ID] = &group{g: g}
	}
	return s, nil
}

// Request adds a requested lease. Each reservation names a known group in
// that group's unit, once, for no more than the group's known limit: a lease
// that could never be granted is refused, so every lease can end.
func (s *Scheduler) Request(l types.Lease) error {
	if _, dup := s.leases[l.ID]; dup {
		return fmt.Errorf("fakesched: lease %s already requested", l.ID)
	}
	if l.State != types.LeaseStateRequested {
		return fmt.Errorf("fakesched: lease %s is %s, not requested", l.ID, l.State)
	}
	seen := map[types.ResourceGroupID]bool{}
	for _, r := range l.Reservations {
		g, ok := s.groups[r.Group]
		switch {
		case !ok:
			return fmt.Errorf("fakesched: lease %s reserves unknown group %s", l.ID, r.Group)
		case seen[r.Group]:
			return fmt.Errorf("fakesched: lease %s reserves group %s twice", l.ID, r.Group)
		case r.Unit != g.g.CapacityModel.Unit:
			return fmt.Errorf("fakesched: lease %s reserves %s of group %s, counted in %s", l.ID, r.Unit, r.Group, g.g.CapacityModel.Unit)
		case g.g.CapacityModel.Limit == nil || int64(r.Amount) > int64(*g.g.CapacityModel.Limit):
			// It could never be granted, so it could never end.
			return fmt.Errorf("fakesched: lease %s reserves more of group %s than it can ever hold", l.ID, r.Group)
		}
		seen[r.Group] = true
	}
	s.leases[l.ID] = l
	return nil
}

// fits reports whether every reservation fits its group now; a group whose
// limit is unknown fits nothing (unknown capacity is never assumed).
func (s *Scheduler) fits(l types.Lease) (bool, types.ResourceGroupID) {
	for _, r := range l.Reservations {
		g := s.groups[r.Group]
		if g.g.CapacityModel.Limit == nil || g.reserved+int64(r.Amount) > int64(*g.g.CapacityModel.Limit) {
			return false, r.Group
		}
	}
	return true, ""
}

// Step makes one deterministic pass over the requested and waiting leases,
// highest priority first, then by id: a lease whose reservations all fit is
// granted and reserves them all at once; one that does not fit waits on the
// blocking group's hold (or returns to requested from waiting when it fits).
// It returns the leases granted.
func (s *Scheduler) Step() ([]types.LeaseID, error) {
	ids := make([]types.LeaseID, 0, len(s.leases))
	for id, l := range s.leases {
		if l.State == types.LeaseStateRequested || l.State == types.LeaseStateWaiting {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(a, b types.LeaseID) int {
		if c := cmp.Compare(s.leases[b].Priority, s.leases[a].Priority); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	var granted []types.LeaseID
	for _, id := range ids {
		l := s.leases[id]
		ok, blocking := s.fits(l)
		var err error
		switch {
		case ok && l.State == types.LeaseStateWaiting:
			l, err = s.apply(l, lease.LeaseEvent{Kind: lease.Requested})
			if err != nil {
				return nil, err
			}
			fallthrough
		case ok:
			if l, err = s.apply(l, lease.LeaseEvent{Kind: lease.Granted}); err != nil {
				return nil, err
			}
			for _, r := range l.Reservations {
				s.groups[r.Group].reserved += int64(r.Amount)
			}
			granted = append(granted, id)
		case l.State == types.LeaseStateRequested:
			hold := "quota_reset"
			if s.groups[blocking].g.Kind == types.ResourceGroupKindBrokerBalance {
				hold = "broker_credit_exhausted"
			}
			if l, err = s.apply(l, lease.LeaseEvent{Kind: lease.Waiting, Hold: &types.LeaseHoldValue{Hold: hold, SignalInstance: string(blocking)}}); err != nil {
				return nil, err
			}
		}
		s.leases[id] = l
	}
	return granted, nil
}

func (s *Scheduler) apply(l types.Lease, e lease.LeaseEvent) (types.Lease, error) {
	return lease.ApplyLease(l, e, false, s.holds)
}

// Run drives a granted lease to its end through the state machine, which
// refuses any other: with an attempt (start, then the given end, then
// settle) or, with no attempt, to expiry and release. Its reservations
// return when it ends.
func (s *Scheduler) Run(id types.LeaseID, attempt *types.AttemptID, end string) error {
	l, ok := s.leases[id]
	if !ok {
		return fmt.Errorf("fakesched: no lease %s", id)
	}
	steps := []lease.LeaseEvent{{Kind: lease.Expired}, {Kind: lease.Released}}
	if attempt != nil { // the state machine refuses an end that ends no active lease
		steps = []lease.LeaseEvent{{Kind: lease.AttemptStarted, Attempt: attempt}, {Kind: end, Reason: "fake scheduler"},
			{Kind: lease.Settled, Settlement: &types.LeaseSettlementValue{Calls: []types.CallID{}, Liabilities: []types.CallID{}}}}
	}
	for _, e := range steps {
		var err error
		if l, err = s.apply(l, e); err != nil {
			return err
		}
	}
	for _, r := range l.Reservations {
		g := s.groups[r.Group]
		if g.reserved < int64(r.Amount) {
			return errors.New("fakesched: a release exceeds what was reserved")
		}
		g.reserved -= int64(r.Amount)
	}
	s.leases[id] = l
	return nil
}

// Lease returns a lease's current record.
func (s *Scheduler) Lease(id types.LeaseID) (types.Lease, bool) {
	l, ok := s.leases[id]
	return l, ok
}

// Reserved is what a group has reserved now.
func (s *Scheduler) Reserved(id types.ResourceGroupID) int64 { return s.groups[id].reserved }
