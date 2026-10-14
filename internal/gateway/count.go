// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// The gateway's own count of a call (#226 entry 69). An adapter's usage is its report, and a hostile one can report
// zero deltas and a zero final; so the gateway records what it saw itself, per request it sends, under one sequence
// (internal/modelcall records each as a usage delta of source gateway_floor or gateway_count):
//   - before the send, durably, a floor: input from the bytes it sends, one token per bytesPerToken (rounded up), and
//     output bounded by the request's max_tokens (or, with none, the lease's token reservation). A floor it cannot
//     record refuses the send, as an unrecorded intent does;
//   - after forwarding the whole response, the count: input the larger of the upstream's prompt_tokens and that
//     floor; output the upstream's completion_tokens, at least the forwarded content's bytes / bytesPerToken. It
//     supersedes the floor at its sequence. A count whose output the content outweighs stays a floor.
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

// floor is the count of body before it is sent, under l.
func floor(body []byte, l types.Lease) Count {
	var req struct {
		MaxTokens           int64 `json:"max_tokens"`
		MaxCompletionTokens int64 `json:"max_completion_tokens"`
	}
	out := int64(0)
	if json.Unmarshal(body, &req) == nil {
		out = max(req.MaxTokens, req.MaxCompletionTokens)
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
	cached := min(clamp(u.Details.Cached), prompt)
	n := Count{In: max(prompt, tokens(s.sent)) - cached, Out: max(out, tokens(content)), CacheRead: cached, Observed: out >= tokens(content)}
	if err := g.cfg.Counts.Count(context.WithoutCancel(r.Context()), b, s.seq, n); err != nil {
		g.observe(b.Attempt, 0, RouteUncounted)
	}
}

type usage struct {
	Prompt     int64 `json:"prompt_tokens"`
	Completion int64 `json:"completion_tokens"`
	Details    struct {
		Cached int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// reply is an OpenAI-compatible response, or one event of a stream.
type reply struct {
	Usage   *usage
	Choices []struct {
		Message, Delta struct {
			Content   string
			ToolCalls []struct{ Function struct{ Arguments string } } `json:"tool_calls"`
		}
	}
}

// content is the bytes of reply's generated text and tool arguments.
func (p reply) content() (n int) {
	for _, c := range p.Choices {
		for _, m := range []string{c.Message.Content, c.Delta.Content} {
			n += len(m)
		}
		for _, t := range append(c.Message.ToolCalls, c.Delta.ToolCalls...) {
			n += len(t.Function.Arguments)
		}
	}
	return n
}

// tally reads a response as it is forwarded: its first maxBody bytes, for a JSON body, and each `data:` event of a
// stream as it ends, keeping only the last usage, so a stream of any length is read whole.
type tally struct {
	body, line        []byte
	dropped, overlong bool // dropped: body is not all of it; overlong: a line past maxBody was dropped
	usage             *usage
	content           int
}

func (t *tally) Write(p []byte) (int, error) {
	n := min(len(p), max(maxBody-len(t.body), 0))
	t.body, t.dropped = append(t.body, p[:n]...), t.dropped || n < len(p)
	t.line = append(t.line, p...)
	for i := bytes.IndexByte(t.line, '\n'); i >= 0; i = bytes.IndexByte(t.line, '\n') {
		t.event(t.line[:i])
		t.line = t.line[i+1:]
	}
	if len(t.line) > maxBody {
		t.overlong, t.line = true, nil // an event no stream sends: its usage is unread
	}
	return len(p), nil
}

func (t *tally) event(line []byte) {
	var e reply
	if data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok && json.Unmarshal(bytes.TrimSpace(data), &e) == nil {
		t.content += e.content()
		if e.Usage != nil {
			t.usage = e.Usage
		}
	}
}

// result is the usage the response reported, with its content's bytes, if it was forwarded whole and reported one:
// a JSON body's, or the last a stream's events carried.
func (t *tally) result(whole bool) (usage, int, bool) {
	var one reply
	switch {
	case !whole || t.overlong:
		return usage{}, 0, false
	case !t.dropped && json.Unmarshal(t.body, &one) == nil:
		t.usage, t.content = one.Usage, one.content()
	default:
		t.event(t.line)
	}
	if t.usage == nil {
		return usage{}, 0, false
	}
	return *t.usage, t.content, true
}
