// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

func fixturesCmd(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(context.Background(), append([]string{"dev", "fixtures"}, args...), &out, &errb, NoTransport{})
	return code, out.String(), errb.String()
}

func TestDevFixtures(t *testing.T) {
	code, out, errs := fixturesCmd("--contract")
	if code != entry.ExitOK || !strings.Contains(out, "idempotency-key-reuse: pass") || !strings.Contains(out, "stale-expected-state: pass") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	code, out, _ = fixturesCmd("--contract", "--list")
	if code != entry.ExitOK || !strings.Contains(out, "stale-expected-state eventstore.expected-state-unchecked\n") {
		t.Fatalf("list: %d %q", code, out)
	}
	code, out, _ = fixturesCmd("--contract", "--only", "stale-expected-state", "--json")
	var doc struct {
		Name string
		Pass bool
		Got  []map[string]string
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil || code != entry.ExitOK || doc.Name != "stale-expected-state" || !doc.Pass || len(doc.Got) != 3 {
		t.Fatalf("json: %d %q", code, out)
	}
	for name, args := range map[string][]string{
		"no --contract":   {},
		"an unknown only": {"--contract", "--only", "nope"},
		"an extra arg":    {"--contract", "x"},
		"an unknown flag": {"--contract", "--all"},
	} {
		if code, _, _ := fixturesCmd(args...); code != entry.ExitUsage {
			t.Errorf("%s: exit %d", name, code)
		}
	}
}
