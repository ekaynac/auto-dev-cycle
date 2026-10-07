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

// tlsLimits are the defaults with the failure throttle out of the way:
// tests that are not about it send many unauthenticated requests.
func tlsLimits() limits {
	lim := defaultLimits
	lim.freeFailures = 1 << 20
	return lim
}

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
// one or a wrong one is 401 naming the Bearer scheme on every registered
// route, on bootstrap and on an unknown path, and the refusal closes the
// connection. Nothing is
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
			if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Cache-Control") != "no-store" || !resp.Close ||
				resp.Header.Get("WWW-Authenticate") != "Bearer" {
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
	httpsHandler(s.w.s, defaultLimits, api.NewSlots(2, 1), newThrottle(3)).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no connection: %d", rec.Code)
	}
}

// A connection keeps the bearer it verified, rechecked without hashing:
// a revoked token is refused on the very connection that verified it.
func TestHTTPSVerifiedBearerRechecked(t *testing.T) {
	s, c, base, _ := serveTLS(t, tlsLimits())
	if code := httpsStatus(t, c, base+api.VersionPath, s.tok.Reveal()); code != http.StatusOK {
		t.Fatalf("version: %d", code)
	}
	// The same token id with another secret is verified in full, and fails.
	id, secret, _ := strings.Cut(s.tok.Reveal(), ".")
	forged := id + "." + map[bool]string{true: "B", false: "A"}[secret[0] == 'A'] + secret[1:]
	if code := httpsStatus(t, c, base+api.VersionPath, forged); code != http.StatusUnauthorized {
		t.Fatalf("a forged secret on a verified connection: %d", code)
	}
	if code := httpsStatus(t, c, base+api.VersionPath, s.tok.Reveal()); code != http.StatusOK {
		t.Fatalf("version again: %d", code)
	}
	s.w.do(s.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeToken(tx, types.TokenID(s.tokenID()), time.Now())
	})
	if code := httpsStatus(t, c, base+api.VersionPath, s.tok.Reveal()); code != http.StatusUnauthorized {
		t.Fatalf("a revoked token on its verified connection: %d", code)
	}
}

// A route trusts the verification its connection already made for the
// same bearer: a read on a verified connection takes no verification slot
// (hashes nothing), while a new connection still needs one.
func TestHTTPSRouteHashesOnce(t *testing.T) {
	s, c, base, cert := serveTLS(t, tlsLimits())
	if code := httpsStatus(t, c, base+api.VersionPath, s.tok.Reveal()); code != http.StatusOK {
		t.Fatalf("version: %d", code)
	}
	r1, ok := s.srv.remote.Take(api.Peer{Addr: netip.MustParseAddr("127.0.0.1")}.Key())
	if !ok {
		t.Fatal("no slot")
	}
	defer r1()
	for _, path := range []string{api.WhoamiPath, api.VersionPath} {
		if code := httpsStatus(t, c, base+path, s.tok.Reveal()); code != http.StatusOK {
			t.Errorf("%s on the verified connection, its address's slot taken: %d", path, code)
		}
	}
	resp := httpsDo(t, c, http.MethodGet, base+api.EventsPath, s.tok.Reveal())
	if resp.StatusCode != http.StatusOK {
		t.Errorf("events on the verified connection, its address's slot taken: %s", resp.Status)
	}
	resp.Body.Close()
	if code := httpsStatus(t, tlsClient(t, cert, "127.0.0.1"), base+api.WhoamiPath, s.tok.Reveal()); code != http.StatusServiceUnavailable {
		t.Errorf("a new connection, its address's slot taken: %d", code)
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

// A peer whose bearers keep failing is held off: a connection it already
// has gets 429 and Retry-After before any verification (and its end is no
// further failure), a new one is closed before its TLS handshake; another
// peer is not held off.
func TestHTTPSFailureThrottle(t *testing.T) {
	lim := tlsLimits()
	lim.freeFailures, lim.preAuth = 2, time.Minute // the early connection outlives the failures, even under -race
	s, c, base, cert := serveTLS(t, lim)
	early, err := tls.Dial("tcp", s.srv.tcp.String(), &tls.Config{RootCAs: cert.pool})
	if err != nil {
		t.Fatal(err)
	}
	defer early.Close()
	for i := range 3 {
		if code := httpsStatus(t, c, base+api.VersionPath, "adc_wrong.secret"); code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i, code)
		}
	}
	r1, _ := s.srv.remote.Take(api.Peer{Addr: netip.MustParseAddr("127.0.0.1")}.Key())
	defer r1()
	early.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(early, "GET "+api.VersionPath+" HTTP/1.1\r\nHost: adcd\r\nAuthorization: Bearer "+s.tok.Reveal()+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(early), nil)
	if err != nil || resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" || !resp.Close {
		t.Fatalf("a held-off peer, its slot taken: %v %v", resp, err)
	}
	if conn, err := tls.Dial("tcp", s.srv.tcp.String(), &tls.Config{RootCAs: cert.pool}); err == nil {
		conn.Close()
		t.Fatal("a held-off peer completed a TLS handshake")
	}
	if code := httpsStatus(t, tlsClient(t, cert, "127.0.0.2"), base+api.VersionPath, s.tok.Reveal()); code != http.StatusOK {
		t.Fatalf("another peer: %d", code)
	} // The 429 was the handler's answer, which counts no failure: the end
	// of its connection adds none either.
	early.Close()
	time.Sleep(300 * time.Millisecond)
	strikesReach(t, s.srv.th, "127.0.0.1", 3)
}

