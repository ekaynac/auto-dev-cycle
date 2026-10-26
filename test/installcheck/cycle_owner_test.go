// SPDX-License-Identifier: Apache-2.0

package installcheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

// pidNS: this process's PID namespace, as owner_of records it.
func pidNS(t *testing.T) string {
	t.Helper()
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		t.Skip("no /proc/self/ns/pid")
	}
	return ns
}

// ownerLib: a script's head sourcing the libraries (by absolute path, so a
// sandbox may change directory), with sandbox_roots_stale's globs (the
// host's /tmp/adc?????? and /dev/shm/adcr??????, and its scratch root's
// /tmp/adcXXXXXX) pointed at $P's: a row sweeps only the directories it
// made, never the host's (Opus #443 r2 B1).
func ownerLib(t *testing.T) string {
	t.Helper()
	lib, err := filepath.Abs("../../scripts/lib")
	if err != nil {
		t.Fatal(err)
	}
	return `set -euo pipefail; PODMAN=/nonexistent/podman; . ` + lib + `/secrets.sh; . ` + lib + `/sandbox-root.sh
f="$(declare -f sandbox_roots_stale)"; g="${f//\/tmp\/adc/$P/adc}"; g="${g//\/dev\/shm\/adcr/$P/adcr}"
[[ "$g" != "$f" && "$g" != *"/tmp/adc"* && "$g" != *"/dev/shm/"* ]] || { echo "the sweep's globs are not the row's own"; exit 1; }
eval "$g"
`
}

