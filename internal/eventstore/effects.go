// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// External effects (docs/design/m0/03 "External effects"): every effect
// (model call, forge push or merge, deploy, notification) has a durable
// identity before it happens. A command declares the intent; the intent
// commits with the command, and only then may the effect start. Every later
// transition is an event too, and the effects table holds each effect's
// current state, changed only with its event, in the same transaction.

// EffectState is where an effect stands.
type EffectState string

// The effect states.
const (
	EffectPending         EffectState = "pending"
	EffectCompleted       EffectState = "completed"
	EffectUncertain       EffectState = "uncertain"
	EffectFailedConfirmed EffectState = "failed_confirmed"
)

// EffectKind declares how an effect kind recovers: an idempotent kind is
// safe to retry with the same effect id; any other is reconciled first and
// never blindly retried. A kind that reconciles from its request (12
// remote_process) keeps it: its request_digest references a blob, which the
// caller publishes, in its RFC 8785 form, before it intends the effect.
// A kind with a RequestSchema has its request validated against it, and a
// Check refuses what the schema cannot (the request against the target).
// Check runs inside the writer transaction: it treats the request bytes as
// immutable, is deterministic and free of side effects, and does no
// blocking I/O and no write, to the store or anything else.
type EffectKind struct {
	Idempotent, RequestBlob bool
	RequestSchema           string
	Check                   func(target string, request json.RawMessage) error
}

// EffectKinds are the effect kinds the controller may intend, by name.
type EffectKinds map[string]EffectKind

// Effect is an effect's durable identity and current state.
type Effect struct {
	ID              string      `json:"id"`
	Kind            string      `json:"kind"`
	Command         string      `json:"command"`
	Target          string      `json:"target"`
	RequestDigest   string      `json:"request_digest"`
	State           EffectState `json:"state"`
	Attempts        int64       `json:"attempts"`
	LastObservation string      `json:"last_observation"`
	ResultRef       *string     `json:"result_ref"`
}

// The effect event kinds, and the schema of their payload.
const effectSchema = "records/effect-event"

var effectEventKinds = []string{"effect.intent/v1", "effect.observed/v1", "effect.failed/v1", "effect.uncertain/v1", "effect.retried/v1", "effect.reconciled/v1"}

// isEffectKind reports a store-owned effect event kind.
func isEffectKind(kind string) bool { return slices.Contains(effectEventKinds, kind) }

// ErrEffectTransition: the effect is not in a state that allows the change.
var ErrEffectTransition = errors.New("eventstore: effect transition not allowed")

type intent struct {
	id, kind, target, digest string
}

// IntendEffect declares an effect of the command being submitted. Its
// intent is appended after the command's events and commits with them; the
// effect may start only after Submit returns. Only a command's Apply may
// call it.
func (t *Tx) IntendEffect(id, kind, target string, request json.RawMessage) error {
	if t.store == nil {
		return errors.New("eventstore: an effect is intended only by a command")
	}
	if err := ids.Check(id, "eff"); err != nil {
		return fmt.Errorf("eventstore: effect: %w", err)
	}
	ek, ok := t.store.effects[kind]
	if !ok {
		return fmt.Errorf("eventstore: unregistered effect kind %q", kind)
	}
	if target == "" || !utf8.ValidString(target) {
		return errors.New("eventstore: an effect names its target (valid UTF-8)")
	}
	d, err := digest(request)
	if err != nil {
		return fmt.Errorf("eventstore: effect %s request: %w", id, err)
	}
	if ek.RequestSchema != "" {
		if err := contract.Validate(ek.RequestSchema, request); err != nil {
			return fmt.Errorf("eventstore: effect %s request: %w", id, err)
		}
	}
	if ek.Check != nil {
		if err := ek.Check(target, request); err != nil {
			return fmt.Errorf("eventstore: effect %s request: %w", id, err)
		}
	}
	if ek.RequestBlob {
		if kept, err := t.store.blobs.Has(d); err != nil || !kept {
			return errors.Join(fmt.Errorf("eventstore: effect %s request %s is not a published blob", id, d), err)
		}
	}
	t.intents = append(t.intents, intent{id, kind, target, d})
	return nil
}

