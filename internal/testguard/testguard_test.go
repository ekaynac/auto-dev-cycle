// SPDX-License-Identifier: Apache-2.0

package testguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withHome makes a fresh temp directory one of the real homes for the test.
func withHome(t *testing.T) string {
	home := resolve(t.TempDir())
	saved := realHomes
	realHomes = append(append([]string(nil), saved...), home)
	t.Cleanup(func() { realHomes = saved })
	return home
}

func TestCheck(t *testing.T) {
	home := withHome(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
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
	} {
		err := Check(c.dir)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: Check(%s) = %v, want %q", c.name, c.dir, err, c.want)
		}
	}
}

// The real homes include the HOME this process started with.
func TestRealHomesIncludeStartHome(t *testing.T) {
	h := os.Getenv("HOME")
	if h == "" || h == "/" {
		t.Skip("no HOME to guard")
	}
	t.Setenv("ADC_SANDBOX_HOST_HOME", "/nonexistent/host-home")
	homes := startHomes()
	for _, want := range []string{resolve(h), "/nonexistent/host-home"} {
		found := false
		for _, got := range homes {
			found = found || got == want
		}
		if !found {
			t.Errorf("real homes %v do not include %s", homes, want)
		}
	}
}

// fatalTB records a Fatalf and ends its goroutine, as testing.T does.
type fatalTB struct {
	testing.TB
	tempDir string
	failed  string
}

func (f *fatalTB) TempDir() string { return f.tempDir }
func (f *fatalTB) Fatalf(format string, args ...any) {
	f.failed = format
	panic(f)
}

// StateDir and Home refuse a temp dir in a real home before using it.
func TestRootsRefuseRealHome(t *testing.T) {
	home := withHome(t)
	for name, use := range map[string]func(testing.TB) string{"StateDir": StateDir, "Home": Home} {
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

func TestHome(t *testing.T) {
	start := os.Getenv("HOME")
	h := Home(t)
	if got, err := os.UserHomeDir(); err != nil || got != h || got == start {
		t.Errorf("home after Home is %q (%v), want %q, not %q", got, err, h, start)
	}
	if err := Check(h); err != nil {
		t.Error(err)
	}
	for v, rel := range xdgDirs {
		if got := os.Getenv(v); got != filepath.Join(h, rel) {
			t.Errorf("%s = %q, want it under %s", v, got, h)
		}
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
