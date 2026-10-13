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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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

var (
	// minGit is the first git with GIT_ATTR_SOURCE (2.41), which an older
	// one ignores silently. It is that feature's floor, not a security floor.
	minGit     = [2]int{2, 41}
	gitVersion = regexp.MustCompile(`^git version (\d+)\.(\d+)`)
	// gitOnce checks once per process: a transient failure (git missing
	// for a moment) is cached for the life of the process.
	gitOnce    = sync.OnceValue(checkGit)
	remoteName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

func checkGit() error {
	out, err := git(context.Background(), "/", nil, "version").Output()
	if err != nil {
		return fmt.Errorf("%w: git version unreadable", ErrGit)
	}
	return checkVersion(out)
}

// checkVersion refuses a `git version` line below minGit.
func checkVersion(out []byte) error {
	m := gitVersion.FindSubmatch(out)
	if m == nil {
		return fmt.Errorf("%w: git version unreadable", ErrGit)
	}
	major, _ := strconv.Atoi(string(m[1]))
	minor, _ := strconv.Atoi(string(m[2]))
	if major < minGit[0] || major == minGit[0] && minor < minGit[1] {
		return fmt.Errorf("%w: git %d.%d is older than %d.%d", ErrGit, major, minor, minGit[0], minGit[1])
	}
	return nil
}

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

// subcommands are what Repo.Git runs.
var subcommands = []string{"status", "add", "rm", "diff", "show", "log", "commit", "checkout", "merge", "fetch", "pull",
	"rev-parse", "ls-files", "cat-file", "read-tree", "write-tree", "update-index", "apply", "hash-object",
	"for-each-ref", "symbolic-ref", "update-ref"}

// Repo is a repository the controller cloned; its .git is never mounted.
// A controller commit identity (user.name, user.email) must be set in it
// before its snapshot is taken (P14): a later config change is refused.
type Repo struct {
	Dir     string
	snap    string
	refused func(url string) bool
}

// refused: url is a local repository a run may have written (one that
// overlaps the sandbox root, where every run's workspace lies, or a
// forbidden path), or a relative or scp-like one this cannot judge.
// Only https and ssh URLs (the scheme case-folded) and absolute paths
// pass at all. file:// URLs are refused: git percent-decodes their path
// (%2e%2e, %73), which no check of the string sees; an absolute path
// names a local repository instead.
func (h Host) refused(url string) bool {
	scheme, _, ok := strings.Cut(url, "://")
	switch scheme = strings.ToLower(scheme); {
	case ok:
		return scheme != "https" && scheme != "ssh"
	case !filepath.IsAbs(url) || filepath.Clean(url) != url || resolve(url) != url:
		return true // only an absolute, canonical path: no "..", ".", or symlink
	}
	// Every place git would open: p, p/.git, p.git, p.git/.git, and what a
	// gitfile among them (gitdir:), a commondir or an alternates file
	// points to, each resolved.
	inside := func(q string) bool {
		if q == "" {
			return true
		}
		q = resolve(q)
		return slices.ContainsFunc(append([]string{h.Root}, h.Forbidden...), func(f string) bool { return overlaps(q, resolve(f)) })
	}
	for _, c := range []string{url, url + "/.git", url + ".git", url + ".git/.git"} {
		if inside(c) {
			return true
		}
		dirs := append([]string{c}, pointers(c, filepath.Dir(c), "gitdir: ")...)
		for _, d := range dirs {
			targets := append(pointers(filepath.Join(d, "commondir"), d, ""),
				pointers(filepath.Join(d, "objects/info/alternates"), filepath.Join(d, "objects"), "")...)
			if slices.ContainsFunc(append(targets, d), inside) {
				return true
			}
		}
	}
	return false
}

// pointers are the lines of the regular file p that start with prefix,
// stripped of it, each taken from base when relative. An unreadable or
// oversized pointer file is a refusal: it names "" (inside everything).
func pointers(p, base, prefix string) []string {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil || len(b) > 64<<10 {
		return []string{""}
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if t, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok && t != "" {
			if !filepath.IsAbs(t) {
				t = filepath.Join(base, t)
			}
			out = append(out, t)
		}
	}
	return out
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
	if err := gitOnce(); err != nil {
		return Repo{}, err
	}
	if out, err := git(ctx, "/", nil, "clone", "--quiet", "--no-local", "--no-checkout", "--no-recurse-submodules",
		"--single-branch", "--branch", branch, "--", url, dst).CombinedOutput(); err != nil {
		return Repo{}, fmt.Errorf("%w: clone: %w: %s", ErrGit, err, strings.TrimSpace(string(out)))
	}
	// GIT_ATTR_SOURCE names SHA-1's empty tree: a SHA-256 repository is refused.
	if out, err := git(ctx, dst, nil, "rev-parse", "--show-object-format").Output(); err != nil || string(out) != "sha1\n" {
		return Repo{}, fmt.Errorf("%w: the repository's object format is not sha1", ErrGit)
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
	// Only allowlisted subcommands, never a global option first (-c,
	// --no-pager, … could hide the subcommand): ls-remote, archive
	// --remote, clone, submodule and remote reach other repositories.
	if len(args) == 0 || !slices.Contains(subcommands, args[0]) {
		return nil, fmt.Errorf("%w: a subcommand the controller's git does not run", ErrSpec)
	}
	if len(args) > 1 && (args[0] == "fetch" || args[0] == "pull") {
		// No option at all (-u, --upload-pack, --exec name commands), and a
		// repository that is a configured remote's name or one refused passes.
		repo := args[1]
		named := remoteName.MatchString(repo) && git(ctx, r.Dir, nil, "config", "--get", "remote."+repo+".url").Run() == nil
		if slices.ContainsFunc(args[1:], func(a string) bool { return strings.HasPrefix(a, "-") }) || !named && r.refused(repo) {
			return nil, fmt.Errorf("%w: fetch with an option, or from a repository a run may have written", ErrSpec)
		}
	}
	if len(args) > 0 && slices.Contains([]string{"diff", "show", "log"}, args[0]) {
		args = append([]string{args[0], "--no-ext-diff", "--no-textconv"}, args[1:]...)
	}
	return git(ctx, tree, []string{"GIT_DIR=" + filepath.Join(r.Dir, ".git"), "GIT_WORK_TREE=" + tree,
		"GIT_CEILING_DIRECTORIES=" + filepath.Dir(tree)}, args...), nil
}
