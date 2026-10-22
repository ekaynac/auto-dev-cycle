// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// runs/ must be its own filesystem: one shared with the state directory
// (a run could fill the ledger's disk) is refused.
func TestOwnDisk(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	h := Host{Podman: "/usr/bin/podman", Root: t.TempDir(), Home: home, Forbidden: []string{state, state + "/adcd.sock"}}
	must(t, os.Mkdir(filepath.Join(h.Root, "runs"), 0o700))
	if err := h.Validate(); !errors.Is(err, ErrHost) {
		t.Fatalf("runs/ on the state directory's filesystem: %v, want ErrHost", err)
	}
	h.Root = t.TempDir()
	if err := h.Validate(); !errors.Is(err, ErrHost) {
		t.Fatalf("no runs/: %v, want ErrHost", err)
	}
	var shm, tmp syscall.Stat_t
	if syscall.Stat("/dev/shm", &shm) != nil || syscall.Stat(state, &tmp) != nil || shm.Dev == tmp.Dev {
		t.Skip("no second filesystem (/dev/shm) for the positive case")
	}
	own, err := os.MkdirTemp("/dev/shm", "runs")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(own) })
	must(t, os.Symlink(own, filepath.Join(h.Root, "runs")))
	if err := h.Validate(); err != nil {
		t.Fatalf("runs/ on its own filesystem: %v", err)
	}
	// A root reached through a symlink is refused: the overlap checks
	// would compare the link, not what the kernel sees.
	link := filepath.Join(t.TempDir(), "root")
	must(t, os.Symlink(h.Root, link))
	if err := (Host{Podman: h.Podman, Root: link, Home: home, Forbidden: h.Forbidden}).Validate(); !errors.Is(err, ErrHost) {
		t.Errorf("a symlinked root: %v, want ErrHost", err)
	}
	// A device of its own on a filesystem type that may share a pool
	// (here sysfs, standing in for btrfs, ZFS or overlay) is refused.
	must(t, os.Remove(filepath.Join(h.Root, "runs")))
	must(t, os.Symlink("/sys", filepath.Join(h.Root, "runs")))
	if err := h.Validate(); !errors.Is(err, ErrHost) {
		t.Errorf("runs/ on sysfs: %v, want ErrHost", err)
	}
}

// What a run leaves is bounded once it ends: over Limits.Disk the run
// fails, under it the run passes.
func TestDiskBoundAfterRun(t *testing.T) {
	for _, c := range []struct {
		size int
		ok   bool
	}{{1 << 10, true}, {2 << 20, false}} {
		h := testHost(t, &fake{})
		r := testRun(t, h, Gate)
		must(t, os.WriteFile(filepath.Join(r.Attempt.Workspace(), "out"), make([]byte, c.size), 0o600))
		if _, err := h.Start(context.Background(), r); (err == nil) != c.ok || !c.ok && !errors.Is(err, ErrSpec) {
			t.Errorf("%d bytes left: %v, want ok=%v", c.size, err, c.ok)
		}
	}
	// A run that fails is bounded too.
	h := testHost(t, &fake{fn: func(_ context.Context, args []string) ([]byte, error) {
		if slices.Contains(args, "run") {
			return nil, errors.New("exit status 1")
		}
		return nil, nil
	}})
	r := testRun(t, h, Gate)
	must(t, os.WriteFile(filepath.Join(r.Attempt.Workspace(), "out"), make([]byte, 2<<20), 0o600))
	if _, err := h.Start(context.Background(), r); !errors.Is(err, ErrSpec) {
		t.Errorf("a failed run over the bound: %v, want ErrSpec", err)
	}
}

// Copy-out refuses more entries or levels than its caps, reading
// directories in batches.
func TestCopyCaps(t *testing.T) {
	saved := [3]int{maxEntries, maxDepth, maxBatch}
	t.Cleanup(func() { maxEntries, maxDepth, maxBatch = saved[0], saved[1], saved[2] })
	maxEntries, maxDepth, maxBatch = 6, 3, 2
	h := testHost(t, &fake{})
	a, err := h.NewAttempt()
	must(t, err)
	must(t, os.WriteFile(filepath.Join(a.Results(), "f0"), nil, 0o600))
	must(t, a.CopyOut(filepath.Join(t.TempDir(), "ok"), 1024))
	for i := range 7 {
		must(t, os.WriteFile(filepath.Join(a.Results(), "f"+string(rune('0'+i))), nil, 0o600))
	}
	if err := a.CopyOut(filepath.Join(t.TempDir(), "wide"), 1024); !errors.Is(err, ErrSpec) {
		t.Errorf("7 entries: %v, want ErrSpec", err)
	}
	a2, err := h.NewAttempt()
	must(t, err)
	must(t, os.MkdirAll(filepath.Join(a2.Results(), "a", "b", "c", "d"), 0o700))
	if err := a2.CopyOut(filepath.Join(t.TempDir(), "deep"), 1024); !errors.Is(err, ErrSpec) {
		t.Errorf("4 levels: %v, want ErrSpec", err)
	}
}

