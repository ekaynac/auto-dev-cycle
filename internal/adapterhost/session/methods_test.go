// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const describeResult = `{"harness":"stub","harness_version":"1.0.0","adapter_version":"0.1.0","contract_version":"%s",` +
	`"capabilities":["text"],"session_semantics":"fresh_process","tool_surface":[],` +
	`"telemetry":{"usage":false,"upstream":false,"stream_deltas":false,"per_turn_coverage":false},"host_side":false,"needs_native_login":false}`

// describes answers describe with a contract version.
func describes(version string) []step {
	return []step{{Lines: []string{resultLine(fmt.Sprintf(describeResult, version))}}}
}

// otherAttempt is a valid attempt id that is not the session's.
const otherAttempt = "att_01m3250vnfeyrv67x6w0ddqr6b"

var foreignHandleJSON = `{"attempt":"` + otherAttempt + `","handle":"x","digest":"sha256:` + strings.Repeat("0", 64) + `","size":1}`

// fakeConfig runs the TypeScript fake adapter (adapters/fake) with node, as
// make test does inside the test sandbox.
func fakeConfig(t *testing.T, scenario string) Config {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH: the fake adapter tests run under make test (the test sandbox provides /opt/node/bin/node)")
	}
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "adapters"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "fake", "node_modules")); err != nil {
		t.Skip("the adapters workspace is not installed: run make ts-deps (make test does)")
	}
	return Config{Command: []string{node, filepath.Join(root, "fake", "src", "main.ts")},
		Env: []string{"PATH=" + os.Getenv("PATH"), "ADC_FAKE_SCENARIO=" + scenario}, Deadline: 30 * time.Second}
}

func handle(name string) types.AdapterHandle {
	return types.AdapterHandle{Attempt: attempt, Handle: name, Digest: "sha256:" + strings.Repeat("0", 64), Size: 0}
}

func prepareParams(mediated ...string) types.AdapterPrepareParams {
	return types.AdapterPrepareParams{Attempt: attempt, FencingToken: token, Role: "builder", Executor: executor, Model: "m",
		Workspace: types.AdapterPrepareParamsWorkspace{Path: "/workspace", Writable: true},
		Exchange:  types.AdapterPrepareParamsExchange{In: "/exchange/in", Out: "/exchange/out"}, Inputs: []types.AdapterHandle{},
		Limits:     types.AdapterPrepareParamsLimits{MaxTurns: 8, MaxOutputTokens: 8000, WallSeconds: 600},
		ToolPolicy: types.AdapterPrepareParamsToolPolicy{Allowed: []string{"Edit"}, Mediated: append([]string{}, mediated...)}}
}

var runParams = types.AdapterRunParams{Attempt: attempt, FencingToken: token, Call: call, Prompt: handle("prompt.md")}
var fencedParams = types.AdapterFencedParams{Attempt: attempt, FencingToken: token}

// prepared describes and prepares the session.
func prepared(t *testing.T, s *Session, mediated ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Describe(ctx); err != nil {
		t.Fatalf("describe: %v", err)
	}
	if o, err := s.Prepare(ctx, prepareParams(mediated...)); err != nil || o.Outcome != types.OutcomeEmpty {
		t.Fatalf("prepare: %+v %v", o, err)
	}
}

// waitBounded waits for the process tree within d.
func waitBounded(t *testing.T, s *Session, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = s.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("the process tree did not exit within %s", d)
	}
}

func TestFakeRoundTrip(t *testing.T) {
	s := open(t, fakeConfig(t, "patch"))
	ctx := context.Background()
	prepared(t, s)
	var names []string
	r, err := s.Run(ctx, runParams, func(e Event) { names = append(names, e.Name) })
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if r.Outcome != types.OutcomeFound || r.Result == nil || r.Result.Kind != types.AdapterRunResultResultValueKindPatch || r.UsageFinal == nil {
		t.Fatalf("run result: %+v", r)
	}
	joined := strings.Join(names, " ")
	for _, want := range []string{"call.sub", "turn.start", "usage", "coverage", "turn.end"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %s event among %s", want, joined)
		}
	}
	if o, err := s.Shutdown(ctx); err != nil || o.Outcome != types.OutcomeEmpty {
		t.Fatalf("shutdown: %+v %v", o, err)
	}
	if err := s.Wait(); err != nil {
		t.Fatalf("the adapter's exit after shutdown: %v (stderr %q)", err, s.Stderr())
	}
	wantEnd(t, s, s.Err(), KindClosed, vocab.ReasonAttemptStopped)
	if _, err := s.Describe(ctx); err == nil {
		t.Fatal("a call after the end succeeded")
	}
}

