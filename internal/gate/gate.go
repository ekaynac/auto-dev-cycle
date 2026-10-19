// SPDX-License-Identifier: Apache-2.0

// Package gate runs a project's gate on a candidate tree in the sandbox
// (P15): the profile's command in gate mode, its results read from the
// controller's results mount, judged against the measured baseline.
//
// Decisions:
//   - The results path is derived, never the profile's: {results_path} in
//     the command is the controller's file in the results mount. A command
//     without it has its stdout written there, by a shell redirect the
//     controller adds (go test -json streams on stdout).
//   - The verdict comes from the results file alone, never from the exit
//     code or the run's output (I13). No file, an unreadable one, or none
//     the run could write (results copied out over their bound) is
//     could_not_run/runner_fault, never empty: results never written.
//   - Ownership (the profile's ownership_rules, default, 07): a failed test
//     or package, and a test skipped against the baseline, are the
//     candidate's (found/gate_test_failed, a builder repair). A missing or
//     incomplete test is the candidate's in a package it changed, or when
//     it is an acceptance test; elsewhere, as no result at all, it is the
//     environment's (could_not_run/runner_fault, a hold on its owner), and
//     any environment fault decides the verdict.
//   - The baseline is bound to the command and the image it was measured
//     with: another one is refused, never compared. A run without a
//     baseline records one, and only a green one (results.Report.Green).
//   - The tree gated is the one checked out: the checkout's own tree id
//     must be the one asked for (a commit id or another tree is refused),
//     and the receipt is about it (receipt.go).
//   - The cwd contract (#275): every Python process of the run starts in
//     the checkout root, and none changes directory before pytest has
//     collected; a probe the controller writes into the results mount
//     records both (cwd/), and anything else invalidates the run
//     (could_not_run/runner_fault). The probe runs in-process, so a test
//     that rewrites its log is decision A's residual, as is a Python run
//     with -E or -I, which never loads it.
//   - Acceptance tests are overlaid on the gate workspace only.
package gate

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/results"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

//go:embed cwd/*.py
var probe embed.FS

const (
	resultsFile = "go-test.json"    // the derived results file, in the results mount
	probeDir    = ".adc-cwd"        // the cwd probe, beside it
	placeholder = "{results_path}"  // the profile's name for the derived path
	maxResults  = results.MaxStream // what a run's results may hold
	maxOverlay  = 16 << 20          // acceptance files, in all
	ownership   = "default"         // the only ownership rules in v1
)

var (
	// ErrSpec: the gate cannot run as asked; nothing ran, or what ran is
	// not this gate's (another tree, baseline or platform).
	ErrSpec = errors.New("gate: refused")
)

// Baseline is a measured baseline: the command and pinned image it was
// measured with, and its report (fixtures/m1/issues/*/baseline-report.json).
type Baseline struct {
	Command []string `json:"command"`
	Image   string   `json:"image"`
	results.Report
}

// In is one gate run.
type In struct {
	Host     sandbox.Host
	Image    sandbox.Image // resolved: its platform is the host's
	Repo     sandbox.Repo
	Tree     string // the git tree to gate
	Changed  []string
	Gate     types.ProjectProfileGate
	Env      string            // the gate environment's name in Gate
	Overlay  map[string][]byte // acceptance tests, repo path → content
	Expected []results.Test
	Baseline *Baseline // nil: measure one
	Limits   sandbox.Limits
}

// Verdict is a gate run's outcome. Why is the controller's own text
// (counts and fixed phrases), never the run's; Stream is the raw results
// file, a restricted artifact that no result's value holds.
type Verdict struct {
	Tree    string
	Outcome vocab.Outcome
	Reason  vocab.Reason
	Why     string
	Results results.Report
	Stream  []byte
	Elapsed time.Duration
}

func (v *Verdict) set(o vocab.Outcome, r vocab.Reason, why string) {
	v.Outcome, v.Reason, v.Why = o, r, why
}

