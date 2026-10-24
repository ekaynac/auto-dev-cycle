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
// (ext2/ext3/ext4, xfs, btrfs, tmpfs), trying $TMPDIR, the runtime
// directory and /dev/shm in that order (the child here has no runtime
// directory: testguard.HomeEnv sets none). With none accepted, it exits 2
// naming what it tried, creates no directory and never starts adcd; it
// never falls back to another filesystem. A fake stat reports each
// directory's type (overlayfs unless under one listed in FAKE_TMPFS); a
// fake adcd records that it was started.
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
	for _, s := range []string{"installed-cycle.sh", "lib/secrets.sh", "lib/base-remote.sh", "lib/sandbox-root.sh"} {
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
	tmp := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(fakeTmpfs string) (string, int) {
		t.Helper()
		os.Remove(marker)
		cmd := exec.Command(filepath.Join(lib, "scripts/installed-cycle.sh"), prefix, "--routes", "stub", "--project", "go-mini", "--case", "preflight", "--keep")
		cmd.Dir = root
		cmd.Env = append(testguard.HomeEnv(t), "PATH="+shims+":/usr/bin:/bin", "TMPDIR="+tmp, "FAKE_TMPFS="+fakeTmpfs)
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
	// kept is the work directory the driver kept (--keep), removed at the
	// test's end.
	kept := func(out string) string {
		_, after, ok := strings.Cut(out, "installed-cycle: kept ")
		if !ok {
			return ""
		}
		dir, _, _ := strings.Cut(after, "\n")
		t.Cleanup(func() { os.RemoveAll(dir) })
		return dir
	}

	out, code := run("")
	if code != 2 || !strings.Contains(out, "could_not_run: no writable temporary directory on ext2/ext3/ext4, xfs, btrfs or tmpfs") ||
		!strings.Contains(out, tmp+" (overlayfs)") || !strings.Contains(out, "/dev/shm (overlayfs)") {
		t.Errorf("no supported filesystem: exit %d, %s", code, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("no supported filesystem: adcd was started")
	}
	if len(entries(tmp)) != 0 || kept(out) != "" {
		t.Error("no supported filesystem: a work directory was created")
	}

	// The positive controls: the driver gets past the selection (to fail on
	// the fake prefix) with a private work directory under $TMPDIR when that
	// is tmpfs, and under /dev/shm when only that is.
	for _, c := range []struct{ tmpfs, parent string }{{tmp, tmp}, {"/dev/shm", "/dev/shm"}} {
		out, code = run(c.tmpfs)
		dir := kept(out)
		if code == 2 || filepath.Dir(dir) != c.parent {
			t.Errorf("tmpfs %s: exit %d, %s", c.tmpfs, code, out)
			continue
		}
		if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("tmpfs %s: the work directory is not private: %v", c.tmpfs, err)
		}
	}
	if len(entries(tmp)) != 1 {
		t.Errorf("tmpfs /dev/shm: a work directory was made under $TMPDIR too")
	}
}
