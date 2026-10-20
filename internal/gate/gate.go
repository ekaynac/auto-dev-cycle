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
//     could_not_run/runner_fault, never empty: results never written. An
//     end that is not the run's own only vetoes (Astra #286 H1): a
//     candidate run cut by its timeout or a signal is
//     could_not_run/runner_fault, whatever it wrote; a test failure's exit
//     status is a normal end.
//   - Ownership (the profile's ownership_rules, default, 07): a failed test
//     or package, and a test skipped against the baseline, are the
//     candidate's (found/gate_test_failed, a builder repair). A missing or
//     incomplete test is the candidate's in a package it changed, or when
//     it is an acceptance test; elsewhere, as no result at all, it is the
//     environment's (could_not_run/runner_fault, a hold on its owner), and
//     any environment fault decides the verdict.
//   - The baseline is measured explicitly (Measure), and only from a run
//     that ended on its own with exit 0 and green results: a run cut by
//     its timeout, a kill or its caller is no baseline (Opus #286 H1), and
//     a run whose caller's context ended is no verdict at all. Run requires
//     one, bound to the command, the platform image (id, OS, architecture)
//     and the base tree it was measured with: another is refused, never
//     compared (Opus #286 M1, M2, L3), and so is one that is no measurement
//     (a binding field missing, no results, not green, a test of no
//     package it reports: Astra #286 M2).
//   - The root go.mod (the module path for ownership) is read as its
//     bounded git blob; one that is not a regular file is refused (Astra
//     #286 M4). The run's stdout is never read.
//   - The tree gated is the one checked out: the checkout's own tree id
//     must be the one asked for (a commit id or another tree is refused),
//     and the receipt is about it (receipt.go).
//   - The cwd contract (#275), for junit_xml (pytest), by construction
//     (Astra #290): the command is pytest run directly (pytest, or python
//     -m pytest; no shell, wrapper or env) with allowlisted arguments only
//     (direct), in the checkout root the sandbox makes its workdir; the
//     controller appends --junitxml={results_path} (unless named) and -o
//     junit_family=xunit1, so each testcase names its file. Anything else
//     is refused (ErrSpec), never run. A Go test may run Python anywhere.
//   - Ownership for junit_xml: when the report holds any failure or error
//     (a collection error included, of a module or a class: Astra #290
//     M5), a missing test is the candidate's; otherwise a missing test is
//     the candidate's when its file, or a conftest.py or __init__.py above
//     it, changed.
//   - A gate environment's runtime.arch "any" (py-mini) relaxes only that
//     environment's check: the image is still the host's OS and
//     architecture.
//   - Acceptance tests are overlaid on the gate workspace only.
package gate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/results"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	placeholder = "{results_path}"  // the profile's name for the derived path
	maxResults  = results.MaxStream // what a run's results may hold
	maxOverlay  = 16 << 20          // acceptance files, in all
	maxGoMod    = 1 << 20           // the root go.mod, read as its blob
	ownership   = "default"         // the only ownership rules in v1
)

var (
	// ErrSpec: the gate cannot run as asked; nothing ran, or what ran is
	// not this gate's (another tree, baseline or platform).
	ErrSpec = errors.New("gate: refused")

	objectID = regexp.MustCompile(`^[0-9a-f]{40}$`)
	imageID  = regexp.MustCompile(`^[0-9a-f]{64}$`)

	// adapters are the result adapters the gate reads: the derived results
	// file, its parser and its name, and whether its runs are pytest's,
	// under the direct-command rule (direct).
	adapters = map[string]adapter{
		results.GoTestJSON: {"go-test.json", results.ParseGoTestJSON, "a go test -json stream", false},
		results.JUnitXML:   {"junit.xml", results.ParseJUnitXML, "a JUnit XML document", true},
	}
)

type adapter struct {
	file   string
	parse  func(io.Reader, []results.Test, *results.Report) (results.Report, error)
	what   string
	pytest bool
}

// Baseline is a measured baseline (fixtures/m1/issues/*/baseline-report.json):
// the command, the image (its pinned reference, and the platform image
// it resolved to, as a receipt binds it) and the base tree it was measured
// with, and its report.
type Baseline struct {
	Command []string `json:"command"`
	Image   string   `json:"image"`
	ImageID string   `json:"image_id"`
	OS      string   `json:"os"`
	Arch    string   `json:"arch"`
	Base    string   `json:"base_tree"`
	results.Report
}

