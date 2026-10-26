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
	for name, real := range map[string]string{"grep": "/usr/bin/grep", "env": "/usr/bin/env", "adc": adcScan(t)} {
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
adc_scan="$D/bin/adc"
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
	// Astra #441 r2 High 2: the caller's variables named as secrets.sh's own
	// globals (their export attribute would survive an assignment) neither
	// stop adcd's start nor reach it.
	cmd.Env = append(testguard.HomeEnv(t), "PATH="+bin+":/usr/bin:/bin", "D="+dir, "S="+secret, "clean_exec_env=anything", "secrets=inherited")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(out)), "ok") {
		t.Fatalf("%v %s", err, out)
	}
	logged, _ := os.ReadFile(log)
	if !strings.Contains(string(logged), "adc dev scan --print-forms --secrets -\n") || !strings.Contains(string(logged), "adc dev scan --secrets - --links-inside -- "+filepath.Join(work, "tree")+"\n") {
		t.Errorf("the scan's adc was not seen: %q", logged)
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
		strings.Contains(e, "BASH_FUNC") || strings.Contains(e, "PRIVATE_VALUE") || strings.Contains(e, "clean_exec_env") || strings.Contains(e, "inherited") || len(strings.Split(strings.TrimSpace(e), "\n")) != 3 {
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
// (known): one redaction (secrets.sh redact, die's too: Astra #441 Medium
// 3), applied before the excerpt is cut, so a text across the cut leaves no
// part of itself (Astra #440 H6). A tail of a log shorter than the excerpt
// is the whole log (Astra #441 Low 4).
func TestExcerptsAreRedacted(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "store/accept_hidden_test.go")
	canary := "\tconst q7vCanary = \"ADC-<q7v>-0123456789abcdef\""
	secret := "sk-never-printed-0123456789abcdef"
	out := filepath.Join(dir, "task.out")
	write(t, out, "adc task create: --attach: open "+store+": permission denied\n"+canary+"\n"+secret+"\n", 0o600)
	cut := filepath.Join(dir, "cut.out")
	write(t, cut, strings.Repeat("x", 395)+canary+"\n", 0o600)
	short := filepath.Join(dir, "short.log")
	write(t, short, "adcd: shutdown failed\n", 0o600)
	cmd := exec.Command("bash", "-c", `set -euo pipefail; . ../../scripts/lib/secrets.sh; adc_scan="$7"; known_text "$1"; known_text "$2"; known "$3"
excerpt "$4" 400; echo; excerpt "$5" 400; echo; excerpt "$5" 30 tail; echo; excerpt "$6" 400 tail; echo; (die "the secret: $3") 2>&1 || true`, "x", store, canary, secret, out, cut, short, adcScan(t))
	cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
	got, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, got)
	}
	lines := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	if len(lines) != 5 || strings.Contains(string(got), store) || strings.Contains(string(got), "q7v") || strings.Contains(string(got), "sk-never") ||
		lines[0] != "adc task create: --attach: open [redacted]: permission denied [redacted] [redacted]" || lines[1] != strings.Repeat("x", 395)+"[reda" ||
		!strings.HasSuffix(lines[2], "x[redacted]") || lines[3] != "adcd: shutdown failed" || lines[4] != "adc: the secret: [redacted]" {
		t.Errorf("excerpts: %q", got)
	}
}

// Opus #441 M1: an entry bash can neither unset nor see (a name that is no
// identifier; a malformed exported function) still reaches every child, so
// clean_exec checks the environment a child gets: it refuses, naming it,
// and never execs its command. Astra #441 High 1: each entry is read whole,
// so a name holding a newline (HOME\nPATH, two allowed names once split) is
// refused too, and a child environment that cannot be read refuses. Its own
// allowlist alone passes. No value is ever printed.
func TestCleanExecRefusesWhatBashCannotSee(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]struct {
		env, pre, want string
	}{
		"its allowlist alone":         {"", "", ""},
		"a name no identifier":        {"ADC-SECRET=private-data", "", "the environment holds more than its allowlist: ADC-SECRET"},
		"a name holding a newline":    {"HOME\nPATH=private-data", "", `the environment holds more than its allowlist: $'HOME\nPATH'`},
		"a malformed function":        {"BASH_FUNC_x%%=private-data", "", "the environment holds more than its allowlist: BASH_FUNC_x%%"},
		"an exported readonly":        {"SHELLOPTS=braceexpand", "", "the environment holds more than its allowlist: SHELLOPTS"},
		"a variable bash can see":     {"OTHER=private-data", "", ""},
		"an environment never read":   {"", "/bin/false", "clean_exec: the environment could not be read"},
		"an environment read in part": {"", filepath.Join(dir, "half-env"), "clean_exec: the environment could not be read"},
	} {
		write(t, filepath.Join(dir, "half-env"), "#!/bin/sh\nprintf 'PATH=/usr/bin:/bin\\0'\nexit 1\n", 0o755)
		ran := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		lib := "../../scripts/lib/secrets.sh"
		if c.pre != "" {
			// The probe is readonly: a copy of secrets.sh names the failing one.
			b, err := os.ReadFile(lib)
			if err != nil || strings.Count(string(b), "\nclean_exec_env=/usr/bin/env\n") != 1 {
				t.Fatalf("secrets.sh's probe: %v", err)
			}
			lib = filepath.Join(dir, "secrets.sh")
			write(t, lib, strings.Replace(string(b), "\nclean_exec_env=/usr/bin/env\n", "\nclean_exec_env="+c.pre+"\n", 1), 0o600)
		}
		script := "set -euo pipefail\n. " + lib + "\n(clean_exec 2 PATH=/usr/bin:/bin ADC_CREDENTIAL_X=v /usr/bin/touch \"$1\")\n"
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
		if err == nil || !strings.Contains(string(out), c.want) || statErr == nil || strings.Contains(string(out), "private-data") {
			t.Errorf("%s: exit %v, ran %v, %s", name, err, statErr == nil, out)
		}
	}
	// The probe is readonly: nothing after secrets.sh can name another.
	cmd := exec.Command("bash", "-c", "set -euo pipefail\n. ../../scripts/lib/secrets.sh\nclean_exec_env=/bin/true\necho reassigned\n")
	cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
	if out, err := cmd.CombinedOutput(); err == nil || strings.Contains(string(out), "reassigned") || !strings.Contains(string(out), "readonly variable") {
		t.Errorf("the probe reassigned: %v %s", err, out)
	}
}

