// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

// AdapterClosure is what the builder's http-chat adapter needs of an
// installed lib/adc (make dist's dist/linux-ARCH/lib/adc; P22a-5d), relative
// to it: the pinned Node, the adapter, the contract package, gen/ts and the
// production node_modules, whose @adc/contract and @adc/contract-gen are
// relative symlinks into the same root. It runs as ROOT/node/bin/node
// ROOT/adapters/http-chat/src/main.ts. The prepared images carry exactly
// this, under /opt/adc/lib (scripts/prepare-go-image.sh).
var AdapterClosure = []string{"node/bin/node", "adapters/http-chat", "adapters/contract", "gen/ts", "node_modules"}

// The closure's bounds: a few hundred entries and Node's ~120 MiB today.
const (
	maxAdapterEntries = 1 << 14
	maxAdapterBytes   = 1 << 30
)

// AdapterDigest is sha256:HEX over the canonical listing of
// AdapterClosure's paths under root, one line each, sorted by path: a
// directory "d PATH", a regular file "f PATH X SHA256" (X is x when any
// execute bit is set, else -; no other mode bit counts), a symlink
// "l PATH TARGET", never followed. A link's target must be relative,
// clean, with its ".." only leading, and lexically within the closure:
// so the closure holds everything the adapter reaches through it. Any
// other file type, a missing closure path, a closure path's parent that is
// not a real directory, a name or target with a space or a control
// character, or more than the bounded entries or bytes is an error. Root
// is opened with os.OpenRoot: nothing outside it is read. Two copies of a
// closure have the same digest; a changed byte, execute bit, file or link
// target changes it.
func AdapterDigest(root string) (string, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return "", fmt.Errorf("sandbox: the adapter closure: %w", err)
	}
	defer r.Close()
	w := walker{root: r}
	for _, top := range AdapterClosure {
		for p := path.Dir(top); p != "."; p = path.Dir(p) {
			if st, err := r.Lstat(p); err != nil || !st.IsDir() {
				return "", fmt.Errorf("sandbox: the adapter closure: %s is not a directory", p)
			}
		}
		if err := w.walk(top); err != nil {
			return "", fmt.Errorf("sandbox: the adapter closure: %w", err)
		}
	}
	slices.SortFunc(w.lines, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	h := sha256.New()
	for _, l := range w.lines {
		io.WriteString(h, l[1]+"\n")
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// walker lists a closure: each line as {path, line}.
type walker struct {
	root    *os.Root
	lines   [][2]string
	entries int
	bytes   int64
}

func (w *walker) walk(p string) error {
	if w.entries++; w.entries > maxAdapterEntries {
		return fmt.Errorf("more than %d entries", maxAdapterEntries)
	}
	if !plainName(p) {
		return fmt.Errorf("the name %q holds a space or a control character", p)
	}
	st, err := w.root.Lstat(p)
	if err != nil {
		return err
	}
	switch st.Mode().Type() {
	case os.ModeDir:
		w.lines = append(w.lines, [2]string{p, "d " + p})
		return w.dir(p, st)
	case os.ModeSymlink:
		target, err := w.root.Readlink(p)
		if err != nil {
			return err
		}
		if !plainName(target) {
			return fmt.Errorf("the link %s's target holds a space or a control character", p)
		}
		if !inClosure(p, target) {
			return fmt.Errorf("the link %s points outside the closure", p)
		}
		w.lines = append(w.lines, [2]string{p, "l " + p + " " + target})
		return nil
	case 0:
		sum, err := w.file(p, st)
		if err != nil {
			return err
		}
		x := "-"
		if st.Mode().Perm()&0o111 != 0 {
			x = "x"
		}
		w.lines = append(w.lines, [2]string{p, "f " + p + " " + x + " " + sum})
		return nil
	}
	return fmt.Errorf("%s is neither a directory, a regular file nor a symlink", p)
}

// dir walks p's entries, read in bounded batches.
func (w *walker) dir(p string, st os.FileInfo) error {
	f, err := w.root.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	if fst, err := f.Stat(); err != nil || !os.SameFile(st, fst) {
		return fmt.Errorf("%s changed while it was read", p)
	}
	for {
		names, err := f.Readdirnames(256)
		for _, n := range names {
			if err := w.walk(p + "/" + n); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// file is p's sha256, within the bytes left.
func (w *walker) file(p string, st os.FileInfo) (string, error) {
	f, err := w.root.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if fst, err := f.Stat(); err != nil || !os.SameFile(st, fst) {
		return "", fmt.Errorf("%s changed while it was read", p)
	}
	h := sha256.New()
	left := maxAdapterBytes - w.bytes
	n, err := io.Copy(h, io.LimitReader(f, left+1))
	if err != nil {
		return "", err
	}
	if w.bytes += n; n > left {
		return "", fmt.Errorf("more than %d bytes", maxAdapterBytes)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// plainName: nothing that would make a listing line ambiguous.
func plainName(s string) bool {
	return s != "" && !strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f })
}

// inClosure: a link's target, read from the link's directory, stays in
// AdapterClosure by its text alone. It is relative and clean, so its ".."
// are only leading: they climb the link's own parents (real directories,
// as walked) before it descends, and where it lands is a closure path or
// below one.
func inClosure(link, target string) bool {
	if path.IsAbs(target) || path.Clean(target) != target {
		return false
	}
	r := path.Join(path.Dir(link), target)
	return slices.ContainsFunc(AdapterClosure, func(c string) bool { return r == c || strings.HasPrefix(r, c+"/") })
}

// ErrAdapter: the image's adapter is not the installed release's. It is an
// ErrManifest, with its own text.
var ErrAdapter error = adapterError{}

type adapterError struct{}

func (adapterError) Error() string {
	return "sandbox: the image's adapter is not the installed release's"
}
func (adapterError) Unwrap() error { return ErrManifest }

// CheckAdapter refuses (ErrAdapter) an image whose adapter, for its own
// platform, is not want: the digest of the installed release's closure
// (AdapterDigest). An image without one, or with only another platform's,
// is refused.
func (img Image) CheckAdapter(want string) error {
	got, ok := img.Manifest.Adapter[img.OS+"/"+img.Arch]
	if !ok || !contract.IsDigest(want) || got != want {
		return ErrAdapter
	}
	return nil
}