// Run gates in.Tree. An error means no verdict: the gate was refused, or
// the controller failed.
func Run(ctx context.Context, in In) (v Verdict, err error) {
	start := time.Now()
	defer func() { v.Elapsed = time.Since(start) }()
	if err := in.check(); err != nil {
		return v, err
	}
	a, err := in.Host.NewAttempt()
	if err != nil {
		return v, err
	}
	defer func() { err = errors.Join(err, a.Teardown()) }()
	if v.Tree, err = checkout(ctx, in.Repo, in.Tree, a.Workspace()); err != nil {
		return v, err
	}
	if v.Tree != in.Tree {
		return v, fmt.Errorf("%w: %s checks out tree %s, not a tree of its own", ErrSpec, in.Tree, v.Tree)
	}
	module := modulePath(a.Workspace())
	if err := writeFiles(a.Workspace(), in.Overlay); err != nil {
		return v, err
	}
	if err := writeProbe(a.Results()); err != nil {
		return v, err
	}
	// The exit code and the run's output are never the verdict; a run
	// refused before it started is no verdict at all. A run that left too
	// much is judged by what it wrote: copy-out bounds it again.
	_, runErr := in.Host.Start(ctx, sandbox.Run{Image: in.Image, Mode: sandbox.Gate, Attempt: a, Limits: in.Limits, Argv: argv(in.Gate.Command)})
	if (errors.Is(runErr, sandbox.ErrSpec) || errors.Is(runErr, sandbox.ErrHost)) && !errors.Is(runErr, sandbox.ErrLeft) {
		return v, runErr
	}
	out := filepath.Join(a.Dir, "out")
	if err := a.CopyOut(out, maxResults+int64(maxOverlay)); err != nil {
		v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, "the run's results could not be copied out within their bounds")
		return v, nil
	}
	if bad := violations(filepath.Join(out, probeDir, "log"), a.Workspace()); bad > 0 {
		v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, fmt.Sprintf("invalidated: %d Python process starts or early chdirs outside the checkout root (the cwd contract)", bad))
		return v, nil
	}
	v.Stream, err = os.ReadFile(filepath.Join(out, resultsFile))
	if err != nil {
		v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, "the run never wrote its results")
		return v, nil
	}
	var base *results.Report
	if in.Baseline != nil {
		base = &in.Baseline.Report
	}
	if v.Results, err = results.ParseGoTestJSON(bytes.NewReader(v.Stream), in.Expected, base); err != nil {
		v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, "the run's results are not a go test -json stream")
		return v, nil
	}
	v.judge(in, module)
	return v, nil
}

// check refuses a gate this package cannot run as asked: another adapter
// or ownership rules, an image of another platform than the gate
// environment's runtime (#226 entry 88), or a baseline measured otherwise.
func (in In) check() error {
	i := slices.IndexFunc(in.Gate.Environments, func(e types.ProjectProfileGateEnvironmentsItem) bool { return string(e.Name) == in.Env })
	if i < 0 {
		return fmt.Errorf("%w: no gate environment %q", ErrSpec, in.Env)
	}
	rt := in.Gate.Environments[i].Facts.Runtime
	switch {
	case in.Gate.ResultAdapter != results.GoTestJSON || in.Gate.OwnershipRules != ownership:
		return fmt.Errorf("%w: result adapter %q with ownership rules %q (only %s, %s)", ErrSpec, in.Gate.ResultAdapter, in.Gate.OwnershipRules, results.GoTestJSON, ownership)
	case rt.Os != in.Image.OS || rt.Arch != in.Image.Arch:
		return fmt.Errorf("%w: the image is %s/%s, the environment %s/%s", ErrSpec, in.Image.OS, in.Image.Arch, rt.Os, rt.Arch)
	case in.Baseline != nil && (!slices.Equal(in.Baseline.Command, in.Gate.Command) || in.Baseline.Image != in.Image.Pinned):
		return fmt.Errorf("%w: the baseline was measured with another command or image", ErrSpec)
	}
	return nil
}

// judge decides v from its report under the default ownership rules.
func (v *Verdict) judge(in In, module string) {
	if in.Baseline == nil {
		if v.Results.Green() {
			v.set(vocab.Empty, vocab.ReasonCompletedClean, "the baseline is green")
		} else {
			v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, "the base is not green: no baseline")
		}
		return
	}
	touched := func(pkg string) bool {
		rel := strings.TrimPrefix(strings.TrimPrefix(pkg, module), "/")
		return module != "" && strings.HasPrefix(pkg, module) && slices.ContainsFunc(in.Changed, func(c string) bool {
			return path.Dir(c) == cmpOr(rel, ".")
		})
	}
	var mine, theirs int
	for _, x := range v.Results.Blocking() {
		if x.Outcome == results.Fail || x.Outcome == results.Skip || x.Expected || touched(x.Package) {
			mine++
		} else {
			theirs++
		}
	}
	for p, o := range v.Results.BlockingPackages() {
		if o == results.Fail || touched(p) {
			mine++
		} else {
			theirs++
		}
	}
	switch {
	case theirs > 0:
		v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, fmt.Sprintf("%d results missing or incomplete outside the candidate's packages (and %d of the candidate's)", theirs, mine))
	case mine > 0:
		v.set(vocab.Found, vocab.ReasonGateTestFailed, fmt.Sprintf("%d tests or packages fail against the baseline", mine))
	case !v.Results.Clean():
		v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, "no test passed")
	default:
		v.set(vocab.Empty, vocab.ReasonCompletedClean, fmt.Sprintf("%d tests pass against the baseline", len(v.Results.Results)))
	}
}

