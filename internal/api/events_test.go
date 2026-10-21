// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

var (
	prjA = "prj_01m3250vkwffc979nzjw0g2sa1"
	prjB = "prj_01m3250vkwffc979nzjw0g2sb2"
	t0   = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
)

// netPoll is the poll bound of ordinary network tests: it is also their
// write deadline, so it leaves scheduling headroom (under -race too). Only
// the controlled timeout tests use shorter ones.
const netPoll = 100 * time.Millisecond

type applyFn = func(*eventstore.Tx) ([]eventstore.Event, error)

type fixture struct {
	t            *testing.T
	s            *eventstore.Store
	srv          *httptest.Server
	now          atomic.Int64 // the server's clock, unix nanoseconds
	sys, op, sup types.Actor
	opTok, supTk identity.Secret
	grant        types.Grant
}

func newID(t *testing.T, prefix string) string {
	t.Helper()
	v, err := ids.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (f *fixture) must(by types.ActorID, apply applyFn) {
	f.t.Helper()
	cmd := newID(f.t, "cmd")
	if _, err := f.s.SubmitApplied(context.Background(), eventstore.Command{ID: cmd, Actor: string(by), Action: "test.command", IdempotencyKey: cmd, Input: []byte(`{}`)}, eventstore.Applies(
		func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := apply(tx)
			return evs, json.RawMessage(`{}`), err
		})); err != nil {
		f.t.Fatal(err)
	}
}

func issue(holder, by types.ActorID, out *identity.Secret) applyFn {
	return func(*eventstore.Tx) ([]eventstore.Event, error) {
		evs, s, err := identity.IssueToken(holder, by, t0)
		*out = s
		return evs, err
	}
}

// appendTo appends one event of kind on project (none when empty).
func (f *fixture) appendTo(kind, project string) {
	f.must(f.op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
		e := eventstore.Event{ID: newID(f.t, "evt"), Kind: kind + "/v1", Subject: prjA, Payload: []byte(`{}`)}
		if project != "" {
			e.Project = &project
		}
		return []eventstore.Event{e}, nil
	})
}

// open starts a store and the stream server, with an operator and a
// supervisor holding a grant that reads fixture events on project A.
func open(t *testing.T) *fixture {
	dir := filepath.Join(t.TempDir(), "state")
	lock, err := eventstore.AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Release() })
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	kinds := identity.Kinds()
	maps.Copy(kinds, authority.GrantKinds())
	maps.Copy(kinds, authority.ProposalKinds())
	maps.Copy(kinds, authority.SeatKinds())
	maps.Copy(kinds, authority.NoteKinds())
	maps.Copy(kinds, authority.ProjectKinds())
	maps.Copy(kinds, authority.TaskKinds())
	maps.Copy(kinds, graph.Kinds())
	for _, k := range []string{"fixture.appended/v1", "lease.granted/v1"} {
		kinds[k] = eventstore.Kind{Schema: "common/defs"}
	}
	f := &fixture{t: t}
	f.now.Store(t0.UnixNano())
	// The store and the stream share one clock: a grant is valid from its
	// event's time (authority.CreateGrant).
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: kinds, Blobs: blobs, Now: func() time.Time { return time.Unix(0, f.now.Load()).UTC() },
		Hooks: eventstore.Hooks{Identify: func(*eventstore.Tx, eventstore.Command) error { return nil }, Authorize: func(*eventstore.Tx, eventstore.Command) error { return nil },
			Project: eventstore.ProjectEach(identity.Projector{}.Project, authority.GrantProjector{}.Project, authority.ProposalProjector{}.Project,
				authority.SeatProjector{}.Project, authority.NoteProjector{}.Project, authority.ProjectProjector{}.Project,
				authority.TaskProjector{}.Project, graph.ProjectM0)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f.s = s
	sys := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
	sys.CreatedBy = sys.ID
	f.sys = sys
	f.op = types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindOperator, Name: "operator", CreatedBy: sys.ID}
	f.sup = types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSupervisor, Name: "supervisor", CreatedBy: f.op.ID}
	for _, a := range []struct {
		by types.ActorID
		a  types.Actor
	}{{sys.ID, sys}, {sys.ID, f.op}, {f.op.ID, f.sup}} {
		f.must(a.by, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(a.a) })
	}
	f.must(sys.ID, issue(f.op.ID, sys.ID, &f.opTok))
	f.must(f.op.ID, issue(f.sup.ID, f.op.ID, &f.supTk))
	f.grant = f.grantFor("fixture")
	f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(tx, f.grant) })
	f.srv = httptest.NewServer(f.handler(netPoll))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) handler(poll time.Duration) api.Events {
	return api.Events{Store: f.s, Poll: poll, Gate: api.Unlimited(), Now: func() time.Time { return time.Unix(0, f.now.Load()) }}
}

