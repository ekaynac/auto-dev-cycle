// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/accept"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session/inproc"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/wire"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/brief"
	"github.com/ekaynac/auto-dev-cycle/internal/dispatch"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/intent"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// builderDescribe is the in-process builder's describe result.
const builderDescribe = `{"harness":"daemon-peer","harness_version":"1.0.0","adapter_version":"0.1.0","contract_version":"1.0.0",` +
	`"capabilities":["patch"],"session_semantics":"fresh_process","tool_surface":[],` +
	`"telemetry":{"usage":true,"upstream":false,"stream_deltas":false,"per_turn_coverage":false},"host_side":false,"needs_native_login":false}`

// builderClean is its clean answer to prepare and shutdown.
const builderClean = `{"outcome":"empty","reason":"completed_clean","detail":""}`

// builder is a builder adapter in this process: it answers describe,
// prepare and shutdown; it sends the prompt it was prepared with on
// prompts; on run it calls edit over its workspace, then reports empty and
// clean (the controller's diff decides), or, when noResult, ends the
// session with no result.
func builder(edit func(workspace string), noResult bool, prompts chan<- []byte) *inproc.Peer {
	return inproc.New(func(in io.Reader, out io.Writer) {
		r, w := wire.NewReader(in), wire.NewWriter(out)
		var prep types.AdapterPrepareParams
		for {
			line, err := r.Next()
			var req wire.Request
			if err != nil || json.Unmarshal(line, &req) != nil {
				return
			}
			res := json.RawMessage(builderClean)
			switch req.Method {
			case "describe":
				res = json.RawMessage(builderDescribe)
			case "prepare":
				if json.Unmarshal(req.Params, &prep) != nil {
					return
				}
				if prompts != nil {
					b, _ := os.ReadFile(filepath.Join(prep.Exchange.In, adapterhost.PromptName))
					prompts <- b
				}
			case "run":
				if edit != nil {
					edit(prep.Workspace.Path)
				}
				if noResult {
					return
				}
				if res, err = json.Marshal(types.AdapterRunResult{Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean}); err != nil {
					return
				}
			}
			b, err := wire.Encode(map[string]any{"jsonrpc": wire.Version, "id": req.ID, "result": res})
			if err != nil || w.Write(b) != nil {
				return
			}
		}
	})
}

// testLog logs to t.
type testLog struct{ t *testing.T }

func (l testLog) Write(b []byte) (int, error) {
	l.t.Log(strings.TrimSpace(string(b)))
	return len(b), nil
}

// peerStarter starts peer as every attempt's adapter.
type peerStarter struct{ peer *inproc.Peer }

func (p peerStarter) Start(run AdapterRun) (AdapterSession, error) {
	return AdapterSession{Config: session.Config{InProcess: p.peer, Deadline: run.Deadline}, Path: func(s string) string { return s }}, nil
}

// countRuns is the runner the loop calls, counting the runs it started.
type countRuns struct {
	r       *runner
	mu      sync.Mutex
	started int
}

func (c *countRuns) Run(node types.NodeID, l types.LeaseID) bool {
	ok := c.r.Run(node, l)
	c.mu.Lock()
	defer c.mu.Unlock()
	if ok {
		c.started++
	}
	return ok
}

func (c *countRuns) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started
}

// runWorld is the loop and adcd's runner over a project whose repository
// is a local one holding a.go, the stub builder its builder: its images
// present when images, peer every attempt's adapter, a gateway that only
// records its ends. ended is called at each end.
type runWorld struct {
	w      *seeder
	l      *reconcile.Loop
	r      *runner
	runs   *countRuns
	task   types.TaskID
	build  types.NodeID
	tmp    string
	exch   string
	ended  func(gateway.Binding)
	opened func(gateway.Binding)
	nudged atomic.Int32 // the runs that nudged the loop
}

