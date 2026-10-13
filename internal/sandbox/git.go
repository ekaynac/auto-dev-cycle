// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// The controller's git (00 §2.6 hostile-git-dir). A run can write its
// workspace's .git, and a git configuration can name a command under keys
// no denylist covers (filter.*, diff.*, merge.*, include.path, ...). So
// the controller never runs git with a .git a run wrote: it copies the
// workspace's tree without any .git (CopyTree) and runs git with GIT_DIR
// set to a repository it cloned itself (Repo), whose config and info/ are
// checked against what the clone wrote before every command. The settings
// below are belt and braces.
var hardened = []string{
	"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=", "-c", "core.untrackedCache=false",
	"-c", "core.attributesFile=/dev/null", "-c", "submodule.recurse=false", "-c", "credential.helper=",
	"-c", "protocol.ext.allow=never", "-c", "core.sshCommand=false", "-c", "core.askPass=",
	"-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false", "-c", "gpg.program=false",
	"-c", "core.editor=false", "-c", "sequence.editor=false", "-c", "core.pager=cat",
}

// ErrGit: git is unusable, or the controller's repository was changed.
var ErrGit = errors.New("sandbox: controller git refused")

// git is a hardened git in dir with exactly env added: no system or global
// configuration, no environment of the controller's, and attributes from
// the empty tree, so no in-tree .gitattributes selects a filter or driver.
func git(ctx context.Context, dir string, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append(slices.Clone(hardened), args...)...)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_ATTR_SOURCE=4b825dc642cb6eb9a060e54bf8d69288fbee4904", "GIT_EDITOR=false", "GIT_PAGER=cat",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1"}, env...)
	return cmd
}

// Repo is a repository the controller cloned; its .git is never mounted.
type Repo struct {
	Dir     string
	snap    string
	refused func(url string) bool
}

// refused: url is a local repository a run may have written (one that
// overlaps the sandbox root, where every run's workspace lies, or a
// forbidden path), or a relative or scp-like one this cannot judge.
func (h Host) refused(url string) bool {
	p, isFile := strings.CutPrefix(url, "file://")
	switch {
	case isFile || filepath.IsAbs(url):
		p = resolve(filepath.Clean(p))
		return slices.ContainsFunc(append([]string{h.Root}, h.Forbidden...), func(f string) bool { return overlaps(p, resolve(f)) })
	case strings.Contains(url, "://"):
		return false
	}
	return true
}

// Clone makes a fresh controller-side clone of url's branch (a
// refs/heads/ name) at dst, without a checkout: one branch, no
// submodules, never sharing files with a local source, and never from a
// repository a run may have written.
func (h Host) Clone(ctx context.Context, url, ref, dst string) (Repo, error) {
	branch, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok || branch == "" || strings.HasPrefix(url, "-") || h.refused(url) {
		return Repo{}, ErrSpec
	}
	if out, err := git(ctx, "/", nil, "clone", "--quiet", "--no-local", "--no-checkout", "--no-recurse-submodules",
		"--single-branch", "--branch", branch, "--", url, dst).CombinedOutput(); err != nil {
		return Repo{}, fmt.Errorf("%w: clone: %w: %s", ErrGit, err, strings.TrimSpace(string(out)))
	}
	r := Repo{Dir: dst, refused: h.refused}
	var err error
	r.snap, err = r.snapshot()
	return r, err
}

// snapshot digests what can name a command or another repository: the
// config files, commondir, alternates and info/.
func (r Repo) snapshot() (string, error) {
	gitDir := filepath.Join(r.Dir, ".git")
	files := []string{"config", "config.worktree", "commondir", "objects/info/alternates"}
	info, err := os.ReadDir(filepath.Join(gitDir, "info"))
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	for _, e := range info {
		files = append(files, "info/"+e.Name())
	}
	h := sha256.New()
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(gitDir, f))
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		fmt.Fprintf(h, "%s %t %d\n%s", f, err == nil, len(b), b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Git is a hardened git on the controller's repository with tree (a
// CopyTree copy) as its work tree, refused if the repository's config or
// info/ changed since Clone. diff, show and log never run a diff driver;
// fetch and pull never reach a repository a run may have written (a
// remote's name is taken as is: its URL was Clone's).
func (r Repo) Git(ctx context.Context, tree string, args ...string) (*exec.Cmd, error) {
	if snap, err := r.snapshot(); err != nil || r.snap == "" || snap != r.snap || r.refused == nil {
		return nil, fmt.Errorf("%w: the controller repository's configuration changed", ErrGit)
	}
	if len(args) > 0 && (args[0] == "fetch" || args[0] == "pull") {
		i := slices.IndexFunc(args[1:], func(a string) bool { return !strings.HasPrefix(a, "-") })
		if i >= 0 && strings.ContainsAny(args[i+1], "/:") && r.refused(args[i+1]) {
			return nil, fmt.Errorf("%w: fetch from a repository a run may have written", ErrSpec)
		}
	}
	if len(args) > 0 && slices.Contains([]string{"diff", "show", "log"}, args[0]) {
		args = append([]string{args[0], "--no-ext-diff", "--no-textconv"}, args[1:]...)
	}
	return git(ctx, tree, []string{"GIT_DIR=" + filepath.Join(r.Dir, ".git"), "GIT_WORK_TREE=" + tree,
		"GIT_CEILING_DIRECTORIES=" + filepath.Dir(tree)}, args...), nil
}
