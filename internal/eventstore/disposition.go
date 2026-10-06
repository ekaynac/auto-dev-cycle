// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// Result is stored and replayed as is: stable, and free of secrets. A
	// refused or invalidated result names its refusal:
	// {"outcome": "failed" | "could_not_run", "reason": <registered>, "detail": …}.
	Result json.RawMessage
	// Credential is a secret the command issued (its events hold only a
	// hash). It is delivered once: returned by this Submit call, never
	// stored, absent on a replay. Only an applied command issues one.
	Credential *IssuedCredential
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

// IssuedCredential is a first-delivery-only secret. Like identity.Secret it
// holds the secret opaquely: every formatting verb shows a placeholder,
// JSON marshalling is refused, and only Reveal returns it.
type IssuedCredential struct {
	id     string
	secret *string
}

// NewIssuedCredential is the credential id (as its events record it) with
// its secret.
func NewIssuedCredential(id, secret string) *IssuedCredential {
	return &IssuedCredential{id: id, secret: &secret}
}

// ID is the credential's public id.
func (c IssuedCredential) ID() string { return c.id }

// Reveal returns the secret, for the one live delivery that shows it.
func (c IssuedCredential) Reveal() string {
	if c.secret == nil {
		return ""
	}
	return *c.secret
}

func (c IssuedCredential) String() string             { return "credential " + c.id + " (secret withheld)" }
func (c IssuedCredential) GoString() string           { return c.String() }
func (c IssuedCredential) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }
func (IssuedCredential) MarshalJSON() ([]byte, error) { return nil, errSecretMarshalled }

var errSecretMarshalled = errors.New("eventstore: an issued credential is never marshalled")

// minSecret is the shortest secret the writer accepts: a shorter one could
// not be told apart from ordinary stored text.
const minSecret = 16

// Committed is a command the writer committed, whatever its disposition.
// It is not a sign that the command took effect: only Applied, which
// refuses every other disposition, yields the Outcome execution advances on.
type Committed struct {
	Disposition       Disposition
	CommandID         string
	Replayed          bool
	Result            json.RawMessage
	FirstSeq, LastSeq int64
	Effects           []Effect
	Credential        *IssuedCredential // the live call's only; nil on a replay
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
	if d.Credential == nil {
		return nil
	}
	if d.Disposition != DispositionApplied || d.Credential.ID() == "" || !printableSecret(d.Credential.Reveal()) {
		return fmt.Errorf("eventstore: only an applied command issues a credential, with an id and a secret of at least %d printable ASCII bytes other than \" and \\", minSecret)
	}
	return nil
}

// printableSecret: canonical JSON stores such a secret verbatim, never
// escaped, so a search of the stored bytes finds every copy of it.
func printableSecret(v string) bool {
	if len(v) < minSecret {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] > 0x7e || v[i] == '"' || v[i] == '\\' {
			return false
		}
	}
	return true
}

// storesSecret reports whether anything the command wrote holds secret:
// its events (every text column, payloads canonical, so an escaped copy
// is stored decoded), its effects and command row, and the projections,
// which are all searched (a command that issues a credential is rare).
// It runs last in the transaction, after every write.
func storesSecret(ctx context.Context, tx *sql.Tx, command string, first, last int64, secret string) (bool, error) {
	var found bool
	err := tx.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM events WHERE seq BETWEEN ? AND ? AND instr(subject || char(0) || coalesce(project, '') || char(0) ||
			coalesce(causation, '') || char(0) || CAST(payload AS TEXT), ?) > 0)
		OR EXISTS(SELECT 1 FROM effects WHERE command = ? AND instr(target || char(0) || last_observation || char(0) || coalesce(result_ref, ''), ?) > 0)
		OR EXISTS(SELECT 1 FROM commands WHERE id = ? AND instr(result_json, ?) > 0)
		OR EXISTS(SELECT 1 FROM proj_kv WHERE instr(key || char(0) || value, ?) > 0)`,
		first, last, secret, command, secret, command, secret, secret).Scan(&found)
	return found, err
}
