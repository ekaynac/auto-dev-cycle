// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
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
		"admin issue-token": "token.issue", "admin revoke-token": "token.revoke", "admin replace-token": "token.replace",
		"seat take": "seat.take", "seat release": "seat.release", "notes add": "note.add",
	} {
		f, args := &fake{resp: applied}, append(strings.Fields(cmd), "--expect", "1", "--reason", "r")
		if _, issues := issuing[action]; issues {
			f.resp, args = issuedResp, append(args, "--token-out", filepath.Join(privateDir(t), "t"))
		}
		if code, _, _ := adc(f, args...); code != entry.ExitOK || f.action != action {
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

// A human-policy proposal prints its id, the effective change and where
// the dashboard confirms it.
func TestHumanProposal(t *testing.T) {
	_, out, _ := adc(&fake{resp: human}, "grant", "extend", "--expect", "5", "--reason", "r", "--scope", `{"grant":"grt_01m3250vkwffc979nzjw0g2sf9"}`)
	for _, want := range []string{"prp_01m3250vm7f6ktw39dszqnyddr", `requested: grant.extend {"grant":"grt_01m3250vkwffc979nzjw0g2sf9"}`, DashboardProposal + "prp_01m3250vm7f6ktw39dszqnyddr"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in %q", want, out)
		}
	}
}

const issuedTok = "tok_01m3250vkwffc979nzjw0g2sa1.zzFAKEzzSECRETzzRECOGNIZABLEzzQQQQQQQQQQQQA"

var issuedResp = strings.TrimSuffix(applied, "}") + `,"issued_credential":"` + issuedTok + `"}`

// privateDir is a new directory of mode 0700, whatever the umask.
func privateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ownToken writes the caller's own token file in a fresh HOME and returns
// its path and bytes, to show they never change.
func ownToken(t *testing.T) (string, []byte) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	own, err := identity.ParseSecret("tok_01m3250vkwffc979nzjw0g2sf9.ownOWNownOWNownOWNownOWNownOWNownOWNownOWNA")
	if err == nil {
		err = client.Files{Home: home, UID: os.Getuid()}.Write(own)
	}
	path := filepath.Join(home, ".config", "adc", "token")
	before, rerr := os.ReadFile(path)
	if err = errors.Join(err, rerr); err != nil {
		t.Fatal(err)
	}
	return path, before
}

// An issuing command's token goes to --token-out alone (0600, the token
// and a newline), in either output mode: never to stdout or stderr, and
// never over the caller's own token file. --json prints the response
// without it.
func TestIssuedCredentialToTokenOut(t *testing.T) {
	own, before := ownToken(t)
	for _, mode := range [][]string{nil, {"--json"}} {
		path := filepath.Join(privateDir(t), "agent.token")
		code, out, errs := adc(&fake{resp: issuedResp}, append([]string{"admin", "enrol-agent", "--expect", "0", "--reason", "r", "--token-out", path}, mode...)...)
		if code != entry.ExitOK || strings.Contains(out+errs, "zzFAKEzz") || !strings.Contains(errs, path) {
			t.Fatalf("%v: %d %q %q", mode, code, out, errs)
		}
		if mode != nil {
			var v map[string]any
			if err := json.Unmarshal([]byte(out), &v); err != nil || v["disposition"] != "applied" || v["issued_credential"] != nil {
				t.Fatalf("--json: %q %v", out, err)
			}
		}
		st, err := os.Stat(path)
		got, rerr := os.ReadFile(path)
		if err != nil || rerr != nil || st.Mode() != 0o600 || string(got) != issuedTok+"\n" {
			t.Fatalf("%v: --token-out %v %v %v", mode, st, err, rerr)
		}
		if after, err := os.ReadFile(own); err != nil || !bytes.Equal(after, before) {
			t.Fatalf("%v: the caller's token file changed (%v)", mode, err)
		}
	}
}

