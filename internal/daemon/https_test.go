// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/apidoc"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// testCert is a self-signed certificate for 127.0.0.1, generated per test:
// no key is ever committed.
type testCert struct {
	certPEM, keyPEM []byte
	pool            *x509.CertPool
}

func newCert(t *testing.T, notBefore, notAfter time.Time) testCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "adcd"}, NotBefore: notBefore, NotAfter: notAfter,
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return testCert{pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}), pool}
}

func validCert(t *testing.T) testCert {
	return newCert(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
}

// files writes the pair to a fresh directory: the key with keyMode.
func (c testCert) files(t *testing.T, keyMode os.FileMode) (cert, key string) {
	t.Helper()
	dir := t.TempDir()
	cert, key = filepath.Join(dir, "tls-cert-name.pem"), filepath.Join(dir, "tls-key-name.pem")
	if err := os.WriteFile(cert, c.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, c.keyPEM, 0o600); err != nil || os.Chmod(key, keyMode) != nil {
		t.Fatal(err)
	}
	return cert, key
}

// tlsLimits are the limits the HTTPS tests serve with.
func tlsLimits() limits { return defaultLimits }

// serveTLS is the assembled server with the HTTPS listener on loopback.
func serveTLS(t *testing.T, lim limits) (*served, *http.Client, string, testCert) {
	t.Helper()
	s, c := serveStore(t, lim, nil), validCert(t)
	cert, key := c.files(t, 0o600)
	tc, err := HTTPSListener{Addr: "127.0.0.1:1", CertFile: cert, KeyFile: key}.load(time.Now(), os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.srv.listenHTTPS(s.w.s, "127.0.0.1:0", tc); err != nil {
		t.Fatal(err)
	}
	return s, tlsClient(t, c, "127.0.0.1"), "https://" + s.srv.tcp.String(), c
}

// tlsClient trusts c and connects from the loopback address from, so a
// test can be two remote peers.
func tlsClient(t *testing.T, c testCert, from string) *http.Client {
	d := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(from)}}
	client := &http.Client{Transport: &http.Transport{DialContext: d.DialContext, TLSClientConfig: &tls.Config{RootCAs: c.pool, MinVersion: tls.VersionTLS13}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func httpsDo(t *testing.T, c *http.Client, method, url, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// httpsStatus is the status of GET url, the body read and closed.
func httpsStatus(t *testing.T, c *http.Client, url, bearer string) int {
	t.Helper()
	resp := httpsDo(t, c, http.MethodGet, url, bearer)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// Off by default: no flag, no listener.
func TestHTTPSOffByDefault(t *testing.T) {
	if cfg, err := LoadConfig([]string{"--state-dir", stateDir(t)}); err != nil || cfg.HTTPS != nil {
		t.Fatalf("%+v %v", cfg.HTTPS, err)
	}
	if s := serveStore(t, defaultLimits, nil); s.srv.tcp != nil || len(s.srv.https) != 1 {
		t.Fatalf("a TCP listener without configuration: %v", s.srv.tcp)
	}
}

// An explicit HTTPS configuration that is invalid refuses the start with
// its own fixed text, before the state directory is touched; adcd run
// exits 64.
func TestHTTPSConfigRefused(t *testing.T) {
	c := validCert(t)
	cert, key := c.files(t, 0o600)
	_, loose := c.files(t, 0o640)
	_, otherKey := validCert(t).files(t, 0o600)
	expired, expiredKey := newCert(t, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)).files(t, 0o600)
	early, earlyKey := newCert(t, time.Now().Add(time.Hour), time.Now().Add(2*time.Hour)).files(t, 0o600)
	dir := t.TempDir()
	link, fifo, garbage := filepath.Join(dir, "tls-link-name.pem"), filepath.Join(dir, "tls-fifo-name.pem"), filepath.Join(dir, "tls-garbage-name.pem")
	big := filepath.Join(dir, "tls-big-name.pem")
	certDir := filepath.Join(dir, "tls-dir-name")
	if os.Symlink(key, link) != nil || unix.Mkfifo(fifo, 0o600) != nil || os.WriteFile(garbage, []byte("not pem"), 0o600) != nil ||
		os.WriteFile(big, append(c.keyPEM, make([]byte, maxPEM)...), 0o600) != nil || os.Mkdir(certDir, 0o700) != nil {
		t.Fatal("fixtures")
	}
	const addr = "127.0.0.1:18443"
	pair := func(a, c, k string) []string { return []string{"--https-addr", a, "--https-cert", c, "--https-key", k} }
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"address only":   {[]string{"--https-addr", addr}, "needs --https-addr, --https-cert and --https-key"},
		"host name":      {pair("localhost:18443", cert, key), "an IP literal and a port from 1 to 65535"},
		"port zero":      {pair("127.0.0.1:0", cert, key), "an IP literal and a port from 1 to 65535"},
		"port 65536":     {pair("127.0.0.1:65536", cert, key), "an IP literal and a port from 1 to 65535"},
		"no port":        {pair("127.0.0.1", cert, key), "an IP literal and a port from 1 to 65535"},
		"relative cert":  {pair(addr, "cert.pem", key), "absolute, clean paths"},
		"missing cert":   {pair(addr, cert+".absent", key), "certificate cannot be opened or read"},
		"cert directory": {pair(addr, certDir, key), "certificate is not a regular file"},
		"cert over 1MiB": {pair(addr, big, key), "certificate is larger than 1 MiB"},
		"garbage cert":   {pair(addr, garbage, key), "does not parse or does not match the key"},
		"key over 1MiB":  {pair(addr, cert, big), "key is larger than 1 MiB"},
		"key mode 0640":  {pair(addr, cert, loose), "key must be mode 0600"},
		"key symlink":    {pair(addr, cert, link), "key cannot be opened or read"},
		"key fifo":       {pair(addr, cert, fifo), "key is not a regular file"},
		"mismatched key": {pair(addr, cert, otherKey), "does not parse or does not match the key"},
		"expired cert":   {pair(addr, expired, expiredKey), "expired or not yet valid"},
		"not yet valid":  {pair(addr, early, earlyKey), "expired or not yet valid"},
	} {
		// Run as RunCommand does, bounded: a guard that let a case through
		// starts a daemon, which the deadline stops and the test reports.
		state := stateDir(t)
		cfg, err := LoadConfig(append([]string{"--state-dir", state}, tc.args...))
		if err == nil {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			err = Run(ctx, cfg, io.Discard, nil)
			cancel()
		}
		if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "-name") || strings.Contains(err.Error(), state) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the state directory was touched: %v", name, err)
		}
	}
	var stderr bytes.Buffer
	state := stateDir(t)
	code := RunCommand(append([]string{"--state-dir", state}, pair(addr, cert, otherKey)...), &stderr)
	if out := stderr.String(); code != entry.ExitUsage || !strings.HasPrefix(out, ErrConfig.Error()) || strings.Contains(out, "-name") || strings.Contains(out, state) {
		t.Errorf("adcd run with a mismatched key: exit %d: %q", code, out)
	}
	// Run validates a configuration it is handed before it reads a file:
	// a relative path is refused as one, not opened against the cwd.
	state = stateDir(t)
	err := Run(t.Context(), Config{StateDir: state, HTTPS: &HTTPSListener{Addr: addr, CertFile: "absent-cert.pem", KeyFile: key}}, io.Discard, nil)
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "absolute, clean paths") {
		t.Errorf("Run with a relative certificate path: %v", err)
	}
	if _, err := (HTTPSListener{Addr: addr, CertFile: cert, KeyFile: key}).load(time.Now(), os.Geteuid()+1); !errors.Is(err, ErrConfig) ||
		!strings.Contains(err.Error(), "key must be mode 0600 and owned by the daemon's user") {
		t.Fatalf("a key of another uid: %v", err)
	}
}

