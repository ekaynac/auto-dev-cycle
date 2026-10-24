// SPDX-License-Identifier: Apache-2.0

package lease

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// RefusalProjection counts each attempt's attempt.output_refused events:
// the read-time derivation of an untrustworthy attempt (09), a fold of the
// log like every projection.
const RefusalProjection = "attempt_refusals"

// FenceAttempt fences an output of attempt id with token against its
// unit's current attempt (05): a *Stale when it may not land.
func FenceAttempt(tx *eventstore.Tx, id types.AttemptID, token int64) error {
	a, _, ok, err := load[types.Attempt](tx, AttemptProjection, string(id))
	switch {
	case err != nil:
		return err
	case !ok:
		return &Stale{fmt.Sprintf("attempt %s does not exist", id)}
	}
	return FenceOutput(tx, a.Unit, id, token)
}

// SubmitResult proposes attempt.result_submitted carrying the admitted
// result r (running → finishing, publication closed), fenced first: a
// *Stale proposes nothing, and the caller records the refusal.
func SubmitResult(tx *eventstore.Tx, id types.AttemptID, token int64, r types.SubmittedResult) ([]eventstore.Event, error) {
	if err := FenceAttempt(tx, id, token); err != nil {
		return nil, err
	}
	a, _, _, err := load[types.Attempt](tx, AttemptProjection, string(id))
	if err != nil {
		return nil, err
	}
	n, err := ApplyAttempt(a, AttemptEvent{Kind: ResultSubmitted})
	if err != nil {
		return nil, err
	}
	ev, err := newEvent(string(id), payload{Transition: ResultSubmitted, Attempt: &n, Result: &r})
	return []eventstore.Event{ev}, err
}

// LoseRunning proposes attempt.lost{reason} for an attempt still running,
// and nothing for one in any other state: an attempt whose adapter is gone
// without an admitted result stops publishing (05).
func LoseRunning(tx *eventstore.Tx, id types.AttemptID, reason types.AttemptLostReasonValue) ([]eventstore.Event, error) {
	a, _, ok, err := load[types.Attempt](tx, AttemptProjection, string(id))
	if err != nil || !ok || a.Status != types.AttemptStatusRunning {
		return nil, errors.Join(err, missing(ok, id))
	}
	n, err := ApplyAttempt(a, AttemptEvent{Kind: Lost, LostReason: reason})
	if err != nil {
		return nil, err
	}
	ev, err := newEvent(string(id), payload{Transition: Lost, Attempt: &n})
	return []eventstore.Event{ev}, err
}

// StopRunning proposes attempt.stop_requested{reason} for an attempt still
// running, and nothing for one in any other state, which keeps its state
// and settles normally (13 supersession: preempt).
func StopRunning(tx *eventstore.Tx, id types.AttemptID, reason types.AttemptStopReasonValue) ([]eventstore.Event, error) {
	a, _, ok, err := load[types.Attempt](tx, AttemptProjection, string(id))
	if err != nil || !ok || a.Status != types.AttemptStatusRunning {
		return nil, errors.Join(err, missing(ok, id))
	}
	n, err := ApplyAttempt(a, AttemptEvent{Kind: StopRequested, StopReason: reason})
	if err != nil {
		return nil, err
	}
	ev, err := newEvent(string(id), payload{Transition: StopRequested, Attempt: &n})
	return []eventstore.Event{ev}, err
}

// AttemptOf is the attempt's record as its events project it, with the
// command that requested its stop, if one did.
func AttemptOf(tx *eventstore.Tx, id types.AttemptID) (a types.Attempt, stoppedBy *string, ok bool, err error) {
	a, _, ok, err = load[types.Attempt](tx, AttemptProjection, string(id))
	if raw, stopped, serr := tx.Get(StopProjection, string(id)); serr != nil || stopped {
		err = errors.Join(err, serr, json.Unmarshal(raw, &stoppedBy))
	}
	return a, stoppedBy, ok, err
}

