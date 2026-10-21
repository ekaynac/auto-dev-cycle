// SPDX-License-Identifier: Apache-2.0

package daemon

// An attempt's workspace (P22a-5c-w; W15): the project's repository at the
// task's base, made by the controller before the attempt starts.
//   - The controller clones the adopted profile's repo.url at repo.base_ref
//     afresh (sandbox.Host.Clone: an https or ssh URL, or an absolute local
//     path no run may have written; one branch; no checkout; never sharing
//     files with a local source). Its .git stays in the controller's state
//     directory and is never mounted.
//   - The clone's origin must name the profile's URL, else the workspace is
//     refused (fail closed): a clone of anything else never becomes a base.
//   - The base is the branch's commit's tree (AttemptBase.Tree), and that
//     commit is checked out, by the controller's hardened git, into a fresh
//     sandbox run directory's workspace: the adapter's workspace path.
//   - Close removes the run directory and the clone, whatever happened.
//   - Inert until launch is active: 22a-5c's attempt run is its caller.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// ErrRemoteMismatch: the clone's origin is not the profile's repository.
var ErrRemoteMismatch = errors.New("daemon: the clone's origin is not the profile's repository; no workspace is made")

// treeID is a sha1 tree id, as rev-parse prints it.
var treeID = regexp.MustCompile(`^[0-9a-f]{40}$`)

// workspace is an attempt's base, cloned and checked out.
type workspace struct {
	run   *sandbox.Attempt
	clone string
	Base  types.GitID // the base commit's tree
}

// Path is the adapter's workspace: the checked-out tree.
func (w *workspace) Path() string { return w.run.Workspace() }

// Close removes the run directory and the clone.
func (w *workspace) Close() error {
	return errors.Join(w.run.Teardown(), os.RemoveAll(w.clone))
}

// openWorkspace makes attempt's workspace from p's repository, cloned
// under clones (the controller's own directory, outside the sandbox root).
func openWorkspace(ctx context.Context, host sandbox.Host, clones string, p types.ProjectProfile, attempt types.AttemptID) (_ *workspace, err error) {
	if err := os.MkdirAll(clones, 0o700); err != nil {
		return nil, err
	}
	dst := filepath.Join(clones, string(attempt))
	repo, err := host.Clone(ctx, p.Repo.Url, p.Repo.BaseRef, dst)
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
	w := &workspace{run: run, clone: dst}
	defer func() {
		if err != nil {
			err = errors.Join(err, run.Teardown())
		}
	}()
	rev, err := repo.Git(ctx, w.Path(), "rev-parse", "--verify", "--end-of-options", p.Repo.BaseRef+"^{tree}")
	if err != nil {
		return nil, err
	}
	out, err := rev.Output()
	tree := strings.TrimSpace(string(out))
	if err != nil || !treeID.MatchString(tree) {
		return nil, fmt.Errorf("%w: the base %s has no tree", sandbox.ErrGit, p.Repo.BaseRef)
	}
	w.Base = types.GitID("git:" + tree)
	co, err := repo.Git(ctx, w.Path(), "checkout", "--quiet", "--force", p.Repo.BaseRef, "--", ".")
	if err != nil {
		return nil, err
	}
	if out, err := co.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%w: checking out the base: %w: %s", sandbox.ErrGit, err, strings.TrimSpace(string(out)))
	}
	return w, nil
}
