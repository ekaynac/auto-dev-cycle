// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// serveOn serves g's Server() on a Unix socket, as 22a-5 does, with each
// handler's end signalled on done.
func serveOn(t *testing.T, g *Gateway, done chan<- bool) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "gateway.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := g.Server()
	if done != nil {
		srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { done <- true }()
			g.ServeHTTP(w, r)
		})
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return sock
}

// overSocket is an HTTP client of the gateway's socket.
func overSocket(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
}

func post(c *http.Client, token string) (int, string, error) {
	r, _ := http.NewRequest(http.MethodPost, BaseURL+"/chat/completions", strings.NewReader(good))
	r.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(r)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get(RefusalHeader), nil
}

// #226 entry 57: at most MaxInFlight requests per token at once; one past
// it is refused before its body is read (a body never written: Opus #308
// L1), nothing sent and no intent; the slot frees when a request ends;
// another token is not counted against it.
func TestInFlightCap(t *testing.T) {
	arrived, release := make(chan bool, 8), make(chan bool)
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		arrived <- true
		<-release
		_, _ = w.Write([]byte(`{"choices":[]}`))
	})
	g, token := rig(t, up, nil)
	free := sync.OnceFunc(func() { close(release) })
	t.Cleanup(free) // a failed row must not hold the upstream's handlers (they keep its Close waiting)
	done := make(chan bool, 8)
	sock := serveOn(t, g, done)
	c := overSocket(sock)
	type res struct {
		status int
		code   string
	}
	results := make(chan res, MaxInFlight+1)
	for range MaxInFlight {
		go func() { s, code, _ := post(c, token); results <- res{s, code} }()
		<-arrived
	}
	intended := g.cfg.Intents.(*intents).n()
	conn, err := net.Dial("unix", sock) // headers announcing a body never sent: the cap must refuse without reading it
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))                                                                                                                // with no cap, it would wait on the held upstream (L2)
	fmt.Fprintf(conn, "POST /chat/completions HTTP/1.1\r\nHost: adc-gateway\r\nAuthorization: Bearer %s\r\nContent-Length: 1000\r\nExpect: 100-continue\r\n\r\n", token) // 100 Continue only once the body is read
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get(RefusalHeader) != "in_flight_cap" {
		t.Fatalf("a request past the cap: %v %v; want 429 in_flight_cap", resp, err)
	}
	resp.Body.Close()
	<-done
	if n := len(up.got()); n != MaxInFlight || g.cfg.Intents.(*intents).n() != intended {
		t.Errorf("past the cap: %d requests reached the upstream (want %d), intents %d -> %d", n, MaxInFlight, intended, g.cfg.Intents.(*intents).n())
	}
	other, err := g.Bind(Binding{Lease: leaseID, Attempt: attempt, Call: "cal_01m4tm99haemss9tvzd438v000"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { s, code, _ := post(c, other); results <- res{s, code} }()
	<-arrived // another token's request is admitted
	free()
	for range MaxInFlight + 1 {
		if r := <-results; r.status != http.StatusOK {
			t.Errorf("an admitted request: %+v", r)
		}
		<-done // its handler has ended (Opus #308 M1)
	}
	if s, _, err := post(c, token); err != nil || s != http.StatusOK { // the slots are free again
		t.Errorf("after the requests end: %d %v", s, err)
	}
	<-done
	g.mu.Lock()
	n := len(g.inflight)
	g.mu.Unlock()
	if n != 0 {
		t.Errorf("%d tokens in flight after every request ended", n)
	}
}

// Opus #308 L4: a slot is released once the upstream exchange is over,
// before its count commits: a slow store never refuses the next request.
func TestASlotIsFreeBeforeItsCountCommits(t *testing.T) {
	g, token := rig(t, newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":1}}`))
	}), nil)
	slow := &holdCounts{Counts: g.cfg.Counts, hold: make(chan bool), held: make(chan bool, 8)}
	g.cfg.Counts = slow
	c := overSocket(serveOn(t, g, nil))
	for range MaxInFlight { // each holds its usage count, its response already read upstream
		go func() { _, _, _ = post(c, token) }()
		<-slow.held
	}
	s, code, err := post(c, token) // post returns once the status is read: before the third's own count
	close(slow.hold)
	if err != nil || s != http.StatusOK {
		t.Errorf("a request while two counts commit: %d %q %v, want admitted", s, code, err)
	}
}

