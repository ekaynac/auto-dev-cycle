// SPDX-License-Identifier: Apache-2.0

//go:build linux

package adapterhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// tree makes each of paths (directories) beneath a new parent, then gives each of modes its mode, and
// returns the parent and its descriptor; the modes are undone at cleanup, for TempDir.
func tree(t *testing.T, paths []string, modes map[string]os.FileMode) (string, int) {
	t.Helper()
	parent := t.TempDir()
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Join(parent, p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for p, m := range modes {
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(parent, p), 0o700) })
		if err := os.Chmod(filepath.Join(parent, p), m); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := unix.Open(parent, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(dir) })
	return parent, dir
}

func cleanup(ctx context.Context, left int) *Cleanup { return &Cleanup{Ctx: ctx, Left: left} }

func left(t *testing.T, parent string) []string {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// L2 (Opus on #261): RemoveAt removes a tree of any depth with a bounded number of descriptors and a bounded stack.
// Under a descriptor limit only 32 above those open, a chain 200 directories deep, 300 entries wide at its top, is
// removed: each subtree past maxDepth is lifted beneath the parent and removed in turn.
func TestRemoveAtTakesBoundedDescriptors(t *testing.T) {
	p := "deep"
	for range 200 {
		p = filepath.Join(p, "d")
	}
	parent, dir := tree(t, []string{p}, nil)
	for i := range 300 {
		if err := os.WriteFile(filepath.Join(parent, "deep", strconv.Itoa(i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	tight := lim
	tight.Cur = uint64(openFDs(t) + 32)
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &tight); err != nil {
		t.Fatal(err)
	}
	c := cleanup(context.Background(), MaxWork)
	gone := c.RemoveAt(dir, "deep")
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	if !gone || c.Err() != nil || len(left(t, parent)) != 0 {
		t.Fatalf("RemoveAt: %v, %v; left %v", gone, c.Err(), left(t, parent))
	}
}

// H1 (Astra on #261): a lift whose every destination exists (here a planted ancestor's name, as when the name was
// derived from the inode) fails and is never queued: RemoveAt returns after four visits, not at the work budget,
// reporting it, and the subtree stays.
func TestALiftCollisionEnds(t *testing.T) {
	const planted = RefusedPrefix + "lift-planted"
	parent, dir := tree(t, []string{planted + "/a/b/c/d"}, nil)
	old := liftName
	liftName = func() string { return planted }
	t.Cleanup(func() { liftName = old })
	done := make(chan *Cleanup)
	go func() {
		c := cleanup(context.Background(), MaxWork)
		c.RemoveAt(dir, planted)
		done <- c
	}()
	select {
	case c := <-done:
		if !errors.Is(c.Err(), unix.EEXIST) || c.Left < MaxWork-5 { // planted, a, b, c: four visits
			t.Fatalf("the collision: %v after %d visits", c.Err(), MaxWork-c.Left)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RemoveAt did not return") // the goroutine leaks: the test binary ends with the failure
	}
	if _, err := os.Lstat(filepath.Join(parent, planted, "a", "b", "c", "d")); err != nil {
		t.Fatalf("the subtree that could not be lifted: %v", err)
	}
}

// M3 (Astra on #261): a directory at mode 000 at the lift boundary (depth 3) and past it (depth 4) is removed: the
// pinned inode is made 0700 before the lift, which needs write on it.
func TestModeZeroAtTheLiftBoundary(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root moves and reads a directory at mode 000")
	}
	parent, dir := tree(t, []string{"root/out/a/b/x", "root/out/a2/b2/c2/y"}, map[string]os.FileMode{
		"root/out/a2/b2/c2": 0, "root/out/a/b": 0,
	})
	c := cleanup(context.Background(), MaxWork)
	if !c.RemoveAt(dir, "root") || c.Err() != nil || len(left(t, parent)) != 0 {
		t.Fatalf("RemoveAt: %v; left %v", c.Err(), left(t, parent))
	}
}

// M2 (Astra on #261): a cleanup stops within its work budget, or when its context ends during a removal, with one
// error, and what is left keeps its name for the next start's sweep, which finishes it.
func TestACleanupStopsWithinItsBudget(t *testing.T) {
	for name, c := range map[string]*Cleanup{
		"a spent budget":      cleanup(context.Background(), 100),
		"a cancelled removal": cleanup(&cancelAfter{Context: context.Background(), n: 100}, MaxWork),
	} {
		t.Run(name, func(t *testing.T) {
			parent, dir := tree(t, []string{RefusedPrefix + "wide"}, nil)
			for i := range 1000 {
				if err := os.WriteFile(filepath.Join(parent, RefusedPrefix+"wide", strconv.Itoa(i)), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if c.RemoveAt(dir, RefusedPrefix+"wide") || c.Err() == nil || c.Left < 0 {
				t.Fatalf("a removal past its bound: %v, %d left", c.Err(), c.Left)
			}
			if got := left(t, parent); len(got) != 1 || got[0] != RefusedPrefix+"wide" {
				t.Fatalf("left %v", got)
			}
			next := cleanup(context.Background(), MaxWork)
			if !next.RemoveAt(dir, RefusedPrefix+"wide") || next.Err() != nil {
				t.Fatalf("the next start: %v", next.Err())
			}
		})
	}
}

// cancelAfter is a context cancelled after n checks of Err.
type cancelAfter struct {
	context.Context
	n int
}

func (c *cancelAfter) Err() error {
	if c.n--; c.n < 0 {
		return context.Canceled
	}
	return nil
}
