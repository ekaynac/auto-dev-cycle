// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/buildinfo"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// served is the assembled server over a seeded store, with an operator
// holding a token.
type served struct {
	dir string
	w   *seeder
	srv *server
	op  types.ActorID
	tok identity.Secret
}

func serveStore(t *testing.T, lim limits, shared *SharedSocket) *served {
	t.Helper()
	s := &served{dir: shortDir(t)}
	s.w = seed(t, s.dir, daemonProjection(t))
	t.Cleanup(s.w.done)
	sys := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
	sys.CreatedBy = sys.ID
	op := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindOperator, Name: "operator", CreatedBy: sys.ID}
	s.op = op.ID
	s.w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(sys) })
	s.w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(op) })
	s.w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
		evs, sec, err := identity.IssueToken(op.ID, sys.ID, time.Now())
		s.tok = sec
		return evs, err
	})
	var err error
	if s.srv, err = listen(s.w.s, s.dir, shared, lim); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.srv.close() })
	return s
}

func unixClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}}
}

func (s *served) client() *http.Client { return unixClient(filepath.Join(s.dir, privateSocket)) }

func (s *served) get(t *testing.T, c *http.Client, path, bearer string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://adcd"+path, nil)
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

type frame struct {
	event string
	data  string
}

// frames reads a stream's frames until it ends.
func frames(resp *http.Response) <-chan frame {
	out := make(chan frame, 4096)
	go func() {
		defer close(out)
		sc, fr := bufio.NewScanner(resp.Body), frame{}
		for sc.Scan() {
			name, value, _ := strings.Cut(sc.Text(), ": ")
			switch name {
			case "":
				out <- fr
				fr = frame{}
			case "event":
				fr.event = value
			case "data":
				fr.data = value
			}
		}
	}()
	return out
}

// closedWith reads frames until the closed one and checks its result.
func closedWith(t *testing.T, c <-chan frame, o vocab.Outcome, reason vocab.Reason) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case fr, ok := <-c:
			if !ok {
				t.Fatal("the stream ended without a closed frame")
			}
			if fr.event != "closed" {
				continue
			}
			var r vocab.Result
			if err := json.Unmarshal([]byte(fr.data), &r); err != nil || r.Outcome != o || r.Reason != reason {
				t.Fatalf("closed with %s (%v)", fr.data, err)
			}
			return
		case <-timeout:
			t.Fatal("no closed frame")
		}
	}
}

func (s *served) tokenID() string {
	id, _, _ := strings.Cut(s.tok.Reveal(), ".")
	return id
}

// The event stream through the assembled server: it outlives the write
// timeout, closes on revocation, and reports an unreadable store as such.
func TestStreamThroughServer(t *testing.T) {
	lim := defaultLimits
	lim.poll, lim.writeTimeout = 100*time.Millisecond, 200*time.Millisecond
	t.Run("revocation", func(t *testing.T) {
		s := serveStore(t, lim, nil)
		stream := frames(s.get(t, s.client(), api.EventsPath, s.tok.Reveal()))
		time.Sleep(3 * lim.writeTimeout) // an idle stream is not cut by the server's write timeout
		s.w.do(s.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return identity.RevokeToken(tx, types.TokenID(s.tokenID()), time.Now())
		})
		closedWith(t, stream, vocab.Failed, vocab.ReasonAuthorityRevoked)
	})
	t.Run("an unreadable store", func(t *testing.T) {
		s := serveStore(t, lim, nil)
		stream := frames(s.get(t, s.client(), api.EventsPath, s.tok.Reveal()))
		s.w.do(s.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return nil, tx.Put(identity.TokenProjection, s.tokenID(), json.RawMessage(`[]`))
		})
		closedWith(t, stream, vocab.CouldNotRun, vocab.ReasonStoreUnreadable)
	})
}

// A client that stops reading is cut off: the connection closes within the
// poll bound once its socket buffers fill, and no closed frame is sent.
func TestStalledReaderCutOff(t *testing.T) {
	lim := defaultLimits
	lim.poll = 200 * time.Millisecond
	s := serveStore(t, lim, nil)
	const total = 3000
	for range total / 500 {
		s.w.do(s.op, func(*eventstore.Tx) ([]eventstore.Event, error) {
			var evs []eventstore.Event
			for range 500 {
				e, err := identity.CreateActor(types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSupervisor,
					Name: strings.Repeat("s", 200), CreatedBy: s.op})
				if err != nil {
					return nil, err
				}
				evs = append(evs, e...)
			}
			return evs, nil
		})
	}
	resp := s.get(t, s.client(), api.EventsPath, s.tok.Reveal())
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

func TestVersionEndpoint(t *testing.T) {
	s := serveStore(t, defaultLimits, nil)
	resp := s.get(t, s.client(), api.VersionPath, "")
	var got buildinfo.Info
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != 200 || got != buildinfo.Get("adcd") || got.SchemaSet == "" {
		t.Fatalf("%s %+v %v", resp.Status, got, err)
	}
	if resp := s.get(t, s.client(), "/v1/commands/task.create", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unmounted route: %s", resp.Status)
	}
}