// holdCounts holds every count after a response (not floors) until hold is
// closed, signalling each on held.
type holdCounts struct {
	Counts
	hold, held chan bool
}

func (h *holdCounts) Count(ctx context.Context, b Binding, seq int64, n Count) error {
	if n.Observed {
		h.held <- true
		<-h.hold
	}
	return h.Counts.Count(ctx, b, seq, n)
}

// #226 entry 58: the minted base_url has no path, and the real http-chat
// adapter's client (adapters/http-chat/src/gateway.ts), over the socket,
// reaches the upstream at its endpoint's /chat/completions. A base_url
// with /v1 fails closed (404 path).
func TestMintedBaseURLThroughTheAdapter(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH: the adapter tests run under make test")
	}
	client, err := filepath.Abs("../../adapters/http-chat/src/gateway.ts")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(client), "..", "..", "node_modules")); err != nil {
		t.Skip("the adapters workspace is not installed: run make ts-deps")
	}
	up := newUpstream(t, nil)
	g, _ := rig(t, up, nil)
	v, err := g.Mint(Binding{Lease: leaseID, Attempt: attempt, Call: "cal_01m4tm99haemss9tvzd438v001"})
	if err != nil || v.BaseUrl != BaseURL || strings.Contains(strings.TrimPrefix(v.BaseUrl, "http://"), "/") || v.Token == "" {
		t.Fatalf("minted %+v, %v", v, err)
	}
	sock := serveOn(t, g, nil)
	for name, c := range map[string]struct {
		base   string
		status int
		code   string
	}{"the minted base_url": {v.BaseUrl, http.StatusOK, ""}, "a base_url with /v1": {v.BaseUrl + "/v1", http.StatusNotFound, "path"}} {
		script := fmt.Sprintf(`import { chat } from %q;
const r = await chat({ base_url: %q, token: %q, socketPath: %q }, %s, AbortSignal.timeout(10000));
process.stdout.write(JSON.stringify({ status: r.status, refusal: r.refusal }));`, client, c.base, v.Token, sock, good)
		cmd := exec.Command(node, "--input-type=module", "-e", script)
		cmd.Dir = filepath.Dir(client)
		out, err := cmd.Output()
		var got struct {
			Status  int    `json:"status"`
			Refusal string `json:"refusal"`
		}
		if err != nil || json.Unmarshal(out, &got) != nil || got.Status != c.status || got.Refusal != c.code {
			t.Errorf("%s: %s %v; want %d %q", name, out, err, c.status, c.code)
		}
	}
	if seen := up.got(); len(seen) != 1 || seen[0].path != "/v1/chat/completions" {
		t.Errorf("the upstream saw %+v; want one request at /v1/chat/completions", seen)
	}
}

// #226 entry 78: an adapter that stops reading ends the handler within the
// write deadline: the upstream response is closed and the call keeps its
// floor (no count after the response).
func TestAStalledReaderEndsTheHandler(t *testing.T) {
	old := writeIdle
	writeIdle = 300 * time.Millisecond
	t.Cleanup(func() { writeIdle = old })
	upDone := make(chan bool, 1)
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		defer func() { upDone <- true }()
		w.Header().Set("Content-Type", "text/plain")
		chunk := []byte(strings.Repeat("x", 1<<16))
		for range 1 << 10 { // 64 MiB, far past the socket's buffers
			if _, err := w.Write(chunk); err != nil || r.Context().Err() != nil {
				return
			}
		}
	})
	g, token := rig(t, up, nil)
	done := make(chan bool, 1)
	conn, err := net.Dial("unix", serveOn(t, g, done))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /chat/completions HTTP/1.1\r\nHost: adc-gateway\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", token, len(good), good)
	_, _ = bufio.NewReader(conn).Peek(1) // the response starts, then nothing more is read
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler did not end while its adapter stopped reading")
	}
	select {
	case <-upDone:
	case <-time.After(10 * time.Second):
		t.Error("the upstream response was not closed")
	}
	for _, c := range countsOf(g).all() {
		if c.n.Observed {
			t.Errorf("a response not forwarded whole was counted as observed: %+v", c)
		}
	}
}