// ErrNoEffect: no effect is recorded under the id asked for.
var ErrNoEffect = errors.New("eventstore: no effect")

// ErrAmbiguousEffect: two recorded effects where one is allowed (rule 2:
// could_not_run/ambiguous_state, never a pick).
var ErrAmbiguousEffect = errors.New("ambiguous_state")

// EffectFor is the effect of kind intended for request, if any: one this
// command intends, or one recorded. An operation resumes through it, so a
// crash after the intent never allocates another effect (13 [G4]).
func (t *Tx) EffectFor(kind string, request json.RawMessage) (Effect, bool, error) {
	d, err := digest(request)
	if err != nil {
		return Effect{}, false, fmt.Errorf("eventstore: effect request: %w", err)
	}
	return t.oneEffect(func(in intent) bool { return in.kind == kind && in.digest == d },
		`kind = ? AND request_digest = ?`, kind, d)
}

// LiveEffect is the effect of kind on target that is pending or uncertain,
// if any: one this command intends, or one recorded. An operation on a
// target waits while another is live (13 [G4]).
func (t *Tx) LiveEffect(kind, target string) (Effect, bool, error) {
	return t.oneEffect(func(in intent) bool { return in.kind == kind && in.target == target },
		`kind = ? AND target = ? AND state IN ('pending', 'uncertain')`, kind, target)
}

// LiveEffects are the recorded effects of kind that are pending or
// uncertain, by id: what a reconciler of the kind settles.
func (t *Tx) LiveEffects(kind string) ([]Effect, error) {
	rows, err := t.tx.QueryContext(t.ctx, `SELECT id FROM effects WHERE kind = ? AND state IN ('pending', 'uncertain') ORDER BY id`, kind)
	if err != nil {
		return nil, t.io(err)
	}
	var live []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, t.io(err)
		}
		live = append(live, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, t.io(err)
	}
	out := make([]Effect, 0, len(live))
	for _, id := range live {
		e, err := loadEffect(t.ctx, t.tx, id)
		if err != nil {
			return nil, t.io(err)
		}
		out = append(out, e)
	}
	return out, nil
}

// oneEffect is the effect this command intends that match selects, or the
// one recorded effect the where clause (a constant) selects; two recorded
// are ErrAmbiguousEffect.
func (t *Tx) oneEffect(match func(intent) bool, where string, args ...any) (Effect, bool, error) {
	for _, in := range t.intents {
		if match(in) {
			return Effect{ID: in.id, Kind: in.kind, Target: in.target, RequestDigest: in.digest, State: EffectPending, Attempts: 1}, true, nil
		}
	}
	var n int
	var id sql.NullString
	err := t.tx.QueryRowContext(t.ctx, `SELECT count(*), min(id) FROM effects WHERE `+where, args...).Scan(&n, &id)
	switch {
	case err != nil:
		return Effect{}, false, t.io(err)
	case n > 1:
		return Effect{}, false, fmt.Errorf("eventstore: %w: %d effects where one is allowed (%v)", ErrAmbiguousEffect, n, args)
	case n == 0:
		return Effect{}, false, nil
	}
	e, err := loadEffect(t.ctx, t.tx, id.String)
	return e, err == nil, t.io(err)
}

// LoadEffect is an effect's current state, read in the transaction.
func (t *Tx) LoadEffect(id string) (Effect, error) { return loadEffect(t.ctx, t.tx, id) }

// EffectKind is how a registered effect kind recovers.
func (s *Store) EffectKind(kind string) (EffectKind, bool) {
	k, ok := s.effects[kind]
	return k, ok
}

