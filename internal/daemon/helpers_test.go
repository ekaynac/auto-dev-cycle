// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
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

func stateDir(t *testing.T) string { return filepath.Join(t.TempDir(), "state") }

// seeder is a store under the state directory, opened outside the daemon
// with a permissive authorizer, to write a history the daemon then starts
// on. project picks the Project hook from the store's blobs and holds.
type seeder struct {
	t     *testing.T
	s     *eventstore.Store
	holds map[string]types.Hold
	done  func()
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
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: kinds, Blobs: blobs,
		Hooks: eventstore.Hooks{Authorize: func(*eventstore.Tx, eventstore.Command) error { return nil }, Project: project(blobs, reg.Holds)}})
	if err != nil {
		t.Fatal(err)
	}
	return &seeder{t: t, s: s, holds: reg.Holds, done: func() {
		if err := errors.Join(s.Close(), lock.Release()); err != nil {
			t.Fatal(err)
		}
	}}
}

// do submits one command as actor, appending what fn returns.
func (w *seeder) do(actor types.ActorID, fn func(tx *eventstore.Tx) ([]eventstore.Event, error)) {
	w.t.Helper()
	cmd := newID(w.t, "cmd")
	if _, err := w.s.Submit(context.Background(), eventstore.Command{ID: cmd, Actor: string(actor), IdempotencyKey: cmd, Input: []byte(`{}`)},
		func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := fn(tx)
			return evs, json.RawMessage(`{}`), err
		}); err != nil {
		w.t.Fatal(err)
	}
}

// grantedLease requests a lease reserving amount tokens of the quota group
// and grants it.
func (w *seeder) grantedLease(actor types.ActorID, amount int64) types.LeaseID {
	w.t.Helper()
	ok := types.PlacementCheck{Outcome: types.Outcome(vocab.Empty), Reason: vocab.ReasonCompletedClean}
	l := types.Lease{ID: types.LeaseID(newID(w.t, "lea")), Unit: types.UnitID(newID(w.t, "unt")), Role: "builder",
		Executor: types.ExecutorID(newID(w.t, "exe")), State: types.LeaseStateRequested,
		Reservations: []types.LeaseReservationsItem{{Group: quota, Amount: types.Amount(amount), Unit: types.CapacityUnitTokens}},
		Footprint:    types.LeaseFootprint{ContextTokens: 1, ExpectedTurns: 1}, Deadline: "2026-10-04T14:00:00Z",
		PlacementChecks: types.LeasePlacementChecks{DataFloor: ok, Destinations: ok, Capability: ok, Independence: ok,
			PaidAuthority: ok, Budget: ok, FallbackScope: ok}}
	w.do(actor, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, l) })
	w.do(actor, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, l.ID, 1, lease.LeaseEvent{Kind: lease.Granted}, w.holds)
	})
	return l.ID
}

// startErr asserts that err is a StartError with outcome and reason.
func startErr(t *testing.T, err error, outcome vocab.Outcome, reason vocab.Reason) {
	t.Helper()
	var se *StartError
	if !errors.As(err, &se) || se.Outcome != outcome || se.Reason != reason {
		t.Fatalf("want %s/%s, got %v", outcome, reason, err)
	}
}
