// SPDX-License-Identifier: Apache-2.0

//go:build linux

package adapterhost

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// M2 (Opus on #259; P21b): the root is made exclusively at its attempt's own name, mode 0700, and removing it
// follows no symlink. An adapter that replaced in/ and out/ by symlinks to a file and a directory outside the root
// changes neither: the file keeps its mode 0600, the directory its mode and entry.
func TestTheExchangeRootFollowsNoSymlink(t *testing.T) {
	parent, outside := t.TempDir(), t.TempDir()
	secret, dir := filepath.Join(outside, "secret"), filepath.Join(outside, "dir")
	if err := errors.Join(os.WriteFile(secret, []byte("s"), 0o600), os.Mkdir(dir, 0o700), os.WriteFile(filepath.Join(dir, "kept"), nil, 0o600),
		os.Chmod(dir, 0o500)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	x, err := newExchange(parent, attID, []byte("prompt"))
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(filepath.Join(parent, ExchangeName(attID))); err != nil || st.Mode() != os.ModeDir|0o700 {
		t.Fatalf("the root is %v, %v; want a directory at 0700", st.Mode(), err)
	}
	if _, err := newExchange(parent, attID, nil); !errors.Is(err, ErrExchange) {
		t.Fatalf("a second root for the attempt: %v", err)
	}
	if err := errors.Join(x.outDir.Close(), os.Chmod(x.in, 0o755), os.RemoveAll(x.in), os.Remove(x.out),
		os.Symlink(secret, x.in), os.Symlink(dir, x.out)); err != nil {
		t.Fatal(err)
	}
	x.outDir = nil
	if err := x.remove(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
		t.Fatalf("left behind: %v %v", entries, err)
	}
	st, err := os.Stat(secret)
	dst, derr := os.Stat(dir)
	_, kerr := os.Lstat(filepath.Join(dir, "kept"))
	if err = errors.Join(err, derr, kerr); err != nil || st.Mode() != 0o600 || dst.Mode() != os.ModeDir|0o500 {
		t.Fatalf("outside the root: %v and %v, %v", st.Mode(), dst.Mode(), err)
	}
}