// CurrentAttempt is unit's current attempt: its first, or the successor
// that superseded the last one; false when none has started.
func CurrentAttempt(tx *eventstore.Tx, unit types.UnitID) (types.AttemptID, bool, error) {
	return unitAttempt(tx, unit)
}

// AttemptsOn are every attempt started on unit, first to last: its
// current attempt and each it supersedes (a unit's every later attempt
// supersedes the current one), read by the unit, never by scanning.
func AttemptsOn(tx *eventstore.Tx, unit types.UnitID) ([]types.Attempt, error) {
	id, ok, err := unitAttempt(tx, unit)
	var out []types.Attempt
	for seen := map[types.AttemptID]bool{}; ok && err == nil; {
		a, _, found, lerr := load[types.Attempt](tx, AttemptProjection, string(id))
		if err = errors.Join(lerr, missing(found, id)); err == nil && seen[id] {
			err = fmt.Errorf("lease: unit %s's attempts supersede in a cycle at %s", unit, id)
		}
		if err == nil {
			seen[id], out = true, append(out, a)
			if ok = a.Supersedes != nil; ok {
				id = *a.Supersedes
			}
		}
	}
	slices.Reverse(out)
	return out, err
}

func missing(ok bool, id types.AttemptID) error {
	if ok {
		return nil
	}
	return fmt.Errorf("lease: attempt %s does not exist", id)
}

// maxDetail is the refusal detail's bound (attempt-output-refusal.json).
const maxDetail = 1024

// Refusal builds the attempt.output_refused event of one refused output:
// reason stale_attempt for the fence's check, unsafe_output for any other.
// The detail is cut to its bound on a rune boundary.
func Refusal(attempt types.AttemptID, h types.AttemptOutputRefusalHandle, check types.AttemptOutputRefusalCheck, detail string) (eventstore.Event, types.AttemptOutputRefusal, error) {
	reason := types.AttemptOutputRefusalReasonUnsafeOutput
	if check == types.AttemptOutputRefusalCheckStaleAttempt {
		reason = types.AttemptOutputRefusalReasonStaleAttempt
	}
	for len(detail) > maxDetail {
		_, size := utf8.DecodeLastRuneInString(detail)
		detail = detail[:len(detail)-size]
	}
	if detail == "" {
		detail = string(check)
	}
	r := types.AttemptOutputRefusal{Attempt: attempt, Handle: h, Reason: reason, Check: check, Detail: detail}
	id, err := ids.New("evt")
	if err != nil {
		return eventstore.Event{}, r, err
	}
	raw, err := json.Marshal(r)
	return eventstore.Event{ID: id, Kind: OutputRefused, Subject: string(attempt), Payload: raw}, r, err
}

// refused counts one refusal under its attempt.
func refused(tx *eventstore.Tx, ev eventstore.Event) error {
	var r types.AttemptOutputRefusal
	if err := json.Unmarshal(ev.Payload, &r); err != nil {
		return err
	}
	if ev.Subject != string(r.Attempt) {
		return fmt.Errorf("subject %s is not its attempt %s", ev.Subject, r.Attempt)
	}
	n, err := refusals(tx, r.Attempt)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(n + 1)
	if err != nil {
		return err
	}
	return tx.Put(RefusalProjection, string(r.Attempt), raw)
}

func refusals(tx *eventstore.Tx, id types.AttemptID) (int64, error) {
	raw, ok, err := tx.Get(RefusalProjection, string(id))
	if err != nil || !ok {
		return 0, err
	}
	var n int64
	return n, json.Unmarshal(raw, &n)
}

// Untrustworthy reports whether any output of the attempt was refused (09:
// derived at read time from its attempt.output_refused events).
func Untrustworthy(tx *eventstore.Tx, id types.AttemptID) (bool, error) {
	n, err := refusals(tx, id)
	return n > 0, err
}
