// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// The sockets' names (08 "Transport"): the private one in the state
// directory, the shared one in the configured run directory.
const (
	privateSocket = "adcd.sock"
	sharedSocket  = "adcd-shared.sock"
)

// socket is a Unix socket this daemon bound. Its directory is held open
// from the check to the removal: the socket is bound, inspected and
// removed through that descriptor (as /proc/self/fd/N/name), never by a
// path someone could have swapped in between.
type socket struct {
	dir     *os.File
	name    string
	dev     uint64
	ino     uint64
	ln      net.Listener
	verify  *api.Slots // bearer verifications on this socket
	removed sync.Once
}

// umask is process-wide: binds take turns setting it, and they run at the
// readiness point, before anything else in the daemon creates files.
var umask sync.Mutex

func (s *socket) path() string { return procPath(s.dir, s.name) }

func procPath(dir *os.File, name string) string {
	return "/proc/self/fd/" + strconv.Itoa(int(dir.Fd())) + "/" + name
}

// bind binds name in dir with perm set at creation by the umask, gives it
// group gid (when gid >= 0) before it listens, and listens on it for via.
// It takes ownership of dir. An existing path is replaced only if it is a
// stale socket of this daemon's uid (clearStale). q is its own quota.
func bind(dir *os.File, name string, perm uint32, gid int, via identity.Listener, q slotQuota) (_ *socket, err error) {
	s := &socket{dir: dir, name: name, verify: api.NewSlots(q.verify, q.verifyPerPeer)}
	defer func() {
		if err != nil {
			_ = s.close() // a refused bind removes what it made; the refusal says why
		}
	}()
	if err := clearStale(dir, name, os.Geteuid()); err != nil {
		return nil, err
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, configError("a Unix socket cannot be created")
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	umask.Lock()
	old := unix.Umask(int(^perm & 0o777))
	err = unix.Bind(fd, &unix.SockaddrUnix{Name: s.path()})
	unix.Umask(old)
	umask.Unlock()
	if err != nil {
		return nil, configError("the socket cannot be bound in its directory")
	}
	var st unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, configError("the bound socket cannot be inspected")
	}
	s.dev, s.ino = st.Dev, st.Ino
	// An ACL inherited from the directory would widen the mode (14 R2).
	if acl, err := aclAttr(dir, name, aclAccess); err != nil || len(acl) > minimalACL {
		return nil, configError("the socket carries an ACL beyond its mode bits")
	}
	if gid >= 0 && unix.Fchownat(int(dir.Fd()), name, -1, gid, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return nil, configError("the shared socket cannot be given its group; the daemon must be a member of it")
	}
	if err := unix.Listen(fd, unix.SOMAXCONN); err != nil {
		return nil, configError("the socket cannot listen")
	}
	ln, err := net.FileListener(f)
	if err != nil {
		return nil, configError("the socket cannot listen")
	}
	s.ln = peerListener{Listener: ln, via: via, daemon: os.Geteuid(), conns: api.NewSlots(q.conns, q.connsPerPeer)}
	return s, nil
}

// clearStale makes way for a socket at name in dir. Nothing there: done.
// A stream socket of the daemon's uid on which a connect is refused
// (ECONNREFUSED: no one listens) is stale and is removed, after its type,
// owner and inode are checked again. Anything else (another type, another
// owner, a link, a live server, a full backlog, a datagram socket, any
// other dial error) is refused and left untouched.
func clearStale(dir *os.File, name string, daemon int) error {
	var st unix.Stat_t
	err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || !staleCandidate(st, daemon) {
		return configError("the socket path holds something that is not a socket of this user's; it is left in place")
	}
	c, err := net.DialTimeout("unix", procPath(dir, name), time.Second)
	if err == nil {
		_ = c.Close()
	}
	if !errors.Is(err, unix.ECONNREFUSED) {
		return configError("the socket path is not provably stale (a server answers, or the connect failed otherwise); it is left in place")
	}
	var again unix.Stat_t
	if unix.Fstatat(int(dir.Fd()), name, &again, unix.AT_SYMLINK_NOFOLLOW) != nil || !staleCandidate(again, daemon) ||
		again.Dev != st.Dev || again.Ino != st.Ino || unix.Unlinkat(int(dir.Fd()), name, 0) != nil {
		return configError("a stale socket cannot be removed")
	}
	return nil
}