func TestFakeConcurrentRequests(t *testing.T) {
	s := open(t, fakeConfig(t, "tool"))
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := s.Describe(ctx); err != nil {
				t.Errorf("concurrent describe: %v", err)
			}
		})
	}
	wg.Wait()
	prepared(t, s, "RunTest")
	acked := make(chan error, 1)
	r, err := s.Run(ctx, runParams, func(e Event) {
		if e.Name != "tool.request" {
			return
		}
		var q struct {
			Turn    int64               `json:"turn"`
			Tool    string              `json:"tool"`
			Request types.AdapterHandle `json:"request"`
		}
		if err := json.Unmarshal(e.Raw, &q); err != nil {
			t.Error(err)
			return
		}
		// Answered from another goroutine while run is pending.
		go func() {
			acked <- s.ToolResponse(ctx, types.AdapterToolResponseParams{Attempt: attempt, FencingToken: token, Call: call, Turn: q.Turn,
				Tool: q.Tool, Request: q.Request, Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Delivered: handle("delivered-1")})
		}()
	})
	if err != nil || r.Outcome != types.OutcomeFound {
		t.Fatalf("run: %+v %v", r, err)
	}
	if err := <-acked; err != nil {
		t.Fatalf("tool.response: %v", err)
	}
}

// hangUntil runs the hang scenario and calls stop once it streams.
func hangUntil(t *testing.T, s *Session, stop func()) (types.AdapterRunResult, error) {
	prepared(t, s)
	var once sync.Once
	return s.Run(context.Background(), runParams, func(e Event) {
		if e.Name == "delta.thinking" {
			once.Do(func() { go stop() })
		}
	})
}

func TestFakeHangEndedByCancel(t *testing.T) {
	s := open(t, fakeConfig(t, "hang"))
	cancelled := make(chan error, 1)
	r, err := hangUntil(t, s, func() {
		_, err := s.Cancel(context.Background(), fencedParams)
		cancelled <- err
	})
	if err != nil || r.Outcome != types.OutcomeCouldNotRun || r.Reason != vocab.ReasonAttemptStopped || r.Result != nil {
		t.Fatalf("run after cancel: %+v %v", r, err)
	}
	if err := <-cancelled; err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitBounded(t, s, 10*time.Second)
}

func TestFakeHangEndedByDeadline(t *testing.T) {
	cfg := fakeConfig(t, "hang")
	cfg.Deadline = 3 * time.Second
	s := open(t, cfg)
	started := time.Now()
	_, err := hangUntil(t, s, func() {})
	wantEnd(t, s, err, KindDeadline, vocab.ReasonAttemptStopped)
	waitBounded(t, s, 5*time.Second)
	if d := time.Since(started); d > 8*time.Second {
		t.Fatalf("the deadline settled the run after %s", d)
	}
}

func TestAnotherContractMajorIsRefused(t *testing.T) {
	// A schema-valid version that carries a secret in its prerelease.
	for _, version := range []string{"2.0.0", "9.0.0-" + strings.ReplaceAll(secret, "_", "-")} {
		s := open(t, adapter(t, script{"describe": describes(version)}))
		_, err := s.Describe(context.Background())
		wantEnd(t, s, err, KindVersion, vocab.ReasonAdapterProtocolError)
		leaks(t, version, err, secret)
	}
}

func TestALateDescribeIsStillChecked(t *testing.T) {
	for late, kind := range map[string]Kind{"[]": KindProtocol, fmt.Sprintf(describeResult, "2.0.0"): KindVersion} {
		s := open(t, adapter(t, script{"describe": {{DelayMs: 300, Lines: []string{resultLine(late)}}}}))
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, err := s.Describe(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the abandoned describe returned %v", err)
		}
		ended(t, s)
		wantEnd(t, s, s.Err(), kind, vocab.ReasonAdapterProtocolError)
	}
}

