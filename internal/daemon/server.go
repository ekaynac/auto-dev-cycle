// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/apidoc"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// limits bound what one client can hold of the server.
type limits struct {
	headerTimeout time.Duration // to read a request's headers
	writeTimeout  time.Duration // to write a response; the event stream replaces it per frame (api.Events)
	idleTimeout   time.Duration // a kept-alive connection between requests
	headerBytes   int           // request line and headers; see the header bound at listen
	bodyBytes     int64         // a request body
	private       slotQuota     // the private socket's own slots: the shared one never starves it
	shared        slotQuota
	poll          time.Duration // the event stream's re-check bound; zero is api.MaxPoll
	shutdown      time.Duration // what a graceful stop waits before closing connections
}

var defaultLimits = limits{headerTimeout: 5 * time.Second, writeTimeout: 30 * time.Second, idleTimeout: time.Minute,
	headerBytes: 16 << 10, bodyBytes: 1 << 20, shutdown: 5 * time.Second,
	private: slotQuota{conns: 64, verify: 2}, shared: slotQuota{conns: 32, connsPerPeer: 8, verify: 2, verifyPerPeer: 1}}

// slotQuota is one listener's concurrent connections (so open streams too:
// each holds one) and bearer verifications (Argon2id, 64 MiB each), in
// total and per peer uid (0: no per-peer cap).
type slotQuota struct{ conns, connsPerPeer, verify, verifyPerPeer int }

// server is the API served on the daemon's sockets.
type server struct {
	sockets []*socket
	https   []*http.Server
	stop    context.CancelFunc // ends every request context: open streams return
	lim     limits
	wg      sync.WaitGroup
	failed  chan error // a listener that stopped on its own
}

// listen binds the private socket in stateDir and, when configured, the
// shared one, and serves the API over store on both. It runs at the
// readiness point only (Run).
func listen(store *eventstore.Store, boot *bootstrapper, stateDir string, shared *SharedSocket, lim limits) (_ *server, err error) {
	s := &server{lim: lim, failed: make(chan error, 2)}
	defer func() {
		if err != nil {
			_ = s.close() // the refusal is err
		}
	}()
	dir, err := openDir(stateDir, func(st unix.Stat_t) bool { return st.Mode&0o777 == 0o700 })
	if errors.Is(err, errUnknownFS) {
		return nil, configError(fsRefusal)
	} else if err != nil {
		return nil, configError("the state directory must be a directory owned by this user with mode 0700")
	}
	if err := s.add(bind(dir, privateSocket, 0o600, -1, identity.PrivateSocket, lim.private)); err != nil {
		return nil, err
	}
	if shared != nil {
		gid, err := shared.gid()
		if err != nil {
			return nil, err
		}
		dir, err := openDir(shared.RunDir, func(st unix.Stat_t) bool {
			return st.Mode&0o022 == 0 && st.Mode&0o010 != 0 && int(st.Gid) == gid
		})
		if errors.Is(err, errUnknownFS) {
			return nil, configError(fsRefusal)
		} else if err != nil {
			return nil, configError("the shared run directory must be a directory owned by this user, of the shared group, group-searchable and writable by no one else")
		}
		if err := s.add(bind(dir, sharedSocket, 0o660, gid, identity.SharedSocket, lim.shared)); err != nil {
			return nil, err
		}
	}
	base, stop := context.WithCancel(context.Background())
	s.stop = stop
	// The header bound: MaxHeaderBytes is headerBytes less net/http's
	// 4096-byte allowance, so a request on a fresh connection, or the first
	// of a pipeline, is held to headerBytes. A pipelined request can also
	// arrive partly in the 4096-byte read buffer already filled, so its
	// effective bound is headerBytes + 4096 (20 KiB by default); memory
	// stays bounded either way.
	for _, sk := range s.sockets {
		hs := &http.Server{Handler: handler(store, boot, lim, sk.verify), ReadHeaderTimeout: lim.headerTimeout, WriteTimeout: lim.writeTimeout,
			IdleTimeout: lim.idleTimeout, MaxHeaderBytes: lim.headerBytes - 4096,
			BaseContext: func(net.Listener) context.Context { return base }, ConnContext: connContext}
		s.https = append(s.https, hs)
		s.wg.Go(func() {
			if err := hs.Serve(sk.ln); !errors.Is(err, http.ErrServerClosed) {
				s.failed <- err
			}
		})
	}
	return s, nil
}

