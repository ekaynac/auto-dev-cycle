// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Package adapterhost runs one attempt through an executor adapter
// (docs/design/m0/09): the exchange root and its prelaunch capability check,
// the launch boundary (attempt.started), the session, and, once the
// adapter's process tree has exited, the fenced ingestion of every final
// output it declared and their submission in one writer transaction. Usage
// is recorded in that transaction whatever the fence says (05: accounting
// is not fenced). Once attempt.started commits, every exit goes through that
// finalisation, under a bounded context of the controller's own, whatever
// the caller's context does.
//
// This increment serves no mediated tool: a tool.request, or a tool.result
// naming a delivered handle (which no delivery record here can match),
// refuses the run (fail closed) instead of being ignored.
package adapterhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/ingest"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

// Size bounds (09 "Size bounds"): M0 constants until profile values
// replace them. BoundEventHandle is any other handle an event declares.
const (
	BoundPatch       int64 = 64 << 20
	BoundText        int64 = 16 << 20
	BoundDecision    int64 = 1 << 20
	BoundEventHandle int64 = 16 << 20
)

// FinalizeTimeout bounds the finalisation after attempt.started (the
// ingestion and the final transaction); shutdownTimeout bounds the polite
// shutdown before the namespace is killed.
const (
	FinalizeTimeout = 2 * time.Minute
	shutdownTimeout = 5 * time.Second
)

// finalizeTimeout is FinalizeTimeout; a test shortens it.
var finalizeTimeout = FinalizeTimeout

// PromptName is the prompt's handle in in/.
const PromptName = "prompt.md"

// ErrShutdownFailed: the adapter answered shutdown with an unsuccessful
// outcome (could_not_run or failed). Like a broken session, it refuses the
// submission; the run's accounting is still recorded.
var ErrShutdownFailed = errors.New("adapterhost: the adapter reported an unsuccessful shutdown; the run is refused")

// ErrUnservedEvent: the run sent a notification this host does not serve
// (a mediated tool request, or a delivered tool result); the run is refused.
var ErrUnservedEvent = errors.New("adapterhost: the run sent a tool notification this host does not serve; the run is refused")

// Host runs attempts against one event store and blob store.
type Host struct {
	Store *eventstore.Store
	Blobs *blobstore.Store
	Actor string // act_…: the controller actor whose commands record the attempt
	Root  string // the directory each attempt's exchange root is made in
}

// Spec is one attempt to run: started by its caller, never by the host.
type Spec struct {
	Lease   types.LeaseID
	Attempt types.AttemptID // started on Lease, its unit's current attempt
	Base    types.AttemptBase
	Call    types.CallID
	Adapter session.Config
	// Prepare is prepare's params; attempt, fencing token, exchange and
	// inputs are the host's.
	Prepare types.AdapterPrepareParams
	Prompt  []byte
	Bounds  Bounds // the waits on the adapter; zero: as Bounds says
	// Tools are the only tools the adapter may describe (EditLoopTools for
	// adcd's launches, #226 entry 65); nil: none.
	Tools []types.AdapterLabel
	// AnyTool allows any described tool: only the M0 harness and the
	// contract fixtures, whose adapters describe their own tools, set it.
	AnyTool bool
}

// Outcome is what an attempt left.
type Outcome struct {
	// Result is the run's result as the adapter returned it; nil when the
	// session ended without one.
	Result *types.AdapterRunResult
	// Session is why the run produced no admissible result: a
	// *session.Error (a broken shutdown or session end included, after a
	// valid result too), ErrUnservedEvent, a context error, or Wait's error
	// when the process tree was not seen to exit. Every text is fixed.
	Session error
	// Submitted is the result attempt.result_submitted recorded, or nil.
	Submitted *types.SubmittedResult
	Refusals  []types.AttemptOutputRefusal
	Usage     []types.UsageMeasurement
	// Hints are the distinct class_hint values of the run's error events,
	// in the order they first arrived: the exit map's input
	// (internal/vocab/exitmap).
	Hints []string
}

// Seams for tests: the prelaunch check, the staging copy, and a hook at
// "describe", "prepare" (before each call), "event:<name>" (each event),
// "exited" (the process tree is gone) and "ingested" (before the final
// transaction).
var (
	probe    = ingest.SupportedIn
	stage    = ingest.Stage
	testHook func(stage string)
	// fenceAttempt and waitTree are lease.FenceAttempt and Session.Wait.
	fenceAttempt = lease.FenceAttempt
	waitTree     = (*session.Session).Wait
)

