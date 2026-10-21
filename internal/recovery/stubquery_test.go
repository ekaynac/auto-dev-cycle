// SPDX-License-Identifier: Apache-2.0

//go:build linux

package recovery_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
)

// stubQuery is X3's usage query (#226 entry 121), test code only: the stub provider's per-call usage
// (fixtures/m1/stub-provider: GET {base}/usage?call=ID), which sums the requests the gateway stamped with the call
// (gateway.CallHeader). It honours ctx (the request carries it, and Query requires it), reads a bounded answer, and
// fails closed: a 404, or a request of the call still served (pending), is no usage found; any other status, an
// answer past maxAnswer, malformed, or for another call is an error. Either keeps the call's liability. No
// production route has a usage query: real providers ignore the header.
type stubQuery struct{ base string }

// maxAnswer bounds a usage answer's bytes.
const maxAnswer = 4 << 10

func (q stubQuery) Usage(ctx context.Context, call types.ModelCall) (modelcall.Usage, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.base+"/usage?call="+url.QueryEscape(string(call.Call)), nil)
	if err != nil {
		return modelcall.Usage{}, false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return modelcall.Usage{}, false, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	switch {
	case err != nil:
		return modelcall.Usage{}, false, err
	case resp.StatusCode == http.StatusNotFound:
		return modelcall.Usage{}, false, nil
	case resp.StatusCode != http.StatusOK:
		return modelcall.Usage{}, false, fmt.Errorf("stub usage query: status %d", resp.StatusCode)
	case len(body) > maxAnswer:
		return modelcall.Usage{}, false, fmt.Errorf("stub usage query: an answer past %d bytes", maxAnswer)
	}
	var a struct {
		Call     string `json:"call"`
		Requests int64  `json:"requests"`
		Pending  *int64 `json:"pending"`
		Usage    *struct {
			Prompt     *int64 `json:"prompt_tokens"`
			Completion *int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil || a.Call != string(call.Call) || a.Requests < 1 || a.Pending == nil || *a.Pending < 0 || a.Usage == nil ||
		a.Usage.Prompt == nil || a.Usage.Completion == nil || *a.Usage.Prompt < 0 || *a.Usage.Completion < 0 {
		return modelcall.Usage{}, false, fmt.Errorf("stub usage query: a malformed answer for %s: %.200q (%v)", call.Call, body, err)
	}
	if *a.Pending > 0 {
		return modelcall.Usage{}, false, nil // a request still served: the sum is not yet the call's
	}
	return modelcall.Usage{In: *a.Usage.Prompt, Out: *a.Usage.Completion}, true, nil
}

// F (#226 entry 121): the stub query fails closed. Each answer that is not a complete usage of the call is no usage
// found or an error, never a figure.
func TestStubQueryFailsClosed(t *testing.T) {
	call := types.ModelCall{Call: "cal_01m3250vm7f6ktw39dszqnydd0"}
	for name, c := range map[string]struct {
		status int
		body   string
		found  bool
		err    bool
	}{
		"the call's usage":     {200, `{"call":"cal_01m3250vm7f6ktw39dszqnydd0","requests":2,"pending":0,"usage":{"prompt_tokens":7,"completion_tokens":3}}`, true, false},
		"no such call":         {404, `{"error":{"message":"no such call"}}`, false, false},
		"a request still held": {200, `{"call":"cal_01m3250vm7f6ktw39dszqnydd0","requests":2,"pending":1,"usage":{"prompt_tokens":7,"completion_tokens":3}}`, false, false},
		"another call's":       {200, `{"call":"cal_other","requests":1,"pending":0,"usage":{"prompt_tokens":7,"completion_tokens":3}}`, false, true},
		"no pending count":     {200, `{"call":"cal_01m3250vm7f6ktw39dszqnydd0","requests":1,"usage":{"prompt_tokens":7,"completion_tokens":3}}`, false, true},
		"a negative figure":    {200, `{"call":"cal_01m3250vm7f6ktw39dszqnydd0","requests":1,"pending":0,"usage":{"prompt_tokens":-7,"completion_tokens":3}}`, false, true},
		"an unknown field":     {200, `{"call":"cal_01m3250vm7f6ktw39dszqnydd0","requests":1,"pending":0,"usage":{"prompt_tokens":7,"completion_tokens":3},"cost":1}`, false, true},
		"a server error":       {500, `{}`, false, true},
		"an answer too long":   {200, `{"call":"cal_01m3250vm7f6ktw39dszqnydd0","requests":1,"pending":0,"usage":{"prompt_tokens":7,"completion_tokens":3}}` + strings.Repeat(" ", maxAnswer), false, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/usage" || r.URL.Query().Get("call") != string(call.Call) {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, c.body)
		}))
		u, found, err := stubQuery{srv.URL + "/v1"}.Usage(context.Background(), call)
		srv.Close()
		if found != c.found || (err != nil) != c.err || found && u != (modelcall.Usage{In: 7, Out: 3}) {
			t.Errorf("%s: %+v, found %v, %v", name, u, found, err)
		}
	}
}

