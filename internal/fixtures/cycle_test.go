// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/profile"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const m1 = "../../fixtures/m1"

// P24b: every installed cycle case (fixtures/m1/cases/<case>/case.json) is
// a testing/cycle-case document whose project has a profile and a T0
// project, whose profile overlay merged over that profile (RFC 7396) is a
// valid profile, whose task is one of that project's, whose scenario
// exists, whose overlay and hook are files of the case, whose expected
// outcomes are registered reasons of their outcomes and holds registered
// holds, and whose canaries the scenario does not carry (the positive
// control: the stub cannot leak them itself). Its usage, when given, is the
// scenario's sum.
func TestCycleCases(t *testing.T) {
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	cases, err := filepath.Glob(m1 + "/cases/*/case.json")
	if err != nil || len(cases) == 0 {
		t.Fatalf("no cycle case: %v", err)
	}
	for _, file := range cases {
		name := filepath.Base(filepath.Dir(file))
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := contract.Validate("testing/cycle-case", raw); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var c types.CycleCase
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		exists := func(what, path string) {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("%s: %s: %v", name, what, err)
			}
		}
		profileFile := filepath.Join(m1, "profiles", string(c.Project)+".json")
		exists("the profile", profileFile)
		if c.ProfileOverlay != nil {
			var base, overlay any
			raw, err := os.ReadFile(profileFile)
			if err == nil {
				err = json.Unmarshal(raw, &base)
			}
			if err == nil {
				err = json.Unmarshal(c.ProfileOverlay, &overlay)
			}
			if err != nil {
				t.Fatal(err)
			}
			merged, _ := json.Marshal(mergePatch(base, overlay))
			if _, err := profile.Load(merged); err != nil {
				t.Errorf("%s: the profile with its overlay: %v", name, err)
			}
		}
		exists("the task", filepath.Join("../../fixtures/t0", string(c.Project), "tasks", string(c.Task), "task.json"))
		scenario := filepath.Join(m1, "stub-provider/scenarios", string(c.Scenario)+".json")
		exists("the scenario", scenario)
		if c.BaseOverlay != nil {
			exists("the base overlay", filepath.Join(filepath.Dir(file), string(*c.BaseOverlay)))
		}
		if c.RemoteHook != nil {
			exists("the remote hook", filepath.Join(filepath.Dir(file), string(*c.RemoteHook)))
		}
		for node, o := range c.Expect.Nodes {
			if err := reg.Check(vocab.Result{Outcome: vocab.Outcome(o.Outcome), Reason: types.ReasonCodeOf(o.Reason), Detail: "expected"}); err != nil {
				t.Errorf("%s: node %s: %v", name, node, err)
			}
		}
		for _, h := range c.Expect.Holds {
			if _, ok := reg.Holds[h]; !ok {
				t.Errorf("%s: %s is not a registered hold", name, h)
			}
		}
		body, err := os.ReadFile(scenario)
		if err != nil {
			continue
		}
		for _, canary := range c.Expect.Canaries {
			if strings.Contains(string(body), canary) {
				t.Errorf("%s: the scenario itself carries the canary %q", name, canary)
			}
		}
		if c.Expect.Usage != nil {
			var s struct {
				Calls map[string]struct {
					Replies []struct {
						Usage *struct {
							PromptTokens     int64 `json:"prompt_tokens"`
							CompletionTokens int64 `json:"completion_tokens"`
						} `json:"usage"`
					}
				}
			}
			if err := json.Unmarshal(body, &s); err != nil {
				t.Fatal(err)
			}
			var in, out int64
			for _, script := range s.Calls {
				for _, r := range script.Replies {
					if r.Usage != nil {
						in, out = in+r.Usage.PromptTokens, out+r.Usage.CompletionTokens
					}
				}
			}
			if in != c.Expect.Usage.In || out != c.Expect.Usage.Out {
				t.Errorf("%s: usage %d in and %d out; the scenario's %d and %d", name, c.Expect.Usage.In, c.Expect.Usage.Out, in, out)
			}
		}
	}
}

// mergePatch is RFC 7396's merge of patch over target.
func mergePatch(target, patch any) any {
	p, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	t, ok := target.(map[string]any)
	if !ok {
		t = map[string]any{}
	}
	out := make(map[string]any, len(t))
	for k, v := range t {
		out[k] = v
	}
	for k, v := range p {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = mergePatch(out[k], v)
		}
	}
	return out
}
