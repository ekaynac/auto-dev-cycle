// SPDX-License-Identifier: Apache-2.0

package capacity_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	gpu   = types.ResourceGroupID("rg_01m3250vkwffc979nzjw0g2sf9")
	quota = types.ResourceGroupID("rg_01m3250vm7f6ktw39dszqnyddr")
	dark  = types.ResourceGroupID("rg_01m3250v9xft08ssr4ygq6g2np")
)

func grp(id types.ResourceGroupID, kind types.ResourceGroupKind, unit types.CapacityUnit, limit *int64) types.ResourceGroup {
	state := types.ResourceGroupCapacityModelQuotaStateKnown
	if limit == nil {
		state = types.ResourceGroupCapacityModelQuotaStateUnknown
	}
	return types.ResourceGroup{ID: id, Kind: kind, ReleaseRule: types.ResourceGroupReleaseRuleOnSettle, Members: []types.ExecutorID{},
		CapacityModel: types.ResourceGroupCapacityModel{Unit: unit, Limit: limit, QuotaState: state}}
}

func lim(v int64) *int64 { return &v }

type world struct {
	t     *testing.T
	s     *eventstore.Store
	holds map[string]types.Hold
}

func open(t *testing.T, groups ...types.ResourceGroup) *world {
	t.Helper()
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	r, err := capacity.NewReservations(groups)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "state")
	lock, err := eventstore.AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	lp := lease.Projector{Blobs: blobs, Holds: reg.Holds}
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: lease.Kinds(), Blobs: blobs,
		Hooks: eventstore.Hooks{Authorize: func(*eventstore.Tx, eventstore.Command) error { return nil },
			Project: func(tx *eventstore.Tx, evs []eventstore.Event) error {
				return errors.Join(lp.Project(tx, evs), r.Project(tx, evs))
			}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); lock.Release() })
	return &world{t: t, s: s, holds: reg.Holds}
}

func (w *world) do(fn func(tx *eventstore.Tx) ([]eventstore.Event, error)) error {
	cmd, err := ids.New("cmd")
	if err != nil {
		w.t.Fatal(err)
	}
	_, err = w.s.Submit(context.Background(), eventstore.Command{ID: cmd, Actor: "act_01m3250v0aer9ss2097svmefj8", IdempotencyKey: cmd, Input: []byte(`{}`)},
		func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := fn(tx)
			return evs, json.RawMessage(`{}`), err
		})
	return err
}

// requested records a requested lease reserving res.
func (w *world) requested(res ...types.LeaseReservationsItem) types.LeaseID {
	w.t.Helper()
	var v [3]string
	for i, p := range []string{"lea", "unt", "exe"} {
		var err error
		if v[i], err = ids.New(p); err != nil {
			w.t.Fatal(err)
		}
	}
	ok := types.PlacementCheck{Outcome: types.Outcome(vocab.Empty), Reason: vocab.ReasonCompletedClean}
	l := types.Lease{ID: types.LeaseID(v[0]), Unit: types.UnitID(v[1]), Role: "builder", Executor: types.ExecutorID(v[2]),
		State: types.LeaseStateRequested, Reservations: res, Footprint: types.LeaseFootprint{ContextTokens: 1, ExpectedTurns: 1},
		Deadline: "2026-10-04T14:00:00Z", PlacementChecks: types.LeasePlacementChecks{DataFloor: ok, Destinations: ok,
			Capability: ok, Independence: ok, PaidAuthority: ok, Budget: ok, FallbackScope: ok}}
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, l) }); err != nil {
		w.t.Fatal(err)
	}
	return l.ID
}

func (w *world) change(id types.LeaseID, rev int64, e lease.LeaseEvent) error {
	return w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.ChangeLease(tx, id, rev, e, w.holds) })
}

func (w *world) outstanding(g types.ResourceGroupID) int64 {
	w.t.Helper()
	var n int64
	if err := w.s.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		n, err = capacity.Outstanding(tx, g)
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func reserve(g types.ResourceGroupID, amount int64, unit types.CapacityUnit) types.LeaseReservationsItem {
	return types.LeaseReservationsItem{Group: g, Amount: types.Amount(amount), Unit: unit}
}

var granted = lease.LeaseEvent{Kind: lease.Granted}

func exhausted(t *testing.T, err error, want vocab.Reason) {
	t.Helper()
	var e *capacity.Exhausted
	if !errors.As(err, &e) || e.Reason != want {
		t.Fatalf("got %v, want a %s refusal", err, want)
	}
}

// A grant reserves every group or none, counting what is outstanding; a
// release returns it, so a refused lease then fits.
func TestGrantReservesAllOrNothing(t *testing.T) {
	w := open(t, grp(gpu, types.ResourceGroupKindGpu, types.CapacityUnitGpuSeconds, lim(10)),
		grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitRequests, lim(3)))
	a := w.requested(reserve(gpu, 6, types.CapacityUnitGpuSeconds), reserve(quota, 1, types.CapacityUnitRequests))
	if err := w.change(a, 1, granted); err != nil {
		t.Fatal(err)
	}
	// b fits the quota but not the GPU: neither is reserved.
	b := w.requested(reserve(quota, 2, types.CapacityUnitRequests), reserve(gpu, 5, types.CapacityUnitGpuSeconds))
	exhausted(t, w.change(b, 1, granted), vocab.ReasonQuotaExhausted)
	if g, q := w.outstanding(gpu), w.outstanding(quota); g != 6 || q != 1 {
		t.Fatalf("outstanding gpu %d, quota %d after a refused grant; want 6 and 1", g, q)
	}
	// a ends without an attempt: its reservations return at release.
	for i, e := range []lease.LeaseEvent{{Kind: lease.Expired}, {Kind: lease.Released}} {
		if err := w.change(a, int64(2+i), e); err != nil {
			t.Fatal(err)
		}
	}
	if g, q := w.outstanding(gpu), w.outstanding(quota); g != 0 || q != 0 {
		t.Fatalf("outstanding gpu %d, quota %d after the release; want 0", g, q)
	}
	if err := w.change(b, 1, granted); err != nil {
		t.Fatalf("the released room was not reusable: %v", err)
	}
	// The outstanding amounts are a fold over the log.
	diffs, err := w.s.RebuildProjections(context.Background())
	if err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %v", diffs, err)
	}
}

