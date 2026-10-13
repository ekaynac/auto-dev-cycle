// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fake is the runner boundary: it records each call and answers from fn.
type fake struct {
	calls [][]string
	envs  [][]string
	fn    func(ctx context.Context, args []string) ([]byte, error)
}

func (f *fake) Run(ctx context.Context, name string, args, env []string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	f.envs = append(f.envs, env)
	if f.fn == nil {
		return nil, nil
	}
	return f.fn(ctx, args)
}

const testID = "4105e2bb7b57b01eaf420afd4f7970386183f64f4d244bca3d684fa9737e0670"

func testHost(t *testing.T, f *fake) Host {
	t.Helper()
	root, err := os.MkdirTemp("", "sbx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return Host{Podman: "/usr/bin/podman", Root: root, Runner: f,
		Forbidden: []string{"/srv/adc/state", "/srv/adc/state/adcd.sock", "/run/adc/adcd-shared.sock", "/run/user/1000/podman/podman.sock"}}
}

func testRun(h Host, mode Mode) Run {
	r := Run{Image: Image{Pinned: "x@sha256:" + testID, ID: testID}, Mode: mode,
		Workspace: filepath.Join(h.Root, "runs/a/workspace"), Results: filepath.Join(h.Root, "runs/a/results"),
		Limits: Limits{Memory: "1g", CPUs: "1", PIDs: 10, Timeout: time.Minute}, Argv: []string{"true"}}
	if mode == Build {
		r.Gateway = "/run/adc-gw/gateway.sock"
	}
	return r
}

func TestValidateRefuses(t *testing.T) {
	for name, h := range map[string]Host{
		"relative root":            {Podman: "/usr/bin/podman", Root: "sbx"},
		"root too long for Podman": {Podman: "/usr/bin/podman", Root: "/" + strings.Repeat("r", 60)},
		"forbidden inside root":    {Podman: "/usr/bin/podman", Root: "/sbx", Forbidden: []string{"/sbx/state"}},
		"forbidden is /":           {Podman: "/usr/bin/podman", Root: "/sbx", Forbidden: []string{"/"}},
	} {
		if err := h.Validate(); !errors.Is(err, ErrHost) {
			t.Errorf("%s: %v, want ErrHost", name, err)
		}
	}
}

// Podman's environment is exactly the host's: a controller credential in
// the controller's environment never reaches Podman.
func TestPodmanEnvironmentIsEmptied(t *testing.T) {
	t.Setenv("ADC_CANARY_TOKEN", "canary")
	f := &fake{}
	h := testHost(t, f)
	if _, err := h.Start(context.Background(), testRun(h, Gate)); err != nil {
		t.Fatal(err)
	}
	env := f.envs[0]
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if strings.Contains(kv, "canary") || (strings.HasPrefix(k, "XDG") || k == "HOME") && !strings.HasPrefix(v, h.Root+"/") {
			t.Errorf("podman env %q escapes the sandbox root", kv)
		}
	}
	if len(env) != 7 {
		t.Errorf("podman env = %q, want exactly HOME, PATH and five XDG_ variables", env)
	}
	args := f.calls[0]
	if !slices.Contains(args, "--root") || !slices.Contains(args, filepath.Join(h.Root, "storage")) {
		t.Errorf("podman storage is not under the sandbox root: %q", args)
	}
}

func TestArgsContainment(t *testing.T) {
	h := testHost(t, &fake{})
	for _, mode := range []Mode{Gate, Build} {
		args, err := h.args(testRun(h, mode), "n")
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"--network=none", "--pull=never", "--cap-drop=all", "--env-host=false", "--read-only", testID} {
			if !slices.Contains(args, want) {
				t.Errorf("%s: args lack %s: %q", mode, want, args)
			}
		}
		gw := slices.ContainsFunc(args, func(a string) bool { return strings.HasSuffix(a, ",dst="+GatewaySocket) })
		if gw != (mode == Build) {
			t.Errorf("%s: gateway mounted = %v", mode, gw)
		}
		if slices.Contains(args, "x@sha256:"+testID) {
			t.Errorf("%s: the run names the reference, not the resolved ID", mode)
		}
	}
}

