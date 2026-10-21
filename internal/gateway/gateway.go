// SPDX-License-Identifier: Apache-2.0

// Package gateway is the controller's model gateway (docs/design/m1/00-slice-plan.md P11; 01 §2.2 "Model
// gateway"): the one way an attempt's model calls leave the controller.
//
// Decisions:
//   - An attempt reaches the gateway with a token bound to its lease and attempt (Bind). The sandbox never holds a
//     credential, and no inbound header is forwarded: only the checked body, under one of the gateway's paths.
//   - Before egress, against the lease, on every request: it is active on the bound attempt, and that attempt is
//     running, its unit's current attempt and the lease's (an active lease alone does not prove it: a stop leaves
//     the lease active on a stopping attempt); its executor is registered; the
//     destination is the registry's for that executor, over an admitted transport (#226 entry 37); and the
//     request's model is exactly the executor's pinned model. The gateway never selects or changes a model (D48): a
//     request naming another, or none, or carrying another model-selecting key, is refused, never rewritten.
//   - Egress dials only the registered origin, ignores proxy settings and refuses every redirect (#226 entries 34
//     and 38). A redirect is recorded as a route refusal; nothing is sent to its target.
//   - The credential is injected per destination (Injector; Bearer by default), the point a governed upstream's
//     trust secret uses later. It is never logged, or put in an error, a refusal or a capture; an upstream response
//     echoing it (raw or JSON-escaped, or cut at EOF) reaches the adapter with the value replaced, recorded as a route
//     refusal. A response header reaches the adapter only as a known media type and charset.
//   - Launch readiness (dispatch.Credentials): Holds reports whether the gateway holds the named credential, read
//     afresh at every check. The registry binds a credential reference to one origin (RuleCredentialRef).
//   - Capture is configured only for exit runs (§8 item 4): the body as sent, before injection, as a restricted
//     blob.
//   - The upstream statuses and route refusals it observed are kept per attempt, for the exit map
//     (exitmap.Exit.Statuses); Unbind hands them over.
//   - Intent before egress (P12a): a binding names its call, and nothing is sent for the call until its
//     `model_call` intent is durable (Intents, internal/modelcall). It is intended once per binding, as the last step
//     before the first send, so a refused request leaves no intent. A gateway without Intents sends nothing.
//   - Counting (P11b, count.go): the gateway counts every request it sends itself (Counts): a floor durable before
//     the send (none, none sent), then the usage the upstream reported, so an adapter's final never settles a call on
//     less than was sent.
//   - Correlation (#226 entry 121): every request it sends carries CallHeader, the binding's call, and nothing
//     else of the gateway's: no counter shared across calls or destinations (Opus #366 L1). It is set last, after
//     injection, every key of its name in any case removed first (L2), so neither the adapter (no inbound header
//     is forwarded) nor an injector sets, overrides or doubles it. A provider with a per-call usage query (the stub
//     provider's, for X3) answers a call's usage by it. Residual: the call id is an opaque random id, no secret; it
//     lets a provider link one call's requests, as the credential largely does already. Real providers ignore the
//     header, so no production route has a usage Query yet, and an unsettled call keeps its liability
//     (modelcall.Reconciler: ObservedNoQuery).
//   - Inert (§1's gating table): nothing in adcd constructs a Gateway; P22a does, when it activates `launch`.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
)

// CallHeader is the correlation header on every request the gateway sends: the binding's call.
const CallHeader = "X-Adc-Call"

// Binding is what a gateway token stands for: one call of one attempt under
// one lease.
type Binding struct {
	Lease   types.LeaseID
	Attempt types.AttemptID
	Call    types.CallID
}

// Intents records a call's model_call intent. Intend returns only once the
// intent is durable; an error means it is not, and nothing is sent.
type Intents interface {
	Intend(ctx context.Context, b Binding, destination types.Slug) error
}

// Leases reads a lease as the store's projection holds it.
type Leases interface {
	Lease(ctx context.Context, id types.LeaseID) (l types.Lease, ok bool, err error)
}

// Attempts reads an attempt as the store's projection holds it, with
// whether it is its unit's current attempt.
type Attempts interface {
	Attempt(ctx context.Context, id types.AttemptID) (a types.Attempt, current, ok bool, err error)
}

