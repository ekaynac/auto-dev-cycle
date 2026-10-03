// SPDX-License-Identifier: Apache-2.0

package schemas_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

const (
	rcpID   = "rcp_01m3250v57fhm9v1z9xd1wk8gw"
	rcpPrev = "rcp_01m3250v57ft7aehkqwq0yrq3n"
	cmdID   = "cmd_01m3250vz2ezn81cgvbkcn17jd"
	attID   = "att_01m3250vnfeyrv67x6w0ddqr6a"
	calID   = "cal_01m3250v9xft08ssr4ygq6g2np"
	actID   = "act_01m3250v0aer9ss2097svmefj8"
	prjID   = "prj_01m3250vkwffc979nzjw0g2sf9"
	prpID   = "prp_01m3250vm7f6ktw39dszqnyddr"
	runID   = "run_01m3250v03fpq92bcrrpmkpsj9"
)

var (
	dgA = "sha256:" + strings.Repeat("a", 64)
	dgB = "sha256:" + strings.Repeat("b", 64)
	git = "git:" + strings.Repeat("c", 40)
)

// subjects holds one valid subject per variant of 06 "Envelope".
var subjects = map[string]map[string]any{
	"tree": {"type": "tree", "project": prjID, "tree": git, "head": nil,
		"environment":       map[string]any{"identity": dgA, "description": "linux/amd64 go1.27.1"},
		"contract_revision": "3", "demand": map[string]any{"key": dgB, "run": runID}},
	"proposal":        {"type": "proposal", "proposal": prpID, "binding_digest": dgA, "demand": nil},
	"object_revision": {"type": "object_revision", "object": map[string]any{"type": "plan", "id": "p1", "version": 3, "digest": dgA}, "demand": nil},
	"environment":     {"type": "environment", "identity": dgA, "description": "staging", "demand": nil},
	"call":            {"type": "call", "call": calID, "demand": nil},
}

// receipt returns a valid gate receipt over the given subject variant.
func receipt(subject string) map[string]any {
	return map[string]any{
		"id":         rcpID,
		"kind":       "gate",
		"instrument": map[string]any{"name": "go-test", "version": "1.0.0", "digest": dgA},
		"subject":    subjects[subject],
		"provenance": map[string]any{"command": cmdID, "attempt": attID, "calls": []any{calID},
			"model_families": []any{"claude"}, "contributors": []any{actID}},
		"produced_at": "2026-10-03T10:00:00Z",
		"valid_until": nil,
		"predicates": []any{map[string]any{"name": "tests", "outcome": "empty", "reason": "none",
			"evidence": []any{map[string]any{"blob": dgB, "media_type": "application/junit+xml", "artifact": "junit"}},
			"payload":  map[string]any{"passed": 12}}},
		"outcome":            "empty",
		"reason":             "none",
		"detail":             "",
		"verdict":            nil,
		"conclusions_digest": dgB,
		"supersedes":         nil,
	}
}

func instrument() map[string]any {
	return map[string]any{
		"name": "go-test", "version": "1.0.0", "digest": dgA, "subject_type": "tree",
		"required_predicates": []any{map[string]any{"name": "tests", "evidence_required": []any{"application/junit+xml"}}},
		"optional_predicates": []any{"reported_status"},
		"preconditions":       []any{"toolchain_present"},
		"payload_schema":      dgB,
		"artifact_classes":    map[string]any{"junit": "diagnostic", "remote_stderr": "restricted"},
	}
}

func validate(t *testing.T, schema string, doc map[string]any) error {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return contract.Validate(schema, b)
}

