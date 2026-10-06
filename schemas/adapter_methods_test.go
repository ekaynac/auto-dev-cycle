// SPDX-License-Identifier: Apache-2.0

package schemas_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

const exeA = "exe_01m3250vnfeyrv67x6w0ddqr6a"

// adapterMethods returns one valid document of each method message (09).
func adapterMethods() map[string]map[string]any {
	m := leaseDocs()["records/usage-measurement"]
	return map[string]map[string]any{
		"adapter/describe-result": {"harness": "claude-code", "harness_version": "2.3.1", "adapter_version": "0.1.0-rc.1+build.7", "contract_version": "1.0.0",
			"capabilities": []any{"patch", "tool_use"}, "session_semantics": "fresh_process", "tool_surface": []any{"Read", "Edit"},
			"telemetry": map[string]any{"usage": true, "upstream": true, "stream_deltas": true, "per_turn_coverage": false},
			"host_side": true, "needs_native_login": true},
		"adapter/prepare-params": {"attempt": attID, "fencing_token": 3, "role": "builder", "executor": exeA, "model": "claude-sonnet-5-5",
			"workspace": map[string]any{"path": "/workspace", "writable": false}, "inputs": []any{handle("context.json")},
			"exchange":     map[string]any{"in": "/exchange/in", "out": "/exchange/out"},
			"limits":       map[string]any{"max_turns": 40, "max_output_tokens": 64000, "wall_seconds": 3600},
			"tool_policy":  map[string]any{"allowed": []any{"Read"}, "mediated": []any{"RunTest"}},
			"gateway":      map[string]any{"base_url": "http://127.0.0.1:7801/v1", "token": strings.Repeat("g", 32)},
			"native_login": map[string]any{"credential_ref": "claude-code", "scope": []any{"inference"}}},
		"adapter/run-params":    {"attempt": attID, "fencing_token": 3, "call": calID, "prompt": handle("prompt.md"), "response_schema": nil},
		"adapter/fenced-params": {"attempt": attID, "fencing_token": 3},
		"adapter/tool-response-params": {"attempt": attID, "fencing_token": 3, "call": calID, "turn": 2, "tool": "RunTest",
			"request": handle("req-1.json"), "outcome": "found", "reason": "completed_with_findings", "detail": "2 failures",
			"delivered": handle("result-1.json")},
		"adapter/outcome": {"outcome": "empty", "reason": "completed_clean", "detail": ""},
		"adapter/run-result": {"outcome": "found", "reason": "completed_with_findings", "detail": "a patch",
			"result":      map[string]any{"kind": "patch", "handle": handle("patch.diff")},
			"usage_final": map[string]any{"tokens": m["tokens"], "turns": 3, "milliseconds": 9000, "cost": m["cost"]}},
	}
}

type caseSet map[string]struct {
	schema string
	doc    map[string]any
}