// F (#226 entries 78, 121): the stub query honours cancellation. A provider that never answers holds the query only
// until its context ends: the query returns the context's error, ordered after the cancel, not by a clock.
func TestStubQueryHonoursCancellation(t *testing.T) {
	asked, hold := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(asked)
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(hold)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := stubQuery{srv.URL + "/v1"}.Usage(ctx, types.ModelCall{Call: "cal_01m3250vm7f6ktw39dszqnydd0"})
		done <- err
	}()
	<-asked
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled query returned %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("a cancelled query did not return")
	}
}

// stubProvider runs fixtures/m1/stub-provider/server.mjs on scenario: its base URL, its barrier directory and the
// request ids it holds at a barrier, as it logs them.
func stubProvider(t *testing.T, scenario string) (base, barriers string, held <-chan string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH: the stub provider's rows run under make test")
	}
	server, err := filepath.Abs("../../fixtures/m1/stub-provider/server.mjs")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "scenario.json")
	if err := os.WriteFile(file, []byte(scenario), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, server, "--scenario", file, "--listen", "127.0.0.1:0", "--barriers", dir)
	out, oerr := cmd.StdoutPipe()
	errs, eerr := cmd.StderrPipe()
	if err := errors.Join(oerr, eerr, cmd.Start()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	base, ok := strings.CutPrefix(strings.TrimSpace(line), "listening ")
	if err != nil || !ok {
		t.Fatalf("the stub provider printed %q: %v", line, err)
	}
	ids := make(chan string, 8)
	go func() {
		for s := bufio.NewScanner(errs); s.Scan(); {
			if id, ok := strings.CutPrefix(s.Text(), "barrier "); ok {
				ids <- id
			}
		}
	}()
	return base, dir, ids
}

