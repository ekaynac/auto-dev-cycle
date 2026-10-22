// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// sandboxLockName is the lock file adcd holds in its sandbox root.
const sandboxLockName = ".adcd.lock"

// lockSandbox takes, until the returned unlock (the daemon's life), an
// exclusive lock on root's sandboxLockName and one on the lock file in the
// directory root's runs/ resolves to: two adcd sharing a sandbox root, or
// two roots whose runs/ are one directory (a symlink or a bind mount), would
// each sweep the other's live runs (sweep.go). A second is refused, as a
// second on one state directory is (Opus 22a-5c-2c M2, Astra chunk 1 H1):
// flock locks the file's inode, so every path to one runs/ directory takes
// the same lock. runs/ is made here when missing, before anything else is
// made in it or swept from it.
func lockSandbox(root string) (unlock func(), err error) {
	rootLock, err := lockIn(root)
	if err != nil {
		return nil, err
	}
	runs := filepath.Join(root, "runs")
	if err := os.MkdirAll(runs, 0o700); err != nil {
		rootLock()
		return nil, couldNotRun(vocab.ReasonStoreUnreadable, "the sandbox root's runs directory cannot be made", err)
	}
	real, err := filepath.EvalSymlinks(runs)
	if err != nil {
		rootLock()
		return nil, couldNotRun(vocab.ReasonStoreUnreadable, "the sandbox root's runs directory cannot be resolved", err)
	}
	runsLock, err := lockIn(real)
	if err != nil {
		rootLock()
		return nil, err
	}
	return func() { runsLock(); rootLock() }, nil
}

// lockIn takes an exclusive lock on dir's sandboxLockName.
func lockIn(dir string) (unlock func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, couldNotRun(vocab.ReasonStoreUnreadable, "the sandbox root cannot be made", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, sandboxLockName), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, couldNotRun(vocab.ReasonStoreUnreadable, "the sandbox root's lock cannot be opened", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, couldNotRun(vocab.ReasonControllerAlreadyRunning, "another adcd holds the sandbox root's lock", err)
		}
		return nil, couldNotRun(vocab.ReasonStoreUnreadable, "the sandbox root's lock cannot be taken", err)
	}
	return func() { _ = f.Close() }, nil
}