// #226 entry 57: Unbind is idempotent and safe with a request in flight: it
// cancels the request (the upstream exchange ends, its floor kept), and
// nothing the request does afterwards binds or observes again.
func TestUnbindWithARequestInFlight(t *testing.T) {
	arrived := make(chan bool, 1)
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		arrived <- true
		<-r.Context().Done() // until the gateway cancels it
		w.WriteHeader(http.StatusInternalServerError)
	})
	g, token := rig(t, up, nil)
	done := make(chan bool, 1)
	c := overSocket(serveOn(t, g, done))
	ended := make(chan error, 1)
	go func() { _, _, err := post(c, token); ended <- err }()
	<-arrived
	g.Unbind(token)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Unbind did not end the request in flight")
	}
	<-ended
	if o := g.Unbind(token); len(o.Statuses)+len(o.Refusals) != 0 {
		t.Errorf("a second Unbind: %+v", o)
	}
	g.mu.Lock()
	n, m, k := len(g.bound), len(g.observed), len(g.inflight)
	g.mu.Unlock()
	if n+m+k != 0 {
		t.Errorf("after Unbind: %d bindings, %d observations, %d in flight", n, m, k)
	}
	if s, code, err := post(c, token); err != nil || s != http.StatusUnauthorized || code != "unbound" {
		t.Errorf("a request after Unbind: %d %q %v", s, code, err)
	}
	if o := g.Unbind("never-bound"); len(o.Statuses)+len(o.Refusals) != 0 {
		t.Errorf("unbinding a token never bound: %+v", o)
	}
}

// blockingLeases holds every lease read, whatever its context, until
// release is closed.
type blockingLeases struct {
	Leases
	arrived, release chan bool
}

func (b blockingLeases) Lease(ctx context.Context, id types.LeaseID) (types.Lease, bool, error) {
	b.arrived <- true
	<-b.release
	return b.Leases.Lease(ctx, id)
}

// Opus #308 M2: a request admitted before Unbind, still on its checks when
// Unbind returns, records no intent and no floor, and sends nothing.
func TestUnbindBeforeTheIntent(t *testing.T) {
	up := newUpstream(t, nil)
	bl := blockingLeases{arrived: make(chan bool, 1), release: make(chan bool)}
	g, token := rig(t, up, func(c *Config) { bl.Leases = c.Leases; c.Leases = bl })
	done := make(chan bool, 1)
	c := overSocket(serveOn(t, g, done))
	go func() { _, _, _ = post(c, token) }()
	<-bl.arrived
	g.Unbind(token)
	close(bl.release)
	<-done
	if n, counted, sent := g.cfg.Intents.(*intents).n(), countsOf(g).all(), up.got(); n != 0 || len(counted) != 0 || len(sent) != 0 {
		t.Errorf("after Unbind: %d intents, counts %+v, %d sent", n, counted, len(sent))
	}
}

// countCapture counts the bodies it keeps.
type countCapture struct{ n *atomic.Int64 }

func (c countCapture) Keep(context.Context, Binding, []byte) error { c.n.Add(1); return nil }

// Opus #322 L4: a request admitted before Unbind, still on its checks when
// Unbind returns, records no capture (an exit run's), and sends nothing.
func TestUnbindBeforeTheCapture(t *testing.T) {
	up := newUpstream(t, nil)
	bl := blockingLeases{arrived: make(chan bool, 1), release: make(chan bool)}
	kept := new(atomic.Int64)
	g, token := rig(t, up, func(c *Config) { bl.Leases = c.Leases; c.Leases = bl; c.Capture = countCapture{kept} })
	done := make(chan bool, 1)
	c := overSocket(serveOn(t, g, done))
	go func() { _, _, _ = post(c, token) }()
	<-bl.arrived
	g.Unbind(token)
	close(bl.release)
	<-done
	if n, sent := kept.Load(), up.got(); n != 0 || len(sent) != 0 {
		t.Errorf("after Unbind: %d captured, %d sent", n, len(sent))
	}
}