func newRunWorld(t *testing.T, images bool, peer *inproc.Peer) *runWorld {
	t.Helper()
	tmp := t.TempDir()
	src := sourceRepo(t, tmp, "src")
	perDay := int64(1 << 50)
	w, sys, x, pl := placingWith(t, &perDay, func(p map[string]any) {
		p["repo"] = map[string]any{"url": src, "base_ref": "refs/heads/main"}
	})
	pl.d.Images, pl.d.Credentials = fakeImages{images}, fakeCredentials{}
	x.Placer = pl
	rw := &runWorld{w: w, tmp: tmp, exch: filepath.Join(tmp, "exchange")}
	socks := filepath.Join(tmp, "gw")
	for _, d := range []string{rw.exch, socks} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	rw.r = &runner{ctx: context.Background(), store: w.s, system: x.System, blobs: w.blobs, place: pl, env: x.Env,
		host: &adapterhost.Host{Store: w.s, Blobs: w.blobs, Actor: string(x.System.Actor()), Principal: x.System, Root: rw.exch},
		box:  sandbox.Host{Root: filepath.Join(tmp, "sbx")}, clones: filepath.Join(tmp, "clones"), sockets: socks,
		gateway: func(b gateway.Binding, _ string) (types.AdapterPrepareParamsGatewayValue, attemptEnd, error) {
			if rw.opened != nil {
				rw.opened(b)
			}
			return types.AdapterPrepareParamsGatewayValue{BaseUrl: "http://gateway.invalid", Token: "a-test-token-of-32-bytes-or-more"}, func(context.Context) (gateway.Observed, error) {
				if rw.ended != nil {
					rw.ended(b)
				}
				return gateway.Observed{}, nil
			}, nil
		},
		adapters: peerStarter{peer}, log: testLog{t}, done: func() { rw.nudged.Add(1) }}
	pl.ready = rw.r.cleared // the runner's holds clear as adcd's do
	x.Placer = pl
	rw.runs = &countRuns{r: rw.r}
	x.Runner = rw.runs
	rw.l = reconcile.NewLoop(x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	_, rw.task = readyTask(t, w, sys)
	return rw
}

// untilRun runs passes until the loop starts a run (at most 10), then waits
// for it to end.
func (rw *runWorld) untilRun(t *testing.T) {
	t.Helper()
	for range 10 {
		if rw.runs.count() > 0 {
			break
		}
		if _, err := rw.l.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	rw.r.Wait()
	if rw.runs.count() != 1 {
		t.Fatalf("%d runs started, want 1", rw.runs.count())
	}
	rw.build = templateNodes(rw.w, rw.task)[types.NodeKindBuild]
}

// attemptOf is the build's first attempt and its lease.
func (rw *runWorld) attemptOf(t *testing.T) (types.Attempt, types.Lease) {
	t.Helper()
	var a types.Attempt
	var l types.Lease
	rw.w.view(func(tx *eventstore.Tx) error {
		first, ok, err := lease.FirstOn(tx, graph.UnitOf(rw.build))
		if err != nil || !ok {
			return errors.Join(err, errors.New("no attempt started"))
		}
		if a, _, _, err = lease.AttemptAt(tx, first); err != nil {
			return err
		}
		l, _, _, err = lease.LeaseOf(tx, a.Lease)
		return err
	})
	return a, l
}

// P22a-5c-2a (W15, W16; #226 entry 126): the loop runs a placed build's
// attempt through adcd's runner. The builder is prepared with the plan's
// brief as its prompt, its title and requirement and nothing else, and
// runs in the project's repository at its base; the gateway ends after the
// run and before the result is recorded. The controller's own diff of the
// workspace decides the result:
//   - an edit: patch/found, recorded by the attempt (graph.CommitAttempt),
//     the diff its value; the attempt sealed and completed, its lease
//     completed and settled, the node completed;
//   - a tree holding a .git look-alike: failed/unsafe_output, recorded by
//     the attempt, never a diff;
//   - no result (the adapter left): the attempt lost and sealed,
//     could_not_run recorded with no attempt, its lease revoked and
//     settled; the node stays claimed and the next pass matches it again.
//
// Each leaves no workspace, clone or exchange behind.
func TestTheRunCompletesTheBuild(t *testing.T) {
	for name, c := range map[string]struct {
		edit     func(string)
		noResult bool
		want     types.Outcome
		reason   vocab.Reason
	}{
		"an edit": {edit: func(ws string) {
			_ = os.WriteFile(filepath.Join(ws, "a.go"), []byte("package a\n\nconst Verbose = true\n"), 0o600)
		},
			want: types.OutcomeFound, reason: vocab.ReasonCompletedWithFindings},
		"a .git look-alike": {edit: func(ws string) {
			_ = os.MkdirAll(filepath.Join(ws, ".GIT"), 0o700)
			_ = os.WriteFile(filepath.Join(ws, ".GIT", "config"), []byte("[core]\n"), 0o600)
		}, want: types.OutcomeFailed, reason: vocab.ReasonUnsafeOutput},
		"no result": {noResult: true, want: types.OutcomeCouldNotRun, reason: vocab.ReasonAdapterProtocolError},
	} {
		t.Run(name, func(t *testing.T) {
			prompts := make(chan []byte, 1)
			rw := newRunWorld(t, true, builder(c.edit, c.noResult, prompts))
			var endedEarly []string
			rw.ended = func(b gateway.Binding) {
				rw.w.view(func(tx *eventstore.Tx) error {
					_, recorded, err := graph.AttemptResult(tx, b.Attempt)
					if a, _, _, aerr := lease.AttemptAt(tx, b.Attempt); recorded || aerr != nil || a.Status == types.AttemptStatusRunning {
						endedEarly = append(endedEarly, string(a.Status))
					}
					return err
				})
			}
			rw.untilRun(t)
			if len(endedEarly) != 0 {
				t.Errorf("the gateway ended while the attempt ran or after its result: %v", endedEarly)
			}
			var want []byte
			rw.w.view(func(tx *eventstore.Tx) error {
				task, err := authority.TaskOf(tx, rw.task)
				if err != nil {
					return err
				}
				raw, _, err := authority.ProjectDataPolicy(tx, task.Project)
				if err != nil {
					return err
				}
				policy, err := brief.Restriction(task.Project, raw)
				if err != nil {
					return err
				}
				b, _, err := brief.Of(task, policy.PolicyDigest)
				if err != nil {
					return err
				}
				p, err := brief.Decode(b)
				want = brief.Prompt(p)
				return err
			})
			select {
			case got := <-prompts:
				if !bytes.Equal(got, want) {
					t.Errorf("the builder's prompt %q, want the brief's %q", got, want)
				}
			default:
				t.Fatal("the builder was never prepared")
			}
			a, l := rw.attemptOf(t)
			var res types.Result
			var byAttempt bool
			rw.w.view(func(tx *eventstore.Tx) (err error) {
				res, byAttempt, err = graph.AttemptResult(tx, a.ID)
				return err
			})
			st := nodesOf(rw.w, rw.task)[types.NodeKindBuild]
			switch c.want {
			case types.OutcomeCouldNotRun:
				if byAttempt || a.Status != types.AttemptStatusSealed || a.LostReason == nil || l.State != types.LeaseStateSettled ||
					st.State.State != types.NodeStateStateClaimed {
					t.Fatalf("no result: by the attempt %v, attempt %+v, lease %s, node %s", byAttempt, a, l.State, st.State.State)
				}
				if _, err := rw.l.Pass(context.Background()); err != nil {
					t.Fatal(err)
				}
				if next, live := leaseOn(rw.w, rw.build); !live || next.ID == l.ID {
					t.Errorf("the build was not matched again: %v %+v", live, next)
				}
			default:
				if !byAttempt || res.Outcome != c.want || res.Reason != c.reason || a.Status != types.AttemptStatusCompleted ||
					a.Completion == nil || a.Completion.Outcome != c.want || l.State != types.LeaseStateSettled || st.State.State != types.NodeStateStateCompleted {
					t.Fatalf("the result %+v (by the attempt %v), attempt %+v, lease %s, node %s", res, byAttempt, a, l.State, st.State.State)
				}
				if c.want == types.OutcomeFound {
					patch, err := blobOf(t, rw.w, res.Value[0].ContentDigest)
					if err != nil || !bytes.Contains(patch, []byte("+const Verbose = true")) {
						t.Errorf("the recorded diff: %s, %v", patch, err)
					}
				} else if len(res.Value) != 0 {
					t.Errorf("an unsafe tree recorded a value: %+v", res.Value)
				}
			}
			for _, dir := range []string{rw.r.clones, rw.exch, filepath.Join(rw.r.box.Root, "runs")} {
				if !empty(dir) {
					t.Errorf("%s left behind", dir)
				}
			}
		})
	}
}

// P22a-5c-2a: a launch that misses (the image missing) starts nothing: the
// prepared exchange is discarded, the lease released and the node held
// wrong_owner by the launch.
func TestALaunchMissStartsNothing(t *testing.T) {
	rw := newRunWorld(t, false, builder(nil, false, nil))
	rw.untilRun(t)
	if _, ok, err := firstAttempt(rw); err != nil || ok {
		t.Fatalf("a missed launch started an attempt: %v %v", ok, err)
	}
	st := nodesOf(rw.w, rw.task)[types.NodeKindBuild].State
	if len(st.Holds) != 1 || st.Holds[0].Hold != vocab.HoldWrongOwner {
		t.Errorf("the node after a miss: %+v", st)
	}
	if _, live := leaseOn(rw.w, rw.build); live {
		t.Error("the missed launch left its lease live")
	}
	if !empty(rw.exch) {
		t.Error("the prepared exchange was not discarded")
	}
}

// firstAttempt is the build's first attempt, if one started.
func firstAttempt(rw *runWorld) (types.AttemptID, bool, error) {
	var id types.AttemptID
	var ok bool
	var err error
	rw.w.view(func(tx *eventstore.Tx) error {
		id, ok, err = lease.FirstOn(tx, graph.UnitOf(rw.build))
		return nil
	})
	return id, ok, err
}

// Mythos 22a-5c-1 M2; Opus 22a-5c-2a H2: a failure between the grant and
// the launch (here the exchange root is missing) ends the lease in the same
// run, its start refused and the lease released, and holds the build
// source_invalid_config on the runner's directories, so passes under the
// failure place no other lease (78 in 3 s before the hold). Once the
// directory is there, the loop clears the hold and matches the build again.
func TestAFailureBeforeTheLaunchEndsTheLease(t *testing.T) {
	rw := newRunWorld(t, true, builder(nil, false, nil))
	root := rw.r.host.Root
	rw.r.host.Root = filepath.Join(rw.tmp, "missing")
	rw.untilRun(t)
	leases := func() []string {
		var ids []string
		rw.w.view(func(tx *eventstore.Tx) (err error) {
			ids, err = tx.Keys(lease.LeaseProjection, "")
			return err
		})
		return ids
	}
	first := leases()
	var l types.Lease
	var free bool
	rw.w.view(func(tx *eventstore.Tx) (err error) {
		if len(first) == 1 {
			if l, _, _, err = lease.LeaseOf(tx, types.LeaseID(first[0])); err != nil {
				return err
			}
		}
		free, err = dispatch.Unplaced(tx, graph.UnitOf(rw.build))
		return err
	})
	st := nodesOf(rw.w, rw.task)[types.NodeKindBuild].State
	if len(first) != 1 || l.State != types.LeaseStateReleased || !free || len(st.Holds) != 1 || st.Holds[0].Hold != vocab.HoldSourceInvalidConfig ||
		st.Holds[0].SignalInstance != reconcile.RunnerSignal("dirs") {
		t.Fatalf("after a failed prepare: %d leases, the lease %s, unplaced %v, node %+v", len(first), l.State, free, st)
	}
	if _, ok, err := firstAttempt(rw); err != nil || ok {
		t.Fatalf("an attempt started: %v %v", ok, err)
	}
	for range 5 {
		if _, err := rw.l.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		rw.r.Wait()
	}
	if n := len(leases()); n != 1 || rw.runs.count() != 1 || rw.nudged.Load() != 0 {
		t.Fatalf("passes under the failure placed %d leases and %d runs (%d nudges), want 1 and none", n, rw.runs.count(), rw.nudged.Load())
	}
	rw.r.host.Root = root // the configuration fixed: the hold clears, the build is matched again
	for range 3 {
		if _, err := rw.l.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if next, live := leaseOn(rw.w, rw.build); !live || next.ID == types.LeaseID(first[0]) {
		t.Errorf("the build was not matched again once configured: %v %+v", live, next)
	}
	rw.r.Wait()
}

// P22a-5c-2a: the runner starts one run per build: a second Run while the
// first goes starts nothing.
func TestOneRunPerBuild(t *testing.T) {
	rw := newRunWorld(t, true, builder(nil, false, nil))
	hold, held := make(chan struct{}), make(chan gateway.Binding, 1)
	rw.opened = func(b gateway.Binding) {
		held <- b
		<-hold
	}
	for range 10 {
		if rw.runs.count() > 0 {
			break
		}
		if _, err := rw.l.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	b := <-held
	build := templateNodes(rw.w, rw.task)[types.NodeKindBuild]
	if rw.r.Run(build, b.Lease) {
		t.Error("a second run started while the first goes")
	}
	if _, err := rw.l.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(hold)
	rw.r.Wait()
	if rw.runs.count() != 1 {
		t.Errorf("%d runs started", rw.runs.count())
	}
}

// P22a-5c-3 with 5c-2a: a forged plan brief is never handed to the
// builder: the run's plan refuses it by the I18 walk (intent.ErrNotTheBrief)
// before it reads anything else.
func TestTheRunRefusesAForgedBrief(t *testing.T) {
	w, x, _, build := forgedPlan(t, ofTask(func(x *types.Task) { x.Spec.Requirement = "CANARY-ACCEPTANCE" }))
	r := &runner{store: w.s, blobs: w.blobs, env: x.Env}
	if _, err := r.plan(context.Background(), build, "lea_01m3250v57fhm9v1z9xd1wk8gy"); !errors.Is(err, intent.ErrNotTheBrief) ||
		strings.Contains(err.Error(), "CANARY") {
		t.Errorf("the run's plan over a forged brief: %v", err)
	}
}

// P22a-5c-2a: a host error (here the host reads another store, so it
// finds no started attempt and runs nothing) leaves the attempt running;
// the runner loses it (agent_disconnect), seals it, records could_not_run
// with no attempt, and ends its lease.
func TestTheRunnerLosesAnAttemptTheHostLeftRunning(t *testing.T) {
	rw := newRunWorld(t, true, builder(nil, false, nil))
	rw.r.host.Store = seed(t, stateDir(t), daemonProjection(t)).s
	rw.untilRun(t)
	a, l := rw.attemptOf(t)
	if a.Status != types.AttemptStatusSealed || a.LostReason == nil || *a.LostReason != types.AttemptLostReasonValueAgentDisconnect || l.State != types.LeaseStateSettled {
		t.Fatalf("the attempt the host left running: %+v, lease %s", a, l.State)
	}
}

// The lead's ruling (22a-5d, #374): a session whose end is unverified (a
// contained adapter's removal failed; session.ErrUnverified) keeps its
// attempt directory and the cid file there, the start sweep's handle on a
// container that may still run; its tree is never diffed, so the build's
// edit is not recorded and, though the attempt submitted a clean result,
// the result is could_not_run/ambiguous_state, recorded with no attempt
// (Opus 22a-5c-2a r2 N1). The clone, which nothing mounts, is removed.
func TestAnUnverifiedEndKeepsTheAttemptDirectory(t *testing.T) {
	rw := newRunWorld(t, true, builder(func(ws string) {
		_ = os.WriteFile(filepath.Join(ws, "a.go"), []byte("package a\n\nconst X = 1\n"), 0o600)
	}, false, nil))
	runs := filepath.Join(rw.r.box.Root, "runs")
	var cid string
	runPrepared = func(p *adapterhost.Prepared, ctx context.Context) (adapterhost.Outcome, error) {
		out, err := p.Run(ctx)
		dirs, _ := os.ReadDir(runs)
		if len(dirs) == 1 { // the container's handle, as 22a-5d's start writes it
			cid = filepath.Join(runs, dirs[0].Name(), "cid")
			_ = os.WriteFile(cid, []byte("0123456789abcdef\n"), 0o600)
		}
		out.Session = session.ErrUnverified // its submission kept (Opus 22a-5c-2a r2 N1)
		return out, err
	}
	t.Cleanup(func() { runPrepared = (*adapterhost.Prepared).Run })
	rw.untilRun(t)
	if _, err := os.Stat(cid); cid == "" || err != nil {
		t.Errorf("the attempt directory's cid after an unverified end: %q, %v", cid, err)
	}
	if !empty(rw.r.clones) {
		t.Error("the clone was kept")
	}
	rw.endedNotRun(t, vocab.ReasonAmbiguousState) // never empty/completed_clean from the submission
}

// ended reports the build's attempt sealed and its lease settled, with the
// result could_not_run for reason, recorded with no attempt.
func (rw *runWorld) endedNotRun(t *testing.T, reason vocab.Reason) {
	t.Helper()
	a, l := rw.attemptOf(t)
	var res types.Result
	var byAttempt bool
	rw.w.view(func(tx *eventstore.Tx) error {
		var err error
		if _, byAttempt, err = graph.AttemptResult(tx, a.ID); err != nil {
			return err
		}
		keys, err := tx.Keys(graph.ResultProjection, "")
		for _, id := range keys {
			var rr types.ResultRecorded
			b, _, gerr := tx.Get(graph.ResultProjection, id)
			if gerr == nil && json.Unmarshal(b, &rr) == nil && rr.Node == rw.build {
				res = rr.Result
			}
		}
		return err
	})
	if a.Status != types.AttemptStatusSealed || l.State != types.LeaseStateSettled || byAttempt || res.Outcome != types.OutcomeCouldNotRun || res.Reason != reason {
		t.Errorf("the attempt %s, the lease %s, the result %+v (by the attempt %v); want sealed, settled, could_not_run/%s", a.Status, l.State, res, byAttempt, reason)
	}
}

// Opus 22a-5c-2a H1: the controller's diff failing after the attempt
// submitted still ends it: sealed, could_not_run/ambiguous_state recorded
// with no attempt, the lease revoked and settled.
func TestADiffFailureStillEndsTheAttempt(t *testing.T) {
	rw := newRunWorld(t, true, builder(func(ws string) {
		_ = os.WriteFile(filepath.Join(ws, "a.go"), []byte("package a\n\nconst X = 1\n"), 0o600)
	}, false, nil))
	diffOf = func(context.Context, sandbox.Repo, string, string) ([]byte, []string, error) {
		return nil, nil, errors.New("the controller repository's configuration changed")
	}
	t.Cleanup(func() { diffOf = accept.Diff })
	rw.untilRun(t)
	rw.endedNotRun(t, vocab.ReasonAmbiguousState)
}

// Opus 22a-5c-2a H1: the daemon's context cancelled mid-run (a shutdown)
// still ends the attempt: its completion reads and records under a context
// of its own, so the attempt is sealed, its lease settled, and its result
// the exit map's (no result: adapter_protocol_error), its facts read.
func TestACancelledRunStillEndsTheAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rw := newRunWorld(t, true, builder(func(string) { cancel() }, true, nil))
	rw.r.ctx = ctx
	rw.untilRun(t)
	rw.endedNotRun(t, vocab.ReasonAdapterProtocolError) // the facts were read: never store_unreadable
}

// Opus 22a-5c-2a M2: a panic in a step after the launch is recovered and
// logged; the attempt is still ended (sealed, could_not_run/ambiguous_state,
// the lease settled), and the build's run is no longer live.
func TestAPanickingRunStillEndsTheAttempt(t *testing.T) {
	rw := newRunWorld(t, true, builder(nil, false, nil))
	runPrepared = func(p *adapterhost.Prepared, _ context.Context) (adapterhost.Outcome, error) {
		_ = p.Discard() // as Run removes its exchange on any end
		panic("a step's bug")
	}
	t.Cleanup(func() { runPrepared = (*adapterhost.Prepared).Run })
	rw.untilRun(t)
	rw.endedNotRun(t, vocab.ReasonAmbiguousState)
	rw.r.mu.Lock()
	live := rw.r.live[rw.build]
	rw.r.mu.Unlock()
	if live {
		t.Error("the panicked run is still live")
	}
}

// Opus 22a-5c-2a L1, L2; Mythos L2: a .git anywhere, at the top too
// (CopyTree would drop it from the patch), a FIFO (dropped as well), and a
// symlink whose target leaves the tree are unsafe_output, never a found
// patch.
func TestScreen(t *testing.T) {
	for name, c := range map[string]struct {
		make   func(root string) error
		unsafe bool
	}{
		"a clean tree": {func(root string) error { return os.WriteFile(filepath.Join(root, "a.go"), nil, 0o600) }, false},
		"a top-level .git (Mythos L2)": {func(root string) error {
			return os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o700)
		}, true},
		"a FIFO (Mythos L2)": {func(root string) error { return syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600) }, true},
		"a nested .git": {func(root string) error {
			if err := os.MkdirAll(filepath.Join(root, "sub", ".git"), 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(root, "sub", ".git", "config"), []byte("[core]\n\tfsmonitor = x\n"), 0o600)
		}, true},
		"a symlink to /etc/passwd":  {func(root string) error { return os.Symlink("/etc/passwd", filepath.Join(root, "evil")) }, true},
		"a symlink up and out":      {func(root string) error { return os.Symlink("../../outside", filepath.Join(root, "evil")) }, true},
		"a symlink within the tree": {func(root string) error { return os.Symlink("a.go", filepath.Join(root, "link")) }, false},
		"a chain through d -> .": {func(root string) error { // e -> d/.. is the tree's parent (r2 L1)
			if err := os.Symlink(".", filepath.Join(root, "d")); err != nil {
				return err
			}
			return os.Symlink("d/..", filepath.Join(root, "e"))
		}, true},
		"a chain through x/up -> ..": {func(root string) error {
			if err := os.MkdirAll(filepath.Join(root, "x"), 0o700); err != nil {
				return err
			}
			if err := os.Symlink("..", filepath.Join(root, "x", "up")); err != nil {
				return err
			}
			return os.Symlink("x/up/x/up/..", filepath.Join(root, "f"))
		}, true},
		"a nested symlink up, still in it": {func(root string) error {
			if err := os.MkdirAll(filepath.Join(root, "d"), 0o700); err != nil {
				return err
			}
			return os.Symlink("../a.go", filepath.Join(root, "d", "link"))
		}, false},
	} {
		root := t.TempDir()
		if err := c.make(root); err != nil {
			t.Fatal(err)
		}
		why, err := screen(root)
		if err != nil || (why != "") != c.unsafe {
			t.Errorf("%s: %q, %v; unsafe %v", name, why, err, c.unsafe)
		}
	}
}

// failingGateway makes rw's gateway fail its first n opens (all when n < 0).
func (rw *runWorld) failingGateway(n int) {
	open := rw.r.gateway
	var mu sync.Mutex
	rw.r.gateway = func(b gateway.Binding, socket string) (types.AdapterPrepareParamsGatewayValue, attemptEnd, error) {
		mu.Lock()
		fail := n != 0
		if n > 0 {
			n--
		}
		mu.Unlock()
		if fail {
			return types.AdapterPrepareParamsGatewayValue{}, nil, errors.New("the gateway's socket could not be made")
		}
		return open(b, socket)
	}
}

// heldOn is the build's runner hold signal, "" when it holds none.
func (rw *runWorld) heldOn() string {
	for _, h := range nodesOf(rw.w, rw.task)[types.NodeKindBuild].State.Holds {
		if reconcile.IsRunnerSignal(h.SignalInstance) {
			return h.SignalInstance
		}
	}
	return ""
}

// passesFor runs n passes, each after wait, waiting for the runs each
// starts.
func (rw *runWorld) passesFor(t *testing.T, n int, wait time.Duration) {
	t.Helper()
	for range n {
		time.Sleep(wait)
		if _, err := rw.l.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		rw.r.Wait()
	}
}

// Opus 22a-5c-2a r2 M1: a failure before the launch that is no
// configuration fault (here the gateway's socket, once) holds the build on
// a bounded retry, "retry 1 until <time>", which does not clear before its
// time; after it, the loop clears it, runs the build again, and completes
// it.
func TestATransientFailureIsRetried(t *testing.T) {
	rw := newRunWorld(t, true, builder(func(ws string) {
		_ = os.WriteFile(filepath.Join(ws, "a.go"), []byte("package a\n\nconst X = 1\n"), 0o600)
	}, false, nil))
	rw.failingGateway(1)
	retryBase = 500 * time.Millisecond
	t.Cleanup(func() { retryBase = time.Minute })
	rw.untilRun(t)
	if h := rw.heldOn(); !strings.HasPrefix(h, reconcile.RunnerSignal("retry 1 until ")) {
		t.Fatalf("the hold after a transient failure: %q", h)
	}
	rw.passesFor(t, 3, 0)
	if rw.runs.count() != 1 {
		t.Fatalf("the retry ran before its time: %d runs", rw.runs.count())
	}
	time.Sleep(retryBase)
	rw.passesFor(t, 4, 0)
	if st := nodesOf(rw.w, rw.task)[types.NodeKindBuild].State; rw.runs.count() != 2 || st.State != types.NodeStateStateCompleted {
		t.Fatalf("after the retry's time: %d runs, the build %s", rw.runs.count(), st.State)
	}
	rw.r.mu.Lock()
	defer rw.r.mu.Unlock()
	if n := rw.r.tries[rw.build]; n != 0 {
		t.Errorf("the launch left %d failures counted", n)
	}
}

// Opus 22a-5c-2a r2 M1: failures before the launch in a row back off, each
// retry waiting twice the last; the fifth spends the retries: "failed",
// which nothing clears (the recovery: revoke the task's approval), and no
// pass runs the build again.
func TestFiveFailuresSpendTheRetries(t *testing.T) {
	rw := newRunWorld(t, true, builder(nil, false, nil))
	rw.failingGateway(-1)
	retryBase = time.Millisecond
	t.Cleanup(func() { retryBase = time.Minute })
	rw.untilRun(t)
	for i := 0; i < 40 && rw.heldOn() != runnerFailed; i++ {
		rw.passesFor(t, 1, 20*time.Millisecond)
	}
	if rw.heldOn() != runnerFailed || rw.runs.count() != maxTries {
		t.Fatalf("after the failures: held on %q, %d runs; want failed after %d", rw.heldOn(), rw.runs.count(), maxTries)
	}
	rw.passesFor(t, 3, 20*time.Millisecond)
	if rw.runs.count() != maxTries {
		t.Errorf("a spent build ran again: %d runs", rw.runs.count())
	}
}

// Opus 22a-5c-2a r3 L1: an unverified end reported through Run's error
// alone, its edit submitted, is abandoned as one through the session.
func TestAnUnverifiedRunErrorIsAbandoned(t *testing.T) {
	rw := newRunWorld(t, true, builder(func(ws string) {
		_ = os.WriteFile(filepath.Join(ws, "a.go"), []byte("package a\n\nconst X = 1\n"), 0o600)
	}, false, nil))
	runPrepared = func(p *adapterhost.Prepared, ctx context.Context) (adapterhost.Outcome, error) {
		out, _ := p.Run(ctx)
		return out, session.ErrUnverified
	}
	t.Cleanup(func() { runPrepared = (*adapterhost.Prepared).Run })
	rw.untilRun(t)
	rw.endedNotRun(t, vocab.ReasonAmbiguousState)
}

// Opus 22a-5c-2a r3 L2: a refusal of what the build takes (the I18 walk's,
// here reported by the gateway's open) holds it "failed" at once: no retry
// fixes it.
func TestARefusedInputFailsAtOnce(t *testing.T) {
	rw := newRunWorld(t, true, builder(nil, false, nil))
	rw.r.gateway = func(gateway.Binding, string) (types.AdapterPrepareParamsGatewayValue, attemptEnd, error) {
		return types.AdapterPrepareParamsGatewayValue{}, nil, intent.ErrNotTheBrief
	}
	rw.untilRun(t)
	if h := rw.heldOn(); h != runnerFailed {
		t.Errorf("a refused input is held on %q, want %q", h, runnerFailed)
	}
}

// Opus 22a-5c-2a r3 L4: a hold that is not written spends no try: the next
// failure is the first retry.
func TestAnUnwrittenHoldSpendsNoTry(t *testing.T) {
	rw := newRunWorld(t, true, builder(nil, false, nil))
	rw.failingGateway(-1)
	raise = func(*eventstore.Tx, types.NodeID, types.NodeHold, *string) ([]eventstore.Event, error) {
		return nil, errors.New("the hold could not be written")
	}
	t.Cleanup(func() { raise = graph.Raise })
	rw.untilRun(t)
	rw.r.mu.Lock()
	n := rw.r.tries[rw.build]
	rw.r.mu.Unlock()
	if n != 0 || rw.heldOn() != "" {
		t.Fatalf("an unwritten hold spent %d tries (held on %q)", n, rw.heldOn())
	}
	raise = graph.Raise
	rw.passesFor(t, 3, 0)
	if h := rw.heldOn(); !strings.HasPrefix(h, reconcile.RunnerSignal("retry 1 until ")) {
		t.Errorf("the next failure is held on %q, want the first retry", h)
	}
}

// Mythos 22a-5c-2a M1: the output files .gitignore kept out of the
// controller's diff are sealed with the attempt (ignored_files), for the
// acceptance to report: the builder's gen/x.go, ignored by the .gitignore
// it wrote, is listed, and the patch holds its edit and its .gitignore.
func TestTheIgnoredFilesAreSealed(t *testing.T) {
	rw := newRunWorld(t, true, builder(func(ws string) {
		_ = os.WriteFile(filepath.Join(ws, ".gitignore"), []byte("gen/\n"), 0o600)
		_ = os.MkdirAll(filepath.Join(ws, "gen"), 0o700)
		_ = os.WriteFile(filepath.Join(ws, "gen", "x.go"), []byte("package gen\n"), 0o600)
		_ = os.WriteFile(filepath.Join(ws, "a.go"), []byte("package a\n\nconst X = 1\n"), 0o600)
	}, false, nil))
	rw.untilRun(t)
	a, _ := rw.attemptOf(t)
	if a.Checkpoint == nil {
		t.Fatalf("the attempt was not sealed: %+v", a)
	}
	b, err := blobOf(t, rw.w, *a.Checkpoint)
	var rec types.AttemptRecord
	if err == nil {
		err = json.Unmarshal(b, &rec)
	}
	if err != nil || rec.IgnoredFiles == nil || !slices.ContainsFunc(*rec.IgnoredFiles, func(f string) bool { return strings.HasPrefix(f, "gen/") }) {
		t.Errorf("the sealed record's ignored files: %v (%v)", rec.IgnoredFiles, err)
	}
}
