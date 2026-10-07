// SPDX-License-Identifier: Apache-2.0

package client

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	// A second write touches nothing: not the descriptor number the first
	// one closed, which a directory opened since most likely reuses.
	other := privateDir(t)
	must(t, os.WriteFile(filepath.Join(other, "agent.token"), []byte("keep"), 0o600))
	d, err := os.Open(other)
	must(t, err)
	defer d.Close()
	if err := o.Write(fakeToken(t)); err == nil {
		t.Fatal("a second write was accepted")
	}
	if _, err := d.Stat(); err != nil {
		t.Fatalf("a second write closed another descriptor: %v", err)
	}
	if _, err := os.Stat(filepath.Join(other, "agent.token")); err != nil {
		t.Fatalf("a second write removed another file: %v", err)
	}
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
	f, own := home(t, fakeToken(t).Reveal())
	t.Setenv("HOME", f.Home)
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
		own:                                  ErrTokenOutOwn,
		filepath.Join(f.dir(), ".", "token"): ErrTokenOutOwn,
		target:                               ErrTokenOutExists,
		filepath.Join(dir, "link"):           ErrTokenOutExists,
		filepath.Join(dir, "dangling"):       ErrTokenOutExists,
		filepath.Join(open, "t"):             ErrTokenOutDir,
		filepath.Join(dir, "dirlink", "t"):   ErrTokenOutDir,
		filepath.Join(dir, "missing", "t"):   ErrTokenOutDir,
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

// The caller's own token file is refused by its directory's identity, even
// before it exists and when spelled through a symlinked parent.
func TestTokenOutRefusesOwnTokenFileNotYetWritten(t *testing.T) {
	f, _ := home(t, "")
	t.Setenv("HOME", f.Home)
	alias := filepath.Join(privateDir(t), "alias")
	must(t, os.Symlink(filepath.Join(f.Home, ".config"), alias))
	for _, path := range []string{filepath.Join(f.dir(), tokenName), filepath.Join(alias, "adc", tokenName)} {
		if _, err := CreateTokenOut(path); !errors.Is(err, ErrTokenOutOwn) {
			t.Errorf("%s: got %v", path, err)
		}
	}
	if entries, _ := os.ReadDir(f.dir()); len(entries) != 0 {
		t.Fatalf("wrote into the token directory: %v", names(entries))
	}
	o, err := CreateTokenOut(filepath.Join(f.dir(), "other"))
	must(t, err)
	o.Discard()
}

// A creation the system refuses (here a directory the caller cannot
// write) is neither a refusal of the caller's path nor a reserved file.
func TestTokenOutSystemError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes a 0500 directory")
	}
	dir := privateDir(t)
	must(t, os.Chmod(dir, 0o500))
	defer os.Chmod(dir, 0o700)
	_, err := CreateTokenOut(filepath.Join(dir, "t"))
	if err == nil || errors.Is(err, ErrTokenOutDir) || errors.Is(err, ErrTokenOutExists) || errors.Is(err, ErrTokenOutOwn) || !errors.Is(err, unix.EACCES) {
		t.Fatalf("got %v", err)
	}
}

// With no token to write, and after a failed write, its directory's sync
// included, the reserved file is removed, and the error never quotes the
// token; a second write is refused.
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
	syncFile = (*os.File).Sync
	defer func(old func(int) error) { syncDir = old }(syncDir)
	syncDir = func(int) error { return errors.New("io error") }
	o, err = CreateTokenOut(filepath.Join(dir, "t"))
	must(t, err)
	err = o.Write(fakeToken(t))
	if err == nil || !strings.Contains(err.Error(), "syncing the --token-out directory") {
		t.Fatalf("a failed directory sync: %v", err)
	}
	noSecret(t, err)
	if entries, _ := os.ReadDir(dir); !slices.Equal(names(entries), nil) {
		t.Fatalf("a token reported unwritten is on disk: %v", names(entries))
	}
	if err := o.Write(fakeToken(t)); err == nil {
		t.Fatal("a second write was accepted")
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
