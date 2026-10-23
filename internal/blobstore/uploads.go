// SPDX-License-Identifier: Apache-2.0

package blobstore

// Uploads (docs/design/m1/00-slice-plan.md P2): bytes a client sends ahead
// of the command that references them, kept apart from the content-addressed
// store at <blobs>/uploads/<actor>/<hex>, the file's mtime its upload time.
// A command by the same actor publishes it (Publish) and, once committed,
// drops it (Drop). An expired one is set aside as a tombstone,
// <actor>/.purging-<hex>, which no upload, publication or count sees, until
// its purge is recorded and the tombstone buried. Nothing serves an
// upload's bytes back.

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
	"slices"
	"strings"
	"time"
)

// The bounds: per upload, and per actor over its live uploads.
const (
	MaxUploadBytes      = 1 << 20
	MaxActorUploads     = 64
	MaxActorUploadBytes = 32 << 20
	UploadTTL           = time.Hour
)

// ErrUploadBound: the upload is over a bound; nothing was stored.
var ErrUploadBound = errors.New("blobstore: the upload exceeds a bound")

// ErrNoUpload: the actor holds no live upload of the digest (none, another
// actor's, dropped, expired, or set aside for its purge); a referencing
// command answers it failed/stale_expected_state.
var ErrNoUpload = errors.New("blobstore: no live upload of that digest by that actor")

// Upload is one upload, live or set aside.
type Upload struct {
	Actor, Digest string
	Size          int64
	UploadedAt    time.Time
}

const tombstone = ".purging-"

var (
	actorRe = regexp.MustCompile(`^act_[0-9a-z]{26}$`)
	hexRe   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// uploadPath is where actor's live upload of digest is.
func (s *Store) uploadPath(actor, digest string) (string, error) {
	hx, ok := hexOf(digest)
	if !ok || !actorRe.MatchString(actor) {
		return "", fmt.Errorf("blobstore: malformed upload %q of %q", digest, actor)
	}
	return filepath.Join(s.root, "uploads", actor, hx), nil
}

func tombOf(p string) string { return filepath.Join(filepath.Dir(p), tombstone+filepath.Base(p)) }

// Upload stores r's bytes as actor's upload at now, unless that would put
// it over a bound, and returns it as stored. A repeat of an actor's live
// digest renews it.
func (s *Store) Upload(actor string, r io.Reader, now time.Time) (Upload, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxUploadBytes+1))
	if err != nil {
		return Upload{}, fmt.Errorf("blobstore: reading the upload: %w", err)
	} else if len(b) > MaxUploadBytes {
		return Upload{}, fmt.Errorf("%w: more than %d bytes", ErrUploadBound, MaxUploadBytes)
	}
	sum := sha256.Sum256(b)
	u := Upload{Actor: actor, Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(b))}
	p, err := s.uploadPath(actor, u.Digest)
	if err != nil {
		return Upload{}, err
	}
	s.up.Lock()
	defer s.up.Unlock()
	held, _, err := s.entries(filepath.Dir(p), false)
	count, total := 1, u.Size
	for _, h := range held {
		if h.Digest != u.Digest {
			count, total = count+1, total+h.Size
		}
	}
	if err == nil && (count > MaxActorUploads || total > MaxActorUploadBytes) {
		err = fmt.Errorf("%w: an actor holds at most %d uploads and %d bytes", ErrUploadBound, MaxActorUploads, MaxActorUploadBytes)
	}
	if err != nil {
		return Upload{}, err
	}
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "put-*") // Open removes it after a crash
	if err != nil {
		return Upload{}, err
	}
	defer removeMissing(tmp.Name()) // gone once renamed; else Open removes a leftover
	_, err = tmp.Write(b)
	if err = errors.Join(err, tmp.Sync(), tmp.Close(), os.Chtimes(tmp.Name(), now, now), os.MkdirAll(filepath.Dir(p), 0o700)); err == nil {
		err = os.Rename(tmp.Name(), p)
	}
	var fi os.FileInfo
	if err == nil {
		fi, err = os.Lstat(p)
	}
	if err != nil {
		return Upload{}, err
	}
	u.UploadedAt = fi.ModTime()
	return u, errors.Join(syncDir(filepath.Dir(p)), syncDir(filepath.Dir(filepath.Dir(p))))
}

