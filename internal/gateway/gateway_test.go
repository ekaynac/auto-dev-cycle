// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/dispatch"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
)

var _ dispatch.Credentials = (*Gateway)(nil) // P8b's launch-readiness seam

const (
	config   = "../../fixtures/m1/config/"
	builder  = types.ExecutorID("exe_01m4tm99haemqt8es2qfbwj8pa") // registry-stub.json: destination stub, credential stub
	reviewer = types.ExecutorID("exe_01m4tm99haemr8ajyf09v4ytzb")
	local    = types.ExecutorID("exe_01m4tm99haemrs8zjs56wjm467") // registry-live.example.json: a local endpoint
	leaseID  = types.LeaseID("lea_01m4tm99haemss9tvzd438vq8j")
	attempt  = types.AttemptID("att_01m4tm99haemss9tvzd438vq8j")
	callID   = types.CallID("cal_01m4tm99haemss9tvzd438vq8j")
	fakeKey  = "fake-credential-0123456789" // a test value, never a real credential
)

// upstream is a loopback provider recording what reached it.
type upstream struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []seen
}

type seen struct {
	path   string
	header http.Header
	body   []byte
}

func newUpstream(t *testing.T, h http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, seen{r.URL.Path, r.Header.Clone(), body})
		u.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "upstream=1")
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) got() []seen {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]seen(nil), u.reqs...)
}

func groups(t *testing.T) []types.ResourceGroup {
	t.Helper()
	var gs []types.ResourceGroup
	raw, err := os.ReadFile(config + "groups.json")
	if err == nil {
		err = json.Unmarshal(raw, &gs)
	}
	if err != nil {
		t.Fatal(err)
	}
	return gs
}

// reg is file's registry with every loopback endpoint pointed at endpoint.
func reg(t *testing.T, file, endpoint string) registry.Registry {
	t.Helper()
	doc, err := registry.Load(config + file)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range doc.Executors {
		if strings.HasPrefix(e.Destination.Endpoint, "http://127.0.0.1:") {
			doc.Executors[i].Destination.Endpoint = endpoint
		}
	}
	r, err := registry.New(doc, groups(t))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type leases map[types.LeaseID]types.Lease

func (l leases) Lease(_ context.Context, id types.LeaseID) (types.Lease, bool, error) {
	v, ok := l[id]
	return v, ok, nil
}

type secrets map[types.Slug]string

func (s secrets) Credential(ref types.Slug) (Credential, bool, error) {
	v, ok := s[ref]
	return NewCredential(v), ok && v != "", nil
}

// attempts are the current attempts, by id.
type attempts map[types.AttemptID]types.Attempt

func (a attempts) Attempt(_ context.Context, id types.AttemptID) (types.Attempt, bool, bool, error) {
	v, ok := a[id]
	return v, ok, ok, nil
}

func running(executor types.ExecutorID) leases {
	a := attempt
	return leases{leaseID: {ID: leaseID, Executor: executor, State: types.LeaseStateActive, Attempt: &a}}
}

// intents records the intents asked for (internal/modelcall's Recorder in
// adcd), failing with err when set.
type intents struct {
	mu   sync.Mutex
	got  []Binding
	dest []types.Slug
	err  error
}

func (i *intents) Intend(_ context.Context, b Binding, destination types.Slug) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.err != nil {
		return i.err
	}
	i.got, i.dest = append(i.got, b), append(i.dest, destination)
	return nil
}

func (i *intents) n() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.got)
}

// counts records the gateway's counts (internal/modelcall's Recorder in
// adcd), failing with err when set.
type counts struct {
	mu       sync.Mutex
	got      []counted
	err      error // for every count
	observed error // for counts after a response, not floors
}

type counted struct {
	b   Binding
	seq int64
	n   Count
}

func (c *counts) Count(_ context.Context, b Binding, seq int64, n Count) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil || n.Observed && c.observed != nil {
		return cmp.Or(c.err, c.observed)
	}
	c.got = append(c.got, counted{b, seq, n})
	return nil
}

func (c *counts) all() []counted {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]counted(nil), c.got...)
}

