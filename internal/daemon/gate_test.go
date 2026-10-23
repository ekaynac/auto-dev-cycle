// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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

// gateModule is the gate world's Go module.
const gateModule = "example.com/a"

// goEvents is a go test -json stream of gateModule's one package: each
// test "Name=outcome", the package failing when one of them fails.
func goEvents(tests ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"Action":"start","Package":%q}`+"\n", gateModule)
	failed := false
	for _, x := range tests {
		test, o, _ := strings.Cut(x, "=")
		fmt.Fprintf(&b, `{"Action":"run","Package":%q,"Test":%q}`+"\n", gateModule, test)
		fmt.Fprintf(&b, `{"Action":%q,"Package":%q,"Test":%q}`+"\n", o, gateModule, test)
		failed = failed || o == "fail"
	}
	fmt.Fprintf(&b, `{"Action":%q,"Package":%q}`+"\n", map[bool]string{true: "fail", false: "pass"}[failed], gateModule)
	return b.String()
}

// inPlace runs a gate container's command in place, the sandbox's Runner
// boundary: the attempt's workspace its cwd, the container's mounts mapped
// to the attempt's directories. fail, when set, is a run the sandbox
// failed.
type inPlace struct{ fail error }

func (l *inPlace) Run(ctx context.Context, _ string, args, _ []string) ([]byte, error) {
	if !slices.Contains(args, "run") {
		return nil, nil // rm, unshare
	}
	if l.fail != nil {
		return nil, l.fail
	}
	var ws, res string
	i := slices.IndexFunc(args, func(a string) bool { return len(a) == 64 && strings.Trim(a, "0123456789abcdef") == "" }) // the image id
	for _, a := range args[:i] {
		if m, ok := strings.CutPrefix(a, "--mount=type=bind,src="); ok {
			src, dst, _ := strings.Cut(m, ",dst=")
			switch dst {
			case sandbox.WorkspaceDir:
				ws = src
			case sandbox.ResultsDir:
				res = src
			}
		}
	}
	mapped := strings.NewReplacer(sandbox.WorkspaceDir, ws, sandbox.ResultsDir, res)
	argv := make([]string, 0, len(args)-i-1)
	for _, a := range args[i+1:] {
		argv = append(argv, mapped.Replace(a))
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = ws, []string{"PATH=/usr/bin:/bin"}
	return cmd.Output()
}

// gateImages resolves every reference to one image of this platform.
type gateImages struct{}

func (gateImages) Resolve(_ context.Context, ref string) (sandbox.Image, error) {
	return sandbox.Image{Pinned: ref, ID: strings.Repeat("b", 64), OS: runtime.GOOS, Arch: runtime.GOARCH}, nil
}

// gateWorld is a run world whose project is a Go module, its base's gate
// stream base, its profile's gate one that prints the tree's
// gate-events.json, and whose build writes files; its build has run, and
// its runner then runs accept and the gate in a sandbox on a tmpfs whose
// runs happen in place (fail: the sandbox fails every run).
func gateWorld(t *testing.T, base string, files map[string]string, fail error) *runWorld {
	t.Helper()
	return gateWorldWith(t, base, files, fail, nil)
}

// gateWorldWith is gateWorld whose profile edit, if any, edits the
// profile further.
func gateWorldWith(t *testing.T, base string, files map[string]string, fail error, edit func(map[string]any)) *runWorld {
	t.Helper()
	rw := newRunWorldWith(t, true, builder(writes(files), false, nil), func(p map[string]any) {
		goModule(t, base)(p)
		if edit != nil {
			edit(p)
		}
	})
	rw.untilRun(t)
	root, err := os.MkdirTemp("/dev/shm", "adcgate")
	if err != nil {
		t.Fatalf("the sandbox root's runs/ needs a tmpfs of its own (/dev/shm): %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	rw.r.box = sandbox.Host{Podman: "/usr/bin/podman", Root: root, Home: "/home/adc-gate-test", Runner: &inPlace{fail: fail}}
	rw.r.place.d.Images = gateImages{}
	return rw
}

// goModule is gateWorld's profile edit: the project's repository a Go
// module whose base's gate stream is base, its gate printing the tree's
// gate-events.json.
func goModule(t *testing.T, base string) func(map[string]any) {
	return func(p map[string]any) {
		src := p["repo"].(map[string]any)["url"].(string)
		for name, body := range map[string]string{"go.mod": "module " + gateModule + "\n\ngo 1.22\n", "gate-events.json": base} {
			if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		gitIn(t, src, "", "add", "go.mod", "gate-events.json")
		gitIn(t, src, "", "commit", "--quiet", "-m", "a module")
		p["gate"] = map[string]any{"command": []string{"sh", "-c", "cat gate-events.json"}, "result_adapter": "go_test_json",
			"baseline": map[string]any{"kind": "measured", "relock_requires": "approval"}, "ownership_rules": "default",
			"environments": []map[string]any{{"name": "ci", "owner": "operator", "facts": map[string]any{"vcs_metadata": false, "network": "none",
				"runtime": map[string]any{"language_version": "1.22", "os": runtime.GOOS, "arch": runtime.GOARCH}}}}}
	}
}

// gating runs rw's passes, accept's and the gate's runs through its
// runner, until the gate node leaves claimed or is held (at most 12).
func (rw *runWorld) gating(t *testing.T) graph.NodeView {
	t.Helper()
	acceptLister = func(sandbox.Host, sandbox.Image) accept.Lister { return nil }
	t.Cleanup(func() { acceptLister = defaultLister })
	rw.x.Acceptor = rw.r
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	var v graph.NodeView
	for range 12 {
		if _, err := rw.l.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		rw.r.Wait()
		v = nodesOf(rw.w, rw.task)[types.NodeKindGate]
		if v.State.State == types.NodeStateStateCompleted || len(v.State.Holds) > 0 {
			break
		}
	}
	return v
}

// P22a-5c-2b-3 (W15, I13): the controller gates accept's tree: the
// baseline measured on the build's base, the gate run on the accepted tree
// against it, its result (the receipt and the report) recorded with no
// attempt. A green candidate is empty/completed_clean; a test the
// candidate fails, or one missing in a package it changed (the changed
// files are the base's and the tree's diff), is found/gate_test_failed;
// each completes the gate.
func TestTheGateRunGatesTheAcceptedTree(t *testing.T) {
	for name, c := range map[string]struct {
		files   map[string]string
		outcome types.Outcome
		reason  vocab.Reason
	}{
		"a green candidate": {files: map[string]string{"README.md": "hello\n"}, outcome: types.OutcomeEmpty, reason: vocab.ReasonCompletedClean},
		"a failing test":    {files: map[string]string{"gate-events.json": goEvents("TestA=fail")}, outcome: types.OutcomeFound, reason: vocab.ReasonGateTestFailed},
		"a test missing in the package it changed": {files: map[string]string{"README.md": "hello\n", "gate-events.json": goEvents()},
			outcome: types.OutcomeFound, reason: vocab.ReasonGateTestFailed},
	} {
		t.Run(name, func(t *testing.T) {
			rw := gateWorld(t, goEvents("TestA=pass"), c.files, nil)
			v := rw.gating(t)
			if v.State.State != types.NodeStateStateCompleted {
				t.Fatalf("the gate node: %s, holds %+v", v.State.State, v.State.Holds)
			}
			rr := selectedOf(t, rw.w, v)
			if rr.Attempt != nil || rr.Result.Kind != types.ResultKindGateResult || rr.Result.Outcome != c.outcome || rr.Result.Reason != c.reason {
				t.Errorf("the gate's result: attempt %v, %s %s/%s %q", rr.Attempt, rr.Result.Kind, rr.Result.Outcome, rr.Result.Reason, rr.Result.Detail)
			}
			if ok, full := graph.GateShape(rr.Result.Value); !ok || !full {
				t.Errorf("the gate's result holds %+v, not its receipt and its report", rr.Result.Value)
			}
		})
	}
}

// P22a-5c-2b-3: a gate that cannot run is could_not_run/
// controller_run_failed, never a green, recorded with no attempt, and the
// node is held, never run again by the next pass: a base whose gate is not
// green, held at once ("runner: failed": Opus 22a-5c-2b-3 M2), its only
// package failing to build with no test result among them (Astra chunk 1
// M2), and a sandbox that fails the run, held for a retry.
func TestAGateThatCannotRunCouldNotRun(t *testing.T) {
	for name, c := range map[string]struct {
		base   string
		fail   error
		detail string
		held   string
	}{
		"a red base": {base: goEvents("TestA=fail"), detail: "not green", held: string(runnerFailed)},
		"a base that does not build": {base: fmt.Sprintf(`{"Action":"start","Package":%q}`+"\n"+`{"Action":"output","Package":%q,"Output":"FAIL\t%s [build failed]\n"}`+"\n"+`{"Action":"fail","Package":%q}`+"\n",
			gateModule, gateModule, gateModule, gateModule), detail: "not green", held: string(runnerFailed)},
		"the sandbox failed": {base: goEvents("TestA=pass"), fail: errors.New("podman: the run failed"), detail: "could not run", held: runnerText("retry 1 until ")},
	} {
		t.Run(name, func(t *testing.T) {
			rw := gateWorld(t, c.base, map[string]string{"README.md": "hello\n"}, c.fail)
			v := rw.gating(t)
			held := ""
			for _, h := range v.State.Holds {
				if isRunnerHold(h.SignalInstance) {
					held = h.SignalInstance
				}
			}
			got := resultsOn(t, rw.w, v.Node.ID)
			if !strings.HasPrefix(held, c.held) || len(got) != 1 || got[0].Attempt != nil || got[0].Result.Outcome != types.OutcomeCouldNotRun ||
				got[0].Result.Reason != vocab.ReasonControllerRunFailed || !strings.Contains(got[0].Result.Detail, c.detail) {
				t.Errorf("the gate after it could not run: %s, held on %q, results %+v", v.State.State, held, got)
			}
		})
	}
}

// P22a-5c-2b-3: a profile whose gate this controller cannot run (M1's
// py-mini: a junit_xml gate) holds the gate node on the configuration,
// recording nothing.
func TestAGateTheProfileCannotRunHoldsOnTheProfile(t *testing.T) {
	rw := newRunWorld(t, true, builder(writes(map[string]string{"README.md": "hello\n"}), false, nil))
	rw.untilRun(t)
	root, err := os.MkdirTemp("/dev/shm", "adcgate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	rw.r.box = sandbox.Host{Podman: "/usr/bin/podman", Root: root, Home: "/home/adc-gate-test", Runner: &inPlace{}}
	rw.r.place.d.Images = gateImages{}
	v := rw.gating(t)
	held := ""
	for _, h := range v.State.Holds {
		if isRunnerHold(h.SignalInstance) {
			held = h.SignalInstance
		}
	}
	if !strings.HasPrefix(held, runnerText("profile ")) || len(resultsOn(t, rw.w, v.Node.ID)) != 0 {
		t.Errorf("the gate of a junit_xml profile: %s, held on %q", v.State.State, held)
	}
}

// P22a-5c-2b-3 (Opus 22a-5c-2b-2 H1, for the gate): a gate held for a
// transient failure (its sandbox failed the run) is retried once its time
// passes, and the next run completes it.
func TestAGateRetryClears(t *testing.T) {
	rw := gateWorld(t, goEvents("TestA=pass"), map[string]string{"README.md": "hello\n"}, errors.New("podman: the run failed"))
	retryBase = 300 * time.Millisecond
	t.Cleanup(func() { retryBase = time.Minute })
	if v := rw.gating(t); len(v.State.Holds) == 0 {
		t.Fatalf("the gate after its sandbox failed: %s, no hold", v.State.State)
	}
	rw.r.box.Runner = &inPlace{}
	time.Sleep(retryBase)
	var v graph.NodeView
	for range 6 {
		if _, err := rw.l.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		rw.r.Wait()
		if v = nodesOf(rw.w, rw.task)[types.NodeKindGate]; v.State.State == types.NodeStateStateCompleted {
			break
		}
	}
	if v.State.State != types.NodeStateStateCompleted {
		t.Fatalf("the gate after its retry's time: %s, holds %+v", v.State.State, v.State.Holds)
	}
}

// Opus 22a-5c-2b-3 H1: the gate's own could_not_run (a candidate stream the
// controller cannot read) is recorded once, and the node is held in the
// same command for a bounded retry: no second run in the passes that
// follow.
func TestAnUnreadableStreamIsHeld(t *testing.T) {
	rw := gateWorld(t, goEvents("TestA=pass"), map[string]string{"gate-events.json": "not a stream\n"}, nil)
	v := rw.gating(t)
	rw.passesFor(t, 4, 0)
	v = nodesOf(rw.w, rw.task)[types.NodeKindGate]
	held := ""
	for _, h := range v.State.Holds {
		if isRunnerHold(h.SignalInstance) {
			held = h.SignalInstance
		}
	}
	got := resultsOn(t, rw.w, v.Node.ID)
	if len(got) != 1 || got[0].Result.Outcome != types.OutcomeCouldNotRun || got[0].Result.Reason != vocab.ReasonRunnerFault ||
		!strings.HasPrefix(held, runnerText("retry 1 until ")) {
		t.Errorf("the gate of an unreadable stream: %s, held on %q, results %+v", v.State.State, held, got)
	}
}

// forgedAccept is an acceptor whose accept records an accepted tree that
// is not the build's patch at its base (tree), the gate run by its runner.
type forgedAccept struct {
	*runner
	t    *testing.T
	tree string
}

func (f forgedAccept) Accept(node types.NodeID) bool {
	d, err := f.blobs.Put(strings.NewReader(`{"schema":"forged"}`), "")
	if err == nil {
		err = f.command(f.ctx, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			cur, err := graph.ViewOf(tx, node)
			if err != nil || cur.State.Key == nil {
				return nil, errors.Join(err, errors.New("no claim"))
			}
			res, err := accept.Result(*cur.State.Key, accept.Verdict{Tree: f.tree, TreeDigest: types.Digest("sha256:" + strings.Repeat("a", 64))}, cur.Project, types.Digest(d))
			if err != nil {
				return nil, err
			}
			return nil, graph.Commit(tx, node, res, f.env)
		})
	}
	if err != nil {
		f.t.Errorf("the forged accept: %v", err)
	}
	return true
}

// K2a (I12 admission): the gate rebuilds accept's tree from the build's
// patch at its base and gates only that tree; an accepted result naming
// another tree is the content's, held "runner: failed" at once, with no
// gate result and no gate run.
func TestAGateRefusesATreeThatDoesNotRebuild(t *testing.T) {
	rw := gateWorld(t, goEvents("TestA=pass"), map[string]string{"README.md": "hello\n"}, nil)
	acceptLister = func(sandbox.Host, sandbox.Image) accept.Lister { return nil }
	t.Cleanup(func() { acceptLister = defaultLister })
	rw.x.Acceptor = forgedAccept{rw.r, t, "4b825dc642cb6eb9a060e54bf8d69288fbee4904"} // the empty tree
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	var v graph.NodeView
	for range 12 {
		if _, err := rw.l.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		rw.r.Wait()
		if v = nodesOf(rw.w, rw.task)[types.NodeKindGate]; v.State.State == types.NodeStateStateCompleted || len(v.State.Holds) > 0 {
			break
		}
	}
	if v.State.State == types.NodeStateStateCompleted || len(v.State.Holds) != 1 || v.State.Holds[0].SignalInstance != runnerFailed ||
		len(resultsOn(t, rw.w, v.Node.ID)) != 0 {
		t.Fatalf("a gate over a tree that does not rebuild: %s, holds %+v, results %+v", v.State.State, v.State.Holds, resultsOn(t, rw.w, v.Node.ID))
	}
}

// K2a (I12 consumption, main's behaviour; #226 entry 181): an accept that
// refuses its candidate (failed/protected_path) records no tree, so the
// gate takes none: the gate claimed on the refusal is held at once
// ("runner: failed", the content's: its input is no tree), visibly, with
// no gate result and no gate run. Nothing repairs the build.
func TestAGateOverARefusedAcceptIsHeld(t *testing.T) {
	rw := gateWorld(t, goEvents("TestA=pass"), map[string]string{"pyproject.toml": "[x]\n"}, nil)
	var log lockedBuffer
	rw.r.log = io.MultiWriter(&log, testLog{t})
	v := rw.gating(t)
	if a := nodesOf(rw.w, rw.task)[types.NodeKindAccept]; a.State.State != types.NodeStateStateCompleted || selectedOf(t, rw.w, a).Result.Reason != vocab.ReasonProtectedPath {
		t.Fatalf("the accept: %s", a.State.State)
	}
	if v.State.State == types.NodeStateStateCompleted || len(v.State.Holds) != 1 || v.State.Holds[0].SignalInstance != runnerFailed ||
		len(resultsOn(t, rw.w, v.Node.ID)) != 0 || !strings.Contains(log.String(), "input is no tree accept recorded") {
		t.Fatalf("a gate over a refused accept: %s, holds %+v, results %+v; %s", v.State.State, v.State.Holds, resultsOn(t, rw.w, v.Node.ID), log.String())
	}
}

// hiddenInv is an acceptance test of gateModule's package inv, which the
// base does not have; its test's name is a canary.
const hiddenInv = "package inv\n\nimport \"testing\"\n\nfunc TestHiddenInvCanary(t *testing.T) {}\n"

// invEvents is a go test -json stream of package inv's one test.
func invEvents(outcome string) string {
	pkg := gateModule + "/inv"
	return fmt.Sprintf(`{"Action":"start","Package":%q}`+"\n"+`{"Action":"run","Package":%q,"Test":"TestHiddenInvCanary"}`+"\n"+
		`{"Action":%q,"Package":%q,"Test":"TestHiddenInvCanary"}`+"\n"+`{"Action":%q,"Package":%q}`+"\n", pkg, pkg, outcome, pkg, outcome, pkg)
}

// overlaid is gateWorld's profile edit for the acceptance rows: its gate
// prints the tree's gate-events.json, then, only when the acceptance test
// is in the workspace at its repository path, the candidate's
// inv-events.json (what the hidden test did on the candidate's code).
func overlaid(p map[string]any) {
	p["gate"].(map[string]any)["command"] = []string{"sh", "-c", "cat gate-events.json; if test -f inv/inv_hidden_test.go; then cat inv-events.json; fi"}
}

// #226 entry 190 (§2.3 assertion 2, §2.6): adcd's gate overlays the task's
// acceptance tests, read from the blob store, at their repository paths on
// the candidate's run only (the base is measured without them), and
// expects the tests they declare: a candidate whose hidden test passes is
// clean; one whose hidden test fails, or never reports it, is
// found/gate_test_failed, a candidate refused. The result derives from the
// acceptance blob (its lineage: the builder never takes it), and no
// operator read holds the acceptance's digest or its test's name (I18, the
// canary rule).
func TestTheGateOverlaysTheAcceptance(t *testing.T) {
	for name, c := range map[string]struct {
		files   map[string]string
		outcome types.Outcome
		reason  vocab.Reason
	}{
		"the hidden test passes":        {map[string]string{"inv-events.json": invEvents("pass")}, types.OutcomeEmpty, vocab.ReasonCompletedClean},
		"the hidden test fails":         {map[string]string{"inv-events.json": invEvents("fail")}, types.OutcomeFound, vocab.ReasonGateTestFailed},
		"the hidden test never reports": {map[string]string{"README.md": "hello\n"}, types.OutcomeFound, vocab.ReasonGateTestFailed},
	} {
		t.Run(name, func(t *testing.T) {
			withAcceptance(t, map[string]string{"inv/inv_hidden_test.go": hiddenInv})
			rw := gateWorldWith(t, goEvents("TestA=pass"), c.files, nil, overlaid)
			v := rw.gating(t)
			if v.State.State != types.NodeStateStateCompleted {
				t.Fatalf("the gate node: %s, holds %+v", v.State.State, v.State.Holds)
			}
			rr := selectedOf(t, rw.w, v)
			if rr.Result.Outcome != c.outcome || rr.Result.Reason != c.reason {
				t.Errorf("the gate's result: %s/%s %q", rr.Result.Outcome, rr.Result.Reason, rr.Result.Detail)
			}
			d := blobDigest([]byte(hiddenInv))
			for _, ref := range rr.Result.Value {
				if !slices.Equal(ref.Lineage, []string{d}) {
					t.Errorf("the gate's %s derives from %v, not the acceptance blob", ref.Type, ref.Lineage)
				}
			}
			restartOn(t, rw).noLeak(t, rw.task, d, strings.TrimPrefix(d, "sha256:"), "TestHiddenInvCanary", "inv_hidden_test")
		})
	}
}

// #226 entry 190: an acceptance test the gate cannot overlay is the task's
// input, no retry fixes it: the gate node is held "runner: failed" before
// any run, its result never recorded: one at a path the profile protects,
// one that would replace a file the base holds, one under such a file, and
// one whose blob is not in the store.
func TestTheGateRefusesAnAcceptanceItCannotOverlay(t *testing.T) {
	for name, c := range map[string]struct {
		at   string
		lost bool
	}{
		"a protected path":         {at: "conftest.py"},
		"a file of the base":       {at: "go.mod"},
		"under a file of the base": {at: "go.mod/x_test.go"},
		"a blob not in the store":  {at: "inv/inv_hidden_test.go", lost: true},
	} {
		t.Run(name, func(t *testing.T) {
			withAcceptance(t, map[string]string{c.at: hiddenInv})
			rw := gateWorldWith(t, goEvents("TestA=pass"), map[string]string{"inv-events.json": invEvents("pass")}, nil, overlaid)
			if c.lost {
				if err := rw.w.blobs.Remove(blobDigest([]byte(hiddenInv))); err != nil {
					t.Fatal(err)
				}
			}
			v := rw.gating(t)
			if v.State.State != types.NodeStateStateHeld || len(v.State.Holds) != 1 || v.State.Holds[0].SignalInstance != runnerFailed || v.State.Result != nil {
				t.Errorf("the gate node: %s, holds %+v, result %v", v.State.State, v.State.Holds, v.State.Result)
			}
		})
	}
}
