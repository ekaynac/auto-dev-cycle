// SPDX-License-Identifier: Apache-2.0

package installcheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

// Opus #443 H1a: a leftover is stale only on evidence: a well-formed owner
// record (pid and start time) whose pid is gone, or whose start time was
// read and differs. owner_of fails, printing nothing, unless it read a
// numeric start time; a record that is malformed, or a start time that
// cannot be read (a cut that dies, a /proc/PID/stat that cannot be read),
// keeps the directory. A live owner's work directory and sandbox root
// survive both sweeps; a gone owner's are removed.
func TestStaleOnlyOnEvidence(t *testing.T) {
	live := exec.Command("sleep", "60")
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Process.Kill(); _ = live.Wait() })
	parent := t.TempDir()
	// A sandbox root as sandbox_root names one (/tmp/adc??????, what sandbox_roots_stale sweeps).
	made, err := exec.Command("mktemp", "-d", "/tmp/adcXXXXXX").Output()
	if err != nil {
		t.Fatal(err)
	}
	root := strings.TrimSpace(string(made))
	t.Cleanup(func() { os.RemoveAll(root) })
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(parent, "adc-cycle.live/.owner"), 0o600) })
	for name, c := range map[string]struct {
		standIn string
		kept    []string
		removed []string
	}{
		"no stand-in":     {"", []string{"live", "malformed-1", "malformed-2", "malformed-3", "root"}, []string{"gone"}},
		"a cut that dies": {`cut() { return 1; }`, []string{"live", "root"}, []string{"gone"}},
		"a stat not read": {`cat() { return 1; }`, []string{"live", "root"}, []string{"gone"}},
		// A cut that dies after part of its output, or prints no number: no start time read.
		"a cut cut short":             {`cut() { command cut "$@" | head -c 2; return 1; }`, []string{"live", "root"}, []string{"gone"}},
		"a cut that prints no number": {`cut() { echo x; }`, []string{"live", "root"}, []string{"gone"}},
		"an owner file not read":      {`chmod 0 "$P/adc-cycle.live/.owner"`, []string{"live"}, []string{"gone"}},
		"owner_of, cut dead": {`cut() { return 1; }; if out="$(owner_of $$)"; then echo "owner_of: $out"; exit 1; fi; [ -z "$out" ] || exit 1
if record_pid "$P/adc-cycle.live" "$LIVE"; then echo "record_pid recorded no start time"; exit 1; fi`, nil, nil},
		"record_pid, the pid gone": {`record_pid "$P/adc-cycle.gone" 4194399; [ ! -e "$P/adc-cycle.gone/.pids" ] || exit 1; work_dirs_stale /nonexistent`, nil, nil},
	} {
		_ = os.Chmod(filepath.Join(parent, "adc-cycle.live/.owner"), 0o600)
		for _, d := range []string{"live", "gone", "malformed-1", "malformed-2", "malformed-3"} {
			if err := os.MkdirAll(filepath.Join(parent, "adc-cycle."+d), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		write(t, filepath.Join(parent, "adc-cycle.gone/.owner"), "4194399 1\n", 0o600)
		write(t, filepath.Join(parent, "adc-cycle.malformed-1/.owner"), strconv.Itoa(live.Process.Pid)+" \n", 0o600)
		write(t, filepath.Join(parent, "adc-cycle.malformed-2/.owner"), "4194399\n", 0o600)
		write(t, filepath.Join(parent, "adc-cycle.malformed-3/.owner"), "4194399 1 2\n", 0o600)
		script := `set -euo pipefail; PODMAN=/nonexistent/podman; . ../../scripts/lib/secrets.sh; . ../../scripts/lib/sandbox-root.sh
rm -f "$P/adc-cycle.live/.owner"; owner_of "$LIVE" >"$P/adc-cycle.live/.owner"
owner_of "$LIVE" >"$R/.owner"
` + c.standIn + `
work_dirs_stale "$P"
sandbox_roots_stale
echo swept`
		cmd := exec.Command("bash", "-c", script)
		cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin", "LIVE="+strconv.Itoa(live.Process.Pid), "P="+parent, "R="+root)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.HasSuffix(strings.TrimSpace(string(out)), "swept") {
			t.Errorf("%s: %v %s", name, err, out)
			continue
		}
		for _, d := range c.kept {
			p := filepath.Join(parent, "adc-cycle."+d)
			if d == "root" {
				p = root
			}
			if _, err := os.Stat(p); err != nil {
				t.Errorf("%s: %s was removed", name, d)
			}
		}
		for _, d := range c.removed {
			if _, err := os.Stat(filepath.Join(parent, "adc-cycle."+d)); err == nil {
				t.Errorf("%s: %s was kept", name, d)
			}
		}
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
