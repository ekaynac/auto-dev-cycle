// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
)

// The writer is the serialization boundary (docs/design/m0/03 "The
// writer"): every state-dependent decision of a command happens inside one
// writer transaction, in this order: idempotency lookup, authorization,
// expected-state check, event append, projection update (resource
// reservations among them, made by the capacity projector from the
// proposed lease events), and the command-result row. Anything that fails
// rolls all of it back.

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
	Subject string `json:"subject"`
	Version int64  `json:"version"`
}

// Tx is the writer transaction as the hooks and the command's logic see it.
// It offers no SQL: every statement is the store's own. Hooks read subject
// versions and read and write projections, all inside the transaction;
// commit and rollback stay with Submit, and events are appended only by
// Submit, through the checked path. A View's Tx reads only.
type Tx struct {
	ctx     context.Context
	tx      *sql.Tx
	store   *Store   // set for a command's transaction
	intents []intent // effects the command intends
	// rebuild: projections go to the rebuild's scratch table instead of
	// the live proj_kv (RebuildProjections).
	rebuild bool
	// readOnly: a View's transaction, on the read-only pool.
	readOnly bool
}

// ErrReadOnly: a View's transaction cannot change projections.
var ErrReadOnly = errors.New("eventstore: a view reads only; projections change inside the writer transaction")

// Projection tables. Table names are never input: they are these constants.
const (
	liveProjections    = "main.proj_kv"
	rebuildProjections = "temp.proj_rebuild"
)

// Replaying reports a rebuild's replay of recorded events, as opposed to the
// admission of new ones: a projector keeps its deterministic fold, and skips
// admission checks that read state outside the log (blob contents, a
// vocabulary that may have changed since).
func (t *Tx) Replaying() bool { return t.rebuild }

// table is the projection table this transaction reads and writes.
func (t *Tx) table() string {
	if t.rebuild {
		return rebuildProjections
	}
	return liveProjections
}

// Projections are derived, rebuildable records (docs/design/m0/03): JSON
// values under a key, grouped by projection name, in the store-owned proj_kv
// table. They change only inside the writer transaction.
var projectionName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// maxProjectionKey bounds a projection key.
const maxProjectionKey = 512

func checkProjection(projection, key string) error {
	if !projectionName.MatchString(projection) {
		return fmt.Errorf("eventstore: projection name %q (lowercase letters, digits and _, starting with a letter)", projection)
	}
	if key == "" || len(key) > maxProjectionKey || !utf8.ValidString(key) || strings.ContainsRune(key, 0) {
		return fmt.Errorf("eventstore: projection key: 1 to %d bytes of UTF-8 without NUL", maxProjectionKey)
	}
	return nil
}

