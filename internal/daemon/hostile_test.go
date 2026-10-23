// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
)

// egressCall is a seeded store (permissive authority, its quota group
// 1,000 tokens) where an attempt runs on the stub builder's lease
// reserving 400, the restart rule run, and adcd's own egress over it
// (newEgress: its Recorder and Reconciler, as the system principal) whose
// registry routes the stub destination to an upstream answering body. It
// returns the attempt's gateway value, its end, and the call.
type egressCall struct {
	w       *seeder
	sys     types.ActorID
	e       *egress
	l       types.LeaseID
	a       types.AttemptID
	call    types.CallID
	socket  string
	v       types.AdapterPrepareParamsGatewayValue
	end     attemptEnd
	quotaID types.ResourceGroupID
}

func newEgressCall(t *testing.T, body string) *egressCall {
	t.Helper()
	t.Setenv("ADC_CREDENTIAL_STUB", "a-stub-credential") // the controller's environment (gateway.EnvSecrets)
	w := seed(t, stateDir(t), daemonProjection(t, tokens(1000)))
	t.Cleanup(w.done)
	sys := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
	sys.CreatedBy = sys.ID
	w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(sys) })
	claim, err := w.s.ClaimSystem()
	if err != nil {
		t.Fatal(err)
	}
	var p identity.Principal
	w.view(func(tx *eventstore.Tx) (err error) {
		p, err = identity.SystemPrincipal(tx, claim, sys.ID)
		return err
	})
	plan := templateNodes(w, func() types.TaskID { _, task := admittedTask(t, w, sys.ID); return task }())[types.NodeKindPlan]
	w.do(sys.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Claim(tx, plan, map[string]graph.Resolved{}, 1)
	})
	ls := w.lease(400)
	ls.Unit, ls.Executor = graph.UnitOf(plan), stubBuilder
	ls.Deadline = types.Time(time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	w.do(sys.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, ls) })
	w.do(sys.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, ls.ID, 1, lease.LeaseEvent{Kind: lease.Granted}, w.holds)
	})
	c := &egressCall{w: w, sys: sys.ID, l: ls.ID, a: types.AttemptID(newID(t, "att")), call: types.CallID(newID(t, "cal")), quotaID: quota}
	w.do(sys.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.StartAttempt(tx, ls.ID, 2, c.a, emptyBase)
	})
	if _, err := w.s.MarkPendingUncertain(context.Background(), string(sys.ID)); err != nil { // adcd's start rule, which the Recorder requires
		t.Fatal(err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(rw, body) }))
	t.Cleanup(up.Close)
	doc, err := registry.Load("../../fixtures/m1/config/registry-stub.json")
	if err != nil {
		t.Fatal(err)
	}
	for i := range doc.Executors {
		doc.Executors[i].Destination.Endpoint = up.URL + "/v1"
	}
	var gs []types.ResourceGroup
	raw, err := os.ReadFile("../../fixtures/m1/config/groups.json")
	if err == nil {
		err = json.Unmarshal(raw, &gs)
	}
	reg, rerr := registry.New(doc, gs)
	if err != nil || rerr != nil {
		t.Fatal(err, rerr)
	}
	if c.e, err = newEgress(&Daemon{store: w.s, blobs: w.blobs, system: p, clock: time.Now}, reg, false); err != nil {
		t.Fatal(err)
	}
	c.socket = filepath.Join(shortDir(t), "gw.sock")
	if c.v, c.end, err = c.e.open(gateway.Binding{Lease: ls.ID, Attempt: c.a, Call: c.call}, c.socket); err != nil {
		t.Fatal(err)
	}
	return c
}

// send sends one request asking for at most 64 output tokens over the
// attempt's socket: adcd's Recorder intends the call before the send and
// counts the response's usage.
func (c *egressCall) send(t *testing.T) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", c.socket)
	}}}
	req, _ := http.NewRequest(http.MethodPost, c.v.BaseUrl+"/chat/completions", strings.NewReader(`{"model":"stub-builder-1","messages":[],"max_tokens":64}`))
	req.Header.Set("Authorization", "Bearer "+c.v.Token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the gateway answered %d %s", resp.StatusCode, resp.Header.Get(gateway.RefusalHeader))
	}
}

