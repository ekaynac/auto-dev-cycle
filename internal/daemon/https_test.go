// SPDX-License-Identifier: Apache-2.0

package daemon

import (
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
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
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
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: c.pool, MinVersion: tls.VersionTLS13}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(client.CloseIdleConnections)
	return s, client, "https://" + s.srv.tcp.String(), c
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

// httpsStatus is the status of GET url, its connection left reusable.
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
// fixed text and exit 64, before the state directory is touched.
func TestHTTPSConfigRefused(t *testing.T) {
	c := validCert(t)
	cert, key := c.files(t, 0o600)
	_, loose := c.files(t, 0o640)
	_, otherKey := validCert(t).files(t, 0o600)
	expired, expiredKey := newCert(t, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)).files(t, 0o600)
	early, earlyKey := newCert(t, time.Now().Add(time.Hour), time.Now().Add(2*time.Hour)).files(t, 0o600)
	link := filepath.Join(t.TempDir(), "tls-link-name.pem")
	fifo := filepath.Join(t.TempDir(), "tls-fifo-name.pem")
	garbage := filepath.Join(t.TempDir(), "tls-garbage-name.pem")
	if os.Symlink(key, link) != nil || unix.Mkfifo(fifo, 0o600) != nil || os.WriteFile(garbage, []byte("not pem"), 0o600) != nil {
		t.Fatal("fixtures")
	}
	addr := "127.0.0.1:18443"
	for name, args := range map[string][]string{
		"address only":   {"--https-addr", addr},
		"host name":      {"--https-addr", "localhost:18443", "--https-cert", cert, "--https-key", key},
		"port zero":      {"--https-addr", "127.0.0.1:0", "--https-cert", cert, "--https-key", key},
		"no port":        {"--https-addr", "127.0.0.1", "--https-cert", cert, "--https-key", key},
		"relative cert":  {"--https-addr", addr, "--https-cert", "cert.pem", "--https-key", key},
		"missing cert":   {"--https-addr", addr, "--https-cert", cert + ".absent", "--https-key", key},
		"garbage cert":   {"--https-addr", addr, "--https-cert", garbage, "--https-key", key},
		"key mode 0640":  {"--https-addr", addr, "--https-cert", cert, "--https-key", loose},
		"key symlink":    {"--https-addr", addr, "--https-cert", cert, "--https-key", link},
		"key fifo":       {"--https-addr", addr, "--https-cert", cert, "--https-key", fifo},
		"mismatched key": {"--https-addr", addr, "--https-cert", cert, "--https-key", otherKey},
		"expired cert":   {"--https-addr", addr, "--https-cert", expired, "--https-key", expiredKey},
		"not yet valid":  {"--https-addr", addr, "--https-cert", early, "--https-key", earlyKey},
	} {
		// Run as RunCommand does, bounded: a guard that let a case through
		// starts a daemon, which the deadline stops and the test reports.
		state := stateDir(t)
		cfg, err := LoadConfig(append([]string{"--state-dir", state}, args...))
		if err == nil {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			err = Run(ctx, cfg, io.Discard, nil)
			cancel()
		}
		if !errors.Is(err, ErrConfig) || strings.Contains(err.Error(), "-name") || strings.Contains(err.Error(), state) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the state directory was touched: %v", name, err)
		}
	}
	// The command's own exit: 64 with the fixed text.
	var stderr bytes.Buffer
	state := stateDir(t)
	code := RunCommand([]string{"--state-dir", state, "--https-addr", addr, "--https-cert", cert, "--https-key", otherKey}, &stderr)
	if out := stderr.String(); code != entry.ExitUsage || !strings.HasPrefix(out, ErrConfig.Error()) || strings.Contains(out, "-name") || strings.Contains(out, state) {
		t.Errorf("adcd run with a mismatched key: exit %d: %q", code, out)
	}
	// The key must be the daemon uid's.
	if _, err := (HTTPSListener{Addr: addr, CertFile: cert, KeyFile: key}).load(time.Now(), os.Geteuid()+1); !errors.Is(err, ErrConfig) {
		t.Fatalf("a key of another uid: %v", err)
	}
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

