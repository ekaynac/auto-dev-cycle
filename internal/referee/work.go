// SPDX-License-Identifier: Apache-2.0

package referee

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/accept"
	"github.com/ekaynac/auto-dev-cycle/internal/regularfile"
	"github.com/ekaynac/auto-dev-cycle/internal/results"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Each run's limits, and the bound on a workspace copy.
const (
	memory   = "4g"
	cpus     = "4"
	pids     = "4096"
	maxCopy  = 4 << 30
	rmTime   = 30 * time.Second
	pullTime = 15 * time.Minute
)

// errPodman: podman could not start a run (exit 125, or no podman at all).
var errPodman = errors.New("referee: podman could not run the container")

// work is one judgement's controller-side clone and run directories.
type work struct {
	podman   string
	item     types.RefereeItem
	host     sandbox.Host
	repo     sandbox.Repo
	pristine *sandbox.Attempt // the base's workspace, after setup
	attempts []*sandbox.Attempt
}

// result is one test run's results and how it ended.
type result struct {
	tests
	exit     int
	timedOut bool
	err      error // the results are unreadable
}

// completed: the run ended on its own (no timeout, no signal, podman ran
// it) with readable results; a candidate's non-zero exit needs a failure
// reported (a crash after the tests otherwise).
func (x result) completed(candidate bool) bool {
	if x.timedOut || x.err != nil || x.exit < 0 || x.exit > 124 {
		return false
	}
	return !candidate || x.exit == 0 || x.failures
}

func (w *work) attempt() (*sandbox.Attempt, error) {
	a, err := w.host.NewAttempt()
	if err == nil {
		w.attempts = append(w.attempts, a)
	}
	return a, err
}

func (w *work) teardown() {
	for _, a := range w.attempts {
		_ = a.Teardown()
	}
}

// prepare clones the item's repository and exports the base into the
// pristine workspace. The controller-side clone takes a branch: a bare copy
// whose branch adc-referee is the base comes first.
func (w *work) prepare(ctx context.Context, stage, dst, src string) error {
	base := w.item.Base
	if gitPlain(ctx, "/", nil, "clone", "--bare", "--quiet", "--no-local", "--", src, stage) != nil {
		return found(InvalidItem, "the item's repository cannot be cloned")
	}
	if gitPlain(ctx, "/", nil, "--git-dir="+stage, "rev-parse", "--verify", "--quiet", base+"^{commit}") != nil ||
		gitPlain(ctx, "/", nil, "--git-dir="+stage, "update-ref", "refs/heads/adc-referee", base) != nil {
		return found(InvalidItem, "the base is not a commit of the item's repository")
	}
	repo, err := w.host.Clone(ctx, stage, "refs/heads/adc-referee", dst)
	if err != nil {
		return err
	}
	w.repo = repo
	if w.pristine, err = w.attempt(); err != nil {
		return err
	}
	cmd, err := w.repo.Git(ctx, w.pristine.Workspace(), "checkout", "--quiet", base, "--", ".")
	if err != nil {
		return err
	}
	if err := cmd.Run(); err != nil {
		return err
	}
	return w.image(ctx)
}

// gitPlain runs git in dir with no configuration but a repository's own,
// never finding a repository above dir.
func gitPlain(ctx context.Context, dir string, stdin []byte, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir, cmd.Env = dir, []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_CEILING_DIRECTORIES=" + filepath.Dir(dir)}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return cmd.Run()
}

// image pulls the item's image when it is not present.
func (w *work) image(ctx context.Context) error {
	if w.cmd(ctx, "image", "exists", string(w.item.Image)).Run() == nil {
		return nil
	}
	pull, cancel := context.WithTimeout(ctx, pullTime)
	defer cancel()
	if w.cmd(pull, "pull", "--quiet", string(w.item.Image)).Run() != nil {
		return found(CouldNotRun, "the item's image is not present and cannot be pulled")
	}
	return nil
}

// cmd is podman with its own environment only: what it needs to find the
// user's storage. The container gets none of it (--env-host=false).
func (w *work) cmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, w.podman, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + os.Getenv("HOME")}
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		cmd.Env = append(cmd.Env, "XDG_RUNTIME_DIR="+x)
	}
	return cmd
}

// check refuses a patch accept refuses, or one touching an acceptance
// path, a directory holding one, or a path below one.
func (w *work) check(ctx context.Context, patch []byte, files []overlay) error {
	pol, err := accept.PolicyOf(types.ProjectProfile{})
	if err != nil {
		return err
	}
	guard := func(_ context.Context, changes []accept.Change, _ accept.Reader) (blocks, _ []accept.Block, _ error) {
		for _, c := range changes {
			for _, p := range slices.Compact([]string{c.Src, c.Dst}) {
				for _, f := range files {
					if p == f.path || strings.HasPrefix(f.path, p+"/") || strings.HasPrefix(p, f.path+"/") {
						blocks = append(blocks, accept.Block{Reason: vocab.ReasonProtectedPath, Path: p, What: "an acceptance path"})
					}
				}
			}
		}
		return blocks, nil, nil
	}
	v, err := accept.Accept(ctx, accept.In{Repo: w.repo, Base: w.item.Base, Patch: patch, Policy: pol, Guard: guard})
	if err != nil {
		return err
	}
	if !v.Accepted() {
		f := &finding{verdict: Rejected}
		for _, b := range v.Blocks {
			f.reasons = append(f.reasons, fmt.Sprintf("the patch is refused: %s: %s", b.What, b.Path))
		}
		return f
	}
	return nil
}