// Get returns the projection record under key, if any.
func (t *Tx) Get(projection, key string) (json.RawMessage, bool, error) {
	if err := checkProjection(projection, key); err != nil {
		return nil, false, err
	}
	var v string
	err := t.tx.QueryRowContext(t.ctx, `SELECT value FROM `+t.table()+` WHERE projection = ? AND key = ?`, projection, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return json.RawMessage(v), true, nil
}

// Put sets the projection record under key to value (stored canonical).
func (t *Tx) Put(projection, key string, value json.RawMessage) error {
	if t.readOnly {
		return ErrReadOnly
	}
	if err := checkProjection(projection, key); err != nil {
		return err
	}
	c, err := canonical(value)
	if err != nil {
		return fmt.Errorf("eventstore: projection %s value: %w", projection, err)
	}
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO `+t.table()+`(projection, key, value) VALUES (?, ?, ?)
		ON CONFLICT(projection, key) DO UPDATE SET value = excluded.value`, projection, key, string(c))
	return err
}

// Delete removes the projection record under key, if any.
func (t *Tx) Delete(projection, key string) error {
	if t.readOnly {
		return ErrReadOnly
	}
	if err := checkProjection(projection, key); err != nil {
		return err
	}
	_, err := t.tx.ExecContext(t.ctx, `DELETE FROM `+t.table()+` WHERE projection = ? AND key = ?`, projection, key)
	return err
}

// Keys lists the projection's keys that start with prefix, sorted.
func (t *Tx) Keys(projection, prefix string) ([]string, error) {
	if !projectionName.MatchString(projection) {
		return nil, fmt.Errorf("eventstore: projection name %q", projection)
	}
	rows, err := t.tx.QueryContext(t.ctx, `SELECT key FROM `+t.table()+` WHERE projection = ? ORDER BY key`, projection)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		if strings.HasPrefix(k, prefix) { // bytes, as Go compares them
			keys = append(keys, k)
		}
	}
	return keys, rows.Err()
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
// (fail closed). Project may be nil.
type Hooks struct {
	Authorize func(t *Tx, c Command) error // internal/authority
	// Project updates projections. It must be a fold: derive its records
	// from the events it is given and the projections alone, whether it
	// gets a command's events at once or one at a time (a rebuild replays
	// them singly), and never from Tx.SubjectVersion.
	Project func(t *Tx, events []Event) error
}

// ProjectEach composes projectors as a rebuild runs them: each event goes
// through every projector, in order, before the next event. Composing them
// batch by batch instead would let a command admit an event against a
// later event of its own batch (a grant before its grantee), which a
// rebuild, replaying singly, would refuse.
func ProjectEach(projectors ...func(t *Tx, events []Event) error) func(t *Tx, events []Event) error {
	return func(t *Tx, events []Event) error {
		for _, e := range events {
			for _, p := range projectors {
				if err := p(t, []Event{e}); err != nil {
					return err
				}
			}
		}
		return nil
	}
}

// Apply is the command's logic: from the current state (read through t) it
// returns the events to append and the command's result (JSON).
type Apply func(t *Tx) (events []Event, result json.RawMessage, err error)

// Outcome is an accepted command: its result and the seqs of its events.
type Outcome struct {
	CommandID         string
	Replayed          bool // a repeat of an accepted command: nothing was run again
	Result            json.RawMessage
	FirstSeq, LastSeq int64    // 0 when the command appended no event
	Effects           []Effect // the command's effects, in their current states (on a replay too)
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
	// The request is the input and the expectations together: a repeat with
	// other expectations is another request. The input itself must be JSON
	// (an absent input is refused, never read as null).
	if _, err := canonical(c.Input); err != nil {
		return Outcome{}, fmt.Errorf("eventstore: command input: %w", err)
	}
	expected := c.Expected
	if expected == nil {
		expected = []Expectation{}
	}
	request, err := json.Marshal(struct {
		Input    json.RawMessage `json:"input"`
		Expected []Expectation   `json:"expected"`
	}{c.Input, expected})
	if err != nil {
		return Outcome{}, fmt.Errorf("eventstore: command request: %w", err)
	}
	inputDigest, err := digest(request)
	if err != nil {
		return Outcome{}, fmt.Errorf("eventstore: command input: %w", err)
	}
	var out Outcome
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		t := &Tx{ctx: ctx, tx: tx, store: s}
		// 1. Idempotency.
		var prev struct {
			id, digest, result string
			first, last        sql.NullInt64
		}
		err := tx.QueryRowContext(ctx, `SELECT id, input_digest, result_json, first_seq, last_seq FROM commands WHERE actor = ? AND idempotency_key = ?`,
			c.Actor, c.IdempotencyKey).Scan(&prev.id, &prev.digest, &prev.result, &prev.first, &prev.last)
		switch {
		case err == nil && prev.digest != inputDigest && !mutation.Is(mutation.IdempotencyIgnoresInput):
			return fmt.Errorf("eventstore: %w: key %q of %s was used for another input", ErrIdempotencyKeyReused, c.IdempotencyKey, c.Actor)
		case err == nil:
			effs, err := commandEffects(ctx, tx, prev.id)
			if err != nil {
				return err
			}
			out = Outcome{CommandID: prev.id, Replayed: true, Result: json.RawMessage(prev.result), FirstSeq: prev.first.Int64, LastSeq: prev.last.Int64, Effects: effs}
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		// 2. Authorization: without an authorizer every new command is
		// refused (a stored result still replays above).
		if s.hooks.Authorize == nil {
			return errors.New("eventstore: no authorizer is configured; every command is refused")
		}
		if err := s.hooks.Authorize(t, c); err != nil {
			return err
		}
		// 3. Expected state.
		for _, e := range c.Expected {
			v, err := t.SubjectVersion(e.Subject)
			if err != nil {
				return err
			}
			if v != e.Version && !mutation.Is(mutation.ExpectedStateUnchecked) {
				return fmt.Errorf("eventstore: %w: %s is at %d, the command expected %d", ErrStaleExpectedState, e.Subject, v, e.Version)
			}
		}
		// 4. Events.
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
			// Effect events are the store's own: only IntendEffect and the
			// transitions write them, so the effects table always agrees.
			if isEffectKind(e.Kind) {
				return fmt.Errorf("eventstore: %s is written only by the store's effect operations", e.Kind)
			}
			// A command's events are its actor's: authority decided for
			// that actor, and projectors take the event's actor as who did it.
			if e.Actor != "" && e.Actor != c.Actor {
				return fmt.Errorf("eventstore: event %s names actor %s, not the command's %s", e.ID, e.Actor, c.Actor)
			}
			e.Actor = c.Actor
			cmd := c.ID
			e.Command = &cmd
			a, err := s.append(ctx, tx, e)
			if err != nil {
				return err
			}
			appended = append(appended, a)
		}
		// Effect intents follow the command's events, in the same transaction.
		intents, err := s.appendIntents(ctx, tx, c, t.intents)
		if err != nil {
			return err
		}
		appended = append(appended, intents...)
		var first, last sql.NullInt64
		if len(appended) > 0 {
			first = sql.NullInt64{Int64: appended[0].Seq, Valid: true}
			last = sql.NullInt64{Int64: appended[len(appended)-1].Seq, Valid: true}
		}
		// 5. Projections, reservations among them (on a copy: a hook cannot
		// change what is recorded).
		if s.hooks.Project != nil && len(appended) > 0 {
			if err := s.hooks.Project(t, slices.Clone(appended)); err != nil {
				return err
			}
		}
		// 6. The command-result row.
		if _, err := tx.ExecContext(ctx, `INSERT INTO commands(id, actor, idempotency_key, input_digest, disposition, result_json, first_seq, last_seq)
			VALUES (?, ?, ?, ?, 'applied', ?, ?, ?)`, c.ID, c.Actor, c.IdempotencyKey, inputDigest, string(canonResult), first, last); err != nil {
			return err
		}
		effs, err := commandEffects(ctx, tx, c.ID)
		if err != nil {
			return err
		}
		out = Outcome{CommandID: c.ID, Result: canonResult, FirstSeq: first.Int64, LastSeq: last.Int64, Effects: effs}
		return nil
	})
	if err != nil {
		return Outcome{}, err
	}
	return out, nil
}