// A TLS 1.2 client is refused at the handshake; plaintext HTTP to the port
// gets no response at all.
func TestHTTPSRefusesOldTLSAndPlaintext(t *testing.T) {
	s, _, _, c := serveTLS(t, defaultLimits)
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
}

// Every request carries a bearer: none (or a wrong one on the version
// route) is 401; with one, whoami reports the HTTPS listener. Nothing is
// redirected or cacheable, and bootstrap is not served.
func TestHTTPSBearerRequired(t *testing.T) {
	s, c, base, _ := serveTLS(t, defaultLimits)
	for _, path := range []string{api.VersionPath, api.WhoamiPath, api.EventsPath, "/v1/grants", "/nowhere"} {
		if resp := httpsDo(t, c, http.MethodGet, base+path, ""); resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s without a bearer: %s %q", path, resp.Status, resp.Header.Get("Cache-Control"))
		}
	}
	if resp := httpsDo(t, c, http.MethodGet, base+api.VersionPath, "adc_wrong.secret"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("version with a wrong bearer: %s", resp.Status)
	}
	if resp := httpsDo(t, c, http.MethodGet, base+api.VersionPath, s.tok.Reveal()); resp.StatusCode != http.StatusOK {
		t.Errorf("version with a bearer: %s", resp.Status)
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
	if resp := httpsDo(t, c, http.MethodPost, base+"/v1/bootstrap", s.tok.Reveal()); resp.StatusCode/100 != 4 {
		t.Errorf("bootstrap over HTTPS: %s", resp.Status)
	}
}

// The HTTPS listener's verification and connection slots are its own: full,
// they refuse HTTPS at once and never the private socket, and the reverse.
func TestHTTPSPoolsIsolated(t *testing.T) {
	lim := defaultLimits
	lim.remote = slotQuota{conns: 2, connsPerPeer: 2, verify: 2, verifyPerPeer: 1}
	s, c, base, cert := serveTLS(t, lim)
	r1, ok1 := s.srv.remote.Take(api.Peer{Addr: netip.MustParseAddr("127.0.0.1")}.Key())
	r2, ok2 := s.srv.remote.Take(-7)
	if !ok1 || !ok2 {
		t.Fatal("the HTTPS gate is smaller than its quota")
	}
	if code := httpsStatus(t, c, base+api.WhoamiPath, s.tok.Reveal()); code != http.StatusServiceUnavailable {
		t.Errorf("whoami through a full HTTPS gate: %d", code)
	}
	if resp := s.get(t, s.client(), api.WhoamiPath, s.tok.Reveal()); resp.StatusCode != http.StatusOK {
		t.Errorf("the private socket under an HTTPS flood: %s", resp.Status)
	}
	r1()
	r2()
	private := s.srv.sockets[0].verify
	var held []func()
	for range defaultLimits.private.verify {
		release, ok := private.Take(os.Geteuid())
		if !ok {
			t.Fatal("the private gate is smaller than its quota")
		}
		held = append(held, release)
	}
	if code := httpsStatus(t, c, base+api.WhoamiPath, s.tok.Reveal()); code != http.StatusOK {
		t.Errorf("HTTPS under a private flood: %d", code)
	}
	for _, release := range held {
		release()
	}
	c.CloseIdleConnections() // the server frees their slots when it sees them close
	// Connections: the address's two, then one past them is closed unserved.
	deadline := time.Now().Add(5 * time.Second)
	for n := 0; n < 2; {
		conn, err := tls.Dial("tcp", s.srv.tcp.String(), &tls.Config{RootCAs: cert.pool})
		if err == nil {
			t.Cleanup(func() { conn.Close() })
			n++
		} else if time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
	if conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", s.srv.tcp.String(), &tls.Config{RootCAs: cert.pool}); err == nil {
		conn.Close()
		t.Fatal("a connection past the cap was served")
	}
	if got := rawGet(t, filepath.Join(s.dir, privateSocket), api.VersionPath); got != "HTTP/1.1 200 OK" {
		t.Errorf("the private socket beside a full HTTPS listener: %q", got)
	}
}
