// SPDX-License-Identifier: Apache-2.0

package installcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// Opus #440 H2, L3: live_after finds the canary in a captured request
// body in any form JSON gives it, decoded (Astra #440 H4: a tab-and-quote
// line, JSON.stringify'd; a write_file argument's JSON string inside the
// body as Go's json.Marshal escapes it, \u003c and \u003e for its angle
// brackets), and in any other artifact of the work directory (H6: adcd's
// log), but not in the intended copies (out/canary, an acceptance copy by
// its digest); it searches the sandbox root too, and fails when that search
// finds the canary or never ran; it requires the body of every reviewer
// call in the capture store, and passes a clean run. The stand-in
// repository's main is unchanged.
func TestLiveAfterCapture(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on PATH")
	}
	scanner := adcScan(t)
	// The scripts by absolute path: the git calls run outside this repository.
	scripts, err := filepath.Abs("../../scripts")
	if err != nil {
		t.Fatal(err)
	}
	const canary = "\tconst q7vCanary = \"ADC-<q7v>-0123456789abcdef\""
	const requirement = "the requirement of issue 170, its first line"
	setup := func(t *testing.T, body string, kept bool) string {
		dir := t.TempDir()
		work := filepath.Join(dir, "work")
		write(t, filepath.Join(work, "out/canary"), canary+"\n", 0o600)
		forms, err := exec.Command(scanner, "dev", "scan", "--print-forms", "--canary", filepath.Join(work, "out/canary")).Output()
		if err != nil {
			t.Fatalf("the canary's forms: %v", err)
		}
		write(t, filepath.Join(work, "out/canary-forms"), string(forms), 0o600)
		accept := "package inventory\n\n" + canary + "\n"
		write(t, filepath.Join(work, "acceptance/1"), accept, 0o600)
		as := sha256.Sum256([]byte(accept))
		write(t, filepath.Join(work, "out/acceptance-digests"), "sha256:"+hex.EncodeToString(as[:])+"\n", 0o600)
		write(t, filepath.Join(work, "out/adcd-1.log"), "adcd: started\n", 0o600)
		sum := sha256.Sum256([]byte(body))
		h := hex.EncodeToString(sum[:])
		write(t, filepath.Join(work, "state/captures/blobs/sha256", h[:2], h[2:4], h), body, 0o600)
		show, _ := json.Marshal(map[string]any{"bench": map[string]any{"leases": []map[string]any{{"id": "lse_r", "role": "reviewer", "calls": []map[string]any{{"id": "cal_r1"}}}}}})
		write(t, filepath.Join(work, "out/show.json"), string(show), 0o600)
		call := "cal_r1"
		if !kept {
			call = "cal_other"
		}
		ev, _ := json.Marshal(map[string]any{"seq": 1, "kind": "blob.capture_kept/v1", "payload": map[string]any{"call": call, "digest": "sha256:" + h}})
		write(t, filepath.Join(work, "out/events.jsonl"), string(ev)+"\n", 0o600)
		task, _ := json.Marshal(map[string]any{"requirement": requirement + "\nmore"})
		write(t, filepath.Join(dir, "issue/task.json"), string(task), 0o600)
		if err := os.MkdirAll(filepath.Join(dir, "root"), 0o700); err != nil {
			t.Fatal(err)
		}
		repo := filepath.Join(dir, "repo.git")
		git := func(args ...string) string {
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
				"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid", "GIT_CONFIG_NOSYSTEM=1")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v %s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		git("init", "-q", "--bare", repo)
		git("--git-dir", repo, "update-ref", "refs/heads/main", git("--git-dir", repo, "commit-tree", "-m", "m", "4b825dc642cb6eb9a060e54bf8d69288fbee4904"))
		return dir
	}
	// The sandbox root's search stands in as ROOT_STATUS says (0 clean, 1 a hit, 3 never ran, 4 its runs/ unresolved), and records it ran.
	run := func(dir string, rootStatus int) (string, error) {
		script := `set -euo pipefail
. "$3/lib/secrets.sh"; . "$3/lib/cycle-live.sh"
step() { :; }; root_scan() { :; }; runs_required() { if [ "$ROOT_STATUS" = 4 ]; then return 3; fi; echo "$D/runs"; }
D="$2"
root_scanned() { echo "$*" >>"$D/root-searched"; if [ "$ROOT_STATUS" = 1 ]; then echo "$D/x"; fi; return "$ROOT_STATUS"; }
json() { "$node" -e 'const d = JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8")); console.log(eval(process.argv[2]) ?? "")' "$1" "$2"; }
node="$1" work="$2/work" root="$2/root" here="$3" issue_dir="$2/issue" repo_url="$2/repo.git" repo_label="the mirror's main" live_secret=() adc_scan="$4"
IFS= read -r live_canary <"$work/out/canary"
main_before="$(git ls-remote -- "$repo_url" refs/heads/main | cut -f1)"
live_after
echo ok`
		cmd := exec.Command("bash", "-c", script, "x", node, dir, scripts, scanner)
		cmd.Dir = dir
		cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin", "ROOT_STATUS="+strconv.Itoa(rootStatus))
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	stringify := func(content string) string {
		b, err := exec.Command(node, "-e", `process.stdout.write(JSON.stringify({messages: [{role: "user", content: process.argv[1]}]}))`, content).Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// The real composition: the adapter's JSON.stringify of a tool call whose argument is itself JSON.stringify'd,
	// carried by Go's json.Marshal as a RawMessage (HTML-escaped: \u003c, \u003e).
	inner, err := exec.Command(node, "-e", `process.stdout.write(JSON.stringify({messages: [{role: "assistant", tool_calls: [{type: "function",
		function: {name: "write_file", arguments: JSON.stringify({path: "x.go", content: process.argv[1]})}}]}]}))`, requirement+"\n"+canary).Output()
	if err != nil {
		t.Fatal(err)
	}
	goBody, err := json.Marshal(map[string]json.RawMessage{"request": inner})
	if err != nil || !strings.Contains(string(goBody), `\u003cq7v\u003e`) {
		t.Fatalf("the Go-escaped body: %v %s", err, goBody)
	}
	goEscaped, err := json.Marshal(canary)
	if err != nil || !strings.Contains(string(goEscaped), `\u003cq7v\u003e`) {
		t.Fatalf("Go's escaping of the canary: %v %s", err, goEscaped)
	}
	clean := stringify(requirement + ": review it")
	dir := setup(t, clean, true)
	if out, err := run(dir, 0); err != nil || !strings.HasSuffix(strings.TrimSpace(out), "ok") {
		t.Errorf("a clean run: %v %s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "root-searched")); !strings.Contains(string(b), "dev scan --canary "+filepath.Join(dir, "work/out/canary")+" --own-digests "+filepath.Join(dir, "work/out/acceptance-digests")+" -- "+filepath.Join(dir, "root")+" "+filepath.Join(dir, "runs")+"\n") {
		t.Errorf("the sandbox root was not searched for the canary: %q", b)
	}
	leaked := stringify(requirement + "\n" + canary)
	if strings.Contains(leaked, canary) {
		t.Fatal("the stand-in body holds the canary verbatim; the row needs it JSON-escaped")
	}
	for name, c := range map[string]struct {
		body string
		kept bool
		edit func(dir string)
		root int
		want string
	}{
		"the canary JSON-escaped in a body":    {leaked, true, nil, 0, `the acceptance canary appears in: "%DIR%/work/state/captures/`},
		"its trimmed core in a body":           {stringify(requirement + " " + strings.TrimSpace(canary)), true, nil, 0, `the acceptance canary appears in: "%DIR%/work/state/captures/`},
		"in a write_file argument, Go-escaped": {requirement + string(goBody), true, nil, 0, `the acceptance canary appears in: "%DIR%/work/state/captures/`},
		// No pattern form is four JSON strings deep: only decoding finds it.
		"four JSON strings deep": {stringify(stringify(stringify(stringify(requirement + "\n" + canary)))), true, nil, 0, `the acceptance canary appears in: "%DIR%/work/state/captures/`},
		// A line that is no JSON document, holding the canary as Go escapes it: only its escaped form finds it.
		"Go-escaped in a log line": {clean, true, func(d string) {
			write(t, filepath.Join(d, "work/out/adcd-1.log"), "adcd: a payload "+strings.TrimSuffix(strings.TrimPrefix(string(goEscaped), `"`), `"`)+"\n", 0o600)
		}, 0, `the acceptance canary appears in: "%DIR%/work/out/adcd-1.log"`},
		"in adcd's log": {clean, true, func(d string) { write(t, filepath.Join(d, "work/out/adcd-1.log"), "adcd: "+canary+"\n", 0o600) }, 0, `the acceptance canary appears in: "%DIR%/work/out/adcd-1.log"`},
		// The work directory's own: a link out of it is a scan error (--links-inside).
		"a link out of the work directory": {clean, true, func(d string) {
			if err := os.Symlink("/etc/hostname", filepath.Join(d, "work/out/host")); err != nil {
				t.Fatal(err)
			}
		}, 0, "the work directory could not be searched for the canary (exit 2)"},
		"in the sandbox root":                 {clean, true, nil, 1, "the acceptance canary appears in the sandbox root: %DIR%/x"},
		"the sandbox root's search never ran": {clean, true, nil, 3, "the sandbox root could not be searched for the canary (exit 3)"},
		"the sandbox root's runs/ unresolved": {clean, true, nil, 4, "the sandbox root's runs/ could not be resolved for the canary search"},
		"a reviewer call's body not kept":     {clean, false, nil, 0, "no reviewer call's body was captured"},
	} {
		dir := setup(t, c.body, c.kept)
		if c.edit != nil {
			c.edit(dir)
		}
		want := strings.ReplaceAll(c.want, "%DIR%", dir)
		if out, err := run(dir, c.root); err == nil || !strings.Contains(out, want) {
			t.Errorf("%s: %v %s", name, err, out)
		}
	}
	// A kept capture whose bytes are gone.
	dir = setup(t, clean, true)
	if err := os.RemoveAll(filepath.Join(dir, "work/state/captures/blobs/sha256")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "work/state/captures/other"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "work/state/captures/other/body"), clean, 0o600)
	if out, err := run(dir, 0); err == nil || !strings.Contains(out, "is not in the capture store") {
		t.Errorf("a reviewer call's body gone: %v %s", err, out)
	}
	// A requirement whose first line is empty: an empty pattern matches every capture, so it is no search (#226 entry 202).
	dir = setup(t, clean, true)
	write(t, filepath.Join(dir, "issue/task.json"), `{"requirement": "\nmore"}`, 0o600)
	if out, err := run(dir, 0); err == nil || !strings.Contains(out, "nothing to find in the capture") {
		t.Errorf("an empty requirement line: %v %s", err, out)
	}
}

// Opus #440 H1: the acceptance reaches adc task create as the work
// directory's copies (made by redirection), never by its store path; each
// copy holds its file's bytes.
func TestLiveAttachesTheCopies(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on PATH")
	}
	dir := t.TempDir()
	const canary = "// ADC-CANARY-attach-0123456789"
	body := "package cli\n\n" + canary + "\n"
	store := filepath.Join(dir, "store/accept_hidden_test.go")
	write(t, store, body, 0o600)
	sum := sha256.Sum256([]byte(body))
	task, _ := json.Marshal(map[string]any{"attachments": []map[string]string{{"name": "internal/cli/accept_hidden_test.go", "digest": "sha256:" + hex.EncodeToString(sum[:])}}})
	write(t, filepath.Join(dir, "issue/task.json"), string(task), 0o600)
	script := `set -euo pipefail
. ../../scripts/lib/secrets.sh; . ../../scripts/lib/cycle-live.sh
cannot() { echo "cannot: $*"; exit 2; }
json() { "$node" -e 'const d = JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8")); console.log(eval(process.argv[2]) ?? "")' "$1" "$2"; }
node="$1" work="$2/work" issue_dir="$2/issue" issue=170 live_canary="$4"
mkdir -p "$work/out"
acceptance=("internal/cli/accept_hidden_test.go=$3")
live_acceptance
printf "%s\n" "${live_pairs[@]}"`
	cmd := exec.Command("bash", "-c", script, "x", node, dir, store, canary)
	cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	copy := filepath.Join(dir, "work/acceptance/1")
	digests, _ := os.ReadFile(filepath.Join(dir, "work/out/acceptance-digests"))
	if got := strings.TrimSpace(string(out)); got != "internal/cli/accept_hidden_test.go="+copy || strings.Contains(got, store) ||
		string(digests) != "sha256:"+hex.EncodeToString(sum[:])+"\n" {
		t.Errorf("live_pairs: %q; acceptance-digests %q", got, digests)
	}
	if b, _ := os.ReadFile(copy); string(b) != body {
		t.Errorf("the copy: %q", b)
	}
}