// effectPayload is the canonical payload of an effect event.
func effectPayload(id, transition string, kind, target, digest, outcome *string, observation string, resultRef *string) ([]byte, error) {
	return json.Marshal(struct {
		Effect        string  `json:"effect"`
		Transition    string  `json:"transition"`
		Kind          *string `json:"kind"`
		Target        *string `json:"target"`
		RequestDigest *string `json:"request_digest"`
		Outcome       *string `json:"outcome"`
		Observation   string  `json:"observation"`
		ResultRef     *string `json:"result_ref"`
	}{id, transition, kind, target, digest, outcome, observation, resultRef})
}

// appendIntents records the command's effect intents: an event and a
// pending effect each, recorded at the command's time (Tx.Now). It returns
// the events.
func (s *Store) appendIntents(ctx context.Context, tx *sql.Tx, c Command, at string, intents []intent) ([]Event, error) {
	var out []Event
	for _, in := range intents {
		p, err := effectPayload(in.id, "intent", &in.kind, &in.target, &in.digest, nil, "", nil)
		if err != nil {
			return nil, err
		}
		cmd := c.ID
		evID, err := ids.New("evt")
		if err != nil {
			return nil, err
		}
		e, err := s.append(ctx, tx, Event{ID: evID, At: at, Kind: "effect.intent/v1", Subject: in.id, Actor: c.Actor, Command: &cmd, Payload: p})
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO effects(id, kind, command, target, request_digest, state, attempts, last_observation, result_ref)
			VALUES (?, ?, ?, ?, ?, 'pending', 1, '', NULL)`, in.id, in.kind, c.ID, in.target, in.digest); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// Transition is a change to an existing effect.
type Transition struct {
	Kind        string  // "observed", "failed", "uncertain", "retried" or "reconciled"
	Outcome     string  // for "reconciled": "completed", "failed_confirmed" or "still_uncertain"
	Observation string  // what was observed
	ResultRef   *string // a published blob with the effect's result, if any
	// Check, if set, runs in the transition's transaction before it, reading the projections as they are then; an
	// error refuses the transition. It writes nothing.
	Check func(*Tx) error
}

// next is the state machine of note 03: the state a transition leads to
// from the current one, or false.
func next(from EffectState, tr Transition, kind EffectKind) (EffectState, bool) {
	switch {
	case from == EffectPending && tr.Kind == "observed":
		return EffectCompleted, true
	case from == EffectPending && tr.Kind == "failed":
		return EffectFailedConfirmed, true
	case from == EffectPending && tr.Kind == "uncertain":
		return EffectUncertain, true
	case from == EffectUncertain && tr.Kind == "retried" && kind.Idempotent:
		return EffectPending, true
	case from == EffectUncertain && tr.Kind == "reconciled":
		switch tr.Outcome {
		case "completed":
			return EffectCompleted, true
		case "failed_confirmed":
			return EffectFailedConfirmed, true
		case "still_uncertain":
			return EffectUncertain, true
		}
	}
	return "", false
}

// RecordEffect applies a transition to an effect, by actor, in one writer
// transaction: the event and the new state together.
func (s *Store) RecordEffect(ctx context.Context, actor, id string, tr Transition) (Effect, error) {
	if err := ids.Check(actor, "act"); err != nil {
		return Effect{}, fmt.Errorf("eventstore: actor: %w", err)
	}
	if !utf8.ValidString(tr.Observation) {
		return Effect{}, errors.New("eventstore: an observation is valid UTF-8")
	}
	var out Effect
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if tr.Check != nil {
			if err := tr.Check(&Tx{ctx: ctx, tx: tx, readOnly: true, owner: s}); err != nil {
				return err
			}
		}
		var err error
		out, err = s.transition(ctx, tx, actor, id, tr)
		return err
	})
	return out, err
}

func (s *Store) transition(ctx context.Context, tx *sql.Tx, actor, id string, tr Transition) (Effect, error) {
	e, err := loadEffect(ctx, tx, id)
	if err != nil {
		return Effect{}, err
	}
	to, ok := next(e.State, tr, s.effects[e.Kind])
	if !ok {
		return Effect{}, fmt.Errorf("%w: %s is %s; %s%s", ErrEffectTransition, id, e.State, tr.Kind, map[bool]string{true: " (" + tr.Outcome + ")", false: ""}[tr.Outcome != ""])
	}
	var outcome *string
	if tr.Kind == "reconciled" {
		outcome = &tr.Outcome
	}
	p, err := effectPayload(id, tr.Kind, nil, nil, nil, outcome, tr.Observation, tr.ResultRef)
	if err != nil {
		return Effect{}, err
	}
	evID, err := ids.New("evt")
	if err != nil {
		return Effect{}, err
	}
	appended, err := s.append(ctx, tx, Event{ID: evID, Kind: "effect." + tr.Kind + "/v1", Subject: id, Actor: actor, Payload: p})
	if err != nil {
		return Effect{}, err
	}
	// Projections change with every event, in the same transaction.
	if s.hooks.Project != nil {
		if err := s.hooks.Project(&Tx{ctx: ctx, tx: tx}, []Event{appended}); err != nil {
			return Effect{}, err
		}
	}
	attempts := e.Attempts
	if tr.Kind == "retried" {
		attempts++
	}
	result := e.ResultRef
	if tr.ResultRef != nil {
		result = tr.ResultRef
	}
	if _, err := tx.ExecContext(ctx, `UPDATE effects SET state = ?, attempts = ?, last_observation = ?, result_ref = ? WHERE id = ?`,
		string(to), attempts, tr.Observation, result, id); err != nil {
		return Effect{}, err
	}
	e.State, e.Attempts, e.LastObservation, e.ResultRef = to, attempts, tr.Observation, result
	return e, nil
}

// MarkPendingUncertain is the restart rule: every pending effect becomes
// uncertain (it may or may not have happened), each with its event. It
// returns how many changed.
func (s *Store) MarkPendingUncertain(ctx context.Context, actor string) (int, error) {
	if err := ids.Check(actor, "act"); err != nil {
		return 0, fmt.Errorf("eventstore: actor: %w", err)
	}
	n := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM effects WHERE state = 'pending' ORDER BY id`)
		if err != nil {
			return err
		}
		var pending []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			pending = append(pending, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, id := range pending {
			if _, err := s.transition(ctx, tx, actor, id, Transition{Kind: "uncertain", Observation: "pending at restart"}); err != nil {
				return err
			}
		}
		n = len(pending)
		return nil
	})
	if err == nil {
		s.restartMarked.Store(true)
	}
	return n, err
}

