// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Disposition is how a committed command ended (docs/design/m0/14 N2).
// Every disposition is committed: the command's events, if any, and its
// command row, so a repeat of the command replays it. A command the writer
// cannot decide (a reused key, an authorization refusal, a stale
// expectation, a fault) is an error instead and commits nothing.
//
// The HTTP response (schemas/api/command-response.json) is a union on
// applied, proposed and refused, and maps these one to one except
// invalidated: a proposal found changed is closed by committed events and
// answered as refused, with the registered reason its result records; the
// invalidation event stays in the log.
type Disposition string

const (
	DispositionApplied     Disposition = "applied"     // the command took effect
	DispositionProposed    Disposition = "proposed"    // a proposal waits for a confirmation; nothing was applied
	DispositionInvalidated Disposition = "invalidated" // a proposal was closed as changed; nothing was applied
	DispositionRefused     Disposition = "refused"     // decided and refused, the refusal recorded; nothing was applied
)

func (d Disposition) known() bool {
	switch d {
	case DispositionApplied, DispositionProposed, DispositionInvalidated, DispositionRefused:
		return true
	}
	return false
}

// Decision is the command's logic's verdict. There is no default: a zero
// Decision is refused.
type Decision struct {
	Disposition Disposition
	Events      []Event
	// Result is stored and replayed as is: stable, and free of secrets (a
	// credential's plaintext never enters the store, 14 N3). A
	// refused or invalidated result names its refusal:
	// {"outcome": "failed" | "could_not_run", "reason": <registered>, "detail": …}.
	Result json.RawMessage
}

// Decide is the command's logic: from the current state (read through t) it
// decides the command.
type Decide func(t *Tx) (Decision, error)

// Applies is the Decide of logic that only ever applies.
func Applies(apply Apply) Decide {
	return func(t *Tx) (Decision, error) {
		evs, result, err := apply(t)
		return Decision{Disposition: DispositionApplied, Events: evs, Result: result}, err
	}
}

// Committed is a command the writer committed, whatever its disposition.
// It is not a sign that the command took effect: only Applied, which
// refuses every other disposition, yields the Outcome execution advances on.
type Committed struct {
	Disposition       Disposition
	CommandID         string
	Replayed          bool // a stored result returned again: nothing ran
	Result            json.RawMessage
	FirstSeq, LastSeq int64
	Effects           []Effect
}

// ErrNotApplied: the command was committed with another disposition.
var ErrNotApplied = errors.New("eventstore: the command was committed but not applied")

// Applied is the outcome of an applied command, or ErrNotApplied.
func (c Committed) Applied() (Outcome, error) {
	if c.Disposition != DispositionApplied {
		return Outcome{}, fmt.Errorf("%w: %s was %s: %s", ErrNotApplied, c.CommandID, c.Disposition, c.Result)
	}
	return Outcome{CommandID: c.CommandID, Replayed: c.Replayed, Result: c.Result, FirstSeq: c.FirstSeq, LastSeq: c.LastSeq, Effects: c.Effects}, nil
}

// SubmitApplied is Submit for a caller that goes on only when the command
// applied: any other disposition, committed or replayed, is ErrNotApplied.
// Every internal caller goes through it.
func (s *Store) SubmitApplied(ctx context.Context, c Command, decide Decide) (Outcome, error) {
	res, err := s.Submit(ctx, c, decide)
	if err != nil {
		return Outcome{}, err
	}
	return res.Applied()
}

// checkDecision is the writer's check of a decision against what the
// command intends and what it stores.
func checkDecision(d Decision, result []byte, intents int) error {
	if !d.Disposition.known() {
		return fmt.Errorf("eventstore: command disposition %q", d.Disposition)
	}
	if d.Disposition != DispositionApplied && intents > 0 {
		return fmt.Errorf("eventstore: a %s command intends no effect", d.Disposition)
	}
	if d.Disposition == DispositionRefused || d.Disposition == DispositionInvalidated {
		var r struct{ Reason string }
		if json.Unmarshal(result, &r) != nil || r.Reason == "" {
			return fmt.Errorf("eventstore: a %s command's result names its reason", d.Disposition)
		}
	}
	return nil
}

// Decided is the Authorize hook deciding the command itself, in place of
// its logic (14 H2 item 4): a proposal, recorded with its events and the
// proposed disposition, or an invalidation. The logic then never runs.
// Authority never applies a command on its logic's behalf, so an applied
// decision is refused here.
func (t *Tx) Decided(d Decision) error {
	switch {
	case t.store == nil || t.readOnly || t.rebuild:
		return errors.New("eventstore: only a command's transaction is decided by its authorizer")
	case t.decided != nil:
		return errors.New("eventstore: the command is already decided")
	case d.Disposition == DispositionApplied || !d.Disposition.known():
		return fmt.Errorf("eventstore: an authorizer does not decide a command %q", d.Disposition)
	}
	t.decided = &d
	return nil
}

// decision is the Authorize hook's decision, if it made one, else the
// command's logic's.
func (t *Tx) decision(decide Decide) (Decision, error) {
	if t.decided != nil {
		return *t.decided, nil
	}
	return decide(t)
}
