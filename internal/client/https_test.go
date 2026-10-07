// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// selfSigned is a certificate for 127.0.0.1 valid from nb to na, generated
// per test: no key is committed.
func selfSigned(t *testing.T, nb, na time.Time) (tls.Certificate, []byte) {
	t.Helper()
	return selfSignedFor(t, net.IPv4(127, 0, 0, 1), nb, na)
}

// selfSignedFor is selfSigned for ip.
func selfSignedFor(t *testing.T, ip net.IP, nb, na time.Time) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "adcd"}, NotBefore: nb, NotAfter: na,
		IPAddresses: []net.IP{ip}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, der
}

func pinOf(der []byte) string {
	d := sha256.Sum256(der)
	return PinPrefix + hex.EncodeToString(d[:])
}

func caFile(t *testing.T, der []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// tlsServer serves h over TLS with cert (maxVersion 0: no cap) and counts
// the requests that reached h, each with its Authorization header.
type tlsServer struct {
	*httptest.Server
	asked  atomic.Int32
	bearer atomic.Value
}

func newTLSServer(t *testing.T, cert tls.Certificate, maxVersion uint16, h http.HandlerFunc) *tlsServer {
	t.Helper()
	s := &tlsServer{}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.asked.Add(1)
		s.bearer.Store(r.Header.Get("Authorization"))
		h(w, r)
	}))
	s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MaxVersion: maxVersion}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

func (s *tlsServer) addr() string { return strings.TrimPrefix(s.URL, "https://") }

func remote(t *testing.T, addr, ca, pin string) Transport {
	t.Helper()
	r, err := NewRemote(addr, ca, pin)
	if err != nil {
		t.Fatal(err)
	}
	tok := fakeToken(t)
	return Transport{Remote: &r, Token: func() (identity.Secret, error) { return tok, nil }}
}

func answerApplied(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, applied) }

