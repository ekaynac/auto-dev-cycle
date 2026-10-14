// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// floor is the input floor of the one request up saw.
func floor(t *testing.T, up *upstream) int64 {
	t.Helper()
	got := up.got()
	if len(got) != 1 {
		t.Fatalf("upstream saw %d requests", len(got))
	}
	return (int64(len(got[0].body)) + bytesPerToken - 1) / bytesPerToken
}

func countsOf(g *Gateway) *counts { return g.cfg.Counts.(*counts) }

// The gateway counts each request it sends, from the usage the upstream
// reported in the response it forwarded (a JSON body, or a stream's last
// usage), split as the http-chat adapter splits it, and never below the
// request's floor: an upstream reporting zero is counted at the floor, and
// one reporting nothing at the floor, unobserved (#226 entry 69).
func TestTheGatewayCountsWhatItSent(t *testing.T) {
	for name, c := range map[string]struct {
		body string
		want func(floor int64) Count
	}{
		"a JSON body": {`{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":200}}}`,
			func(int64) Count { return Count{In: 800, Out: 50, CacheRead: 200, Observed: true} }},
		"a stream": {"data: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":9}}\n\ndata: {\"choices\":[]}\n\n" +
			"data: {\"usage\":{\"prompt_tokens\":700,\"completion_tokens\":30}}\n\ndata: [DONE]\n\n",
			func(int64) Count { return Count{In: 700, Out: 30, Observed: true} }},
		"zero usage": {`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`, func(f int64) Count { return Count{In: f, Observed: true} }},
		"no usage":   {`{"choices":[]}`, func(f int64) Count { return Count{In: f} }},
		"negative usage, more cached than prompted": {`{"usage":{"prompt_tokens":3,"completion_tokens":-5,"prompt_tokens_details":{"cached_tokens":9}}}`,
			func(f int64) Count { return Count{In: f - 3, CacheRead: 3, Observed: true} }},
		"not JSON": {`<html>busy</html>`, func(f int64) Count { return Count{In: f} }},
	} {
		up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, c.body) })
		g, token := rig(t, up, nil)
		call(g, token, "/chat/completions", good, nil)
		f := floor(t, up)
		got := countsOf(g).all()
		if f < 1 || len(got) != 1 || got[0].n != c.want(f) || got[0].seq != 0 || got[0].b.Call != callID {
			t.Errorf("%s: counted %+v, want %+v (floor %d)", name, got, c.want(f), f)
		}
	}
}

// Each request of a call is counted under its own sequence; a request
// sent but not forwarded (a redirect, an unsolicited encoding) is counted
// at its floor; a request refused before egress is not counted.
func TestEveryRequestSentIsCountedOnce(t *testing.T) {
	up := newUpstream(t, nil)
	g, token := rig(t, up, nil)
	for range 2 {
		call(g, token, "/chat/completions", good, nil)
	}
	call(g, token, "/chat/completions", `{"model":"stub-reviewer-1"}`, nil) // refused: another model
	if got := countsOf(g).all(); len(got) != 2 || got[0].seq != 0 || got[1].seq != 1 {
		t.Fatalf("counted %+v", got)
	}
	for name, h := range map[string]http.HandlerFunc{
		"a redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://127.0.0.1:1/x", http.StatusFound)
		},
		"an encoding": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "br")
			_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":5000}}`)
		},
	} {
		up := newUpstream(t, h)
		g, token := rig(t, up, nil)
		if w := call(g, token, "/chat/completions", good, nil); w.Code != http.StatusBadGateway {
			t.Fatalf("%s: %d", name, w.Code)
		}
		if got := countsOf(g).all(); len(got) != 1 || got[0].n != (Count{In: floor(t, up)}) {
			t.Errorf("%s: counted %+v", name, got)
		}
	}
}

// A count the gateway cannot record is observed for the exit map; the
// response still reaches the adapter, and the call, with no count, cannot
// settle on the adapter's final (internal/capacity/ledger).
func TestAnUnrecordedCountIsObserved(t *testing.T) {
	g, token := rig(t, newUpstream(t, nil), func(c *Config) { c.Counts = &counts{err: errors.New("disk full")} })
	if w := call(g, token, "/chat/completions", good, nil); w.Code != http.StatusOK {
		t.Fatalf("%d", w.Code)
	}
	if o := g.Unbind(token); len(o.Refusals) != 1 || o.Refusals[0] != RouteUncounted {
		t.Fatalf("observed %+v", o)
	}
}

// failing is an adapter that hung up: every write fails.
type failing struct{ http.ResponseWriter }

func (failing) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// A response the adapter did not take whole is unobserved: an adapter
// hanging up before the usage reached it cannot have its call counted as
// if the gateway had read it.
func TestAResponseNotForwardedWholeIsUnobserved(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":900,"completion_tokens":90}}`)
	})
	g, token := rig(t, up, nil)
	r := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(good))
	r.Header.Set("Authorization", "Bearer "+token)
	g.ServeHTTP(failing{httptest.NewRecorder()}, r)
	if got := countsOf(g).all(); len(got) != 1 || got[0].n != (Count{In: floor(t, up)}) {
		t.Fatalf("counted %+v", got)
	}
}
