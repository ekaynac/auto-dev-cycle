// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// openNotes is the boundary with the production note.add handler, its
// supervisor seated or not.
func openNotes(t *testing.T, seated bool) *boundary {
	b := openBoundaryGrace(t, 0, seated)
	b.az.Handlers[authority.NoteAction] = authority.NoteHandler{}
	return b
}

func (b *boundary) noteAddCmd(p identity.Principal, key string, in any) eventstore.Command {
	b.t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		b.t.Fatal(err)
	}
	return p.Command(newID(b.t, "cmd"), authority.NoteAction, key, raw, nil)
}

// addNote submits c with note.add's own logic.
func (b *boundary) addNote(c eventstore.Command) (eventstore.Committed, error) {
	return b.s.Submit(context.Background(), c, authority.AddNote(c))
}

func (b *boundary) storedNote(id types.NoteID) (n types.Note, ok bool) {
	b.t.Helper()
	b.view(func(tx *eventstore.Tx) error {
		raw, found, err := tx.Get(authority.NoteProjection, id)
		if err != nil || !found {
			return err
		}
		ok = true
		return contract.UnmarshalStrict(raw, &n)
	})
	return n, ok
}

// lastEvent is the log's last event.
func (b *boundary) lastEvent() (e eventstore.Event) {
	b.t.Helper()
	h := b.head()
	b.view(func(tx *eventstore.Tx) error {
		evs, err := tx.EventsAfter(h-1, 1)
		if err == nil && len(evs) == 1 {
			e = evs[0]
		}
		return err
	})
	return e
}

