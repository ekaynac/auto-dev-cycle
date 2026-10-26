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
	// adc: dev credential-env as the example registry's mapping (TestDevCredentialEnv runs the real one), its last
	// line without its newline (read all the same, never dropped); dev scan, this checkout's (known's forms); nothing else.
	write(t, filepath.Join(prefix, "bin/adc"), "#!/bin/sh\n[ \"$1 $2\" = \"dev credential-env\" ] && { printf 'api-reviewer ADC_CREDENTIAL_API_REVIEWER'; exit 0; }\n[ \"$1 $2\" = \"dev scan\" ] && exec "+adcScan(t)+" \"$@\"\nexit 1\n", 0o755)
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
	for _, s := range []string{"installed-cycle.sh", "lib/clean-shell.sh", "lib/secrets.sh", "lib/base-remote.sh", "lib/sandbox-root.sh", "lib/cycle-live.sh"} {
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
	// Issue 171: the same attachment, another project's, so a run that gets past the credentials stops at the profile.
	other171, _ := json.Marshal(map[string]any{"project": "other", "title": "t", "requirement": "r",
		"attachments": []map[string]string{{"name": "internal/cli/accept_hidden_test.go", "digest": "sha256:" + hex.EncodeToString(sum[:])}}})
	write(t, filepath.Join(lib, "fixtures/m1/issues/171/task.json"), string(other171), 0o644)
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
		// Opus #441 r3 H1: the credential crosses the driver's re-exec on fd 3
		// and is found by its name: the run gets past it, to the profile.
		"the credential received": {held, []string{"--issue", "171", "--acceptance", given, "--canary-file", own}, "issue 171's task is project other's"},
		// Astra #440 H5: a live run's sandbox root is always its own.
		"a shared sandbox root": {held, []string{"--acceptance", given, "--canary-file", own, "--sandbox-root", root}, "--sandbox-root: the live route always makes a sandbox root of its own"},
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
	// Opus #441 r4 N2: what arrives on fd 3 is checked whole before anything
	// of it is kept or printed (clean_shell_credentials): a name that is no
	// ADC_CREDENTIAL_*, an entry without "=", and an unterminated last entry,
	// each with its counted end record, refuse the run (exit 2), naming no
	// content. The fd is crafted by a caller whose environment is exactly the
	// re-exec's (the one path on which the driver reads an fd 3 it did not
	// write itself).
	// testguard.HomeEnv's own entries (its home and XDG directories), in the re-exec's order.
	home := map[string]string{}
	for _, kv := range testguard.HomeEnv(t) {
		k, _, _ := strings.Cut(kv, "=")
		home[k] = kv
	}
	exact := []string{"ADC_CLEAN_SHELL=1", "LANG=C", home["HOME"], "PATH=" + wrap + ":/usr/bin:/bin", "TMPDIR=" + t.TempDir()}
	for _, k := range []string{"RUNTIME_DIR", "CONFIG_HOME", "DATA_HOME", "STATE_HOME", "CACHE_HOME"} {
		if kv, ok := home["XDG"+"_"+k]; ok {
			exact = append(exact, kv)
		}
	}
	for name, c := range map[string]struct{ fd3, want string }{
		"another name":         {"OTHER=" + secret + "\x00adc-credentials-end 1\x00", "the credentials received on fd 3 are malformed"},
		"no =":                 {"ADC_CREDENTIAL_API_REVIEWER" + secret + "\x00adc-credentials-end 1\x00", "the credentials received on fd 3 are malformed"},
		"an unterminated last": {"ADC_CREDENTIAL_API_REVIEWER=" + secret + "\x00adc-credentials-end 1\x00ADC_CREDENTIAL_API_REVIEWER=" + secret, "the credentials received on fd 3 are malformed"},
	} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.WriteString(c.fd3); err != nil {
			t.Fatal(err)
		}
		w.Close()
		cmd := exec.Command(filepath.Join(lib, "scripts/installed-cycle.sh"), prefix, "--routes", "live", "--project", "repo", "--issue", "170", "--until", "candidate",
			"--bench", filepath.Join(root, "bench.json"), "--registry", filepath.Join(root, "registry.json"), "--groups", filepath.Join(root, "groups.json"),
			"--acceptance", given, "--canary-file", own)
		cmd.Env = exact
		cmd.ExtraFiles = []*os.File{r}
		out, err := cmd.CombinedOutput()
		r.Close()
		if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 2 || !strings.Contains(string(out), "installed-cycle: could_not_run: "+c.want) || strings.Contains(string(out), secret) {
			t.Errorf("fd 3 with %s: %v %s", name, err, out)
		}
	}
	logged, err := os.ReadFile(argv)
	if err != nil || !strings.Contains(string(logged), "/sha256sum ") {
		t.Fatalf("the wrappers logged nothing of the runs (%v): %q", err, logged)
	}
	// The driver's re-exec (env -i ... bash "$0" "$@"; Opus #441 r3 H1) is no
	// child: it replaces the driver's own process, whose arguments the
	// operator gave. Every other line is a child's.
	var children []string
	for _, l := range strings.Split(string(logged), "\n") {
		if !strings.Contains(l, " -i ADC_CLEAN_SHELL=1 ") {
			children = append(children, l)
		}
	}
	for _, s := range []string{canary, "ADC-CANARY-live-test", acceptance, other, secret} {
		if strings.Contains(strings.Join(children, "\n"), s) {
			t.Errorf("a child process's arguments hold %q", s)
		}
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
	if i := strings.Index(string(lib), "live_write_bench() {"); i < 0 || strings.Contains(string(lib)[:i], `"$bench"`) || strings.Contains(string(lib), "install ") {
		t.Error("cycle-live.sh writes --bench other than in live_write_bench")
	}
}

