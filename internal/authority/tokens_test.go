// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// 14 M4, PR 7b: token.issue, token.revoke and token.replace through the
// Authorizer and the writer.

func openTokens(t *testing.T) *boundary {
	b := openAdmin(t)
	maps.Copy(b.az.Handlers, authority.TokenHandlers())
	return b
}

func tokenIn(id string) []byte        { return []byte(`{"token":"` + id + `"}`) }
func actorIn(id types.ActorID) []byte { return []byte(`{"actor":"` + string(id) + `"}`) }

func tokenID(s identity.Secret) string {
	id, _, _ := strings.Cut(s.Reveal(), ".")
	return id
}

// issue submits an issuing action (token.issue or token.replace) through
// its request layer, as the command endpoint does.
func (b *boundary) issue(p identity.Principal, action, key string, in []byte, expect *eventstore.State) (eventstore.Committed, identity.Secret, bool, error) {
	c := b.adminCmd(p, action, key, in, expect)
	request := b.az.TokenIssueRequest
	if action == authority.TokenReplaceAction {
		request = b.az.TokenReplaceRequest
	}
	decide, deliver, err := request(context.Background(), b.s, c)
	if err != nil {
		return eventstore.Committed{}, identity.Secret{}, false, err
	}
	out, err := b.s.Submit(context.Background(), c, decide)
	secret, ok := deliver(out)
	return out, secret, ok && err == nil, err
}

func (b *boundary) revokeToken(p identity.Principal, key, id string, expect *eventstore.State) (eventstore.Committed, error) {
	c := b.adminCmd(p, authority.TokenRevokeAction, key, tokenIn(id), expect)
	return b.s.Submit(context.Background(), c, b.az.RevokeToken(c))
}

// refusedAs requires a committed refusal with reason, and nothing else
// recorded.
func refusedAs(t *testing.T, name string, out eventstore.Committed, err error, reason vocab.Reason) {
	t.Helper()
	var res struct {
		Reason string `json:"reason"`
	}
	if err != nil || out.Disposition != eventstore.DispositionRefused || json.Unmarshal(out.Result, &res) != nil ||
		res.Reason != reason.String() || out.LastSeq != out.FirstSeq {
		t.Errorf("%s: %+v %v", name, out, err)
	}
}

// An operator issues a supervisor another token: delivered once, it
// authenticates as that supervisor beside its first; its result names the
// token, never the secret; a replay delivers and mints nothing.
func TestOperatorIssuesToken(t *testing.T) {
	b := openTokens(t)
	mints := authority.CountMints(t)
	sup := b.sup.Actor()
	at := b.versionOf(string(sup))
	out, secret, ok, err := b.issue(b.op, authority.TokenIssueAction, "k", actorIn(sup), version(at))
	if err != nil || out.Disposition != eventstore.DispositionApplied || !ok || out.LastSeq != out.FirstSeq {
		t.Fatalf("the issue: %+v %t %v", out, ok, err)
	}
	_, raw, _ := strings.Cut(secret.Reveal(), ".")
	if strings.Contains(string(out.Result), raw) || !strings.Contains(string(out.Result), tokenID(secret)) {
		t.Errorf("the result %s", out.Result)
	}
	for _, tok := range []identity.Secret{secret, b.supTok} {
		if a, err := b.authenticates(tok.Reveal()); err != nil || a.ID != sup {
			t.Errorf("a bearer of the supervisor: %+v %v", a, err)
		}
	}
	if rec := recordOf[types.Token](b, identity.TokenProjection, tokenID(secret)); rec.CreatedBy != b.op.Actor() || rec.CreatedAt != b.recorded[tokenID(secret)].At {
		t.Errorf("the token record: %+v", rec)
	}
	h := b.head()
	again, secret2, ok, err := b.issue(b.op, authority.TokenIssueAction, "k", actorIn(sup), version(at))
	if err != nil || !again.Replayed || ok || secret2.Reveal() != "" || b.head() != h || *mints != 1 {
		t.Errorf("the replay: %+v %t %v (%d mints)", again, ok, err, *mints)
	}
	if _, _, ok, err := b.issue(b.op, authority.TokenIssueAction, "k2", actorIn(sup), version(at-1)); !errors.Is(err, eventstore.ErrStaleExpectedState) || ok || b.head() != h {
		t.Errorf("a stale expectation: %t %v", ok, err)
	}
	b.rebuilds()
}

