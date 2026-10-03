// SPDX-License-Identifier: Apache-2.0

// Package blobstore is the content-addressed blob store
// (<state_dir>/blobs/sha256/ab/cd/<hex>, docs/design/m0/03). A blob is
// written to a temp file, fsynced, digest-verified and renamed into place,
// and its directory chain is fsynced, before Put returns, so an event that
// references the digest can never outlive the blob. Blobs are immutable.
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
	"sync"
)

// Store is a blob store rooted at <state_dir>/blobs.
type Store struct {
	root string
	mu   sync.Mutex // serialises publication, so a concurrent Put never returns before the blob is durable
}

// ErrDigestMismatch: the bytes do not hash to the declared digest.
var ErrDigestMismatch = errors.New("blob digest mismatch")

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
	return &Store{root: root}, nil
}

func (s *Store) path(hexsum string) string {
	return filepath.Join(s.root, "sha256", hexsum[:2], hexsum[2:4], hexsum)
}

// Put stores r's bytes and returns their digest. If declared is non-empty,
// the bytes must hash to it, or nothing is published. When Put returns, the
// blob and every directory on its path are fsynced.
func (s *Store) Put(r io.Reader, declared string) (string, error) {
	if declared != "" && !digestRe.MatchString(declared) {
		return "", fmt.Errorf("blobstore: malformed digest %q", declared)
	}
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "put-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), r)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	got := "sha256:" + sum
	if declared != "" && declared != got {
		return "", fmt.Errorf("%w: declared %s, got %s", ErrDigestMismatch, declared, got)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	dst := s.path(sum)
	if _, err := os.Stat(dst); err == nil {
		// Already present: prove it is intact and durable before reporting success.
		if err := s.Verify(got); err != nil {
			return "", err
		}
		return got, s.syncChain(filepath.Dir(dst))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o400); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", err
	}
	return got, s.syncChain(filepath.Dir(dst))
}

// syncChain fsyncs dir and each parent up to the store's sha256 root, so new
// directory entries survive a crash.
func (s *Store) syncChain(dir string) error {
	stop := filepath.Join(s.root, "sha256")
	for d := dir; ; d = filepath.Dir(d) {
		if err := syncDir(d); err != nil {
			return err
		}
		if d == stop {
			return syncDir(s.root)
		}
	}
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Open returns the blob's bytes.
func (s *Store) Open(digest string) (io.ReadCloser, error) {
	m := digestRe.FindStringSubmatch(digest)
	if m == nil {
		return nil, fmt.Errorf("blobstore: malformed digest %q", digest)
	}
	return os.Open(s.path(m[1]))
}

// Has reports whether the blob is published. I/O errors are errors, never
// "absent".
func (s *Store) Has(digest string) (bool, error) {
	m := digestRe.FindStringSubmatch(digest)
	if m == nil {
		return false, fmt.Errorf("blobstore: malformed digest %q", digest)
	}
	_, err := os.Stat(s.path(m[1]))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
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
		return fmt.Errorf("%w: stored %s now hashes to %s", ErrDigestMismatch, digest, got)
	}
	return nil
}
