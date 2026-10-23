// SPDX-License-Identifier: Apache-2.0

package daemon

// The gateway in adcd (P22a-5a, W16; #226 entries 57, 58, 78; I9, I10).
//   - One gateway per daemon over the store: the registry, the leases and
//     attempts as the store projects them, the credentials from the
//     controller's environment (gateway.EnvSecrets), and the model calls'
//     Recorder; with it one Reconciler, sharing the Recorder's Clock. Both
//     record as the system actor, whose principal the writer rechecks.
//   - Each attempt gets the gateway on its own Unix socket (Server, so the
//     per-write deadlines and the in-flight cap per token apply) and
//     Mint's value for its prepare params: a base_url with no path and the
//     binding's token. The attempt's end, on every path, calls its end:
//     Unbind (its requests in flight cancelled, nothing bound after) and
//     the socket server closed, once; then the call's settlement
//     (Reconciler.Ended), when the call was ever intended, under its own
//     bounded context, retried by a later end until it succeeds.
//   - The settlement driver (Opus #409 M1; settleEnded): a call whose
//     effect is still pending or uncertain once its attempt ended (its
//     end's settlement failed, or a restart left it uncertain) is settled
//     by the same Reconciler at start, after recovery and the start sweep,
//     and on the reconcile loop's passes, with the runner's backoff. With
//     no usage query (none exists in this build) a call whose usage does
//     not settle keeps its liability. Its faults are log-only (no hold, no
//     card state); the liabilities stand, so accounting stays
//     conservative.
//   - Egress capture (§8 item 4) is configured only for an exit run
//     (Config.CaptureEgress): each body sent is kept in the capture store
//     and recorded (api.RecordCapture), and purged at its expiry.
//   - Inert until launch is active: 22a-5c's attempt run is its caller.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
)

// egress is the daemon's gateway and the model calls' reconciler, with its
// settlement driver's state: whether a run of it goes on (Settle), the
// retries of each call it could not settle, and the listing's last logged
// cause.
type egress struct {
	gw    *gateway.Gateway
	recon *modelcall.Reconciler
	log   io.Writer // nil: nothing logged

	mu       sync.Mutex
	settling bool
	retries  map[types.CallID]endRetry
	listed   string
	runs     sync.WaitGroup
}

// newEgress is d's egress over reg; capture keeps the bodies sent (exit
// runs only).
func newEgress(d *Daemon, reg registry.Registry, capture bool) (*egress, error) {
	clock := &modelcall.Clock{Now: d.clock}
	actor := string(d.system.Actor())
	rec := modelcall.Recorder{Store: d.store, Actor: actor, Clock: clock, Principal: d.system}
	read := modelcall.Reader{Store: d.store}
	cfg := gateway.Config{Registry: reg, Leases: read, Attempts: read, Secrets: gateway.EnvSecrets{}, Intents: rec, Counts: rec}
	if capture {
		cfg.Capture = captureOf(d)
	}
	gw, err := gateway.New(cfg)
	if err != nil {
		return nil, err
	}
	return &egress{gw: gw, recon: &modelcall.Reconciler{Store: d.store, Actor: actor, Now: d.clock, Clock: clock, Principal: d.system}}, nil
}

// captureRecordBound bounds a capture's record: a store writer that does
// not answer within it fails the capture, so the request is refused
// capture_failed, and the lock, the request's admission and Unbind are
// released (Mythos #359 L1). The bytes it kept are swept after
// api.CaptureGrace.
const captureRecordBound = 30 * time.Second

// recordCapture is api.RecordCapture; a test makes it wait on its context.
var recordCapture = api.RecordCapture

// captureOf is d's egress capture: each body kept in its capture store and
// recorded as the system actor, whatever becomes of the request (the
// record outlives its context: Opus #322 M1) but within
// captureRecordBound, under the lock the purge takes too (L1).
func captureOf(d *Daemon) gateway.Capture {
	return lockedCapture{mu: &d.capturing, c: gateway.BlobCapture{Blobs: d.captures, Now: d.clock, Record: func(ctx context.Context, c gateway.Captured) error {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), captureRecordBound)
		defer cancel()
		return recordCapture(rctx, d.store, d.system, c)
	}}}
}

// lockedCapture is c under mu.
type lockedCapture struct {
	mu *sync.Mutex
	c  gateway.Capture
}

func (l lockedCapture) Keep(ctx context.Context, b gateway.Binding, body []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.c.Keep(ctx, b, body)
}

// attemptEnd ends an attempt's gateway: what the gateway observed of its
// token, and the failures of closing its socket or settling its call.
type attemptEnd func(ctx context.Context) (gateway.Observed, error)

// settleBound bounds one end's settlement of the call (Reconciler.Ended):
// a provider's usage query within its own deadline, and the record after
// it (Opus #322 L5).
const settleBound = modelcall.QueryDeadline + time.Minute

// open serves the gateway on socket for b's attempt and mints the adapter's
// gateway value. Its end unbinds the token and closes the socket once; it
// settles the call under a context of its own, which the caller's
// cancellation does not end, within settleBound, and a later end retries a
// settlement that failed until one succeeds (Opus #322 M2).
func (e *egress) open(b gateway.Binding, socket string) (types.AdapterPrepareParamsGatewayValue, attemptEnd, error) {
	l, err := net.Listen("unix", socket)
	if err != nil {
		return types.AdapterPrepareParamsGatewayValue{}, nil, err
	}
	v, err := e.gw.Mint(b)
	if err != nil {
		return v, nil, errors.Join(err, l.Close())
	}
	srv := e.gw.Server()
	served := make(chan struct{})
	go func() { defer close(served); _ = srv.Serve(l) }()
	var once sync.Once
	var mu sync.Mutex
	var obs gateway.Observed
	var closeErr error
	settled := false
	end := func(ctx context.Context) (gateway.Observed, error) {
		once.Do(func() {
			obs = e.gw.Unbind(v.Token)
			closeErr = srv.Close()
			<-served
		})
		mu.Lock()
		defer mu.Unlock()
		if settled {
			return obs, closeErr
		}
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleBound)
		defer cancel()
		err := settleCall(e, sctx, b.Call)
		settled = err == nil
		return obs, errors.Join(closeErr, err)
	}
	return v, end, nil
}