// rig is a gateway over the stub registry pointed at up, with the builder's
// lease running and its credential held, and a bound token.
func rig(t *testing.T, up *upstream, edit func(*Config)) (*Gateway, string) {
	t.Helper()
	cfg := Config{Registry: reg(t, "registry-stub.json", up.URL+"/v1"), Leases: running(builder), Secrets: secrets{"stub": fakeKey}, Intents: &intents{},
		Counts: &counts{}, Attempts: attempts{attempt: {ID: attempt, Lease: leaseID, Status: types.AttemptStatusRunning}}}
	if edit != nil {
		edit(&cfg)
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	token, err := g.Bind(Binding{Lease: leaseID, Attempt: attempt, Call: callID})
	if err != nil {
		t.Fatal(err)
	}
	return g, token
}

func call(g *Gateway, token, path, body string, header map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(deadlined{w}, r)
	return w
}

// deadlined is a recorder that takes write deadlines, as a connection does:
// the gateway never writes without one (Opus #308 L5).
type deadlined struct{ *httptest.ResponseRecorder }

func (deadlined) SetWriteDeadline(time.Time) error { return nil }

const good = `{"model":"stub-builder-1","messages":[{"role":"user","content":"hi"}]}`

func TestForwardsThePinnedModelWithTheCredentialInjected(t *testing.T) {
	up := newUpstream(t, nil)
	g, token := rig(t, up, nil)
	w := call(g, token, "/chat/completions", good, map[string]string{"X-Leak": "1", "Cookie": "c=1"})
	if w.Code != http.StatusOK || w.Body.String() != `{"choices":[]}` || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("response %d %q %v", w.Code, w.Body, w.Header())
	}
	got := up.got()
	if len(got) != 1 {
		t.Fatalf("upstream saw %d requests", len(got))
	}
	s := got[0]
	if s.path != "/v1/chat/completions" || s.header.Get("Authorization") != "Bearer "+fakeKey {
		t.Fatalf("upstream saw %s with %v", s.path, s.header)
	}
	if s.header.Get("X-Leak") != "" || s.header.Get("Cookie") != "" || strings.Contains(fmt.Sprint(s.header), token) {
		t.Fatalf("an inbound header was forwarded: %v", s.header)
	}
	var sent map[string]any
	if json.Unmarshal(s.body, &sent) != nil || sent["model"] != "stub-builder-1" {
		t.Fatalf("the model was changed: %s", s.body)
	}
	if o := g.Unbind(token); len(o.Statuses) != 1 || o.Statuses[0] != 200 || len(o.Refusals) != 0 {
		t.Fatalf("observed %+v", o)
	}
	if w := call(g, token, "/chat/completions", good, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("an unbound token got %d", w.Code)
	}
}