func (s *server) add(sk *socket, err error) error {
	if err == nil {
		s.sockets = append(s.sockets, sk)
	}
	return err
}

// aclFilesystems are the filesystems whose ACL semantics are known: POSIX
// ACLs only, readable as the system.posix_acl_* attributes, so a missing
// attribute (ENODATA, ENOTSUP) means mode bits alone decide. Any other
// (NFS, CIFS/SMB, FUSE, overlay, unknown) may enforce ACLs these checks
// cannot read, and is refused (14 R2).
var aclFilesystems = map[int64]string{
	unix.EXT4_SUPER_MAGIC:  "ext2/ext3/ext4",
	unix.XFS_SUPER_MAGIC:   "xfs",
	unix.BTRFS_SUPER_MAGIC: "btrfs",
	unix.TMPFS_MAGIC:       "tmpfs",
}

const fsRefusal = "the state and run directories must be on ext2/ext3/ext4, xfs, btrfs or tmpfs, whose ACL semantics are known"

var errUnknownFS = errors.New("daemon: a filesystem whose ACL semantics are unknown")

// fsType is the filesystem type of fd (statfs f_type); a test replaces it.
var fsType = func(fd int) (int64, error) {
	var st unix.Statfs_t
	err := unix.Fstatfs(fd, &st)
	return int64(st.Type), err
}

// openDir opens path as a directory (no link followed) that this user
// owns, on a filesystem in aclFilesystems, with no default ACL and no
// access ACL beyond its mode bits (so g+x is the group's, not an ACL
// mask), and that ok accepts, all checked on the opened descriptor.
func openDir(path string, ok func(unix.Stat_t) bool) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if t, err := fsType(fd); err != nil || aclFilesystems[t] == "" {
		f.Close()
		return nil, errUnknownFS
	}
	var st unix.Stat_t
	def, derr := aclAttr(f, "", aclDefault) // a default ACL would widen what is created in it
	acc, aerr := aclAttr(f, "", aclAccess)
	if err := unix.Fstat(fd, &st); err != nil || derr != nil || aerr != nil || len(def) > 0 || len(acc) > minimalACL ||
		int(st.Uid) != os.Geteuid() || !ok(st) {
		f.Close()
		return nil, errors.New("daemon: directory refused")
	}
	return f, nil
}

// connContext registers the connection for api.Events' hard cutoff and
// records which listener admitted it and the peer's uid.
func connContext(ctx context.Context, c net.Conn) context.Context {
	ctx = api.ConnContext(ctx, c)
	if pc, ok := c.(*peerConn); ok {
		ctx = api.WithPeer(ctx, pc.peer)
	}
	return ctx
}

// handler is the API's routes, every request body bounded. Command routes
// join with their handlers.
func handler(store *eventstore.Store, boot *bootstrapper, lim limits, gate *api.Slots) http.Handler {
	mux := http.NewServeMux()
	if boot != nil {
		mux.Handle("POST "+apidoc.BootstrapPath, boot.handler(gate))
	}
	mux.Handle("GET "+api.EventsPath, api.Events{Store: store, Poll: lim.poll, Gate: gate})
	mux.HandleFunc("GET "+api.VersionPath, api.Version)
	reads := api.Reads{Store: store, Gate: gate, Resources: api.Registered()}
	mux.HandleFunc("GET "+api.WhoamiPath, reads.Whoami)
	mux.HandleFunc("GET "+api.ReadPath, reads.Resource)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > lim.bodyBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, lim.bodyBytes)
		mux.ServeHTTP(w, r)
	})
}

// close stops the server: request contexts end (streams return), the
// servers stop accepting and drain within the shutdown bound, then
// connections still open are closed, and the sockets are removed.
func (s *server) close() error {
	if s.stop != nil {
		s.stop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.lim.shutdown)
	defer cancel()
	var errs []error
	for _, hs := range s.https {
		if err := hs.Shutdown(ctx); err != nil {
			errs = append(errs, hs.Close())
		}
	}
	s.wg.Wait()
	for _, sk := range s.sockets {
		errs = append(errs, sk.close())
	}
	return errors.Join(errs...)
}
