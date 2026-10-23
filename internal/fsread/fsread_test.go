// SPDX-License-Identifier: Apache-2.0

package fsread

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRegular: a record or report is a regular file read within its
// bound: never through a link, never a device, never past the bound, even
// when its size says otherwise (Astra #306 M4).
func TestRegular(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok.json")
	if err := os.WriteFile(ok, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ok, filepath.Join(dir, "link.json")); err != nil {
		t.Fatal(err)
	}
	if b, err := Regular(ok, 10); err != nil || string(b) != "0123456789" {
		t.Errorf("a file at its bound: %q, %v", b, err)
	}
	for name, c := range map[string]struct {
		path  string
		limit int64
		why   string
	}{
		"a link to a regular file": {filepath.Join(dir, "link.json"), 10, "symbolic link"},
		"a link to /dev/zero":      {filepath.Join(dir, "zero.json"), 10, "symbolic link"},
		"/dev/zero":                {"/dev/zero", 10, "not a regular file"},
		"/dev/null":                {"/dev/null", 10, "not a regular file"},
		"a directory":              {dir, 10, "not a regular file"},
		"a file past its bound":    {ok, 9, "is 10 bytes, over 9"},
		"a file whose size lies":   {"/proc/self/status", 10, "grew past 10 bytes"},
	} {
		if name == "a link to /dev/zero" {
			if err := os.Symlink("/dev/zero", c.path); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := os.Lstat(c.path); err != nil {
			t.Logf("%s: %v; skipped", name, err)
			continue
		}
		if b, err := Regular(c.path, c.limit); err == nil || !strings.Contains(err.Error(), c.why) || int64(len(b)) > c.limit+1 {
			t.Errorf("%s: %d bytes, %v; want %q, and never past the bound", name, len(b), err, c.why)
		}
	}
}

// The size classification is its own (ErrOver), the guard's list bound
// told from any other failure.
func TestOverIsItsOwn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Regular(p, 9); !errors.Is(err, ErrOver) {
		t.Errorf("past its bound: %v", err)
	}
	if _, err := Regular(filepath.Join(t.TempDir(), "none"), 9); err == nil || errors.Is(err, ErrOver) {
		t.Errorf("a missing file: %v", err)
	}
	if _, err := Regular("/proc/self/status", 10); !errors.Is(err, ErrOver) || !strings.Contains(err.Error(), "grew past") {
		t.Errorf("a file whose size lies: %v", err)
	}
}