// No live holder, no token: a missing or revoked actor, the system actor;
// each a committed refusal, with no mint.
func TestIssueNeedsALiveHolder(t *testing.T) {
	b := openTokens(t)
	mints := authority.CountMints(t)
	gone := b.supervisor().Actor()
	if out, err := b.revoke(b.op, "gone", gone); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatal(out, err)
	}
	for name, a := range map[string]types.ActorID{"a missing actor": types.ActorID(newID(t, "act")), "a revoked actor": gone, "the system actor": b.sys.Actor()} {
		out, _, ok, err := b.issue(b.op, authority.TokenIssueAction, newID(t, "cmd"), actorIn(a), nil)
		refusedAs(t, name, out, err, vocab.ReasonTokenNotLive)
		if ok {
			t.Errorf("%s: delivered", name)
		}
	}
	if *mints != 0 {
		t.Errorf("%d mints for no holder", *mints)
	}
}

// Revoking a token refuses its bearer, and its streams' recheck closes
// them, while its holder keeps its actor, its other tokens and its grants.
// Revoking it again, or a token that never was, is a committed refusal.
// An operator's last token may go: offline recovery issues another
// (04 "Enrolment and revocation"), so this rule is not tightened.
func TestRevokeToken(t *testing.T) {
	b := openTokens(t)
	sup := b.sup.Actor()
	var second identity.Secret
	b.seed(b.op, issueTo(sup, b.op.Actor(), &second))
	h := b.head()
	at := b.versionOf(tokenID(b.supTok))
	if _, err := b.revokeToken(b.op, "stale", tokenID(b.supTok), version(at-1)); !errors.Is(err, eventstore.ErrStaleExpectedState) || b.head() != h {
		t.Errorf("a stale expectation: %v", err)
	}
	out, err := b.revokeToken(b.op, "r", tokenID(b.supTok), version(at))
	if err != nil || out.Disposition != eventstore.DispositionApplied || out.LastSeq != out.FirstSeq {
		t.Fatalf("the revocation: %+v %v", out, err)
	}
	if _, err := b.authenticates(b.supTok.Reveal()); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Errorf("the revoked bearer: %v", err)
	}
	b.view(func(tx *eventstore.Tx) error {
		if err := identity.StillLive(tx, b.supTok.Reveal(), sup); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Errorf("a stream's recheck: %v", err)
		}
		return nil
	})
	if a, err := b.authenticates(second.Reveal()); err != nil || a.ID != sup || a.RevokedAt != nil {
		t.Errorf("the holder's other token: %+v %v", a, err)
	}
	if out, err := b.note(b.noteCmd(b.login(second), "n", prjA)); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Errorf("the holder's grant under its other token: %+v %v", out, err)
	}
	again, err := b.revokeToken(b.op, "r2", tokenID(b.supTok), nil)
	refusedAs(t, "a revoked token", again, err, vocab.ReasonTokenNotLive)
	missing, err := b.revokeToken(b.op, "r3", newID(t, "tok"), nil)
	refusedAs(t, "a missing token", missing, err, vocab.ReasonTokenNotLive)
	if replay, err := b.revokeToken(b.op, "r2", tokenID(b.supTok), nil); err != nil || !replay.Replayed || replay.CommandID != again.CommandID {
		t.Errorf("the refusal's replay: %+v %v", replay, err)
	}
	if out, err := b.revokeToken(b.op, "own", tokenID(b.opTok), nil); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Errorf("the last operator's last token: %+v %v", out, err)
	}
	b.rebuilds()
}

