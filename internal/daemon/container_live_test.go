// SPDX-License-Identifier: Apache-2.0

package daemon

// P22a-5d's end-to-end live rows (Opus #393 M1): a placed build's attempt
// runs through adcd's runner with the builder's adapter in the attempt's
// container, under rootless Podman; and a placed review's, with the
// reviewer's (Mythos chunk 4a H1). It runs only when asked, outside the
// test sandbox, with an OCI archive of a prepared image that carries the
// adapter (scripts/prepare-go-image.sh, go-mini's) and Node for the stub
// provider:
//
//	ADC_LIVE_PODMAN_ARCHIVE=/path/prepared.oci.tar ADC_LIVE_ADAPTER=1 go test -count=1 -run LiveContainer ./internal/daemon/
//
// The image is loaded into a private storage under a fresh sandbox root
// (its runs/ on /dev/shm, a filesystem of its own); nothing of the user's
// Podman is read or written. The stub provider listens on loopback; the
// gateway is adcd's own, on the attempt's socket. ADC_LIVE_ADAPTER_RELEASE
// is required: a release's lib/adc (dist/linux-ARCH/lib/adc of the image's
// commit), which stands for the installed release, so the image's adapter
// must be that release's, not merely its own label's (Opus 22a-5d-4 L4).
//
//	ADC_LIVE_PODMAN_ARCHIVE=/path/prepared.oci.tar ADC_LIVE_ADAPTER=1 \
//	ADC_LIVE_ADAPTER_RELEASE=/path/dist/linux-ARCH/lib/adc go test -count=1 -run LiveContainer ./internal/daemon/

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// liveBox is a sandbox host over a private storage holding the archive's
// image, its pin, and the adapter digest the installed release stands for.
type liveBox struct {
	h    sandbox.Host
	env  []string // the test's own home (testguard.HomeEnv), for podmanIn
	ref  string
	want string
}

// podmanIn runs podman over h's private storage, with the sandbox's global
// flags (its storage and runroot) and the test's own home
// (testguard.HomeEnv): nothing of the user's Podman is read or written.
func podmanIn(t *testing.T, h sandbox.Host, env []string, args ...string) []byte {
	t.Helper()
	global := []string{"--root", filepath.Join(h.Root, "storage"), "--runroot", filepath.Join(h.Root, "r"), "--cgroup-manager=cgroupfs", "--events-backend=none"}
	cmd := exec.Command(h.Podman, append(global, args...)...)
	cmd.Env = append(slices.Clone(env), "PATH=/usr/bin:/bin")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("podman %v: %v", args, err)
	}
	return out
}

func newLiveBox(t *testing.T) liveBox {
	t.Helper()
	archive := os.Getenv("ADC_LIVE_PODMAN_ARCHIVE")
	if archive == "" || os.Getenv("ADC_LIVE_ADAPTER") == "" {
		t.Skip("set ADC_LIVE_PODMAN_ARCHIVE (a prepared image with the adapter) and ADC_LIVE_ADAPTER to run the end-to-end live row")
	}
	rel := os.Getenv("ADC_LIVE_ADAPTER_RELEASE")
	if rel == "" {
		t.Fatal("ADC_LIVE_ADAPTER_RELEASE (the dist lib/adc of the image's commit) is required: without it the image's label is checked against itself")
	}
	root, err := os.MkdirTemp("", "adcl") // short: Podman's runroot limit
	if err != nil {
		t.Fatal(err)
	}
	runs, err := os.MkdirTemp("/dev/shm", "adcl-runs")
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	h := sandbox.Host{Podman: "/usr/bin/podman", Root: root, Home: filepath.Clean(home)}
	if err := os.Symlink(runs, filepath.Join(root, "runs")); err != nil {
		t.Fatal(err)
	}
	env := testguard.HomeEnv(t)
	t.Cleanup(func() {
		cmd := exec.Command(h.Podman, "--root", filepath.Join(root, "storage"), "--runroot", filepath.Join(root, "r"), "--cgroup-manager=cgroupfs",
			"--events-backend=none", "system", "reset", "--force")
		cmd.Env = append(slices.Clone(env), "PATH=/usr/bin:/bin")
		_ = cmd.Run()
		_ = os.RemoveAll(root)
		_ = os.RemoveAll(runs)
	})
	podmanIn(t, h, env, "pull", "--quiet", "oci-archive:"+archive)
	var imgs []struct{ RepoDigests []string }
	if err := json.Unmarshal(podmanIn(t, h, env, "images", "--format", "json"), &imgs); err != nil || len(imgs) != 1 || len(imgs[0].RepoDigests) == 0 {
		t.Fatalf("the archive's image: %+v, %v", imgs, err)
	}
	b := liveBox{h: h, env: env, ref: imgs[0].RepoDigests[0]}
	img, err := h.Resolve(context.Background(), b.ref)
	if err != nil {
		t.Fatal(err)
	}
	if b.want, err = sandbox.AdapterDigest(rel); err != nil {
		t.Fatal(err)
	}
	if err := img.CheckAdapter(b.want); err != nil {
		t.Fatalf("the image's adapter is not ADC_LIVE_ADAPTER_RELEASE's: %v", err)
	}
	return b
}