func TestADescribeThatLeaksIsRefusedClean(t *testing.T) {
	s := open(t, adapter(t, script{"describe": describes(secret)})) // schema-invalid
	_, err := s.Describe(context.Background())
	wantEnd(t, s, err, KindProtocol, vocab.ReasonAdapterProtocolError)
	leaks(t, "describe", err, secret)
}

// TestAForeignHandleIsRefused: a handle of another attempt, in any event or
// in the run's result, is never passed on; it ends the session with an
// unsafe_output refusal (check foreign_handle).
func TestAForeignHandleIsRefused(t *testing.T) {
	h := foreignHandleJSON
	for where, line := range map[string]string{
		"tool.request": eventLine(`"event":"tool.request","turn":1,"tool":"RunTest","request":`+h, ownStamp),
		"error":        eventLine(`"event":"error","turn":1,"class_hint":"other","raw_excerpt":`+h, ownStamp),
		"tool.result":  eventLine(`"event":"tool.result","turn":1,"tool":"RunTest","mediated_by_controller":true,"delivered":`+h+`,"bytes":1`, ownStamp),
		"result":       resultLine(`{"outcome":"found","reason":"completed_with_findings","detail":"a text","result":{"kind":"text","handle":` + h + `},"usage_final":null}`),
	} {
		s := open(t, adapter(t, script{"run": {{Lines: []string{line}}}}))
		prepared(t, s)
		var seen []string
		_, err := s.Run(context.Background(), runParams, func(e Event) { seen = append(seen, e.Name) })
		wantEnd(t, s, err, KindForeignHandle, vocab.ReasonUnsafeOutput)
		var e *Error
		if errors.As(err, &e); e.Check != CheckForeignHandle || len(seen) != 0 {
			t.Fatalf("%s: check %q, events passed on %v", where, e.Check, seen)
		}
		if f, ok := s.Foreign(); !ok || f.Attempt != otherAttempt || f.Handle != "x" || f.Size != 1 {
			t.Fatalf("%s: the refused handle kept: %+v %v", where, f, ok)
		}
	}
}

// pendingRun starts a run the script never answers and waits until it is
// registered.
func pendingRun(t *testing.T, s *Session) {
	t.Helper()
	go func() { _, _ = s.Run(context.Background(), runParams, nil) }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		s.mu.Lock()
		running := s.run != nil
		s.mu.Unlock()
		if running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the run never started")
		}
	}
}

func toolResponse() types.AdapterToolResponseParams {
	return types.AdapterToolResponseParams{Attempt: attempt, FencingToken: token, Call: call, Turn: 1, Tool: "RunTest",
		Request: handle("req"), Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Delivered: handle("d")}
}