// Astra #443 r2 High 3: the canary a case's setup mints reaches no
// process's arguments: every child of every case's --setup-only (each
// program on PATH, and the release's node, logging its arguments) is given
// the canary's file, never the canary. The driver's own re-exec is no child
// (it replaces the driver, its arguments the operator's).
func TestNoCanaryInArgv(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on PATH")
	}
	root := t.TempDir()
	prefix := filepath.Join(root, "prefix")
	lib := filepath.Join(prefix, "lib/adc")
	argv := filepath.Join(root, "argv.log")
	wrap := filepath.Join(root, "wrap")
	logs := func(real string) string {
		return "#!/bin/sh\nprintf '%s %s\\n' \"$0\" \"$*\" >>" + argv + "\nexec " + real + " \"$@\"\n"
	}
	write(t, filepath.Join(prefix, "bin/adcd"), "#!/bin/sh\nexit 1\n", 0o755)
	write(t, filepath.Join(prefix, "bin/adc"), "#!/bin/sh\nexit 1\n", 0o755)
	write(t, filepath.Join(lib, "node/bin/node"), logs(node), 0o755)
	for _, c := range []string{"git", "find", "grep", "sed", "cat", "cp", "mktemp", "stat", "od", "tr", "head", "tail", "sort", "env", "timeout",
		"realpath", "dirname", "basename", "mkdir", "rm", "ln", "chmod", "sha256sum", "cut", "wc", "mv", "touch", "readlink", "curl"} {
		if real, err := exec.LookPath(c); err == nil {
			write(t, filepath.Join(wrap, c), logs(real), 0o755)
		}
	}
	for _, s := range []string{"installed-cycle.sh", "lib/clean-shell.sh", "lib/secrets.sh", "lib/base-remote.sh", "lib/sandbox-root.sh"} {
		b, err := os.ReadFile("../../scripts/" + s)
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(lib, "scripts", s), string(b), 0o755)
	}
	for _, d := range []string{"fixtures/m1/profiles", "fixtures/t0/go-mini", "fixtures/m1/cases"} {
		if err := os.MkdirAll(filepath.Join(lib, filepath.Dir(d)), 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("cp", "-R", "../../"+d, filepath.Join(lib, filepath.Dir(d))+"/").CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", d, err, out)
		}
	}
	cases, err := os.ReadDir("../../fixtures/m1/cases")
	if err != nil {
		t.Fatal(err)
	}
	minted := 0
	for _, c := range cases {
		cmd := exec.Command(filepath.Join(lib, "scripts/installed-cycle.sh"), prefix, "--routes", "stub", "--project", "go-mini",
			"--case", c.Name(), "--expect", "candidate", "--setup-only", "--keep")
		cmd.Dir = root
		cmd.Env = append(testguard.HomeEnv(t), "PATH="+wrap+":/usr/bin:/bin", "TMPDIR="+t.TempDir())
		out, err := cmd.CombinedOutput()
		_, kept, _ := strings.Cut(string(out), "installed-cycle: kept ")
		kept, _, _ = strings.Cut(kept, "\n")
		if err != nil || kept == "" {
			t.Errorf("%s: --setup-only --keep: %v %s", c.Name(), err, out)
			continue
		}
		canary, err := os.ReadFile(filepath.Join(kept, "out/canary"))
		os.RemoveAll(kept)
		if err != nil {
			t.Errorf("%s: no canary: %v", c.Name(), err)
			continue
		}
		minted++
		logged, err := os.ReadFile(argv)
		if err != nil || !strings.Contains(string(logged), "/node ") {
			t.Fatalf("%s: the wrappers logged nothing (%v)", c.Name(), err)
		}
		for _, l := range strings.Split(string(logged), "\n") {
			if !strings.Contains(l, " -i ADC_CLEAN_SHELL=1 ") && strings.Contains(l, strings.TrimSpace(string(canary))) {
				t.Errorf("%s: a child's arguments hold the canary: %q", c.Name(), l)
			}
		}
		if err := os.Remove(argv); err != nil {
			t.Fatal(err)
		}
	}
	if minted == 0 {
		t.Error("no case minted a canary")
	}
}
