// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// httpsCommanded is c's store served over an HTTPS listener of its own,
// with the daemon's command logic, and a client from 127.0.0.1.
func httpsCommanded(t *testing.T, c *commanded) (*server, *http.Client, string) {
	t.Helper()
	cert := validCert(t)
	certFile, keyFile := cert.files(t, 0o600)
	tc, err := HTTPSListener{Addr: "127.0.0.1:1", CertFile: certFile, KeyFile: keyFile}.load(time.Now(), os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	srv := &server{lim: tlsLimits(), failed: make(chan error, 3),
		cmds: commands{store: c.d.store, reg: c.d.az.Registry, logic: logics(c.d.az), issue: issuers(c.d.az), log: c.logs}}
	srv.base, srv.stop = context.WithCancel(context.Background())
	if err := srv.listenHTTPS(c.d.store, "127.0.0.1:0", tc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.close() })
	return srv, tlsClient(t, cert, "127.0.0.1"), "https://" + srv.tcp.String()
}

// Commands over HTTPS, behind the listener's bearer check: no bearer is
// 401; an operator's agent.enrol applies, its mint under one of the HTTPS
// listener's verification slots for the peer's address (taken, it is
// refused with 503); a command on the connection whose bearer the
// listener verified takes no slot (actor.revoke applies with it taken).
func TestHTTPSCommands(t *testing.T) {
	c := runCommanded(t)
	srv, client, base := httpsCommanded(t, c)
	post := func(action, bearer, key, scope string, expected int64) (int, []byte) {
		t.Helper()
		body := `{"idempotency_key":"` + key + `","expected":{"version":` + strconv.FormatInt(expected, 10) + `,"digest":null},"scope":` + scope + `,"reason":"r"}`
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/commands/"+action, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}
	enrol := func(key string) string {
		return `{"actor":"` + newID(t, "act") + `","name":"agent-` + key + `","declared_agent":null}`
	}
	before := c.commandsOf(t)
	if code, _ := post(authority.EnrolAction, "", "e0", enrol("e0"), 0); code != http.StatusUnauthorized {
		t.Fatalf("no bearer: HTTP %d", code)
	}
	if c.commandsOf(t) != before {
		t.Fatal("a command without a bearer was recorded")
	}
	sup := newID(t, "act")
	code, raw := post(authority.EnrolAction, c.op.Reveal(), "e1", `{"actor":"`+sup+`","name":"agent","declared_agent":null}`, 0)
	a, ok := response(t, string(raw)).(*types.CommandApplied)
	if code != http.StatusOK || !ok || a.IssuedCredential == nil {
		t.Fatalf("the enrolment over HTTPS: HTTP %d %s", code, raw)
	}
	_, issued, _ := strings.Cut(*a.IssuedCredential, ".")
	if strings.Contains(c.logs.String(), issued) {
		t.Fatal("the issued credential is in the controller log")
	}
	key := api.Peer{Addr: netip.MustParseAddr("127.0.0.1")}.Key()
	strikes := strikesOf(srv.th, key) // the 401 without a bearer
	release, ok := srv.remote.Take(key)
	if !ok {
		t.Fatal("no verification slot for the peer")
	}
	code, raw = post(authority.RevokeActorAction, c.op.Reveal(), "r1", `{"actor":"`+sup+`"}`, a.FirstSeq)
	if _, ok := response(t, string(raw)).(*types.CommandApplied); code != http.StatusOK || !ok {
		t.Fatalf("a command on the verified connection, the peer's slot taken: HTTP %d %s", code, raw)
	}
	if code, raw := post(authority.EnrolAction, c.op.Reveal(), "e2", enrol("e2"), 0); code != http.StatusServiceUnavailable {
		t.Fatalf("an enrolment, the peer's HTTPS slot taken: HTTP %d %s", code, raw)
	}
	if n := strikesOf(srv.th, key); n != strikes {
		t.Fatalf("a busy mint's 503 counted: %d failures, was %d", n, strikes)
	}
	release()
	code, raw = post(authority.EnrolAction, c.op.Reveal(), "e2", enrol("e2"), 0)
	if a, ok := response(t, string(raw)).(*types.CommandApplied); code != http.StatusOK || !ok || a.IssuedCredential == nil {
		t.Fatalf("the enrolment, the slot free again: HTTP %d %s", code, raw)
	}
}

// A mint that panics gives its slot back: the peer's key is not locked out.
func TestMintPanicReleasesSlot(t *testing.T) {
	c := runCommanded(t)
	gate := api.NewSlots(1, 1)
	h := commands{store: c.d.store, gate: gate, reg: c.d.az.Registry, logic: logics(c.d.az), log: c.logs,
		issue: map[string]issuer{authority.EnrolAction: func(context.Context, *eventstore.Store, eventstore.Command) (eventstore.Decide, func(eventstore.Committed) (identity.Secret, bool), error) {
			panic("the issuer failed")
		}}}
	body := `{"idempotency_key":"e","expected":{"version":0,"digest":null},"scope":{"actor":"` + newID(t, "act") + `","name":"a","declared_agent":null},"reason":"r"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/commands/"+authority.EnrolAction, strings.NewReader(body))
	req.SetPathValue("action", authority.EnrolAction)
	req.Header.Set("Authorization", "Bearer "+c.op.Reveal())
	peer := api.Peer{Via: identity.HTTPS, UID: -1, Addr: netip.MustParseAddr("192.0.2.7")}
	req = req.WithContext(api.WithPeer(req.Context(), peer))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the issuer's panic did not reach the server")
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	release, ok := gate.Take(peer.Key())
	if !ok {
		t.Fatal("the slot was kept after the mint panicked")
	}
	release()
}

// A request whose bearer the listener verified (api.WithPrincipal) is
// authenticated from that principal, rechecked for revocation only: no
// verification slot and no second hash (the bearer is not even read).
func TestAuthenticateUsesVerifiedPrincipal(t *testing.T) {
	s := serveStore(t, defaultLimits, nil)
	var p identity.Principal
	if err := s.w.s.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		p, err = identity.Authenticated(tx, s.tok.Reveal(), identity.HTTPS)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	gate := api.NewSlots(1, 1)
	peer := api.Peer{Via: identity.HTTPS, UID: -1, Addr: netip.MustParseAddr("192.0.2.7")}
	release, _ := gate.Take(peer.Key())
	defer release()
	req, _ := http.NewRequestWithContext(api.WithPrincipal(api.WithPeer(t.Context(), peer), p), http.MethodPost, "/v1/commands/seat.take", nil)
	req.Header.Set("Authorization", "Bearer adc_not.this-one")
	got, err := api.Authenticate(s.w.s, req, gate)
	if err != nil || got.Actor() != p.Actor() {
		t.Fatalf("a verified principal, its slot taken and another bearer sent: %v %v", got.Actor(), err)
	}
}
