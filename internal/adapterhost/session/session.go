// SPDX-License-Identifier: Apache-2.0

// The session's transport (docs/design/m0/09) over the adapter process:
// requests correlated with replies, each reply checked in the reader by its
// method's check before any state changes or delivery, every event checked
// against schemas/adapter/event.json and the run's envelope, and during a
// run the adapter must send heartbeats. The typed methods are in methods.go. A broken line, an invalid or unexpected
// message, silence, the output ending early or the deadline end the session
// with an *Error; ending it kills the namespace and settles every wait.
//
// Nothing the adapter sends, nor the params the core sends (a prepare's
// gateway token, say), ever goes into an *Error or any other error of this
// file: their details are fixed texts. The adapter's own words reach the
// caller only through the run's results and events, and Process.Stderr.

package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session/inproc"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/wire"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// DefaultHeartbeat is the longest silence allowed between heartbeats during a
// run: 09's 10 s plus 1 s of scheduling slack.
const DefaultHeartbeat = 11 * time.Second

// Config is one session's adapter and bounds.
type Config struct {
	Command  []string
	Env      []string      // the adapter's whole environment
	Deadline time.Duration // the whole session, from spawn
	// Heartbeat is the longest silence between heartbeats during a run
	// (DefaultHeartbeat when zero).
	Heartbeat time.Duration
	// InProcess, when set, is the adapter in place of Command and Env: a
	// function of this process over the session's pipes, never spawned nor
	// contained. Only the fixture harness can make one (package inproc,
	// whose importers a test restricts).
	InProcess *inproc.Peer
}

// Kind is why a call failed or the session ended.
type Kind string

const (
	KindProtocol  Kind = "protocol"  // a broken line, an invalid or unexpected message, a bad stamp
	KindHeartbeat Kind = "heartbeat" // silence during a run
	KindExited    Kind = "exited"    // the output ended (or input broke) before the session was shut down
	KindDeadline  Kind = "deadline"  // the session's deadline killed it
	KindClosed    Kind = "closed"    // the core shut it down or closed it
	KindRefused   Kind = "refused"   // the adapter answered with a JSON-RPC error; the session goes on
)

// Error is a failed call or the session's end. Its fields never carry the
// adapter's output, stderr or environment, nor the core's params.
type Error struct {
	Kind   Kind
	Method string // the method concerned, if any
	Detail string // a fixed text
	Code   int64  // for KindRefused, the adapter's JSON-RPC error code if reserved (rpcCode), else 0; its message is dropped
	// Check is the ingestion check that refused an output: CheckForeignHandle
	// for KindForeignHandle, else empty.
	Check types.AttemptOutputRefusalCheck
}

func (e *Error) Error() string {
	s := "adapter session " + string(e.Kind)
	if e.Method != "" {
		s += " (" + e.Method + ")"
	}
	return s + ": " + e.Detail
}

// Reason is the registered reason: the core's own stop (deadline, close) is
// attempt_stopped, a foreign handle unsafe_output; everything else is the
// adapter breaking the contract.
func (e *Error) Reason() vocab.Reason {
	switch e.Kind {
	case KindDeadline, KindClosed:
		return vocab.ReasonAttemptStopped
	case KindForeignHandle:
		return vocab.ReasonUnsafeOutput
	}
	return vocab.ReasonAdapterProtocolError
}

func protocol(detail string) *Error { return &Error{Kind: KindProtocol, Detail: detail} }

// Event is one validated event of the run. It is the adapter's: Raw may
// hold anything the schema allows, secrets it was given included.
type Event struct {
	Name  string // the event field: heartbeat, usage, tool.request, …
	Raw   json.RawMessage
	Value types.AdapterEvent
}

type stamp struct {
	Event        string `json:"event"`
	Attempt      string `json:"attempt"`
	FencingToken int64  `json:"fencing_token"`
	Call         string `json:"call"`
}

type run struct {
	id       int64
	want     stamp
	onEvent  func(Event)
	check    func(json.RawMessage) *Error // the run's own check of each event, after the envelope's
	lastBeat time.Time
}

// expect is what a call needs beyond its method and params.
type expect struct {
	// admit runs under the session's lock before the request is
	// registered; an error refuses the call and nothing is sent.
	admit func() error
	// check validates (and decodes) a result in the reader before any state
	// changes or delivery, also when the caller has stopped waiting.
	check func(json.RawMessage) (any, *Error)
	// refused runs in the reader when the adapter answers with an error,
	// before delivery.
	refused func()
}

// pending is a request awaiting its reply.
type pending struct {
	method string
	expect
	ch chan reply // buffered: a reply nobody waits for is dropped
}

