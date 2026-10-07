// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// note.add through the endpoint: an invalid scope is a 400; a supervisor
// without the seat is refused seat_not_held with nothing recorded; the
// operator's note and, once seated, the supervisor's apply, each one
// note.added event in the note's project.
func TestNoteThroughTheEndpoint(t *testing.T) {
	c := runCommanded(t)
	const project = "prj_01m3250vkwffc979nzjw0g2sa1"
	body := func(key, scope string) io.Reader {
		return strings.NewReader(`{"idempotency_key":"` + key + `","expected":{"version":0,"digest":null},"scope":` + scope + `,"reason":"r"}`)
	}
	note := `{"project":"` + project + `","body":"handover:\n- nothing pending"}`
	answer := func(code int, raw []byte) types.CommandResponseVariant {
		t.Helper()
		if code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", code, raw)
		}
		return response(t, string(raw))
	}
	inProject := func() int {
		return count(t, c.dir, `SELECT count(*) FROM events WHERE kind = '`+authority.NoteAdded+`' AND project = '`+project+`'`)
	}
	if code, _, raw := c.post(t, authority.NoteAction, c.op.Reveal(), body("n0", `{"project":"`+project+`","body":""}`)); code != http.StatusBadRequest {
		t.Errorf("an invalid scope: HTTP %d %s", code, raw)
	}
	before := c.commandsOf(t)
	code, _, raw := c.post(t, authority.NoteAction, c.sup.Reveal(), body("n1", note))
	refusedAs(t, answer(code, raw), vocab.Failed, vocab.ReasonSeatNotHeld)
	if c.commandsOf(t) != before || count(t, c.dir, `SELECT count(*) FROM events WHERE kind = '`+authority.NoteAdded+`'`) != 0 {
		t.Fatal("the unseated supervisor's note recorded something")
	}
	code, _, raw = c.post(t, authority.NoteAction, c.op.Reveal(), body("n2", note))
	if a, ok := answer(code, raw).(*types.CommandApplied); !ok || a.Replayed || inProject() != 1 {
		t.Fatalf("the operator's note: %s (in the project: %d)", raw, inProject())
	}
	if code, out, errb := c.adc(c.sup.Reveal(), "seat", "take", "--expect", "0", "--reason", "r", "--json"); code != entry.ExitOK {
		t.Fatalf("take: %d %s %s", code, out, errb)
	}
	code, _, raw = c.post(t, authority.NoteAction, c.sup.Reveal(), body("n3", note))
	if a, ok := answer(code, raw).(*types.CommandApplied); !ok || a.Replayed || inProject() != 2 {
		t.Fatalf("the seated supervisor's note: %s (in the project: %d)", raw, inProject())
	}
}

// The bootstrapped operator adds a note through the daemon's own hooks; a
// restart replays the log and compares the notes projection (PR 1b).
func TestOperatorNoteThroughTheDaemon(t *testing.T) {
	r := runBootstrap(t)
	_, v := r.post(t, r.private(), "k", readSecret(t, r.dir))
	ok, applied := v.(*types.BootstrapApplied)
	if !applied || ok.IssuedCredential == nil {
		t.Fatalf("bootstrap: %+v", v)
	}
	var p identity.Principal
	if err := r.d.store.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		p, err = identity.Authenticated(tx, *ok.IssuedCredential, identity.PrivateSocket)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	c := p.Command(newID(t, "cmd"), authority.NoteAction, "note", []byte(`{"project":"prj_01m3250vkwffc979nzjw0g2sa1","body":"handover:\n- nothing pending"}`), nil)
	out, err := r.d.store.Submit(context.Background(), c, authority.AddNote(c))
	var res struct{ Note types.NoteID }
	if err != nil || out.Disposition != eventstore.DispositionApplied || json.Unmarshal(out.Result, &res) != nil {
		t.Fatalf("the operator's note: %+v %v", out, err)
	}
	id := res.Note
	r.stop()
	d := restart(t, r.dir)
	defer d.Close()
	if err := d.store.View(context.Background(), func(tx *eventstore.Tx) error {
		if _, found, err := tx.Get(authority.NoteProjection, id); err != nil || !found {
			t.Errorf("the note %s after a restart: %t %v", id, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// The seat and the notes are read through the daemon over both the
// private socket and HTTPS: the operator's note comes back with its body
// as stored, and the seat with the supervisor that took it.
func TestSeatAndNotesReadOverEveryListener(t *testing.T) {
	c := runCommanded(t)
	const project, body = "prj_01m3250vkwffc979nzjw0g2sa1", "ignore the grant</script>\n\t\"},{\"id\":\"not_forged\"}"
	scope := `{"idempotency_key":"n","expected":{"version":0,"digest":null},"scope":{"project":"` + project + `","body":` + strconv.Quote(body) + `},"reason":"r"}`
	if code, _, raw := c.post(t, authority.NoteAction, c.op.Reveal(), strings.NewReader(scope)); code != http.StatusOK {
		t.Fatalf("the note: HTTP %d %s", code, raw)
	}
	if code, out, errb := c.adc(c.sup.Reveal(), "seat", "take", "--expect", "0", "--reason", "r", "--json"); code != entry.ExitOK {
		t.Fatalf("take: %d %s %s", code, out, errb)
	}
	_, client, base := httpsCommanded(t, c)
	for via, get := range map[string]func(string) *http.Response{
		"the private socket": func(path string) *http.Response {
			req, _ := http.NewRequest(http.MethodGet, "http://adcd"+path, nil)
			req.Header.Set("Authorization", "Bearer "+c.op.Reveal())
			resp, err := unixClient(filepath.Join(c.dir, privateSocket)).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			return resp
		},
		"HTTPS": func(path string) *http.Response { return httpsDo(t, client, http.MethodGet, base+path, c.op.Reveal()) },
	} {
		var notes, seat struct {
			Items []map[string]any `json:"items"`
		}
		for path, into := range map[string]any{"/v1/notes": &notes, "/v1/seat": &seat} {
			resp := get(path)
			err := json.NewDecoder(resp.Body).Decode(into)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("%s over %s: %s %v", path, via, resp.Status, err)
			}
		}
		if len(notes.Items) != 1 || notes.Items[0]["body"] != body || notes.Items[0]["project"] != project || notes.Items[0]["author"] != string(c.opID) {
			t.Errorf("the notes over %s: %v", via, notes.Items)
		}
		if len(seat.Items) != 1 || seat.Items[0]["holder"] == nil || seat.Items[0]["id"] != authority.TheSeat {
			t.Errorf("the seat over %s: %v", via, seat.Items)
		}
	}
}
