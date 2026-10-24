// SPDX-License-Identifier: Apache-2.0

package installcheck

import (
	"cmp"
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
		mode, _ := v["mode"].(string)
		cmd := exec.Command(node, "../../scripts/lib/cycle-expect.mjs", filepath.Join(dir, "case.json"), dir, "c", cmp.Or(mode, "stub"))
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
	// #436 (Opus #436 answer 5): each such node's recorded results, read from
	// bench.nodes[].results[]: "own", an attempt's own refused result, one
	// per attempt, each naming one of the node's attempts; "none", the
	// controller's result with no attempt.
	refused := func(id, att string) map[string]any {
		var a any = att
		if att == "" {
			a = nil
		}
		return map[string]any{"id": id, "outcome": "could_not_run", "reason": "provider_request_refused", "selected": false, "attempt": a}
	}
	unselected := func() map[string]any {
		v := base()
		set(v, map[string]any{"outcome": "could_not_run", "reason": "provider_request_refused", "selected": false, "attempt": "own"}, "case", "expect", "nodes", "build")
		set(v, map[string]any{"id": "n2", "kind": "build", "round": 1, "outcome": nil, "reason": nil, "claimed_at": "t",
			"attempts": []map[string]any{{"id": "att_1", "lease": "lse_1"}, {"id": "att_2", "lease": "lse_2"}},
			"results":  []map[string]any{refused("res_1", "att_1"), refused("res_2", "att_2")}}, "show", "bench", "nodes", 0)
		fa := v["show"].(map[string]any)["first_attempt"].(map[string]any)
		fa["node"], fa["outcome"], fa["reason"] = "n2", "could_not_run", "provider_request_refused"
		return v
	}
	if out, code := run(t, unselected()); code != 0 || !strings.Contains(out, "build could_not_run/provider_request_refused (none selected)") {
		t.Errorf("a kind none of whose results is selected: exit %d, %s", code, out)
	}
	controller := func() map[string]any {
		v := unselected()
		set(v, "none", "case", "expect", "nodes", "build", "attempt")
		set(v, []map[string]any{refused("res_1", ""), refused("res_2", "")}, "show", "bench", "nodes", 0, "results")
		return v
	}
	if out, code := run(t, controller()); code != 0 {
		t.Errorf("results the controller recorded with no attempt: exit %d, %s", code, out)
	}
	results := func(v map[string]any) []map[string]any {
		return v["show"].(map[string]any)["bench"].(map[string]any)["nodes"].([]map[string]any)[0]["results"].([]map[string]any)
	}
	for name, c := range map[string]struct {
		base func() map[string]any
		edit func(map[string]any)
		want string
	}{
		"a selected result": {unselected, func(v map[string]any) {
			set(v, "found", "show", "bench", "nodes", 0, "outcome")
			set(v, "completed_with_findings", "show", "bench", "nodes", 0, "reason")
		}, "build n2 (round 1) has a selected result"},
		"the first attempt's other result": {unselected, func(v map[string]any) {
			v["show"].(map[string]any)["first_attempt"].(map[string]any)["reason"] = "executor_incomplete"
		}, "the first attempt (build n2) recorded could_not_run/executor_incomplete"},
		"no results list": {unselected, func(v map[string]any) {
			set(v, nil, "show", "bench", "nodes", 0, "results")
		}, "build n2 shows no recorded results"},
		"an empty results list": {unselected, func(v map[string]any) {
			set(v, []map[string]any{}, "show", "bench", "nodes", 0, "results")
		}, "build n2 shows no recorded results"},
		"a result selected": {unselected, func(v map[string]any) { results(v)[1]["selected"] = true }, "build n2's result res_2 is selected (true)"},
		"a result of another reason": {unselected, func(v map[string]any) { results(v)[0]["reason"] = "adapter_protocol_error" },
			"build n2's result res_1 is could_not_run/adapter_protocol_error"},
		"own: a result with no attempt": {unselected, func(v map[string]any) { results(v)[1]["attempt"] = nil },
			"build n2's result res_2 names attempt null, not one of the node's"},
		"own: an attempt not the node's": {unselected, func(v map[string]any) { results(v)[1]["attempt"] = "att_9" },
			"build n2's result res_2 names attempt att_9, not one of the node's"},
		"own: one result fewer than attempts": {unselected, func(v map[string]any) {
			set(v, results(v)[:1], "show", "bench", "nodes", 0, "results")
		}, "build n2 records 1 results for its 2 attempts, not one per attempt"},
		"own: two results for one attempt": {unselected, func(v map[string]any) { results(v)[1]["attempt"] = "att_1" },
			"build n2 records 2 results for its 2 attempts, not one per attempt"},
		"none: a result naming an attempt": {controller, func(v map[string]any) { results(v)[0]["attempt"] = "att_1" },
			"build n2's result res_1 names attempt att_1, where the controller records it with none"},
	} {
		v := c.base()
		c.edit(v)
		if out, code := run(t, v); code != 1 || !strings.Contains(out, c.want) {
			t.Errorf("unselected, %s: exit %d, %s", name, code, out)
		}
	}
	// D2, the live route: review is active, so the task closes a candidate
	// after review, and adcd's start never says review stays inactive. It has
	// no stub log (the driver checks the egress capture); out/canary is the
	// acceptance's canary, in neither operator read.
	livebase := func() map[string]any {
		v := base()
		v["mode"] = "live"
		v["nostub"] = true
		ex := v["case"].(map[string]any)["expect"].(map[string]any)
		delete(ex, "builder_sees") // stub-only: the driver's live expect block names neither
		delete(ex, "usage")
		v["log"] = "adcd: started\n"
		set(v, "done_candidate", "show", "card", "column")
		set(v, map[string]any{"id": "n5", "kind": "review", "state": "completed", "holds": []map[string]any{}}, "show", "card", "nodes", 4)
		return v
	}
	if out, code := run(t, livebase()); code != 0 || !strings.HasPrefix(out, "| c | closed a candidate after review |") {
		t.Errorf("a live run that shows its expect block: exit %d, %s", code, out)
	}
	for name, edit := range map[string]func(map[string]any){
		"review stays inactive":             func(v map[string]any) { v["log"] = "adcd: review stays inactive: P31's qualification on record: x\n" },
		"not closed":                        func(v map[string]any) { set(v, "verifying", "show", "card", "column") },
		"the acceptance canary in the read": func(v map[string]any) { set(v, "ADC-CANARY-x", "show", "card", "title") },
	} {
		v := livebase()
		edit(v)
		if out, code := run(t, v); code != 1 || !strings.Contains(out, "fail: ") {
			t.Errorf("live, %s: exit %d, %s", name, code, out)
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
