// SPDX-License-Identifier: Apache-2.0

package installcheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

// #226 entry 203 (Astra #441 r1-r3, Opus #443): a producer that dies, or
// is cut short, must never read as success. In the drivers and the libraries they source,
// no loop reads a process substitution (`< <(...)`: its producer's status is
// lost, so a list cut short is a shorter list), and no command ends in
// `|| true` or `|| :` (a status discarded), and no command substitution
// sits in a test, a case word or an echo/printf argument (its status lost).
// Every list a check consumes is
// acquired first by a checked assignment (`x="$(cmd)" || die ...`), and
// grep's 1 is mapped explicitly. What remains is listed here, each use with
// why its outcome decides no check; a use not listed, or a listed one gone,
// fails.
func TestNoDiscardedStatusInTheDrivers(t *testing.T) {
	type use struct{ line, reason string }
	allowed := map[string][]use{
		"installed-cycle.sh": {
			{`kill -9 "$p" 2>/dev/null || true`, "cleanup: stopping the run's own process; one already gone is the goal"},
			{`wait "$p" 2>/dev/null || true`, "cleanup: reaping a stopped process; its status decides nothing"},
			{`kill "$stub" 2>/dev/null || true`, "the preflight's stub stopped once its reply was checked"},
			{`wait "$stub" 2>/dev/null || true`, "reaping that stub; its status decides nothing"},
			{`case "$(stat -f -c %T -- "$1" 2>/dev/null)" in`, "fs_known: a type that cannot be read is no known type, never chosen"},
			{`how() { if (($1 > 128)); then echo "signal $(kill -l`, "naming a signal in a message already failing the run"},
		},
		"installed-cycle-set.sh": {
			{`tail -n 15 "$cwd/$c.out" >&2 || true`, "a failed case's last lines, printed after its row already says FAILED"},
			{`grep -m 3 -E '^fail:|^installed-cycle: [a-z]' "$cwd/$c.out" || true`, "the text of a row already marked FAILED"},
		},
		"lib/secrets.sh": {
			{`unset -f "$v" 2>/dev/null || true; done < <(declare -Fx`, "clean_exec: anything left, env_extra's check after it refuses"},
			{`unset "$v" 2>/dev/null || true; done < <(compgen -e)`, "clean_exec: anything left, env_extra's check after it refuses"},
			{`done < <("$clean_exec_env" -0; printf 'adc-env-status %s\0' "$?")`, "env_extra: the stream ends in env's own status, checked; a producer that dies leaves none"},
		},
		"lib/sandbox-root.sh": {
			{`root_podman "$tmp" unshare rm -rf "$r" 2>/dev/null || rm -rf "$r" 2>/dev/null || true`, "removing a stale runs/: best effort; the next sweep finds what is left"},
			{`root_podman "$tmp" unshare rm -rf "$tmp/storage" "$tmp/r" 2>/dev/null || true`, "removing the sweep's scratch root's storage before the root itself"},
			{`rm -rf "$w" 2>/dev/null || true`, "removing a stale work directory: best effort; the next sweep finds what is left"},
			{`printf '%s\n' "$rec" >>"$1/.pids" 2>/dev/null || true`, "record_pid's write alone: .pids serves only a sweep after this run is killed without its cleanup; a lost line leaves that process to end by itself"},
			{`case "$(uname -m)" in`, "host_arch: an unknown or unread machine is no prepared platform, refused"},
			{`[[ -x "$1" ]] && [[ "$("$1" info --format '{{.Host.Security.Rootless}}' 2>/dev/null)" == true ]]`, "rootless_podman: an info that fails is not rootless, refused"},
			{`p="$(cat)"; [[ -n "$p" && "$(grep -c "" <<<"$p")" -ge "$n" ]] || exit 2;`, "inside the namespace script: a count that fails is no count, exit 2"},
			{`root_podman "$1" rm --all --force --time 0 >/dev/null 2>&1 || true`, "sweeping a root's containers: removal, decides nothing"},
			{`root_podman "$root" system reset --force >/dev/null 2>&1 || true`, "removing a private root: its storage, then the directory"},
			{`root_podman "$root" unshare rm -rf "$root/storage" "$root/r" ${runs:+"$runs"} 2>/dev/null || true`, "removing a private root's files in its namespace"},
			{`[[ -z "$runs" ]] || rm -rf "$runs" 2>/dev/null || true`, "removing a private root's runs/"},
		},
		"lib/cycle-case.sh": {
			{`[[ "$(tail -n 1 "$work/out/events.jsonl" | sed -n`, "a wait's condition: a read that fails is 'not yet', and the wait's deadline fails the case"},
			{`wait "$events" 2>/dev/null || true; }`, "stopping the event reader before a new one; its status decides nothing"},
			{`  wait "$events" 2>/dev/null || true`, "reaping the stopped event reader"},
		},
		"lib/base-remote.sh": {},
		"lib/clean-shell.sh": {
			{`exec 3< <(n=0; for v in "${!ADC_CREDENTIAL_@}"`, "the fd-3 writer, no loop of a check: its stream ends in a counting record clean_shell_credentials checks"},
		},
	}
	// A status discarded (`< <(`, `|| true`, `|| :`), or lost in a command
	// substitution inside a test, a case word or an echo/printf argument (Opus #443).
	discard := regexp.MustCompile(`< <\(|\|\|\s*(true|:)(\s|;|\)|}|$)|\[\[[^]]*\$\([^(]|case "?\$\([^(]|\b(echo|printf)\b[^#]*\$\([^(]`)
	for l, want := range map[string]bool{`x || true`: true, `x || :`: true, `{ x || true; }`: true, `done < <(y)`: true, `x || rc=$?`: false, `x || die "y"`: false, `x <<<"$y"`: false, `[[ "$(f)" == y ]]`: true, `case "$(f)" in`: true, `echo "a $(f)"`: true, `printf '%s' "$((1 + 2))"`: false, `x="$(f)" || die y`: false} {
		if discard.MatchString(l) != want {
			t.Errorf("the row reads %q as discarding a status: %v", l, !want)
		}
	}
	for file, uses := range allowed {
		b, err := os.ReadFile("../../scripts/" + file)
		if err != nil {
			t.Fatal(err)
		}
		seen := make([]int, len(uses))
		for n, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "#") || !discard.MatchString(l) {
				continue
			}
			found := false
			for i, u := range uses {
				if strings.Contains(l, u.line) {
					seen[i]++
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s:%d discards a status, unlisted: %s", file, n+1, strings.TrimSpace(l))
			}
		}
		for i, u := range uses {
			if seen[i] != 1 {
				t.Errorf("%s: the listed use %q (%s) is matched %d times, not once", file, u.line, u.reason, seen[i])
			}
		}
	}
}