// F: a model switch. The gateway never selects or changes a model: any
// request naming another than the lease's pinned model, or none, is refused
// with nothing sent.
func TestAModelSwitchIsRefusedBeforeEgress(t *testing.T) {
	up := newUpstream(t, nil)
	g, token := rig(t, up, nil)
	for name, body := range map[string]string{
		"another registered model":      `{"model":"stub-reviewer-1","messages":[]}`,
		"no model":                      `{"messages":[]}`,
		"a model that is not a string":  `{"model":["stub-builder-1"]}`,
		"case variant":                  `{"model":"Stub-Builder-1"}`,
		"the pinned model, then others": `{"model":"stub-builder-1","model":"stub-reviewer-1"}`,
		"not an object":                 `["stub-builder-1"]`,
		"a case variant of the key":     `{"model":"stub-builder-1","Model":"stub-reviewer-1"}`,
		"an upper-case key":             `{"model":"stub-builder-1","MODEL":"stub-reviewer-1"}`,
		"a fallback list":               `{"model":"stub-builder-1","models":["stub-reviewer-1"]}`,
		"fallbacks":                     `{"model":"stub-builder-1","fallbacks":["stub-reviewer-1"]}`,
		"a route":                       `{"model":"stub-builder-1","Route":"fallback"}`,
		"a router's provider":           `{"model":"stub-builder-1","provider":{"order":["other"]}}`,
		"a vendor passthrough":          `{"model":"stub-builder-1","extra_body":{"model":"stub-reviewer-1"}}`,
		"an unknown key":                `{"model":"stub-builder-1","zz_unlisted":1}`,
		"an upper-case allowed key":     `{"model":"stub-builder-1","MESSAGES":[]}`,
		"logit_bias, not needed":        `{"model":"stub-builder-1","logit_bias":{}}`,
		"null":                          `null`,
	} {
		if w := call(g, token, "/chat/completions", body, nil); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "model_not_pinned") {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if n := len(up.got()); n != 0 {
		t.Fatalf("upstream saw %d refused requests", n)
	}
	// Another model first, the pinned one last: the upstream gets one key,
	// the pinned model, whatever its parser keeps.
	if w := call(g, token, "/chat/completions", `{"model":"stub-reviewer-1","model":"stub-builder-1"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if body := string(up.got()[0].body); strings.Count(body, `"model"`) != 1 || strings.Contains(body, "stub-reviewer-1") {
		t.Fatalf("the upstream got %s", body)
	}
	// A full legitimate request, every allowed key, passes unchanged.
	full := `{"model":"stub-builder-1","messages":[{"role":"user","content":"hi"}],"temperature":0.2,"top_p":1,` +
		`"max_tokens":100,"max_completion_tokens":100,"stream":true,"stream_options":{"include_usage":true},` +
		`"stop":["END"],"n":1,"seed":7,"presence_penalty":0,"frequency_penalty":0,"tools":[],"tool_choice":"auto",` +
		`"parallel_tool_calls":false,"response_format":{"type":"text"},"user":"att","logprobs":false,"top_logprobs":0}`
	if w := call(g, token, "/chat/completions", full, nil); w.Code != http.StatusOK {
		t.Fatalf("a full request: %d %s", w.Code, w.Body)
	}
	var sent, asked map[string]any
	if json.Unmarshal(up.got()[1].body, &sent) != nil || json.Unmarshal([]byte(full), &asked) != nil || fmt.Sprint(sent) != fmt.Sprint(asked) {
		t.Fatalf("the full request changed: %s", up.got()[1].body)
	}
}

// The lease, not the request, names the executor and destination: a
// request outside a running attempt is refused with nothing sent.
func TestTheLeaseIsCheckedBeforeEgress(t *testing.T) {
	up := newUpstream(t, nil)
	other := types.AttemptID("att_01m4tm99haemr8ajyf09v4ytzb")
	cases := map[string]struct {
		edit func(*Config)
		path string
		code int
	}{
		"a lease not active": {func(c *Config) {
			l := c.Leases.(leases)[leaseID]
			l.State = types.LeaseStateSettling
			c.Leases = leases{leaseID: l}
		}, "", 409},
		"another attempt running":                   {func(c *Config) { l := c.Leases.(leases)[leaseID]; l.Attempt = &other; c.Leases = leases{leaseID: l} }, "", 409},
		"no such lease":                             {func(c *Config) { c.Leases = leases{} }, "", 409},
		"an unregistered executor":                  {func(c *Config) { c.Leases = running(local) }, "", 403},
		"a path off the list":                       {nil, "/models", 404},
		"an absolute-form target off the path list": {nil, "http://127.0.0.2:1/elsewhere", 404},
	}
	for name, c := range cases {
		g, token := rig(t, up, c.edit)
		path := "/chat/completions"
		if c.path != "" {
			path = c.path
		}
		if w := call(g, token, path, good, nil); w.Code != c.code {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	g, _ := rig(t, up, nil)
	if w := call(g, "not-a-token", "/chat/completions", good, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("a forged token: %d", w.Code)
	}
	// L6: the lease is checked before the body is read; an oversized body
	// to a lease not running is a lease refusal, not a read.
	g, token := rig(t, up, cases["no such lease"].edit)
	if w := call(g, token, "/chat/completions", strings.Repeat("x", maxBody+1), nil); w.Code != http.StatusConflict {
		t.Errorf("an oversized body to no lease: %d", w.Code)
	}
	if n := len(up.got()); n != 0 {
		t.Fatalf("upstream saw %d refused requests", n)
	}
	// An absolute-form request target: its host is never used; the call
	// goes to the registered origin only.
	g, token = rig(t, up, nil)
	if w := call(g, token, "http://evil.example/chat/completions", good, nil); w.Code != http.StatusOK {
		t.Fatalf("absolute form: %d %s", w.Code, w.Body)
	}
	if got := up.got(); len(got) != 1 || got[0].path != "/v1/chat/completions" {
		t.Fatalf("absolute form reached %+v", got)
	}
}

// L1: a gateway without its lease reader or secrets is never built.
func TestNewNeedsLeasesAndSecrets(t *testing.T) {
	if _, err := New(Config{Secrets: secrets{}}); err == nil {
		t.Error("no lease reader")
	}
	if _, err := New(Config{Leases: leases{}}); err == nil {
		t.Error("no secrets")
	}
	if _, err := New(Config{Leases: leases{}, Attempts: attempts{}, Secrets: secrets{}}); err == nil {
		t.Error("no intents")
	}
	if _, err := New(Config{Leases: leases{}, Secrets: secrets{}, Intents: &intents{}, Counts: &counts{}}); err == nil {
		t.Error("no attempt reader")
	}
	if _, err := New(Config{Leases: leases{}, Attempts: attempts{}, Secrets: secrets{}, Intents: &intents{}}); err == nil {
		t.Error("no counts")
	}
	up := newUpstream(t, nil)
	g, token := rig(t, up, nil)
	g.cfg.Secrets = nil // forward refuses on its own too
	if w := call(g, token, "/chat/completions", good, nil); w.Code != http.StatusServiceUnavailable || len(up.got()) != 0 {
		t.Fatalf("no secrets: %d", w.Code)
	}
}

// H1: an upstream echoing the credential, in an error body or in a stream
// chunk split across writes, never hands it to the adapter.
func TestAnEchoedCredentialNeverReachesTheAdapter(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"a 401 body": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"bad key `+r.Header.Get("Authorization")+`"}`)
		},
		"an SSE chunk split across two writes": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"x\":\"fake-credential-0123")
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, "456789 tail\"}\n\ndata: [DONE]\n\n")
		},
	} {
		g, token := rig(t, newUpstream(t, h), nil)
		w := call(g, token, "/chat/completions", good, nil)
		body := w.Body.String()
		if strings.Contains(body, fakeKey) || strings.Contains(body, "0123456789") || !strings.Contains(body, redacted) {
			t.Errorf("%s: the adapter got %q", name, body)
		}
		if o := g.Unbind(token); len(o.Refusals) != 1 || o.Refusals[0] != RouteEchoed {
			t.Errorf("%s: observed %+v", name, o)
		}
	}
	var out bytes.Buffer // a held-back tail shorter than minTail reaches the adapter at EOF
	r := &redacting{w: &out, secret: []byte(fakeKey)}
	_, _ = r.Write([]byte("abc fake-cr"))
	if _ = r.Close(); out.String() != "abc fake-cr" || r.hit {
		t.Fatalf("tail %q", out.String())
	}
	// Mythos M-2: a key split across two writes, deterministically. After
	// the first write exactly 6*len(key)-1 bytes are held back, the longest
	// spelling but one byte (every byte escaped as \uXXXX).
	out.Reset()
	r = &redacting{w: &out, secret: []byte(fakeKey)}
	_, _ = r.Write([]byte(strings.Repeat("a", 200) + "data: fake-credential-0123"))
	if out.String() != strings.Repeat("a", 226-6*len(fakeKey)+1) || len(r.held) != 6*len(fakeKey)-1 {
		t.Fatalf("after one write: %q, held %d", out.String(), len(r.held))
	}
	_, _ = r.Write([]byte("456789 tail"))
	if _ = r.Close(); strings.Contains(out.String(), "0123456789") || out.String() != strings.Repeat("a", 200)+"data: "+redacted+" tail" || !r.hit {
		t.Fatalf("split: %q hit %v", out.String(), r.hit)
	}
}