// holdCapture holds every capture until release is closed, signalling it.
type holdCapture struct{ arrived, release chan bool }

func (h holdCapture) Keep(context.Context, Binding, []byte) error {
	h.arrived <- true
	<-h.release
	return nil
}

// Opus #308 M2, past the capture: a request Unbind finds inside its
// capture records no intent and no floor after it, and sends nothing
// (Unbind waits for the capture: Astra #308 2).
func TestUnbindAfterTheCapture(t *testing.T) {
	up := newUpstream(t, nil)
	hc := holdCapture{make(chan bool, 1), make(chan bool)}
	g, token := rig(t, up, func(c *Config) { c.Capture = hc })
	done := make(chan bool, 1)
	c := overSocket(serveOn(t, g, done))
	go func() { _, _, _ = post(c, token) }()
	<-hc.arrived
	unbound := make(chan bool)
	go func() { g.Unbind(token); close(unbound) }()
	select {
	case <-unbound:
		t.Fatal("Unbind returned inside the capture it admitted")
	case <-time.After(200 * time.Millisecond): // Unbind has run up to its wait
	}
	close(hc.release)
	<-unbound
	<-done
	if n, counted, sent := g.cfg.Intents.(*intents).n(), countsOf(g).all(), up.got(); n != 0 || len(counted) != 0 || len(sent) != 0 {
		t.Errorf("after Unbind: %d intents, counts %+v, %d sent", n, counted, len(sent))
	}
}

// gated holds one side effect (a capture, an intent or a floor) until
// release is closed, signalling on arrived, and marks it recorded.
type gated struct {
	arrived, release chan bool
	recorded         atomic.Bool
}

func (g *gated) hold() {
	g.arrived <- true
	<-g.release
	g.recorded.Store(true)
}

type gatedCapture struct{ *gated }

func (c gatedCapture) Keep(context.Context, Binding, []byte) error { c.hold(); return nil }

type gatedIntents struct {
	Intents
	*gated
}

func (i gatedIntents) Intend(ctx context.Context, b Binding, d types.Slug) error {
	i.hold()
	return i.Intents.Intend(context.WithoutCancel(ctx), b, d) // as one that never reads its context
}

type gatedFloors struct {
	Counts
	*gated
}

func (c gatedFloors) Count(ctx context.Context, b Binding, seq int64, n Count) error {
	if !n.Observed {
		c.hold()
	}
	return c.Counts.Count(ctx, b, seq, n)
}

// Astra #308 2: a request held inside its capture, its intent or its
// floor's commit, already admitted when Unbind runs: Unbind returns only
// once that side effect is recorded, and the request records nothing
// after it.
func TestUnbindWaitsForWhatItAdmitted(t *testing.T) {
	for _, at := range []string{"capture", "intent", "floor"} {
		t.Run(at, func(t *testing.T) {
			up := newUpstream(t, nil)
			gt := &gated{arrived: make(chan bool, 1), release: make(chan bool)}
			var cs *counts
			g, token := rig(t, up, func(c *Config) {
				cs = c.Counts.(*counts)
				switch at {
				case "capture":
					c.Capture = gatedCapture{gt}
				case "intent":
					c.Intents = gatedIntents{c.Intents, gt}
				case "floor":
					c.Counts = gatedFloors{c.Counts, gt}
				}
			})
			done := make(chan bool, 1)
			c := overSocket(serveOn(t, g, done))
			go func() { _, _, _ = post(c, token) }()
			<-gt.arrived
			unbound := make(chan bool)
			go func() { g.Unbind(token); unbound <- gt.recorded.Load() }()
			select {
			case <-unbound:
				t.Fatal("Unbind returned while a side effect it admitted was still running")
			case <-time.After(200 * time.Millisecond):
			}
			close(gt.release)
			if recorded := <-unbound; !recorded {
				t.Error("Unbind returned before the admitted side effect was recorded")
			}
			floors := cs.all()
			<-done
			if n := len(up.got()); n != 0 || len(cs.all()) != len(floors) {
				t.Errorf("after Unbind: %d sent, counts %+v (at Unbind's return %+v)", n, cs.all(), floors)
			}
		})
	}
}

