// SPDX-License-Identifier: Apache-2.0

package daemon

// An attempt's workspace (P22a-5c-w; W15): the project's repository at the
// task's base, made by the controller before the attempt starts.
//   - The controller clones the adopted profile's repo.url at repo.base_ref
//     afresh (sandbox.Host.Clone: https, or an absolute local path the
//     controller owns and no run may have written; one branch; no checkout;
//     every object checked, so a tree holding .git, .GIT, git~1 or a
//     duplicate entry refuses the clone). Its .git stays in the
//     controller's state directory and is never mounted.
//   - The clone's origin must name the profile's URL, else the workspace is
//     refused (fail closed): a clone of anything else never becomes a base.
//   - The base is a tree id: the pinned one when given (the unit's first
//     AttemptBase.Tree, so a retry or a supersession always gets its
//     predecessor's base; Opus #326 M4), which the fresh clone must hold,
//     else the branch's commit's tree. That id, never the branch name, is
//     checked out, by the controller's hardened git, into a fresh sandbox run
//     directory's workspace: the adapter's workspace path. An empty tree
//     checks out nothing.
//   - The workspace must equal the base: the index the checkout wrote is
//     the base tree, and no file of it is missing from the workspace (a
//     path or a symlink a checkout drops). Otherwise it is refused, never
//     silently short (Opus #326 M2).
//   - Close removes the run directory and the clone, whatever happened. A
//     crash between the two leaves both behind: 22a-5c sweeps them at
//     restart, before launch is active.
//   - 22a-5c's attempt run is its caller, while launch is active (this
//     build, P22a).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// ErrRemoteMismatch: the clone's origin is not the profile's repository.
var ErrRemoteMismatch = errors.New("daemon: the clone's origin is not the profile's repository; no workspace is made")

// ErrBaseMismatch: the workspace is not the base: the pinned tree is not in
// the clone, or the checkout did not write the whole tree.
var ErrBaseMismatch = errors.New("daemon: the workspace would not be the base tree; no workspace is made")

// treeID is a sha1 tree id, as rev-parse prints it; emptyTree is the
// empty tree's.
var treeID = regexp.MustCompile(`^[0-9a-f]{40}$`)

const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// cloneRepo is sandbox.Host.Clone; a test returns another clone.
var cloneRepo = sandbox.Host.Clone

// workspace is an attempt's base, cloned and checked out.
type workspace struct {
	run   *sandbox.Attempt
	clone string
	repo  sandbox.Repo // the clone: what the build's diff is taken against
	Base  types.GitID  // the base tree, checked out

	attempt types.AttemptID
}

// keptName names the file a kept run directory holds: its attempt's id,
// the start sweep's handle on it (sweep.go).
const keptName = "kept-attempt"

// Path is the adapter's workspace: the checked-out tree.
func (w *workspace) Path() string { return w.run.Workspace() }

// Close removes the run directory and the clone.
func (w *workspace) Close() error {
	return errors.Join(w.run.Teardown(), os.RemoveAll(w.clone))
}

// Keep removes the clone and keeps the run directory, naming its attempt
// in it (keptName): a session whose end is unverified may still use it
// (run.go), and the start sweep tears it down once its container is
// removed (sweep.go).
func (w *workspace) Keep() error {
	return errors.Join(os.WriteFile(filepath.Join(w.run.Dir, keptName), []byte(w.attempt), 0o600), os.RemoveAll(w.clone))
}

// emptyWorkspace is a reviewer attempt's workspace (P22a-6c): a fresh,
// empty run directory, its base the empty tree, no clone (a reviewer has no
// tool to read one), mounted read-only and never hashed for the image's
// lockfiles (AdapterRun.Review; Mythos chunk 4a H1, L1).
func emptyWorkspace(host sandbox.Host, attempt types.AttemptID) (*workspace, error) {
	if err := ids.Check(string(attempt), "att"); err != nil {
		return nil, err
	}
	run, err := host.NewAttempt()
	if err != nil {
		return nil, err
	}
	return &workspace{run: run, Base: types.GitID("git:" + emptyTree), attempt: attempt}, nil
}

// openWorkspace makes attempt's workspace from p's repository, cloned
// under clones (the controller's own directory, outside the sandbox root),
// at pin when it is set.
func openWorkspace(ctx context.Context, host sandbox.Host, clones string, p types.ProjectProfile, attempt types.AttemptID, pin *types.GitID) (_ *workspace, err error) {
	if err := ids.Check(string(attempt), "att"); err != nil { // it names a directory (Opus #326 L5)
		return nil, err
	}
	if err := os.MkdirAll(clones, 0o700); err != nil {
		return nil, err
	}
	dst := filepath.Join(clones, string(attempt))
	repo, err := cloneRepo(host, ctx, p.Repo.Url, p.Repo.BaseRef, dst)
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(dst))
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(dst))
		}
	}()
	if origin, err := repo.Origin(ctx); err != nil || origin != p.Repo.Url {
		return nil, errors.Join(ErrRemoteMismatch, err)
	}
	run, err := host.NewAttempt()
	if err != nil {
		return nil, err
	}
	w := &workspace{run: run, clone: dst, repo: repo, attempt: attempt}
	defer func() {
		if err != nil {
			err = errors.Join(err, run.Teardown())
		}
	}()
	git := func(args ...string) (string, error) {
		cmd, err := repo.Git(ctx, w.Path(), args...)
		if err != nil {
			return "", err
		}
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	want := p.Repo.BaseRef + "^{tree}"
	if pin != nil {
		want = strings.TrimPrefix(string(*pin), "git:") + "^{tree}"
	}
	tree, err := git("rev-parse", "--verify", "--quiet", "--end-of-options", want)
	if err != nil || !treeID.MatchString(tree) || pin != nil && "git:"+tree != string(*pin) {
		return nil, fmt.Errorf("%w: the base %s is not a tree of the clone", ErrBaseMismatch, want)
	}
	w.Base = types.GitID("git:" + tree)
	if tree != emptyTree { // an empty base checks out nothing (Opus #326 L4)
		if _, err := git("checkout", "--quiet", "--force", tree, "--", "."); err != nil {
			return nil, fmt.Errorf("%w: checking out the base: %w", sandbox.ErrGit, err)
		}
	}
	// The workspace is the base: the index is the tree, and nothing of it
	// is missing from the work tree.
	index, err := git("write-tree")
	if err != nil || index != tree {
		return nil, fmt.Errorf("%w: the checkout's index is %s, not %s", ErrBaseMismatch, index, tree)
	}
	missing, err := git("ls-files", "--deleted", "-z")
	if err != nil || missing != "" {
		return nil, fmt.Errorf("%w: files of the base are missing from the workspace", ErrBaseMismatch)
	}
	return w, nil
}