// An operator and the seated supervisor add notes: each note is under a
// minted not_ id, by its author, at its command's time (Tx.Now), and
// valid as a record; a repeat replays; the notes rebuild.
func TestNoteAdd(t *testing.T) {
	b := openNotes(t, true)
	subject := newID(t, "prp")
	for _, c := range []struct {
		by   identity.Principal
		in   map[string]any
		subj *string
	}{
		{b.op, map[string]any{"project": prjB, "body": "operator\tnote\nline two"}, nil},
		{b.sup, map[string]any{"project": prjA, "subject": subject, "body": strings.Repeat("ç", 8192)}, &subject},
	} {
		cmd := b.noteAddCmd(c.by, "k", c.in)
		out, err := b.addNote(cmd)
		if err != nil || out.Disposition != eventstore.DispositionApplied {
			t.Fatalf("%s's note: %+v %v", c.by.Actor(), out, err)
		}
		var res struct{ Note types.NoteID }
		if err := json.Unmarshal(out.Result, &res); err != nil || !strings.HasPrefix(res.Note, "not_") {
			t.Fatalf("the result %s: %v", out.Result, err)
		}
		id := res.Note
		n, ok := b.storedNote(id)
		ev := b.lastEvent()
		want := types.Note{ID: id, Project: c.in["project"].(types.ProjectID), Subject: c.subj, Author: c.by.Actor(), Body: c.in["body"].(string), CreatedAt: ev.At}
		if got, _ := json.Marshal(n); !ok || string(got) != string(must(json.Marshal(want))) || ev.Subject != id || ev.Kind != authority.NoteAdded ||
			ev.Project == nil || *ev.Project != string(want.Project) {
			t.Fatalf("the note %s, want %+v (event %+v)", got, want, ev)
		}
		if err := contract.Validate("records/note", must(json.Marshal(n))); err != nil {
			t.Errorf("the record: %v", err)
		}
		h := b.head()
		again, err := b.addNote(b.noteAddCmd(c.by, "k", c.in))
		if err != nil || !again.Replayed || again.CommandID != out.CommandID || string(again.Result) != string(out.Result) || b.head() != h {
			t.Fatalf("the repeat: %+v %v", again, err)
		}
	}
	b.rebuilds()
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// A supervisor without the seat, or outside its grant, is refused before
// the logic, and so is an input its schema refuses; an expected state of
// the new note other than its absence is stale.
func TestNoteAddRefusals(t *testing.T) {
	unseated := openNotes(t, false)
	ok := map[string]any{"project": prjA, "body": "a note"}
	unseated.refusedNote("no seat", unseated.noteAddCmd(unseated.sup, "a", ok), isReason(vocab.ReasonSeatNotHeld))
	b := openNotes(t, true)
	b.refusedNote("outside the grant", b.noteAddCmd(b.sup, "a", map[string]any{"project": prjB, "body": "x"}), isReason(vocab.ReasonActionNotGranted))
	invalid := func(err error) bool {
		var c *contract.Error
		return errors.As(err, &c)
	}
	for name, in := range map[string]map[string]any{
		"too long":      {"project": prjA, "body": strings.Repeat("a", 8193)},
		"empty":         {"project": prjA, "body": ""},
		"no body":       {"project": prjA},
		"a NUL":         {"project": prjA, "body": "a\x00b"},
		"a CR":          {"project": prjA, "body": "a\rb"},
		"an ESC":        {"project": prjA, "body": "\x1b[31mred"},
		"a DEL":         {"project": prjA, "body": "a\x7f"},
		"a C1 control":  {"project": prjA, "body": "a\u0085b"},
		"not a project": {"project": newID(t, "prp"), "body": "x"},
		"a bad subject": {"project": prjA, "body": "x", "subject": "note-1"},
		"an id":         {"project": prjA, "body": "x", "id": newID(t, "not")},
		"an author":     {"project": prjA, "body": "x", "author": b.op.Actor()},
	} {
		b.refusedNote(name, b.noteAddCmd(b.op, name, in), invalid)
		// The schema itself refuses it, not only the strict decoding.
		if contract.Validate(authority.NoteAddScope, must(json.Marshal(in))) == nil {
			t.Errorf("%s: the scope schema admits it", name)
		}
	}
	// The logic is note.add's alone, and writes only under a derived id.
	b.az.Handlers["task.create"] = authority.NoteHandler{}
	other := b.noteAddCmd(b.op, "other", ok)
	other.Action = "task.create"
	if _, err := b.addNote(other); err == nil || !strings.Contains(err.Error(), "no derived note.add") {
		t.Errorf("note.add's logic ran for another action: %v", err)
	}
	b.view(func(tx *eventstore.Tx) error {
		if _, err := authority.AddNote(b.noteAddCmd(b.op, "x", ok))(tx); err == nil {
			t.Error("note.add's logic ran without a derivation")
		}
		return nil
	})
	// ... and only under a not_ id: a derivation binding another id is
	// refused by the logic itself, before any record is built.
	b.az.Handlers[authority.NoteAction] = misboundNote{}
	h := b.head()
	if _, err := b.addNote(b.noteAddCmd(b.op, "misbound", ok)); err == nil || !strings.Contains(err.Error(), `want "not"`) || b.head() != h {
		t.Errorf("note.add's logic wrote under a bound %s: %v", prjB, err)
	}
	b.az.Handlers[authority.NoteAction] = authority.NoteHandler{}
	// The new note does not exist: only version 0 is current.
	stale := b.noteAddCmd(b.op, "v1", ok)
	stale.Expect = version(1)
	b.refusedNote("version 1 of the new note", stale, is(eventstore.ErrStaleExpectedState))
	fresh := b.noteAddCmd(b.op, "v0", ok)
	fresh.Expect = version(0)
	if out, err := b.addNote(fresh); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("version 0 of the new note: %+v %v", out, err)
	}
	// Only operators and supervisors write notes.
	b.refusedNote("the system", b.noteAddCmd(b.sys, "sys", ok), isReason(vocab.ReasonActorKindRefused))
}

// A grant tightening note.add to an independent review would make the
// supervisor's note a proposal; its id is minted, so a confirmation could
// never revalidate it, and it is refused with no proposal and no note.
func TestNoteAddIsNeverProposed(t *testing.T) {
	b := openNotes(t, true)
	tight := b.supGrant
	tight.ID, tight.ConfirmationOverrides = types.GrantID(newID(t, "grt")), map[string]types.GrantConfirmationOverridesValue{authority.NoteAction: "independent_review"}
	b.seed(b.op, createGrant(tight))
	b.refusedNote("a tightened note", b.noteAddCmd(b.sup, "tight", map[string]any{"project": prjA, "body": "x"}), isReason(vocab.ReasonActionNotProposable))
	b.view(func(tx *eventstore.Tx) error {
		if keys, err := tx.Keys(authority.ProposalProjection, ""); err != nil || len(keys) != 0 {
			t.Errorf("a proposal was recorded: %v %v", keys, err)
		}
		return nil
	})
}

