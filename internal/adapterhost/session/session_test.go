// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session/inproc"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The identities the tests drive an attempt with (as the conformance kit).
const (
	attempt  = "att_01m3250vnfeyrv67x6w0ddqr6a"
	call     = "cal_01m3250v9xft08ssr4ygq6g2np"
	executor = "exe_01m3250vnfeyrv67x6w0ddqr6a"
	token    = 3
)

// step is one scripted answer: lines written after a delay, %ID% replaced
// by the request's id. A step with no lines answers nothing.
type step struct {
	DelayMs int      `json:"delay_ms,omitempty"`
	Lines   []string `json:"lines"`
	Exit    bool     `json:"exit,omitempty"` // exit 0 after writing
}

// script answers each method's n-th request with its n-th step (the last
// one repeating). A method it does not name gets a clean outcome; "*"
// stands for every method it does not name.
type script map[string][]step

const cleanOutcome = `{"outcome":"empty","reason":"completed_clean","detail":""}`

func init() { stubs["script"] = scriptStub }

// scriptStub plays STUB_SCRIPT, answering requests concurrently (so a
// delayed answer lets later ones overtake it); STUB_STDERR goes to stderr
// first.
func scriptStub() {
	var sc script
	if json.Unmarshal([]byte(os.Getenv("STUB_SCRIPT")), &sc) != nil {
		os.Exit(4)
	}
	os.Stderr.WriteString(os.Getenv("STUB_STDERR"))
	var mu sync.Mutex
	seen := map[string]int{}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(nil, 1<<20)
	for in.Scan() {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil {
			os.Exit(4)
		}
		steps, ok := sc[req.Method]
		if !ok {
			steps, ok = sc["*"]
		}
		if !ok {
			steps = []step{{Lines: []string{`{"jsonrpc":"2.0","id":%ID%,"result":` + cleanOutcome + `}`}}}
		}
		mu.Lock()
		st := steps[min(seen[req.Method], len(steps)-1)]
		seen[req.Method]++
		mu.Unlock()
		go func() {
			time.Sleep(time.Duration(st.DelayMs) * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			for _, l := range st.Lines {
				os.Stdout.WriteString(strings.ReplaceAll(l, "%ID%", strconv.FormatInt(req.ID, 10)) + "\n")
			}
			if st.Exit {
				os.Exit(0)
			}
		}()
	}
	time.Sleep(time.Hour) // the core ends it
}

func scripted(t *testing.T, sc script, env ...string) Config {
	t.Helper()
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	return stubConfig(t, "script", append([]string{"STUB_SCRIPT=" + string(b)}, env...)...)
}

func stubConfig(t *testing.T, mode string, env ...string) Config {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Config{Command: []string{self}, Env: append(append([]string{"PATH=" + os.Getenv("PATH"), stubEnv + "=" + mode}, testguard.HomeEnv(t)...), env...), Deadline: 30 * time.Second}
}

func open(t *testing.T, cfg Config) *Session {
	t.Helper()
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// line formats one message; result is a raw JSON value.
func resultLine(result string) string { return `{"jsonrpc":"2.0","id":%ID%,"result":` + result + `}` }
func errorLine(code string) string {
	return `{"jsonrpc":"2.0","id":%ID%,"error":{"code":` + code + `,"message":"no"}}`
}
func eventLine(body string, stamp string) string {
	return `{"jsonrpc":"2.0","method":"adapter.event","params":{` + body + `,` + stamp + `}}`
}

var ownStamp = `"attempt":"` + attempt + `","fencing_token":3,"call":"` + call + `"`

// object is a check accepting any JSON object, returned as its bytes.
func object(raw json.RawMessage) (any, *Error) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, protocol("not an object")
	}
	return string(raw), nil
}

func plain(s *Session, ctx context.Context, method string) (any, error) {
	return s.call(ctx, method, "", nil, nil, expect{check: object})
}

func runCall(s *Session, ctx context.Context) (any, error) {
	return s.call(ctx, "run", "", nil, &run{want: stamp{Attempt: attempt, FencingToken: token, Call: call}}, expect{check: object})
}

// wantEnd checks a call's error and the session's end are the kind and reason.
func wantEnd(t *testing.T, s *Session, err error, kind Kind, reason vocab.Reason) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Kind != kind || e.Reason() != reason {
		t.Fatalf("got %v, want a %s error with reason %s", err, kind, reason)
	}
	if !errors.As(s.Err(), &e) || e.Kind != kind {
		t.Fatalf("the session ended with %v, want %s", s.Err(), kind)
	}
}

// ended waits for the session to end on its own.
func ended(t *testing.T, s *Session) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); s.Err() == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the session did not end")
		}
	}
}

func TestRepliesAreCorrelated(t *testing.T) {
	s := open(t, scripted(t, script{
		"slow": {{DelayMs: 200, Lines: []string{resultLine(`{"m":"slow"}`)}}},
		"fast": {{Lines: []string{resultLine(`{"m":"fast"}`)}}},
	}))
	var wg sync.WaitGroup
	for i := range 8 {
		method := []string{"slow", "fast"}[i%2]
		wg.Go(func() {
			v, err := plain(s, context.Background(), method)
			if err != nil || v != `{"m":"`+method+`"}` {
				t.Errorf("%s got %v, %v", method, v, err)
			}
		})
	}
	wg.Wait()
}