// stubProvider starts the stub provider with scenario on loopback; it
// returns its base URL and its log.
func stubProvider(t *testing.T, scenario string) (string, string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on PATH for the stub provider")
	}
	log := filepath.Join(t.TempDir(), "stub.jsonl")
	cmd := exec.Command(node, "../../fixtures/m1/stub-provider/server.mjs", "--scenario", "../../fixtures/m1/stub-provider/scenarios/"+scenario+".json",
		"--listen", "127.0.0.1:0", "--log", log)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	base, ok := strings.CutPrefix(strings.TrimSpace(line), "listening ")
	if err != nil || !ok {
		t.Fatalf("the stub provider: %q, %v", line, err)
	}
	return base, log
}

// goMiniRepo is a local repository of T0 go-mini at its base.
func goMiniRepo(t *testing.T, tmp string) string {
	t.Helper()
	src := filepath.Join(tmp, "go-mini")
	if out, err := exec.Command("cp", "-R", "../../fixtures/t0/go-mini/repo", src).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	gitIn(t, src, "", "init", "--quiet", "--initial-branch=main")
	gitIn(t, src, "", "add", "-A")
	gitIn(t, src, "", "commit", "--quiet", "-m", "base")
	return src
}

// liveWorld is runWorld over go-mini's repository, pinning the live image,
// with adcd's runner on the live sandbox host: the container adapter, the
// gateway (its registry reg's stub destination the stub provider), the
// exchanges and sockets under the host's runs/; edit, if any, edits the
// profile further.
func liveWorld(t *testing.T, want func(liveBox) string, edit func(map[string]any)) (*runWorld, liveBox, string, registry.Registry) {
	t.Helper()
	tmp := t.TempDir()
	src := goMiniRepo(t, tmp)
	box := newLiveBox(t)
	base, log := stubProvider(t, "go-mini-feature-1")
	t.Setenv("ADC_CREDENTIAL_STUB", "a-stub-credential")
	rw := newRunWorldWith(t, true, nil, func(p map[string]any) {
		p["repo"] = map[string]any{"url": src, "base_ref": "refs/heads/main"}
		tc := p["toolchain"].(map[string]any) // what a go-mini image is prepared for
		tc["images"] = map[string]any{"go": box.ref}
		tc["dependency_recipe"].(map[string]any)["lockfiles"] = []string{"go.mod"}
		if edit != nil {
			edit(p)
		}
	})
	box.h.Forbidden = []string{rw.w.dir} // the controller's state directory
	doc, err := registry.Load("../../fixtures/m1/config/registry-stub.json")
	if err != nil {
		t.Fatal(err)
	}
	for i := range doc.Executors {
		doc.Executors[i].Destination.Endpoint = base
	}
	var gs []types.ResourceGroup
	if raw, err := os.ReadFile("../../fixtures/m1/config/groups.json"); err == nil {
		err = json.Unmarshal(raw, &gs)
	}
	reg, err := registry.New(doc, gs)
	if err != nil {
		t.Fatal(err)
	}
	// adcd's start rule, which the model calls' recorder requires (startup.go).
	if _, err := rw.w.s.MarkPendingUncertain(context.Background(), string(rw.r.system.Actor())); err != nil {
		t.Fatal(err)
	}
	e, err := newEgress(&Daemon{store: rw.w.s, blobs: rw.w.blobs, system: rw.r.system, clock: time.Now}, reg, false)
	if err != nil {
		t.Fatal(err)
	}
	h := box.h
	rw.exch = h.ExchangeRoot()
	rw.r.box, rw.r.host.Root, rw.r.sockets, rw.r.gateway = h, rw.exch, filepath.Join(h.Root, "runs", "gateway"), e.open
	for _, d := range []string{rw.r.host.Root, rw.r.sockets} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	rw.r.adapters = &containerAdapter{Box: h, Want: want(box)}
	return rw, box, log, reg
}

// stubRequests are the stub provider's logged requests.
func stubRequests(t *testing.T, log string) int {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return bytes.Count(b, []byte(`"kind":"request"`))
}