// readPEM reads into one buffer it never grows, so the caller's clear
// reaches the only copy of the key; a file of exactly 1 MiB is read whole
// and one byte more is refused.
func TestReadPEMOneBuffer(t *testing.T) {
	dir := t.TempDir()
	anyFile := func(unix.Stat_t) bool { return true }
	for name, size := range map[string]int{"small": 300, "exactly 1 MiB": maxPEM, "empty": 0} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		want := bytes.Repeat([]byte{'k'}, size)
		if os.WriteFile(path, want, 0o600) != nil {
			t.Fatal("fixture")
		}
		b, err := readPEM(path, anyFile)
		if err != nil || !bytes.Equal(b, want) || cap(b) != maxPEM+1 {
			t.Errorf("%s: err %v, %d bytes, equal %v, cap %d; want one buffer of %d", name, err, len(b), bytes.Equal(b, want), cap(b), maxPEM+1)
		}
	}
	over := filepath.Join(dir, "over")
	if os.WriteFile(over, make([]byte, maxPEM+1), 0o600) != nil {
		t.Fatal("fixture")
	}
	if b, err := readPEM(over, anyFile); !errors.Is(err, refusedSize) || b != nil {
		t.Errorf("1 MiB + 1: %v, %d bytes", err, len(b))
	}
}

