// SPDX-License-Identifier: Apache-2.0

//go:build linux

package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The test binary doubles as a scripted adapter: stubEnv names its mode,
// secretEnv is the secret it must be given and tries to leak.
const (
	stubEnv   = "ADC_HARNESS_STUB"
	secretEnv = "ADC_HARNESS_SECRET"
	secret    = "SENTINEL-harness-secret-7f3a"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(stubEnv); mode != "" {
		os.Exit(stub(mode))
	}
	os.Exit(m.Run())
}

const describe = `{"harness":"stub","harness_version":"1.0.0","adapter_version":"0.1.0","contract_version":"1.0.0",` +
	`"capabilities":["patch"],"session_semantics":"fresh_process","tool_surface":[],` +
	`"telemetry":{"usage":true,"upstream":false,"stream_deltas":false,"per_turn_coverage":false},"host_side":false,"needs_native_login":false}`

const usage = `"tokens":{"in":10,"out":1,"cache_read":0,"cache_write":0},"turns":1,"milliseconds":5,"cost":{"amount_micros":11,"currency":"USD","basis":"cash","estimate":true}`

// stub plays mode: hang-describe, hang-prepare, hang-run and
// hang-shutdown never answer that method; leak answers everything, with
// the secret in its stderr and its result's detail (and exits 3 without
// it); linger answers shutdown and stays; credit-hang hints
// credit_exhausted, then never answers run; env exits 3 unless its
// environment is exactly the allowed set; chmod leaves an unreadable
// directory in its TMPDIR.
func stub(mode string) int {
	in := bufio.NewScanner(os.Stdin)
	send := func(line string) { fmt.Println(line) }
	reply := func(id int64, result string) { send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, id, result)) }
	for in.Scan() {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
			Params struct {
				Attempt      string `json:"attempt"`
				FencingToken int64  `json:"fencing_token"`
				Call         string `json:"call"`
			} `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil {
			return 4
		}
		if mode == "hang-"+req.Method {
			continue
		}
		switch req.Method {
		case "describe":
			reply(req.ID, describe)
		case "run":
			s := os.Getenv(secretEnv)
			if mode == "leak" && s != secret {
				return 3
			}
			fmt.Fprintln(os.Stderr, s)
			stamp := fmt.Sprintf(`"attempt":%q,"fencing_token":%d,"call":%q`, req.Params.Attempt, req.Params.FencingToken, req.Params.Call)
			switch mode {
			case "credit-hang":
				h := fmt.Sprintf(`{"attempt":%q,"handle":"x.txt","digest":"sha256:%s","size":1}`, req.Params.Attempt, strings.Repeat("0", 64))
				send(`{"jsonrpc":"2.0","method":"adapter.event","params":{"event":"error","turn":1,"class_hint":"credit_exhausted","raw_excerpt":` + h + `,` + stamp + `}}`)
				continue // run is never answered; shutdown still is
			case "env":
				var names []string
				for _, kv := range os.Environ() {
					if n, _, _ := strings.Cut(kv, "="); n != "PWD" { // bubblewrap sets PWD
						names = append(names, n)
					}
				}
				slices.Sort(names)
				if strings.Join(names, ",") != stubEnv+",PATH,TMPDIR" {
					return 3
				}
			case "chmod":
				d := filepath.Join(os.Getenv("TMPDIR"), "locked", "inner")
				if os.MkdirAll(d, 0o700) != nil || os.WriteFile(filepath.Join(d, "f"), []byte("x"), 0o600) != nil ||
					os.Chmod(d, 0) != nil || os.Chmod(filepath.Dir(d), 0) != nil {
					return 3
				}
			}
			send(`{"jsonrpc":"2.0","method":"adapter.event","params":{"event":"usage","sub_call":"s1","sequence":0,` + usage + `,` + stamp + `}}`)
			reply(req.ID, `{"outcome":"empty","reason":"completed_clean","detail":"`+s+`","result":null,"usage_final":{`+usage+`}}`)
		default:
			reply(req.ID, `{"outcome":"empty","reason":"completed_clean","detail":""}`)
			if req.Method == "shutdown" {
				if mode == "linger" {
					time.Sleep(time.Hour)
				}
				return 0
			}
		}
	}
	return 0
}

// harness runs Main with args and env, returning its exit code, the
// JSON report (when one was printed) and stdout and stderr.
func harness(t *testing.T, env map[string]string, args ...string) (int, Report, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	getenv := func(k string) string {
		if k == "PATH" {
			return os.Getenv("PATH")
		}
		return env[k]
	}
	code := Main(context.Background(), args, &out, &errOut, getenv)
	var r Report
	if bytes.HasPrefix(out.Bytes(), []byte("{")) && json.Unmarshal(out.Bytes(), &r) != nil {
		t.Fatalf("not one JSON report: %q", out.String())
	}
	return code, r, out.String() + errOut.String()
}

func self(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeCommand is the TypeScript fake adapter under node, as make test
// provides it.
func fakeCommand(t *testing.T) []string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH: the fake adapter tests run under make test")
	}
	root, err := filepath.Abs(filepath.Join("..", "..", "adapters", "fake"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "node_modules")); err != nil {
		t.Skip("the adapters workspace is not installed: run make ts-deps (make test does)")
	}
	return []string{node, filepath.Join(root, "src", "main.ts")}
}

// The fake adapter's safe patch is submitted, with its usage recorded.
func TestFakeRunIsSubmitted(t *testing.T) {
	code, r, text := harness(t, map[string]string{"ADC_FAKE_SCENARIO": "safe-output"},
		append([]string{"--json", "--env", "ADC_FAKE_SCENARIO", "--"}, fakeCommand(t)...)...)
	if code != entry.ExitOK || r.Outcome != types.OutcomeFound || r.Submitted == nil || r.Submitted.Blob == nil || len(r.Usage) != 2 {
		t.Fatalf("exit %d, report %+v: %s", code, r, text)
	}
	for _, m := range r.Usage {
		if m.Attempt != r.Attempt || m.SourceOfTruth != types.UsageMeasurementSourceOfTruthAdapterReport {
			t.Fatalf("usage %+v", m)
		}
	}
}

// Each wait passes its bound: could_not_run/attempt_stopped, exit 2,
// naming the wait, and nothing submitted.
func TestEveryWaitIsBounded(t *testing.T) {
	for _, wait := range []string{"describe", "prepare", "run", "shutdown"} {
		t.Run(wait, func(t *testing.T) {
			bounded(t, map[string]string{"describe": "start", "prepare": "handshake", "run": "result", "shutdown": "shutdown"}[wait],
				vocab.ReasonAttemptStopped, map[string]string{stubEnv: "hang-" + wait}, "--env", stubEnv, "--", self(t))
		})
	}
	// Answering shutdown and staying: the tree's exit is within the bound.
	t.Run("linger", func(t *testing.T) {
		bounded(t, "shutdown", vocab.ReasonAttemptStopped, map[string]string{stubEnv: "linger"}, "--env", stubEnv, "--", self(t))
	})
}

// A passed bound keeps the exit map's precedence: a credit hint before the
// hang outranks the stop.
func TestABoundKeepsTheExitMap(t *testing.T) {
	bounded(t, "result", vocab.ReasonBrokerCreditExhausted, map[string]string{stubEnv: "credit-hang"}, "--env", stubEnv, "--", self(t))
}

// The fake adapter's hang scenario streams until it is stopped: the
// result's bound stops it.
func TestFakeHangIsBounded(t *testing.T) {
	bounded(t, "result", vocab.ReasonAttemptStopped, map[string]string{"ADC_FAKE_SCENARIO": "hang"}, append([]string{"--env", "ADC_FAKE_SCENARIO", "--"}, fakeCommand(t)...)...)
}

// bounded runs the harness with only wait's bound short; every other wait
// gets generousBound, so a slow start under load (Node's, say) never
// passes a bound the test is not about.
func bounded(t *testing.T, wait string, reason vocab.Reason, env map[string]string, args ...string) {
	t.Helper()
	flags := []string{"--json"}
	for _, w := range []string{"start", "handshake", "result", "shutdown"} {
		d := generousBound
		if w == wait {
			d = shortBound
		}
		flags = append(flags, "--"+w+"-timeout", d.String())
	}
	began := time.Now()
	code, r, text := harness(t, env, append(flags, args...)...)
	if code != entry.ExitCouldNotRun || r.Outcome != types.OutcomeCouldNotRun || r.Reason != reason.String() ||
		r.Detail != "the adapter's "+wait+" passed its bound" || r.Submitted != nil {
		t.Fatalf("exit %d, report %+v: %s", code, r, text)
	}
	if d := time.Since(began); d >= generousBound {
		t.Fatalf("took %s: the bound did not end the wait", d)
	}
}

// The bound of the wait under test, and every other wait's.
const (
	shortBound    = 500 * time.Millisecond
	generousBound = 30 * time.Second
)

// Nothing the adapter is given or says reaches the output: not its
// environment, its stderr nor its result's detail.
func TestNoSecretIsPrinted(t *testing.T) {
	env := map[string]string{stubEnv: "leak", secretEnv: secret}
	for _, mode := range []string{"--json", "--prompt=" + writePrompt(t, secret)} {
		code, _, text := harness(t, env, mode, "--env", stubEnv, "--env", secretEnv, "--", self(t))
		if code != entry.ExitOK || strings.Contains(text, secret) || !strings.Contains(text, "completed_clean") {
			t.Fatalf("%s: exit %d: %s", mode, code, text)
		}
	}
}

func writePrompt(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "prompt.md")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Bad arguments are usage errors (64), and nothing runs. Every variable but
// ADC_HARNESS_UNSET is set, so a name is refused for itself.
func TestUsageIsRefused(t *testing.T) {
	getenv := func(k string) string {
		if k == "ADC_HARNESS_UNSET" {
			return ""
		}
		return "set"
	}
	big := writePrompt(t, strings.Repeat("x", MaxPrompt+1))
	for name, args := range map[string][]string{
		"no command":       {"--json"},
		"unknown flag":     {"--bogus", "--", "true"},
		"zero start":       {"--start-timeout", "0s", "--", "true"},
		"zero handshake":   {"--handshake-timeout", "0s", "--", "true"},
		"negative result":  {"--result-timeout", "-1s", "--", "true"},
		"zero shutdown":    {"--shutdown-timeout", "0s", "--", "true"},
		"unset env":        {"--env", "ADC_HARNESS_UNSET", "--", "true"},
		"bad env name":     {"--env", "A=B", "--", "true"},
		"env PATH":         {"--env", "PATH", "--", "true"},
		"env TMPDIR":       {"--env", "TMPDIR", "--", "true"},
		"relative ws":      {"--workspace", "ws", "--", "true"},
		"missing prompt":   {"--prompt", filepath.Join(t.TempDir(), "none"), "--", "true"},
		"oversized prompt": {"--prompt", big, "--", "true"},
	} {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := Main(context.Background(), args, &out, &errOut, getenv); code != entry.ExitUsage || out.Len() != 0 {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
			}
		})
	}
}

// The report goes through the CLI's escaping: an adapter-chosen handle
// cannot move or restyle the terminal's text.
func TestReportIsEscaped(t *testing.T) {
	r := Report{Outcome: types.OutcomeFailed, Reason: "unsafe_output", Refusals: []types.AttemptOutputRefusal{{
		Handle: types.AttemptOutputRefusalHandle{Handle: "\x1b[2Jpatch\u202e\u009b.diff"}, Check: types.AttemptOutputRefusalCheckPath}}}
	var human, doc bytes.Buffer
	if err := write(&human, r, false); err != nil {
		t.Fatal(err)
	}
	if err := write(&doc, r, true); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(human.String(), "\x1b\u202e\u009b") || strings.Contains(doc.String(), "\u009b") {
		t.Fatalf("unescaped: %q, %q", human.String(), doc.String())
	}
	if !strings.Contains(human.String(), `\u{001B}[2Jpatch\u{202E}\u{009B}.diff`) || !strings.Contains(doc.String(), `\u009b`) {
		t.Fatalf("human output %q", human.String())
	}
}

// -h prints the synopsis and the flags, exit 0.
func TestHelpShowsTheSynopsis(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Main(context.Background(), []string{"-h"}, &out, &errOut, os.Getenv); code != entry.ExitOK ||
		!strings.Contains(errOut.String(), "usage: adc-harness run [flags] -- COMMAND") || !strings.Contains(errOut.String(), "-result-timeout") {
		t.Fatalf("exit %d: %q", code, errOut.String())
	}
}

// The adapter's environment is exactly PATH, TMPDIR and each --env (and
// bubblewrap's PWD): nothing of the harness's own leaks.
func TestTheAdapterEnvironmentIsStripped(t *testing.T) {
	t.Setenv("ADC_HARNESS_CANARY", "canary")
	if code, r, text := harness(t, map[string]string{stubEnv: "env"}, "--json", "--env", stubEnv, "--", self(t)); code != entry.ExitOK {
		t.Fatalf("exit %d, %+v: %s", code, r, text)
	}
}

// The temporary state is gone after the run, also when the adapter left
// unreadable directories in it.
func TestTheTemporaryStateIsRemoved(t *testing.T) {
	for _, mode := range []string{"env", "chmod"} {
		t.Run(mode, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			code, r, text := harness(t, map[string]string{stubEnv: mode}, "--json", "--env", stubEnv, "--", self(t))
			left, err := filepath.Glob(filepath.Join(tmp, "adc-harness-*"))
			if err != nil || len(left) != 0 || code != entry.ExitOK {
				_ = exec.Command("chmod", "-R", "u+rwx", tmp).Run()
				t.Fatalf("exit %d, %+v, left %v: %s", code, r, left, text)
			}
		})
	}
}

// The session's deadline is the backstop: finite, past every bound.
func TestTheSessionDeadlineIsABackstop(t *testing.T) {
	b := adapterhost.Bounds{Start: time.Second, Handshake: 2 * time.Second, Result: 3 * time.Second, Shutdown: 4 * time.Second}
	if d := sessionDeadline(b); d < 10*time.Second || d > 10*time.Second+time.Minute {
		t.Fatalf("deadline %s for bounds summing to 10s", d)
	}
}

// report's mapping to the exit-code table (08).
func TestReportExitCodes(t *testing.T) {
	sub := func(o types.Outcome, reason vocab.Reason) *types.SubmittedResult {
		return &types.SubmittedResult{Outcome: o, Reason: reason}
	}
	refusal := func(r types.AttemptOutputRefusalReason) []types.AttemptOutputRefusal {
		return []types.AttemptOutputRefusal{{Reason: r, Check: types.AttemptOutputRefusalCheckDigest}}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	timeout := &adapterhost.TimeoutError{Wait: "result"}
	for name, tc := range map[string]struct {
		ctx    context.Context
		out    adapterhost.Outcome
		err    error
		code   int
		reason vocab.Reason
	}{
		"found":                 {nil, adapterhost.Outcome{Submitted: sub(types.OutcomeFound, vocab.ReasonCompletedWithFindings)}, nil, entry.ExitOK, vocab.ReasonCompletedWithFindings},
		"empty":                 {nil, adapterhost.Outcome{Submitted: sub(types.OutcomeEmpty, vocab.ReasonCompletedClean)}, nil, entry.ExitOK, vocab.ReasonCompletedClean},
		"failed":                {nil, adapterhost.Outcome{Submitted: sub(types.OutcomeFailed, vocab.ReasonUnsafeOutput)}, nil, entry.ExitRefused, vocab.ReasonUnsafeOutput},
		"could_not_run":         {nil, adapterhost.Outcome{Submitted: sub(types.OutcomeCouldNotRun, vocab.ReasonProviderUnavailable)}, nil, entry.ExitCouldNotRun, vocab.ReasonProviderUnavailable},
		"submitted, then error": {nil, adapterhost.Outcome{Submitted: sub(types.OutcomeEmpty, vocab.ReasonCompletedClean)}, errors.New("cleanup"), entry.ExitOK, vocab.ReasonCompletedClean},
		"refusal":               {nil, adapterhost.Outcome{Refusals: refusal(types.AttemptOutputRefusalReasonUnsafeOutput)}, nil, entry.ExitRefused, vocab.ReasonUnsafeOutput},
		"stale":                 {nil, adapterhost.Outcome{Refusals: refusal(types.AttemptOutputRefusalReasonStaleAttempt)}, nil, entry.ExitStale, vocab.ReasonStaleAttempt},
		"start refused":         {nil, adapterhost.Outcome{StartRefused: errors.New("no capability")}, nil, entry.ExitCouldNotRun, vocab.ReasonPreconditionFailed},
		"host error":            {nil, adapterhost.Outcome{}, errors.New("bwrap"), entry.ExitCouldNotRun, vocab.ReasonExecutorIncomplete},
		"stopped before start":  {cancelled, adapterhost.Outcome{}, context.Canceled, entry.ExitCouldNotRun, vocab.ReasonAttemptStopped},
		"timeout":               {nil, adapterhost.Outcome{Session: timeout}, nil, entry.ExitCouldNotRun, vocab.ReasonAttemptStopped},
		"timeout, quota hint":   {nil, adapterhost.Outcome{Session: timeout, Hints: []string{"rate_limited"}}, nil, entry.ExitCouldNotRun, vocab.ReasonQuotaExhausted},
		"protocol":              {nil, adapterhost.Outcome{Session: &session.Error{Kind: session.KindProtocol}}, nil, entry.ExitCouldNotRun, vocab.ReasonAdapterProtocolError},
		"deadline backstop":     {nil, adapterhost.Outcome{Session: &session.Error{Kind: session.KindDeadline}}, nil, entry.ExitCouldNotRun, vocab.ReasonAttemptStopped},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := tc.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			r, code := report(ctx, adapterhost.Spec{}, tc.out, tc.err)
			if code != tc.code || r.Reason != tc.reason.String() {
				t.Fatalf("exit %d, reason %s; want %d, %s", code, r.Reason, tc.code, tc.reason)
			}
		})
	}
}

// A harness fault is could_not_run, exit 2, with its fixed text on stderr.
func TestAHarnessFaultIsShown(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	code, r, text := harness(t, nil, "--json", "--", "true")
	if code != entry.ExitCouldNotRun || r.Reason != vocab.ReasonExecutorIncomplete.String() || !strings.Contains(text, "adc-harness run: ") {
		t.Fatalf("exit %d, %+v: %s", code, r, text)
	}
}

// --prompt is read through its bound: a FIFO that never ends is refused once
// it passes MaxPrompt, not read to its end.
func TestThePromptReadIsBounded(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "prompt")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.Write(make([]byte, MaxPrompt+2))
		<-done // the writer stays open: no end of file
	}()
	code := make(chan int, 1)
	go func() {
		var out, errOut bytes.Buffer
		code <- Main(context.Background(), []string{"--prompt", fifo, "--", "true"}, &out, &errOut, os.Getenv)
	}()
	select {
	case c := <-code:
		if c != entry.ExitUsage {
			t.Fatalf("exit %d", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt was read past its bound")
	}
}

// Without the walk (a tree not seen to exit), an unreadable directory is
// left as it is, and the failure names the state.
func TestRemoveStateWalksOnlyWhenTold(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "adc-harness-x")
	locked := filepath.Join(dir, "tmp", "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	if err := removeState(dir, false); err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("without the walk: %v", err)
	}
	if fi, err := os.Lstat(locked); err != nil || fi.Mode().Perm() != 0 {
		t.Fatalf("the locked directory was changed: %v %v", fi, err)
	}
	if err := removeState(dir, true); err != nil {
		t.Fatalf("with the walk: %v", err)
	}
	if _, err := os.Lstat(dir); err == nil {
		t.Fatal("the state is still there")
	}
}
