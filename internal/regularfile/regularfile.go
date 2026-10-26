// SPDX-License-Identifier: Apache-2.0

// Package regularfile is the one rule for reading a file a walk listed as
// regular, when another process may change the tree meanwhile (the sandbox
// copy of a workspace; secretscan's search): open it without following a
// link and without blocking on a FIFO, then check that what was opened is
// still a regular file, and, when the walk's Lstat is given, the same one.
package regularfile

import (
	"errors"
	"os"
	"syscall"
)

// Flags opens read-only, never through a link swapped in (O_NOFOLLOW), and
// never blocking on a FIFO swapped in (O_NONBLOCK; a regular file's reads
// are unaffected).
const Flags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// ErrChanged: what was opened is no longer the regular file listed.
var ErrChanged = errors.New("not the regular file listed")

// Check returns the opened file's FileInfo when it is a regular file and,
// with want non-nil (the walk's Lstat), the same file (device and inode).
func Check(f *os.File, want os.FileInfo) (os.FileInfo, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || (want != nil && !os.SameFile(st, want)) {
		return nil, ErrChanged
	}
	return st, nil
}