// Opus #308 L3: Server's bounds, and a client that never finishes its
// headers is closed.
func TestServerBounds(t *testing.T) {
	g, _ := rig(t, newUpstream(t, nil), nil)
	if srv := g.Server(); srv.Handler != g || srv.ReadHeaderTimeout != readHeaderTimeout || srv.ReadTimeout != time.Minute ||
		srv.WriteTimeout != RequestDeadline+time.Minute || srv.IdleTimeout != time.Minute {
		t.Errorf("the server's bounds: %+v", srv)
	}
	old := readHeaderTimeout
	readHeaderTimeout = 200 * time.Millisecond
	t.Cleanup(func() { readHeaderTimeout = old })
	conn, err := net.Dial("unix", serveOn(t, g, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("POST /chat/completions HTTP/1.1\r\nHost: adc-"))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("a client that never finishes its headers: %v, want the server to close it", err)
	}
}

// Opus #308 L5: a ResponseWriter that cannot take a write deadline ends the
// request: nothing is written without one, and the call keeps its floor.
//
// Astra #308 3: so does a refusal (an unbound token, the cap): it aborts,
// writing nothing, and sends nothing.
func TestNoWriteWithoutADeadline(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":1}}`))
	})
	g, token := rig(t, up, nil)
	for name, tok := range map[string]string{"a refusal": "not-a-token", "a forwarded response": token} {
		r, _ := http.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(good))
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder() // no SetWriteDeadline
		func() {
			defer func() {
				if p := recover(); p != http.ErrAbortHandler {
					t.Errorf("%s: %v, want the handler aborted", name, p)
				}
			}()
			g.ServeHTTP(w, r)
		}()
		if w.Body.Len() != 0 || w.Code != http.StatusOK || len(w.Header()) != 0 {
			t.Errorf("%s: written without a deadline: %d %v %q", name, w.Code, w.Header(), w.Body)
		}
	}
	if n := len(up.got()); n != 0 {
		t.Errorf("%d sent", n)
	}
}

// Astra #308 1: the count's latency never expires the response's end: a
// count held past writeIdle, the adapter reads the whole chunked response
// to its EOF.
func TestASlowCountEndsTheResponseWhole(t *testing.T) {
	old := writeIdle
	writeIdle = 200 * time.Millisecond
	t.Cleanup(func() { writeIdle = old })
	const usage = `{"usage":{"prompt_tokens":10,"completion_tokens":1}}`
	g, token := rig(t, newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(usage))
	}), nil)
	slow := &holdCounts{Counts: g.cfg.Counts, hold: make(chan bool), held: make(chan bool, 8)}
	g.cfg.Counts = slow
	go func() {
		<-slow.held
		time.Sleep(5 * writeIdle) // the count commits well past the last write's deadline
		close(slow.hold)
	}()
	r, _ := http.NewRequest(http.MethodPost, BaseURL+"/chat/completions", strings.NewReader(good))
	r.Header.Set("Authorization", "Bearer "+token)
	done := make(chan bool, 1)
	t.Cleanup(func() { <-done }) // the handler has ended: nothing reads writeIdle as it is reset
	resp, err := overSocket(serveOn(t, g, done)).Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != usage || len(resp.TransferEncoding) == 0 {
		t.Errorf("the response: %q %v (transfer %v), want it whole to its EOF", body, err, resp.TransferEncoding)
	}
}

// deadlines is a recorder that takes write deadlines, recording each, and
// whether the call's usage count had committed when it was set.
type deadlines struct {
	*httptest.ResponseRecorder
	counted *atomic.Bool
	set     []deadline
}

