// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// LockFile is the process-exclusivity lock in the state directory (03
// "Process exclusivity"). It is acquired before the database is opened.
const LockFile = "adcd.lock"

// ErrAlreadyRunning: another process holds the state directory
// (could_not_run/controller_already_running).
var ErrAlreadyRunning = errors.New("controller_already_running")

// Lock is a held exclusive lock on a state directory. At most one Store is
// open on it, and it cannot be released while that store is open.
type Lock struct {
	mu    sync.Mutex
	f     *os.File
	dir   string
	owner uint64 // the claim of the Store open on this lock; 0 when none
	next  uint64 // the last claim handed out
}

// AcquireLock takes the exclusive advisory lock on <stateDir>/adcd.lock and
// records this process's pid and process start time in it (so pid reuse is
// detectable). The state directory must be private (0700); the lock file is
// opened without following symlinks and without truncation, so a failed
// attempt never damages the holder's record.
func AcquireLock(stateDir string) (*Lock, error) { return acquire(stateDir, false) }

// ProbeLock takes the same exclusive lock for a probe, such as log
// verification, that must change nothing (03 "Process exclusivity"): the
// state directory and its lock file must already exist, and the lock file
// is opened read-only, never created, truncated or rewritten, so it keeps
// the last holder's record.
func ProbeLock(stateDir string) (*Lock, error) { return acquire(stateDir, true) }

func acquire(stateDir string, probe bool) (*Lock, error) {
	if !probe {
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return nil, err
		}
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
	path := filepath.Join(dir, LockFile)
	flags := os.O_RDWR | os.O_CREATE | syscall.O_NOFOLLOW
	if probe {
		flags = os.O_RDONLY | syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	// Before anything is written: the lock file must be a private regular
	// file owned by this user with one link. A hardlink to another file, or
	// a file someone else owns, is refused untouched.
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		f.Close()
		return nil, err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Nlink != 1 || int(st.Uid) != os.Geteuid() || st.Mode&0o077 != 0 {
		f.Close()
		return nil, fmt.Errorf("eventstore: %s must be a private regular file owned by this user with one link; refused", path)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s is locked by another process", ErrAlreadyRunning, dir)
		}
		return nil, err
	}
	if probe {
		return &Lock{f: f, dir: dir}, nil
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
func (l *Lock) Held() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f != nil
}

// claim marks a store open on the lock and returns its claim; a second
// store is refused.
func (l *Lock) claim() (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.f == nil:
		return 0, errors.New("eventstore: the state directory lock must be held before the database is opened")
	case l.owner != 0:
		return 0, errors.New("eventstore: a store is already open on this lock")
	}
	l.next++
	l.owner = l.next
	return l.owner, nil
}

// unclaim ends the claim it is given; a stale claim (an earlier store's)
// never frees the lock under its successor.
func (l *Lock) unclaim(claim uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.owner == claim {
		l.owner = 0
	}
}

// Dir is the absolute state directory the lock guards.
func (l *Lock) Dir() string { return l.dir }

// Release drops the lock. The file stays; its content is the last holder's.
// It is refused while a store is open on the lock: close the store first.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	if l.owner != 0 {
		return errors.New("eventstore: close the store before releasing its lock")
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}