// Observed is what the gateway saw of an attempt's upstream requests: the
// HTTP statuses received, and the route refusals (a redirect) it recorded.
type Observed struct {
	Statuses []int
	Refusals []string
}

// Config is what a Gateway checks and injects with.
type Config struct {
	Registry registry.Registry
	Leases   Leases
	Attempts Attempts
	Secrets  Secrets
	Intents  Intents
	Counts   Counts
	Inject   map[types.Slug]Injector // by destination id; Bearer when absent
	Capture  Capture                 // nil except in exit runs
}

// Gateway is the model gateway; serve it on the gateway socket.
type Gateway struct {
	cfg      Config
	client   *http.Client
	mu       sync.Mutex
	bound    map[[32]byte]Binding          // by the token's hash
	inflight map[[32]byte]map[*flight]bool // each token's requests in flight (serve.go)
	ops      map[[32]byte]*sync.WaitGroup  // each token's side effects admitted (admit), which Unbind waits for
	intended map[[32]byte]bool             // tokens whose call's intent is durable, by the token's hash
	observed map[types.AttemptID]Observed
	// seq numbers every request's count, across calls, so a call's are distinct however often it is bound. A
	// restart resets it, but no call sends again after one: its effect is then uncertain, and Intend refuses it.
	seq int64
}

// New is a gateway with no binding. A zero Registry registers nothing, so
// it refuses every call.
func New(cfg Config) (*Gateway, error) {
	if cfg.Leases == nil || cfg.Attempts == nil || cfg.Secrets == nil || cfg.Intents == nil || cfg.Counts == nil {
		return nil, errors.New("gateway: a gateway needs the lease and attempt readers, the secrets, the intents and the counts")
	}
	return &Gateway{cfg: cfg, client: client(), bound: map[[32]byte]Binding{}, inflight: map[[32]byte]map[*flight]bool{}, ops: map[[32]byte]*sync.WaitGroup{}, intended: map[[32]byte]bool{},
		observed: map[types.AttemptID]Observed{}}, nil
}

// The paths an attempt may call, under its destination's endpoint.
var paths = map[string]bool{"/chat/completions": true}

const maxBody = 8 << 20 // bytes of one request body

// The route refusals: a redirect, a response echoing the credential, and
// a request body carrying it (the strongest leak signal the controller sees).
const (
	RouteRedirect = "redirect"
	RouteEchoed   = "credential_echoed"
	RouteInBody   = "credential_in_body"
)

// Bind mints the token an attempt's adapter calls the gateway with.
func (g *Gateway) Bind(b Binding) (string, error) {
	if err := ids.Check(string(b.Call), "cal"); err != nil {
		return "", fmt.Errorf("gateway: a binding names its call: %w", err)
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw[:])
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, other := range g.bound {
		if other.Call == b.Call && other != b {
			return "", errors.New("gateway: the call is bound under another binding")
		}
	}
	g.bound[sha256.Sum256([]byte(token))] = b
	return token, nil
}

// Unbind ends token's binding, cancels its requests in flight, and returns
// what was observed for its attempt. It is idempotent: a token unbound
// already, or never bound, gives Observed{} (serve.go).
//
// It blocks until the side effects its token's requests were admitted to
// before it (a capture, an intent, a floor's commit) have ended; a floor's
// commit is never cancelled. So it is never called from a Capture, Intents
// or Counts implementation, nor while holding a lock one of them takes (a
// store transaction, the attempt end's own mutex): it would deadlock
// (Opus #308 r2 L2).
func (g *Gateway) Unbind(token string) Observed {
	g.mu.Lock()
	key := sha256.Sum256([]byte(token))
	for f := range g.inflight[key] {
		f.cancel()
	}
	delete(g.inflight, key)
	ops := g.ops[key]
	delete(g.ops, key)
	b, ok := g.bound[key]
	var o Observed
	if ok {
		o = g.observed[b.Attempt]
		delete(g.bound, key)
		delete(g.observed, b.Attempt)
		delete(g.intended, key)
	}
	g.mu.Unlock()
	if ops != nil { // the side effects admitted before it end before it returns (Astra #308 2)
		ops.Wait()
	}
	return o
}

