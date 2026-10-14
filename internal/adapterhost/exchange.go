// SPDX-License-Identifier: Apache-2.0

//go:build linux

package adapterhost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
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

// ExchangePrefix starts an exchange root's name, followed by its attempt's
// id and a dash: a root a crash left behind names its attempt, whose output
// restart recovery refuses (internal/recovery, P21).
const ExchangePrefix = "exchange-"

// newExchange makes the root under parent; on any failure it removes what
// it made.
func newExchange(parent string, attempt types.AttemptID, prompt []byte) (*exchange, error) {
	root, err := os.MkdirTemp(parent, ExchangePrefix+string(attempt)+"-")
	if err != nil {
		return nil, ErrExchange
	}
	x := &exchange{root: root, in: filepath.Join(root, "in"), out: filepath.Join(root, "out")}
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

// remove closes out/ and removes the root. A failure is reported with a
// fixed text (wrapping ErrExchange): the paths in it are the adapter's.
func (x *exchange) remove() error {
	var err error
	if x.outDir != nil {
		err = x.outDir.Close()
		x.outDir = nil
	}
	if cerr := os.Chmod(x.in, 0o755); cerr != nil && !errors.Is(cerr, fs.ErrNotExist) {
		err = errors.Join(err, cerr)
	}
	if err = errors.Join(err, os.RemoveAll(x.root)); err != nil {
		return ErrExchange
	}
	return nil
}
