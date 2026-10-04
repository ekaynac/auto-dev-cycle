// SPDX-License-Identifier: Apache-2.0

package fakesched

import (
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	gTokens = types.ResourceGroupID("rg_01m3250vkwffc979nzjw0g2sf9")
	gBroker = types.ResourceGroupID("rg_01m3250vnfeyrv67x6w0ddqr6a")
	gSlots  = types.ResourceGroupID("rg_01m3250vm7f6ktw39dszqnyddr")
)

func n(v int64) *int64 { return &v }

func groups() []types.ResourceGroup {
	known := types.ResourceGroupCapacityModelQuotaStateKnown
	mk := func(id types.ResourceGroupID, kind types.ResourceGroupKind, unit types.CapacityUnit, limit int64) types.ResourceGroup {
		return types.ResourceGroup{ID: id, Kind: kind, ReleaseRule: types.ResourceGroupReleaseRuleOnSettle, Members: []types.ExecutorID{},
			CapacityModel: types.ResourceGroupCapacityModel{Unit: unit, Limit: n(limit), QuotaState: known}}
	}
	return []types.ResourceGroup{
		mk(gTokens, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, 1000),
		mk(gBroker, types.ResourceGroupKindBrokerBalance, types.CapacityUnitUsdMicros, 5_000_000),
		mk(gSlots, types.ResourceGroupKindConcurrency, types.CapacityUnitSlots, 3),
	}
}

func holds(t *testing.T) map[string]types.Hold {
	t.Helper()
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg.Holds
}

func newLease(i int, priority int64, res ...types.LeaseReservationsItem) types.Lease {
	ok := types.PlacementCheck{Outcome: types.Outcome(vocab.Empty), Reason: vocab.ReasonCompletedClean}
	return types.Lease{ID: types.LeaseID(fmt.Sprintf("lea_01m3250v57fhm9v1z9xd%06d", i)), Unit: "unt_01m3250vz2ezn81cgvbkcn17jd",
		Role: "builder", Executor: "exe_01m3250vnfeyrv67x6w0ddqr6a", State: types.LeaseStateRequested, Reservations: res,
		Priority: priority, Footprint: types.LeaseFootprint{}, Deadline: "2026-10-05T14:00:00Z",
		PlacementChecks: types.LeasePlacementChecks{DataFloor: ok, Destinations: ok, Capability: ok, Independence: ok,
			PaidAuthority: ok, Budget: ok, FallbackScope: ok}}
}

func res(g types.ResourceGroupID, unit types.CapacityUnit, amount int64) types.LeaseReservationsItem {
	return types.LeaseReservationsItem{Group: g, Unit: unit, Amount: types.Amount(amount)}
}

// population is a seeded random set of leases, each reserving one to three
// groups within their limits.
func population(r *rand.Rand, count int) []types.Lease {
	options := []func() types.LeaseReservationsItem{
		func() types.LeaseReservationsItem { return res(gTokens, types.CapacityUnitTokens, 1+r.Int63n(1000)) },
		func() types.LeaseReservationsItem {
			return res(gBroker, types.CapacityUnitUsdMicros, 1+r.Int63n(5_000_000))
		},
		func() types.LeaseReservationsItem { return res(gSlots, types.CapacityUnitSlots, 1+r.Int63n(3)) },
	}
	var out []types.Lease
	for i := 0; i < count; i++ {
		picks := r.Perm(3)[:1+r.Intn(3)]
		var rs []types.LeaseReservationsItem
		for _, p := range picks {
			rs = append(rs, options[p]())
		}
		out = append(out, newLease(i, r.Int63n(10), rs...))
	}
	return out
}

// checkReserved: each group holds exactly what its granted leases reserve
// (all or nothing per lease), never over its limit.
func checkReserved(t *testing.T, s *Scheduler, leases []types.Lease) {
	t.Helper()
	want := map[types.ResourceGroupID]int64{}
	for _, l := range leases {
		cur, _ := s.Lease(l.ID)
		if cur.State == types.LeaseStateGranted {
			for _, r := range cur.Reservations {
				want[r.Group] += int64(r.Amount)
			}
		}
	}
	for _, g := range groups() {
		if got := s.Reserved(g.ID); got != want[g.ID] || got > *g.CapacityModel.Limit {
			t.Fatalf("group %s reserves %d, granted leases %d, limit %d", g.ID, got, want[g.ID], *g.CapacityModel.Limit)
		}
	}
}