// In is one gate run.
type In struct {
	Host     sandbox.Host
	Image    sandbox.Image // resolved: its platform is the host's
	Repo     sandbox.Repo
	Tree     string // the git tree to gate
	Base     string // the candidate's base tree, its baseline's
	Changed  []string
	Gate     types.ProjectProfileGate
	Env      string            // the gate environment's name in Gate
	Overlay  map[string][]byte // acceptance tests, repo path → content
	Expected []results.Test
	Baseline *Baseline // Run's; Measure measures one
	Limits   sandbox.Limits
}

// Verdict is a gate run's outcome. Why is the controller's own text
// (counts and fixed phrases), never the run's; Stream is the raw results
// file, a restricted artifact that no result's value holds.
type Verdict struct {
	Tree     string
	Outcome  vocab.Outcome
	Reason   vocab.Reason
	Why      string
	Results  results.Report
	Stream   []byte
	Elapsed  time.Duration
	baseline *Baseline // Run's: what the receipt's demand binds; none for Measure's, never a gate result
}

func (v *Verdict) set(o vocab.Outcome, r vocab.Reason, why string) {
	v.Outcome, v.Reason, v.Why = o, r, why
}

// Run gates in.Tree against in.Baseline, which it requires (Opus #286
// M1: a run without one is no gate). An error means no verdict: the gate
// was refused, its context ended, or the controller failed.
func Run(ctx context.Context, in In) (Verdict, error) {
	if in.Baseline == nil {
		return Verdict{}, fmt.Errorf("%w: no baseline (Measure measures one)", ErrSpec)
	}
	return run(ctx, in)
}

// Measure measures in.Tree's baseline. Only a run that ended on its own
// (exit 0 within its timeout) with green results is one (Opus #286 H1: a
// cut run's stream cannot show what never ran): the exit code may veto a
// baseline, never make one. Its verdict never becomes a gate result.
func Measure(ctx context.Context, in In) (Baseline, Verdict, error) {
	in.Baseline = nil
	v, err := run(ctx, in)
	if err != nil || v.Outcome != vocab.Empty {
		return Baseline{}, v, err
	}
	return Baseline{Command: slices.Clone(in.Gate.Command), Image: in.Image.Pinned, ImageID: in.Image.ID, OS: in.Image.OS, Arch: in.Image.Arch,
		Base: v.Tree, Report: v.Results}, v, nil
}

func run(ctx context.Context, in In) (v Verdict, err error) {
	start := time.Now()
	defer func() { v.Elapsed = time.Since(start) }()
	if err := in.check(); err != nil {
		return v, err
	}
	// The validated baseline, copied before execution (Astra #286 r2 M1):
	// the comparison and the receipt's demand use this copy, never the
	// caller's object, which may change before Result.
	v.baseline = in.Baseline.snapshot()
	a, err := in.Host.NewAttempt()
	if err != nil {
		return v, err
	}
	defer func() { err = errors.Join(err, a.Teardown()) }()
	var module string
	if v.Tree, module, err = checkout(ctx, in.Repo, in.Tree, a.Workspace()); err != nil {
		return v, err
	}
	if v.Tree != in.Tree {
		return v, fmt.Errorf("%w: %s checks out tree %s, not a tree of its own", ErrSpec, in.Tree, v.Tree)
	}
	if err := writeFiles(a.Workspace(), in.Overlay); err != nil {
		return v, err
	}
	// The exit code and the run's output are never a gate's verdict; a run
	// refused before it started, or cut by its caller, is no verdict at
	// all. A run that left too much is judged by what it wrote: copy-out
	// bounds it again.
	ad := adapters[in.Gate.ResultAdapter]
	_, runErr := in.Host.Start(ctx, sandbox.Run{Image: in.Image, Mode: sandbox.Gate, Attempt: a, Limits: in.Limits, Argv: argv(in.Gate.Command, ad)})
	if (errors.Is(runErr, sandbox.ErrSpec) || errors.Is(runErr, sandbox.ErrHost)) && !errors.Is(runErr, sandbox.ErrLeft) {
		return v, runErr
	}
	if ctx.Err() != nil {
		return v, fmt.Errorf("gate: the run was cut: %w", ctx.Err())
	}
	out := filepath.Join(a.Dir, "out")
	if err := a.CopyOut(out, maxResults); err != nil {
		v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, "the run's results could not be copied out within their bounds")
		return v, nil
	}
	// Astra #286 H1: a candidate run cut by its timeout or a signal may have
	// written a complete-looking stream of what ran; what never ran is not
	// in it. An end that is not its own vetoes; the verdict is never made
	// from the exit code.
	zero, normal := ending(runErr)
	if in.Baseline != nil && !normal {
		v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, "the run did not finish (its timeout, a signal or the runner)")
		return v, nil
	}
	v.Stream, err = os.ReadFile(filepath.Join(out, ad.file))
	if err != nil {
		v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, "the run never wrote its results")
		return v, nil
	}
	var base *results.Report
	if v.baseline != nil {
		base = &v.baseline.Report
	}
	if v.Results, err = ad.parse(bytes.NewReader(v.Stream), in.Expected, base); err != nil {
		v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, "the run's results are not "+ad.what)
		return v, nil
	}
	v.judge(in, module, zero)
	return v, nil
}

