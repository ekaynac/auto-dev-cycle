// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
)

// The gateway's own count of a call (#226 entry 69). An adapter's usage is its report, and a hostile one can report
// zero deltas and a zero final; so for every request it sends, the gateway records what it saw itself, numbered per
// call (internal/modelcall records it as a usage delta of source gateway_count, or gateway_floor when unobserved):
//   - input: the larger of the upstream's prompt_tokens and a floor from the bytes it sent, one token per
//     bytesPerToken (rounded up), so a request it sent is never counted at zero;
//   - output: the upstream's completion_tokens, read from the response the gateway forwarded (a JSON body, or the
//     last usage a stream's `data:` events carry).
//
// A response whose usage the gateway did not read (none reported, a stream cut short, a request refused after it was
// sent) is unobserved: its count is a floor only, and the liability ledger then settles the call on no adapter final,
// only on a provider reconciliation. Otherwise a final settles the call when it is not below the count
// (ledger.StandingOf). The split mirrors the http-chat adapter's (P13): cached prompt tokens are cache reads, the rest
// input. An upstream under-reporting its own usage is beyond any count: the provider's reconciliation (P20) settles
// that.

// Counts records the gateway's count of one request it sent for a call. Count returns once the report is durable; an
// error means it is not, and the call's count then lacks that request (observed as RouteUncounted).
type Counts interface {
	Count(ctx context.Context, b Binding, sequence int64, n Count) error
}

// Count is what the gateway counted for one request, in tokens. Observed: it read the upstream's usage report.
type Count struct {
	In, Out, CacheRead int64
	Observed           bool
}

// bytesPerToken bounds the request bytes one input token stands for. Text and code tokenize at about 3 to 6 bytes a
// token, JSON framing included, so the floor stays below an honest prompt_tokens; a request holding mostly images
// could exceed it, which only keeps its call uncertain until a reconciliation.
const bytesPerToken = 16

// RouteUncounted: the gateway could not record its count of a request it sent.
const RouteUncounted = "count_unrecorded"

const maxCount = 9007199254740991 // the contract's integers

// count records what the gateway counted for one request it sent for b's call: sent bytes, and the response body it
// forwarded, whole (nil when it forwarded none, or not all of it). It is recorded even if the adapter has hung up.
func (g *Gateway) count(r *http.Request, b Binding, sent int, body []byte) {
	g.mu.Lock()
	seq := g.seq[b.Call]
	g.seq[b.Call]++
	g.mu.Unlock()
	if err := g.cfg.Counts.Count(context.WithoutCancel(r.Context()), b, seq, countOf(sent, body)); err != nil {
		g.observe(b.Attempt, 0, RouteUncounted)
	}
}

// countOf is the count of a request of sent bytes whose response was body.
func countOf(sent int, body []byte) Count {
	u, ok := usageOf(body)
	clamp := func(v int64) int64 { return min(max(v, 0), maxCount) }
	prompt := clamp(u.Prompt)
	cached := min(clamp(u.Details.Cached), prompt)
	in := max(prompt, (int64(sent)+bytesPerToken-1)/bytesPerToken)
	return Count{In: in - cached, Out: clamp(u.Completion), CacheRead: cached, Observed: ok}
}

type usage struct {
	Prompt     int64 `json:"prompt_tokens"`
	Completion int64 `json:"completion_tokens"`
	Details    struct {
		Cached int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// usageOf is the usage an OpenAI-compatible response reports, and whether it reports one: a JSON body's, or the last
// one a stream's `data:` events carry.
func usageOf(body []byte) (u usage, ok bool) {
	var one struct{ Usage *usage }
	if json.Unmarshal(body, &one) == nil && one.Usage != nil {
		return *one.Usage, true
	}
	for line := range bytes.Lines(body) {
		data, event := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		one.Usage = nil
		if event && json.Unmarshal(bytes.TrimSpace(data), &one) == nil && one.Usage != nil {
			u, ok = *one.Usage, true
		}
	}
	return u, ok
}

// kept keeps up to maxBody bytes written to it, the response the gateway reads its count from; whole is false once
// it has dropped some.
type kept struct {
	bytes.Buffer
	dropped bool
}

func (k *kept) Write(p []byte) (int, error) {
	n := min(len(p), max(maxBody-k.Len(), 0))
	k.Buffer.Write(p[:n])
	k.dropped = k.dropped || n < len(p)
	return len(p), nil
}