// setup runs the item's setup step in the pristine workspace, with the
// network.
func (w *work) setup(ctx context.Context, r *Report) error {
	if w.item.Setup == nil {
		return nil
	}
	_, run, err := w.run(ctx, w.pristine.Workspace(), *w.item.Setup, true)
	r.Runs["setup"] = run
	if err != nil || run.TimedOut || run.Exit != 0 {
		return found(CouldNotRun, "the setup step failed")
	}
	return nil
}

// test runs the test command in a fresh copy of the pristine workspace,
// with patch applied and files overlaid, and reads its results.
func (w *work) test(ctx context.Context, r *Report, name string, patch []byte, files []overlay) (result, error) {
	a, err := w.attempt()
	if err != nil {
		return result{}, err
	}
	ws := a.Workspace()
	if err := errors.Join(os.Remove(ws), w.pristine.CopyTree(ws, maxCopy)); err != nil {
		return result{}, err
	}
	// The workspace is no repository (no .git, none found above it): git
	// applies the patch accept checked as plain files, refusing any path
	// beyond a symlink. (The hardened git's apply to a work tree crashes
	// with GIT_ATTR_SOURCE set, git 2.43.)
	if len(patch) > 0 && gitPlain(ctx, ws, patch, "apply", "--whitespace=nowarn", "-") != nil {
		return result{}, found(CouldNotRun, "the patch does not apply to the workspace after setup")
	}
	root, err := os.OpenRoot(ws)
	if err != nil {
		return result{}, err
	}
	defer root.Close()
	for _, f := range files {
		if errors.Join(root.MkdirAll(filepath.Dir(f.path), 0o700), root.WriteFile(f.path, f.data, 0o600)) != nil {
			return result{}, found(CouldNotRun, "the acceptance cannot be overlaid on the workspace")
		}
	}
	path := string(w.item.Results.Path)
	if path != "stdout" {
		if err := root.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return result{}, err
		}
	}
	out, run, err := w.run(ctx, ws, w.item.TestCommand, false)
	r.Runs[name] = run
	if err != nil {
		return result{}, err
	}
	if path != "stdout" {
		out, err = readResults(root, path)
	}
	x := result{exit: run.Exit, timedOut: run.TimedOut, err: err}
	if err == nil {
		x.tests, x.err = parse(string(w.item.Results.Format), out)
	}
	for _, o := range x.by {
		run.Tests++
		switch o {
		case pass:
			run.Passed++
		case skip:
			run.Skipped++
		default:
			run.Failed++
		}
	}
	return x, nil
}

// readResults reads the results file a run wrote, never through a link.
func readResults(root *os.Root, path string) ([]byte, error) {
	f, err := root.OpenFile(path, regularfile.Flags, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := regularfile.Check(f, nil); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, results.MaxStream+1))
	if err == nil && len(b) > results.MaxStream {
		err = errUnreadable
	}
	return b, err
}

// run runs argv in the item's image on ws, with the sandbox's confinement,
// no network unless network, and the item's timeout; the container is then
// removed by name whatever happened. It returns the run's stdout (bounded).
func (w *work) run(ctx context.Context, ws string, argv types.Argv, network bool) ([]byte, *Run, error) {
	run := &Run{Exit: -1}
	entry, err := json.Marshal([]string(argv))
	if err != nil || strings.ContainsAny(ws, ",\n") {
		return nil, run, errPodman
	}
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	name := "adc-referee-" + hex.EncodeToString(b)
	timeout := time.Duration(w.item.TimeoutSeconds) * time.Second
	args := []string{"run", "--rm", "--name=" + name, "--pull=never"}
	if !network {
		args = append(args, "--network=none")
	}
	args = append(append(args, sandbox.Confinement()...), "--memory="+memory, "--cpus="+cpus, "--pids-limit="+pids,
		"--timeout="+strconv.FormatInt(w.item.TimeoutSeconds+int64(rmTime/time.Second), 10),
		"--mount=type=bind,src="+ws+",dst="+sandbox.WorkspaceDir, "--entrypoint="+string(entry), string(w.item.Image))
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := w.cmd(rctx, args...)
	var out capped
	cmd.Stdout = &out
	start := time.Now()
	err = cmd.Run()
	run.Seconds = time.Since(start).Seconds()
	rm, cancelRm := context.WithTimeout(context.WithoutCancel(ctx), rmTime)
	defer cancelRm()
	_ = w.cmd(rm, "rm", "--force", "--ignore", "--time=0", name).Run()
	var exit *exec.ExitError
	switch {
	case errors.Is(rctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		run.TimedOut = true
	case err == nil:
		run.Exit = 0
	case errors.As(err, &exit) && exit.ExitCode() != 125:
		run.Exit = exit.ExitCode()
	default:
		return nil, run, errPodman
	}
	if out.over {
		return nil, run, nil
	}
	return out.b.Bytes(), run, nil
}

// capped keeps up to results.MaxStream bytes and notes any beyond.
type capped struct {
	b    bytes.Buffer
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if c.over = c.over || c.b.Len()+len(p) > results.MaxStream; !c.over {
		c.b.Write(p)
	}
	return len(p), nil
}