func TestARefusalKeepsTheSession(t *testing.T) {
	s := open(t, scripted(t, script{"x": {{Lines: []string{errorLine("-32601")}}}}))
	_, err := plain(s, context.Background(), "x")
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindRefused || e.Code != -32601 || s.Err() != nil {
		t.Fatalf("refusal: %#v, session %v", err, s.Err())
	}
	if _, err := plain(s, context.Background(), "y"); err != nil {
		t.Fatalf("the session after a refusal: %v", err)
	}
}

func TestOnlyReservedRPCCodesAreKept(t *testing.T) {
	for code, want := range map[int64]int64{-32768: -32768, -32000: -32000, -32603: -32603, -32769: 0, -31999: 0, 0: 0, 7123456789012345: 0} {
		if got := rpcCode(code); got != want {
			t.Errorf("rpcCode(%d) = %d, want %d", code, got, want)
		}
	}
}

func TestABrokenLineEndsTheSession(t *testing.T) {
	s := open(t, scripted(t, script{"x": {{Lines: []string{"this is not JSON"}}}}))
	_, err := plain(s, context.Background(), "x")
	wantEnd(t, s, err, KindProtocol, vocab.ReasonAdapterProtocolError)
}

func TestAReplyNotPendingEndsTheSession(t *testing.T) {
	s := open(t, scripted(t, script{"x": {{Lines: []string{`{"jsonrpc":"2.0","id":999,"result":{}}`}}}}))
	_, err := plain(s, context.Background(), "x")
	wantEnd(t, s, err, KindProtocol, vocab.ReasonAdapterProtocolError)
}

// TestALateReplyIsStillChecked: a caller that gave up does not exempt its
// reply from its check.
func TestALateReplyIsStillChecked(t *testing.T) {
	s := open(t, scripted(t, script{"x": {{DelayMs: 300, Lines: []string{resultLine(`[]`)}}}}))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := plain(s, ctx, "x"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the abandoned call returned %v", err)
	}
	ended(t, s)
	wantEnd(t, s, s.Err(), KindProtocol, vocab.ReasonAdapterProtocolError)
}

// TestARejectedReplyChangesNothing: a run or shutdown reply its check
// rejects ends the session without closing the run or marking shutdown.
func TestARejectedReplyChangesNothing(t *testing.T) {
	for _, method := range []string{"run", "shutdown"} {
		s := open(t, scripted(t, script{method: {{Lines: []string{resultLine(`[]`)}, Exit: true}}}))
		var err error
		if method == "run" {
			_, err = runCall(s, context.Background())
		} else {
			_, err = plain(s, context.Background(), method)
		}
		wantEnd(t, s, err, KindProtocol, vocab.ReasonAdapterProtocolError)
		s.mu.Lock()
		changed := (method == "run" && s.run == nil) || s.shut
		s.mu.Unlock()
		if changed {
			t.Fatalf("a rejected %s reply changed the lifecycle", method)
		}
	}
}

func TestHeartbeatSilenceEndsTheRun(t *testing.T) {
	cfg := scripted(t, script{"run": {{}}})
	cfg.Heartbeat = 300 * time.Millisecond
	s := open(t, cfg)
	started := time.Now()
	_, err := runCall(s, context.Background())
	wantEnd(t, s, err, KindHeartbeat, vocab.ReasonAdapterProtocolError)
	if d := time.Since(started); d > 5*time.Second {
		t.Fatalf("silence was noticed after %s", d)
	}
}

func TestHeartbeatsKeepTheRunAlive(t *testing.T) {
	beat := eventLine(`"event":"heartbeat"`, ownStamp)
	cfg := scripted(t, script{"run": {{Lines: []string{beat}}}, "beat": {{Lines: []string{beat, resultLine(`{}`)}}}})
	cfg.Heartbeat = 400 * time.Millisecond
	s := open(t, cfg)
	done := make(chan error, 1)
	go func() { _, err := runCall(s, context.Background()); done <- err }()
	for range 6 {
		time.Sleep(150 * time.Millisecond)
		if _, err := plain(s, context.Background(), "beat"); err != nil {
			t.Fatalf("beat: %v", err)
		}
	}
	if s.Err() != nil {
		t.Fatalf("a run with heartbeats ended: %v", s.Err())
	}
}

