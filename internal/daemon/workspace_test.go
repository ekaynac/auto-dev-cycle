// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// gitIn runs git in dir, failing t.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// P22a-5c-w: an attempt's workspace is the profile's repository at its
// base_ref: the clone's base tree is the branch's commit's, its files are
// checked out into a sandbox run's workspace with no .git there, and Close
// removes the run and the clone. A repository URL the sandbox refuses (a
// file:// URL; a path inside the sandbox root, where runs write) makes no
// workspace and leaves nothing behind.
func TestWorkspaceAtTheBase(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	gitIn(t, tmp, "init", "--quiet", "--initial-branch=main", src)
	if err := os.WriteFile(filepath.Join(src, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, src, "add", "a.go")
	gitIn(t, src, "commit", "--quiet", "-m", "base")
	tree := gitIn(t, src, "rev-parse", "main^{tree}")
	host := sandbox.Host{Root: filepath.Join(tmp, "sbx")}
	clones := filepath.Join(tmp, "clones")
	p := types.ProjectProfile{Repo: types.ProjectProfileRepo{Url: src, BaseRef: "refs/heads/main"}}
	ctx := context.Background()
	w, err := openWorkspace(ctx, host, clones, p, types.AttemptID(newID(t, "att")))
	if err != nil {
		t.Fatal(err)
	}
	if w.Base != types.GitID("git:"+tree) {
		t.Errorf("the base %s, want git:%s", w.Base, tree)
	}
	if b, err := os.ReadFile(filepath.Join(w.Path(), "a.go")); err != nil || string(b) != "package a\n" {
		t.Errorf("the checked-out file: %q, %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(w.Path(), ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a .git in the workspace: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(clones); len(left) != 0 {
		t.Errorf("clones left: %v", left)
	}
	if left, _ := os.ReadDir(filepath.Join(host.Root, "runs")); len(left) != 0 {
		t.Errorf("runs left: %v", left)
	}
	inside := filepath.Join(host.Root, "runs", "planted")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, tmp, "clone", "--quiet", "--bare", src, filepath.Join(inside, "r.git"))
	for name, url := range map[string]string{"a file:// URL": "file://" + src, "a repository runs may write": filepath.Join(inside, "r.git")} {
		p.Repo.Url = url
		if _, err := openWorkspace(ctx, host, clones, p, types.AttemptID(newID(t, "att"))); !errors.Is(err, sandbox.ErrSpec) {
			t.Errorf("%s: %v", name, err)
		}
		if left, _ := os.ReadDir(clones); len(left) != 0 {
			t.Errorf("%s: clones left: %v", name, left)
		}
	}
}
