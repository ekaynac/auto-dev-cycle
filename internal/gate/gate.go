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
//   - The cwd contract (#275) is P15b's, with the junit_xml (pytest)
//     adapter: a Go gate runs no Python probe (go test runs each test
//     binary in its package directory, Opus #286 M3).
//   - Acceptance tests are overlaid on the gate workspace only, each at its
//     repository path, replacing what the candidate put there (a file, a
//     link or a directory at it, or a non-directory where one of its
//     directories should be). The top-level tests their Go test files
//     declare are expected (AcceptanceTests): one the run does not report
//     is the candidate's missing test, never no result (#226 entry 190).
//     An acceptance go test ./... could never run is the task's, refused
//     before any run (ErrAcceptance; Opus #429 H1, M1, L1): a Go test file
//     that does not parse, under a testdata, vendor, _ or . directory, or
//     that the gate environment's platform excludes (go/build's rules: its
//     name and its build constraints), a go.mod or go.work among it, or no
//     expected test at all. A candidate tree with no Go module is the
//     candidate's (found/gate_test_failed): its hidden tests cannot run.
//   - A hidden test's result is only as sound as accept's forgery guard
//     (Opus #429 L2): the gate reads the stream as the run wrote it, so a
//     candidate whose code printed a forged stream would pass it; accept
//     blocks such code (an initialisation it cannot resolve, os.Exit,
//     os.Stdout), and the gate runs only the tree accept accepted.
package gate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/mod/modfile"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/results"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	resultsFile = "go-test.json"    // the derived results file, in the results mount
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
)

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
	overlaid bool      // Run applied an acceptance overlay: Result needs its digests (Opus #321 M2)
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
	v.overlaid = len(in.Overlay) > 0
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
	expected := in.Expected
	if len(in.Overlay) > 0 {
		hidden, err := AcceptanceTests(in.Image.OS, in.Image.Arch, in.Overlay)
		if err != nil {
			return v, err
		}
		if module == "" { // the candidate removed the module its hidden tests are in (Opus #429 L1)
			v.Stream = []byte{}
			v.set(vocab.Found, vocab.ReasonGateTestFailed, whyNoModule)
			return v, nil
		}
		expected = slices.Clone(expected)
		for _, h := range hidden {
			expected = append(expected, results.Test{Package: strings.TrimSuffix(module+"/"+h.Dir, "/."), Name: h.Name})
		}
	}
	if err := writeFiles(a.Workspace(), in.Overlay); err != nil {
		return v, err
	}
	// The exit code and the run's output are never a gate's verdict; a run
	// refused before it started, or cut by its caller, is no verdict at
	// all. A run that left too much is judged by what it wrote: copy-out
	// bounds it again.
	_, runErr := in.Host.Start(ctx, sandbox.Run{Image: in.Image, Mode: sandbox.Gate, Attempt: a, Limits: in.Limits, Argv: argv(in.Gate.Command)})
	if (errors.Is(runErr, sandbox.ErrSpec) || errors.Is(runErr, sandbox.ErrHost)) && !errors.Is(runErr, sandbox.ErrLeft) {
		return v, runErr
	}
	if ctx.Err() != nil {
		return v, fmt.Errorf("gate: the run was cut: %w", ctx.Err())
	}
	out := filepath.Join(a.Dir, "out")
	if err := a.CopyOut(out, maxResults); err != nil {
		v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, whyCopyOut)
		return v, nil
	}
	// Astra #286 H1: a candidate run cut by its timeout or a signal may have
	// written a complete-looking stream of what ran; what never ran is not
	// in it. An end that is not its own vetoes; the verdict is never made
	// from the exit code.
	zero, normal := ending(runErr)
	if in.Baseline != nil && !normal {
		v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, whyUnfinished)
		return v, nil
	}
	v.Stream, err = os.ReadFile(filepath.Join(out, resultsFile))
	if err != nil {
		v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, whyNoResults)
		return v, nil
	}
	var base *results.Report
	if v.baseline != nil {
		base = &v.baseline.Report
	}
	if v.Results, err = results.ParseGoTestJSON(bytes.NewReader(v.Stream), expected, base); err != nil {
		v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, whyNotStream)
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
// result's package among its packages for go_test_json.
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
		if seen[x.Test] || adapter == results.GoTestJSON && !pkg {
			return refuse("holds a test twice, or a test of no package it reports")
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

// check refuses a gate this package cannot run as asked: another adapter
// or ownership rules, an image of another platform than the gate
// environment's runtime (#226 entry 88), or a baseline measured with
// another command, platform image or base tree (Opus #286 M2, L3).
func (in In) check() error {
	i := slices.IndexFunc(in.Gate.Environments, func(e types.ProjectProfileGateEnvironmentsItem) bool { return string(e.Name) == in.Env })
	if i < 0 {
		return fmt.Errorf("%w: no gate environment %q", ErrSpec, in.Env)
	}
	rt, b := in.Gate.Environments[i].Facts.Runtime, in.Baseline
	switch {
	case in.Gate.ResultAdapter != results.GoTestJSON || in.Gate.OwnershipRules != ownership:
		return fmt.Errorf("%w: result adapter %q with ownership rules %q (only %s, %s)", ErrSpec, in.Gate.ResultAdapter, in.Gate.OwnershipRules, results.GoTestJSON, ownership)
	case rt.Os != in.Image.OS || rt.Arch != in.Image.Arch:
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
			v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, whyBaseRed)
		case !ended:
			v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, whyBaseUnfinished)
		default:
			v.set(vocab.Empty, vocab.ReasonCompletedClean, whyBaseGreen)
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
		v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, fmt.Sprintf(whyTheirs, theirs, mine))
	case mine > 0:
		v.set(vocab.Found, vocab.ReasonGateTestFailed, fmt.Sprintf(whyFailing, mine))
	case !v.Results.Clean():
		v.set(vocab.CouldNotRun, vocab.ReasonRunnerFault, whyNonePassed)
	default:
		v.set(vocab.Empty, vocab.ReasonCompletedClean, fmt.Sprintf(whyPassing, len(v.Results.Results)))
	}
}

