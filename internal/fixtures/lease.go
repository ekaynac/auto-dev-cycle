// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// oneTask binds every unit to one project and task (no seal is under test).
type oneTask struct{}

func (oneTask) ProjectTask(*eventstore.Tx, types.UnitID) (types.ProjectID, types.TaskID, bool, error) {
	return "prj_01m3250vkwffc979nzjw0g2sf9", "tsk_01m3250v9xft08ssr4ygq6g2np", true, nil
}

// Startable admits an attempt on every unit: the M0 fixtures run leases
// with no graph.
func (oneTask) Startable(*eventstore.Tx, types.UnitID) error { return nil }

// outputKind is the event an attempt's output appends in this fixture.
const outputKind = "fixture.output/v1"

// revokedAttemptLateResult: a running attempt's output lands; once the
// attempt is stopped (revoked), its late output is refused and writes
// nothing (11: "a revoked attempt's late result"; 05 "Fencing").
func revokedAttemptLateResult(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	reg, err := vocab.Load()
	if err != nil {
		return nil, nil, err
	}
	state := filepath.Join(dir, "state")
	lock, err := eventstore.AcquireLock(state)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, lock.Release()) }()
	blobs, err := blobstore.Open(state)
	if err != nil {
		return nil, nil, err
	}
	kinds := lease.Kinds()
	kinds[outputKind] = eventstore.Kind{Schema: "common/defs"}
	s, err := eventstore.Open(ctx, lock, eventstore.Config{Kinds: kinds, Blobs: blobs,
		Hooks: eventstore.Hooks{Identify: func(*eventstore.Tx, eventstore.Command) error { return nil }, Authorize: func(*eventstore.Tx, eventstore.Command) error { return nil },
			Project: lease.Projector{Blobs: blobs, Holds: reg.Holds, Units: oneTask{}}.Project}})
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	actor, err := ids.New("act")
	if err != nil {
		return nil, nil, err
	}
	do := func(fn func(*eventstore.Tx) ([]eventstore.Event, error)) error {
		cmd, err := ids.New("cmd")
		if err != nil {
			return err
		}
		_, err = s.SubmitApplied(ctx, eventstore.Command{ID: cmd, Actor: actor, Action: "fixture.command", IdempotencyKey: cmd, Input: []byte(`{}`)}, eventstore.Applies(
			func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				evs, err := fn(tx)
				return evs, json.RawMessage(`{}`), err
			}))
		return err
	}
	l, attempt, err := freshLease()
	if err != nil {
		return nil, nil, err
	}
	base := types.AttemptBase{Tree: types.GitID("git:" + "cccccccccccccccccccccccccccccccccccccccc"), ContractRevision: "7"}
	for _, setup := range []func(*eventstore.Tx) ([]eventstore.Event, error){
		func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, l) },
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.ChangeLease(tx, l.ID, 1, lease.LeaseEvent{Kind: lease.Granted}, reg.Holds)
		},
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.StartAttempt(tx, l.ID, 2, attempt, base)
		},
	} {
		if err := do(setup); err != nil {
			return nil, nil, err
		}
	}
	// output is the attempt's output landing as one step: a command that
	// fences and appends one event. The step's head shows whether it wrote.
	var o observe
	output := func() (int64, error) {
		if err := o.step(do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			if err := lease.FenceOutput(tx, l.Unit, attempt, 1); err != nil {
				return nil, err
			}
			ev, err := ids.New("evt")
			return []eventstore.Event{{ID: ev, Kind: outputKind, Subject: string(attempt), Payload: []byte(`{}`)}}, err
		})); err != nil {
			return 0, err
		}
		h, err := s.Head(ctx)
		return h.Seq, err
	}
	landed, err := output()
	if err != nil {
		return nil, nil, err
	}
	// The revocation is setup, not a step: a refused stop is an error.
	if err := do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeAttempt(tx, attempt, 1, lease.AttemptEvent{Kind: lease.StopRequested, StopReason: types.AttemptStopReasonValueRevoke})
	}); err != nil {
		return nil, nil, err
	}
	stopped, err := s.Head(ctx)
	if err != nil {
		return nil, nil, err
	}
	late, err := output()
	if err != nil {
		return nil, nil, err
	}
	o.check(late == stopped.Seq, "the late output wrote: the log's head moved from %d to %d", stopped.Seq, late)
	o.check(landed < stopped.Seq, "the running attempt's output did not land before the stop")
	return o.steps, o.violations, nil
}

// freshLease is a requested lease on a new unit, with its attempt's id.
func freshLease() (types.Lease, types.AttemptID, error) {
	var idv [4]string
	for i, p := range []string{"lea", "unt", "exe", "att"} {
		v, err := ids.New(p)
		if err != nil {
			return types.Lease{}, "", err
		}
		idv[i] = v
	}
	ok := types.PlacementCheck{Outcome: types.Outcome(vocab.Empty), Reason: vocab.ReasonCompletedClean}
	return types.Lease{ID: types.LeaseID(idv[0]), Unit: types.UnitID(idv[1]), Role: "builder", Executor: types.ExecutorID(idv[2]),
		State:        types.LeaseStateRequested,
		Reservations: []types.LeaseReservationsItem{{Group: "rg_01m3250vkwffc979nzjw0g2sf9", Amount: 1000, Unit: types.CapacityUnitTokens}},
		Footprint:    types.LeaseFootprint{ContextTokens: 1000, ExpectedTurns: 5}, Deadline: "2026-10-04T14:00:00Z",
		PlacementChecks: types.LeasePlacementChecks{DataFloor: ok, Destinations: ok, Capability: ok, Independence: ok,
			PaidAuthority: ok, Budget: ok, FallbackScope: ok}}, types.AttemptID(idv[3]), nil
}
