// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// ErrUnsafeTree: the output tree holds a git directory or a look-alike of
// one, or a path git refuses to add. The caller records the build
// failed/unsafe_output: it is the candidate's fault, never the
// controller's (so no hold, no page).
var ErrUnsafeTree = errors.New("accept: the output tree is unsafe")

// maxIgnored bounds the ignored output files Diff reports.
const maxIgnored = 100

// Diff is the build's change (13 "Facts, not claims"): the controller's
// own diff, in git's binary patch form, of tree (a sandbox output tree
// copied without any .git, sandbox.Attempt.CopyTree) against base. It is
// empty when nothing changed, and is what graph.BuildFacts.Diff digests.
// The adapter's own patch is a claim and is never read for it. ignored
// are output files .gitignore kept out of the diff (at most maxIgnored),
// for In.Ignored. Diff holds the whole diff in memory: its size is bounded
// by the tree's, which CopyTree's maxBytes bounds; over maxPatch it fails.
func Diff(ctx context.Context, repo sandbox.Repo, base, tree string) (patch []byte, ignored []string, err error) {
	// Before git reads the tree: a .git look-alike (case, NTFS and HFS+
	// forms) that CopyTree's exact ".git" left in.
	err = fs.WalkDir(os.DirFS(tree), ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && p != "." && dotGit(d.Name()) {
			err = fmt.Errorf("%w: %q is a git directory", ErrUnsafeTree, p)
		}
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	g, cleanup, err := newIndex(ctx, repo, base)
	if err != nil {
		return nil, nil, err
	}
	defer cleanup()
	g.tree = tree
	if _, err := g.run(ctx, nil, "add", "--all", "--", "."); errors.Is(err, errFailed) {
		return nil, nil, fmt.Errorf("%w: git refuses to add it", ErrUnsafeTree)
	} else if err != nil {
		return nil, nil, err
	}
	others, err := g.run(ctx, nil, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory")
	if err != nil {
		return nil, nil, err
	}
	for _, p := range strings.Split(string(others), "\x00") {
		if p != "" && len(ignored) < maxIgnored {
			ignored = append(ignored, p)
		}
	}
	patch, err = g.run(ctx, nil, "diff", "--cached", "--binary", "--full-index", "--no-renames", base, "--")
	if err == nil && len(patch) > maxPatch {
		return nil, nil, fmt.Errorf("accept: the build's diff is over %d bytes", maxPatch)
	}
	return patch, ignored, err
}
