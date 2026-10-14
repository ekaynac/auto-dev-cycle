// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// asked is a request with an output bound: its floor's output.
const asked = `{"model":"stub-builder-1","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`

// sentFloor is the floor of the one request up saw, max_tokens 64.
func sentFloor(t *testing.T, up *upstream) Count {
	t.Helper()
	got := up.got()
	if len(got) != 1 {
		t.Fatalf("upstream saw %d requests", len(got))
	}
	return Count{In: tokens(len(got[0].body)), Out: 64}
}

func countsOf(g *Gateway) *counts { return g.cfg.Counts.(*counts) }

// zeroOut is usage of 300 prompt tokens and no output; none is no count
// after the floor.
const zeroOut = `{"prompt_tokens":300,"completion_tokens":0}`

func none(Count) []Count { return nil }

// H1 (Opus on #256): each request's floor is durable before it is sent,
// and the count from the response it forwarded follows at the same
// sequence: the upstream's usage, split as the http-chat adapter splits
// it, never below the floor's input or the content's bytes / 16 in output.
// A response with no usage leaves the floor alone; output the content
// outweighs leaves the count a floor (M2).
func TestTheGatewayCountsWhatItSent(t *testing.T) {
	long := strings.Repeat("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n", maxBody/40)
	for name, c := range map[string]struct {
		body string
		want func(f Count) []Count // after the floor
	}{
		"a JSON body": {`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":200}}}`,
			func(Count) []Count { return []Count{{In: 800, Out: 50, CacheRead: 200, Observed: true}} }},
		"a stream": {"data: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":9}}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
			"data: {\"usage\":{\"prompt_tokens\":700,\"completion_tokens\":30}}\n\ndata: [DONE]\n\n",
			func(Count) []Count { return []Count{{In: 700, Out: 30, Observed: true}} }},
		"a stream past maxBody, usage last": {long + "data: {\"usage\":{\"prompt_tokens\":700,\"completion_tokens\":20000}}\n\n",
			func(Count) []Count { return []Count{{In: 700, Out: 20000, Observed: true}} }},
		"zero usage": {`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`, func(f Count) []Count { return []Count{{In: f.In, Observed: true}} }},
		"zero output, 160 bytes of content": {`{"choices":[{"message":{"content":"` + strings.Repeat("y", 150) + `","tool_calls":[{"function":{"arguments":"0123456789"}}]}}],"usage":{"prompt_tokens":300,"completion_tokens":0}}`,
			func(Count) []Count { return []Count{{In: 300, Out: 10}} }},
		"more cached than prompted": {`{"usage":{"prompt_tokens":3,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":9}}}`,
			func(f Count) []Count { return []Count{{In: f.In - 3, Out: 5, CacheRead: 3, Observed: true}} }},
		// Astra on #256: whatever a reader cannot read exactly keeps the floor.
		"negative usage":                        {`{"usage":{"prompt_tokens":3,"completion_tokens":-5}}`, none},
		"usage half there":                      {`{"usage":{"prompt_tokens":3}}`, none},
		"content not text":                      {`{"choices":[{"message":{"content":[{"text":"x"}]}}],"usage":` + zeroOut + `}`, none},
		"H1: Choices empty":                     {`{"choices":[{"message":{"content":"` + strings.Repeat("y", 1600) + `"}}],"Choices":[],"usage":` + zeroOut + `}`, none},
		"H1: a case variant in a choice":        {`{"choices":[{"message":{"content":"` + strings.Repeat("y", 1600) + `"},"Message":{}}],"usage":` + zeroOut + `}`, none},
		"H1: content under a case variant only": {`{"choices":[],"Choices":[{"message":{"content":"` + strings.Repeat("y", 1600) + `"}}],"usage":` + zeroOut + `}`, none},
		"H1: a case variant in a message":       {`{"choices":[{"message":{"content":"` + strings.Repeat("y", 1600) + `","Content":""}}],"usage":` + zeroOut + `}`, none},
		"H1: tool arguments under a case variant": {`{"choices":[{"message":{"tool_calls":[{"function":{"arguments":"` +
			strings.Repeat("y", 1600) + `","Arguments":""}}]}}],"usage":` + zeroOut + `}`, none},
		"H2: an event over several data lines, any line ending": {"data: {\"choices\":[\r\ndata: {\"delta\":{\"content\":\"" +
			strings.Repeat("y", 1600) + "\"}}]}\r\rdata: {\"usage\":" + zeroOut + "}\n\ndata: [DONE]\n\n",
			func(Count) []Count { return []Count{{In: 300, Out: 100}} }},
		"H2: malformed content, then valid usage": {"data: {\"choices\":[{\"delta\":{\"content\":7}}]}\n\ndata: {\"usage\":" + zeroOut + "}\n\n", none},
		"H2: a cut event, then valid usage":       {"data: {\"choices\":[\n\ndata: {\"usage\":" + zeroOut + "}\n\n", none},
		// Mythos on #256, M1 and L4: a leading BOM is stripped, so the
		// first event is read; a usage event cut at EOF is still read.
		"M1: a BOM before the first event": {"\xEF\xBB\xBFdata: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("y", 1600) +
			"\"}}]}\n\ndata: {\"usage\":" + zeroOut + "}\n\n", func(Count) []Count { return []Count{{In: 300, Out: 100}} }},
		"L4: usage with no blank line after it": {"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"usage\":{\"prompt_tokens\":300,\"completion_tokens\":30}}",
			func(Count) []Count { return []Count{{In: 300, Out: 30, Observed: true}} }},
		"no usage": {`{"choices":[]}`, func(Count) []Count { return nil }},
		"not JSON": {`<html>busy</html>`, func(Count) []Count { return nil }},
	} {
		up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, c.body) })
		g, token := rig(t, up, nil)
		call(g, token, "/chat/completions", asked, nil)
		f := sentFloor(t, up)
		want := append([]Count{f}, c.want(f)...)
		got := countsOf(g).all()
		ok := len(got) == len(want)
		for i := 0; ok && i < len(got); i++ {
			ok = got[i].n == want[i] && got[i].seq == 0 && got[i].b.Call == callID
		}
		if !ok || f.In < 1 {
			t.Errorf("%s: counted %+v, want %+v", name, got, want)
		}
	}
}

