// SPDX-License-Identifier: Apache-2.0

package installcheck

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

// #226 entry 201: the stub's --answer-by-prompt mode is the live-route
// rehearsal's alone. Nothing but scripts/installed-cycle-rehearsal.sh (its
// one caller, with the live route) passes it: the stub set's driver, its set
// runner and the Makefile never do, and no scenario can ask for it (it is a
// flag, not a scenario key).
func TestAnswerByPromptIsTheRehearsalsAlone(t *testing.T) {
	var users []string
	for _, glob := range []string{"../../scripts/*.sh", "../../scripts/lib/*", "../../Makefile", "../../fixtures/m1/stub-provider/scenarios/*.json", "../../fixtures/m1/cases/*/case.json"} {
		files, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "answer-by-prompt") || strings.Contains(string(b), "answered_by_prompt") {
				users = append(users, filepath.Base(f))
			}
		}
	}
	for _, u := range users {
		if u != "installed-cycle-rehearsal.sh" {
			t.Errorf("--answer-by-prompt is named by %s, which is not the rehearsal", u)
		}
	}
}

// The mode itself: a reviewer-model request whose first user message's
// digest the map names gets that decision and is no call of its model's
// script (the scenario's own reply for that model is still the first one);
// another prompt, or the same prompt from another model (a builder's: a map
// cannot steer a build; Opus #441 L2), gets the scenario's reply. A map that
// is not a plain object of plain objects is refused at start.
func TestStubAnswersByPrompt(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on PATH")
	}
	dir := t.TempDir()
	sum := sha256.Sum256([]byte("a qualification prompt"))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	for name, bad := range map[string]string{
		"an array":            `[]`,
		"no model":            `{"answers":{"` + digest + `":"no_go"}}`,
		"answers as an array": `{"model":"stub-reviewer-1","answers":[]}`,
		"an empty model":      `{"model":"","answers":{}}`,
		"another key":         `{"model":"stub-reviewer-1","answers":{},"x":1}`,
		"a bad decision":      `{"model":"stub-reviewer-1","answers":{"` + digest + `":"maybe"}}`,
	} {
		write(t, filepath.Join(dir, "bad.json"), bad, 0o600)
		cmd := exec.Command(node, "../../fixtures/m1/stub-provider/server.mjs", "--scenario", "../../fixtures/m1/stub-provider/scenarios/go-mini-feature-1.json",
			"--listen", "127.0.0.1:0", "--answer-by-prompt", filepath.Join(dir, "bad.json"))
		cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "--answer-by-prompt: ") {
			t.Errorf("%s: %v %s", name, err, out)
		}
	}
	write(t, filepath.Join(dir, "answers.json"), `{"model":"stub-reviewer-1","answers":{"`+digest+`":"no_go"}}`, 0o600)
	cmd := exec.Command(node, "../../fixtures/m1/stub-provider/server.mjs", "--scenario", "../../fixtures/m1/stub-provider/scenarios/go-mini-feature-1.json",
		"--listen", "127.0.0.1:0", "--answer-by-prompt", filepath.Join(dir, "answers.json"))
	cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "listening ") {
		t.Fatalf("%q %v", line, err)
	}
	base := strings.TrimSpace(strings.TrimPrefix(line, "listening "))
	post := func(model, prompt string) string {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"model": model, "messages": []map[string]string{{"role": "system", "content": "s"}, {"role": "user", "content": prompt}}})
		res, err := http.Post(base+"/chat/completions", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var doc struct {
			Choices []struct{ Message struct{ Content string } }
		}
		if err := json.NewDecoder(res.Body).Decode(&doc); err != nil || len(doc.Choices) != 1 {
			t.Fatalf("status %d: %v", res.StatusCode, err)
		}
		return doc.Choices[0].Message.Content
	}
	decision := func(prompt string) string {
		t.Helper()
		var d struct{ Decision string }
		if err := json.Unmarshal([]byte(post("stub-reviewer-1", prompt)), &d); err != nil {
			t.Fatal(err)
		}
		return d.Decision
	}
	if c := post("stub-builder-1", "a qualification prompt"); strings.Contains(c, "no_go") || strings.Contains(c, "The change is wrong.") {
		t.Errorf("a builder's request was answered from the map: %q", c)
	}
	if d := decision("a qualification prompt"); d != "no_go" {
		t.Errorf("the named prompt: %q", d)
	}
	if d := decision("a qualification prompt"); d != "no_go" {
		t.Errorf("the named prompt again: %q", d)
	}
	if d := decision("the task's review"); d != "go" {
		t.Errorf("another prompt: the scenario's first reviewer reply is go, got %q", d)
	}
}