// ErrNotStarted: the prepared attempt is not one its caller started: not
// running on the spec's lease, from the spec's base, as its unit's current
// attempt.
var ErrNotStarted = errors.New("adapterhost: the attempt is not started on its lease (the host never starts one)")

func hook(stage string) {
	if testHook != nil {
		testHook(stage)
	}
}

// Prepared is an attempt's exchange, made and checked before its start.
type Prepared struct {
	h    *Host
	spec Spec
	x    *exchange
	used bool // Run or Discard was called: a Prepared is single-use (Opus #326 L3)
}

// ErrUsed: a Prepared is run or discarded once.
var ErrUsed = errors.New("adapterhost: the prepared attempt was already run or discarded")

// Prepare makes spec's exchange and runs the prelaunch check on it, before
// any start (I16): the caller starts the attempt only once it passes
// (dispatch.Launch in adcd), and refuses the start otherwise. A failed
// check wraps ingest.ErrUnsupported; nothing is recorded and the exchange
// is gone.
func (h *Host) Prepare(spec Spec) (*Prepared, error) {
	x, err := newExchange(h.Root, spec.Attempt, spec.Prompt)
	if err != nil {
		return nil, err
	}
	if perr := probe(x.outDir); perr != nil {
		return nil, errors.Join(perr, x.remove())
	}
	return &Prepared{h: h, spec: spec, x: x}, nil
}

// Discard removes an exchange its caller will not run (a refused start).
func (p *Prepared) Discard() error {
	if p.used {
		return ErrUsed
	}
	p.used = true
	return p.x.remove()
}

// Run runs the prepared attempt, which its caller started, to its end, and
// removes the exchange; once (ErrUsed after). The host never starts an
// attempt: one that is not running on the spec's lease, from the spec's
// base, as its unit's current attempt is refused (ErrNotStarted), nothing
// launched. Its fencing token is the attempt's. An error is the host's (the
// stores, the exchange root, a spawn, an ingestion fault), with a fixed
// text; the adapter's failures are in the Outcome. After a host error the
// attempt may still be running: its caller loses it (lease.LoseRunning)
// and starts the next through dispatch.Launch, never by running it again.
func (p *Prepared) Run(ctx context.Context) (out Outcome, err error) {
	if p.used {
		return Outcome{}, ErrUsed
	}
	p.used = true
	defer func() { err = errors.Join(err, p.x.remove()) }()
	var a types.Attempt
	var current types.AttemptID
	var started, ok bool
	if err := p.h.Store.View(ctx, func(tx *eventstore.Tx) (err error) {
		a, _, ok, err = lease.AttemptOf(tx, p.spec.Attempt)
		if err == nil && ok {
			current, started, err = lease.CurrentAttempt(tx, a.Unit)
		}
		return err
	}); err != nil {
		return Outcome{}, err
	}
	if !ok || !started || current != p.spec.Attempt || a.Status != types.AttemptStatusRunning || a.Lease != p.spec.Lease || a.Base != p.spec.Base {
		return Outcome{}, ErrNotStarted
	}
	r := &attemptRun{h: p.h, spec: p.spec, token: a.FencingToken, x: p.x}
	return r.run(ctx)
}

// attemptRun is one started attempt's state.
type attemptRun struct {
	h     *Host
	spec  Spec
	token int64
	x     *exchange

	mu       sync.Mutex // the session's reader adds to these
	usage    []*types.UsageEvent
	declared []types.AdapterHandle // final outputs the events declared, deduplicated
	unserved bool
	// toolRefused is a tool the adapter ran that its launch does not allow.
	toolRefused string
	cancelRun   context.CancelFunc

	out           Outcome
	staged        map[types.AdapterHandle]*blobstore.Staged
	declaredFinal *types.AdapterRunResultUsageFinalValue // a refused result's final usage
	measured      []types.UsageMeasurement
	// admitted: every final output was checked and copied, none refused.
	// denied: some output was refused, whether or not its record could be
	// kept. Only admitted && !denied lets the result be submitted.
	admitted, denied bool
	tempLeft         bool // a failed staging left its temporary file
}

