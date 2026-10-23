// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/accept"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session/inproc"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/wire"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/capability"
	"github.com/ekaynac/auto-dev-cycle/internal/cli"
	"github.com/ekaynac/auto-dev-cycle/internal/dispatch"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/qualify"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// launchPending runs f with launch pending (a test edit of the table, as
// before its activation): every reader adcd has derives it inactive.
func launchPending(t *testing.T, f func()) {
	t.Helper()
	withTable(t, func(tb []capability.Capability) {
		tb[slices.IndexFunc(tb, func(c capability.Capability) bool { return c.Name == "launch" })].Pending = []string{"a test"}
	}, f)
}

// upstream is a loopback provider the test opens. At each request it
// records the call's model_call effect as the store holds it then: the
// gateway intends a call before it sends (IntendEffect).
type upstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []eventstore.Effect // the live effects at each request
}

func newUpstream(t *testing.T, s *eventstore.Store) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var live []eventstore.Effect
		_ = s.View(r.Context(), func(tx *eventstore.Tx) error {
			ids, err := tx.LiveEffects(modelcall.Kind)
			for _, id := range ids {
				e, lerr := tx.LoadEffect(id)
				if lerr == nil {
					live = append(live, e)
				}
			}
			return err
		})
		u.mu.Lock()
		u.seen = append(u.seen, live...)
		u.mu.Unlock()
		_, _ = io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) requests() []eventstore.Effect {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]eventstore.Effect(nil), u.seen...)
}

// sent is what a builder's request through its gateway got: the status
// and the gateway's refusal, if any.
type sent struct {
	status  int
	refusal string
	err     error
}

// socketStarter starts the in-process builder as every attempt's adapter,
// as peerStarter does, keeping each attempt's gateway socket for it.
type socketStarter struct {
	mu      sync.Mutex
	sockets map[types.AttemptID]string
	peer    *inproc.Peer
}

func (s *socketStarter) Start(ctx context.Context, run AdapterRun) (AdapterSession, error) {
	s.mu.Lock()
	s.sockets[run.Attempt] = run.Socket
	s.mu.Unlock()
	return (&peerStarter{peer: s.peer}).Start(ctx, run)
}

func (s *socketStarter) socket(a types.AttemptID) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sockets[a]
}

// requestingBuilder is builder(writes(files)) whose run first sends one
// chat request over its attempt's gateway socket, with its gateway token,
// after before (when set), and reports what it got on out.
func requestingBuilder(files map[string]string, s *socketStarter, before func(), out chan<- sent) *inproc.Peer {
	return inproc.New(func(in io.Reader, w io.Writer) {
		r, wr := wire.NewReader(in), wire.NewWriter(w)
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
			case "run":
				writes(files)(prep.Workspace.Path)
				if before != nil {
					before()
				}
				out <- chatOn(s.socket(prep.Attempt), prep.Gateway)
				if res, err = json.Marshal(types.AdapterRunResult{Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean}); err != nil {
					return
				}
			}
			b, err := wire.Encode(map[string]any{"jsonrpc": wire.Version, "id": req.ID, "result": res})
			if err != nil || wr.Write(b) != nil {
				return
			}
		}
	})
}