type reply struct {
	value any
	err   *Error // KindRefused: the adapter answered with an error
}

// Session is one adapter process and its client. Methods are safe for
// concurrent use: tool.response and cancel go while run is pending.
type Session struct {
	p         *Process
	heartbeat time.Duration
	wmu       sync.Mutex
	w         *wire.Writer

	mu       sync.Mutex
	next     int64
	pending  map[int64]pending
	run      *run
	shutID   int64                // the shutdown request
	shut     bool                 // shutdown answered: the output ending is the clean end
	fence    fence                // the session's one attempt (methods.go)
	foreign  *types.AdapterHandle // the first handle of another attempt refused (Foreign)
	declared *Declaration         // a refused run result's declaration (Declared)
	err      *Error
	ended    chan struct{}
}

// Open spawns the adapter and starts reading it.
func Open(cfg Config) (*Session, error) {
	var p *Process
	var err error
	if cfg.InProcess != nil {
		p, err = startPeer(cfg.InProcess, cfg.Deadline)
	} else {
		p, err = Spawn(ProcessSpec{Command: cfg.Command, Env: cfg.Env, Deadline: cfg.Deadline})
	}
	if err != nil {
		return nil, err
	}
	s := &Session{p: p, heartbeat: cfg.Heartbeat, w: wire.NewWriter(p.Stdin()), pending: map[int64]pending{}, ended: make(chan struct{})}
	if s.heartbeat <= 0 {
		s.heartbeat = DefaultHeartbeat
	}
	go s.read()
	go s.watch()
	return s, nil
}

// end records the session's end (the first cause wins), kills the namespace
// and wakes every waiter.
func (s *Session) end(e *Error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = e
		close(s.ended)
	}
	s.mu.Unlock()
	s.p.Kill()
}

// Done is closed once the session has ended (Err is then set).
func (s *Session) Done() <-chan struct{} { return s.ended }

// Err is why the session ended, or nil while it is live.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		return nil
	}
	return s.err
}

func (s *Session) read() {
	defer s.p.Stdout().Close()
	r := wire.NewReader(s.p.Stdout())
	for {
		line, err := r.Next()
		if err != nil {
			s.mu.Lock()
			shut := s.shut
			s.mu.Unlock()
			switch {
			case s.p.Expired():
				s.end(&Error{Kind: KindDeadline, Detail: "the session's deadline passed"})
			case errors.Is(err, io.EOF) && shut:
				s.end(&Error{Kind: KindClosed, Detail: "the session was shut down"})
			case errors.Is(err, io.EOF):
				s.end(&Error{Kind: KindExited, Detail: "the adapter's output ended before shutdown"})
			default:
				s.end(protocol("the output stream broke: " + framing(err)))
			}
			return
		}
		if e := s.accept(line); e != nil {
			s.end(e)
			return
		}
	}
}

func (s *Session) accept(line []byte) *Error {
	msg, err := wire.Classify(line)
	if err != nil {
		// The refusal's detail can quote the line (a duplicate key, a
		// number): only its stage is kept.
		if r, ok := err.(*wire.Refusal); ok && r.Stage == "envelope" {
			return protocol("a message that is neither a reply nor an adapter.event notification")
		}
		return protocol("a line that is not strict JSON")
	}
	if msg.Kind == wire.KindEvent {
		return s.event(msg.Event)
	}
	id := msg.Response.ID
	s.mu.Lock()
	q, ok := s.pending[id]
	delete(s.pending, id)
	s.mu.Unlock()
	if !ok { // the id is the adapter's: never in the error
		return protocol("a reply for a request that is not pending")
	}
	var rp reply
	if e := msg.Response.Error; e != nil {
		rp.err = &Error{Kind: KindRefused, Method: q.method, Detail: "the adapter answered with an error", Code: rpcCode(e.Code)}
		if q.refused != nil {
			q.refused()
		}
	} else {
		v, bad := q.check(msg.Response.Result)
		if bad != nil {
			return bad // the session ends before any state changes
		}
		rp.value = v
	}
	s.mu.Lock()
	if s.run != nil && s.run.id == id {
		s.run = nil // events after the run's reply are outside it
	}
	s.shut = s.shut || (id == s.shutID && rp.err == nil)
	s.mu.Unlock()
	q.ch <- rp
	return nil
}

// rpcCode keeps an adapter's error code only within JSON-RPC's reserved
// range (−32768 to −32000): any other number is the adapter's to choose,
// and could carry a secret.
func rpcCode(code int64) int64 {
	if code >= -32768 && code <= -32000 {
		return code
	}
	return 0
}

