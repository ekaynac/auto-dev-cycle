// SPDX-License-Identifier: Apache-2.0

package client

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// home makes ~/.config/adc (0700) in a temp home, with a token file of mode
// 0600 holding content unless content is empty.
func home(t *testing.T, content string) (Files, string) {
	t.Helper()
	f := Files{Home: t.TempDir(), UID: os.Getuid()}
	if err := os.MkdirAll(f.dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.dir(), tokenName)
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return f, path
}

func TestLoadToken(t *testing.T) {
	tok := fakeToken(t)
	f, _ := home(t, tok.Reveal()+"\n")
	got, err := f.Load()
	if err != nil || got.Reveal() != tok.Reveal() {
		t.Fatalf("Load: %v", err)
	}
}

func TestLoadTokenRefusals(t *testing.T) {
	tok := fakeToken(t)
	for name, c := range map[string]struct {
		setup func(t *testing.T, f Files, path string)
		want  error
	}{
		"absent": {func(t *testing.T, _ Files, path string) { os.Remove(path) }, ErrNoToken},
		"a symlink": {func(t *testing.T, _ Files, path string) {
			real := filepath.Join(t.TempDir(), "real")
			must(t, os.WriteFile(real, []byte(tok.Reveal()), 0o600))
			must(t, os.Remove(path))
			must(t, os.Symlink(real, path))
		}, ErrTokenNotRegular},
		"a FIFO": {func(t *testing.T, _ Files, path string) {
			must(t, os.Remove(path))
			must(t, unix.Mkfifo(path, 0o600))
		}, ErrTokenNotRegular},
		"a directory": {func(t *testing.T, _ Files, path string) {
			must(t, os.Remove(path))
			must(t, os.Mkdir(path, 0o700))
		}, ErrTokenNotRegular},
		"group-readable":           {func(t *testing.T, _ Files, path string) { must(t, os.Chmod(path, 0o640)) }, ErrTokenMode},
		"owner-read-only":          {func(t *testing.T, _ Files, path string) { must(t, os.Chmod(path, 0o400)) }, ErrTokenMode},
		"group-writable directory": {func(t *testing.T, f Files, _ string) { must(t, os.Chmod(f.dir(), 0o770)) }, ErrTokenDir},
		"other-writable directory": {func(t *testing.T, f Files, _ string) { must(t, os.Chmod(f.dir(), 0o703)) }, ErrTokenDir},
		"a symlinked directory": {func(t *testing.T, f Files, _ string) {
			must(t, os.Rename(f.dir(), f.dir()+".real"))
			must(t, os.Symlink(f.dir()+".real", f.dir()))
		}, ErrTokenDir},
		"malformed": {func(t *testing.T, _ Files, path string) {
			must(t, os.WriteFile(path, []byte("Bearer "+tok.Reveal()), 0o600))
		}, ErrTokenMalformed},
		"oversized": {func(t *testing.T, _ Files, path string) {
			must(t, os.WriteFile(path, []byte(strings.Repeat(tok.Reveal(), 100)), 0o600))
		}, ErrTokenMalformed},
	} {
		t.Run(name, func(t *testing.T) {
			f, path := home(t, tok.Reveal())
			c.setup(t, f, path)
			_, err := f.Load()
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			noSecret(t, err)
		})
	}
}

// The file must be the caller's: an owner other than the uid is refused,
// and so is a directory of another owner.
func TestLoadTokenRefusesAnotherOwner(t *testing.T) {
	tok := fakeToken(t)
	f, _ := home(t, tok.Reveal())
	defer func(old func(*unix.Stat_t) int) { fileOwner = old }(fileOwner)
	fileOwner = func(st *unix.Stat_t) int { return int(st.Uid) + 1 }
	if _, err := f.Load(); !errors.Is(err, ErrTokenOwner) {
		t.Fatalf("file: got %v", err)
	}
	f.UID++
	if _, err := f.Load(); !errors.Is(err, ErrTokenDir) {
		t.Fatalf("directory: got %v", err)
	}
}

