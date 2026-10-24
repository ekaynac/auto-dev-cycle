// SPDX-License-Identifier: Apache-2.0

package installcheck

import (
	"cmp"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

// P24b, D1 (#226 entry 188): a case's setup (its base remote and its
// profile) needs no Podman, and --setup-only stops there, exit 0, starting
// no adcd; without rootless Podman the case is refused after its setup,
// exit 2, naming the Podman it looked for, and adcd is never started. The
// prefix is a fake one: the real installed node, the committed go-mini
// fixtures and the candidate case, and an adcd that records it was started.
func TestInstalledCycleRefusesWithoutPodman(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on PATH (make ci's installed-cycle-setup runs the installed one)")
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
	for _, s := range []string{"installed-cycle.sh", "lib/secrets.sh", "lib/base-remote.sh", "lib/sandbox-root.sh"} {
		b, err := os.ReadFile("../../scripts/" + s)
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(lib, "scripts", s), string(b), 0o755)
	}
	for _, d := range []string{"fixtures/m1/profiles", "fixtures/t0/go-mini", "fixtures/m1/cases/candidate"} {
		if err := os.MkdirAll(filepath.Join(lib, filepath.Dir(d)), 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("cp", "-R", "../../"+d, filepath.Join(lib, filepath.Dir(d))+"/").CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", d, err, out)
		}
	}
	tmpdir := ""
	run := func(args ...string) (string, int) {
		t.Helper()
		os.Remove(marker)
		cmd := exec.Command(filepath.Join(lib, "scripts/installed-cycle.sh"), append([]string{prefix, "--routes", "stub", "--project", "go-mini",
			"--case", "candidate", "--expect", "candidate"}, args...)...)
		cmd.Dir = root
		cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin", "TMPDIR="+cmp.Or(tmpdir, t.TempDir()))
		out, err := cmd.CombinedOutput()
		code := 0
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), code
	}
	started := func() bool { _, err := os.Stat(marker); return err == nil }

	out, code := run("--setup-only")
	if code != 0 || !strings.Contains(out, "installed-cycle: candidate: setup checked (--setup-only)") || started() {
		t.Errorf("--setup-only: exit %d, adcd started %v, %s", code, started(), out)
	}
	out, code = run("--podman", "/nonexistent/podman")
	if code != 2 || !strings.Contains(out, "installed-cycle: could_not_run: rootless Podman (/nonexistent/podman) is required") ||
		!strings.Contains(out, "installed-cycle: candidate: the case's profile") || started() {
		t.Errorf("no Podman: exit %d, adcd started %v, %s", code, started(), out)
	}
	// Opus #435 L1: a value flag given last, with no value, is a usage refusal.
	for _, flag := range []string{"--podman", "--sandbox-root", "--expect"} {
		if out, code := run(flag); code != 2 || !strings.Contains(out, "installed-cycle: could_not_run: usage: ") || started() {
			t.Errorf("%s with no value: exit %d, adcd started %v, %s", flag, code, started(), out)
		}
	}

	// A run killed before its cleanup (SIGKILL runs no trap) leaves its work
	// directory, each recording its owner (pid and start time): the next run
	// stops the processes that run recorded (.pids), still themselves, and
	// removes it; a process it did not record, though its command line names
	// that directory, and a recorded pid whose start time is another
	// process's (a reused pid), are never killed (no pattern kill: several
	// runs share a host). A live owner's directory, and a kept one's (no
	// owner), are never touched. The stale owner is a pid above the kernel's
	// pid_max limit (4194304): never a live process.
	tmpdir = t.TempDir()
	startOf := func(pid int) string {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			t.Skip("no /proc")
		}
		return strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+2:]))[19]
	}
	dirs := map[string]string{"stale": "4194305 0\n", "live": strconv.Itoa(os.Getpid()) + " " + startOf(os.Getpid()) + "\n", "kept": ""}
	for name, owner := range dirs {
		write(t, filepath.Join(tmpdir, "adc-cycle."+name, "state/x"), "", 0o600)
		if owner != "" {
			write(t, filepath.Join(tmpdir, "adc-cycle."+name, ".owner"), owner, 0o600)
		}
	}
	spawn := func() (*exec.Cmd, chan error) {
		c := exec.Command("sh", "-c", "sleep 300", filepath.Join(tmpdir, "adc-cycle.stale/state/adcd"))
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- c.Wait() }()
		t.Cleanup(func() { _ = c.Process.Kill() })
		return c, done
	}
	orphan, orphanDone := spawn()
	bystander, bystanderDone := spawn()
	reused, reusedDone := spawn()
	write(t, filepath.Join(tmpdir, "adc-cycle.stale/.pids"), fmt.Sprintf("%d %s\n%d 1\n", orphan.Process.Pid, startOf(orphan.Process.Pid), reused.Process.Pid), 0o600)
	if out, code := run("--setup-only"); code != 0 {
		t.Fatalf("the sweeping run: exit %d, %s", code, out)
	}
	select {
	case <-orphanDone:
	case <-time.After(10 * time.Second):
		t.Error("the stale run's recorded child still runs")
	}
	for name, done := range map[string]chan error{"an unrecorded process naming the directory": bystanderDone, "a recorded pid of another start time": reusedDone} {
		select {
		case <-done:
			t.Errorf("%s was killed", name)
		default:
		}
	}
	_, _ = bystander, reused
	for name := range dirs {
		if _, err := os.Stat(filepath.Join(tmpdir, "adc-cycle."+name)); (err == nil) == (name == "stale") {
			t.Errorf("adc-cycle.%s: present %v after the sweep", name, err == nil)
		}
	}
}