func (s *Session) event(raw json.RawMessage) *Error {
	s.mu.Lock()
	r := s.run
	s.mu.Unlock()
	if r == nil {
		return protocol("an event outside a run")
	}
	v, err := contract.Decode[types.AdapterEvent]("adapter/event", raw)
	if err != nil {
		return protocol("an event that does not match adapter/event")
	}
	var got stamp
	if err := json.Unmarshal(raw, &got); err != nil {
		return protocol("an event without its envelope")
	}
	if got.Attempt != r.want.Attempt || got.FencingToken != r.want.FencingToken || got.Call != r.want.Call {
		return protocol("an event stamped for another attempt, fencing token or call than the run's")
	}
	if r.check != nil {
		if e := r.check(raw); e != nil {
			return e
		}
	}
	if got.Event == "heartbeat" {
		s.mu.Lock()
		r.lastBeat = time.Now()
		s.mu.Unlock()
	}
	if r.onEvent != nil {
		r.onEvent(Event{Name: got.Event, Raw: raw, Value: v})
	}
	return nil
}

// watch ends the session when a run is silent for longer than the heartbeat
// bound. Time the watch itself was not scheduled (a late tick) is not
// counted against the adapter.
func (s *Session) watch() {
	tick := max(s.heartbeat/10, 10*time.Millisecond)
	t := time.NewTicker(tick)
	defer t.Stop()
	prev := time.Now()
	for {
		select {
		case <-s.ended:
			return
		case <-t.C:
		}
		now := time.Now()
		var gap time.Duration
		s.mu.Lock()
		if r := s.run; r != nil {
			if late := now.Sub(prev) - tick; late > tick {
				r.lastBeat = r.lastBeat.Add(late)
			}
			gap = now.Sub(r.lastBeat)
		}
		s.mu.Unlock()
		prev = now
		if gap > s.heartbeat {
			s.end(&Error{Kind: KindHeartbeat, Method: "run", Detail: fmt.Sprintf("no heartbeat for %s during the run (09: every ≤ 10 s)", gap.Round(time.Millisecond))})
			return
		}
	}
}

// call sends one request (params validated against schema) and waits for
// its reply, which the reader has checked. A cancelled ctx abandons the
// wait; the reply, when it comes, is still checked, then dropped.
func (s *Session) call(ctx context.Context, method, schema string, params any, r *run, x expect) (any, error) {
	var raw json.RawMessage
	if schema != "" {
		b, err := contract.Encode(schema, params)
		if err != nil { // its detail can quote the params: a gateway token
			return nil, fmt.Errorf("adapterhost: the %s params do not match %s", method, schema)
		}
		raw = b
	}
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return nil, s.err
	}
	if r != nil && s.run != nil {
		s.mu.Unlock()
		return nil, errors.New("adapterhost: a run is already in progress")
	}
	if x.admit != nil {
		if err := x.admit(); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	s.next++
	id, ch := s.next, make(chan reply, 1)
	s.pending[id] = pending{method: method, expect: x, ch: ch}
	if r != nil {
		r.id, r.lastBeat, s.run = id, time.Now(), r
	}
	if method == "shutdown" {
		s.shutID = id
	}
	s.mu.Unlock()
	msg, err := wire.Encode(wire.Request{JSONRPC: wire.Version, ID: id, Method: method, Params: raw})
	if err == nil {
		s.wmu.Lock()
		err = s.w.Write(msg)
		s.wmu.Unlock()
	}
	if err != nil {
		s.end(&Error{Kind: KindExited, Method: method, Detail: "writing to the adapter failed"})
	}
	got := func(rp reply) (any, error) {
		if rp.err != nil {
			return nil, rp.err
		}
		return rp.value, nil
	}
	select {
	case rp := <-ch:
		return got(rp)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ended:
		select {
		case rp := <-ch: // answered just before the end
			return got(rp)
		default:
			return nil, s.Err()
		}
	}
}

// Wait returns once the adapter's process tree has exited (by itself, at
// the deadline or by Close), with Process.Wait's error (ErrUnverified when it
// was not seen to).
func (s *Session) Wait() error { return s.p.Wait() }

// Close ends the session now, kills the namespace and waits for it.
func (s *Session) Close() error {
	s.end(&Error{Kind: KindClosed, Detail: "closed by the core"})
	return s.p.Wait()
}

// Stderr is Process.Stderr: unredacted, possibly with secrets.
func (s *Session) Stderr() string { return s.p.Stderr() }

// framing keeps a read error's kind: wire's framing errors are fixed texts
// (no message content); any other error is the pipe's.
func framing(err error) string {
	if errors.Is(err, wire.ErrFraming) {
		return err.Error()
	}
	return "the pipe failed"
}
