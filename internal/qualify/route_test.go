// SPDX-License-Identifier: Apache-2.0

//go:build linux

package qualify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/review"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

const config = "../../fixtures/m1/config/"

type secrets struct{}

func (secrets) Credential(types.Slug) (gateway.Credential, bool, error) {
	return gateway.NewCredential("fake-credential-0123456789"), true, nil // a test value
}

// scripted is a loopback chat-completions provider answering each request
// with reply(prompt), and keeping what it was sent.
type scripted struct {
	*httptest.Server
	mu    sync.Mutex
	reply func(prompt string) string
	seen  []map[string]any
	auth  []string
}

func newScripted(t *testing.T) *scripted {
	u := &scripted{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		prompt := ""
		if ms, _ := body["messages"].([]any); len(ms) > 0 {
			last, _ := ms[len(ms)-1].(map[string]any)
			prompt, _ = last["content"].(string)
		}
		u.mu.Lock()
		u.seen, u.auth = append(u.seen, body), append(u.auth, r.Header.Get("Authorization"))
		content := u.reply(prompt)
		u.mu.Unlock()
		out, _ := json.Marshal(map[string]any{"id": "chatcmpl-1", "object": "chat.completion", "model": body["model"],
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": content}}},
			"usage":   map[string]any{"prompt_tokens": 1000, "completion_tokens": 50, "total_tokens": 1050}})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	t.Cleanup(u.Close)
	return u
}

func decision(d string) string {
	b, _ := json.Marshal(review.Decision{Decision: d, Summary: "scripted", Findings: []review.Finding{}})
	return string(b)
}

// stubRoute is the stub registry's reviewer, its endpoint pointed at up,
// run through the http-chat adapter with node (as make test runs it).
func stubRoute(t *testing.T, up *scripted) (Route, types.QualifiedRoute) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH: the adapter tests run under make test (the test sandbox provides /opt/node/bin/node)")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap is not on PATH")
	}
	adapters, err := filepath.Abs(filepath.Join("..", "..", "adapters"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(adapters, "node_modules")); err != nil {
		t.Skip("the adapters workspace is not installed: run make ts-deps (make test does)")
	}
	doc, err := registry.Load(config + "registry-stub.json")
	if err != nil {
		t.Fatal(err)
	}
	for i := range doc.Executors {
		if strings.HasPrefix(doc.Executors[i].Destination.Endpoint, "http://127.0.0.1:") {
			doc.Executors[i].Destination.Endpoint = up.URL + "/v1"
		}
	}
	var groups []types.ResourceGroup
	raw, err := os.ReadFile(config + "groups.json")
	if err == nil {
		err = json.Unmarshal(raw, &groups)
	}
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.New(doc, groups)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := reg.Executor(route.Executor)
	return Route{Registry: reg, Executor: route.Executor, Secrets: secrets{},
		Adapter: session.Config{Command: []string{node, filepath.Join(adapters, "http-chat", "src", "main.ts")},
			Env: append([]string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + t.TempDir()}, testguard.HomeEnv(t)...), Deadline: 60 * time.Second}}, RouteOf(e)
}

