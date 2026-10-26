// SPDX-License-Identifier: Apache-2.0

// Package referee is adc dev referee (stage 2, P-A): it judges an outside
// agent's patch on an item (schemas/testing/referee-item.json) with nothing
// per language. The item supplies its image, its setup and test commands,
// where its results are (JUnit XML or go test -json), its hidden acceptance
// and the acceptance's test ids; the referee adds nothing per language.
//
// The base is exported from a controller-side clone (sandbox.Host.Clone) into
// a fresh workspace; the setup step alone runs with the network, before
// anything hidden is in the workspace. The patch is checked by accept (no
// unsafe path, symlink, gitlink or git control path; here, no acceptance
// path and no results path either). Three runs then start, each in a fresh
// copy, in rootless Podman with the sandbox's confinement and no network:
// base (B, visible tests only), hidden (H, the acceptance overlaid) and
// candidate (C, the patch applied, then every path it touched outside the
// item's editable globs, and the item's protected paths, its harness, inside
// them, restored to the base's (each one the patch changed back to the
// base's version, each file it added there removed), then the acceptance
// overlaid). So a patch's edit of a test hook (conftest.py), a runner's
// configuration (addopts, phpunit.xml), a test file or a dependency, or a new
// file outside the solution's paths (a root pytest.py or sitecustomize.py
// shadowing the runner), never runs; the record lists the protected paths
// and the paths outside editable it touched for audit.
//
// Residual: C still runs the candidate's own code, in the same process as
// the tests. Code under test can forge results from inside (write the
// results file at exit, print framed test2json lines, patch the assertion
// library at import). The editable and protected sets close the harness
// routes only.
//
// Verdict: invalid_item when B reports no test at all, when B reports a
// hidden test (it is not hidden), or when every hidden test passes in H (an
// acceptance that passes at base proves nothing; some of its tests may
// guard what the base already does); accepted when C completed (no timeout
// or crash; a non-zero exit only with failures reported), every hidden test
// is reported in C and passes, and every test that passed in B is reported
// in C and passes. Reasons are fixed texts naming test ids or paths; no
// acceptance file's content is ever printed. With a logs directory, each
// run's stdout and the tail of its stderr are written there (they may hold
// the hidden tests' names and output: the directory is private).
package referee

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/accept"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/profile"
	"github.com/ekaynac/auto-dev-cycle/internal/regularfile"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// Verdict is the referee's outcome.
type Verdict string

const (
	Accepted    Verdict = "accepted"
	Rejected    Verdict = "rejected"
	InvalidItem Verdict = "invalid_item"
	CouldNotRun Verdict = "could_not_run"
)

// Bounds on what the referee reads.
const (
	MaxItem       = 1 << 20
	MaxPatch      = 64 << 20
	maxAcceptance = 1 << 20
	maxReasons    = 100
)

// Run is one container run's summary.
type Run struct {
	Tests    int     `json:"tests"`
	Passed   int     `json:"passed"`
	Failed   int     `json:"failed"`
	Skipped  int     `json:"skipped"`
	Exit     int     `json:"exit"` // -1: none (timed out, or not started)
	TimedOut bool    `json:"timed_out"`
	Seconds  float64 `json:"seconds"`
}

// Report is the verdict record.
type Report struct {
	Verdict     Verdict         `json:"verdict"`
	Reasons     []string        `json:"reasons"`
	Image       string          `json:"image,omitempty"`
	Base        string          `json:"base,omitempty"`
	PatchSHA256 string          `json:"patch_sha256"`
	ItemSHA256  string          `json:"item_sha256"`
	Runs        map[string]*Run `json:"runs"` // setup, base, hidden, candidate
	// ProtectedTouched are the protected paths the patch changed, added or
	// deleted: the candidate run had the base's versions instead.
	ProtectedTouched []string `json:"protected_touched"`
	// OutsideEditable are the paths outside the item's editable globs the
	// patch changed, added or deleted: the candidate run had the base's
	// versions instead (none where the base had none).
	OutsideEditable []string `json:"outside_editable"`
	Seconds         float64  `json:"seconds"`
}

// In is one judgement: the item's bytes and the directory its relative
// paths are taken from, the patch, the podman binary, and a private
// directory for the runs' logs ("" for none).
type In struct {
	Podman  string
	Item    []byte
	ItemDir string
	Patch   []byte
	Logs    string
}

// finding ends a judgement with a verdict and its fixed reasons.
type finding struct {
	verdict Verdict
	reasons []string
}

func (f *finding) Error() string { return string(f.verdict) }

func found(v Verdict, format string, a ...any) error {
	return &finding{v, []string{fmt.Sprintf(format, a...)}}
}

