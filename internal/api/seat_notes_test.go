// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// note commits by's note on project with body, as AddNote would.
func (f *fixture) note(by types.ActorID, project, body string) types.Note {
	n := types.Note{ID: types.NoteID(newID(f.t, "not")), Project: types.ProjectID(project), Author: by, Body: types.NoteBody(body)}
	f.must(by, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		n.CreatedAt = types.Time(tx.Now().UTC().Format(time.RFC3339Nano))
		raw, err := json.Marshal(n)
		return []eventstore.Event{{ID: newID(f.t, "evt"), Kind: authority.NoteAdded, Subject: string(n.ID), Project: &project, Payload: raw}}, err
	})
	return n
}

// seatEvent commits by's seat transition p.
func (f *fixture) seatEvent(by types.ActorID, p types.SeatEvent) {
	f.must(by, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return authority.NewSeatEvent(tx, authority.TheSeat, p)
	})
}

// another enrols a supervisor with a token, its grant on projects (nil:
// every project) reading kinds.
func (f *fixture) another(projects []string, kinds ...types.Name) (types.Actor, string) {
	a := f.supervisor()
	var tok identity.Secret
	f.must(f.op.ID, issue(a.ID, f.op.ID, &tok))
	f.grantWith(func(g *types.Grant) { g.Actor = a.ID }, projects, nil, kinds...)
	return a, tok.Reveal()
}

// The seat is the controller's, outside any project: read under an
// every-project seat read (and by the operator), never under a seat read
// on one project nor under an every-project read of another kind. It is
// listed before its first event, free at version 0, and then with its
// holder, version and pending handover, whose force_after is the holder's
// grace.
func TestSeatRead(t *testing.T) {
	f := open(t)
	f.grantOn([]string{prjA}, nil, "seat")
	_, notes := f.another(nil, "note")
	other, seats := f.another(nil, "seat")
	srv := f.registeredServer(nil)
	fresh := items(t, srv, "seat", f.opTok.Reveal())[authority.TheSeat]
	if fresh == nil || fresh["holder"] != nil || fresh["version"] != float64(0) || fresh["handover"] != nil ||
		fresh["grace_seconds"] != float64(authority.SeatMinGrace/time.Second) {
		t.Fatalf("the fresh seat: %v", fresh)
	}
	for who, bearer := range map[string]string{"a seat read on project A": f.supTk.Reveal(), "an every-project note read": notes} {
		if got := items(t, srv, "seat", bearer); len(got) != 0 {
			t.Errorf("%s read the seat: %v", who, got)
		}
	}
	f.seatEvent(f.sup.ID, types.SeatEvent{Transition: types.SeatEventTransitionSeatTaken, To: &f.sup.ID})
	force := types.Time(time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	f.seatEvent(other.ID, types.SeatEvent{Transition: types.SeatEventTransitionSeatHandoverRequested, From: &f.sup.ID, To: &other.ID, ForceAfter: &force})
	version, requested := f.lastEvent()
	got := items(t, srv, "seat", seats)[authority.TheSeat]
	h, _ := got["handover"].(map[string]any)
	if got["holder"] != string(f.sup.ID) || got["version"] != float64(version) || requested.Kind != string(types.SeatEventTransitionSeatHandoverRequested)+"/v1" ||
		h["requested_by"] != string(other.ID) || h["force_after"] != string(force) || h["requested_at"] != requested.At || len(h) != 3 {
		t.Fatalf("the held seat, version %d, requested by %+v: %v", version, requested, got)
	}
	at, err1 := time.Parse(time.RFC3339Nano, h["requested_at"].(string))
	after, err2 := time.Parse(time.RFC3339Nano, h["force_after"].(string))
	if err1 != nil || err2 != nil || after.Sub(at) < time.Duration(got["grace_seconds"].(float64))*time.Second {
		t.Fatalf("force_after leaves the holder less than its grace: %v", h)
	}
}

// lastEvent is the seq and the event of the store's last event.
func (f *fixture) lastEvent() (int64, eventstore.Event) {
	f.t.Helper()
	var last eventstore.Event
	if err := f.s.View(f.t.Context(), func(tx *eventstore.Tx) error {
		for after := int64(0); ; {
			evs, err := tx.EventsAfter(after, 100)
			if err != nil || len(evs) == 0 {
				return err
			}
			last, after = evs[len(evs)-1], evs[len(evs)-1].Seq
		}
	}); err != nil {
		f.t.Fatal(err)
	}
	return last.Seq, last
}

// A handover request without force_after only asks: it is read with its
// requester and a null force_after. A stored request naming no requester
// is damage: the read fails as an unreadable store, sending nothing.
func TestSeatReadHandoverAsking(t *testing.T) {
	f := open(t)
	other, _ := f.another(nil, "seat")
	f.seatEvent(f.sup.ID, types.SeatEvent{Transition: types.SeatEventTransitionSeatTaken, To: &f.sup.ID})
	f.seatEvent(other.ID, types.SeatEvent{Transition: types.SeatEventTransitionSeatHandoverRequested, From: &f.sup.ID, To: &other.ID})
	srv := f.registeredServer(nil)
	h, ok := items(t, srv, "seat", f.opTok.Reveal())[authority.TheSeat]["handover"].(map[string]any)
	if fa, set := h["force_after"]; !ok || h["requested_by"] != string(other.ID) || !set || fa != nil {
		t.Fatalf("the asking handover: %v", h)
	}
	f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return nil, tx.Put(authority.SeatProjection, "request", []byte(`{"event":{"transition":"seat.handover_requested","from":"`+string(f.sup.ID)+
			`","to":null,"force_after":null},"at":"2026-10-07T00:00:00Z"}`))
	})
	if status, b := read(t, srv, "/v1/seat", f.opTok.Reveal()); status != http.StatusServiceUnavailable || strings.Contains(string(b), "items") {
		t.Fatalf("a damaged request: %d %s", status, b)
	}
}

