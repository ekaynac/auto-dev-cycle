// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// HTTPSListener enables the HTTPS listener (08 "Transport"): bearer clients
// over TLS 1.3 on a TCP address. Off by default; an explicit one that is
// invalid refuses the start (14 R2). Clients verify the certificate against
// a CA or a pinned fingerprint of their own configuration (14 R3); there is
// no plaintext listener and no insecure mode.
type HTTPSListener struct {
	Addr     string // IP:PORT with an IP literal (0.0.0.0 or [::] for every interface)
	CertFile string // absolute and clean: the PEM certificate chain, leaf first
	KeyFile  string // absolute and clean: its PEM private key, mode 0600, the daemon uid's
}

const maxPEM = 1 << 20 // bytes

func (h HTTPSListener) validate(absClean func(string) bool) error {
	host, port, err := net.SplitHostPort(h.Addr)
	n, perr := strconv.Atoi(port)
	_, aerr := netip.ParseAddr(host)
	switch {
	case h.Addr == "" || h.CertFile == "" || h.KeyFile == "":
		return configError("the HTTPS listener needs --https-addr, --https-cert and --https-key")
	case err != nil || perr != nil || aerr != nil || n < 1 || n > 65535:
		return configError("--https-addr is not IP:PORT with an IP literal and a port from 1 to 65535")
	case !absClean(h.CertFile) || !absClean(h.KeyFile):
		return configError("the HTTPS certificate and key must be absolute, clean paths")
	}
	return nil
}

// load reads the certificate and the key at startup, before the store
// opens: each a regular file (no link followed, no FIFO waited on), the
// key mode 0600 and owned by uid; the pair must match and the leaf be
// valid at now. Its refusals are fixed text.
func (h HTTPSListener) load(now time.Time, uid int) (*tls.Config, error) {
	certPEM, err := readPEM(h.CertFile, func(unix.Stat_t) bool { return true })
	if err != nil {
		return nil, configError("the HTTPS certificate is not a readable regular file of at most 1 MiB")
	}
	keyPEM, err := readPEM(h.KeyFile, func(st unix.Stat_t) bool { return st.Mode&0o777 == 0o600 && int(st.Uid) == uid })
	if err != nil {
		return nil, configError("the HTTPS key must be a readable regular file of at most 1 MiB, mode 0600, owned by the daemon's user")
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	clear(keyPEM)
	if err != nil {
		return nil, configError("the HTTPS certificate does not parse or does not match the key")
	}
	if leaf, err := x509.ParseCertificate(pair.Certificate[0]); err != nil || now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return nil, configError("the HTTPS certificate is expired or not yet valid")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, NextProtos: []string{"http/1.1"}}, nil
}

func readPEM(path string, ok func(unix.Stat_t) bool) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || !ok(st) {
		return nil, errors.New("daemon: file refused")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxPEM+1))
	if err == nil && len(b) > maxPEM {
		err = errors.New("daemon: file too large")
	}
	return b, err
}

// listenHTTPS binds addr at the readiness point and serves the API on it
// over tc, with its own connection and verification slots.
func (s *server) listenHTTPS(store *eventstore.Store, addr string, tc *tls.Config) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return configError("the HTTPS address cannot be bound")
	}
	q := s.lim.remote
	s.tcp, s.remote = ln.Addr(), api.NewSlots(q.verify, q.verifyPerPeer)
	tl := tls.NewListener(tcpListener{Listener: ln, conns: api.NewSlots(q.conns, q.connsPerPeer)}, tc)
	// A failed handshake would log the remote address: nothing to log.
	s.serve(tl, httpsHandler(store, s.lim, s.remote), log.New(io.Discard, "", 0))
	return nil
}

// tcpListener admits a connection while its remote address has a slot; any
// other is closed before a byte is read.
type tcpListener struct {
	net.Listener
	conns *api.Slots
}

func (l tcpListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok {
			peer := api.Peer{Via: identity.HTTPS, UID: -1, Addr: ta.AddrPort().Addr().Unmap()}
			if release, ok := l.conns.Take(peer.Key()); ok {
				return &httpsConn{Conn: c, peer: peer, release: release}, nil
			}
		}
		_ = c.Close()
	}
}

// httpsConn is an admitted TCP connection. Its first byte must open a TLS
// handshake record; anything else (plaintext HTTP above all) fails the
// read, so the connection closes with nothing written: net/http would
// otherwise answer it with a plaintext 400.
type httpsConn struct {
	net.Conn
	peer    api.Peer
	release func() // its connection slot
	started bool
}

const tlsHandshakeRecord = 0x16

var errNotTLS = errors.New("daemon: the connection does not open with a TLS handshake")

// Read is called only by the TLS layer, which serializes its reads.
func (c *httpsConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if !c.started && n > 0 {
		c.started = true
		if b[0] != tlsHandshakeRecord {
			return 0, errNotTLS
		}
	}
	return n, err
}

func (c *httpsConn) Close() error {
	c.release()
	return c.Conn.Close()
}

// httpsHandler is the API on the HTTPS listener. Every request needs a
// bearer: one without is refused before any route, and the version route
// (unauthenticated on the sockets) verifies it here; the routes that read
// verify it themselves. Bootstrap is not served. No response redirects (a
// client following one would resend its bearer) and none may be cached.
func httpsHandler(store *eventstore.Store, lim limits, gate *api.Slots) http.Handler {
	routes := handler(store, nil, lim, gate)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		code := http.StatusUnauthorized
		if ok && bearer != "" {
			code = 0
			if r.URL.Path == api.VersionPath {
				code = verifyBearer(r, store, gate, bearer)
			}
		}
		if code != 0 {
			http.Error(w, http.StatusText(code), code)
			return
		}
		routes.ServeHTTP(&noRedirect{ResponseWriter: w}, r)
	})
}

// verifyBearer authenticates bearer on r's listener through gate: 0 when it
// names a live actor, else the refusal's status.
func verifyBearer(r *http.Request, store *eventstore.Store, gate *api.Slots, bearer string) int {
	peer, _ := api.PeerOf(r.Context()) // none recorded: no listener, which authenticates nothing
	release, ok := gate.Take(peer.Key())
	if !ok {
		return http.StatusServiceUnavailable
	}
	defer release()
	err := store.View(r.Context(), func(tx *eventstore.Tx) error {
		p, err := identity.Authenticated(tx, bearer, peer.Via)
		if err == nil {
			_, err = p.Live(tx)
		}
		return err
	})
	switch {
	case err == nil:
		return 0
	case errors.Is(err, identity.ErrUnauthenticated):
		return http.StatusUnauthorized
	}
	return http.StatusServiceUnavailable
}

// noRedirect turns any redirect (net/http's ServeMux issues them to clean
// a path) into a bare 404 with no Location and no body.
type noRedirect struct {
	http.ResponseWriter
	dropped bool
}

func (w *noRedirect) WriteHeader(code int) {
	if code >= 300 && code < 400 {
		w.Header().Del("Location")
		w.dropped, code = true, http.StatusNotFound
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *noRedirect) Write(b []byte) (int, error) {
	if w.dropped {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection (api.Events).
func (w *noRedirect) Unwrap() http.ResponseWriter { return w.ResponseWriter }