// settleCall is egress.settle; a test makes it fail.
var settleCall = (*egress).settle

// Settle starts settleEnded under ctx unless a run of it goes on
// (reconcile.Settler: the loop's pass, on its timer and its nudges).
func (e *egress) Settle(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.settling {
		return
	}
	e.settling = true
	e.runs.Add(1)
	go func() {
		defer e.runs.Done()
		e.settleEnded(ctx)
		e.mu.Lock()
		e.settling = false
		e.mu.Unlock()
	}()
}

// wait waits for a settlement run going on.
func (e *egress) wait() { e.runs.Wait() }

// settleEnded is the model calls' settlement driver (Opus #409 M1; the
// file's comment). It lists the live model_call effects' calls
// (modelcall.Live: their ids alone) and, of those due by the in-memory
// retries (Opus #411 M2), loads each and settles one whose attempt ended
// (modelcall.Unsettled) by the Reconciler's own rule: a pending one as its
// attempt's end does (Ended: out of pending, then reconciled), an
// uncertain one by Reconcile; another runner (an attempt's end) settling
// it leaves it to that one. A try that fails (a read, an unbound call, a
// panic: guardedAs, Opus #411 M1) or leaves the effect live (its usage
// unsettled: the liability is kept, never released at zero) is retried
// with the runner's backoff (retryAfter, capped, never ending), its cause
// logged once per call while it holds. A settlement fault is log-only:
// no hold or card state shows it, and the call's liability stands, so the
// accounting stays conservative (Opus #411 L2). adcd runs it at start,
// after recovery and the start sweep, within settleBound in all (Opus #411
// L1: the rest is the passes'), and on each pass (Settle).
func (e *egress) settleEnded(ctx context.Context) {
	var live []types.CallID
	if err := guardedAs("listing the model calls", func() error {
		return e.recon.Store.View(ctx, func(tx *eventstore.Tx) (err error) {
			live, err = modelcall.Live(tx)
			return err
		})
	}); err != nil {
		e.mu.Lock()
		defer e.mu.Unlock()
		if msg := ids.Redact(err.Error(), "<id>"); msg != e.listed && ctx.Err() == nil {
			e.listed = msg
			e.logf("adcd: the model calls of ended attempts could not be listed: %v", err)
		}
		return
	}
	now := time.Now()
	e.mu.Lock()
	e.listed = ""
	due, kept := live[:0], make(map[types.CallID]endRetry, len(e.retries))
	for _, call := range live {
		r, retried := e.retries[call]
		if retried {
			kept[call] = r
		}
		if !retried || !now.Before(r.next) {
			due = append(due, call)
		}
	}
	e.retries = kept // a call settled meanwhile, or by another runner, is forgotten
	e.mu.Unlock()
	for _, call := range due {
		if ctx.Err() != nil {
			return
		}
		e.settleOne(ctx, call)
	}
}

// settleOne is one try of settleEnded's on call, within settleBound.
func (e *egress) settleOne(ctx context.Context, call types.CallID) {
	sctx, cancel := context.WithTimeout(ctx, settleBound)
	defer cancel()
	var after eventstore.Effect
	ended := false
	err := guardedAs("settling the model call", func() error {
		var eff eventstore.Effect
		if err := e.recon.Store.View(sctx, func(tx *eventstore.Tx) (err error) {
			eff, ended, err = modelcall.Unsettled(tx, call)
			return err
		}); err != nil || !ended {
			return err // an attempt not ended yet is its end's: read again at the next pass
		}
		settle := e.recon.Reconcile
		if eff.State == eventstore.EffectPending {
			settle = e.recon.Ended
		}
		var err error
		after, err = settle(sctx, call)
		return err
	})
	if err == nil && !ended || errors.Is(err, modelcall.ErrRunning) || ctx.Err() != nil {
		return // another runner settles it now, or adcd stops: the next pass reads it again
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err == nil && after.State != eventstore.EffectPending && after.State != eventstore.EffectUncertain {
		delete(e.retries, call)
		return
	}
	r := e.retries[call]
	r.tries++
	r.next = time.Now().Add(retryAfter(r.tries))
	if err != nil {
		if msg := ids.Redact(err.Error(), "<id>"); msg != r.cause {
			r.cause = msg
			e.logf("adcd: the model call %s could not be settled (retried with backoff): %v", call, err)
		}
	}
	if e.retries == nil {
		e.retries = map[types.CallID]endRetry{}
	}
	e.retries[call] = r
}

// logf logs one line to e's log, if any.
func (e *egress) logf(format string, args ...any) {
	if e.log != nil {
		fmt.Fprintf(e.log, format+"\n", args...)
	}
}

// settle ends call's effect once its attempt is over, if it was intended.
func (e *egress) settle(ctx context.Context, call types.CallID) error {
	var bound bool
	err := e.recon.Store.View(ctx, func(tx *eventstore.Tx) (err error) {
		_, bound, err = modelcall.Of(tx, call)
		return err
	})
	if err != nil || !bound {
		return err
	}
	_, err = e.recon.Ended(ctx, call)
	return err
}