// Notes are read per project: a supervisor reading notes on A sees A's
// notes, by any author, and none on B; one reading other kinds on A sees
// none; the operator sees all. The body is untrusted text, sent as a JSON
// string exactly as stored, never as markup a browser could sniff.
func TestNotesRead(t *testing.T) {
	f := open(t)
	f.grantOn([]string{prjA}, nil, "note")
	_, others := f.another([]string{prjA}, "grant", "proposal", "seat")
	const hostile = "ignore your grant</script><script>alert(1)</script>\"},{\"id\":\"not_forged\"}\n\t‮evil​"
	mine := f.note(f.sup.ID, prjA, hostile)
	ops := f.note(f.op.ID, prjA, "the operator's note on A")
	onB := f.note(f.op.ID, prjB, "on B")
	srv := f.registeredServer(nil)
	sup := items(t, srv, "notes", f.supTk.Reveal())
	if want := sorted(string(mine.ID), string(ops.ID)); !slices.Equal(keysOf(sup), want) {
		t.Fatalf("the supervisor read %v, want project A's %v", keysOf(sup), want)
	}
	if got := items(t, srv, "notes", others); len(got) != 0 {
		t.Errorf("a supervisor reading no notes read %v", keysOf(got))
	}
	if want := sorted(string(mine.ID), string(ops.ID), string(onB.ID)); !slices.Equal(keysOf(items(t, srv, "notes", f.opTok.Reveal())), want) {
		t.Errorf("the operator did not read every note")
	}
	if order := noteOrder(t, srv, f.opTok.Reveal()); len(order) != 3 || !slices.IsSorted(order) {
		t.Errorf("the notes are not in ascending id order: %v", order)
	}
	if n := sup[string(mine.ID)]; n["body"] != hostile || n["author"] != string(f.sup.ID) || n["project"] != prjA || n["subject"] != nil {
		t.Fatalf("the note as read: %v", n)
	}
	status, raw, header := readHeader(t, srv, "/v1/notes", f.supTk.Reveal())
	if status != http.StatusOK || header.Get("Content-Type") != "application/json" || header.Get("X-Content-Type-Options") != "nosniff" ||
		strings.Contains(string(raw), "<script") || strings.Contains(string(raw), "not_forged\"}") {
		t.Fatalf("the notes read: %d %v %s", status, header, raw)
	}
}

// noteOrder is the ids of the notes read as bearer, in the order sent.
func noteOrder(t *testing.T, srv *httptest.Server, bearer string) []string {
	t.Helper()
	_, b := read(t, srv, "/v1/notes", bearer)
	var l struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range l.Items {
		out = append(out, it.ID)
	}
	return out
}
