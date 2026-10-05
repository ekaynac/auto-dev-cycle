// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	applied  = `{"disposition":"applied","command":"cmd_01m3250vz2ezn81cgvbkcn17jd","replayed":false,"approval":null,"first_seq":4,"last_seq":4,"effects":[],"result":{}}`
	human    = `{"disposition":"proposed","proposal":"prp_01m3250vm7f6ktw39dszqnyddr","policy":"human","binding_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	review   = `{"disposition":"proposed","proposal":"prp_01m3250vm7f6ktw39dszqnyddr","policy":"independent_review","binding_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	notFound = `{"disposition":"refused","outcome":"failed","reason":"seat_not_held","detail":"held by another supervisor"}`
)

func refused(outcome, reason string) string {
	return `{"disposition":"refused","outcome":"` + outcome + `","reason":"` + reason + `","detail":"d"}`
}

type fake struct {
	resp   string
	err    error
	calls  int
	action string
	req    types.CommandRequest
}

func (f *fake) Command(_ context.Context, action string, body []byte) ([]byte, error) {
	f.calls++
	f.action, f.req = action, types.CommandRequest{}
	if err := json.Unmarshal(body, &f.req); err != nil {
		return nil, err
	}
	return []byte(f.resp), f.err
}

func adc(f *fake, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(context.Background(), args, &out, &errb, f)
	return code, out.String(), errb.String()
}

// Every action has exactly one command, under its family; nothing else is a
// command (the CLI half of the parity check, 08 "Dashboard parity").
func TestEveryActionHasOneCommand(t *testing.T) {
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := slices.Sorted(maps.Keys(commands)), slices.Sorted(maps.Keys(reg.Actions)); !slices.Equal(got, want) {
		t.Fatalf("commands %v, actions %v", got, want)
	}
	seen := map[command]string{}
	verb := regexp.MustCompile(`^[a-z]+(-[a-z]+)*$`)
	for action, c := range commands {
		if c.noun != string(reg.Actions[action].Family) || !verb.MatchString(c.verb) {
			t.Errorf("%s: %v", action, c)
		}
		if other, dup := seen[c]; dup {
			t.Errorf("%s and %s share %v", action, other, c)
		}
		seen[c] = action
		if got, ok := actionFor(c.noun, c.verb); !ok || got != action {
			t.Errorf("%v names %s", c, got)
		}
	}
}

// Each command reaches the action it names (the table pinned by meaning).
func TestCommandsReachTheirActions(t *testing.T) {
	for cmd, action := range map[string]string{
		"project create": "project.create", "project adopt-profile": "profile.adopt",
		"project change-protected-document": "knowledge.change_protected_document", "project change-topology": "integration.change_topology",
		"task create": "task.create", "task request-review": "review.request", "holds answer": "input.answer",
		"approve confirm": "proposal.confirm", "propose withdraw": "proposal.withdraw",
		"grant create": "grant.create", "grant extend": "grant.extend", "grant revoke": "grant.revoke",
		"admin enrol-agent": "agent.enrol", "admin revoke-actor": "actor.revoke",
		"seat take": "seat.take", "seat release": "seat.release", "notes add": "note.add",
	} {
		f := &fake{resp: applied}
		if code, _, _ := adc(f, append(strings.Fields(cmd), "--expect", "1", "--reason", "r")...); code != entry.ExitOK || f.action != action {
			t.Errorf("adc %s: exit %d, action %q, want %q", cmd, code, f.action, action)
		}
	}
}

func TestEnvelope(t *testing.T) {
	f := &fake{resp: applied}
	code, out, _ := adc(f, "grant", "extend", "--expect", "12", "--reason", "runs past expiry", "--scope", `{"grant":"grt_01m3250vkwffc979nzjw0g2sf9"}`)
	if code != entry.ExitOK || f.action != "grant.extend" || *f.req.Expected.Version != 12 || f.req.Expected.Digest != nil ||
		f.req.Reason != "runs past expiry" || string(f.req.Scope) != `{"grant":"grt_01m3250vkwffc979nzjw0g2sf9"}` {
		t.Fatalf("%d %+v", code, f)
	}
	if !regexp.MustCompile(`^cli-[0-9a-f]{32}$`).MatchString(f.req.IdempotencyKey) || !strings.HasPrefix(out, "applied: cmd_") {
		t.Fatalf("key %q, output %q", f.req.IdempotencyKey, out)
	}
	first := f.req.IdempotencyKey
	adc(f, "seat", "release", "--expect", "sha256:"+strings.Repeat("b", 64), "--reason", "r")
	if f.req.IdempotencyKey == first || f.req.Expected.Version != nil || string(*f.req.Expected.Digest) != "sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("%+v", f.req)
	}
	adc(f, "notes", "add", "--expect", "0", "--reason", "r", "--idempotency-key", "retry-7")
	if f.req.IdempotencyKey != "retry-7" || string(f.req.Scope) != "{}" {
		t.Fatalf("%+v", f.req)
	}
}

