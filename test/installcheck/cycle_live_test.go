// SPDX-License-Identifier: Apache-2.0

package installcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

// P25, D2 (§2.3's live route): before adcd starts, the live route refuses
// (exit 2, named, never printing a credential) an acceptance file that is
// not its issue's recorded digest, an attachment the issue names that is
// not given, a canary that is no line of the acceptance, and a registry
// credential the caller's environment does not hold. The prefix is a fake
// one: the real node, this repository's profile and issue fixtures, and an
// adcd that records it was started.
func TestInstalledCycleLiveRefusals(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on PATH")
	}
	root := t.TempDir()
	prefix := filepath.Join(root, "prefix")
	lib := filepath.Join(prefix, "lib/adc")
	marker := filepath.Join(root, "adcd-started")
	write(t, filepath.Join(prefix, "bin/adcd"), "#!/bin/sh\ntouch "+marker+"\n", 0o755)
	write(t, filepath.Join(prefix, "bin/adc"), "#!/bin/sh\nexit 1\n", 0o755)
	if err := os.MkdirAll(filepath.Join(lib, "node/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(node, filepath.Join(lib, "node/bin/node")); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"installed-cycle.sh", "lib/secrets.sh", "lib/base-remote.sh", "lib/sandbox-root.sh", "lib/cycle-live.sh"} {
		b, err := os.ReadFile("../../scripts/" + s)
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(lib, "scripts", s), string(b), 0o755)
	}
	profile, err := os.ReadFile("../../fixtures/m1/profiles/auto-dev-cycle.json")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(lib, "fixtures/m1/profiles/auto-dev-cycle.json"), string(profile), 0o644)
	// The issue: its project the profile's, one attachment, the acceptance file's digest recorded.
	var p struct{ ID string }
	if err := json.Unmarshal(profile, &p); err != nil {
		t.Fatal(err)
	}
	const canary = "// ADC-CANARY-live-test-0123456789"
	acceptance := filepath.Join(root, "accept_hidden_test.go")
	body := "package cli\n\n" + canary + "\n"
	write(t, acceptance, body, 0o644)
	sum := sha256.Sum256([]byte(body))
	issue, _ := json.Marshal(map[string]any{"project": p.ID, "title": "t", "requirement": "r",
		"attachments": []map[string]string{{"name": "internal/cli/accept_hidden_test.go", "digest": "sha256:" + hex.EncodeToString(sum[:])}}})
	write(t, filepath.Join(lib, "fixtures/m1/issues/170/task.json"), string(issue), 0o644)
	registry, err := os.ReadFile("../../fixtures/m1/config/registry-live.example.json")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "registry.json"), string(registry), 0o644)
	write(t, filepath.Join(root, "groups.json"), "[]", 0o644)
	const secret = "sk-not-a-real-credential-0123456789abcdef"
	run := func(env []string, args ...string) (string, int) {
		t.Helper()
		os.Remove(marker)
		cmd := exec.Command(filepath.Join(lib, "scripts/installed-cycle.sh"), append([]string{prefix, "--routes", "live", "--project", "repo",
			"--issue", "170", "--until", "candidate", "--bench", filepath.Join(root, "bench.json"), "--registry", filepath.Join(root, "registry.json"),
			"--groups", filepath.Join(root, "groups.json")}, args...)...)
		cmd.Dir = root
		cmd.Env = append(append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin", "TMPDIR="+t.TempDir()), env...)
		out, err := cmd.CombinedOutput()
		code := 0
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("%v: adcd was started", args)
		}
		if strings.Contains(string(out), secret) {
			t.Errorf("%v: the credential was printed", args)
		}
		if strings.Contains(string(out), acceptance) || strings.Contains(string(out), filepath.Join(root, "other.go")) {
			t.Errorf("%v: an acceptance file's path was printed: %s", args, out)
		}
		return string(out), code
	}
	given := "internal/cli/accept_hidden_test.go=" + acceptance
	other := filepath.Join(root, "other.go")
	write(t, other, "package cli\n", 0o644)
	held := []string{"ADC_CREDENTIAL_API_REVIEWER=" + secret}
	for name, c := range map[string]struct {
		env  []string
		args []string
		want string
	}{
		"another file":       {held, []string{"--acceptance", "internal/cli/accept_hidden_test.go=" + other, "--canary", canary}, "its file is not the recorded sha256:"},
		"another name":       {held, []string{"--acceptance", "internal/cli/x_test.go=" + acceptance, "--canary", canary}, "names no such attachment"},
		"a canary not in it": {held, []string{"--acceptance", given, "--canary", "// ADC-CANARY-somewhere-else-0123"}, "--canary is no line of the acceptance files"},
		"no credential":      {nil, []string{"--acceptance", given, "--canary", canary}, "the registry's credential api-reviewer is not in this environment (ADC_CREDENTIAL_API_REVIEWER)"},
		"no canary":          {held, []string{"--acceptance", given}, "usage: installed-cycle.sh"},
		"--canary no value":  {held, []string{"--acceptance", given, "--canary"}, "usage: installed-cycle.sh"},
	} {
		out, code := run(c.env, c.args...)
		if code != 2 || !strings.Contains(out, "installed-cycle: could_not_run: ") || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, %s", name, code, out)
		}
	}
}

// D2: an excerpt of a live run's output (a failed task create, a timed-out
// wait) names neither the operator's acceptance file, by its path, nor the
// unpublished canary.
func TestLiveExcerptsAreRedacted(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "store/accept_hidden_test.go")
	canary := "// ADC-CANARY-live-0123456789abcdef"
	out := filepath.Join(dir, "task.out")
	write(t, out, "adc task create: --attach: open "+store+": permission denied\n"+canary+"\n", 0o600)
	cmd := exec.Command("bash", "-c", `set -euo pipefail; . ../../scripts/lib/cycle-live.sh; acceptance=("internal/cli/accept_hidden_test.go=$1"); live_canary="$2"; redacted "$3" 400`, "x", store, canary, out)
	cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
	got, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, got)
	}
	if s := string(got); strings.Contains(s, store) || strings.Contains(s, canary) || !strings.Contains(s, "open <acceptance file>: permission denied <canary>") {
		t.Errorf("redacted: %q", s)
	}
}

// #226 entry 199: the live route records its benchmark only from the
// restarted adcd: after the driver stopped adcd, verified its log and
// started it again on the same state (a refused start dies with its
// excerpt), and before the restarted adcd is stopped.
func TestLiveBenchOnlyAfterTheRestart(t *testing.T) {
	b, err := os.ReadFile("../../scripts/installed-cycle.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	at := func(what string, from int) int {
		t.Helper()
		i := strings.Index(s[from:], what)
		if i < 0 {
			t.Fatalf("installed-cycle.sh has no %q after byte %d", what, from)
		}
		return from + i
	}
	first := at("\nstop\n", 0)
	restarted := at(`within 60 "adcd to restart" started`, first)
	bench := at("live_bench", restarted)
	at("\nstop\n", bench)
	if n := strings.Count(s, "live_bench"); n != 1 {
		t.Errorf("installed-cycle.sh calls live_bench %d times, not once", n)
	}
	if strings.Contains(s[:first], "live_bench") {
		t.Error("live_bench is called before adcd is stopped")
	}
	lib, err := os.ReadFile("../../scripts/lib/cycle-live.sh")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(lib), "dev bench record --task"); n != 1 || !strings.Contains(string(lib), "live_bench() {") {
		t.Errorf("cycle-live.sh records the benchmark %d times, or outside live_bench", n)
	}
}