// containersIn are the containers Podman lists in h's storage.
func containersIn(t *testing.T, b liveBox) string {
	t.Helper()
	return strings.TrimSpace(string(podmanIn(t, b.h, b.env, "ps", "--all", "--format", "{{.Names}}")))
}

// The I17 effect path, live: the loop places go-mini's build and adcd's
// runner runs its attempt with the builder's adapter in the attempt's
// container: the image resolved and checked, the exchange prepared with the
// container's paths, the adapter's requests through adcd's gateway on the
// attempt's socket to the stub provider, its edits in /workspace, its
// result ingested from /exchange/out, the controller's diff recorded by the
// attempt (CommitAttempt). The container, the attempt directory, the
// exchange and the clone are gone after it.
func TestLiveContainerRunsTheBuild(t *testing.T) {
	rw, box, log, _ := liveWorld(t, func(b liveBox) string { return b.want }, nil)
	rw.untilRun(t)
	a, l := rw.attemptOf(t)
	var res types.Result
	var byAttempt bool
	rw.w.view(func(tx *eventstore.Tx) (err error) {
		res, byAttempt, err = graph.AttemptResult(tx, a.ID)
		return err
	})
	if !byAttempt || res.Outcome != types.OutcomeFound || a.Status != types.AttemptStatusCompleted || l.State != types.LeaseStateSettled || len(res.Value) != 1 {
		t.Fatalf("the result %+v (by the attempt %v), attempt %s, lease %s", res, byAttempt, a.Status, l.State)
	}
	patch, err := blobOf(t, rw.w, res.Value[0].ContentDigest)
	if err != nil || !bytes.Contains(patch, []byte("+func (s *Store) Transfer(")) {
		t.Errorf("the recorded diff: %s, %v", patch, err)
	}
	if n := stubRequests(t, log); n < 2 {
		t.Errorf("the stub provider saw %d requests, want the builder's turns", n)
	}
	leftBehind(t, rw, box)
}

// A5 (the activation plan §3 A): the live build through adcd's own wiring,
// under a test build of the table where launch is active (launchActive):
// adcd's egress (newEgress), its runner on the live sandbox host with the
// container adapter (runnerOn, as runnerOf makes it) and its own loop
// (reconciler, the gates derived from the table; none set by hand). The
// build is placed, launched in its container, its adapter's turns reach
// the stub provider through the gateway, and its diff is recorded.
func TestLiveContainerRunsTheBuildThroughAdcd(t *testing.T) {
	launchActive(t)
	rw, box, log, reg := liveWorld(t, func(b liveBox) string { return b.want }, nil)
	d := &Daemon{store: rw.w.s, blobs: rw.w.blobs, system: rw.r.system, clock: time.Now}
	eg, err := newEgress(d, reg, false)
	if err != nil {
		t.Fatal(err)
	}
	run, err := runnerOn(context.Background(), d, rw.w.dir, box.h, box.h, reg, eg, &containerAdapter{Box: box.h, Want: box.want}, testLog{t})
	if err != nil {
		t.Fatal(err)
	}
	rw.r = run
	rw.l = reconciler(d, reg, box.h, eg, run, time.Hour, false, io.Discard)
	for range 12 {
		rw.build = templateNodes(rw.w, rw.task)[types.NodeKindBuild]
		if id, ok, err := firstAttempt(rw); rw.build != "" && err == nil && ok && attemptAt(rw.w, id).Status == types.AttemptStatusCompleted {
			break
		}
		if _, err := rw.l.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		run.Wait()
	}
	rw.build = templateNodes(rw.w, rw.task)[types.NodeKindBuild]
	a, l := rw.attemptOf(t)
	var res types.Result
	var byAttempt bool
	rw.w.view(func(tx *eventstore.Tx) (err error) {
		res, byAttempt, err = graph.AttemptResult(tx, a.ID)
		return err
	})
	if !byAttempt || res.Outcome != types.OutcomeFound || a.Status != types.AttemptStatusCompleted || l.State != types.LeaseStateSettled || len(res.Value) != 1 {
		t.Fatalf("the result %+v (by the attempt %v), attempt %s, lease %s", res, byAttempt, a.Status, l.State)
	}
	patch, err := blobOf(t, rw.w, res.Value[0].ContentDigest)
	if err != nil || !bytes.Contains(patch, []byte("+func (s *Store) Transfer(")) {
		t.Errorf("the recorded diff: %s, %v", patch, err)
	}
	if n := stubRequests(t, log); n < 2 {
		t.Errorf("the stub provider saw %d requests, want the builder's turns", n)
	}
	leftBehind(t, rw, box)
}

