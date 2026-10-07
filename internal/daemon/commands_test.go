// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/cli"
	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// commanded is a running daemon over a seeded store: an operator; sup, a
// supervisor with a seat grant; bare, one with no grant; review and human,
// ones whose grants tighten seat.take to that confirmation; each with a
// token. revokedTok is a revoked token of sup; gone, a revoked supervisor's.
type commanded struct {
	dir                   string
	d                     *Daemon
	logs                  *logBuffer
	opID, supID, bareID   types.ActorID
	op, sup, bare, review identity.Secret
	human, revokedTok     identity.Secret
	gone                  identity.Secret
	hook                  atomic.Pointer[func(identity.Principal)] // the endpoint's hook, when set
}

func runCommanded(t *testing.T) *commanded {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // never the user's real ~/.config/adc
	c := &commanded{dir: shortDir(t), logs: &logBuffer{}}
	w := seed(t, c.dir, daemonProjection(t))
	actor := func(kind types.ActorKind, by types.ActorID) types.Actor {
		a := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: kind, Name: string(kind), CreatedBy: by}
		if by == "" {
			a.CreatedBy = a.ID
		}
		w.do(a.CreatedBy, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(a) })
		return a
	}
	now := time.Now().UTC()
	token := func(to, by types.ActorID) (sec identity.Secret) {
		w.do(by, func(*eventstore.Tx) ([]eventstore.Event, error) {
			evs, s, err := identity.IssueToken(to, by, now)
			sec = s
			return evs, err
		})
		return sec
	}
	sys := actor(types.ActorKindSystem, "")
	op := actor(types.ActorKindOperator, sys.ID)
	c.opID, c.op = op.ID, token(op.ID, sys.ID)
	supervisor := func(override types.GrantConfirmationOverridesValue, grant bool) (types.ActorID, identity.Secret) {
		a := actor(types.ActorKindSupervisor, op.ID)
		g := types.Grant{ID: types.GrantID(newID(t, "grt")), Actor: a.ID, Projects: types.ProjectScope{All: true},
			Actions: []types.ActionKind{"seat.take", "seat.release", authority.WithdrawAction, authority.NoteAction}, Reads: types.GrantReads{Projects: types.GrantReadsProjects{Same: true}, Kinds: []types.Name{}},
			ValidFrom: types.Time(now.Add(-time.Hour).Format(time.RFC3339)), ExpiresAt: types.Time(now.Add(time.Hour).Format(time.RFC3339)),
			ConfirmationOverrides: map[string]types.GrantConfirmationOverridesValue{}, IssuedBy: op.ID}
		if override != "" {
			g.ConfirmationOverrides["seat.take"] = override
		}
		if grant {
			w.do(op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(tx, g) })
		}
		return a.ID, token(a.ID, op.ID)
	}
	var supID, goneID types.ActorID
	supID, c.sup = supervisor("", true)
	c.supID = supID
	c.bareID, c.bare = supervisor("", false)
	_, c.review = supervisor(types.GrantConfirmationOverridesValueIndependentReview, true)
	_, c.human = supervisor(types.GrantConfirmationOverridesValueHuman, true)
	c.revokedTok = token(supID, op.ID)
	goneID, c.gone = supervisor("", true)
	w.do(op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		id, _, _ := strings.Cut(c.revokedTok.Reveal(), ".")
		return identity.RevokeToken(tx, types.TokenID(id), now)
	})
	w.do(op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return identity.RevokeActor(tx, goneID, now) })
	w.done()
	ctx, cancel := context.WithCancel(context.Background())
	done, ready := make(chan error, 1), make(chan *Daemon, 1)
	cfg := Config{StateDir: c.dir, authenticated: func(p identity.Principal) {
		if f := c.hook.Load(); f != nil {
			(*f)(p)
		}
	}}
	go func() { done <- Run(ctx, cfg, c.logs, func(d *Daemon) { ready <- d }) }()
	select {
	case c.d = <-ready:
	case err := <-done:
		t.Fatalf("the daemon did not start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return c
}

// post sends body to action's command endpoint with bearer (none when
// empty), and returns the status, the Cache-Control header and the body.
func (c *commanded) post(t *testing.T, action, bearer string, body io.Reader) (int, string, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://adcd/v1/commands/"+action, body)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := unixClient(filepath.Join(c.dir, privateSocket)).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Cache-Control"), raw
}

func (c *commanded) commandsOf(t *testing.T) int {
	return count(t, c.dir, `SELECT count(*) FROM commands WHERE actor != (SELECT actor FROM events WHERE seq = 1)`)
}

// adc runs the CLI as the holder of tok, over the private socket.
func (c *commanded) adc(tok string, args ...string) (int, string, string) {
	env := map[string]string{cli.SocketEnv: filepath.Join(c.dir, privateSocket), client.TokenEnv: tok}
	var out, errb bytes.Buffer
	code := cli.Main(context.Background(), args, &out, &errb, cli.ClientFromEnv(func(k string) string { return env[k] }))
	return code, out.String(), errb.String()
}

// response decodes --json output, validated against the response schema.
func response(t *testing.T, out string) types.CommandResponseVariant {
	t.Helper()
	r, err := contract.Decode[types.CommandResponse]("api/command-response", []byte(out))
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return r.Variant
}

func refusedAs(t *testing.T, v types.CommandResponseVariant, o vocab.Outcome, reason vocab.Reason) {
	t.Helper()
	if r, ok := v.(*types.CommandRefused); !ok || string(r.Outcome) != string(o) || r.Reason != reason {
		t.Fatalf("want %s/%s, got %+v", o, reason, v)
	}
}

// The CLI against the running daemon: an applied command and its replay, a
// stale expectation, a reused key, a refusal recorded by the handler, an
// unwired action, an unknown flag and an unauthenticated caller, each with
// its exit code; --json is the schema's response.
func TestCommandsEndToEnd(t *testing.T) {
	c := runCommanded(t)
	sup, op := c.sup.Reveal(), c.op.Reveal()
	take := []string{"seat", "take", "--reason", "r", "--json"}

	code, out, errb := c.adc(sup, append(take, "--expect", "0", "--idempotency-key", "take-1")...)
	a, ok := response(t, out).(*types.CommandApplied)
	if code != entry.ExitOK || !ok || a.Replayed || a.LastSeq == 0 {
		t.Fatalf("take: %d %s %s", code, out, errb)
	}
	seat := strconv.FormatInt(a.LastSeq, 10)
	code, out, _ = c.adc(sup, append(take, "--expect", "0", "--idempotency-key", "take-1")...)
	if r, ok := response(t, out).(*types.CommandApplied); code != entry.ExitOK || !ok || !r.Replayed || r.Command != a.Command {
		t.Fatalf("replay: %d %s", code, out)
	}
	code, out, _ = c.adc(sup, append(take, "--expect", seat, "--idempotency-key", "take-1")...)
	if refusedAs(t, response(t, out), vocab.Failed, vocab.ReasonIdempotencyKeyReused); code != entry.ExitStale {
		t.Fatalf("reused key: %d", code)
	}
	code, out, _ = c.adc(op, append(take, "--expect", "0")...)
	if refusedAs(t, response(t, out), vocab.Failed, vocab.ReasonStaleExpectedState); code != entry.ExitStale {
		t.Fatalf("stale: %d", code)
	}
	code, out, _ = c.adc(op, "seat", "take", "--reason", "r", "--json", "--expect", "sha256:"+strings.Repeat("a", 64))
	if refusedAs(t, response(t, out), vocab.Failed, vocab.ReasonStaleExpectedState); code != entry.ExitStale {
		t.Fatalf("stale digest: %d", code)
	}
	// The expectation holds; the handler refuses an operator the seat.
	code, out, _ = c.adc(op, append(take, "--expect", seat)...)
	if refusedAs(t, response(t, out), vocab.Failed, vocab.ReasonSeatUnavailable); code != entry.ExitRefused {
		t.Fatalf("operator take: %d", code)
	}
	code, out, _ = c.adc(op, "task", "create", "--expect", "0", "--reason", "r", "--json")
	if refusedAs(t, response(t, out), vocab.CouldNotRun, vocab.ReasonActionDeferred); code != entry.ExitCouldNotRun {
		t.Fatalf("unwired: %d", code)
	}
	if code, out, errb := c.adc(op, append(take, "--expect", seat, "--bogus")...); code != entry.ExitUsage || out != "" || !strings.Contains(errb, "bogus") {
		t.Fatalf("unknown flag: %d %s %s", code, out, errb)
	}
	// Human output names the disposition.
	if code, out, _ := c.adc(sup, "seat", "release", "--expect", seat, "--reason", "r"); code != entry.ExitOK || !strings.HasPrefix(out, "applied: cmd_") {
		t.Fatalf("release: %d %s", code, out)
	}
	// A well-formed token never issued is refused, and nothing is printed of it.
	id, err := ids.New("tok")
	if err != nil {
		t.Fatal(err)
	}
	stranger := id + ".zzFAKEzzSECRETzzRECOGNIZABLEzzQQQQQQQQQQQQA"
	if code, out, errb := c.adc(stranger, append(take, "--expect", "0")...); code != entry.ExitRefused || out != "" || !strings.Contains(errb, "unauthenticated") || strings.Contains(errb, "zzFAKEzz") {
		t.Fatalf("unauthenticated: %d %s %s", code, out, errb)
	}
}

const good = `{"idempotency_key":"k","expected":{"version":0,"digest":null},"scope":{},"reason":"r"}`

// Requests that are never decided: no bearer, a revoked token, a revoked
// actor, a revocation between authentication and the writer's recheck (all
// 401), a body the request schema or the scope schema refuses, a query
// (400), an oversized body, declared or not (413); nothing is committed.
// An action outside the registry is a registered refusal, sent no-store.
func TestCommandsUndecided(t *testing.T) {
	c := runCommanded(t)
	sup := c.sup.Reveal()
	for name, tc := range map[string]struct {
		action, bearer, body string
		want                 int
	}{
		"no bearer":         {"seat.take", "", good, http.StatusUnauthorized},
		"a revoked token":   {"seat.take", c.revokedTok.Reveal(), good, http.StatusUnauthorized},
		"a revoked actor":   {"seat.take", c.gone.Reveal(), good, http.StatusUnauthorized},
		"a request extra":   {"seat.take", sup, strings.Replace(good, `"r"}`, `"r","actor":"x"}`, 1), http.StatusBadRequest},
		"a scope extra":     {"seat.take", sup, strings.Replace(good, `{}`, `{"force_after":"1s"}`, 1), http.StatusBadRequest},
		"a confirm scope":   {"proposal.confirm", c.op.Reveal(), strings.Replace(good, `{}`, `{"proposal":"x"}`, 1), http.StatusBadRequest},
		"a confirm extra":   {"proposal.confirm", c.op.Reveal(), strings.Replace(good, `{}`, `{"extra":1}`, 1), http.StatusBadRequest},
		"a query parameter": {"seat.take?as=x", sup, good, http.StatusBadRequest},
	} {
		if code, _, raw := c.post(t, tc.action, tc.bearer, strings.NewReader(tc.body)); code != tc.want {
			t.Errorf("%s: HTTP %d %s, want %d", name, code, raw, tc.want)
		}
	}
	big := strings.Repeat(" ", int(defaultLimits.bodyBytes)) + good
	if code, _, _ := c.post(t, "seat.take", sup, strings.NewReader(big)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("a declared oversized body: HTTP %d", code)
	}
	if code, _, _ := c.post(t, "seat.take", sup, io.MultiReader(strings.NewReader(big))); code != http.StatusRequestEntityTooLarge {
		t.Errorf("an undeclared oversized body: HTTP %d", code)
	}
	// The operator's token is revoked after it authenticated: the writer's
	// recheck refuses it, and the command is not committed.
	revoke := func(identity.Principal) {
		issued, err := identity.Mint(c.opID, c.d.system.Actor(), time.Now())
		if err != nil {
			t.Error(err)
			return
		}
		cmd := newID(t, "cmd")
		if _, err := c.d.store.SubmitApplied(context.Background(), c.d.system.Command(cmd, recoverAction, cmd, []byte(`{}`), nil), eventstore.Applies(
			func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				others, err := liveTokens(tx, c.opID)
				evs, eerr := identity.RecoverToken(issued, others)
				return evs, json.RawMessage(`{}`), errors.Join(err, eerr)
			})); err != nil {
			t.Error(err)
		}
	}
	before := c.commandsOf(t)
	c.hook.Store(&revoke)
	code, _, _ := c.post(t, "seat.take", c.op.Reveal(), strings.NewReader(good))
	c.hook.Store(nil)
	if code != http.StatusUnauthorized {
		t.Errorf("revoked between: HTTP %d", code)
	}
	if n := c.commandsOf(t); n != before {
		t.Errorf("%d commands committed", n-before)
	}
	code, cache, raw := c.post(t, "no.such", sup, strings.NewReader(good))
	if code != http.StatusOK || cache != "no-store" {
		t.Fatalf("unregistered: HTTP %d %q %s", code, cache, raw)
	}
	refusedAs(t, response(t, string(raw)), vocab.Failed, vocab.ReasonActionUnregistered)
}

