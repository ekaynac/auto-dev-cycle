// SPDX-License-Identifier: Apache-2.0

package installcheck

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// P24b, D1: scripts/lib/cycle-expect.mjs decides whether a stub case's run
// shows its expect block. A run that shows it passes and prints its summary
// row; each assertion, broken alone in an otherwise passing run, fails with
// its own line. In stub mode review stays inactive (#226 entry 189): the
// candidate is the gate completed and the review held review_inactive.
func TestCycleExpect(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on PATH (make ci's installed-cycle rows run the installed one)")
	}
	type doc = map[string]any
	base := func() map[string]any {
		nodes := []doc{
			{"id": "n1", "kind": "plan", "state": "completed", "holds": []doc{}},
			{"id": "n2", "kind": "build", "state": "completed", "holds": []doc{}},
			{"id": "n3", "kind": "accept", "state": "completed", "holds": []doc{}},
			{"id": "n4", "kind": "gate", "state": "completed", "holds": []doc{}},
			{"id": "n5", "kind": "review", "state": "held", "holds": []doc{{"hold": "review_inactive", "signal_instance": "capability: review"}}},
		}
		bench := []doc{
			{"id": "n2", "kind": "build", "round": 1, "outcome": "found", "reason": "completed_with_findings", "claimed_at": "t"},
			{"id": "n3", "kind": "accept", "round": 1, "outcome": "empty", "reason": "completed_clean", "claimed_at": "t"},
			{"id": "n4", "kind": "gate", "round": 1, "outcome": "empty", "reason": "completed_clean", "claimed_at": "t"},
			{"id": "n5", "kind": "review", "round": 1, "outcome": nil, "reason": nil, "claimed_at": nil},
		}
		return map[string]any{
			"case": doc{"project": "go-mini", "task": "t", "scenario": "s", "control": "c", "expect": doc{
				"until": "candidate", "merge": false, "holds": []string{}, "canaries": []string{"HIDDEN-CANARY-1"},
				"nodes":  doc{"build": doc{"outcome": "found", "reason": "completed_with_findings"}, "gate": doc{"outcome": "empty", "reason": "completed_clean"}},
				"absent": []string{"plan"}, "usage": doc{"in": 100, "out": 20}, "builder_sees": []string{"refused: a symlink"},
			}},
			"show": doc{"card": doc{"column": "verifying", "nodes": nodes}, "bench": doc{"nodes": bench},
				"first_attempt": doc{"seconds_after_approval": 1, "status": "completed", "tokens": doc{"in": 100, "out": 20}, "charge": doc{}}},
			"events": []doc{{"seq": 1, "kind": "task.created/v1"}, {"seq": 2, "kind": "approval.recorded/v1"}, {"seq": 3, "kind": "node.claimed/v1"},
				{"seq": 4, "kind": "attempt.started/v1"}},
			"stub": []doc{{"kind": "request", "request_id": "r1", "model": "stub-builder-1", "body": doc{"messages": []doc{{"content": "refused: a symlink"}}}},
				{"kind": "usage", "request_id": "r1", "usage": doc{"prompt_tokens": 100, "completion_tokens": 20}}},
			"log":  "adcd: review stays inactive: P31's qualification on record: no record of the route\nadcd: started\n",
			"caps": doc{"capabilities": []doc{{"name": "merge-local", "active": false}, {"name": "merge-github", "active": false}}},
		}
	}
	// at walks path (keys and indices) in v, the last step's parent returned
	// with its key, so a row can set or delete one field.
	set := func(v map[string]any, value any, path ...any) {
		var cur any = v
		for _, p := range path[:len(path)-1] {
			switch k := p.(type) {
			case string:
				cur = cur.(map[string]any)[k]
			case int:
				cur = cur.([]map[string]any)[k]
			}
		}
		switch k := path[len(path)-1].(type) {
		case string:
			if value == nil {
				delete(cur.(map[string]any), k)
			} else {
				cur.(map[string]any)[k] = value
			}
		case int:
			cur.([]map[string]any)[k] = value.(map[string]any)
		}
	}
	run := func(t *testing.T, v map[string]any) (string, int) {
		t.Helper()
		dir := t.TempDir()
		js := func(x any) string {
			b, err := json.Marshal(x)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
		lines := func(xs []map[string]any) string {
			var b strings.Builder
			for _, x := range xs {
				b.WriteString(js(x) + "\n")
			}
			return b.String()
		}
		files := map[string]string{"case.json": js(v["case"]), "show.json": js(v["show"]), "events.jsonl": lines(v["events"].([]map[string]any)),
			"stub.jsonl": lines(v["stub"].([]map[string]any)), "adcd-1.log": v["log"].(string), "capabilities.json": js(v["caps"]), "canary": "ADC-CANARY-x\n"}
		if _, ok := v["nostub"]; ok {
			delete(files, "stub.jsonl")
		}
		if s, ok := v["second"]; ok {
			files["second.json"] = js(s)
		}
		for name, content := range files {
			write(t, filepath.Join(dir, name), content, 0o644)
		}
		cmd := exec.Command(node, "../../scripts/lib/cycle-expect.mjs", filepath.Join(dir, "case.json"), dir, "c")
		out, err := cmd.CombinedOutput()
		code := 0
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), code
	}
	if out, code := run(t, base()); code != 0 || !strings.HasPrefix(out, "| c | gate completed, review held review_inactive | build found/completed_with_findings; gate empty/completed_clean | 100/20 | 1/0 |") {
		t.Fatalf("a run that shows its expect block: exit %d, %s", code, out)
	}
	// A kind whose results are never selected (selected: false): no node of
	// it has a selected result, and the first attempt's own result, when the
	// first attempt is such a node's, is the expected one.
	unselected := func() map[string]any {
		v := base()
		set(v, map[string]any{"outcome": "could_not_run", "reason": "provider_request_refused", "selected": false}, "case", "expect", "nodes", "build")
		set(v, map[string]any{"id": "n2", "kind": "build", "round": 1, "outcome": nil, "reason": nil, "claimed_at": "t"}, "show", "bench", "nodes", 0)
		fa := v["show"].(map[string]any)["first_attempt"].(map[string]any)
		fa["node"], fa["outcome"], fa["reason"] = "n2", "could_not_run", "provider_request_refused"
		return v
	}
	if out, code := run(t, unselected()); code != 0 || !strings.Contains(out, "build could_not_run/provider_request_refused (none selected)") {
		t.Errorf("a kind none of whose results is selected: exit %d, %s", code, out)
	}
	for name, c := range map[string]struct {
		edit func(map[string]any)
		want string
	}{
		"a selected result": {func(v map[string]any) {
			set(v, map[string]any{"id": "n2", "kind": "build", "round": 1, "outcome": "found", "reason": "completed_with_findings", "claimed_at": "t"}, "show", "bench", "nodes", 0)
		}, "build n2 (round 1) has a selected result"},
		"the first attempt's other result": {func(v map[string]any) {
			v["show"].(map[string]any)["first_attempt"].(map[string]any)["reason"] = "executor_incomplete"
		}, "the first attempt (build n2) recorded could_not_run/executor_incomplete"},
	} {
		v := unselected()
		c.edit(v)
		if out, code := run(t, v); code != 1 || !strings.Contains(out, c.want) {
			t.Errorf("unselected, %s: exit %d, %s", name, code, out)
		}
	}
	for name, c := range map[string]struct {
		edit func(map[string]any)
		want string
	}{
		"the review not held review_inactive": {func(v map[string]any) {
			set(v, []map[string]any{}, "show", "card", "nodes", 4, "holds")
		}, "is held, not held review_inactive"},
		"adcd's start not naming why": {func(v map[string]any) { v["log"] = "adcd: started\n" }, "does not name why review is inactive"},
		"the task closed":             {func(v map[string]any) { set(v, "done_candidate", "show", "card", "column") }, "the task closed (done_candidate)"},
		"the gate not completed":      {func(v map[string]any) { set(v, "claimed", "show", "card", "nodes", 3, "state") }, "the gate is not completed: claimed"},
		"no review node": {func(v map[string]any) {
			v["show"].(map[string]any)["card"].(map[string]any)["nodes"] = v["show"].(map[string]any)["card"].(map[string]any)["nodes"].([]map[string]any)[:4]
		}, "no review node"},
		"a hold not expected": {func(v map[string]any) {
			set(v, []map[string]any{{"hold": "budget_rail", "signal_instance": "rail"}}, "show", "card", "nodes", 1, "holds")
		}, "the task's holds are [budget_rail], not []"},
		"an expected hold signal absent": {func(v map[string]any) {
			set(v, []string{"placement: destination stub"}, "case", "expect", "hold_signals")
		}, `no hold's signal is "placement: destination stub"`},
		"another outcome": {func(v map[string]any) { set(v, "failed", "show", "bench", "nodes", 2, "outcome") }, "gate n4 (round 1) is failed/completed_clean, not empty/completed_clean"},
		"no bench read":   {func(v map[string]any) { set(v, nil, "show", "bench") }, "the task read carries no bench nodes"},
		"an absent kind claimed": {func(v map[string]any) {
			v["show"].(map[string]any)["bench"].(map[string]any)["nodes"] = append(v["show"].(map[string]any)["bench"].(map[string]any)["nodes"].([]map[string]any),
				map[string]any{"id": "n1", "kind": "plan", "outcome": "empty", "reason": "completed_clean", "claimed_at": "t"})
		}, "plan n1 was claimed"},
		"a merge node": {func(v map[string]any) {
			v["show"].(map[string]any)["card"].(map[string]any)["nodes"] = append(v["show"].(map[string]any)["card"].(map[string]any)["nodes"].([]map[string]any),
				map[string]any{"id": "n9", "kind": "merge", "state": "waiting", "holds": []map[string]any{}})
		}, "a merge node is recorded"},
		"merge-local active": {func(v map[string]any) {
			set(v, true, "caps", "capabilities", 0, "active")
		}, "shows merge-local not inactive"},
		"the claim before the approval": {func(v map[string]any) {
			set(v, "node.claimed/v1", "events", 1, "kind")
			set(v, "approval.recorded/v1", "events", 2, "kind")
		}, "approval.recorded (3) does not precede the first node.claimed (2)"},
		"a gap in the stream": {func(v map[string]any) { set(v, 7, "events", 3, "seq") }, "the event stream skips from seq 3 to 7"},
		"a canary in a request": {func(v map[string]any) {
			set(v, map[string]any{"messages": []map[string]any{{"content": "refused: a symlink HIDDEN-CANARY-1"}}}, "stub", 0, "body")
		}, "canary HIDDEN-CANARY-1 in a builder request (r1)"},
		"what the builder must see unseen": {func(v map[string]any) {
			set(v, map[string]any{"messages": []map[string]any{{"content": "ok"}}}, "stub", 0, "body")
		}, "no builder request carries refused: a symlink"},
		"other tokens": {func(v map[string]any) {
			set(v, map[string]any{"prompt_tokens": 101, "completion_tokens": 20}, "stub", 1, "usage")
		}, "the stub served 101/20 tokens, not 100/20"},
		"no first attempt": {func(v map[string]any) { set(v, nil, "show", "first_attempt") }, "the task read has no first attempt"},
		"an attempt where none may start": {func(v map[string]any) {
			set(v, true, "case", "expect", "no_attempt")
		}, "an attempt started"},
		"the second task admitted": {func(v map[string]any) {
			set(v, false, "case", "expect", "second_task_admitted")
			v["second"] = map[string]any{"card": map[string]any{"admitted": true}}
		}, "the second task is admitted (true) while the first is held"},
		"gateway requests named": {func(v map[string]any) {
			set(v, []map[string]any{{"file": "requests/a.json", "status": 403}}, "case", "expect", "gateway_requests")
		}, "gateway_requests: this driver does not send requests"},
		// Opus #435 H1: the run's minted canary (hostile-gate-output's gate
		// writes it into its results mount) in a request, or in a saved
		// operator read.
		"the minted canary in a builder request": {func(v map[string]any) {
			set(v, map[string]any{"messages": []map[string]any{{"content": "refused: a symlink ADC-CANARY-x"}}}, "stub", 0, "body")
		}, "the run's minted canary in a builder request (r1)"},
		"the minted canary in the task read": {func(v map[string]any) {
			set(v, "ADC-CANARY-x", "show", "card", "title")
		}, "the run's minted canary in the operator's show.json"},
		"the minted canary in the event stream": {func(v map[string]any) {
			set(v, "ADC-CANARY-x", "events", 0, "subject")
		}, "the run's minted canary in the operator's events.jsonl"},
		// Opus #435 M3: an unreadable stub log is a failure, never no requests;
		// an attempt that started made a builder request.
		"no stub log": {func(v map[string]any) { v["nostub"] = true }, "the stub provider's log cannot be read"},
		"an attempt and no builder request": {func(v map[string]any) {
			v["stub"] = []map[string]any{{"kind": "usage", "request_id": "r1", "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 20}}}
		}, "an attempt started, and the stub logged no builder request"},
		// Mythos #435 L2: a full stub log stopped logging requests.
		"a full stub log": {func(v map[string]any) {
			v["stub"] = append(v["stub"].([]map[string]any), map[string]any{"kind": "log_full", "logged": 1000})
		}, "the stub provider's log is full"},
		// Mythos #435 L3: an absent key of the first attempt is missing.
		"the first attempt's charge absent": {func(v map[string]any) {
			delete(v["show"].(map[string]any)["first_attempt"].(map[string]any), "charge")
		}, "the first attempt's charge is null or absent"},
	} {
		t.Run(name, func(t *testing.T) {
			v := base()
			c.edit(v)
			out, code := run(t, v)
			if code != 1 || !strings.Contains(out, "fail: ") || !strings.Contains(out, c.want) {
				t.Errorf("exit %d, %q; want a failure naming %q", code, out, c.want)
			}
		})
	}
}