// RestartMarked reports whether MarkPendingUncertain has run on this open
// store, so a pending effect was intended since it opened: an effect kind
// that resumes a pending effect (model_call's re-intend) requires it.
func (s *Store) RestartMarked() bool { return s.restartMarked.Load() }

// LoadEffect returns an effect's current state.
func (s *Store) LoadEffect(ctx context.Context, id string) (e Effect, err error) {
	err = s.do(func() error {
		var err error
		e, err = loadEffect(ctx, s.r, id)
		return err
	})
	return e, err
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func loadEffect(ctx context.Context, q querier, id string) (Effect, error) {
	var e Effect
	var state string
	var result sql.NullString
	err := q.QueryRowContext(ctx, `SELECT id, kind, command, target, request_digest, state, attempts, last_observation, result_ref FROM effects WHERE id = ?`, id).
		Scan(&e.ID, &e.Kind, &e.Command, &e.Target, &e.RequestDigest, &state, &e.Attempts, &e.LastObservation, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return Effect{}, fmt.Errorf("%w %s", ErrNoEffect, id)
	}
	if err != nil {
		return Effect{}, err
	}
	e.State = EffectState(state)
	if result.Valid {
		e.ResultRef = &result.String
	}
	return e, nil
}

// commandEffects lists a command's effects with their current states.
func commandEffects(ctx context.Context, q querier, command string) ([]Effect, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM effects WHERE command = ? ORDER BY id`, command)
	if err != nil {
		return nil, err
	}
	var idsOf []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		idsOf = append(idsOf, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	var out []Effect
	for _, id := range idsOf {
		e, err := loadEffect(ctx, q, id)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}