// drive grants and ends a population; it returns the grant order.
func drive(t *testing.T, seed int64) []types.LeaseID {
	t.Helper()
	r := rand.New(rand.NewSource(seed))
	s, err := New(groups(), holds(t))
	if err != nil {
		t.Fatal(err)
	}
	leases := population(r, 40)
	for _, l := range leases {
		if err := s.Request(l); err != nil {
			t.Fatal(err)
		}
	}
	var order []types.LeaseID
	ends := []string{lease.Completed, lease.Revoked, lease.Preempted, lease.Expired}
	for round := 0; ; round++ {
		if round > 1000 {
			t.Fatal("the population never drained")
		}
		granted, err := s.Step()
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, granted...)
		checkReserved(t, s, leases)
		open := 0
		for i, l := range leases {
			cur, _ := s.Lease(l.ID)
			switch cur.State {
			case types.LeaseStateGranted:
				if r.Intn(2) == 0 { // some run now, some next round
					open++
					continue
				}
				var att *types.AttemptID
				if i%3 != 0 {
					a := types.AttemptID(fmt.Sprintf("att_01m3250v57fhm9v1z9xd%06d", i))
					att = &a
				}
				if err := s.Run(l.ID, att, ends[r.Intn(len(ends))]); err != nil {
					t.Fatal(err)
				}
				checkReserved(t, s, leases)
				open++
			case types.LeaseStateRequested, types.LeaseStateWaiting:
				open++
			}
		}
		if open == 0 {
			break
		}
	}
	// 05: every lease reaches settled or released; all capacity returns.
	for _, l := range leases {
		cur, _ := s.Lease(l.ID)
		if cur.State != types.LeaseStateSettled && cur.State != types.LeaseStateReleased {
			t.Fatalf("lease %s ended %s", l.ID, cur.State)
		}
	}
	for _, g := range groups() {
		if s.Reserved(g.ID) != 0 {
			t.Fatalf("group %s still reserves %d", g.ID, s.Reserved(g.ID))
		}
	}
	return order
}

func TestEveryLeaseEndsAllOrNothing(t *testing.T) {
	for seed := int64(1); seed <= 20; seed++ {
		first := drive(t, seed)
		if second := drive(t, seed); !slices.Equal(first, second) {
			t.Fatalf("seed %d: the grant order is not deterministic", seed)
		}
	}
}