func TestTokenEnv(t *testing.T) {
	tok := fakeToken(t)
	t.Setenv("HOME", t.TempDir()) // never the user's real ~/.config/adc
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if _, err := Token(getenv); !errors.Is(err, ErrNoToken) {
		t.Fatalf("nothing: %v", err)
	}
	env[TokenEnv] = tok.Reveal()
	if got, err := Token(getenv); err != nil || got.Reveal() != tok.Reveal() {
		t.Fatalf("env: %v", err)
	}
	env[TokenEnv] = tok.Reveal() + " "
	_, err := Token(getenv)
	if !errors.Is(err, ErrTokenMalformed) {
		t.Fatalf("malformed env: %v", err)
	}
	noSecret(t, err)
}

func TestWriteToken(t *testing.T) {
	old, tok := fakeToken(t), fakeToken(t)
	f := Files{Home: t.TempDir(), UID: os.Getuid()}
	must(t, f.Write(old))
	path := filepath.Join(f.dir(), tokenName)
	held, err := os.Open(path)
	must(t, err)
	defer held.Close()
	must(t, f.Write(tok))

	if got, err := f.Load(); err != nil || got.Reveal() != tok.Reveal() {
		t.Fatalf("Load: %v", err)
	}
	// Replaced by a rename, never rewritten in place: the old file is intact.
	b := make([]byte, 200)
	n, _ := held.Read(b)
	if string(b[:n]) != old.Reveal()+"\n" {
		t.Fatal("the old token file was rewritten in place")
	}
	for p, want := range map[string]os.FileMode{path: 0o600, f.dir(): 0o700 | os.ModeDir} {
		if st, err := os.Lstat(p); err != nil || st.Mode() != want {
			t.Fatalf("%s: %v %v, want %v", p, st.Mode(), err, want)
		}
	}
	entries, err := os.ReadDir(f.dir())
	must(t, err)
	if len(entries) != 1 {
		t.Fatalf("leftovers: %v", entries)
	}
}

// A symlink at the token path is replaced, not written through.
func TestWriteTokenReplacesSymlinkAtTarget(t *testing.T) {
	tok := fakeToken(t)
	f, path := home(t, "")
	victim := filepath.Join(t.TempDir(), "victim")
	must(t, os.WriteFile(victim, []byte("victim"), 0o644))
	must(t, os.Symlink(victim, path))
	must(t, f.Write(tok))
	if b, err := os.ReadFile(victim); err != nil || string(b) != "victim" {
		t.Fatalf("the symlink was followed: %q %v", b, err)
	}
	if st, err := os.Lstat(path); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("token: %v %v", st, err)
	}
}

// A path planted at the temporary name is never followed or reused.
func TestWriteTokenRefusesPlantedTemp(t *testing.T) {
	tok := fakeToken(t)
	defer func(old func() string) { tempName = old }(tempName)
	tempName = func() string { return "token.tmp-planted" }
	for name, existing := range map[string]bool{"dangling": false, "existing": true} {
		t.Run(name, func(t *testing.T) {
			f, path := home(t, "")
			victim := filepath.Join(t.TempDir(), "victim")
			if existing {
				must(t, os.WriteFile(victim, []byte("victim"), 0o644))
			}
			must(t, os.Symlink(victim, filepath.Join(f.dir(), tempName())))
			err := f.Write(tok)
			if err == nil {
				t.Fatal("wrote through a planted temp path")
			}
			noSecret(t, err)
			b, rerr := os.ReadFile(victim)
			if existing && string(b) != "victim" || !existing && !errors.Is(rerr, os.ErrNotExist) {
				t.Fatalf("victim: %q %v", b, rerr)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("token written: %v", err)
			}
		})
	}
}

// Write checks an existing directory as Load does.
func TestWriteTokenRefusesUnsafeDir(t *testing.T) {
	tok := fakeToken(t)
	f, _ := home(t, "")
	must(t, os.Chmod(f.dir(), 0o777))
	err := f.Write(tok)
	if !errors.Is(err, ErrTokenDir) {
		t.Fatalf("got %v", err)
	}
	entries, _ := os.ReadDir(f.dir())
	if !slices.Equal(names(entries), nil) {
		t.Fatalf("wrote into an unsafe directory: %v", entries)
	}
}

func names(es []os.DirEntry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}