// The F fault at the boundary: a run that would mount the state
// directory, a controller socket or Podman's storage is refused before
// Podman starts.
func TestArgsRefuseControllerMounts(t *testing.T) {
	f := &fake{}
	h := testHost(t, f)
	cases := map[string]func(*Run){
		"workspace is the state directory":  func(r *Run) { r.Workspace = "/srv/adc/state" },
		"results inside the state dir":      func(r *Run) { r.Results = "/srv/adc/state/blobs" },
		"workspace contains the state dir":  func(r *Run) { r.Workspace = "/srv/adc" },
		"gateway is adcd's shared socket":   func(r *Run) { r.Gateway = "/run/adc/adcd-shared.sock" },
		"gateway is the Podman API socket":  func(r *Run) { r.Gateway = "/run/user/1000/podman/podman.sock" },
		"workspace is Podman's storage":     func(r *Run) { r.Workspace = filepath.Join(h.Root, "storage") },
		"relative workspace":                func(r *Run) { r.Workspace = "ws" },
		"gate run with a gateway socket":    func(r *Run) { r.Mode = Gate },
		"build run without a gateway":       func(r *Run) { r.Gateway = "" },
		"unresolved image":                  func(r *Run) { r.Image.ID = "" },
		"no pids limit":                     func(r *Run) { r.Limits.PIDs = 0 },
		"no timeout":                        func(r *Run) { r.Limits.Timeout = 0 },
		"results contain the Podman socket": func(r *Run) { r.Results = "/run/user/1000/podman" },
		"a comma adds mount options":        func(r *Run) { r.Workspace = "/w,dst=/srv/adc/state" },
	}
	for name, mutate := range cases {
		r := testRun(h, Build)
		mutate(&r)
		if _, err := h.Start(context.Background(), r); !errors.Is(err, ErrSpec) {
			t.Errorf("%s: %v, want ErrSpec", name, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("podman ran for a refused run: %q", f.calls)
	}
}

// A run that outlives its timeout is removed by name: killing the client
// alone would leave the container running.
func TestTimeoutRemovesContainer(t *testing.T) {
	f := &fake{fn: func(ctx context.Context, args []string) ([]byte, error) {
		if args[len(args)-1] == "true" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return nil, nil
	}}
	h := testHost(t, f)
	r := testRun(h, Gate)
	r.Limits.Timeout = 10 * time.Millisecond
	if _, err := h.Start(context.Background(), r); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start = %v, want the deadline", err)
	}
	name := ""
	for _, a := range f.calls[0] {
		if n, ok := strings.CutPrefix(a, "--name="); ok {
			name = n
		}
	}
	if len(f.calls) != 2 || name == "" || !slices.Contains(f.calls[1], "rm") || f.calls[1][len(f.calls[1])-1] != name {
		t.Fatalf("calls = %q, want the run then rm of its name", f.calls)
	}
}

func TestResolve(t *testing.T) {
	ref := "docker.io/library/debian@sha256:" + strings.Repeat("a", 64)
	inspect := func(digests string) func(context.Context, []string) ([]byte, error) {
		return func(context.Context, []string) ([]byte, error) {
			return []byte(`[{"Id":"` + testID + `","RepoDigests":[` + digests + `],"Labels":{"dev.adc.prepared":"{\"toolchain\":\"go\",\"lockfiles\":{\"go.sum\":\"sha256:1\"}}"}}]`), nil
		}
	}
	h := testHost(t, &fake{fn: inspect(`"` + ref + `"`)})
	img, err := h.Resolve(context.Background(), ref)
	if err != nil || img.ID != testID || img.Manifest.Toolchain != "go" || img.Manifest.Lockfiles["go.sum"] != "sha256:1" {
		t.Fatalf("Resolve = %+v, %v", img, err)
	}
	// The F fault for images: another digest under the same name.
	other := "docker.io/library/debian@sha256:" + strings.Repeat("b", 64)
	for name, h := range map[string]Host{
		"digest differs": testHost(t, &fake{fn: inspect(`"` + other + `"`)}),
		"absent":         testHost(t, &fake{fn: func(context.Context, []string) ([]byte, error) { return nil, errors.New("image not known") }}),
	} {
		if _, err := h.Resolve(context.Background(), ref); !errors.Is(err, ErrImage) {
			t.Errorf("%s: %v, want ErrImage", name, err)
		}
	}
	f := &fake{}
	h = testHost(t, f)
	for _, bad := range []string{"debian:bookworm", "debian@sha256:abc", "-x@sha256:" + strings.Repeat("a", 64) + " "} {
		if _, err := h.Resolve(context.Background(), bad); !errors.Is(err, ErrImage) {
			t.Errorf("%q: %v, want ErrImage", bad, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("podman ran for an unpinned reference")
	}
}

func TestCheckManifest(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "go.sum"), []byte("sum\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("go.sum", filepath.Join(base, "link.sum")); err != nil {
		t.Fatal(err)
	}
	sum, err := fileSum(base, "go.sum")
	if err != nil {
		t.Fatal(err)
	}
	ok := Image{Manifest: Manifest{Toolchain: "go", Lockfiles: map[string]string{"go.sum": sum}}}
	if err := ok.CheckManifest("go", base, []string{"go.sum"}); err != nil {
		t.Fatalf("a matching manifest: %v", err)
	}
	for name, c := range map[string]struct {
		img       Image
		toolchain string
		lockfiles []string
	}{
		"other toolchain":       {ok, "python", []string{"go.sum"}},
		"no manifest":           {Image{}, "go", []string{"go.sum"}},
		"lockfile changed":      {Image{Manifest: Manifest{Toolchain: "go", Lockfiles: map[string]string{"go.sum": "sha256:00"}}}, "go", []string{"go.sum"}},
		"another lockfile set":  {ok, "go", []string{"go.sum", "go.mod"}},
		"a symlinked lockfile":  {Image{Manifest: Manifest{Toolchain: "go", Lockfiles: map[string]string{"link.sum": sum}}}, "go", []string{"link.sum"}},
		"a lockfile outside it": {Image{Manifest: Manifest{Toolchain: "go", Lockfiles: map[string]string{"../go.sum": sum}}}, "go", []string{"../go.sum"}},
	} {
		if err := c.img.CheckManifest(c.toolchain, base, c.lockfiles); !errors.Is(err, ErrManifest) {
			t.Errorf("%s: %v, want ErrManifest", name, err)
		}
	}
}

func TestCopyOut(t *testing.T) {
	h := testHost(t, &fake{})
	a, err := h.NewAttempt()
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "state.db")
	must(t, os.WriteFile(secret, []byte("controller state"), 0o600))
	must(t, os.MkdirAll(filepath.Join(a.Results(), "sub"), 0o700))
	must(t, os.WriteFile(filepath.Join(a.Results(), "sub", "junit.xml"), []byte("<ok/>"), 0o600))
	// A run plants links to controller files: never followed.
	must(t, os.Symlink(secret, filepath.Join(a.Results(), "leak")))
	must(t, os.Symlink(filepath.Dir(secret), filepath.Join(a.Results(), "dir")))
	dst := filepath.Join(t.TempDir(), "out")
	if err := a.CopyOut(dst, 1024); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "sub", "junit.xml"))
	if err != nil || string(got) != "<ok/>" {
		t.Fatalf("copied = %q, %v", got, err)
	}
	for _, p := range []string{"leak", "dir"} {
		if _, err := os.Lstat(filepath.Join(dst, p)); !os.IsNotExist(err) {
			t.Errorf("%s was copied out: %v", p, err)
		}
	}
	if err := a.CopyOut(filepath.Join(t.TempDir(), "small"), 3); !errors.Is(err, ErrSpec) {
		t.Errorf("an over-budget copy-out: %v, want ErrSpec", err)
	}
	must(t, a.Teardown())
	if _, err := os.Stat(a.Dir); !os.IsNotExist(err) {
		t.Errorf("teardown left %s", a.Dir)
	}
}

// A run's output is bounded: more is an error, never a silent cut.
func TestOutputBounded(t *testing.T) {
	out, err := execRunner{}.Run(context.Background(), "/bin/sh", []string{"-c", "echo ok"}, nil)
	if err != nil || string(out) != "ok\n" {
		t.Fatalf("Run = %q, %v", out, err)
	}
	if _, err := (execRunner{}).Run(context.Background(), "/bin/sh", []string{"-c", "yes | head -c 1100000"}, nil); err == nil {
		t.Fatal("an output over the bound was accepted")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
