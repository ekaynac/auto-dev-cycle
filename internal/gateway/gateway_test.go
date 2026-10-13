// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
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

func running(executor types.ExecutorID) leases {
	a := attempt
	return leases{leaseID: {ID: leaseID, Executor: executor, State: types.LeaseStateActive, Attempt: &a}}
}

// rig is a gateway over the stub registry pointed at up, with the builder's
// lease running and its credential held, and a bound token.
func rig(t *testing.T, up *upstream, edit func(*Config)) (*Gateway, string) {
	t.Helper()
	cfg := Config{Registry: reg(t, "registry-stub.json", up.URL+"/v1"), Leases: running(builder), Secrets: secrets{"stub": fakeKey}}
	if edit != nil {
		edit(&cfg)
	}
	g := New(cfg)
	token, err := g.Bind(Binding{Lease: leaseID, Attempt: attempt})
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
	g.ServeHTTP(w, r)
	return w
}

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
		"another attempt running":  {func(c *Config) { l := c.Leases.(leases)[leaseID]; l.Attempt = &other; c.Leases = leases{leaseID: l} }, "", 409},
		"no such lease":            {func(c *Config) { c.Leases = leases{} }, "", 409},
		"an unregistered executor": {func(c *Config) { c.Leases = running(local) }, "", 403},
		"a path off the list":      {nil, "/models", 404},
		"a proxy-form target":      {nil, "http://127.0.0.2:1/elsewhere", 404},
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
	if n := len(up.got()); n != 0 {
		t.Fatalf("upstream saw %d refused requests", n)
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
	if EnvName("api-reviewer") != "ADC_CREDENTIAL_API_REVIEWER" {
		t.Fatal(EnvName("api-reviewer"))
	}
	if c, ok, _ := (EnvSecrets{}).Credential("api-reviewer"); !ok || c.Reveal() != fakeKey {
		t.Fatal("env credential not read")
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
	if w := call(g, token, "/chat/completions", `{"model":"stub-builder-1","x":"`+fakeKey+`"}`, nil); w.Code != http.StatusForbidden {
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
