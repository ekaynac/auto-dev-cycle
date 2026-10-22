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

// lockSandbox takes an exclusive lock on root's sandboxLockName, held until
// the returned unlock (the daemon's life): two adcd sharing a sandbox root
// would each sweep the other's live runs (sweep.go), so a second is
// refused, as a second on one state directory is (Opus 22a-5c-2c M2).
func lockSandbox(root string) (unlock func(), err error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, couldNotRun(vocab.ReasonStoreUnreadable, "the sandbox root cannot be made", err)
	}
	f, err := os.OpenFile(filepath.Join(root, sandboxLockName), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
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
