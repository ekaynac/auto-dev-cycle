// SPDX-License-Identifier: Apache-2.0

// Package blobstore is the content-addressed blob store
// (<state_dir>/blobs/sha256/ab/cd/<hex>, docs/design/m0/03). A blob is
// written to a temp file, fsynced, digest-verified and renamed into place,
// and its directory chain is fsynced, before Put returns, so an event that
// references the digest can never outlive the blob. Blobs are immutable: a
// stored blob whose bytes changed is reported, never overwritten, and is
// repaired only by restoring it from a backup (03 "Backups").
//
// One process owns the store: the caller holds the state-directory lock, and
// Open removes every temp file it finds as a crash leftover.
package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Store is a blob store rooted at <state_dir>/blobs.
type Store struct {
	root string
	mu   sync.Mutex // serialises publication, so a concurrent Put never returns before the blob is durable
}

// ErrDigestMismatch: the bytes do not hash to the declared digest, or a
// stored blob no longer hashes to its own.
var ErrDigestMismatch = errors.New("blob digest mismatch")

// ErrDeclaredDigest: the incoming bytes do not hash to the digest the caller
// declared (the supplier's fault). Put wraps it together with
// ErrDigestMismatch; a stored blob that no longer hashes to its own digest
// (the store's fault) is ErrDigestMismatch without it.
var ErrDeclaredDigest = errors.New("incoming bytes do not match the declared digest")

// ErrNotABlob: something other than a regular file sits at a blob's path.
var ErrNotABlob = errors.New("not a regular blob file")

// syncDir fsyncs a directory; a variable so tests can record the order.
var syncDir = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

var digestRe = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)

// Open returns the blob store under stateDir. The caller holds the state
// directory lock; leftover temp files from a crash are removed here.
func Open(stateDir string) (*Store, error) {
	root := filepath.Join(stateDir, "blobs")
	for _, d := range []string{filepath.Join(root, "sha256"), filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	left, err := filepath.Glob(filepath.Join(root, "tmp", "put-*"))
	if err != nil {
		return nil, err
	}
	for _, f := range left {
		if err := os.Remove(f); err != nil {
			return nil, err
		}
	}
	// Make the store's own directories durable. The state directory's entry
	// in its parent belongs to whoever created the state directory.
	for _, d := range []string{filepath.Join(root, "sha256"), filepath.Join(root, "tmp"), root, stateDir} {
		if err := syncDir(d); err != nil {
			return nil, err
		}
	}
	return &Store{root: root}, nil
}

// regular reports whether a regular file (not a symlink, not a directory)
// is at p, and an error for anything else that is there.
func regular(p string) (bool, error) {
	fi, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !fi.Mode().IsRegular():
		return false, fmt.Errorf("%w: %s is %s", ErrNotABlob, p, fi.Mode().Type())
	}
	return true, nil
}

func (s *Store) path(hexsum string) string {
	return filepath.Join(s.root, "sha256", hexsum[:2], hexsum[2:4], hexsum)
}

// Put stores r's bytes and returns their digest. If declared is non-empty,
// the bytes must hash to it, or nothing is published. When Put returns, the
// blob and every directory on its path are fsynced.
func (s *Store) Put(r io.Reader, declared string) (string, error) {
	st, err := s.Stage(r, declared)
	if err != nil {
		return "", err
	}
	defer st.Discard()
	return st.Publish()
}

// Staged is a verified copy not yet published: its bytes are fsynced in the
// store's tmp/, where nothing references them and Open removes them after
// a crash. Publish or Discard it.
type Staged struct {
	s      *Store
	tmp    string
	digest string
}

// Digest is the staged bytes' digest.
func (st *Staged) Digest() string { return st.digest }

