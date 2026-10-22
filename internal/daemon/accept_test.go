// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/accept"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
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

// selectedOf is node view v's selected result.
func selectedOf(t *testing.T, w *seeder, v graph.NodeView) types.ResultRecorded {
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
			rr := selectedOf(t, rw.w, v)
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

// P22a-5c-2b-2: accept's run that fails is could_not_run/
// controller_run_failed (Opus 22a-5c-2b-2 M2: accept's own reason, its
// hold the runner's), recorded with no attempt, never an accepted tree,
// and the node is held, so the next pass does not run it again:
//   - a guard that does not end is cut at acceptBound, the candidate's
//     integrity unverifiable: held for a retry;
//   - the pinned image not present: the configuration's, held on the
//     profile.
func TestAFailedAcceptRunCouldNotRun(t *testing.T) {
	hang := func(ctx context.Context, _ string, _ [2]string, _, _ []byte, _ []string) (map[string]accept.Listed, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	for name, c := range map[string]struct {
		noImage bool
		detail  string
		held    string
	}{
		"a hanging guard":       {detail: "integrity is unverifiable", held: "retry 1 until "},
		"the image not present": {noImage: true, detail: "accept could not run", held: "image"},
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
			if !strings.HasPrefix(held, reconcile.RunnerSignal(c.held)) {
				t.Errorf("the accept node after its run failed: %s, held on %q", v.State.State, held)
			}
			got := resultsOn(t, rw.w, v.Node.ID)
			if len(got) != 1 || got[0].Attempt != nil || got[0].Result.Outcome != types.OutcomeCouldNotRun || got[0].Result.Reason != vocab.ReasonControllerRunFailed ||
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
	if res := selectedOf(t, rw.w, v).Result; res.Outcome != types.OutcomeFailed || res.Reason != vocab.ReasonTestIntegrityViolation || !strings.Contains(res.Detail, "unverifiable") {
		t.Errorf("accept's result: %s/%s %q", res.Outcome, res.Reason, res.Detail)
	}
}

// Opus 22a-5c-2b-2 H1: a runner hold on an accept node clears as a build's
// does: a transient failure (the guard cut at the run's bound) holds the
// node for a retry; once its time passes, a pass clears it and the next
// run (its go list now ending) completes the node.
func TestAnAcceptRetryClears(t *testing.T) {
	rw := goTestWorld(t)
	retryBase = 300 * time.Millisecond
	bound := acceptBound
	acceptBound = 2 * time.Second
	t.Cleanup(func() { retryBase, acceptBound = time.Minute, bound })
	if v := rw.accepting(t, func(ctx context.Context, _ string, _ [2]string, _, _ []byte, _ []string) (map[string]accept.Listed, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}); len(v.State.Holds) == 0 {
		t.Fatalf("the accept node after a transient failure: %s, no hold", v.State.State)
	}
	acceptLister = func(sandbox.Host, sandbox.Image) accept.Lister {
		return func(context.Context, string, [2]string, []byte, []byte, []string) (map[string]accept.Listed, error) {
			return nil, errors.New("no listing")
		}
	}
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
}

// Opus 22a-5c-2b-2 L3: adcd's executor runs builds, accepts and gates
// through its runner, and none without one.
func TestTheExecutorRunsThroughTheRunner(t *testing.T) {
	r := &runner{}
	x := executorOf(&Daemon{}, registry.Registry{}, fakeImages{true}, fakeCredentials{}, r)
	if x.Runner != reconcile.Runner(r) || x.Acceptor != reconcile.Acceptor(r) {
		t.Errorf("the executor's runner %v and acceptor %v, want %p", x.Runner, x.Acceptor, r)
	}
	if x := executorOf(&Daemon{}, registry.Registry{}, fakeImages{true}, fakeCredentials{}, nil); x.Runner != nil || x.Acceptor != nil {
		t.Errorf("an executor with no runner: %v %v", x.Runner, x.Acceptor)
	}
}

// Opus 22a-5c-2b-2 L2: a profile pinning other than one image is the
// configuration's: the run's checkout refuses it before any clone.
func TestACheckoutNeedsOneImage(t *testing.T) {
	r := &runner{}
	for _, images := range []map[string]types.PinnedImage{{}, {"a": "x", "b": "y"}} {
		var p types.ProjectProfile
		p.Toolchain.Images = images
		if _, _, err := r.checkout(context.Background(), p, t.TempDir()); !errors.Is(err, errConfig) || !strings.Contains(err.Error(), "images, not one") {
			t.Errorf("%d images: %v", len(images), err)
		}
	}
}

// Opus 22a-5c-2b-2 L1: a blob read by its digest is refused when its bytes
// are not that digest's (damaged on disk), or over the read's bound.
func TestABlobIsReadByItsDigest(t *testing.T) {
	dir := t.TempDir()
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	d, err := blobs.Put(strings.NewReader("the patch"), "")
	if err != nil {
		t.Fatal(err)
	}
	r := &runner{blobs: blobs}
	if b, err := r.readBlob(types.Digest(d), 64); err != nil || string(b) != "the patch" {
		t.Fatalf("the blob: %q %v", b, err)
	}
	if _, err := r.readBlob(types.Digest(d), 4); err == nil {
		t.Error("a blob over its bound was read")
	}
	hexsum := strings.TrimPrefix(d, "sha256:")
	p := filepath.Join(dir, "blobs", "sha256", hexsum[:2], hexsum[2:4], hexsum)
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("the other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.readBlob(types.Digest(d), 64); err == nil {
		t.Error("a damaged blob was read as its digest's")
	}
}

// countAccepts counts the accept and gate runs an Acceptor is asked for.
type countAccepts struct {
	r *runner
	n atomic.Int32
}

func (c *countAccepts) Accept(node types.NodeID) bool                 { c.n.Add(1); return c.r.Accept(node) }
func (c *countAccepts) Gate(node types.NodeID) bool                   { c.n.Add(1); return c.r.Gate(node) }
func (c *countAccepts) Reseal(nodes []types.NodeID) bool              { return c.r.Reseal(nodes) }
func (c *countAccepts) End(task types.TaskID, node types.NodeID) bool { return c.r.End(task, node) }

// Opus 22a-5c-2b-3 H2 (and 22a-5c-2b-2 M1, L1): while the build attempt
// that recorded accept's input is not sealed, the loop decides no Accept:
// no run in N passes, the accept node claimed with no hold.
func TestAnUnsealedInputIsNotAccepted(t *testing.T) {
	sealWith = func(*eventstore.Tx, types.AttemptID, int64, lease.Units, lease.Publisher, *types.Digest, []string, types.ActorID) ([]eventstore.Event, error) {
		return nil, nil // the seal's command never lands
	}
	t.Cleanup(func() { sealWith = lease.SealWith })
	rw := newRunWorld(t, true, builder(writes(map[string]string{"README.md": "hello\n"}), false, nil))
	rw.untilRun(t)
	if a, _ := rw.attemptOf(t); a.Checkpoint != nil {
		t.Fatalf("the build's attempt was sealed: %+v", a)
	}
	c := &countAccepts{r: rw.r}
	rw.x.Acceptor = c
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	rw.passesFor(t, 5, 0)
	if v := nodesOf(rw.w, rw.task)[types.NodeKindAccept]; c.n.Load() != 0 || v.State.State != types.NodeStateStateClaimed || len(v.State.Holds) != 0 {
		t.Errorf("accept on an unsealed input: %d runs, %s, holds %+v", c.n.Load(), v.State.State, v.State.Holds)
	}
}

// Opus 22a-5c-2b-3 M1: a node held because its pinned image was not present
// ("runner: image") is released once the image resolves, and its next run
// completes it.
func TestAnImageHoldClearsOnceItResolves(t *testing.T) {
	rw := newRunWorld(t, true, builder(writes(map[string]string{"README.md": "hello\n"}), false, nil))
	rw.untilRun(t)
	rw.r.place.d.Images = fakeImages{false}
	if v := rw.accepting(t, nil); len(v.State.Holds) != 1 || v.State.Holds[0].SignalInstance != runnerImage {
		t.Fatalf("the accept node after its image missed: %s, holds %+v", v.State.State, v.State.Holds)
	}
	rw.passesFor(t, 2, 0)
	if v := nodesOf(rw.w, rw.task)[types.NodeKindAccept]; len(v.State.Holds) != 1 {
		t.Fatalf("the image hold cleared while the image was absent: %+v", v.State.Holds)
	}
	rw.r.place.d.Images = fakeImages{true}
	var v graph.NodeView
	for range 6 {
		rw.passesFor(t, 1, 0)
		if v = nodesOf(rw.w, rw.task)[types.NodeKindAccept]; v.State.State == types.NodeStateStateCompleted {
			break
		}
	}
	if v.State.State != types.NodeStateStateCompleted {
		t.Fatalf("the accept node once its image resolved: %s, holds %+v", v.State.State, v.State.Holds)
	}
}

// Opus 22a-5c-2b-3 M1: a clone the network failed (sandbox.ErrGit) is a
// bounded retry; a repository the controller refuses (sandbox.ErrSpec) is
// the configuration's, held on the profile.
func TestACloneFailureIsClassed(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		held string
	}{
		"a network failure":    {sandbox.ErrGit, reconcile.RunnerSignal("retry 1 until ")},
		"a refused repository": {sandbox.ErrSpec, reconcile.RunnerSignal("profile ")},
	} {
		t.Run(name, func(t *testing.T) {
			rw := newRunWorld(t, true, builder(writes(map[string]string{"README.md": "hello\n"}), false, nil))
			rw.untilRun(t)
			cloneRepo = func(sandbox.Host, context.Context, string, string, string) (sandbox.Repo, error) {
				return sandbox.Repo{}, fmt.Errorf("%w: the clone", c.err)
			}
			t.Cleanup(func() { cloneRepo = sandbox.Host.Clone })
			v := rw.accepting(t, nil)
			if len(v.State.Holds) != 1 || !strings.HasPrefix(v.State.Holds[0].SignalInstance, c.held) {
				t.Errorf("the accept node after its clone failed: %s, holds %+v", v.State.State, v.State.Holds)
			}
		})
	}
}

// Main's obligation for 5c-2c (after Opus 22a-5c-2b-3 H2): a build seal
// that failed while adcd runs is re-run by the loop once the accept node
// has waited resealAfter: the seal fails once, then works; the attempt is
// sealed, and accept runs and completes.
func TestAFailedSealIsRedone(t *testing.T) {
	failed := false
	sealWith = func(tx *eventstore.Tx, id types.AttemptID, rev int64, u lease.Units, p lease.Publisher, d *types.Digest, ig []string, a types.ActorID) ([]eventstore.Event, error) {
		if !failed {
			failed = true
			return nil, errors.New("the seal's blob could not be published")
		}
		return lease.SealWith(tx, id, rev, u, p, d, ig, a)
	}
	t.Cleanup(func() { sealWith = lease.SealWith })
	resealAfter = 0
	t.Cleanup(func() { resealAfter = 2 * time.Minute })
	rw := newRunWorld(t, true, builder(writes(map[string]string{"README.md": "hello\n"}), false, nil))
	rw.untilRun(t)
	if a, _ := rw.attemptOf(t); a.Checkpoint != nil {
		t.Fatalf("the build's attempt was sealed: %+v", a)
	}
	rw.x.Acceptor = rw.r
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	var v graph.NodeView
	for range 8 {
		rw.passesFor(t, 1, 0)
		if v = nodesOf(rw.w, rw.task)[types.NodeKindAccept]; v.State.State == types.NodeStateStateCompleted {
			break
		}
	}
	if a, _ := rw.attemptOf(t); a.Checkpoint == nil || v.State.State != types.NodeStateStateCompleted {
		t.Fatalf("after the redone seal: the attempt %s (checkpoint %v), accept %s, holds %+v", a.Status, a.Checkpoint, v.State.State, v.State.Holds)
	}
}

// The redone seal's tries are bounded: a seal that never lands holds the
// accept node "runner: failed" with its cause once maxTries are spent.
func TestASealThatNeverLandsIsHeld(t *testing.T) {
	sealWith = func(*eventstore.Tx, types.AttemptID, int64, lease.Units, lease.Publisher, *types.Digest, []string, types.ActorID) ([]eventstore.Event, error) {
		return nil, errors.New("the seal's blob could not be published")
	}
	t.Cleanup(func() { sealWith = lease.SealWith })
	resealAfter = 0
	t.Cleanup(func() { resealAfter = 2 * time.Minute })
	rw := newRunWorld(t, true, builder(writes(map[string]string{"README.md": "hello\n"}), false, nil))
	rw.untilRun(t)
	rw.x.Acceptor = rw.r
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	rw.passesFor(t, maxTries+2, 0)
	v := nodesOf(rw.w, rw.task)[types.NodeKindAccept]
	if len(v.State.Holds) != 1 || v.State.Holds[0].SignalInstance != runnerFailed {
		t.Errorf("the accept node after its input's seal never landed: %s, holds %+v", v.State.State, v.State.Holds)
	}
}

// podmanStub is the sandbox's podman: run ends with run, rm with rm.
type podmanStub struct{ run, rm error }

func (p podmanStub) Run(_ context.Context, _ string, args, _ []string) ([]byte, error) {
	switch {
	case slices.Contains(args, "run"):
		return nil, p.run
	case slices.Contains(args, "rm"):
		return nil, p.rm
	}
	return nil, nil
}

// exitOf is a real exit error: code, or -1 for a process killed.
func exitOf(t *testing.T, code int) error {
	t.Helper()
	script := fmt.Sprintf("exit %d", code)
	if code < 0 {
		script = "kill -KILL $$"
	}
	err := exec.Command("sh", "-c", script).Run()
	if err == nil {
		t.Fatalf("no exit error for %d", code)
	}
	return err
}

// Astra chunk 1 H2 and round 2 (Lead decision, #226 entry 147): the
// guard's go list runs through the real sandbox (accept.SandboxLister,
// sandbox.Host.Start) on a podman stub. Only positive evidence against the
// candidate is its refusal: go list exiting 1 on its code is
// failed/test_integrity_violation, selected. Everything else is the
// controller's: could_not_run/controller_run_failed with no attempt and a
// bounded retry hold, never the profile's:
//   - the run's attempt not made (its runs/ not writable, as ENOSPC);
//   - a mount the sandbox refuses (a comma in its path: sandbox.ErrSpec);
//   - podman's client killed (-1);
//   - a good run whose container's removal exits 1 (sandbox.ErrCleanup).
func TestOnlyTheCandidatesOwnFailureIsItsRefusal(t *testing.T) {
	for name, c := range map[string]struct {
		root    string // under its /dev/shm directory
		podman  podmanStub
		lockRun bool // runs/ not writable
		refusal bool
	}{
		"go list exited 1 on its code": {root: "sbx", podman: podmanStub{run: exitOf(t, 1)}, refusal: true},
		"the attempt not made":         {root: "sbx", lockRun: true},
		"a mount the sandbox refuses":  {root: "s,bx"},
		"its client killed":            {root: "sbx", podman: podmanStub{run: exitOf(t, -1)}},
		"a good run, rm exiting 1":     {root: "sbx", podman: podmanStub{rm: exitOf(t, 1)}},
	} {
		t.Run(name, func(t *testing.T) {
			rw := goTestWorld(t)
			shm, err := os.MkdirTemp("/dev/shm", "adclist")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(filepath.Join(shm, c.root, "runs"), 0o700); os.RemoveAll(shm) })
			h := sandbox.Host{Podman: "/usr/bin/podman", Root: filepath.Join(shm, c.root), Home: "/home/adc-gate-test", Runner: c.podman}
			if err := os.MkdirAll(filepath.Join(h.Root, "runs"), 0o700); err != nil {
				t.Fatal(err)
			}
			if c.lockRun {
				if err := os.Chmod(filepath.Join(h.Root, "runs"), 0o500); err != nil {
					t.Fatal(err)
				}
			}
			img := sandbox.Image{Pinned: "localhost/adc/go@sha256:" + strings.Repeat("1", 64), ID: strings.Repeat("b", 64), OS: runtime.GOOS, Arch: runtime.GOARCH}
			lister := accept.SandboxLister(h, img, listLimits)
			v := rw.accepting(t, lister)
			got := resultsOn(t, rw.w, v.Node.ID)
			if c.refusal {
				if v.State.State != types.NodeStateStateCompleted || len(got) != 1 || got[0].Result.Outcome != types.OutcomeFailed ||
					got[0].Result.Reason != vocab.ReasonTestIntegrityViolation {
					t.Errorf("go list failing on the candidate: %s, results %+v", v.State.State, got)
				}
				return
			}
			if v.State.State == types.NodeStateStateCompleted || len(v.State.Holds) != 1 ||
				!strings.HasPrefix(v.State.Holds[0].SignalInstance, reconcile.RunnerSignal("retry 1 until ")) ||
				len(got) != 1 || got[0].Attempt != nil || got[0].Result.Outcome != types.OutcomeCouldNotRun ||
				got[0].Result.Reason != vocab.ReasonControllerRunFailed {
				t.Errorf("accept after its guard did not run: %s, holds %+v, results %+v", v.State.State, v.State.Holds, got)
			}
		})
	}
}