// Rotation: one command revokes the token and issues its holder a new one,
// delivered once; the holder keeps its actor and grants, the old bearer is
// refused and its streams close. A replay delivers and mints nothing; a
// token already rotated is a committed refusal, with no mint.
func TestReplaceToken(t *testing.T) {
	b := openTokens(t)
	mints := authority.CountMints(t)
	sup := b.sup.Actor()
	at := b.versionOf(tokenID(b.supTok))
	out, fresh, ok, err := b.issue(b.op, authority.TokenReplaceAction, "k", tokenIn(tokenID(b.supTok)), version(at))
	if err != nil || out.Disposition != eventstore.DispositionApplied || !ok || out.LastSeq-out.FirstSeq != 1 {
		t.Fatalf("the rotation: %+v %t %v", out, ok, err)
	}
	var res struct {
		Actor    types.ActorID `json:"actor"`
		Token    string        `json:"token"`
		Replaced string        `json:"replaced"`
	}
	if err := json.Unmarshal(out.Result, &res); err != nil || res.Actor != sup || res.Token != tokenID(fresh) || res.Replaced != tokenID(b.supTok) {
		t.Errorf("the result %s %v", out.Result, err)
	}
	if _, err := b.authenticates(b.supTok.Reveal()); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Errorf("the replaced bearer: %v", err)
	}
	b.view(func(tx *eventstore.Tx) error {
		if err := identity.StillLive(tx, b.supTok.Reveal(), sup); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Errorf("the replaced bearer's stream: %v", err)
		}
		return identity.StillLive(tx, fresh.Reveal(), sup)
	})
	next := b.login(fresh)
	if next.Actor() != sup {
		t.Fatalf("the new token's actor: %s", next.Actor())
	}
	if c, err := b.note(b.noteCmd(next, "n", prjA)); err != nil || c.Disposition != eventstore.DispositionApplied {
		t.Errorf("the holder's grant under its new token: %+v %v", c, err)
	}
	h := b.head()
	again, secret, ok, err := b.issue(b.op, authority.TokenReplaceAction, "k", tokenIn(tokenID(b.supTok)), version(at))
	if err != nil || !again.Replayed || ok || secret.Reveal() != "" || b.head() != h {
		t.Errorf("the replay: %+v %t %v", again, ok, err)
	}
	refused, _, ok, err := b.issue(b.op, authority.TokenReplaceAction, "k2", tokenIn(tokenID(b.supTok)), nil)
	refusedAs(t, "a replaced token", refused, err, vocab.ReasonTokenNotLive)
	if ok || *mints != 1 {
		t.Errorf("a refused rotation delivered (%t) or minted (%d)", ok, *mints)
	}
	b.rebuilds()
}

// A token revoked after the request layer read it live (and minted) is
// refused inside the writer, and the token minted is neither recorded nor
// delivered.
func TestReplaceRechecksInTheWriter(t *testing.T) {
	b := openTokens(t)
	c := b.adminCmd(b.op, authority.TokenReplaceAction, "k", tokenIn(tokenID(b.supTok)), nil)
	decide, deliver, err := b.az.TokenReplaceRequest(context.Background(), b.s, c)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := b.revokeToken(b.op, "r", tokenID(b.supTok), nil); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatal(out, err)
	}
	out, err := b.s.Submit(context.Background(), c, decide)
	refusedAs(t, "the revoked token", out, err, vocab.ReasonTokenNotLive)
	if _, ok := deliver(out); ok {
		t.Error("a refused rotation delivered its token")
	}
	if n, _ := b.tokens(); n != 2 {
		t.Errorf("%d tokens recorded, want the operator's and the supervisor's", n)
	}
}

// versionOf is subject's version, which an expectation names.
func (b *boundary) versionOf(subject string) (v int64) {
	b.view(func(tx *eventstore.Tx) (err error) {
		v, err = tx.SubjectVersion(subject)
		return err
	})
	return v
}

