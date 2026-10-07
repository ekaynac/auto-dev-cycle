// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// 14 PR 7b-1 end to end, the CLI against the running daemon: an
// enrolment's token reaches its --token-out file alone, never stdout or
// the caller's own token file, and is served; a repeat delivers none and
// names the recovery; no secret is stored or logged.
func TestEnrolTokenOutThroughTheCLI(t *testing.T) {
	c := runCommanded(t)
	dir := filepath.Join(t.TempDir(), "out")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	enrol := []string{"admin", "enrol-agent", "--expect", "0", "--reason", "r", "--idempotency-key", "enrol-1",
		"--scope", `{"actor":"` + newID(t, "act") + `","name":"agent","declared_agent":null}`}
	path := filepath.Join(dir, "agent.token")
	code, out, errb := c.adc(c.op.Reveal(), append(enrol, "--json", "--token-out", path)...)
	a, ok := response(t, out).(*types.CommandApplied)
	raw, err := os.ReadFile(path)
	if code != entry.ExitOK || !ok || a.IssuedCredential != nil || err != nil {
		t.Fatalf("the enrolment: %d %s %s %v", code, out, errb, err)
	}
	tok := strings.TrimSuffix(string(raw), "\n")
	if strings.Contains(out+errb, tok) {
		t.Fatal("the token was printed")
	}
	body := strings.NewReader(`{"idempotency_key":"r","expected":{"version":0,"digest":null},"scope":{},"reason":"r"}`)
	if code, _, _ := c.post(t, "seat.release", tok, body); code != http.StatusOK {
		t.Errorf("the enrolled token: HTTP %d", code)
	}
	again := filepath.Join(dir, "again")
	code, out, errb = c.adc(c.op.Reveal(), append(enrol, "--token-out", again)...)
	if id, _, _ := strings.Cut(tok, "."); code != entry.ExitCouldNotRun || out != "" || !strings.Contains(errb, id) || !strings.Contains(errb, "revoke the actor") {
		t.Errorf("the repeat: %d %q %q", code, out, errb)
	}
	if _, err := os.Lstat(again); !os.IsNotExist(err) {
		t.Errorf("the repeat left its --token-out: %v", err)
	}
	_, secret, _ := strings.Cut(tok, ".")
	secretsAbsent(t, &running{dir: c.dir, logs: c.logs}, secret)
}

// A mint whose request has no peer recorded takes its slot under -1
// (api.SlotKey), never root's uid 0: with uid 0's one slot taken it still
// applies.
func TestMintWithoutAPeerIsNotRoot(t *testing.T) {
	c := runCommanded(t)
	gate := api.NewSlots(2, 1)
	if _, ok := gate.Take(0); !ok {
		t.Fatal("uid 0's slot")
	}
	var p identity.Principal
	if err := c.d.store.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		p, err = identity.Authenticated(tx, c.op.Reveal(), identity.PrivateSocket)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h := commands{store: c.d.store, gate: gate, reg: c.d.az.Registry, logic: logics(c.d.az), issue: issuers(c.d.az), log: c.logs}
	body := `{"idempotency_key":"e","expected":{"version":0,"digest":null},"scope":{"actor":"` + newID(t, "act") + `","name":"a","declared_agent":null},"reason":"r"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/commands/"+authority.EnrolAction, strings.NewReader(body))
	req.SetPathValue("action", authority.EnrolAction)
	req.Header.Set("Authorization", "Bearer "+c.op.Reveal())
	req = req.WithContext(api.WithPrincipal(req.Context(), p))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	raw, _ := io.ReadAll(rec.Body)
	if a, ok := response(t, string(raw)).(*types.CommandApplied); rec.Code != http.StatusOK || !ok || a.IssuedCredential == nil {
		t.Fatalf("HTTP %d", rec.Code)
	}
}
