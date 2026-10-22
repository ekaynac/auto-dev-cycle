// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/accept"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// accepting makes rw's loop run accept nodes through its runner, with
// lister as the guard's go list, and runs passes until the accept node
// leaves claimed or is held (at most 10), waiting for each pass's runs.
func (rw *runWorld) accepting(t *testing.T, lister accept.Lister) graph.NodeView {
	t.Helper()
	acceptLister = func(sandbox.Host, sandbox.Image) accept.Lister { return lister }
	t.Cleanup(func() {
		acceptLister = func(h sandbox.Host, img sandbox.Image) accept.Lister { return accept.SandboxLister(h, img, listLimits) }
	})
	rw.x.Acceptor = rw.r
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	var v graph.NodeView
	for range 10 {
		if _, err := rw.l.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		rw.r.Wait()
		v = nodesOf(rw.w, rw.task)[types.NodeKindAccept]
		if v.State.State != types.NodeStateStateClaimed || len(v.State.Holds) > 0 {
			break
		}
	}
	return v
}

// resultOf is node view v's selected result.
func resultOf(t *testing.T, w *seeder, v graph.NodeView) types.ResultRecorded {
	t.Helper()
	var rr types.ResultRecorded
	if v.State.Result == nil {
		t.Fatalf("node %s selected no result: %s", v.Node.ID, v.State.State)
	}
	w.view(func(tx *eventstore.Tx) error {
		raw, _, err := tx.Get(graph.ResultProjection, string(*v.State.Result))
		if err == nil {
			err = json.Unmarshal(raw, &rr)
		}
		return err
	})
	return rr
}

// writes is a builder's edit writing files, name → content.
func writes(files map[string]string) func(string) {
	return func(ws string) {
		for name, content := range files {
			_ = os.MkdirAll(filepath.Dir(filepath.Join(ws, name)), 0o700)
			_ = os.WriteFile(filepath.Join(ws, name), []byte(content), 0o600)
		}
	}
}

// P22a-5c-2b-2 (W15): the controller runs the claimed accept node over the
// build's patch at the build attempt's base, recording accept's result
// with no attempt: a clean patch is accepted, its tree the value; one
// touching a protected path is failed/protected_path; the output files
// .gitignore kept out of the build's patch are its findings. Each leaves no
// clone behind.
func TestTheAcceptRunAcceptsTheBuildsPatch(t *testing.T) {
	for name, c := range map[string]struct {
		files   map[string]string
		outcome types.Outcome
		reason  vocab.Reason
		detail  string
	}{
		"a clean patch":     {files: map[string]string{"README.md": "hello\n"}, outcome: types.OutcomeEmpty, reason: vocab.ReasonCompletedClean},
		"a protected path":  {files: map[string]string{"pyproject.toml": "[x]\n"}, outcome: types.OutcomeFailed, reason: vocab.ReasonProtectedPath, detail: "pyproject.toml"},
		"an ignored output": {files: map[string]string{".gitignore": "gen/\n", "gen/x.txt": "x\n", "README.md": "hello\n"}, outcome: types.OutcomeFound, reason: vocab.ReasonCompletedWithFindings, detail: "gen/"},
	} {
		t.Run(name, func(t *testing.T) {
			rw := newRunWorld(t, true, builder(writes(c.files), false, nil))
			rw.untilRun(t)
			v := rw.accepting(t, nil)
			if v.State.State != types.NodeStateStateCompleted {
				t.Fatalf("the accept node: %s, holds %+v", v.State.State, v.State.Holds)
			}
			rr := resultOf(t, rw.w, v)
			res := rr.Result
			if rr.Attempt != nil || res.Kind != types.ResultKindTree || res.Outcome != c.outcome || res.Reason != c.reason || !strings.Contains(res.Detail, c.detail) {
				t.Errorf("accept's result: attempt %v, %s %s/%s %q", rr.Attempt, res.Kind, res.Outcome, res.Reason, res.Detail)
			}
			if c.outcome != types.OutcomeFailed && (len(res.Value) != 2 || res.Value[0].Type != types.RefTypeGitTree) {
				t.Errorf("an accepted result's value: %+v", res.Value)
			}
			if left, _ := filepath.Glob(filepath.Join(rw.tmp, "clones", "accept-*")); len(left) != 0 {
				t.Errorf("clones left: %v", left)
			}
		})
	}
}

// goTestWorld is a run world whose build adds a test to a module, so the
// guard lists (in the profile's pinned image); its build has run.
func goTestWorld(t *testing.T) *runWorld {
	t.Helper()
	rw := newRunWorld(t, true, builder(writes(map[string]string{
		"go.mod":    "module example.com/a\n\ngo 1.22\n",
		"a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { t.Log(\"a\") }\n",
	}), false, nil))
	rw.untilRun(t)
	return rw
}

