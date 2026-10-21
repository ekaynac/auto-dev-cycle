// SPDX-License-Identifier: Apache-2.0

package capacity_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
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

// overGateway is a gateway counting into w's store through c
// (modelcall.Recorder when nil), with r's call bound, to an upstream
// answering with h.
func (w *world) overGateway(r running, h http.HandlerFunc, c gateway.Counts) (*gateway.Gateway, string) {
	w.t.Helper()
	up := httptest.NewServer(h)
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
	if c == nil {
		c = modelcall.Recorder{Store: w.s, Actor: testActor}
	}
	reader := modelcall.Reader{Store: w.s}
	g, err := gateway.New(gateway.Config{Registry: reg, Leases: asBuilder{reader}, Attempts: reader, Secrets: stubSecrets{}, Intents: bound{}, Counts: c})
	if err != nil {
		w.t.Fatal(err)
	}
	token, err := g.Bind(gateway.Binding{Lease: r.lease, Attempt: r.attempt, Call: r.call})
	if err != nil {
		w.t.Fatal(err)
	}
	return g, token
}

// asked is a request asking for at most 64 output tokens: its floor is 4
// input tokens (56 bytes / 16) and 64 output.
const asked = `{"model":"stub-builder-1","messages":[],"max_tokens":64}`

func send(t *testing.T, g *gateway.Gateway, token string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(asked))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	if g.ServeHTTP(deadlined{rec}, req); rec.Code != http.StatusOK {
		t.Errorf("the gateway answered %d %s", rec.Code, rec.Body)
	}
}

// answering is an upstream answering body.
func answering(body string) http.HandlerFunc {
	return func(rw http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(rw, body) }
}

// sendThroughGateway sends one request of r's call through a gateway
// counting into w's store, to an upstream answering with body.
func (w *world) sendThroughGateway(r running, body string) {
	w.t.Helper()
	g, token := w.overGateway(r, answering(body), nil)
	send(w.t, g, token)
	if o := g.Unbind(token); len(o.Refusals) != 0 {
		w.t.Fatalf("observed %+v", o)
	}
}

// F (P11b), the gate for P17 and P22a activating launch (#226 entry 69): a
// hostile zero final. The gateway forwarded a response reporting 300 input
// and 200 output tokens; the adapter reports zero deltas and a zero final.
// The call never settles on that: it stays a liability at the gateway's
// count (500, over the 400-token reservation), and admission counts it. A
// final that agrees with the count settles, unless the gateway read no
// usage (the request keeps its floor), and a provider reconciliation still
// settles whatever the count.
// zeroOut is usage of 300 prompt tokens and no output; y1600 is 1600
// content bytes, 100 tokens by the content floor.
const zeroOut = `{"prompt_tokens":300,"completion_tokens":0}`

var y1600 = strings.Repeat("y", 1600)

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
		// No usage reported: the request keeps its floor, so no final settles.
		"a final, usage unread":      {`{"choices":[]}`, 300, 200, 500},
		"a zero final, usage unread": {`{"choices":[]}`, 0, 0, 400},
		// Astra on #256: 1600 content bytes the adapter reads, hidden from a
		// case-insensitive or line-by-line reader, never settle at zero output.
		"H1: content behind a case-variant Choices": {`{"choices":[{"message":{"content":"` + y1600 + `"}}],"Choices":[],"usage":` + zeroOut + `}`, 300, 0, 400},
		"H1: tool arguments behind a case variant": {`{"choices":[{"message":{"tool_calls":[{"function":{"arguments":"` + y1600 +
			`","Arguments":""}}]}}],"usage":` + zeroOut + `}`, 300, 0, 400},
		"H2: content over several data lines": {"data: {\"choices\":[\ndata: {\"delta\":{\"content\":\"" + y1600 + "\"}}]}\n\ndata: {\"usage\":" + zeroOut + "}\n\n", 300, 0, 400},
		"H2: malformed content, then usage":   {"data: {\"choices\":[{\"delta\":{\"content\":7}}]}\n\ndata: {\"usage\":" + zeroOut + "}\n\n", 300, 0, 400},
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