// adapterMethodCases returns the other valid shapes and the refused ones.
func adapterMethodCases() (good, bad caseSet) {
	docs := adapterMethods()
	p, run, tool, desc := docs["adapter/prepare-params"], docs["adapter/run-result"], docs["adapter/tool-response-params"], docs["adapter/describe-result"]
	gw := func(url string) map[string]any {
		return with(p, "gateway", map[string]any{"base_url": url, "token": strings.Repeat("g", 32)})
	}
	good = caseSet{
		"no gateway and no login":           {"adapter/prepare-params", with(with(p, "gateway", nil), "native_login", nil)},
		"a reusable process":                {"adapter/describe-result", with(desc, "session_semantics", "reusable_process")},
		"a failed run, no result":           {"adapter/run-result", with(with(with(with(run, "outcome", "failed"), "reason", "unsafe_output"), "detail", "x"), "result", nil)},
		"unavailable final usage":           {"adapter/run-result", with(run, "usage_final", nil)},
		"a response schema handle":          {"adapter/run-params", with(docs["adapter/run-params"], "response_schema", handle("schema.json"))},
		"a tool that could not run":         {"adapter/tool-response-params", with(with(with(tool, "outcome", "could_not_run"), "reason", "runner_fault"), "detail", "the sandbox is down")},
		"a failed tool with its diagnostic": {"adapter/tool-response-params", with(with(with(tool, "outcome", "failed"), "reason", "unsafe_output"), "detail", "x")},
		"a clean tool result":               {"adapter/tool-response-params", with(with(with(tool, "outcome", "empty"), "reason", "completed_clean"), "detail", "")},
		"a could_not_run outcome":           {"adapter/outcome", map[string]any{"outcome": "could_not_run", "reason": "ambiguous_state", "detail": "x"}},
		"a plain semver":                    {"adapter/describe-result", with(desc, "contract_version", "10.20.30")},
		"a gateway by name and port":        {"adapter/prepare-params", gw("https://gateway.internal:8443/v1/models")},
		"a gateway on IPv6":                 {"adapter/prepare-params", gw("http://[::1]:7801")},
		"a gateway name with the root dot":  {"adapter/prepare-params", gw("https://gateway.internal./v1")},
		"a gateway on IPv4-mapped IPv6":     {"adapter/prepare-params", gw("http://[::ffff:127.0.0.1]:7801/v1")},
		"a gateway on the highest port":     {"adapter/prepare-params", gw("http://localhost:65535")},
	}
	bad = caseSet{
		"a failed run with a result":            {"adapter/run-result", with(with(with(run, "outcome", "failed"), "reason", "unsafe_output"), "detail", "x")},
		"a could_not_run run with a result":     {"adapter/run-result", with(with(run, "outcome", "could_not_run"), "reason", "ambiguous_state")},
		"a found run with no detail":            {"adapter/run-result", with(run, "detail", "")},
		"an unknown result kind":                {"adapter/run-result", with(run, "result", map[string]any{"kind": "binary", "handle": handle("x")})},
		"half a usage report":                   {"adapter/run-result", with(run, "usage_final", map[string]any{"turns": 3})},
		"a failed outcome with no detail":       {"adapter/outcome", map[string]any{"outcome": "failed", "reason": "unsafe_output", "detail": ""}},
		"an unregistered reason":                {"adapter/outcome", map[string]any{"outcome": "empty", "reason": "nope", "detail": ""}},
		"a relative workspace":                  {"adapter/prepare-params", with(p, "workspace", map[string]any{"path": "workspace", "writable": true})},
		"no exchange root":                      {"adapter/prepare-params", without(p, "exchange")},
		"an exchange without out":               {"adapter/prepare-params", with(p, "exchange", map[string]any{"in": "/exchange/in"})},
		"a relative exchange out":               {"adapter/prepare-params", with(p, "exchange", map[string]any{"in": "/exchange/in", "out": "out"})},
		"a NUL in the exchange in":              {"adapter/prepare-params", with(p, "exchange", map[string]any{"in": "/x\x00", "out": "/exchange/out"})},
		"an unknown exchange field":             {"adapter/prepare-params", with(p, "exchange", map[string]any{"in": "/i", "out": "/o", "tmp": "/t"})},
		"a NUL in the workspace path":           {"adapter/prepare-params", with(p, "workspace", map[string]any{"path": "/w\x00x", "writable": true})},
		"a raw path as an input":                {"adapter/prepare-params", with(p, "inputs", []any{"/etc/passwd"})},
		"zero turns":                            {"adapter/prepare-params", with(p, "limits", map[string]any{"max_turns": 0, "max_output_tokens": 1, "wall_seconds": 1})},
		"a week and a second":                   {"adapter/prepare-params", with(p, "limits", map[string]any{"max_turns": 1, "max_output_tokens": 1, "wall_seconds": 604801})},
		"a gateway that is not http":            {"adapter/prepare-params", gw("file:///x")},
		"a gateway with no host":                {"adapter/prepare-params", gw("http:///path")},
		"a gateway that is only a fragment":     {"adapter/prepare-params", gw("http://#fragment")},
		"a gateway with a NUL":                  {"adapter/prepare-params", gw("http://h/a\x00b")},
		"a gateway with a fragment":             {"adapter/prepare-params", gw("http://h/a#frag")},
		"a gateway over ftp":                    {"adapter/prepare-params", gw("ftp://h/x")},
		"a gateway on a short IPv6 literal":     {"adapter/prepare-params", gw("http://[abcd]/v1")},
		"a gateway on a triple colon":           {"adapter/prepare-params", gw("http://[:::]/v1")},
		"a gateway on two double colons":        {"adapter/prepare-params", gw("http://[1::2::3]/v1")},
		"a gateway with a zone":                 {"adapter/prepare-params", gw("http://[fe80::1%25eth0]/v1")},
		"a gateway past the last port":          {"adapter/prepare-params", gw("http://h:99999/v1")},
		"a gateway on port zero":                {"adapter/prepare-params", gw("http://h:0/v1")},
		"a gateway on an out-of-range IPv4":     {"adapter/prepare-params", gw("http://999.1.1.1/v1")},
		"a gateway with an empty label":         {"adapter/prepare-params", gw("http://h..i/v1")},
		"a gateway label ending in a hyphen":    {"adapter/prepare-params", gw("http://h-/v1")},
		"a gateway label of 64 characters":      {"adapter/prepare-params", gw("http://" + strings.Repeat("a", 64) + ".x/v1")},
		"a gateway with userinfo":               {"adapter/prepare-params", gw("http://u:p@h/v1")},
		"a gateway with a space":                {"adapter/prepare-params", gw("http://h/a b")},
		"a short gateway token":                 {"adapter/prepare-params", with(p, "gateway", map[string]any{"base_url": "http://x", "token": "short"})},
		"a login with no scope":                 {"adapter/prepare-params", with(p, "native_login", map[string]any{"credential_ref": "c", "scope": []any{}})},
		"an unknown prepare field":              {"adapter/prepare-params", with(p, "home", "/home/op")},
		"a tool response delivering nothing":    {"adapter/tool-response-params", with(tool, "delivered", nil)},
		"a found tool result with no detail":    {"adapter/tool-response-params", with(tool, "detail", "")},
		"a tool response with no request":       {"adapter/tool-response-params", without(tool, "request")},
		"a zero fencing token":                  {"adapter/fenced-params", map[string]any{"attempt": attID, "fencing_token": 0}},
		"a run with no prompt":                  {"adapter/run-params", without(docs["adapter/run-params"], "prompt")},
		"another session semantics":             {"adapter/describe-result", with(desc, "session_semantics", "persistent")},
		"a contract version that is not semver": {"adapter/describe-result", with(desc, "contract_version", "1")},
		"a version with a leading zero":         {"adapter/describe-result", with(desc, "contract_version", "01.0.0")},
		"an empty prerelease":                   {"adapter/describe-result", with(desc, "adapter_version", "1.0.0-")},
		"a capability with a space":             {"adapter/describe-result", with(desc, "capabilities", []any{"tool use"})},
		"telemetry missing a field":             {"adapter/describe-result", with(desc, "telemetry", map[string]any{"usage": true})},
	}
	return good, bad
}