type deadline struct {
	at      time.Time
	counted bool
}

func (d *deadlines) SetWriteDeadline(at time.Time) error {
	d.set = append(d.set, deadline{at, d.counted.Load()})
	return nil
}

type markCounts struct {
	Counts
	counted *atomic.Bool
}

func (m markCounts) Count(ctx context.Context, b Binding, seq int64, n Count) error {
	err := m.Counts.Count(ctx, b, seq, n)
	m.counted.Store(m.counted.Load() || n.Observed)
	return err
}

// Astra #308 1: each write's deadline is cleared after it, and once the
// usage count has committed the response's end gets a fresh deadline: the
// last deadline set is a time, set after the count.
func TestTheResponseEndHasAFreshDeadline(t *testing.T) {
	g, token := rig(t, newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":1}}`))
	}), nil)
	counted := new(atomic.Bool)
	g.cfg.Counts = markCounts{g.cfg.Counts, counted}
	r, _ := http.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(good))
	r.Header.Set("Authorization", "Bearer "+token)
	w := &deadlines{ResponseRecorder: httptest.NewRecorder(), counted: counted}
	g.ServeHTTP(w, r)
	cleared := slices.ContainsFunc(w.set, func(d deadline) bool { return d.at.IsZero() && !d.counted }) // the request's first deadline
	if last := w.set[len(w.set)-1]; w.Code != http.StatusOK || !counted.Load() || !cleared || last.at.IsZero() || !last.counted {
		t.Errorf("the deadlines set: %+v (counted %v, status %d)", w.set, counted.Load(), w.Code)
	}
}

// order is a recorder that takes write deadlines and records, in order,
// each deadline set (zero: cleared) and each header written.
type order struct {
	*httptest.ResponseRecorder
	events []string
}

func (o *order) SetWriteDeadline(at time.Time) error {
	o.events = append(o.events, map[bool]string{true: "clear", false: "arm"}[at.IsZero()])
	return nil
}

func (o *order) WriteHeader(code int) {
	o.events = append(o.events, "header")
	o.ResponseRecorder.WriteHeader(code)
}

// Opus #308 r2 M1: a refusal arms its own deadline right before its
// header, after the request's first one was cleared.
func TestARefusalArmsItsOwnDeadline(t *testing.T) {
	g, _ := rig(t, newUpstream(t, nil), nil)
	r, _ := http.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(good))
	r.Header.Set("Authorization", "Bearer not-a-token")
	w := &order{ResponseRecorder: httptest.NewRecorder()}
	g.ServeHTTP(w, r)
	if i := slices.Index(w.events, "header"); w.Code != http.StatusUnauthorized || i < 2 || w.events[i-1] != "arm" || !slices.Contains(w.events[:i-1], "clear") {
		t.Errorf("a refusal's deadlines and header: %v (status %d)", w.events, w.Code)
	}
}

// Opus #308 r2 M1: a refusal decided past writeIdle (an upstream slower
// than it, answering with a Content-Encoding) reaches the adapter whole: a
// refusal arms its own deadline.
func TestALateRefusalIsWhole(t *testing.T) {
	old := writeIdle
	writeIdle = 200 * time.Millisecond
	t.Cleanup(func() { writeIdle = old })
	g, token := rig(t, newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * writeIdle)
		w.Header().Set("Content-Encoding", "br")
		_, _ = w.Write([]byte("x"))
	}), nil)
	done := make(chan bool, 1)
	s, code, err := post(overSocket(serveOn(t, g, done)), token)
	if err != nil || s != http.StatusBadGateway || code != "encoding" {
		t.Errorf("a refusal past writeIdle: %d %q %v, want 502 encoding", s, code, err)
	}
	<-done // the handler has ended: nothing reads writeIdle as it is reset
}

// slowLeases answers every lease read after wait.
type slowLeases struct {
	Leases
	wait time.Duration
}