// Opus #443 H1a: a leftover is stale only on evidence: a well-formed owner
// record (pid, start time and PID namespace) of this namespace whose pid is
// gone, or whose start time was read and differs. owner_of fails, printing
// nothing, unless it read a numeric start time and a namespace; a record
// that is malformed (one with no namespace too), of another namespace (r2
// B1), or a start time or namespace that cannot be read (a cut that dies, a
// /proc/PID/stat that cannot be read, a readlink that dies), keeps the
// directory. A live owner's work directory, sandbox root and runs/ survive
// both sweeps; a gone owner's are removed.
func TestStaleOnlyOnEvidence(t *testing.T) {
	ns := pidNS(t)
	live := exec.Command("sleep", "60")
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	liveEnded := make(chan error, 1)
	go func() { liveEnded <- live.Wait() }()
	t.Cleanup(func() { _ = live.Process.Kill(); <-liveEnded })
	parent := t.TempDir()
	// Named as sandbox_root names them (adc + 6, adcr + 6), under the row's own parent.
	dirs := map[string]string{"root": "adcROOT01", "runs": "adcrRUNS01", "gone-root": "adcGONE01"}
	for _, d := range []string{"live", "gone", "other-ns", "malformed-1", "malformed-2", "malformed-3", "malformed-4", "malformed-5", "malformed-6"} {
		dirs[d] = "adc-cycle." + d
	}
	all := []string{"live", "root", "runs", "other-ns", "malformed-1", "malformed-2", "malformed-3", "malformed-4", "malformed-5", "malformed-6"}
	gone := []string{"gone", "gone-root"}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(parent, dirs["live"], ".owner"), 0o600) })
	// pids: the gone run's .pids records the live owner, as record_pid would:
	// a start time that cannot be read now is not that process, never killed.
	for name, c := range map[string]struct {
		standIn string
		pids    bool
		kept    []string
		removed []string
	}{
		"no stand-in":     {"", false, all, gone},
		"a cut that dies": {`cut() { return 1; }`, true, []string{"live", "root", "runs"}, gone},
		"a stat not read": {`cat() { return 1; }`, true, []string{"live", "root", "runs"}, gone},
		// A cut that dies after part of its output, or prints no number: no start time read.
		"a cut cut short":             {`cut() { command cut "$@" | head -c 2; return 1; }`, true, []string{"live", "root", "runs"}, gone},
		"a cut that prints no number": {`cut() { echo x; }`, true, []string{"live", "root", "runs"}, gone},
		// No namespace read, the sweep's own or the owner's: nothing is evidence.
		"a readlink that dies":   {`readlink() { return 1; }`, true, append(append([]string{}, all...), gone...), nil},
		"an owner file not read": {`chmod 0 "$P/adc-cycle.live/.owner"`, false, []string{"live"}, gone},
		"owner_of, cut dead": {`cut() { return 1; }; if out="$(owner_of $$)"; then echo "owner_of: $out"; exit 1; fi; [ -z "$out" ] || exit 1
if record_pid "$P/adc-cycle.live" "$LIVE"; then echo "record_pid recorded no start time"; exit 1; fi`, false, nil, nil},
		"owner_of, readlink dead":                {`readlink() { return 1; }; if out="$(owner_of $$)"; then echo "owner_of: $out"; exit 1; fi; [ -z "$out" ] || exit 1`, false, nil, nil},
		"owner_of, readlink prints no namespace": {`readlink() { echo x; }; if out="$(owner_of $$)"; then echo "owner_of: $out"; exit 1; fi; [ -z "$out" ] || exit 1`, false, nil, nil},
		"record_pid, the pid gone":               {`record_pid "$P/adc-cycle.gone" 4194399; [ ! -e "$P/adc-cycle.gone/.pids" ] || exit 1; work_dirs_stale /nonexistent`, false, nil, nil},
	} {
		_ = os.Chmod(filepath.Join(parent, dirs["live"], ".owner"), 0o600)
		_ = os.Remove(filepath.Join(parent, dirs["gone"], ".pids"))
		for _, d := range dirs {
			if err := os.MkdirAll(filepath.Join(parent, d), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		for d, owner := range map[string]string{
			"gone": "4194399 1 " + ns, "gone-root": "4194399 1 " + ns, "other-ns": "4194399 1 pid:[1]",
			"malformed-1": strconv.Itoa(live.Process.Pid) + " ", "malformed-2": "4194399", "malformed-3": "4194399 1 2", "malformed-4": "4194399 1",
			// This namespace's, but with no start time, or with more after it.
			"malformed-5": "4194399 " + ns, "malformed-6": "4194399 1 " + ns + " x",
		} {
			write(t, filepath.Join(parent, dirs[d], ".owner"), owner+"\n", 0o600)
		}
		pids := ""
		if c.pids {
			pids = `owner_of "$LIVE" >"$P/adc-cycle.gone/.pids"` + "\n"
		}
		script := ownerLib(t) + `rm -f "$P/adc-cycle.live/.owner"; owner_of "$LIVE" >"$P/adc-cycle.live/.owner"
owner_of "$LIVE" >"$P/adcROOT01/.owner"; owner_of "$LIVE" >"$P/adcrRUNS01/.owner"
` + pids + c.standIn + `
work_dirs_stale "$P"
sandbox_roots_stale
echo swept`
		cmd := exec.Command("bash", "-c", script)
		cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin", "LIVE="+strconv.Itoa(live.Process.Pid), "P="+parent)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.HasSuffix(strings.TrimSpace(string(out)), "swept") {
			t.Errorf("%s: %v %s", name, err, out)
			continue
		}
		for _, d := range c.kept {
			if _, err := os.Stat(filepath.Join(parent, dirs[d])); err != nil {
				t.Errorf("%s: %s was removed", name, d)
			}
		}
		for _, d := range c.removed {
			if _, err := os.Stat(filepath.Join(parent, dirs[d])); err == nil {
				t.Errorf("%s: %s was kept", name, d)
			}
		}
		if c.pids {
			select {
			case err := <-liveEnded:
				liveEnded <- err
				t.Fatalf("%s: the live owner, recorded in a gone run's .pids, was killed: %v", name, err)
			case <-time.After(300 * time.Millisecond):
			}
		}
	}
}