// Astra chunk 1 M1: accept's final commit refused (its input invalidated
// while the guard ran: graph.Invalidate on the build) records nothing and
// never reruns at once: the accept node, still holding the claim its run
// was of, is held for a bounded retry, so the passes that follow start no
// second run of it.
func TestARefusedAcceptCommitIsHeld(t *testing.T) {
	rw := goTestWorld(t)
	var invalidated error
	v := rw.accepting(t, func(context.Context, string, [2]string, []byte, []byte, []string) (map[string]accept.Listed, error) {
		invalidated = rw.w.try(rw.w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Invalidate(tx, rw.build)
		})
		return nil, errors.New("no listing")
	})
	if invalidated != nil {
		t.Fatalf("the build's invalidation: %v", invalidated)
	}
	c := &countAccepts{r: rw.r}
	rw.x.Acceptor = c
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	rw.passesFor(t, 4, 0)
	v = nodesOf(rw.w, rw.task)[types.NodeKindAccept]
	if got := resultsOn(t, rw.w, v.Node.ID); len(got) != 0 || c.n.Load() != 0 || len(v.State.Holds) != 1 ||
		!strings.HasPrefix(v.State.Holds[0].SignalInstance, reconcile.RunnerSignal("retry 1 until ")) {
		t.Errorf("accept after its refused commit: %s, holds %+v, %d more runs, results %+v", v.State.State, v.State.Holds, c.n.Load(), got)
	}
}
