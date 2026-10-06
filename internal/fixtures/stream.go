// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// frameSlack is scheduling headroom over a poll bound.
const frameSlack = 150 * time.Millisecond

// frameWait bounds how long the fixture waits for the stream's next frame.
const frameWait = 5 * time.Second

// sseFrame is one Server-Sent Events frame: its event name and data.
type sseFrame struct{ event, data string }

// frames reads r's frames onto a channel, closed at the stream's end.
func frames(r io.Reader) <-chan sseFrame {
	out := make(chan sseFrame)
	go func() {
		defer close(out)
		sc, f := bufio.NewScanner(r), sseFrame{}
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				out <- f
				f = sseFrame{}
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return out
}

// streamAfterGrantRevocation: a supervisor streams the events its grant
// reads; once an operator revokes the grant, the stream closes with
// failed/authority_revoked before any later event, and reopening it is
// refused (11 "a stream after grant revocation"; 04 "Read authorization";
// 08 "Reads, commands, events").
func streamAfterGrantRevocation(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	kinds := identity.Kinds()
	maps.Copy(kinds, authority.GrantKinds())
	kinds["fixture.appended/v1"] = eventstore.Kind{Schema: "common/defs"}
	s, done, err := storeWith(ctx, dir, kinds, eventstore.ProjectEach(identity.Projector{}.Project, authority.GrantProjector{}.Project))
	if err != nil {
		return nil, nil, err
	}
	defer done()
	submit := func(by types.ActorID, apply func(*eventstore.Tx) ([]eventstore.Event, error)) error {
		cmd, err := ids.New("cmd")
		if err != nil {
			return err
		}
		_, err = s.Submit(ctx, eventstore.Command{ID: cmd, Actor: string(by), IdempotencyKey: cmd, Input: []byte(`{}`)},
			func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				evs, err := apply(tx)
				return evs, json.RawMessage(`{}`), err
			})
		return err
	}
	sup, op, sys, _, err := actors()
	if err != nil {
		return nil, nil, err
	}
	sys.CreatedBy, sup.CreatedBy = sys.ID, op.ID
	op.CreatedBy = sys.ID
	now := time.Now().UTC()
	// The server's clock runs ahead of the wall clock by skew (an expiry).
	var skew atomic.Int64
	clock := func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	newGrant := func() (types.Grant, error) {
		apr, err1 := ids.New("apr")
		grt, err2 := ids.New("grt")
		return types.Grant{ID: types.GrantID(grt), Actor: sup.ID, Projects: types.ProjectScope{List: []types.ProjectID{projectA}},
			Actions: []types.ActionKind{}, Reads: types.GrantReads{Projects: types.GrantReadsProjects{Same: true}, Kinds: []types.Name{"fixture"}},
			ValidFrom: types.Time(now.Add(-time.Hour).Format(time.RFC3339Nano)), ExpiresAt: types.Time(clock().UTC().Add(time.Hour).Format(time.RFC3339Nano)),
			ConfirmationOverrides: map[string]types.GrantConfirmationOverridesValue{}, IssuedBy: op.ID, Approval: types.ApprovalID(apr)}, errors.Join(err1, err2)
	}
	g, err := newGrant()
	if err != nil {
		return nil, nil, err
	}
	var token identity.Secret
	for _, c := range []struct {
		by    types.ActorID
		apply func(*eventstore.Tx) ([]eventstore.Event, error)
	}{
		{sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(sys) }},
		{sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(op) }},
		{op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(sup) }},
		{op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
			evs, secret, err := identity.IssueToken(sup.ID, op.ID, now)
			token = secret
			return evs, err
		}},
		{op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(g) }},
	} {
		if err := submit(c.by, c.apply); err != nil {
			return nil, nil, err
		}
	}
	appendEvent := func() error {
		return submit(op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
			id, err := ids.New("evt")
			p := string(projectA)
			return []eventstore.Event{{ID: id, Kind: "fixture.appended/v1", Subject: p, Project: &p, Payload: []byte(`{}`)}}, err
		})
	}
	// Over HTTP/2 with TLS, and idling several polls before each change:
	// an idle stream holds no write deadline that could reset it.
	handler := api.Events{Store: s, Now: clock, Poll: 100 * time.Millisecond}
	idle := func() { time.Sleep(5 * handler.Poll) }
	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	// Room for every wait to time out (a red run) without the fixture
	// failing to run.
	ctx, cancel := context.WithTimeout(ctx, 12*frameWait)
	defer cancel()
	get := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+api.EventsPath, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token.Reveal())
		return srv.Client().Do(req)
	}
	resp, err := get()
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("the stream opened with %s", resp.Status)
	}
	var o observe
	stream := frames(resp.Body)
	// next records the stream's next frame as a step: a delivered event is
	// empty/completed_clean, a closed frame its result.
	next := func() {
		select {
		case f, ok := <-stream:
			switch {
			case !ok:
				o.check(false, "the stream ended without a closed frame")
			case f.event == "event":
				o.steps = append(o.steps, Step{vocab.Empty, vocab.ReasonCompletedClean.String()})
			default:
				var r vocab.Result
				o.check(json.Unmarshal([]byte(f.data), &r) == nil, "frame %q carries no result: %s", f.event, f.data)
				o.steps = append(o.steps, Step{r.Outcome, r.Reason.String()})
			}
		case <-time.After(frameWait):
			o.check(false, "no frame within %s", frameWait)
		}
	}
	o.check(resp.ProtoMajor == 2, "the stream is %s, not HTTP/2", resp.Proto)
	idle()
	if err := appendEvent(); err != nil {
		return nil, nil, err
	}
	next()
	idle()
	// Revoke, then append: the event after the revocation is never sent.
	if err := submit(op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return authority.RevokeGrant(tx, sup.ID, g.ID, time.Now())
	}); err != nil {
		return nil, nil, err
	}
	if err := appendEvent(); err != nil {
		return nil, nil, err
	}
	next()
	select {
	case _, more := <-stream:
		o.check(!more, "the stream went on after it closed")
	case <-time.After(frameWait):
		o.check(false, "the stream stayed open after the revocation")
	}
	// Reopening with the same token is refused: no grant reads anything.
	again, err := get()
	if err != nil {
		return nil, nil, err
	}
	defer again.Body.Close()
	var r vocab.Result
	if err := json.NewDecoder(again.Body).Decode(&r); err != nil {
		return nil, nil, fmt.Errorf("reopening the stream (%s): %w", again.Status, err)
	}
	o.check(again.StatusCode == http.StatusForbidden, "reopening the stream gave %s", again.Status)
	o.steps = append(o.steps, Step{r.Outcome, r.Reason.String()})

	// A slow client: the grant is revoked, or expires, while a delivery is
	// under way; a slow write that succeeds spans a revocation (which must
	// be seen within one poll bound of the check before it); then a client
	// stalls through a revocation. Each runs under a new grant with events
	// waiting.
	revoke := func(g types.Grant) func() {
		return func() {
			o.check(submit(op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return authority.RevokeGrant(tx, sup.ID, g.ID, time.Now())
			}) == nil, "the revocation was refused")
		}
	}
	for _, c := range []struct {
		name   string
		end    func(types.Grant) func()
		stall  bool
		delay  time.Duration
		events int
	}{
		{"a revocation mid-delivery", revoke, false, 0, 3},
		{"an expiry mid-delivery", func(types.Grant) func() { return func() { skew.Add(int64(2 * time.Hour)) } }, false, 0, 3},
		{"a slow write through a revocation", revoke, false, 3 * handler.Poll / 4, 1},
		{"a stalled client through a revocation", revoke, true, 0, 3},
	} {
		g, err := newGrant()
		if err == nil {
			err = submit(op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(g) })
		}
		for i := 0; i < c.events && err == nil; i++ {
			err = appendEvent()
		}
		if err != nil {
			return nil, nil, err
		}
		cl := &slowClient{stall: c.stall, delay: c.delay, before: c.end(g)}
		started := time.Now()
		bound := 2 * (handler.Poll + frameSlack) // a delivery, then a check
		cl.serve(ctx, handler, token.Reveal(), 4*bound)
		o.check(time.Since(started) < 4*bound, "%s: the stream outlived the client by %s", c.name, time.Since(started))
		o.check(cl.maxDeadline <= handler.Poll, "%s: a write deadline of %s, past the poll bound", c.name, cl.maxDeadline)
		if c.stall {
			o.check(cl.written() == "", "%s: frames after the stall: %q", c.name, cl.written())
			continue
		}
		o.check(cl.closedAt.Sub(cl.changedAt) <= handler.Poll+frameSlack, "%s: the change was seen %s after it landed",
			c.name, cl.closedAt.Sub(cl.changedAt))
		o.steps = append(o.steps, cl.steps()...)
	}
	return o.steps, o.violations, nil
}

