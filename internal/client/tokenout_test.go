// SPDX-License-Identifier: Apache-2.0

package client

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// A reserved --token-out is created 0600 before anything is sent (whatever
// the umask), and holds the token once written.
func TestTokenOutWrites(t *testing.T) {
	path := filepath.Join(privateDir(t), "agent.token")
	defer syscall.Umask(syscall.Umask(0o077))
	syscall.Umask(0)
	o, err := CreateTokenOut(path)
	must(t, err)
	if st, err := os.Lstat(path); err != nil || st.Mode() != 0o600 {
		t.Fatalf("reserved: %v %v", st, err)
	}
	tok := fakeToken(t)
	must(t, o.Write(tok))
	o.Discard() // a no-op once written
	got, err := os.ReadFile(path)
	if err != nil || string(got) != tok.Reveal()+"\n" {
		t.Fatalf("the file: %v", err)
	}
}

// Nothing existing is ever written through or replaced: a file (the
// caller's own token among them), a symlink, a dangling one; and the
// directory must be the caller's, not group or other writable, and not a
// symlink. Each refusal leaves what was there.
func TestTokenOutRefusals(t *testing.T) {
	_, own := home(t, fakeToken(t).Reveal())
	before, err := os.ReadFile(own)
	must(t, err)
	dir := privateDir(t)
	target := filepath.Join(dir, "target")
	must(t, os.WriteFile(target, []byte("keep"), 0o600))
	must(t, os.Symlink(target, filepath.Join(dir, "link")))
	must(t, os.Symlink(filepath.Join(dir, "absent"), filepath.Join(dir, "dangling")))
	open := privateDir(t)
	must(t, os.Chmod(open, 0o770))
	must(t, os.Symlink(dir, filepath.Join(dir, "dirlink")))
	for path, want := range map[string]error{
		own:                                ErrTokenOutExists,
		target:                             ErrTokenOutExists,
		filepath.Join(dir, "link"):         ErrTokenOutExists,
		filepath.Join(dir, "dangling"):     ErrTokenOutExists,
		filepath.Join(open, "t"):           ErrTokenOutDir,
		filepath.Join(dir, "dirlink", "t"): ErrTokenOutDir,
		filepath.Join(dir, "missing", "t"): ErrTokenOutDir,
	} {
		if _, err := CreateTokenOut(path); !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", path, err, want)
		}
	}
	if got, err := os.ReadFile(own); err != nil || string(got) != string(before) {
		t.Fatal("the caller's own token file changed")
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "keep" {
		t.Fatal("a link was followed")
	}
	if _, err := os.Lstat(filepath.Join(dir, "absent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a dangling link was followed")
	}
	entries, _ := os.ReadDir(open)
	if len(entries) != 0 {
		t.Fatalf("wrote into an unsafe directory: %v", names(entries))
	}
}

// A directory of another owner is refused.
func TestTokenOutRefusesAnotherOwner(t *testing.T) {
	defer func(old func(*unix.Stat_t) int) { fileOwner = old }(fileOwner)
	fileOwner = func(st *unix.Stat_t) int { return int(st.Uid) + 1 }
	if _, err := CreateTokenOut(filepath.Join(privateDir(t), "t")); !errors.Is(err, ErrTokenOutDir) {
		t.Fatalf("got %v", err)
	}
}

// With no token to write, and after a failed write, the reserved file is
// removed, and the error never quotes the token.
func TestTokenOutRemovedWithoutToken(t *testing.T) {
	dir := privateDir(t)
	o, err := CreateTokenOut(filepath.Join(dir, "t"))
	must(t, err)
	o.Discard()
	(*TokenOut)(nil).Discard()
	defer func(old func(*os.File) error) { syncFile = old }(syncFile)
	syncFile = func(*os.File) error { return errors.New("disk full") }
	o, err = CreateTokenOut(filepath.Join(dir, "t"))
	must(t, err)
	err = o.Write(fakeToken(t))
	if err == nil {
		t.Fatal("a failed write succeeded")
	}
	noSecret(t, err)
	if entries, _ := os.ReadDir(dir); !slices.Equal(names(entries), nil) {
		t.Fatalf("leftovers %v", names(entries))
	}
}

// privateDir is a new directory of mode 0700, whatever the umask.
func privateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	must(t, os.Mkdir(dir, 0o700))
	must(t, os.Chmod(dir, 0o700))
	return dir
}