// cutRequest sends c's request as the gateway did before the kill (its call stamped: TestEveryRequestCarriesItsCall is
// the gateway's own row), waits until the stub holds it at its barrier, and then cuts it, as the kill cut it at the
// gateway.
func cutRequest(t *testing.T, base string, held <-chan string, c cut) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions",
		strings.NewReader(`{"model":"stub-builder-1","messages":[{"role":"user","content":"build"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(gateway.CallHeader, string(c.Call))
	sent := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			err = fmt.Errorf("the held request was answered %d", resp.StatusCode)
		}
		sent <- err
	}()
	select {
	case <-held:
	case <-time.After(time.Minute):
		t.Fatal("the stub never held the request")
	}
	cancel()
	if err := <-sent; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cut request: %v", err)
	}
}

// released releases the stub's barrier and waits, bounded, until the stub's answer for c's call is complete (the held
// request ended, its usage set), or not found.
func released(t *testing.T, q stubQuery, barriers string, c cut) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(barriers, "x3.release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
		resp, err := http.Get(q.base + "/usage?call=" + url.QueryEscape(string(c.Call)))
		var a struct{ Pending int64 }
		if err == nil {
			err = json.NewDecoder(resp.Body).Decode(&a)
			resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound || err == nil && a.Pending == 0 {
				return
			}
		}
		if time.Now().After(end) {
			t.Fatalf("the released request never ended: %v", err)
		}
	}
}

// laterThanGrace is a clock past every floor's grace (FloorGrace), as P24's driver waits past it.
func laterThanGrace() time.Time { return time.Now().Add(modelcall.FloorGrace + time.Minute) }

// effect is c's call's effect.
func (w *world) effect(c cut) eventstore.Effect {
	w.t.Helper()
	e, err := w.s.LoadEffect(context.Background(), modelcall.EffectOf(c.Call))
	if err != nil {
		w.t.Fatal(err)
	}
	return e
}

// X3 (00-slice-plan.md §2.4), both variants, in process: kill -9 mid-call (TestKillMidCall's controller: the cut
// request's floor of 4/64 and two deltas of 350/200 durable), its request held at the stub provider's barrier and cut.
// After the restart and recovery's steps 1-4, the call is a liability of 550 and its effect uncertain.
//   - kill-mid-build: while the cut request is still held, the stub's answer is incomplete, and the effect stays
//     uncertain with its liability. Released, the stub serves its usage, 400/220, once; the reconciler, past
//     FloorGrace, records it as the call's provider reconciliation, and the effect is completed. Asked again, it
//     records nothing more (Opus #366 L4).
//   - kill-mid-build-noquery (the stub's usage query disabled): the effect stays uncertain and the liability is kept.
func TestKillMidBuildReconciles(t *testing.T) {
	for _, v := range []struct {
		name  string
		query bool
	}{{"kill-mid-build", true}, {"kill-mid-build-noquery", false}} {
		t.Run(v.name, func(t *testing.T) {
			base, barriers, held := stubProvider(t, fmt.Sprintf(`{"name":%q,"usage_query":%v,"calls":{"stub-builder-1":{"persistent":`+
				`{"say":"partial","barrier":"x3","usage":{"prompt_tokens":400,"completion_tokens":220}}}}}`, v.name, v.query))
			dir, c := killMidCall(t)
			cutRequest(t, base, held, c)
			w := restarted(t, dir)
			w.run()
			q := stubQuery{base}
			rc := &modelcall.Reconciler{Store: w.s, Actor: string(actor), Queries: map[types.Slug]modelcall.Query{"stub": q}, Now: laterThanGrace}
			if e, err := rc.Reconcile(context.Background(), c.Call); err != nil || e.State != eventstore.EffectUncertain || e.LastObservation != modelcall.ObservedNotFound {
				t.Fatalf("while the cut request is held: %+v, %v; want uncertain, not found", e, err)
			}
			released(t, q, barriers, c)
			for round := 1; round <= 2; round++ { // the second: the answer counted once, the state kept
				e, err := rc.Reconcile(context.Background(), c.Call)
				liable, _ := w.liable()
				var ms []types.UsageMeasurement
				w.view(func(tx *eventstore.Tx) (err error) { ms, err = ledger.Measurements(tx, c.Call); return err })
				var answered []types.UsageMeasurementTokens
				for _, m := range ms {
					if m.Kind == types.UsageMeasurementKindProviderReconciliation {
						answered = append(answered, m.Tokens)
					}
				}
				switch {
				case err != nil:
					t.Fatalf("reconciliation %d: %v", round, err)
				case v.query && (e.State != eventstore.EffectCompleted || round == 1 && e.LastObservation != modelcall.ObservedQueried || len(answered) != 1 ||
					answered[0] != types.UsageMeasurementTokens{In: 400, Out: 220} || !ledger.StandingOf(ms, c.Call).Settled):
					t.Fatalf("kill-mid-build, reconciliation %d: %+v, answered %+v; want completed by the stub's 400/220, once", round, e, answered)
				case !v.query && (e.State != eventstore.EffectUncertain || e.LastObservation != modelcall.ObservedNotFound || len(answered) != 0 || liable != 550):
					t.Fatalf("kill-mid-build-noquery, reconciliation %d: %+v, answered %+v, liable %d; want uncertain, the liability of 550 kept", round, e, answered, liable)
				}
				if got := w.effect(c); got.State != e.State {
					t.Fatalf("the effect is %+v after reconciliation %d returned %+v", got, round, e)
				}
			}
		})
	}
}
