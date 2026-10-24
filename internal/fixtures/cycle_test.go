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
// holds, whose absent kinds are none of its nodes', and whose canaries
// each occur in a file of the task's hidden/ (its acceptance) or, a decoy
// (P24c), of the case's base overlay, and are not
// carried by the scenario (the positive control: the stub cannot leak them
// itself). Its usage, when given, is the scenario's sum, and is given only
// for a scenario whose every script is replies.
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
		if c.Expect.Absent != nil {
			for _, k := range *c.Expect.Absent {
				if _, ok := c.Expect.Nodes[string(k)]; ok {
					t.Errorf("%s: %s is both expected and absent", name, k)
				}
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
		var hidden []string
		hiddenDir := filepath.Join("../../fixtures/t0", string(c.Project), "tasks", string(c.Task), "hidden")
		if err := filepath.WalkDir(hiddenDir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(path)
			hidden = append(hidden, string(b))
			return err
		}); err != nil {
			t.Errorf("%s: the task's hidden/: %v", name, err)
		}
		// P24c: the contents of the case's base overlay's regular files (a
		// symlink in it is never followed).
		var overlay []string
		if c.BaseOverlay != nil {
			_ = filepath.WalkDir(filepath.Join(filepath.Dir(file), string(*c.BaseOverlay)), func(path string, d os.DirEntry, err error) error {
				if err == nil && d.Type().IsRegular() {
					b, rerr := os.ReadFile(path)
					overlay, err = append(overlay, string(b)), rerr
				}
				return err
			})
		}
		in := func(files []string, s string) bool {
			for _, f := range files {
				if strings.Contains(f, s) {
					return true
				}
			}
			return false
		}
		for _, canary := range c.Expect.Canaries {
			if strings.Contains(string(body), canary) {
				t.Errorf("%s: the scenario itself carries the canary %q", name, canary)
			}
			if !in(hidden, canary) && !in(overlay, canary) {
				t.Errorf("%s: the canary %q is in no file of %s nor of its base overlay", name, canary, hiddenDir)
			}
		}
		// P24c: a reviewer canary is in a file of the case's base overlay, or
		// is the driver's {canary}, which the case's profile overlay carries;
		// the scenario never carries it (the stub cannot leak it itself). Its
		// case records a review: it names its review node, or ends at the
		// candidate (stub mode: the review recorded and held review_inactive,
		// #226 entry 189; the live route reviews it), so a run with no review
		// cannot hold it trivially where a reviewer runs.
		if c.Expect.ReviewerCanaries != nil {
			if _, ok := c.Expect.Nodes["review"]; !ok && c.Expect.Until != "candidate" {
				t.Errorf("%s: it has reviewer canaries, but records no review (no review node, and it does not end at the candidate)", name)
			}
			for _, canary := range *c.Expect.ReviewerCanaries {
				found := canary == "{canary}" && strings.Contains(string(c.ProfileOverlay), "{canary}") || in(overlay, canary)
				if !found || strings.Contains(string(body), canary) {
					t.Errorf("%s: the reviewer canary %q is in no file of its base overlay (nor its profile overlay's {canary}), or the scenario carries it", name, canary)
				}
			}
		}
		// P24c: what the builder must see comes from the run, never from the scenario.
		if c.Expect.BuilderSees != nil {
			for _, s := range *c.Expect.BuilderSees {
				if strings.Contains(string(body), s) {
					t.Errorf("%s: the scenario itself carries %q, which the builder must see", name, s)
				}
			}
		}
		if c.Expect.GatewayRequests != nil {
			for _, r := range *c.Expect.GatewayRequests {
				exists("a gateway request", filepath.Join(filepath.Dir(file), r.File))
			}
		}
		// The task's tokens are the builder's alone: in stub mode review stays
		// inactive (#226 entry 189), so the reviewer's script is never called.
		if c.Expect.Usage != nil {
			var s struct {
				Calls map[string]struct {
					Turns      json.RawMessage `json:"turns"`
					Persistent json.RawMessage `json:"persistent"`
					Replies    []struct {
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
			script := s.Calls["stub-builder-1"]
			if script.Turns != nil || script.Persistent != nil || script.Replies == nil {
				t.Errorf("%s: usage is given, but the scenario's builder script is not a replies script", name)
			}
			for _, r := range script.Replies {
				if r.Usage != nil {
					in, out = in+r.Usage.PromptTokens, out+r.Usage.CompletionTokens
				}
			}
			if in != c.Expect.Usage.In || out != c.Expect.Usage.Out {
				t.Errorf("%s: usage %d in and %d out; the scenario's builder's %d and %d", name, c.Expect.Usage.In, c.Expect.Usage.Out, in, out)
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