// Stage copies r's bytes into tmp/, fsyncs them and checks them against
// declared (when non-empty), publishing nothing.
func (s *Store) Stage(r io.Reader, declared string) (*Staged, error) {
	if declared != "" && !digestRe.MatchString(declared) {
		return nil, fmt.Errorf("blobstore: malformed digest %q", declared)
	}
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "put-*")
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), r)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if err == nil && declared != "" && declared != got {
		err = fmt.Errorf("%w: %w: declared %s, got %s", ErrDigestMismatch, ErrDeclaredDigest, declared, got)
	}
	if err != nil {
		if rerr := removeMissing(tmp.Name()); rerr != nil {
			// Kept apart from the copy's error: a refusal of the bytes is
			// not a failure to clean up, nor the reverse.
			err = errors.Join(err, fmt.Errorf("%w: %v", ErrTempLeft, rerr))
		}
		return nil, err
	}
	return &Staged{s: s, tmp: tmp.Name(), digest: got}, nil
}

// Discard removes the staged copy (nothing once it was published).
func (st *Staged) Discard() error { return removeMissing(st.tmp) }

// ErrTempLeft: a failed Stage could not remove its temporary file (Open
// removes it after a restart).
var ErrTempLeft = errors.New("blobstore: a temporary file could not be removed")

// remove is os.Remove; a test makes it fail.
var remove = os.Remove

func removeMissing(p string) error {
	if err := remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Publish moves the staged copy into place and fsyncs its directory chain,
// returning its digest. A blob already there is verified instead.
func (st *Staged) Publish() (string, error) {
	s, got := st.s, st.digest
	s.mu.Lock()
	defer s.mu.Unlock()
	dst := s.path(strings.TrimPrefix(got, "sha256:"))
	if present, err := regular(dst); err != nil {
		return "", err
	} else if present {
		// Already present: prove it is intact and durable before reporting
		// success. This also completes a Put that crashed after its rename.
		if err := s.Verify(got); err != nil {
			return "", err
		}
		return got, errors.Join(s.syncChain(filepath.Dir(dst)), st.Discard())
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(st.tmp, 0o400); err != nil {
		return "", err
	}
	if err := os.Rename(st.tmp, dst); err != nil {
		return "", err
	}
	return got, s.syncChain(filepath.Dir(dst))
}

// syncChain fsyncs dir and each parent up to the store's sha256 root, then
// the store root, so new directory entries survive a crash.
func (s *Store) syncChain(dir string) error {
	stop := filepath.Join(s.root, "sha256")
	if rel, err := filepath.Rel(stop, dir); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("blobstore: %s is outside %s", dir, stop)
	}
	for d := dir; ; d = filepath.Dir(d) {
		if err := syncDir(d); err != nil {
			return err
		}
		if d == stop {
			return syncDir(s.root)
		}
	}
}

// Open returns the blob's bytes. It refuses anything but a regular file.
func (s *Store) Open(digest string) (io.ReadCloser, error) {
	m := digestRe.FindStringSubmatch(digest)
	if m == nil {
		return nil, fmt.Errorf("blobstore: malformed digest %q", digest)
	}
	p := s.path(m[1])
	if present, err := regular(p); err != nil {
		return nil, err
	} else if !present {
		return nil, fmt.Errorf("blobstore: %s: %w", digest, fs.ErrNotExist)
	}
	return os.Open(p)
}

// Has reports whether a regular file is stored under the digest. It is a
// stat only: it neither re-hashes the bytes nor makes them durable. Before an
// event references a blob, publish it with Put, which verifies and fsyncs
// even when the blob is already there. I/O errors are errors, never "absent".
func (s *Store) Has(digest string) (bool, error) {
	m := digestRe.FindStringSubmatch(digest)
	if m == nil {
		return false, fmt.Errorf("blobstore: malformed digest %q", digest)
	}
	return regular(s.path(m[1]))
}

// Verify re-hashes a stored blob and fails if its bytes changed on disk.
func (s *Store) Verify(digest string) error {
	f, err := s.Open(digest)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != digest {
		return fmt.Errorf("%w: stored %s now hashes to %s; restore it from a backup", ErrDigestMismatch, digest, got)
	}
	return nil
}
