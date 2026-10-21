// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// Serving the gateway on an attempt's socket (P22a-5g; #226 entries 57, 58
// and 78). 22a-5 serves Server() on the attempt's Unix socket, hands the
// adapter Mint's value, and calls Unbind at every attempt end.
//
// Decisions:
//   - The minted base_url has no path (entry 58): the adapter appends
//     /chat/completions, the one path the gateway serves; its host is a
//     label, the socket being the way in (ADC_GATEWAY_SOCKET).
//   - At most MaxInFlight requests per token at once (entry 57): one past
//     it is refused before its body is read, nothing sent, no intent
//     recorded (429 in_flight_cap, by the refusal header: the adapter never
//     reads it as an upstream's 429).
//   - Every write to the adapter has its own deadline, writeIdle away
//     (entry 78): an adapter that stops reading ends the handler, the
//     upstream response is closed, and the call keeps its floor (a
//     response not forwarded whole). The server's WriteTimeout is the
//     backstop past RequestDeadline.
//   - A slot is held for the upstream exchange only: it is released once
//     the response is forwarded (or cut) and closed, before its count
//     commits (Opus #308 L4).
//   - Unbind is idempotent, and cancels the token's requests in flight:
//     their upstream exchanges end and keep their floors, and nothing they
//     do afterwards binds or observes again. A request's capture, intent
//     and floor are each admitted under the gateway's lock, only while the
//     request is in flight, and Unbind waits for those already admitted:
//     none is recorded after it returns (Opus #308 M2, #322 L4; Astra #308
//     2). What a request already sent counts afterwards (its usage) is
//     still recorded: never an undercount.
//   - Served unwrapped: every write, a refusal's and a header included,
//     has its deadline (each write's own, cleared after it, and a fresh
//     one for the response's end once the count has committed: Astra #308
//     1, 3), so a ResponseWriter that cannot set one aborts the request
//     rather than write without it (Opus #308 L5).
//   - A refusal is written whole at once and closes the connection: an
//     unread body is never drained first, with or without Expect (Astra
//     #308 4).

// BaseURL is the base_url Mint hands an adapter: no path.
const BaseURL = "http://adc-gateway"

// MaxInFlight is how many requests one token may have in flight at once.
const MaxInFlight = 2

// The socket server's bounds; writeIdle is a var only for the tests.
var (
	writeIdle         = 30 * time.Second
	readHeaderTimeout = 30 * time.Second
)

// Mint binds b and is the gateway value of the adapter's prepare params.
func (g *Gateway) Mint(b Binding) (types.AdapterPrepareParamsGatewayValue, error) {
	token, err := g.Bind(b)
	if err != nil {
		return types.AdapterPrepareParamsGatewayValue{}, err
	}
	return types.AdapterPrepareParamsGatewayValue{BaseUrl: BaseURL, Token: token}, nil
}

// Server is the gateway's socket server: serve it on a listener with
// Serve, and Close it at shutdown. Serve g itself: a middleware whose
// ResponseWriter cannot set a write deadline makes every response fail.
func (g *Gateway) Server() *http.Server {
	return &http.Server{Handler: g, ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: time.Minute,
		WriteTimeout: RequestDeadline + time.Minute, IdleTimeout: time.Minute}
}

// flight is one request in flight on a token.
type flight struct{ cancel context.CancelFunc }

// enter admits r on its token's binding, within the in-flight cap, as f;
// leave releases it.
func (g *Gateway) enter(r *http.Request, f *flight) (Binding, *refusal) {
	key, ok := tokenKey(r)
	if !ok {
		return Binding{}, &refusal{http.StatusUnauthorized, "unbound"}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	b, ok := g.bound[key]
	switch {
	case !ok:
		return Binding{}, &refusal{http.StatusUnauthorized, "unbound"}
	case len(g.inflight[key]) >= MaxInFlight:
		return Binding{}, &refusal{http.StatusTooManyRequests, "in_flight_cap"}
	}
	if g.inflight[key] == nil {
		g.inflight[key] = map[*flight]bool{}
	}
	g.inflight[key][f] = true
	return b, nil
}

// admit admits one side effect of r (its capture, its intent or its
// floor) while f is in flight on r's token and r is not cancelled; done
// ends it. Unbind waits for every side effect admitted before it, so none
// is recorded after it returns (Astra #308 2).
func (g *Gateway) admit(r *http.Request, f *flight) (done func(), ok bool) {
	key, _ := tokenKey(r)
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.inflight[key][f] || r.Context().Err() != nil {
		return nil, false
	}
	ops := g.ops[key]
	if ops == nil {
		ops = new(sync.WaitGroup)
		g.ops[key] = ops
	}
	ops.Add(1)
	return ops.Done, true
}

// effect runs fn, one side effect of r, only when admit admits it, and
// ends it however fn ends, a panic included: Unbind never waits for a
// side effect that cannot end (Opus #308 r2 L1).
func (g *Gateway) effect(r *http.Request, f *flight, fn func() error) (ok bool, err error) {
	done, ok := g.admit(r, f)
	if !ok {
		return false, nil
	}
	defer done()
	return true, fn()
}

// arm gives the response's next writes their deadline, writeIdle away;
// past a writer that cannot take one, the handler aborts, writing nothing
// more (Opus #308 L5; Astra #308 3).
func arm(rc *http.ResponseController) {
	if rc.SetWriteDeadline(time.Now().Add(writeIdle)) != nil {
		panic(http.ErrAbortHandler)
	}
}

func (g *Gateway) leave(r *http.Request, f *flight) {
	key, _ := tokenKey(r)
	g.mu.Lock()
	defer g.mu.Unlock()
	if delete(g.inflight[key], f); len(g.inflight[key]) == 0 {
		delete(g.inflight, key)
		delete(g.ops, key) // none pending: each flight's ended before it left
	}
}

// flushing flushes after every write, so a streamed response reaches the
// adapter as it arrives, and gives each write its deadline (entry 78).
type flushing struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (f flushing) Write(p []byte) (int, error) {
	arm(f.rc) // each write's own deadline: never a write without one (Opus #308 L5)
	n, err := f.w.Write(p)
	if err == nil {
		err = f.rc.Flush()
		if err == http.ErrNotSupported {
			err = nil
		}
	}
	return n, err
}
