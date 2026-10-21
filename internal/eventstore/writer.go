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
	"sync/atomic"
	"time"
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
	// Expect is the request's expected state of the object its handler
	// derives (Tx.BindObject); nil: the request expects none.
	Expect *State
	// Principal is who sent the command, as its transport authenticated
	// it; Hooks.Identify rechecks it before anything else.
	Principal Principal
}

// ErrSystemClaimed: the store's system claim was already handed out.
var ErrSystemClaimed = errors.New("eventstore: the system claim of this store is already held")

// SystemClaim is the capability to act as the controller's own system
// actor on one store (14 N4): identity makes the system principal only
// from it. ClaimSystem hands it out once per opened store, and the daemon
// claims it at startup, right after Open, so no later caller can. The zero
// claim holds nothing.
type SystemClaim struct{ s *Store }

// ClaimSystem returns the store's system claim, the first time only.
func (s *Store) ClaimSystem() (SystemClaim, error) {
	if !s.systemClaimed.CompareAndSwap(false, true) {
		return SystemClaim{}, ErrSystemClaimed
	}
	return SystemClaim{s}, nil
}

// Holds reports whether c is the claim of the store t is a transaction on.
func (c SystemClaim) Holds(t *Tx) bool { return c.s != nil && t != nil && t.owner == c.s }

// Principal is a command's authenticated sender. Only internal/identity
// makes one the daemon's Identify hook accepts; the store itself never
// trusts it.
type Principal interface{ PrincipalActor() string }

// Expectation: the subject's last event is seq Version (0: it has none).
type Expectation struct {
	Subject string `json:"subject"`
	Version int64  `json:"version"`
}

// State is what a request expects of the object its command changes
// (CommandRequest.expected, 14 P3): its version (the seq of its last event;
// 0: it does not exist), its content digest (ContentDigest of its current
// record), or both. Every one supplied is enforced.
type State struct {
	Version *int64  `json:"version"`
	Digest  *string `json:"digest"`
}

// Object is a command's authoritative object, as its handler derives it.
type Object struct {
	Subject string
	// Record is the object's current record and whether it exists: the
	// authority on its existence, and its digest definition. Nil: the
	// object has none, and any expected state of it is refused.
	Record func(t *Tx) (json.RawMessage, bool, error)
	// Version is the object's version, for an object whose record changes
	// by events of other subjects too (the seat, when its holder's actor is
	// revoked): the seq of the last event that changed it. Nil: the seq of
	// its subject's last event (SubjectVersion).
	Version func(t *Tx) (int64, error)
}

// BindObject records the command's object, as its Authorize hook derived
// it, for the writer's expected-state check: one derivation serves both.
// A command binds at most one object.
func (t *Tx) BindObject(o Object) error {
	switch {
	case t.store == nil || t.readOnly || t.rebuild:
		return errors.New("eventstore: only a command's transaction binds an object")
	case t.object != nil:
		return errors.New("eventstore: the command's object is already bound")
	case o.Subject == "":
		return errors.New("eventstore: an object needs a subject")
	}
	t.object = &o
	return nil
}

// ContentDigest is a record's content digest: sha256 of its canonical
// (RFC 8785) bytes, so neither field order nor spacing changes it.
//
// It refuses any number that is not an integer within ±(2^53−1) and
// trailing data, but not duplicate keys or bad escapes: a caller digesting
// untrusted bytes reads them strictly first (contract.ReadStrict).
func ContentDigest(record json.RawMessage) (string, error) { return digest(record) }

// Canonical is record's RFC 8785 form: the bytes a kept request's blob
// holds (03 "Kept requests"), whose digest is ContentDigest's.
func Canonical(record json.RawMessage) ([]byte, error) { return canonical(record) }

// Tx is the writer transaction as the hooks and the command's logic see it.
// It offers no SQL: every statement is the store's own. Hooks read subject
// versions and read and write projections, all inside the transaction;
// commit and rollback stay with Submit, and events are appended only by
// Submit, through the checked path. A View's Tx reads only.
type Tx struct {
	ctx     context.Context
	tx      *sql.Tx
	store   *Store   // set for a command's transaction
	owner   *Store   // the store a command's or a View's transaction is on
	intents []intent // effects the command intends
	emitted []Event  // events the logic recorded itself (Emit)
	logic   bool     // the command's logic is running: only it emits
	object  *Object  // the command's object, as Authorize bound it
	// decided: the command's decision, when Authorize made it (Tx.Decided).
	decided *Decision
	// rebuild: projections go to the rebuild's scratch table instead of
	// the live proj_kv (RebuildProjections).
	rebuild bool
	// readOnly: a View's transaction, on the read-only pool.
	readOnly bool
	// fault is the first storage error a read or write met: a projector
	// that fails on it has not refused the log (RebuildProjections).
	fault error
	// at is when a command's events are recorded (Tx.Now).
	at time.Time
	// action is the action of the command being submitted (Tx.Action).
	action string
}

