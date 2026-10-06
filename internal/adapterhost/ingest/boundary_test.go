// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ingest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
)

func wantCheck(t *testing.T, err error, check string) {
	t.Helper()
	var u *Unsafe
	if !errors.As(err, &u) || u.Check != check {
		t.Fatalf("Ingest = %v, want *Unsafe %s", err, check)
	}
}

func wantUnsupported(t *testing.T, err error) {
	t.Helper()
	var u *Unsafe
	if errors.As(err, &u) || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported and no *Unsafe", err)
	}
}

func TestSupported(t *testing.T) {
	if err := Supported(); err != nil {
		t.Fatalf("Supported() = %v on this host", err)
	}
}

func ingestContent(e *env) error {
	_, err := Ingest(e.dir, "res", digestOf(content), int64(len(content)), bound, e.store)
	return err
}

// The checked file replaced by a FIFO between the inspection and the read:
// the reader works on the pinned inode, so the FIFO is never opened (no
// IN_OPEN on it), and the unlinked original is refused.
func TestFIFOSubstitutionNeverOpened(t *testing.T) {
	withFallback(t, func(t *testing.T) {
		e := newEnv(t)
		e.write(filepath.Join(e.out, "res"), content)
		fifo := filepath.Join(e.out, "res")
		in, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
		must(t, err)
		defer unix.Close(in)
		setHook(t, func(stage string) {
			if stage == "checked" {
				must(t, os.Remove(fifo))
				must(t, unix.Mkfifo(fifo, 0o600))
				_, err := unix.InotifyAddWatch(in, fifo, unix.IN_OPEN)
				must(t, err)
			}
		})
		done := make(chan error, 1)
		go func() { done <- ingestContent(e) }()
		select {
		case err := <-done:
			wantCheck(t, err, CheckReplaced)
		case <-time.After(5 * time.Second):
			t.Fatal("Ingest blocked on the substituted FIFO")
		}
		buf := make([]byte, 4096)
		if n, err := unix.Read(in, buf); !errors.Is(err, unix.EAGAIN) {
			t.Fatalf("the substituted FIFO was opened: inotify read %d, %v", n, err)
		}
		e.assertEmpty()
		// The watch does see an open, so its silence above means something.
		fd, err := unix.Open(fifo, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		must(t, err)
		unix.Close(fd)
		if n, err := unix.Read(in, buf); err != nil || n == 0 {
			t.Fatalf("inotify missed a real open: %d, %v", n, err)
		}
	})
}

// An unchanged file the controller may not read is unreadable, not replaced.
func TestUnreadableFile(t *testing.T) {
	withFallback(t, func(t *testing.T) {
		e := newEnv(t)
		p := filepath.Join(e.out, "res")
		e.write(p, content)
		must(t, os.Chmod(p, 0))
		if f, err := os.Open(p); err == nil {
			f.Close()
			t.Skip("mode 000 is readable here (privileged)")
		}
		wantCheck(t, ingestContent(e), CheckUnreadable)
		e.assertEmpty()
	})
}

// Without /proc there is no trusted reopen of the pinned inode: a host gap,
// ErrUnsupported, never the adapter's unsafe_output.
func TestNoProcFailsClosed(t *testing.T) {
	withFallback(t, func(t *testing.T) {
		e := newEnv(t)
		e.write(filepath.Join(e.out, "res"), content)
		procFD = filepath.Join(e.root, "no-proc") + "/"
		t.Cleanup(func() { procFD = "/proc/self/fd/" })
		wantUnsupported(t, ingestContent(e))
		wantUnsupported(t, Supported())
		e.assertEmpty()
	})
}

// A kernel that does not report the mount id cannot prove the mount
// boundary: ErrUnsupported, never the adapter's unsafe_output.
func TestNoMountIDFailsClosed(t *testing.T) {
	withFallback(t, func(t *testing.T) {
		e := newEnv(t)
		e.write(filepath.Join(e.out, "res"), content)
		statx = func(fd int, p string, flags, mask int, x *unix.Statx_t) error {
			err := unix.Statx(fd, p, flags, mask, x)
			x.Mask &^= unix.STATX_MNT_ID | unix.STATX_MNT_ID_UNIQUE
			return err
		}
		t.Cleanup(func() { statx = unix.Statx })
		wantUnsupported(t, ingestContent(e))
		wantUnsupported(t, Supported())
		e.assertEmpty()
	})
}

func TestSameMount(t *testing.T) {
	if err := sameMount(meta{mnt: 7}, 7, "x"); err != nil {
		t.Fatal(err)
	}
	// Same st_dev, another mount: a bind mount on the same filesystem.
	var u *Unsafe
	if err := sameMount(meta{dev: 5, mnt: 8}, 7, "x"); !errors.As(err, &u) || u.Check != CheckSpecialFile {
		t.Fatalf("sameMount across mounts = %v", err)
	}
}

// A same-filesystem bind mount of an outside file over out/res has out/'s
// st_dev, so only the mount id tells it apart. bwrap makes the mount in a
// rootless user and mount namespace and runs this test binary inside it
// (a bare CLONE_NEWUSER child gets no capabilities where unprivileged user
// namespaces are restricted, as on Ubuntu). Without bwrap, or where it may
// not create the namespace, the case is skipped and TestSameMount covers the
// comparison.
func TestBindMountRefused(t *testing.T) {
	if root := os.Getenv("INGEST_BIND_ROOT"); root != "" {
		bindChild(t, root)
		return
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("no bwrap to make a rootless bind mount")
	}
	root := t.TempDir()
	for _, d := range []string{"out", "outside", "state"} {
		must(t, os.Mkdir(filepath.Join(root, d), 0o700))
	}
	secret, res := filepath.Join(root, "outside", "secret"), filepath.Join(root, "out", "res")
	must(t, os.WriteFile(secret, content, 0o600))
	must(t, os.WriteFile(res, nil, 0o600))
	if err := exec.Command(bwrap, "--dev-bind", "/", "/", "--", "/bin/true").Run(); err != nil {
		t.Skipf("bwrap cannot create a namespace here: %v", err)
	}
	cmd := exec.Command(bwrap, "--dev-bind", "/", "/", "--bind", secret, res, "--",
		os.Args[0], "-test.run=^TestBindMountRefused$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), "INGEST_BIND_ROOT="+root)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: TestBindMountRefused") {
		t.Fatalf("child: %v\n%s", err, out)
	}
}

func bindChild(t *testing.T, root string) {
	out, res := filepath.Join(root, "out"), filepath.Join(root, "out", "res")
	var a, b unix.Stat_t
	must(t, unix.Stat(out, &a))
	must(t, unix.Stat(res, &b))
	if a.Dev != b.Dev || b.Size != int64(len(content)) {
		t.Fatalf("want the outside file bound over out/res on the same device: dev %d/%d, size %d", a.Dev, b.Dev, b.Size)
	}
	store, err := blobstore.Open(filepath.Join(root, "state"))
	must(t, err)
	withFallback(t, func(t *testing.T) {
		e := &env{t: t, root: root, out: out, stateDir: filepath.Join(root, "state"), store: store, dir: openDir(t, out)}
		wantCheck(t, ingestContent(e), CheckSpecialFile)
		e.assertEmpty()
	})
}

// Corruption of an already stored blob is the store's fault: a store error,
// never the adapter's unsafe_output.
func TestCorruptStoredBlobIsStoreError(t *testing.T) {
	e := newEnv(t)
	e.write(filepath.Join(e.out, "res"), content)
	d, err := e.store.Put(strings.NewReader(string(content)), "")
	must(t, err)
	hexsum := strings.TrimPrefix(d, "sha256:")
	p := filepath.Join(e.stateDir, "blobs", "sha256", hexsum[:2], hexsum[2:4], hexsum)
	must(t, os.Chmod(p, 0o600))
	must(t, os.WriteFile(p, []byte("rot"), 0o600))
	err = ingestContent(e)
	var u *Unsafe
	if errors.As(err, &u) || !errors.Is(err, blobstore.ErrDigestMismatch) {
		t.Fatalf("Ingest onto a corrupt blob = %v, want a store ErrDigestMismatch", err)
	}
}

// A same-size in-place overwrite during the copy: refused as replaced where
// the clock moved, otherwise by the digest; never published.
func TestSameSizeOverwriteDuringCopy(t *testing.T) {
	withFallback(t, func(t *testing.T) {
		e := newEnv(t)
		p := filepath.Join(e.out, "res")
		e.write(p, content)
		setHook(t, func(stage string) {
			if stage == "copying" {
				f, err := os.OpenFile(p, os.O_WRONLY, 0)
				must(t, err)
				_, err = f.WriteAt([]byte("XXXX"), int64(len(content))-4)
				must(t, err)
				must(t, f.Close())
			}
		})
		var u *Unsafe
		if err := ingestContent(e); !errors.As(err, &u) || (u.Check != CheckReplaced && u.Check != CheckDigest) {
			t.Fatalf("Ingest = %v, want replaced or digest", err)
		}
		e.assertEmpty()
	})
}