func (f *fixture) grantFor(kinds ...types.Name) types.Grant {
	return types.Grant{ID: types.GrantID(newID(f.t, "grt")), Actor: f.sup.ID, Projects: types.ProjectScope{List: []types.ProjectID{types.ProjectID(prjA)}},
		Actions: []types.ActionKind{}, Reads: types.GrantReads{Projects: types.GrantReadsProjects{Same: true}, Kinds: kinds},
		ValidFrom: types.Time(t0.Add(-time.Hour).Format(time.RFC3339)), ExpiresAt: types.Time(t0.Add(time.Hour).Format(time.RFC3339)),
		ConfirmationOverrides: map[string]types.GrantConfirmationOverridesValue{}, IssuedBy: f.op.ID}
}

func (f *fixture) get(method, query, bearer string) *http.Response {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f.t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, method, f.srv.URL+api.EventsPath+query, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

type frame struct {
	event string
	raw   string
	ev    eventstore.Event
	res   vocab.Result
}

// frames reads a stream's frames until it ends.
func frames(t *testing.T, resp *http.Response) <-chan frame {
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("the stream opened with %s %s", resp.Status, resp.Header.Get("Content-Type"))
	}
	out := make(chan frame, 64)
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
				fr.raw = value
				var err error
				if fr.event == "event" {
					err = json.Unmarshal([]byte(value), &fr.ev)
				} else {
					err = json.Unmarshal([]byte(value), &fr.res)
				}
				if err != nil {
					t.Errorf("frame data %s: %v", value, err)
				}
			}
		}
	}()
	return out
}

func next(t *testing.T, c <-chan frame) (frame, bool) {
	t.Helper()
	select {
	case f, ok := <-c:
		return f, ok
	case <-time.After(5 * time.Second):
		t.Fatal("no frame")
	}
	return frame{}, false
}

func TestStreamRefusals(t *testing.T) {
	f := open(t)
	for name, c := range map[string]struct {
		method, query, bearer string
		status                int
	}{
		"no bearer":           {http.MethodGet, "", "", http.StatusUnauthorized},
		"a wrong bearer":      {http.MethodGet, "", f.supTk.Reveal() + "x", http.StatusUnauthorized},
		"a post":              {http.MethodPost, "", f.supTk.Reveal(), http.StatusMethodNotAllowed},
		"an unknown param":    {http.MethodGet, "?since=3", f.supTk.Reveal(), http.StatusBadRequest},
		"a negative after":    {http.MethodGet, "?after=-1", f.supTk.Reveal(), http.StatusBadRequest},
		"a repeated param":    {http.MethodGet, "?after=1&after=2", f.supTk.Reveal(), http.StatusBadRequest},
		"a malformed project": {http.MethodGet, "?project=nope", f.supTk.Reveal(), http.StatusBadRequest},
		"an empty kind":       {http.MethodGet, "?kinds=a,,b", f.supTk.Reveal(), http.StatusBadRequest},
	} {
		if resp := f.get(c.method, c.query, c.bearer); resp.StatusCode != c.status {
			t.Errorf("%s: %s, want %d", name, resp.Status, c.status)
		}
	}
	// A supervisor with no grant in force is refused with the reason.
	f.now.Store(t0.Add(2 * time.Hour).UnixNano())
	resp := f.get(http.MethodGet, "", f.supTk.Reveal())
	var r vocab.Result
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || resp.StatusCode != http.StatusForbidden || r.Reason != vocab.ReasonActionNotGranted {
		t.Fatalf("%s %+v %v", resp.Status, r, err)
	}
}