// Opus #435 L2: removing a sandbox root follows its runs/ link only to a
// directory sandbox_root makes (/dev/shm/adcrXXXXXX); a runs/ directory left
// after its root is gone is swept once its owner is gone, never while it
// lives (a root sandbox_root makes records its owner in both). M2: a canary
// path under the root's runs/ is found (in the root's user namespace: a
// stand-in podman whose unshare runs its command).
func TestSandboxRootRemovalStaysInItsRuns(t *testing.T) {
	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skip("no /dev/shm")
	}
	sh := func(script string) string {
		t.Helper()
		cmd := exec.Command("bash", "-c", "set -euo pipefail; PODMAN=/nonexistent/podman; . ../../scripts/lib/sandbox-root.sh; "+script)
		cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v %s", script, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	elsewhere := t.TempDir()
	write(t, filepath.Join(elsewhere, "keep"), "", 0o600)
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(root, "runs")); err != nil {
		t.Fatal(err)
	}
	sh("sandbox_root_remove " + root)
	if _, err := os.Stat(filepath.Join(elsewhere, "keep")); err != nil {
		t.Errorf("a runs/ link to %s was followed: %v", elsewhere, err)
	}
	if _, err := os.Stat(root); err == nil {
		t.Errorf("the root %s is still there", root)
	}

	runs := map[string]string{"stale": "4194305 0", "live": sh("owner_of $PPID")}
	for name, owner := range runs {
		dir := sh("mktemp -d /dev/shm/adcrXXXXXX")
		t.Cleanup(func() { os.RemoveAll(dir) })
		write(t, filepath.Join(dir, ".owner"), owner+"\n", 0o600)
		runs[name] = dir
	}
	_ = sh("sandbox_roots_stale")
	for name, dir := range runs {
		if _, err := os.Stat(dir); (err == nil) == (name == "stale") {
			t.Errorf("the %s runs/ directory %s: present %v after the sweep", name, dir, err == nil)
		}
	}

	fake := filepath.Join(t.TempDir(), "podman")
	write(t, fake, "#!/bin/sh\nwhile [ $# -gt 0 ] && [ \"$1\" != unshare ]; do shift; done\nshift\nexec \"$@\"\n", 0o755)
	// The root's owner (this bash) is gone once it returns: it is compared,
	// then rewritten to the test's own process, so no concurrent sweep takes
	// the root during the rest.
	made, same, _ := strings.Cut(sh("r=$(sandbox_root ''); s=no; cmp -s \"$r/.owner\" \"$r/runs/.owner\" && s=yes; owner_of $PPID | tee \"$r/.owner\" >\"$r/runs/.owner\"; echo \"$r $s\""), " ")
	t.Cleanup(func() { sh("PODMAN=" + fake + "; sandbox_root_remove " + made) })
	if same != "yes" {
		t.Errorf("the runs/ of a root sandbox_root made does not record its root's owner (%s)", same)
	}
	name := "canary-root-" + strconv.Itoa(os.Getpid())
	if out := sh("PODMAN=" + fake + "; root_canary_path " + made + " " + name + " && echo none"); out != "none" {
		t.Errorf("no canary: %q", out)
	}
	write(t, filepath.Join(made, "runs/att_1/x", name), "", 0o600)
	runsDir, _ := filepath.EvalSymlinks(filepath.Join(made, "runs"))
	if out := sh("PODMAN=" + fake + "; rc=0; root_canary_path " + made + " " + name + " || rc=$?; echo rc=$rc"); !strings.Contains(out, filepath.Join(runsDir, "att_1/x", name)) || !strings.HasSuffix(out, "rc=1") {
		t.Errorf("a canary under the root's runs/: %q", out)
	}
}
