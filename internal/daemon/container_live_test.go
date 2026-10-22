// SPDX-License-Identifier: Apache-2.0

package daemon

// P22a-5d's end-to-end live row (Opus #393 M1): a placed build's attempt
// runs through adcd's runner with the builder's adapter in the attempt's
// container, under rootless Podman. It runs only when asked, outside the
// test sandbox, with an OCI archive of a prepared image that carries the
// adapter (scripts/prepare-go-image.sh, go-mini's) and Node for the stub
// provider:
//
//	ADC_LIVE_PODMAN_ARCHIVE=/path/prepared.oci.tar ADC_LIVE_ADAPTER=1 go test -count=1 -run LiveContainer ./internal/daemon/
//
// The image is loaded into a private storage under a fresh sandbox root
// (its runs/ on /dev/shm, a filesystem of its own); nothing of the user's
// Podman is read or written. The stub provider listens on loopback; the
// gateway is adcd's own, on the attempt's socket. With
// ADC_LIVE_ADAPTER_RELEASE (a release's lib/adc, e.g. dist/linux-ARCH/lib/adc
// of the image's commit), the image's adapter must be that release's;
// otherwise the image's own label stands for the installed release.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// liveBox is a sandbox host over a private storage holding the archive's
// image, its pin, and the adapter digest the installed release stands for.
type liveBox struct {
	h    sandbox.Host
	ref  string
	want string
}

// podmanIn runs podman over h's private storage, as the sandbox runs it
// (its global flags and its emptied environment).
func podmanIn(t *testing.T, h sandbox.Host, args ...string) []byte {
	t.Helper()
	home := filepath.Join(h.Root, "home")
	global := []string{"--root", filepath.Join(h.Root, "storage"), "--runroot", filepath.Join(h.Root, "r"), "--cgroup-manager=cgroupfs", "--events-backend=none"}
	cmd := exec.Command(h.Podman, append(global, args...)...)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "XDG_RUNTIME_DIR=" + filepath.Join(h.Root, "xdg"), "XDG_CONFIG_HOME=" + home + "/.config",
		"XDG_DATA_HOME=" + home + "/.local/share", "XDG_STATE_HOME=" + home + "/.local/state", "XDG_CACHE_HOME=" + home + "/.cache"}
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
	if err := errors.Join(os.Symlink(runs, filepath.Join(root, "runs")), os.MkdirAll(filepath.Join(root, "home"), 0o700),
		os.MkdirAll(filepath.Join(root, "xdg"), 0o700)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd := exec.Command("/bin/sh", "-c", `"$0" --root "$1/storage" --runroot "$1/r" --cgroup-manager=cgroupfs --events-backend=none system reset --force`, h.Podman, root)
		cmd.Env = []string{"HOME=" + filepath.Join(root, "home"), "PATH=/usr/bin:/bin", "XDG_RUNTIME_DIR=" + filepath.Join(root, "xdg")}
		_ = cmd.Run()
		_ = os.RemoveAll(root)
		_ = os.RemoveAll(runs)
	})
	podmanIn(t, h, "pull", "--quiet", "oci-archive:"+archive)
	var imgs []struct{ RepoDigests []string }
	if err := json.Unmarshal(podmanIn(t, h, "images", "--format", "json"), &imgs); err != nil || len(imgs) != 1 || len(imgs[0].RepoDigests) == 0 {
		t.Fatalf("the archive's image: %+v, %v", imgs, err)
	}
	b := liveBox{h: h, ref: imgs[0].RepoDigests[0]}
	img, err := h.Resolve(context.Background(), b.ref)
	if err != nil {
		t.Fatal(err)
	}
	b.want = img.Manifest.Adapter[runtime.GOOS+"/"+runtime.GOARCH]
	if rel := os.Getenv("ADC_LIVE_ADAPTER_RELEASE"); rel != "" {
		if b.want, err = sandbox.AdapterDigest(rel); err != nil {
			t.Fatal(err)
		}
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
// gateway (its registry's stub destination the stub provider), the
// exchanges and sockets under the host's runs/.
func liveWorld(t *testing.T, want func(liveBox) string) (*runWorld, liveBox, string) {
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
	return rw, box, log
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
func containersIn(t *testing.T, h sandbox.Host) string {
	t.Helper()
	return strings.TrimSpace(string(podmanIn(t, h, "ps", "--all", "--format", "{{.Names}}")))
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
	rw, box, log := liveWorld(t, func(b liveBox) string { return b.want })
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
	for _, dir := range []string{rw.r.clones, rw.exch} {
		if !empty(dir) {
			t.Errorf("%s left behind", dir)
		}
	}
	if left := containersIn(t, box.h); left != "" {
		t.Errorf("containers left: %q", left)
	}
}

// An image whose adapter is not the installed release's is refused before
// anything is prepared: no attempt starts, no request reaches the stub, no
// container is made, and the build is held on the project's profile.
func TestLiveContainerRefusesAnotherAdapter(t *testing.T) {
	rw, box, log := liveWorld(t, func(liveBox) string { return "sha256:" + strings.Repeat("0", 64) })
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
	if left := containersIn(t, box.h); left != "" {
		t.Errorf("containers made: %q", left)
	}
}
