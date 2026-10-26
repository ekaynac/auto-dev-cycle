// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

// `adc dev referee` writes its verdict record to a new file only: an
// existing --out is refused before anything runs, and left as it was.
func TestDevRefereeOutput(t *testing.T) {
	dir := t.TempDir()
	item, patch, out := filepath.Join(dir, "item.json"), filepath.Join(dir, "p.diff"), filepath.Join(dir, "verdict.json")
	for p, c := range map[string]string{item: `{"repo": "r"}`, patch: ""} {
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) (string, int) {
		var stdout, stderr bytes.Buffer
		code := Main(context.Background(), append([]string{"dev", "referee"}, args...), &stdout, &stderr, nil)
		return stdout.String(), code
	}
	if _, code := run("--item", item, "--patch", patch); code != entry.ExitUsage {
		t.Fatalf("no --out: exit %d", code)
	}
	stdout, code := run("--item", item, "--patch", patch, "--out", out, "--podman", "/bin/false")
	if code != entry.ExitRefused || stdout != "invalid_item\n" {
		t.Fatalf("an item off its schema: exit %d, %q", code, stdout)
	}
	b, err := os.ReadFile(out)
	var rep struct{ Verdict string }
	if err != nil || json.Unmarshal(b, &rep) != nil || rep.Verdict != "invalid_item" {
		t.Fatalf("the record: %s %v", b, err)
	}
	if _, code := run("--item", item, "--patch", patch, "--out", out, "--podman", "/bin/false"); code != entry.ExitUsage {
		t.Fatalf("an existing --out: exit %d", code)
	}
	if again, _ := os.ReadFile(out); !bytes.Equal(again, b) {
		t.Fatal("an existing --out was overwritten")
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(filepath.Join(dir, "absent.json"), link); err != nil {
		t.Fatal(err)
	}
	if _, code := run("--item", item, "--patch", patch, "--out", link, "--podman", "/bin/false"); code != entry.ExitUsage {
		t.Fatalf("a dangling symlink --out: exit %d", code)
	}
	if _, err := os.Lstat(filepath.Join(dir, "absent.json")); err == nil {
		t.Fatal("written through a symlink")
	}
}
