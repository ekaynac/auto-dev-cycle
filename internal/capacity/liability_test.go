// SPDX-License-Identifier: Apache-2.0

package capacity_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const testActor = "act_01m3250v0aer9ss2097svmefj8"

// openLedger is a store composed as P22a will compose adcd's: leases with
// capacity, model-call bindings, then the usage ledger, event by event.
func openLedger(t *testing.T, groups ...types.ResourceGroup) *world {
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
	kinds := lease.Kinds()
	maps.Copy(kinds, modelcall.Kinds())
	maps.Copy(kinds, ledger.Kinds())
	ok := func(*eventstore.Tx, eventstore.Command) error { return nil }
	lp := lease.Projector{Blobs: blobs, Holds: reg.Holds, Units: anyUnit{}}
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: kinds, Blobs: blobs, Effects: modelcall.Effects(),
		Hooks: eventstore.Hooks{Identify: ok, Authorize: ok, Project: eventstore.ProjectEach(capacity.Hook(lp, r), modelcall.Project, ledger.Project)}})
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	done := func() { once.Do(func() { s.Close(); lock.Release() }) }
	t.Cleanup(done)
	return &world{t: t, s: s, holds: reg.Holds, close: done}
}

// running is a granted lease reserving res, running one attempt, with one
// call bound to it.
type running struct {
	lease   types.LeaseID
	attempt types.AttemptID
	call    types.CallID
}

func (w *world) running(res ...types.LeaseReservationsItem) running {
	w.t.Helper()
	r := running{lease: w.requested(res...), attempt: types.AttemptID(newID(w.t, "att")), call: types.CallID(newID(w.t, "cal"))}
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, r.lease, 1, granted, w.holds)
	})
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.StartAttempt(tx, r.lease, 2, r.attempt, types.AttemptBase{Tree: "git:cccccccccccccccccccccccccccccccccccccccc", ContractRevision: "7"})
	})
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return modelcall.Intend(tx, gateway.Binding{Lease: r.lease, Attempt: r.attempt, Call: r.call}, "stub")
	})
	return r
}

// stop ends the attempt and revokes the lease: it is settling.
func (w *world) stop(r running) {
	w.t.Helper()
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeAttempt(tx, r.attempt, 1, lease.AttemptEvent{Kind: lease.StopRequested, StopReason: types.AttemptStopReasonValueRevoke})
	})
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, r.lease, 3, lease.LeaseEvent{Kind: lease.Revoked, Reason: "test"}, w.holds)
	})
}

func (w *world) settleAs(r running, calls, liabilities []types.CallID) error {
	return w.change(r.lease, 4, lease.LeaseEvent{Kind: lease.Settled, Settlement: &types.LeaseSettlementValue{Calls: calls, Liabilities: liabilities}})
}

// settleDerived settles r as capacity.Settlement derives.
func (w *world) settleDerived(r running) types.LeaseSettlementValue {
	w.t.Helper()
	var s types.LeaseSettlementValue
	w.view(func(tx *eventstore.Tx) (err error) { s, err = capacity.Settlement(tx, r.lease); return err })
	if err := w.settleAs(r, s.Calls, s.Liabilities); err != nil {
		w.t.Fatal(err)
	}
	return s
}

func (w *world) measure(r running, kind types.UsageMeasurementKind, seq, in, out int64) error {
	return w.measureIn(r, "USD", kind, seq, in, out)
}

func (w *world) measureIn(r running, currency string, kind types.UsageMeasurementKind, seq, in, out int64) error {
	m := types.UsageMeasurement{ID: types.MeasurementID(newID(w.t, "msr")), Call: r.call, Attempt: r.attempt, Lease: r.lease, Kind: kind,
		Sequence: seq, Tokens: types.UsageMeasurementTokens{In: in, Out: out}, Turns: 1, Milliseconds: 5,
		Cost:          types.UsageMeasurementCost{AmountMicros: 0, Currency: currency, Basis: types.UsageMeasurementCostBasisCash, Estimate: true},
		SourceOfTruth: types.UsageMeasurementSourceOfTruthAdapterReport}
	return w.record(m)
}

func (w *world) record(m types.UsageMeasurement) error {
	if kind := m.Kind; kind == types.UsageMeasurementKindProviderReconciliation {
		m.SourceOfTruth = types.UsageMeasurementSourceOfTruthProviderUsage
	}
	return w.do(func(*eventstore.Tx) ([]eventstore.Event, error) {
		ev, err := ledger.Measure(m)
		return []eventstore.Event{ev}, err
	})
}