// snapshot is a deep copy of b (nil for none).
func (b *Baseline) snapshot() *Baseline {
	if b == nil {
		return nil
	}
	c := *b
	c.Command, c.Results, c.Packages = slices.Clone(b.Command), slices.Clone(b.Results), maps.Clone(b.Packages)
	return &c
}

// ending is how a run ended: zero, on its own with exit status 0; normal,
// on its own, with a status a test failure gives (1-124), or 0 with the
// attempt over its disk bound (sandbox.ErrLeft). Anything else is cut:
// its timeout, a signal (an exit status of 128+n, as podman reports a
// killed container process, or a killed client), podman's own failures
// (125-127), or an error with no status.
func ending(runErr error) (zero, normal bool) {
	var exit *exec.ExitError
	switch {
	case runErr == nil:
		return true, true
	case errors.Is(runErr, context.DeadlineExceeded):
		return false, false
	case errors.As(runErr, &exit):
		return false, exit.ExitCode() >= 1 && exit.ExitCode() <= 124
	}
	return false, errors.Is(runErr, sandbox.ErrLeft)
}

// valid: b is a measured baseline (Astra #286 M2): every binding field
// present, its base a full tree id, and a green report with results, each
// result's package among its packages for go_test_json, and none for
// junit_xml (its adapter, by its report's shape).
func (b *Baseline) valid(adapter string) error {
	refuse := func(why string) error { return fmt.Errorf("%w: the baseline %s", ErrSpec, why) }
	switch {
	case len(b.Command) == 0 || !imageID.MatchString(b.ImageID) || b.OS == "" || b.Arch == "" || !objectID.MatchString(b.Base):
		return refuse("lacks a binding field (command, image id, OS, architecture, base tree)")
	case len(b.Results) == 0 || !b.Green():
		return refuse("holds no results, or is not green")
	}
	seen := map[results.Test]bool{}
	for _, x := range b.Results {
		_, pkg := b.Packages[x.Package]
		if seen[x.Test] || adapter == results.GoTestJSON && !pkg || adapter == results.JUnitXML && len(b.Packages) > 0 {
			return refuse("holds a test twice, a test of no package it reports, or (junit_xml) a package outcome")
		}
		seen[x.Test] = true
	}
	return nil
}

// Digest is b's content digest: its JSON encoding's sha256 (maps encode in
// key order).
func (b *Baseline) Digest() (types.Digest, error) {
	enc, err := json.Marshal(b)
	sum := sha256.Sum256(enc)
	return types.Digest("sha256:" + hex.EncodeToString(sum[:])), err
}

// check refuses a gate this package cannot run as asked: an image that is
// not the host's OS and architecture (Opus #290 H1: the binding part
// alone is a measured baseline's, wherever it is read), then bound.
func (in In) check() error {
	if in.Image.OS != runtime.GOOS || in.Image.Arch != runtime.GOARCH {
		return fmt.Errorf("%w: the image is %s/%s, the host %s/%s", ErrSpec, in.Image.OS, in.Image.Arch, runtime.GOOS, runtime.GOARCH)
	}
	return in.bound()
}