// An address that cannot be bound refuses the start after readiness, and
// the store and the lock are released.
func TestHTTPSBindRefused(t *testing.T) {
	cert, key := validCert(t).files(t, 0o600)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	state := stateDir(t)
	err = Run(t.Context(), Config{StateDir: state, HTTPS: &HTTPSListener{Addr: taken.Addr().String(), CertFile: cert, KeyFile: key}}, io.Discard, nil)
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "the HTTPS address cannot be bound") {
		t.Fatalf("a taken address: %v", err)
	}
	lockFree(t, state)
	gone(t, filepath.Join(state, privateSocket))
}

// Run binds the listener after readiness, serves TLS 1.3 only, and closes
// it at shutdown.
func TestHTTPSRunAndShutdown(t *testing.T) {
	c := validCert(t)
	cert, key := c.files(t, 0o600)
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()
	ctx, cancel := context.WithCancel(t.Context())
	logs, done, ready := &logBuffer{}, make(chan error, 1), make(chan *Daemon, 1)
	cfg := Config{StateDir: stateDir(t), HTTPS: &HTTPSListener{Addr: addr, CertFile: cert, KeyFile: key}}
	go func() { done <- Run(ctx, cfg, logs, func(d *Daemon) { ready <- d }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("the daemon did not start: %v", err)
	}
	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: c.pool, NextProtos: []string{"h2", "http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if st := conn.ConnectionState(); st.Version != tls.VersionTLS13 || st.NegotiatedProtocol != "http/1.1" {
		t.Errorf("negotiated %x %q", st.Version, st.NegotiatedProtocol)
	}
	conn.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		t.Error("the HTTPS listener outlived shutdown")
	}
	if out := logs.String(); !strings.Contains(out, "the HTTPS listener is open") || strings.Contains(out, cert) {
		t.Errorf("log: %q", out)
	}
}

// A TLS 1.2 client is refused at the handshake; plaintext HTTP gets no
// response at all; neither is logged (a failed handshake names the remote
// address); a connection that sends nothing is closed at the header bound.
func TestHTTPSRefusesOldTLSAndPlaintext(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged) // where net/http logs when a server has no ErrorLog
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	lim := tlsLimits()
	lim.headerTimeout = 300 * time.Millisecond
	s, _, _, c := serveTLS(t, lim)
	addr := s.srv.tcp.String()
	if conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: c.pool, MaxVersion: tls.VersionTLS12}); err == nil {
		conn.Close()
		t.Fatal("a TLS 1.2 client was served")
	}
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := io.WriteString(raw, "GET "+api.VersionPath+" HTTP/1.1\r\nHost: adcd\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	raw.SetReadDeadline(time.Now().Add(5 * time.Second))
	if got, _ := io.ReadAll(raw); len(got) != 0 {
		t.Fatalf("plaintext HTTP was answered: %q", got)
	}
	silent, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: c.pool})
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	silent.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	if _, err := silent.Read(make([]byte, 1)); err == nil || time.Since(start) > 3*time.Second {
		t.Errorf("a silent connection: %v after %s", err, time.Since(start))
	}
	time.Sleep(200 * time.Millisecond) // the server logs, if it does, as it closes
	if logged.Len() != 0 {
		t.Errorf("the server logged: %q", logged.String())
	}
}