// Authority through the endpoint: no grant, a release without the seat, a
// proposal forced by a grant override (each policy), and a confirmation
// the controller refuses.
func TestCommandsAuthority(t *testing.T) {
	c := runCommanded(t)
	take := []string{"seat", "take", "--expect", "0", "--reason", "r", "--json"}
	code, out, _ := c.adc(c.bare.Reveal(), take...)
	if refusedAs(t, response(t, out), vocab.Failed, vocab.ReasonActionNotGranted); code != entry.ExitRefused {
		t.Fatalf("no grant: %d", code)
	}
	code, out, _ = c.adc(c.sup.Reveal(), "seat", "release", "--expect", "0", "--reason", "r", "--json")
	if refusedAs(t, response(t, out), vocab.Failed, vocab.ReasonSeatNotHeld); code != entry.ExitRefused {
		t.Fatalf("release: %d", code)
	}
	code, out, _ = c.adc(c.review.Reveal(), take...)
	p, ok := response(t, out).(*types.CommandProposed)
	if code != entry.ExitProposed || !ok || p.Policy != types.CommandProposedPolicyIndependentReview {
		t.Fatalf("review: %d %s", code, out)
	}
	code, out, _ = c.adc(c.human.Reveal(), take[:len(take)-1]...)
	if code != entry.ExitProposed || !strings.Contains(out, "requested: seat.take {}") || !strings.Contains(out, cli.DashboardProposal+"prp_") {
		t.Fatalf("human: %d %s", code, out)
	}
	version := count(t, c.dir, `SELECT max(seq) FROM events WHERE subject = '`+string(p.Proposal)+`'`)
	confirm := func(proposal string) (int, string) {
		code, out, _ := c.adc(c.op.Reveal(), "approve", "confirm", "--expect", strconv.Itoa(version), "--reason", "r", "--json",
			"--scope", `{"proposal":"`+proposal+`","review":"`+newID(t, "rcp")+`"}`)
		return code, out
	}
	code, out = confirm(string(p.Proposal))
	if refusedAs(t, response(t, out), vocab.Failed, vocab.ReasonConfirmationRefused); code != entry.ExitRefused {
		t.Fatalf("confirm: %d", code)
	}
	// A fault no refusal names is logged with its detail and answered 503
	// with a fixed text.
	var logs bytes.Buffer
	fails := func(eventstore.Command) eventstore.Decide {
		return func(*eventstore.Tx) (eventstore.Decision, error) {
			return eventstore.Decision{}, errors.New("zzDETAILzz")
		}
	}
	h := commands{store: c.d.store, gate: api.Unlimited(), reg: c.d.az.Registry, logic: map[string]logic{"seat.take": fails}, log: &logs}
	req := httptest.NewRequest(http.MethodPost, "/v1/commands/seat.take", strings.NewReader(good))
	req.SetPathValue("action", "seat.take")
	req.Header.Set("Authorization", "Bearer "+c.sup.Reveal())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req) // no listener recorded: nothing authenticates
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no listener: HTTP %d", rec.Code)
	}
	req = req.WithContext(api.WithPeer(req.Context(), api.Peer{Via: identity.PrivateSocket, UID: os.Getuid()}))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "zzDETAILzz") || !strings.Contains(logs.String(), "adcd: command seat.take: zzDETAILzz") {
		t.Fatalf("a fault: HTTP %d %q, log %q", rec.Code, rec.Body.String(), logs.String())
	}
}

