// SPDX-License-Identifier: Apache-2.0

//go:build linux

package adapterhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/ingest"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

const att = string(attID)

// ended checks that the attempt is finalised whatever happened: it no
// longer publishes, and the usage it reported is durable.
func ended(t *testing.T, w *world, usage int) types.Attempt {
	t.Helper()
	a := w.attempt()
	if a.Publication != types.AttemptPublicationClosed || a.Status == types.AttemptStatusRunning {
		t.Fatalf("attempt %s, publication %s", a.Status, a.Publication)
	}
	if n := len(w.usage()); n != usage {
		t.Fatalf("%d measurements recorded, want %d", n, usage)
	}
	return a
}

// onlyRefusal checks the run left exactly one refusal, of check on handle
// name, and no submission.
func onlyRefusal(t *testing.T, w *world, out Outcome, name string, check types.AttemptOutputRefusalCheck) {
	t.Helper()
	if len(out.Refusals) != 1 || out.Refusals[0].Check != check || out.Refusals[0].Handle.Handle != name || out.Submitted != nil {
		t.Fatalf("refusals %+v, submitted %v; want one %s refusal of %s (session %v)", out.Refusals, out.Submitted, check, name, out.Session)
	}
	if !w.untrustworthy() {
		t.Fatal("not untrustworthy after a refusal")
	}
}

// Astra #108 finding 3: every final output events declare is checked after
// exit; one unsafe excerpt refuses the run even with a safe result.
func TestEventDeclaredOutputsAreChecked(t *testing.T) {
	w := newWorld(t)
	excerpt := handleJSON(att, "excerpt.txt", "outside")
	out, err := w.host.Run(context.Background(), spec(stub(t, stubPlan{
		Files:  map[string]string{"patch.diff": patch},
		Links:  map[string]string{"excerpt.txt": "outside"},
		Events: []string{`"event":"error","turn":1,"class_hint":"other","raw_excerpt":` + excerpt, usageEvent(10), `"event":"error","turn":1,"class_hint":"other","raw_excerpt":` + excerpt},
		Result: resultJSON("patch", handleJSON(att, "patch.diff", patch), 10),
	})))
	if err != nil {
		t.Fatal(err)
	}
	onlyRefusal(t, w, out, "excerpt.txt", types.AttemptOutputRefusalCheckSymlink)
	ended(t, w, 2)
	if w.tmpFiles() != 0 {
		t.Fatal("a temporary copy stayed beside a refused excerpt")
	}
}

// The fake's error scenarios declare excerpts they never write: unreadable.
func TestAMissingExcerptIsUnreadable(t *testing.T) {
	w := newWorld(t)
	out, err := w.host.Run(context.Background(), spec(fake(t, "http-5xx")))
	if err != nil {
		t.Fatal(err)
	}
	onlyRefusal(t, w, out, "error-http_5xx.txt", types.AttemptOutputRefusalCheckUnreadable)
	ended(t, w, 1)
}

// A tool notification this host does not serve refuses the run (fail
// closed): a mediated tool request, or a tool result naming a delivery.
func TestUnservedToolNotificationsRefuseTheRun(t *testing.T) {
	t.Run("tool.request", func(t *testing.T) {
		w := newWorld(t)
		sp := spec(fake(t, "tool"))
		sp.Prepare.ToolPolicy.Mediated = []string{"RunTest"}
		out, err := w.host.Run(context.Background(), sp)
		if err != nil || !errors.Is(out.Session, ErrUnservedEvent) || out.Submitted != nil {
			t.Fatalf("run: %v, session %v, submitted %v", err, out.Session, out.Submitted)
		}
		if a := ended(t, w, 0); a.Status != types.AttemptStatusLost {
			t.Fatalf("attempt %s", a.Status)
		}
	})
	t.Run("tool.result", func(t *testing.T) {
		w := newWorld(t)
		out, err := w.host.Run(context.Background(), spec(stub(t, stubPlan{
			Events: []string{`"event":"tool.result","turn":1,"tool":"RunTest","mediated_by_controller":true,"delivered":` + handleJSON(att, "answer", "x") + `,"bytes":1`},
		})))
		if err != nil || !errors.Is(out.Session, ErrUnservedEvent) || out.Submitted != nil {
			t.Fatalf("run: %v, session %v, submitted %v", err, out.Session, out.Submitted)
		}
		ended(t, w, 0)
	})
}