// Opus #440 L2, Astra #440 M8: live_write_bench publishes as writeOnce does:
// never over a file, even one made since the run's start; the whole record
// or nothing (an unreadable record leaves no file at --bench); no temporary
// file left beside it either way; the record 0644.
func TestLiveBenchNeverClobbers(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "out/bench.json"), `{"record":1}`, 0o600)
	bench := filepath.Join(dir, "pub/bench.json")
	write(t, filepath.Join(dir, "pub/.keep"), "", 0o600)
	run := func() (string, error) {
		cmd := exec.Command("bash", "-c", `set -euo pipefail; . ../../scripts/lib/secrets.sh; . ../../scripts/lib/cycle-live.sh; step() { echo "$*"; }; work="$1" bench="$2"; live_write_bench`, "x", dir, bench)
		cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	left := func() []string {
		m, _ := filepath.Glob(filepath.Join(dir, "pub/.bench-*"))
		return m
	}
	write(t, bench, "someone else's\n", 0o644)
	if out, err := run(); err == nil || !strings.Contains(out, "the benchmark cannot be written") {
		t.Errorf("over an existing file: %v %s", err, out)
	}
	if b, _ := os.ReadFile(bench); string(b) != "someone else's\n" || len(left()) > 0 {
		t.Errorf("the existing file was overwritten (%q), or a temporary file is left: %v", b, left())
	}
	os.Remove(bench)
	if err := os.Chmod(filepath.Join(dir, "out/bench.json"), 0o000); err != nil {
		t.Fatal(err)
	}
	if out, err := run(); err == nil || len(left()) > 0 {
		t.Errorf("an unreadable record: %v %s; temporary files %v", err, out, left())
	}
	if _, err := os.Lstat(bench); err == nil {
		t.Error("an unreadable record left a file at --bench")
	}
	if err := os.Chmod(filepath.Join(dir, "out/bench.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if b, _ := os.ReadFile(bench); string(b) != `{"record":1}` || len(left()) > 0 {
		t.Errorf("the record written: %q; temporary files %v", b, left())
	}
	if fi, err := os.Stat(bench); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("the record's mode: %v %v", fi, err)
	}
}

// Opus #441 L1: the live route registers every form of its canary for
// redaction, not the raw line alone: an excerpt of a JSON output (show.json,
// an adapter's stderr) holding the canary JSON-escaped, twice, or as Go
// escapes it, prints none of them.
func TestLiveCanaryFormsAreRedacted(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on PATH")
	}
	scripts, err := filepath.Abs("../../scripts")
	if err != nil {
		t.Fatal(err)
	}
	const canary = "\tconst q7vCanary = \"ADC-<q7v>-0123456789abcdef\""
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	once, _ := json.Marshal(canary) // Go's escaping: \u003c
	twice, _ := json.Marshal(map[string]string{"stderr": `{"text":` + strings.ReplaceAll(strings.ReplaceAll(string(once), `\u003c`, "<"), `\u003e`, ">") + `}`})
	show := filepath.Join(work, "show.json")
	write(t, show, `{"a":`+string(once)+`,"b":`+string(twice)+`,"c":"`+strings.TrimSpace(canary)+`"}`, 0o600)
	cmd := exec.Command("bash", "-c", `set -euo pipefail; . "$3/lib/secrets.sh"; . "$3/lib/cycle-live.sh"; umask 077
work="$1" node="$2" here="$3" live_canary="$4" adc_scan="$6"; live_canary_known; excerpt "$5" 2000`, "x", work, node, scripts, canary, show, adcScan(t))
	cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Contains(string(out), "q7v") || strings.Count(string(out), "[redacted]") != 3 {
		t.Errorf("an excerpt of a JSON output: %v %s", err, out)
	}
}