// Every request's bearer is verified before any route: none, a malformed
// one or a wrong one is 401 on every registered route, on bootstrap and on
// an unknown path, and the refusal closes the connection. Nothing is
// redirected or cacheable, and bootstrap is not served.
func TestHTTPSBearerRequired(t *testing.T) {
	s, c, base, _ := serveTLS(t, tlsLimits())
	id, secret, _ := strings.Cut(s.tok.Reveal(), ".")
	wrong := id + "." + map[bool]string{true: "B", false: "A"}[secret[0] == 'A'] + secret[1:]
	routes := map[string]string{api.VersionPath: http.MethodGet, api.WhoamiPath: http.MethodGet, api.EventsPath: http.MethodGet,
		apidoc.BootstrapPath: http.MethodPost, "/nowhere": http.MethodGet}
	for name := range api.Registered() {
		routes["/v1/"+name] = http.MethodGet
	}
	for path, method := range routes {
		for _, bearer := range []string{"", "adc_malformed", wrong} {
			resp := httpsDo(t, c, method, base+path, bearer)
			if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Cache-Control") != "no-store" || !resp.Close {
				t.Errorf("%s %s with bearer %q: %s %v", method, path, bearer, resp.Status, resp.Header)
			}
		}
	}
	if code := httpsStatus(t, c, base+api.VersionPath, s.tok.Reveal()); code != http.StatusOK {
		t.Errorf("version with a bearer: %d", code)
	}
	resp := httpsDo(t, c, http.MethodGet, base+api.WhoamiPath, s.tok.Reveal())
	var got types.Whoami
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != http.StatusOK || got.Listener != types.WhoamiListenerHttps ||
		got.Actor != s.op || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("whoami over HTTPS: %s %+v %v", resp.Status, got, err)
	}
	resp = httpsDo(t, c, http.MethodGet, base+"/"+api.VersionPath, s.tok.Reveal()) // ServeMux would redirect it
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusNotFound || resp.Header.Get("Location") != "" || len(b) != 0 {
		t.Errorf("an unclean path: %s %q %q", resp.Status, resp.Header.Get("Location"), b)
	}
	if resp := httpsDo(t, c, http.MethodPost, base+apidoc.BootstrapPath, s.tok.Reveal()); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("bootstrap over HTTPS: %s", resp.Status)
	}
}

