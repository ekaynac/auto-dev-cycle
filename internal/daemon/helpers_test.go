// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const quota = types.ResourceGroupID("rg_01m3250vm7f6ktw39dszqnyddr")

func tokens(limit int64) types.ResourceGroup {
	return types.ResourceGroup{ID: quota, Kind: types.ResourceGroupKindAccountQuota, ReleaseRule: types.ResourceGroupReleaseRuleOnSettle,
		Members: []types.ExecutorID{}, CapacityModel: types.ResourceGroupCapacityModel{Unit: types.CapacityUnitTokens, Limit: &limit,
			QuotaState: types.ResourceGroupCapacityModelQuotaStateKnown}}
}

func newID(t *testing.T, prefix string) string {
	t.Helper()
	v, err := ids.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func stateDir(t *testing.T) string { return testguard.StateDir(t) }

// seeder is a store under the state directory, opened outside the daemon
// with a permissive authorizer, to write a history the daemon then starts
// on. project picks the Project hook from the store's blobs and holds.
type seeder struct {
	t     *testing.T
	s     *eventstore.Store
	holds map[string]types.Hold
	done  func()
	// sys and op are the system actor and the operator a test seeded, if
	// any (readyTaskIn, plannedIn).
	sys, op types.ActorID
	dir     string // the state directory
}

type projectFor func(blobs lease.Blobs, holds map[string]types.Hold) func(*eventstore.Tx, []eventstore.Event) error

// daemonProjection is the daemon's own composition over groups.
func daemonProjection(t *testing.T, groups ...types.ResourceGroup) projectFor {
	return func(blobs lease.Blobs, holds map[string]types.Hold) func(*eventstore.Tx, []eventstore.Event) error {
		r, err := capacity.NewReservations(groups)
		if err != nil {
			t.Fatal(err)
		}
		return projector(blobs, holds, r)
	}
}

// leasesOnly projects leases without capacity: a history capacity cannot
// account for.
func leasesOnly(blobs lease.Blobs, holds map[string]types.Hold) func(*eventstore.Tx, []eventstore.Event) error {
	return lease.Projector{Blobs: blobs, Holds: holds}.Project
}

// seed opens the seeder; extra registers more identity-schema kinds, which
// the daemon does not read.
func seed(t *testing.T, dir string, project projectFor, extra ...string) *seeder {
	t.Helper()
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	kinds, err := eventKinds()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range extra {
		kinds[k] = eventstore.Kind{Schema: identity.EventSchema}
	}
	lock, err := eventstore.AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: kinds, Blobs: blobs, Effects: modelcall.Effects(), // a history with model calls (P22a-1)
		Hooks: eventstore.Hooks{Identify: func(*eventstore.Tx, eventstore.Command) error { return nil }, Authorize: func(*eventstore.Tx, eventstore.Command) error { return nil }, Project: project(blobs, reg.Holds)}})
	if err != nil {
		t.Fatal(err)
	}
	return &seeder{t: t, s: s, holds: reg.Holds, dir: dir, done: func() {
		if err := errors.Join(s.Close(), lock.Release()); err != nil {
			t.Fatal(err)
		}
	}}
}

// do submits one command as actor, appending what fn returns.
func (w *seeder) do(actor types.ActorID, fn func(tx *eventstore.Tx) ([]eventstore.Event, error)) {
	w.t.Helper()
	if err := w.try(actor, fn); err != nil {
		w.t.Fatal(err)
	}
}

// try is do, returning the command's refusal.
func (w *seeder) try(actor types.ActorID, fn func(tx *eventstore.Tx) ([]eventstore.Event, error)) error {
	cmd := newID(w.t, "cmd")
	_, err := w.s.SubmitApplied(context.Background(), eventstore.Command{ID: cmd, Actor: string(actor), Action: "test.command", IdempotencyKey: cmd, Input: []byte(`{}`)}, eventstore.Applies(
		func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := fn(tx)
			return evs, json.RawMessage(`{}`), err
		}))
	return err
}

func (w *seeder) view(fn func(tx *eventstore.Tx) error) {
	w.t.Helper()
	if err := w.s.View(context.Background(), fn); err != nil {
		w.t.Fatal(err)
	}
}

func (w *seeder) head() int64 {
	w.t.Helper()
	h, err := w.s.Head(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	return h.Seq
}

// grantedLease requests a lease reserving amount tokens of the quota group
// and grants it.
func (w *seeder) grantedLease(actor types.ActorID, amount int64) types.LeaseID {
	w.t.Helper()
	l := w.lease(amount)
	w.do(actor, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, l) })
	w.do(actor, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, l.ID, 1, lease.LeaseEvent{Kind: lease.Granted}, w.holds)
	})
	return l.ID
}