// P22a-5c-2b-2: accept's run that fails is could_not_run/runner_fault,
// recorded with no attempt, never an accepted tree, and the node is held
// for a retry, so the next pass does not run it again:
//   - a guard that does not end is cut at acceptBound, the candidate's
//     integrity unverifiable;
//   - a sandbox failure: the pinned image does not resolve.
func TestAFailedAcceptRunCouldNotRun(t *testing.T) {
	hang := func(ctx context.Context, _ string, _ [2]string, _, _ []byte, _ []string) (map[string]accept.Listed, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	for name, c := range map[string]struct {
		noImage bool
		detail  string
	}{
		"a hanging guard":   {detail: "integrity is unverifiable"},
		"a sandbox failure": {noImage: true, detail: "accept could not run"},
	} {
		t.Run(name, func(t *testing.T) {
			rw := goTestWorld(t)
			if c.noImage {
				rw.r.place.d.Images = fakeImages{false}
			}
			bound := acceptBound
			acceptBound = 3 * time.Second
			t.Cleanup(func() { acceptBound = bound })
			v := rw.accepting(t, hang)
			held := ""
			for _, h := range v.State.Holds {
				if reconcile.IsRunnerSignal(h.SignalInstance) {
					held = h.SignalInstance
				}
			}
			if !strings.HasPrefix(held, reconcile.RunnerSignal("retry 1 until ")) {
				t.Errorf("the accept node after its run failed: %s, held on %q", v.State.State, held)
			}
			got := resultsOn(t, rw.w, v.Node.ID)
			if len(got) != 1 || got[0].Attempt != nil || got[0].Result.Outcome != types.OutcomeCouldNotRun || got[0].Result.Reason != vocab.ReasonRunnerFault ||
				!strings.Contains(got[0].Result.Detail, c.detail) || len(got[0].Result.Value) != 0 {
				t.Errorf("accept's results: %+v", got)
			}
		})
	}
}

// P22a-5c-2b-2: the guard's go list failing in its sandbox is the guard's
// to judge: the test's package is unverifiable, and the candidate is
// refused (failed/test_integrity_violation), never accepted.
func TestAFailedGoListRefusesTheCandidate(t *testing.T) {
	rw := goTestWorld(t)
	v := rw.accepting(t, func(context.Context, string, [2]string, []byte, []byte, []string) (map[string]accept.Listed, error) {
		return nil, errors.New("podman: the sandbox failed")
	})
	if v.State.State != types.NodeStateStateCompleted {
		t.Fatalf("the accept node: %s", v.State.State)
	}
	if res := resultOf(t, rw.w, v).Result; res.Outcome != types.OutcomeFailed || res.Reason != vocab.ReasonTestIntegrityViolation || !strings.Contains(res.Detail, "unverifiable") {
		t.Errorf("accept's result: %s/%s %q", res.Outcome, res.Reason, res.Detail)
	}
}

// Opus 22a-5c-2b-2 H1: a runner hold on an accept node clears as a build's
// does: a transient failure (the image misses once) holds the node for a
// retry; once its time passes, a pass clears it and the next run completes
// the node.
func TestAnAcceptRetryClears(t *testing.T) {
	rw := newRunWorld(t, true, builder(writes(map[string]string{"README.md": "hello\n"}), false, nil))
	rw.untilRun(t)
	retryBase = 300 * time.Millisecond
	t.Cleanup(func() { retryBase = time.Minute })
	rw.r.place.d.Images = fakeImages{false}
	if v := rw.accepting(t, nil); len(v.State.Holds) == 0 {
		t.Fatalf("the accept node after a transient failure: %s, no hold", v.State.State)
	}
	rw.r.place.d.Images = fakeImages{true}
	time.Sleep(retryBase)
	var v graph.NodeView
	for range 6 {
		if _, err := rw.l.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		rw.r.Wait()
		if v = nodesOf(rw.w, rw.task)[types.NodeKindAccept]; v.State.State == types.NodeStateStateCompleted {
			break
		}
	}
	if v.State.State != types.NodeStateStateCompleted {
		t.Fatalf("the accept node after its retry's time: %s, holds %+v", v.State.State, v.State.Holds)
	}
	if res := resultOf(t, rw.w, v).Result; res.Outcome != types.OutcomeEmpty {
		t.Errorf("the retried accept's result: %s/%s", res.Outcome, res.Reason)
	}
}
