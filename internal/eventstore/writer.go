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
// writer transaction, in this order: the identity recheck, idempotency
// lookup, authorization, expected-state check, event append, projection
// update (resource reservations among them, made by the capacity projector
// from the proposed lease events), and the command-result row. Anything that fails
// rolls all of it back.

// Refusals a command can meet (failed/<reason>).
var (
	ErrIdempotencyKeyReused = errors.New("idempotency_key_reused")
	ErrStaleExpectedState   = errors.New("stale_expected_state")
)

// maxIdempotencyKey bounds an idempotency key.
const maxIdempotencyKey = 256

// actionName is a canonical action: lowercase dotted words, like grant.create.
var actionName = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// maxAction bounds an action name.
const maxAction = 128

// Command is one request to change state.
type Command struct {
	ID             string        // cmd_…
	Actor          string        // act_…
	Action         string        // the canonical action, like grant.create
	IdempotencyKey string        // unique per actor; a repeat replays or is refused
	Input          []byte        // the request as JSON
	Expected       []Expectation // object versions the command was decided against
	// Principal is who sent the command, as its transport authenticated
	// it; Hooks.Identify rechecks it before anything else.
	Principal Principal
}

// Principal is a command's authenticated sender. Only internal/identity
// makes one the daemon's Identify hook accepts; the store itself never
// trusts it.
type Principal interface{ PrincipalActor() string }

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
	// fault is the first storage error a read or write met: a projector
	// that fails on it has not refused the log (RebuildProjections).
	fault error
}

// io records a storage error as the transaction's fault and returns it.
func (t *Tx) io(err error) error {
	if err != nil && t.fault == nil {
		t.fault = err
	}
	return err
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
		return nil, false, t.io(err)
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
	return t.io(err)
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
	return t.io(err)
}

// Keys lists the projection's keys that start with prefix, sorted.
func (t *Tx) Keys(projection, prefix string) ([]string, error) {
	if !projectionName.MatchString(projection) {
		return nil, fmt.Errorf("eventstore: projection name %q", projection)
	}
	rows, err := t.tx.QueryContext(t.ctx, `SELECT key FROM `+t.table()+` WHERE projection = ? ORDER BY key`, projection)
	if err != nil {
		return nil, t.io(err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, t.io(err)
		}
		if strings.HasPrefix(k, prefix) { // bytes, as Go compares them
			keys = append(keys, k)
		}
	}
	return keys, t.io(rows.Err())
}

// SubjectVersion is the seq of the subject's last event, or 0.
func (t *Tx) SubjectVersion(subject string) (int64, error) {
	var v sql.NullInt64
	if err := t.tx.QueryRowContext(t.ctx, `SELECT max(seq) FROM events WHERE subject = ?`, subject).Scan(&v); err != nil {
		return 0, t.io(err)
	}
	return v.Int64, nil
}

