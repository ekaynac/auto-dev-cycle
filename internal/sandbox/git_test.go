// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitOK(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%q: %v: %s", cmd.Args, err, out)
	}
}

type hostileRepo struct {
	src, ws, canary string
	a               *Attempt
}

// hostile is a base repository and a workspace a run cloned from it and
// then armed with every command hook the review named (00 §2.6
// hostile-git-dir), in .git/config, .git/info/attributes, an included
// file, core.attributesFile and the tree, each touching canary.
func hostile(t *testing.T) hostileRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	a, err := testHost(t, &fake{}).NewAttempt()
	must(t, err)
	h := hostileRepo{src: filepath.Join(tmp, "src"), ws: a.Workspace(), canary: filepath.Join(tmp, "canary"), a: a}
	id := []string{"-c", "user.name=t", "-c", "user.email=t@t"}
	gitOK(t, git(ctx, tmp, nil, "init", "--quiet", "--initial-branch=main", h.src))
	must(t, os.WriteFile(filepath.Join(h.src, "a.txt"), []byte("a\n"), 0o600))
	gitOK(t, git(ctx, h.src, nil, "add", "a.txt"))
	gitOK(t, git(ctx, h.src, nil, append(id, "commit", "--quiet", "-m", "base")...))
	must(t, os.Remove(h.ws))
	gitOK(t, git(ctx, tmp, nil, "clone", "--quiet", h.src, h.ws)) // the run's own clone

	touch := "touch " + h.canary + "; cat"
	inc, attrs := filepath.Join(tmp, "inc"), filepath.Join(tmp, "attrs")
	must(t, os.WriteFile(inc, []byte("[filter \"evil2\"]\n\tclean = "+touch+"\n\tsmudge = "+touch+"\n"), 0o600))
	must(t, os.WriteFile(attrs, []byte("* filter=evil2\n"), 0o600))
	for _, kv := range [][2]string{
		{"core.fsmonitor", touch}, {"core.hooksPath", ".githooks"}, {"filter.evil.clean", touch}, {"filter.evil.smudge", touch},
		{"diff.evil.command", touch}, {"diff.evil.textconv", touch}, {"merge.evil.driver", touch}, {"include.path", inc},
		{"core.attributesFile", attrs}, {"gpg.program", touch}, {"commit.gpgSign", "true"}, {"core.editor", touch},
		{"remote.origin.uploadpack", touch}, {"core.pager", touch},
	} {
		gitOK(t, exec.Command("git", "-C", h.ws, "config", kv[0], kv[1]))
	}
	evil := []byte("* filter=evil diff=evil merge=evil\n")
	must(t, os.WriteFile(filepath.Join(h.ws, ".git", "info", "attributes"), evil, 0o600))
	must(t, os.WriteFile(filepath.Join(h.ws, ".gitattributes"), evil, 0o600))
	must(t, os.MkdirAll(filepath.Join(h.ws, ".githooks"), 0o700))
	for _, hook := range []string{"post-checkout", "pre-commit", "post-merge"} {
		must(t, os.WriteFile(filepath.Join(h.ws, ".githooks", hook), []byte("#!/bin/sh\ntouch "+h.canary+"\n"), 0o700))
	}
	must(t, os.WriteFile(filepath.Join(h.ws, "a.txt"), []byte("b\n"), 0o600))
	return h
}

// The controller's git runs none of a run's commands: it never uses the
// .git the run wrote, and its own repository carries none.
func TestGitIsHardened(t *testing.T) {
	h := hostile(t)
	ctx := context.Background()
	repo, err := Clone(ctx, h.src, "refs/heads/main", filepath.Join(t.TempDir(), "ctl"))
	must(t, err)
	tree := filepath.Join(t.TempDir(), "tree")
	must(t, h.a.CopyTree(tree, 1<<20))
	id := []string{"-c", "user.name=t", "-c", "user.email=t@t"}
	for _, args := range [][]string{
		{"status", "--porcelain"}, {"add", "--all"}, {"diff", "--cached"}, append(id, "commit", "--quiet", "-m", "c"),
		{"diff", "HEAD~1"}, {"log", "-p"}, {"checkout", "--quiet", "HEAD~1", "--", "a.txt"}, {"fetch", "--quiet", h.src, "main"},
		append(id, "merge", "--no-edit", "FETCH_HEAD"), {"show", "HEAD"},
	} {
		cmd, err := repo.Git(ctx, tree, args...)
		must(t, err)
		gitOK(t, cmd)
	}
	if _, err := os.Stat(h.canary); !os.IsNotExist(err) {
		t.Fatalf("a run's command ran under the controller's git: %v", err)
	}
	// Belt and braces: the controller's own repository, changed, is refused.
	for _, f := range []string{"config", "info/attributes"} {
		p := filepath.Join(repo.Dir, ".git", f)
		before, _ := os.ReadFile(p)
		must(t, os.WriteFile(p, append(before, "\n[core]\n\tfsmonitor = x\n"...), 0o600))
		if _, err := repo.Git(ctx, tree, "status"); !errors.Is(err, ErrGit) {
			t.Errorf("a changed %s: %v, want ErrGit", f, err)
		}
		must(t, os.WriteFile(p, before, 0o600))
	}
}

// The discriminating red for TestGitIsHardened: plain git on the run's
// workspace runs the planted commands.
func TestPlainGitRunsHostileCommands(t *testing.T) {
	h := hostile(t)
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = h.ws
	gitOK(t, cmd)
	if _, err := os.Stat(h.canary); err != nil {
		t.Fatalf("plain git did not run the planted commands, so the hardened test proves nothing: %v", err)
	}
}

func TestCloneRefusesOddArguments(t *testing.T) {
	for _, c := range [][2]string{{"/src", "main"}, {"/src", "refs/heads/"}, {"--upload-pack=x", "refs/heads/main"}} {
		if _, err := Clone(context.Background(), c[0], c[1], t.TempDir()+"/d"); err != ErrSpec {
			t.Errorf("Clone(%q, %q) = %v, want ErrSpec", c[0], c[1], err)
		}
	}
}
