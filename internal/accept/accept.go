// SPDX-License-Identifier: Apache-2.0

// Package accept is P14a (docs/design/m1/00 §5.2): the build's controller
// diff and the accept boundary. The builder's manifest.intent (I18) is
// P16's; the integrity guard is P14b's.
// Accept applies a candidate patch to a temporary index of the base in the
// controller's own repository (sandbox.Repo: hardened git, never a .git a
// run wrote) and takes the file list from git (01 §2.5 "Accept"). It
// refuses, failed: unsafe_output (a patch git will not apply, a path that
// is not plain, a symlink, a gitlink); protected_path; and, through the
// Guard seam, test_integrity_violation (the integrity guard is P14b's).
// Warnings go to the reviewer; each verdict keeps its elapsed time and
// blocks (D55).
//
// The line (#226 entry 168): a block is a finding about the candidate's
// code, its refusal, which repairs the build (graph.AcceptRefused). The
// guard's own failures (its go list failing in its sandbox, its process not
// dispatched or not started, a panic in it, its type-check or whole-run
// deadline) are its run's error (ErrGuardRun): Accept returns them, and
// adcd records could_not_run/controller_run_failed, retried, bounded, then
// a visible hold, never a repair.
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
	"strconv"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// maxPatch is adapterhost's patch bound, and the bound on any git output
// accept reads (a var: tests lower it).
var maxPatch int64 = 64 << 20

var objectID = regexp.MustCompile(`^[0-9a-f]{40}$`)

// In is one accept.
type In struct {
	Repo    sandbox.Repo
	Base    string           // the base commit, a full sha1 id
	Patch   []byte           // the build's diff
	Policy  Policy           // PolicyOf's
	Ignored []string         // Diff's ignored output files: warnings
	Guard   Guard            // nil: no integrity guard (P14b's)
	Now     func() time.Time // nil: time.Now
}