// A supervisor's stream carries only what its grant reads; the caller's
// filters narrow it further, and after resumes it past a seq. An operator
// reads everything.
func TestStreamFilters(t *testing.T) {
	f := open(t)
	f.appendTo("fixture.appended", prjA) // read
	f.appendTo("fixture.appended", prjB) // another project
	f.appendTo("lease.granted", prjA)    // another kind
	f.appendTo("fixture.appended", "")   // outside any project
	f.appendTo("fixture.appended", prjA) // read
	sup := frames(t, f.get(http.MethodGet, "", f.supTk.Reveal()))
	first, _ := next(t, sup)
	second, _ := next(t, sup)
	for _, fr := range []frame{first, second} {
		if fr.event != "event" || fr.ev.Kind != "fixture.appended/v1" || fr.ev.Project == nil || *fr.ev.Project != prjA {
			t.Fatalf("the supervisor got %+v", fr)
		}
	}
	resumed := frames(t, f.get(http.MethodGet, "?after="+strconv.FormatInt(first.ev.Seq, 10), f.supTk.Reveal()))
	if fr, _ := next(t, resumed); fr.ev.Seq != second.ev.Seq {
		t.Errorf("resumed after %d at %d, want %d", first.ev.Seq, fr.ev.Seq, second.ev.Seq)
	}
	op := frames(t, f.get(http.MethodGet, "?kinds=lease.granted,fixture.appended/v1&project="+prjB, f.opTok.Reveal()))
	if fr, _ := next(t, op); fr.ev.Kind != "fixture.appended/v1" || *fr.ev.Project != prjB {
		t.Errorf("the operator's filtered stream gave %+v", fr)
	}
	f.appendTo("lease.granted", prjB)
	if fr, _ := next(t, op); fr.ev.Kind != "lease.granted/v1" {
		t.Errorf("the operator's filtered stream gave %+v", fr)
	}
}

// The stream closes with failed/authority_revoked, before any later event,
// when the grant is revoked, expires or is narrowed, or the token or actor
// is revoked.
func TestStreamClosesWhenAuthorityEnds(t *testing.T) {
	for name, end := range map[string]func(f *fixture){
		"a revoked grant": func(f *fixture) {
			f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return authority.RevokeGrant(tx, f.sup.ID, f.grant.ID, t0)
			})
		},
		"an expired grant": func(f *fixture) { f.now.Store(t0.Add(time.Hour).UnixNano()) },
		"a narrowed grant": func(f *fixture) {
			f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return authority.RevokeGrant(tx, f.sup.ID, f.grant.ID, t0)
			})
			f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return authority.CreateGrant(tx, f.grantFor("lease"))
			})
		},
		"a revoked token": func(f *fixture) {
			id, _, _ := strings.Cut(f.supTk.Reveal(), ".")
			f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return identity.RevokeToken(tx, types.TokenID(id), t0)
			})
		},
		"a revoked actor": func(f *fixture) {
			f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return identity.RevokeActor(tx, f.sup.ID, t0) })
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := open(t)
			stream := frames(t, f.get(http.MethodGet, "", f.supTk.Reveal()))
			f.appendTo("fixture.appended", prjA)
			if fr, _ := next(t, stream); fr.event != "event" {
				t.Fatalf("before: %+v", fr)
			}
			end(f)
			f.appendTo("fixture.appended", prjA)
			fr, _ := next(t, stream)
			if fr.event != "closed" || fr.res.Outcome != vocab.Failed || fr.res.Reason != vocab.ReasonAuthorityRevoked {
				t.Fatalf("after: %+v", fr)
			}
			if _, more := next(t, stream); more {
				t.Fatal("the stream went on after it closed")
			}
		})
	}
}

// The stream never carries credential material: an operator reading the
// bootstrap's token.issued events gets no hash, and a kind with no
// allow-list entry streams with its payload withheld.
func TestStreamCarriesNoCredentials(t *testing.T) {
	f := open(t)
	f.appendTo("fixture.appended", prjA)
	stream := frames(t, f.get(http.MethodGet, "", f.opTok.Reveal()))
	tokens := 0
	for tokens < 2 {
		fr, ok := next(t, stream)
		if !ok {
			t.Fatal("the stream ended")
		}
		if strings.Contains(string(fr.ev.Payload), `"hash"`) || strings.Contains(fr.raw, "argon2") {
			t.Fatalf("a frame carries credential material: %s", fr.raw)
		}
		if fr.ev.Kind == "token.issued/v1" {
			tokens++
			var p struct {
				Token map[string]any `json:"token"`
			}
			if err := json.Unmarshal(fr.ev.Payload, &p); err != nil || p.Token["id"] == nil || p.Token["actor"] == nil {
				t.Fatalf("a token event lost its allowed fields: %s %v", fr.ev.Payload, err)
			}
		}
	}
	for {
		fr, _ := next(t, stream)
		if fr.ev.Kind == "fixture.appended/v1" {
			if string(fr.ev.Payload) != "null" || !strings.Contains(fr.raw, `"payload_withheld":true`) {
				t.Fatalf("an unlisted kind streamed its payload: %s", fr.raw)
			}
			return
		}
	}
}