// Hooks are the decisions other packages make inside the writer
// transaction. Identify and Authorize are required: with either missing,
// every command is refused (fail closed); Identify's refusal covers a
// replay too. Project may be nil.
type Hooks struct {
	// Identify rechecks the command's principal (internal/identity): its
	// token and actor still live and its actor the command's, so a stored
	// result is replayed only to its own, still-authenticated actor.
	Identify  func(t *Tx, c Command) error
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

// Apply is logic that only ever applies: from the current state (read
// through t) it returns the events to append and the command's result
// (JSON). Applies makes it a Decide.
type Apply func(t *Tx) (events []Event, result json.RawMessage, err error)

// Outcome is an applied command: its result and the seqs of its events.
// Only Committed.Applied makes one.
type Outcome struct {
	CommandID         string
	Replayed          bool // a repeat of an accepted command: nothing was run again
	Result            json.RawMessage
	FirstSeq, LastSeq int64    // 0 when the command appended no event
	Effects           []Effect // the command's effects, in their current states (on a replay too)
}

// Submit runs c through the writer transaction and returns what it
// committed, whatever the disposition; SubmitApplied is for callers that go
// on only when it applied.
func (s *Store) Submit(ctx context.Context, c Command, decide Decide) (Committed, error) {
	return s.submit(ctx, c, decide, true)
}

// ErrNotBootstrap: Bootstrap was asked for anything but an empty store's
// first event, written by the actor it creates.
var ErrNotBootstrap = errors.New("eventstore: bootstrap writes only an empty store's first event, by its own subject")

// Bootstrap appends the first event of an empty store without the
// Authorize hook: the controller's own initialization (the system actor
// creating itself, before any request can reach the writer). It is refused
// unless the store holds no event, command or effect, and e's subject is the
// command's actor. Validation, the append and the projections run as in
// Submit, so the projectors still decide whether the event is admissible.
func (s *Store) Bootstrap(ctx context.Context, c Command, e Event) (Outcome, error) {
	if e.Subject != c.Actor {
		return Outcome{}, ErrNotBootstrap
	}
	res, err := s.submit(ctx, c, Applies(func(t *Tx) ([]Event, json.RawMessage, error) {
		var used bool
		if err := t.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events) OR EXISTS(SELECT 1 FROM commands) OR EXISTS(SELECT 1 FROM effects)`).Scan(&used); err != nil {
			return nil, nil, err
		}
		if used || len(t.intents) > 0 {
			return nil, nil, ErrNotBootstrap
		}
		return []Event{e}, json.RawMessage(`{}`), nil
	}), false)
	if err != nil {
		return Outcome{}, err
	}
	return res.Applied()
}

func (s *Store) submit(ctx context.Context, c Command, decide Decide, authorize bool) (Committed, error) {
	if err := ids.Check(c.ID, "cmd"); err != nil {
		return Committed{}, fmt.Errorf("eventstore: command: %w", err)
	}
	if err := ids.Check(c.Actor, "act"); err != nil {
		return Committed{}, fmt.Errorf("eventstore: command actor: %w", err)
	}
	if len(c.Action) > maxAction || !actionName.MatchString(c.Action) {
		return Committed{}, fmt.Errorf("eventstore: command action %q (lowercase dotted words, at most %d bytes)", c.Action, maxAction)
	}
	if c.IdempotencyKey == "" || len(c.IdempotencyKey) > maxIdempotencyKey {
		return Committed{}, fmt.Errorf("eventstore: an idempotency key is 1 to %d bytes", maxIdempotencyKey)
	}
	// The request is the action, the input and the expectations together
	// (14 N1): a repeat of the key with another action, input or
	// expectation is another request. The input itself must be JSON (an
	// absent input is refused, never read as null).
	if _, err := canonical(c.Input); err != nil {
		return Committed{}, fmt.Errorf("eventstore: command input: %w", err)
	}
	expected := c.Expected
	if expected == nil {
		expected = []Expectation{}
	}
	request, err := json.Marshal(struct {
		Action   string          `json:"action"`
		Input    json.RawMessage `json:"input"`
		Expected []Expectation   `json:"expected"`
	}{c.Action, c.Input, expected})
	if err != nil {
		return Committed{}, fmt.Errorf("eventstore: command request: %w", err)
	}
	inputDigest, err := digest(request)
	if err != nil {
		return Committed{}, fmt.Errorf("eventstore: command input: %w", err)
	}
	var out Committed
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		t := &Tx{ctx: ctx, tx: tx, store: s}
		// 0. The writer-boundary identity recheck (14 "Replay
		// authentication"), before the idempotency lookup, so it covers new
		// commands, reused keys and replays alike. Bootstrap (authorize
		// false) is exempt: it runs only on an empty store, before any
		// identity exists.
		if authorize && s.hooks.Identify == nil {
			return errors.New("eventstore: no identity check is configured; every command is refused")
		}
		if authorize {
			if err := s.hooks.Identify(t, c); err != nil {
				return err
			}
		}
		// 1. Idempotency.
		var prev struct {
			id, digest, disposition, result string
			first, last                     sql.NullInt64
		}
		err := tx.QueryRowContext(ctx, `SELECT id, input_digest, disposition, result_json, first_seq, last_seq FROM commands WHERE actor = ? AND idempotency_key = ?`,
			c.Actor, c.IdempotencyKey).Scan(&prev.id, &prev.digest, &prev.disposition, &prev.result, &prev.first, &prev.last)
		switch {
		case err == nil && prev.digest != inputDigest && !mutation.Is(mutation.IdempotencyIgnoresInput):
			return fmt.Errorf("eventstore: %w: key %q of %s was used for another request", ErrIdempotencyKeyReused, c.IdempotencyKey, c.Actor)
		case err == nil:
			d := Disposition(prev.disposition)
			if !d.known() {
				return fmt.Errorf("eventstore: command %s has an unknown disposition %q", prev.id, prev.disposition)
			}
			effs, err := commandEffects(ctx, tx, prev.id)
			if err != nil {
				return err
			}
			// A replay carries no credential: none is stored, none is minted.
			out = Committed{Disposition: d, CommandID: prev.id, Replayed: true, Result: json.RawMessage(prev.result), FirstSeq: prev.first.Int64, LastSeq: prev.last.Int64, Effects: effs}
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		// 2. Authorization: without an authorizer every new command is
		// refused (a stored result still replays above).
		if authorize && s.hooks.Authorize == nil {
			return errors.New("eventstore: no authorizer is configured; every command is refused")
		}
		if authorize {
			if err := s.hooks.Authorize(t, c); err != nil {
				return err
			}
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
		// 4. The decision and its events.
		d, err := decide(t)
		if err != nil {
			return err
		}
		// A decision made over a storage fault is not committed, whatever
		// its disposition.
		if t.fault != nil {
			return fmt.Errorf("eventstore: the command was decided over a storage fault: %w", t.fault)
		}
		canonResult, err := canonical(d.Result)
		if err != nil {
			return fmt.Errorf("eventstore: command result: %w", err)
		}
		if err := checkDecision(d, canonResult, len(t.intents)); err != nil {
			return err
		}
		appended := make([]Event, 0, len(d.Events))
		for _, e := range d.Events {
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
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, c.ID, c.Actor, c.IdempotencyKey, inputDigest, string(d.Disposition), string(canonResult), first, last); err != nil {
			return err
		}
		effs, err := commandEffects(ctx, tx, c.ID)
		if err != nil {
			return err
		}
		out = Committed{Disposition: d.Disposition, CommandID: c.ID, Result: canonResult, FirstSeq: first.Int64, LastSeq: last.Int64, Effects: effs}
		return nil
	})
	if err != nil {
		return Committed{}, err
	}
	return out, nil
}
