// SPDX-License-Identifier: Apache-2.0

package client

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

// home makes ~/.config/adc (0700) in a temp home, with a token file of mode
// 0600 holding content unless content is empty.
func home(t *testing.T, content string) (Files, string) {
	t.Helper()
	return homeAt(t, t.TempDir(), content)
}

// homeAt is home in the directory dir.
func homeAt(t *testing.T, dir, content string) (Files, string) {
	t.Helper()
	f := Files{Home: dir, UID: os.Getuid()}
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
			if !strings.Contains(err.Error(), "~/.config/adc") {
				t.Fatalf("the refusal names no file: %v", err)
			}
			noSecret(t, err)
		})
	}
}

// A huge token file is refused without being read whole.
func TestLoadTokenReadsBounded(t *testing.T) {
	f, path := home(t, fakeToken(t).Reveal())
	must(t, os.Truncate(path, 64<<20)) // sparse
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := f.Load()
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrTokenMalformed) {
		t.Fatalf("got %v", err)
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 1<<20 {
		t.Fatalf("read %d bytes", n)
	}
}

// Write refuses a token not in the issued form and writes nothing.
func TestWriteTokenRefusesMalformed(t *testing.T) {
	f := Files{Home: t.TempDir(), UID: os.Getuid()}
	if err := f.Write(identity.Secret{}); !errors.Is(err, ErrTokenMalformed) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.Home, ".config")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrote: %v", err)
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
	testguard.Home(t) // never the user's real ~/.config/adc
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

// A path planted at the temporary name is never followed, reused or
// overwritten: the new file is created exclusively.
func TestWriteTokenRefusesPlantedTemp(t *testing.T) {
	tok := fakeToken(t)
	defer func(old func() string) { tempName = old }(tempName)
	tempName = func() string { return "token.tmp-planted" }
	for _, plant := range []string{"a dangling symlink", "a symlink", "a regular file"} {
		t.Run(plant, func(t *testing.T) {
			f, path := home(t, "")
			planted := filepath.Join(f.dir(), tempName())
			victim := filepath.Join(t.TempDir(), "victim")
			switch plant {
			case "a regular file":
				victim = planted
				must(t, os.WriteFile(victim, []byte("victim"), 0o600))
			case "a symlink":
				must(t, os.WriteFile(victim, []byte("victim"), 0o644))
				fallthrough
			default:
				must(t, os.Symlink(victim, planted))
			}
			err := f.Write(tok)
			if err == nil {
				t.Fatal("wrote through a planted temp path")
			}
			noSecret(t, err)
			b, rerr := os.ReadFile(victim)
			if plant == "a dangling symlink" && !errors.Is(rerr, os.ErrNotExist) || plant != "a dangling symlink" && string(b) != "victim" {
				t.Fatalf("victim: %q %v", b, rerr)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("token written: %v", err)
			}
		})
	}
}

// Under a umask that takes the owner's write bit, the token is still 0600:
// Write sets the mode explicitly. A subprocess, since the umask is the
// process's.
func TestWriteTokenUnderRestrictiveUmask(t *testing.T) {
	if home := os.Getenv("ADC_TEST_UMASK_HOME"); home != "" {
		unix.Umask(0o277)
		if err := (Files{Home: home, UID: os.Getuid()}).Write(fakeToken(t)); err != nil {
			t.Fatal(err)
		}
		return
	}
	f, path := home(t, "")
	cmd := exec.Command(os.Args[0], "-test.run=^TestWriteTokenUnderRestrictiveUmask$", "-test.count=1")
	cmd.Env = append(os.Environ(), "ADC_TEST_UMASK_HOME="+f.Home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if st, err := os.Lstat(path); err != nil || st.Mode() != 0o600 {
		t.Fatalf("token: %v %v", st.Mode(), err)
	}
	if _, err := f.Load(); err != nil {
		t.Fatal(err)
	}
}

// The new file is synced before the rename and the directory after it; a
// failed step fails the write, and a failure before the rename leaves the
// old token in place and no temporary file.
func TestWriteTokenSyncs(t *testing.T) {
	old, tok := fakeToken(t), fakeToken(t)
	defer func(f func(*os.File) error, r func(int, string, int, string) error, d func(int) error) {
		syncFile, renameAt, syncDir = f, r, d
	}(syncFile, renameAt, syncDir)
	var steps []string
	fail := ""
	step := func(name string) error {
		steps = append(steps, name)
		if name == fail {
			return errors.New(name + " failed")
		}
		return nil
	}
	syncFile = func(f *os.File) error { must(t, f.Sync()); return step("sync file") }
	renameAt = func(a int, b string, c int, d string) error {
		if err := step("rename"); err != nil {
			return err
		}
		return unix.Renameat(a, b, c, d)
	}
	syncDir = func(fd int) error { must(t, unix.Fsync(fd)); return step("sync dir") }

	f, _ := home(t, "")
	must(t, f.Write(old))
	if want := []string{"sync file", "rename", "sync dir"}; !slices.Equal(steps, want) {
		t.Fatalf("steps %v, want %v", steps, want)
	}
	for _, fail = range []string{"sync file", "rename", "sync dir"} {
		steps = nil
		err := f.Write(tok)
		if err == nil || !strings.Contains(err.Error(), fail) {
			t.Fatalf("%s: got %v", fail, err)
		}
		noSecret(t, err)
		got, lerr := f.Load()
		if lerr != nil {
			t.Fatal(lerr)
		}
		if fail != "sync dir" && got.Reveal() != old.Reveal() {
			t.Fatalf("%s: the token was replaced", fail)
		}
		entries, _ := os.ReadDir(f.dir())
		if !slices.Equal(names(entries), []string{tokenName}) {
			t.Fatalf("%s: leftovers %v", fail, names(entries))
		}
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
