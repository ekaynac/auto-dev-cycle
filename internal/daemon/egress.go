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
//     binding's token. The attempt's end, on every path, calls its end
//     once: Unbind (its requests in flight cancelled, nothing bound after),
//     the socket server closed, then the call's settlement
//     (Reconciler.Ended) when the call was ever intended.
//   - Egress capture (§8 item 4) is configured only for an exit run
//     (Config.CaptureEgress): each body sent is kept in the capture store
//     and recorded (api.RecordCapture), and purged at its expiry.
//   - Inert until launch is active: 22a-5c's attempt run is its caller.

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
)

// egress is the daemon's gateway and the model calls' reconciler.
type egress struct {
	gw    *gateway.Gateway
	recon *modelcall.Reconciler
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

// captureOf is d's egress capture: each body kept in its capture store and
// recorded as the system actor.
func captureOf(d *Daemon) gateway.BlobCapture {
	return gateway.BlobCapture{Blobs: d.captures, Now: d.clock, Record: func(ctx context.Context, c gateway.Captured) error {
		return api.RecordCapture(ctx, d.store, d.system, c)
	}}
}

// attemptEnd ends an attempt's gateway: what the gateway observed of its
// token, and the failures of closing its socket or settling its call.
type attemptEnd func(ctx context.Context) (gateway.Observed, error)

// open serves the gateway on socket for b's attempt and mints the adapter's
// gateway value. Its end is idempotent: the first call does the work, and
// every later one returns what it returned.
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
	var obs gateway.Observed
	var endErr error
	end := func(ctx context.Context) (gateway.Observed, error) {
		once.Do(func() {
			obs = e.gw.Unbind(v.Token)
			endErr = srv.Close()
			<-served
			endErr = errors.Join(endErr, e.settle(ctx, b.Call))
		})
		return obs, endErr
	}
	return v, end, nil
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