// Opus #443 r2 B1: a pid is evidence only in its own PID namespace. A sweep
// from another namespace sharing the directories, in either direction,
// keeps a live run's work directory, sandbox root and runs/; its own
// namespace's gone record it still removes. Skipped only where bwrap cannot
// make a PID namespace.
func TestStaleAcrossPIDNamespaces(t *testing.T) {
	bwrap := []string{"bwrap", "--bind", "/", "/", "--unshare-pid", "--die-with-parent", "--proc", "/proc", "--dev", "/dev"}
	if out, err := exec.Command(bwrap[0], append(bwrap[1:], "true")...).CombinedOutput(); err != nil {
		t.Skipf("bwrap cannot make a PID namespace here: %v %s", err, out)
	}
	ns := pidNS(t)
	lib := ownerLib(t)
	dirs := []string{"adc-cycle.live", "adcROOT01", "adcrRUNS01", "adc-cycle.gone"}
	made := func() string {
		p := t.TempDir()
		for _, d := range dirs {
			if err := os.MkdirAll(filepath.Join(p, d), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	check := func(direction, p string, out []byte, err error) {
		t.Helper()
		if err != nil || !strings.HasSuffix(strings.TrimSpace(string(out)), "swept") {
			t.Fatalf("%s: %v %s", direction, err, out)
		}
		for _, d := range dirs[:3] {
			if _, err := os.Stat(filepath.Join(p, d)); err != nil {
				t.Errorf("%s: the live run's %s was removed", direction, d)
			}
		}
		if _, err := os.Stat(filepath.Join(p, "adc-cycle.gone")); err == nil {
			t.Errorf("%s: the sweep's own namespace's gone record was kept", direction)
		}
	}
	records := `for d in adc-cycle.live adcROOT01 adcrRUNS01; do owner_of "$LIVE" >"$P/$d/.owner"; done
`
	sweep := `printf '4194399 1 %s\n' "$(readlink /proc/self/ns/pid)" >"$P/adc-cycle.gone/.owner"
work_dirs_stale "$P"
sandbox_roots_stale
echo swept`

	// The sweeper inside, the owner on the host.
	live := exec.Command("sleep", "60")
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Process.Kill(); _ = live.Wait() })
	p := made()
	env := append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin", "LIVE="+strconv.Itoa(live.Process.Pid), "P="+p)
	cmd := exec.Command("bash", "-c", lib+records)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the host's records: %v %s", err, out)
	}
	if b, err := os.ReadFile(filepath.Join(p, "adc-cycle.live/.owner")); err != nil || !strings.HasSuffix(strings.TrimSpace(string(b)), " "+ns) {
		t.Fatalf("the host's record names no namespace %s: %q %v", ns, b, err)
	}
	cmd = exec.Command(bwrap[0], append(bwrap[1:], "bash", "-c", lib+sweep)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	check("a sweep inside, the owner on the host", p, out, err)
	if err := live.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("the host's owner is gone: %v", err)
	}

	// The owner inside, the sweeper on the host.
	p = made()
	ready := filepath.Join(p, "ready")
	inside := exec.Command(bwrap[0], append(bwrap[1:], "bash", "-c", lib+`LIVE=$$
`+records+`: >"$P/ready"; exec sleep 60`)...)
	inside.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin", "P="+p)
	if err := inside.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { ended <- inside.Wait() }()
	t.Cleanup(func() { _ = inside.Process.Kill(); <-ended })
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case err := <-ended:
			ended <- err
			t.Fatalf("the run inside ended before its records: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the run inside wrote no records")
		}
	}
	cmd = exec.Command("bash", "-c", lib+sweep)
	cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin", "P="+p)
	out, err = cmd.CombinedOutput()
	check("a sweep on the host, the owner inside", p, out, err)
	select {
	case err := <-ended:
		ended <- err
		t.Errorf("the run inside is gone: %v", err)
	default:
	}
}

// Opus #443 L3: the set refuses a run of no case, which would otherwise pass
// having run none.
func TestInstalledCycleSetRefusesNoCase(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "prefix")
	write(t, filepath.Join(prefix, "lib/adc/scripts/installed-cycle.sh"), "#!/bin/sh\nexit 0\n", 0o755)
	cmd := exec.Command("../../scripts/installed-cycle-set.sh", "run", prefix)
	cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin", "TMPDIR="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "installed-cycle-set: run: no case given") {
		t.Errorf("a run of no case: %v %s", err, out)
	}
}