// pick keeps only the allowed paths, at any depth.
// A grant lineage streams its old grant and the complete new one.
func TestRedactKeepsTheLineage(t *testing.T) {
	r := api.Redact(eventstore.Event{Kind: authority.GrantReplaced, Payload: []byte(`{"transition":"grant.replaced","replaces":"grt_1","grant":{"id":"grt_2"}}`)})
	if r.PayloadWithheld || !strings.Contains(string(r.Payload), `"replaces":"grt_1"`) || !strings.Contains(string(r.Payload), `"id":"grt_2"`) {
		t.Errorf("the lineage: %s", r.Payload)
	}
}

func TestRedactKeepsOnlyAllowedPaths(t *testing.T) {
	ev := eventstore.Event{Kind: "token.issued/v1", Payload: []byte(`{"transition":"token.issued","actor":null,` +
		`"token":{"id":"tok_1","actor":"act_1","hash":"$argon2id$x","secret":"s","created_by":"act_2","created_at":"t","revoked_at":null},"extra":{"nonce":"n"}}`)}
	r := api.Redact(ev)
	b := r.Payload
	if r.PayloadWithheld {
		t.Fatal("an allow-listed kind was withheld")
	}
	for _, leak := range []string{"hash", "argon2", "secret", "nonce", "extra"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("%s leaked: %s", leak, b)
		}
	}
	if !strings.Contains(string(b), `"actor":null`) || !strings.Contains(string(b), `"id":"tok_1"`) {
		t.Errorf("allowed fields lost: %s", b)
	}
}

// clientWriter is a client as the handler's ResponseWriter sees it: it
// records frames and write deadlines, can run a hook before an event
// frame is written, and can stall. A stalled write never finishes: only
// its deadline ends it, so it fails at once if one is set, as a write
// does when its deadline comes, and with none it counts as unbounded
// (a real one would hold the stream for good). No clock is read (#226
// entry 113).
type clientWriter struct {
	mu        sync.Mutex
	header    http.Header
	body      strings.Builder
	deadline  time.Time
	deadlines []time.Duration // each deadline, from when it was set
	events    int
	before    func(n int) // before the n-th event frame (1-based)
	stall     bool
	cut       bool          // a stalled write was ended by its deadline
	unbounded int           // stalled writes with no deadline set
	afterCut  int           // calls into the writer after the cut
	delay     time.Duration // each event write takes this long, and succeeds
	closedAt  time.Time     // when the closed frame was written
}

func (c *clientWriter) Header() http.Header {
	if c.header == nil {
		c.header = http.Header{}
	}
	return c.header
}
func (c *clientWriter) WriteHeader(int) {}
func (c *clientWriter) FlushError() error {
	c.touched()
	return nil
}
func (c *clientWriter) SetWriteDeadline(d time.Time) error {
	c.touched()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = d
	c.deadlines = append(c.deadlines, time.Until(d))
	return nil
}

// touched counts a call into the writer after the cut.
func (c *clientWriter) touched() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cut {
		c.afterCut++
	}
}

func (c *clientWriter) Write(b []byte) (int, error) {
	c.touched()
	if strings.Contains(string(b), "event: event") {
		c.events++
		if c.before != nil {
			c.before(c.events)
		}
		if c.stall {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.deadline.IsZero() {
				c.unbounded++
			}
			c.cut = true
			return 0, os.ErrDeadlineExceeded
		}
	}
	if strings.Contains(string(b), "event: event") {
		time.Sleep(c.delay)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.Contains(string(b), "event: closed") {
		c.closedAt = time.Now()
	}
	return c.body.WriteString(string(b))
}
func (c *clientWriter) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body.String()
}