// --token-out is required by an issuing command and taken by no other,
// and an existing file (the caller's own token among them) is refused:
// each a usage error before anything is sent.
func TestTokenOutUsage(t *testing.T) {
	own, before := ownToken(t)
	ok := []string{"--expect", "0", "--reason", "r"}
	for name, args := range map[string][]string{
		"none for enrol-agent":       append([]string{"admin", "enrol-agent"}, ok...),
		"one for revoke-actor":       append([]string{"admin", "revoke-actor", "--token-out", filepath.Join(privateDir(t), "t")}, ok...),
		"none for issue-token":       append([]string{"admin", "issue-token"}, ok...),
		"none for replace-token":     append([]string{"admin", "replace-token"}, ok...),
		"one for revoke-token":       append([]string{"admin", "revoke-token", "--token-out", filepath.Join(privateDir(t), "t")}, ok...),
		"the caller's own token":     append([]string{"admin", "enrol-agent", "--token-out", own}, ok...),
		"an unsafe directory":        append([]string{"admin", "enrol-agent", "--token-out", filepath.Join(t.TempDir(), "x", "t")}, ok...),
		"one for a seat command too": append([]string{"seat", "take", "--token-out", filepath.Join(privateDir(t), "t")}, ok...),
	} {
		f := &fake{resp: issuedResp}
		code, _, errs := adc(f, args...)
		if code != entry.ExitUsage || f.calls != 0 || (strings.HasPrefix(name, "none") && !strings.Contains(errs, "--token-out FILE is required")) {
			t.Errorf("%s: exit %d after %d calls (%s)", name, code, f.calls, errs)
		}
	}
	if after, err := os.ReadFile(own); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("the caller's token file changed (%v)", err)
	}
}

// When no token arrives, the reserved --token-out is removed and the
// recovery is named: a lost response, a repeat of an applied command
// (whose token id it reports), a refusal, and a token that could not be
// written; a credential a non-issuing command was sent is never written.
func TestTokenNotDelivered(t *testing.T) {
	own, before := ownToken(t)
	replayed := strings.Replace(applied, `"replayed":false`, `"replayed":true`, 1)
	replayed = strings.Replace(replayed, `"result":{}`, `"result":{"actor":"act_01m3250vkwffc979nzjw0g2sa1","token":"tok_01m3250vkwffc979nzjw0g2sb2"}`, 1)
	defer func(old func(*client.TokenOut, identity.Secret) error) { writeToken = old }(writeToken)
	writeToken = func(o *client.TokenOut, tok identity.Secret) error {
		if strings.Contains(tok.Reveal(), "FAIL") {
			o.Discard()
			return errors.New("disk full")
		}
		return o.Write(tok)
	}
	failing := strings.Replace(issuedResp, "zzFAKEzz", "zzFAILzz", 1)
	for name, c := range map[string]struct {
		cmd, resp string
		err       error
		code      int
		want      string
	}{
		"a lost enrolment":       {"enrol-agent", "", errors.New("connection reset"), entry.ExitCouldNotRun, "a repeat delivers no token: revoke the actor (adc admin revoke-actor) and enrol it again under a new actor id"},
		"a repeated enrolment":   {"enrol-agent", replayed, nil, entry.ExitCouldNotRun, `applied earlier (token "tok_01m3250vkwffc979nzjw0g2sb2") and a repeat delivers no token; revoke the actor`},
		"a refused enrolment":    {"enrol-agent", notFound, nil, entry.ExitRefused, "refused"},
		"an unwritable token":    {"enrol-agent", failing, nil, entry.ExitCouldNotRun, "could not be written (disk full) and is not delivered again; revoke the actor"},
		"a non-issuing response": {"revoke-actor", issuedResp, nil, entry.ExitCouldNotRun, "discarded and nothing was written"},
		"no credential returned": {"enrol-agent", applied, nil, entry.ExitCouldNotRun, "applied but no credential was returned; revoke the actor"},
		"a lost rotation":        {"replace-token", "", errors.New("connection reset"), entry.ExitCouldNotRun, "a repeat delivers no token: the old token is revoked"},
		"a repeated issue":       {"issue-token", replayed, nil, entry.ExitCouldNotRun, `applied earlier (token "tok_01m3250vkwffc979nzjw0g2sb2") and a repeat delivers no token; a repeat`},
		"an unwritable rotation": {"replace-token", failing, nil, entry.ExitCouldNotRun, "could not be written (disk full) and is not delivered again; the old token is revoked"},
		"a revocation's token":   {"revoke-token", issuedResp, nil, entry.ExitCouldNotRun, "discarded and nothing was written"},
	} {
		dir := privateDir(t)
		args := []string{"admin", c.cmd, "--expect", "1", "--reason", "r"}
		if _, issues := issuing[commandAction(c.cmd)]; issues {
			args = append(args, "--token-out", filepath.Join(dir, "t"))
		}
		code, out, errs := adc(&fake{resp: c.resp, err: c.err}, args...)
		if code != c.code || !strings.Contains(out+errs, c.want) || strings.Contains(out+errs, "zzFA") {
			t.Errorf("%s: %d %q %q", name, code, out, errs)
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Errorf("%s: --token-out left behind (%v)", name, err)
		}
	}
	if after, err := os.ReadFile(own); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("the caller's token file changed (%v)", err)
	}
}