// tokens counts the token records, and the live ones.
func (b *boundary) tokens() (n, live int) {
	b.view(func(tx *eventstore.Tx) error {
		keys, err := tx.Keys(identity.TokenProjection, "")
		for _, k := range keys {
			raw, _, gerr := tx.Get(identity.TokenProjection, k)
			var rec types.Token
			if err = errors.Join(err, gerr, json.Unmarshal(raw, &rec)); rec.RevokedAt == nil {
				live++
			}
			n++
		}
		return err
	})
	return n, live
}

// Only an operator administers tokens: a supervisor (granted all three and
// seated, even for its own token) and the system actor are refused before
// any logic runs or any token is minted.
func TestSupervisorTokenAdminRefused(t *testing.T) {
	b := openTokens(t)
	mints := authority.CountMints(t)
	g := b.supGrant
	g.ID, g.Actions = types.GrantID(newID(t, "grt")), []types.ActionKind{authority.TokenIssueAction, authority.TokenRevokeAction, authority.TokenReplaceAction}
	b.seed(b.op, createGrant(g))
	own := tokenIn(tokenID(b.supTok))
	for _, p := range []identity.Principal{b.sup, b.sys} {
		for action, in := range map[string][]byte{authority.TokenIssueAction: actorIn(b.sup.Actor()), authority.TokenRevokeAction: own, authority.TokenReplaceAction: own} {
			b.refusedUnrun(string(p.Actor())+" "+action, b.adminCmd(p, action, newID(t, "cmd"), in, nil), isReason(vocab.ReasonOperatorOnly))
		}
		for _, action := range []string{authority.TokenIssueAction, authority.TokenReplaceAction} {
			in := own
			if action == authority.TokenIssueAction {
				in = actorIn(b.sup.Actor())
			}
			if _, _, ok, err := b.issue(p, action, newID(t, "cmd"), in, nil); !isReason(vocab.ReasonOperatorOnly)(err) || ok {
				t.Errorf("%s through its request layer: %v", action, err)
			}
		}
	}
	if *mints != 0 {
		t.Errorf("%d mints", *mints)
	}
	if _, err := b.authenticates(b.supTok.Reveal()); err != nil {
		t.Fatal(err)
	}
}

// Each scope is checked against its schema; each request layer and logic
// serves its own action only.
func TestTokenScopesAndActionsGuarded(t *testing.T) {
	b := openTokens(t)
	mints := authority.CountMints(t)
	for action, in := range map[string]string{
		authority.TokenIssueAction:   `{"actor":"` + newID(t, "tok") + `"}`,
		authority.TokenRevokeAction:  `{"token":"` + newID(t, "act") + `"}`,
		authority.TokenReplaceAction: `{"token":"` + tokenID(b.supTok) + `","actor":"` + string(b.sup.Actor()) + `"}`,
	} {
		b.refusedUnrun(action, b.adminCmd(b.op, action, newID(t, "cmd"), []byte(in), nil), is(authority.ErrInvalidScope))
	}
	h := b.head()
	issue := b.adminCmd(b.op, authority.TokenReplaceAction, "a", actorIn(b.sup.Actor()), nil)
	if _, _, err := b.az.TokenIssueRequest(context.Background(), b.s, issue); err == nil || !strings.Contains(err.Error(), "is not token.issue") {
		t.Errorf("TokenIssueRequest for another action: %v", err)
	}
	replace := b.adminCmd(b.op, authority.TokenIssueAction, "b", tokenIn(tokenID(b.supTok)), nil)
	if _, _, err := b.az.TokenReplaceRequest(context.Background(), b.s, replace); err == nil || !strings.Contains(err.Error(), "is not token.replace") {
		t.Errorf("TokenReplaceRequest for another action: %v", err)
	}
	seed := b.adminCmd(b.op, "test.seed", "c", tokenIn(tokenID(b.supTok)), nil)
	if _, err := b.s.Submit(context.Background(), seed, b.az.RevokeToken(seed)); err == nil || !strings.Contains(err.Error(), "is not token.revoke") {
		t.Errorf("RevokeToken for another action: %v", err)
	}
	if b.head() != h || *mints != 0 {
		t.Errorf("a guarded call wrote or minted (%d)", *mints)
	}
}
