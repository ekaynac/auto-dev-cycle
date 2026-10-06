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
// name is resolved exactly once, beneath that fd, to an O_PATH descriptor
// (which never opens a FIFO or device for I/O). Everything after that works
// on the pinned inode: it is inspected, reopened for reading through
// /proc/self/fd (never through the name again), read once while hashing, and
// inspected again before the blob store may publish it.
package ingest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The checks an unsafe output can fail (Unsafe.Check).
const (
	CheckPath        = "path"         // the handle is not one plain path component
	CheckSymlink     = "symlink"      // a symlink
	CheckSpecialFile = "special_file" // a FIFO, socket, device or directory, or a file on another mount
	CheckHardLink    = "hard_link"    // link count is not 1
	CheckSize        = "size"         // not the declared size, or over the bound
	CheckDigest      = "digest"       // the bytes do not hash to the declared digest
	CheckReplaced    = "replaced"     // unlinked or changed between inspection and the end of the copy
	CheckUnreadable  = "unreadable"   // missing, not readable, or an I/O error
)

// ErrUnsupported: this host cannot verify the boundary (statx reports no
// mount id, or /proc/self/fd is unavailable). It is a host capability gap,
// never the adapter's fault and never an *Unsafe: the host refuses to start
// attempts (could_not_run) and does not mark one untrustworthy for it.
// Supported probes for it before an attempt starts.
var ErrUnsupported = errors.New("ingest: host cannot verify the output boundary")

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

const (
	resolveFlags = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV
	pathFlags    = unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC
	// The reopen goes through the magic link of the pinned O_PATH fd, so it
	// must follow that link: no O_NOFOLLOW.
	readFlags = unix.O_RDONLY | unix.O_NOCTTY | unix.O_CLOEXEC
	statxWant = unix.STATX_TYPE | unix.STATX_NLINK | unix.STATX_INO | unix.STATX_SIZE | unix.STATX_MTIME | unix.STATX_CTIME
)

// Seams for tests: openat2 to force the openat fallback, statx to withhold
// the mount id, procFD to take /proc away.
var (
	openat2 = unix.Openat2
	statx   = unix.Statx
	procFD  = "/proc/self/fd/"
)

// testHook, when set, is called with "checked" after the inspection and
// before the reopen, and with "copying" after the first read of the copy.
var testHook func(stage string)

// skipCheck, when set, disables a named check; for mutation tests only.
var skipCheck func(check string) bool

func skipped(check string) bool { return skipCheck != nil && skipCheck(check) }

// openBeneath opens name (one validated component) beneath dir as O_PATH:
// with openat2 and every resolve restriction, or with plain openat where
// openat2 is missing or filtered (ENOSYS, EPERM). The fallback's missing
// RESOLVE_NO_XDEV is made up by the mount-id comparison.
func openBeneath(dir int, name string) (int, error) {
	fd, err := openat2(dir, name, &unix.OpenHow{Flags: pathFlags, Resolve: resolveFlags})
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) {
		return unix.Openat(dir, name, pathFlags, 0)
	}
	return fd, err
}

// meta is what the checks compare, from statx on a descriptor.
type meta struct {
	mode         uint16
	nlink        uint32
	size         int64
	dev, ino     uint64
	mnt          uint64
	mtime, ctime unix.StatxTimestamp
}

// stat reads fd's meta. A kernel or filesystem that does not report the
// mount id (or any other compared field) fails closed with ErrUnsupported.
func stat(fd int, what string) (meta, error) {
	var x unix.Statx_t
	err := statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, statxWant|unix.STATX_MNT_ID|unix.STATX_MNT_ID_UNIQUE, &x)
	if err != nil {
		return meta{}, unsafeErr(CheckUnreadable, "stat %s: %v", what, err)
	}
	if x.Mask&statxWant != statxWant || x.Mask&(unix.STATX_MNT_ID|unix.STATX_MNT_ID_UNIQUE) == 0 {
		return meta{}, fmt.Errorf("%w: stat %s: mount id or another field not reported (mask %#x)", ErrUnsupported, what, x.Mask)
	}
	return meta{mode: x.Mode, nlink: x.Nlink, size: int64(x.Size), dev: unix.Mkdev(x.Dev_major, x.Dev_minor),
		ino: x.Ino, mnt: x.Mnt_id, mtime: x.Mtime, ctime: x.Ctime}, nil
}

