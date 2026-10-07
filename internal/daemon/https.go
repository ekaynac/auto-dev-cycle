// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"crypto/sha256"
	"crypto/subtle"
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
	"sync"
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
// opens: each a regular file of at most 1 MiB (no link followed, no FIFO
// waited on), the key mode 0600 and owned by uid; the pair must match and
// the leaf be valid at now. Each refusal is its own fixed text.
func (h HTTPSListener) load(now time.Time, uid int) (*tls.Config, error) {
	certPEM, err := readPEM(h.CertFile, func(unix.Stat_t) bool { return true })
	if err != nil {
		return nil, configError("the HTTPS certificate " + err.Error())
	}
	keyPEM, err := readPEM(h.KeyFile, func(st unix.Stat_t) bool { return st.Mode&0o777 == 0o600 && int(st.Uid) == uid })
	if err != nil {
		return nil, configError("the HTTPS key " + err.Error())
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

// fileRefusal is readPEM's refusal: fixed text, never the path or an OS
// error that would name it.
type fileRefusal string

func (f fileRefusal) Error() string { return string(f) }

const (
	refusedOpen fileRefusal = "cannot be opened or read"
	refusedType fileRefusal = "is not a regular file"
	refusedSize fileRefusal = "is larger than 1 MiB"
	refusedMode fileRefusal = "must be mode 0600 and owned by the daemon's user"
)

func readPEM(path string, ok func(unix.Stat_t) bool) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, refusedOpen
	}
	defer f.Close()
	var st unix.Stat_t
	switch {
	case unix.Fstat(int(f.Fd()), &st) != nil:
		return nil, refusedOpen
	case st.Mode&unix.S_IFMT != unix.S_IFREG:
		return nil, refusedType
	case !ok(st):
		return nil, refusedMode
	}
	// One buffer, never grown, so clearing what this returns clears the
	// only copy of the key's bytes (a growing read leaves earlier ones on
	// the heap); a refusal clears it here.
	b := make([]byte, maxPEM+1)
	n, err := io.ReadFull(f, b)
	switch {
	case err == nil:
		clear(b)
		return nil, refusedSize
	case !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF):
		clear(b)
		return nil, refusedOpen
	}
	return b[:n], nil
}

// listenHTTPS binds addr at the readiness point and serves the API on it
// over tc, with its own connection and verification slots.
func (s *server) listenHTTPS(store *eventstore.Store, addr string, tc *tls.Config) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return configError("the HTTPS address cannot be bound")
	}
	q := s.lim.remote
	s.tcp, s.remote, s.th = ln.Addr(), api.NewSlots(q.verify, q.verifyPerPeer), newThrottle(s.lim.freeFailures)
	tl := tls.NewListener(tcpListener{Listener: ln, conns: api.NewSlots(q.conns, q.connsPerPeer), preAuth: s.lim.preAuth, preAuthFree: s.lim.preAuthFree, th: s.th}, tc)
	// A failed handshake would log the remote address: nothing to log.
	s.serve(tl, httpsHandler(store, s.lim, s.remote, s.th), log.New(io.Discard, "", 0))
	return nil
}

// tcpListener admits a connection while its remote address is not held
// off (th) and has a slot; any other is closed before a byte is read, so a
// held-off peer costs no TLS handshake and takes no slot. An admitted
// connection that has not verified a bearer within preAuth of its accept
// is closed, whatever stage it is at (handshake, headers, verification):
// net/http resets its own deadlines at each stage, so they alone would let
// a caller with no bearer hold the slot for their sum. A connection that
// ends without a request ever reaching the handler counts as a failure
// (th) when it completed its TLS handshake, held its slot for preAuthFree
// or reached that bound (httpsConn.ended), so a peer that only reconnects
// to hold slots is held off like one that guesses.
type tcpListener struct {
	net.Listener
	conns       *api.Slots
	preAuth     time.Duration
	preAuthFree time.Duration
	th          *throttle
}

func (l tcpListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		// The peer's address, unmapped as Key would; none has no key but a
		// shared one, so it is refused.
		if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok && ta.AddrPort().Addr().IsValid() {
			peer := api.Peer{Via: identity.HTTPS, UID: -1, Addr: ta.AddrPort().Addr().Unmap()}
			if l.th.wait(peer.Key()) > 0 {
				_ = c.Close()
				continue
			}
			if release, ok := l.conns.Take(peer.Key()); ok {
				hc := &httpsConn{Conn: c, peer: peer, release: release, th: l.th, since: time.Now(), free: l.preAuthFree}
				hc.mu.Lock() // expire must see the timer it stops
				hc.expiry = time.AfterFunc(l.preAuth, hc.expire)
				hc.mu.Unlock()
				return hc, nil
			}
		}
		_ = c.Close()
	}
}

// httpsConn is an admitted TCP connection. Its first byte must open a TLS
// handshake record; anything else (plaintext HTTP above all) fails the
// read, so the connection closes with nothing written: net/http would
// otherwise answer it with a plaintext 400. It remembers the bearer it
// verified (as a SHA-256 digest) and its principal.
type httpsConn struct {
	net.Conn
	peer    api.Peer
	release func()        // its connection slot
	th      *throttle     // what an end with no request counts against
	since   time.Time     // its accept
	free    time.Duration // how long it may hold its slot, no handshake done, and end without a failure
	started bool
	mu      sync.Mutex
	digest  [sha256.Size]byte
	who     identity.Principal // the zero Principal until a bearer is verified
	asked   bool               // a request on it reached the handler, whose answer counts any failure
	expired bool               // it reached the pre-auth bound
	counted bool               // its end was judged (ended): a failure counts once
	expiry  *time.Timer        // the pre-auth bound: a no-op once a bearer is verified
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
	c.mu.Lock()
	c.expiry.Stop()
	c.mu.Unlock()
	c.release()
	return c.Conn.Close()
}