// A lease that does not fit waits on its blocking group's hold, keeps
// nothing reserved, and is granted, all at once, when capacity returns.
func TestWaitsThenGrantsAtOnce(t *testing.T) {
	s, err := New(groups(), holds(t))
	if err != nil {
		t.Fatal(err)
	}
	big := newLease(1, 5, res(gTokens, types.CapacityUnitTokens, 900))
	both := newLease(2, 1, res(gTokens, types.CapacityUnitTokens, 200), res(gBroker, types.CapacityUnitUsdMicros, 10))
	broke := newLease(3, 0, res(gBroker, types.CapacityUnitUsdMicros, 4_999_995))
	for _, l := range []types.Lease{big, both, broke} {
		if err := s.Request(l); err != nil {
			t.Fatal(err)
		}
	}
	granted, err := s.Step()
	if err != nil || !slices.Equal(granted, []types.LeaseID{big.ID, broke.ID}) {
		t.Fatalf("first step: %v, %v", granted, err)
	}
	w, _ := s.Lease(both.ID)
	if w.State != types.LeaseStateWaiting || w.Hold.Hold != "quota_reset" || w.Hold.SignalInstance != string(gTokens) {
		t.Fatalf("the blocked lease: %+v", w)
	}
	if s.Reserved(gBroker) != 4_999_995 { // nothing partial for the waiting lease
		t.Fatalf("broker reserved %d", s.Reserved(gBroker))
	}
	if granted, _ := s.Step(); len(granted) != 0 {
		t.Fatalf("granted while still blocked: %v", granted)
	}
	if err := s.Run(big.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	// Now tokens fit, but the broker does not: it waits on the broker's hold.
	if granted, _ := s.Step(); len(granted) != 0 {
		t.Fatalf("granted without broker credit: %v", granted)
	}
	if w, _ := s.Lease(both.ID); w.State != types.LeaseStateWaiting || w.Hold.Hold != "broker_credit_exhausted" || w.Hold.SignalInstance != string(gBroker) {
		t.Fatalf("the lease now blocked on the broker: %s %+v", w.State, w.Hold)
	}
	// A lease blocked only by broker credit waits on the broker's hold.
	poor := newLease(4, 0, res(gBroker, types.CapacityUnitUsdMicros, 4_999_990)) // fits beside both's 10
	if err := s.Request(poor); err != nil {
		t.Fatal(err)
	}
	if granted, _ := s.Step(); len(granted) != 0 {
		t.Fatalf("granted without broker credit: %v", granted)
	}
	if p, _ := s.Lease(poor.ID); p.State != types.LeaseStateWaiting || p.Hold.Hold != "broker_credit_exhausted" {
		t.Fatalf("the broker-blocked lease: %+v", p.Hold)
	}
	if err := s.Run(broke.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	if granted, _ := s.Step(); !slices.Equal(granted, []types.LeaseID{both.ID, poor.ID}) {
		t.Fatalf("after capacity returned: %v", granted)
	}
	if s.Reserved(gTokens) != 200 || s.Reserved(gBroker) != 5_000_000 {
		t.Fatalf("reserved %d tokens, %d micros", s.Reserved(gTokens), s.Reserved(gBroker))
	}
}

func TestRefusals(t *testing.T) {
	s, err := New(groups(), holds(t))
	if err != nil {
		t.Fatal(err)
	}
	unknownGroup := newLease(1, 0, res("rg_01m3250v57ft7aehkqwq0yrq3n", types.CapacityUnitTokens, 1))
	twice := newLease(2, 0, res(gTokens, types.CapacityUnitTokens, 1), res(gTokens, types.CapacityUnitTokens, 1))
	wrongUnit := newLease(3, 0, res(gTokens, types.CapacityUnitSlots, 1))
	tooBig := newLease(4, 0, res(gTokens, types.CapacityUnitTokens, 1001))
	negative := newLease(8, 0, res(gTokens, types.CapacityUnitTokens, -1))
	notRequested := newLease(5, 0)
	notRequested.State = types.LeaseStateGranted
	for name, l := range map[string]types.Lease{"an unknown group": unknownGroup, "a group twice": twice, "another unit": wrongUnit,
		"more than the group can hold": tooBig, "a lease not requested": notRequested, "a negative amount": negative} {
		if err := s.Request(l); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := newLease(6, 0, res(gTokens, types.CapacityUnitTokens, 1))
	if err := s.Request(ok); err != nil {
		t.Fatal(err)
	}
	if err := s.Request(ok); err == nil {
		t.Error("the same lease requested twice")
	}
	if err := s.Run(ok.ID, nil, ""); err == nil {
		t.Error("running a lease not granted")
	}
	s.Step()
	a := types.AttemptID("att_01m3250vnfeyrv67x6w0ddqr6a")
	if err := s.Run(ok.ID, &a, lease.Settled); err == nil {
		t.Error("an attempt ended by settling directly")
	}
	unknown := groups()
	unknown[0].CapacityModel.Limit, unknown[0].CapacityModel.QuotaState = nil, types.ResourceGroupCapacityModelQuotaStateUnknown
	if s, err := New(unknown, holds(t)); err != nil {
		t.Fatal(err)
	} else if err := s.Request(newLease(7, 0, res(gTokens, types.CapacityUnitTokens, 1))); err == nil {
		t.Error("a reservation on a group of unknown capacity")
	}
	target := groups()
	target[2].Kind = types.ResourceGroupKindEvidenceTarget // three slots: not a valid target
	if _, err := New(target, holds(t)); err == nil {
		t.Error("an invalid evidence-target group")
	}
	target[2].CapacityModel.Limit = n(1)
	if s, err := New(target, holds(t)); err != nil {
		t.Fatal(err)
	} else if err := s.Request(newLease(9, 0, res(gSlots, types.CapacityUnitSlots, 1))); err == nil {
		t.Error("a reservation on an evidence target, which settles only through a restore")
	}
	noHold := maps.Clone(holds(t))
	delete(noHold, "broker_credit_exhausted")
	if _, err := New(groups(), noHold); err == nil {
		t.Error("a hold the scheduler waits on is not registered")
	}
	if s.Reserved("rg_01m3250v57ft7aehkqwq0yrq3n") != 0 {
		t.Error("an unknown group reserves something")
	}
	if _, err := New(append(groups(), groups()[0]), holds(t)); err == nil {
		t.Error("a group twice")
	}
}