func (w *world) must(fn func(tx *eventstore.Tx) ([]eventstore.Event, error)) {
	w.t.Helper()
	if err := w.do(fn); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) view(fn func(tx *eventstore.Tx) error) {
	w.t.Helper()
	if err := w.s.View(context.Background(), fn); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) liable(g types.ResourceGroupID) int64 {
	w.t.Helper()
	var n int64
	w.view(func(tx *eventstore.Tx) (err error) { n, err = capacity.Liable(tx, g); return err })
	return n
}

// replays asserts that the projections are exactly what the log derives.
func (w *world) replays() {
	w.t.Helper()
	diffs, err := w.s.RebuildProjections(context.Background())
	if err != nil || len(diffs) > 0 {
		w.t.Fatalf("projections differ from replay: %v %+v", err, diffs)
	}
}

func newID(t *testing.T, prefix string) string {
	t.Helper()
	id, err := ids.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func refused(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want a refusal naming %q", err, want)
	}
}

// F (P12b): a settlement with an unmeasured call. The call is never settled
// and never dropped: its liability is the lease's positive reservation,
// moved from reservation to liability once, so the group's available
// capacity is unchanged by the settlement.
func TestUnmeasuredCallSettlesAsItsReservation(t *testing.T) {
	w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
	r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
	w.stop(r)
	refused(t, w.settleAs(r, []types.CallID{r.call}, []types.CallID{}), "no final usage")
	refused(t, w.settleAs(r, []types.CallID{}, []types.CallID{}), "bound calls")
	refused(t, w.settleAs(r, []types.CallID{}, []types.CallID{r.call, "cal_01m3250vkwffc979nzjw0g2sf9"}), "bound calls")
	if n, l := w.outstanding(quota), w.liable(quota); n != 400 || l != 0 {
		t.Fatalf("after refused settlements: outstanding %d, liable %d; want 400, 0", n, l)
	}
	if s := w.settleDerived(r); len(s.Calls) != 0 || len(s.Liabilities) != 1 || s.Liabilities[0] != r.call {
		t.Fatalf("derived settlement %+v, want the call as a liability", s)
	}
	if n, l := w.outstanding(quota), w.liable(quota); n != 0 || l != 400 {
		t.Fatalf("after settlement: outstanding %d, liable %d; want 0, 400", n, l)
	}
	exhausted(t, w.change(w.requested(reserve(quota, 601, types.CapacityUnitTokens)), 1, granted), vocab.ReasonCapacityReservedFull)
	if err := w.change(w.requested(reserve(quota, 600, types.CapacityUnitTokens)), 1, granted); err != nil {
		t.Fatalf("what the liability leaves was not granted: %v", err)
	}
	w.replays()
}

// Late usage on a call left as a liability, after its model_call effect
// left pending (#226 entry 59), is accepted, and recomputes the liability:
// deltas raise it past the reservation, never below it, and a final
// releases it.
func TestLateUsageRecomputesTheLiability(t *testing.T) {
	w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
	r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
	w.stop(r)
	w.settleDerived(r)
	if n, err := w.s.MarkPendingUncertain(context.Background(), testActor); err != nil || n != 1 {
		t.Fatalf("marked %d pending effects uncertain (%v), want the call's", n, err)
	}
	steps := []struct {
		kind         types.UsageMeasurementKind
		seq, in, out int64
		liable       int64
	}{
		{types.UsageMeasurementKindDelta, 0, 1, 0, 400},    // a tiny delta never lowers the reservation
		{types.UsageMeasurementKindDelta, 1, 200, 20, 400}, // 221 tokens: still the reservation
		{types.UsageMeasurementKindDelta, 2, 200, 20, 441}, // equal figures at another sequence both count
		{types.UsageMeasurementKindDelta, 2, 200, 20, 441}, // the same delta again: deduplicated
		{types.UsageMeasurementKindFinal, 0, 0, 0, 0},      // settled: the liability is released
		{types.UsageMeasurementKindDelta, 2, 900, 900, 0},  // and stays released
	}
	for i, s := range steps {
		if err := w.measure(r, s.kind, s.seq, s.in, s.out); err != nil {
			t.Fatalf("step %d: late usage refused: %v", i, err)
		}
		if got := w.liable(quota); got != s.liable {
			t.Fatalf("step %d: liable %d, want %d", i, got, s.liable)
		}
	}
	w.replays()
}

// The liability does not depend on whether a report came before or after
// the settlement.
func TestLiabilityIsOrderIndependent(t *testing.T) {
	w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
	r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
	if err := w.measure(r, types.UsageMeasurementKindDelta, 0, 100, 20); err != nil {
		t.Fatal(err)
	}
	w.stop(r)
	refused(t, w.settleAs(r, []types.CallID{r.call}, []types.CallID{}), "no final usage")
	w.settleDerived(r)
	if n, l := w.outstanding(quota), w.liable(quota); n != 0 || l != 400 {
		t.Fatalf("outstanding %d, liable %d; want 0, 400", n, l)
	}
	if err := w.measure(r, types.UsageMeasurementKindDelta, 1, 400, 0); err != nil {
		t.Fatal(err)
	}
	if l := w.liable(quota); l != 520 {
		t.Fatalf("liable %d, want the 520 measured", l)
	}
	w.replays()
}

