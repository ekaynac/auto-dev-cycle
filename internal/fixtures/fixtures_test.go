// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	_ "github.com/ekaynac/auto-dev-cycle/internal/vocab" // the reason check
)

// Every contract fixture yields its golden sequence through the real
// components.
func TestFixturesAreGreen(t *testing.T) {
	gs, err := Goldens()
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != len(runners) {
		t.Fatalf("%d goldens, %d runners", len(gs), len(runners))
	}
	for _, g := range gs {
		r, err := Run(context.Background(), g)
		if err != nil || !r.Pass {
			t.Errorf("%s: %v %s", g.Name, err, r.Detail)
		}
	}
}

func TestGoldenRefusals(t *testing.T) {
	good := `{"name": "idempotency-key-reuse", "invariant": "x", "expected": [{"outcome": "empty", "reason": "completed_clean"}], "mutation": "eventstore.idempotency-ignores-input"}`
	stale := `{"name": "stale-expected-state", "invariant": "x", "expected": [{"outcome": "empty", "reason": "completed_clean"}], "mutation": "eventstore.expected-state-unchecked"}`
	for name, c := range map[string]struct {
		files map[string]string
		want  string
	}{
		"a golden named after another file": {map[string]string{"m0/other.json": good, "m0/stale-expected-state.json": stale}, "names fixture"},
		"a golden with no runner": {map[string]string{"m0/idempotency-key-reuse.json": good, "m0/stale-expected-state.json": stale,
			"m0/nope.json": strings.Replace(good, "idempotency-key-reuse", "nope", 1)}, "has no runner"},
		"an unknown mutation": {map[string]string{"m0/idempotency-key-reuse.json": strings.Replace(good, "eventstore.idempotency-ignores-input", "eventstore.nothing", 1),
			"m0/stale-expected-state.json": stale}, "unknown mutation"},
		"a runner with no golden": {map[string]string{"m0/idempotency-key-reuse.json": good}, "has no golden"},
		"an unregistered reason":  {map[string]string{"m0/idempotency-key-reuse.json": strings.Replace(good, "completed_clean", "no_such_reason", 1), "m0/stale-expected-state.json": stale}, "contract"},
		"an incompatible pair":    {map[string]string{"m0/idempotency-key-reuse.json": strings.Replace(good, `"reason": "completed_clean"`, `"reason": "idempotency_key_reused"`, 1), "m0/stale-expected-state.json": stale}, "does not accompany"},
		"an unknown field":        {map[string]string{"m0/idempotency-key-reuse.json": strings.Replace(good, `"invariant"`, `"extra": 1, "invariant"`, 1), "m0/stale-expected-state.json": stale}, "contract"},
		"no expected steps":       {map[string]string{"m0/idempotency-key-reuse.json": strings.Replace(good, `[{"outcome": "empty", "reason": "completed_clean"}]`, `[]`, 1), "m0/stale-expected-state.json": stale}, "contract"},
	} {
		fsys := fstest.MapFS{}
		for f, b := range c.files {
			fsys[f] = &fstest.MapFile{Data: []byte(b)}
		}
		if _, err := goldensFrom(fsys); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A mismatch is reported with both sequences.
func TestRunReportsAMismatch(t *testing.T) {
	gs, err := Goldens()
	if err != nil {
		t.Fatal(err)
	}
	g := gs[0]
	g.Expected = append(g.Expected[:len(g.Expected):len(g.Expected)], Step{"empty", "completed_clean"})
	r, err := Run(context.Background(), g)
	if err != nil || r.Pass || !strings.Contains(r.Detail, "want") {
		t.Fatalf("%+v %v", r, err)
	}
}

// A violated assertion about committed state fails the fixture even when
// its steps match the golden, and is reported.
func TestRunReportsAViolation(t *testing.T) {
	gs, err := Goldens()
	if err != nil {
		t.Fatal(err)
	}
	g := gs[0]
	real := runners[g.Name]
	t.Cleanup(func() { runners[g.Name] = real })
	runners[g.Name] = func(context.Context, string) ([]Step, []string, error) {
		return g.Expected, []string{"step 2: the replay's result differs"}, nil
	}
	r, err := Run(context.Background(), g)
	if err != nil || r.Pass || !strings.Contains(r.Detail, "the replay's result differs") || strings.Contains(r.Detail, "want [") {
		t.Fatalf("%+v %v", r, err)
	}
}