// The lists and flags the drivers' checks consume, each acquired by a
// checked assignment before its loop (#226 entry 203; Astra #441 r3 High 2):
// the line, as it stands, is the row, so a check that is dropped fails here.
// Those a unit can reach run too (TestProducersThatFailRefuse).
func TestChecksConsumeCheckedLists(t *testing.T) {
	for file, lines := range map[string][]string{
		"installed-cycle.sh": {
			`  files="$(cd "$overlay" && find . \( -type f -o -type l \))" || die "the case's overlay could not be listed"`,
			`  withheld="$(json "$case_file" 'd.credential_withheld === true')" || die "the case's credential_withheld could not be read"`,
			`  canary_names="$(json "$case_file" '(d.expect.canary_paths ?? []).join("\n")')" || die "the case's canary paths could not be read"`,
			`  done <<<"$canary_names"`,
			`    *) die "the case could not be read for its canary (exit $rc)" ;;`,
			`secrets_after="$(find "$work" -name bootstrap.secret -printf '%p %i\n' | LC_ALL=C sort)" || die "the bootstrap secrets cannot be listed"`,
			`    sums="$(find "$config" -type f -exec sha256sum -- {} + | LC_ALL=C sort)" || return 1`,
			`  work_dev="$(stat -c %d "$work")" || cannot "$work's filesystem cannot be read"`,
			`shopt -s inherit_errexit`,
			`. "$adc_here/lib/clean-shell.sh" || exit 2`,
			`. "$here/lib/secrets.sh" || exit 2`,
			`. "$here/lib/base-remote.sh" || exit 2`,
			`. "$here/lib/sandbox-root.sh" || exit 2`,
		},
		"installed-cycle-set.sh": {
			`shopt -s inherit_errexit`,
			`. "$adc_here/lib/clean-shell.sh" || exit 2`,
			`. "$prefix/lib/adc/scripts/lib/secrets.sh" || exit 2`,
			`. "$prefix/lib/adc/scripts/lib/sandbox-root.sh" || exit 2`,
		},
		"lib/cycle-case.sh": {
			`  hidden="$(cd "$t/hidden" && find . -type f | LC_ALL=C sort)" || die "the task's hidden files could not be listed"`,
			`  second="$(json "$case_file" 'd.expect.second_task_admitted === false')" || die "the case's second_task_admitted could not be read"`,
		},
	} {
		b, err := os.ReadFile("../../scripts/" + file)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range lines {
			if n := strings.Count(string(b), "\n"+l+"\n"); n != 1 {
				t.Errorf("%s: %q appears %d times, not once", file, l, n)
			}
		}
	}
}