func TestEventsAreCheckedAgainstTheRun(t *testing.T) {
	cases := map[string]script{
		"stamped for another token": {"run": {{Lines: []string{eventLine(`"event":"heartbeat"`, strings.Replace(ownStamp, `"fencing_token":3`, `"fencing_token":4`, 1))}}}},
		"not an adapter event":      {"run": {{Lines: []string{eventLine(`"event":"nope"`, ownStamp)}}}},
		"outside a run":             {"x": {{Lines: []string{eventLine(`"event":"heartbeat"`, ownStamp)}}}},
	}
	for name, sc := range cases {
		s := open(t, scripted(t, sc))
		var err error
		if _, isRun := sc["run"]; isRun {
			_, err = runCall(s, context.Background())
		} else {
			_, err = plain(s, context.Background(), "x")
		}
		var e *Error
		if !errors.As(err, &e) || e.Kind != KindProtocol {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestShutdownIsTheCleanEnd(t *testing.T) {
	s := open(t, scripted(t, script{"shutdown": {{Lines: []string{resultLine(cleanOutcome)}, Exit: true}}}))
	if _, err := plain(s, context.Background(), "shutdown"); err != nil {
		t.Fatal(err)
	}
	if err := s.Wait(); err != nil {
		t.Fatalf("exit after shutdown: %v", err)
	}
	ended(t, s)
	wantEnd(t, s, s.Err(), KindClosed, vocab.ReasonAttemptStopped)
	if _, err := plain(s, context.Background(), "x"); err == nil {
		t.Fatal("a call after the end succeeded")
	}
}

func TestCloseKillsTheWholeTree(t *testing.T) {
	marker := newMarker()
	s := open(t, stubConfig(t, "tree", "STUB_MARKER="+marker, "STUB_QUIET=1"))
	for deadline := time.Now().Add(10 * time.Second); !running(marker); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the stub's descendant never started")
		}
	}
	_ = s.Close()
	if running(marker) { // Close returns once the namespace is empty
		t.Fatal("a descendant in a session of its own survived Close")
	}
	wantEnd(t, s, s.Err(), KindClosed, vocab.ReasonAttemptStopped)
}

// leaks fails if any rendering of err (its string and every exported
// field) holds one of the secrets.
func leaks(t *testing.T, what string, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error", what)
	}
	for _, text := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
		for _, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Fatalf("%s: the error leaks a secret: %s", what, text)
			}
		}
	}
}

// The secrets an adapter may have been given: a gateway token, and a
// numeric one that fits the wire's integers.
const (
	secret        = "sk-fake-0123456789abcdef"
	numericSecret = "7123456789012345"
)

// TestNoSecretLeaksIntoErrors: whatever the adapter writes, no returned
// error carries it.
func TestNoSecretLeaksIntoErrors(t *testing.T) {
	cases := map[string]string{
		"a broken line":   "not JSON " + secret,
		"a duplicate key": resultLine(`{"` + secret + `":1,"` + secret + `":1}`),
		"an RPC message":  `{"jsonrpc":"2.0","id":%ID%,"error":{"code":-32603,"message":"` + secret + `"}}`,
		"a reply's id":    `{"jsonrpc":"2.0","id":` + numericSecret + `,"result":{}}`,
		"an RPC code":     errorLine(numericSecret),
		"a bad event":     eventLine(`"event":"`+secret+`"`, ownStamp),
		"a bad stamp":     eventLine(`"event":"heartbeat"`, `"attempt":"`+secret+`","fencing_token":`+numericSecret+`,"call":"`+call+`"`),
	}
	for what, line := range cases {
		s := open(t, scripted(t, script{"run": {{Lines: []string{line}}}}, "STUB_STDERR="+secret))
		_, err := runCall(s, context.Background())
		leaks(t, what, err, secret, numericSecret)
		if s.Err() != nil {
			leaks(t, what+" (session)", s.Err(), secret, numericSecret)
		}
		_ = s.Close()
		if !strings.Contains(s.Stderr(), secret) {
			t.Fatalf("%s: the stderr accessor lost what the adapter wrote", what)
		}
	}
	// Params their schema refuses: the encode error never quotes them.
	s := open(t, scripted(t, script{}))
	_, err := s.call(context.Background(), "prepare", "adapter/prepare-params", map[string]string{"token": secret}, nil, expect{check: object})
	leaks(t, "params", err, secret)
}

// A peer in this process is a session's adapter over pipes: its replies are
// checked like a spawned one's, and closing the session (or its deadline)
// closes the peer's pipes, so Wait returns once the peer has returned.
func TestAPeerIsTheAdapterUntilClosed(t *testing.T) {
	for _, deadline := range []time.Duration{30 * time.Second, 50 * time.Millisecond} {
		returned := make(chan struct{})
		s := open(t, Config{Deadline: deadline, InProcess: inproc.New(func(in io.Reader, out io.Writer) {
			defer close(returned)
			r := bufio.NewScanner(in)
			for r.Scan() { // answers one request, then only reads
				var req struct{ ID int64 }
				if json.Unmarshal(r.Bytes(), &req) != nil {
					return
				}
				fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%d,"result":%s}`+"\n", req.ID, fmt.Sprintf(describeResult, "1.0.0"))
				_, _ = io.Copy(io.Discard, in)
				return
			}
		})})
		if _, err := s.Describe(context.Background()); err != nil {
			t.Fatalf("describe: %v", err)
		}
		if deadline < time.Second {
			<-s.ended
			if e, ok := s.Err().(*Error); !ok || e.Kind != KindDeadline {
				t.Fatalf("ended with %v, want the deadline", s.Err())
			}
		}
		_ = s.Close()
		select {
		case <-returned:
		default:
			t.Fatal("Wait returned before the peer did")
		}
	}
}
