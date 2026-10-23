// SPDX-License-Identifier: Apache-2.0

// Package fsread is the one bounded reader of a regular file a run or a
// record left (Astra #306 M4; Astra chunk 5b M4): qualify's records and
// reports, and the guard's listing outputs.
package fsread

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// ErrOver marks a file past its limit, by its size or by what it grew to
// while it was read: a caller that classifies a file's size apart (the
// guard's list bound) tells it from any other failure.
var ErrOver = errors.New("fsread: over its bound")

// over is a size refusal, its text why, marked ErrOver.
type over string

func (o over) Error() string        { return string(o) }
func (o over) Is(target error) bool { return target == ErrOver }

// Regular is the regular file at p, opened never through a link and never
// a special file, of at most limit bytes: its size is checked before it is
// read, and the read stops past the limit.
func Regular(p string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	switch {
	case err != nil:
		return nil, err
	case !fi.Mode().IsRegular():
		return nil, errors.New("not a regular file")
	case fi.Size() > limit:
		return nil, over(fmt.Sprintf("is %d bytes, over %d", fi.Size(), limit))
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1)) // a file that grows, or whose size lies (/proc), is still read within the bound
	if err == nil && int64(len(b)) > limit {
		err = over(fmt.Sprintf("grew past %d bytes", limit))
	}
	return b, err
}
