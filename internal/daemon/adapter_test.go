// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// fakeBox is a sandbox that resolves one image and records the
// interactive runs it is asked for.
type fakeBox struct {
	img      sandbox.Image
	resolve  error
	interact error
	refs     []string
	runs     []sandbox.Run
	attempts []string
}

func (b *fakeBox) Resolve(_ context.Context, ref string) (sandbox.Image, error) {
	b.refs = append(b.refs, ref)
	return b.img, b.resolve
}

func (b *fakeBox) Interactive(r sandbox.Run, attempt string) (sandbox.Interactive, error) {
	b.runs, b.attempts = append(b.runs, r), append(b.attempts, attempt)
	if b.interact != nil {
		return sandbox.Interactive{}, b.interact
	}
	return sandbox.Interactive{Command: []string{"/usr/bin/podman", "run", "x"}, Env: []string{"PATH=/usr/bin"}, Deadline: r.Limits.Timeout,
		Remove: func(context.Context) error { return nil }}, nil
}

// containerWorld is an attempt's directory, its exchange and socket paths,
// a release's adapter closure and an image prepared for go-mini with that
// adapter (its manifest's toolchain go, its lockfile go.mod at the base).
type containerWorld struct {
	box  *fakeBox
	c    *containerAdapter
	run  AdapterRun
	want string
}

func newContainerWorld(t *testing.T) containerWorld {
	t.Helper()
	tmp := t.TempDir()
	a, err := (sandbox.Host{Root: filepath.Join(tmp, "sbx")}).NewAttempt()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Teardown() })
	lib := filepath.Join(tmp, "lib", "adc")
	for name, body := range map[string]string{"node/bin/node": "node", "adapters/http-chat/src/main.ts": "main", "adapters/contract/src/index.ts": "index",
		"gen/ts/validators.cjs": "v", "node_modules/ajv/index.js": "ajv", filepath.Join(a.Workspace(), "go.mod"): "module m\n"} {
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(lib, name)
		}
		if err := errors.Join(os.MkdirAll(filepath.Dir(p), 0o755), os.WriteFile(p, []byte(body), 0o755)); err != nil {
			t.Fatal(err)
		}
	}
	want, err := sandbox.AdapterDigest(lib)
	if err != nil {
		t.Fatal(err)
	}
	label, _ := json.Marshal(map[string]any{"toolchain": "go", "lockfiles": map[string]string{"go.mod": sha256File(t, filepath.Join(a.Workspace(), "go.mod"))},
		"adapter": map[string]string{runtime.GOOS + "/" + runtime.GOARCH: want}})
	m, err := sandbox.ParseManifest(label)
	if err != nil {
		t.Fatal(err)
	}
	box := &fakeBox{img: sandbox.Image{Pinned: "localhost/go-mini@sha256:" + testDigest, ID: testDigest, OS: runtime.GOOS, Arch: runtime.GOARCH, Manifest: m}}
	att := types.AttemptID("att_01m3250v9xft08ssr4ygq6g2np")
	run := AdapterRun{Attempt: att, Box: a, Workspace: a.Workspace(), Exchange: filepath.Join(tmp, "sbx", "runs", "exchange", "exchange-"+string(att)),
		Socket: filepath.Join(tmp, "sbx", "runs", "gateway", string(att)+".sock"), Deadline: time.Minute, Image: box.img.Pinned, Toolchain: "go", Lockfiles: []string{"go.mod"}}
	return containerWorld{box: box, c: &containerAdapter{Box: box, Release: lib}, run: run, want: want}
}

const testDigest = "4105e2bb7b57b01eaf420afd4f7970386183f64f4d244bca3d684fa9737e0670"

