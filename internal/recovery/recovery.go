// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Package recovery is restart recovery (docs/design/m1/00-slice-plan.md P21, X3; 05 "Attempt and fencing").
//
// Decisions:
//   - Run takes X3's steps 1-4, each its own command, in order:
//     1. every running attempt is lost{controller_restart}, its publication closed in that transaction
//     (lease.RecoverAfterRestart); then a cut run's exchange output is refused, never ingested (orphans.go);
//     2. its lease is revoked{controller_restart} (active → settling: the fold admits it once publication is closed);
//     3. it is sealed{host_restart} with M1's minimal AttemptRecord, partial_diff null: a cut attempt's workspace is
//     never read, and nothing continues from the record in M1 (L1);
//     4. the lease settles as capacity.Settlement derives (P12b): each call settled or an uncertain liability.
//   - Steps 5 and 6 are placement: dispatch.Match refuses a unit whose last lease has not settled, so the replacement
//     is sized after step 4; Launch's lease.StartNext supersedes the sealed attempt with the next token in one event.
//     No lease.completed occurs: it needs a completed attempt.
//   - Next decides each step from the records alone, again inside its command, so a crash between steps resumes
//     where it stopped, and a run with nothing left records no event. It acts only on an attempt lost on a
//     controller restart that is still its lease's.
//   - The cut calls' effects are uncertain by the restart rule (MarkPendingUncertain); P20's Reconciler settles them,
//     and late usage recomputes the liability (P12b). Recovery queries no provider.
//   - Inert: P22a constructs it, runs it at start after MarkPendingUncertain and before any other command, and
//     authorises its actions for the system actor only.
package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

// Step is the recovery step an attempt needs next, named by its command's action.
type Step string

const (
	ActionLose   = "recovery.lose"
	ActionRefuse = "recovery.refuse_output"

	Done   Step = ""
	Revoke Step = "recovery.revoke" // 2
	Seal   Step = "recovery.seal"   // 3
	Settle Step = "recovery.settle" // 4
)

const restart = types.AttemptLostReasonValueControllerRestart

// Next is the step attempt a, under its lease l, needs next: none unless a was lost on a controller restart and is
// still l's attempt, nor once l has left settling.
func Next(a types.Attempt, l types.Lease) Step {
	switch {
	case a.LostReason == nil || *a.LostReason != restart || l.ID != a.Lease || l.Attempt == nil || *l.Attempt != a.ID:
		return Done
	case l.State == types.LeaseStateActive:
		return Revoke
	case l.State == types.LeaseStateSettling && a.Status == types.AttemptStatusLost:
		return Seal
	case l.State == types.LeaseStateSettling:
		return Settle
	}
	return Done
}

// Recovery runs the steps on a store.
type Recovery struct {
	Store     *eventstore.Store
	Actor     types.ActorID   // the controller's system actor
	Units     lease.Units     // the seal's project and task (graph.Units)
	Blobs     lease.Publisher // where the seal's record is published
	Exchanges string          // the adapter host's exchange root (adapterhost.Host.Root); "" when none
}

// Run runs steps 1 to 4 for every attempt the restart cut. An exchange root it could not remove is reported last.
func (r Recovery) Run(ctx context.Context) error {
	if err := r.submit(ctx, ActionLose, nil, lease.RecoverAfterRestart); err != nil {
		return err
	}
	cleanup, err := r.refuseOrphans(ctx)
	var cut []types.AttemptID
	if err == nil {
		err = r.Store.View(ctx, func(tx *eventstore.Tx) error {
			keys, err := tx.Keys(lease.AttemptProjection, "")
			for i := 0; err == nil && i < len(keys); i++ {
				var a types.Attempt
				if a, _, _, err = lease.AttemptAt(tx, types.AttemptID(keys[i])); a.LostReason != nil && *a.LostReason == restart {
					cut = append(cut, a.ID)
				}
			}
			return err
		})
	}
	for _, id := range cut {
		for n, done := 0, false; err == nil && !done && n < 3; n++ { // at most revoke, seal, settle
			done, err = r.step(ctx, id)
		}
	}
	return errors.Join(err, cleanup)
}

// step applies, as its own command, the step attempt id needs now, or reports done. The command decides again in
// its transaction, and proposes nothing if the records moved on.
func (r Recovery) step(ctx context.Context, id types.AttemptID) (bool, error) {
	var planned Step
	if err := r.Store.View(ctx, func(tx *eventstore.Tx) error {
		_, _, a, l, err := records(tx, id)
		planned = Next(a, l)
		return err
	}); err != nil || planned == Done {
		return true, err
	}
	return false, r.submit(ctx, string(planned), &id, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		arev, lrev, a, l, err := records(tx, id)
		switch {
		case err != nil || Next(a, l) != planned:
			return nil, err
		case planned == Revoke:
			return lease.ChangeLease(tx, l.ID, lrev, lease.LeaseEvent{Kind: lease.Revoked, Reason: string(restart)}, nil)
		case planned == Seal:
			return lease.SealMinimal(tx, id, arev, r.Units, r.Blobs, nil, r.Actor)
		}
		s, err := capacity.Settlement(tx, l.ID)
		if err != nil {
			return nil, err
		}
		return lease.ChangeLease(tx, l.ID, lrev, lease.LeaseEvent{Kind: lease.Settled, Settlement: &s}, nil)
	})
}

// records is attempt id and its lease, with their revisions.
func records(tx *eventstore.Tx, id types.AttemptID) (arev, lrev int64, a types.Attempt, l types.Lease, err error) {
	a, arev, ok, err := lease.AttemptAt(tx, id)
	if err == nil && ok {
		l, lrev, ok, err = lease.LeaseOf(tx, a.Lease)
	}
	if err == nil && !ok {
		err = fmt.Errorf("recovery: attempt %s or its lease is missing", id)
	}
	return arev, lrev, a, l, err
}

// submit records one recovery command of action, about attempt (nil: every attempt), applying what decide proposes.
func (r Recovery) submit(ctx context.Context, action string, attempt *types.AttemptID, decide func(*eventstore.Tx) ([]eventstore.Event, error)) error {
	cmd, err := ids.New("cmd")
	input, merr := json.Marshal(map[string]any{"attempt": attempt})
	if err = errors.Join(err, merr); err == nil {
		_, err = r.Store.SubmitApplied(ctx, eventstore.Command{ID: cmd, Actor: string(r.Actor), Action: action, IdempotencyKey: cmd, Input: input},
			eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				evs, err := decide(tx)
				return evs, json.RawMessage(`{}`), err
			}))
	}
	if err != nil {
		return fmt.Errorf("recovery: %s: %w", action, err)
	}
	return nil
}