// The configuration: an address with a port, exactly one trust anchor, a
// pin of the one form, a CA file that is a bounded PEM bundle.
func TestNewRemoteRefused(t *testing.T) {
	_, der := selfSigned(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	ca, pin := caFile(t, der), pinOf(der)
	dir := t.TempDir()
	garbage, big, fifo := filepath.Join(dir, "garbage.pem"), filepath.Join(dir, "big.pem"), filepath.Join(dir, "fifo.pem")
	pemBytes, _ := os.ReadFile(ca)
	if os.WriteFile(garbage, []byte("not pem"), 0o644) != nil || os.WriteFile(big, append(pemBytes, make([]byte, maxCABytes)...), 0o644) != nil ||
		syscall.Mkfifo(fifo, 0o600) != nil {
		t.Fatal("fixtures")
	}
	for name, tc := range map[string]struct{ addr, ca, pin, want string }{
		"no port":       {"127.0.0.1", ca, "", "HOST:PORT"},
		"port zero":     {"127.0.0.1:0", ca, "", "HOST:PORT"},
		"port 65536":    {"127.0.0.1:65536", ca, "", "HOST:PORT"},
		"no host":       {":8443", ca, "", "HOST:PORT"},
		"space in host": {"adcd example:8443", ca, "", "HOST:PORT"},
		"slash in host": {"adcd/x:8443", ca, "", "HOST:PORT"},
		"hyphen label":  {"-adcd.example:8443", ca, "", "HOST:PORT"},
		"empty label":   {"adcd..example:8443", ca, "", "HOST:PORT"},
		"long label":    {strings.Repeat("a", 64) + ".example:8443", ca, "", "HOST:PORT"},
		"no anchor":     {"127.0.0.1:8443", "", "", "exactly one trust anchor"},
		"both anchors":  {"127.0.0.1:8443", ca, pin, "exactly one trust anchor"},
		"pin no prefix": {"127.0.0.1:8443", "", strings.TrimPrefix(pin, PinPrefix), "64 hex digits"},
		"pin short":     {"127.0.0.1:8443", "", pin[:len(pin)-2], "64 hex digits"},
		"pin not hex":   {"127.0.0.1:8443", "", PinPrefix + strings.Repeat("z", 64), "64 hex digits"},
		"ca missing":    {"127.0.0.1:8443", ca + ".absent", "", "CA file"},
		"ca directory":  {"127.0.0.1:8443", dir, "", "not a regular file"},
		"ca garbage":    {"127.0.0.1:8443", garbage, "", "CA file"},
		"ca over 1MiB":  {"127.0.0.1:8443", big, "", "CA file"},
		"ca fifo":       {"127.0.0.1:8443", fifo, "", "not a regular file"}, // never waited on
	} {
		if _, err := NewRemote(tc.addr, tc.ca, tc.pin); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, addr := range []string{"adcd.example:8443", "[::1]:8443", "adcd-1:8443"} {
		if _, err := NewRemote(addr, ca, ""); err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
	}
}

// A certificate that verifies against the CA, or matches the pin, carries
// the command and its bearer.
func TestRemoteCommand(t *testing.T) {
	cert, der := selfSigned(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	s := newTLSServer(t, cert, 0, answerApplied)
	for name, tr := range map[string]Transport{"ca": remote(t, s.addr(), caFile(t, der), ""), "pin": remote(t, s.addr(), "", pinOf(der))} {
		if raw, err := tr.Command(t.Context(), "task.create", request(t)); err != nil || string(raw) != applied {
			t.Fatalf("%s: %s %v", name, raw, err)
		}
		if b, _ := s.bearer.Load().(string); !strings.HasPrefix(b, "Bearer tok_") {
			t.Fatalf("%s: the bearer did not arrive", name)
		}
	}
}

// Every server that fails verification, or offers less than TLS 1.3, gets
// no request and so no bearer; the refusal is fixed text.
func TestRemoteFailsClosed(t *testing.T) {
	now := time.Now()
	cert, der := selfSigned(t, now.Add(-time.Hour), now.Add(time.Hour))
	_, other := selfSigned(t, now.Add(-time.Hour), now.Add(time.Hour))
	expired, expiredDER := selfSigned(t, now.Add(-2*time.Hour), now.Add(-time.Hour))
	elsewhere, elsewhereDER := selfSignedFor(t, net.IPv4(127, 0, 0, 2), now.Add(-time.Hour), now.Add(time.Hour))
	good := newTLSServer(t, cert, 0, answerApplied)
	misnamed := newTLSServer(t, elsewhere, 0, answerApplied)
	old := newTLSServer(t, cert, tls.VersionTLS12, answerApplied)
	stale := newTLSServer(t, expired, 0, answerApplied)
	future, futureDER := selfSigned(t, now.Add(time.Hour), now.Add(2*time.Hour))
	early := newTLSServer(t, future, 0, answerApplied)
	// An attacker's own leaf, with the pinned certificate behind it in
	// the chain: the pin is the leaf's, never any certificate's.
	attacker, _ := selfSigned(t, now.Add(-time.Hour), now.Add(time.Hour))
	attacker.Certificate = append(attacker.Certificate, der)
	chained := newTLSServer(t, attacker, 0, answerApplied)
	for name, tc := range map[string]struct {
		s    *tlsServer
		tr   Transport
		want error
	}{
		"wrong ca":      {good, remote(t, good.addr(), caFile(t, other), ""), ErrServerIdentity},
		"wrong pin":     {good, remote(t, good.addr(), "", pinOf(other)), ErrServerIdentity},
		"wrong host":    {misnamed, remote(t, misnamed.addr(), caFile(t, elsewhereDER), ""), ErrServerIdentity},
		"expired pin":   {stale, remote(t, stale.addr(), "", pinOf(expiredDER)), ErrServerIdentity},
		"early pin":     {early, remote(t, early.addr(), "", pinOf(futureDER)), ErrServerIdentity},
		"pin in chain":  {chained, remote(t, chained.addr(), "", pinOf(der)), ErrServerIdentity},
		"tls 1.2 (ca)":  {old, remote(t, old.addr(), caFile(t, der), ""), ErrHandshake},
		"tls 1.2 (pin)": {old, remote(t, old.addr(), "", pinOf(der)), ErrHandshake},
	} {
		_, err := tc.tr.Command(t.Context(), "task.create", request(t))
		if err != tc.want {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := tc.tr.Stream(t.Context(), "/v1/events"); err != tc.want {
			t.Errorf("%s, stream: %v", name, err)
		}
		if n := tc.s.asked.Load(); n != 0 {
			t.Errorf("%s: %d requests reached the server", name, n)
		}
	}
}

// A 429 is ErrHeldOff with the wait when it is a sane number of seconds,
// never the server's text; the body is not read.
func TestRemoteHeldOff(t *testing.T) {
	cert, der := selfSigned(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	retry := atomic.Value{}
	s := newTLSServer(t, cert, 0, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", retry.Load().(string))
		http.Error(w, "zzSERVERzzTEXT", http.StatusTooManyRequests)
	})
	tr := remote(t, s.addr(), "", pinOf(der))
	for value, want := range map[string]int{"7": 7, "3600": 3600, "0": 0, "3601": 0, "zzSERVERzz": 0, "": 0, "-5": 0} {
		retry.Store(value)
		_, err := tr.Command(t.Context(), "task.create", request(t))
		var h HeldOff
		if !errors.As(err, &h) || h.RetryAfter != want || !errors.Is(err, ErrHeldOff) || strings.Contains(err.Error(), "zzSERVERzz") {
			t.Errorf("Retry-After %q: %v", value, err)
		}
		if _, err := tr.Stream(t.Context(), "/v1/events"); !errors.Is(err, ErrHeldOff) {
			t.Errorf("Retry-After %q, stream: %v", value, err)
		}
	}
	if !strings.Contains(HeldOff{RetryAfter: 7}.Error(), "retry in 7 s") {
		t.Fatal(HeldOff{RetryAfter: 7}.Error())
	}
}

// A listener that closes the connection before the handshake (a held-off
// address) is ErrHandshakeRefused; one that never answers the handshake is
// cut off at the handshake bound.
func TestRemoteHandshakeRefusedAndBounded(t *testing.T) {
	_, der := selfSigned(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	closer, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	go func() {
		for {
			c, err := closer.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if _, err := remote(t, closer.Addr().String(), "", pinOf(der)).Command(t.Context(), "task.create", request(t)); err != ErrHandshakeRefused {
		t.Fatalf("closed before the handshake: %v", err)
	}
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	defer func(d time.Duration) { handshakeTimeout = d }(handshakeTimeout)
	handshakeTimeout = 200 * time.Millisecond
	// A stream has no overall timeout: only the handshake bound ends it.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if _, err := remote(t, silent.Addr().String(), "", pinOf(der)).Stream(ctx, "/v1/events"); err != ErrHandshake || ctx.Err() != nil {
		t.Fatalf("a silent server: %v", err)
	}
}

// The bootstrap is never sent over HTTPS: no connection is made.
func TestRemoteBootstrapRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	_, der := selfSigned(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if _, err := remote(t, ln.Addr().String(), "", pinOf(der)).Bootstrap(t.Context(), []byte(`{}`)); err != ErrRemoteBootstrap {
		t.Fatalf("bootstrap over HTTPS: %v", err)
	}
	ln.Close()
	if n := accepted.Load(); n != 0 {
		t.Fatalf("%d connections", n)
	}
}

// A server whose answer net/http cannot parse is ErrUnreadable: net/http's
// own error would quote it.
func TestRemoteUnreadable(t *testing.T) {
	cert, der := selfSigned(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	s := newTLSServer(t, cert, 0, func(w http.ResponseWriter, _ *http.Request) {
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		rw.WriteString("HTTP/1.1 200 OK\r\nzzSERVERzz no colon\r\n\r\n")
		rw.Flush()
	})
	tr := remote(t, s.addr(), "", pinOf(der))
	if _, err := tr.Command(t.Context(), "task.create", request(t)); err != ErrUnreadable {
		t.Fatalf("a malformed response: %v", err)
	}
	if _, err := tr.Stream(t.Context(), "/v1/events"); err != ErrUnreadable {
		t.Fatalf("a malformed response, stream: %v", err)
	}
}

// A dial error names only the configured address and passes through.
func TestRemoteDialErrorPassesThrough(t *testing.T) {
	_, der := selfSigned(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond) // shorter than dialTimeout
	defer cancel()
	start := time.Now()
	_, err := remote(t, "192.0.2.1:443", "", pinOf(der)).Command(ctx, "task.create", request(t))
	var op *net.OpError
	dialed := errors.As(err, &op) && op.Op == "dial" || errors.Is(err, context.DeadlineExceeded) // unroutable, or cut at the deadline
	if !dialed || errors.Is(err, ErrUnreadable) || time.Since(start) > dialTimeout {
		t.Fatalf("a dial that cannot complete: %v after %s", err, time.Since(start))
	}
}

// The pin check refuses a handshake with no certificate (crypto/tls never
// offers one; the check does not rely on that).
func TestPinNeedsACertificate(t *testing.T) {
	_, der := selfSigned(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	r, err := NewRemote("127.0.0.1:8443", "", pinOf(der))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.tls.VerifyConnection(tls.ConnectionState{}); err != ErrServerIdentity {
		t.Fatalf("no certificate: %v", err)
	}
}

// The HTTPS client's shape: only dialTLS connects, no proxy, TLS 1.3 at
// least, no session cache, and the CA mode verifies.
func TestRemoteTransportShape(t *testing.T) {
	_, der := selfSigned(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	for name, tr := range map[string]Transport{"ca": remote(t, "127.0.0.1:8443", caFile(t, der), ""), "pin": remote(t, "127.0.0.1:8443", "", pinOf(der))} {
		ht := tr.httpClient(true).Transport.(*http.Transport)
		c := tr.Remote.tls
		if ht.DialContext != nil || ht.DialTLSContext == nil || ht.Proxy != nil || ht.TLSClientConfig != nil ||
			c.ClientSessionCache != nil || c.MinVersion != tls.VersionTLS13 || (name == "ca") == c.InsecureSkipVerify || c.ServerName != "127.0.0.1" {
			t.Errorf("%s: %+v %+v", name, ht, c)
		}
	}
}