// serve runs the stream for bearer into c until it returns, or fails the
// test after a bound.
func (f *fixture) serve(c *clientWriter, poll time.Duration, bearer string) {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodGet, api.EventsPath, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); f.handler(poll).ServeHTTP(c, req.WithContext(ctx)) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		f.t.Fatalf("the stream did not end; wrote %q", c.String())
	}
}

func (f *fixture) revokeGrant() {
	f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return authority.RevokeGrant(tx, f.sup.ID, f.grant.ID, t0)
	})
}

// A revocation or an expiry during a delivery stops it at the next frame:
// events loaded before it are not sent after it.
func TestStreamStopsMidDelivery(t *testing.T) {
	for name, end := range map[string]func(f *fixture){
		"a revocation": (*fixture).revokeGrant,
		"an expiry":    func(f *fixture) { f.now.Store(t0.Add(time.Hour).UnixNano()) },
	} {
		t.Run(name, func(t *testing.T) {
			f := open(t)
			for range 3 {
				f.appendTo("fixture.appended", prjA)
			}
			c := &clientWriter{before: func(n int) {
				if n == 1 {
					end(f)
				}
			}}
			f.serve(c, netPoll, f.supTk.Reveal())
			out := c.String()
			if got := strings.Count(out, "event: event"); got != 1 || !strings.HasSuffix(out, "event: closed\ndata: "+
				`{"outcome":"failed","reason":"authority_revoked","detail":"the caller's token, actor or grant changed in a way that removes access"}`+"\n\n") {
				t.Fatalf("wrote %d events: %q", got, out)
			}
		})
	}
}

// A client that stalls is cut off within the poll bound: every write has a
// deadline no later than it, the stalled write is ended by its deadline,
// and the stream then ends on its own, sending nothing more, whatever
// changes meanwhile. By the order of what the writer sees, never by a
// clock (#226 entry 113): serve fails the row if the stream ends only once
// the client leaves.
func TestStreamCutsOffAStalledClient(t *testing.T) {
	f := open(t)
	for range 3 {
		f.appendTo("fixture.appended", prjA)
	}
	const poll = 50 * time.Millisecond
	c := &clientWriter{stall: true, before: func(n int) {
		if n == 1 {
			f.revokeGrant()
		}
	}}
	f.serve(c, poll, f.supTk.Reveal())
	if !c.cut || c.unbounded > 0 {
		t.Errorf("the stalled write: cut %v, %d with no deadline (never cut off)", c.cut, c.unbounded)
	}
	if out := c.String(); strings.Contains(out, "event: ") || c.afterCut > 0 {
		t.Errorf("after the stall, %d calls into the writer and frames %q", c.afterCut, out)
	}
	for _, d := range c.deadlines {
		if d > poll {
			t.Errorf("a write deadline %s past the poll bound %s", d, poll)
		}
	}
	if len(c.deadlines) == 0 || c.events != 1 {
		t.Errorf("%d deadlines, %d event writes", len(c.deadlines), c.events)
	}
}

// A store that cannot be read closes the stream as could_not_run, never as
// a revocation; a lost token is a revocation.
func TestStreamStoreErrorIsNotRevocation(t *testing.T) {
	f := open(t)
	tokID, _, _ := strings.Cut(f.supTk.Reveal(), ".")
	stream := frames(t, f.get(http.MethodGet, "", f.supTk.Reveal()))
	f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return nil, tx.Put(identity.TokenProjection, tokID, json.RawMessage(`[]`))
	})
	fr, _ := next(t, stream)
	if fr.event != "closed" || fr.res.Outcome != vocab.CouldNotRun || fr.res.Reason != vocab.ReasonStoreUnreadable {
		t.Fatalf("a damaged token record closed the stream with %+v", fr)
	}
	f.view(func(tx *eventstore.Tx) {
		if err := identity.StillLive(tx, f.supTk.Reveal(), f.sup.ID); err == nil || errors.Is(err, identity.ErrUnauthenticated) {
			t.Errorf("a damaged record reads as %v", err)
		}
		if err := identity.StillLive(tx, f.opTok.Reveal(), f.sup.ID); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Errorf("another actor's token reads as %v", err)
		}
	})
}

func (f *fixture) view(fn func(*eventstore.Tx)) {
	f.t.Helper()
	if err := f.s.View(context.Background(), func(tx *eventstore.Tx) error { fn(tx); return nil }); err != nil {
		f.t.Fatal(err)
	}
}

