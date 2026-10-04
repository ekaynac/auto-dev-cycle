// SPDX-License-Identifier: Apache-2.0

package vocab

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

func mustLoad(t *testing.T) *Registry {
	t.Helper()
	reg, err := Load()
	if err != nil {
		t.Fatalf("embedded vocabulary: %v", err)
	}
	return reg
}

func TestEmbeddedRegistryLoads(t *testing.T) {
	reg := mustLoad(t)
	if len(reg.Reasons) == 0 || len(reg.Holds) == 0 {
		t.Fatalf("empty registry: %d reasons, %d holds", len(reg.Reasons), len(reg.Holds))
	}
}

// The Go constants and outcomes.yaml name exactly the same four outcomes.
func TestOutcomeConstantsMatchRegistry(t *testing.T) {
	reg := mustLoad(t)
	want := map[Outcome]bool{Found: true, Empty: true, CouldNotRun: true, Failed: true}
	if len(reg.Outcomes) != len(want) {
		t.Fatalf("registry has %d outcomes, want 4", len(reg.Outcomes))
	}
	for o := range reg.Outcomes {
		if !want[o] {
			t.Errorf("registry outcome %q has no constant", o)
		}
	}
}

// Note 02 names these as reason codes; they must stay registered.
func TestReasonsNamedByTheDesignAreRegistered(t *testing.T) {
	reg := mustLoad(t)
	for _, c := range []string{"routing_violation", "quota_unknown", "stale_attempt", "protected_path",
		"test_integrity_violation", "evidence_inapplicable", "broker_credit_exhausted", "precondition_failed",
		"idempotency_key_reused", "stale_expected_state", "controller_already_running", "ambiguous_state",
		"authority_revoked", "receipt_incomplete", "unsafe_output", "project_unconfigured"} {
		if _, ok := reg.Reasons[c]; !ok {
			t.Errorf("reason %s is not registered", c)
		}
	}
}

