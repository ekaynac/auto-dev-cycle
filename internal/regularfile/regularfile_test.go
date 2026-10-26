// SPDX-License-Identifier: Apache-2.0

package regularfile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The rule: a link is never opened through, a FIFO never blocks, and what
// was opened must be a regular file and, given the walk's Lstat, that file.
func TestOpenRegular(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fa, err := os.OpenFile(a, Flags, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer fa.Close()
	la, _ := os.Lstat(a)
	lb, _ := os.Lstat(b)
	if _, err := Check(fa, la); err != nil {
		t.Errorf("the file listed: %v", err)
	}
	if _, err := Check(fa, lb); !errors.Is(err, ErrChanged) {
		t.Errorf("another file than the one listed: %v", err)
	}
	if err := os.Symlink(a, filepath.Join(dir, "l")); err != nil {
		t.Fatal(err)
	}
	if f, err := os.OpenFile(filepath.Join(dir, "l"), Flags, 0); err == nil {
		f.Close()
		t.Error("a link was opened through")
	}
	fifo := filepath.Join(dir, "p")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(fifo, Flags, 0) // never blocks
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := Check(f, nil); !errors.Is(err, ErrChanged) {
		t.Errorf("a FIFO: %v", err)
	}
}