// Holds is launch readiness (dispatch.Credentials): whether the gateway
// holds the credential named credential, which the registry gives
// destination's executors.
func (g *Gateway) Holds(_ context.Context, destination, credential types.Slug) (bool, error) {
	if destination == "" || credential == "" || g.cfg.Secrets == nil {
		return false, nil
	}
	c, ok, err := g.cfg.Secrets.Credential(credential)
	if ok && err == nil && !redactable(c.Reveal()) {
		return false, fmt.Errorf("gateway: credential %s holds a backslash or a non-ASCII byte, which the gateway never injects", credential)
	}
	return ok && err == nil, err
}

// redactable: the redaction matches v in every JSON spelling, so the gateway injects it. A credential with a
// backslash (raw or escaped, ambiguously) or a non-ASCII byte (a \u escape spells a rune, not a byte) is refused,
// as not held (Astra on #256, M1).
func redactable(v string) bool {
	return v != "" && !strings.ContainsFunc(v, func(r rune) bool { return r == '\\' || r > 0x7e })
}

// refusal is a request the gateway did not send: its status to the adapter
// and a fixed code, never a value.
type refusal struct {
	status int
	code   string
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The outbound request keeps this deadline, set before its floor is submitted: the upstream exchange ends within
	// RequestDeadline of the floor's commit (modelcall.Clock). The floor's own commit is not bounded by it; a blocked
	// downstream write is, by its own deadline (serve.go, #226 entry 78).
	ctx, cancel := context.WithTimeout(r.Context(), RequestDeadline)
	defer cancel()
	r = r.WithContext(ctx)
	rc := http.NewResponseController(w)
	arm(rc)                              // a writer that cannot take a deadline aborts here, before anything is written (Astra #308 3)
	_ = rc.SetWriteDeadline(time.Time{}) // none armed while the request is read and forwarded: each write arms its own (Opus #308 r2 M1)
	defer arm(rc)                        // the response's end (its last chunk) has a fresh deadline, however long the count took (Astra #308 1)
	f := &flight{cancel}
	b, no := g.enter(r, f)
	if no != nil {
		refuse(w, rc, no)
		return
	}
	release := sync.OnceFunc(func() { g.leave(r, f) })
	defer release()
	resp, s, no := g.forward(r, b, f)
	if no != nil {
		refuse(w, rc, no)
		return
	}
	if ct, ok := contentType(resp.Header.Get("Content-Type")); ok {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	out := &redacting{w: flushing{w, rc}, secret: []byte(s.cred.Reveal())}
	var t tally
	_, err := io.Copy(out, io.TeeReader(resp.Body, &t))
	err = errors.Join(err, out.Close(), resp.Body.Close())
	release() // the upstream exchange is over: the count's commit holds no slot (Opus #308 L4)
	if out.hit {
		g.observe(b.Attempt, 0, RouteEchoed)
	}
	g.count(r, b, s, &t, err == nil)
}

// The response media types and charsets the gateway forwards: a header is
// never a channel past the redaction, so any other value, or any other
// parameter, is dropped rather than scanned (#226 entry 56).
var (
	mediaTypes = map[string]bool{"application/json": true, "text/event-stream": true, "text/plain": true, "text/html": true}
	charsets   = map[string]bool{"utf-8": true, "us-ascii": true, "iso-8859-1": true}
)

// contentType is the Content-Type v forwarded as: a known media type, with
// its charset when that is known too.
func contentType(v string) (string, bool) {
	mt, params, err := mime.ParseMediaType(v)
	if err != nil || !mediaTypes[mt] {
		return "", false
	}
	if cs := strings.ToLower(params["charset"]); charsets[cs] {
		return mime.FormatMediaType(mt, map[string]string{"charset": cs}), true
	}
	return mt, true
}

// redacting writes to w with every spelling of secret replaced by the
// placeholder, so an upstream echoing the credential (in an auth error,
// say) never hands it to the sandbox. It holds back the last bytes that may
// begin a spelling split across writes, until the next write or Close. The
// stream goes on rather than being cut: what arrived stays usable and its
// status reaches the exit map, and the echo is recorded as a route refusal.
//
// A spelling is the key with each byte raw or JSON-escaped, however an
// honest encoder escapes it (#226 entry 56): `\uXXXX` in either case of
// hex, or a short escape (`\/`, `\"`, `\b`, `\f`, `\n`, `\r`, `\t`).
// Base64, URL-encoded, partial and masked echoes are not scanned: only a
// hostile upstream makes them, and it already holds the key (an accepted
// M1 residual).
type redacting struct {
	w      io.Writer
	secret []byte
	held   []byte
	hit    bool
}

// minTail is how many bytes of the key past its public prefix (publicPrefix,
// such as `sk-proj-`) a tail held at EOF must spell for Close to drop it as
// an echo (#226 entry 55): a coincidental run that long of a random key is
// negligible. A key under minTail+1 bytes past its prefix is never detected
// cut at EOF.
const minTail = 8

var publicPrefix = regexp.MustCompile(`^[a-z]+(-[a-z0-9]+)*-`)

// shortEscapes are JSON's two-character escapes, by the byte they stand for; `\\` is left out, since a key
// holding a backslash is never injected (redactable).
var shortEscapes = map[byte]byte{'/': '/', '"': '"', '\b': 'b', '\f': 'f', '\n': 'n', '\r': 'r', '\t': 't'}

// unit is how many bytes of buf's start spell c: 0 when they do not, -1
// when buf ends inside what may still.
func unit(buf []byte, c byte) int {
	switch {
	case len(buf) == 0 || len(buf) == 1 && buf[0] == '\\':
		return -1
	case buf[0] == c:
		return 1
	case buf[0] != '\\':
		return 0
	case buf[1] == 'u':
		if len(buf) < 6 {
			return -1
		}
		if v, err := strconv.ParseUint(string(buf[2:6]), 16, 16); err == nil && v == uint64(c) {
			return 6
		}
	case shortEscapes[c] != 0 && buf[1] == shortEscapes[c]:
		return 2
	}
	return 0
}

// spell reads secret at buf's start: the bytes spanned and the key bytes
// spelled, and whether buf ended first.
func spell(buf, secret []byte) (n, spelled int, cut bool) {
	for _, c := range secret {
		u := unit(buf[n:], c)
		if u <= 0 {
			return n, spelled, u < 0
		}
		n, spelled = n+u, spelled+1
	}
	return n, spelled, false
}

func (r *redacting) Write(p []byte) (int, error) {
	if len(r.secret) == 0 {
		return r.w.Write(p)
	}
	buf, out, from := append(r.held, p...), []byte(nil), 0
	for i := 0; i < len(buf); i++ {
		if n, spelled, _ := spell(buf[i:], r.secret); spelled == len(r.secret) {
			out, r.hit = append(append(out, buf[from:i]...), redacted...), true
			from, i = i+n, i+n-1
		}
	}
	keep := min(6*len(r.secret)-1, len(buf)-from)
	out, r.held = append(out, buf[from:len(buf)-keep]...), append([]byte(nil), buf[len(buf)-keep:]...)
	if len(out) > 0 {
		if _, err := r.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Close writes the held-back tail, which spells no whole key. A tail
// ending in a spelling of minTail key bytes or more past its public prefix
// is an echo cut at EOF: it is replaced, and the echo recorded.
func (r *redacting) Close() error {
	tail, enough := r.held, len(publicPrefix.Find(r.secret))+minTail
	for i := range tail {
		if _, spelled, cut := spell(tail[i:], r.secret); cut && spelled >= enough {
			tail, r.hit = append(tail[:i:i], redacted...), true
			break
		}
	}
	r.held = nil
	if len(tail) == 0 {
		return nil
	}
	_, err := r.w.Write(tail)
	return err
}

// tokenKey is the hash of r's bearer token, the key of its binding.
func tokenKey(r *http.Request) ([32]byte, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return sha256.Sum256([]byte(token)), ok && token != ""
}

// forward checks r against b's lease and sends it, returning the response
// and the credential it injected, or refuses it with nothing sent. The
// lease is checked before the body is read, so only a running attempt can
// make the gateway buffer one.
func (g *Gateway) forward(r *http.Request, b Binding, f *flight) (*http.Response, sending, *refusal) {
	ctx, none := r.Context(), sending{}
	if r.Method != http.MethodPost {
		return nil, none, &refusal{http.StatusMethodNotAllowed, "method"}
	}
	if !paths[r.URL.Path] {
		return nil, none, &refusal{http.StatusNotFound, "path"}
	}
	l, ok, err := g.cfg.Leases.Lease(ctx, b.Lease)
	switch {
	case err != nil:
		return nil, none, &refusal{http.StatusServiceUnavailable, "lease_unreadable"}
	case !ok || l.State != types.LeaseStateActive || l.Attempt == nil || *l.Attempt != b.Attempt:
		return nil, none, &refusal{http.StatusConflict, "lease_not_running_attempt"}
	}
	// An active lease does not prove its attempt runs: a stop leaves the
	// lease active on a stopping attempt. Checked on every request, cached
	// intent or not, when it is admitted. The residual: all requests already
	// admitted before a stop, including those whose body is read after it,
	// may still send (a first one is refused by the Recorder's own check);
	// MaxInFlight bounds them per token, and Unbind cancels them (serve.go,
	// #226 entry 57), and their late usage is accepted
	// (#226 entry 59).
	a, current, ok, err := g.cfg.Attempts.Attempt(ctx, b.Attempt)
	switch {
	case err != nil:
		return nil, none, &refusal{http.StatusServiceUnavailable, "lease_unreadable"}
	case !ok || !current || a.Status != types.AttemptStatusRunning || a.Lease != b.Lease || a.Unit != l.Unit:
		return nil, none, &refusal{http.StatusConflict, "lease_not_running_attempt"}
	}
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		return nil, none, &refusal{http.StatusRequestEntityTooLarge, "body"}
	}
	e, ok := g.cfg.Registry.Executor(l.Executor)
	if !ok {
		return nil, none, &refusal{http.StatusForbidden, "executor_not_registered"}
	}
	target, ok := route(e.Destination, r.URL.Path)
	if !ok {
		return nil, none, &refusal{http.StatusForbidden, "destination_not_admitted"}
	}
	out, ok := pinned(body, e.Model)
	if !ok {
		return nil, none, &refusal{http.StatusForbidden, "model_not_pinned"}
	}
	var cred Credential
	if e.CredentialRef != nil {
		c, held, err := Credential{}, false, errors.New("gateway: no secrets")
		if g.cfg.Secrets != nil {
			c, held, err = g.cfg.Secrets.Credential(*e.CredentialRef)
		}
		if err != nil || !held || !redactable(c.Reveal()) {
			return nil, none, &refusal{http.StatusServiceUnavailable, "credential_not_held"}
		}
		cred = c
	}
	if v := cred.Reveal(); v != "" && bytes.Contains(out, []byte(v)) {
		g.observe(b.Attempt, 0, RouteInBody)
		return nil, none, &refusal{http.StatusForbidden, RouteInBody}
	}
	// Unbound meanwhile: nothing captured (Opus #322 L4); Unbind waits for it (Astra #308 2).
	ok, err = g.effect(r, f, func() error {
		if g.cfg.Capture == nil {
			return nil
		}
		return g.cfg.Capture.Keep(ctx, b, out)
	})
	switch {
	case !ok:
		return nil, none, &refusal{http.StatusUnauthorized, "unbound"}
	case err != nil:
		return nil, none, &refusal{http.StatusServiceUnavailable, "capture_failed"}
	}
	ok, err = g.effect(r, f, func() error { return g.intend(r, b, e.Destination.ID) }) // unbound meanwhile: no intent (Opus #308 M2)
	switch {
	case !ok:
		return nil, none, &refusal{http.StatusUnauthorized, "unbound"}
	case err != nil:
		return nil, none, &refusal{http.StatusServiceUnavailable, "intent_unrecorded"}
	}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, originKey{}, dialAddr(target)), http.MethodPost, target.String(), bytes.NewReader(out))
	if err != nil {
		return nil, none, &refusal{http.StatusForbidden, "destination_not_admitted"}
	}
	var s sending
	ok, err = g.effect(r, f, func() error { // unbound meanwhile: no floor (Opus #308 M2)
		g.mu.Lock()
		s = sending{cred, g.seq, len(out)}
		g.seq++
		g.mu.Unlock()
		return g.cfg.Counts.Count(context.WithoutCancel(ctx), b, s.seq, floor(out, l))
	})
	if !ok {
		return nil, none, &refusal{http.StatusUnauthorized, "unbound"}
	}
	if err != nil {
		g.observe(b.Attempt, 0, RouteUncounted)
		return nil, none, &refusal{http.StatusServiceUnavailable, RouteUncounted}
	}
	req.Header.Set("Content-Type", "application/json")
	if e.CredentialRef != nil {
		inject := g.cfg.Inject[e.Destination.ID]
		if inject == nil {
			inject = Bearer
		}
		inject(req.Header, cred)
	}
	for k := range req.Header { // after injection, in any case: nothing else sets or doubles it (Opus #366 L2)
		if http.CanonicalHeaderKey(k) == CallHeader {
			delete(req.Header, k)
		}
	}
	req.Header.Set(CallHeader, string(b.Call))
	resp, err := g.client.Do(req)
	if resp != nil {
		g.observe(b.Attempt, resp.StatusCode, "")
	}
	if op := (*net.OpError)(nil); errors.As(err, &op) && op.Op == "dial" || errors.Is(err, errOffOrigin) {
		if g.cfg.Counts.Count(context.WithoutCancel(ctx), b, s.seq, Count{Observed: true}) != nil {
			g.observe(b.Attempt, 0, RouteUncounted) // never sent, but its floor stands
		}
	}
	switch {
	case errors.Is(err, errRedirect):
		g.observe(b.Attempt, 0, RouteRedirect)
		return nil, none, &refusal{http.StatusBadGateway, RouteRedirect}
	case err != nil:
		return nil, none, &refusal{http.StatusBadGateway, "upstream_unreachable"}
	case resp.Header.Get("Content-Encoding") != "":
		// Go decoded its own gzip and cleared the header; another hides the credential.
		resp.Body.Close()
		return nil, none, &refusal{http.StatusBadGateway, "encoding"}
	}
	return resp, s, nil
}