// chatOn posts one chat completion over socket with gv's token.
func chatOn(socket string, gv *types.AdapterPrepareParamsGatewayValue) sent {
	if gv == nil {
		return sent{err: io.ErrUnexpectedEOF}
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	req, _ := http.NewRequest(http.MethodPost, gv.BaseUrl+"/chat/completions", strings.NewReader(`{"model":"stub-builder-1","messages":[],"max_tokens":64}`))
	req.Header.Set("Authorization", "Bearer "+gv.Token)
	resp, err := client.Do(req)
	if err != nil {
		return sent{err: err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return sent{status: resp.StatusCode, refusal: resp.Header.Get(gateway.RefusalHeader)}
}

// activated is a run world wired as adcd wires itself, launch active (this
// build's table, no Gates set by hand): adcd's egress
// (newEgress: its gateway, Recorder and Reconciler, the credential from the
// controller's environment) to a loopback upstream; adcd's runner
// (runnerOn), its adapter the in-process requesting builder and its
// sandbox one running commands in place; and adcd's own loop
// (reconciler: executorOf's placer, planner, Env and acceptor, the gates
// derived from the table). Only the processes' boundaries are fakes: the
// adapter's, the sandbox's commands, the image store and accept's lister.
type activated struct {
	rw   *runWorld
	d    *Daemon
	eg   *egress
	up   *upstream
	reg  registry.Registry
	gs   []types.ResourceGroup
	log  *logBuffer
	sent chan sent
	// peer, when set, is the adapter a start with a sandbox root runs
	// (activatedOn); else the requesting builder.
	peer *inproc.Peer
}

// activatedWorld is an activated world whose images resolve with images;
// with sandboxed false, adcd started with no sandbox root (no runner).
// before runs in the builder's run, before its request.
func activatedWorld(t *testing.T, images dispatch.Images, sandboxed bool, before func()) *activated {
	t.Helper()
	t.Setenv("ADC_CREDENTIAL_STUB", "a-stub-credential") // the controller's environment (gateway.EnvSecrets)
	files := map[string]string{"gate-events.json": goEvents("TestA=pass", "TestB=pass")}
	rw := newRunWorldWith(t, true, builder(nil, false, nil), goModule(t, goEvents("TestA=pass")))
	if _, err := rw.w.s.MarkPendingUncertain(context.Background(), string(rw.x.System.Actor())); err != nil { // adcd's start rule
		t.Fatal(err)
	}
	a := &activated{rw: rw, log: &logBuffer{}, sent: make(chan sent, 4), up: newUpstream(t, rw.w.s)}
	const config = "../../fixtures/m1/config/"
	raw, err := os.ReadFile(config + "groups.json")
	if err == nil {
		err = json.Unmarshal(raw, &a.gs)
	}
	doc, lerr := registry.Load(config + "registry-stub.json")
	if err != nil || lerr != nil {
		t.Fatal(err, lerr)
	}
	for i := range doc.Executors {
		doc.Executors[i].Destination.Endpoint = a.up.srv.URL + "/v1"
	}
	if a.reg, err = registry.New(doc, a.gs); err != nil {
		t.Fatal(err)
	}
	a.d = &Daemon{store: rw.w.s, blobs: rw.w.blobs, system: rw.x.System, clock: time.Now}
	if a.eg, err = newEgress(a.d, a.reg, false); err != nil {
		t.Fatal(err)
	}
	a.eg.log = a.log
	var run *runner
	if sandboxed {
		root, err := os.MkdirTemp("/dev/shm", "adcact")
		if err != nil {
			t.Fatalf("the sandbox root's runs/ needs a tmpfs of its own (/dev/shm): %v", err)
		}
		t.Cleanup(func() { os.RemoveAll(root) })
		acceptLister = func(sandbox.Host, sandbox.Image) accept.Lister { return nil }
		t.Cleanup(func() { acceptLister = defaultLister })
		starter := &socketStarter{sockets: map[types.AttemptID]string{}}
		starter.peer = requestingBuilder(files, starter, before, a.sent)
		box := sandbox.Host{Podman: "/usr/bin/podman", Root: root, Home: "/home/adc-gate-test", Runner: &inPlace{}}
		if run, err = runnerOn(context.Background(), a.d, rw.w.dir, box, images, a.reg, a.eg, starter, a.log); err != nil {
			t.Fatal(err)
		}
		rw.r = run
		t.Cleanup(run.Wait)
	}
	rw.l = reconciler(a.d, a.reg, images, a.eg, run, time.Hour, reviewsOf(a.reg, a.log), a.log)
	return a
}

// passes runs n of the loop's passes, each waiting for the runs it
// started, until done (nil: all n).
func (a *activated) passes(t *testing.T, n int, done func() bool) {
	t.Helper()
	for range n {
		if done != nil && done() {
			return
		}
		if _, err := a.rw.l.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		if a.rw.r != nil {
			a.rw.r.Wait()
		}
	}
}

// node is the task's node of kind (the last recorded).
func (a *activated) node(kind types.NodeKind) graph.NodeView {
	return nodesOf(a.rw.w, a.rw.task)[kind]
}

// The activation's gates come from the table alone: with launch pending
// (as before its activation), adcd's loop evaluates nothing, registers no
// model_call and wires no task.raise_rail; under this build's table, launch
// active, the same functions derive all three.
func TestTheGatesAreTheTables(t *testing.T) {
	launchPending(t, func() {
		if l := reconciler(&Daemon{}, registry.Registry{}, nil, nil, nil, 0, false, io.Discard); l.Gates.Evaluates || !l.Gates.Admits {
			t.Errorf("launch pending: gates %+v", l.Gates)
		}
		if ks, _ := effectKinds(); len(ks) != 0 {
			t.Errorf("launch pending: effect kinds %v", ks)
		}
		if _, ok := handlers()["task.raise_rail"]; ok {
			t.Error("launch pending: task.raise_rail wired")
		}
	})
	if l := reconciler(&Daemon{}, registry.Registry{}, nil, nil, nil, 0, false, io.Discard); !l.Gates.Evaluates || !l.Gates.Admits {
		t.Errorf("launch active: gates %+v", l.Gates)
	}
	if ks, _ := effectKinds(); ks[modelcall.Kind].RequestSchema != modelcall.Schema {
		t.Errorf("launch active: model_call not registered: %v", ks)
	}
	if _, ok := handlers()["task.raise_rail"]; !ok {
		t.Error("launch active: task.raise_rail not wired")
	}
	// The deferred matrix derives from the same table (Opus #414 L2):
	// task.raise_rail is wired, never also deferred.
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Matrix(reg)
	if err != nil {
		t.Fatalf("the matrix under launch active: %v", err)
	}
	if i := slices.IndexFunc(rows, func(r Row) bool { return r.Action == "task.raise_rail" }); i < 0 || rows[i].Class == Deferred {
		t.Errorf("task.raise_rail under launch active: %+v", rows[max(i, 0)])
	}
}

// A2 and A7 (the activation plan §3 A): launch through adcd's own loop.
// An admitted task's plan is recorded, its build placed and launched; the
// builder's one request goes through adcd's egress, its gateway intending
// the call before the send (the upstream finds its model_call effect
// pending at the request); the attempt's end settles it (out of pending,
// its usage kept); accept and the gate run. adcd then starts again on the
// state: its projections equal the log's replay, and verify-log passes.
func TestLaunchThroughAdcdsOwnLoop(t *testing.T) {
	a := activatedWorld(t, gateImages{}, true, nil)
	a.passes(t, 24, func() bool {
		g := a.node(types.NodeKindGate)
		return g.State.State == types.NodeStateStateCompleted
	})
	for _, kind := range []types.NodeKind{types.NodeKindPlan, types.NodeKindBuild, types.NodeKindAccept, types.NodeKindGate} {
		if v := a.node(kind); v.State.State != types.NodeStateStateCompleted {
			var results []types.ResultRecorded
			for _, k := range []types.NodeKind{types.NodeKindBuild, types.NodeKindAccept} {
				results = append(results, resultsOn(t, a.rw.w, a.node(k).Node.ID)...)
			}
			t.Fatalf("the %s: %s, holds %+v\nresults %+v\n%s", kind, v.State.State, v.State.Holds, results, a.log.String())
		}
	}
	if g := a.node(types.NodeKindGate); g.Outcome == nil || *g.Outcome != types.OutcomeEmpty {
		t.Errorf("the gate: %v", g.Outcome)
	}
	var got sent
	select {
	case got = <-a.sent:
	default:
		t.Fatal("the builder sent nothing")
	}
	if got.err != nil || got.status != http.StatusOK {
		t.Fatalf("the builder's request: %+v", got)
	}
	seen := a.up.requests()
	if len(seen) != 1 || seen[0].State != eventstore.EffectPending {
		t.Fatalf("the call's effect at the upstream's receipt: %+v", seen)
	}
	if e, err := a.rw.w.s.LoadEffect(context.Background(), seen[0].ID); err != nil || e.State == eventstore.EffectPending {
		t.Errorf("the call after the attempt's end: %+v %v", e, err)
	}
	dir := a.rw.w.dir
	a.rw.w.done()
	d, err := Start(context.Background(), Config{StateDir: dir, Groups: a.gs}) // Start refuses projections that differ from the replay
	if err != nil {
		t.Fatalf("adcd's start on the state: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := cli.Main(context.Background(), []string{"dev", "verify-log", "--state", dir}, &out, &errb, nil); code != entry.ExitOK {
		t.Errorf("verify-log: %d %s %s", code, out.String(), errb.String())
	}
}

// A3 (I21 Fold and Effect), through adcd's own loop.
//   - Fold: a launch refused (the project's image does not resolve) holds
//     the build wrong_owner and starts no attempt; a rebuild of every
//     projection from the log equals them, and the round's derived attempt
//     count is the same.
//   - Effect: the credential, held when the launch checked readiness, is
//     removed before the builder's request: the gateway refuses it
//     credential_not_held, and nothing reaches the upstream.
func TestLaunchReadinessThroughAdcd(t *testing.T) {
	t.Run("a refused launch rebuilds", func(t *testing.T) {
		a := activatedWorld(t, fakeImages{false}, true, nil)
		a.passes(t, 12, func() bool { return len(a.node(types.NodeKindBuild).State.Holds) > 0 })
		b := a.node(types.NodeKindBuild)
		if len(b.State.Holds) != 1 || b.State.Holds[0].Hold != vocab.HoldWrongOwner {
			t.Fatalf("the build after a refused launch: %s %+v\n%s", b.State.State, b.State.Holds, a.log.String())
		}
		count := func() int64 {
			var n int64
			a.rw.w.view(func(tx *eventstore.Tx) (err error) {
				var started bool
				if _, started, err = lease.FirstOn(tx, graph.UnitOf(b.Node.ID)); err == nil && started {
					t.Error("an attempt started on a refused launch")
				}
				if err == nil {
					n, err = graph.RoundAttempts(tx, b.Node.ID)
				}
				return err
			})
			return n
		}
		before := count()
		if diffs, err := a.rw.w.s.RebuildProjections(context.Background()); err != nil || len(diffs) > 0 {
			t.Fatalf("the rebuild: %v %+v", err, diffs)
		}
		if after := count(); after != before {
			t.Errorf("the round's attempts: %d live, %d rebuilt", before, after)
		}
	})
	t.Run("a credential removed after readiness", func(t *testing.T) {
		a := activatedWorld(t, gateImages{}, true, func() { os.Unsetenv("ADC_CREDENTIAL_STUB") })
		a.passes(t, 12, func() bool { return len(a.sent) > 0 })
		var got sent
		select {
		case got = <-a.sent:
		default:
			t.Fatalf("the builder sent nothing\n%s", a.log.String())
		}
		if got.err != nil || got.status != http.StatusServiceUnavailable || got.refusal != "credential_not_held" {
			t.Fatalf("the request after the credential's removal: %+v", got)
		}
		if n := len(a.up.requests()); n != 0 {
			t.Errorf("%d requests reached the upstream", n)
		}
	})
}

// #226 entry 171 (plan §0.3, A1): adcd with launch active and no sandbox
// root starts, says so, and holds every build no_sandbox, the operator's,
// visible on the task's card: nothing placed, no lease. A start with a
// sandbox root clears the hold (Unblock, runner.cleared) and the build runs.
func TestNoSandboxHoldsTheBuild(t *testing.T) {
	a := activatedWorld(t, gateImages{}, false, nil)
	a.passes(t, 12, func() bool { return len(a.node(types.NodeKindBuild).State.Holds) > 0 })
	b := a.node(types.NodeKindBuild)
	if b.State.State != types.NodeStateStateHeld || len(b.State.Holds) != 1 || b.State.Holds[0].Hold != vocab.HoldNoSandbox ||
		b.State.Holds[0].Owner != types.OwnerOperator {
		t.Fatalf("the build with no sandbox: %s %+v\n%s", b.State.State, b.State.Holds, a.log.String())
	}
	a.passes(t, 4, nil)
	if _, live := leaseOn(a.rw.w, b.Node.ID); live {
		t.Fatal("a lease with no sandbox")
	}
	a.rw.w.view(func(tx *eventstore.Tx) error { // Opus #414 L4: no Unblock offered while no runner could clear it
		s, err := reconcile.Observe(tx, a.rw.l.Gates)
		if err != nil || len(s.PlacingHeld) != 0 {
			t.Errorf("with no runner, offered to Unblock: %+v %v", s.PlacingHeld, err)
		}
		return nil
	})
	c, ok := taskReadAs(t, a.rw.w, a.rw.w.op, a.rw.task).Card.Variant.(*types.BoardPlacedCard)
	shown := false
	for _, n := range c.Nodes {
		for _, h := range n.Holds {
			shown = shown || n.ID == b.Node.ID && h.Hold == vocab.HoldNoSandbox
		}
	}
	if !ok || !shown {
		t.Errorf("the card does not show the build's no_sandbox hold: %+v", c)
	}
	// The next start has a sandbox root.
	started := activatedOn(t, a)
	started.passes(t, 12, func() bool { return len(started.sent) > 0 })
	if b := started.node(types.NodeKindBuild); len(b.State.Holds) != 0 {
		t.Fatalf("the build after a start with a sandbox root: %s %+v\n%s", b.State.State, b.State.Holds, started.log.String())
	}
	if len(started.sent) == 0 {
		t.Fatalf("the build did not run\n%s", started.log.String())
	}
}

// activatedOn is a's world started again with a sandbox root: its runner
// and loop made as activatedWorld makes them.
func activatedOn(t *testing.T, a *activated) *activated {
	t.Helper()
	root, err := os.MkdirTemp("/dev/shm", "adcact")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	acceptLister = func(sandbox.Host, sandbox.Image) accept.Lister { return nil }
	t.Cleanup(func() { acceptLister = defaultLister })
	starter := &socketStarter{sockets: map[types.AttemptID]string{}}
	starter.peer = requestingBuilder(map[string]string{"gate-events.json": goEvents("TestA=pass", "TestB=pass")}, starter, nil, a.sent)
	if a.peer != nil {
		starter.peer = a.peer
	}
	box := sandbox.Host{Podman: "/usr/bin/podman", Root: root, Home: "/home/adc-gate-test", Runner: &inPlace{}}
	run, err := runnerOn(context.Background(), a.d, a.rw.w.dir, box, gateImages{}, a.reg, a.eg, starter, a.log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Wait)
	a.rw.r = run
	a.rw.l = reconciler(a.d, a.reg, gateImages{}, a.eg, run, time.Hour, false, a.log)
	return a
}

// adcd says at its start that launch is active with no sandbox root; it
// starts.
func TestAStartWithNoSandboxSaysBuildsHold(t *testing.T) {
	var log logBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, Config{StateDir: stateDir(t)}, &log, func(*Daemon) { cancel() }) }()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "adcd: launch is active with no --sandbox-root: builds hold no_sandbox\n") {
		t.Errorf("the start log: %q", log.String())
	}
	launchPending(t, func() {
		log = logBuffer{}
		ctx, cancel = context.WithCancel(context.Background())
		go func() { done <- Run(ctx, Config{StateDir: stateDir(t)}, &log, func(*Daemon) { cancel() }) }()
		if err := <-done; err != nil || strings.Contains(log.String(), "no_sandbox") {
			t.Errorf("launch pending: %v %q", err, log.String())
		}
	})
}

// activatedWithout is a's world started again with no sandbox root: no
// runner, adcd's loop with sandboxless (executorOf).
func activatedWithout(a *activated) *activated {
	a.rw.r = nil
	a.rw.l = reconciler(a.d, a.reg, gateImages{}, a.eg, nil, time.Hour, false, a.log)
	return a
}

// Opus #414 H1: work placed before a restart with no sandbox root is held
// no_sandbox, never left with no hold: a granted lease is released and its
// build held; a claimed accept held. A start with a sandbox root clears the
// holds and the work runs on.
func TestNoSandboxHoldsPlacedWork(t *testing.T) {
	t.Run("a granted lease", func(t *testing.T) {
		a := activatedWorld(t, gateImages{}, true, nil)
		var granted types.Lease
		a.passes(t, 12, func() bool {
			b := a.node(types.NodeKindBuild).Node.ID
			l, live := leaseOn(a.rw.w, b)
			granted = l
			return b != "" && live && l.State == types.LeaseStateGranted
		})
		if granted.State != types.LeaseStateGranted {
			t.Fatalf("no granted lease: %+v", granted)
		}
		activatedWithout(a).passes(t, 1, nil) // one pass: released and held in one command
		b := a.node(types.NodeKindBuild)
		if len(b.State.Holds) != 1 || b.State.Holds[0].Hold != vocab.HoldNoSandbox {
			t.Fatalf("the granted build after a start with no sandbox root: %s %+v\n%s", b.State.State, b.State.Holds, a.log.String())
		}
		var l types.Lease
		a.rw.w.view(func(tx *eventstore.Tx) (err error) { l, _, _, err = lease.LeaseOf(tx, granted.ID); return err })
		if l.State != types.LeaseStateReleased {
			t.Fatalf("its lease: %s", l.State)
		}
		started := activatedOn(t, a)
		started.passes(t, 12, func() bool { return len(started.sent) > 0 })
		if b := started.node(types.NodeKindBuild); len(b.State.Holds) != 0 || len(started.sent) == 0 {
			t.Fatalf("the build after a start with a sandbox root: %s %+v\n%s", b.State.State, b.State.Holds, started.log.String())
		}
	})
	t.Run("a claimed accept", func(t *testing.T) {
		a := activatedWorld(t, gateImages{}, true, nil)
		a.passes(t, 16, func() bool {
			acc := a.node(types.NodeKindAccept)
			return acc.Node.ID != "" && acc.State.State == types.NodeStateStateClaimed
		})
		if acc := a.node(types.NodeKindAccept); acc.State.State != types.NodeStateStateClaimed {
			t.Fatalf("no claimed accept: %s", acc.State.State)
		}
		activatedWithout(a).passes(t, 4, nil)
		acc := a.node(types.NodeKindAccept)
		if len(acc.State.Holds) != 1 || acc.State.Holds[0].Hold != vocab.HoldNoSandbox {
			t.Fatalf("the claimed accept after a start with no sandbox root: %s %+v\n%s", acc.State.State, acc.State.Holds, a.log.String())
		}
		started := activatedOn(t, a)
		started.passes(t, 16, func() bool { return started.node(types.NodeKindGate).State.State == types.NodeStateStateCompleted })
		if g := started.node(types.NodeKindGate); g.State.State != types.NodeStateStateCompleted {
			t.Fatalf("the gate after a start with a sandbox root: %s %+v\n%s", g.State.State, g.State.Holds, started.log.String())
		}
	})
}

// reviewActive plants a passing qualification (only qualify.Route.Review
// makes a qualifying record): review's rows kept, a start then derives
// review active (reviewsAtStart).
func reviewActive(t *testing.T) {
	t.Helper()
	qualifiedReviewers = func(string, string, []types.RegistryExecutor) error { return nil }
	t.Cleanup(func() { qualifiedReviewers = qualify.Reviewers })
}

// activatedReviewing is a's world started again with a sandbox root and
// reg, under a table where review is derived active (reviewActive).
func activatedReviewing(t *testing.T, a *activated, reg registry.Registry) *activated {
	t.Helper()
	reviewActive(t)
	a.reg = reg
	a = activatedOn(t, a)
	reviews := reviewsOf(reg, a.log)
	if !reviews {
		t.Fatalf("review is not derived active\n%s", a.log.String())
	}
	a.rw.l = reconciler(a.d, reg, gateImages{}, a.eg, a.rw.r, time.Hour, reviews, a.log)
	return a
}

// #226 entry 173 (Opus #414 H2): with launch active and review inactive,
// the completed gate's review nodes are recorded (RecordReview) and the
// review held review_inactive, the operator's, visible on the task's card:
// never a stop with no hold. A start that derives review active clears it
// and the review goes on: claimed under its roster; under a registry whose
// reviewer's family changed, held reviewer_unavailable at its claim and
// superseded onto the registry's roster (reviews.Unheld).
func TestAnInactiveReviewIsHeld(t *testing.T) {
	held := func(t *testing.T) (*activated, graph.NodeView) {
		// Review inactive explicitly (Opus #423 L2): the qualification fails,
		// whatever the test binary's release root holds.
		qualifiedReviewers = func(string, string, []types.RegistryExecutor) error {
			return errors.New("no qualification on record (the row's)")
		}
		t.Cleanup(func() { qualifiedReviewers = qualify.Reviewers })
		a := activatedWorld(t, gateImages{}, true, nil)
		if reviewsOf(a.reg, a.log) {
			t.Fatal("review is derived active")
		}
		a.passes(t, 24, func() bool {
			r := a.node(types.NodeKindReview)
			return slices.ContainsFunc(r.State.Holds, func(h types.NodeHold) bool { return h.Hold == vocab.HoldReviewInactive })
		})
		r := a.node(types.NodeKindReview)
		if r.Node.ID == "" || len(r.State.Holds) != 1 || r.State.Holds[0].Hold != vocab.HoldReviewInactive || r.State.Holds[0].Owner != types.OwnerOperator ||
			r.State.Holds[0].SignalInstance != reconcile.ReviewSignal {
			t.Fatalf("the review with review inactive: %+v %+v\n%s", r.Node.ID, r.State, a.log.String())
		}
		c, ok := taskReadAs(t, a.rw.w, a.rw.w.op, a.rw.task).Card.Variant.(*types.BoardPlacedCard)
		shown := false
		for _, n := range c.Nodes {
			for _, h := range n.Holds {
				shown = shown || n.ID == r.Node.ID && h.Hold == vocab.HoldReviewInactive
			}
		}
		if !ok || !shown {
			t.Fatalf("the card does not show the review's hold: %+v", c)
		}
		return a, r
	}
	t.Run("review derived active", func(t *testing.T) {
		a, r := held(t)
		started := activatedReviewing(t, a, a.reg)
		started.passes(t, 12, func() bool {
			v := started.node(types.NodeKindReview)
			return len(v.State.Holds) == 0 && v.State.State != types.NodeStateStateWaiting && v.State.State != types.NodeStateStateRunnable
		})
		started.rw.w.view(func(tx *eventstore.Tx) error { // review active: no review is held review_inactive again
			s, err := reconcile.Observe(tx, started.rw.l.Gates)
			if err != nil || len(s.Inactive) != 0 {
				t.Errorf("review active, reviews to hold inactive: %+v %v", s.Inactive, err)
			}
			return nil
		})
		if v := started.node(types.NodeKindReview); v.Node.ID != r.Node.ID || len(v.State.Holds) != 0 ||
			v.State.State == types.NodeStateStateWaiting || v.State.State == types.NodeStateStateRunnable {
			t.Fatalf("the review once review is active: %s %+v\n%s", v.State.State, v.State.Holds, started.log.String())
		}
	})
	t.Run("the reviewer's family changed", func(t *testing.T) {
		a, r := held(t)
		other := stubRegistry(t, nil, map[types.ExecutorID]types.Name{stubReviewer: "another-reviewer"})
		started := activatedReviewing(t, a, other)
		started.passes(t, 12, func() bool {
			return started.node(types.NodeKindReview).Node.ID != r.Node.ID
		})
		v := started.node(types.NodeKindReview)
		p, err := v.Node.ParamsAsReviewNodeParams()
		if v.Node.ID == r.Node.ID || err != nil || len(p.Roster) != 1 || p.Roster[0].Family != "another-reviewer" {
			t.Fatalf("the review under the changed registry: %s %s %+v %v\n%s", v.Node.ID, v.State.State, p.Roster, err, started.log.String())
		}
	})
	// Opus #414 re-check L1: a review placed and granted while review was
	// active, then started again with review inactive (its qualification
	// gone): held review_inactive, its lease released in the same command,
	// never run.
	t.Run("granted, then review inactive", func(t *testing.T) {
		a, r := held(t)
		started := activatedReviewing(t, a, a.reg)
		started.passes(t, 12, func() bool {
			l, live := leaseOn(started.rw.w, r.Node.ID)
			return live && l.State == types.LeaseStateGranted
		})
		l, live := leaseOn(started.rw.w, r.Node.ID)
		if !live || l.State != types.LeaseStateGranted {
			t.Fatalf("the review's lease under review active: %v %+v\n%s", live, l, started.log.String())
		}
		qualifiedReviewers = func(string, string, []types.RegistryExecutor) error { return errors.New("the qualification is gone") }
		again := activatedOn(t, started)
		if reviewsOf(again.reg, again.log) {
			t.Fatal("review is still derived active")
		}
		again.passes(t, 6, func() bool {
			return slices.ContainsFunc(again.node(types.NodeKindReview).State.Holds, func(h types.NodeHold) bool { return h.Hold == vocab.HoldReviewInactive })
		})
		v := again.node(types.NodeKindReview)
		var ended types.Lease
		var still bool
		again.rw.w.view(func(tx *eventstore.Tx) (err error) {
			_, still, err = lease.LiveOn(tx, graph.UnitOf(r.Node.ID))
			if err == nil {
				ended, _, _, err = lease.LeaseOf(tx, l.ID)
			}
			return err
		})
		if v.Node.ID != r.Node.ID || len(v.State.Holds) != 1 || v.State.Holds[0].Hold != vocab.HoldReviewInactive || still ||
			ended.State != types.LeaseStateReleased && ended.State != types.LeaseStateReleasing {
			t.Fatalf("the granted review with review inactive: %s %+v, lease live %v, %s\n%s", v.State.State, v.State.Holds, still, ended.State, again.log.String())
		}
		if ended.Attempt != nil {
			t.Errorf("the released review lease started attempt %s", *ended.Attempt)
		}
	})
}

// #226 entry 173: no stop with no hold when no reviewer can review.
// registry.New refuses a registry with no reviewer (it needs a builder and
// a reviewer), so the case is adcd with no registry: the placer holds the
// build at its configuration hold (placement: registry) before any lease
// or spend. A start with a registry clears it and the build is placed.
func TestNoReviewerHoldsTheBuild(t *testing.T) {
	if _, err := registry.New(types.ExecutorRegistry{}, nil); err == nil {
		t.Fatal("a registry with no reviewer is accepted")
	}
	a := activatedWorld(t, gateImages{}, true, nil)
	reg := a.reg
	a.reg = registry.Registry{}
	a = activatedOn(t, a)
	a.passes(t, 12, func() bool { return len(a.node(types.NodeKindBuild).State.Holds) > 0 })
	b := a.node(types.NodeKindBuild)
	if len(b.State.Holds) != 1 || b.State.Holds[0].Hold != vocab.HoldSourceInvalidConfig || b.State.Holds[0].SignalInstance != dispatch.PlacementSignal("registry") {
		t.Fatalf("the build with no registry: %s %+v\n%s", b.State.State, b.State.Holds, a.log.String())
	}
	if _, live := leaseOn(a.rw.w, b.Node.ID); live {
		t.Fatal("a lease with no registry")
	}
	a.reg = reg
	a = activatedOn(t, a)
	a.passes(t, 12, func() bool { return len(a.sent) > 0 })
	if b := a.node(types.NodeKindBuild); len(b.State.Holds) != 0 || len(a.sent) == 0 {
		t.Fatalf("the build once a registry is given: %s %+v\n%s", b.State.State, b.State.Holds, a.log.String())
	}
}

// Opus #423 M1: review derived active (a passing qualification, review's
// rows kept: reviewsAtStart), never a hand-set Gates, through adcd's own
// loop. With a go reviewer the task ends done as a candidate (every node
// completed), then adcd starts again on the state (projections equal to
// the replay) and verify-log passes. With a no_go reviewer the review is
// repaired: round 2's build, accept, gate and review are recorded, and its
// build runs.
func TestReviewDerivedActiveThroughAdcd(t *testing.T) {
	files := map[string]string{"gate-events.json": goEvents("TestA=pass", "TestB=pass")}
	for name, decision := range map[string]string{"go": goDecision, "no_go": noGoDecision} {
		t.Run(name, func(t *testing.T) {
			a := activatedWorld(t, gateImages{}, true, nil)
			a.passes(t, 24, func() bool { return a.node(types.NodeKindGate).State.State == types.NodeStateStateCompleted })
			a.peer = reviewer(decision, false, nil, writes(files))
			started := activatedReviewing(t, a, a.reg)
			if g := started.rw.l.Gates; !g.Admits || !g.Evaluates || !g.Reviews || !g.Runs {
				t.Fatalf("the derived gates: %+v", g)
			}
			if decision == noGoDecision {
				rw := started.rw
				started.passes(t, 40, func() bool {
					return roundOf(t, rw, 2).build.Node.ID != "" && attemptOn(rw, roundOf(t, rw, 2).build.Node.ID)
				})
				one, two := roundOf(t, rw, 1), roundOf(t, rw, 2)
				if one.review.State.State != types.NodeStateStateCompleted || one.review.Outcome == nil || *one.review.Outcome != types.OutcomeFound {
					t.Fatalf("round 1's review: %s %v\n%s", one.review.State.State, one.review.Outcome, started.log.String())
				}
				if two.build.Node.ID == "" || two.accept.Node.ID == "" || two.gate.Node.ID == "" || two.review.Node.ID == "" || !attemptOn(rw, two.build.Node.ID) {
					t.Fatalf("round 2: %+v\n%s", two, started.log.String())
				}
				return
			}
			started.passes(t, 40, func() bool {
				st, _ := taskState(t, started.rw.w, started.rw.task)
				return st == authority.TaskDoneState
			})
			if st, as := taskState(t, started.rw.w, started.rw.task); st != authority.TaskDoneState || as != string(types.TaskDoneAsCandidate) {
				t.Fatalf("the task: %s %s (task.done as a candidate)\n%s", st, as, started.log.String())
			}
			c, ok := taskReadAs(t, started.rw.w, started.rw.w.op, started.rw.task).Card.Variant.(*types.BoardPlacedCard)
			if !ok || c.Column != types.BoardPlacedCardColumnDoneCandidate {
				t.Errorf("the card: %+v", c)
			}
			for _, kind := range []types.NodeKind{types.NodeKindPlan, types.NodeKindBuild, types.NodeKindAccept, types.NodeKindGate, types.NodeKindReviewRequirement, types.NodeKindReview} {
				if v := started.node(kind); v.State.State != types.NodeStateStateCompleted {
					t.Errorf("the %s: %s %+v", kind, v.State.State, v.State.Holds)
				}
			}
			if v := started.node(types.NodeKindReview); v.Outcome == nil || *v.Outcome != types.OutcomeEmpty {
				t.Errorf("the review's outcome: %v", v.Outcome)
			}
			started.rw.r.Wait()
			dir := started.rw.w.dir
			started.rw.w.done()
			d, err := Start(context.Background(), Config{StateDir: dir, Groups: started.gs}) // Start refuses projections that differ from the replay
			if err != nil {
				t.Fatalf("adcd's start on the state: %v", err)
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			var out, errb bytes.Buffer
			if code := cli.Main(context.Background(), []string{"dev", "verify-log", "--state", dir}, &out, &errb, nil); code != entry.ExitOK {
				t.Errorf("verify-log: %d %s %s", code, out.String(), errb.String())
			}
		})
	}
}

// attemptOn reports whether node's unit has started an attempt.
func attemptOn(rw *runWorld, node types.NodeID) bool {
	var ok bool
	rw.w.view(func(tx *eventstore.Tx) (err error) {
		_, ok, err = lease.FirstOn(tx, graph.UnitOf(node))
		return err
	})
	return ok
}