// A git too old for GIT_ATTR_SOURCE, and a SHA-256 repository, are refused.
func TestGitVersionAndObjectFormat(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if err := checkGit(); err != nil {
		t.Fatalf("this host's git: %v", err)
	}
	saved := minGit
	minGit = [2]int{99, 0}
	err := checkGit()
	minGit = saved
	if !errors.Is(err, ErrGit) {
		t.Fatalf("a git older than the minimum: %v, want ErrGit", err)
	}
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "src")
	gitOK(t, git(ctx, "/", nil, "init", "--quiet", "--object-format=sha256", "--initial-branch=main", src))
	gitOK(t, git(ctx, src, nil, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--quiet", "--allow-empty", "-m", "b"))
	h := testHost(t, &fake{})
	if _, err := h.Clone(ctx, src, "refs/heads/main", filepath.Join(t.TempDir(), "ctl")); !errors.Is(err, ErrGit) {
		t.Fatalf("a sha256 repository: %v, want ErrGit", err)
	}
}

// A pin must name its registry, as Podman's RepoDigests do.
func TestShortPinRefused(t *testing.T) {
	f := &fake{}
	h := testHost(t, f)
	for _, ref := range []string{"debian@sha256:" + strings.Repeat("a", 64), "library/debian@sha256:" + strings.Repeat("a", 64)} {
		if _, err := h.Resolve(context.Background(), ref); !errors.Is(err, ErrImage) {
			t.Errorf("%s: %v, want ErrImage", ref, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("podman ran for a short pin")
	}
}

// URLs: only https, ssh and file are judged, the scheme case-folded; a
// fetch judges every repository argument that is not a remote's name.
func TestURLTightening(t *testing.T) {
	h := hostile(t)
	encoded := strings.Replace(h.ws, "/runs/", "/%72uns/", 1) // git decodes %72 to r
	for url, want := range map[string]bool{
		// HTTPS:// passes this classifier only; git itself refuses the scheme.
		"https://example.org/r.git": false, "HTTPS://example.org/r.git": false, "ssh://git@example.org/r": false,
		"FILE://" + h.ws: true, "File://" + h.ws: true, "git://example.org/r": true, "http://example.org/r": true,
		"ext::sh -c x": true, "..": true, "file://host/r": true, h.src: false, "file://" + h.src: true,
		"file:///srv/ok/%2e%2e/adc/state": true, "file:///srv/%61dc/state": true, "file://" + encoded: true,
	} {
		if got := h.host.refused(url); got != want {
			t.Errorf("refused(%q) = %v, want %v", url, got, want)
		}
	}
	ctx := context.Background()
	repo, err := h.host.Clone(ctx, h.src, "refs/heads/main", filepath.Join(t.TempDir(), "ctl"))
	must(t, err)
	for _, args := range [][]string{{"fetch", ".."}, {"fetch", "../ws"}, {"fetch", "FILE://" + h.ws}, {"fetch", "evil"},
		{"fetch", "-u", "x", "origin"}, {"fetch", "--upload-pack=touch x", "origin"}, {"pull", "--exec=x", "origin"}, {"fetch", "origin", "--quiet"}} {
		if _, err := repo.Git(ctx, t.TempDir(), args...); !errors.Is(err, ErrSpec) {
			t.Errorf("%q: %v, want ErrSpec", args, err)
		}
	}
	if _, err := repo.Git(ctx, t.TempDir(), "fetch", "origin", "main"); err != nil {
		t.Errorf("fetch origin: %v", err)
	}
}

// Opus #393 M3: a forbidden path under a directory the controller cannot
// search (root's 0700 /run/podman; here one of the test's own, mode 0) is
// measured as that directory's device, so it does not refuse the host;
// the probe under it gives EACCES (the control).
func TestDevOfAnUnsearchableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root searches any directory")
	}
	closed := filepath.Join(t.TempDir(), "closed")
	must(t, os.Mkdir(closed, 0o700))
	must(t, os.Chmod(closed, 0))
	t.Cleanup(func() { _ = os.Chmod(closed, 0o700) })
	sock := filepath.Join(closed, "podman.sock")
	var st syscall.Stat_t
	if err := syscall.Stat(sock, &st); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("the control: %v, want EACCES", err)
	}
	var parent syscall.Stat_t
	must(t, syscall.Stat(filepath.Dir(closed), &parent))
	if d, err := devOf(sock, true); err != nil || d != uint64(parent.Dev) {
		t.Fatalf("devOf: %d, %v; want the device %d of the directory above", d, err, parent.Dev)
	}
	if _, err := devOf(sock, false); !errors.Is(err, syscall.EACCES) {
		t.Errorf("devOf without climbing: %v, want EACCES", err)
	}
}
