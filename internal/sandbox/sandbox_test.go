// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
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
	return Host{Podman: "/usr/bin/podman", Root: root, Runner: f, Home: "/home/op",
		Forbidden: []string{"/srv/adc/state", "/srv/adc/state/adcd.sock", "/run/adc/adcd-shared.sock", "/run/user/1000/podman/podman.sock"}}
}

// socket listens on a fresh unix socket and returns its path.
func socket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gw")
	must(t, err)
	p := filepath.Join(dir, "gw.sock")
	ln, err := net.Listen("unix", p)
	must(t, err)
	t.Cleanup(func() { ln.Close(); os.RemoveAll(dir) })
	return p
}

func testRun(t *testing.T, h Host, mode Mode) Run {
	t.Helper()
	a, err := h.NewAttempt()
	must(t, err)
	r := Run{Image: Image{Pinned: "x@sha256:" + testID, ID: testID}, Mode: mode, Attempt: a,
		Limits: Limits{Memory: 1 << 30, CPUs: "1", PIDs: 10, Timeout: time.Minute}, Argv: []string{"true"}}
	if mode == Build {
		r.Gateway = socket(t)
	}
	return r
}

func TestValidateRefuses(t *testing.T) {
	for name, h := range map[string]Host{
		"relative root":            {Podman: "/usr/bin/podman", Root: "sbx", Home: "/home/op"},
		"no home":                  {Podman: "/usr/bin/podman", Root: "/sbx"},
		"root too long for Podman": {Podman: "/usr/bin/podman", Root: "/" + strings.Repeat("r", 60), Home: "/home/op"},
		"forbidden inside root":    {Podman: "/usr/bin/podman", Root: "/sbx", Home: "/home/op", Forbidden: []string{"/sbx/state"}},
		"forbidden is /":           {Podman: "/usr/bin/podman", Root: "/sbx", Home: "/home/op", Forbidden: []string{"/"}},
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
	if _, err := h.Start(context.Background(), testRun(t, h, Gate)); err != nil {
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
		args, err := h.args(testRun(t, h, mode), "n")
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
// directory, a controller socket, Podman's storage or the home, directly
// or through a symlink, is refused before Podman starts.
func TestArgsRefuseControllerMounts(t *testing.T) {
	f := &fake{}
	h := testHost(t, f)
	state := t.TempDir() // a controller directory outside the root
	n := 0
	link := func(dir, target string) string {
		n++
		p := filepath.Join(dir, "l"+strconv.Itoa(n))
		must(t, os.Symlink(target, p))
		return p
	}
	runs, outside := filepath.Join(h.Root, "runs"), t.TempDir()
	attemptAt := func(dir string) *Attempt {
		must(t, os.MkdirAll(filepath.Join(dir, "workspace"), 0o700))
		must(t, os.MkdirAll(filepath.Join(dir, "results"), 0o700))
		return &Attempt{Dir: dir}
	}
	plain := filepath.Join(t.TempDir(), "plain")
	must(t, os.WriteFile(plain, nil, 0o600))
	cases := map[string]func(*Run, *Host){
		"attempt outside the root":         func(r *Run, _ *Host) { r.Attempt = attemptAt(state) },
		"attempt is Podman's storage":      func(r *Run, _ *Host) { r.Attempt = &Attempt{Dir: filepath.Join(h.Root, "storage")} },
		"attempt linked to the state dir":  func(r *Run, _ *Host) { attemptAt(state); r.Attempt = &Attempt{Dir: link(runs, state)} },
		"attempt with a comma":             func(r *Run, _ *Host) { r.Attempt = attemptAt(filepath.Join(h.Root, "runs", "a,dst=x")) },
		"no attempt":                       func(r *Run, _ *Host) { r.Attempt = nil },
		"forbidden through a symlink":      func(r *Run, h *Host) { h.Forbidden = []string{link(outside, r.Attempt.Workspace())} },
		"workspace contains the home":      func(r *Run, h *Host) { h.Home = filepath.Join(r.Attempt.Workspace(), "op") },
		"gateway is a forbidden socket":    func(r *Run, h *Host) { h.Forbidden = []string{r.Gateway} },
		"gateway is a regular file":        func(r *Run, _ *Host) { r.Gateway = plain },
		"gateway is a symlink to a socket": func(r *Run, _ *Host) { r.Gateway = link(outside, r.Gateway) },
		"gate run with a gateway socket":   func(r *Run, _ *Host) { r.Mode = Gate },
		"build run without a gateway":      func(r *Run, _ *Host) { r.Gateway = "" },
		"unresolved image":                 func(r *Run, _ *Host) { r.Image.ID = "" },
		"no pids limit":                    func(r *Run, _ *Host) { r.Limits.PIDs = 0 },
		"no timeout":                       func(r *Run, _ *Host) { r.Limits.Timeout = 0 },
	}
	for name, mutate := range cases {
		r, hh := testRun(t, h, Build), h
		mutate(&r, &hh)
		if _, err := hh.Start(context.Background(), r); !errors.Is(err, ErrSpec) {
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
	r := testRun(t, h, Gate)
	r.Limits.Timeout = 10 * time.Millisecond
	if _, err := h.Start(context.Background(), r); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start = %v, want the deadline", err)
	}
	if r.Attempt.unsafe {
		t.Fatal("an attempt whose container was removed is marked unsafe")
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

// CopyTree keeps the tree, symlinks as links, and drops every .git; a FIFO
// is never opened; Teardown removes directories a run made unreadable.
func TestCopyTreeAndTeardown(t *testing.T) {
	h := testHost(t, &fake{})
	a, err := h.NewAttempt()
	must(t, err)
	ws := a.Workspace()
	for _, d := range []string{".git/hooks", "pkg/.git", "pkg/sub", "locked/in"} {
		must(t, os.MkdirAll(filepath.Join(ws, d), 0o700))
	}
	must(t, os.WriteFile(filepath.Join(ws, ".git", "config"), []byte("[core]\n\tfsmonitor = evil\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(ws, "pkg", "sub", "a.go"), []byte("package a\n"), 0o600))
	must(t, os.Symlink("/etc/passwd", filepath.Join(ws, "pkg", "link")))
	must(t, syscall.Mkfifo(filepath.Join(ws, "pkg", "fifo"), 0o600))
	dst := filepath.Join(t.TempDir(), "tree")
	if err := a.CopyTree(dst, 1<<20); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "pkg", "sub", "a.go")); err != nil || string(b) != "package a\n" {
		t.Errorf("a.go = %q, %v", b, err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "pkg", "link")); err != nil || target != "/etc/passwd" {
		t.Errorf("link = %q, %v; want the link itself", target, err)
	}
	for _, p := range []string{".git", "pkg/.git", "pkg/fifo"} {
		if _, err := os.Lstat(filepath.Join(dst, p)); !os.IsNotExist(err) {
			t.Errorf("%s was copied: %v", p, err)
		}
	}
	must(t, os.Chmod(filepath.Join(ws, "locked"), 0))
	must(t, a.Teardown())
	if _, err := os.Lstat(a.Dir); !os.IsNotExist(err) {
		t.Errorf("teardown left %s: %v", a.Dir, err)
	}
}

// When a timed-out container could not be removed, nothing is copied out:
// it may still be writing.
func TestUnsafeAttemptRefusesCopyOut(t *testing.T) {
	f := &fake{fn: func(ctx context.Context, args []string) ([]byte, error) {
		if args[len(args)-1] == "true" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return nil, errors.New("rm failed")
	}}
	h := testHost(t, f)
	r := testRun(t, h, Gate)
	r.Limits.Timeout = 10 * time.Millisecond
	if _, err := h.Start(context.Background(), r); err == nil {
		t.Fatal("Start succeeded")
	}
	if err := r.Attempt.CopyOut(filepath.Join(t.TempDir(), "out"), 1024); !errors.Is(err, ErrSpec) {
		t.Fatalf("CopyOut of an unsafe attempt: %v, want ErrSpec", err)
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
