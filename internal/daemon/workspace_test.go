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

// gitIn runs git in dir with stdin, failing t.
func gitIn(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// sourceRepo is a repository at tmp/name whose main holds a.go.
func sourceRepo(t *testing.T, tmp, name string) string {
	t.Helper()
	src := filepath.Join(tmp, name)
	gitIn(t, tmp, "", "init", "--quiet", "--initial-branch=main", src)
	if err := os.WriteFile(filepath.Join(src, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, src, "", "add", "a.go")
	gitIn(t, src, "", "commit", "--quiet", "-m", "base")
	return src
}

// workspaceWorld is a sandbox host and a clones directory under tmp.
func workspaceWorld(t *testing.T) (tmp string, host sandbox.Host, clones string) {
	tmp = t.TempDir()
	return tmp, sandbox.Host{Root: filepath.Join(tmp, "sbx")}, filepath.Join(tmp, "clones")
}

// empty reports whether dir holds nothing (or does not exist).
func empty(dir string) bool {
	left, _ := os.ReadDir(dir)
	return len(left) == 0
}

// P22a-5c-w: an attempt's workspace is the profile's repository at its
// base_ref: the clone's base tree is the branch's commit's, its files are
// checked out into a sandbox run's workspace with no .git there, and Close
// removes the run and the clone. A repository URL the sandbox refuses (a
// file:// URL; a path inside the sandbox root, where runs write) makes no
// workspace and leaves nothing behind.
func TestWorkspaceAtTheBase(t *testing.T) {
	tmp, host, clones := workspaceWorld(t)
	src := sourceRepo(t, tmp, "src")
	tree := gitIn(t, src, "", "rev-parse", "main^{tree}")
	p := types.ProjectProfile{Repo: types.ProjectProfileRepo{Url: src, BaseRef: "refs/heads/main"}}
	ctx := context.Background()
	w, err := openWorkspace(ctx, host, clones, p, types.AttemptID(newID(t, "att")), nil)
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
	if !empty(clones) || !empty(filepath.Join(host.Root, "runs")) {
		t.Error("Close left a clone or a run")
	}
	inside := filepath.Join(host.Root, "runs", "planted")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, tmp, "", "clone", "--quiet", "--bare", src, filepath.Join(inside, "r.git"))
	for name, url := range map[string]string{"a file:// URL": "file://" + src, "a repository runs may write": filepath.Join(inside, "r.git")} {
		p.Repo.Url = url
		if _, err := openWorkspace(ctx, host, clones, p, types.AttemptID(newID(t, "att")), nil); !errors.Is(err, sandbox.ErrSpec) {
			t.Errorf("%s: %v", name, err)
		}
		if !empty(clones) {
			t.Errorf("%s: a clone left", name)
		}
	}
}

// Opus #326 M2: a base whose tree holds .git, .GIT, git~1, a nested .git,
// or a duplicate entry (a symlink and a directory of one name) refuses the
// clone, so no workspace silently differs from AttemptBase.Tree.
func TestWorkspaceRefusesAHostileTree(t *testing.T) {
	tmp, host, clones := workspaceWorld(t)
	ctx := context.Background()
	for name, entries := range map[string]string{
		".git":                       "100644 blob %[1]s\t.git\n100644 blob %[1]s\tok\n",
		".GIT":                       "100644 blob %[1]s\t.GIT\n100644 blob %[1]s\tok\n",
		"git~1":                      "100644 blob %[1]s\tgit~1\n100644 blob %[1]s\tok\n",
		"sub/.git":                   "040000 tree %[2]s\tsub\n100644 blob %[1]s\tok\n",
		"a symlink and a dir of one": "120000 blob %[3]s\ta\n040000 tree %[4]s\ta\n",
	} {
		t.Run(name, func(t *testing.T) {
			src := sourceRepo(t, tmp, strings.NewReplacer("/", "-", ".", "_", "~", "-", " ", "-").Replace(name))
			blob := gitIn(t, src, "x\n", "hash-object", "-w", "--stdin")
			dotGit := gitIn(t, src, fmtEntries("100644 blob %[1]s\t.git\n", blob), "mktree")
			link := gitIn(t, src, "/etc", "hash-object", "-w", "--stdin")
			pwn := gitIn(t, src, fmtEntries("100644 blob %[1]s\tpwn\n", blob), "mktree")
			tree := gitIn(t, src, fmtEntries(entries, blob, dotGit, link, pwn), "mktree", "--missing")
			commit := gitIn(t, src, "", "commit-tree", tree, "-m", "hostile")
			gitIn(t, src, "", "update-ref", "refs/heads/main", commit)
			p := types.ProjectProfile{Repo: types.ProjectProfileRepo{Url: src, BaseRef: "refs/heads/main"}}
			w, err := openWorkspace(ctx, host, clones, p, types.AttemptID(newID(t, "att")), nil)
			if err == nil {
				_ = w.Close()
			}
			if !errors.Is(err, sandbox.ErrGit) || errors.Is(err, ErrBaseMismatch) { // refused at the clone, by its object checks
				t.Fatalf("a tree with %s: %v", name, err)
			}
			if !empty(clones) || !empty(filepath.Join(host.Root, "runs")) {
				t.Errorf("%s: a clone or a run left", name)
			}
		})
	}
}

// fmtEntries is entries with its %[n]s verbs replaced by args.
func fmtEntries(entries string, args ...string) string {
	for i, a := range args {
		entries = strings.ReplaceAll(entries, "%["+string(rune('1'+i))+"]s", a)
	}
	return entries
}

// Opus #326 M4, L4, L5, L8: a pinned base is the one checked out after the
// branch moved on; a pin the clone does not hold is refused; an empty base
// checks out nothing; an attempt id that is no attempt's is refused before
// it names a directory (another attempt's clone kept); a clone whose
// origin is not the profile's URL is refused, leaving nothing.
func TestWorkspacePinsItsBase(t *testing.T) {
	tmp, host, clones := workspaceWorld(t)
	src := sourceRepo(t, tmp, "src")
	first := types.GitID("git:" + gitIn(t, src, "", "rev-parse", "main^{tree}"))
	if err := os.WriteFile(filepath.Join(src, "a.go"), []byte("package a // moved on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, src, "", "commit", "--quiet", "-am", "later")
	p := types.ProjectProfile{Repo: types.ProjectProfileRepo{Url: src, BaseRef: "refs/heads/main"}}
	ctx := context.Background()
	w, err := openWorkspace(ctx, host, clones, p, types.AttemptID(newID(t, "att")), &first)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(w.Path(), "a.go")); w.Base != first || string(b) != "package a\n" {
		t.Errorf("the pinned base: %s, %q", w.Base, b)
	}
	_ = w.Close()
	absent := types.GitID("git:" + strings.Repeat("e", 40))
	if _, err := openWorkspace(ctx, host, clones, p, types.AttemptID(newID(t, "att")), &absent); !errors.Is(err, ErrBaseMismatch) || !empty(clones) {
		t.Errorf("a pin the clone does not hold: %v", err)
	}
	bare := filepath.Join(tmp, "empty")
	gitIn(t, tmp, "", "init", "--quiet", "--initial-branch=main", bare)
	gitIn(t, bare, "", "commit", "--quiet", "--allow-empty", "-m", "empty")
	p.Repo.Url = bare
	w, err = openWorkspace(ctx, host, clones, p, types.AttemptID(newID(t, "att")), nil)
	if err != nil || w.Base != types.GitID("git:"+emptyTree) || !empty(w.Path()) {
		t.Fatalf("an empty base: %v", err)
	}
	_ = w.Close()
	keep := filepath.Join(clones, "att_kept")
	if err := os.MkdirAll(keep, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := openWorkspace(ctx, host, clones, p, "", nil); err == nil {
		t.Error("an empty attempt id made a workspace")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("another attempt's clone was removed: %v", err)
	}
	if err := os.Remove(keep); err != nil {
		t.Fatal(err)
	}
	other := sourceRepo(t, tmp, "other")
	cloneRepo = func(h sandbox.Host, ctx context.Context, _, ref, dst string) (sandbox.Repo, error) {
		return h.Clone(ctx, other, ref, dst)
	}
	t.Cleanup(func() { cloneRepo = sandbox.Host.Clone })
	p.Repo.Url = src
	if _, err := openWorkspace(ctx, host, clones, p, types.AttemptID(newID(t, "att")), nil); !errors.Is(err, ErrRemoteMismatch) || !empty(clones) || !empty(filepath.Join(host.Root, "runs")) {
		t.Errorf("a clone of another repository: %v", err)
	}
}