// Action is the action of the command being submitted, in its transaction's hooks and logic; "" in a view, a rebuild
// or an effect transition. The log does not record a command's action, so a fold that checks it checks at commit
// only.
func (t *Tx) Action() string { return t.action }

// Emit records evs as the command's own events, ahead of those its logic
// returns, so a logic can write events it never hands back to a caller
// (graph.Commit: 13 [G4]). Emitted events always precede the returned ones,
// whatever the order of the logic's calls. Only a command's logic emits,
// while it runs: not a hook, before or after it, and not a view or a
// rebuild. Only an applied command records what it emitted.
func (t *Tx) Emit(evs ...Event) error {
	if t.store == nil || t.readOnly || t.rebuild || !t.logic {
		return errors.New("eventstore: only a command's logic emits events")
	}
	t.emitted = append(t.emitted, evs...)
	return nil
}

// Now is the command's one clock reading: every time decision in the
// command (authorization, grant validity, proposal creation, the logic's
// times) reads it, and every event the command appends is recorded at it
// (Event.At). It is the store clock (Config.Now) read once when the
// transaction begins, or the previous event's time when the clock trails
// it. Projectors read Event.At (a rebuild has no command time); outside a
// command it is the zero time.
func (t *Tx) Now() time.Time { return t.at }

// Bound is the subject of the object Authorize bound for the command
// (BindObject), if one was bound: the logic writes under the same id.
func (t *Tx) Bound() (string, bool) {
	if t.object == nil {
		return "", false
	}
	return t.object.Subject, true
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
	projectionRows.Add(1)
	err := t.tx.QueryRowContext(t.ctx, `SELECT value FROM `+t.table()+` WHERE projection = ? AND key = ?`, projection, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, t.io(err)
	}
	return json.RawMessage(v), true, nil
}

// Size is the byte length of the projection record under key, if any,
// read without reading the record (a bound checked before a Get).
func (t *Tx) Size(projection, key string) (int64, bool, error) {
	if err := checkProjection(projection, key); err != nil {
		return 0, false, err
	}
	var n int64
	projectionRows.Add(1)
	err := t.tx.QueryRowContext(t.ctx, `SELECT length(CAST(value AS BLOB)) FROM `+t.table()+` WHERE projection = ? AND key = ?`, projection, key).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, t.io(err)
	}
	return n, true, nil
}

// KeysAtMost is Keys for a non-empty prefix, at most n of them (Astra #321
// M5): the prefix's range, read with a LIMIT, so a caller stops at its
// budget.
func (t *Tx) KeysAtMost(projection, prefix string, n int) ([]string, error) {
	if err := checkProjection(projection, prefix); err != nil || n < 0 {
		return nil, errors.Join(err, fmt.Errorf("eventstore: keys at most %d", n))
	}
	return t.keys(projection, prefix, n)
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

// Keys lists the projection's keys that start with prefix, sorted. Only
// the keys in the prefix's range are read (the primary key's order), so a
// prefix bounds the work (Astra #316 M3).
func (t *Tx) Keys(projection, prefix string) ([]string, error) {
	return t.keys(projection, prefix, -1)
}

// FirstKey is the projection's first key that starts with prefix, in
// order; false: none. It reads one key.
func (t *Tx) FirstKey(projection, prefix string) (string, bool, error) {
	keys, err := t.keys(projection, prefix, 1)
	if err != nil || len(keys) == 0 {
		return "", false, err
	}
	return keys[0], true, nil
}

// keys are the projection's keys in prefix's range, at most limit (-1: no
// limit).
func (t *Tx) keys(projection, prefix string, limit int) ([]string, error) {
	if !projectionName.MatchString(projection) {
		return nil, fmt.Errorf("eventstore: projection name %q", projection)
	}
	q, args := `SELECT key FROM `+t.table()+` WHERE projection = ? AND key >= ?`, []any{projection, prefix}
	if end, ok := prefixEnd(prefix); ok {
		q, args = q+` AND key < ?`, append(args, end)
	}
	rows, err := t.tx.QueryContext(t.ctx, q+` ORDER BY key LIMIT ?`, append(args, limit)...)
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
		projectionRows.Add(1)
		if strings.HasPrefix(k, prefix) { // bytes, as Go compares them
			keys = append(keys, k)
		}
	}
	return keys, t.io(rows.Err())
}

// prefixEnd is the least string after every one that starts with prefix
// (its last byte below 0xff incremented, the bytes after it dropped);
// false: no such bound (an empty prefix, or one of 0xff bytes).
func prefixEnd(prefix string) (string, bool) {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1]), true
		}
	}
	return "", false
}

// projectionRows counts the projection rows every read returns (Get's one,
// each key Keys reads), for tests that count work rather than time it.
var projectionRows atomic.Int64