// TestTheCoresParamsAreFenced: the core's own params must name the prepared
// attempt and token (and the run's call), with handles of that attempt;
// otherwise nothing is sent and the session goes on.
func TestTheCoresParamsAreFenced(t *testing.T) {
	ctx := context.Background()
	s := open(t, adapter(t, script{"run": {{}}, "tool.response": {{Lines: []string{resultLine(`{}`)}}}}))
	if _, err := s.Run(ctx, runParams, nil); !errors.Is(err, ErrSequence) {
		t.Fatalf("run before prepare: %v", err)
	}
	foreignInput := prepareParams()
	foreignInput.Inputs = []types.AdapterHandle{{Attempt: otherAttempt, Handle: "in", Digest: "sha256:" + strings.Repeat("0", 64)}}
	if _, err := s.Prepare(ctx, foreignInput); !errors.Is(err, ErrForeignParams) {
		t.Fatalf("prepare with a foreign input: %v", err)
	}
	prepared(t, s)
	other := runParams
	other.FencingToken = token + 1
	if _, err := s.Run(ctx, other, nil); !errors.Is(err, ErrForeignParams) {
		t.Fatalf("run with another token: %v", err)
	}
	other = runParams
	other.Prompt.Attempt = otherAttempt
	if _, err := s.Run(ctx, other, nil); !errors.Is(err, ErrForeignParams) {
		t.Fatalf("run with a foreign prompt: %v", err)
	}
	if _, err := s.Cancel(ctx, types.AdapterFencedParams{Attempt: otherAttempt, FencingToken: token}); !errors.Is(err, ErrForeignParams) {
		t.Fatalf("cancel of another attempt: %v", err)
	}
	if err := s.ToolResponse(ctx, toolResponse()); !errors.Is(err, ErrForeignParams) {
		t.Fatalf("tool.response with no run: %v", err)
	}
	pendingRun(t, s)
	bad := toolResponse()
	bad.Delivered.Attempt = otherAttempt
	if err := s.ToolResponse(ctx, bad); !errors.Is(err, ErrForeignParams) {
		t.Fatalf("tool.response delivering a foreign handle: %v", err)
	}
	bad = toolResponse()
	bad.Call = "cal_01m3250v9xft08ssr4ygq6g2nq"
	if err := s.ToolResponse(ctx, bad); !errors.Is(err, ErrForeignParams) {
		t.Fatalf("tool.response for another call: %v", err)
	}
	if err := s.ToolResponse(ctx, toolResponse()); err != nil {
		t.Fatalf("tool.response of the run: %v", err)
	}
	if s.Err() != nil {
		t.Fatalf("a refused param ended the session: %v", s.Err())
	}
}

// TestASessionPreparesOnce: one accepted prepare, nothing overlapping it; a
// refused one (an error or a non-accepting outcome) leaves the session
// unprepared, with nothing fenced to its stamp.
func TestASessionPreparesOnce(t *testing.T) {
	ctx := context.Background()
	refusedOutcome := `{"outcome":"could_not_run","reason":"executor_incomplete","detail":"no"}`
	s := open(t, adapter(t, script{"prepare": {
		{Lines: []string{errorLine("-32602")}},
		{Lines: []string{resultLine(refusedOutcome)}},
		{DelayMs: 300, Lines: []string{resultLine(cleanOutcome)}},
	}}))
	var e *Error
	if _, err := s.Prepare(ctx, prepareParams()); !errors.As(err, &e) || e.Kind != KindRefused {
		t.Fatalf("a refused prepare: %v", err)
	}
	if _, err := s.Cancel(ctx, fencedParams); !errors.Is(err, ErrSequence) {
		t.Fatalf("cancel after a refused prepare: %v", err)
	}
	if o, err := s.Prepare(ctx, prepareParams()); err != nil || o.Outcome != types.OutcomeCouldNotRun {
		t.Fatalf("a prepare the adapter declines: %+v %v", o, err)
	}
	if _, err := s.Run(ctx, runParams, nil); !errors.Is(err, ErrSequence) {
		t.Fatalf("run after a declined prepare: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.Prepare(ctx, prepareParams()); done <- err }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		s.mu.Lock()
		preparing := s.fence.preparing
		s.mu.Unlock()
		if preparing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the prepare never started")
		}
	}
	if _, err := s.Prepare(ctx, prepareParams()); !errors.Is(err, ErrSequence) {
		t.Fatalf("an overlapping prepare: %v", err)
	}
	if _, err := s.Run(ctx, runParams, nil); !errors.Is(err, ErrSequence) {
		t.Fatalf("a run overlapping prepare: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the accepted prepare: %v", err)
	}
	if _, err := s.Prepare(ctx, prepareParams()); !errors.Is(err, ErrSequence) {
		t.Fatalf("a second prepare: %v", err)
	}
	if s.Err() != nil {
		t.Fatalf("the session ended: %v", s.Err())
	}
}

