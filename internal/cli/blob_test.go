// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// uploading is a fake that also takes uploads: each answers digest, or
// refuses with refuse.
type uploading struct {
	fake
	digest string
	refuse *client.Refused
	sent   [][]byte
}

func (u *uploading) Blob(_ context.Context, data []byte) ([]byte, error) {
	u.sent = append(u.sent, data)
	if u.refuse != nil {
		return nil, u.refuse
	}
	return []byte(`{"digest":"` + u.digest + `","size":5,"actor":"act_01m3250vkwffc979nzjw0g2sa1",` +
		`"uploaded_at":"2026-10-13T12:00:00Z","expires_at":"2026-10-13T13:00:00Z"}`), nil
}

// --scope-file is the scope; --attach uploads each file first and lists it
// in the scope's attachments; an upload refused sends no command; and an
// over-bound file is sent cut one byte past the bound, for the server to
// refuse.
func TestScopeFileAndAttach(t *testing.T) {
	dir := t.TempDir()
	scope, att, big := filepath.Join(dir, "spec.json"), filepath.Join(dir, "accept_test.go"), filepath.Join(dir, "big.bin")
	if os.WriteFile(scope, []byte(`{"title":"t"}`), 0o600) != nil || os.WriteFile(att, []byte("hello"), 0o600) != nil ||
		os.WriteFile(big, make([]byte, 2<<20), 0o600) != nil {
		t.Fatal("writing the files")
	}
	digest := "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	f := &uploading{fake: fake{resp: applied}, digest: digest}
	run := func(args ...string) int {
		var out, errb bytes.Buffer
		return Main(context.Background(), args, &out, &errb, f)
	}
	if code := run("task", "create", "--expect", "0", "--reason", "r", "--scope-file", scope, "--attach", att); code != entry.ExitOK {
		t.Fatalf("exit %d", code)
	}
	var got map[string]any
	_ = json.Unmarshal(f.req.Scope, &got)
	want := map[string]any{"title": "t", "attachments": []any{map[string]any{"name": "accept_test.go", "digest": digest}}}
	if b, w := mustJSON(got), mustJSON(want); b != w || len(f.sent) != 1 || string(f.sent[0]) != "hello" {
		t.Fatalf("scope %s, want %s; sent %d", b, w, len(f.sent))
	}
	if code := run("task", "create", "--expect", "0", "--reason", "r", "--scope", "{}", "--scope-file", scope); code != entry.ExitUsage {
		t.Fatalf("--scope and --scope-file: exit %d", code)
	}
	if code := run("task", "create", "--expect", "0", "--reason", "r", "--attach", att, "--attach", att); code != entry.ExitUsage || len(f.sent) != 1 {
		t.Fatalf("an attachment named twice: exit %d, %d uploads", code, len(f.sent))
	}
	if code := run("task", "create", "--expect", "0", "--reason", "r", "--scope", "[]", "--attach", att); code != entry.ExitUsage {
		t.Fatalf("--attach to a non-object: exit %d", code)
	}
	f.refuse, f.calls = &client.Refused{Result: vocab.Result{Outcome: vocab.Failed, Reason: vocab.ReasonUploadBoundExceeded, Detail: "d"}}, 0
	if code := run("task", "create", "--expect", "0", "--reason", "r", "--attach", big); code != entry.ExitRefused || f.calls != 0 {
		t.Fatalf("a refused attachment: exit %d, %d commands sent", code, f.calls)
	}
	if code := run("blob", "put", "--file", big); code != entry.ExitRefused || len(f.sent[len(f.sent)-1]) != 1<<20+1 {
		t.Fatalf("blob put: exit %d, %d bytes sent", code, len(f.sent[len(f.sent)-1]))
	}
	if code := run("blob", "put"); code != entry.ExitUsage {
		t.Fatalf("no --file: exit %d", code)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