// ProjectionRowsRead is how many projection rows reads have returned in
// this process.
func ProjectionRowsRead() int64 { return projectionRows.Load() }

// SubjectVersion is the seq of the subject's last event, or 0.
func (t *Tx) SubjectVersion(subject string) (int64, error) {
	var v sql.NullInt64
	if err := t.tx.QueryRowContext(t.ctx, `SELECT max(seq) FROM events WHERE subject = ?`, subject).Scan(&v); err != nil {
		return 0, t.io(err)
	}
	return v.Int64, nil
}

// HasCommand reports whether actor has a command stored under key, so a
// request layer can skip work a replay would discard (14 N3). Advisory: the
// writer's own lookup decides.
func (t *Tx) HasCommand(actor, key string) (bool, error) {
	var n int
	if err := t.tx.QueryRowContext(t.ctx, `SELECT count(*) FROM commands WHERE actor = ? AND idempotency_key = ?`, actor, key).Scan(&n); err != nil {
		return false, t.io(err)
	}
	return n > 0, nil
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
	// (Authorize also binds the command's object, Tx.BindObject; a command
	// expecting a state of an unbound object is refused.)
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
	if x := c.Expect; x != nil && (x.Version == nil && x.Digest == nil || x.Version != nil && *x.Version < 0) {
		return Committed{}, errors.New("eventstore: an expected state is a version (0 or more), a digest or both")
	}
	expected := c.Expected
	if expected == nil {
		expected = []Expectation{}
	}
	request, err := json.Marshal(struct {
		Action   string          `json:"action"`
		Input    json.RawMessage `json:"input"`
		Expected []Expectation   `json:"expected"`
		Expect   *State          `json:"expect,omitempty"` // absent: a request without one keeps its digest
	}{c.Action, c.Input, expected, c.Expect})
	if err != nil {
		return Committed{}, fmt.Errorf("eventstore: command request: %w", err)
	}
	inputDigest, err := digest(request)
	if err != nil {
		return Committed{}, fmt.Errorf("eventstore: command input: %w", err)
	}
	var out Committed
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		at, aterr := s.headTime(ctx, tx)
		if aterr != nil {
			return aterr
		}
		t := &Tx{ctx: ctx, tx: tx, store: s, owner: s, at: at, action: c.Action}
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
		if c.Expect != nil {
			if err := checkExpect(t, c.Expect); err != nil {
				return err
			}
		}
		// 4. The decision and its events.
		d, err := t.decision(decide)
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
		if d.Disposition != DispositionApplied && len(t.emitted) > 0 {
			return fmt.Errorf("eventstore: a %s command emits no event", d.Disposition)
		}
		events := append(slices.Clone(t.emitted), d.Events...)
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
			e.Actor, e.At = c.Actor, t.at.Format(time.RFC3339Nano)
			cmd := c.ID
			e.Command = &cmd
			a, err := s.append(ctx, tx, e)
			if err != nil {
				return err
			}
			appended = append(appended, a)
		}
		// Effect intents follow the command's events, in the same transaction.
		intents, err := s.appendIntents(ctx, tx, c, t.at.Format(time.RFC3339Nano), t.intents)
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

// checkExpect enforces every state x a command expects of the object its
// Authorize hook bound, before the command's logic runs. Existence is the
// object's record's: a missing record matches only version 0 without a
// digest, a present one never version 0. Against anything else it is
// stale, not an error.
func checkExpect(t *Tx, x *State) error {
	o := t.object
	if o == nil {
		return errors.New("eventstore: no object was bound for the command; its expected state is refused")
	}
	if o.Record == nil {
		return fmt.Errorf("eventstore: %s has no record definition; an expected state of it is refused", o.Subject)
	}
	rec, exists, err := o.Record(t)
	if err != nil {
		return err
	}
	if mutation.Is(mutation.ExpectedStateUnchecked) {
		return nil
	}
	if !exists {
		if x.Version != nil && *x.Version == 0 && x.Digest == nil {
			return nil
		}
		return fmt.Errorf("eventstore: %w: %s does not exist", ErrStaleExpectedState, o.Subject)
	}
	if x.Version != nil {
		version := o.Version
		if version == nil {
			version = func(t *Tx) (int64, error) { return t.SubjectVersion(o.Subject) }
		}
		v, err := version(t)
		if err != nil {
			return err
		}
		if *x.Version == 0 || v != *x.Version {
			return fmt.Errorf("eventstore: %w: %s exists at %d, the command expected %d", ErrStaleExpectedState, o.Subject, v, *x.Version)
		}
	}
	if x.Digest != nil {
		got, err := ContentDigest(rec)
		if err != nil {
			return fmt.Errorf("eventstore: %s record: %w", o.Subject, err)
		}
		if got != *x.Digest {
			return fmt.Errorf("eventstore: %w: %s is at %s, the command expected %s", ErrStaleExpectedState, o.Subject, got, *x.Digest)
		}
	}
	return nil
}
