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
	// Every child the driver runs through PATH, and its node, log their
	// arguments (Opus #440 H1, M2): no run may name the canary or an
	// acceptance file to any of them.
	argv := filepath.Join(root, "argv.log")
	wrap := filepath.Join(root, "wrap")
	logs := func(real string) string {
		return "#!/bin/sh\nprintf '%s %s\\n' \"$0\" \"$*\" >>" + argv + "\nexec " + real + " \"$@\"\n"
	}
	write(t, filepath.Join(lib, "node/bin/node"), logs(node), 0o755)
	for _, c := range []string{"grep", "env", "cat", "sha256sum", "stat", "head", "tr", "sed", "cut", "sort", "git", "timeout", "realpath", "mktemp", "find"} {
		real, err := exec.LookPath(c)
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(wrap, c), logs(real), 0o755)
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
		cmd.Env = append(append(testguard.HomeEnv(t), "PATH="+wrap+":/usr/bin:/bin", "TMPDIR="+t.TempDir()), env...)
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
	// The canary from a file of the caller's, mode 0600 (Opus #440 M2).
	canaryFile := func(name, content string, mode os.FileMode) string {
		f := filepath.Join(root, name)
		write(t, f, content, mode)
		if err := os.Chmod(f, mode); err != nil {
			t.Fatal(err)
		}
		return f
	}
	own := canaryFile("canary", canary+"\n", 0o600)
	for name, c := range map[string]struct {
		env  []string
		args []string
		want string
	}{
		"another file":               {held, []string{"--acceptance", "internal/cli/accept_hidden_test.go=" + other, "--canary-file", own}, "its file is not the recorded sha256:"},
		"another name":               {held, []string{"--acceptance", "internal/cli/x_test.go=" + acceptance, "--canary-file", own}, "names no such attachment"},
		"a canary not in it":         {held, []string{"--acceptance", given, "--canary-file", canaryFile("elsewhere", "// ADC-CANARY-somewhere-else-0123\n", 0o600)}, "the canary is no line of the acceptance files"},
		"no credential":              {nil, []string{"--acceptance", given, "--canary-file", own}, "the registry's credential api-reviewer is not in this environment (ADC_CREDENTIAL_API_REVIEWER)"},
		"no canary":                  {held, []string{"--acceptance", given}, "usage: installed-cycle.sh"},
		"--canary-file no value":     {held, []string{"--acceptance", given, "--canary-file"}, "usage: installed-cycle.sh"},
		"a canary file others read":  {held, []string{"--acceptance", given, "--canary-file", canaryFile("loose", canary+"\n", 0o644)}, "a file of yours, mode 0600"},
		"a canary file of two lines": {held, []string{"--acceptance", given, "--canary-file", canaryFile("two", canary+"\nmore\n", 0o600)}, "must hold the canary line alone"},
		"a canary of whitespace":     {held, []string{"--acceptance", given, "--canary-file", canaryFile("blank", "\t\t   \n", 0o600)}, "at least 16 characters past its whitespace"},
	} {
		out, code := run(c.env, c.args...)
		if code != 2 || !strings.Contains(out, "installed-cycle: could_not_run: ") || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, %s", name, code, out)
		}
	}
	// Opus #440 L1: a kept live run says what its directory holds.
	if out, code := run(nil, "--acceptance", given, "--canary-file", own, "--keep"); code != 2 ||
		!strings.Contains(out, "holds the acceptance canary, the acceptance files and every captured request: never share it") {
		t.Errorf("a kept live run: exit %d, %s", code, out)
	}
	logged, err := os.ReadFile(argv)
	if err != nil || !strings.Contains(string(logged), "/sha256sum ") {
		t.Fatalf("the wrappers logged nothing of the runs (%v): %q", err, logged)
	}
	for _, s := range []string{canary, "ADC-CANARY-live-test", acceptance, other, secret} {
		if strings.Contains(string(logged), s) {
			t.Errorf("a child process's arguments hold %q", s)
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
	// Opus #440 M1: --bench is written last, after the real ~/.config/adc
	// is found unchanged, right before "ok"; nothing else writes it.
	checked := at(`[[ "$after" == "$before" ]] || die`, bench)
	write := at("live_write_bench", checked)
	if ok := at(`step "ok"`, write); strings.Contains(s[write+len("live_write_bench"):ok], "die") || strings.Count(s, "live_write_bench") != 1 {
		t.Error("live_write_bench is not the last check before ok, or is called more than once")
	}
	if strings.Count(string(lib), `"$bench"`) != 1 || strings.Contains(string(lib), "install ") {
		t.Error("cycle-live.sh writes --bench other than in live_write_bench")
	}
}

// Opus #440 L2: live_write_bench never writes over a file, even one made
// since the run's start; it writes the record when none is there.
func TestLiveBenchNeverClobbers(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "out/bench.json"), `{"record":1}`, 0o600)
	bench := filepath.Join(dir, "bench.json")
	run := func() (string, error) {
		cmd := exec.Command("bash", "-c", `set -euo pipefail; . ../../scripts/lib/secrets.sh; . ../../scripts/lib/cycle-live.sh; step() { echo "$*"; }; work="$1" bench="$2"; live_write_bench`, "x", dir, bench)
		cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	write(t, bench, "someone else's\n", 0o644)
	if out, err := run(); err == nil || !strings.Contains(out, "the benchmark cannot be written") {
		t.Errorf("over an existing file: %v %s", err, out)
	}
	if b, _ := os.ReadFile(bench); string(b) != "someone else's\n" {
		t.Errorf("the existing file was overwritten: %q", b)
	}
	os.Remove(bench)
	if out, err := run(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if b, _ := os.ReadFile(bench); string(b) != `{"record":1}` {
		t.Errorf("the record written: %q", b)
	}
}
