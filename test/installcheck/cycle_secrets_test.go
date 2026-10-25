// SPDX-License-Identifier: Apache-2.0

package installcheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

// Opus #440 H1: no credential is ever a process argument (/proc/PID/cmdline
// is readable by every local user). The shared scan gives grep the forms on
// its stdin, and the driver's start_adcd (taken from installed-cycle.sh as
// it is) puts the credential in adcd's environment by builtins, never
// through env(1): a grep and an env on PATH that log their arguments see
// none of the secret's forms, and no process's cmdline holds it while the
// stand-in adcd runs; the stand-in's environment holds the credential and
// nothing else of the caller's.
func TestNoSecretInArgv(t *testing.T) {
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("no /proc")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	log := filepath.Join(dir, "argv.log")
	for name, real := range map[string]string{"grep": "/usr/bin/grep", "env": "/usr/bin/env"} {
		write(t, filepath.Join(bin, name), "#!/bin/sh\nprintf '%s %s\\n' "+name+" \"$*\" >>"+log+"\nexec "+real+" \"$@\"\n", 0o755)
	}
	work := filepath.Join(dir, "work")
	write(t, filepath.Join(work, "tree/clean"), "nothing here\n", 0o600)
	write(t, filepath.Join(dir, "prefix/bin/adcd"), "#!/bin/sh\ntr '\\0' '\\n' </proc/self/environ >"+work+"/out/environ\necho ready >"+work+"/out/ready\nexec sleep 30\n", 0o755)
	if err := os.MkdirAll(filepath.Join(work, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("../../scripts/installed-cycle.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "\nstart_adcd() {\n")
	j := strings.Index(s[i:], "\n}\n")
	if i < 0 || j < 0 {
		t.Fatal("installed-cycle.sh has no start_adcd")
	}
	write(t, filepath.Join(dir, "start_adcd.sh"), s[i:i+j+3], 0o600)
	const secret = "sk-live-PROBE-0123456789abcdef0123456789abcdef"
	script := `set -euo pipefail
. ../../scripts/lib/secrets.sh; . ../../scripts/lib/sandbox-root.sh; . "$D/start_adcd.sh"
secret="$S"
# The detector's positive control: a process with the secret in its argv is found.
(exec -a "$secret" sleep 30) </dev/null >/dev/null 2>&1 & probe=$!
sleep 0.2
found="$(printf '%s\n' "$secret" | /usr/bin/grep -laF -f - /proc/[0-9]*/cmdline 2>/dev/null || true)"
kill "$probe"; wait "$probe" 2>/dev/null || true
[[ "$found" == *"/proc/$probe/cmdline"* ]] || { echo "the detector found nothing: $found"; exit 1; }
known "$secret"
scan "" "$secret" "$D/work/tree"
work="$D/work" prefix="$D/prefix" run=0 groups_file=/dev/null fixtures="$D" sandbox=() capture=()
run_vars=(PATH="$D/bin:/usr/bin:/bin" RUN_DIR="$D/work")
cred=(ADC_CREDENTIAL_PROBE="$secret")
export CALLERS_OWN=leak
caller_helper() { : PRIVATE_VALUE; }; export -f caller_helper
start_adcd
for i in $(seq 100); do [[ -f "$D/work/out/ready" ]] && break; sleep 0.1; done
found="$(printf '%s\n' "$secret" | /usr/bin/grep -laF -f - /proc/[0-9]*/cmdline 2>/dev/null || true)"
kill "$adcd"; wait "$adcd" 2>/dev/null || true
[[ -z "$found" ]] || { echo "the secret in a cmdline: $found"; exit 1; }
# Astra #440 H2: a readonly exported variable cannot be unset: the start refuses, adcd never runs.
rm -f "$D/work/out/ready"
declare -rx CALLERS_RO=1
rc=0; (start_adcd; wait "$adcd") || rc=$?
[[ $rc != 0 && ! -f "$D/work/out/ready" ]] && grep -q "holds more than its allowlist: CALLERS_RO" "$D/work/out/adcd-2.log" || { echo "a readonly variable: rc=$rc $(cat "$D/work/out/adcd-2.log")"; exit 1; }
echo ok`
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(testguard.HomeEnv(t), "PATH="+bin+":/usr/bin:/bin", "D="+dir, "S="+secret)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(out)), "ok") {
		t.Fatalf("%v %s", err, out)
	}
	logged, _ := os.ReadFile(log)
	if !strings.Contains(string(logged), "grep -rlaF -f - --") {
		t.Errorf("the scan's grep was not seen: %q", logged)
	}
	for _, f := range []string{secret, secret[:16], secret[len(secret)-16:]} {
		if strings.Contains(string(logged), f) {
			t.Errorf("a form of the secret in a process's arguments: %q", logged)
		}
	}
	environ, err := os.ReadFile(filepath.Join(work, "out/environ"))
	if err != nil {
		t.Fatal(err)
	}
	if e := string(environ); !strings.Contains(e, "ADC_CREDENTIAL_PROBE="+secret+"\n") || !strings.Contains(e, "RUN_DIR="+work+"\n") || strings.Contains(e, "CALLERS_OWN") ||
		strings.Contains(e, "BASH_FUNC") || strings.Contains(e, "PRIVATE_VALUE") || len(strings.Split(strings.TrimSpace(e), "\n")) != 3 {
		t.Errorf("adcd's environment: %q", e)
	}
}

// A case's pinned image reaches its sandbox root by a pull by digest (root_image: Podman's pull keeps its RepoDigests, which a
// loaded archive lacks), its platform checked; never a load.
func TestRootImagePullsByDigest(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "podman.log")
	fake := filepath.Join(dir, "podman")
	write(t, fake, `#!/bin/sh
printf '%s\n' "$*" >>`+log+`
while [ $# -gt 0 ] && [ "${1#--}" != "$1" ]; do case "$1" in --root|--runroot) shift 2 ;; *) shift ;; esac; done
case "$1 $2" in
  "image exists") exit 1 ;;
  "image inspect") echo "linux/arm64 sha256:0123" ;;
esac
exit 0
`, 0o755)
	const pin = "ghcr.io/ekaynac/adc-prepared-go@sha256:4e8cf51b55c9115963f1b9485f6261e66dbbd5aa631b0e5142d13eec04f320c7"
	root := filepath.Join(dir, "root")
	cmd := exec.Command("bash", "-c", `set -euo pipefail; PODMAN="$1"; . ../../scripts/lib/sandbox-root.sh; mkdir -p "$2"; root_image "$2" "$3" arm64`, "x", fake, root, pin)
	cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "sha256:0123" {
		t.Fatalf("%v %s", err, out)
	}
	logged, _ := os.ReadFile(log)
	if l := string(logged); !strings.Contains(l, "--root "+root+"/storage") || !strings.Contains(l, "pull --quiet --arch arm64 "+pin+"\n") || strings.Contains(l, " load") {
		t.Errorf("the image's way into the root: %q", l)
	}
}

// An excerpt of a run's output (a failed task create, a timed-out wait)
// names no known text (known_text: a path, a canary) and no credential
// (known): one redaction (secrets.sh excerpt), applied before the excerpt
// is cut, so a text across the cut leaves no part of itself (Astra #440 H6).
func TestExcerptsAreRedacted(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "store/accept_hidden_test.go")
	canary := "\tconst q7vCanary = \"ADC-<q7v>-0123456789abcdef\""
	secret := "sk-never-printed-0123456789abcdef"
	out := filepath.Join(dir, "task.out")
	write(t, out, "adc task create: --attach: open "+store+": permission denied\n"+canary+"\n"+secret+"\n", 0o600)
	cut := filepath.Join(dir, "cut.out")
	write(t, cut, strings.Repeat("x", 395)+canary+"\n", 0o600)
	cmd := exec.Command("bash", "-c", `set -euo pipefail; . ../../scripts/lib/secrets.sh; known_text "$1"; known_text "$2"; known "$3"
excerpt "$4" 400; echo; excerpt "$5" 400; echo; excerpt "$5" 30 tail`, "x", store, canary, secret, out, cut)
	cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
	got, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, got)
	}
	lines := strings.Split(string(got), "\n")
	if len(lines) != 3 || strings.Contains(string(got), store) || strings.Contains(string(got), "q7v") || strings.Contains(string(got), "sk-never") ||
		lines[0] != "adc task create: --attach: open [redacted]: permission denied [redacted] [redacted]" || lines[1] != strings.Repeat("x", 395)+"[reda" ||
		!strings.HasSuffix(lines[2], "x[redacted]") {
		t.Errorf("excerpts: %q", got)
	}
}

