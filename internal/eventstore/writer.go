// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// The writer is the serialization boundary (docs/design/m0/03 "The
// writer"): every state-dependent decision of a command happens inside one
// writer transaction, in this order: idempotency lookup, authorization,
// expected-state check, reservations, event append, projection update, and
// the command-result row. Anything that fails rolls all of it back.

// Refusals a command can meet (failed/<reason>).
var (
	ErrIdempotencyKeyReused = errors.New("idempotency_key_reused")
	ErrStaleExpectedState   = errors.New("stale_expected_state")
)

// maxIdempotencyKey bounds an idempotency key.
const maxIdempotencyKey = 256

// Command is one request to change state.
type Command struct {
	ID             string        // cmd_…
	Actor          string        // act_…
	IdempotencyKey string        // unique per actor; a repeat replays or is refused
	Input          []byte        // the request as JSON; its canonical digest identifies it
	Expected       []Expectation // object versions the command was decided against
}

// Expectation: the subject's last event is seq Version (0: it has none).
type Expectation struct {
	Subject string
	Version int64
}

// Tx is the writer transaction as the hooks and the command's logic see it.
type Tx struct {
	ctx context.Context
	tx  *sql.Tx
}

// SubjectVersion is the seq of the subject's last event, or 0.
func (t *Tx) SubjectVersion(subject string) (int64, error) {
	var v sql.NullInt64
	if err := t.tx.QueryRowContext(t.ctx, `SELECT max(seq) FROM events WHERE subject = ?`, subject).Scan(&v); err != nil {
		return 0, err
	}
	return v.Int64, nil
}

// Hooks are the decisions other packages make inside the writer
// transaction. Authorize is required: with none, every command is refused
// (fail closed). Reserve and Project may be nil.
type Hooks struct {
	Authorize func(t *Tx, c Command) error      // internal/authority
	Reserve   func(t *Tx, c Command) error      // capacity reservations
	Project   func(t *Tx, events []Event) error // projection updates
}

// Apply is the command's logic: from the current state (read through t) it
// returns the events to append and the command's result (JSON).
type Apply func(t *Tx) (events []Event, result json.RawMessage, err error)

// Outcome is an accepted command: its result and the seqs of its events.
type Outcome struct {
	CommandID         string
	Replayed          bool // a repeat of an accepted command: nothing was run again
	Result            json.RawMessage
	FirstSeq, LastSeq int64 // 0 when the command appended no event
}

// Submit runs c through the writer transaction.
func (s *Store) Submit(ctx context.Context, c Command, apply Apply) (Outcome, error) {
	if err := ids.Check(c.ID, "cmd"); err != nil {
		return Outcome{}, fmt.Errorf("eventstore: command: %w", err)
	}
	if err := ids.Check(c.Actor, "act"); err != nil {
		return Outcome{}, fmt.Errorf("eventstore: command actor: %w", err)
	}
	if c.IdempotencyKey == "" || len(c.IdempotencyKey) > maxIdempotencyKey {
		return Outcome{}, fmt.Errorf("eventstore: an idempotency key is 1 to %d bytes", maxIdempotencyKey)
	}
	inputDigest, err := digest(c.Input)
	if err != nil {
		return Outcome{}, fmt.Errorf("eventstore: command input: %w", err)
	}
	if s.hooks.Authorize == nil {
		return Outcome{}, errors.New("eventstore: no authorizer is configured; every command is refused")
	}
	var out Outcome
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		t := &Tx{ctx: ctx, tx: tx}
		// 1. Idempotency.
		var prev struct {
			id, digest, result string
			first, last        sql.NullInt64
		}
		err := tx.QueryRowContext(ctx, `SELECT id, input_digest, result_json, first_seq, last_seq FROM commands WHERE actor = ? AND idempotency_key = ?`,
			c.Actor, c.IdempotencyKey).Scan(&prev.id, &prev.digest, &prev.result, &prev.first, &prev.last)
		switch {
		case err == nil && prev.digest != inputDigest:
			return fmt.Errorf("eventstore: %w: key %q of %s was used for another input", ErrIdempotencyKeyReused, c.IdempotencyKey, c.Actor)
		case err == nil:
			out = Outcome{CommandID: prev.id, Replayed: true, Result: json.RawMessage(prev.result), FirstSeq: prev.first.Int64, LastSeq: prev.last.Int64}
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		// 2. Authorization.
		if err := s.hooks.Authorize(t, c); err != nil {
			return err
		}
		// 3. Expected state.
		for _, e := range c.Expected {
			v, err := t.SubjectVersion(e.Subject)
			if err != nil {
				return err
			}
			if v != e.Version {
				return fmt.Errorf("eventstore: %w: %s is at %d, the command expected %d", ErrStaleExpectedState, e.Subject, v, e.Version)
			}
		}
		// 4. Reservations.
		if s.hooks.Reserve != nil {
			if err := s.hooks.Reserve(t, c); err != nil {
				return err
			}
		}
		// 5. Events.
		events, result, err := apply(t)
		if err != nil {
			return err
		}
		canonResult, err := canonical(result)
		if err != nil {
			return fmt.Errorf("eventstore: command result: %w", err)
		}
		appended := make([]Event, 0, len(events))
		for _, e := range events {
			if e.Actor == "" {
				e.Actor = c.Actor
			}
			cmd := c.ID
			e.Command = &cmd
			a, err := s.append(ctx, tx, e)
			if err != nil {
				return err
			}
			appended = append(appended, a)
		}
		// 6. Projections.
		if s.hooks.Project != nil && len(appended) > 0 {
			if err := s.hooks.Project(t, appended); err != nil {
				return err
			}
		}
		// 7. The command-result row.
		var first, last sql.NullInt64
		if len(appended) > 0 {
			first = sql.NullInt64{Int64: appended[0].Seq, Valid: true}
			last = sql.NullInt64{Int64: appended[len(appended)-1].Seq, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO commands(id, actor, idempotency_key, input_digest, disposition, result_json, first_seq, last_seq)
			VALUES (?, ?, ?, ?, 'applied', ?, ?, ?)`, c.ID, c.Actor, c.IdempotencyKey, inputDigest, string(canonResult), first, last); err != nil {
			return err
		}
		out = Outcome{CommandID: c.ID, Result: canonResult, FirstSeq: first.Int64, LastSeq: last.Int64}
		return nil
	})
	if err != nil {
		return Outcome{}, err
	}
	return out, nil
}