// refusedNote submits c with note.add's logic and requires err to be what
// check accepts, the logic never run (the derivation refused first),
// nothing appended and no note recorded.
func (b *boundary) refusedNote(name string, c eventstore.Command, check func(error) bool) {
	b.t.Helper()
	h, n, ran := b.head(), b.noteCount(), false
	_, err := b.s.Submit(context.Background(), c, func(tx *eventstore.Tx) (eventstore.Decision, error) {
		ran = true
		return authority.AddNote(c)(tx)
	})
	if !check(err) || ran || b.noteCount() != n || b.head() != h {
		b.t.Errorf("%s: %v (logic ran: %t)", name, err, ran)
	}
}

// misboundNote derives note.add but binds a project id, not a note id.
type misboundNote struct{}

func (misboundNote) Derive(tx *eventstore.Tx, by types.Actor, input json.RawMessage) (authority.Derived, error) {
	d, err := authority.NoteHandler{}.Derive(tx, by, input)
	d.Object, d.Record = string(prjB), authority.NoteRecord(string(prjB))
	return d, err
}

func (b *boundary) noteCount() (n int) {
	b.t.Helper()
	b.view(func(tx *eventstore.Tx) error {
		keys, err := tx.Keys(authority.NoteProjection, "")
		n = len(keys)
		return err
	})
	return n
}

// The projector admits a note only under its id, in its project, by its
// author, a live operator or supervisor, at its event's time, and only once.
func TestNoteProjectorGuards(t *testing.T) {
	b := openNotes(t, true)
	a, other := string(prjA), string(prjB)
	addedIn := func(project *string, id types.NoteID, subject string, author types.ActorID, skew time.Duration) applyFn {
		return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			at := tx.Now().Add(skew).UTC().Format(time.RFC3339Nano)
			p := must(json.Marshal(types.Note{ID: id, Project: prjA, Author: author, Body: "x", CreatedAt: at}))
			return []eventstore.Event{{ID: newID(t, "evt"), Kind: authority.NoteAdded, Subject: subject, Project: project, Payload: p}}, nil
		}
	}
	added := func(id types.NoteID, subject string, author types.ActorID, skew time.Duration) applyFn {
		return addedIn(&a, id, subject, author, skew)
	}
	id := types.NoteID(newID(t, "not"))
	for name, c := range map[string]struct {
		by    identity.Principal
		apply applyFn
	}{
		"another subject": {b.op, added(id, newID(t, "not"), b.op.Actor(), 0)},
		"another author":  {b.op, added(id, id, b.sup.Actor(), 0)},
		"the system":      {b.sys, added(id, id, b.sys.Actor(), 0)},
		"a later time":    {b.op, added(id, id, b.op.Actor(), time.Millisecond)},
		"an earlier time": {b.op, added(id, id, b.op.Actor(), -time.Millisecond)},
		"no project":      {b.op, addedIn(nil, id, id, b.op.Actor(), 0)},
		"another project": {b.op, addedIn(&other, id, id, b.op.Actor(), 0)},
	} {
		if err := b.try(c.by, c.apply); err == nil || !strings.Contains(err.Error(), "note") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The payload is the note record, checked at append: no extra field,
	// the nullable subject present.
	for name, edit := range map[string]func(map[string]any){
		"an extra field": func(m map[string]any) { m["extra"] = 1 },
		"no subject":     func(m map[string]any) { delete(m, "subject") },
	} {
		err := b.try(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			var m map[string]any
			if err := json.Unmarshal(must(json.Marshal(types.Note{ID: id, Project: prjA, Author: b.op.Actor(), Body: "x",
				CreatedAt: tx.Now().UTC().Format(time.RFC3339Nano)})), &m); err != nil {
				return nil, err
			}
			edit(m)
			return []eventstore.Event{{ID: newID(t, "evt"), Kind: authority.NoteAdded, Subject: id, Project: &a, Payload: must(json.Marshal(m))}}, nil
		})
		if err == nil || !strings.Contains(err.Error(), "records/note") {
			t.Errorf("%s: %v", name, err)
		}
	}
	b.seed(b.op, added(id, id, b.op.Actor(), 0))
	if err := b.try(b.sup, added(id, id, b.sup.Actor(), 0)); err == nil || !strings.Contains(err.Error(), "the note exists") {
		t.Errorf("a second note under one id: %v", err)
	}
	if n, ok := b.storedNote(id); !ok || n.Author != b.op.Actor() {
		t.Errorf("the first note: %+v %t", n, ok)
	}
	b.rebuilds()
}