// Opus #441 M1: an entry bash can neither unset nor see (a name that is no
// identifier; a malformed exported function) still reaches every child, so
// clean_exec checks the environment a child gets: it refuses, naming it,
// and never execs its command. Its own allowlist alone passes.
func TestCleanExecRefusesWhatBashCannotSee(t *testing.T) {
	dir := t.TempDir()
	script := `set -euo pipefail
. ../../scripts/lib/secrets.sh
(clean_exec 2 PATH=/usr/bin:/bin ADC_CREDENTIAL_X=v /usr/bin/touch "$1")
`
	for name, c := range map[string]struct {
		env  string
		want string
	}{
		"its allowlist alone":     {"", ""},
		"a name no identifier":    {"ADC-SECRET=a-secret", "the environment holds more than its allowlist: ADC-SECRET "},
		"a malformed function":    {"BASH_FUNC_x%%=junk", "the environment holds more than its allowlist: BASH_FUNC_x%% "},
		"an exported readonly":    {"SHELLOPTS=braceexpand", "the environment holds more than its allowlist: SHELLOPTS "},
		"a variable bash can see": {"OTHER=x", ""},
	} {
		ran := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		cmd := exec.Command("bash", "-c", script, "bash", ran)
		cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
		if c.env != "" {
			cmd.Env = append(cmd.Env, c.env)
		}
		out, err := cmd.CombinedOutput()
		_, statErr := os.Stat(ran)
		if c.want == "" {
			if err != nil || statErr != nil {
				t.Errorf("%s: %v %v %s", name, err, statErr, out)
			}
			continue
		}
		if err == nil || !strings.Contains(string(out), c.want) || statErr == nil || strings.Contains(string(out), "a-secret") {
			t.Errorf("%s: exit %v, ran %v, %s", name, err, statErr == nil, out)
		}
	}
}