// An image whose adapter is not the installed release's is refused before
// anything is prepared: no attempt starts, no request reaches the stub, no
// container is made, and the build is held on the project's profile.
func TestLiveContainerRefusesAnotherAdapter(t *testing.T) {
	rw, box, log, _ := liveWorld(t, func(liveBox) string { return "sha256:" + strings.Repeat("0", 64) }, nil)
	rw.untilRun(t)
	if _, ok, err := firstAttempt(rw); err != nil || ok {
		t.Fatalf("an attempt started: %v %v", ok, err)
	}
	st := nodesOf(rw.w, rw.task)[types.NodeKindBuild].State
	if len(st.Holds) != 1 || st.Holds[0].Hold != vocab.HoldSourceInvalidConfig || !strings.HasPrefix(st.Holds[0].SignalInstance, reconcile.RunnerSignal("profile ")) {
		t.Errorf("the node: %+v", st)
	}
	if n := stubRequests(t, log); n != 0 {
		t.Errorf("the stub provider saw %d requests", n)
	}
	if left := containersIn(t, box); left != "" {
		t.Errorf("containers made: %q", left)
	}
}

// leftBehind reports what a live run left: the clones, the exchanges, the
// attempt directories, the containers.
func leftBehind(t *testing.T, rw *runWorld, box liveBox) {
	t.Helper()
	for _, dir := range []string{rw.r.clones, rw.exch} {
		if !empty(dir) {
			t.Errorf("%s left behind", dir)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(box.h.Root, "runs", "a*")); len(left) != 0 {
		t.Errorf("attempt directories left: %q", left)
	}
	if left := containersIn(t, box); left != "" {
		t.Errorf("containers left: %q", left)
	}
}

// The review's activation prerequisite (Mythos chunk 4a H1): live, under
// go-mini's profile (its gate, its go.mod lockfile), the loop places the
// build, accept and the gate run on the live sandbox, and the placed
// review runs in the reviewer's container with its empty, read-only
// workspace: the image checked with no base to hash, the stub provider's
// reviewer answering go through adcd's gateway. The review completes with
// that verdict, recorded by its attempt, and nothing is left behind.
func TestLiveContainerRunsTheReview(t *testing.T) {
	var gomini struct{ Gate map[string]any }
	raw, err := os.ReadFile("../../fixtures/m1/profiles/go-mini.json")
	if err == nil {
		err = json.Unmarshal(raw, &gomini)
	}
	if err != nil {
		t.Fatal(err)
	}
	rw, box, log, reg := liveWorld(t, func(b liveBox) string { return b.want }, func(p map[string]any) {
		g := gomini.Gate
		g["environments"].([]any)[0].(map[string]any)["facts"].(map[string]any)["runtime"].(map[string]any)["arch"] = runtime.GOARCH
		p["gate"] = g
	})
	rw.untilRun(t) // the build
	// accept's and the gate's image is the live one, resolved as adcd does.
	rw.r.place.d.Images = box.h
	pl := rw.x.Placer.(placer)
	pl.d.Images = box.h
	rw.x.Placer = pl
	rw.reviewing(reg)
	// The launch's destinations are the grant's; builds, accept, the gate
	// and the review run through adcd's runner.
	rw.r.env.Families, rw.r.place.d.Registry = reg, reg
	rw.x.Runner = rw.runs
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true, Reviews: true}, time.Hour, io.Discard)
	builderCalls := stubRequests(t, log)
	var review graph.NodeView
	for range 16 {
		if _, err := rw.l.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		rw.r.Wait()
		review = reviewNodes(rw)[types.NodeKindReview]
		if review.State.State == types.NodeStateStateCompleted {
			break
		}
	}
	gate := nodesOf(rw.w, rw.task)[types.NodeKindGate]
	if review.State.State != types.NodeStateStateCompleted || review.Outcome == nil || *review.Outcome != types.OutcomeEmpty {
		t.Fatalf("the review: %s %v, holds %+v (the gate: %s, holds %+v)", review.State.State, review.Outcome, review.State.Holds, gate.State.State, gate.State.Holds)
	}
	rs := reviewResults(rw, review.Node.ID)
	if len(rs) != 1 || rs[0].Attempt == nil || rs[0].Result.Reason != vocab.ReasonCompletedClean {
		t.Fatalf("its results: %+v", rs)
	}
	if n := stubRequests(t, log) - builderCalls; n < 1 {
		t.Errorf("the stub provider saw %d reviewer requests", n)
	}
	leftBehind(t, rw, box)
}
