// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A probe never creates the state directory or the lock file, never writes
// the lock record, and excludes and is excluded by a holder like a lock.
func TestProbeLockChangesNothing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "state")
	if _, err := ProbeLock(missing); err == nil {
		t.Fatal("a probe of a missing state directory succeeded")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the probe created the state directory: %v", err)
	}
	bare := filepath.Join(t.TempDir(), "bare")
	if err := os.Mkdir(bare, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ProbeLock(bare); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a probe with no lock file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bare, LockFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the probe created the lock file: %v", err)
	}

	dir := stateDir(t)
	holder, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ProbeLock(dir); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("a probe while held: %v", err)
	}
	holder.Release()
	// A record no process of ours wrote: the probe must leave it exactly.
	record := []byte("pid 1\nprocess_start_ticks 7\n")
	if err := os.WriteFile(filepath.Join(dir, LockFile), record, 0o600); err != nil {
		t.Fatal(err)
	}
	probe, err := ProbeLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(dir); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("a lock while probed: %v", err)
	}
	if err := probe.Release(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, LockFile)); string(b) != string(record) {
		t.Fatalf("the probe rewrote the lock record: %q", b)
	}
}
