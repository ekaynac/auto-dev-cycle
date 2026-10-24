// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// The gateway's own count of a call (#226 entry 69). An adapter's usage is its report, and a hostile one can report
// zero deltas and a zero final; so the gateway records what it saw itself, per request it sends, under one sequence
// (internal/modelcall records each as a usage delta of source gateway_floor or gateway_count):
//   - before the send, durably, a floor: input from the bytes it sends, one token per bytesPerToken (rounded up), and
//     output bounded by the request's max_tokens (or, with none, the lease's token reservation). A floor it cannot
//     record refuses the send, as an unrecorded intent does. The adapter's max_tokens cannot lower a charge: while
//     a call has a floor, it owes at least the lease's reservation, whatever the floor says;
//   - after forwarding the whole response, the count: input the larger of the upstream's prompt_tokens and that
//     floor; output the upstream's completion_tokens, at least the forwarded content's bytes / bytesPerToken. It
//     supersedes the floor at its sequence. A count whose output the content outweighs stays a floor. The response is
//     read exactly (by exact keys, its stream events assembled) or not at all: what it cannot read keeps the floor.
//
// A request with no count after its floor (its usage unread: none reported, a 429 or another error with none, the
// response not forwarded whole; a crash or a failed commit; a request still in flight) leaves a floor, and the
// liability ledger then settles the call on no adapter final, only on a provider reconciliation; otherwise a final
// settles it when it is not below the count (ledger.StandingOf). A request never sent (its dial failed) is counted at
// zero. The split mirrors the http-chat adapter's (P13): cached prompt tokens are cache reads, the rest input. An
// upstream under-reporting its usage past these floors is beyond any count: the provider's reconciliation (P20).

// Counts records the gateway's count of one request it sent for a call. Count returns once the report is durable.
type Counts interface {
	Count(ctx context.Context, b Binding, sequence int64, n Count) error
}

// Count is what the gateway counted for one request, in tokens. Observed: a count, not a floor.
type Count struct {
	In, Out, CacheRead int64
	Observed           bool
}

// bytesPerToken bounds the bytes one token stands for. Text and code tokenize at about 3 to 6 bytes a token, JSON
// framing included, so the floors stay below an honest count; a request holding mostly images could exceed it,
// which only keeps its call uncertain until a reconciliation.
const bytesPerToken = 16

// RouteUncounted: the gateway could not record its count of a request.
const RouteUncounted = "count_unrecorded"

const maxCount = 9007199254740991 // the contract's integers

// sending is a request sent: its credential, its count's sequence, and its size.
type sending struct {
	cred Credential
	seq  int64
	sent int
}

// tokens is the floor of a text of n bytes.
func tokens(n int) int64 { return (int64(n) + bytesPerToken - 1) / bytesPerToken }

// Floor is the count of body before it is sent, under l: the one count a
// request's floor is, which adcd's per-call floors are derived through
// too (qualify.RequestFloor).
func Floor(body []byte, l types.Lease) Count {
	out := int64(0)
	if req, ok := object(body, "max_tokens", "max_completion_tokens"); ok {
		for _, k := range []string{"max_tokens", "max_completion_tokens"} {
			if v := int64(0); present(req, k) && count(req[k], &v) {
				out = max(out, v)
			}
		}
	}
	for _, r := range l.Reservations {
		if out <= 0 && r.Unit == types.CapacityUnitTokens {
			out = int64(r.Amount)
		}
	}
	return Count{In: tokens(len(body)), Out: min(max(out, 0), maxCount)}
}

// count records the count of request s from its response t, once forwarded whole, superseding its floor. It is
// recorded even if the adapter has hung up.
func (g *Gateway) count(r *http.Request, b Binding, s sending, t *tally, whole bool) {
	u, content, ok := t.result(whole)
	if !ok {
		return // its usage unread: the floor stands
	}
	clamp := func(v int64) int64 { return min(max(v, 0), maxCount) }
	prompt, out := clamp(u.Prompt), clamp(u.Completion)
	cached := min(clamp(u.Cached), prompt)
	n := Count{In: max(prompt, tokens(s.sent)) - cached, Out: max(out, tokens(content)), CacheRead: cached, Observed: out >= tokens(content)}
	if err := g.cfg.Counts.Count(context.WithoutCancel(r.Context()), b, s.seq, n); err != nil {
		g.observe(b.Attempt, 0, RouteUncounted, 0)
	}
}

// usage is the upstream's usage report, its counters non-negative integers.
type usage struct{ Prompt, Completion, Cached int64 }

// object decodes raw as a JSON object, read by exact key only (Go's struct decoding matches keys case-insensitively,
// and the adapter reads them exactly). It is not ok when raw is no object, or when it holds a key that only a case
// differs from one of names: another parser may read that one, so the reading is uncertain (Astra on #256, H1).
func object(raw json.RawMessage, names ...string) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, false
	}
	for k := range m {
		if slices.ContainsFunc(names, func(n string) bool { return n != k && strings.EqualFold(n, k) }) {
			return nil, false
		}
	}
	return m, true
}

// present: the key is there and not null.
func present(m map[string]json.RawMessage, key string) bool {
	v, ok := m[key]
	return ok && string(v) != "null"
}

// parse reads an OpenAI-compatible reply, a body or one stream event: its usage (nil with none) and its generated
// text's and tool arguments' bytes. It is not ok when any of those cannot be read exactly; the caller keeps the floor.
func parse(raw []byte) (u *usage, content int, ok bool) {
	top, ok := object(raw, "choices", "usage")
	if ok && present(top, "usage") {
		u, ok = usageOf(top["usage"])
	}
	var choices []json.RawMessage
	if ok && present(top, "choices") {
		ok = json.Unmarshal(top["choices"], &choices) == nil
	}
	for _, c := range choices {
		choice, fine := object(c, "message", "delta")
		ok = ok && fine
		for _, part := range []string{"message", "delta"} {
			if ok && present(choice, part) {
				var n int
				n, ok = generated(choice[part])
				content += n
			}
		}
	}
	return u, content, ok
}