// A call whose usage settled is settled, never kept as a liability.
func TestSettledCallOwesNothing(t *testing.T) {
	w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
	r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
	if err := w.measure(r, types.UsageMeasurementKindProviderReconciliation, 0, 10, 1); err != nil {
		t.Fatal(err)
	}
	w.stop(r)
	refused(t, w.settleAs(r, []types.CallID{}, []types.CallID{r.call}), "is settled")
	if s := w.settleDerived(r); len(s.Calls) != 1 || len(s.Liabilities) != 0 {
		t.Fatalf("derived settlement %+v, want the call settled", s)
	}
	if n, l := w.outstanding(quota), w.liable(quota); n != 0 || l != 0 {
		t.Fatalf("outstanding %d, liable %d; want 0, 0", n, l)
	}
	w.replays()
}

// Outside tokens, the reservation stays held while the call is uncertain.
func TestSlotLiabilityHoldsTheSlot(t *testing.T) {
	w := openLedger(t, grp(gpu, types.ResourceGroupKindConcurrency, types.CapacityUnitSlots, lim(2)))
	r := w.running(reserve(gpu, 1, types.CapacityUnitSlots))
	if err := w.measure(r, types.UsageMeasurementKindDelta, 0, 100, 20); err != nil {
		t.Fatal(err)
	}
	w.stop(r)
	w.settleDerived(r)
	if n, l := w.outstanding(gpu), w.liable(gpu); n != 0 || l != 1 {
		t.Fatalf("outstanding %d, liable %d; want 0, 1", n, l)
	}
	if err := w.measure(r, types.UsageMeasurementKindFinal, 0, 100, 20); err != nil {
		t.Fatal(err)
	}
	if l := w.liable(gpu); l != 0 {
		t.Fatalf("liable %d after the final, want 0", l)
	}
	w.replays()
}

// H1 (Opus pre-review of #253): no report wedges a settlement. Reports in
// two currencies, or deltas past the contract's integers, leave the call an
// uncertain liability, flagged; late reports are still accepted, and a
// provider reconciliation releases it.
func TestUnreconcilableReportsNeverWedge(t *testing.T) {
	for name, reports := range map[string]func(w *world, r running) error{
		"two currencies": func(w *world, r running) error {
			return errors.Join(w.measureIn(r, "USD", types.UsageMeasurementKindDelta, 0, 100, 20),
				w.measureIn(r, "EUR", types.UsageMeasurementKindDelta, 1, 100, 20))
		},
		"overflow": func(w *world, r running) error {
			return errors.Join(w.measure(r, types.UsageMeasurementKindDelta, 0, maxTokens, 0),
				w.measure(r, types.UsageMeasurementKindDelta, 1, maxTokens, 0))
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
			r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
			if err := reports(w, r); err != nil {
				t.Fatal(err)
			}
			w.stop(r)
			if s := w.settleDerived(r); len(s.Liabilities) != 1 {
				t.Fatalf("derived settlement %+v, want the call as a liability", s)
			}
			want := int64(400)
			if name == "overflow" {
				want = maxTokens // saturated, never wrapped
			}
			var inconsistent bool
			w.view(func(tx *eventstore.Tx) (err error) {
				_, inconsistent, err = capacity.LiabilitiesOf(tx, r.lease, []types.ResourceGroupID{quota})
				return err
			})
			if l := w.liable(quota); l != want || !inconsistent {
				t.Fatalf("liable %d, inconsistent %v; want %d, flagged", l, inconsistent, want)
			}
			if err := w.measureIn(r, "GBP", types.UsageMeasurementKindDelta, 2, maxTokens, 0); err != nil {
				t.Fatalf("a late report was refused: %v", err)
			}
			if err := w.measureIn(r, "JPY", types.UsageMeasurementKindProviderReconciliation, 0, 10, 1); err != nil {
				t.Fatal(err)
			}
			if l := w.liable(quota); l != 0 {
				t.Fatalf("liable %d after the provider reconciliation, want 0", l)
			}
			w.replays()
		})
	}
}

