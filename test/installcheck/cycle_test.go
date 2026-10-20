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

// P24b: the installed cycle harness puts its work directory, which holds
// adcd's state and run directories, only on a filesystem adcd accepts
// (ext2/ext3/ext4, xfs, btrfs, tmpfs), trying $TMPDIR, $XDG_RUNTIME_DIR and
// /dev/shm in that order. With none, it exits 2 naming what it tried,
// creates no directory and never starts adcd; it never falls back to
// another filesystem. A fake stat reports each directory's type (overlayfs
// unless listed in FAKE_TMPFS); a fake adcd records that it was started.
func TestInstalledCycleWorkFilesystem(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "prefix")
	lib := filepath.Join(prefix, "lib/adc")
	marker := filepath.Join(root, "adcd-started")
	for name, content := range map[string]string{
		"bin/adcd":              "#!/bin/sh\ntouch " + marker + "\n",
		"bin/adc":               "#!/bin/sh\nexit 1\n",
		"lib/adc/node/bin/node": "#!/bin/sh\nexit 1\n",
		"lib/adc/fixtures/m1/profiles/go-mini.json": "{}",
		"lib/adc/fixtures/t0/go-mini/repo/go.mod":   "module m\n",
	} {
		write(t, filepath.Join(prefix, name), content, 0o755)
	}
	for _, s := range []string{"installed-cycle.sh", "lib/secrets.sh", "lib/base-remote.sh"} {
		b, err := os.ReadFile("../../scripts/" + s)
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(lib, "scripts", s), string(b), 0o755)
	}
	shims := filepath.Join(root, "shims")
	realStat, err := exec.LookPath("stat")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(shims, "stat"), `#!/bin/bash
if [[ "$1" == -f ]]; then
  p="${@: -1}"
  IFS=: read -ra tmpfs <<<"${FAKE_TMPFS:-}"
  for d in "${tmpfs[@]}"; do [[ -n "$d" && "$p" == "$d"* ]] && { echo tmpfs; exit 0; }; done
  echo overlayfs
  exit 0
fi
exec `+realStat+` "$@"
`, 0o755)
	tmp, runtime := filepath.Join(root, "tmp"), filepath.Join(root, "run")
	for _, d := range []string{tmp, runtime} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	run := func(fakeTmpfs string) (string, int) {
		t.Helper()
		os.Remove(marker)
		cmd := exec.Command(filepath.Join(lib, "scripts/installed-cycle.sh"), prefix, "--routes", "stub", "--project", "go-mini", "--case", "preflight", "--keep")
		cmd.Dir = root
		cmd.Env = append(testguard.HomeEnv(t), "PATH="+shims+":/usr/bin:/bin", "TMPDIR="+tmp, "XDG_RUNTIME_DIR="+runtime, "FAKE_TMPFS="+fakeTmpfs)
		out, err := cmd.CombinedOutput()
		code := 0
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), code
	}
	entries := func(d string) []os.DirEntry {
		es, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		return es
	}

	out, code := run("")
	if code != 2 || !strings.Contains(out, "could_not_run: no writable temporary directory on ext2/ext3/ext4, xfs, btrfs or tmpfs") ||
		!strings.Contains(out, tmp+" (overlayfs)") || !strings.Contains(out, runtime+" (overlayfs)") || !strings.Contains(out, "/dev/shm (overlayfs)") {
		t.Errorf("no supported filesystem: exit %d, %s", code, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("no supported filesystem: adcd was started")
	}
	if len(entries(tmp))+len(entries(runtime)) != 0 {
		t.Error("no supported filesystem: a work directory was created")
	}

	// The positive control: $TMPDIR is overlayfs, $XDG_RUNTIME_DIR tmpfs, so
	// the work directory is a private one under $XDG_RUNTIME_DIR, and the
	// driver gets past the selection (to fail on the fake prefix).
	out, code = run(runtime)
	if code == 2 || !strings.Contains(out, "installed-cycle: kept "+runtime+"/") {
		t.Errorf("a tmpfs $XDG_RUNTIME_DIR: exit %d, %s", code, out)
	}
	if len(entries(tmp)) != 0 || len(entries(runtime)) != 1 {
		t.Errorf("a tmpfs $XDG_RUNTIME_DIR: the work directory is not under it")
	} else if fi, err := os.Stat(filepath.Join(runtime, entries(runtime)[0].Name())); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the work directory is not private: %v %v", fi.Mode(), err)
	}
}