func TestReceiptEverySubjectVariant(t *testing.T) {
	for name := range subjects {
		t.Run(name, func(t *testing.T) {
			if err := validate(t, "records/receipt", receipt(name)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReceiptOutcomesAndVerdicts(t *testing.T) {
	cases := []struct {
		name string
		edit func(r map[string]any)
		ok   bool
	}{
		{"empty with empty detail", func(r map[string]any) {}, true},
		{"found with detail", func(r map[string]any) { r["outcome"], r["detail"] = "found", "2 failures" }, true},
		{"found without detail", func(r map[string]any) { r["outcome"] = "found" }, false},
		{"failed without detail", func(r map[string]any) { r["outcome"] = "failed" }, false},
		{"could_not_run without detail", func(r map[string]any) { r["outcome"] = "could_not_run" }, false},
		{"review go on empty", func(r map[string]any) { r["kind"], r["verdict"] = "review", "go" }, true},
		{"review no_go on found", func(r map[string]any) {
			r["kind"], r["verdict"], r["outcome"], r["detail"] = "review", "no_go", "found", "blocking finding"
		}, true},
		{"review go on found", func(r map[string]any) {
			r["kind"], r["verdict"], r["outcome"], r["detail"] = "review", "go", "found", "blocking finding"
		}, false},
		{"review without verdict", func(r map[string]any) { r["kind"] = "review" }, false},
		{"gate with a verdict", func(r map[string]any) { r["verdict"] = "no_go" }, false},
		{"boolean ok field", func(r map[string]any) { r["ok"] = true }, false},
		{"unknown kind", func(r map[string]any) { r["kind"] = "lint" }, false},
		{"unknown subject type", func(r map[string]any) {
			r["subject"] = map[string]any{"type": "branch", "demand": nil}
		}, false},
		{"tree subject missing environment", func(r map[string]any) {
			s := map[string]any{}
			for k, v := range subjects["tree"] {
				s[k] = v
			}
			delete(s, "environment")
			r["subject"] = s
		}, false},
		{"subject mixing two variants", func(r map[string]any) {
			s := map[string]any{"call": calID}
			for k, v := range subjects["proposal"] {
				s[k] = v
			}
			r["subject"] = s
		}, false},
		{"command id with the wrong prefix", func(r map[string]any) {
			r["provenance"].(map[string]any)["command"] = attID
		}, false},
		{"evidence without artifact name", func(r map[string]any) {
			ev := r["predicates"].([]any)[0].(map[string]any)["evidence"].([]any)[0].(map[string]any)
			delete(ev, "artifact")
		}, false},
		{"media type with parameters", func(r map[string]any) {
			ev := r["predicates"].([]any)[0].(map[string]any)["evidence"].([]any)[0].(map[string]any)
			ev["media_type"] = "text/plain; charset=utf-8"
		}, false},
		{"demand run with the wrong prefix", func(r map[string]any) {
			s := map[string]any{}
			for k, v := range subjects["tree"] {
				s[k] = v
			}
			s["demand"] = map[string]any{"key": dgB, "run": attID}
			r["subject"] = s
		}, false},
		{"supersedes a receipt", func(r map[string]any) { r["supersedes"] = rcpPrev }, true},
		{"valid_until set", func(r map[string]any) { r["valid_until"] = "2026-10-04T10:00:00Z" }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := receipt("tree")
			c.edit(r)
			err := validate(t, "records/receipt", r)
			if c.ok && err != nil {
				t.Fatalf("want valid, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestInstrumentContract(t *testing.T) {
	cases := []struct {
		name string
		edit func(m map[string]any)
		ok   bool
	}{
		{"valid", func(m map[string]any) {}, true},
		{"no payload schema", func(m map[string]any) { m["payload_schema"] = nil }, true},
		{"no required predicates", func(m map[string]any) { m["required_predicates"] = []any{} }, false},
		{"unknown artifact class", func(m map[string]any) { m["artifact_classes"] = map[string]any{"junit": "public"} }, false},
		{"unknown subject type", func(m map[string]any) { m["subject_type"] = "branch" }, false},
		{"duplicate optional predicate", func(m map[string]any) {
			m["optional_predicates"] = []any{"reported_status", "reported_status"}
		}, false},
		{"missing digest", func(m map[string]any) { delete(m, "digest") }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := instrument()
			c.edit(m)
			err := validate(t, "records/instrument-contract", m)
			if c.ok && err != nil {
				t.Fatalf("want valid, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