// A 401 closes the connection: the server reads nothing more on it.
func TestHTTPSRefusalClosesConnection(t *testing.T) {
	s, _, _, c := serveTLS(t, tlsLimits())
	conn, err := tls.Dial("tcp", s.srv.tcp.String(), &tls.Config{RootCAs: c.pool})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "GET "+api.WhoamiPath+" HTTP/1.1\r\nHost: adcd\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || !resp.Close {
		t.Fatalf("%v %v", resp, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if rest, err := io.ReadAll(br); err != nil || len(rest) != 0 {
		t.Fatalf("after a 401: %q %v", rest, err)
	}
}

// A connection that has not verified a bearer is closed at the pre-auth
// bound, though net/http's own (header) bound is longer; one that has
// verified is kept past it.
func TestHTTPSPreAuthBound(t *testing.T) {
	lim := tlsLimits()
	lim.preAuth, lim.headerTimeout = 2*time.Second, 10*time.Second // a verification fits in the bound, even under -race
	s, c, base, cert := serveTLS(t, lim)
	silent, err := tls.Dial("tcp", s.srv.tcp.String(), &tls.Config{RootCAs: cert.pool})
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	silent.SetReadDeadline(time.Now().Add(9 * time.Second))
	start := time.Now()
	if _, err := silent.Read(make([]byte, 1)); err == nil || time.Since(start) > 6*time.Second {
		t.Errorf("an unauthenticated connection: %v after %s", err, time.Since(start))
	}
	var conns int
	trace := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) {
		if !i.Reused {
			conns++
		}
	}}
	for range 2 {
		req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace), http.MethodGet, base+api.VersionPath, nil)
		req.Header.Set("Authorization", "Bearer "+s.tok.Reveal())
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("version: %s", resp.Status)
		}
		time.Sleep(2500 * time.Millisecond) // past the bound
	}
	if conns != 1 {
		t.Errorf("a verified connection was closed at the pre-auth bound: %d connections", conns)
	}
}

// A request that no HTTPS connection admitted authenticates nothing.
func TestHTTPSHandlerNeedsConnection(t *testing.T) {
	s := serveStore(t, defaultLimits, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, api.VersionPath, nil)
	req.Header.Set("Authorization", "Bearer "+s.tok.Reveal())
	httpsHandler(s.w.s, defaultLimits, api.NewSlots(2, 1)).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no connection: %d", rec.Code)
	}
}

// The version route refuses when it cannot verify: a full gate, or a
// store it cannot read. Either refusal closes the connection.
func TestHTTPSVersionFailsClosed(t *testing.T) {
	s, c, base, _ := serveTLS(t, tlsLimits())
	r1, _ := s.srv.remote.Take(api.Peer{Addr: netip.MustParseAddr("127.0.0.1")}.Key())
	if resp := httpsDo(t, c, http.MethodGet, base+api.VersionPath, s.tok.Reveal()); resp.StatusCode != http.StatusServiceUnavailable || !resp.Close {
		t.Errorf("version through a full gate: %s close=%v", resp.Status, resp.Close)
	}
	r1()
	s.w.do(s.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return nil, tx.Put(identity.TokenProjection, s.tokenID(), json.RawMessage(`[]`))
	})
	if code := httpsStatus(t, c, base+api.VersionPath, s.tok.Reveal()); code != http.StatusServiceUnavailable {
		t.Errorf("version over an unreadable store: %d", code)
	}
}

// Every way a connection ends gives its slot back: a failed handshake,
// plaintext, a 401 and the pre-auth bound. With one slot, a verified
// request still gets through after each.
func TestHTTPSSlotReleasedOnEveryEnd(t *testing.T) {
	lim := tlsLimits()
	lim.remote = slotQuota{conns: 1, connsPerPeer: 1, verify: 1, verifyPerPeer: 1}
	lim.preAuth = 2 * time.Second // a verification fits in it, even under -race
	s, _, base, cert := serveTLS(t, lim)
	addr := s.srv.tcp.String()
	// answered retries until bearer gets want: the server closes its side
	// of an earlier connection after the client does.
	answered := func(what, bearer string, want int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			c := tlsClient(t, cert, "127.0.0.1")
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, base+api.VersionPath, nil)
			req.Header.Set("Authorization", "Bearer "+bearer)
			resp, err := c.Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				c.CloseIdleConnections()
				if resp.StatusCode == want {
					return
				}
				err = errors.New(resp.Status)
			}
			if time.Now().After(deadline) {
				t.Fatalf("after %s: the slot was not given back: %v", what, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	served := func(what string) { t.Helper(); answered(what, s.tok.Reveal(), http.StatusOK) }
	served("nothing")
	if conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: cert.pool, MaxVersion: tls.VersionTLS12}); err == nil {
		conn.Close()
	}
	served("a failed handshake")
	if raw, err := net.Dial("tcp", addr); err == nil {
		_, _ = io.WriteString(raw, "GET / HTTP/1.1\r\nHost: adcd\r\n\r\n")
		_, _ = io.ReadAll(raw)
		raw.Close()
	}
	served("plaintext")
	answered("plaintext", "adc_wrong.secret", http.StatusUnauthorized)
	served("a 401")
	silent, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: cert.pool})
	if err == nil {
		_, _ = silent.Read(make([]byte, 1)) // until the pre-auth bound closes it
		silent.Close()
	}
	served("the pre-auth bound")
}

