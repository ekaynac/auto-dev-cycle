// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// LockFile is the process-exclusivity lock in the state directory (03
// "Process exclusivity"). It is acquired before the database is opened.
const LockFile = "adcd.lock"

// ErrAlreadyRunning: another process holds the state directory
// (could_not_run/controller_already_running).
var ErrAlreadyRunning = errors.New("controller_already_running")

// Lock is a held exclusive lock on a state directory.
type Lock struct {
	f   *os.File
	dir string
}

// AcquireLock takes the exclusive advisory lock on <stateDir>/adcd.lock and
// records this process's pid and process start time in it (so pid reuse is
// detectable). The state directory must be private (0700); the lock file is
// opened without following symlinks and without truncation, so a failed
// attempt never damages the holder's record.
func AcquireLock(stateDir string) (*Lock, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("eventstore: state directory %s is accessible to group or others (%v); it must be 0700", dir, fi.Mode().Perm())
	}
	f, err := os.OpenFile(filepath.Join(dir, LockFile), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s is locked by another process", ErrAlreadyRunning, dir)
		}
		return nil, err
	}
	record := "pid " + strconv.Itoa(os.Getpid()) + "\nprocess_start_ticks " + processStart() + "\n"
	for _, step := range []func() error{
		func() error { return f.Truncate(0) },
		func() error { _, err := f.WriteAt([]byte(record), 0); return err },
		f.Sync,
	} {
		if err := step(); err != nil {
			f.Close()
			return nil, err
		}
	}
	return &Lock{f: f, dir: dir}, nil
}

// processStart is field 22 of /proc/self/stat (start time in clock ticks
// since boot), which together with the pid identifies the process.
func processStart() string {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return "unknown"
	}
	s := string(b)
	// The command name (field 2) may contain spaces; fields resume after ')'.
	rest := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(rest) < 20 {
		return "unknown"
	}
	return rest[19] // field 22 = index 19 after the first two fields
}

// Held reports whether the lock is still held by this process.
func (l *Lock) Held() bool { return l != nil && l.f != nil }

// Dir is the absolute state directory the lock guards.
func (l *Lock) Dir() string { return l.dir }

// Release drops the lock. The file stays; its content is the last holder's.
func (l *Lock) Release() error {
	if !l.Held() {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}
