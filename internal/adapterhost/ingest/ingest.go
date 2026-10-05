// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Package ingest is the controller side of the blob-exchange ingestion
// boundary (docs/design/m0/09 "Blob exchange: the ingestion boundary"): it
// copies one adapter-written file out of an attempt's out/ directory into the
// blob store, refusing anything but a plain regular file of the declared
// size and digest that stayed the same while it was read.
//
// The caller holds out/ as a directory fd opened O_DIRECTORY|O_NOFOLLOW and
// calls Ingest only after the adapter's process tree has exited (09). The
// file is resolved beneath that fd, inspected through an O_PATH descriptor
// (which never opens a FIFO or device for I/O), reopened read-only, matched
// by device and inode to what was inspected, read exactly once while hashing,
// and stat-checked again before the blob store may publish it.
package ingest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The checks an unsafe output can fail (Unsafe.Check).
const (
	CheckSymlink     = "symlink"      // a symlink, or a name that would leave out/
	CheckSpecialFile = "special_file" // a FIFO, socket, device or directory, or a file on another mount
	CheckHardLink    = "hard_link"    // link count is not 1
	CheckSize        = "size"         // not the declared size, or over the bound
	CheckDigest      = "digest"       // the bytes do not hash to the declared digest
	CheckReplaced    = "replaced"     // replaced or changed between inspection and the end of the copy
	CheckUnreadable  = "unreadable"   // missing, or an I/O error while inspecting or reading
)

// Unsafe is the refusal of an adapter output: failed/unsafe_output. Nothing
// is published when Ingest returns it.
type Unsafe struct {
	Check  string
	Detail string
}

func (u *Unsafe) Error() string {
	return "ingest: " + vocab.ReasonUnsafeOutput.String() + ": " + u.Check + ": " + u.Detail
}

// Reason is the registered reason of the refusal.
func (u *Unsafe) Reason() vocab.Reason { return vocab.ReasonUnsafeOutput }

func unsafeErr(check, format string, args ...any) *Unsafe {
	return &Unsafe{Check: check, Detail: fmt.Sprintf(format, args...)}
}

// handleRe is the handle pattern of schemas/adapter/event.json: one path
// component that cannot be "." or "..".
var handleRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)

var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

const resolveFlags = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV

const (
	pathFlags = unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC
	readFlags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_NOCTTY | unix.O_CLOEXEC
)

// openat2 is unix.Openat2; tests replace it to force the openat fallback.
var openat2 = unix.Openat2

// testHook, when set, is called with "checked" after the inspection and
// before the reopen, and with "copying" after the first read of the copy.
var testHook func(stage string)

// skipCheck, when set, disables a named check; for mutation tests only.
var skipCheck func(check string) bool

func skipped(check string) bool { return skipCheck != nil && skipCheck(check) }

// openBeneath opens name (one validated component) beneath dir: with
// openat2 and every resolve restriction, or with plain openat where openat2
// is missing or filtered (ENOSYS, EPERM). flags always include O_NOFOLLOW.
func openBeneath(dir int, name string, flags int) (int, error) {
	fd, err := openat2(dir, name, &unix.OpenHow{Flags: uint64(flags), Resolve: resolveFlags})
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) {
		return unix.Openat(dir, name, flags, 0)
	}
	return fd, err
}

// classify names the check a file mode fails, or "" for a regular file.
func classify(mode uint32) string {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return ""
	case unix.S_IFLNK:
		return CheckSymlink
	default: // S_IFDIR, S_IFIFO, S_IFSOCK, S_IFCHR, S_IFBLK and anything unknown
		return CheckSpecialFile
	}
}

// inspect applies the type, link-count, mount and size checks to st.
func inspect(st *unix.Stat_t, dirDev uint64, size, bound int64) error {
	if c := classify(st.Mode); c != "" && !skipped(c) {
		return unsafeErr(c, "mode %#o is not a regular file", st.Mode&unix.S_IFMT)
	}
	if st.Nlink != 1 && !skipped(CheckHardLink) {
		return unsafeErr(CheckHardLink, "link count %d, want 1", st.Nlink)
	}
	if st.Dev != dirDev && !skipped(CheckSpecialFile) {
		return unsafeErr(CheckSpecialFile, "on another mount than out/")
	}
	if !skipped(CheckSize) && (st.Size != size || st.Size > bound) {
		return unsafeErr(CheckSize, "size %d, declared %d, bound %d", st.Size, size, bound)
	}
	return nil
}

