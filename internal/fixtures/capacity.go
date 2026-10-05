// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// concurrentLastUnitReservation: a group of two requests has one held by a
// granted lease; eight requested leases race through the writer to be
// granted the last one. Exactly one is granted and the rest are refused
// capacity_reserved_full, all or nothing. Once the winner ends without an attempt
// and is released, its unit returns: one loser is then granted and the next
// refused again (11 "concurrent last-unit reservation"; 03 "The writer";
// 05 "Lease").
func concurrentLastUnitReservation(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	reg, err := vocab.Load()
	if err != nil {
		return nil, nil, err
	}
	const group = types.ResourceGroupID("rg_01m3250vkwffc979nzjw0g2sf9")
	limit := int64(2)
	r, err := capacity.NewReservations([]types.ResourceGroup{{ID: group, Kind: types.ResourceGroupKindAccountQuota,
		ReleaseRule: types.ResourceGroupReleaseRuleOnSettle, Members: []types.ExecutorID{},
		CapacityModel: types.ResourceGroupCapacityModel{Unit: types.CapacityUnitRequests, Limit: &limit,
			QuotaState: types.ResourceGroupCapacityModelQuotaStateKnown}}})
	if err != nil {
		return nil, nil, err
	}
	s, done, err := storeWith(ctx, dir, lease.Kinds(), capacity.Hook(lease.Projector{Holds: reg.Holds}, r))
	if err != nil {
		return nil, nil, err
	}
	defer done()
	actor, err := ids.New("act")
	if err != nil {
		return nil, nil, err
	}
	do := func(fn func(*eventstore.Tx) ([]eventstore.Event, error)) error {
		cmd, err := ids.New("cmd")
		if err != nil {
			return err
		}
		_, err = s.Submit(ctx, eventstore.Command{ID: cmd, Actor: actor, IdempotencyKey: cmd, Input: []byte(`{}`)},
			func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				evs, err := fn(tx)
				return evs, json.RawMessage(`{}`), err
			})
		return err
	}
	change := func(id types.LeaseID, rev int64, e lease.LeaseEvent) error {
		return do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.ChangeLease(tx, id, rev, e, reg.Holds)
		})
	}
	grant := lease.LeaseEvent{Kind: lease.Granted}
	const n = 8
	leases := make([]types.LeaseID, n+1)
	for i := range leases {
		l, _, err := freshLease()
		if err != nil {
			return nil, nil, err
		}
		l.Reservations = []types.LeaseReservationsItem{{Group: group, Amount: 1, Unit: types.CapacityUnitRequests}}
		if err := do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, l) }); err != nil {
			return nil, nil, err
		}
		leases[i] = l.ID
	}
	if err := change(leases[0], 1, grant); err != nil { // the first unit
		return nil, nil, err
	}
	var o observe
	outstanding := func(want int64, when string) error {
		var got int64
		err := s.View(ctx, func(tx *eventstore.Tx) (err error) {
			got, err = capacity.Outstanding(tx, group)
			return err
		})
		o.check(err == nil && got == want, "%s: %d of the group's %d requests are reserved, want %d", when, got, limit, want)
		return err
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs[i] = change(leases[i+1], 1, grant) })
	}
	wg.Wait()
	var winners []int
	for i, err := range errs {
		st, err := capacityStep(err)
		if err != nil {
			return nil, nil, err
		}
		if st.Outcome == vocab.Empty {
			winners = append(winners, i+1)
		}
		o.steps = append(o.steps, st)
	}
	slices.SortFunc(o.steps, func(a, b Step) int { return strings.Compare(a.String(), b.String()) })
	if err := outstanding(limit, "after the race"); err != nil {
		return nil, nil, err
	}
	if len(winners) != 1 {
		return o.steps, append(o.violations, "the last unit was granted to "+strconv.Itoa(len(winners))+" leases, want 1"), nil
	}
	winner := winners[0]
	// The winner ends without an attempt; its unit returns at release.
	for i, e := range []lease.LeaseEvent{{Kind: lease.Expired}, {Kind: lease.Released}} {
		if err := change(leases[winner], int64(2+i), e); err != nil {
			return nil, nil, err
		}
	}
	if err := outstanding(limit-1, "after the release"); err != nil {
		return nil, nil, err
	}
	var losers []types.LeaseID
	for i, id := range leases[1:] {
		if i+1 != winner {
			losers = append(losers, id)
		}
	}
	for _, id := range losers[:2] {
		st, err := capacityStep(change(id, 1, grant))
		if err != nil {
			return nil, nil, err
		}
		o.steps = append(o.steps, st)
	}
	if err := outstanding(limit, "after the regrant"); err != nil {
		return nil, nil, err
	}
	return o.steps, o.violations, nil
}

// capacityStep maps a grant's outcome to its step, a capacity refusal in
// the transaction included.
func capacityStep(err error) (Step, error) {
	if e := new(capacity.Exhausted); errors.As(err, &e) {
		return Step{vocab.CouldNotRun, e.Reason.String()}, nil
	}
	return commandStep(err)
}
