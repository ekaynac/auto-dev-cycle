// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// secretNotPrinted fails if any output carries a token's secret.
func secretNotPrinted(t *testing.T, outs []string, toks ...identity.Secret) {
	t.Helper()
	for _, tok := range toks {
		_, secret, _ := strings.Cut(tok.Reveal(), ".")
		for _, o := range outs {
			if strings.Contains(o, secret) {
				t.Fatal("a token's secret was printed")
			}
		}
	}
}

// 14 PR 10b against the daemon: the read commands, a note body rendered
// escaped (and byte-exact in --json), and a refusal's exit code.
func TestReadsThroughTheCLI(t *testing.T) {
	c := runCommanded(t)
	op, bare := c.op.Reveal(), c.bare.Reveal()
	var outs []string
	adc := func(tok string, args ...string) (int, string) {
		t.Helper()
		code, out, errb := c.adc(tok, args...)
		outs = append(outs, out, errb)
		return code, out
	}
	if code, out := adc(op, "admin", "whoami", "--json"); code != entry.ExitOK || contract.Validate("api/whoami", []byte(out)) != nil || !strings.Contains(out, string(c.opID)) {
		t.Fatalf("whoami: %d %s", code, out)
	}
	note := "hi\u202eevil\u200b\nnotes forged"
	scope, _ := json.Marshal(map[string]string{"project": "prj_01m3250vkwffc979nzjw0g2sa1", "body": note})
	if code, out := adc(op, "notes", "add", "--expect", "0", "--reason", "r", "--scope", string(scope)); code != entry.ExitOK {
		t.Fatalf("note: %d %s", code, out)
	}
	code, out := adc(op, "notes", "list", "--json")
	var list struct{ Items []struct{ Body string } }
	if code != entry.ExitOK || json.Unmarshal([]byte(out), &list) != nil || len(list.Items) != 1 || list.Items[0].Body != note {
		t.Fatalf("notes --json: %d %s", code, out)
	}
	if code, out := adc(op, "notes", "list"); code != entry.ExitOK || strings.ContainsAny(out, "\u202e\u200b") ||
		!strings.Contains(out, "    | hi\\u{202E}evil\\u{200B}\n    | notes forged\n") {
		t.Fatalf("notes: %d %s", code, out)
	}
	if code, out := adc(op, "seat", "status"); code != entry.ExitOK || !strings.HasPrefix(out, "seat sea_") {
		t.Fatalf("seat: %d %s", code, out)
	}
	if code, _ := adc(bare, "grant", "list"); code != entry.ExitRefused {
		t.Fatalf("no grant: %d", code)
	}

	secretNotPrinted(t, outs, c.op, c.bare)
}

// M1 P1: adcd projects project.created and serves the projects read, but
// project.create stays deferred until P22a activates intake: the fixture
// profile is refused action_deferred and no project is listed.
func TestProjectCreateStaysDeferred(t *testing.T) {
	c := runCommanded(t)
	op := c.op.Reveal()
	profile, err := os.ReadFile("../../fixtures/m1/profiles/py-mini.json")
	if err != nil {
		t.Fatal(err)
	}
	code, out, errb := c.adc(op, "project", "create", "--expect", "0", "--reason", "r", "--scope", string(profile))
	if code != entry.ExitCouldNotRun || !strings.Contains(out+errb, "action_deferred") {
		t.Fatalf("project create: %d %s %s", code, out, errb)
	}
	code, out, errb = c.adc(op, "project", "list", "--json")
	if code != entry.ExitOK || contract.Validate("api/read-list", []byte(out)) != nil || !strings.Contains(out, `"outcome":"empty"`) {
		t.Fatalf("project list: %d %s %s", code, out, errb)
	}
}