// lease is a requested lease reserving amount tokens of the quota group.
func (w *seeder) lease(amount int64) types.Lease {
	ok := types.PlacementCheck{Outcome: types.Outcome(vocab.Empty), Reason: vocab.ReasonCompletedClean}
	l := types.Lease{ID: types.LeaseID(newID(w.t, "lea")), Unit: types.UnitID(newID(w.t, "unt")), Role: "builder",
		Executor: types.ExecutorID(newID(w.t, "exe")), State: types.LeaseStateRequested,
		Reservations: []types.LeaseReservationsItem{{Group: quota, Amount: types.Amount(amount), Unit: types.CapacityUnitTokens}},
		Footprint:    types.LeaseFootprint{ContextTokens: 1, ExpectedTurns: 1}, Deadline: "2026-10-04T14:00:00Z",
		PlacementChecks: types.LeasePlacementChecks{DataFloor: ok, Destinations: ok, Capability: ok, Independence: ok,
			PaidAuthority: ok, Budget: ok, FallbackScope: ok}}
	return l
}

// startErr asserts that err is a StartError with outcome and reason.
func startErr(t *testing.T, err error, outcome vocab.Outcome, reason vocab.Reason) {
	t.Helper()
	var se *StartError
	if !errors.As(err, &se) || se.Outcome != outcome || se.Reason != reason {
		t.Fatalf("want %s/%s, got %v", outcome, reason, err)
	}
}

// fullHistory writes, through w, a record for every projector the daemon
// composes: actors, a supervisor and its grant in one command, a granted
// lease, a lease waiting on a hold, and a usage report. It returns the
// operator.
func fullHistory(t *testing.T, w *seeder) types.ActorID {
	t.Helper()
	sys := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
	sys.CreatedBy = sys.ID
	op := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindOperator, Name: "operator", CreatedBy: sys.ID}
	sup := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSupervisor, Name: "supervisor", CreatedBy: op.ID}
	w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(sys) })
	w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(op) })
	w.sys, w.op = sys.ID, op.ID
	now := time.Now().UTC()
	g := types.Grant{ID: types.GrantID(newID(t, "grt")), Actor: sup.ID, Projects: types.ProjectScope{List: []types.ProjectID{"prj_01m3250vkwffc979nzjw0g2sa1"}},
		Actions: []types.ActionKind{}, Reads: types.GrantReads{Projects: types.GrantReadsProjects{Same: true}, Kinds: []types.Name{}},
		ValidFrom: types.Time(now.Add(-time.Hour).Format(time.RFC3339)), ExpiresAt: types.Time(now.Add(time.Hour).Format(time.RFC3339)),
		ConfirmationOverrides: map[string]types.GrantConfirmationOverridesValue{}, IssuedBy: op.ID}
	// The grantee and its grant in one command: admitted only if each event
	// is projected through every projector before the next one.
	w.do(op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		a, err := identity.CreateActor(sup)
		if err != nil {
			return nil, err
		}
		gr, err := authority.CreateGrant(tx, g)
		return append(a, gr...), err
	})
	w.grantedLease(op.ID, 4)
	w.heldLease(op.ID)
	m := types.UsageMeasurement{ID: types.MeasurementID(newID(t, "msr")), Call: types.CallID(newID(t, "cal")), Attempt: types.AttemptID(newID(t, "att")),
		Lease: types.LeaseID(newID(t, "lea")), Kind: types.UsageMeasurementKindDelta, Sequence: 1,
		Tokens: types.UsageMeasurementTokens{In: 10, Out: 1}, Turns: 1, Milliseconds: 20,
		Cost:          types.UsageMeasurementCost{AmountMicros: 30, Currency: "USD", Basis: types.UsageMeasurementCostBasisCash, Estimate: true},
		SourceOfTruth: types.UsageMeasurementSourceOfTruthAdapterReport}
	w.do(op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
		e, err := ledger.Measure(m)
		return []eventstore.Event{e}, err
	})
	return op.ID
}

// heldLease requests a lease and parks it waiting on the quota_reset hold.
func (w *seeder) heldLease(actor types.ActorID, unit ...types.UnitID) {
	w.t.Helper()
	l := w.lease(1)
	if len(unit) > 0 {
		l.Unit = unit[0]
	}
	w.do(actor, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, l) })
	w.do(actor, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, l.ID, 1, lease.LeaseEvent{Kind: lease.Waiting,
			Hold: &types.LeaseHoldValue{Hold: "quota_reset", SignalInstance: "2026-10-04T15:00:00Z"}}, w.holds)
	})
}

// listen is serve with every command refused as not wired (tests).
func listen(store *eventstore.Store, boot *bootstrapper, stateDir string, shared *SharedSocket, lim limits) (*server, error) {
	reg, err := vocab.Load()
	if err != nil {
		return nil, err
	}
	return serve(store, boot, stateDir, shared, lim, commands{store: store, reg: reg, log: io.Discard})
}