// strikesOf is how many failures th holds against key.
func strikesOf(th *throttle, key any) int {
	th.mu.Lock()
	defer th.mu.Unlock()
	if s := th.peers[key]; s != nil {
		return s.n
	}
	return 0
}

// strikesReach waits until th holds want failures against addr's key.
func strikesReach(t *testing.T, th *throttle, addr string, want int) {
	t.Helper()
	key := api.Peer{Addr: netip.MustParseAddr(addr)}.Key()
	for deadline := time.Now().Add(10 * time.Second); strikesOf(th, key) != want; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d failures, want %d", addr, strikesOf(th, key), want)
		}
	}
}

// A connection closed at the pre-auth bound with no request counts as one
// failure for its peer, once, even with no handshake. A request that
// reached the handler is judged there (a 401 is one failure; a bearer
// still being verified none), so its connection's end adds nothing.
func TestHTTPSExpiryWithoutBearerFails(t *testing.T) {
	lim := tlsLimits()
	lim.preAuth, lim.preAuthFree = time.Second, time.Hour // the bound alone makes the failure
	s, _, _, _ := serveTLS(t, lim)
	silent, err := net.Dial("tcp", s.srv.tcp.String())
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	silent.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := silent.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("a silent connection at the pre-auth bound: %v", err)
	}
	strikesReach(t, s.srv.th, "127.0.0.1", 1)
	time.Sleep(300 * time.Millisecond)
	strikesReach(t, s.srv.th, "127.0.0.1", 1)
	for name, bearer := range map[string]string{"a wrong bearer": "adc_wrong.secret", "no bearer": ""} {
		server, client := net.Pipe()
		defer client.Close()
		th := newThrottle(0)
		hc := &httpsConn{Conn: server, peer: api.Peer{UID: -1, Addr: netip.MustParseAddr("192.0.2.7")}, release: func() {}, th: th,
			since: time.Now().Add(-time.Hour), free: time.Second, expiry: time.NewTimer(time.Hour)}
		req := httptest.NewRequestWithContext(context.WithValue(t.Context(), httpsConnKey{}, hc), http.MethodGet, api.VersionPath, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		httpsHandler(s.w.s, lim, api.NewSlots(2, 1), th).ServeHTTP(rec, req)
		hc.expire()
		hc.ended(true)
		if n := strikesOf(th, hc.peer.Key()); rec.Code != http.StatusUnauthorized || n != 1 {
			t.Errorf("%s: %d, then %d failures at its end", name, rec.Code, n)
		}
	}
}

