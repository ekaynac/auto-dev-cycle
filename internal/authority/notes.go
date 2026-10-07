// SPDX-License-Identifier: Apache-2.0

package authority

// Notes (docs/design/m0/14 PR 8; 08 notes; 02-supervisor-role §5):
// note.add records an operator's or a supervisor's note on a project,
// optionally about one record. A note is untrusted text (03-knowledge-layer
// "Supervisor continuity"): bounded by its schema, stored and served as
// is, never interpreted. Notes are only ever added.
//
// Decisions:
//   - Derive mints the note's id (02 "Identifiers": the controller mints
//     ids) and binds it; the logic writes under the bound id (Tx.Bound).
//     A retry under the same key replays before any derivation.
//   - A supervisor's note is a supervisor write and needs the seat (08
//     "Supervisor seat"; the holder writes its handover note before it
//     lets the seat go, 02 §5).
//   - The project is judged as authority judges any project: a supervisor
//     needs a grant covering it. This build has no project registry
//     (project.create is unwired, 14 D1), so its existence is not checked.
//   - The subject is a reference, never resolved, so note.add answers
//     nothing about another project's records.
//   - The note's id is minted, so a note.add a tightened grant would make a
//     proposal is refused (Derived.Minted, action_not_proposable): a
//     confirmation could never revalidate it. NoteHandler is no Applier.
//   - Only operators and supervisors write notes (actor_kind_refused).

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The note action, its scope schema, its event kind and the projection
// holding the notes (types.Note, by id).
const (
	NoteAction     = "note.add"
	NoteAddScope   = "api/scope/note-add"
	NoteAdded      = "note.added/v1"
	NoteProjection = "notes"
)

// NoteKinds registers the note event kind; its payload is the note.
func NoteKinds() eventstore.Kinds {
	return eventstore.Kinds{NoteAdded: {Schema: "records/note"}}
}

// NoteRecord is note id's record, which --expect is checked against.
func NoteRecord(id types.NoteID) func(*eventstore.Tx) (json.RawMessage, bool, error) {
	return func(tx *eventstore.Tx) (json.RawMessage, bool, error) { return tx.Get(NoteProjection, id) }
}

// NoteHandler derives note.add.
type NoteHandler struct{}

// Derive checks the scope against its schema, then derives a new note,
// under a fresh id, in the scope's project.
func (NoteHandler) Derive(_ *eventstore.Tx, by types.Actor, input json.RawMessage) (Derived, error) {
	in, err := noteInputOf(input)
	if err != nil {
		return Derived{}, err
	}
	if by.Kind != types.ActorKindOperator && by.Kind != types.ActorKindSupervisor {
		return Derived{}, refusal(vocab.ReasonActorKindRefused, "actor %s is a %s; only operators and supervisors write notes", by.ID, by.Kind)
	}
	id, err := ids.New("not")
	return Derived{Object: id, Minted: true, Project: &in.Project, Record: NoteRecord(id)}, err
}

// noteInputOf is the scope, refused as ErrInvalidScope unless its schema
// admits it.
func noteInputOf(raw json.RawMessage) (types.NoteAddScope, error) {
	in, err := contract.Decode[types.NoteAddScope](NoteAddScope, raw)
	if err != nil {
		return in, fmt.Errorf("%w: %s input: %w", ErrInvalidScope, NoteAction, err)
	}
	return in, nil
}

// AddNote is note.add's logic for c, a command the Authorizer allowed:
// the note under the bound id (a not_ id), in its project, by c's actor,
// at the command's time.
func AddNote(c eventstore.Command) eventstore.Decide {
	return eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
		id, bound := tx.Bound()
		if c.Action != NoteAction || !bound {
			return nil, nil, fmt.Errorf("authority: %s is no derived %s", c.Action, NoteAction)
		}
		in, err := noteInputOf(c.Input)
		if err := errors.Join(err, ids.Check(id, "not")); err != nil {
			return nil, nil, err
		}
		evs, err := event(NoteAdded, id, &in.Project, types.Note{ID: id, Project: in.Project, Subject: in.Subject, Author: c.Actor, Body: in.Body,
			CreatedAt: tx.Now().UTC().Format(time.RFC3339Nano)})
		res, err2 := json.Marshal(map[string]any{"note": id})
		return evs, res, errors.Join(err, err2)
	})
}

// NoteProjector admits note.added events into NoteProjection; other kinds
// pass through. It reads only the events and the identity projection, so
// a rebuild reaches the same notes.
type NoteProjector struct{}

// Project is the eventstore.Hooks.Project fold for the notes.
func (NoteProjector) Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		if ev.Kind != NoteAdded {
			continue
		}
		if err := addNote(tx, ev); err != nil {
			return fmt.Errorf("authority: event %s: note: %w", ev.ID, err)
		}
	}
	return nil
}

// addNote admits a new note under its event's subject, project, actor and
// time, by a live operator or supervisor.
func addNote(tx *eventstore.Tx, ev eventstore.Event) error {
	var n types.Note
	if err := json.Unmarshal(ev.Payload, &n); err != nil {
		return err
	}
	if ev.Subject != n.ID || ev.Project == nil || *ev.Project != string(n.Project) || ev.Actor != n.Author || later(n.CreatedAt, ev.At) || later(ev.At, n.CreatedAt) {
		return errors.New("a note's event is under its id, in its project, by its author, at its time")
	}
	by, err := liveActor(tx, n.Author)
	if err != nil || by.Kind != types.ActorKindOperator && by.Kind != types.ActorKindSupervisor {
		return errors.Join(err, errors.New("a live operator or supervisor writes a note"))
	}
	if _, exists, err := tx.Get(NoteProjection, n.ID); err != nil || exists {
		return errors.Join(err, errors.New("the note exists"))
	}
	return tx.Put(NoteProjection, n.ID, ev.Payload)
}