func TestAdapterMethods(t *testing.T) {
	for schema, d := range adapterMethods() {
		if err := valid(t, schema, d); err != nil {
			t.Errorf("%s: %v", schema, err)
		}
	}
	good, bad := adapterMethodCases()
	for name, c := range good {
		if err := valid(t, c.schema, c.doc); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, c := range bad {
		if valid(t, c.schema, c.doc) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// roundTrip decodes a document into its generated type and encodes it back
// to the same JSON values.
func roundTrip[T any](t *testing.T, schema string, d map[string]any) {
	t.Helper()
	b, _ := json.Marshal(d)
	v, err := contract.Decode[T](schema, b)
	if err != nil {
		t.Errorf("%s: %v", schema, err)
		return
	}
	out, err := contract.Encode(schema, v)
	if err != nil {
		t.Errorf("%s: %v", schema, err)
		return
	}
	var got, want any
	if json.Unmarshal(out, &got) != nil || json.Unmarshal(b, &want) != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("%s: re-encoded as %s", schema, out)
	}
}

func TestAdapterMethodTypesRoundTrip(t *testing.T) {
	docs := adapterMethods()
	good, _ := adapterMethodCases()
	roundTrip[types.AdapterDescribeResult](t, "adapter/describe-result", docs["adapter/describe-result"])
	roundTrip[types.AdapterPrepareParams](t, "adapter/prepare-params", docs["adapter/prepare-params"])
	roundTrip[types.AdapterPrepareParams](t, "adapter/prepare-params", good["no gateway and no login"].doc)
	roundTrip[types.AdapterRunParams](t, "adapter/run-params", docs["adapter/run-params"])
	roundTrip[types.AdapterFencedParams](t, "adapter/fenced-params", docs["adapter/fenced-params"])
	roundTrip[types.AdapterToolResponseParams](t, "adapter/tool-response-params", docs["adapter/tool-response-params"])
	roundTrip[types.AdapterOutcome](t, "adapter/outcome", docs["adapter/outcome"])
	roundTrip[types.AdapterRunResult](t, "adapter/run-result", docs["adapter/run-result"])
	roundTrip[types.AdapterRunResult](t, "adapter/run-result", good["unavailable final usage"].doc)
}

// A controller-mediated tool completes its exchange for every outcome: the
// adapter's tool.request, the core's tool.response, and the adapter's
// tool.result reporting exactly the delivered handle, with one tool name
// throughout, the policy's included.
func TestToolExchange(t *testing.T) {
	docs := adapterMethods()
	const tool = "RunTest.v2/x+y" // capitals and punctuation the label allows
	policy := with(docs["adapter/prepare-params"], "tool_policy", map[string]any{"allowed": []any{"Read"}, "mediated": []any{tool}})
	if err := valid(t, "adapter/prepare-params", policy); err != nil {
		t.Fatalf("policy: %v", err)
	}
	envelope := func(e map[string]any) map[string]any {
		e["attempt"], e["fencing_token"], e["call"] = attID, 3, calID
		return e
	}
	request := envelope(map[string]any{"event": "tool.request", "turn": 2, "tool": tool, "request": handle("req-1.json")})
	if err := valid(t, "adapter/event", request); err != nil {
		t.Fatalf("tool.request: %v", err)
	}
	for _, o := range []struct{ outcome, reason, detail string }{
		{"empty", "completed_clean", ""}, {"found", "completed_with_findings", "2 failures"},
		{"failed", "unsafe_output", "the tool's output was unsafe"}, {"could_not_run", "runner_fault", "the sandbox is down"},
	} {
		delivered := handle("res-" + o.outcome + ".txt")
		response := with(with(with(with(with(docs["adapter/tool-response-params"], "tool", tool), "outcome", o.outcome), "reason", o.reason), "detail", o.detail), "delivered", delivered)
		if err := valid(t, "adapter/tool-response-params", response); err != nil {
			t.Errorf("%s response: %v", o.outcome, err)
		}
		result := envelope(map[string]any{"event": "tool.result", "turn": 2, "tool": tool, "mediated_by_controller": true, "delivered": delivered, "bytes": 3})
		if err := valid(t, "adapter/event", result); err != nil {
			t.Errorf("%s result: %v", o.outcome, err)
		}
	}
}