// The controller's phrases, a verdict's Why: fixed, or with counts (%d).
// CheckReport takes a report's detail only when it is one of them
// (Mythos #348 L1: no free text of another source).
const (
	whyCopyOut        = "the run's results could not be copied out within their bounds"
	whyUnfinished     = "the run did not finish (its timeout, a signal or the runner)"
	whyNoResults      = "the run never wrote its results"
	whyNotStream      = "the run's results are not a go test -json stream"
	whyBaseRed        = "the base is not green: no baseline"
	whyBaseUnfinished = "the base run did not finish: no baseline"
	whyBaseGreen      = "the baseline is green"
	whyTheirs         = "%d results missing or incomplete outside the candidate's packages (and %d of the candidate's)"
	whyFailing        = "%d tests or packages fail against the baseline"
	whyNonePassed     = "no test passed"
	whyPassing        = "%d tests pass against the baseline"
	whyNoModule       = "the tree has no Go module for its acceptance tests"
)

var whys = []string{whyCopyOut, whyUnfinished, whyNoResults, whyNotStream, whyBaseRed, whyBaseUnfinished, whyBaseGreen, whyTheirs, whyFailing, whyNonePassed, whyPassing, whyNoModule}

// isWhy is whether s is one of the controller's phrases, each %d a count:
// 1 to 19 decimal digits.
func isWhy(s string) bool {
	return slices.ContainsFunc(whys, func(f string) bool { return phrased(f, s) })
}

// phrased is whether s is the phrase f with each %d a count.
func phrased(f, s string) bool {
	for f != "" {
		if rest, ok := strings.CutPrefix(f, "%d"); ok {
			n := 0
			for n < len(s) && n < 19 && '0' <= s[n] && s[n] <= '9' {
				n++
			}
			if n == 0 {
				return false
			}
			f, s = rest, s[n:]
			continue
		}
		if s == "" || f[0] != s[0] {
			return false
		}
		f, s = f[1:], s[1:]
	}
	return s == ""
}

func cmpOr(a, b string) string {
	if a == "" {
		return b
	}
	return a
}

// argv is the command the container runs: the profile's, its results path
// derived.
func argv(command []string) []string {
	res := sandbox.ResultsDir + "/" + resultsFile
	cmd, named := make([]string, len(command)), false
	for i, a := range command {
		cmd[i] = strings.ReplaceAll(a, placeholder, res)
		named = named || cmd[i] != a
	}
	if !named { // a stdout adapter: the controller writes its stream to the results file
		return append([]string{"sh", "-c", `exec "$@" >"$0"`, res}, cmd...)
	}
	return append([]string{"sh", "-c", `exec "$@" >/dev/null`, "sh"}, cmd...) // the run's stdout is never read: none reaches the runner's bound
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
	return id, modfile.ModulePath(b), nil // the go command's own reading: quoted, commented (Opus #429 H1)
}