// Shutdown ends open streams, stops accepting and removes the sockets it
// made; the directory then takes a new server.
func TestShutdownRemovesSockets(t *testing.T) {
	run := runDir(t, 0o750)
	s := serveStore(t, defaultLimits, &SharedSocket{RunDir: run, Group: egid()})
	stream := frames(s.get(t, s.client(), api.EventsPath, s.tok.Reveal()))
	start := time.Now()
	if err := s.srv.close(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > defaultLimits.shutdown {
		t.Errorf("shutdown took %s", took)
	}
	for range stream {
	}
	gone(t, filepath.Join(s.dir, privateSocket))
	gone(t, filepath.Join(run, sharedSocket))
	again, err := listen(s.w.s, s.dir, nil, defaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	again.close()
}

// Header and body sizes and the header read time are bounded.
func TestRequestLimits(t *testing.T) {
	lim := defaultLimits
	lim.headerTimeout = 200 * time.Millisecond
	s := serveStore(t, lim, nil)
	req, _ := http.NewRequest(http.MethodGet, "http://adcd"+api.VersionPath, nil)
	req.Header.Set("X-Big", strings.Repeat("a", 64<<10))
	if resp, err := s.client().Do(req); err != nil || resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("a 64 KiB header: %v %v", resp, err)
	}
	req, _ = http.NewRequest(http.MethodGet, "http://adcd"+api.VersionPath, strings.NewReader(strings.Repeat("a", int(lim.bodyBytes)+1)))
	if resp, err := s.client().Do(req); err != nil || resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over the limit: %v %v", resp, err)
	}
	c, err := net.Dial("unix", filepath.Join(s.dir, privateSocket))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "GET /v1/version HTTP/1.1\r\nHost: adcd\r\n"); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, err := io.ReadAll(c)
	if err != nil || strings.Contains(string(b), "200 OK") {
		t.Errorf("a slow header was not cut off: %q %v", b, err)
	}
}

// Bearer verifications are capped per listener and, on the shared socket,
// per peer: a full shared gate refuses at once with fixed text and never
// delays or refuses the private socket; the private socket has its own cap.
func TestVerificationGate(t *testing.T) {
	s := serveStore(t, defaultLimits, &SharedSocket{RunDir: runDir(t, 0o750), Group: egid()})
	private, shared := s.srv.sockets[0], s.srv.sockets[1]
	sharedClient := unixClient(filepath.Join(shared.dir.Name(), sharedSocket))
	uid := os.Geteuid()
	// The shared flood: this peer's one slot, and another peer's.
	r1, ok1 := shared.verify.Take(uid)
	r2, ok2 := shared.verify.Take(uid + 1)
	if !ok1 || !ok2 {
		t.Fatal("the shared gate has fewer than two peers' slots")
	}
	resp := s.get(t, sharedClient, api.EventsPath, s.tok.Reveal())
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" || strings.Contains(string(b), s.tok.Reveal()) {
		t.Fatalf("a full shared gate: %s %q", resp.Status, b)
	}
	start := time.Now()
	if resp := s.get(t, s.client(), api.EventsPath, s.tok.Reveal()); resp.StatusCode != http.StatusOK || time.Since(start) > 5*time.Second {
		t.Fatalf("the private socket under a shared flood: %s after %s", resp.Status, time.Since(start))
	}
	r1()
	r1() // a release counts once
	r2()
	if resp := s.get(t, sharedClient, api.EventsPath, s.tok.Reveal()); resp.StatusCode != http.StatusOK {
		t.Fatalf("a free shared gate: %s", resp.Status)
	}
	for range defaultLimits.private.verify {
		if _, ok := private.verify.Take(uid); !ok {
			t.Fatal("the private gate is smaller than its quota")
		}
	}
	if resp := s.get(t, s.client(), api.EventsPath, s.tok.Reveal()); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a full private gate: %s", resp.Status)
	}
}

// One peer cannot take every slot: it is held to its own share, and other
// peers take the rest up to the total.
func TestSlotsPerPeer(t *testing.T) {
	sl := api.NewSlots(4, 2)
	for _, c := range []struct {
		peer int
		ok   bool
	}{{1, true}, {1, true}, {1, false}, {2, true}, {2, true}, {3, false}} {
		if _, ok := sl.Take(c.peer); ok != c.ok {
			t.Fatalf("peer %d took a slot: %v", c.peer, ok)
		}
	}
}

// rawGet sends GET path on a new connection to sock and returns the
// status line, keeping the connection open until the test ends.
func rawGet(t *testing.T, sock, path string) string {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: adcd\r\n\r\n"); err != nil {
		return err.Error()
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err.Error()
	}
	return strings.TrimSpace(line)
}

// Connections, and so open streams, are capped per listener and per peer
// on the shared socket; one past the cap is refused at once with fixed
// text, and a closed connection frees its slot. The shared socket's
// connections never take the private socket's.
func TestConnectionQuota(t *testing.T) {
	lim := defaultLimits
	lim.private = slotQuota{conns: 2, verify: 2}
	lim.shared = slotQuota{conns: 4, connsPerPeer: 2, verify: 2, verifyPerPeer: 1}
	run := runDir(t, 0o750)
	s := serveStore(t, lim, &SharedSocket{RunDir: run, Group: egid()})
	sharedSock, privateSock := filepath.Join(run, sharedSocket), filepath.Join(s.dir, privateSocket)
	// One peer's two streams fill its share of the shared socket.
	var streams []*http.Response
	for range 2 {
		streams = append(streams, s.get(t, unixClient(sharedSock), api.EventsPath, s.tok.Reveal()))
	}
	if got := rawGet(t, sharedSock, api.VersionPath); got != "HTTP/1.1 503 Service Unavailable" {
		t.Fatalf("a third stream's connection: %q", got)
	}
	for range 2 {
		if got := rawGet(t, privateSock, api.VersionPath); got != "HTTP/1.1 200 OK" {
			t.Fatalf("the private socket beside a full shared peer: %q", got)
		}
	}
	if got := rawGet(t, privateSock, api.VersionPath); got != "HTTP/1.1 503 Service Unavailable" {
		t.Fatalf("one past the private cap: %q", got)
	}
	streams[0].Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for rawGet(t, sharedSock, api.VersionPath) != "HTTP/1.1 200 OK" {
		if time.Now().After(deadline) {
			t.Fatal("a closed stream did not free its slot")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
