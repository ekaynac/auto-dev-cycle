// SPDX-License-Identifier: Apache-2.0

package blobstore

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const helloDigest = "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" // sha256("hello")

func TestPutOpenVerify(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Put(strings.NewReader("hello"), "")
	if err != nil || d != helloDigest {
		t.Fatalf("Put = %q, %v", d, err)
	}
	again, err := s.Put(strings.NewReader("hello"), helloDigest)
	if err != nil || again != d {
		t.Fatalf("second Put = %q, %v", again, err)
	}
	r, err := s.Open(d)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	if has, err := s.Has(d); string(b) != "hello" || !has || err != nil {
		t.Fatalf("read back %q", b)
	}
	if err := s.Verify(d); err != nil {
		t.Fatal(err)
	}
}

func TestDeclaredDigestMismatchPublishesNothing(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	wrong := "sha256:" + strings.Repeat("0", 64)
	if _, err := s.Put(strings.NewReader("hello"), wrong); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want ErrDigestMismatch, got %v", err)
	}
	if a, _ := s.Has(wrong); a {
		t.Fatal("a mismatched blob was published")
	}
	if b, _ := s.Has(helloDigest); b {
		t.Fatal("a mismatched blob was published")
	}
	if left, _ := os.ReadDir(filepath.Join(dir, "blobs", "tmp")); len(left) != 0 {
		t.Fatalf("temp files left behind: %d", len(left))
	}
}

func TestVerifyDetectsChangedBytes(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	d, _ := s.Put(strings.NewReader("hello"), "")
	p := s.path(strings.TrimPrefix(d, "sha256:"))
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("HELLO"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(d); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want ErrDigestMismatch, got %v", err)
	}
}

func TestMalformedDigestsRefused(t *testing.T) {
	s, _ := Open(t.TempDir())
	for _, d := range []string{"", "sha256:xyz", "md5:" + strings.Repeat("a", 32), "sha256:../../etc/passwd"} {
		if _, err := s.Open(d); err == nil {
			t.Errorf("Open(%q) accepted", d)
		}
		if has, err := s.Has(d); has || err == nil {
			t.Errorf("Has(%q) = %v, %v; want a malformed-digest error", d, has, err)
		}
	}
	if _, err := s.Put(strings.NewReader("x"), "sha256:bad"); err == nil {
		t.Error("Put with a malformed declared digest accepted")
	}
}

// Re-putting bytes whose stored copy was tampered with must not report
// success: the existing blob is verified before Put returns.
func TestPutRefusesATamperedExistingBlob(t *testing.T) {
	s, _ := Open(t.TempDir())
	d, _ := s.Put(strings.NewReader("hello"), "")
	p := s.path(strings.TrimPrefix(d, "sha256:"))
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("HELLO"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(strings.NewReader("hello"), ""); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want ErrDigestMismatch, got %v", err)
	}
}

func TestOpenCleansCrashLeftovers(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "blobs", "tmp", "put-123")
	if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale temp file survived Open: %v", err)
	}
}

// Concurrent puts of the same and of different bytes all succeed with the
// right digest, leave exactly one file per blob and no temp files.
func TestConcurrentPuts(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const workers = 16
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			body := "hello"
			if i%2 == 1 {
				body = fmt.Sprintf("blob %d", i)
			}
			want := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body)))
			got, err := s.Put(strings.NewReader(body), "")
			if err == nil && got != want {
				err = fmt.Errorf("Put(%q) = %s, want %s", body, got, want)
			}
			errs <- err
		}(i)
	}
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	var blobs int
	err = filepath.WalkDir(filepath.Join(s.root, "sha256"), func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			blobs++
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := 1 + workers/2; blobs != want {
		t.Errorf("%d blob files, want %d", blobs, want)
	}
	if left, _ := filepath.Glob(filepath.Join(s.root, "tmp", "*")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}