// intend makes the call's intent durable before r's token first sends.
// The cache is per token, never per call: another binding of a call always
// reaches Intents, which refuses it. Concurrent first requests may both
// reach Intents, which keeps one intent per call.
func (g *Gateway) intend(r *http.Request, b Binding, destination types.Slug) error {
	key, _ := tokenKey(r)
	g.mu.Lock()
	done := g.intended[key]
	g.mu.Unlock()
	if done {
		return nil
	}
	if err := g.cfg.Intents.Intend(r.Context(), b, destination); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.bound[key] == b {
		g.intended[key] = true // only while bound: Unbind forgets it
	}
	return nil
}

// observe records for a while its attempt is bound.
func (g *Gateway) observe(a types.AttemptID, status int, route string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !slices.ContainsFunc(slices.Collect(maps.Values(g.bound)), func(b Binding) bool { return b.Attempt == a }) {
		return // unbound: Unbind has handed the attempt's observations over
	}
	o := g.observed[a]
	if status != 0 {
		o.Statuses = append(o.Statuses, status)
	}
	if route != "" {
		o.Refusals = append(o.Refusals, route)
	}
	g.observed[a] = o
}

// RefusalHeader marks the gateway's own refusals: an upstream's headers
// never reach the adapter (only its parsed Content-Type), so an upstream
// cannot forge it.
const RefusalHeader = "Adc-Gateway-Refusal"

// refuse writes no, whole and at once, and closes the connection: an unread
// body is never drained first, with or without Expect: 100-continue, and
// never read after (Astra #308 4).
func refuse(w http.ResponseWriter, rc *http.ResponseController, no *refusal) {
	arm(rc) // its own deadline, however long the request took to refuse (Opus #308 r2 M1)
	body := fmt.Sprintf(`{"error":{"type":"gateway_refused","code":%q}}`, no.code)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set(RefusalHeader, no.code)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Connection", "close")
	w.WriteHeader(no.status)
	_, _ = io.WriteString(w, body)
	_ = rc.Flush()
	_ = rc.SetReadDeadline(time.Now()) // not supported off a connection (a recorder): nothing to read there
}
