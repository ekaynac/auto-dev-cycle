// SPDX-License-Identifier: Apache-2.0

//go:build linux

package adapterhost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// ErrExchange: the exchange root could not be made or removed. Its text is
// fixed: out/ holds names the adapter chose.
var ErrExchange = errors.New("adapterhost: the attempt's exchange root could not be set up or removed")

// setupFault, when a test sets it, fails the named setup step.
var setupFault func(step string) error

func fault(step string) error {
	if setupFault != nil {
		return setupFault(step)
	}
	return nil
}

// exchange is an attempt's exchange root: in/ (read-only by mode once the
// prompt is in it) and out/, held open for ingestion.
type exchange struct {
	root, in, out string
	outDir        *os.File
	prompt        types.AdapterHandle
}

// ExchangePrefix starts every exchange root's name.
const ExchangePrefix = "exchange-"

// ExchangeName is attempt's exchange root under Host.Root: restart recovery
// finds a cut attempt's root from its record, never a name it reads (P21b).
func ExchangeName(attempt types.AttemptID) string { return ExchangePrefix + string(attempt) }

// newExchange makes the root under parent, exclusively and mode 0700; on any
// failure it removes what it made.
func newExchange(parent string, attempt types.AttemptID, prompt []byte) (*exchange, error) {
	x := &exchange{root: filepath.Join(parent, ExchangeName(attempt))}
	if ids.Check(string(attempt), "att") != nil || os.Mkdir(x.root, 0o700) != nil { // EEXIST: never a root found
		return nil, ErrExchange
	}
	x.in, x.out = filepath.Join(x.root, "in"), filepath.Join(x.root, "out")
	if err := x.setup(attempt, prompt); err != nil {
		return nil, errors.Join(ErrExchange, x.remove())
	}
	return x, nil
}

func (x *exchange) setup(attempt types.AttemptID, prompt []byte) error {
	steps := []struct {
		name string
		do   func() error
	}{
		{"in", func() error { return os.Mkdir(x.in, 0o755) }},
		{"out", func() error { return os.Mkdir(x.out, 0o755) }},
		{"prompt", func() error { return os.WriteFile(filepath.Join(x.in, PromptName), prompt, 0o444) }},
		{"seal", func() error { return os.Chmod(x.in, 0o555) }},
		{"open", func() error {
			fd, err := unix.Open(x.out, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err == nil {
				x.outDir = os.NewFile(uintptr(fd), x.out)
			}
			return err
		}},
	}
	for _, st := range steps {
		if err := errors.Join(st.do(), fault(st.name)); err != nil {
			return err
		}
	}
	sum := sha256.Sum256(prompt)
	x.prompt = types.AdapterHandle{Attempt: attempt, Handle: PromptName, Digest: types.Digest("sha256:" + hex.EncodeToString(sum[:])), Size: types.Amount(len(prompt))}
	return nil
}

// remove closes out/ and removes the root (RemoveAt). A failure is reported
// with a fixed text (wrapping ErrExchange): the paths in it are the adapter's.
func (x *exchange) remove() error {
	var err error
	if x.outDir != nil {
		err = x.outDir.Close()
		x.outDir = nil
	}
	dir, perr := unix.Open(filepath.Dir(x.root), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if perr == nil {
		perr = RemoveAt(dir, filepath.Base(x.root))
		unix.Close(dir)
	}
	if err = errors.Join(err, perr); err != nil {
		return ErrExchange
	}
	return nil
}

// RemoveAt removes name beneath directory dir, following no symlink: a
// directory is pinned (O_PATH|O_NOFOLLOW), made 0700 through it (an adapter
// may have changed any mode), emptied beneath it, removed. Gone is no error.
func RemoveAt(dir int, name string) error {
	if err := unix.Unlinkat(dir, name, 0); err == nil || err == unix.ENOENT {
		return nil
	} else if err != unix.EISDIR {
		return err
	}
	fd, err := unix.Openat(dir, name, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	// An O_PATH descriptor has no fchmod; its /proc link reaches the pinned inode, never a name.
	if err := unix.Chmod("/proc/self/fd/"+strconv.Itoa(fd), 0o700); err != nil {
		return err
	}
	lfd, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	list := os.NewFile(uintptr(lfd), name)
	names, err := list.Readdirnames(-1)
	err = errors.Join(err, list.Close())
	for _, n := range names {
		err = errors.Join(err, RemoveAt(fd, n))
	}
	return errors.Join(err, unix.Unlinkat(dir, name, unix.AT_REMOVEDIR))
}
