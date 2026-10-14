// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Package recovery is restart recovery (docs/design/m1/00-slice-plan.md P21a, X3; 05 "Attempt and fencing").
//
// Decisions:
//   - Run takes X3's steps 1-4, each its own command, in order:
//     1. every running attempt is lost{controller_restart}, its publication closed in that transaction
//     (lease.RecoverAfterRestart), so the fence refuses any later output of it (stale_attempt);
//     2. its lease is revoked{controller_restart} (active → settling: the fold admits it once publication is closed);
//     3. it is sealed with M1's minimal AttemptRecord, partial_diff null: a cut attempt's workspace is never read,
//     and nothing continues from the record in M1 (L1);
//     4. the lease settles as capacity.Settlement derives (P12b): each call settled or an uncertain liability.
//   - Steps 2-4 also converge an attempt the crash left stopping, or lost for another reason (agent_disconnect,
//     stream_dead), or sealed from either while its lease is still live: at start no adapter runs, so nothing else
//     would. Its seal is abrupt_loss (drain for a drain stop, lease.SealReason). A finishing attempt, or one sealed
//     from finishing, holds an admitted result: it is the completion path's (P22a), never revoked here.
//   - Steps 5 and 6 are placement: dispatch.Match refuses a unit whose last lease has not settled, and
//     lease.SupersedeAttempt refuses a replacement while the old attempt's lease settles, so the replacement is
//     sized after step 4 and supersedes the sealed attempt with the next token in one event. No lease.completed occurs.
//   - Next decides each step from the records alone, again inside its command, so a crash between steps resumes
//     where it stopped, and a run with nothing left records no event.
//   - Recovery never inspects the adapter's workspace or exchange directory. Step 3 publishes the seal's record to
//     the controller's own, trusted blob store, which the seal's admission reads back: a failure there stops that
//     attempt's recovery only (it stays at its last step until the next run), never another attempt's.
//   - The cut calls' effects are uncertain by the restart rule (MarkPendingUncertain); P20's Reconciler settles them,
//     and late usage recomputes the liability (P12b). Recovery queries no provider.
//   - Inert: P22a constructs it, runs it at start after MarkPendingUncertain and before any other command, and
//     authorises its actions for the system actor only. It then runs RefuseOrphans, whose failure never blocks start.
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
	ActionLose = "recovery.lose"

	Done   Step = ""
	Revoke Step = "recovery.revoke" // 2
	Seal   Step = "recovery.seal"   // 3
	Settle Step = "recovery.settle" // 4
)

const restart = types.AttemptLostReasonValueControllerRestart

// Next is the step attempt a, under its lease l, needs next at start: none unless a is still l's attempt and was cut
// (lost, stopping, or sealed from either), nor once l has left settling.
func Next(a types.Attempt, l types.Lease) Step {
	cut := a.Status == types.AttemptStatusLost || a.Status == types.AttemptStatusStopping ||
		a.Status == types.AttemptStatusSealed && (a.LostReason != nil || a.StopReason != nil)
	switch {
	case !cut || l.ID != a.Lease || l.Attempt == nil || *l.Attempt != a.ID:
		return Done
	case l.State == types.LeaseStateActive:
		return Revoke
	case l.State == types.LeaseStateSettling && a.Status != types.AttemptStatusSealed:
		return Seal
	case l.State == types.LeaseStateSettling:
		return Settle
	}
	return Done
}

// Recovery runs the steps on a store.
type Recovery struct {
	Store *eventstore.Store
	Actor types.ActorID   // the controller's system actor
	Units lease.Units     // the seal's project and task (graph.Units)
	Blobs lease.Publisher // where the seal's record is published
	// Exchanges is where exchange roots are made (adapterhost.Host.Root); empty: RefuseOrphans does nothing.
	Exchanges string
}

// Run runs step 1, then steps 2 to 4 for every attempt cut, each to its end; it reports every failure.
func (r Recovery) Run(ctx context.Context) error {
	if err := r.submit(ctx, ActionLose, "", nil, lease.RecoverAfterRestart); err != nil {
		return err
	}
	var all []string
	err := r.Store.View(ctx, func(tx *eventstore.Tx) (err error) {
		all, err = tx.Keys(lease.AttemptProjection, "")
		return err
	})
	for _, id := range all {
		var aerr error
		for n, done := 0, false; aerr == nil && !done && n < 3; n++ { // at most revoke, seal, settle
			done, aerr = r.step(ctx, types.AttemptID(id))
		}
		err = errors.Join(err, aerr)
	}
	return err
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
	return false, r.submit(ctx, string(planned), "", &id, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
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

// submit records one recovery command of action, about attempt (nil: every attempt), applying what decide proposes;
// a repeat of key replays the first (empty: the command's own id).
func (r Recovery) submit(ctx context.Context, action, key string, attempt *types.AttemptID, decide func(*eventstore.Tx) ([]eventstore.Event, error)) error {
	cmd, err := ids.New("cmd")
	input, merr := json.Marshal(map[string]any{"attempt": attempt})
	if key == "" {
		key = cmd
	}
	if err = errors.Join(err, merr); err == nil {
		_, err = r.Store.SubmitApplied(ctx, eventstore.Command{ID: cmd, Actor: string(r.Actor), Action: action, IdempotencyKey: key, Input: input},
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
