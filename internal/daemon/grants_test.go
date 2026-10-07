// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// 14 PR 7c through the daemon's own writer and handlers: an operator
// grants a supervisor and revokes the grant, and the history rebuilds.
func TestDaemonGrantsAndRevokes(t *testing.T) {
	dir := stateDir(t)
	w := seed(t, dir, daemonProjection(t, tokens(10)))
	sys := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
	sys.CreatedBy = sys.ID
	op := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindOperator, Name: "operator", CreatedBy: sys.ID}
	sup := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSupervisor, Name: "supervisor", CreatedBy: op.ID}
	var tok identity.Secret
	w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(sys) })
	w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(op) })
	w.do(op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(sup) })
	w.do(sys.ID, func(*eventstore.Tx) (evs []eventstore.Event, err error) {
		evs, tok, err = identity.IssueToken(op.ID, sys.ID, time.Now())
		return evs, err
	})
	w.done()
	d, err := Start(context.Background(), Config{StateDir: dir, Groups: []types.ResourceGroup{tokens(10)}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var p identity.Principal
	if err := d.store.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		p, err = identity.Authenticated(tx, tok.Reveal(), identity.PrivateSocket)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	grant := newID(t, "grt")
	create := p.Command(newID(t, "cmd"), authority.GrantCreateAction, "create", []byte(`{"id":"`+grant+`","actor":"`+string(sup.ID)+
		`","projects":"*","actions":["note.add"],"reads":{"projects":"same","kinds":[]},"expires_at":"`+
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`","confirmation_overrides":{}}`), nil)
	revoke := p.Command(newID(t, "cmd"), authority.GrantRevokeAction, "revoke", []byte(`{"grant":"`+grant+`"}`), nil)
	for _, c := range []eventstore.Command{create, revoke} {
		h, ok := handlers()[c.Action].(interface {
			Logic(eventstore.Command) eventstore.Decide
		})
		if !ok {
			t.Fatalf("%s is not wired with its logic", c.Action)
		}
		if out, err := d.store.Submit(context.Background(), c, h.Logic(c)); err != nil || out.Disposition != eventstore.DispositionApplied {
			t.Fatalf("%s: %+v %v", c.Action, out, err)
		}
	}
	if err := d.store.View(context.Background(), func(tx *eventstore.Tx) error {
		grants, revoked, err := authority.GrantsOf(tx, sup.ID)
		if err == nil && (len(grants) != 1 || !revoked[types.GrantID(grant)] || grants[0].IssuedBy != op.ID) {
			t.Errorf("the supervisor's grants: %+v %v", grants, revoked)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if diffs, err := d.store.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Errorf("the grant history does not rebuild: %v %v", diffs, err)
	}
}

// The grant actions through the command endpoint: an operator's
// grant.create is applied, a supervisor's grant.revoke is refused
// operator_only, and a scope the action's schema refuses is a 400.
func TestGrantCommands(t *testing.T) {
	c := runCommanded(t)
	grant := newID(t, "grt")
	body := func(scope string) *strings.Reader {
		return strings.NewReader(`{"idempotency_key":"` + newID(t, "cmd") + `","expected":{"version":0,"digest":null},"scope":` + scope + `,"reason":"r"}`)
	}
	create := `{"id":"` + grant + `","actor":"` + string(c.supID) + `","projects":"*","actions":[],"reads":{"projects":"same","kinds":[]},"expires_at":"` +
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `","confirmation_overrides":{}}`
	code, _, raw := c.post(t, authority.GrantCreateAction, c.op.Reveal(), body(create))
	if _, ok := response(t, string(raw)).(*types.CommandApplied); code != http.StatusOK || !ok {
		t.Fatalf("an operator's grant.create: HTTP %d %s", code, raw)
	}
	code, _, raw = c.post(t, authority.GrantRevokeAction, c.sup.Reveal(), body(`{"grant":"`+grant+`"}`))
	if code != http.StatusOK {
		t.Fatalf("a supervisor's grant.revoke: HTTP %d %s", code, raw)
	}
	refusedAs(t, response(t, string(raw)), vocab.Failed, vocab.ReasonOperatorOnly)
	before := c.commandsOf(t)
	invalid := strings.Replace(create, `"reads"`, `"valid_from":"2026-10-07T00:00:00Z","reads"`, 1)
	if code, _, raw := c.post(t, authority.GrantCreateAction, c.op.Reveal(), body(invalid)); code != http.StatusBadRequest {
		t.Errorf("a scope with valid_from: HTTP %d %s", code, raw)
	}
	if c.commandsOf(t) != before {
		t.Error("an invalid scope was committed")
	}
}

// grant.extend through the command endpoint: an operator's, bound to the
// old grant's version, is applied; a supervisor without a grant is refused
// action_not_granted; a scope naming the grantee is a 400.
func TestGrantExtendCommand(t *testing.T) {
	c := runCommanded(t)
	old, next := newID(t, "grt"), newID(t, "grt")
	body := func(version int64, scope string) *strings.Reader {
		return strings.NewReader(`{"idempotency_key":"` + newID(t, "cmd") + `","expected":{"version":` + strconv.FormatInt(version, 10) +
			`,"digest":null},"scope":` + scope + `,"reason":"r"}`)
	}
	grant := func(id, lead string) string {
		return `{` + lead + `"id":"` + id + `","projects":"*","actions":[],"reads":{"projects":"same","kinds":[]},"expires_at":"` +
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `","confirmation_overrides":{}}`
	}
	code, _, raw := c.post(t, authority.GrantCreateAction, c.op.Reveal(), body(0, grant(old, `"actor":"`+string(c.supID)+`",`)))
	created, ok := response(t, string(raw)).(*types.CommandApplied)
	if code != http.StatusOK || !ok {
		t.Fatalf("grant.create: HTTP %d %s", code, raw)
	}
	extend := grant(next, `"replaces":"`+old+`",`)
	code, _, raw = c.post(t, authority.GrantExtendAction, c.bare.Reveal(), body(created.LastSeq, extend))
	if code != http.StatusOK {
		t.Fatalf("a supervisor's grant.extend: HTTP %d %s", code, raw)
	}
	refusedAs(t, response(t, string(raw)), vocab.Failed, vocab.ReasonActionNotGranted)
	if code, _, raw := c.post(t, authority.GrantExtendAction, c.op.Reveal(), body(created.LastSeq, strings.Replace(extend, `{`, `{"actor":"`+string(c.supID)+`",`, 1))); code != http.StatusBadRequest {
		t.Errorf("a scope naming the grantee: HTTP %d %s", code, raw)
	}
	code, _, raw = c.post(t, authority.GrantExtendAction, c.op.Reveal(), body(created.LastSeq, extend))
	if _, ok := response(t, string(raw)).(*types.CommandApplied); code != http.StatusOK || !ok {
		t.Fatalf("an operator's grant.extend: HTTP %d %s", code, raw)
	}
}