// proposal.withdraw through the endpoint: another supervisor, though its
// grant covers the action, is refused action_not_granted with nothing
// recorded; an invalid scope is a 400; the proposer's withdrawal applies.
func TestWithdrawThroughTheEndpoint(t *testing.T) {
	c := runCommanded(t)
	code, out, _ := c.adc(c.review.Reveal(), "seat", "take", "--expect", "0", "--reason", "r", "--json")
	p, ok := response(t, out).(*types.CommandProposed)
	if code != entry.ExitProposed || !ok {
		t.Fatalf("the proposal: %d %s", code, out)
	}
	version := strconv.Itoa(count(t, c.dir, `SELECT max(seq) FROM events WHERE subject = '`+string(p.Proposal)+`'`))
	body := func(key string) io.Reader {
		return strings.NewReader(`{"idempotency_key":"` + key + `","expected":{"version":` + version + `,"digest":null},"scope":{"proposal":"` + string(p.Proposal) + `"},"reason":"r"}`)
	}
	answer := func(code int, raw []byte) types.CommandResponseVariant {
		t.Helper()
		if code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", code, raw)
		}
		return response(t, string(raw))
	}
	events := func(kind string) int {
		return count(t, c.dir, `SELECT count(*) FROM events WHERE subject = '`+string(p.Proposal)+`' AND kind LIKE '`+kind+`'`)
	}
	before, all := c.commandsOf(t), events("%")
	bad := strings.NewReader(`{"idempotency_key":"w0","expected":{"version":0,"digest":null},"scope":{"proposal":"x"},"reason":"r"}`)
	if code, _, raw := c.post(t, authority.WithdrawAction, c.review.Reveal(), bad); code != http.StatusBadRequest {
		t.Errorf("an invalid scope: HTTP %d %s", code, raw)
	}
	code, _, raw := c.post(t, authority.WithdrawAction, c.sup.Reveal(), body("w1"))
	refusedAs(t, answer(code, raw), vocab.Failed, vocab.ReasonActionNotGranted)
	if c.commandsOf(t) != before || events("%") != all {
		t.Fatal("the stranger's withdrawal recorded something")
	}
	code, _, raw = c.post(t, authority.WithdrawAction, c.review.Reveal(), body("w2"))
	if a, ok := answer(code, raw).(*types.CommandApplied); !ok || a.Replayed {
		t.Fatalf("the proposer's withdrawal: %s", raw)
	}
	if n := events(authority.ProposalWithdrawn); n != 1 {
		t.Fatalf("proposal.withdrawn events: %d", n)
	}
}

