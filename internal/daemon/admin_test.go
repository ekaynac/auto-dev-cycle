// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// 14 PR 7a through the daemon's own writer: the bootstrapped operator
// enrols a supervisor (its secret delivered once, never stored: not in the
// database, its WAL or the log) and revokes it.
func TestDaemonEnrolsAndRevokes(t *testing.T) {
	r := runBootstrap(t)
	_, v := r.post(t, r.private(), "k-boot", readSecret(t, r.dir))
	boot, ok := v.(*types.BootstrapApplied)
	if !ok || boot.IssuedCredential == nil {
		t.Fatalf("bootstrap: %+v", v)
	}
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	az := authority.Authorizer{Registry: reg, Handlers: handlers()}
	login := func(bearer string) (p identity.Principal) {
		t.Helper()
		if err := r.d.store.View(context.Background(), func(tx *eventstore.Tx) (err error) {
			p, err = identity.Authenticated(tx, bearer, identity.PrivateSocket)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return p
	}
	op := login(*boot.IssuedCredential)
	sup := types.ActorID(newID(t, "act"))
	enrol := op.Command(newID(t, "cmd"), authority.EnrolAction, "enrol", []byte(`{"actor":"`+string(sup)+`","name":"agent","declared_agent":null}`), nil)
	out, secret, delivered, err := az.Enrol(context.Background(), r.d.store, enrol)
	if err != nil || out.Disposition != eventstore.DispositionApplied || !delivered {
		t.Fatalf("the enrolment: %+v %v", out, err)
	}
	enrol.ID = newID(t, "cmd")
	if again, replayed, delivered, err := az.Enrol(context.Background(), r.d.store, enrol); err != nil || !again.Replayed || delivered || replayed.Reveal() != "" {
		t.Fatalf("the replay: %+v %v", again, err)
	}
	if login(secret.Reveal()).Actor() != sup {
		t.Fatal("the delivered token is not the supervisor's")
	}
	revoke := op.Command(newID(t, "cmd"), authority.RevokeActorAction, "revoke", []byte(`{"actor":"`+string(sup)+`"}`), nil)
	if out, err := r.d.store.Submit(context.Background(), revoke, az.RevokeActor(revoke)); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("the revocation: %+v %v", out, err)
	}
	if err := r.d.store.View(context.Background(), func(tx *eventstore.Tx) error {
		_, err := identity.Authenticate(tx, secret.Reveal())
		return err
	}); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Errorf("the revoked supervisor's bearer: %v", err)
	}
	_, raw, _ := strings.Cut(secret.Reveal(), ".")
	secretsAbsent(t, r, raw)
	r.stop()
	secretsAbsent(t, r, raw)
}

// agent.enrol and actor.revoke through the command endpoint: the issued
// credential is in the applied response once, absent from its replay and
// never stored; a supervisor's enrolment is refused; the revocation applies.
func TestAdminThroughTheEndpoint(t *testing.T) {
	c := runCommanded(t)
	sup := types.ActorID(newID(t, "act"))
	body := func(key, scope string) io.Reader {
		return strings.NewReader(`{"idempotency_key":"` + key + `","expected":{"version":0,"digest":null},"scope":` + scope + `,"reason":"r"}`)
	}
	enrol := `{"actor":"` + string(sup) + `","name":"agent","declared_agent":null}`
	answer := func(code int, raw []byte) types.CommandResponseVariant {
		t.Helper()
		if code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", code, raw)
		}
		return response(t, string(raw))
	}
	code, _, raw := c.post(t, authority.EnrolAction, c.op.Reveal(), body("e1", enrol))
	a, ok := answer(code, raw).(*types.CommandApplied)
	if !ok || a.Replayed || a.IssuedCredential == nil {
		t.Fatalf("the enrolment: %s", raw)
	}
	secret := *a.IssuedCredential
	code, _, raw = c.post(t, authority.EnrolAction, c.op.Reveal(), body("e1", enrol))
	if r, ok := answer(code, raw).(*types.CommandApplied); !ok || !r.Replayed || r.IssuedCredential != nil || r.Command != a.Command {
		t.Fatalf("the replay: %s", raw)
	}
	code, _, raw = c.post(t, authority.EnrolAction, c.sup.Reveal(), body("e2", `{"actor":"`+newID(t, "act")+`","name":"x","declared_agent":null}`))
	refusedAs(t, answer(code, raw), vocab.Failed, vocab.ReasonOperatorOnly)
	if code, _, _ := c.post(t, authority.EnrolAction, c.op.Reveal(), body("e3", `{"actor":"x"}`)); code != http.StatusBadRequest {
		t.Errorf("an invalid scope: HTTP %d", code)
	}
	revoke := strings.NewReader(`{"idempotency_key":"r1","expected":{"version":` + strconv.FormatInt(a.FirstSeq, 10) + `,"digest":null},"scope":{"actor":"` + string(sup) + `"},"reason":"r"}`)
	code, _, raw = c.post(t, authority.RevokeActorAction, c.op.Reveal(), revoke)
	if _, ok := answer(code, raw).(*types.CommandApplied); !ok {
		t.Fatalf("the revocation: %s", raw)
	}
	if code, _, _ := c.post(t, "seat.take", secret, body("t", `{}`)); code != http.StatusUnauthorized {
		t.Errorf("the revoked supervisor's bearer: HTTP %d", code)
	}
	_, raw2, _ := strings.Cut(secret, ".")
	secretsAbsent(t, &running{dir: c.dir, logs: c.logs}, raw2)
}

// An enrolment's mint takes one of the listener's Argon2 slots: with the
// last one taken after authentication, it is refused at once (503, fixed
// text) and nothing is recorded.
func TestEnrolMintHoldsASlot(t *testing.T) {
	c := runCommanded(t)
	gate := api.NewSlots(1, 0)
	h := commands{store: c.d.store, gate: gate, reg: c.d.az.Registry, logic: logics(c.d.az), issue: issuers(c.d.az), log: c.logs,
		authenticated: func(identity.Principal) {
			if _, ok := gate.Take(os.Getuid()); !ok {
				t.Error("the slot was not free after authentication")
			}
		}}
	body := `{"idempotency_key":"e","expected":{"version":0,"digest":null},"scope":{"actor":"` + newID(t, "act") + `","name":"a","declared_agent":null},"reason":"r"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/commands/"+authority.EnrolAction, strings.NewReader(body))
	req.SetPathValue("action", authority.EnrolAction)
	req.Header.Set("Authorization", "Bearer "+c.op.Reveal())
	req = req.WithContext(api.WithPeer(req.Context(), api.Peer{Via: identity.PrivateSocket, UID: os.Getuid()}))
	before := c.commandsOf(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || c.commandsOf(t) != before {
		t.Fatalf("HTTP %d %q", rec.Code, rec.Body.String())
	}
}