// ended judges a connection that is over, once: if no request on it
// reached the handler (which counts its own refusals, does not blame a
// bearer still being verified, and verifies every bearer), and it completed its
// TLS handshake or held its slot for free (or to the pre-auth bound), that
// is a failure for its peer. Only a connection that closed within free
// without completing a handshake (a TCP health check, a failed handshake)
// is not.
func (c *httpsConn) ended(handshook bool) {
	c.mu.Lock()
	fail := !c.counted && !c.asked && (handshook || c.expired || time.Since(c.since) >= c.free)
	c.counted = true
	c.mu.Unlock()
	if fail {
		c.th.fail(c.peer.Key())
	}
}

// connState is for http.Server.ConnState: it judges each HTTPS connection
// when net/http is done with it (httpsConn.ended). The handshake state is
// read only then, on net/http's own goroutine, so no handshake can hold
// it.
func connState(c net.Conn, state http.ConnState) {
	tc, ok := c.(*tls.Conn)
	if !ok || state != http.StateClosed {
		return
	}
	if hc, ok := tc.NetConn().(*httpsConn); ok {
		hc.ended(tc.ConnectionState().HandshakeComplete)
	}
}

// expire closes the connection if no bearer has been verified on it.
func (c *httpsConn) expire() {
	c.mu.Lock()
	authed := c.who.Actor() != ""
	c.expired = !authed
	c.mu.Unlock()
	if !authed {
		_ = c.Close()
	}
}

// ask records that a request on the connection reached the handler.
func (c *httpsConn) ask() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = true
}

// verified is the principal whose bearer this connection verified, if
// bearer is that one.
func (c *httpsConn) verified(bearer string) (identity.Principal, bool) {
	d := sha256.Sum256([]byte(bearer))
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.who, c.who.Actor() != "" && subtle.ConstantTimeCompare(d[:], c.digest[:]) == 1
}

func (c *httpsConn) remember(bearer string, who identity.Principal) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.digest, c.who = sha256.Sum256([]byte(bearer)), who
}

type httpsConnKey struct{}

// httpsHandler is the API on the HTTPS listener. Every request's bearer is
// authenticated before any route (authenticate), so nothing, not even a
// 404, answers a caller without one. A peer whose bearers keep failing is
// held off (throttle) before any verification. The routes get the verified
// principal (api.WithPrincipal), so they recheck it rather than hash the
// bearer again. Bootstrap is not served.
// Responses go through guard: no redirect, no caching, and a refusal
// closes the connection.
func httpsHandler(store *eventstore.Store, lim limits, gate *api.Slots, th *throttle) http.Handler {
	routes := handler(store, nil, lim, gate)
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w := &guard{ResponseWriter: rw}
		w.Header().Set("Cache-Control", "no-store")
		hc, _ := r.Context().Value(httpsConnKey{}).(*httpsConn)
		if hc == nil { // not admitted by tcpListener: authenticates nothing
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		hc.ask()
		key := hc.peer.Key()
		defer func() {
			if w.status == http.StatusUnauthorized {
				th.fail(key)
			}
		}()
		if wait := th.wait(key); wait > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
			http.Error(w, "too many failed authentications; retry later", http.StatusTooManyRequests)
			return
		}
		bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		code, who := http.StatusUnauthorized, identity.Principal{}
		if ok && bearer != "" {
			who, code = authenticate(r, hc, store, gate, bearer)
		}
		if code != 0 {
			http.Error(w, http.StatusText(code), code)
			return
		}
		routes.ServeHTTP(w, r.WithContext(api.WithPrincipal(r.Context(), who)))
	})
}

// authenticate checks bearer on hc: the bearer hc already verified is
// rechecked for revocation without hashing (Principal.Live); any other is
// verified in full (Argon2id) through gate. Its principal and 0 when it
// names a live actor, else the refusal's status; any store error refuses.
func authenticate(r *http.Request, hc *httpsConn, store *eventstore.Store, gate *api.Slots, bearer string) (identity.Principal, int) {
	who, known := hc.verified(bearer)
	release := func() {}
	if !known {
		var ok bool
		if release, ok = gate.Take(hc.peer.Key()); !ok {
			return who, http.StatusServiceUnavailable
		}
	}
	defer release()
	err := store.View(r.Context(), func(tx *eventstore.Tx) error {
		var err error
		if !known {
			who, err = identity.Authenticated(tx, bearer, hc.peer.Via)
		}
		if err == nil {
			_, err = who.Live(tx)
		}
		return err
	})
	switch {
	case err == nil:
		hc.remember(bearer, who)
		return who, 0
	case errors.Is(err, identity.ErrUnauthenticated):
		return who, http.StatusUnauthorized
	}
	return who, http.StatusServiceUnavailable
}

// guard is every HTTPS response. A redirect (net/http's ServeMux issues
// them to clean a path, and a client following one resends its bearer)
// becomes a bare 404 with no Location and no body. A refusal (401, 429,
// 503) closes the connection, and a 401 names the Bearer scheme.
type guard struct {
	http.ResponseWriter
	status  int
	dropped bool
}

func (w *guard) WriteHeader(code int) {
	if w.status == 0 {
		h := w.Header()
		if code >= 300 && code < 400 {
			h.Del("Location")
			w.dropped, code = true, http.StatusNotFound
		}
		switch code {
		case http.StatusUnauthorized:
			h.Set("WWW-Authenticate", "Bearer")
			h.Set("Connection", "close")
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			h.Set("Connection", "close")
		}
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *guard) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.dropped {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection (api.Events).
func (w *guard) Unwrap() http.ResponseWriter { return w.ResponseWriter }