// run launches the adapter, converses, ends the session, waits for the
// process tree, then finalises. The finalisation's bounded context of the
// controller's own starts only when finalisation does (after the session
// and the process tree are gone, or at a failed spawn), so a long run never
// eats into it; the caller's ctx bounds only the conversation.
func (r *attemptRun) run(ctx context.Context) (out Outcome, err error) {
	defer func() { err = errors.Join(err, r.discard()) }()
	s, err := session.Open(r.spec.Adapter)
	if err != nil {
		fctx, cancel := r.finalizing(ctx)
		defer cancel()
		out, ferr := r.finish(fctx, nil, err, false)
		return out, errors.Join(err, ferr)
	}
	result, runErr := r.converse(ctx, s)
	shutErr := r.shutdown(ctx, s)
	waitErr := waitTree(s)
	hook("exited")
	fctx, cancel := r.finalizing(ctx)
	defer cancel()
	endErr := sessionEnd(fctx, s, shutErr)
	r.mu.Lock()
	unserved, toolRefused := r.unserved, r.toolRefused
	r.mu.Unlock()
	switch {
	case toolRefused != "":
		// With the tree's own end: an unverified exit (session.ErrUnverified)
		// is never hidden from a caller that must not clean up (Opus #322 L3).
		return r.finish(fctx, nil, errors.Join(fmt.Errorf("%w: %q", ErrToolRefused, toolRefused), waitErr), false)
	case errors.Is(waitErr, session.ErrUnverified):
		return r.finish(fctx, nil, waitErr, false) // the tree may still write: nothing is ingested (09)
	case unserved && runErr == nil:
		return r.finish(fctx, &result, ErrUnservedEvent, false) // its final usage is still recorded
	case unserved:
		return r.finish(fctx, nil, ErrUnservedEvent, false)
	case runErr != nil:
		hostErr := r.refused(fctx, s)
		out, ferr := r.finish(fctx, nil, runErr, false)
		return out, errors.Join(hostErr, ferr)
	case endErr != nil:
		// A valid result, then a broken shutdown or session end: nothing is
		// admitted, its accounting is recorded.
		return r.finish(fctx, &result, endErr, false)
	}
	hostErr := r.admit(fctx, result)
	hook("ingested")
	out, ferr := r.finish(fctx, &result, hostErr, hostErr == nil)
	return out, errors.Join(hostErr, ferr)
}

// finalizing is the finalisation's context: the caller's values, never its
// cancellation, bounded by finalizeTimeout from now.
func (r *attemptRun) finalizing(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), finalizeTimeout)
}

// shutdown shuts the session down and waits for the process tree to exit,
// both within the shutdown bound (Bounds.ShutdownBound), killing the
// namespace otherwise, and returns the shutdown's error (a *TimeoutError
// when the bound passed); an outcome other than empty or found is
// ErrShutdownFailed. The session's deadline is only the backstop.
func (r *attemptRun) shutdown(ctx context.Context, s *session.Session) error {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.spec.Bounds.ShutdownBound())
	defer cancel()
	o, err := s.Shutdown(sctx)
	if err == nil {
		select {
		case <-s.Exited():
		case <-sctx.Done():
			err = sctx.Err()
		}
	}
	if err != nil {
		_ = s.Close()
		if errors.Is(sctx.Err(), context.DeadlineExceeded) {
			return &TimeoutError{Wait: "shutdown"}
		}
		return err
	}
	if o.Outcome != types.OutcomeEmpty && o.Outcome != types.OutcomeFound {
		return ErrShutdownFailed
	}
	return nil
}

// sessionEnd is why the session did not end cleanly, once its process tree is
// gone: the shutdown's error, or the session's own end when that was not
// the clean close after shutdown (a line after the reply, say). A session
// not seen to end within ctx is not clean either.
func sessionEnd(ctx context.Context, s *session.Session, shutErr error) error {
	if shutErr != nil {
		return shutErr
	}
	select {
	case <-s.Done():
	case <-ctx.Done():
		return ctx.Err()
	}
	var e *session.Error
	if end := s.Err(); !errors.As(end, &e) || e.Kind != session.KindClosed {
		return end
	}
	return nil
}

