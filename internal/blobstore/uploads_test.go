// SPDX-License-Identifier: Apache-2.0

package blobstore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	alice = "act_01m3250vkwffc979nzjw0g2sa1"
	bob   = "act_01m3250vkwffc979nzjw0g2sa2"
)

var t0 = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func pub(s *Store, actor, digest string, now time.Time) error {
	_, err := s.Publish(actor, digest, now)
	return err
}

// Drop takes the upload as published: one renewed since stays.
func TestDropLeavesARenewal(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.Upload(alice, strings.NewReader("hello"), t0)
	at, err := s.Publish(alice, helloDigest, t0)
	if err != nil || !at.Equal(t0) {
		t.Fatalf("Publish = %v, %v", at, err)
	}
	renewed, _ := s.Upload(alice, strings.NewReader("hello"), t0.Add(time.Minute))
	if err := s.Drop(alice, helloDigest, at); err != nil || len(listed(t, s)) != 1 || listed(t, s)[0] != renewed {
		t.Fatalf("Drop of the published upload took the renewal: %v %+v", err, listed(t, s))
	}
	if err := s.Drop(alice, helloDigest, renewed.UploadedAt); err != nil || len(listed(t, s)) != 0 {
		t.Fatalf("Drop: %v", err)
	}
}

func listed(t *testing.T, s *Store) []Upload {
	t.Helper()
	us, err := s.Uploads()
	if err != nil {
		t.Fatal(err)
	}
	return us
}

