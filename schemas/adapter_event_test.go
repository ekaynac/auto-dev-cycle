// SPDX-License-Identifier: Apache-2.0

package schemas_test

import (
	"encoding/json"
	"io/fs"
	"reflect"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/schemas"
)

func handle(name string) map[string]any {
	return map[string]any{"attempt": attID, "handle": name, "digest": dgA, "size": 120}
}

// adapterEvents returns one valid notification of each kind (09).
func adapterEvents() map[string]map[string]any {
	m := leaseDocs()["records/usage-measurement"]
	evs := map[string]map[string]any{
		"call.sub":        {"sub_call": "s1", "model_requested": "claude-sonnet-5-5"},
		"turn.start":      {"turn": 1, "sub_call": "s1", "reported_at": "2026-10-04T10:00:00Z"},
		"turn.end":        {"turn": 1, "sub_call": "s1", "reported_at": "2026-10-04T10:00:09Z"},
		"delta.text":      {"turn": 1, "bytes": 512},
		"delta.thinking":  {"turn": 1, "bytes": 64},
		"delta.tool_call": {"turn": 1, "bytes": 80},
		"tool.request":    {"turn": 2, "tool": "run_test", "request": handle("req-1.json")},
		"tool.result": {"turn": 2, "tool": "run_test", "mediated_by_controller": true, "delivered": handle("res-1.json"),
			"bytes": 120},
		"coverage":   {"turn": 2, "observable": "partial", "unobservable": []any{"harness_tool"}},
		"usage":      {"sub_call": "s1", "sequence": 0, "tokens": m["tokens"], "turns": 1, "milliseconds": 9500, "cost": m["cost"]},
		"upstream":   {"turn": 1, "sub_call": "s1", "provider": "anthropic", "model_reported": "claude-sonnet-5-5"},
		"compaction": {"turn": 3, "before_tokens": 180000, "after_tokens": 40000},
		"error":      {"turn": 3, "class_hint": "rate_limited", "raw_excerpt": handle("err-2.txt")},
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
		"unknown event":             {"heartbeat", func(e map[string]any) { e["event"] = "thought" }},
		"no attempt":                {"heartbeat", func(e map[string]any) { delete(e, "attempt") }},
		"no call":                   {"delta.text", func(e map[string]any) { delete(e, "call") }},
		"token zero":                {"heartbeat", func(e map[string]any) { e["fencing_token"] = 0 }},
		"an extra field":            {"heartbeat", func(e map[string]any) { e["note"] = "x" }},
		"negative bytes":            {"delta.text", func(e map[string]any) { e["bytes"] = -1 }},
		"a raw path handle":         {"tool.request", func(e map[string]any) { e["request"] = handle("../../etc/passwd") }},
		"a handle without digest":   {"tool.request", func(e map[string]any) { delete(e["request"].(map[string]any), "digest") }},
		"mediated without handle":   {"tool.result", func(e map[string]any) { e["delivered"] = nil }},
		"declared with a handle":    {"tool.result", func(e map[string]any) { e["mediated_by_controller"] = false }},
		"complete yet unobserved":   {"coverage", func(e map[string]any) { e["observable"] = "complete" }},
		"unobservable kind":         {"coverage", func(e map[string]any) { e["unobservable"] = []any{"vibes"} }},
		"error class":               {"error", func(e map[string]any) { e["class_hint"] = "teapot" }},
		"a sub-call with a space":   {"call.sub", func(e map[string]any) { e["sub_call"] = "s 1" }},
		"usage without its figures": {"usage", func(e map[string]any) { delete(e, "tokens") }},
		// Astra #52: the controller owns the measurement's identity and kind.
		"usage naming a measurement id": {"usage", func(e map[string]any) { e["id"] = "msr_01m3250vm7f6ktw39dszqnyddr" }},
		"usage naming the lease":        {"usage", func(e map[string]any) { e["lease"] = "lea_01m3250v57fhm9v1z9xd1wk8gw" }},
		"usage naming its kind":         {"usage", func(e map[string]any) { e["kind"] = "final" }},
		"usage naming its source":       {"usage", func(e map[string]any) { e["source_of_truth"] = "provider_usage" }},
		"a zero turn":                   {"turn.start", func(e map[string]any) { e["turn"] = 0 }},
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

// Astra #52: adapter turns are the turns the ContextManifest records count,
// so a valid notification's turn is always valid manifest evidence: every
// turn field of manifest-delivery and manifest-finalized has the adapter's
// bounds, starting at 1.
func TestAdapterTurnsMatchManifests(t *testing.T) {
	load := func(path string) any {
		b, err := fs.ReadFile(schemas.FS(), path)
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	// turnBounds collects the integer bounds of every property named turn
	// (directly, or in a nullable oneOf).
	var turnBounds func(v any, out *[][2]float64)
	integer := func(n map[string]any) ([2]float64, bool) {
		lo, ok1 := n["minimum"].(float64)
		hi, ok2 := n["maximum"].(float64)
		return [2]float64{lo, hi}, ok1 && ok2
	}
	turnBounds = func(v any, out *[][2]float64) {
		switch x := v.(type) {
		case map[string]any:
			if props, ok := x["properties"].(map[string]any); ok {
				if turn, ok := props["turn"].(map[string]any); ok {
					if b, ok := integer(turn); ok {
						*out = append(*out, b)
					}
					if branches, ok := turn["oneOf"].([]any); ok {
						for _, br := range branches {
							if b, ok := integer(br.(map[string]any)); ok {
								*out = append(*out, b)
							}
						}
					}
				}
			}
			for _, y := range x {
				turnBounds(y, out)
			}
		case []any:
			for _, y := range x {
				turnBounds(y, out)
			}
		}
	}
	adapter, ok := integer(load("adapter/event.json").(map[string]any)["$defs"].(map[string]any)["AdapterTurn"].(map[string]any))
	if !ok || adapter[0] != 1 {
		t.Fatalf("adapter turns %v", adapter)
	}
	for _, path := range []string{"records/manifest-delivery.json", "records/manifest-finalized.json"} {
		var found [][2]float64
		turnBounds(load(path), &found)
		if len(found) == 0 {
			t.Fatalf("%s: no turn field found", path)
		}
		for _, b := range found {
			if b != adapter {
				t.Errorf("%s: a turn field bounded %v, adapter turns %v", path, b, adapter)
			}
		}
	}
}