// Judge judges in. Every failure is a verdict: could_not_run when the
// referee itself could not complete.
func Judge(ctx context.Context, in In) Report {
	start := time.Now()
	r := Report{Verdict: Accepted, PatchSHA256: digest(in.Patch), ItemSHA256: digest(in.Item), Runs: map[string]*Run{}, Reasons: []string{}, ProtectedTouched: []string{}, OutsideEditable: []string{}}
	var f *finding
	if err := judge(ctx, in, &r); errors.As(err, &f) {
		r.Verdict, r.Reasons = f.verdict, slices.Sorted(slices.Values(f.reasons))
	} else if err != nil {
		r.Verdict, r.Reasons = CouldNotRun, []string{"the referee could not complete"}
	}
	if n := len(r.Reasons); n > maxReasons {
		r.Reasons = append(r.Reasons[:maxReasons], fmt.Sprintf("and %d more", n-maxReasons))
	}
	r.Seconds = time.Since(start).Seconds()
	return r
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// ran is a base or hidden run's error: could_not_run unless the run's
// preparation found otherwise.
func ran(x result, err error, name string) error {
	switch {
	case err != nil && !errors.Is(err, errPodman):
		return err
	case err != nil || !x.completed(false):
		return found(CouldNotRun, "the %s run did not complete", name)
	}
	return nil
}

// judge judges in into r: nil when the candidate is accepted.
func judge(ctx context.Context, in In, r *Report) error {
	it, err := contract.Decode[types.RefereeItem]("testing/referee-item", in.Item)
	if err != nil {
		return found(InvalidItem, "the item does not fit schemas/testing/referee-item.json")
	}
	r.Image, r.Base = string(it.Image), it.Base
	files, err := acceptance(it, in.ItemDir)
	if err != nil {
		return err
	}
	if res := string(it.Results.Path); res != "stdout" && !accept.Plain(res) {
		return found(InvalidItem, "the results path is not a plain repository path")
	}
	protected, err := profile.Matcher(it.Protected)
	if err != nil {
		return found(InvalidItem, "a protected glob is not a repository path pattern")
	}
	editable, err := profile.Matcher(it.Editable)
	if err != nil {
		return found(InvalidItem, "an editable glob is not a repository path pattern")
	}
	tmp, err := os.MkdirTemp("", "adc-referee-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if tmp, err = filepath.EvalSymlinks(tmp); err != nil { // the clone takes canonical paths only
		return err
	}
	w := &work{podman: in.Podman, item: it, logs: in.Logs, protected: protected, editable: editable, host: sandbox.Host{Podman: in.Podman, Root: filepath.Join(tmp, "sandbox")}}
	defer w.teardown()
	if err := w.prepare(ctx, filepath.Join(tmp, "stage"), filepath.Join(tmp, "repo"), abs(in.ItemDir, it.Repo)); err != nil {
		return err
	}
	err = w.check(ctx, in.Patch, files)
	r.ProtectedTouched = append(r.ProtectedTouched, slices.Compact(slices.Sorted(slices.Values(w.touched)))...)
	r.OutsideEditable = append(r.OutsideEditable, slices.Compact(slices.Sorted(slices.Values(w.outside)))...)
	if err != nil {
		return err
	}
	if err := w.setup(ctx, r); err != nil {
		return err
	}

	b, err := w.test(ctx, r, "base", nil, nil)
	if err := ran(b, err, "base"); err != nil {
		return err
	}
	if len(b.by) == 0 {
		return found(InvalidItem, "the base run reports no test")
	}
	h, err := w.test(ctx, r, "hidden", nil, files)
	if err := ran(h, err, "hidden"); err != nil {
		return err
	}
	red := false
	for _, x := range it.HiddenTests {
		if _, ok := b.by[x]; ok {
			return found(InvalidItem, "a hidden test is reported without the acceptance: %s", x)
		}
		red = red || h.by[x] != pass
	}
	if !red {
		return found(InvalidItem, "every hidden test passes at the base")
	}
	c, err := w.test(ctx, r, "candidate", in.Patch, files)
	switch {
	case errors.Is(err, errPodman):
		return found(CouldNotRun, "the candidate run did not start")
	case err != nil:
		return err
	case c.timedOut:
		return found(Rejected, "the candidate run timed out")
	case c.err != nil:
		return found(Rejected, "the candidate run's results are unreadable")
	case !c.completed(true):
		return found(Rejected, "the candidate run did not complete: exit %d", c.exit)
	}
	var reasons []string
	for _, x := range it.HiddenTests {
		reasons = append(reasons, why(c, x, "hidden")...)
	}
	for x, o := range b.by {
		if o == pass {
			reasons = append(reasons, why(c, x, "visible")...)
		}
	}
	if len(reasons) > 0 {
		return &finding{Rejected, reasons}
	}
	return nil
}

// why is why test x of the given kind does not pass in c, if it does not.
func why(c result, x, kind string) []string {
	switch c.by[x] {
	case pass:
		return nil
	case 0:
		return []string{fmt.Sprintf("%s test not reported in the candidate run: %s", kind, x)}
	case skip:
		return []string{fmt.Sprintf("%s test skipped in the candidate run: %s", kind, x)}
	}
	return []string{fmt.Sprintf("%s test failing in the candidate run: %s", kind, x)}
}

// overlay is one acceptance file: its repository path and its content.
type overlay struct {
	path string
	data []byte
}

// acceptance reads the item's acceptance files, each at a plain path no
// other one takes.
func acceptance(it types.RefereeItem, dir string) ([]overlay, error) {
	var out []overlay
	for _, a := range it.Acceptance {
		p := string(a.Path)
		if !accept.Plain(p) || slices.ContainsFunc(out, func(o overlay) bool { return o.path == p }) {
			return nil, found(InvalidItem, "an acceptance path is not a plain repository path, or is listed twice: %s", p)
		}
		b, err := ReadFile(abs(dir, a.File), maxAcceptance)
		if err != nil {
			return nil, found(InvalidItem, "an acceptance file cannot be read: %s", p)
		}
		out = append(out, overlay{p, b})
	}
	return out, nil
}

func abs(dir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

// ReadFile reads the regular file p, never through a final symlink, of at
// most max bytes.
func ReadFile(p string, max int64) ([]byte, error) {
	f, err := os.OpenFile(p, regularfile.Flags, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := regularfile.Check(f, nil); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err == nil && int64(len(b)) > max {
		err = fmt.Errorf("referee: %s is over %d bytes", filepath.Base(p), max)
	}
	return b, err
}