// Each request has its own sequence; a request refused before egress has
// no floor; one sent but not forwarded (a redirect, an unsolicited
// encoding) keeps its floor; one never sent (its dial failed) is counted
// at zero. With no max_tokens, the floor's output is the lease's token
// reservation.
func TestEveryRequestSentIsCountedOnce(t *testing.T) {
	g, token := rig(t, newUpstream(t, nil), nil)
	for range 2 {
		call(g, token, "/chat/completions", good, nil)
	}
	call(g, token, "/chat/completions", `{"model":"stub-reviewer-1"}`, nil) // refused: another model
	if got := countsOf(g).all(); len(got) != 2 || got[0].seq != 0 || got[1].seq != 1 || got[0].n.Out != 0 {
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
		if w := call(g, token, "/chat/completions", asked, nil); w.Code != http.StatusBadGateway {
			t.Fatalf("%s: %d", name, w.Code)
		}
		if got := countsOf(g).all(); len(got) != 1 || got[0].n != sentFloor(t, up) {
			t.Errorf("%s: counted %+v", name, got)
		}
	}
	up := newUpstream(t, nil)
	up.Close() // nothing listens: the dial fails
	g, token = rig(t, up, func(c *Config) {
		l := running(builder)
		l[leaseID] = withTokens(l[leaseID], 900)
		c.Leases = l
	})
	if w := call(g, token, "/chat/completions", good, nil); w.Code != http.StatusBadGateway {
		t.Fatalf("a failed dial: %d", w.Code)
	}
	if got := countsOf(g).all(); len(got) != 2 || got[0].n.Out != 900 || got[1].n != (Count{Observed: true}) || got[1].seq != got[0].seq {
		t.Fatalf("a failed dial: counted %+v", got)
	}
}