// A deprecated action is refused as unregistered, before anything else.
func TestDeprecatedAction(t *testing.T) {
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	a := reg.Actions["seat.take"]
	since := "0.1.0"
	a.Deprecated = &since
	reg.Actions["seat.take"] = a
	v, err := commands{reg: reg}.decide(context.Background(), identity.Principal{}, "seat.take", types.CommandRequest{})
	if err != nil {
		t.Fatal(err)
	}
	refusedAs(t, v, vocab.Failed, vocab.ReasonActionUnregistered)
}

// A full verification gate answers 503 at once, with Retry-After.
func TestCommandsBusy(t *testing.T) {
	gate := api.NewSlots(1, 0)
	if _, ok := gate.Take(-1); !ok {
		t.Fatal("no slot")
	}
	rec := httptest.NewRecorder()
	commands{gate: gate}.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/commands/seat.take", strings.NewReader(good)))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("HTTP %d", rec.Code)
	}
}

// Every wired handler has its logic, and nothing else does.
func TestEveryHandlerHasLogic(t *testing.T) {
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	ls, is := logics(authorizer(reg)), issuers(authorizer(reg))
	for action := range handlers() {
		if (ls[action] == nil) == (is[action] == nil) {
			t.Errorf("%s has a handler and not exactly one of a logic and an issuer", action)
		}
	}
	if len(ls)+len(is) != len(handlers()) || is[authority.EnrolAction] == nil || is[authority.TokenIssueAction] == nil || is[authority.TokenReplaceAction] == nil || ls[authority.TokenRevokeAction] == nil || ls[authority.RevokeActorAction] == nil || ls[authority.WithdrawAction] == nil || ls[authority.NoteAction] == nil ||
		ls[authority.GrantCreateAction] == nil || ls[authority.GrantRevokeAction] == nil || ls[authority.GrantExtendAction] == nil {
		t.Errorf("logics %d, issuers %d, handlers %d", len(ls), len(is), len(handlers()))
	}
}