// sha256File is p's sha256:HEX, as a prepared image's manifest gives a lockfile's.
func sha256File(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// P22a-5d-3 (#226 entries 126, 143): the builder's adapter starts in the
// attempt's container. Start resolves the profile's pinned image and
// checks it before anything is prepared; the container's command is made
// only at Ready, once the exchange is (the sandbox refuses an exchange
// without in/ and out/): a build run of the attempt's directory, gateway
// socket and exchange, under the session's deadline, running the image's
// adapter. The adapter sees the workspace, its exchange and its socket at
// the container's paths, and nothing else of the host.
func TestTheAdapterStartsInTheAttemptsContainer(t *testing.T) {
	w := newContainerWorld(t)
	s, err := w.c.Start(context.Background(), w.run)
	if err != nil {
		t.Fatal(err)
	}
	ct := s.Config.Container
	if ct == nil || len(ct.Command) != 0 || len(w.box.runs) != 0 || s.Config.Command != nil || s.Config.InProcess != nil || s.Config.Deadline != time.Minute {
		t.Fatalf("before Ready: %+v, %d interactive runs", s.Config, len(w.box.runs))
	}
	if !slices.Equal(w.box.refs, []string{w.run.Image}) {
		t.Errorf("resolved %q", w.box.refs)
	}
	if err := s.Ready(); err != nil {
		t.Fatal(err)
	}
	r := w.box.runs[0]
	want := containerLimits
	want.Timeout = time.Minute
	if len(w.box.runs) != 1 || w.box.attempts[0] != string(w.run.Attempt) || r.Mode != sandbox.Build || r.Attempt != w.run.Box || r.Gateway != w.run.Socket ||
		r.Exchange != w.run.Exchange || r.Limits != want || r.Image.ID != testDigest ||
		!slices.Equal(r.Argv, []string{"/opt/adc/lib/node/bin/node", "/opt/adc/lib/adapters/http-chat/src/main.ts"}) {
		t.Fatalf("the interactive run: %+v (%q)", r, w.box.attempts)
	}
	if !slices.Equal(ct.Command, []string{"/usr/bin/podman", "run", "x"}) || !slices.Equal(ct.Env, []string{"PATH=/usr/bin"}) || ct.Remove == nil {
		t.Errorf("the container after Ready: %+v", ct)
	}
	for host, inside := range map[string]string{w.run.Workspace: "/workspace", filepath.Join(w.run.Exchange, "in"): "/exchange/in",
		filepath.Join(w.run.Exchange, "out"): "/exchange/out", w.run.Socket: "/run/adc/gateway.sock",
		w.run.Exchange: "", filepath.Dir(w.run.Workspace): "", "/etc/passwd": "", w.run.Workspace + "/a.go": ""} {
		if got := s.Path(host); got != inside {
			t.Errorf("Path(%s) = %q, want %q", host, got, inside)
		}
	}
}

// Start refuses, and nothing is made or run: a run without its attempt's
// directory, or whose workspace is another, or with no deadline; an image
// the sandbox does not resolve (absent, another digest or platform); an
// image not prepared for the toolchain or for the base's lockfiles
// (ErrManifest); an image whose adapter is not the installed release's
// (ErrAdapter, an ErrManifest: a configuration fault the profile's re-pin
// clears); a release whose adapter cannot be digested (errConfig). A Ready
// the sandbox refuses fails.
func TestTheAdapterContainerRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(w *containerWorld)
		want error
	}{
		"no attempt directory": {func(w *containerWorld) { w.run.Box = nil }, nil},
		"another workspace":    {func(w *containerWorld) { w.run.Workspace = filepath.Dir(w.run.Workspace) }, nil},
		"no deadline":          {func(w *containerWorld) { w.run.Deadline = 0 }, nil},
		"an image not present": {func(w *containerWorld) { w.box.resolve = sandbox.ErrImage }, sandbox.ErrImage},
		"another toolchain":    {func(w *containerWorld) { w.run.Toolchain = "python" }, sandbox.ErrManifest},
		"a lockfile changed": {func(w *containerWorld) {
			_ = os.WriteFile(filepath.Join(w.run.Workspace, "go.mod"), []byte("module n\n"), 0o644)
		}, sandbox.ErrManifest},
		"another adapter": {func(w *containerWorld) {
			w.box.img.Manifest.Adapter[runtime.GOOS+"/"+runtime.GOARCH] = "sha256:" + testDigest
		}, sandbox.ErrAdapter},
		"an image with none": {func(w *containerWorld) { w.box.img.Manifest.Adapter = nil }, sandbox.ErrAdapter},
		"the release's adapter changed": {func(w *containerWorld) {
			_ = os.WriteFile(filepath.Join(w.c.Release, "adapters/http-chat/src/main.ts"), []byte("other"), 0o755)
		}, sandbox.ErrAdapter},
		"no release": {func(w *containerWorld) { w.c.Release = filepath.Join(w.c.Release, "missing") }, errConfig},
	} {
		w := newContainerWorld(t)
		c.edit(&w)
		_, err := w.c.Start(context.Background(), w.run)
		if err == nil || c.want != nil && !errors.Is(err, c.want) || len(w.box.runs) != 0 {
			t.Errorf("%s: %v (want %v), %d interactive runs", name, err, c.want, len(w.box.runs))
		}
		if errors.Is(err, sandbox.ErrManifest) != configFault(err) && !errors.Is(err, errConfig) {
			t.Errorf("%s: a manifest refusal is not a configuration fault: %v", name, err)
		}
	}
	w := newContainerWorld(t)
	w.box.interact = sandbox.ErrSpec
	s, err := w.c.Start(context.Background(), w.run)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(); !errors.Is(err, sandbox.ErrSpec) || len(s.Config.Container.Command) != 0 {
		t.Errorf("a refused Ready: %v, %+v", err, s.Config.Container)
	}
}
