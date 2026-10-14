// SPDX-License-Identifier: Apache-2.0

//go:build linux

package adapterhost

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

const (
	RefusedPrefix = "refused-" // a lifted subtree's or refused root's name: each start sweeps them
	MaxWork       = 1 << 16    // the entries one cleanup may visit; the rest waits for the next sweep
	maxDepth      = 3          // the root, in/ and out/, their entries: deeper is lifted
)

var errWork = errors.New("adapterhost: the cleanup's work budget is spent")

// liftName names a lifted subtree; a test replaces it.
var liftName = func() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return RefusedPrefix + "lift-" + hex.EncodeToString(b[:])
}

// Cleanup is one bounded removal: its context, the entries it may still
// visit, and its first failure (Err; later ones are dropped).
type Cleanup struct {
	Ctx  context.Context
	Left int
	err  error
}

func (c *Cleanup) Err() error { return c.err }

// Visit spends one entry, or records why the cleanup stops.
func (c *Cleanup) Visit() bool {
	if err := c.stop(); err != nil {
		c.err = cmp.Or(c.err, err)
		return false
	}
	c.Left--
	return true
}

// stop is why the cleanup may visit nothing more, if it may not.
func (c *Cleanup) stop() error {
	if c.Left <= 0 {
		return errWork
	}
	return c.Ctx.Err()
}

// RemoveAt removes name beneath dir following no symlink: a directory is
// pinned (O_PATH|O_NOFOLLOW), made 0700 through it, emptied, removed; past
// maxDepth it is lifted to dir under a fresh name (RENAME_NOREPLACE) and
// removed in turn, for bounded descriptors and stack. It reports whether name
// is gone; what is left keeps a name the next start sweeps.
func (c *Cleanup) RemoveAt(dir int, name string) bool {
	gone := true
	for todo := []string{name}; len(todo) > 0; todo = todo[1:] {
		lifted, err := c.removeTree(dir, dir, todo[0], 0)
		c.err, gone = cmp.Or(c.err, err), gone && (err == nil || todo[0] != name)
		todo = append(todo, lifted...)
	}
	return gone
}

// removeTree removes name beneath dir, at depth below top; lifted are the
// subtrees it moved to top, each by a rename that succeeded.
func (c *Cleanup) removeTree(top, dir int, name string, depth int) (lifted []string, err error) {
	if !c.Visit() {
		return nil, c.stop()
	}
	if err := unix.Unlinkat(dir, name, 0); err == nil || err == unix.ENOENT {
		return nil, nil
	} else if err != unix.EISDIR {
		return nil, err
	}
	fd, err := unix.Openat(dir, name, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	// An O_PATH descriptor has no fchmod; its /proc link reaches the pinned
	// inode, never a name. Before a lift too: moving a directory needs write.
	if err := unix.Chmod("/proc/self/fd/"+strconv.Itoa(fd), 0o700); err != nil {
		return nil, err
	}
	if depth == maxDepth {
		for range 3 { // fresh names tried
			to := liftName()
			if err = unix.Renameat2(dir, name, top, to, unix.RENAME_NOREPLACE); err == nil {
				return []string{to}, nil
			} else if err != unix.EEXIST {
				break
			}
		}
		return nil, err
	}
	lfd, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	list := os.NewFile(uintptr(lfd), name)
	defer list.Close()
	for {
		names, rerr := list.Readdirnames(256)
		for _, n := range names {
			l, e := c.removeTree(top, fd, n, depth+1)
			lifted, err = append(lifted, l...), cmp.Or(err, e) // the first failure only
		}
		if rerr == nil && c.stop() == nil {
			continue
		} else if rerr == io.EOF && err == nil {
			return lifted, unix.Unlinkat(dir, name, unix.AT_REMOVEDIR)
		}
		return lifted, cmp.Or(err, c.stop(), rerr)
	}
}
