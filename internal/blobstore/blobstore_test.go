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
	"testing/iotest"
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
	for _, d := range []string{wrong, helloDigest} {
		if has, err := s.Has(d); has || err != nil {
			t.Fatalf("Has(%s) = %v, %v after a mismatched Put", d, has, err)
		}
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
	for _, d := range []string{"", "sha256:xyz", "md5:" + strings.Repeat("a", 32), "sha256:../../etc/passwd",
		strings.ToUpper(helloDigest[:7]) + helloDigest[7:], "sha256:" + strings.ToUpper(helloDigest[7:]),
		helloDigest + "\n", " " + helloDigest, helloDigest[:len(helloDigest)-1], helloDigest + "0",
		helloDigest[:len(helloDigest)-1] + "/"} {
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

// recordSyncs replaces syncDir for one test and returns the directories
// synced, relative to base.
func recordSyncs(t *testing.T, base string) *[]string {
	t.Helper()
	var got []string
	orig := syncDir
	syncDir = func(dir string) error {
		rel, err := filepath.Rel(base, dir)
		if err != nil {
			return err
		}
		got = append(got, filepath.ToSlash(rel))
		return orig(dir)
	}
	t.Cleanup(func() { syncDir = orig })
	return &got
}

func TestDurability(t *testing.T) {
	dir := t.TempDir()
	syncs := recordSyncs(t, dir)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"blobs/sha256", "blobs/tmp", "blobs", "."}; !equal(*syncs, want) {
		t.Fatalf("Open synced %v, want %v", *syncs, want)
	}
	*syncs = nil
	if _, err := s.Put(strings.NewReader("hello"), ""); err != nil {
		t.Fatal(err)
	}
	if want := []string{"blobs/sha256/2c/f2", "blobs/sha256/2c", "blobs/sha256", "blobs"}; !equal(*syncs, want) {
		t.Fatalf("Put synced %v, want %v", *syncs, want)
	}
}

// A crash after the rename but before the directory fsync leaves the blob in
// place; the next Put of the same bytes verifies it and makes it durable.
func TestPutCompletesACrashedPut(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := s.path(strings.TrimPrefix(helloDigest, "sha256:"))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("hello"), 0o400); err != nil {
		t.Fatal(err)
	}
	syncs := recordSyncs(t, dir)
	if d, err := s.Put(strings.NewReader("hello"), helloDigest); err != nil || d != helloDigest {
		t.Fatalf("Put = %q, %v", d, err)
	}
	if want := []string{"blobs/sha256/2c/f2", "blobs/sha256/2c", "blobs/sha256", "blobs"}; !equal(*syncs, want) {
		t.Fatalf("Put synced %v, want %v", *syncs, want)
	}
}

func TestReaderFailureLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	boom := errors.New("boom")
	if _, err := s.Put(iotest.ErrReader(boom), ""); !errors.Is(err, boom) {
		t.Fatalf("want the reader's error, got %v", err)
	}
	if left, _ := os.ReadDir(filepath.Join(dir, "blobs", "tmp")); len(left) != 0 {
		t.Fatalf("temp files left behind: %d", len(left))
	}
}

func TestHasReportsIOErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	s, _ := Open(t.TempDir())
	fan := filepath.Join(s.root, "sha256", "2c")
	if err := os.MkdirAll(fan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fan, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(fan, 0o700) })
	if has, err := s.Has(helloDigest); has || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Has = %v, %v; want a permission error", has, err)
	}
}

// Only a regular file is a blob: a directory or a symlink at the path is an
// error for Has, Open and Put, never "present".
func TestNonRegularFilesAreNotBlobs(t *testing.T) {
	for name, plant := range map[string]func(p, outside string) error{
		"directory": func(p, _ string) error { return os.Mkdir(p, 0o700) },
		"symlink":   func(p, outside string) error { return os.Symlink(outside, p) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s, _ := Open(dir)
			outside := filepath.Join(dir, "outside")
			if err := os.WriteFile(outside, []byte("hello"), 0o600); err != nil {
				t.Fatal(err)
			}
			p := s.path(strings.TrimPrefix(helloDigest, "sha256:"))
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := plant(p, outside); err != nil {
				t.Fatal(err)
			}
			if has, err := s.Has(helloDigest); has || !errors.Is(err, ErrNotABlob) {
				t.Errorf("Has = %v, %v; want ErrNotABlob", has, err)
			}
			if _, err := s.Open(helloDigest); !errors.Is(err, ErrNotABlob) {
				t.Errorf("Open: want ErrNotABlob, got %v", err)
			}
			if _, err := s.Put(strings.NewReader("hello"), ""); !errors.Is(err, ErrNotABlob) {
				t.Errorf("Put: want ErrNotABlob, got %v", err)
			}
		})
	}
}

func TestSyncChainRefusesPathsOutsideTheStore(t *testing.T) {
	s, _ := Open(t.TempDir())
	for _, d := range []string{"/", s.root, filepath.Join(s.root, "tmp"), filepath.Join(s.root, "sha256x")} {
		if err := s.syncChain(d); err == nil {
			t.Errorf("syncChain(%s) accepted", d)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
