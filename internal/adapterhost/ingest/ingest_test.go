// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const bound = 1 << 20

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// env is one test's exchange: out/ (held open), a directory outside it and a
// blob store.
type env struct {
	t        *testing.T
	root     string
	out      string
	outside  string
	stateDir string
	store    *blobstore.Store
	dir      *os.File
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{t: t, root: root, out: filepath.Join(root, "out"), outside: filepath.Join(root, "outside"), stateDir: filepath.Join(root, "state")}
	for _, d := range []string{e.out, e.outside, e.stateDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	if e.store, err = blobstore.Open(e.stateDir); err != nil {
		t.Fatal(err)
	}
	e.dir = openDir(t, e.out)
	return e
}

func openDir(t *testing.T, p string) *os.File {
	t.Helper()
	fd, err := unix.Open(p, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fd), p)
	t.Cleanup(func() { f.Close() })
	return f
}

func (e *env) write(p string, b []byte) {
	e.t.Helper()
	if err := os.WriteFile(p, b, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// assertEmpty proves the blob store published nothing and kept no temp file.
func (e *env) assertEmpty() {
	e.t.Helper()
	for _, d := range []string{"sha256", "tmp"} {
		ents, err := os.ReadDir(filepath.Join(e.stateDir, "blobs", d))
		if err != nil {
			e.t.Fatal(err)
		}
		if len(ents) != 0 {
			e.t.Fatalf("blob store %s/ is not empty: %v", d, ents)
		}
	}
}

// withFallback runs f once with openat2 and once with openat2 reporting
// ENOSYS, so every case covers the openat fallback too.
func withFallback(t *testing.T, f func(t *testing.T)) {
	t.Run("openat2", f)
	t.Run("fallback", func(t *testing.T) {
		openat2 = func(int, string, *unix.OpenHow) (int, error) { return -1, unix.ENOSYS }
		t.Cleanup(func() { openat2 = unix.Openat2 })
		f(t)
	})
}

func setHook(t *testing.T, h func(stage string)) {
	testHook = h
	t.Cleanup(func() { testHook = nil })
}

// content is several io.Copy buffers (32 KiB) long, so a "copying" hook
// mutates the file between chunks of the copy.
var content = bytes.Repeat([]byte("adapter output\n"), 14000)

type ingestCase struct {
	name    string
	handle  *string      // nil: "res"
	setup   func(e *env) // creates the file under e.out
	declare func(e *env) (digest string, size int64)
	hook    func(e *env) func(stage string)
	want    string // the Unsafe check, or "" for success
}

func cases() []ingestCase {
	plain := func(e *env) { e.write(filepath.Join(e.out, "res"), content) }
	return []ingestCase{
		{name: "clean file", setup: plain},
		{name: "symlink inside out", setup: func(e *env) {
			e.write(filepath.Join(e.out, "real"), content)
			must(e.t, os.Symlink("real", filepath.Join(e.out, "res")))
		}, want: CheckSymlink},
		{name: "symlink outside", setup: func(e *env) {
			e.write(filepath.Join(e.outside, "secret"), content)
			must(e.t, os.Symlink(filepath.Join(e.outside, "secret"), filepath.Join(e.out, "res")))
		}, want: CheckSymlink},
		{name: "dangling symlink", setup: func(e *env) {
			must(e.t, os.Symlink("nowhere", filepath.Join(e.out, "res")))
		}, want: CheckSymlink},
		{name: "unix socket", setup: func(e *env) {
			must(e.t, unix.Mknod(filepath.Join(e.out, "res"), unix.S_IFSOCK|0o600, 0))
		}, want: CheckSpecialFile},
		{name: "directory", setup: func(e *env) { must(e.t, os.Mkdir(filepath.Join(e.out, "res"), 0o700)) }, want: CheckSpecialFile},
		{name: "hard link", setup: func(e *env) {
			plain(e)
			must(e.t, os.Link(filepath.Join(e.out, "res"), filepath.Join(e.out, "other")))
		}, want: CheckHardLink},
		{name: "hard link to outside", setup: func(e *env) {
			e.write(filepath.Join(e.outside, "secret"), content)
			must(e.t, os.Link(filepath.Join(e.outside, "secret"), filepath.Join(e.out, "res")))
		}, want: CheckHardLink},
		{name: "missing", setup: func(*env) {}, want: CheckUnreadable},
		{name: "oversize", setup: func(e *env) { e.write(filepath.Join(e.out, "res"), make([]byte, bound+1)) },
			declare: func(*env) (string, int64) { return digestOf(make([]byte, bound+1)), bound + 1 }, want: CheckSize},
		{name: "size mismatch", setup: plain,
			declare: func(*env) (string, int64) { return digestOf(content), int64(len(content)) - 1 }, want: CheckSize},
		{name: "digest mismatch", setup: plain,
			declare: func(*env) (string, int64) { return digestOf([]byte("other")), int64(len(content)) }, want: CheckDigest},
		{name: "malformed digest", setup: plain,
			declare: func(*env) (string, int64) { return "sha256:xyz", int64(len(content)) }, want: CheckDigest},
		{name: "renamed over between checks and read", setup: plain, hook: func(e *env) func(string) {
			return func(stage string) {
				if stage == "checked" {
					e.write(filepath.Join(e.out, "new"), content)
					must(e.t, os.Rename(filepath.Join(e.out, "new"), filepath.Join(e.out, "res")))
				}
			}
		}, want: CheckReplaced},
		{name: "replaced by a symlink between checks and read", setup: plain, hook: func(e *env) func(string) {
			return func(stage string) {
				if stage == "checked" {
					e.write(filepath.Join(e.outside, "secret"), content)
					must(e.t, os.Remove(filepath.Join(e.out, "res")))
					must(e.t, os.Symlink(filepath.Join(e.outside, "secret"), filepath.Join(e.out, "res")))
				}
			}
		}, want: CheckReplaced},
		{name: "modified after checks", setup: plain, hook: func(e *env) func(string) {
			return func(stage string) {
				if stage == "checked" {
					appendTo(e.t, filepath.Join(e.out, "res"))
				}
			}
		}, want: CheckReplaced},
		{name: "outside hard link created between checks and read", setup: plain, hook: func(e *env) func(string) {
			return func(stage string) {
				if stage == "checked" {
					must(e.t, os.Link(filepath.Join(e.out, "res"), filepath.Join(e.outside, "grabbed")))
				}
			}
		}, want: CheckHardLink},
		{name: "outside hard link created during the copy", setup: plain, hook: func(e *env) func(string) {
			return func(stage string) {
				if stage == "copying" {
					must(e.t, os.Link(filepath.Join(e.out, "res"), filepath.Join(e.outside, "grabbed")))
				}
			}
		}, want: CheckHardLink},
		{name: "unlinked during the copy", setup: plain, hook: func(e *env) func(string) {
			return func(stage string) {
				if stage == "copying" {
					must(e.t, os.Remove(filepath.Join(e.out, "res")))
				}
			}
		}, want: CheckReplaced},
		{name: "modified during the copy", setup: plain, hook: func(e *env) func(string) {
			return func(stage string) {
				if stage == "copying" {
					appendTo(e.t, filepath.Join(e.out, "res"))
				}
			}
		}, want: CheckReplaced},
		{name: "truncated during the copy", setup: plain, hook: func(e *env) func(string) {
			return func(stage string) {
				if stage == "copying" {
					must(e.t, os.Truncate(filepath.Join(e.out, "res"), 10))
				}
			}
		}, want: CheckReplaced},
		{name: "slash in name", handle: ptr("sub/res"), setup: func(e *env) {
			must(e.t, os.Mkdir(filepath.Join(e.out, "sub"), 0o700))
			e.write(filepath.Join(e.out, "sub", "res"), content)
		}, want: CheckPath},
		{name: "dot-dot", handle: ptr(".."), setup: plain, want: CheckPath},
		{name: "dot-dot path", handle: ptr("../outside/secret"), setup: func(e *env) {
			e.write(filepath.Join(e.outside, "secret"), content)
		}, want: CheckPath},
		{name: "empty name", handle: ptr(""), setup: plain, want: CheckPath},
		{name: "pattern mismatch", handle: ptr(".hidden"), setup: plain, want: CheckPath},
	}
}

func ptr(s string) *string { return &s }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, p string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	must(t, err)
	_, err = f.Write([]byte("more"))
	must(t, err)
	must(t, f.Close())
}

func run(t *testing.T, c ingestCase) {
	e := newEnv(t)
	c.setup(e)
	digest, size := digestOf(content), int64(len(content))
	if c.declare != nil {
		digest, size = c.declare(e)
	}
	if c.hook != nil {
		setHook(t, c.hook(e))
	}
	handle := "res"
	if c.handle != nil {
		handle = *c.handle
	}
	got, err := Ingest(e.dir, handle, digest, size, bound, e.store)
	if c.want == "" {
		if err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		if got != digest {
			t.Fatalf("digest %s, want %s", got, digest)
		}
		must(t, e.store.Verify(got))
		return
	}
	var u *Unsafe
	if !errors.As(err, &u) {
		t.Fatalf("Ingest = %q, %v; want *Unsafe %s", got, err, c.want)
	}
	if u.Check != c.want {
		t.Fatalf("check %s (%s), want %s", u.Check, u.Detail, c.want)
	}
	if u.Reason() != vocab.ReasonUnsafeOutput {
		t.Fatalf("reason %s", u.Reason())
	}
	e.assertEmpty()
}

func TestIngest(t *testing.T) {
	for _, c := range cases() {
		t.Run(c.name, func(t *testing.T) { withFallback(t, func(t *testing.T) { run(t, c) }) })
	}
}

// A FIFO is inspected through O_PATH only, so Ingest refuses it without
// ever blocking on an open for reading.
func TestFIFONeverBlocks(t *testing.T) {
	withFallback(t, func(t *testing.T) {
		e := newEnv(t)
		must(t, unix.Mkfifo(filepath.Join(e.out, "res"), 0o600))
		done := make(chan error, 1)
		go func() {
			_, err := Ingest(e.dir, "res", digestOf(nil), 0, bound, e.store)
			done <- err
		}()
		select {
		case err := <-done:
			var u *Unsafe
			if !errors.As(err, &u) || u.Check != CheckSpecialFile {
				t.Fatalf("Ingest(fifo) = %v, want special_file", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Ingest blocked on a FIFO")
		}
		e.assertEmpty()
	})
}

// A device node is refused by type; /dev/null is the one reachable without
// privilege, held as its own "out/".
func TestDeviceNode(t *testing.T) {
	if _, err := os.Lstat("/dev/null"); err != nil {
		t.Skip("no /dev/null:", err)
	}
	withFallback(t, func(t *testing.T) {
		e := newEnv(t)
		_, err := Ingest(openDir(t, "/dev"), "null", digestOf(nil), 0, bound, e.store)
		var u *Unsafe
		if !errors.As(err, &u) || u.Check != CheckSpecialFile {
			t.Fatalf("Ingest(/dev/null) = %v, want special_file", err)
		}
		e.assertEmpty()
	})
}

func TestClassify(t *testing.T) {
	for mode, want := range map[uint32]string{
		unix.S_IFREG | 0o644: "",
		unix.S_IFLNK | 0o777: CheckSymlink,
		unix.S_IFDIR | 0o755: CheckSpecialFile,
		unix.S_IFIFO | 0o600: CheckSpecialFile,
		unix.S_IFSOCK:        CheckSpecialFile,
		unix.S_IFCHR | 0o666: CheckSpecialFile,
		unix.S_IFBLK | 0o660: CheckSpecialFile,
		0:                    CheckSpecialFile,
	} {
		if got := classify(mode); got != want {
			t.Errorf("classify(%#o) = %q, want %q", mode, got, want)
		}
	}
}

// out/ replaced by a symlink after its fd was opened: the held fd still
// names the original directory, so Ingest reads only what was in it.
func TestOutReplacedAfterOpen(t *testing.T) {
	withFallback(t, func(t *testing.T) {
		e := newEnv(t)
		e.write(filepath.Join(e.out, "res"), content)
		e.write(filepath.Join(e.outside, "res"), []byte("escaped"))
		must(t, os.Rename(e.out, filepath.Join(e.root, "out.old")))
		must(t, os.Symlink(e.outside, e.out))
		got, err := Ingest(e.dir, "res", digestOf(content), int64(len(content)), bound, e.store)
		if err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		if got != digestOf(content) {
			t.Fatalf("ingested %s, want the original out/ file", got)
		}
		_, err = Ingest(e.dir, "res", digestOf([]byte("escaped")), 7, bound, e.store)
		var u *Unsafe
		if !errors.As(err, &u) {
			t.Fatalf("the outside file was ingested through the replaced out/: %v", err)
		}
	})
}

// Mutation test: with the link-count check switched off the hard-link case
// ingests, so that case is caught by exactly that check.
func TestMutationHardLink(t *testing.T) {
	skipCheck = func(c string) bool { return c == CheckHardLink }
	t.Cleanup(func() { skipCheck = nil })
	for _, c := range cases() {
		if c.name == "hard link" {
			c.want = ""
			withFallback(t, func(t *testing.T) { run(t, c) })
		}
	}
}