func cmpOr(a, b string) string {
	if a == "" {
		return b
	}
	return a
}

// argv is the command the container runs: the profile's, its results path
// derived, with the cwd probe's environment.
func argv(command []string) []string {
	res := sandbox.ResultsDir + "/" + resultsFile
	cmd, named := make([]string, len(command)), false
	for i, a := range command {
		cmd[i] = strings.ReplaceAll(a, placeholder, res)
		named = named || cmd[i] != a
	}
	if !named { // a stdout adapter: the controller writes its stream to the results file
		cmd = append([]string{"sh", "-c", `exec "$@" >"$0"`, res}, cmd...)
	}
	dir := sandbox.ResultsDir + "/" + probeDir
	return append([]string{"env", "PYTHONPATH=" + dir, "PYTEST_PLUGINS=adc_gate_cwd", "PYTHONDONTWRITEBYTECODE=1",
		"ADC_GATE_ROOT=" + sandbox.WorkspaceDir, "ADC_GATE_LOG=" + dir + "/log"}, cmd...)
}

// writeProbe writes the cwd probe into the results mount.
func writeProbe(results string) error {
	dir := filepath.Join(results, probeDir)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	for _, name := range []string{"sitecustomize.py", "adc_gate_cwd.py"} {
		b, err := probe.ReadFile("cwd/" + name)
		if err == nil {
			err = os.WriteFile(filepath.Join(dir, name), b, 0o600)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// violations counts the probe's records outside the checkout root:
// sandbox.WorkspaceDir in the container, or ws itself for a host's Runner
// that runs the command in place.
func violations(log, ws string) int {
	b, err := os.ReadFile(log)
	if err != nil {
		return 0 // no Python ran
	}
	n := 0
	for line := range strings.Lines(string(b)) {
		_, dir, _ := strings.Cut(strings.TrimSuffix(line, "\n"), "\t")
		if dir != sandbox.WorkspaceDir && !sameDir(dir, ws) {
			n++
		}
	}
	return n
}

func sameDir(a, b string) bool {
	ra, ea := filepath.EvalSymlinks(a)
	rb, eb := filepath.EvalSymlinks(b)
	return ea == nil && eb == nil && ra == rb
}

// checkout writes tree into ws through a temporary index and returns the
// tree id the index holds.
func checkout(ctx context.Context, repo sandbox.Repo, tree, ws string) (string, error) {
	idx, err := os.MkdirTemp("", "adc-gate-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(idx)
	git := func(args ...string) ([]byte, error) {
		cmd, err := repo.Git(ctx, ws, args...)
		if err != nil {
			return nil, err
		}
		cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+filepath.Join(idx, "index"))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("gate: git %s: %w: %s", args[0], err, bytes.TrimSpace(stderr.Bytes()))
		}
		return out, nil
	}
	if _, err := git("read-tree", "--", tree); err != nil {
		return "", err
	}
	if _, err := git("checkout", "--", "."); err != nil {
		return "", err
	}
	out, err := git("write-tree")
	return strings.TrimSpace(string(out)), err
}

// writeFiles overlays files on ws: each a clean relative path, inside it.
func writeFiles(ws string, files map[string][]byte) error {
	root, err := os.OpenRoot(ws)
	if err != nil {
		return err
	}
	defer root.Close()
	total := 0
	for p, b := range files {
		if total += len(b); total > maxOverlay || !filepath.IsLocal(p) || path.Clean(p) != p {
			return fmt.Errorf("%w: an acceptance file %q is not a clean path inside the workspace, or the files are too large", ErrSpec, p)
		}
		if err := root.MkdirAll(path.Dir(p), 0o700); err != nil {
			return err
		}
		if err := root.WriteFile(p, b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// modulePath is the module line of ws's go.mod, or "".
func modulePath(ws string) string {
	b, err := os.ReadFile(filepath.Join(ws, "go.mod"))
	if err != nil {
		return ""
	}
	for line := range strings.Lines(string(b)) {
		if m, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.Trim(strings.TrimSpace(m), `"`)
		}
	}
	return ""
}