func TestCheck(t *testing.T) {
	reg := mustLoad(t)
	cases := []struct {
		name string
		r    Result
		want string // "" = accepted
	}{
		{"valid could_not_run", Result{CouldNotRun, r("quota_exhausted"), "429 from source"}, ""},
		{"valid empty without detail", Result{Empty, r("completed_clean"), ""}, ""},
		{"unregistered reason", Result{Failed, r("oops"), "x"}, "unregistered reason"},
		{"reason with the wrong outcome", Result{Empty, r("quota_exhausted"), ""}, "does not accompany"},
		{"non-empty result with empty detail", Result{Failed, r("stale_attempt"), ""}, "non-empty detail"},
		{"unknown outcome", Result{"ok", r("completed_clean"), ""}, "unknown outcome"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := reg.Check(c.r)
			if c.want == "" {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrMalformed) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want ErrMalformed containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestRequireEmptyNeverAcceptsCouldNotRun(t *testing.T) {
	if err := RequireEmpty(Result{Empty, r("completed_clean"), ""}); err != nil {
		t.Fatalf("empty refused: %v", err)
	}
	for _, o := range []Outcome{CouldNotRun, Found, Failed} {
		if err := RequireEmpty(Result{o, r("x"), "d"}); !errors.Is(err, ErrNotEmpty) {
			t.Errorf("%s accepted as empty", o)
		}
	}
}

func TestHoldFor(t *testing.T) {
	reg := mustLoad(t)
	h, ok := reg.HoldFor(r("broker_credit_exhausted"))
	if !ok || h.Hold != "broker_credit_exhausted" || string(h.Signal) == "time" {
		t.Fatalf("broker_credit_exhausted → %+v, %v (signal must never be time)", h, ok)
	}
	if _, ok := reg.HoldFor(r("routing_violation")); ok {
		t.Error("routing_violation should not lead to a hold")
	}
}

const (
	outcomesYAML = `schema: adc.vocab.outcomes/v1
outcomes:
  - {outcome: found, description: a}
  - {outcome: empty, description: b}
  - {outcome: could_not_run, description: c}
  - {outcome: failed, description: d}
`
	holdsYAML = `schema: adc.vocab.holds/v1
holds:
  - {hold: h1, signal: probe, signal_detail: x, owner: system, page_after: PT1H, escalate_to: null}
`
)

func TestLoaderRefuses(t *testing.T) {
	reason := func(code, hold string) string {
		s := "  - {code: " + code + ", outcomes: [failed], domain: ops, description: d, introduced: 0.1.0, deprecated: null, source: \"02\""
		if hold != "" {
			s += ", hold: " + hold
		}
		return s + "}\n"
	}
	cases := []struct{ name, reasons, want string }{
		{"dangling hold", "schema: adc.vocab.reasons/v1\nreasons:\n" + reason("r1", "nope"), "unregistered hold nope"},
		{"duplicate code", "schema: adc.vocab.reasons/v1\nreasons:\n" + reason("r1", "") + reason("r1", ""), "registered twice"},
		{"schema violation", "schema: adc.vocab.reasons/v1\nreasons:\n  - {code: R1}\n", "contract vocab/registry"},
		{"duplicate YAML key", "schema: adc.vocab.reasons/v1\nschema: adc.vocab.reasons/v1\nreasons: []\n", "already defined"},
		{"a second YAML document", "schema: adc.vocab.reasons/v1\nreasons:\n" + reason("r1", "") + "---\nschema: adc.vocab.reasons/v1\nreasons:\n" + reason("r2", ""), "exactly one YAML document"},
		{"a malformed trailing document", "schema: adc.vocab.reasons/v1\nreasons:\n" + reason("r1", "") + "---\n: [\n", "exactly one YAML document"},
		{"deprecated omitted", "schema: adc.vocab.reasons/v1\nreasons:\n  - {code: r1, outcomes: [failed], domain: ops, description: d, introduced: 0.1.0, source: \"02\"}\n", "deprecated"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fsys := fstest.MapFS{
				"vocab/outcomes.yaml": {Data: []byte(outcomesYAML)},
				"vocab/holds.yaml":    {Data: []byte(holdsYAML)},
				"vocab/reasons.yaml":  {Data: []byte(c.reasons)},
			}
			_, err := loadFrom(fsys, []string{"vocab/holds.yaml", "vocab/outcomes.yaml", "vocab/reasons.yaml"})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}

// Four entries with a repeated outcome satisfy the schema's item count; the
// loader must not let the map silently drop the missing outcome.
func TestLoaderRefusesDuplicateOutcome(t *testing.T) {
	dup := strings.Replace(outcomesYAML, "outcome: could_not_run", "outcome: found", 1)
	fsys := fstest.MapFS{
		"vocab/outcomes.yaml": {Data: []byte(dup)},
		"vocab/holds.yaml":    {Data: []byte(holdsYAML)},
		"vocab/reasons.yaml":  {Data: []byte("schema: adc.vocab.reasons/v1\nreasons:\n  - {code: r1, outcomes: [failed], domain: ops, description: d, introduced: 0.1.0, deprecated: null, source: \"02\"}\n")},
	}
	_, err := loadFrom(fsys, []string{"vocab/holds.yaml", "vocab/outcomes.yaml", "vocab/reasons.yaml"})
	if err == nil || !strings.Contains(err.Error(), "outcome found registered twice") {
		t.Fatalf("want a duplicate-outcome error, got %v", err)
	}
}

const reasonsYAML = "schema: adc.vocab.reasons/v1\nreasons:\n" +
	"  - {code: r1, outcomes: [failed], domain: ops, description: d, introduced: 0.1.0, deprecated: null, source: \"02\"}\n" +
	"  - {code: old, outcomes: [failed], domain: ops, description: d, introduced: 0.1.0, deprecated: 0.2.0, source: \"02\"}\n"

// Each registry kind exactly once: a missing or a second registry is an error.
func TestLoaderNeedsEachKindOnce(t *testing.T) {
	fsys := fstest.MapFS{
		"vocab/outcomes.yaml": {Data: []byte(outcomesYAML)},
		"vocab/holds.yaml":    {Data: []byte(holdsYAML)},
		"vocab/reasons.yaml":  {Data: []byte(reasonsYAML)},
		"vocab/more.yaml":     {Data: []byte(reasonsYAML)},
	}
	if _, err := loadFrom(fsys, []string{"vocab/holds.yaml", "vocab/outcomes.yaml"}); err == nil || !strings.Contains(err.Error(), "exactly one outcomes, reasons and holds registry") {
		t.Errorf("a missing reasons registry: got %v", err)
	}
	if _, err := loadFrom(fsys, []string{"vocab/holds.yaml", "vocab/more.yaml", "vocab/outcomes.yaml", "vocab/reasons.yaml"}); err == nil || !strings.Contains(err.Error(), "a second") {
		t.Errorf("a second reasons registry: got %v", err)
	}
}

// Readers accept every registered code; writers may not emit a deprecated one.
func TestCheckRefusesDeprecatedReasons(t *testing.T) {
	fsys := fstest.MapFS{
		"vocab/outcomes.yaml": {Data: []byte(outcomesYAML)},
		"vocab/holds.yaml":    {Data: []byte(holdsYAML)},
		"vocab/reasons.yaml":  {Data: []byte(reasonsYAML)},
	}
	reg, err := loadFrom(fsys, []string{"vocab/holds.yaml", "vocab/outcomes.yaml", "vocab/reasons.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Reasons["old"]; !ok {
		t.Fatal("a deprecated code is not readable")
	}
	if err := reg.Check(Result{Failed, r("r1"), "x"}); err != nil {
		t.Fatalf("a current code refused: %v", err)
	}
	if err := reg.Check(Result{Failed, r("old"), "x"}); err == nil || !strings.Contains(err.Error(), "deprecated") {
		t.Fatalf("a deprecated code was accepted for writing: %v", err)
	}
}

func TestLoaderRefusesDuplicateHold(t *testing.T) {
	fsys := fstest.MapFS{
		"vocab/outcomes.yaml": {Data: []byte(outcomesYAML)},
		"vocab/holds.yaml":    {Data: []byte(holdsYAML + "  - {hold: h1, signal: time, signal_detail: y, owner: system, page_after: null, escalate_to: null}\n")},
		"vocab/reasons.yaml":  {Data: []byte(reasonsYAML)},
	}
	_, err := loadFrom(fsys, []string{"vocab/holds.yaml", "vocab/outcomes.yaml", "vocab/reasons.yaml"})
	if err == nil || !strings.Contains(err.Error(), "hold h1 registered twice") {
		t.Fatalf("want a duplicate-hold error, got %v", err)
	}
}

func TestCheckNeedsDetailForEveryNonEmptyOutcome(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []Result{{Found, r("gate_test_failed"), ""}, {CouldNotRun, r("quota_exhausted"), ""}, {Failed, r("stale_attempt"), ""}} {
		if err := reg.Check(r); err == nil || !strings.Contains(err.Error(), "non-empty detail") {
			t.Errorf("%s/%s with an empty detail: got %v", r.Outcome, r.Reason, err)
		}
	}
}

// The hold registry against note 02's table and paging defaults: every hold,
// its signal, owner, escalation and page_after.
func TestHoldsMatchNote02(t *testing.T) {
	type want struct{ signal, owner, page, escalate string }
	note02 := map[string]want{
		"quota_reset":              {"time", "system", "PT30M", ""},
		"source_invalid_config":    {"config_change", "operator", "PT1H", ""},
		"awaiting_approval":        {"approval", "grant_holder", "PT1H", ""},
		"awaiting_confirmation":    {"approval", "confirmation_policy", "PT1H", ""},
		"awaiting_receipt":         {"receipt", "system", "PT30M", ""},
		"reviewer_unavailable":     {"probe", "system", "PT1H", ""},
		"wrong_owner":              {"human_act", "named_owner", "PT1H", ""},
		"maintenance_boundary":     {"revision", "system", "PT45M", ""},
		"budget_rail":              {"approval", "operator", "PT1H", ""},
		"base_moved_conflict":      {"revision", "system", "PT30M", ""},
		"broker_credit_exhausted":  {"probe", "operator", "PT1H", ""},
		"awaiting_input":           {"approval", "grant_holder", "PT1H", ""},
		"evidence_restore_pending": {"receipt", "system", "PT1H", "environment_owner"},
		"precondition_failed":      {"probe", "environment_owner", "PT1H", ""},
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Holds) != len(note02) {
		t.Fatalf("%d holds registered, note 02 has %d", len(reg.Holds), len(note02))
	}
	for name, w := range note02 {
		h, ok := reg.Holds[name]
		if !ok {
			t.Errorf("hold %s is not registered", name)
			continue
		}
		page, escalate := "", ""
		if h.PageAfter != nil {
			page = string(*h.PageAfter)
		}
		if h.EscalateTo != nil {
			escalate = string(*h.EscalateTo)
		}
		got := want{string(h.Signal), string(h.Owner), page, escalate}
		if got != w {
			t.Errorf("hold %s = %+v, note 02 wants %+v", name, got, w)
		}
	}
}

// r builds a reason from any code, unchecked: tests build malformed results
// on purpose (test files are not scanned by adc dev vocab check).
func r(code string) Reason { return types.ReasonCodeOf(code) }

// Astra #28: even the vocabulary package cannot replace an installed check;
// init installed both, so a second installation is refused.
func TestReasonChecksInstallOnce(t *testing.T) {
	allow := func(string, bool) error { return nil }
	if err := types.SetReasonCheck(allow); err == nil {
		t.Error("the generated types' reason check was installed twice")
	}
	if err := contract.SetReasonCheck(allow); err == nil {
		t.Error("the contract runtime's reason check was installed twice")
	}
	if _, err := contract.Encode("common/defs", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := checkReason("typo_after_install", true); err == nil {
		t.Fatal("the vocabulary check accepted an unregistered code")
	}
}
