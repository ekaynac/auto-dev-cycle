// SPDX-License-Identifier: Apache-2.0

// Package client is adc's transport to the controller over its Unix socket
// (docs/design/m0/08 "Transport"): HTTP/1.1 + JSON with a bearer token, sent
// only on a connection whose peer runs as the daemon uid [M3], with the token
// read from ADC_TOKEN or a private token file [R5].
package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/apidoc"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// Bounds on what the controller may send back and how long it may take.
const (
	maxResponseBytes = 1 << 20
	maxHeaderBytes   = 64 << 10
	commandTimeout   = 30 * time.Second
	headerTimeout    = 10 * time.Second
	dialTimeout      = 5 * time.Second
)

// ErrPeerNotDaemon: the socket's peer does not run as the expected daemon
// uid, so nothing was sent on it.
var ErrPeerNotDaemon = errors.New("the controller socket is not served by the expected daemon user; no credential was sent")

// Transport sends commands to the controller on Socket. Every connection is
// checked against DaemonUID before a request is written on it; Token is
// called once per request.
type Transport struct {
	Socket    string
	DaemonUID int
	Token     func() (identity.Secret, error)
}

// dial connects to the socket and returns the connection only if its peer
// runs as the daemon uid.
func (t Transport) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	d := net.Dialer{Timeout: dialTimeout}
	c, err := d.DialContext(ctx, "unix", t.Socket)
	if err != nil {
		return nil, err
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		c.Close()
		return nil, ErrPeerNotDaemon
	}
	uid, err := peerUID(uc)
	if err != nil || uid != t.DaemonUID {
		c.Close()
		return nil, ErrPeerNotDaemon
	}
	return c, nil
}

// httpClient has no overall timeout when stream is set (an event stream
// stays open); the response headers are bounded either way.
func (t Transport) httpClient(stream bool) *http.Client {
	c := &http.Client{
		Transport: &http.Transport{
			DialContext:            t.dial,
			DisableKeepAlives:      true,
			ResponseHeaderTimeout:  headerTimeout,
			MaxResponseHeaderBytes: maxHeaderBytes,
		},
		// A redirect is never followed, so a credential never follows one;
		// the 3xx response itself goes through the same checks as any other.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if !stream {
		c.Timeout = commandTimeout
	}
	return c
}

// do sends one request with the bearer token on a verified connection.
func (t Transport) do(ctx context.Context, method, path string, body io.Reader, stream bool) (*http.Response, error) {
	tok, err := t.Token()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://adcd"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.Reveal())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return t.httpClient(stream).Do(req)
}

// Command posts a validated CommandRequest to /v1/commands/{action} and
// returns the response body once it validates as a CommandResponse (a
// refusal may come with any status).
func (t Transport) Command(ctx context.Context, action string, body []byte) ([]byte, error) {
	if err := checkRequest(body); err != nil {
		return nil, err
	}
	resp, err := t.do(ctx, http.MethodPost, "/v1/commands/"+url.PathEscape(action), bytes.NewReader(body), false)
	return answer(resp, err, "a command response", func(raw []byte) error { _, err := DecodeResponse(raw); return err })
}

// Bootstrap posts a BootstrapRequest to /v1/bootstrap on a verified
// connection, without a bearer (the setup secret is the credential), and
// returns the response body once it validates as a BootstrapResponse. No
// error quotes the request.
func (t Transport) Bootstrap(ctx context.Context, body []byte) ([]byte, error) {
	if contract.Validate("api/bootstrap-request", body) != nil {
		return nil, errors.New("the bootstrap request is not a valid BootstrapRequest")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://adcd"+apidoc.BootstrapPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.httpClient(false).Do(req)
	return answer(resp, err, "a bootstrap response", func(raw []byte) error { return contract.Validate("api/bootstrap-response", raw) })
}

// answer reads a bounded response body and returns it once check accepts
// it (a refusal may come with any status).
func answer(resp *http.Response, err error, what string, check func([]byte) error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading the controller's response: %w", err)
	}
	if len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("the controller's response exceeds %d bytes", maxResponseBytes)
	}
	if err := check(raw); err != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("the controller answered HTTP %d without %s", resp.StatusCode, what)
		}
		return nil, err
	}
	return raw, nil
}

// Stream opens a long-lived GET (the event stream, 08 "Events") on a
// verified connection and returns its body; the caller closes it.
func (t Transport) Stream(ctx context.Context, path string) (io.ReadCloser, error) {
	resp, err := t.do(ctx, http.MethodGet, path, nil, true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("the controller answered HTTP %d to the stream", resp.StatusCode)
	}
	return resp.Body, nil
}