// bound refuses another adapter or ownership rules, an image of another
// platform than the gate environment's runtime (#226 entry 88), or a
// baseline measured with another command, platform image or base tree
// (Opus #286 M2, L3).
func (in In) bound() error {
	i := slices.IndexFunc(in.Gate.Environments, func(e types.ProjectProfileGateEnvironmentsItem) bool { return string(e.Name) == in.Env })
	if i < 0 {
		return fmt.Errorf("%w: no gate environment %q", ErrSpec, in.Env)
	}
	rt, b := in.Gate.Environments[i].Facts.Runtime, in.Baseline
	switch {
	case adapters[in.Gate.ResultAdapter].file == "" || in.Gate.OwnershipRules != ownership:
		return fmt.Errorf("%w: result adapter %q with ownership rules %q (only %s or %s, %s)", ErrSpec, in.Gate.ResultAdapter, in.Gate.OwnershipRules, results.GoTestJSON, results.JUnitXML, ownership)
	case adapters[in.Gate.ResultAdapter].pytest && direct(in.Gate.Command) != nil:
		return direct(in.Gate.Command)
	case rt.Os != in.Image.OS || rt.Arch != in.Image.Arch && rt.Arch != "any":
		return fmt.Errorf("%w: the image is %s/%s, the environment %s/%s", ErrSpec, in.Image.OS, in.Image.Arch, rt.Os, rt.Arch)
	case b != nil && (!slices.Equal(b.Command, in.Gate.Command) || b.ImageID != in.Image.ID || b.OS != in.Image.OS || b.Arch != in.Image.Arch):
		return fmt.Errorf("%w: the baseline was measured with another command or platform image", ErrSpec)
	case b != nil && b.Base != in.Base:
		return fmt.Errorf("%w: the baseline was measured on base tree %s, the candidate's base is %s", ErrSpec, b.Base, in.Base)
	case b != nil:
		return b.valid(in.Gate.ResultAdapter)
	}
	return nil
}