func commandAction(verb string) string {
	a, _ := actionFor("admin", verb)
	return a
}

// A response that fails validation is never quoted.
func TestInvalidResponseNotQuoted(t *testing.T) {
	bad := strings.TrimSuffix(applied, "}") + `,"issued_credential":"tok_zzLEAKEDzzCREDENTIALzz"}`
	for _, mode := range [][]string{nil, {"--json"}} {
		code, out, errs := adc(&fake{resp: bad}, append([]string{"seat", "take", "--expect", "0", "--reason", "r"}, mode...)...)
		if code != entry.ExitCouldNotRun || out != "" || strings.Contains(errs, "zzLEAKEDzz") {
			t.Fatalf("%v: %d %q %q", mode, code, out, errs)
		}
	}
}

// A malformed request the controller refuses is a usage error.
func TestBadRequest(t *testing.T) {
	if code, _, _ := adc(&fake{err: client.ErrBadRequest}, "seat", "take", "--expect", "0", "--reason", "r"); code != entry.ExitUsage {
		t.Fatalf("exit %d", code)
	}
}

// A refused token is a refusal, not a could_not_run.
func TestUnauthenticated(t *testing.T) {
	if code, out, errs := adc(&fake{err: client.ErrUnauthenticated}, "seat", "take", "--expect", "0", "--reason", "r", "--json"); code != entry.ExitRefused || out != "" || !strings.Contains(errs, "refused") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
}

// The response is re-encoded without its credential before the token is
// written: if that fails, nothing is written and the token is reported
// undelivered, which is then true.
func TestTokenOutUnencodableResponse(t *testing.T) {
	defer func(old func(types.CommandResponse) ([]byte, error)) { encodeResponse = old }(encodeResponse)
	encodeResponse = func(types.CommandResponse) ([]byte, error) { return nil, errors.New("no") }
	dir := privateDir(t)
	code, out, errs := adc(&fake{resp: issuedResp}, "admin", "enrol-agent", "--expect", "0", "--reason", "r", "--token-out", filepath.Join(dir, "t"))
	if code != entry.ExitCouldNotRun || out != "" || strings.Contains(errs, "zzFAKEzz") || !strings.Contains(errs, "was not written and is not delivered again; revoke the actor") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("--token-out left behind (%v)", err)
	}
}

// A --token-out the system cannot create (here in a directory the caller
// cannot write) is could_not_run, not a usage error; nothing is sent.
func TestTokenOutSystemErrorCouldNotRun(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes a 0500 directory")
	}
	dir := privateDir(t)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	f := &fake{resp: issuedResp}
	code, _, errs := adc(f, "admin", "enrol-agent", "--expect", "0", "--reason", "r", "--token-out", filepath.Join(dir, "t"))
	if code != entry.ExitCouldNotRun || f.calls != 0 || !strings.Contains(errs, "nothing was sent") {
		t.Fatalf("%d after %d calls: %q", code, f.calls, errs)
	}
}
