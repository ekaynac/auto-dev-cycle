// SPDX-License-Identifier: Apache-2.0

//go:build linux

package adapterhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/ingest"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

// Fail closed: every error or missing piece of information on the way to
// a submission denies it. Each case forces one, on an otherwise safe run
// (a safe patch result beside a safe event-declared excerpt), and checks
// that no attempt.result_submitted was recorded and no blob published.
func TestAdmissionFailsClosed(t *testing.T) {
	excerpt := handleJSON(att, "excerpt.txt", "an excerpt")
	plan := stubPlan{
		Files:  map[string]string{"patch.diff": patch, "excerpt.txt": "an excerpt"},
		Events: []string{`"event":"error","turn":1,"class_hint":"other","raw_excerpt":` + excerpt, usageEvent(10)},
		Result: resultJSON("patch", handleJSON(att, "patch.diff", patch), 10),
	}
	only := func(name string, err error) func(string) error {
		return func(n string) error {
			if n == name {
				return err
			}
			return nil
		}
	}
	for name, tc := range map[string]struct {
		fence func(*eventstore.Tx, types.AttemptID, int64) error
		copy  func(name string) error // nil: copy normally; errSkip: report no copy and no error
		wait  error
		plan  func(*stubPlan) // edits the run's plan
		late  bool            // the run outlasts the finalisation timeout and is cancelled late
		// published: the result's copy is in the store although nothing
		// was submitted; only until staging (the stacked change) withholds
		// it past the second fence.
		published bool
	}{
		"the fence fails (a store or decode error)": {fence: func(*eventstore.Tx, types.AttemptID, int64) error { return errors.New("decode: bad record") }},
		"the attempt is unknown":                    {fence: func(*eventstore.Tx, types.AttemptID, int64) error { return &lease.Stale{Detail: "no such attempt"} }},
		"the result is not copied, without error":   {copy: only("patch.diff", errSkip)},
		"an excerpt is skipped, without error":      {copy: only("excerpt.txt", errSkip)},
		"an ingestion I/O fault":                    {copy: only("patch.diff", errors.New("read: input/output error"))},
		"the host cannot verify (ErrUnsupported)":   {copy: only("excerpt.txt", fmt.Errorf("%w: statx", ingest.ErrUnsupported))},
		"a refusal its record cannot hold":          {copy: only("excerpt.txt", &ingest.Unsafe{Check: "not-a-check", Detail: "x"}), published: true},
		"the tree was not seen to exit":             {wait: session.ErrUnverified},
		"a malformed shutdown reply":                {plan: func(p *stubPlan) { p.Shutdown = `{"outcome":"bogus"}` }},
		"a run outlasting the finalisation timeout, cancelled late": {late: true,
			plan: func(p *stubPlan) { p.HeartbeatMs = 800 }},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			if tc.fence != nil {
				fenceAttempt = tc.fence
			}
			if tc.copy != nil {
				ingestFile = func(dir *os.File, n, declared string, size, bound int64, store *blobstore.Store) (string, error) {
					switch err := tc.copy(n); {
					case errors.Is(err, errSkip):
						return "", nil
					case err != nil:
						return "", err
					}
					return ingest.Ingest(dir, n, declared, size, bound, store)
				}
			}
			if tc.wait != nil {
				waitTree = func(s *session.Session) error { _ = s.Wait(); return tc.wait }
			}
			t.Cleanup(func() {
				fenceAttempt, ingestFile, waitTree = lease.FenceAttempt, ingest.Ingest, (*session.Session).Wait
			})
			p := plan
			if tc.plan != nil {
				tc.plan(&p)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.late {
				began := time.Now()
				finalizeTimeout = 200 * time.Millisecond
				testHook = func(s string) {
					if s == "event:heartbeat" && time.Since(began) > 2*finalizeTimeout {
						cancel()
					}
				}
				t.Cleanup(func() { testHook, finalizeTimeout = nil, FinalizeTimeout })
			}
			out, _ := w.host.Run(ctx, spec(stub(t, p)))
			if out.Submitted != nil {
				t.Fatalf("submitted %+v", out.Submitted)
			}
			if a := w.attempt(); a.Status == types.AttemptStatusFinishing || a.Status == types.AttemptStatusRunning {
				t.Fatalf("attempt %s: submitted, or left running", a.Status)
			}
			if w.published(patch) != tc.published || w.tmpFiles() != 0 {
				t.Fatalf("published %v, %d temp files", w.published(patch), w.tmpFiles())
			}
		})
	}
}

// errSkip makes the ingestion seam report neither a copy nor an error.
var errSkip = errors.New("skip")
