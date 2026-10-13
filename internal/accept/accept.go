// SPDX-License-Identifier: Apache-2.0

// Package accept is P14 (docs/design/m1/00 §5.2): the build's controller
// diff, the accept boundary and the builder's minimal manifest.intent.
// Accept applies a candidate patch to a temporary index of the base in the
// controller's own repository (sandbox.Repo: hardened git, never a .git a
// run wrote) and takes the file list from git (01 §2.5 "Accept"). It
// refuses, failed: unsafe_output (a patch git will not apply, a path that
// is not plain, a symlink, a gitlink); protected_path; and
// test_integrity_violation (the guard's conclusive findings). Warnings go
// to the reviewer; each verdict keeps its elapsed time and blocks (D55).
package accept

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Bounds: adapterhost's patch bound; the guard reads at most maxRead bytes
// in all, maxFile per file (a larger one is left to the reviewer).
const (
	maxPatch = 64 << 20
	maxRead  = 64 << 20
	maxFile  = 4 << 20
)

var objectID = regexp.MustCompile(`^[0-9a-f]{40}$`)

// In is one accept.
type In struct {
	Repo    sandbox.Repo
	Base    string           // the base commit, a full sha1 id
	Patch   []byte           // the build's diff
	Policy  Policy           // PolicyOf's
	Ignored []string         // Diff's ignored output files: warnings
	Now     func() time.Time // nil: time.Now
}

// Block is one finding: a block refusing the candidate, or a warning.
type Block struct {
	Reason vocab.Reason `json:"reason"`
	Path   string       `json:"path"`
	What   string       `json:"what"`
}

// Verdict is an accept's outcome; accepted, Tree is the accepted tree.
type Verdict struct {
	Blocks     []Block       `json:"blocks"`
	Warnings   []Block       `json:"warnings"`
	Files      int           `json:"files"`
	Tree       string        `json:"tree,omitempty"`
	TreeDigest types.Digest  `json:"tree_digest,omitempty"`
	Elapsed    time.Duration `json:"elapsed_ns"`
}

// Accepted reports whether nothing blocks the candidate.
func (v Verdict) Accepted() bool { return len(v.Blocks) == 0 }

// Reason is the verdict's failed reason: unsafe output outranks a
// protected path, which outranks a test integrity violation.
func (v Verdict) Reason() (r vocab.Reason) {
	rank := map[vocab.Reason]int{vocab.ReasonTestIntegrityViolation: 1, vocab.ReasonProtectedPath: 2, vocab.ReasonUnsafeOutput: 3}
	for _, b := range v.Blocks {
		if rank[b.Reason] > rank[r] {
			r = b.Reason
		}
	}
	return r
}

// change is one entry of git's raw diff of the candidate against the base.
type change struct {
	srcMode, dstMode, srcBlob, dstBlob, status string
	src, dst                                   string // dst is src unless renamed or copied
}

// Accept checks in's patch. An error is the controller's own fault (git
// unusable, the repository changed); a candidate's fault is a verdict.
func Accept(ctx context.Context, in In) (v Verdict, err error) {
	if in.Now == nil {
		in.Now = time.Now
	}
	start := in.Now()
	defer func() { v.Elapsed = in.Now().Sub(start) }()
	if !in.Policy.ok {
		return v, errors.New("accept: a policy PolicyOf did not build")
	}
	for _, p := range in.Ignored {
		v.Warnings = append(v.Warnings, Block{vocab.ReasonCompletedWithFindings, p, "output .gitignore kept out of the diff"})
	}
	if len(in.Patch) > maxPatch {
		v.Blocks = append(v.Blocks, Block{vocab.ReasonUnsafeOutput, "", fmt.Sprintf("the patch is over %d bytes", maxPatch)})
		return v, nil
	}
	g, cleanup, err := newIndex(ctx, in.Repo, in.Base)
	if err != nil {
		return v, err
	}
	defer cleanup()
	if len(in.Patch) > 0 {
		if _, err := g.run(ctx, in.Patch, "apply", "--cached", "--whitespace=nowarn", "-"); err != nil {
			if !errors.Is(err, errFailed) {
				return v, err
			}
			v.Blocks = append(v.Blocks, Block{vocab.ReasonUnsafeOutput, "", "git does not apply the patch to the base: a path git refuses (.., .git, absolute) or a hunk that does not apply"})
			return v, nil
		}
	}
	raw, err := g.run(ctx, nil, "diff", "--cached", "--raw", "-z", "--no-abbrev", "-M", in.Base, "--")
	if err != nil {
		return v, err
	}
	changes, err := parseRaw(raw)
	if err != nil {
		return v, err
	}
	v.Files = len(changes)
	for _, c := range changes {
		v.Blocks = append(v.Blocks, in.Policy.check(c)...)
	}
	blocks, warnings, err := guard(ctx, g, changes)
	if err != nil {
		return v, err
	}
	v.Blocks, v.Warnings = append(v.Blocks, blocks...), append(v.Warnings, warnings...)
	if !v.Accepted() {
		return v, nil
	}
	tree, err := g.run(ctx, nil, "write-tree")
	if err != nil {
		return v, err
	}
	v.Tree = strings.TrimSpace(string(tree))
	obj, err := g.run(ctx, nil, "cat-file", "tree", v.Tree)
	s := sha256.Sum256(obj) // the tree object's bytes
	v.TreeDigest = types.Digest("sha256:" + hex.EncodeToString(s[:]))
	return v, err
}

