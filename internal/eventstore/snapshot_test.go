// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// verifyStore makes a closed store with a few events and verifies it with
// hooks installed, returning the error and the state directory.
func verifyStore(t *testing.T, copied func(file, src string), cleanup func(dir string)) (string, error) {
	t.Helper()
	dir := stateDir(t)
	e := openEnv(t, dir)
	e.appendN(t, 3)
	e.close()
	file := filepath.Join(dir, DBFile)
	if copied != nil {
		testHooks.copied = func(src string) { copied(file, src) }
	}
	testHooks.cleanup = cleanup
	t.Cleanup(func() { testHooks.copied, testHooks.cleanup = nil, nil })
	lock, err := ProbeLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	_, err = VerifyExisting(context.Background(), lock, nil)
	return dir, err
}

// A source written while it is copied (here: touched, or its log appearing)
// is not verified as a mixture of two moments: the check could not run.
func TestVerifyExistingRefusesASourceChangedDuringTheCopy(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, file string){
		"the database touched": func(t *testing.T, file string) {
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(file, later, later); err != nil {
				t.Fatal(err)
			}
		},
		"a log appearing": func(t *testing.T, file string) {
			if err := os.WriteFile(file+"-wal", nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := verifyStore(t, func(file, src string) {
				if src == file {
					change(t, file)
				}
			}, nil)
			if err == nil || errors.Is(err, ErrChainBroken) || !strings.Contains(err.Error(), "changed while it was copied") {
				t.Fatalf("%v", err)
			}
		})
	}
}

// The copy is never written inside the state directory, also when TMPDIR
// reaches it through a symbolic link, or through a link followed by ".."
// (resolved in filesystem order, not cleaned away first); a TMPDIR that does
// not exist is refused before anything is created.
func TestVerifyExistingRefusesATempDirInsideTheState(t *testing.T) {
	dir := stateDir(t)
	e := openEnv(t, dir)
	e.appendN(t, 1)
	e.close()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	links := t.TempDir()
	for name, target := range map[string]string{"alias": dir, "subalias": filepath.Join(dir, "sub")} {
		if err := os.Symlink(target, filepath.Join(links, name)); err != nil {
			t.Fatal(err)
		}
	}
	for tmp, want := range map[string]string{
		dir:                             "inside the state directory",
		filepath.Join(dir, "sub"):       "inside the state directory",
		filepath.Join(links, "alias"):   "inside the state directory",
		links + "/subalias/..":          "inside the state directory",
		filepath.Join(dir, "not-yet"):   "the temporary directory",
		filepath.Join(links, "nowhere"): "the temporary directory",
	} {
		before, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("TMPDIR", tmp)
		lock, err := ProbeLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		_, err = VerifyExisting(context.Background(), lock, nil)
		lock.Release()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("TMPDIR=%s: %v, want %q", tmp, err, want)
		}
		if after, _ := os.ReadDir(dir); len(after) != len(before) {
			t.Errorf("TMPDIR=%s: the state directory has %d entries, had %d", tmp, len(after), len(before))
		}
	}
}

// The copy is made under TMPDIR resolved once: retargeting a TMPDIR link
// after it was checked, or before the copy is removed, neither steers the
// copy into the state directory nor leaves it behind unreported.
func TestVerifyExistingIgnoresARetargetedTempDir(t *testing.T) {
	ext := t.TempDir()
	links := t.TempDir()
	alias := filepath.Join(links, "tmp")
	if err := os.Symlink(ext, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", alias)
	var dir string
	retarget := func() {
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dir, alias); err != nil {
			t.Fatal(err)
		}
	}
	for name, at := range map[string]*func(){"after the check": &testHooks.resolved, "before the cleanup": nil} {
		dir = stateDir(t)
		e := openEnv(t, dir)
		e.appendN(t, 2)
		e.close()
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(ext, alias); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadDir(dir)
		if at != nil {
			*at = retarget
		} else {
			testHooks.cleanup = func(string) { retarget() }
		}
		lock, err := ProbeLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		_, err = VerifyExisting(context.Background(), lock, nil)
		lock.Release()
		testHooks.resolved, testHooks.cleanup = nil, nil
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if after, _ := os.ReadDir(dir); len(after) != len(before) {
			t.Errorf("%s: the state directory has %d entries, had %d", name, len(after), len(before))
		}
		if left, _ := os.ReadDir(ext); len(left) != 0 {
			t.Errorf("%s: %d entries left in the temporary directory", name, len(left))
		}
	}
}

// A copy directory moved away before its removal is reported, not silently
// lost track of.
func TestVerifyExistingReportsAMovedCopy(t *testing.T) {
	moved := filepath.Join(t.TempDir(), "moved")
	_, err := verifyStore(t, nil, func(dir string) {
		if err := os.Rename(dir, moved); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "is no longer there") {
		t.Fatalf("%v", err)
	}
}

// A copy that cannot be removed is reported with its location, after a
// verification that otherwise succeeded.
func TestVerifyExistingReportsACopyLeftBehind(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes anything")
	}
	var kept string
	_, err := verifyStore(t, nil, func(dir string) {
		kept = dir
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
	})
	t.Cleanup(func() {
		os.Chmod(kept, 0o700)
		os.RemoveAll(kept)
	})
	if err == nil || errors.Is(err, ErrChainBroken) || !strings.Contains(err.Error(), "could not be removed") || !strings.Contains(err.Error(), kept) {
		t.Fatalf("%v", err)
	}
}