// report records the adapter's report of kind for the call.
func (c *egressCall) report(t *testing.T, kind types.UsageMeasurementKind, in, out int64) {
	t.Helper()
	m := types.UsageMeasurement{ID: types.MeasurementID(newID(t, "msr")), Call: c.call, Attempt: c.a, Lease: c.l, Kind: kind,
		Tokens: types.UsageMeasurementTokens{In: in, Out: out}, Turns: 1, Milliseconds: 5,
		Cost:          types.UsageMeasurementCost{Currency: "USD", Basis: types.UsageMeasurementCostBasisCash, Estimate: true},
		SourceOfTruth: types.UsageMeasurementSourceOfTruthAdapterReport}
	c.w.do(c.sys, func(*eventstore.Tx) ([]eventstore.Event, error) {
		e, err := ledger.Measure(m)
		return []eventstore.Event{e}, err
	})
}

// liable is the quota group's liability.
func (c *egressCall) liable(t *testing.T) int64 {
	t.Helper()
	var n int64
	c.w.view(func(tx *eventstore.Tx) (err error) { n, err = capacity.Liable(tx, c.quotaID); return err })
	return n
}

// A4 (#226 entry 69; the P11b and P17b gates), through adcd's own egress
// wiring (newEgress's Recorder and Reconciler), not the package gateway:
// the upstream reports 300 input and 200 output tokens; the adapter
// reports a zero delta and a final. A hostile zero final never settles the
// call: the attempt's end leaves its effect uncertain and, its lease ended,
// the call a liability at the gateway's count (500, over the 400
// reserved), which the settlement driver keeps; a provider
// reconciliation then settles it through the driver. A final agreeing
// with the count settles at the attempt's end.
func TestAHostileZeroFinalThroughAdcdsEgress(t *testing.T) {
	const usage = `{"choices":[],"usage":{"prompt_tokens":300,"completion_tokens":200}}`
	for name, f := range map[string]struct {
		in, out int64
		liable  int64
		state   eventstore.EffectState
	}{
		"a hostile zero final": {0, 0, 500, eventstore.EffectUncertain},
		"a final agreeing":     {300, 200, 0, eventstore.EffectCompleted},
	} {
		t.Run(name, func(t *testing.T) {
			c := newEgressCall(t, usage)
			c.send(t)
			c.report(t, types.UsageMeasurementKindDelta, 0, 0)
			c.report(t, types.UsageMeasurementKindFinal, f.in, f.out)
			if _, err := c.end(context.Background()); err != nil {
				t.Fatalf("the attempt's end: %v", err)
			}
			c.w.do(c.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.LoseRunning(tx, c.a, types.AttemptLostReasonValueAgentDisconnect)
			})
			ended(c.w, c.sys, c.a)
			if got, n := effectOf(t, c.w.s, c.call), c.liable(t); got.State != f.state || n != f.liable {
				t.Fatalf("after the attempt's end: effect %s (%q), liable %d, want %s and %d", got.State, got.LastObservation, n, f.state, f.liable)
			}
			if f.liable == 0 {
				return
			}
			if err := c.e.settleEnded(context.Background()); err != nil { // its attempt is not sealed (lost): not the driver's yet
				t.Fatal(err)
			}
			c.w.do(c.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				_, rev, _, err := lease.AttemptAt(tx, c.a)
				if err != nil {
					return nil, err
				}
				return lease.SealMinimal(tx, c.a, rev, graph.Units{Tasks: authority.Tasks{}}, c.w.blobs, nil, c.sys)
			})
			if err := c.e.settleEnded(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got, n := effectOf(t, c.w.s, c.call), c.liable(t); got.State != eventstore.EffectUncertain || n != f.liable {
				t.Fatalf("the driver over the hostile final: effect %s (%q), liable %d", got.State, got.LastObservation, n)
			}
			c.e.recon.Queries = map[types.Slug]modelcall.Query{"stub": answered{modelcall.Usage{In: 310, Out: 200}}}
			c.e.mu.Lock()
			r := c.e.retries[c.call]
			r.next = time.Now() // its retry is due
			c.e.retries[c.call] = r
			c.e.mu.Unlock()
			if err := c.e.settleEnded(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got, n := effectOf(t, c.w.s, c.call), c.liable(t); got.State != eventstore.EffectCompleted || n != 0 {
				t.Fatalf("after the provider's answer: effect %s (%q), liable %d", got.State, got.LastObservation, n)
			}
		})
	}
}

// answered is a usage query answering u.
type answered struct{ u modelcall.Usage }

func (a answered) Usage(context.Context, types.ModelCall) (modelcall.Usage, bool, error) {
	return a.u, true, nil
}
