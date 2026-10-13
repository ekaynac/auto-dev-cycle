// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A local path reaches only where it says: a ".." after a symlink, and
// each place git looks beside a path (p/.git, p.git, a gitfile), are
// refused for clone and fetch. Plain git reaches the run's repository
// through every one of them, so each case discriminates.
func TestLocalPathBypasses(t *testing.T) {
	h := hostile(t)
	ctx := context.Background()
	out := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(h.ws, "d/e/f/g/h"), 0o700))
	must(t, os.Symlink(filepath.Join(h.ws, "d/e/f/g/h"), filepath.Join(out, "link")))
	must(t, os.MkdirAll(filepath.Join(out, "q"), 0o700))
	must(t, os.Symlink(filepath.Join(h.ws, ".git"), filepath.Join(out, "q", ".git")))
	must(t, os.Symlink(h.ws, filepath.Join(out, "r.git")))
	must(t, os.MkdirAll(filepath.Join(out, "s"), 0o700))
	must(t, os.WriteFile(filepath.Join(out, "s", ".git"), []byte("gitdir: "+filepath.Join(h.ws, ".git")+"\n"), 0o600))
	repo, err := h.host.Clone(ctx, h.src, "refs/heads/main", filepath.Join(t.TempDir(), "ctl"))
	must(t, err)
	for name, p := range map[string]string{
		"a .. after a symlink": filepath.Join(out, "link") + "/../../../../..",
		"p/.git pointing in":   filepath.Join(out, "q"),
		"p.git":                filepath.Join(out, "r"),
		"a gitfile":            filepath.Join(out, "s"),
	} {
		gitOK(t, git(ctx, "/", nil, "clone", "--quiet", "--no-local", p, filepath.Join(t.TempDir(), "plain")))
		if _, err := h.host.Clone(ctx, p, "refs/heads/main", filepath.Join(t.TempDir(), "d")); err != ErrSpec {
			t.Errorf("clone through %s: %v, want ErrSpec", name, err)
		}
		if _, err := repo.Git(ctx, t.TempDir(), "fetch", p); !errors.Is(err, ErrSpec) {
			t.Errorf("fetch through %s: %v, want ErrSpec", name, err)
		}
	}
	// Subcommands outside the allowlist reach other repositories without
	// the fetch guard: refused, as is a global option before any.
	for _, args := range [][]string{{"--no-pager", "fetch", "--upload-pack=touch x", "origin"}, {"ls-remote", h.ws},
		{"archive", "--remote=" + h.ws, "HEAD"}, {"clone", h.ws, "x"}, {"remote", "add", "w", h.ws}, {"submodule", "update", "--init"}} {
		if _, err := repo.Git(ctx, t.TempDir(), args...); !errors.Is(err, ErrSpec) {
			t.Errorf("%q: %v, want ErrSpec", args, err)
		}
	}
}

// Blocks allocated count, not only sizes: a zero-length file with 2 MiB
// preallocated (fallocate KEEP_SIZE) is over a 1 MiB bound.
func TestPreallocatedBlocksCount(t *testing.T) {
	h := testHost(t, &fake{})
	r := testRun(t, h, Gate)
	f, err := os.Create(filepath.Join(r.Attempt.Workspace(), "hole"))
	must(t, err)
	err = syscall.Fallocate(int(f.Fd()), 1 /* FALLOC_FL_KEEP_SIZE */, 0, 2<<20)
	f.Close()
	if err != nil {
		t.Skipf("no fallocate here: %v", err)
	}
	if _, err := h.Start(context.Background(), r); !errors.Is(err, ErrSpec) {
		t.Fatalf("2 MiB preallocated in a zero-length file: %v, want ErrSpec", err)
	}
}

// A run refused at the entry cap is still torn down, unreadable
// directories included: teardown walks without the cap.
func TestTeardownAfterEntryCap(t *testing.T) {
	saved := maxEntries
	t.Cleanup(func() { maxEntries = saved })
	maxEntries = 5
	h := testHost(t, &fake{})
	r := testRun(t, h, Gate)
	for i := range 20 {
		must(t, os.WriteFile(filepath.Join(r.Attempt.Workspace(), "f"+strconv.Itoa(i)), nil, 0o600))
	}
	must(t, os.MkdirAll(filepath.Join(r.Attempt.Results(), "locked", "in"), 0o700))
	must(t, os.Chmod(filepath.Join(r.Attempt.Results(), "locked"), 0))
	if _, err := h.Start(context.Background(), r); !errors.Is(err, ErrSpec) {
		t.Fatalf("20 entries over a cap of 5: %v, want ErrSpec", err)
	}
	must(t, r.Attempt.Teardown())
	if _, err := os.Lstat(r.Attempt.Dir); !os.IsNotExist(err) {
		t.Fatalf("teardown left %s: %v", r.Attempt.Dir, err)
	}
}

// The probe's end-to-end deadline (the caller's context) fires in image
// inspection and in the run alike, and the attempt is still torn down.
func TestProbeDeadline(t *testing.T) {
	block := func(ctx context.Context, args []string) ([]byte, error) {
		if slices.Contains(args, "rm") {
			return nil, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	h := testHost(t, &fake{fn: block})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := h.Resolve(ctx, "docker.io/library/debian@sha256:"+strings.Repeat("a", 64)); !errors.Is(err, ErrImage) || ctx.Err() == nil {
		t.Fatalf("inspection past the deadline: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := h.Probe(ctx, Image{Pinned: "x@sha256:" + testID, ID: testID}, Gate, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a probe past the deadline: %v, want the deadline", err)
	}
	if runs, _ := os.ReadDir(filepath.Join(h.Root, "runs")); len(runs) != 0 {
		t.Errorf("the timed-out probe's attempt was not torn down: %v", runs)
	}
}

// The floor is git 2.41, which brought GIT_ATTR_SOURCE.
func TestGitVersionFloor(t *testing.T) {
	for line, ok := range map[string]bool{"git version 2.40.9\n": false, "git version 2.41.0\n": true,
		"git version 1.99.0\n": false, "git version 3.0.0\n": true, "nonsense\n": false} {
		if err := checkVersion([]byte(line)); (err == nil) != ok {
			t.Errorf("%q: %v, want ok=%v", line, err, ok)
		}
	}
}
