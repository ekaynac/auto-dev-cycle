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
	run := func(args ...string) (string, int) {
		t.Helper()
		os.Remove(marker)
		cmd := exec.Command(filepath.Join(lib, "scripts/installed-cycle.sh"), append([]string{prefix, "--routes", "stub", "--project", "go-mini",
			"--case", "candidate", "--expect", "candidate"}, args...)...)
		cmd.Dir = root
		cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin", "TMPDIR="+t.TempDir())
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
}