// slowClient is the client as the stream's ResponseWriter sees it: it
// runs before() once, as the first event frame is written (the change
// lands during a delivery), and, stalling, blocks every event write
// until its deadline.
type slowClient struct {
	mu          sync.Mutex
	header      http.Header
	body        strings.Builder
	deadline    time.Time
	maxDeadline time.Duration
	before      func()
	stall       bool
	delay       time.Duration // an event write takes this long, and succeeds
	changedAt   time.Time     // when before ran
	closedAt    time.Time     // when the closed frame was written
}

func (c *slowClient) Header() http.Header {
	if c.header == nil {
		c.header = http.Header{}
	}
	return c.header
}

func (c *slowClient) WriteHeader(int) {}

func (c *slowClient) FlushError() error { return nil }

func (c *slowClient) SetWriteDeadline(d time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline, c.maxDeadline = d, max(c.maxDeadline, time.Until(d))
	return nil
}

func (c *slowClient) Write(b []byte) (int, error) {
	if strings.Contains(string(b), "event: event") {
		if c.before != nil {
			c.before()
			c.before, c.changedAt = nil, time.Now()
		}
		time.Sleep(c.delay)
		if c.stall {
			c.mu.Lock()
			d := c.deadline
			c.mu.Unlock()
			time.Sleep(time.Until(d))
			return 0, os.ErrDeadlineExceeded
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.Contains(string(b), "event: closed") {
		c.closedAt = time.Now()
	}
	return c.body.Write(b)
}

func (c *slowClient) written() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body.String()
}

// serve runs the stream into c until it ends, or bound passes.
func (c *slowClient) serve(ctx context.Context, h http.Handler, bearer string, bound time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, api.EventsPath, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	h.ServeHTTP(c, req)
}

// steps reads c's frames as steps, as next does over HTTP.
func (c *slowClient) steps() []Step {
	var out []Step
	for f := range frames(strings.NewReader(c.written())) {
		if f.event == "event" {
			out = append(out, Step{vocab.Empty, vocab.ReasonCompletedClean.String()})
			continue
		}
		var r vocab.Result
		if json.Unmarshal([]byte(f.data), &r) == nil {
			out = append(out, Step{r.Outcome, r.Reason.String()})
		}
	}
	return out
}