// judge decides v from its report under the default ownership rules; a
// measurement (no baseline) also needs the run to have ended on its own.
func (v *Verdict) judge(in In, module string, ended bool) {
	if in.Baseline == nil {
		switch {
		case !v.Results.Green():
			v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, "the base is not green: no baseline")
		case !ended:
			v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, "the base run did not finish: no baseline")
		default:
			v.set(vocab.Empty, vocab.ReasonCompletedClean, "the baseline is green")
		}
		return
	}
	junit, failed := in.Gate.ResultAdapter == results.JUnitXML, false
	for _, x := range v.Results.Results {
		failed = failed || junit && x.Outcome == results.Fail // a collection error too (Astra #290 M5)
	}
	touched := func(pkg string) bool {
		file, _, _ := strings.Cut(pkg, "::") // junit_xml: file::classname
		rel := strings.TrimPrefix(strings.TrimPrefix(pkg, module), "/")
		return failed || slices.ContainsFunc(in.Changed, func(c string) bool {
			switch {
			case !junit:
				return module != "" && strings.HasPrefix(pkg, module) && path.Dir(c) == cmpOr(rel, ".")
			case path.Base(c) == "conftest.py" || path.Base(c) == "__init__.py": // the files below it
				return path.Dir(c) == "." || strings.HasPrefix(file, path.Dir(c)+"/")
			}
			return c == file
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
// derived; for pytest, with --junitxml (unless named) and xunit1 forced.
func argv(command []string, ad adapter) []string {
	res := sandbox.ResultsDir + "/" + ad.file
	cmd, named := make([]string, len(command)), false
	for i, a := range command {
		cmd[i] = strings.ReplaceAll(a, placeholder, res)
		named = named || cmd[i] != a
	}
	if ad.pytest {
		if !named {
			cmd, named = append(cmd, "--junitxml="+res), true
		}
		cmd = append(cmd, "-o", "junit_family=xunit1")
	}
	if !named { // a stdout adapter: the controller writes its stream to the results file
		return append([]string{"sh", "-c", `exec "$@" >"$0"`, res}, cmd...)
	}
	return append([]string{"sh", "-c", `exec "$@" >/dev/null`, "sh"}, cmd...) // the run's stdout is never read: none reaches the runner's bound
}

// iniKeys are the ini options a gate command may set with -o.
var iniKeys = []string{"xfail_strict", "log_level", "console_output_style", "junit_logging"}

// direct refuses a pytest gate command that is not pytest run directly
// with allowlisted arguments: test paths or node ids inside the checkout,
// -q, -x, -p no:<plugin>, -k and -m expressions, --maxfail=<n>, -o for
// iniKeys, -n <workers>, and --junitxml={results_path}. Anything else
// (--rootdir, -c, --confcutdir, -p <plugin>, --pyargs, --import-mode, -W,
// another -o) is refused: it could move what pytest collects, and where.
func direct(cmd []string) error {
	refuse := func(why string) error { return fmt.Errorf("%w: the junit_xml gate command %s", ErrSpec, why) }
	var args []string
	switch {
	case len(cmd) > 0 && cmd[0] == "pytest":
		args = cmd[1:]
	case len(cmd) > 2 && (cmd[0] == "python" || cmd[0] == "python3") && cmd[1] == "-m" && cmd[2] == "pytest":
		args = cmd[3:]
	default:
		return refuse("is not pytest, or python -m pytest, run directly")
	}
	for i := 0; i < len(args); i++ {
		a, next := args[i], ""
		if i+1 < len(args) {
			next = args[i+1]
		}
		file, _, _ := strings.Cut(a, "::")
		switch _, nerr := strconv.Atoi(strings.TrimPrefix(a, "--maxfail=")); {
		case a == "-q" || a == "-x" || a == "--junitxml="+placeholder || strings.HasPrefix(a, "--maxfail=") && nerr == nil:
		case a == "-p" && strings.HasPrefix(next, "no:"), a == "-k" && next != "", a == "-m" && next != "":
			i++
		case a == "-o" && slices.Contains(iniKeys, strings.SplitN(next, "=", 2)[0]):
			i++
		case a == "-n":
			if _, err := strconv.Atoi(next); err != nil {
				return refuse("has -n without a worker count")
			}
			i++
		case !strings.HasPrefix(a, "-") && filepath.IsLocal(file) && path.Clean(file) == file && !strings.Contains(a, `\`):
		default:
			return refuse(fmt.Sprintf("has an argument outside its allowlist (%.64q)", a))
		}
	}
	return nil
}

// checkout writes tree into ws through a temporary index and returns the
// tree id the index holds and the module path of its root go.mod ("" for
// none). go.mod is read as its bounded git blob, never through the
// workspace (Astra #286 M4: a symlink there would point anywhere on the
// controller); a go.mod that is not a regular file is refused.
func checkout(ctx context.Context, repo sandbox.Repo, tree, ws string) (id, module string, err error) {
	idx, err := os.MkdirTemp("", "adc-gate-")
	if err != nil {
		return "", "", err
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
		return "", "", err
	}
	if _, err := git("checkout", "--", "."); err != nil {
		return "", "", err
	}
	out, err := git("write-tree")
	if err != nil {
		return "", "", err
	}
	id = strings.TrimSpace(string(out))
	entry, err := git("ls-files", "--stage", "--", "go.mod")
	if err != nil || len(entry) == 0 {
		return id, "", err
	}
	f := strings.Fields(string(entry)) // mode, blob, stage, path
	if len(f) < 2 || f[0] != "100644" && f[0] != "100755" || !objectID.MatchString(f[1]) {
		return "", "", fmt.Errorf("%w: the root go.mod is not a regular file", ErrSpec)
	}
	size, err := git("cat-file", "-s", f[1])
	if n, perr := strconv.Atoi(strings.TrimSpace(string(size))); err != nil || perr != nil || n > maxGoMod {
		return "", "", errors.Join(err, perr, fmt.Errorf("%w: the root go.mod is over %d bytes", ErrSpec, maxGoMod))
	}
	b, err := git("cat-file", "blob", f[1])
	if err != nil {
		return "", "", err
	}
	for line := range strings.Lines(string(b)) {
		if m, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return id, strings.Trim(strings.TrimSpace(m), `"`), nil
		}
	}
	return id, "", nil
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