// check is the path and mode rules for one change: both its paths.
func (pol Policy) check(c change) []Block {
	paths := []string{c.src}
	if c.dst != c.src {
		paths = append(paths, c.dst)
	}
	var out []Block
	for _, p := range paths {
		switch {
		case unsafePath(p):
			return []Block{{vocab.ReasonUnsafeOutput, p, "not one plain repository path"}}
		case pol.Protected(p):
			out = append(out, Block{vocab.ReasonProtectedPath, p, "a protected path"})
		}
	}
	switch {
	case c.dstMode == "120000":
		return []Block{{vocab.ReasonUnsafeOutput, c.dst, "a symlink"}}
	case c.srcMode == "160000" || c.dstMode == "160000":
		return []Block{{vocab.ReasonUnsafeOutput, c.dst, "a gitlink (submodule)"}}
	}
	return out
}

// parseRaw reads `git diff --raw -z`: ":srcmode dstmode src dst status",
// then the path, and a second path for a rename or a copy.
func parseRaw(raw []byte) ([]change, error) {
	fields := strings.Split(string(raw), "\x00")
	var out []change
	for i := 0; i < len(fields) && fields[i] != ""; {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) != 5 || i+1 >= len(fields) {
			return nil, fmt.Errorf("accept: unreadable raw diff entry %q", fields[i])
		}
		c := change{srcMode: meta[0], dstMode: meta[1], srcBlob: meta[2], dstBlob: meta[3], status: meta[4][:1], src: fields[i+1]}
		c.dst, i = c.src, i+2
		if c.status == "R" || c.status == "C" {
			if i >= len(fields) {
				return nil, fmt.Errorf("accept: a rename without its new path")
			}
			c.dst, i = fields[i], i+1
		}
		out = append(out, c)
	}
	return out, nil
}

// index is a temporary index of a base in the controller's repository.
type index struct {
	repo       sandbox.Repo
	tree, file string
}

// errFailed: git ran and failed; any other error is the controller's.
var errFailed = errors.New("accept: git failed")

// newIndex reads base into a fresh temporary index, beside an empty work
// tree that index-only commands never touch.
func newIndex(ctx context.Context, repo sandbox.Repo, base string) (index, func(), error) {
	if !objectID.MatchString(base) {
		return index{}, nil, fmt.Errorf("accept: the base %q is not a full commit id", base)
	}
	dir, err := os.MkdirTemp("", "adc-accept-")
	if err != nil {
		return index{}, nil, err
	}
	g := index{repo: repo, tree: filepath.Join(dir, "tree"), file: filepath.Join(dir, "index")}
	cleanup := func() { os.RemoveAll(dir) }
	err = os.Mkdir(g.tree, 0o700)
	if err == nil {
		_, err = g.run(ctx, nil, "read-tree", base)
	}
	if err != nil {
		cleanup()
		return index{}, nil, err
	}
	return g, cleanup, nil
}

// run runs the hardened git on the temporary index, with stdin.
func (g index) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd, err := g.repo.Git(ctx, g.tree, args...)
	if err != nil {
		return nil, err
	}
	cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+g.file)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.Output()
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) && exit.ExitCode() > 0 {
		return nil, fmt.Errorf("%w: %s: %w: %s", errFailed, args[0], err, bytes.TrimSpace(exit.Stderr))
	}
	if err != nil {
		return nil, fmt.Errorf("accept: git %s: %w", args[0], err)
	}
	return out, nil
}
