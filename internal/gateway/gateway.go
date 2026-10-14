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
//   - Counting (P11b, count.go): the gateway counts every request it sends itself (Counts), from the bytes it sent
//     and the usage the upstream reported, so an adapter's zero final never settles a call that was sent.
//   - Inert (§1's gating table): nothing in adcd constructs a Gateway; P22a does, when it activates `launch`.
package gateway

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
)

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
	bound    map[[32]byte]Binding // by the token's hash
	intended map[[32]byte]bool    // tokens whose call's intent is durable, by the token's hash
	observed map[types.AttemptID]Observed
	seq      map[types.CallID]int64 // each call's next count, never reset, so a rebound call's counts stay distinct
}

// New is a gateway with no binding. A zero Registry registers nothing, so
// it refuses every call.
func New(cfg Config) (*Gateway, error) {
	if cfg.Leases == nil || cfg.Attempts == nil || cfg.Secrets == nil || cfg.Intents == nil || cfg.Counts == nil {
		return nil, errors.New("gateway: a gateway needs the lease and attempt readers, the secrets, the intents and the counts")
	}
	return &Gateway{cfg: cfg, client: client(), bound: map[[32]byte]Binding{}, intended: map[[32]byte]bool{},
		observed: map[types.AttemptID]Observed{}, seq: map[types.CallID]int64{}}, nil
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

// Unbind ends token's binding and returns what was observed for its
// attempt.
func (g *Gateway) Unbind(token string) Observed {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := sha256.Sum256([]byte(token))
	b, ok := g.bound[key]
	if !ok {
		return Observed{}
	}
	o := g.observed[b.Attempt]
	delete(g.bound, key)
	delete(g.observed, b.Attempt)
	delete(g.intended, key)
	return o
}

// Holds is launch readiness (dispatch.Credentials): whether the gateway
// holds the credential named credential, which the registry gives
// destination's executors.
func (g *Gateway) Holds(_ context.Context, destination, credential types.Slug) (bool, error) {
	if destination == "" || credential == "" || g.cfg.Secrets == nil {
		return false, nil
	}
	_, ok, err := g.cfg.Secrets.Credential(credential)
	return ok && err == nil, err
}

// refusal is a request the gateway did not send: its status to the adapter
// and a fixed code, never a value.
type refusal struct {
	status int
	code   string
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, ok := g.binding(r)
	if !ok {
		refuse(w, &refusal{http.StatusUnauthorized, "unbound"})
		return
	}
	resp, cred, no := g.forward(r, b)
	if no != nil {
		refuse(w, no)
		return
	}
	defer resp.Body.Close()
	if ct, ok := contentType(resp.Header.Get("Content-Type")); ok {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	out := &redacting{w: flushing{w}, secret: []byte(cred.Reveal())}
	var body kept
	_, err := io.Copy(out, io.TeeReader(resp.Body, &body))
	if err = errors.Join(err, out.Close()); err != nil || body.dropped {
		body.Reset() // not forwarded whole: its usage is unobserved
	}
	if out.hit {
		g.observe(b.Attempt, 0, RouteEchoed)
	}
	g.count(r, b, int(resp.Request.ContentLength), body.Bytes()) // the request as sent
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

// redacting writes to w with every form of secret it scans for replaced by
// the placeholder, so an upstream echoing the credential (in an auth error,
// say) never hands it to the sandbox. It holds back the last bytes that may
// begin a form split across writes, until the next write or Close. The
// stream goes on rather than being cut: what arrived stays usable and its
// status reaches the exit map, and the echo is recorded as a route refusal.
//
// The forms are the raw value and its JSON-escaped spellings (#226 entry
// 56): an honest upstream quoting the key in a JSON error may escape '/' as
// `\/` or '<', '>' and '&' as `\u003c` and the like. Base64, URL-encoded,
// partial and masked echoes are not scanned: only a hostile upstream makes
// them, and it already holds the key (an accepted M1 residual).
type redacting struct {
	w      io.Writer
	secret []byte
	forms  [][]byte // forms(secret), set at the first write
	held   []byte
	hit    bool
}

// minTail is the shortest prefix of a form, held at EOF, that Close drops
// as an echo (#226 entry 55): a coincidental prefix that long of a random
// key is negligible.
const minTail = 8

// forms are the spellings of secret the redaction scans for, the longest
// first, each once.
func forms(secret []byte) [][]byte {
	out := [][]byte{secret}
	for _, slash := range []string{"/", `\/`} {
		for _, html := range []string{"", `\u%04x`, `\u%04X`} {
			var b strings.Builder
			for _, c := range string(secret) {
				switch {
				case c == '/':
					b.WriteString(slash)
				case html != "" && strings.ContainsRune("<>&", c):
					fmt.Fprintf(&b, html, c)
				case c == '"' || c == '\\':
					b.WriteString(`\` + string(c))
				case c < 0x20:
					fmt.Fprintf(&b, `\u%04x`, c)
				default:
					b.WriteRune(c)
				}
			}
			out = append(out, []byte(b.String()))
		}
	}
	slices.SortFunc(out, func(a, b []byte) int { return cmp.Or(len(b)-len(a), bytes.Compare(a, b)) })
	return slices.CompactFunc(out, bytes.Equal)
}

func (r *redacting) Write(p []byte) (int, error) {
	if len(r.secret) == 0 {
		return r.w.Write(p)
	}
	if r.forms == nil {
		r.forms = forms(r.secret)
	}
	buf, out := append(r.held, p...), []byte(nil)
	for i, n := r.match(buf); i >= 0; i, n = r.match(buf) {
		out = append(append(out, buf[:i]...), redacted...)
		buf, r.hit = buf[i+n:], true
	}
	keep := min(len(r.forms[0])-1, len(buf))
	out, r.held = append(out, buf[:len(buf)-keep]...), append([]byte(nil), buf[len(buf)-keep:]...)
	if len(out) > 0 {
		if _, err := r.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// match is the first form in buf: where it starts and its length, or -1.
func (r *redacting) match(buf []byte) (at, n int) {
	at = -1
	for _, f := range r.forms {
		if i := bytes.Index(buf, f); i >= 0 && (at < 0 || i < at) {
			at, n = i, len(f)
		}
	}
	return at, n
}

// Close writes the held-back tail, which holds no whole form. A tail ending
// in minTail bytes or more of a form's start is an echo cut at EOF: those
// bytes are replaced, and the echo recorded.
func (r *redacting) Close() error {
	tail := r.held
	for i := 0; i+minTail <= len(tail); i++ {
		if slices.ContainsFunc(r.forms, func(f []byte) bool { return bytes.HasPrefix(f, tail[i:]) }) {
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

func (g *Gateway) binding(r *http.Request) (Binding, bool) {
	key, ok := tokenKey(r)
	if !ok {
		return Binding{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	b, ok := g.bound[key]
	return b, ok
}

// forward checks r against b's lease and sends it, returning the response
// and the credential it injected, or refuses it with nothing sent. The
// lease is checked before the body is read, so only a running attempt can
// make the gateway buffer one.
func (g *Gateway) forward(r *http.Request, b Binding) (*http.Response, Credential, *refusal) {
	ctx, none := r.Context(), Credential{}
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
	// P22a bounds them (#226 entry 57), and their late usage is accepted
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
		if err != nil || !held || c.Reveal() == "" {
			return nil, none, &refusal{http.StatusServiceUnavailable, "credential_not_held"}
		}
		cred = c
	}
	if v := cred.Reveal(); v != "" && bytes.Contains(out, []byte(v)) {
		g.observe(b.Attempt, 0, RouteInBody)
		return nil, none, &refusal{http.StatusForbidden, RouteInBody}
	}
	if g.cfg.Capture != nil {
		if g.cfg.Capture.Keep(ctx, b, out) != nil {
			return nil, none, &refusal{http.StatusServiceUnavailable, "capture_failed"}
		}
	}
	if err := g.intend(r, b, e.Destination.ID); err != nil {
		return nil, none, &refusal{http.StatusServiceUnavailable, "intent_unrecorded"}
	}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, originKey{}, dialAddr(target)), http.MethodPost, target.String(), bytes.NewReader(out))
	if err != nil {
		return nil, none, &refusal{http.StatusForbidden, "destination_not_admitted"}
	}
	req.Header.Set("Content-Type", "application/json")
	if e.CredentialRef != nil {
		inject := g.cfg.Inject[e.Destination.ID]
		if inject == nil {
			inject = Bearer
		}
		inject(req.Header, cred)
	}
	resp, err := g.client.Do(req)
	if resp != nil {
		g.observe(b.Attempt, resp.StatusCode, "")
	}
	if err != nil || resp.Header.Get("Content-Encoding") != "" {
		g.count(r, b, len(out), nil) // possibly sent, and nothing forwarded
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
	return resp, cred, nil
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

func refuse(w http.ResponseWriter, no *refusal) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(RefusalHeader, no.code)
	w.WriteHeader(no.status)
	_, _ = fmt.Fprintf(w, `{"error":{"type":"gateway_refused","code":%q}}`, no.code)
}

// flushing flushes after every write, so a streamed response reaches the
// adapter as it arrives.
type flushing struct{ w http.ResponseWriter }

func (f flushing) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
