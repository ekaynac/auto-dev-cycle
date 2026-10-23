// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"slices"
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
	remote        slotQuota     // the HTTPS listener's, per remote address (api.Peer.Key)
	freeFailures  int           // failed HTTPS authentications a remote peer has before it is held off (throttle)
	preAuth       time.Duration // an HTTPS connection's bound from accept to a verified bearer (tcpListener)
	preAuthFree   time.Duration // a request-less HTTPS connection that ends sooner, no handshake done, is not a failure (httpsConn.ended)
	poll          time.Duration // the event stream's re-check bound; zero is api.MaxPoll
	shutdown      time.Duration // what a graceful stop waits before closing connections
}

var defaultLimits = limits{headerTimeout: 5 * time.Second, writeTimeout: 30 * time.Second, idleTimeout: time.Minute,
	headerBytes: 16 << 10, bodyBytes: 1 << 20, shutdown: 5 * time.Second,
	private: slotQuota{conns: 64, verify: 2}, shared: slotQuota{conns: 32, connsPerPeer: 8, verify: 2, verifyPerPeer: 1},
	remote: slotQuota{conns: 128, connsPerPeer: 4, verify: 2, verifyPerPeer: 1}, freeFailures: 3, preAuth: 5 * time.Second, preAuthFree: time.Second}

// slotQuota is one listener's concurrent connections (so open streams too:
// each holds one) and bearer verifications (Argon2id, 64 MiB each), in
// total and per peer (0: no per-peer cap).
type slotQuota struct{ conns, connsPerPeer, verify, verifyPerPeer int }

// server is the API served on the daemon's sockets.
type server struct {
	sockets []*socket
	https   []*http.Server
	base    context.Context    // every request's, until stop
	stop    context.CancelFunc // ends every request context: open streams return
	tcp     net.Addr           // the HTTPS listener's bound address; nil when off
	remote  *api.Slots         // the HTTPS listener's bearer verifications
	th      *throttle          // the HTTPS listener's failure throttle
	cmds    commands           // the command endpoint, on every listener
	lim     limits
	wg      sync.WaitGroup
	failed  chan error // a listener that stopped on its own
}

// serve binds the private socket in stateDir and, when configured, the
// shared one, and serves the API over store on both, cmds answering
// commands (and on the HTTPS listener, listenHTTPS). It runs at the
// readiness point only (Run).
func serve(store *eventstore.Store, boot *bootstrapper, stateDir string, shared *SharedSocket, lim limits, cmds commands) (_ *server, err error) {
	s := &server{lim: lim, cmds: cmds, failed: make(chan error, 3)}
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
	s.base, s.stop = context.WithCancel(context.Background())
	// The header bound: MaxHeaderBytes is headerBytes less net/http's
	// 4096-byte allowance, so a request on a fresh connection, or the first
	// of a pipeline, is held to headerBytes. A pipelined request can also
	// arrive partly in the 4096-byte read buffer already filled, so its
	// effective bound is headerBytes + 4096 (20 KiB by default); memory
	// stays bounded either way.
	for _, sk := range s.sockets {
		s.serveOn(sk.ln, handler(store, boot, lim, sk.verify, cmds), nil)
	}
	return s, nil
}

// serveOn serves h on ln until close, under the server's limits.
func (s *server) serveOn(ln net.Listener, h http.Handler, errLog *log.Logger) {
	hs := &http.Server{Handler: h, ReadHeaderTimeout: s.lim.headerTimeout, WriteTimeout: s.lim.writeTimeout,
		IdleTimeout: s.lim.idleTimeout, MaxHeaderBytes: s.lim.headerBytes - 4096, ErrorLog: errLog,
		BaseContext: func(net.Listener) context.Context { return s.base }, ConnContext: connContext, ConnState: connState}
	s.https = append(s.https, hs)
	s.wg.Go(func() {
		if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			s.failed <- err
		}
	})
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
// records which listener admitted it and the peer: its uid on a socket,
// its address over HTTPS.
func connContext(ctx context.Context, c net.Conn) context.Context {
	ctx = api.ConnContext(ctx, c)
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	switch pc := c.(type) {
	case *peerConn:
		ctx = api.WithPeer(ctx, pc.peer)
	case *httpsConn:
		ctx = context.WithValue(api.WithPeer(ctx, pc.peer), httpsConnKey{}, pc)
	}
	return ctx
}

// bootstrapRoute is served only where a bootstrapper is: the private socket.
const bootstrapRoute = "POST " + apidoc.BootstrapPath

// routeList is every pattern the daemon serves, in registration order.
var routeList = []string{bootstrapRoute, "GET " + api.EventsPath, "GET " + api.VersionPath,
	"POST " + CommandsPath, "GET " + api.WhoamiPath, "GET " + api.ReadPath, "POST " + api.BlobsPath, "GET " + api.BlobsPath,
	"POST " + api.SandboxProbePath}

// Routes are the patterns the daemon serves (the parity harness,
// internal/parity, checks each against the API description).
func Routes() []string { return slices.Clone(routeList) }

// routes is the API's mux over routeList: every pattern has exactly one
// handler (the bootstrap's only with boot), and any mismatch is a
// programming error.
func routes(store *eventstore.Store, boot *bootstrapper, lim limits, gate *api.Slots, cmds commands) *http.ServeMux {
	cmds.gate = gate
	reads := api.Reads{Store: store, Gate: gate, Resources: api.Registered(), Stalled: cmds.stalled}
	blobs := api.Blobs{Store: store, Blobs: cmds.blobs, Gate: gate, Now: cmds.clock, Log: cmds.log}
	served := map[string]http.Handler{
		"GET " + api.EventsPath:  api.Events{Store: store, Poll: lim.poll, Gate: gate},
		"GET " + api.VersionPath: http.HandlerFunc(api.Version),
		"POST " + CommandsPath:   cmds,
		"GET " + api.WhoamiPath:  http.HandlerFunc(reads.Whoami),
		"GET " + api.ReadPath:    http.HandlerFunc(reads.Resource),
		"POST " + api.BlobsPath:  blobs,
		"GET " + api.BlobsPath:   blobs,
	}
	served["POST "+api.SandboxProbePath] = api.SandboxProbe{Store: store, Gate: gate, Host: cmds.probe, Log: cmds.log}
	if boot != nil {
		served[bootstrapRoute] = boot.handler(gate)
	}
	mux := http.NewServeMux()
	for _, p := range routeList {
		h, ok := served[p]
		if !ok && p != bootstrapRoute {
			panic("daemon: route " + p + " has no handler")
		}
		if ok {
			mux.Handle(p, h)
			delete(served, p)
		}
	}
	if len(served) != 0 {
		panic("daemon: a handler without a route in routeList")
	}
	return mux
}

// handler is the API's routes, every request body bounded. Commands
// answer through the wired actions' logic.
func handler(store *eventstore.Store, boot *bootstrapper, lim limits, gate *api.Slots, cmds commands) http.Handler {
	mux := routes(store, boot, lim, gate, cmds)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The blobs route bounds its own body (blobstore.MaxUploadBytes),
		// read up to the bound before it is refused, so a client that sends
		// no more than one byte past it (adc) reads the refusal rather than
		// a broken pipe.
		if r.URL.Path != api.BlobsPath {
			if r.ContentLength > lim.bodyBytes {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, lim.bodyBytes)
		}
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
