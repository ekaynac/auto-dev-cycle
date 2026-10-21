// SPDX-License-Identifier: Apache-2.0

package adapterhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// secondLease requests and grants another lease on the world's unit,
// returning its id.
func (w *world) secondLease(id types.LeaseID) {
	w.t.Helper()
	reg, err := vocab.Load()
	if err != nil {
		w.t.Fatal(err)
	}
	ok := types.PlacementCheck{Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean}
	l := types.Lease{ID: id, Unit: untID, Role: "builder", Executor: "exe_01m3250vnfeyrv67x6w0ddqr6a", State: types.LeaseStateRequested,
		Reservations: []types.LeaseReservationsItem{{Group: "rg_01m3250vkwffc979nzjw0g2sf9", Amount: 1000, Unit: types.CapacityUnitTokens}},
		Footprint:    types.LeaseFootprint{ContextTokens: 1000, ExpectedTurns: 5}, Deadline: "2026-10-04T14:00:00Z",
		PlacementChecks: types.LeasePlacementChecks{DataFloor: ok, Destinations: ok, Capability: ok, Independence: ok,
			PaidAuthority: ok, Budget: ok, FallbackScope: ok}}
	w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, l) })
	w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, id, 1, lease.LeaseEvent{Kind: lease.Granted}, reg.Holds)
	})
}

// Opus #326 M3 and L2, L3: the host runs only an attempt its caller started
// on the spec's lease, from the spec's base, still running; an attempt
// started on another lease, lost, stopping or from another base is refused
// (ErrNotStarted), nothing launched; a Prepared is single-use (ErrUsed),
// Discard included.
func TestOnlyAStartedAttemptRuns(t *testing.T) {
	other := base
	other.ContractRevision = "9"
	for name, tc := range map[string]struct {
		after func(w *world)
		edit  func(sp *Spec)
	}{
		"on another lease": {edit: func(sp *Spec) { sp.Lease = "lea_01m3250v57fhm9v1z9xd1wk8gx" }},
		"lost": {after: func(w *world) {
			w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.LoseRunning(tx, attID, types.AttemptLostReasonValueControllerRestart)
			})
		}},
		"stopping": {after: func(w *world) {
			w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.StopRunning(tx, attID, types.AttemptStopReasonValuePreempt)
			})
		}},
		"from another base": {edit: func(sp *Spec) { sp.Base = other }},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			marker := filepath.Join(t.TempDir(), "launched")
			sp := spec(stub(t, stubPlan{PrepareTo: marker, Result: clean}))
			w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.StartAttempt(tx, leaID, 2, attID, base)
			})
			if tc.after != nil {
				tc.after(w)
			}
			if tc.edit != nil {
				tc.edit(&sp)
			}
			p, err := w.host.Prepare(sp)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Run(context.Background()); !errors.Is(err, ErrNotStarted) {
				t.Fatalf("run: %v", err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the adapter was launched")
			}
			if _, err := p.Run(context.Background()); !errors.Is(err, ErrUsed) {
				t.Errorf("a second run: %v", err)
			}
		})
	}
	w := newWorld(t)
	p, err := w.host.Prepare(spec(session.Config{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(w.host.Root); err != nil || len(entries) != 0 {
		t.Errorf("Discard left the exchange: %v %v", entries, err)
	}
	if _, err := p.Run(context.Background()); !errors.Is(err, ErrUsed) {
		t.Errorf("a run after Discard: %v", err)
	}
}

// Opus #326 M3: a superseded unit's replacement runs under token 2, and
// the adapter's prepare receives fencing_token 2.
func TestTheReplacementRunsUnderItsToken(t *testing.T) {
	worldUnits = boundUnit{} // a seal binds its unit's project and task
	t.Cleanup(func() { worldUnits = anyUnit{} })
	w := newWorld(t)
	w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.StartAttempt(tx, leaID, 2, attID, base)
	})
	w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.LoseRunning(tx, attID, types.AttemptLostReasonValueControllerRestart)
	})
	w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.SealMinimal(tx, attID, 2, boundUnit{}, w.host.Blobs, nil, actID)
	})
	for _, e := range []lease.LeaseEvent{{Kind: lease.Revoked, Reason: "grant_revoked"}, // the old lease ends first (#226 entry 83)
		{Kind: lease.Settled, Settlement: &types.LeaseSettlementValue{Calls: []types.CallID{}, Liabilities: []types.CallID{}}}} {
		w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			_, rev, _, err := lease.LeaseOf(tx, leaID)
			if err != nil {
				return nil, err
			}
			return lease.ChangeLease(tx, leaID, rev, e, nil)
		})
	}
	second, replacement := types.LeaseID("lea_01m3250v57fhm9v1z9xd1wk8gy"), types.AttemptID("att_01m3250vnfeyrv67x6w0ddqr6b")
	w.secondLease(second)
	w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.SupersedeAttempt(tx, attID, 3, second, 2, replacement, base)
	})
	token := filepath.Join(t.TempDir(), "token")
	sp := spec(stub(t, stubPlan{PrepareTo: token, Result: clean}))
	sp.Lease, sp.Attempt = second, replacement
	p, err := w.host.Prepare(sp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if b, err := os.ReadFile(token); err != nil || string(b) != "2" {
		t.Errorf("the prepare's fencing token: %q, %v", b, err)
	}
}

// boundUnit binds every unit to one project and task, as a seal needs.
type boundUnit struct{ anyUnit }

func (boundUnit) ProjectTask(*eventstore.Tx, types.UnitID) (types.ProjectID, types.TaskID, bool, error) {
	return "prj_01m3250vkwffc979nzjw0g2sa1", "tsk_01m3250v9xft08ssr4ygq6g2np", true, nil
}