// Mythos M-1 and L-1: a response with an encoding the gateway did not ask
// for is refused, its status recorded; a Content-Type reaches the adapter
// as its media type and charset only.
func TestResponseHeadersCarryNoCredential(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		_, _ = io.WriteString(w, r.Header.Get("Authorization"))
	})
	g, token := rig(t, up, nil)
	if w := call(g, token, "/chat/completions", good, nil); w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), fakeKey) || !strings.Contains(w.Body.String(), `"encoding"`) {
		t.Fatalf("br: %d %q", w.Code, w.Body)
	}
	if o := g.Unbind(token); len(o.Statuses) != 1 || o.Statuses[0] != 200 {
		t.Fatalf("observed %+v", o)
	}
	up = newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", `text/event-stream; charset=utf-8; leak="`+r.Header.Get("Authorization")+`"`)
	})
	g, token = rig(t, up, nil)
	if w := call(g, token, "/chat/completions", good, nil); w.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Fatalf("content type %q", w.Header().Get("Content-Type"))
	}
}

// The gateway's own refusals carry RefusalHeader with their code; an
// upstream setting it, or sending a refusal-shaped body, never does.
func TestOnlyTheGatewayMarksARefusal(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(RefusalHeader, "lease_not_running_attempt")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"type":"gateway_refused","code":"lease_not_running_attempt"}}`)
	})
	g, token := rig(t, up, nil)
	if w := call(g, token, "/chat/completions", good, nil); w.Code != http.StatusConflict || w.Header().Get(RefusalHeader) != "" {
		t.Fatalf("an upstream's forged refusal: %d %v", w.Code, w.Header())
	}
	g, token = rig(t, up, func(c *Config) { c.Leases = leases{} })
	if w := call(g, token, "/chat/completions", good, nil); w.Header().Get(RefusalHeader) != "lease_not_running_attempt" {
		t.Fatalf("the gateway's refusal: %d %v", w.Code, w.Header())
	}
}

// Mythos L-3 and L-4: a request body carrying the credential is refused and
// recorded without a capture too; an empty credential value is not held.
func TestACredentialInTheBodyOrEmptyIsRefused(t *testing.T) {
	up := newUpstream(t, nil)
	g, token := rig(t, up, nil)
	if w := call(g, token, "/chat/completions", `{"model":"stub-builder-1","user":"`+fakeKey+`"}`, nil); w.Code != http.StatusForbidden {
		t.Fatalf("a body with the credential: %d", w.Code)
	}
	if o := g.Unbind(token); len(o.Refusals) != 1 || o.Refusals[0] != RouteInBody {
		t.Fatalf("observed %+v", o)
	}
	g, token = rig(t, up, func(c *Config) { c.Secrets = emptyHeld{} })
	if w := call(g, token, "/chat/completions", good, nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("an empty credential: %d", w.Code)
	}
	if n := len(up.got()); n != 0 {
		t.Fatalf("upstream saw %d refused requests", n)
	}
}

// emptyHeld is a store that reports an empty value as held.
type emptyHeld struct{}

func (emptyHeld) Credential(types.Slug) (Credential, bool, error) {
	return NewCredential(""), true, nil
}

// L4: a status landing after Unbind is dropped, never kept for a
// finished attempt.
func TestAStatusAfterUnbindIsDropped(t *testing.T) {
	g, token := rig(t, newUpstream(t, nil), nil)
	g.Unbind(token)
	g.observe(attempt, 500, "")
	if len(g.observed) != 0 {
		t.Fatalf("observed %+v", g.observed)
	}
}

// F: an off-list destination. #226 entries 34 and 38: every redirect is
// refused and recorded as a route refusal; nothing reaches its target, on
// the same origin or another.
func TestRedirectsAreRefusedAndRecorded(t *testing.T) {
	elsewhere := newUpstream(t, nil)
	for name, location := range map[string]string{"another origin": elsewhere.URL + "/v1/chat/completions", "the same origin": "/v1/moved"} {
		up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/moved" {
				return
			}
			http.Redirect(w, r, location, http.StatusTemporaryRedirect)
		})
		g, token := rig(t, up, nil)
		w := call(g, token, "/chat/completions", good, nil)
		if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), RouteRedirect) {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
		if got := up.got(); len(got) != 1 {
			t.Fatalf("%s: the redirect was followed: %d requests", name, len(got))
		}
		if o := g.Unbind(token); len(o.Refusals) != 1 || o.Refusals[0] != RouteRedirect || len(o.Statuses) != 1 || o.Statuses[0] != 307 {
			t.Fatalf("%s: observed %+v", name, o)
		}
	}
	if n := len(elsewhere.got()); n != 0 {
		t.Fatalf("the redirect target saw %d requests", n)
	}
}

// F: an off-list destination. The dial is the second layer: even a client
// that followed a redirect dials only the registered origin.
func TestEgressDialsOnlyTheRegisteredOrigin(t *testing.T) {
	elsewhere := newUpstream(t, nil)
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/v1/chat/completions", http.StatusTemporaryRedirect)
	})
	g, token := rig(t, up, nil)
	g.client.CheckRedirect = nil // as if the redirect refusal were lost
	if w := call(g, token, "/chat/completions", good, nil); w.Code != http.StatusBadGateway {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if n := len(elsewhere.got()); n != 0 {
		t.Fatalf("another origin was dialled: %d requests", n)
	}
	req, _ := http.NewRequest(http.MethodGet, elsewhere.URL, nil)
	if _, err := client().Do(req); !errors.Is(err, errOffOrigin) {
		t.Fatalf("a request with no origin bound dialled: %v", err)
	}
}

// #226 entry 37, re-checked at egress.
func TestTransportDecision(t *testing.T) {
	direct, localEP := types.RegistryDestinationKindDirectApi, types.RegistryDestinationKindLocalEndpoint
	for _, c := range []struct {
		kind     types.RegistryDestinationKind
		endpoint string
		ok       bool
	}{
		{direct, "https://api.example.com/v1", true},
		{direct, "http://127.0.0.1:8080/v1", true},
		{direct, "http://[::1]:8080/v1", true},
		{direct, "http://api.example.com/v1", false},
		{direct, "http://localhost:8080/v1", false},
		{direct, "http://10.0.0.5/v1", false},
		{direct, "https://user:pw@api.example.com/v1", false},
		{direct, "https://api.example.com/v1?key=x", false},
		{direct, "ftp://api.example.com/v1", false},
		{localEP, "http://127.0.0.1:11434/v1", true},
		{localEP, "http://192.168.1.20:11434/v1", true},
		{localEP, "http://172.16.0.9/v1", true},
		{localEP, "http://[fd00::1]:8080/v1", true},
		{localEP, "https://10.1.2.3/v1", true},
		{localEP, "http://8.8.8.8/v1", false},
		{localEP, "http://[2001:db8::1]/v1", false},
		{localEP, "http://localhost:11434/v1", false},
		{localEP, "http://[fe80::1%25eth0]/v1", false},
		{localEP, "http://[fd00::1%25eth0]/v1", false},
		{localEP, "http://[::ffff:10.0.0.1]/v1", true},
		{types.RegistryDestinationKindBroker, "https://broker.example.com/v1", false},
		{types.RegistryDestinationKindGovernedUpstream, "https://gov.example.com/v1", false},
	} {
		_, ok := route(types.RegistryDestination{Kind: c.kind, Endpoint: c.endpoint}, "/chat/completions")
		if ok != c.ok {
			t.Errorf("%s %s: admitted %v", c.kind, c.endpoint, ok)
		}
	}
	u, ok := route(types.RegistryDestination{Kind: direct, Endpoint: "https://api.example.com/v1/"}, "/chat/completions")
	if !ok || u.String() != "https://api.example.com/v1/chat/completions" || dialAddr(u) != "api.example.com:443" {
		t.Fatalf("route %v", u)
	}
}

// A local endpoint has no credential: nothing is injected.
func TestALocalEndpointGetsNoCredential(t *testing.T) {
	up := newUpstream(t, nil)
	g, token := rig(t, up, func(c *Config) {
		c.Registry, c.Leases = reg(t, "registry-live.example.json", up.URL+"/v1"), running(local)
	})
	// The live example's other endpoint is https://api.example.com; only the local one is called.
	if w := call(g, token, "/chat/completions", `{"model":"local-coder:32b"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if h := up.got()[0].header; h.Get("Authorization") != "" {
		t.Fatalf("a local endpoint got %v", h)
	}
}