// A producer that fails is refused, never read as a shorter list or a clean
// result: the forms of a secret (known, scan), the filter of scan's own file
// (grep's 1 mapped, anything else refused; Astra #441 r3 High 3), and the
// hostile files of a base remote's overlay.
func TestProducersThatFailRefuse(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "tree/leak"), "sk-filter-PROBE-0123456789abcdef\n", 0o600)
	write(t, filepath.Join(dir, "tree/own"), "sk-filter-PROBE-0123456789abcdef\n", 0o600)
	for name, c := range map[string]struct{ script, want string }{
		"known, its forms failing":  {`forms() { printf 'sk-filter-PROBE-0123456789abcdef\n'; return 1; }; known x; echo ran`, "known: the forms of a secret could not be made"},
		"scan, its forms failing":   {`forms() { printf 'sk-filter-PROBE-0123456789abcdef\n'; return 1; }; scan "" x "$D/tree"; echo ran`, "scan: the forms of the secret could not be made"},
		"scan, its filter failing":  {`grep() { if [[ "$1" == -vxF ]]; then return 2; fi; command grep "$@"; }; scan "$D/tree/own" sk-filter-PROBE-0123456789abcdef "$D/tree"; echo ran`, "scan: the filter of OWN failed (exit 2)"},
		"known, its base64 failing": {`base64() { return 1; }; known sk-filter-PROBE-0123456789abcdef; echo ran`, "known: the forms of a secret could not be made"},
		"known, its tr failing":     {`tr() { return 1; }; known sk-filter-PROBE-0123456789abcdef; echo ran`, "known: the forms of a secret could not be made"},
		"scan, its filter clean":    {`rm "$D/tree/leak"; scan "$D/tree/own" sk-filter-PROBE-0123456789abcdef "$D/tree"; echo ran`, "ran"},
	} {
		cmd := exec.Command("bash", "-c", "set -euo pipefail; . ../../scripts/lib/secrets.sh; "+c.script)
		cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin", "D="+dir)
		out, _ := cmd.CombinedOutput()
		if !strings.Contains(string(out), c.want) || (c.want != "ran" && strings.Contains(string(out), "ran")) {
			t.Errorf("%s: %s", name, out)
		}
	}
	// base_remote: a listing of the overlay's hostile files that fails (a
	// find that dies after printing part of its list) fails the base remote,
	// never a remote with some of them left unrenamed.
	repo, task, overlay := filepath.Join(dir, "repo"), filepath.Join(dir, "task"), filepath.Join(dir, "overlay")
	write(t, filepath.Join(repo, "go.mod"), "module m\n", 0o644)
	write(t, filepath.Join(task, "task.json"), "{}", 0o644)
	write(t, filepath.Join(overlay, "a.go.hostile"), "package m\n", 0o644)
	write(t, filepath.Join(overlay, "b.go.hostile"), "package m\n", 0o644)
	bin := filepath.Join(dir, "bin")
	write(t, filepath.Join(bin, "find"), "#!/bin/sh\ncase \"$*\" in *hostile*) [ -e \"$D/find-dies\" ] && { echo ./a.go.hostile; exit 1; } ;; esac\nexec /usr/bin/find \"$@\"\n", 0o755)
	run := func() (string, error) {
		// As the driver calls it: in a checked assignment, where errexit does not reach inside.
		cmd := exec.Command("bash", "-c", `set -euo pipefail; . ../../scripts/lib/base-remote.sh; rm -rf "$D/remote.git"; tree="$(base_remote "$D/repo" "$D/task" "$D/overlay" "$D/remote.git")" || exit 7`)
		cmd.Env = append(testguard.HomeEnv(t), "PATH="+bin+":/usr/bin:/bin", "D="+dir)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run(); err != nil {
		t.Fatalf("the positive control: %v %s", err, out)
	}
	write(t, filepath.Join(dir, "find-dies"), "", 0o600)
	if out, err := run(); err == nil {
		t.Errorf("a listing of the hostile files that fails: %s", out)
	}
}
