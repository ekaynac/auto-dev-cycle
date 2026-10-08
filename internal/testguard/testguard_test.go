// SPDX-License-Identifier: Apache-2.0

package testguard

import (
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// withHome makes a fresh temp directory one of the real homes for the test.
func withHome(t *testing.T) string {
	home := mustResolve(t, t.TempDir())
	saved := realHomes
	realHomes = append(append([]string(nil), saved...), home)
	t.Cleanup(func() { realHomes = saved })
	return home
}

func mustResolve(t *testing.T, p string) string {
	t.Helper()
	r, err := resolve(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCheck(t *testing.T) {
	// Order matters: the first t.TempDir (in withHome) fixes this test's
	// temp root before TMPDIR moves below, and later calls reuse it.
	home := withHome(t)
	links := t.TempDir()
	link, sub, dangling := filepath.Join(links, "link"), filepath.Join(links, "sub"), filepath.Join(links, "dangling")
	if err := os.MkdirAll(filepath.Join(home, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	for l, target := range map[string]string{link: home, sub: filepath.Join(home, "subdir"), dangling: filepath.Join(home, "missing")} {
		if err := os.Symlink(target, l); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(filepath.Dir(tempRoot), "adc-not-temp")
	t.Setenv("TMPDIR", outside) // a test may move TMPDIR; the guard keeps the starting one
	for _, c := range []struct {
		name, dir, want string
	}{
		{"a temp root", t.TempDir(), ""},
		{"a root not yet created", filepath.Join(t.TempDir(), "a", "b"), ""},
		{"a root in the real home", filepath.Join(home, "state"), "real home"},
		{"the real home itself", home, "real home"},
		{"a symlink into the real home", filepath.Join(link, "state"), "real home"},
		{"a root outside the temp directory", outside, "outside the temp directory"},
		{"a temp-looking path that climbs out", filepath.Join(tempRoot, "..", "etc"), "outside the temp directory"},
		{"a root in a TMPDIR moved outside", filepath.Join(os.TempDir(), "state"), "outside the temp directory"},
		// sub/.. is the home on the filesystem, links/ once cleaned.
		{"a literal .. after a symlink", sub + "/../credentials", ".. component"},
		{"a literal .. that stays inside", t.TempDir() + "/a/../b", ".. component"},
		// A write through a dangling link creates its target in the home.
		{"a dangling symlink into the real home", dangling, "does not resolve"},
		{"a root below a dangling symlink", filepath.Join(dangling, "state"), "does not resolve"},
	} {
		err := Check(c.dir)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: Check(%s) = %v, want %q", c.name, c.dir, err, c.want)
		}
	}
}

// The guarded homes (the package's own, not a fresh computation) include
// the starting HOME, the sandbox's host home when set, and the user
// database's home.
func TestRealHomesIncludeStartHome(t *testing.T) {
	var want []string
	for _, h := range []string{os.Getenv("HOME"), os.Getenv("ADC_SANDBOX_HOST_HOME")} {
		if h != "" && filepath.Clean(h) != "/" {
			want = append(want, mustResolve(t, h))
		}
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "/" && u.HomeDir != "" {
		want = append(want, mustResolve(t, u.HomeDir))
	}
	if len(want) == 0 {
		t.Skip("no home to guard")
	}
	for _, w := range want {
		if !slices.Contains(realHomes, w) {
			t.Errorf("real homes %v do not include %s", realHomes, w)
		}
	}
}

// A relative HOME or HOME=/ is never a guarded home: "/" would refuse every
// root, and a relative one would be resolved against the working directory.
func TestStartHomesSkipsRelativeAndRoot(t *testing.T) {
	t.Setenv("ADC_SANDBOX_HOST_HOME", "")
	for _, h := range []string{"relative/home", "/"} {
		t.Setenv("HOME", h)
		for _, got := range startHomes() {
			if got == "/" || !filepath.IsAbs(got) || strings.HasSuffix(got, "relative/home") {
				t.Errorf("HOME=%s gives the guarded home %s", h, got)
			}
		}
	}
}

// startHomes takes the sandbox's host home when it is set.
func TestStartHomesTakesHostHome(t *testing.T) {
	t.Setenv("ADC_SANDBOX_HOST_HOME", "/nonexistent/host-home")
	if homes := startHomes(); !slices.Contains(homes, "/nonexistent/host-home") {
		t.Errorf("startHomes() = %v, without the host home", homes)
	}
}

func TestWithin(t *testing.T) {
	for p, want := range map[string]bool{
		tempRoot:                           true,
		filepath.Join(tempRoot, "x"):       true,
		filepath.Dir(tempRoot):             false,
		tempRoot + "-sibling/x":            false,
		filepath.Join(tempRoot, "..", "x"): false,
	} {
		if got := within(tempRoot, p); got != want {
			t.Errorf("within(%s, %s) = %v, want %v", tempRoot, p, got, want)
		}
	}
}

// fatalTB records a Fatalf and ends its goroutine, as testing.T does.
type fatalTB struct {
	testing.TB
	tempDir string
	made    bool // TempDir was called
	failed  string
}

func (f *fatalTB) TempDir() string { f.made = true; return f.tempDir }
func (f *fatalTB) Fatalf(format string, args ...any) {
	f.failed = format
	panic(f)
}

// StateDir and Home refuse a temp dir in a real home before using it.
func TestRootsRefuseRealHome(t *testing.T) {
	home := withHome(t)
	homeEnv := func(tb testing.TB) string { return strings.Join(HomeEnv(tb), " ") }
	for name, use := range map[string]func(testing.TB) string{"StateDir": StateDir, "Home": Home, "HomeEnv": homeEnv} {
		f := &fatalTB{TB: t, tempDir: filepath.Join(home, "tmp")}
		func() {
			defer func() {
				if r := recover(); r != nil && r != any(f) {
					panic(r)
				}
			}()
			got := use(f)
			t.Errorf("%s returned %s inside the real home", name, got)
		}()
		if f.failed == "" {
			t.Errorf("%s did not fail the test", name)
		}
	}
	if os.Getenv("HOME") == filepath.Join(home, "tmp") {
		t.Error("a refused Home still moved HOME")
	}
}

// With TMPDIR moved outside the temp directory, a root fails before
// t.TempDir creates anything there.
func TestRootChecksTempDirFirst(t *testing.T) {
	f := &fatalTB{TB: t, tempDir: t.TempDir()}
	t.Setenv("TMPDIR", filepath.Join(filepath.Dir(tempRoot), "adc-not-temp"))
	func() {
		defer func() {
			if r := recover(); r != nil && r != any(f) {
				panic(r)
			}
		}()
		StateDir(f)
	}()
	if f.failed == "" || f.made {
		t.Errorf("StateDir under a moved TMPDIR: failed %q, TempDir called %v", f.failed, f.made)
	}
}

func TestHome(t *testing.T) {
	start := os.Getenv("HOME")
	h := Home(t)
	if got, err := os.UserHomeDir(); err != nil || got != h || got == start {
		t.Errorf("home after Home is %q (%v), want %q, not %q", got, err, h, start)
	}
	if err := Check(h); err != nil {
		t.Error(err)
	}
	// Spelled out, not read from xdgDirs: dropping a mapping must fail here.
	for v, rel := range map[string]string{
		"XDG_CONFIG_HOME": ".config", "XDG_STATE_HOME": ".local/state",
		"XDG_DATA_HOME": ".local/share", "XDG_CACHE_HOME": ".cache",
	} {
		if got := os.Getenv(v); got != filepath.Join(h, rel) {
			t.Errorf("%s = %q, want it under %s", v, got, h)
		}
	}
}

// A child's HOME and XDG directories lie in one checked temp directory.
func TestHomeEnv(t *testing.T) {
	env := HomeEnv(t)
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	h := got["HOME"]
	if err := Check(h); err != nil || h == "" {
		t.Fatalf("HomeEnv HOME %q: %v", h, err)
	}
	for v, rel := range map[string]string{
		"XDG_CONFIG_HOME": ".config", "XDG_STATE_HOME": ".local/state",
		"XDG_DATA_HOME": ".local/share", "XDG_CACHE_HOME": ".cache",
	} {
		if got[v] != filepath.Join(h, rel) {
			t.Errorf("%s = %q, want it under %s", v, got[v], h)
		}
	}
	if len(env) != 5 || os.Getenv("HOME") == h {
		t.Errorf("HomeEnv = %v; it must not move this process's HOME", env)
	}
}

func TestStateDir(t *testing.T) {
	d := StateDir(t)
	if _, err := os.Lstat(d); !os.IsNotExist(err) {
		t.Errorf("StateDir %s exists already (%v): AcquireLock must create it private", d, err)
	}
	if err := Check(d); err != nil {
		t.Error(err)
	}
}