// Astra #108 finding 6: a result handle the schema refuses still yields its
// refusal, in 09's order: the fence, then the foreign handle, then the path.
func TestInvalidPathsAreRefused(t *testing.T) {
	other := att[:len(att)-1] + "b"
	for name, tc := range map[string]struct {
		attempt string
		stale   bool
		check   types.AttemptOutputRefusalCheck
	}{
		"path":         {att, false, types.AttemptOutputRefusalCheckPath},
		"stale first":  {att, true, types.AttemptOutputRefusalCheckStaleAttempt},
		"foreign then": {other, false, types.AttemptOutputRefusalCheckForeignHandle},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			if tc.stale {
				testHook = func(s string) {
					if s == "exited" {
						w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
							return lease.ChangeAttempt(tx, attID, 1, lease.AttemptEvent{Kind: lease.StopRequested, StopReason: types.AttemptStopReasonValueRevoke})
						})
					}
				}
				t.Cleanup(func() { testHook = nil })
			}
			out, err := w.host.Run(context.Background(), spec(stub(t, stubPlan{
				Events: []string{usageEvent(10)},
				Result: resultJSON("patch", handleJSON(tc.attempt, "../patch.diff", patch), 10),
			})))
			if err != nil {
				t.Fatal(err)
			}
			onlyRefusal(t, w, out, "../patch.diff", tc.check)
			ended(t, w, 1) // a result that is not schema-valid has no final
		})
	}
}

// Size bounds by kind, and a declared output that is missing.
func TestSizeAndUnreadableAreRefused(t *testing.T) {
	big := fmt.Sprintf(`{"attempt":%q,"handle":"d.json","digest":"sha256:%s","size":%d}`, att, strings.Repeat("0", 64), BoundDecision+1)
	for name, tc := range map[string]struct {
		result string
		file   string
		check  types.AttemptOutputRefusalCheck
	}{
		"a decision over its bound": {resultJSON("decision", big, 1), "d.json", types.AttemptOutputRefusalCheckSize},
		"a missing patch":           {resultJSON("patch", handleJSON(att, "patch.diff", patch), 1), "patch.diff", types.AttemptOutputRefusalCheckUnreadable},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			out, err := w.host.Run(context.Background(), spec(stub(t, stubPlan{Result: tc.result})))
			if err != nil {
				t.Fatal(err)
			}
			onlyRefusal(t, w, out, tc.file, tc.check)
			ended(t, w, 1)
		})
	}
}

// Astra #108 finding 7: a result refused for a foreign handle still has its
// final usage recorded, here above its deltas.
func TestAForeignResultKeepsItsFinal(t *testing.T) {
	w := newWorld(t)
	other := att[:len(att)-1] + "b"
	out, err := w.host.Run(context.Background(), spec(stub(t, stubPlan{
		Files:  map[string]string{"patch.diff": patch},
		Events: []string{usageEvent(10)},
		Result: resultJSON("patch", handleJSON(other, "patch.diff", patch), 500),
	})))
	if err != nil {
		t.Fatal(err)
	}
	onlyRefusal(t, w, out, "patch.diff", types.AttemptOutputRefusalCheckForeignHandle)
	ended(t, w, 2)
	calls, err := ledger.Reconcile(w.usage())
	if err != nil || calls[calID].Usage.In != 500 || calls[calID].Uncertain {
		t.Fatalf("reconciled %+v %v", calls[calID], err)
	}
}

// A descendant of the adapter that would write after the shutdown reply:
// once the process tree is seen to exit (ingestion's precondition), nothing
// of it writes any more. Whatever it managed before is what ingestion sees.
func TestNothingWritesAfterTheTreeExits(t *testing.T) {
	w := newWorld(t)
	marker := filepath.Join(t.TempDir(), "late-wrote")
	var atExit bool
	testHook = func(s string) {
		if s == "exited" {
			_, err := os.Stat(marker)
			atExit = err == nil
		}
	}
	t.Cleanup(func() { testHook = nil })
	out, err := w.host.Run(context.Background(), spec(stub(t, stubPlan{
		Files:  map[string]string{"patch.diff": patch},
		Result: resultJSON("patch", handleJSON(att, "patch.diff", patch), 1),
		Late:   "patch.diff",
		Marker: marker,
	})))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second) // the descendant's write would have come by now
	if _, err := os.Stat(marker); (err == nil) != atExit {
		t.Fatal("a descendant wrote after the process tree was seen to exit")
	}
	if atExit {
		onlyRefusal(t, w, out, "patch.diff", types.AttemptOutputRefusalCheckSize)
	} else if out.Submitted == nil || !w.published(patch) {
		t.Fatalf("the untouched patch was not admitted: %+v", out)
	}
}