func TestAPerDestinationInjector(t *testing.T) {
	up := newUpstream(t, nil)
	g, token := rig(t, up, func(c *Config) {
		c.Inject = map[types.Slug]Injector{"stub": func(h http.Header, cred Credential) { h.Set("X-Api-Key", cred.Reveal()) }}
	})
	if w := call(g, token, "/chat/completions", good, nil); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if h := up.got()[0].header; h.Get("X-Api-Key") != fakeKey || h.Get("Authorization") != "" {
		t.Fatalf("injected %v", h)
	}
}

// Launch readiness, and a credential gone between launch and call.
func TestHoldsAndAMissingCredential(t *testing.T) {
	up := newUpstream(t, nil)
	held := secrets{"stub": fakeKey}
	g, token := rig(t, up, func(c *Config) { c.Secrets = held })
	ctx := context.Background()
	if ok, err := g.Holds(ctx, "stub", "stub"); !ok || err != nil {
		t.Fatalf("holds %v %v", ok, err)
	}
	for _, c := range [][2]types.Slug{{"stub", "other"}, {"", "stub"}, {"stub", ""}} {
		if ok, _ := g.Holds(ctx, c[0], c[1]); ok {
			t.Errorf("holds %v", c)
		}
	}
	delete(held, "stub")
	if ok, _ := g.Holds(ctx, "stub", "stub"); ok {
		t.Fatal("a removed credential is still held")
	}
	if w := call(g, token, "/chat/completions", good, nil); w.Code != http.StatusServiceUnavailable || len(up.got()) != 0 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestEnvSecrets(t *testing.T) {
	t.Setenv("ADC_CREDENTIAL_API_REVIEWER", fakeKey)
	if name, ok := EnvName("api-reviewer"); !ok || name != "ADC_CREDENTIAL_API_REVIEWER" {
		t.Fatal(name)
	}
	if c, ok, _ := (EnvSecrets{}).Credential("api-reviewer"); !ok || c.Reveal() != fakeKey {
		t.Fatal("env credential not read")
	}
	// M3: two references never share a variable; "api_reviewer" would
	// read the key of "api-reviewer".
	if c, ok, err := (EnvSecrets{}).Credential("api_reviewer"); ok || err == nil || c.Reveal() == fakeKey {
		t.Fatalf("a colliding reference read another's credential: %v %v", ok, err)
	}
	seen := map[string]types.Slug{}
	for _, ref := range []types.Slug{"a-b", "a_b", "a-_b", "a_-b", "a--b", "a__b", "ab", "a-b-c"} {
		if name, ok := EnvName(ref); ok {
			if prev, dup := seen[name]; dup {
				t.Errorf("%s and %s share %s", prev, ref, name)
			}
			seen[name] = ref
		}
	}
	if _, ok, _ := (EnvSecrets{}).Credential("absent"); ok {
		t.Fatal("an absent env credential is held")
	}
}

func TestACredentialNeverPrints(t *testing.T) {
	c := NewCredential(fakeKey)
	s := fmt.Sprintf("%v %s %q %#v %d %x %+v", c, c, c, c, c, c, struct{ C Credential }{c})
	if strings.Contains(s, fakeKey) || strings.Contains(c.String()+c.GoString(), fakeKey) {
		t.Fatalf("printed: %s", s)
	}
	if b, err := json.Marshal(struct{ C Credential }{c}); err == nil || bytes.Contains(b, []byte(fakeKey)) {
		t.Fatalf("marshalled: %s %v", b, err)
	}
}

// §8 item 4: the capture is the body as sent, before injection, a
// restricted blob expiring P7D later; a body holding the credential is
// neither captured nor sent.
func TestCaptureKeepsTheSentBodyAsARestrictedBlob(t *testing.T) {
	up := newUpstream(t, nil)
	blobs, err := blobstore.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	var kept []Captured
	g, token := rig(t, up, func(c *Config) {
		c.Capture = BlobCapture{Blobs: blobs, Now: func() time.Time { return now },
			Record: func(_ context.Context, k Captured) error { kept = append(kept, k); return nil }}
	})
	if w := call(g, token, "/chat/completions", good, nil); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(kept) != 1 || kept[0].Class != ClassRestricted || !kept[0].ExpiresAt.Equal(now.Add(7*24*time.Hour)) ||
		kept[0].Lease != leaseID || kept[0].Attempt != attempt {
		t.Fatalf("kept %+v", kept)
	}
	rc, err := blobs.Open(kept[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(body, up.got()[0].body) || bytes.Contains(body, []byte(fakeKey)) {
		t.Fatalf("captured %s", body)
	}
	if w := call(g, token, "/chat/completions", `{"model":"stub-builder-1","user":"`+fakeKey+`"}`, nil); w.Code != http.StatusForbidden {
		t.Fatalf("a body with the credential: %d", w.Code)
	}
	if len(kept) != 1 || len(up.got()) != 1 {
		t.Fatalf("a body with the credential was captured or sent")
	}
	g.cfg.Capture = BlobCapture{Blobs: blobs, Now: func() time.Time { return now },
		Record: func(context.Context, Captured) error { return errors.New("record failed") }}
	if w := call(g, token, "/chat/completions", good, nil); w.Code != http.StatusServiceUnavailable || len(up.got()) != 1 {
		t.Fatalf("an unrecorded capture was sent: %d", w.Code)
	}
}
