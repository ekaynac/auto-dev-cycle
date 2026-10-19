// SPDX-License-Identifier: Apache-2.0

package accept

// The live smoke check of the production loader (P14e-2c): SandboxLister
// runs go list in the prepared Go image under rootless Podman, which the
// test sandbox (bubblewrap) does not offer, so it runs only when asked:
//
//	ADC_LIVE_GO_ROOT=/tmp/adcs ADC_LIVE_GO_PIN=ghcr.io/…@sha256:… ADC_LIVE_GO_ID=<id> \
//	  go test -run TestLiveSandboxLister ./internal/accept/
//
// ADC_LIVE_GO_ROOT is a sandbox root (sandbox.Host.Root) prepared beforehand
// with the pinned image in its storage, by the ID the pin resolves to on this
// host's architecture (podman image inspect PIN), nothing pulled here:
//
//	podman save --format oci-archive -o go.tar ID
//	mkdir -p ROOT/home ROOT/xdg && ln -s "$(mktemp -d /dev/shm/adcg-runs.XXXX)" ROOT/runs
//	env -i HOME=ROOT/home XDG_RUNTIME_DIR=ROOT/xdg PATH=/usr/bin:/bin podman --root ROOT/storage \
//	  --runroot ROOT/r --cgroup-manager=cgroupfs --events-backend=none pull oci-archive:go.tar
//
// runs/ must be on a filesystem of its own (here /dev/shm, tmpfs).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

func TestLiveSandboxLister(t *testing.T) {
	root, pin, id := os.Getenv("ADC_LIVE_GO_ROOT"), os.Getenv("ADC_LIVE_GO_PIN"), os.Getenv("ADC_LIVE_GO_ID")
	if root == "" || pin == "" || id == "" {
		t.Skip("set ADC_LIVE_GO_ROOT, ADC_LIVE_GO_PIN and ADC_LIVE_GO_ID to run the live loader smoke check")
	}
	h := sandbox.Host{Podman: "/usr/bin/podman", Root: root, Home: filepath.Clean(os.Getenv("HOME"))}
	list := SandboxLister(h, sandbox.Image{Pinned: pin, ID: id}, sandbox.Limits{Memory: 2 << 30, CPUs: "2", PIDs: 512, Timeout: 3 * time.Minute, Disk: 1 << 30})
	f := newGuardFixture(t)
	change := map[string]string{"pkg/calc.go": "package pkg\n\nimport (\n\t\"fmt\"\n\n\t\"golang.org/x/sys/unix\"\n)\n\nfunc Add(a, b int) int { return a + b }\n\n" +
		"func Sum(xs ...int) int { return len(fmt.Sprint(xs)) + unix.Getpagesize()*0 }\n"}
	typed := func(v Verdict) bool { return v.Accepted() && !finds(v.Blocks, "not loaded") }
	for name, c := range map[string]struct {
		files map[string]string
		ok    func(Verdict) bool
	}{
		"a change typed on both platforms": {change, typed},
		"a raised toolchain line":          {maps2(change, "go.mod", guardBase["go.mod"]+"\ntoolchain go1.99.0\n"), typed},
		"a raised go line": {maps2(change, "go.mod", strings.Replace(guardBase["go.mod"], "go 1.26.0", "go 1.99", 1)),
			func(v Verdict) bool { return finds(v.Blocks, "go list in the prepared image") }},
		"a forgery": {map[string]string{"pkg/forge.go": forgeGo}, func(v Verdict) bool { return finds(v.Blocks, "can forge") }},
	} {
		begin := time.Now()
		v, err := Accept(context.Background(), In{Repo: f.repo, Base: f.base, Patch: f.patch(t, c.files), Policy: f.policy, Guard: Integrity(f.repo, f.base, list, filepath.Join(root, "runs"))})
		t.Logf("%s: %v, blocks %q", name, time.Since(begin).Round(time.Millisecond), whats(v.Blocks))
		if err != nil || !c.ok(v) {
			t.Errorf("%s: %v %q", name, err, whats(v.Blocks))
		}
	}
}
