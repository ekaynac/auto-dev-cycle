// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

// --sandbox-root is refused at start unless the root is short, apart from
// the state directory, and its runs/ on a filesystem of its own; the
// refusal documents cgroup delegation. The forbidden paths are the state
// directory, the shared socket and the Podman API sockets.
func TestSandboxConfig(t *testing.T) {
	state := stateDir(t)
	root, err := os.MkdirTemp("", "sbx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err := os.Mkdir(filepath.Join(root, "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, sbx := range map[string]string{
		"a root too long for the runroot": "/" + strings.Repeat("r", 60),
		"a root inside the state dir":     filepath.Join(state, "sbx"),
		"runs/ on a shared filesystem":    root,
		"a relative root":                 "sbx",
	} {
		_, err := LoadConfig([]string{"--state-dir", state, "--sandbox-root", sbx})
		if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "Delegate=yes") || strings.Contains(err.Error(), sbx) {
			t.Errorf("%s: %v, want the fixed sandbox refusal", name, err)
		}
	}
	var shm, tmp syscall.Stat_t
	if syscall.Stat("/dev/shm", &shm) != nil || syscall.Stat(root, &tmp) != nil || shm.Dev == tmp.Dev {
		t.Skip("no second filesystem (/dev/shm) for the accepted case")
	}
	own, err := os.MkdirTemp("/dev/shm", "runs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(own) })
	os.Remove(filepath.Join(root, "runs"))
	if err := os.Symlink(own, filepath.Join(root, "runs")); err != nil {
		t.Fatal(err)
	}
	run := t.TempDir()
	cfg, err := LoadConfig([]string{"--state-dir", state, "--sandbox-root", root, "--shared-run-dir", run, "--shared-group", "0"})
	if err != nil || cfg.Sandbox == nil || cfg.Sandbox.Podman != "/usr/bin/podman" {
		t.Fatalf("an accepted sandbox: %+v %v", cfg.Sandbox, err)
	}
	for _, f := range []string{state, filepath.Join(run, sharedSocket), "/run/podman/podman.sock"} {
		if !slices.Contains(cfg.Sandbox.Forbidden, f) {
			t.Errorf("forbidden %q missing from %q", f, cfg.Sandbox.Forbidden)
		}
	}
}

// The probe route is an operator's; with no sandbox configured it is
// could_not_run/precondition_failed; it takes exactly image and mode.
func TestSandboxProbeRoute(t *testing.T) {
	c := runCommanded(t)
	img := "--image=docker.io/library/debian@sha256:" + strings.Repeat("a", 64)
	if code, _, errb := c.adc(c.sup.Reveal(), "dev", "sandbox", "probe", img, "--mode", "gate"); code != entry.ExitRefused || !strings.Contains(errb, "failed/operator_only") {
		t.Fatalf("a supervisor: %d %s", code, errb)
	}
	if code, _, errb := c.adc(c.op.Reveal(), "dev", "sandbox", "probe", img, "--mode", "gate"); code != entry.ExitCouldNotRun || !strings.Contains(errb, "could_not_run/precondition_failed") {
		t.Fatalf("no sandbox: %d %s", code, errb)
	}
	if code, _, _ := c.adc(c.op.Reveal(), "dev", "sandbox", "probe", img, "--mode", "other"); code != entry.ExitUsage {
		t.Fatalf("another mode: %d", code)
	}
	for _, q := range []string{"", "?mode=gate", "?image=x&mode=gate&extra=1", "?image=x&mode=other"} {
		req, _ := http.NewRequest(http.MethodPost, "http://adcd"+api.SandboxProbePath+q, nil)
		req.Header.Set("Authorization", "Bearer "+c.op.Reveal())
		resp, err := unixClient(filepath.Join(c.dir, privateSocket)).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("query %q: %d, want 400", q, resp.StatusCode)
		}
	}
}