// Ingest copies the adapter file name, beneath the held out/ directory, into
// store and returns its digest. declared is the adapter's "sha256:<hex>"
// digest, size its declared size and bound the largest size this kind of
// output may have. Any boundary violation is an *Unsafe and publishes
// nothing; other errors (the blob store's own) are returned as they are.
func Ingest(outDir *os.File, name, declared string, size, bound int64, store *blobstore.Store) (string, error) {
	if !handleRe.MatchString(name) {
		return "", unsafeErr(CheckSymlink, "handle %q is not one plain path component", name)
	}
	if !digestRe.MatchString(declared) {
		return "", unsafeErr(CheckDigest, "malformed declared digest %q", declared)
	}
	if size < 0 || size > bound {
		return "", unsafeErr(CheckSize, "declared size %d outside [0, %d]", size, bound)
	}
	dir := int(outDir.Fd())
	var dirSt unix.Stat_t
	if err := unix.Fstat(dir, &dirSt); err != nil {
		return "", unsafeErr(CheckUnreadable, "stat out/: %v", err)
	}

	pfd, err := openBeneath(dir, name, pathFlags)
	switch {
	case errors.Is(err, unix.ELOOP):
		return "", unsafeErr(CheckSymlink, "resolving %q: %v", name, err)
	case errors.Is(err, unix.EXDEV): // a mount point: the name is one validated component, so never ".."
		return "", unsafeErr(CheckSpecialFile, "resolving %q: %v", name, err)
	case err != nil:
		return "", unsafeErr(CheckUnreadable, "resolving %q: %v", name, err)
	}
	defer unix.Close(pfd)
	var before unix.Stat_t
	if err := unix.Fstat(pfd, &before); err != nil {
		return "", unsafeErr(CheckUnreadable, "stat %q: %v", name, err)
	}
	if err := inspect(&before, dirSt.Dev, size, bound); err != nil {
		return "", err
	}
	if testHook != nil {
		testHook("checked")
	}

	rfd, err := openBeneath(dir, name, readFlags)
	if err != nil {
		return "", unsafeErr(CheckReplaced, "reopening %q: %v", name, err)
	}
	f := os.NewFile(uintptr(rfd), name)
	defer f.Close()
	var opened unix.Stat_t
	if err := unix.Fstat(rfd, &opened); err != nil {
		return "", unsafeErr(CheckUnreadable, "stat reopened %q: %v", name, err)
	}
	if (opened.Dev != before.Dev || opened.Ino != before.Ino) && !skipped(CheckReplaced) {
		return "", unsafeErr(CheckReplaced, "%q is another file than the one inspected", name)
	}

	r := &guard{r: io.LimitReader(f, bound+1), fd: rfd, before: &before, size: size}
	got, err := store.Put(r, declared)
	var u *Unsafe
	switch {
	case errors.As(err, &u):
		return "", u
	case errors.Is(err, blobstore.ErrDigestMismatch):
		return "", unsafeErr(CheckDigest, "%v", err)
	case err != nil:
		return "", err
	}
	return got, nil
}

// guard counts what is read and, before reporting the end of the bytes,
// proves the file is the size and age it was when inspected; otherwise it
// fails the read, so the blob store publishes nothing.
type guard struct {
	r      io.Reader
	fd     int
	before *unix.Stat_t
	size   int64
	n      int64
	reads  int
}

func (g *guard) Read(p []byte) (int, error) {
	n, err := g.r.Read(p)
	g.n += int64(n)
	if g.reads++; g.reads == 1 && testHook != nil {
		testHook("copying")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, unsafeErr(CheckUnreadable, "reading: %v", err)
	}
	if err == nil && g.n <= g.size {
		return n, nil
	}
	var after unix.Stat_t
	if serr := unix.Fstat(g.fd, &after); serr != nil {
		return n, unsafeErr(CheckUnreadable, "stat after copy: %v", serr)
	}
	b := g.before
	if (after.Size != b.Size || after.Mtim != b.Mtim || after.Ctim != b.Ctim) && !skipped(CheckReplaced) {
		return n, unsafeErr(CheckReplaced, "changed while it was read")
	}
	if g.n != g.size {
		return n, unsafeErr(CheckSize, "read %d bytes, declared %d", g.n, g.size)
	}
	return n, err
}