// The mapping of every submission outcome, including those no wired
// action reaches yet: an approval and effects, a delivered credential, an
// invalidation, a refusal whose reason the registry rejects, and the
// writer's refusals.
func TestAnswer(t *testing.T) {
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	h := commands{reg: reg}
	apr, eff := newID(t, "apr"), newID(t, "eff")
	v, err := h.answer(eventstore.Committed{Disposition: eventstore.DispositionApplied, CommandID: newID(t, "cmd"), Result: json.RawMessage(`{"approval":"` + apr + `"}`),
		FirstSeq: 3, LastSeq: 4, Effects: []eventstore.Effect{{ID: eff, State: eventstore.EffectPending}}}, nil)
	if a, ok := v.(*types.CommandApplied); err != nil || !ok || a.Approval == nil || string(*a.Approval) != apr || len(a.Effects) != 1 ||
		string(a.Effects[0].ID) != eff || a.Effects[0].State != "pending" || a.FirstSeq != 3 || a.LastSeq != 4 || a.IssuedCredential != nil {
		t.Fatalf("applied: %+v %v", v, err)
	}
	if _, err := contract.Encode("api/command-response", types.CommandResponse{Variant: v}); err != nil {
		t.Fatal(err)
	}
	tok := fakeSecret(t)
	if v, err := applied(eventstore.Committed{CommandID: newID(t, "cmd"), Result: json.RawMessage(`{}`)}, &tok); err != nil || *v.(*types.CommandApplied).IssuedCredential != tok.Reveal() {
		t.Fatalf("a credential: %+v %v", v, err)
	}
	v, err = h.answer(eventstore.Committed{Disposition: eventstore.DispositionInvalidated, Result: json.RawMessage(`{"outcome":"failed","reason":"proposal_invalidated","detail":"d"}`)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	refusedAs(t, v, vocab.Failed, vocab.ReasonProposalInvalidated)
	if _, err := h.answer(eventstore.Committed{Disposition: eventstore.DispositionRefused, Result: json.RawMessage(`{"outcome":"failed","reason":"ambiguous_state","detail":"d"}`)}, nil); err == nil {
		t.Error("a reason the registry rejects for its outcome")
	}
	// A stored refusal whose reason was deprecated since still replays.
	r := reg.Reasons[vocab.ReasonSeatNotHeld.String()]
	since := "0.1.0"
	r.Deprecated = &since
	reg.Reasons[vocab.ReasonSeatNotHeld.String()] = r
	stored := json.RawMessage(`{"outcome":"failed","reason":"seat_not_held","detail":"d"}`)
	if _, err := h.answer(eventstore.Committed{Disposition: eventstore.DispositionRefused, Replayed: true, Result: stored}, nil); err != nil {
		t.Errorf("a replayed refusal: %v", err)
	}
	if _, err := h.answer(eventstore.Committed{Disposition: eventstore.DispositionRefused, Result: stored}, nil); err == nil {
		t.Error("a fresh refusal with a deprecated reason")
	}
	for err, want := range map[error]vocab.Reason{
		fmt.Errorf("x: %w", eventstore.ErrIdempotencyKeyReused):          vocab.ReasonIdempotencyKeyReused,
		fmt.Errorf("x: %w", eventstore.ErrStaleExpectedState):            vocab.ReasonStaleExpectedState,
		&authority.Refusal{Reason: vocab.ReasonSeatNotHeld, Detail: "d"}: vocab.ReasonSeatNotHeld,
		fmt.Errorf("x: %w", authority.ErrActionNotWired):                 vocab.ReasonActionDeferred,
	} {
		v, aerr := h.answer(eventstore.Committed{}, err)
		if r, ok := v.(*types.CommandRefused); aerr != nil || !ok || r.Reason != want {
			t.Errorf("%v: %+v %v", err, v, aerr)
		}
	}
	if v, err := h.answer(eventstore.Committed{}, errors.New("other")); v != nil || err == nil {
		t.Errorf("an unmapped error: %+v %v", v, err)
	}
}

// A credential not in the issued form is refused before it is set, and a
// response carrying one that fails the schema is never logged with it.
func TestCredentialNeverLogged(t *testing.T) {
	if v, err := applied(eventstore.Committed{CommandID: newID(t, "cmd"), Result: json.RawMessage(`{}`)}, &identity.Secret{}); v != nil || err == nil {
		t.Fatalf("an empty credential: %+v %v", v, err)
	}
	var logs bytes.Buffer
	bad := "tok_zzLEAKEDzzCREDENTIALzz"
	rec := httptest.NewRecorder()
	commands{log: &logs}.respond(rec, &types.CommandApplied{Disposition: "applied", Command: types.CommandID(newID(t, "cmd")),
		Effects: []types.CommandAppliedEffectsItem{}, Result: json.RawMessage(`{}`), IssuedCredential: &bad})
	if rec.Code != http.StatusServiceUnavailable || logs.Len() == 0 || strings.Contains(logs.String()+rec.Body.String(), "zzLEAKEDzz") {
		t.Fatalf("HTTP %d %q, log %q", rec.Code, rec.Body.String(), logs.String())
	}
}

func fakeSecret(t *testing.T) identity.Secret {
	t.Helper()
	s, err := identity.ParseSecret(newID(t, "tok") + ".zzFAKEzzSECRETzzRECOGNIZABLEzzQQQQQQQQQQQQA")
	if err != nil {
		t.Fatal(err)
	}
	return s
}