// The ACL extended attributes, and the size of an ACL that only mirrors
// the mode bits: a 4-byte header and three 8-byte entries (owner, group,
// other).
const (
	aclAccess  = "system.posix_acl_access"
	aclDefault = "system.posix_acl_default"
	minimalACL = 4 + 3*8
)

// aclAttr reads attr of dir itself (name "") or of name in it; none is
// empty. A test replaces it.
var aclAttr = func(dir *os.File, name, attr string) ([]byte, error) {
	buf := make([]byte, 4096)
	var n int
	var err error
	if name == "" {
		n, err = unix.Fgetxattr(int(dir.Fd()), attr, buf)
	} else {
		n, err = unix.Lgetxattr(procPath(dir, name), attr, buf)
	}
	if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// staleCandidate: only a socket owned by uid may be replaced.
func staleCandidate(st unix.Stat_t, uid int) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFSOCK && int(st.Uid) == uid
}

// close stops listening and removes the socket, once, if the path still
// holds the very socket this daemon bound; then it releases the directory.
func (s *socket) close() error {
	var err error
	s.removed.Do(func() {
		if s.ln != nil {
			err = s.ln.Close()
			if errors.Is(err, net.ErrClosed) {
				err = nil
			}
		}
		var st unix.Stat_t
		fd := int(s.dir.Fd())
		if s.ino != 0 && unix.Fstatat(fd, s.name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Dev == s.dev && st.Ino == s.ino {
			err = errors.Join(err, unix.Unlinkat(fd, s.name, 0))
		}
		err = errors.Join(err, s.dir.Close())
	})
	return err
}

// peerListener admits a connection only if its peer credentials (taken by
// the kernel at connect, SO_PEERCRED) pass admits; any other is closed
// before a byte is read. One past the connection quota is refused with
// fixed text and closed. The connection carries the listener and the
// peer's uid to the request context (connContext).
type peerListener struct {
	net.Listener
	via    identity.Listener
	daemon int        // the daemon's uid
	conns  *api.Slots // released when the connection closes
}

// busy is the fixed-text refusal of a connection past the quota.
const busy = "HTTP/1.1 503 Service Unavailable\r\nContent-Type: text/plain\r\nRetry-After: 1\r\nConnection: close\r\nContent-Length: 21\r\n\r\ntoo many connections\n"

func (l peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uc, ok := c.(*net.UnixConn)
		if ok {
			if uid, err := peerUID(uc); err == nil && admits(l.via, uid, l.daemon) {
				if release, ok := l.conns.Take(uid); ok {
					return &peerConn{UnixConn: uc, peer: api.Peer{Via: l.via, UID: uid}, release: release}, nil
				}
				_ = uc.SetWriteDeadline(time.Now().Add(time.Second))
				_, _ = uc.Write([]byte(busy)) // refused either way
			}
		}
		_ = c.Close() // refused: the client sees the connection close
	}
}

// admits is M3: the private socket admits only the daemon's uid; the
// shared socket admits whom its group and mode let connect. Either way
// every request still needs a bearer.
func admits(via identity.Listener, peer, daemon int) bool {
	switch via {
	case identity.PrivateSocket:
		return peer == daemon
	case identity.SharedSocket:
		return true
	}
	return false
}

func peerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return -1, err
	}
	var cred *unix.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) { cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return -1, err
	}
	if cerr != nil {
		return -1, cerr
	}
	return int(cred.Uid), nil
}

// peerConn is an admitted connection and who it came from.
type peerConn struct {
	*net.UnixConn
	peer    api.Peer
	release func() // its connection slot
}

func (c *peerConn) Close() error {
	c.release()
	return c.UnixConn.Close()
}