// A connection that ends with no request counts as one failure if it
// completed its TLS handshake, however soon it closed, or held its slot
// for preAuthFree, handshake or not. Only one that closed within
// preAuthFree without completing a handshake (a TCP connect, a cut-off
// ClientHello) does not. A bearer-less request is its 401 alone.
func TestHTTPSClosedWithoutBearerFails(t *testing.T) {
	lim := tlsLimits()
	lim.preAuth, lim.preAuthFree = time.Minute, 500*time.Millisecond
	s, _, base, cert := serveTLS(t, lim)
	addr := s.srv.tcp.String()
	from := func(ip string) *net.Dialer { return &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)}} }
	if raw, err := from("127.0.0.2").Dial("tcp", addr); err == nil {
		raw.Close()
	}
	if raw, err := from("127.0.0.3").Dial("tcp", addr); err == nil {
		_, _ = raw.Write([]byte{0x16, 0x03, 0x01, 0x00, 0x40, 0x01}) // a ClientHello, cut off
		raw.Close()
	}
	conn, err := tls.DialWithDialer(from("127.0.0.1"), "tcp", addr, &tls.Config{RootCAs: cert.pool})
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if raw, err := from("127.0.0.4").Dial("tcp", addr); err == nil {
		time.Sleep(2 * lim.preAuthFree) // held, no handshake, closed long before the bound
		raw.Close()
	}
	if code := httpsStatus(t, tlsClient(t, cert, "127.0.0.5"), base+api.VersionPath, ""); code != http.StatusUnauthorized {
		t.Fatalf("no bearer: %d", code)
	}
	for _, ip := range []string{"127.0.0.1", "127.0.0.4", "127.0.0.5"} {
		strikesReach(t, s.srv.th, ip, 1)
	}
	time.Sleep(300 * time.Millisecond)
	for ip, want := range map[string]int{"127.0.0.1": 1, "127.0.0.2": 0, "127.0.0.3": 0, "127.0.0.4": 1, "127.0.0.5": 1} {
		strikesReach(t, s.srv.th, ip, want)
	}
	for name, tc := range map[string]struct {
		age                time.Duration
		expired, handshook bool
		want               int
	}{
		"short, no handshake":        {0, false, false, 0},
		"short, handshake":           {0, true, true, 1},
		"long, no handshake":         {2 * time.Second, false, false, 1},
		"no handshake, at the bound": {0, true, false, 1},
	} {
		th := newThrottle(0)
		server, client := net.Pipe()
		defer client.Close()
		hc := &httpsConn{Conn: server, peer: api.Peer{UID: -1, Addr: netip.MustParseAddr("192.0.2.7")}, release: func() {}, th: th,
			since: time.Now().Add(-tc.age), free: time.Second, expiry: time.NewTimer(time.Hour)}
		if tc.expired {
			hc.expire()
		}
		hc.ended(tc.handshook)
		hc.ended(tc.handshook)
		if n := strikesOf(th, hc.peer.Key()); n != tc.want {
			t.Errorf("%s, judged twice: %d failures, want %d", name, n, tc.want)
		}
	}
}

// An authenticated event stream that outlives preAuthFree and the pre-auth
// bound ends with no failure.
func TestHTTPSStreamEndNoFailure(t *testing.T) {
	lim := tlsLimits()
	lim.poll, lim.preAuth, lim.preAuthFree = 100*time.Millisecond, 2*time.Second, time.Millisecond // a verification fits in the bound, even under -race
	s, c, base, _ := serveTLS(t, lim)
	resp := httpsDo(t, c, http.MethodGet, base+api.EventsPath, s.tok.Reveal())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events over HTTPS: %s", resp.Status)
	}
	time.Sleep(lim.preAuth + 500*time.Millisecond)
	resp.Body.Close()
	c.CloseIdleConnections()
	time.Sleep(500 * time.Millisecond)
	strikesReach(t, s.srv.th, "127.0.0.1", 0)
}