// TestRoute runs the suite through the real http-chat adapter and gateway
// against a scripted upstream (the lead's decision 4: CI checks the
// harness, never a model): a discriminating reviewer qualifies the route,
// an all-go reviewer does not, and a reviewer whose answers are no
// decision misses every case.
func TestRoute(t *testing.T) {
	s := suite(t)
	gobin, env := goEnv(t)
	up := newScripted(t)
	r, qr := stubRoute(t, up)
	ctx := context.Background()
	prompts := map[string][]byte{}
	for _, c := range s.Cases {
		p, err := s.Prompt(ctx, c, gobin, t.TempDir(), env)
		if err != nil {
			t.Fatal(err)
		}
		prompts[c.ID] = p
	}
	byPrompt := map[string]Case{}
	for _, c := range s.Cases {
		byPrompt[string(prompts[c.ID])] = c
	}
	run := func(reply func(c Case) string) map[string]Outcome { // the upstream answers the prompt it was sent, not the loop's case (Opus #306 M5)
		out := map[string]Outcome{}
		for _, c := range s.Cases {
			var got string
			up.mu.Lock()
			up.reply = func(p string) string {
				got = p
				if x, ok := byPrompt[p]; ok {
					return reply(x)
				}
				return "an unknown prompt"
			}
			up.mu.Unlock()
			o, err := r.Review(ctx, prompts[c.ID])
			if err != nil {
				t.Fatalf("%s: %v", c.ID, err)
			}
			up.mu.Lock()
			if got != string(prompts[c.ID]) {
				t.Errorf("%s: the provider was sent another prompt (%d bytes, want %d)", c.ID, len(got), len(prompts[c.ID]))
			}
			up.mu.Unlock()
			var rep report
			if err := readReport(o.Report, &rep); err != nil || rep.Schema != "adc.review-report/v1" || rep.Decision != o.Decision {
				t.Errorf("%s: the report %s, %v", c.ID, o.Report, err)
			}
			out[c.ID] = o
		}
		return out
	}
	decided := func(out map[string]Outcome) map[string]string {
		m := map[string]string{}
		for id, o := range out {
			m[id] = o.Decision
		}
		return m
	}
	records := t.TempDir()
	good := run(func(c Case) string { return decision(c.Expect) })
	if noGo, goes, ok := s.Score(decided(good)); !ok || noGo != 4 || goes != 4 {
		t.Errorf("the discriminating reviewer: %d no_go, %d go, qualified %v", noGo, goes, ok)
	}
	if _, err := s.Record(records, "good", qr, time.Now(), good); err != nil {
		t.Fatal(err)
	}
	if err := Check(records, qr, s); err != nil {
		t.Errorf("the discriminating reviewer's record: %v", err)
	}
	up.mu.Lock()
	for i, body := range up.seen {
		ms, _ := body["messages"].([]any)
		rf, _ := body["response_format"].(map[string]any)
		if body["model"] != "stub-reviewer-1" || body["tools"] != nil || rf["type"] != "json_schema" || len(ms) == 0 ||
			up.auth[i] != "Bearer fake-credential-0123456789" {
			t.Errorf("request %d: %v (authorization injected: %v)", i, body, up.auth[i] == "Bearer fake-credential-0123456789")
		}
	}
	if n := len(up.seen); n != len(s.Cases) {
		t.Errorf("%d requests for %d cases", n, len(s.Cases))
	}
	up.mu.Unlock()
	lax := t.TempDir()
	allGo := run(func(Case) string { return decision(review.Go) })
	if noGo, _, ok := s.Score(decided(allGo)); ok || noGo != 0 {
		t.Errorf("the all-go reviewer: %d no_go, qualified %v", noGo, ok)
	}
	if rec, err := s.Record(lax, "all-go", qr, time.Now(), allGo); err != nil || rec.Qualified {
		t.Fatalf("%+v, %v", rec, err)
	}
	if err := Check(lax, qr, s); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), "under the bar") {
		t.Errorf("the all-go reviewer's record: %v", err)
	}
	noGos := t.TempDir() // Opus #306 L1: a reviewer that always says no_go
	allNoGo := run(func(Case) string { return decision(review.NoGo) })
	if noGo, goes, ok := s.Score(decided(allNoGo)); ok || noGo != 4 || goes != 0 {
		t.Errorf("the all-no_go reviewer: %d no_go, %d go, qualified %v", noGo, goes, ok)
	}
	if _, err := s.Record(noGos, "all-no-go", qr, time.Now(), allNoGo); err != nil {
		t.Fatal(err)
	}
	if err := Check(noGos, qr, s); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), "under the bar: 4 of 4 defects no_go, 0 go") {
		t.Errorf("the all-no_go reviewer's record: %v", err)
	}
	for id, o := range run(func(Case) string { return "not a decision" }) {
		if o.Decision != "" {
			t.Errorf("%s: a malformed answer decided %q", id, o.Decision)
		}
	}
}
