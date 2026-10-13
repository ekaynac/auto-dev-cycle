// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func listDir(dir string) ([]string, error) {
	es, err := os.ReadDir(dir)
	names := []string{}
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names, err
}

func gitOK(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%q: %v: %s", cmd.Args, err, out)
	}
}

// hostile is a controller-side clone whose .git and tree carry every
// command hook of the hostile-git-dir case, each touching canary.
func hostile(t *testing.T) (dir, canary string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	src, dir, canary := filepath.Join(tmp, "src"), filepath.Join(tmp, "copy"), filepath.Join(tmp, "canary")
	gitOK(t, Git(ctx, tmp, "init", "--quiet", "--initial-branch=main", src))
	must(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("a\n"), 0o600))
	gitOK(t, Git(ctx, src, "add", "a.txt"))
	gitOK(t, Git(ctx, src, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--quiet", "-m", "base"))
	must(t, Clone(ctx, src, "refs/heads/main", dir))
	touch := "touch " + canary + "; cat"
	for _, kv := range [][2]string{{"core.fsmonitor", touch}, {"core.hooksPath", ".githooks"},
		{"filter.evil.clean", touch}, {"filter.evil.smudge", touch}, {"diff.evil.textconv", touch}} {
		gitOK(t, exec.Command("git", "-C", dir, "config", kv[0], kv[1]))
	}
	must(t, os.MkdirAll(filepath.Join(dir, ".githooks"), 0o700))
	must(t, os.WriteFile(filepath.Join(dir, ".githooks", "post-checkout"), []byte("#!/bin/sh\ntouch "+canary+"\n"), 0o700))
	must(t, os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("* filter=evil diff=evil\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("b\n"), 0o600))
	return dir, canary
}

// The controller's git runs none of a repository's commands.
func TestGitIsHardened(t *testing.T) {
	dir, canary := hostile(t)
	ctx := context.Background()
	gitOK(t, Git(ctx, dir, "status", "--porcelain"))
	gitOK(t, Git(ctx, dir, "add", "--all"))
	gitOK(t, Git(ctx, dir, "diff", "--cached", "--no-ext-diff", "--no-textconv"))
	gitOK(t, Git(ctx, dir, "checkout", "--quiet", "HEAD", "--", "a.txt"))
	if _, err := os.Stat(canary); !os.IsNotExist(err) {
		t.Fatalf("a repository command ran under the hardened git: %v", err)
	}
}

// The discriminating red for TestGitIsHardened: plain git on the same
// copy runs the planted commands.
func TestPlainGitRunsHostileCommands(t *testing.T) {
	dir, canary := hostile(t)
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = dir
	gitOK(t, cmd)
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("plain git did not run the planted fsmonitor or filter, so the hardened test proves nothing: %v", err)
	}
}

func TestCloneRefusesOddArguments(t *testing.T) {
	for _, c := range [][2]string{{"/src", "main"}, {"/src", "refs/heads/"}, {"--upload-pack=x", "refs/heads/main"}} {
		if err := Clone(context.Background(), c[0], c[1], t.TempDir()+"/d"); err != ErrSpec {
			t.Errorf("Clone(%q, %q) = %v, want ErrSpec", c[0], c[1], err)
		}
	}
}