// Supported reports whether this host can run Ingest's checks: statx
// reports mount ids and /proc/self/fd reopens a pinned descriptor. It
// returns an error wrapping ErrUnsupported when it cannot.
func Supported() error {
	fd, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%w: open /: %v", ErrUnsupported, err)
	}
	defer unix.Close(fd)
	if _, err := stat(fd, "/"); err != nil {
		if errors.Is(err, ErrUnsupported) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	rfd, err := reopen(fd, unix.O_DIRECTORY)
	if err != nil {
		return err
	}
	return unix.Close(rfd)
}

// reopen opens the inode pinned by the O_PATH descriptor pfd for reading,
// through its /proc/self/fd magic link, never through a name. A missing
// /proc is ErrUnsupported; any other failure is the file's (unreadable).
func reopen(pfd, extra int) (int, error) {
	rfd, err := unix.Open(procFD+strconv.Itoa(pfd), readFlags|extra, 0)
	switch {
	case errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENOTDIR):
		return -1, fmt.Errorf("%w: reopen through %s: %v", ErrUnsupported, procFD, err)
	case err != nil:
		return -1, unsafeErr(CheckUnreadable, "reopening: %v", err)
	}
	return rfd, nil
}

// asReplaced reports a check that held at inspection and fails later (on
// the reopened descriptor or after the copy) as replaced.
func asReplaced(err error) error {
	var u *Unsafe
	if errors.As(err, &u) && !skipped(CheckReplaced) {
		return unsafeErr(CheckReplaced, "%s after inspection: %s", u.Check, u.Detail)
	}
	return err
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

// sameMount refuses a descriptor whose mount is not out/'s. A bind mount of
// an outside file on the same filesystem has out/'s st_dev but its own mount.
func sameMount(m meta, dirMnt uint64, what string) error {
	if m.mnt != dirMnt && !skipped(CheckSpecialFile) {
		return unsafeErr(CheckSpecialFile, "%s is on mount %d, out/ on %d", what, m.mnt, dirMnt)
	}
	return nil
}

// links refuses a link count other than 1: 0 is an unlinked (replaced) file,
// more is a hard link, possibly to a file outside out/.
func links(m meta) error {
	switch {
	case m.nlink == 0 && !skipped(CheckReplaced):
		return unsafeErr(CheckReplaced, "unlinked after inspection")
	case m.nlink > 1 && !skipped(CheckHardLink):
		return unsafeErr(CheckHardLink, "link count %d, want 1", m.nlink)
	}
	return nil
}

// unchanged refuses a file whose size or times moved since inspection.
func unchanged(before, now meta) error {
	if (now.size != before.size || now.mtime != before.mtime || now.ctime != before.ctime) && !skipped(CheckReplaced) {
		return unsafeErr(CheckReplaced, "changed after inspection")
	}
	return nil
}

// Ingest copies the adapter file name, beneath the held out/ directory, into
// store and returns its digest. declared is the adapter's "sha256:<hex>"
// digest, size its declared size and bound the largest size this kind of
// output may have. Any boundary violation is an *Unsafe and publishes
// nothing. A host that cannot verify the boundary gets ErrUnsupported (see
// Supported). Other errors (the blob store's own, including a corrupt
// stored blob) are returned as they are. A check that held at inspection and
// fails on the reopened descriptor or after the copy is Check replaced.
//
// What the checks prove: the bytes published are the bytes of the one inode
// the name resolved to, a regular file on out/'s mount with link count 1 at
// inspection, at the reopen and at the end of the copy, and they hash to the
// declared digest. What they do not prove: change detection by size, mtime
// and ctime is only as fine as the filesystem clock, so a same-size
// in-place write, or a rename of the file out of out/ and back, within one
// clock tick is not seen. The digest is then the backstop for the content
// (the copied stream hashes to what the adapter declared), but it is not a
// snapshot: it says nothing about the file's state between the reads.
func Ingest(outDir *os.File, name, declared string, size, bound int64, store *blobstore.Store) (string, error) {
	if !handleRe.MatchString(name) {
		return "", unsafeErr(CheckPath, "handle %q is not one plain path component", name)
	}
	if !digestRe.MatchString(declared) {
		return "", unsafeErr(CheckDigest, "malformed declared digest %q", declared)
	}
	if size < 0 || size > bound {
		return "", unsafeErr(CheckSize, "declared size %d outside [0, %d]", size, bound)
	}
	dirMeta, err := stat(int(outDir.Fd()), "out/")
	if err != nil {
		return "", err
	}

	pfd, err := openBeneath(int(outDir.Fd()), name)
	switch {
	case errors.Is(err, unix.ELOOP):
		return "", unsafeErr(CheckSymlink, "resolving %q: %v", name, err)
	case errors.Is(err, unix.EXDEV): // a mount point: the name is one validated component, so never ".."
		return "", unsafeErr(CheckSpecialFile, "resolving %q: %v", name, err)
	case err != nil:
		return "", unsafeErr(CheckUnreadable, "resolving %q: %v", name, err)
	}
	defer unix.Close(pfd)
	before, err := stat(pfd, name)
	if err != nil {
		return "", err
	}
	if c := classify(uint32(before.mode)); c != "" && !skipped(c) {
		return "", unsafeErr(c, "mode %#o is not a regular file", before.mode&unix.S_IFMT)
	}
	if err := errors.Join(links(before), sameMount(before, dirMeta.mnt, name)); err != nil {
		return "", firstUnsafe(err)
	}
	if !skipped(CheckSize) && before.size != size {
		return "", unsafeErr(CheckSize, "size %d, declared %d", before.size, size)
	}
	if testHook != nil {
		testHook("checked")
	}

	// The pinned inode is a regular file, so this open cannot reach a FIFO
	// or device whatever now sits at the name.
	rfd, err := reopen(pfd, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(rfd), name)
	defer f.Close()
	opened, err := stat(rfd, name+" (reopened)")
	if err != nil {
		return "", err
	}
	if (opened.dev != before.dev || opened.ino != before.ino) && !skipped(CheckReplaced) {
		return "", unsafeErr(CheckReplaced, "%q reopened as another file than the one inspected", name)
	}
	if err := errors.Join(links(opened), sameMount(opened, dirMeta.mnt, name+" (reopened)"), unchanged(before, opened)); err != nil {
		return "", asReplaced(firstUnsafe(err))
	}

	r := &guard{r: io.LimitReader(f, bound+1), fd: rfd, before: before, size: size}
	got, err := store.Put(r, declared)
	var u *Unsafe
	switch {
	case errors.As(err, &u):
		return "", u
	case errors.Is(err, blobstore.ErrDeclaredDigest):
		return "", unsafeErr(CheckDigest, "%v", err)
	case err != nil:
		return "", err
	}
	return got, nil
}

// firstUnsafe returns the first *Unsafe joined in err.
func firstUnsafe(err error) error {
	var u *Unsafe
	if errors.As(err, &u) {
		return u
	}
	return err
}

// guard counts what is read and, before reporting the end of the bytes,
// proves the file still has one link and the size and times it had when
// inspected; otherwise it fails the read, so the blob store publishes nothing.
type guard struct {
	r      io.Reader
	fd     int
	before meta
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
	after, serr := stat(g.fd, "after the copy")
	if serr != nil {
		return n, serr
	}
	if cerr := errors.Join(links(after), unchanged(g.before, after)); cerr != nil {
		return n, asReplaced(firstUnsafe(cerr))
	}
	if g.n != g.size {
		return n, unsafeErr(CheckSize, "read %d bytes, declared %d", g.n, g.size)
	}
	return n, err
}