// The throttle's wait doubles past the free failures, is capped, and is
// forgotten after a quiet spell.
func TestThrottleBackoff(t *testing.T) {
	now := time.Unix(0, 0)
	th := newThrottle(1)
	th.now = func() time.Time { return now }
	for i, want := range []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, maxBackoff, maxBackoff} {
		th.fail("p")
		if got := max(th.wait("p"), 0); got != want {
			t.Fatalf("after failure %d: wait %s, want %s", i+1, got, want)
		}
	}
	if th.wait("q") > 0 {
		t.Fatal("another peer is held off")
	}
	now = now.Add(forgetAfter + maxBackoff + time.Second)
	th.fail("p")
	if th.wait("p") > 0 {
		t.Fatal("a quiet peer was not forgotten")
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
	l := tcpListener{Listener: ln, conns: api.NewSlots(4, 4), preAuth: time.Minute, th: newThrottle(0)}
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

// A held-off peer's connection is closed at accept, before its TLS
// handshake and without taking a slot; another peer's is admitted.
func TestHTTPSAcceptRefusesHeldOff(t *testing.T) {
	th := newThrottle(0)
	held := api.Peer{UID: -1, Addr: netip.MustParseAddr("192.0.2.7")}
	th.fail(held.Key())
	ln := &addrListener{conns: make(chan net.Conn, 2)}
	server, client := net.Pipe()
	defer client.Close()
	ln.conns <- addrConn{Conn: server, remote: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 7), Port: 1}}
	valid, other := net.Pipe()
	defer other.Close()
	ln.conns <- addrConn{Conn: valid, remote: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 8), Port: 1}}
	close(ln.conns)
	slots := api.NewSlots(2, 1)
	l := tcpListener{Listener: ln, conns: slots, preAuth: time.Minute, th: th}
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if hc := c.(*httpsConn); hc.peer.Addr != netip.MustParseAddr("192.0.2.8") {
		t.Fatalf("admitted %v", hc.peer.Addr)
	}
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the held-off connection: %v", err)
	}
	release, ok := slots.Take(held.Key())
	if !ok {
		t.Fatal("the held-off connection took a slot")
	}
	release()
}

// The HTTPS listener's default share per address is small enough that
// filling every connection slot takes at least 32 addresses.
func TestHTTPSDefaultRemoteShare(t *testing.T) {
	q := defaultLimits.remote
	if q.connsPerPeer < 1 || q.conns/q.connsPerPeer < 32 {
		t.Fatalf("%d connections, %d per address: %d addresses fill them", q.conns, q.connsPerPeer, q.conns/max(q.connsPerPeer, 1))
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

// The throttle's table is bounded: a full one records no new peer, is
// swept for quiet peers at most once per sweepEvery however many new
// peers fail, and makes room once peers go quiet. A tracked peer is held
// off throughout, and one peer's failures never hold off another.
func TestThrottleBounded(t *testing.T) {
	now := time.Unix(0, 0)
	th := newThrottle(0)
	th.now = func() time.Time { return now }
	for i := range maxTracked {
		th.fail(i)
	}
	for i := range 10 * maxTracked {
		th.fail(maxTracked + i)
	}
	if len(th.peers) != maxTracked || th.sweeps != 1 {
		t.Fatalf("a full table: %d peers, %d sweeps", len(th.peers), th.sweeps)
	}
	if th.wait(0) <= 0 || th.wait(maxTracked) > 0 {
		t.Fatalf("held off: tracked %s, untracked %s", th.wait(0), th.wait(maxTracked))
	}
	now = now.Add(sweepEvery)
	th.fail("late")
	if th.sweeps != 2 || th.wait("late") > 0 {
		t.Fatalf("a full table a sweep later: %d sweeps, late held off %s", th.sweeps, th.wait("late"))
	}
	now = now.Add(forgetAfter + time.Second)
	th.fail("new")
	if th.sweeps != 3 || len(th.peers) != 1 || th.wait("new") <= 0 {
		t.Fatalf("after a quiet spell: %d sweeps, %d peers, new held off %s", th.sweeps, len(th.peers), th.wait("new"))
	}
}

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