// lostCount is the Recorder losing every count after a response, from
// request lose on: a failed commit, or a crash before it.
type lostCount struct {
	modelcall.Recorder
	lose int64
}

func (l lostCount) Count(ctx context.Context, b gateway.Binding, seq int64, n gateway.Count) error {
	if n.Observed && seq >= l.lose {
		return errors.New("lost")
	}
	return l.Recorder.Count(ctx, b, seq, n)
}

// H1 (Opus on #256): a final settles a call only once every request the
// gateway sent for it is counted. Each request's floor is durable before
// it is sent, so a count that fails, never comes (a crash after the
// forward), or is still in flight when the final is recorded, leaves a
// floor that keeps the call a liability: max(400, 304 in + 264 out) = 568,
// request 0's count with request 1's floor.
func TestACallSettlesOnlyWhenEveryRequestIsCounted(t *testing.T) {
	const usage = `{"choices":[],"usage":{"prompt_tokens":300,"completion_tokens":200}}`
	t.Run("the count of request 1 of 2 lost", func(t *testing.T) {
		w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(2000)))
		r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
		g, token := w.overGateway(r, answering(usage), lostCount{modelcall.Recorder{Store: w.s, Actor: testActor}, 1})
		send(t, g, token)
		send(t, g, token)
		if err := w.measure(r, types.UsageMeasurementKindFinal, 0, 300, 200); err != nil { // request 0's figure alone
			t.Fatal(err)
		}
		w.stop(r)
		if s := w.settleDerived(r); len(s.Liabilities) != 1 || w.liable(quota) != 568 {
			t.Fatalf("settlement %+v, liable %d; want a liability of 568", s, w.liable(quota))
		}
		w.replays()
	})
	t.Run("a final while a request is in flight", func(t *testing.T) {
		w := openLedger(t, grp(quota, types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, lim(2000)))
		r := w.running(reserve(quota, 400, types.CapacityUnitTokens))
		floored, release := make(chan int, 2), make(chan bool)
		g, token := w.overGateway(r, func(rw http.ResponseWriter, _ *http.Request) {
			var floors int // what the store holds as the request reaches the upstream
			w.view(func(tx *eventstore.Tx) error {
				ms, err := ledger.Measurements(tx, r.call)
				for _, m := range ms {
					if m.SourceOfTruth == types.UsageMeasurementSourceOfTruthGatewayFloor {
						floors++
					}
				}
				return err
			})
			if floored <- floors; floors == 2 {
				<-release
			}
			_, _ = io.WriteString(rw, usage)
		}, nil)
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		t.Cleanup(unblock) // before the upstream closes, even on a failure
		send(t, g, token)
		done := make(chan bool)
		go func() { send(t, g, token); close(done) }()
		if first, second := <-floored, <-floored; first != 1 || second != 2 {
			t.Fatalf("floors durable at the sends: %d, %d; want each request's", first, second)
		}
		if err := w.measure(r, types.UsageMeasurementKindFinal, 0, 300, 200); err != nil { // what is counted so far
			t.Fatal(err)
		}
		w.stop(r)
		if s := w.settleDerived(r); len(s.Liabilities) != 1 || w.liable(quota) != 568 {
			t.Fatalf("settlement %+v, liable %d; want a liability of 568", s, w.liable(quota))
		}
		unblock()
		<-done
		if l := w.liable(quota); l != 1000 { // the late count: 600 in and 400 out, over the final
			t.Fatalf("liable %d after the count, want 1000", l)
		}
		if err := w.measure(r, types.UsageMeasurementKindFinal, 1, 600, 400); err != nil {
			t.Fatal(err)
		}
		if l := w.liable(quota); l != 0 {
			t.Fatalf("liable %d after a final the counts bear out, want 0", l)
		}
		w.replays()
	})
}

// deadlined is a recorder that takes write deadlines, as the gateway's
// socket connection does: the gateway never writes without one (Opus #308 L5).
type deadlined struct{ *httptest.ResponseRecorder }

func (deadlined) SetWriteDeadline(time.Time) error { return nil }