// A connection without a remote address has no key of its own, so the
// listener refuses it rather than count it under a shared one.
func TestHTTPSAcceptNeedsAddress(t *testing.T) {
	ln := &addrListener{conns: make(chan net.Conn, 2)}
	server, client := net.Pipe()
	defer client.Close()
	ln.conns <- addrConn{Conn: server, remote: &net.TCPAddr{}}
	valid, other := net.Pipe()
	defer other.Close()
	ln.conns <- addrConn{Conn: valid, remote: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 7), Port: 1}}
	close(ln.conns)
	l := tcpListener{Listener: ln, conns: api.NewSlots(4, 4), preAuth: time.Minute}
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if hc := c.(*httpsConn); hc.peer.Addr != netip.MustParseAddr("192.0.2.7") {
		t.Fatalf("admitted %v", hc.peer.Addr)
	}
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the addressless connection: %v", err)
	}
}

type addrListener struct {
	net.Listener
	conns chan net.Conn
}

func (l *addrListener) Accept() (net.Conn, error) {
	if c, ok := <-l.conns; ok {
		return c, nil
	}
	return nil, net.ErrClosed
}

type addrConn struct {
	net.Conn
	remote net.Addr
}

func (c addrConn) RemoteAddr() net.Addr { return c.remote }

// The event stream over HTTPS: served through the response guard, closed
// on revocation.
func TestHTTPSStream(t *testing.T) {
	lim := tlsLimits()
	lim.poll = 100 * time.Millisecond
	s, c, base, _ := serveTLS(t, lim)
	resp := httpsDo(t, c, http.MethodGet, base+api.EventsPath, s.tok.Reveal())
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events over HTTPS: %s", resp.Status)
	}
	stream := frames(resp)
	s.w.do(s.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeToken(tx, types.TokenID(s.tokenID()), time.Now())
	})
	closedWith(t, stream, vocab.Failed, vocab.ReasonAuthorityRevoked)
}

// A stalled reader over TLS is cut off like one on a socket. Loopback TCP
// buffers grow to about 10 MB (tcp_wmem plus tcp_rmem), so the stream is
// larger than that: the server's writes must block within the stall. A
// server slowed by -race may still not fill them in time; there, api's
// TestStreamCutsOffAStalledTLSClient (over a pipe) carries the cutoff
// beneath TLS alone.
func TestHTTPSStalledReaderCutOff(t *testing.T) {
	if raceEnabled {
		t.Skip("buffer-timed: see the comment")
	}
	lim := tlsLimits()
	lim.poll = 200 * time.Millisecond
	s, c, base, _ := serveTLS(t, lim)
	const total = 15000 // about 13 MB of frames
	s.fill(t, total)
	resp := httpsDo(t, c, http.MethodGet, base+api.EventsPath, s.tok.Reveal())
	time.Sleep(2 * time.Second) // stalled: nothing read
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(resp.Body); done <- string(b) }()
	select {
	case body := <-done:
		if n := strings.Count(body, "event: event"); n >= total || strings.Contains(body, "event: closed") {
			t.Errorf("a stalled reader got %d events and closed=%v", n, strings.Contains(body, "event: closed"))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stalled connection is still open")
	}
}

