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

// The mode itself: a request whose first user message's digest the map
// names gets that decision and is no call of its model's script (the
// scenario's own reply for that model is still the first one); another
// prompt gets the scenario's reply.
func TestStubAnswersByPrompt(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on PATH")
	}
	dir := t.TempDir()
	sum := sha256.Sum256([]byte("a qualification prompt"))
	write(t, filepath.Join(dir, "answers.json"), `{"sha256:`+hex.EncodeToString(sum[:])+`":"no_go"}`, 0o600)
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
	decision := func(prompt string) string {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"model": "stub-reviewer-1", "messages": []map[string]string{{"role": "system", "content": "s"}, {"role": "user", "content": prompt}}})
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
		var d struct{ Decision string }
		if err := json.Unmarshal([]byte(doc.Choices[0].Message.Content), &d); err != nil {
			t.Fatal(err)
		}
		return d.Decision
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