// entries are the uploads (or, with purging, the tombstones) in one
// actor's dir, the caller holding s.up. A malformed entry is skipped and
// named in bad; err is a directory that cannot be read.
func (s *Store) entries(dir string, purging bool) (out []Upload, bad, err error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	for _, e := range ents {
		name, tomb := strings.CutPrefix(e.Name(), tombstone)
		if tomb != purging {
			continue
		}
		fi, ierr := e.Info()
		if ierr != nil || !hexRe.MatchString(name) || !fi.Mode().IsRegular() {
			bad = errors.Join(bad, fmt.Errorf("%w: %s: %v", ErrNotABlob, filepath.Join(dir, e.Name()), ierr))
			continue
		}
		out = append(out, Upload{Actor: filepath.Base(dir), Digest: "sha256:" + name, Size: fi.Size(), UploadedAt: fi.ModTime()})
	}
	return out, bad, err
}

// Uploads lists the live uploads, oldest first; Tombstones, those set
// aside whose purge is unfinished. Whatever cannot be read as one (a
// malformed entry, an unreadable directory) is skipped and named in the
// error, beside the list of the rest.
func (s *Store) Uploads() ([]Upload, error)    { return s.list(false) }
func (s *Store) Tombstones() ([]Upload, error) { return s.list(true) }

func (s *Store) list(purging bool) ([]Upload, error) {
	s.up.Lock()
	defer s.up.Unlock()
	root := filepath.Join(s.root, "uploads")
	actors, skipped := os.ReadDir(root)
	var out []Upload
	for _, a := range actors {
		if !actorRe.MatchString(a.Name()) {
			skipped = errors.Join(skipped, fmt.Errorf("%w: %s", ErrNotABlob, filepath.Join(root, a.Name())))
			continue
		}
		us, bad, err := s.entries(filepath.Join(root, a.Name()), purging)
		out, skipped = append(out, us...), errors.Join(skipped, bad, err)
	}
	slices.SortFunc(out, func(a, b Upload) int { return a.UploadedAt.Compare(b.UploadedAt) })
	return out, skipped
}

// Publish puts actor's live upload of digest into the store before now's
// expiry, and keeps the upload: a referencing command publishes before it
// commits, so its events may reference the digest, and calls Drop only
// once it has committed, with the upload time Publish returns; a retry
// finds the upload still there. Any other actor's upload, or none, is
// ErrNoUpload, and nothing is published.
func (s *Store) Publish(actor, digest string, now time.Time) (time.Time, error) {
	p, err := s.uploadPath(actor, digest)
	if err != nil {
		return time.Time{}, errors.Join(ErrNoUpload, err)
	}
	s.up.Lock()
	defer s.up.Unlock()
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && (!fi.Mode().IsRegular() || !now.Before(fi.ModTime().Add(UploadTTL)))) {
		return time.Time{}, ErrNoUpload
	}
	f, oerr := os.Open(p)
	if err = errors.Join(err, oerr); err != nil {
		return time.Time{}, err
	}
	_, err = s.Put(f, digest)
	return fi.ModTime(), errors.Join(err, f.Close())
}

// Drop removes actor's upload of digest as published at uploadedAt (the
// time Publish returned); a referencing command calls it after it
// committed. An upload renewed since, or none, is left alone. The blob
// stays published.
func (s *Store) Drop(actor, digest string, uploadedAt time.Time) error {
	p, err := s.uploadPath(actor, digest)
	if err != nil {
		return err
	}
	s.up.Lock()
	defer s.up.Unlock()
	if fi, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.ModTime().Equal(uploadedAt)) {
		return nil
	} else if err != nil {
		return err
	}
	if err := remove(p); err != nil {
		return err
	}
	return syncDir(filepath.Dir(p))
}

// Tombstone sets u aside for its purge, reporting false when it was
// renewed or dropped since it was listed. From here no upload, Publish or
// count sees it; Bury removes it once the purge is recorded.
func (s *Store) Tombstone(u Upload) (bool, error) {
	p, err := s.uploadPath(u.Actor, u.Digest)
	if err != nil {
		return false, err
	}
	s.up.Lock()
	defer s.up.Unlock()
	if fi, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.ModTime().Equal(u.UploadedAt)) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if _, err := os.Lstat(tombOf(p)); !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("blobstore: an unfinished purge of %s by %s is in the way", u.Digest, u.Actor)
	}
	if err := os.Rename(p, tombOf(p)); err != nil {
		return false, err
	}
	return true, syncDir(filepath.Dir(p))
}

// Bury removes u's tombstone (none: nothing to do).
func (s *Store) Bury(u Upload) error {
	p, err := s.uploadPath(u.Actor, u.Digest)
	if err != nil {
		return err
	}
	s.up.Lock()
	defer s.up.Unlock()
	if err := removeMissing(tombOf(p)); err != nil {
		return err
	}
	return syncDir(filepath.Dir(p))
}