func (l slowLeases) Lease(ctx context.Context, id types.LeaseID) (types.Lease, bool, error) {
	time.Sleep(l.wait)
	return l.Leases.Lease(ctx, id)
}

// Opus #308 r2 M1, its 100 Continue variant: a request with Expect:
// 100-continue whose lease read takes longer than writeIdle still has its
// 100 Continue and then its whole response.
func TestALate100ContinueIsWritten(t *testing.T) {
	old := writeIdle
	writeIdle = 200 * time.Millisecond
	t.Cleanup(func() { writeIdle = old })
	const usage = `{"usage":{"prompt_tokens":10,"completion_tokens":1}}`
	g, token := rig(t, newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(usage))
	}), func(c *Config) { c.Leases = slowLeases{c.Leases, 5 * writeIdle} })
	done := make(chan bool, 1)
	t.Cleanup(func() { <-done }) // the handler has ended: nothing reads writeIdle as it is reset
	conn, err := net.Dial("unix", serveOn(t, g, done))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "POST /chat/completions HTTP/1.1\r\nHost: adc-gateway\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nExpect: 100-continue\r\n\r\n", token, len(good))
	br := bufio.NewReader(conn)
	cont, err := http.ReadResponse(br, nil)
	if err != nil || cont.StatusCode != http.StatusContinue {
		t.Fatalf("the 100 Continue: %v %v", cont, err)
	}
	_, _ = io.WriteString(conn, good)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if body, err := io.ReadAll(resp.Body); err != nil || resp.StatusCode != http.StatusOK || string(body) != usage {
		t.Errorf("the response after a late 100 Continue: %d %q %v", resp.StatusCode, body, err)
	}
}

// panicCapture panics on every capture.
type panicCapture struct{}

func (panicCapture) Keep(context.Context, Binding, []byte) error { panic("capture") }

// Opus #308 r2 L1: a side effect that panics still ends: with another
// request of the same token in flight, Unbind returns.
func TestUnbindAfterAPanickingSideEffect(t *testing.T) {
	arrived, release := make(chan bool, 1), make(chan bool)
	held := true
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		if held {
			held = false
			arrived <- true
			<-release
		}
	})
	t.Cleanup(sync.OnceFunc(func() { close(release) }))
	g, token := rig(t, up, nil)
	done := make(chan bool, 4)
	c := overSocket(serveOn(t, g, done))
	go func() { _, _, _ = post(c, token) }()
	<-arrived // the first request is in flight, upstream
	g.cfg.Capture = panicCapture{}
	_, _, _ = post(c, token) // the second's capture panics; net/http recovers it
	<-done
	unbound := make(chan bool)
	go func() { g.Unbind(token); close(unbound) }()
	select {
	case <-unbound:
	case <-time.After(5 * time.Second):
		t.Fatal("Unbind did not return after a side effect panicked")
	}
}

// Astra #308 4: one request past the cap, its body withheld and no
// Expect: 100-continue, has its 429 at once and its connection closed;
// nothing of its body is waited for.
func TestTheCapNeverWaitsForABody(t *testing.T) {
	arrived, release := make(chan bool, 8), make(chan bool)
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		arrived <- true
		<-release
	})
	t.Cleanup(sync.OnceFunc(func() { close(release) }))
	g, token := rig(t, up, nil)
	sock := serveOn(t, g, nil)
	c := overSocket(sock)
	for range MaxInFlight {
		go func() { _, _, _ = post(c, token) }()
		<-arrived
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second)) // a drain would wait for the server's one-minute read timeout
	fmt.Fprintf(conn, "POST /chat/completions HTTP/1.1\r\nHost: adc-gateway\r\nAuthorization: Bearer %s\r\nContent-Length: 1000\r\n\r\n", token)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get(RefusalHeader) != "in_flight_cap" || !resp.Close {
		t.Fatalf("a request past the cap: %v %v; want 429 in_flight_cap, the connection closed", resp, err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Errorf("the refusal's body: %v", err)
	}
	if _, err := br.ReadByte(); err != io.EOF {
		t.Errorf("after the refusal: %v, want the connection closed", err)
	}
}