// A slow write that succeeds does not stretch the time between checks: a
// revocation landing as a frame starts is seen one poll bound after the
// check before that frame, not a write's time plus a poll later.
func TestStreamSlowWriteKeepsTheCheckSchedule(t *testing.T) {
	f := open(t)
	f.appendTo("fixture.appended", prjA)
	const poll = 400 * time.Millisecond
	var revokedAt time.Time
	c := &clientWriter{delay: 300 * time.Millisecond, before: func(n int) {
		if n == 1 {
			f.revokeGrant()
			revokedAt = time.Now()
		}
	}}
	f.serve(c, poll, f.supTk.Reveal())
	if c.closedAt.IsZero() {
		t.Fatalf("no closed frame: %q", c.String())
	}
	if gap := c.closedAt.Sub(revokedAt); gap > poll+150*time.Millisecond {
		t.Errorf("the revocation was seen %s after it landed; the bound is %s", gap, poll)
	}
}

// A healthy HTTP/2 stream idles for several polls, then delivers, then
// idles again and closes on a revocation: no write deadline is left armed
// while it idles.
func TestStreamIdlesOverHTTP2(t *testing.T) {
	f := open(t)
	ts := httptest.NewUnstartedServer(f.handler(netPoll))
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+api.EventsPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.supTk.Reveal())
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.ProtoMajor != 2 {
		t.Fatalf("the stream is %s, not HTTP/2", resp.Proto)
	}
	stream := frames(t, resp)
	time.Sleep(6 * netPoll)
	f.appendTo("fixture.appended", prjA)
	if fr, ok := next(t, stream); !ok || fr.event != "event" {
		t.Fatalf("after idling: %+v (open %v)", fr, ok)
	}
	time.Sleep(6 * netPoll)
	f.revokeGrant()
	if fr, ok := next(t, stream); !ok || fr.event != "closed" || fr.res.Reason != vocab.ReasonAuthorityRevoked {
		t.Fatalf("after the revocation: %+v (open %v)", fr, ok)
	}
}

// pipeListener serves connections made with dial: unbuffered pipes, so a
// client that stops reading blocks the server's next write.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }
func (l *pipeListener) dial() net.Conn {
	client, server := net.Pipe()
	l.conns <- server
	return client
}

// A TLS HTTP/1.1 client that stops reading is cut off at the write
// deadline: the handler returns and the connection closes within the
// budget, not after a graceful TLS close waits out its own five seconds.
func TestStreamCutsOffAStalledTLSClient(t *testing.T) {
	f := open(t)
	certs := httptest.NewUnstartedServer(nil)
	certs.StartTLS()
	serverTLS := certs.TLS.Clone()
	clientTLS := certs.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	clientTLS.ServerName = "example.com"
	certs.Close()
	const poll = 300 * time.Millisecond
	returned, closed := make(chan time.Time, 1), make(chan time.Time, 1)
	srv := &http.Server{ConnContext: api.ConnContext, ConnState: func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed {
			closed <- time.Now()
		}
	}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.handler(poll).ServeHTTP(w, r)
		returned <- time.Now()
	})}
	ln := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
	go func() { _ = srv.Serve(tls.NewListener(ln, serverTLS)) }()
	t.Cleanup(func() { srv.Close() })
	conn := tls.Client(ln.dial(), clientTLS)
	t.Cleanup(func() { conn.Close() })
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: example.com\r\nAuthorization: Bearer %s\r\n\r\n", api.EventsPath, f.supTk.Reveal()); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("%v %v", resp, err)
	}
	// The client reads nothing more; the next frame cannot be written.
	stalled := time.Now()
	f.appendTo("fixture.appended", prjA)
	// The frame waits up to one poll for the stream to see it, then up to
	// one more for the client: the budget, plus slack. The connection is
	// gone by then too, not held open by a graceful TLS close.
	budget := 2*poll + 700*time.Millisecond
	for what, c := range map[string]chan time.Time{"the handler returned": returned, "the connection closed": closed} {
		select {
		case at := <-c:
			if took := at.Sub(stalled); took > budget {
				t.Errorf("%s %s after the stall; the budget is %s", what, took, budget)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: never", what)
		}
	}
}