// TestAToolResponseRacingTheRunsEnd: the run answers right after its
// tool.request. The response is either refused here (the run is over) or
// acknowledged; the session goes on either way, and one after the run's
// reply is always refused here.
func TestAToolResponseRacingTheRunsEnd(t *testing.T) {
	ctx := context.Background()
	request := eventLine(`"event":"tool.request","turn":1,"tool":"RunTest","request":{"attempt":"`+attempt+`","handle":"req","digest":"sha256:`+strings.Repeat("0", 64)+`","size":0}`, ownStamp)
	done := resultLine(`{"outcome":"empty","reason":"completed_clean","detail":"","result":null,"usage_final":null}`)
	s := open(t, adapter(t, script{"run": {{Lines: []string{request, done}}}, "tool.response": {{Lines: []string{resultLine(`{}`)}}}}))
	prepared(t, s, "RunTest")
	answered := make(chan error, 1)
	if _, err := s.Run(ctx, runParams, func(e Event) {
		if e.Name == "tool.request" {
			go func() { answered <- s.ToolResponse(ctx, toolResponse()) }()
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-answered; err != nil && !errors.Is(err, ErrForeignParams) {
		t.Fatalf("the racing tool.response: %v", err)
	}
	if err := s.ToolResponse(ctx, toolResponse()); !errors.Is(err, ErrForeignParams) {
		t.Fatalf("a tool.response after the run: %v", err)
	}
	if s.Err() != nil {
		t.Fatalf("the session ended: %v", s.Err())
	}
}

// TestRejectedRepliesChangeNothing: a run, shutdown or tool.response reply
// its method refuses ends the session and leaves the run open and shutdown
// unmarked.
func TestRejectedRepliesChangeNothing(t *testing.T) {
	ctx := context.Background()
	for _, method := range []string{"run", "shutdown", "tool.response"} {
		sc := script{"run": {{}}, method: {{Lines: []string{resultLine(`{"outcome":"nonsense"}`)}, Exit: method == "shutdown"}}}
		s := open(t, adapter(t, sc))
		prepared(t, s)
		var err error
		switch method {
		case "run":
			_, err = s.Run(ctx, runParams, nil)
		case "shutdown":
			_, err = s.Shutdown(ctx)
		case "tool.response":
			pendingRun(t, s)
			err = s.ToolResponse(ctx, toolResponse())
		}
		wantEnd(t, s, err, KindProtocol, vocab.ReasonAdapterProtocolError)
		s.mu.Lock()
		changed := (method != "shutdown" && s.run == nil) || s.shut
		s.mu.Unlock()
		if changed {
			t.Fatalf("a rejected %s reply changed the lifecycle", method)
		}
	}
}

// adapter is a scripted stub that describes itself as this contract's.
func adapter(t *testing.T, sc script) Config {
	t.Helper()
	if _, ok := sc["describe"]; !ok {
		sc["describe"] = describes("1.0.0")
	}
	return scripted(t, sc)
}

// A refused run result keeps its declaration: a handle with a name the
// schema refuses (for a path refusal), and the final usage of a
// schema-valid result refused for a foreign handle.
func TestARefusedResultIsDeclared(t *testing.T) {
	final := `{"tokens":{"in":9,"out":1,"cache_read":0,"cache_write":0},"turns":1,"milliseconds":5,"cost":{"amount_micros":10,"currency":"USD","basis":"cash","estimate":true}}`
	own := `{"attempt":"` + attempt + `","handle":"../patch.diff","digest":"sha256:` + strings.Repeat("0", 64) + `","size":1}`
	for name, tc := range map[string]struct {
		result, handle string
		final          bool
	}{
		"an invalid path":  {`{"outcome":"found","reason":"completed_with_findings","detail":"a patch","result":{"kind":"patch","handle":` + own + `},"usage_final":` + final + `}`, "../patch.diff", false},
		"a foreign handle": {`{"outcome":"found","reason":"completed_with_findings","detail":"a patch","result":{"kind":"patch","handle":` + foreignHandleJSON + `},"usage_final":` + final + `}`, "x", true},
	} {
		s := open(t, adapter(t, script{"run": {{Lines: []string{resultLine(tc.result)}}}}))
		prepared(t, s)
		if _, err := s.Run(context.Background(), runParams, nil); err == nil {
			t.Fatalf("%s: run accepted", name)
		}
		d, ok := s.Declared()
		if !ok || d.Handle == nil || d.Handle.Handle != tc.handle || (d.UsageFinal != nil) != tc.final {
			t.Fatalf("%s: declared %+v %v", name, d, ok)
		}
	}
}
