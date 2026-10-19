// SPDX-License-Identifier: Apache-2.0

package accept

// The live smoke check of the production loader (P14e-2c): SandboxLister
// runs go list in the prepared Go image under rootless Podman, which the
// test sandbox (bubblewrap) does not offer, so it runs only when asked:
//
//	ADC_LIVE_GO_ARCHIVE=/path/go.oci.tar ADC_LIVE_GO_PIN=ghcr.io/…@sha256:… ADC_LIVE_GO_ID=<id> \
//	  go test -run TestLiveSandboxLister ./internal/accept/
//
// The archive is the pinned image saved by the ID its pin resolves to on
// this host's architecture (podman save --format oci-archive -o x.tar ID);
// it is loaded into a fresh, private storage, nothing pulled, and its ID
// must be ADC_LIVE_GO_ID. The sandbox's runs/ is on /dev/shm (tmpfs).

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

func TestLiveSandboxLister(t *testing.T) {
	archive, pin, id := os.Getenv("ADC_LIVE_GO_ARCHIVE"), os.Getenv("ADC_LIVE_GO_PIN"), os.Getenv("ADC_LIVE_GO_ID")
	if archive == "" || pin == "" || id == "" {
		t.Skip("set ADC_LIVE_GO_ARCHIVE, ADC_LIVE_GO_PIN and ADC_LIVE_GO_ID to run the live loader smoke check")
	}
	root, err := os.MkdirTemp("", "adcg") // short: Podman's runroot limit
	runs, err2 := os.MkdirTemp("/dev/shm", "adcg-runs")
	if err = errors.Join(err, err2); err == nil {
		err = os.Symlink(runs, filepath.Join(root, "runs"))
	}
	if err != nil {
		t.Fatal(err)
	}
	podman := func(args ...string) ([]byte, error) {
		cmd := exec.Command("/usr/bin/podman", append([]string{"--root", filepath.Join(root, "storage"), "--runroot", filepath.Join(root, "r"),
			"--cgroup-manager=cgroupfs", "--events-backend=none"}, args...)...)
		cmd.Env = []string{"HOME=" + filepath.Join(root, "home"), "XDG_RUNTIME_DIR=" + filepath.Join(root, "xdg"), "PATH=/usr/bin:/bin"}
		return cmd.Output()
	}
	for _, d := range []string{"home", "xdg"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = podman("system", "reset", "--force")
		_ = os.RemoveAll(root)
		_ = os.RemoveAll(runs)
	})
	out, err := podman("pull", "-q", "oci-archive:"+archive)
	if loaded := strings.TrimPrefix(strings.TrimSpace(string(out)), "sha256:"); err != nil || loaded != id {
		t.Fatalf("loading the archive: %v, %q (want %s)", err, out, id)
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
		v, err := Accept(context.Background(), In{Repo: f.repo, Base: f.base, Patch: f.patch(t, c.files), Policy: f.policy, Guard: Integrity(f.repo, f.base, list)})
		t.Logf("%s: %v, blocks %q", name, time.Since(begin).Round(time.Millisecond), whats(v.Blocks))
		if err != nil || !c.ok(v) {
			t.Errorf("%s: %v %q", name, err, whats(v.Blocks))
		}
	}
}
