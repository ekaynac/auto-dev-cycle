// SPDX-License-Identifier: Apache-2.0

// Package testguard is the in-process test layer of docs/design/m0/11 ("Tests
// run inside a sandbox"): temp roots for the event store and blob store, and a
// temporary HOME, each refused when it would lie in a real home or outside
// the temp directory. It is defence in depth; the boundary is the test
// sandbox (scripts/test-sandbox.sh). Only _test files import it: it imports
// testing. The fakes are in internal/testguard/fakes.
//
// A temp directory inside a real home (TMPDIR=$HOME/tmp, say) makes every
// root fail as "in the real home": the guard fails closed, loudly.
package testguard

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// realHomes are the homes no test root may lie in: the HOME this process
// started with, the host's home the test sandbox names, and the user
// database's home for this uid. tempRoot is the temp directory the process
// started with: a test that moves TMPDIR does not move it.
var (
	realHomes         = startHomes()
	tempRoot, tempErr = resolve(os.TempDir())
)

func startHomes() []string {
	candidates := []string{os.Getenv("HOME"), os.Getenv("ADC_SANDBOX_HOST_HOME")}
	if u, err := user.Current(); err == nil {
		candidates = append(candidates, u.HomeDir)
	}
	var homes []string
	for _, h := range candidates {
		if h == "" || !filepath.IsAbs(h) || filepath.Clean(h) == "/" {
			continue
		}
		r, err := resolve(h)
		if err != nil {
			r = filepath.Clean(h)
		}
		homes = append(homes, r)
	}
	return homes
}

// xdgDirs are the base directories Home moves with HOME, so code that reads
// one of them never reaches the user's.
var xdgDirs = map[string]string{
	"XDG_CONFIG_HOME": ".config", "XDG_STATE_HOME": ".local/state",
	"XDG_DATA_HOME": ".local/share", "XDG_CACHE_HOME": ".cache",
}

// StateDir returns a not-yet-existing state directory for an event store and
// its blob store under the test's temp dir (AcquireLock creates it 0700;
// t.TempDir itself may be group-accessible).
func StateDir(t testing.TB) string {
	t.Helper()
	return filepath.Join(root(t), "state")
}

// Home points HOME and the XDG base directories at a fresh directory under
// the test's temp dir for the test's duration, and returns it.
func Home(t testing.TB) string {
	t.Helper()
	h := root(t)
	t.Setenv("HOME", h)
	for v, rel := range xdgDirs {
		t.Setenv(v, filepath.Join(h, rel))
	}
	return h
}

// HomeEnv is a child process's HOME and XDG base directories, in a fresh
// checked directory under the test's temp dir, for an explicit environment
// (exec.Cmd.Env, an adapter's Env) that would otherwise name none and leave
// the child to fall back to the real home.
func HomeEnv(t testing.TB) []string {
	t.Helper()
	h := root(t)
	env := []string{"HOME=" + h}
	for _, v := range slices.Sorted(maps.Keys(xdgDirs)) {
		env = append(env, v+"="+filepath.Join(h, xdgDirs[v]))
	}
	return env
}

func root(t testing.TB) string {
	t.Helper()
	// The temp directory first: t.TempDir creates its root before Check.
	if err := Check(os.TempDir()); err != nil {
		t.Fatalf("testguard: %v", err)
	}
	d := t.TempDir()
	if err := Check(d); err != nil {
		t.Fatalf("testguard: %v", err)
	}
	return d
}

// Check refuses a test root that, with symlinks resolved, lies outside the
// starting temp directory or in a real home. It refuses a ".." component
// outright: cleaning one away before resolving the symlink before it would
// check another path than the one the filesystem reaches.
func Check(dir string) error {
	if slices.Contains(strings.Split(filepath.ToSlash(dir), "/"), "..") {
		return fmt.Errorf("test root %s has a .. component", dir)
	}
	if tempErr != nil {
		return fmt.Errorf("the temp directory does not resolve: %w", tempErr)
	}
	d, err := resolve(dir)
	if err != nil {
		return fmt.Errorf("test root %s: %w", dir, err)
	}
	if !within(tempRoot, d) {
		return fmt.Errorf("test root %s is outside the temp directory %s", d, tempRoot)
	}
	for _, h := range realHomes {
		if within(h, d) {
			return fmt.Errorf("test root %s is in the real home %s", d, h)
		}
	}
	return nil
}

// resolve is p made absolute with symlinks resolved, as far as it exists.
// A component that exists but does not resolve (a dangling symlink, which a
// write would follow) is an error.
func resolve(p string) (string, error) {
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rest := ""
	for {
		r, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(r, rest), nil
		}
		if _, lerr := os.Lstat(p); !errors.Is(lerr, fs.ErrNotExist) {
			return "", fmt.Errorf("%s exists but does not resolve: %w", p, err)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest), nil
		}
		rest, p = filepath.Join(filepath.Base(p), rest), parent
	}
}

// within reports whether p is dir or below it.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && filepath.IsLocal(rel)
}