// H1: a floor the gateway cannot record refuses the send, as an
// unrecorded intent does; a count it cannot record after the response is
// observed, and the floor stands.
func TestCountingFailsClosed(t *testing.T) {
	up := newUpstream(t, nil)
	g, token := rig(t, up, func(c *Config) { c.Counts = &counts{err: errors.New("disk full")} })
	if w := call(g, token, "/chat/completions", good, nil); w.Code != http.StatusServiceUnavailable || w.Header().Get(RefusalHeader) != RouteUncounted || len(up.got()) != 0 {
		t.Fatalf("an unrecorded floor: %d %v, upstream saw %d", w.Code, w.Header(), len(up.got()))
	}
	up = newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	g, token = rig(t, up, func(c *Config) { c.Counts = &counts{observed: errors.New("disk full")} })
	if w := call(g, token, "/chat/completions", good, nil); w.Code != http.StatusOK {
		t.Fatalf("%d", w.Code)
	}
	if o, got := g.Unbind(token), countsOf(g).all(); len(o.Refusals) != 1 || o.Refusals[0] != RouteUncounted || len(got) != 1 || got[0].n.Observed {
		t.Fatalf("observed %+v, counted %+v", o, got)
	}
}

// failing is an adapter that hung up: every write fails.
type failing struct{ http.ResponseWriter }

func (failing) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// A response the adapter did not take whole leaves the floor alone: an
// adapter hanging up before the usage reached it cannot have its call
// counted as if the gateway had read it.
func TestAResponseNotForwardedWholeIsUnobserved(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":900,"completion_tokens":90}}`)
	})
	g, token := rig(t, up, nil)
	r := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(asked))
	r.Header.Set("Authorization", "Bearer "+token)
	g.ServeHTTP(failing{httptest.NewRecorder()}, r)
	if got := countsOf(g).all(); len(got) != 1 || got[0].n != sentFloor(t, up) {
		t.Fatalf("counted %+v", got)
	}
}

// withTokens is l reserving amount tokens.
func withTokens(l types.Lease, amount int64) types.Lease {
	l.Reservations = []types.LeaseReservationsItem{{Group: "rg_01m4tm99haemss9tvzd438vq8j", Amount: types.Amount(amount), Unit: types.CapacityUnitTokens}}
	return l
}

// Astra on #256: concurrent requests take distinct sequences, and a count
// lands at its own request's sequence whatever order the requests end in.
func TestCountsKeepTheirRequestsSequence(t *testing.T) {
	var n atomic.Int32
	arrived, first := make(chan bool), make(chan bool)
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		k := n.Add(1)
		if k == 1 {
			arrived <- true
			<-first // the first request ends last
		}
		_, _ = fmt.Fprintf(w, `{"usage":{"prompt_tokens":%d,"completion_tokens":1}}`, 100+k)
	})
	g, token := rig(t, up, nil)
	done := make(chan bool)
	go func() { call(g, token, "/chat/completions", good, nil); close(done) }()
	<-arrived
	call(g, token, "/chat/completions", good, nil)
	close(first)
	<-done
	var order []counted
	for _, c := range countsOf(g).all() {
		if c.n.Observed {
			order = append(order, c)
		}
	}
	if len(order) != 2 || order[0].n.In != 102 || order[0].seq != 1 || order[1].n.In != 101 || order[1].seq != 0 {
		t.Fatalf("counts in completion order %+v; want request 1's at 1, then request 0's at 0", order)
	}
	g, token = rig(t, newUpstream(t, nil), nil)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() { call(g, token, "/chat/completions", good, nil) })
	}
	wg.Wait()
	seqs := map[int64]bool{}
	for _, c := range countsOf(g).all() {
		seqs[c.seq] = true
	}
	if len(seqs) != 16 {
		t.Fatalf("16 concurrent requests took %d sequences", len(seqs))
	}
}

// Mythos on #256, L4: a stream fed one byte at a time, its BOM and every
// CRLF split across writes, is read whole: a CR is not taken for a line end
// before its LF arrives.
func TestAStreamReadByteByByte(t *testing.T) {
	stream := "\xEF\xBB\xBFdata: {\"choices\":[\r\ndata: {\"delta\":{\"content\":\"hello\"}}]}\r\n\r\n" +
		"data: {\"usage\":{\"prompt_tokens\":300,\"completion_tokens\":30}}\r\n\r\n"
	var tl tally
	for i := range len(stream) {
		_, _ = tl.Write([]byte{stream[i]})
	}
	if u, n, ok := tl.result(true); !ok || u.Prompt != 300 || u.Completion != 30 || n != 5 {
		t.Fatalf("read %+v, %d content bytes, ok %v", u, n, ok)
	}
}
