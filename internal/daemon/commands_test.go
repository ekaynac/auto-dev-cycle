// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
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

// commanded is a running daemon over a store seeded with an operator and a
// supervisor holding a seat grant, each with a token.
type commanded struct {
	dir     string
	op, sup identity.Secret
}

func runCommanded(t *testing.T) *commanded {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // never the user's real ~/.config/adc
	c := &commanded{dir: shortDir(t)}
	w := seed(t, c.dir, daemonProjection(t))
	sys := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
	sys.CreatedBy = sys.ID
	op := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindOperator, Name: "operator", CreatedBy: sys.ID}
	sup := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSupervisor, Name: "supervisor", CreatedBy: op.ID}
	now := time.Now().UTC()
	g := types.Grant{ID: types.GrantID(newID(t, "grt")), Actor: sup.ID, Projects: types.ProjectScope{All: true},
		Actions: []types.ActionKind{"seat.take", "seat.release"}, Reads: types.GrantReads{Projects: types.GrantReadsProjects{Same: true}, Kinds: []types.Name{}},
		ValidFrom: types.Time(now.Add(-time.Hour).Format(time.RFC3339)), ExpiresAt: types.Time(now.Add(time.Hour).Format(time.RFC3339)),
		ConfirmationOverrides: map[string]types.GrantConfirmationOverridesValue{}, IssuedBy: op.ID, Approval: types.ApprovalID(newID(t, "apr"))}
	for _, a := range []types.Actor{sys, op, sup} {
		w.do(a.CreatedBy, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(a) })
	}
	w.do(op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(g) })
	for _, to := range []struct {
		id, by types.ActorID
		tok    *identity.Secret
	}{{op.ID, sys.ID, &c.op}, {sup.ID, op.ID, &c.sup}} {
		w.do(to.by, func(*eventstore.Tx) ([]eventstore.Event, error) {
			evs, sec, err := identity.IssueToken(to.id, to.by, now)
			*to.tok = sec
			return evs, err
		})
	}
	w.done()
	ctx, cancel := context.WithCancel(context.Background())
	done, ready := make(chan error, 1), make(chan struct{})
	go func() { done <- Run(ctx, Config{StateDir: c.dir}, &logBuffer{}, func(*Daemon) { close(ready) }) }()
	select {
	case <-ready:
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

// Without a bearer, or with a body the request schema refuses, nothing is
// decided.
func TestCommandsUndecided(t *testing.T) {
	c := runCommanded(t)
	action, last := "seat.take", []byte(nil)
	post := func(bearer, body string) int {
		req, err := http.NewRequest(http.MethodPost, "http://adcd/v1/commands/"+action, strings.NewReader(body))
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
		last, _ = io.ReadAll(resp.Body)
		return resp.StatusCode
	}
	good := `{"idempotency_key":"k","expected":{"version":0,"digest":null},"scope":{},"reason":"r"}`
	for name, got := range map[string]int{
		"no bearer":       post("", good),
		"a request extra": post(c.sup.Reveal(), `{"idempotency_key":"k","expected":{"version":0,"digest":null},"scope":{},"reason":"r","actor":"x"}`),
		"a scope extra":   post(c.sup.Reveal(), `{"idempotency_key":"k","expected":{"version":0,"digest":null},"scope":{"force_after":"1s"},"reason":"r"}`),
	} {
		want := http.StatusBadRequest
		if name == "no bearer" {
			want = http.StatusUnauthorized
		}
		if got != want {
			t.Errorf("%s: HTTP %d, want %d", name, got, want)
		}
	}
	// An action outside the registry is a registered refusal.
	action = "no.such"
	if code := post(c.sup.Reveal(), good); code != http.StatusOK {
		t.Fatalf("unregistered: HTTP %d %s", code, last)
	}
	refusedAs(t, response(t, string(last)), vocab.Failed, vocab.ReasonActionUnregistered)
	// None of them took the seat.
	code, out, _ := c.adc(c.sup.Reveal(), "seat", "take", "--expect", "0", "--reason", "r", "--json")
	if _, ok := response(t, out).(*types.CommandApplied); code != entry.ExitOK || !ok {
		t.Fatalf("take after: %d %s", code, out)
	}
}

// Every wired handler has its logic, and nothing else does.
func TestEveryHandlerHasLogic(t *testing.T) {
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	ls := logics(reg)
	for action := range handlers() {
		if ls[action] == nil {
			t.Errorf("%s has a handler and no logic", action)
		}
	}
	if len(ls) != len(handlers()) {
		t.Errorf("logics %d, handlers %d", len(ls), len(handlers()))
	}
}
