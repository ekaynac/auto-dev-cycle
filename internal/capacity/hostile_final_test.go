// SPDX-License-Identifier: Apache-2.0

package capacity_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	stubConfig  = "../../fixtures/m1/config/"
	stubBuilder = types.ExecutorID("exe_01m4tm99haemqt8es2qfbwj8pa") // registry-stub.json: destination stub, credential stub
)

// asBuilder reads leases as the store holds them, run by the stub
// registry's builder: the gateway checks the executor against its registry.
type asBuilder struct{ modelcall.Reader }

func (a asBuilder) Lease(ctx context.Context, id types.LeaseID) (types.Lease, bool, error) {
	l, ok, err := a.Reader.Lease(ctx, id)
	l.Executor = stubBuilder
	return l, ok, err
}

type stubSecrets struct{}

func (stubSecrets) Credential(types.Slug) (gateway.Credential, bool, error) {
	return gateway.NewCredential("fake-credential-0123456789"), true, nil
}

// bound: the binding is the running lease's, made by modelcall.Intend.
type bound struct{}

func (bound) Intend(context.Context, gateway.Binding, types.Slug) error { return nil }

// sendThroughGateway sends one request of r's call through a gateway
// counting into w's store, to an upstream answering with body.
func (w *world) sendThroughGateway(r running, body string) {
	w.t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(rw, body) }))
	w.t.Cleanup(up.Close)
	doc, err := registry.Load(stubConfig + "registry-stub.json")
	if err != nil {
		w.t.Fatal(err)
	}
	for i, e := range doc.Executors {
		if strings.HasPrefix(e.Destination.Endpoint, "http://127.0.0.1:") {
			doc.Executors[i].Destination.Endpoint = up.URL + "/v1"
		}
	}
	var groups []types.ResourceGroup
	raw, err := os.ReadFile(stubConfig + "groups.json")
	if err == nil {
		err = json.Unmarshal(raw, &groups)
	}
	reg, rerr := registry.New(doc, groups)
	if err != nil || rerr != nil {
		w.t.Fatal(err, rerr)
	}
	reader := modelcall.Reader{Store: w.s}
	g, err := gateway.New(gateway.Config{Registry: reg, Leases: asBuilder{reader}, Attempts: reader, Secrets: stubSecrets{}, Intents: bound{},
		Counts: modelcall.Recorder{Store: w.s, Actor: testActor}})
	if err != nil {
		w.t.Fatal(err)
	}
	token, err := g.Bind(gateway.Binding{Lease: r.lease, Attempt: r.attempt, Call: r.call})
	if err != nil {
		w.t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{"model":"stub-builder-1","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if o := g.Unbind(token); rec.Code != http.StatusOK || len(o.Refusals) != 0 {
		w.t.Fatalf("the gateway answered %d %s, observed %+v", rec.Code, rec.Body, o)
	}
}

// F (P11b), the gate for P17 and P22a activating launch (#226 entry 69): a
// hostile zero final. The gateway forwarded a response reporting 300 input
// and 200 output tokens; the adapter reports zero deltas and a zero final.
// The call never settles on that: it stays a liability at the gateway's
// count (500, over the 400-token reservation), and admission counts it. A
// final that agrees with the count settles, unless the gateway read no
// usage (its count is then a floor), and a provider reconciliation still
// settles whatever the count.
func TestAHostileZeroFinalKeepsAConservativeCharge(t *testing.T) {
	const usage = `{"choices":[],"usage":{"prompt_tokens":300,"completion_tokens":200}}`
	for name, c := range map[string]struct {
		response string
		in, out  int64
		liable   int64
	}{
		"a hostile zero final":   {usage, 0, 0, 500},
		"a final under it":       {usage, 300, 199, 500},
		"a final agreeing":       {usage, 300, 200, 0},
		"a final over the count": {usage, 320, 200, 0},
		// No usage reported: the gateway's count is a floor, so no final settles.
		"a final, usage unread":      {`{"choices":[]}`, 300, 200, 500},
		"a zero final, usage unread": {`{"choices":[]}`, 0, 0, 400},
	} {
		t.Run(name, func(t *testing.T) {
			w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(1000)))
			r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
			w.sendThroughGateway(r, c.response)
			if err := w.measure(r, types.UsageMeasurementKindDelta, 0, 0, 0); err != nil {
				t.Fatal(err)
			}
			if err := w.measure(r, types.UsageMeasurementKindFinal, 0, c.in, c.out); err != nil {
				t.Fatal(err)
			}
			w.stop(r)
			s := w.settleDerived(r)
			if got := w.liable(quota); got != c.liable || (len(s.Liabilities) == 1) != (c.liable > 0) {
				t.Fatalf("liable %d with settlement %+v, want %d", got, s, c.liable)
			}
			free := 1000 - c.liable
			exhausted(t, w.change(w.requested(reserve(quota, free+1, types.CapacityUnitTokens)), 1, granted), vocab.ReasonCapacityReservedFull)
			if err := w.change(w.requested(reserve(quota, free, types.CapacityUnitTokens)), 1, granted); err != nil {
				t.Fatalf("what the liability leaves was not granted: %v", err)
			}
			w.replays()
			if err := w.measure(r, types.UsageMeasurementKindProviderReconciliation, 0, 10, 1); err != nil {
				t.Fatal(err)
			}
			if got := w.liable(quota); got != 0 {
				t.Fatalf("liable %d after the provider reconciliation, want 0", got)
			}
		})
	}
}
