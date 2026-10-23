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
	"slices"
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
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
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
	// run is one run of the suite, begun in dir, in its seed's order: the
	// upstream answers the prompt it was sent, not the loop's case (Opus
	// #306 M5).
	run := func(dir, name string, reply func(c Case) string) (*Run, map[string]Outcome) {
		x, err := s.Begin(dir, name, qr, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]Outcome{}
		for seq, c := range s.Order(x.Seed) {
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
			o, err := r.Review(ctx, x, c, prompts[c.ID])
			if err != nil {
				t.Fatalf("%s: %v", c.ID, err)
			}
			up.mu.Lock()
			if got != string(prompts[c.ID]) {
				t.Errorf("%s: the provider was sent another prompt (%d bytes, want %d)", c.ID, len(got), len(prompts[c.ID]))
			}
			up.mu.Unlock()
			if rep, err := review.ReadReport(o.Report); err != nil || rep.Decision != o.Decision {
				t.Errorf("%s: the report %s, %v", c.ID, o.Report, err)
			} else if o.evidence == nil || !slices.Equal(rep.ChargeLinks, []types.CallID{o.evidence.Call}) {
				t.Errorf("%s: the charge links %v, want the call the gateway counted", c.ID, rep.ChargeLinks)
			}
			// Astra #306 H1; Opus #306 r2 M1, L2: the evidence is the execution's.
			if ev := o.evidence; ev == nil || ev.RunID != x.ID || ev.RanAt != types.Time(x.At.Format(time.RFC3339)) || ev.Seq != int64(seq) ||
				ev.PromptDigest != c.PromptDigest || ev.Route != qr || ev.AdapterDigest != s.Adapter || ev.SuiteDigest != s.Digest {
				t.Errorf("%s: the evidence %+v", c.ID, ev)
			}
			out[c.ID] = o
		}
		return x, out
	}
	decided := func(out map[string]Outcome) map[string]string {
		m := map[string]string{}
		for id, o := range out {
			m[id] = o.Decision
		}
		return m
	}
	records := t.TempDir()
	goodRun, good := run(records, "good", func(c Case) string { return decision(c.Expect) })
	if noGo, goes, ok := s.Score(decided(good)); !ok || noGo != 4 || goes != 4 {
		t.Errorf("the discriminating reviewer: %d no_go, %d go, qualified %v", noGo, goes, ok)
	}
	if err := Check(records, qr, s); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), "an incomplete run") {
		t.Errorf("a run every case of which is reviewed, before Record: %v, want unqualified (Opus #306 r2 M2)", err)
	}
	if _, err := s.Record(goodRun, good); err != nil {
		t.Fatal(err)
	}
	if err := Check(records, qr, s); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), "one run of the route") {
		t.Errorf("the discriminating reviewer's one record: %v, want unqualified (#226 entry 110)", err)
	}
	up.mu.Lock()
	for i, body := range up.seen {
		ms, _ := body["messages"].([]any)
		rf, _ := body["response_format"].(map[string]any)
		if body["model"] != "stub-reviewer-1" || body["tools"] != nil || rf["type"] != "json_schema" || len(ms) == 0 ||
			up.auth[i] != "Bearer fake-credential-0123456789" {
			t.Errorf("request %d: %v (authorization injected: %v)", i, body, up.auth[i] == "Bearer fake-credential-0123456789")
		}
		if i < len(s.Cases) { // Astra #306 M5: the requests came in the seed's order
			last, _ := ms[len(ms)-1].(map[string]any)
			if want := s.Order(goodRun.Seed)[i]; last["content"] != string(prompts[want.ID]) {
				t.Errorf("request %d is not the seed's case %s", i, want.ID)
			}
		}
	}
	if n := len(up.seen); n != len(s.Cases) {
		t.Errorf("%d requests for %d cases", n, len(s.Cases))
	}
	up.mu.Unlock()
	againRun, again := run(records, "good-again", func(c Case) string { return decision(c.Expect) }) // a second run, its own seed: the route qualifies
	if _, err := s.Record(againRun, again); err != nil {
		t.Fatal(err)
	}
	if err := Check(records, qr, s); err != nil {
		t.Errorf("the discriminating reviewer's two records: %v", err)
	}
	lax := t.TempDir()
	goRun, allGo := run(lax, "all-go", func(Case) string { return decision(review.Go) })
	if noGo, _, ok := s.Score(decided(allGo)); ok || noGo != 0 {
		t.Errorf("the all-go reviewer: %d no_go, qualified %v", noGo, ok)
	}
	if rec, err := s.Record(goRun, allGo); err != nil || rec.Qualified {
		t.Fatalf("%+v, %v", rec, err)
	}
	if err := Check(lax, qr, s); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), "under the bar") {
		t.Errorf("the all-go reviewer's record: %v", err)
	}
	noGos := t.TempDir() // Opus #306 L1: a reviewer that always says no_go
	noGoRun, allNoGo := run(noGos, "all-no-go", func(Case) string { return decision(review.NoGo) })
	if noGo, goes, ok := s.Score(decided(allNoGo)); ok || noGo != 4 || goes != 0 {
		t.Errorf("the all-no_go reviewer: %d no_go, %d go, qualified %v", noGo, goes, ok)
	}
	if _, err := s.Record(noGoRun, allNoGo); err != nil {
		t.Fatal(err)
	}
	if err := Check(noGos, qr, s); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), "under the bar: 4 of 4 defects no_go, 0 go") {
		t.Errorf("the all-no_go reviewer's record: %v", err)
	}
	// Astra #306 H1: the all-no_go run's defects and the all-go run's
	// controls, unchanged, recorded as either run (or a new one) are not
	// a run: Record refuses them.
	mixed := map[string]Outcome{}
	for _, c := range s.Cases {
		mixed[c.ID] = allGo[c.ID]
		if c.Expect == review.NoGo {
			mixed[c.ID] = allNoGo[c.ID]
		}
	}
	fresh, err := s.Begin(t.TempDir(), "mixed", qr, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		x   *Run
		why string
	}{"the all-go run": {goRun, "has no outcome of the run"}, "the all-no_go run": {noGoRun, "has no outcome of the run"}, "a new run": {fresh, "reviewed 0 of its 8 cases"}} {
		if _, err := s.Record(c.x, mixed); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("two runs' reports recorded as %s: %v, want %q", name, err, c.why)
		}
	}
	garbledRun, garbled := run(t.TempDir(), "garbled", func(Case) string { return "not a decision" })
	for id, o := range garbled {
		if o.Decision != "" {
			t.Errorf("%s: a malformed answer decided %q", id, o.Decision)
		}
	}
	// Opus #306 r2 M1, its two probe drivers and their neighbours: Review
	// admits each case once, in the seed's order, with its own prompt.
	before := len(up.seen)
	probe, err := s.Begin(t.TempDir(), "probe", qr, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	order := s.Order(probe.Seed)
	other := r
	other.Adapter.Command = []string{r.Adapter.Command[0], filepath.Join(t.TempDir(), "main.ts")}
	for name, c := range map[string]struct {
		r      Route
		x      *Run
		c      Case
		prompt []byte
		why    string
	}{
		"the suite's order, not the seed's (driver 1)": {r, probe, order[1], prompts[order[1].ID], "is not the run's next"},
		"another case's prompt":                        {r, probe, order[0], prompts[order[1].ID], "the prompt is not the case's"},
		"a run Begin did not start":                    {r, &Run{ID: probe.ID, Seed: probe.Seed}, order[0], prompts[order[0].ID], "not a run Begin started"},
		"an adapter outside the root (r2 L3)":          {other, probe, order[0], prompts[order[0].ID], "is not adapters/http-chat/src/main.ts"},
		"a case of an ended run":                       {r, garbledRun, order[0], prompts[order[0].ID], "has reviewed every case"},
	} {
		if _, err := c.r.Review(ctx, c.x, c.c, c.prompt); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: %v, want %q", name, err, c.why)
		}
	}
	// The charge link names only a call the gateway counted: an adapter
	// that exits before its first frame (its node fails to load, as
	// without make ts-deps) made none, and its report links none.
	dead := r
	dead.Adapter.Env = append(slices.Clone(r.Adapter.Env), "NODE_OPTIONS=--require="+filepath.Join(t.TempDir(), "missing.cjs"))
	crashed, err := s.Begin(t.TempDir(), "crashed", qr, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	first := s.Order(crashed.Seed)[0]
	o0, err := dead.Review(ctx, crashed, first, prompts[first.ID])
	if err != nil {
		t.Fatal(err)
	}
	if rep, err := review.ReadReport(o0.Report); err != nil || rep.Reason != vocab.ReasonAdapterProtocolError.String() || len(rep.ChargeLinks) != 0 {
		t.Errorf("an adapter that never started: the report %s, %v; want adapter_protocol_error and no charge link", o0.Report, err)
	}
	// Astra #306 H1: the evidence's route is the executor's the review ran
	// on, never the run's claim; a run claimed for another model is
	// refused by Record.
	claimed := qr
	claimed.Model = "another-model"
	x, err := s.Begin(t.TempDir(), "claimed", claimed, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c := s.Order(x.Seed)[0]
	o, err := r.Review(ctx, x, c, prompts[c.ID])
	if err != nil || o.evidence == nil || o.evidence.Route != qr {
		t.Fatalf("a review in a run claimed for another model: %+v, %v", o.evidence, err)
	}
	// Opus #306 r2 M1, driver 2: the best of three answers for one case.
	if _, err := r.Review(ctx, x, c, prompts[c.ID]); err == nil || !strings.Contains(err.Error(), "is not the run's next") {
		t.Errorf("a case reviewed again: %v", err)
	}
	if n := len(up.seen) - before; n != 1 {
		t.Errorf("%d requests upstream since the probe rows; want 1, the claimed run's review: a refused review sends none", n)
	}
	out := finish(t, s, x, all(s, ""), map[string]Outcome{c.ID: o})
	if _, err := s.Record(x, out); err == nil || !strings.Contains(err.Error(), "ran on another route") {
		t.Errorf("a review on the executor's route recorded as another model's: %v", err)
	}
}

// P31a: a reviewer is prepared with no tool, neither its own nor mediated,
// and a read-only workspace. The http-chat adapter never offers a reviewer
// a tool either (TestRoute: no request carries tools), so this checks the
// controller's own params, which an adapter that honoured them would use.
func TestReviewerPrepare(t *testing.T) {
	limits := types.AdapterPrepareParamsLimits{MaxTurns: MaxTurns, MaxOutputTokens: MaxOutputTokens, WallSeconds: 60}
	p := reviewerPrepare("exe_01m4tm99haemqt8es2qfbwj8pb", "stub-reviewer-1", "/work", "a-token-of-at-least-sixteen", limits)
	if p.Role != "reviewer" || p.Workspace.Writable || len(p.ToolPolicy.Allowed) != 0 || len(p.ToolPolicy.Mediated) != 0 ||
		p.ToolPolicy.Allowed == nil || p.ToolPolicy.Mediated == nil {
		t.Errorf("a reviewer's prepare params: %+v", p)
	}
}