// L1: a group's liability saturates at the contract's integers, so the sum
// over leases never wraps and admission stays closed.
func TestLiableSaturates(t *testing.T) {
	w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
	var rs []running
	for range 3 { // all granted before any liability
		rs = append(rs, w.running(reserve(quota, 1, types.CapacityUnitTokens)))
	}
	for _, r := range rs {
		if err := w.measure(r, types.UsageMeasurementKindDelta, 0, maxTokens, maxTokens); err != nil {
			t.Fatal(err)
		}
		w.stop(r)
		w.settleDerived(r)
	}
	if l := w.liable(quota); l != maxTokens {
		t.Fatalf("liable %d, want it saturated at %d", l, int64(maxTokens))
	}
	exhausted(t, w.change(w.requested(reserve(quota, 1, types.CapacityUnitTokens)), 1, granted), vocab.ReasonCapacityReservedFull)
	w.replays()
}

const maxTokens = 9007199254740991

// A bound call's report names its binding's attempt and lease (#226 entry
// 61): a report charging the call to another lease is refused.
func TestReportMatchesTheBinding(t *testing.T) {
	w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
	a := w.running(reserve(quota, 100, types.CapacityUnitTokens))
	b := w.running(reserve(quota, 100, types.CapacityUnitTokens))
	for name, other := range map[string]running{
		"another lease":   {lease: b.lease, attempt: a.attempt, call: a.call},
		"another attempt": {lease: a.lease, attempt: b.attempt, call: a.call},
	} {
		t.Run(name, func(t *testing.T) {
			refused(t, w.measure(other, types.UsageMeasurementKindDelta, 0, 1, 1), "is bound to")
		})
	}
	if err := w.measure(a, types.UsageMeasurementKindDelta, 0, 1, 1); err != nil {
		t.Fatalf("the binding's own report was refused: %v", err)
	}
}

// Astra on #253, H1: a call settled on a small final becomes uncertain when
// later deltas overflow, and owes again. The liability is the same whether
// the lease settles after the final, after every report, or before any;
// a provider reconciliation then releases it.
func TestSettledCallOwesAgainWhateverTheOrder(t *testing.T) {
	type report struct {
		kind    types.UsageMeasurementKind
		seq, in int64
	}
	reports := []report{{types.UsageMeasurementKindFinal, 0, 1}, {types.UsageMeasurementKindDelta, 0, maxTokens}, {types.UsageMeasurementKindDelta, 1, 1}}
	for _, settleAfter := range []int{1, 3, 0} {
		t.Run(fmt.Sprintf("settled after %d reports", settleAfter), func(t *testing.T) {
			w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
			r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
			w.stop(r)
			for i, m := range reports {
				if i == settleAfter {
					w.settleDerived(r)
				}
				if err := w.measure(r, m.kind, m.seq, m.in, 0); err != nil {
					t.Fatalf("report %d refused: %v", i, err)
				}
				if l := w.liable(quota); i == 0 && l != 0 {
					t.Fatalf("liable %d after the small final, before the deltas; want 0", l)
				}
			}
			if settleAfter == len(reports) {
				w.settleDerived(r)
			}
			var inconsistent bool
			w.view(func(tx *eventstore.Tx) (err error) {
				_, inconsistent, err = capacity.LiabilitiesOf(tx, r.lease, []types.ResourceGroupID{quota})
				return err
			})
			if l := w.liable(quota); l != maxTokens || !inconsistent {
				t.Fatalf("liable %d, inconsistent %v; want %d, flagged", l, inconsistent, int64(maxTokens))
			}
			w.replays() // the overflowed liability is what replay derives
			if err := w.measure(r, types.UsageMeasurementKindProviderReconciliation, 0, 10, 1); err != nil {
				t.Fatal(err)
			}
			if l := w.liable(quota); l != 0 {
				t.Fatalf("liable %d after the provider reconciliation, want 0", l)
			}
			w.replays()
		})
	}
}

// Astra on #253, L1: two late deltas in one command both count, as replay
// counts them, only because every event is projected on its own
// (eventstore.ProjectEach) through capacity, the bindings and the ledger.
func TestLateDeltasInOneCommand(t *testing.T) {
	w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
	r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
	w.stop(r)
	w.settleDerived(r)
	w.must(func(*eventstore.Tx) ([]eventstore.Event, error) {
		var evs []eventstore.Event
		for seq := range int64(2) {
			ev, err := ledger.Measure(types.UsageMeasurement{ID: types.MeasurementID(newID(t, "msr")), Call: r.call, Attempt: r.attempt,
				Lease: r.lease, Kind: types.UsageMeasurementKindDelta, Sequence: seq, Tokens: types.UsageMeasurementTokens{In: 300},
				Cost:          types.UsageMeasurementCost{Currency: "USD", Basis: types.UsageMeasurementCostBasisCash, Estimate: true},
				SourceOfTruth: types.UsageMeasurementSourceOfTruthAdapterReport})
			if err != nil {
				return nil, err
			}
			evs = append(evs, ev)
		}
		return evs, nil
	})
	if l := w.liable(quota); l != 600 {
		t.Fatalf("liable %d, want both deltas' 600", l)
	}
	w.replays()
}
