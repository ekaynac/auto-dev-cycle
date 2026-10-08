// SPDX-License-Identifier: Apache-2.0

// Package testguard is the in-process test layer of docs/design/m0/11 ("Tests
// run inside a sandbox"): temp roots for the event store and blob store, and a
// temporary HOME, each refused when it would lie in a real home or outside
// the temp directory. It is defence in depth; the boundary is the test
// sandbox (scripts/test-sandbox.sh). Only _test files import it: it imports
// testing. The fakes are in internal/testguard/fakes.
package testguard

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// realHomes are the homes no test root may lie in: the HOME this process
// started with, and the host's home the test sandbox names. tempRoot is the
// temp directory it started with: a test that moves TMPDIR does not move it.
var realHomes, tempRoot = startHomes(), resolve(os.TempDir())

func startHomes() []string {
	var homes []string
	for _, v := range []string{"HOME", "ADC_SANDBOX_HOST_HOME"} {
		if h := os.Getenv(v); h != "" && filepath.IsAbs(h) && filepath.Clean(h) != "/" {
			homes = append(homes, resolve(h))
		}
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

func root(t testing.TB) string {
	t.Helper()
	d := t.TempDir()
	if err := Check(d); err != nil {
		t.Fatalf("testguard: %v", err)
	}
	return d
}

// Check refuses a test root that, with symlinks resolved, lies outside the
// starting temp directory or in a real home.
func Check(dir string) error {
	d := resolve(dir)
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
func resolve(p string) string {
	p, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest, p = filepath.Join(filepath.Base(p), rest), parent
	}
}

// within reports whether p is dir or below it.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && filepath.IsLocal(rel)
}