func TestGrantRefusals(t *testing.T) {
	w := open(t, grp(gpu, types.ResourceGroupKindGpu, types.CapacityUnitGpuSeconds, lim(10)),
		grp(dark, types.ResourceGroupKindAccountQuota, types.CapacityUnitRequests, nil))
	for name, c := range map[string]struct {
		res  []types.LeaseReservationsItem
		want string
	}{
		"an unknown limit": {[]types.LeaseReservationsItem{reserve(dark, 1, types.CapacityUnitRequests)}, "quota_unknown"},
		"an unknown group": {[]types.LeaseReservationsItem{reserve(quota, 1, types.CapacityUnitRequests)}, "unknown group"},
		"another unit":     {[]types.LeaseReservationsItem{reserve(gpu, 1, types.CapacityUnitTokens)}, "counted in"},
		"a group twice": {[]types.LeaseReservationsItem{reserve(gpu, 1, types.CapacityUnitGpuSeconds),
			reserve(gpu, 1, types.CapacityUnitGpuSeconds)}, "twice"},
		"over the limit": {[]types.LeaseReservationsItem{reserve(gpu, 11, types.CapacityUnitGpuSeconds)}, "quota_exhausted"},
	} {
		t.Run(name, func(t *testing.T) {
			err := w.change(w.requested(c.res...), 1, granted)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want a refusal naming %q", err, c.want)
			}
			if n := w.outstanding(gpu); n != 0 {
				t.Fatalf("a refused grant left %d outstanding", n)
			}
		})
	}
}

// Settlement returns the reservations, except while an uncertain
// liability would have to stay on the group's ledger.
func TestSettlementReturns(t *testing.T) {
	w := open(t, grp(gpu, types.ResourceGroupKindGpu, types.CapacityUnitGpuSeconds, lim(4)))
	id := w.requested(reserve(gpu, 4, types.CapacityUnitGpuSeconds))
	att := types.AttemptID("att_01m3250vkwffc979nzjw0g2sf9")
	steps := []func(*eventstore.Tx) ([]eventstore.Event, error){
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.ChangeLease(tx, id, 1, granted, w.holds)
		},
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.StartAttempt(tx, id, 2, att, types.AttemptBase{Tree: "git:cccccccccccccccccccccccccccccccccccccccc", ContractRevision: "7"})
		},
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.ChangeAttempt(tx, att, 1, lease.AttemptEvent{Kind: lease.StopRequested, StopReason: types.AttemptStopReasonValueRevoke})
		},
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.ChangeLease(tx, id, 3, lease.LeaseEvent{Kind: lease.Revoked, Reason: "test"}, w.holds)
		},
	}
	for _, s := range steps {
		if err := w.do(s); err != nil {
			t.Fatal(err)
		}
	}
	settle := func(liabilities []types.CallID) error {
		return w.change(id, 4, lease.LeaseEvent{Kind: lease.Settled, Settlement: &types.LeaseSettlementValue{Calls: []types.CallID{}, Liabilities: liabilities}})
	}
	if err := settle([]types.CallID{"cal_01m3250vkwffc979nzjw0g2sf9"}); err == nil || !strings.Contains(err.Error(), "uncertain liabilities") {
		t.Fatalf("a settlement with an uncertain liability: %v", err)
	}
	if n := w.outstanding(gpu); n != 4 {
		t.Fatalf("outstanding %d after the refused settlement, want 4", n)
	}
	if err := settle([]types.CallID{}); err != nil {
		t.Fatal(err)
	}
	if n := w.outstanding(gpu); n != 0 {
		t.Fatalf("outstanding %d after settlement, want 0", n)
	}
}

func TestNewReservationsRefusesUnwiredGroups(t *testing.T) {
	broker := grp(gpu, types.ResourceGroupKindBrokerBalance, types.CapacityUnitUsdMicros, lim(1))
	window := grp(gpu, types.ResourceGroupKindAccountQuota, types.CapacityUnitRequests, lim(1))
	window.ReleaseRule = types.ResourceGroupReleaseRuleOnWindowReset
	plain := grp(gpu, types.ResourceGroupKindGpu, types.CapacityUnitGpuSeconds, lim(1))
	for name, gs := range map[string][]types.ResourceGroup{"a broker balance": {broker}, "a window reset": {window}, "a group twice": {plain, plain},
		"a broken group": {grp(gpu, types.ResourceGroupKindBrokerBalance, types.CapacityUnitTokens, lim(1))}} {
		if _, err := capacity.NewReservations(gs); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