// Astra #108 finding 2: the caller's cancellation, at any point after the
// start, still finalises the attempt under the controller's own context.
func TestCancellationStillFinalises(t *testing.T) {
	for _, tc := range []struct {
		at        string
		usage     int
		submitted bool
	}{{"describe", 0, false}, {"prepare", 0, false}, {"event:usage", 1, false}, {"ingested", 2, true}} {
		t.Run(tc.at, func(t *testing.T) {
			w := newWorld(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			testHook = func(s string) {
				if s == tc.at {
					cancel()
				}
			}
			t.Cleanup(func() { testHook = nil })
			plan := stubPlan{Files: map[string]string{"patch.diff": patch}, Events: []string{usageEvent(10)}}
			if tc.at != "event:usage" {
				plan.Result = resultJSON("patch", handleJSON(att, "patch.diff", patch), 10)
			}
			out, err := w.host.Run(ctx, spec(stub(t, plan)))
			if err != nil {
				t.Fatal(err)
			}
			if (out.Submitted != nil) != tc.submitted {
				t.Fatalf("submitted %v, session %v", out.Submitted, out.Session)
			}
			ended(t, w, tc.usage)
		})
	}
}

// Astra #108 finding 8: a host fault after launch is reported with a fixed
// text (classification kept), never the underlying text, and the attempt
// is still finalised.
func TestHostFaultsHaveFixedTexts(t *testing.T) {
	const secret = "SENTINEL-patch.diff"
	for name, fault := range map[string]error{
		"capability": fmt.Errorf("%w: stat %s: mount id not reported", ingest.ErrUnsupported, secret),
		"io":         fmt.Errorf("reading %s: input/output error", secret),
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			ingestFile = func(*os.File, string, string, int64, int64, *blobstore.Store) (string, error) { return "", fault }
			t.Cleanup(func() { ingestFile = ingest.Ingest })
			out, err := w.host.Run(context.Background(), spec(fake(t, "safe-output")))
			if !errors.Is(err, ErrIngest) || strings.Contains(err.Error(), secret) || strings.Contains(fmt.Sprint(out.Session), secret) {
				t.Fatalf("run error %q, session %v", err, out.Session)
			}
			if errors.Is(err, ingest.ErrUnsupported) != (name == "capability") {
				t.Fatalf("classification lost: %v", err)
			}
			ended(t, w, 2)
		})
	}
}

// Astra #108 finding 1: a failure at any exchange setup step removes what
// was made (no panic, directory or descriptor left) and starts nothing.
func TestExchangeSetupFailuresCleanUp(t *testing.T) {
	for _, step := range []string{"in", "out", "prompt", "seal", "open"} {
		t.Run(step, func(t *testing.T) {
			w := newWorld(t)
			before := openFDs(t)
			setupFault = func(s string) error {
				if s == step {
					return errors.New("injected")
				}
				return nil
			}
			t.Cleanup(func() { setupFault = nil })
			out, err := w.host.Run(context.Background(), spec(stub(t, stubPlan{})))
			if !errors.Is(err, ErrExchange) || out.Result != nil {
				t.Fatalf("run: %v", err)
			}
			if entries, err := os.ReadDir(w.host.Root); err != nil || len(entries) != 0 {
				t.Fatalf("left behind: %v %v", entries, err)
			}
			if after := openFDs(t); after != before {
				t.Fatalf("%d descriptors open, %d before", after, before)
			}
			if _, ok := record[types.Attempt](w, lease.AttemptProjection, string(attID)); ok {
				t.Fatal("an attempt was started")
			}
		})
	}
}

func openFDs(t *testing.T) int {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(fds)
}

// Astra #108 round 2 (High): the finalisation's bound starts when
// finalisation does. A run longer than it, cancelled late, still ends
// closed with its usage durable.
func TestALongRunStillFinalises(t *testing.T) {
	w := newWorld(t)
	finalizeTimeout = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	began := time.Now()
	testHook = func(s string) {
		if s == "event:heartbeat" && time.Since(began) > 2*finalizeTimeout {
			cancel()
		}
	}
	t.Cleanup(func() { testHook, finalizeTimeout = nil, FinalizeTimeout })
	out, err := w.host.Run(ctx, spec(stub(t, stubPlan{
		Files:       map[string]string{"patch.diff": patch},
		Events:      []string{usageEvent(10)},
		HeartbeatMs: 800,
		Result:      resultJSON("patch", handleJSON(att, "patch.diff", patch), 10),
	})))
	if err != nil || out.Submitted != nil || time.Since(began) < finalizeTimeout {
		t.Fatalf("run: %v, submitted %v after %s", err, out.Submitted, time.Since(began))
	}
	ended(t, w, 1)
}

// Astra #108 round 2 (Medium): a valid result followed by a malformed
// shutdown reply is a broken session: nothing is submitted or published,
// and the result's accounting is still recorded.
func TestABrokenShutdownRefusesTheResult(t *testing.T) {
	w := newWorld(t)
	out, err := w.host.Run(context.Background(), spec(stub(t, stubPlan{
		Files:    map[string]string{"patch.diff": patch},
		Events:   []string{usageEvent(10)},
		Result:   resultJSON("patch", handleJSON(att, "patch.diff", patch), 10),
		Shutdown: `{"outcome":"bogus"}`,
	})))
	var e *session.Error
	if err != nil || out.Submitted != nil || !errors.As(out.Session, &e) || e.Kind != session.KindProtocol {
		t.Fatalf("run: %v, submitted %v, session %v", err, out.Submitted, out.Session)
	}
	if w.published(patch) {
		t.Fatal("published after a broken shutdown")
	}
	if a := ended(t, w, 2); a.Status != types.AttemptStatusLost {
		t.Fatalf("attempt %s", a.Status)
	}
}