// ErrAcceptance: a task's acceptance the gate cannot run as its hidden
// tests (AcceptanceTests): the task's, no retry or profile change fixes it.
var ErrAcceptance = errors.New("gate: the task's acceptance cannot be run")

// AcceptanceTest is a top-level test an acceptance's Go test file
// declares: its directory (the repository path; "." the root) and name.
type AcceptanceTest struct{ Dir, Name string }

// AcceptanceTests are the top-level tests (func TestXxx(*testing.T)) the Go
// test files of overlay declare, sorted by file, which go test ./... runs
// on goos/goarch. It refuses (ErrAcceptance) what go test ./... would never
// run, so it is never expected: a Go test file that does not parse, in a
// directory go ignores (testdata, vendor, or one starting with _ or .),
// one go/build excludes on the platform (its name's _GOOS/_GOARCH, its
// build constraints: accept's go/build context, with the file's own
// bytes), a go.mod or go.work among overlay (a module of its own); and an
// acceptance declaring no such test at all (Opus #429 M1). Files that are
// not Go test files beside a test are data, and stay.
func AcceptanceTests(goos, goarch string, overlay map[string][]byte) ([]AcceptanceTest, error) {
	var out []AcceptanceTest
	for _, p := range slices.Sorted(maps.Keys(overlay)) {
		if b := path.Base(p); b == "go.mod" || b == "go.work" {
			return nil, fmt.Errorf("%w: %s would make a module of its own", ErrAcceptance, p)
		}
		if !strings.HasSuffix(p, "_test.go") {
			continue
		}
		segs := strings.Split(p, "/")
		for i, seg := range segs {
			if strings.HasPrefix(seg, "_") || strings.HasPrefix(seg, ".") || i < len(segs)-1 && (seg == "testdata" || seg == "vendor") {
				return nil, fmt.Errorf("%w: the acceptance test %s is where go test ./... never looks", ErrAcceptance, p)
			}
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, overlay[p], parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("%w: the acceptance test %s does not parse", ErrAcceptance, p)
		}
		src := overlay[p]
		bc := build.Context{GOOS: goos, GOARCH: goarch, Compiler: "gc", ReleaseTags: build.Default.ReleaseTags,
			OpenFile: func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(src)), nil }}
		if ok, err := bc.MatchFile(path.Dir(p), path.Base(p)); err != nil || !ok {
			return nil, fmt.Errorf("%w: the acceptance test %s is not built on %s/%s", ErrAcceptance, p, goos, goarch)
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && goTest(fn) {
				out = append(out, AcceptanceTest{Dir: path.Dir(p), Name: fn.Name.Name})
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no Go test file of it declares a test go test runs", ErrAcceptance)
	}
	return out, nil
}

// goTest reports whether fn is a test go test runs: TestXxx, Xxx not
// starting with a lower-case letter, taking one *testing.T.
func goTest(fn *ast.FuncDecl) bool {
	rest, ok := strings.CutPrefix(fn.Name.Name, "Test")
	if !ok || rest != "" && unicode.IsLower([]rune(rest)[0]) || fn.Type.TypeParams != nil || len(fn.Type.Params.List) != 1 || len(fn.Type.Params.List[0].Names) > 1 {
		return false
	}
	star, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "T"
}

// writeFiles overlays files on ws: each a clean relative path, inside it,
// replacing what is there: a file, a link or a directory at it, and a
// non-directory (a file, a link) where one of its directories should be
// (Opus #429 M3). The daemon has checked the base holds nothing there, so
// all of it is the candidate's.
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
		for d := range parents(p) {
			if fi, err := root.Lstat(d); err == nil && !fi.IsDir() {
				if err := root.Remove(d); err != nil {
					return err
				}
			} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		if err := root.MkdirAll(path.Dir(p), 0o700); err != nil {
			return err
		}
		if err := root.RemoveAll(p); err != nil {
			return err
		}
		if err := root.WriteFile(p, b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// parents are p's directories, from the top (a/b/c: a, a/b).
func parents(p string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for i := range len(p) {
			if p[i] == '/' && !yield(p[:i]) {
				return
			}
		}
	}
}