// Block is one finding: a block refusing the candidate, or a warning.
type Block struct {
	Reason vocab.Reason `json:"reason"`
	Path   string       `json:"path"`
	What   string       `json:"what"`
	// Additive: a finding P14b-out added (Astra #276): the report gives
	// it only the capacity the base guard's findings leave.
	Additive bool `json:"-"`
	// Typed: an Additive warning all of whose signals are P14b-out-3's types
	// check's: the report gives it what all others leave (Astra #282 r2 H1).
	Typed bool `json:"-"`
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

// Change is one entry of git's raw diff of the candidate against the base.
type Change struct {
	SrcMode, DstMode, SrcBlob, DstBlob, Status string
	Src, Dst                                   string // Dst is Src unless renamed or copied
}

// Guard is the integrity guard's seam (P14b owns 01 §2.5's guard): it
// sees the changes of a candidate that passed every structural and
// protected-path check, and reads their blobs through read. Its blocks
// refuse the candidate (test_integrity_violation); its warnings go to the
// reviewer.
type Guard func(ctx context.Context, changes []Change, read Reader) (blocks, warnings []Block, err error)

// Reader reads a blob of the candidate's repository: ok false, and
// nothing read, when it is over max bytes.
type Reader func(ctx context.Context, blob string, max int64) (b []byte, ok bool, err error)

// Accept checks in's patch. An error is the controller's own fault (git
// unusable, the repository changed); a candidate's fault is a verdict.
func Accept(ctx context.Context, in In) (v Verdict, err error) {
	if in.Now == nil {
		in.Now = time.Now
	}
	start := in.Now()
	defer func() {
		// Any git output over its bound (the file list, the tree object,
		// git's diagnostics) is the candidate's size, never the
		// controller's fault: unsafe_output, with no accepted tree.
		if errors.Is(err, ErrTooLarge) {
			v.Blocks = append(v.Blocks, Block{vocab.ReasonUnsafeOutput, "", "a git output is over its bound", false, false})
			v.Tree, v.TreeDigest, err = "", "", nil
		}
		v.Elapsed = in.Now().Sub(start)
	}()
	if !in.Policy.ok {
		return v, errors.New("accept: a policy PolicyOf did not build")
	}
	for _, p := range in.Ignored {
		v.Warnings = append(v.Warnings, Block{vocab.ReasonCompletedWithFindings, p, "output .gitignore kept out of the diff", false, false})
	}
	if int64(len(in.Patch)) > maxPatch {
		v.Blocks = append(v.Blocks, Block{vocab.ReasonUnsafeOutput, "", fmt.Sprintf("the patch is over %d bytes", maxPatch), false, false})
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
				return v, err // ErrTooLarge included: the defer refuses it
			}
			v.Blocks = append(v.Blocks, Block{vocab.ReasonUnsafeOutput, "", "git does not apply the patch to the base: a path git refuses (.., .git, absolute) or a hunk that does not apply", false, false})
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
	// A structural refusal is final: no blob of a refused candidate (a
	// gitlink's commit id, say) is ever read.
	if in.Guard != nil && v.Accepted() {
		blocks, warnings, err := in.Guard(ctx, changes, g.blob)
		if err != nil {
			return v, err
		}
		v.Blocks, v.Warnings = append(v.Blocks, blocks...), append(v.Warnings, warnings...)
	}
	if !v.Accepted() {
		return v, nil
	}
	tree, err := g.run(ctx, nil, "write-tree")
	if err != nil {
		return v, err
	}
	id := strings.TrimSpace(string(tree))
	obj, err := g.run(ctx, nil, "cat-file", "tree", id)
	if err != nil {
		return v, err
	}
	s := sha256.Sum256(obj) // the tree object's bytes, read whole
	v.Tree, v.TreeDigest = id, types.Digest("sha256:"+hex.EncodeToString(s[:]))
	return v, nil
}

// check is the path and mode rules for one change: both its paths.
func (pol Policy) check(c Change) []Block {
	paths := []string{c.Src}
	if c.Dst != c.Src {
		paths = append(paths, c.Dst)
	}
	var out []Block
	for _, p := range paths {
		switch {
		case unsafePath(p):
			return []Block{{vocab.ReasonUnsafeOutput, p, "not one plain repository path", false, false}}
		case pol.Protected(p):
			out = append(out, Block{vocab.ReasonProtectedPath, p, "a protected path", false, false})
		}
	}
	switch {
	case c.DstMode == "120000":
		return []Block{{vocab.ReasonUnsafeOutput, c.Dst, "a symlink", false, false}}
	case c.SrcMode == "160000" || c.DstMode == "160000":
		return []Block{{vocab.ReasonUnsafeOutput, c.Dst, "a gitlink (submodule)", false, false}}
	}
	return out
}

// parseRaw reads `git diff --raw -z`: ":srcmode dstmode src dst status",
// then the path, and a second path for a rename or a copy.
func parseRaw(raw []byte) ([]Change, error) {
	fields := strings.Split(string(raw), "\x00")
	var out []Change
	for i := 0; i < len(fields) && fields[i] != ""; {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) != 5 || i+1 >= len(fields) {
			return nil, fmt.Errorf("accept: unreadable raw diff entry %q", fields[i])
		}
		c := Change{SrcMode: meta[0], DstMode: meta[1], SrcBlob: meta[2], DstBlob: meta[3], Status: meta[4][:1], Src: fields[i+1]}
		c.Dst, i = c.Src, i+2
		if c.Status == "R" || c.Status == "C" {
			if i >= len(fields) {
				return nil, fmt.Errorf("accept: a rename without its new path")
			}
			c.Dst, i = fields[i], i+1
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

// run runs the hardened git on the temporary index, with stdin, reading
// at most maxPatch bytes of its output: past that, git is killed and
// reaped, and run returns ErrTooLarge, never the whole output buffered.
func (g index) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd, err := g.repo.Git(ctx, g.tree, args...)
	if err != nil {
		return nil, err
	}
	cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+g.file)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, stderr, err := drain(ctx, cmd, maxPatch, maxStderr)
	if errors.Is(err, ErrTooLarge) {
		return nil, fmt.Errorf("%w (git %s)", err, args[0])
	}
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) && exit.ExitCode() > 0 {
		return nil, fmt.Errorf("%w: %s: %w: %s", errFailed, args[0], err, bytes.TrimSpace(stderr))
	}
	if err != nil {
		return nil, fmt.Errorf("accept: git %s: %w", args[0], err)
	}
	return out, nil
}

// blob is a Reader on the index's repository.
func (g index) blob(ctx context.Context, id string, max int64) ([]byte, bool, error) {
	if !objectID.MatchString(id) {
		return nil, false, fmt.Errorf("accept: %q is not an object id", id)
	}
	size, err := g.run(ctx, nil, "cat-file", "-s", id)
	if err != nil {
		return nil, false, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(size)), 10, 64)
	if err != nil || n > max {
		return nil, false, err
	}
	b, err := g.run(ctx, nil, "cat-file", "blob", id)
	return b, err == nil, err
}
