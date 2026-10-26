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
	// substitution inside a test (past any bracket expression in it; Opus #443
	// r2 O2), a case word or an echo/printf argument (Opus #443).
	discard := regexp.MustCompile(`< <\(|\|\|\s*(true|:)(\s|;|\)|}|$)|\[\[([^]]|\][^]])*\$\([^(]|case "?\$\([^(]|\b(echo|printf)\b[^#]*\$\([^(]`)
	for l, want := range map[string]bool{`x || true`: true, `x || :`: true, `{ x || true; }`: true, `done < <(y)`: true, `x || rc=$?`: false, `x || die "y"`: false, `x <<<"$y"`: false, `[[ "$(f)" == y ]]`: true, `[[ "$p" =~ ^[0-9]+$ && "$(f)" == y ]] && z`: true, `[[ "$p" =~ ^[0-9]+$ ]] && x="$(f)"`: false, `case "$(f)" in`: true, `echo "a $(f)"`: true, `printf '%s' "$((1 + 2))"`: false, `x="$(f)" || die y`: false} {
		if discard.MatchString(l) != want {
			t.Errorf("the row reads %q as discarding a status: %v", l, !want)
		}
	}
	// Every library the drivers source is in the row: a new one is audited, never left out.
	libs, err := filepath.Glob("../../scripts/lib/*.sh")
	if err != nil || len(libs) == 0 {
		t.Fatalf("the libraries: %v %v", libs, err)
	}
	for _, l := range libs {
		if _, ok := allowed["lib/"+filepath.Base(l)]; !ok {
			t.Errorf("lib/%s is not in the row", filepath.Base(l))
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

// Astra #443 High 2: in the libraries the drivers source (scripts/lib/*.sh),
// every assignment of a command substitution is checked (`x="$(cmd)" ||
// ...`): a library function runs in its caller's condition, where errexit
// never reaches, so an unchecked one hands its check a list cut short. No
// `local x="$(...)"` either: local's own status hides the substitution's.
// The statement runs from its line to the first that closes a substitution
// (`)"` at its end, or before `||`).
func TestLibrariesCheckEveryAssignment(t *testing.T) {
	assign := regexp.MustCompile(`^\s*(local\s+)?[A-Za-z_][A-Za-z0-9_]*\+?="?\$\([^(]`)
	closes := regexp.MustCompile(`\)"?\s*(\|\||$)`)
	checked := regexp.MustCompile(`\)"?\s*\|\|`)
	for l, want := range map[string]bool{`x="$(f)" || die y`: true, `x="$(f)"`: false, `  x="$(f | g)" ||`: true} {
		if checked.MatchString(l) != want || !closes.MatchString(l) {
			t.Errorf("the row reads %q as checked: %v", l, !want)
		}
	}
	files, err := filepath.Glob("../../scripts/lib/*.sh")
	if err != nil || len(files) < 5 {
		t.Fatalf("the libraries: %v %v", files, err)
	}
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		for i, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "#") || !assign.MatchString(l) {
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(l), "local ") {
				t.Errorf("%s:%d: local hides the substitution's status: %s", filepath.Base(file), i+1, strings.TrimSpace(l))
				continue
			}
			j := i
			for j < len(lines)-1 && !closes.MatchString(lines[j]) {
				j++
			}
			if !checked.MatchString(lines[j]) {
				t.Errorf("%s:%d: an unchecked assignment: %s", filepath.Base(file), i+1, strings.TrimSpace(l))
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
			`record_pid "$work" "$stub" || cannot "the stub provider's start time cannot be read (its sweep record)"`,
			`  record_pid "$work" "$adcd" || cannot "adcd's start time cannot be read (its sweep record)"`,
		},
		"installed-cycle-set.sh": {
			`shopt -s inherit_errexit`,
			`. "$adc_here/lib/clean-shell.sh" || exit 2`,
			`. "$prefix/lib/adc/scripts/lib/secrets.sh" || exit 2`,
			`. "$prefix/lib/adc/scripts/lib/sandbox-root.sh" || exit 2`,
		},
		"lib/cycle-case.sh": {
			`  (cd "$t/hidden" && find . -type f -print0) | LC_ALL=C sort -z >"$list" || die "the task's hidden files could not be listed"`,
			`  done <"$list"`,
			`  [[ -z "$f" ]] || die "the task's hidden files: the list ends in a name cut short"`,
			`  second="$(json "$case_file" 'd.expect.second_task_admitted === false')" || die "the case's second_task_admitted could not be read"`,
			`  record_pid "$work" "$events" || cannot "the event reader's start time cannot be read (its sweep record)"`,
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

// Astra #443 r2 High 4: the task's hidden files reach task create by their
// whole names: a regular file named accept_test.go and a newline is
// attached as that name (the CLI then refuses it), never as the
// accept_test.go beside it, a symlink to another test (find lists no
// link).
func TestHiddenFilesKeepTheirNames(t *testing.T) {
	dir := t.TempDir()
	hidden := filepath.Join(dir, "fx/t0/p/tasks/t/hidden/inventory")
	write(t, filepath.Join(dir, "fx/t0/p/tasks/t/task.json"), "{}", 0o644)
	write(t, filepath.Join(hidden, "accept_test.go\n"), "package inventory // the hidden test\n", 0o644)
	write(t, filepath.Join(dir, "other/pass_test.go"), "package inventory // passes\n", 0o644)
	if err := os.Symlink(filepath.Join(dir, "other/pass_test.go"), filepath.Join(hidden, "accept_test.go")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", `set -euo pipefail; . ../../scripts/lib/secrets.sh; . ../../scripts/lib/cycle-case.sh
step() { :; }
adc() { if [[ "$1 $2" == "project create" ]]; then echo '{"project":"prj_x"}'; return 0; fi; printf '%s\0' "$@" >"$D/args"; return 7; }
work="$D/work" fixtures="$D/fx" project=p task=t node=/bin/true
mkdir -p "$work/out"
cycle_case`)
	cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin", "D="+dir)
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "task create failed") {
		t.Fatalf("the stand-in task create: %v %s", err, out)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	want := "\x00--attach\x00inventory/accept_test.go\n=" + filepath.Join(hidden, "accept_test.go\n") + "\x00"
	if !strings.Contains(string(args), want) || strings.Count(string(args), "--attach") != 1 {
		t.Errorf("the attachments: %q", args)
	}
}

// A producer that fails is refused, never read as a shorter list or a clean
// result: the forms of a secret and every search are adc dev scan's (#226
// entry 206), so known dies when it fails or is cut short, and scan and
// root_secret_scan take its 1 as a finding and anything else as a search
// that did not complete; and the hostile files of a base remote's overlay.
// The search itself: a link to OWN, and a copy named OWN's name and a
// newline, are findings (Astra #443 High 1, r2 High 1); an unreadable file
// whose name is the secret ends the search, its name printed nowhere (Astra
// #443 r2 High 2).
func TestProducersThatFailRefuse(t *testing.T) {
	const secret = "sk-filter-PROBE-0123456789abcdef"
	scanner := adcScan(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "tree/leak"), secret+"\n", 0o600)
	write(t, filepath.Join(dir, "tree/own"), secret+"\n", 0o600)
	// The hex of the secret's bytes alone (it is base64url).
	write(t, filepath.Join(dir, "hex/leak"), "b24f9f8a5b5eafe3d138113ed35db7e39ebbf3d69b71d79f\n", 0o600)
	// A scan program that prints part of the forms, then fails; one that fails at once.
	partial := filepath.Join(dir, "bin/partial")
	write(t, partial, "#!/bin/sh\necho sk-filter-PROBE-0123456789abcdef\nexit 1\n", 0o755)
	failing := filepath.Join(dir, "bin/failing")
	write(t, failing, "#!/bin/sh\nexit 3\n", 0o755)
	for name, c := range map[string]struct{ script, want string }{
		"known, its forms cut short": {`adc_scan=` + partial + `; known ` + secret + `; echo ran`, "known: the forms of a secret could not be made"},
		"known, no scan program":     {`adc_scan=""; known ` + secret + `; echo ran`, "the scan's program (adc_scan) is not set to the release's adc"},
		"scan, its program failing":  {`adc_scan=` + failing + `; scan "" ` + secret + ` "$D/tree"; echo ran`, "scan: the search under " + filepath.Join(dir, "tree") + " did not complete (exit 3)"},
		"scan, no scan program":      {`adc_scan=""; scan "" ` + secret + ` "$D/tree"; echo ran`, "the scan's program (adc_scan) is not set to the release's adc"},
		"scan, a copy named OWN and a newline": {`mkdir "$D/nl"; printf '%s\n' "$S" >"$D/nl/token"; printf '%s\n' "$S" >"$D/nl/token"$'\n'
scan "$D/nl/token" "$S" "$D/nl"; echo ran`, `a secret appears in: "` + filepath.Join(dir, "nl/token") + `\n"`},
		"scan, a link to OWN": {`mkdir -p "$D/ln/home" "$D/ln/out"; printf '%s\n' "$S" >"$D/ln/home/token"; ln -s ../home/token "$D/ln/out/copy"
scan "$D/ln/home/token" "$S" "$D/ln"; echo ran`, `a secret appears in: "` + filepath.Join(dir, "ln/out/copy") + `"`},
		"scan, an unreadable file named the secret": {`mkdir "$D/ur"; : >"$D/ur/$S"; chmod 0 "$D/ur/$S"; scan "" "$S" "$D/ur"; echo ran`,
			"scan: the search under " + filepath.Join(dir, "ur") + " did not complete (exit 2)"},
		"scan, the hex form found": {`scan "" ` + secret + ` "$D/hex"; echo ran`, `a secret appears in: "` + filepath.Join(dir, "hex/leak") + `"`},
		"scan, OWN exempt":         {`rm "$D/tree/leak"; scan "$D/tree/own" ` + secret + ` "$D/tree"; echo ran`, "ran"},
	} {
		cmd := exec.Command("bash", "-c", "set -euo pipefail; . ../../scripts/lib/secrets.sh; adc_scan="+scanner+"; "+c.script)
		cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin", "D="+dir, "S="+secret)
		out, _ := cmd.CombinedOutput()
		if !strings.Contains(string(out), c.want) || (c.want != "ran" && strings.Contains(string(out), "ran")) {
			t.Errorf("%s: %s", name, out)
		}
		if strings.Contains(name, "unreadable") && strings.Contains(string(out), secret) {
			t.Errorf("%s: the secret printed: %s", name, out)
		}
	}
	// root_secret_scan, its search standing in run here (podman unshare's
	// part; the lead's ruling on #443: no link followed under a root): a
	// dangling link and an absolute system link (a container layer's) are
	// clean, never followed; a file holding the secret is a finding (1); a
	// scan program that fails is a search that did not complete (2).
	for _, l := range [][2]string{{"/nonexistent/adc-scan-probe", "root/layer/etc/gone"}, {"/usr/bin", "root/layer/bin"}, {"/etc/hostname", "root/layer/etc/hostname"}} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(l[1])), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(l[0], filepath.Join(dir, l[1])); err != nil {
			t.Fatal(err)
		}
	}
	incomplete := filepath.Join(dir, "bin/incomplete")
	write(t, incomplete, "#!/bin/sh\necho 'adc dev scan: could_not_run: the search did not complete' >&2\nexit 2\n", 0o755)
	rootScan := func(prog string) string {
		cmd := exec.Command("bash", "-c", `set -euo pipefail; PODMAN=/nonexistent/podman; . ../../scripts/lib/secrets.sh; . ../../scripts/lib/sandbox-root.sh
adc_scan="$P"; runs_required() { echo "$D/root"; }; root_scanned() { shift 2; "$@"; }
rc=0; root_secret_scan "$D/root" "$S" || rc=$?; echo "root_secret_scan: $rc"`)
		cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin", "D="+dir, "S="+secret, "P="+prog)
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	if out := rootScan(scanner); !strings.Contains(out, "root_secret_scan: 0") {
		t.Errorf("root_secret_scan over a layer's dangling and absolute links: %s", out)
	}
	write(t, filepath.Join(dir, "root/config.json"), `{"env":["ADC_CREDENTIAL_STUB=`+secret+`"]}`, 0o600)
	if out := rootScan(scanner); !strings.Contains(out, "root_secret_scan: 1") || !strings.Contains(out, filepath.Join(dir, "root/config.json")) {
		t.Errorf("root_secret_scan over a config holding the secret: %s", out)
	}
	if out := rootScan(incomplete); !strings.Contains(out, "root_secret_scan: 2") {
		t.Errorf("root_secret_scan with a scan that did not complete: %s", out)
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
