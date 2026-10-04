// SPDX-License-Identifier: Apache-2.0

package schemas_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

func handle(name string) map[string]any {
	return map[string]any{"attempt": attID, "handle": name, "digest": dgA, "size": 120}
}

// adapterEvents returns one valid notification of each kind (09).
func adapterEvents() map[string]map[string]any {
	usage := leaseDocs()["records/usage-measurement"]
	evs := map[string]map[string]any{
		"call.sub":        {"sub_call": "s1", "model_requested": "claude-sonnet-5-5"},
		"turn.start":      {"turn": 0, "sub_call": "s1", "reported_at": "2026-10-04T10:00:00Z"},
		"turn.end":        {"turn": 0, "sub_call": "s1", "reported_at": "2026-10-04T10:00:09Z"},
		"delta.text":      {"turn": 0, "bytes": 512},
		"delta.thinking":  {"turn": 0, "bytes": 64},
		"delta.tool_call": {"turn": 0, "bytes": 80},
		"tool.request":    {"turn": 1, "tool": "run_test", "request": handle("req-1.json")},
		"tool.result": {"turn": 1, "tool": "run_test", "mediated_by_controller": true, "delivered": handle("res-1.json"),
			"bytes": 120},
		"coverage":   {"turn": 1, "observable": "partial", "unobservable": []any{"harness_tool"}},
		"usage":      {"sub_call": "s1", "measurement": usage},
		"upstream":   {"turn": 0, "sub_call": "s1", "provider": "anthropic", "model_reported": "claude-sonnet-5-5"},
		"compaction": {"turn": 2, "before_tokens": 180000, "after_tokens": 40000},
		"error":      {"turn": 2, "class_hint": "rate_limited", "raw_excerpt": handle("err-2.txt")},
		"heartbeat":  {},
	}
	for kind, e := range evs {
		e["event"], e["attempt"], e["fencing_token"], e["call"] = kind, attID, 3, calID
	}
	return evs
}

func TestAdapterEvents(t *testing.T) {
	for kind, e := range adapterEvents() {
		if err := valid(t, "adapter/event", e); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
	}
	// A declared (not controller-mediated) result names no handle; complete
	// coverage lists nothing unobservable.
	declared := adapterEvents()["tool.result"]
	declared["mediated_by_controller"], declared["delivered"] = false, nil
	complete := adapterEvents()["coverage"]
	complete["observable"], complete["unobservable"] = "complete", []any{}
	for name, e := range map[string]map[string]any{"declared result": declared, "complete coverage": complete} {
		if err := valid(t, "adapter/event", e); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	cases := map[string]struct {
		kind   string
		mutate func(e map[string]any)
	}{
		"unknown event":            {"heartbeat", func(e map[string]any) { e["event"] = "thought" }},
		"no attempt":               {"heartbeat", func(e map[string]any) { delete(e, "attempt") }},
		"no call":                  {"delta.text", func(e map[string]any) { delete(e, "call") }},
		"token zero":               {"heartbeat", func(e map[string]any) { e["fencing_token"] = 0 }},
		"an extra field":           {"heartbeat", func(e map[string]any) { e["note"] = "x" }},
		"negative bytes":           {"delta.text", func(e map[string]any) { e["bytes"] = -1 }},
		"a raw path handle":        {"tool.request", func(e map[string]any) { e["request"] = handle("../../etc/passwd") }},
		"a handle without digest":  {"tool.request", func(e map[string]any) { delete(e["request"].(map[string]any), "digest") }},
		"mediated without handle":  {"tool.result", func(e map[string]any) { e["delivered"] = nil }},
		"declared with a handle":   {"tool.result", func(e map[string]any) { e["mediated_by_controller"] = false }},
		"complete yet unobserved":  {"coverage", func(e map[string]any) { e["observable"] = "complete" }},
		"unobservable kind":        {"coverage", func(e map[string]any) { e["unobservable"] = []any{"vibes"} }},
		"error class":              {"error", func(e map[string]any) { e["class_hint"] = "teapot" }},
		"a sub-call with a space":  {"call.sub", func(e map[string]any) { e["sub_call"] = "s 1" }},
		"usage without its report": {"usage", func(e map[string]any) { delete(e, "measurement") }},
	}
	for name, c := range cases {
		e := adapterEvents()[c.kind]
		c.mutate(e)
		if err := valid(t, "adapter/event", e); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Every notification round-trips through the generated union unchanged.
func TestAdapterEventRoundTrip(t *testing.T) {
	for kind, e := range adapterEvents() {
		in, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		v, err := contract.Decode[types.AdapterEvent]("adapter/event", in)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		out, err := contract.Encode("adapter/event", v)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		var a, b any
		if err := json.Unmarshal(in, &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(out, &b); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s: round trip changed the event:\nin  %s\nout %s", kind, in, out)
		}
	}
}
