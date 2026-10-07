// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// handshakeTimeout bounds the TLS handshake, after the dial's own bound;
// tests shorten it.
var handshakeTimeout = 10 * time.Second

// maxCABytes bounds the trust-anchor file.
const maxCABytes = 1 << 20

// Refusals on the HTTPS transport: fixed text, never the server's words
// nor the token [R3].
var (
	// ErrServerIdentity: the controller's certificate did not verify
	// against the configured CA or pin; nothing was sent.
	ErrServerIdentity = errors.New("the controller's certificate does not verify against the configured CA or pin; no credential was sent")
	// ErrHandshakeRefused: the controller closed the connection before
	// the TLS handshake completed, as it does to an address it holds off.
	ErrHandshakeRefused = errors.New("the controller closed the connection before the TLS handshake (it holds off an address after repeated failed authentications); no credential was sent; retry later")
	// ErrHandshake: any other failed TLS handshake; nothing was sent.
	ErrHandshake = errors.New("the TLS 1.3 handshake with the controller failed; no credential was sent")
	// ErrHeldOff: the controller answered 429 (see HeldOff).
	ErrHeldOff = errors.New("the controller is holding off this address after repeated failed authentications; nothing was decided")
	// ErrRemoteBootstrap: the bootstrap is served only on the private
	// socket [R4], so it is never sent over HTTPS.
	ErrRemoteBootstrap = errors.New("the bootstrap is only possible over the private socket (ADC_SOCKET), never over HTTPS")
)

// HeldOff is ErrHeldOff with the wait the controller asked for, when it
// gave a whole number of seconds from 1 to maxRetryAfter (else 0).
type HeldOff struct{ RetryAfter int }

const maxRetryAfter = 3600

func (h HeldOff) Error() string {
	if h.RetryAfter == 0 {
		return ErrHeldOff.Error() + "; retry later"
	}
	return fmt.Sprintf("%v; retry in %d s", ErrHeldOff, h.RetryAfter)
}

func (HeldOff) Is(target error) bool { return target == ErrHeldOff }

// heldOff reads Retry-After as the number it must be; anything else is
// dropped, never quoted.
func heldOff(retryAfter string) HeldOff {
	n, err := strconv.Atoi(retryAfter)
	if err != nil || n < 1 || n > maxRetryAfter {
		n = 0
	}
	return HeldOff{RetryAfter: n}
}

// Remote is the controller's HTTPS listener (08 "Transport"): its address
// and the trust anchor its certificate must verify against [R3], either a
// CA bundle of the caller's (the system roots are never used) or the
// SHA-256 fingerprint of the controller's exact certificate. Build it with
// NewRemote.
type Remote struct {
	addr string
	tls  *tls.Config
}

// PinPrefix opens a pinned fingerprint: sha256: and the 64 hex
// digits of the SHA-256 of the certificate's DER encoding.
const PinPrefix = "sha256:"

// NewRemote validates addr (HOST:PORT) and exactly one trust anchor: caFile,
// a PEM file of CA certificates, or pin, a PinPrefix fingerprint.
func NewRemote(addr, caFile, pin string) (Remote, error) {
	host, port, err := net.SplitHostPort(addr)
	if n, perr := strconv.Atoi(port); err != nil || perr != nil || host == "" || n < 1 || n > 65535 {
		return Remote{}, errors.New("the HTTPS address is not HOST:PORT with a port from 1 to 65535")
	}
	// No session cache: every connection verifies the certificate in full.
	tc := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: host}
	switch {
	case (caFile == "") == (pin == ""):
		return Remote{}, errors.New("the HTTPS transport needs exactly one trust anchor: a CA file or a pinned certificate fingerprint")
	case caFile != "":
		pool, err := readCA(caFile)
		if err != nil {
			return Remote{}, err
		}
		tc.RootCAs = pool
	default:
		want, err := parsePin(pin)
		if err != nil {
			return Remote{}, err
		}
		// The pin replaces chain building, not verification: the exact
		// certificate is the trust anchor, checked on every handshake
		// (VerifyConnection runs on resumed ones too) before any request
		// is written; its validity dates still apply.
		tc.InsecureSkipVerify = true
		tc.VerifyConnection = func(cs tls.ConnectionState) error {
			leaf := cs.PeerCertificates[0]
			got := sha256.Sum256(leaf.Raw)
			if subtle.ConstantTimeCompare(got[:], want) != 1 {
				return ErrServerIdentity
			}
			if now := time.Now(); now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
				return ErrServerIdentity
			}
			return nil
		}
	}
	return Remote{addr: addr, tls: tc}, nil
}

func parsePin(pin string) ([]byte, error) {
	h, ok := strings.CutPrefix(pin, PinPrefix)
	b, err := hex.DecodeString(h)
	if !ok || err != nil || len(b) != sha256.Size {
		return nil, errors.New("the pinned fingerprint is not sha256: and 64 hex digits")
	}
	return b, nil
}

// readCA reads a PEM bundle of at most maxCABytes, never waiting on a FIFO.
func readCA(path string) (*x509.CertPool, error) {
	refused := errors.New("the HTTPS CA file cannot be read as a PEM certificate bundle of at most 1 MiB")
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, refused
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxCABytes+1))
	pool := x509.NewCertPool()
	if err != nil || len(raw) > maxCABytes || !pool.AppendCertsFromPEM(raw) {
		return nil, refused
	}
	return pool, nil
}

// dialTLS connects to the controller and completes a TLS 1.3 handshake that
// verified its certificate, both under deadlines; only then may a request,
// and so the bearer, be written on the connection.
func (r Remote) dialTLS(ctx context.Context, _, _ string) (net.Conn, error) {
	d := net.Dialer{Timeout: dialTimeout}
	c, err := d.DialContext(ctx, "tcp", r.addr)
	if err != nil {
		return nil, err
	}
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	tc := tls.Client(c, r.tls.Clone())
	if err := tc.HandshakeContext(hctx); err != nil {
		c.Close()
		return nil, handshakeError(err)
	}
	return tc, nil
}

// handshakeError is the fixed refusal for a failed handshake.
func handshakeError(err error) error {
	var cv *tls.CertificateVerificationError // a CA chain, host name or date that does not verify
	switch {
	case errors.Is(err, ErrServerIdentity), errors.As(err, &cv):
		return ErrServerIdentity
	case errors.Is(err, io.EOF), errors.Is(err, syscall.ECONNRESET):
		return ErrHandshakeRefused
	}
	return ErrHandshake
}

// fixed returns the transport's own refusal inside err (net/http wraps
// it with the URL), or err.
func fixed(err error) error {
	for _, f := range []error{ErrServerIdentity, ErrHandshakeRefused, ErrHandshake} {
		if errors.Is(err, f) {
			return f
		}
	}
	return err
}
