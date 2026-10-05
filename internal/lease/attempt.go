// SPDX-License-Identifier: Apache-2.0

package lease

import (
	"errors"
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The attempt event kinds (attempt.started is Start and Supersede's).
const (
	StopRequested   = "attempt.stop_requested"
	ResultSubmitted = "attempt.result_submitted"
	Lost            = "attempt.lost"
	Sealed          = "attempt.sealed"
	AttemptDone     = "attempt.completed"
	Superseded      = "attempt.superseded"
)

// AttemptEvent is one attempt transition with the data it carries.
type AttemptEvent struct {
	Kind       string
	StopReason types.AttemptStopReasonValue  // attempt.stop_requested
	LostReason types.AttemptLostReasonValue  // attempt.lost
	Checkpoint types.Digest                  // attempt.sealed: the AttemptRecord blob
	Completion *types.AttemptCompletionValue // attempt.completed
}

type attemptStep struct {
	from  types.AttemptStatus
	event string
}

// attemptTable is note 05's attempt state machine. Superseded is Supersede's.
var attemptTable = map[attemptStep]types.AttemptStatus{
	{types.AttemptStatusRunning, StopRequested}:   types.AttemptStatusStopping,
	{types.AttemptStatusRunning, ResultSubmitted}: types.AttemptStatusFinishing,
	{types.AttemptStatusRunning, Lost}:            types.AttemptStatusLost,
	{types.AttemptStatusStopping, Sealed}:         types.AttemptStatusSealed,
	{types.AttemptStatusFinishing, Sealed}:        types.AttemptStatusSealed,
	{types.AttemptStatusLost, Sealed}:             types.AttemptStatusSealed,
	{types.AttemptStatusSealed, AttemptDone}:      types.AttemptStatusCompleted,
}

// ApplyAttempt returns the attempt after event e. Every event leaves running
// or a later state, so publication is closed in the same step.
func ApplyAttempt(a types.Attempt, e AttemptEvent) (types.Attempt, error) {
	to, ok := attemptTable[attemptStep{a.Status, e.Kind}]
	if !ok {
		return types.Attempt{}, fmt.Errorf("%w: %s on a %s attempt", ErrTransition, e.Kind, a.Status)
	}
	n := a
	n.Status, n.Publication = to, types.AttemptPublicationClosed
	switch e.Kind {
	case StopRequested:
		if e.StopReason == "" {
			return types.Attempt{}, errors.New("lease: a stop names its reason")
		}
		r := e.StopReason
		n.StopReason = &r
	case Lost:
		if e.LostReason == "" {
			return types.Attempt{}, errors.New("lease: a lost attempt names its reason")
		}
		r := e.LostReason
		n.LostReason = &r
	case Sealed:
		if e.Checkpoint == "" {
			return types.Attempt{}, errors.New("lease: a seal names its AttemptRecord")
		}
		c := e.Checkpoint
		n.Checkpoint = &c
	case AttemptDone:
		if e.Completion == nil {
			return types.Attempt{}, errors.New("lease: attempt.completed carries its outcome and reason")
		}
		c := *e.Completion
		n.Completion = &c
	}
	return n, nil
}

// Start mints a unit's first attempt: token 1, running, publication open.
func Start(id types.AttemptID, unit types.UnitID, lease types.LeaseID, base types.AttemptBase) types.Attempt {
	return types.Attempt{ID: id, Unit: unit, Lease: lease, FencingToken: 1, Base: base,
		Status: types.AttemptStatusRunning, Publication: types.AttemptPublicationOpen}
}

// Supersede retires a sealed attempt and mints its replacement with the next
// token, as one step: the caller writes attempt.superseded and
// attempt.started in one transaction.
func Supersede(old types.Attempt, id types.AttemptID, lease types.LeaseID, base types.AttemptBase) (types.Attempt, types.Attempt, error) {
	if old.Status != types.AttemptStatusSealed {
		return types.Attempt{}, types.Attempt{}, fmt.Errorf("%w: only a sealed attempt is superseded, not a %s one", ErrTransition, old.Status)
	}
	if id == old.ID {
		return types.Attempt{}, types.Attempt{}, errors.New("lease: a replacement has its own id")
	}
	retired := old
	retired.Status = types.AttemptStatusSuperseded
	by := id
	retired.SupersededBy = &by
	next := Start(id, old.Unit, lease, base)
	next.FencingToken = old.FencingToken + 1
	prev := old.ID
	next.Supersedes = &prev
	return retired, next, nil
}

// LoseOnRestart is the first step after a controller restart: a running
// attempt becomes lost (publication closed) before anything else runs. Any
// other attempt is returned unchanged.
func LoseOnRestart(a types.Attempt) types.Attempt {
	if a.Status != types.AttemptStatusRunning {
		return a
	}
	n, err := ApplyAttempt(a, AttemptEvent{Kind: Lost, LostReason: types.AttemptLostReasonValueControllerRestart})
	if err != nil {
		panic(err) // running → lost is always in the table
	}
	return n
}

// Stale is the refusal of an output from anything but the current attempt
// with its exact token and open publication: failed/stale_attempt, recorded.
type Stale struct{ Detail string }

func (s *Stale) Error() string {
	return "lease: " + vocab.ReasonStaleAttempt.String() + ": " + s.Detail
}

// Reason is the registered reason of the refusal.
func (s *Stale) Reason() vocab.Reason { return vocab.ReasonStaleAttempt }

// Fence decides whether an output (a result, patch or checkpoint
// contribution) from attempt id with token may land, given the unit's
// current attempt. Usage reports are never fenced: they go to the ledger
// from any attempt.
func Fence(current types.Attempt, id types.AttemptID, token int64) error {
	switch {
	case id != current.ID:
		return &Stale{fmt.Sprintf("attempt %s is not the unit's current attempt %s", id, current.ID)}
	case token != current.FencingToken:
		return &Stale{fmt.Sprintf("token %d is not the current token %d", token, current.FencingToken)}
	case current.Publication != types.AttemptPublicationOpen && !mutation.Is(mutation.FenceIgnoresClosedPublication): // open exactly while running (the schema's rule)
		return &Stale{fmt.Sprintf("attempt %s is %s; its publication is closed", id, current.Status)}
	}
	return nil
}