func usageOf(raw json.RawMessage) (*usage, bool) {
	m, ok := object(raw, "prompt_tokens", "completion_tokens", "prompt_tokens_details")
	var u usage
	ok = ok && count(m["prompt_tokens"], &u.Prompt) && count(m["completion_tokens"], &u.Completion)
	if ok && present(m, "prompt_tokens_details") {
		d, fine := object(m["prompt_tokens_details"], "cached_tokens")
		ok = fine && (!present(d, "cached_tokens") || count(d["cached_tokens"], &u.Cached))
	}
	return &u, ok
}

// count reads a non-negative integer.
func count(raw json.RawMessage, v *int64) bool { return json.Unmarshal(raw, v) == nil && *v >= 0 }

// generated is the bytes of a message's or delta's text and tool arguments.
func generated(raw json.RawMessage) (n int, ok bool) {
	m, ok := object(raw, "content", "tool_calls")
	var text string
	if ok && present(m, "content") {
		ok = json.Unmarshal(m["content"], &text) == nil
	}
	var calls []json.RawMessage
	if ok && present(m, "tool_calls") {
		ok = json.Unmarshal(m["tool_calls"], &calls) == nil
	}
	n = len(text)
	for _, c := range calls {
		call, fine := object(c, "function")
		if !fine {
			return 0, false
		}
		if !present(call, "function") {
			continue
		}
		f, fine := object(call["function"], "arguments")
		var args string
		if !fine || present(f, "arguments") && json.Unmarshal(f["arguments"], &args) != nil {
			return 0, false
		}
		n += len(args)
	}
	return n, ok
}

// tally reads a response as it is forwarded: its first maxBody bytes, for a JSON body, and, for a stream, its
// server-sent events, assembled as the specification assembles them (lines ending in CRLF, CR or LF; `data:` lines
// joined with LF; an event ending at a blank line). Any event it cannot read exactly, or past maxBody, makes the
// accounting incomplete for good: a later usage event does not clear that, and the floor stands (Astra on #256, H2).
type tally struct {
	body, pending, data []byte
	dropped, incomplete bool // dropped: body is not all of it
	events, started     bool // events: data is an event being assembled; started: past a leading BOM
	scanned             int  // pending[:scanned] holds no line ending
	usage               *usage
	content             int
}

func (t *tally) Write(p []byte) (int, error) {
	n := min(len(p), max(maxBody-len(t.body), 0))
	t.body, t.dropped = append(t.body, p[:n]...), t.dropped || n < len(p)
	t.pending = append(t.pending, p...)
	if bom := []byte("\xEF\xBB\xBF"); !t.started && (len(t.pending) >= len(bom) || !bytes.HasPrefix(bom, t.pending)) {
		t.pending, t.started = bytes.TrimPrefix(t.pending, bom), true // the specification strips one (Mythos on #256, M1)
	}
	for t.started {
		t.scanned = min(t.scanned, len(t.pending)) // never past what is pending
		i := bytes.IndexAny(t.pending[t.scanned:], "\r\n")
		if i < 0 {
			t.scanned = len(t.pending)
			break
		}
		if i += t.scanned; t.pending[i] == '\r' && i+1 == len(t.pending) {
			t.scanned = i // a CR whose LF may follow
			break
		}
		end := i + 1
		if t.pending[i] == '\r' && t.pending[end] == '\n' {
			end++
		}
		t.line(t.pending[:i])
		t.pending, t.scanned = t.pending[end:], 0
	}
	if len(t.pending) > maxBody {
		t.incomplete, t.pending, t.scanned = true, nil, 0
	}
	return len(p), nil
}

func (t *tally) line(l []byte) {
	field, value, _ := bytes.Cut(l, []byte(":"))
	switch {
	case len(l) > maxBody: // as a line overflowing pending is, however the reads split it
		t.incomplete = true
	case len(l) == 0:
		t.dispatch()
	case string(field) == "data" && len(t.data)+len(value) <= maxBody:
		t.data, t.events = append(append(t.data, bytes.TrimPrefix(value, []byte(" "))...), '\n'), true
	case string(field) == "data":
		t.incomplete = true
	}
}

func (t *tally) dispatch() {
	if !t.events {
		return
	}
	data := bytes.TrimSuffix(t.data, []byte("\n"))
	t.data, t.events = t.data[:0], false
	if string(data) == "[DONE]" {
		return
	}
	u, n, ok := parse(data)
	t.incomplete, t.content = t.incomplete || !ok, t.content+n
	if u != nil && ok {
		t.usage = u
	}
}

// result is the usage the response reported, with its content's bytes, when it was forwarded whole and read exactly:
// a JSON body's, or the last a stream's events carried.
func (t *tally) result(whole bool) (usage, int, bool) {
	if !whole {
		return usage{}, 0, false
	}
	if !t.dropped && json.Valid(t.body) {
		u, n, ok := parse(t.body)
		if !ok || u == nil {
			return usage{}, 0, false
		}
		return *u, n, true
	}
	t.line(bytes.TrimSuffix(t.pending, []byte("\r"))) // the last line, and the event it ends
	t.dispatch()
	if t.incomplete || t.usage == nil {
		return usage{}, 0, false
	}
	return *t.usage, t.content, true
}
