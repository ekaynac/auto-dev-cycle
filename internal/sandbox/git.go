// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"os/exec"
	"strings"
)

// hardened are the settings every controller `git` runs with, whatever a
// repository's own files say (the hostile-git-dir case, 00 §2.6): no
// hooks, no fsmonitor, no external diff, no submodule recursion, no
// credential helper, and no transport that runs a command.
var hardened = []string{
	"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=", "-c", "core.untrackedCache=false",
	"-c", "diff.external=", "-c", "submodule.recurse=false", "-c", "credential.helper=",
	"-c", "protocol.ext.allow=never", "-c", "core.sshCommand=false", "-c", "core.askPass=",
}

// Git is a controller `git` in dir, which must be a controller-side copy:
// the controller never runs git on a .git directory a run could write. It
// reads no system or global configuration and no environment of the
// controller's. Attributes come from the empty tree, so no in-tree
// .gitattributes selects a filter, diff driver or textconv; a diff also
// passes --no-ext-diff and --no-textconv.
func Git(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append(append([]string{}, hardened...), args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_ATTR_SOURCE=4b825dc642cb6eb9a060e54bf8d69288fbee4904",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1"}
	return cmd
}

// Clone makes a fresh controller-side clone of url's branch (a
// refs/heads/ name) at dst: one branch, no submodules, no hardlinks into
// the source.
func Clone(ctx context.Context, url, ref, dst string) error {
	branch, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok || branch == "" || strings.HasPrefix(url, "-") {
		return ErrSpec
	}
	return Git(ctx, "/", "clone", "--quiet", "--no-hardlinks", "--no-recurse-submodules", "--single-branch",
		"--branch", branch, "--", url, dst).Run()
}