// converse describes, prepares and runs the call under the caller's ctx,
// each wait within its bound (Bounds).
func (r *attemptRun) converse(ctx context.Context, s *session.Session) (types.AdapterRunResult, error) {
	b := r.spec.Bounds
	hook("describe")
	d, err := bounded(ctx, b.Start, "start", s.Describe)
	if err != nil {
		return types.AdapterRunResult{}, err
	}
	p := r.spec.Prepare
	if err := exclusive(r.spec, d.ToolSurface); err != nil {
		return types.AdapterRunResult{}, err
	}
	p.Attempt, p.FencingToken, p.Inputs = r.spec.Attempt, r.token, []types.AdapterHandle{r.x.prompt}
	p.Exchange = types.AdapterPrepareParamsExchange{In: r.x.in, Out: r.x.out}
	hook("prepare")
	o, err := bounded(ctx, b.Handshake, "handshake", func(ctx context.Context) (types.AdapterOutcome, error) { return s.Prepare(ctx, p) })
	if err != nil {
		return types.AdapterRunResult{}, err
	}
	if o.Outcome != types.OutcomeEmpty && o.Outcome != types.OutcomeFound {
		return types.AdapterRunResult{}, errors.New("adapterhost: the adapter did not prepare the attempt")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.mu.Lock()
	r.cancelRun = cancel
	r.mu.Unlock()
	return bounded(runCtx, b.Result, "result", func(ctx context.Context) (types.AdapterRunResult, error) {
		return s.Run(ctx, types.AdapterRunParams{Attempt: r.spec.Attempt, FencingToken: r.token, Call: r.spec.Call, Prompt: r.x.prompt}, r.onEvent)
	})
}

// EditLoopTools are the edit loop's own tools: its reads and writes of its
// workspace, one at a time, as a fresh slice. adcd's launch makes them an
// attempt's only ones (Spec.Tools): no executable or concurrent tool, since
// the workspace's confinement holds only while that loop is its sole
// writer (#226 entry 65).
func EditLoopTools() []types.AdapterLabel {
	return []types.AdapterLabel{"read_file", "list_dir", "write_file", "delete_file"}
}

// unknownTool is what the http-chat adapter reports for a tool call it
// refused and did not run.
const unknownTool = "unknown"

// ErrToolRefused: the adapter describes, or reports running, a tool its
// Spec does not allow.
var ErrToolRefused = errors.New("adapterhost: the adapter has a tool its launch does not allow (#226 entry 65)")

// exclusive refuses an adapter whose described tools reach past spec's:
// its Tools, none when nil (fail closed), any only with AnyTool.
func exclusive(spec Spec, surface []types.AdapterLabel) error {
	for _, t := range surface {
		if !spec.AnyTool && !slices.Contains(spec.Tools, t) {
			return fmt.Errorf("%w: %q", ErrToolRefused, t)
		}
	}
	return nil
}

// onEvent keeps usage reports and the final outputs events declare, and
// refuses the run on a tool notification this host does not serve.
func (r *attemptRun) onEvent(e session.Event) {
	hook("event:" + e.Name)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch v := e.Value.Variant.(type) {
	case *types.UsageEvent:
		r.usage = append(r.usage, v)
	case *types.ErrorEvent:
		if !slices.Contains(r.out.Hints, string(v.ClassHint)) {
			r.out.Hints = append(r.out.Hints, string(v.ClassHint))
		}
		for _, h := range r.declared {
			if h == v.RawExcerpt {
				return
			}
		}
		r.declared = append(r.declared, v.RawExcerpt)
	case *types.ToolRequestEvent:
		r.unserve()
	case *types.ToolResultEvent:
		switch {
		case v.Delivered != nil:
			r.unserve() // no delivery record in this increment can match it
		case !v.MediatedByController && !r.spec.AnyTool && v.Tool != unknownTool && !slices.Contains(r.spec.Tools, v.Tool):
			r.toolRefused = string(v.Tool) // a tool it ran that the launch does not allow (Opus #322 L2)
			r.unserve()
		}
	}
}

func (r *attemptRun) unserve() {
	r.unserved = true
	if r.cancelRun != nil {
		r.cancelRun()
	}
}

// ErrCleanup: a staged copy could not be removed (the blob store's Open
// removes it after a restart). Its text is fixed.
var ErrCleanup = errors.New("adapterhost: removing a staged copy of an adapter output failed")

// discard removes every staged copy not published; a copy it could not
// remove stays tracked and is reported (ErrCleanup).
func (r *attemptRun) discard() error {
	failed := r.tempLeft
	for h, st := range r.staged {
		if st.Discard() != nil {
			failed = true // still tracked: the copy is still there
			continue
		}
		delete(r.staged, h)
	}
	if failed {
		return ErrCleanup
	}
	return nil
}

// command runs one writer command of the attempt, action attempt.<step>.
// It goes on only when the command applied (SubmitApplied).
func (h *Host) command(ctx context.Context, attempt types.AttemptID, step string, decide eventstore.Decide) (eventstore.Outcome, error) {
	id, err := newID("cmd")
	if err != nil {
		return eventstore.Outcome{}, err
	}
	return h.Store.SubmitApplied(ctx, eventstore.Command{ID: id, Actor: h.Actor, Action: "attempt." + step, IdempotencyKey: id,
		Input: []byte(`{"attempt":"` + string(attempt) + `","step":"` + step + `"}`)}, decide)
}

// applies is the Decide of an attempt step that only ever applies.
func applies(apply func(*eventstore.Tx) ([]eventstore.Event, error)) eventstore.Decide {
	return eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
		evs, err := apply(tx)
		return evs, json.RawMessage(`{}`), err
	})
}