// The HTTPS listener's slots are its own, with a per-address share below
// the total: one address held to its share leaves the rest to another,
// and neither listener's flood touches the other.
func TestHTTPSPoolsIsolated(t *testing.T) {
	lim := tlsLimits()
	lim.remote = slotQuota{conns: 4, connsPerPeer: 2, verify: 2, verifyPerPeer: 1}
	s, c, base, cert := serveTLS(t, lim)
	other := tlsClient(t, cert, "127.0.0.2")
	// A route verifies under the address too, never a shared uid: with the
	// uid -1 share taken, a verified connection still reads.
	if code := httpsStatus(t, c, base+api.WhoamiPath, s.tok.Reveal()); code != http.StatusOK {
		t.Fatalf("whoami: %d", code)
	}
	rx, _ := s.srv.remote.Take(-1)
	if code := httpsStatus(t, c, base+api.WhoamiPath, s.tok.Reveal()); code != http.StatusOK {
		t.Errorf("a verified connection beside a uid's full share: %d", code)
	}
	rx()
	c.CloseIdleConnections()
	// Verification: this address's one slot taken, the other address's free.
	r1, ok := s.srv.remote.Take(api.Peer{Addr: netip.MustParseAddr("127.0.0.1")}.Key())
	if !ok {
		t.Fatal("no slot")
	}
	if code := httpsStatus(t, c, base+api.WhoamiPath, s.tok.Reveal()); code != http.StatusServiceUnavailable {
		t.Errorf("an address past its verification share: %d", code)
	}
	if code := httpsStatus(t, other, base+api.WhoamiPath, s.tok.Reveal()); code != http.StatusOK {
		t.Errorf("another address beside it: %d", code)
	}
	r2, _ := s.srv.remote.Take(-7)
	c3 := tlsClient(t, cert, "127.0.0.3")
	if code := httpsStatus(t, c3, base+api.WhoamiPath, s.tok.Reveal()); code != http.StatusServiceUnavailable {
		t.Errorf("a full HTTPS gate: %d", code)
	}
	if resp := s.get(t, s.client(), api.WhoamiPath, s.tok.Reveal()); resp.StatusCode != http.StatusOK {
		t.Errorf("the private socket under an HTTPS flood: %s", resp.Status)
	}
	r1()
	r2()
	var held []func()
	for range defaultLimits.private.verify {
		release, ok := s.srv.sockets[0].verify.Take(os.Geteuid())
		if !ok {
			t.Fatal("the private gate is smaller than its quota")
		}
		held = append(held, release)
	}
	c4 := tlsClient(t, cert, "127.0.0.4")
	if code := httpsStatus(t, c4, base+api.WhoamiPath, s.tok.Reveal()); code != http.StatusOK {
		t.Errorf("HTTPS under a private flood: %d", code)
	}
	for _, release := range held {
		release()
	}
	for _, client := range []*http.Client{c, other, c3, c4} {
		client.CloseIdleConnections()
	}
	// Connections: this address's two, then one past them is closed
	// unserved, while another address still connects.
	dial := func(from string) error {
		d := &net.Dialer{Timeout: 5 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(from)}}
		conn, err := tls.DialWithDialer(d, "tcp", s.srv.tcp.String(), &tls.Config{RootCAs: cert.pool})
		if err == nil {
			t.Cleanup(func() { conn.Close() })
		}
		return err
	}
	deadline := time.Now().Add(5 * time.Second) // closed connections free their slots as the server sees them close
	for n := 0; n < 2; {
		if err := dial("127.0.0.1"); err == nil {
			n++
		} else if time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
	if dial("127.0.0.1") == nil {
		t.Fatal("a connection past the address's share was served")
	}
	for n := 0; n < 2; {
		if err := dial("127.0.0.5"); err == nil {
			n++
		} else if time.Now().After(deadline) {
			t.Fatal("another address was refused beside a full one")
		}
	}
	if dial("127.0.0.6") == nil {
		t.Fatal("a connection past the listener's total was served")
	}
	if got := rawGet(t, filepath.Join(s.dir, privateSocket), api.VersionPath); got != "HTTP/1.1 200 OK" {
		t.Errorf("the private socket beside a full HTTPS listener: %q", got)
	}
}