func TestUsage(t *testing.T) {
	ok := []string{"--expect", "1", "--reason", "r"}
	for name, args := range map[string][]string{
		"missing verb":           {"grant"},
		"unknown verb":           {"grant", "destroy"},
		"no --expect":            {"grant", "revoke", "--reason", "r"},
		"a negative version":     {"grant", "revoke", "--expect", "-1", "--reason", "r"},
		"a word for --expect":    {"grant", "revoke", "--expect", "latest", "--reason", "r"},
		"a malformed digest":     {"grant", "revoke", "--expect", "sha256:zz", "--reason", "r"},
		"no reason":              {"grant", "revoke", "--expect", "1"},
		"a reason not UTF-8":     {"grant", "revoke", "--expect", "1", "--reason", "bad\xff"},
		"scope not JSON":         append([]string{"grant", "revoke", "--scope", "{"}, ok...),
		"scope not an object":    append([]string{"grant", "revoke", "--scope", "[]"}, ok...),
		"a spaced key":           append([]string{"grant", "revoke", "--idempotency-key", "a b"}, ok...),
		"an unknown flag":        append([]string{"grant", "revoke", "--force"}, ok...),
		"an extra argument":      append([]string{"grant", "revoke"}, append(ok, "grt_x")...),
		"an unknown top command": {"frobnicate"},
	} {
		f := &fake{resp: applied}
		if code, _, _ := adc(f, args...); code != entry.ExitUsage || f.calls != 0 {
			t.Errorf("%s: exit %d after %d calls", name, code, f.calls)
		}
	}
	// The revision is the caller's to name, never fetched [M13].
	if _, _, errs := adc(&fake{resp: applied}, "grant", "revoke", "--reason", "r"); !strings.Contains(errs, "--expect is required") {
		t.Errorf("no --expect: %q", errs)
	}
}

func TestExitCodes(t *testing.T) {
	for name, c := range map[string]struct {
		resp string
		err  error
		want int
	}{
		"applied":                                {resp: applied, want: entry.ExitOK},
		"proposed (human)":                       {resp: human, want: entry.ExitProposed},
		"proposed (review)":                      {resp: review, want: entry.ExitProposed},
		"refused":                                {resp: notFound, want: entry.ExitRefused},
		"stale expected state":                   {resp: refused("failed", "stale_expected_state"), want: entry.ExitStale},
		"stale attempt":                          {resp: refused("failed", "stale_attempt"), want: entry.ExitStale},
		"idempotency key reused":                 {resp: refused("failed", "idempotency_key_reused"), want: entry.ExitStale},
		"could not run":                          {resp: refused("could_not_run", "ambiguous_state"), want: entry.ExitCouldNotRun},
		"transport error":                        {err: errors.New("connection refused"), want: entry.ExitCouldNotRun},
		"invalid response":                       {resp: refused("found", "stale_attempt"), want: entry.ExitCouldNotRun},
		"not JSON":                               {resp: "<html>", want: entry.ExitCouldNotRun},
		"an applied response missing its fields": {resp: `{"disposition":"applied","command":"cmd_01m3250vz2ezn81cgvbkcn17jd"}`, want: entry.ExitCouldNotRun},
		"a reason with the wrong outcome":        {resp: refused("failed", "ambiguous_state"), want: entry.ExitCouldNotRun},
	} {
		code, _, errs := adc(&fake{resp: c.resp, err: c.err}, "seat", "take", "--expect", "3", "--reason", "r")
		if code != c.want || (c.err != nil && !strings.Contains(errs, c.err.Error())) ||
			(c.want == entry.ExitCouldNotRun && c.resp != refused("could_not_run", "ambiguous_state") && !strings.Contains(errs, "--idempotency-key cli-")) {
			t.Errorf("%s: exit %d, want %d (%s)", name, code, c.want, errs)
		}
	}
}

func TestOutput(t *testing.T) {
	code, out, _ := adc(&fake{resp: human}, "approve", "confirm", "--expect", "5", "--reason", "r", "--json")
	if code != entry.ExitProposed || out != human+"\n" || !json.Valid([]byte(out)) {
		t.Fatalf("%d %q", code, out)
	}
	if _, out, _ := adc(&fake{resp: human}, "approve", "confirm", "--expect", "5", "--reason", "r"); !strings.Contains(out, "dashboard") {
		t.Errorf("a human confirmation does not point to the dashboard: %q", out)
	}
	if _, out, _ := adc(&fake{resp: review}, "approve", "confirm", "--expect", "5", "--reason", "r"); strings.Contains(out, "dashboard") {
		t.Errorf("an independent review points to the dashboard: %q", out)
	}
	if _, out, errs := adc(&fake{err: errors.New("down")}, "seat", "take", "--expect", "3", "--reason", "r", "--json"); out != "" || !strings.Contains(errs, "could_not_run") {
		t.Errorf("a transport failure: stdout %q, stderr %q", out, errs)
	}
	if code, _, _ := adc(&fake{}, "seat", "take", "-h"); code != entry.ExitOK {
		t.Errorf("help on an action: exit %d", code)
	}
	// Anything that is not an action command is entry's.
	if code, out, _ := adc(&fake{}, "version"); code != entry.ExitOK || out == "" {
		t.Errorf("version: %d %q", code, out)
	}
}