// An upload is listed with its actor, size and stored time, and only its
// actor's reference before expiry publishes it, keeping the upload until
// Drop (a retried command publishes again): another actor's, an expired
// one and a dropped one are ErrNoUpload, publishing nothing.
func TestUploadOwnershipAndExpiry(t *testing.T) {
	s, _ := Open(t.TempDir())
	u, err := s.Upload(alice, strings.NewReader("hello"), t0)
	if err != nil || u.Digest != helloDigest || u.Size != 5 || !u.UploadedAt.Equal(t0) {
		t.Fatalf("Upload = %+v, %v", u, err)
	}
	if us := listed(t, s); len(us) != 1 || us[0] != u {
		t.Fatalf("Uploads = %+v", us)
	}
	if err := pub(s, bob, helloDigest, t0); !errors.Is(err, ErrNoUpload) {
		t.Fatalf("a foreign actor's reference: %v", err)
	}
	if err := pub(s, alice, helloDigest, t0.Add(UploadTTL)); !errors.Is(err, ErrNoUpload) {
		t.Fatalf("an expired reference: %v", err)
	}
	if err := pub(s, "../"+alice, helloDigest, t0); !errors.Is(err, ErrNoUpload) {
		t.Fatalf("a malformed actor: %v", err)
	}
	if has, _ := s.Has(helloDigest); has {
		t.Fatal("a refused reference published the blob")
	}
	for range 2 { // a retry publishes again
		if err := pub(s, alice, helloDigest, t0.Add(UploadTTL-time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if has, _ := s.Has(helloDigest); !has || len(listed(t, s)) != 1 {
		t.Fatal("Publish did not publish the blob, or dropped the upload")
	}
	if err := s.Drop(alice, helloDigest, u.UploadedAt); err != nil || len(listed(t, s)) != 0 {
		t.Fatalf("Drop: %v", err)
	}
	if err := pub(s, alice, helloDigest, t0); !errors.Is(err, ErrNoUpload) {
		t.Fatalf("a dropped upload: %v", err)
	}
}

// Over a bound, nothing is stored: one upload over 1 MiB; a 65th upload
// or a 33rd MiB for one actor, while another actor is unaffected. A repeat
// of a held digest is no new upload.
func TestUploadBounds(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	if _, err := s.Upload(alice, bytes.NewReader(make([]byte, MaxUploadBytes+1)), t0); !errors.Is(err, ErrUploadBound) {
		t.Fatalf("oversized: %v", err)
	}
	if us := listed(t, s); len(us) != 0 {
		t.Fatalf("an oversized upload was stored: %+v", us)
	}
	if left, _ := os.ReadDir(filepath.Join(dir, "blobs", "tmp")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	for i := range 32 {
		b := bytes.Repeat([]byte{byte(i)}, MaxUploadBytes)
		if _, err := s.Upload(alice, bytes.NewReader(b), t0); err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
	}
	if _, err := s.Upload(alice, strings.NewReader("one byte more"), t0); !errors.Is(err, ErrUploadBound) {
		t.Fatalf("over 32 MiB: %v", err)
	}
	if _, err := s.Upload(alice, bytes.NewReader(bytes.Repeat([]byte{0}, MaxUploadBytes)), t0.Add(time.Minute)); err != nil {
		t.Fatalf("a repeat: %v", err)
	}
	for i := range MaxActorUploads {
		if _, err := s.Upload(bob, strings.NewReader(strings.Repeat("x", i+1)), t0); err != nil {
			t.Fatalf("bob's upload %d: %v", i, err)
		}
	}
	if _, err := s.Upload(bob, strings.NewReader("sixty-fifth"), t0); !errors.Is(err, ErrUploadBound) {
		t.Fatalf("a 65th upload: %v", err)
	}
	if us := listed(t, s); len(us) != 32+MaxActorUploads || !us[len(us)-1].UploadedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("%d uploads, the renewed one last: %+v", len(us), us[len(us)-1])
	}
}

// A purge sets an upload aside before it records it: from then a reference
// is refused, a re-upload of the digest is a new upload that the burial
// leaves alone, and an upload renewed since it was listed is not set aside.
func TestPurgeAgainstReferenceAndRenewal(t *testing.T) {
	s, _ := Open(t.TempDir())
	u, _ := s.Upload(alice, strings.NewReader("hello"), t0)
	if renewed, _ := s.Upload(alice, strings.NewReader("hello"), t0.Add(time.Minute)); renewed.UploadedAt.Equal(u.UploadedAt) {
		t.Fatal("the renewal kept the old time")
	}
	if set, err := s.Tombstone(u); set || err != nil {
		t.Fatalf("a renewed upload was set aside: %v", err)
	}
	u = listed(t, s)[0]
	if set, err := s.Tombstone(u); !set || err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	if err := pub(s, alice, helloDigest, t0); !errors.Is(err, ErrNoUpload) {
		t.Fatalf("a reference to an upload set aside: %v", err)
	}
	again, err := s.Upload(alice, strings.NewReader("hello"), t0.Add(2*time.Minute))
	if tombs, _ := s.Tombstones(); err != nil || len(tombs) != 1 || tombs[0] != u {
		t.Fatalf("re-upload %v; tombstones %+v", err, tombs)
	}
	if err := s.Bury(u); err != nil || len(listed(t, s)) != 1 || listed(t, s)[0] != again {
		t.Fatalf("Bury: %v, %+v", err, listed(t, s))
	}
	if tombs, _ := s.Tombstones(); len(tombs) != 0 {
		t.Fatalf("tombstones left: %+v", tombs)
	}
}

// Raced against a purge, a reference either publishes the blob (before the
// upload was set aside) or is refused because it was set aside; never a
// refusal with the upload live, nor a success that published nothing.
func TestReferenceRacesPurge(t *testing.T) {
	for i := range 50 {
		s, _ := Open(t.TempDir())
		u, _ := s.Upload(alice, strings.NewReader("hello"), t0)
		var wg sync.WaitGroup
		var perr error
		var set bool
		wg.Go(func() { _, perr = s.Publish(alice, u.Digest, t0) })
		wg.Go(func() { set, _ = s.Tombstone(u) })
		wg.Wait()
		has, _ := s.Has(u.Digest)
		if (perr == nil) != has || (perr != nil && (!errors.Is(perr, ErrNoUpload) || !set)) {
			t.Fatalf("run %d: published %v (%v), set aside %v", i, has, perr, set)
		}
	}
}

// What cannot be read as an upload is skipped and named, and the rest is
// still listed.
func TestMalformedEntriesSkipped(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	u, _ := s.Upload(alice, strings.NewReader("hello"), t0)
	up := filepath.Join(dir, "blobs", "uploads")
	if os.WriteFile(filepath.Join(up, alice, "junk"), nil, 0o600) != nil || os.Mkdir(filepath.Join(up, "not-an-actor"), 0o700) != nil {
		t.Fatal("planting")
	}
	us, err := s.Uploads()
	if !errors.Is(err, ErrNotABlob) || !strings.Contains(err.Error(), "junk") || !strings.Contains(err.Error(), "not-an-actor") || len(us) != 1 || us[0] != u {
		t.Fatalf("Uploads = %+v, %v", us, err)
	}
}
