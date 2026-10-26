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
	"syscall"
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
	maxTail  = 64 << 10 // of a run's stderr kept for its log
	rmTime   = 30 * time.Second
	pullTime = 15 * time.Minute
)

// errPodman: podman could not start a run (exit 125, or no podman at all).
var errPodman = errors.New("referee: podman could not run the container")

// work is one judgement's controller-side clone and run directories.
type work struct {
	podman    string
	item      types.RefereeItem
	logs      string // a private directory for the runs' logs, or ""
	protected func(path string) bool
	touched   []string // the protected paths the patch changes
	host      sandbox.Host
	repo      sandbox.Repo
	pristine  *sandbox.Attempt // the base's workspace, after setup
	attempts  []*sandbox.Attempt
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

// gitPlain runs git in dir with no configuration but a repository's own (no
// HOME: no user configuration either), never finding a repository above dir.
func gitPlain(ctx context.Context, dir string, stdin []byte, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir, cmd.Env = dir, []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1",
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

// cmd is the user's podman, with the referee's environment (its storage
// and runtime directory are found from it). The container gets none of it
// (--env-host=false, --http-proxy=false).
func (w *work) cmd(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, w.podman, args...)
}

// check refuses a patch accept refuses, or one touching an acceptance path
// or the results path, a directory holding one, or a path below one. It
// notes the protected paths the patch touches.
func (w *work) check(ctx context.Context, patch []byte, files []overlay) error {
	pol, err := accept.PolicyOf(types.ProjectProfile{})
	if err != nil {
		return err
	}
	overlaps := func(p, q string) bool { return p == q || strings.HasPrefix(q, p+"/") || strings.HasPrefix(p, q+"/") }
	guard := func(_ context.Context, changes []accept.Change, _ accept.Reader) (blocks, _ []accept.Block, _ error) {
		for _, c := range changes {
			for _, p := range slices.Compact([]string{c.Src, c.Dst}) {
				for _, f := range files {
					if overlaps(p, f.path) {
						blocks = append(blocks, accept.Block{Reason: vocab.ReasonProtectedPath, Path: p, What: "an acceptance path"})
					}
				}
				if res := string(w.item.Results.Path); res != "stdout" && overlaps(p, res) {
					blocks = append(blocks, accept.Block{Reason: vocab.ReasonProtectedPath, Path: p, What: "the results path"})
				}
				if w.protected(p) {
					w.touched = append(w.touched, p)
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
	_, run, err := w.run(ctx, "setup", w.pristine.Workspace(), *w.item.Setup, true)
	r.Runs["setup"] = run
	if err != nil || run.TimedOut || run.Exit != 0 {
		return found(CouldNotRun, "the setup step failed")
	}
	return nil
}

// test runs the test command in a fresh copy of the pristine workspace,
// with patch applied, the protected paths it touched restored, and files
// overlaid, and reads its results.
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
	if len(patch) > 0 {
		if err := w.restore(root); err != nil {
			return result{}, err
		}
	}
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
	out, run, err := w.run(ctx, name, ws, w.item.TestCommand, false)
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

// restore gives the workspace (root) the base's version of each protected
// path the patch touched: removed, then copied back from the pristine
// workspace (the base after setup) when it is there.
func (w *work) restore(root *os.Root) error {
	base, err := os.OpenRoot(w.pristine.Workspace())
	if err != nil {
		return err
	}
	defer base.Close()
	for _, p := range w.touched {
		if err := root.RemoveAll(p); err != nil {
			return err
		}
		st, err := base.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return err
		case st.Mode()&fs.ModeSymlink != 0:
			var target string
			if target, err = base.Readlink(p); err == nil {
				err = errors.Join(root.MkdirAll(filepath.Dir(p), 0o700), root.Symlink(target, p))
			}
		case st.Mode().IsRegular():
			var b []byte
			if b, err = base.ReadFile(p); err == nil {
				err = errors.Join(root.MkdirAll(filepath.Dir(p), 0o700), root.WriteFile(p, b, st.Mode().Perm()))
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// run runs argv in the item's image on ws, with the sandbox's confinement,
// no network unless network, and the item's timeout; the container is then
// removed by name whatever happened. It returns the run's stdout (bounded).
// With a logs directory, the run's stdout and its stderr's tail go there as
// <run>.stdout and <run>.stderr.
func (w *work) run(ctx context.Context, run string, ws string, argv types.Argv, network bool) ([]byte, *Run, error) {
	x := &Run{Exit: -1}
	entry, err := json.Marshal([]string(argv))
	if err != nil || strings.ContainsAny(ws, ",\n") {
		return nil, x, errPodman
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
	var stderr tail
	cmd.Stdout, cmd.Stderr = &out, &stderr
	start := time.Now()
	err = cmd.Run()
	x.Seconds = time.Since(start).Seconds()
	rm, cancelRm := context.WithTimeout(context.WithoutCancel(ctx), rmTime)
	defer cancelRm()
	_ = w.cmd(rm, "rm", "--force", "--ignore", "--time=0", name).Run()
	if w.logs != "" {
		if lerr := errors.Join(writeLog(w.logs, run+".stdout", out.b.Bytes()), writeLog(w.logs, run+".stderr", stderr.bytes())); lerr != nil {
			return nil, x, lerr
		}
	}
	var exit *exec.ExitError
	switch {
	case errors.Is(rctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		x.TimedOut = true
	case err == nil:
		x.Exit = 0
	case errors.As(err, &exit) && exit.ExitCode() != 125:
		x.Exit = exit.ExitCode()
	default:
		return nil, x, errPodman
	}
	if out.over {
		return nil, x, nil
	}
	return out.b.Bytes(), x, nil
}

// writeLog writes a new file, 0600, in the logs directory.
func writeLog(dir, name string, b []byte) error {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	return errors.Join(err, f.Close())
}

// tail keeps the last maxTail bytes written.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	if t.b = append(t.b, p...); len(t.b) > 2*maxTail {
		t.b = append([]byte(nil), t.b[len(t.b)-maxTail:]...)
	}
	return len(p), nil
}

func (t *tail) bytes() []byte { return t.b[max(0, len(t.b)-maxTail):] }

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
