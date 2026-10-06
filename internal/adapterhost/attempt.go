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
	"sync"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/ingest"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
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

// Spec is one attempt to run under a granted lease.
type Spec struct {
	Lease         types.LeaseID
	LeaseRevision int64           // the granted lease's projection revision
	Attempt       types.AttemptID // a fresh id: the unit's first attempt, token 1
	Base          types.AttemptBase
	Call          types.CallID
	Adapter       session.Config
	// Prepare is prepare's params; attempt, fencing token, exchange and
	// inputs are the host's.
	Prepare types.AdapterPrepareParams
	Prompt  []byte
}

// Outcome is what an attempt left.
type Outcome struct {
	// StartRefused: the prelaunch check failed (wrapping
	// ingest.ErrUnsupported), lease.start_refused{precondition_failed} was
	// recorded, no attempt exists and nothing was launched.
	StartRefused error
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

func hook(stage string) {
	if testHook != nil {
		testHook(stage)
	}
}

// Run runs spec's attempt to its end. An error is the host's (the stores,
// the exchange root, a spawn, an ingestion fault), with a fixed text; the
// adapter's failures are in the Outcome.
func (h *Host) Run(ctx context.Context, spec Spec) (out Outcome, err error) {
	x, err := newExchange(h.Root, spec.Attempt, spec.Prompt)
	if err != nil {
		return Outcome{}, err
	}
	defer func() { err = errors.Join(err, x.remove()) }()
	if perr := probe(x.outDir); perr != nil {
		_, err := h.command(ctx, spec.Attempt, "start-refused", func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.ChangeLease(tx, spec.Lease, spec.LeaseRevision,
				lease.LeaseEvent{Kind: lease.StartRefused, Reason: vocab.ReasonPreconditionFailed.String()}, nil)
		})
		return Outcome{StartRefused: perr}, err
	}
	if _, err := h.command(ctx, spec.Attempt, "start", func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.StartAttempt(tx, spec.Lease, spec.LeaseRevision, spec.Attempt, spec.Base)
	}); err != nil {
		return Outcome{}, err
	}
	r := &attemptRun{h: h, spec: spec, token: 1, x: x} // lease.StartAttempt mints token 1
	return r.run(ctx)
}

// attemptRun is one started attempt's state.
type attemptRun struct {
	h     *Host
	spec  Spec
	token int64
	x     *exchange

	mu        sync.Mutex // the session's reader adds to these
	usage     []*types.UsageEvent
	declared  []types.AdapterHandle // final outputs the events declared, deduplicated
	unserved  bool
	cancelRun context.CancelFunc

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
	unserved := r.unserved
	r.mu.Unlock()
	switch {
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

// shutdown shuts the session down within shutdownTimeout, killing the
// namespace otherwise, and returns the shutdown's error; an outcome other
// than empty or found is ErrShutdownFailed.
func (r *attemptRun) shutdown(ctx context.Context, s *session.Session) error {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	o, err := s.Shutdown(sctx)
	if err != nil {
		_ = s.Close()
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

// converse describes, prepares and runs the call under the caller's ctx.
func (r *attemptRun) converse(ctx context.Context, s *session.Session) (types.AdapterRunResult, error) {
	hook("describe")
	if _, err := s.Describe(ctx); err != nil {
		return types.AdapterRunResult{}, err
	}
	p := r.spec.Prepare
	p.Attempt, p.FencingToken, p.Inputs = r.spec.Attempt, r.token, []types.AdapterHandle{r.x.prompt}
	p.Exchange = types.AdapterPrepareParamsExchange{In: r.x.in, Out: r.x.out}
	hook("prepare")
	o, err := s.Prepare(ctx, p)
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
	return s.Run(runCtx, types.AdapterRunParams{Attempt: r.spec.Attempt, FencingToken: r.token, Call: r.spec.Call, Prompt: r.x.prompt}, r.onEvent)
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
		for _, h := range r.declared {
			if h == v.RawExcerpt {
				return
			}
		}
		r.declared = append(r.declared, v.RawExcerpt)
	case *types.ToolRequestEvent:
		r.unserve()
	case *types.ToolResultEvent:
		if v.Delivered != nil {
			r.unserve() // no delivery record in this increment can match it
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

// command runs one writer command of the attempt.
func (h *Host) command(ctx context.Context, attempt types.AttemptID, step string, apply func(*eventstore.Tx) ([]eventstore.Event, error)) (eventstore.Outcome, error) {
	id, err := newID("cmd")
	if err != nil {
		return eventstore.Outcome{}, err
	}
	return h.Store.Submit(ctx, eventstore.Command{ID: id, Actor: h.Actor, IdempotencyKey: id,
		Input: []byte(`{"attempt":"` + string(attempt) + `","step":"` + step + `"}`)},
		func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := apply(tx)
			return evs, json.RawMessage(`{}`), err
		})
}
