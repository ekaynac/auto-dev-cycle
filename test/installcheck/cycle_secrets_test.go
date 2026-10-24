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
work="$D/work" prefix="$D/prefix" run=0 groups_file=/dev/null sandbox=() capture=()
run_vars=(PATH="$D/bin:/usr/bin:/bin" RUN_DIR="$D/work")
cred=(ADC_CREDENTIAL_PROBE="$secret")
export CALLERS_OWN=leak
start_adcd
for i in $(seq 100); do [[ -f "$D/work/out/ready" ]] && break; sleep 0.1; done
found="$(printf '%s\n' "$secret" | /usr/bin/grep -laF -f - /proc/[0-9]*/cmdline 2>/dev/null || true)"
kill "$adcd"; wait "$adcd" 2>/dev/null || true
[[ -z "$found" ]] || { echo "the secret in a cmdline: $found"; exit 1; }
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
	if e := string(environ); !strings.Contains(e, "ADC_CREDENTIAL_PROBE="+secret+"\n") || !strings.Contains(e, "RUN_DIR="+work+"\n") || strings.Contains(e, "CALLERS_OWN") {
		t.Errorf("adcd's environment: %q", e)
	}
}

// Opus #440 H2, L3: live_after finds the canary in a captured request
// body in any form JSON gives it (a tab-and-quote line, JSON.stringify'd),
// requires the body of every reviewer call in the capture store, and
// passes a clean capture. The stand-in repository's main is unchanged.
func TestLiveAfterCapture(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on PATH")
	}
	const canary = "\tconst q7vCanary = \"ADC-q7v-0123456789abcdef\""
	const requirement = "the requirement of issue 170, its first line"
	setup := func(t *testing.T, body string, kept bool) string {
		dir := t.TempDir()
		work := filepath.Join(dir, "work")
		write(t, filepath.Join(work, "out/canary"), canary+"\n", 0o600)
		if out, err := exec.Command(node, "../../scripts/lib/cycle-expect.mjs", "--canary-forms", filepath.Join(work, "out")).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
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
		repo := filepath.Join(dir, "repo.git")
		git := func(args ...string) string {
			cmd := exec.Command("git", args...)
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
	run := func(dir string) (string, error) {
		script := `set -euo pipefail
. ../../scripts/lib/secrets.sh; . ../../scripts/lib/cycle-live.sh
step() { :; }; root_scan() { :; }
json() { "$node" -e 'const d = JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8")); console.log(eval(process.argv[2]) ?? "")' "$1" "$2"; }
node="$1" work="$2/work" issue_dir="$2/issue" repo_url="$2/repo.git" live_secret=()
IFS= read -r live_canary <"$work/out/canary"
main_before="$(git ls-remote -- "$repo_url" refs/heads/main | cut -f1)"
live_after
echo ok`
		cmd := exec.Command("bash", "-c", script, "x", node, dir)
		cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
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
	clean := stringify(requirement + ": review it")
	if out, err := run(setup(t, clean, true)); err != nil || !strings.HasSuffix(strings.TrimSpace(out), "ok") {
		t.Errorf("a clean capture: %v %s", err, out)
	}
	leaked := stringify(requirement + "\n" + canary)
	if strings.Contains(leaked, canary) {
		t.Fatal("the stand-in body holds the canary verbatim; the row needs it JSON-escaped")
	}
	for name, c := range map[string]struct {
		body string
		kept bool
		want string
	}{
		"the canary JSON-escaped in a body": {leaked, true, "the acceptance canary appears in a captured request"},
		"its trimmed core in a body":        {stringify(requirement + " " + strings.TrimSpace(canary)), true, "the acceptance canary appears in a captured request"},
		"a reviewer call's body not kept":   {clean, false, "no reviewer call's body was captured"},
	} {
		if out, err := run(setup(t, c.body, c.kept)); err == nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v %s", name, err, out)
		}
	}
	// A kept capture whose bytes are gone.
	dir := setup(t, clean, true)
	if err := os.RemoveAll(filepath.Join(dir, "work/state/captures/blobs/sha256")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "work/state/captures/other"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "work/state/captures/other/body"), clean, 0o600)
	if out, err := run(dir); err == nil || !strings.Contains(out, "is not in the capture store") {
		t.Errorf("a reviewer call's body gone: %v %s", err, out)
	}
}

// The live route's pinned image reaches its private sandbox root by a pull
// by digest (root_image: Podman's pull keeps its RepoDigests, which a
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
mkdir -p "$work"
acceptance=("internal/cli/accept_hidden_test.go=$3")
live_acceptance
printf '%s\n' "${live_attach[@]}"`
	cmd := exec.Command("bash", "-c", script, "x", node, dir, store, canary)
	cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	copy := filepath.Join(dir, "work/acceptance/1")
	if got := strings.TrimSpace(string(out)); got != "--attach\ninternal/cli/accept_hidden_test.go="+copy || strings.Contains(got, store) {
		t.Errorf("live_attach: %q", got)
	}
	if b, _ := os.ReadFile(copy); string(b) != body {
		t.Errorf("the copy: %q", b)
	}
}
