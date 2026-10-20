// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
// it is refused before its body is read, nothing sent; the slot frees when
// a request ends; another token is not counted against it.
func TestInFlightCap(t *testing.T) {
	arrived, release := make(chan bool, 8), make(chan bool)
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		arrived <- true
		<-release
		_, _ = w.Write([]byte(`{"choices":[]}`))
	})
	g, token := rig(t, up, nil)
	c := overSocket(serveOn(t, g, nil))
	type res struct {
		status int
		code   string
	}
	results := make(chan res, MaxInFlight)
	for range MaxInFlight {
		go func() { s, code, _ := post(c, token); results <- res{s, code} }()
		<-arrived
	}
	if s, code, err := post(c, token); err != nil || s != http.StatusTooManyRequests || code != "in_flight_cap" {
		t.Errorf("a request past the cap: %d %q %v; want 429 in_flight_cap", s, code, err)
	}
	if n := len(up.got()); n != MaxInFlight {
		t.Errorf("%d requests reached the upstream, want %d", n, MaxInFlight)
	}
	other, err := g.Bind(Binding{Lease: leaseID, Attempt: attempt, Call: "cal_01m4tm99haemss9tvzd438v000"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { s, code, _ := post(c, other); results <- res{s, code} }()
	<-arrived // another token's request is admitted
	close(release)
	for range MaxInFlight + 1 {
		if r := <-results; r.status != http.StatusOK {
			t.Errorf("an admitted request: %+v", r)
		}
	}
	if s, _, err := post(c, token); err != nil || s != http.StatusOK { // the slots are free again
		t.Errorf("after the requests end: %d %v", s, err)
	}
	if len(g.inflight) != 0 {
		t.Errorf("in flight after every request ended: %v", g.inflight)
	}
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
