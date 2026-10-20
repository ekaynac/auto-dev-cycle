// SPDX-License-Identifier: Apache-2.0

// Package qualify is reviewer qualification (D56; P31): a reviewer route is
// qualified when, on the suite under fixtures/m1/review-qualification/, it
// returns no_go on every planted defect and go on at least three of the
// four clean controls, and a committed record of that run
// (adc.review-qualification/v1) still binds the route, the prompt
// (review.PromptDigest) and the suite. P22a's activation of the review
// capability calls Check (the I-inventory's obligation P22a-6).
//
// Decisions (the lead, 2026-10-20):
//   - Check fails closed: no record, a stale route, prompt or suite digest,
//     a report file missing or not its digest, or a score under the bar
//     each leave the route unqualified. The score is recomputed from the
//     cases; the record's own counts and verdict must agree with it.
//   - The run goes through the real http-chat adapter and gateway
//     (Route.Review, adapterhost.Host.Run); a live run is the lead's,
//     committed as a record. CI runs the harness against a scripted
//     upstream only: CI cannot qualify a model.
//   - A reviewer sees review.Prompt only: the task's requirement, the
//     candidate's diff and the gate's summary. Never a case's id, the
//     suite's notes, the sealed probes or the task's hidden tests.
package qualify

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/review"
)

// Dir is the suite's directory, from the repository root.
const Dir = "fixtures/m1/review-qualification"

const recordSchema = "adc.review-qualification/v1"

var caseID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,95}$`)

// Case is one candidate of the suite: a patch of its task's repository.
type Case struct {
	ID     string `json:"id"`
	Task   string `json:"task"`   // the project's task the candidate is for
	Expect string `json:"expect"` // go (a clean control) or no_go (a planted defect)
	Defect string `json:"defect,omitempty"`
}

// Suite is the qualification suite, read from root's Dir.
type Suite struct {
	Project string `json:"project"` // the project's fixture directory, from the root
	Pass    struct {
		NoGo int `json:"no_go"`
		Go   int `json:"go"`
	} `json:"pass"`
	Cases  []Case       `json:"cases"`
	Root   string       `json:"-"`
	Digest types.Digest `json:"-"` // suite.json, every case's patch and every sealed file
}

// Load reads the suite under root, strictly: every planted defect counts
// toward the bar (no_go equals their number) and at least the bar's
// controls exist.
func Load(root string) (Suite, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Suite{}, err
	}
	dir := filepath.Join(root, Dir)
	raw, err := os.ReadFile(filepath.Join(dir, "suite.json"))
	if err != nil {
		return Suite{}, err
	}
	var s Suite
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return Suite{}, fmt.Errorf("qualify: suite.json: %w", err)
	}
	s.Root = root
	defects, controls, seen := 0, 0, map[string]bool{}
	for _, c := range s.Cases {
		if !caseID.MatchString(c.ID) || seen[c.ID] || (c.Expect == review.NoGo) != (c.Defect != "") || c.Expect != review.Go && c.Expect != review.NoGo {
			return Suite{}, fmt.Errorf("qualify: case %q is not one", c.ID)
		} else if _, err := os.Stat(filepath.Join(s.taskDir(c), "task.json")); err != nil {
			return Suite{}, fmt.Errorf("qualify: case %s: %w", c.ID, err)
		}
		seen[c.ID] = true
		defects, controls = defects+b2i(c.Expect == review.NoGo), controls+b2i(c.Expect == review.Go)
	}
	if s.Pass.NoGo < 1 || defects != s.Pass.NoGo || s.Pass.Go < 1 || controls < s.Pass.Go {
		return Suite{}, fmt.Errorf("qualify: the bar (%d no_go, %d go) does not fit %d defects and %d controls", s.Pass.NoGo, s.Pass.Go, defects, controls)
	}
	if s.Digest, err = digestDir(dir); err != nil {
		return Suite{}, err
	}
	return s, nil
}

// digestDir is the digest of every file under dir but records/ and the
// README, by path and content.
func digestDir(dir string) (types.Digest, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(dir, p)
		switch {
		case err != nil:
			return err
		case e.IsDir() && rel == "records":
			return filepath.SkipDir
		case e.IsDir() || rel == "README.md":
			return nil
		case !e.Type().IsRegular():
			return fmt.Errorf("qualify: %s is not a regular file", rel)
		}
		b, err := os.ReadFile(p)
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(b))
		h.Write(b)
		return err
	})
	return types.Digest("sha256:" + hex.EncodeToString(h.Sum(nil))), err
}

func (s Suite) taskDir(c Case) string { return filepath.Join(s.Root, s.Project, "tasks", c.Task) }

// Patch is case c's candidate patch.
func (s Suite) Patch(c Case) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.Root, Dir, "cases", c.ID, "candidate.patch"))
}

// Requirement is the requirement of case c's task.
func (s Suite) Requirement(c Case) (string, error) {
	raw, err := os.ReadFile(filepath.Join(s.taskDir(c), "task.json"))
	if err != nil {
		return "", err
	}
	var t struct {
		Requirement string `json:"requirement"`
	}
	if err := json.Unmarshal(raw, &t); err != nil || t.Requirement == "" {
		return "", fmt.Errorf("qualify: task %s has no requirement: %v", c.Task, err)
	}
	return t.Requirement, nil
}

// Score is the run's counts: the defects decided no_go and the controls
// decided go; qualified when both meet the suite's bar. A case without a
// decision is a miss.
func (s Suite) Score(decided map[string]string) (noGo, goes int, qualified bool) {
	for _, c := range s.Cases {
		if decided[c.ID] == c.Expect {
			noGo, goes = noGo+b2i(c.Expect == review.NoGo), goes+b2i(c.Expect == review.Go)
		}
	}
	return noGo, goes, noGo >= s.Pass.NoGo && goes >= s.Pass.Go
}

// RouteOf is a registry reviewer executor's route.
func RouteOf(e types.RegistryExecutor) types.QualifiedRoute {
	return types.QualifiedRoute{Executor: e.ID, Family: e.Family, Adapter: e.Adapter, Model: e.Model, Destination: e.Destination.ID, Endpoint: e.Destination.Endpoint}
}

// Outcome is one case's run: the decision (review.Go, review.NoGo or "",
// a miss) and its D55 report.
type Outcome struct {
	Decision string
	Report   []byte
}

// Record is the record of the suite's run on route, at, with each case's
// outcome; it writes each report beside it, under dir/<name>/.
func (s Suite) Record(dir, name string, route types.QualifiedRoute, at time.Time, out map[string]Outcome) (types.ReviewQualification, error) {
	rec := types.ReviewQualification{Schema: recordSchema, Route: route, PromptDigest: review.PromptDigest, SuiteDigest: s.Digest,
		RanAt: types.Time(at.UTC().Format(time.RFC3339)), Cases: []types.QualifiedCase{}}
	decided := map[string]string{}
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		return rec, err
	}
	for _, c := range s.Cases {
		o, ok := out[c.ID]
		if !ok || len(o.Report) == 0 {
			return rec, fmt.Errorf("qualify: case %s has no outcome", c.ID)
		}
		decided[c.ID] = o.Decision
		if err := os.WriteFile(filepath.Join(dir, name, c.ID+".review-report.json"), o.Report, 0o644); err != nil {
			return rec, err
		}
		rec.Cases = append(rec.Cases, types.QualifiedCase{ID: c.ID, Expect: types.QualifiedCaseExpect(c.Expect),
			Decision: types.QualifiedCaseDecision(cmp.Or(o.Decision, "none")), ReportDigest: digest(o.Report)})
	}
	noGo, goes, ok := s.Score(decided)
	rec.NoGoOnDefects, rec.GoOnControls, rec.Qualified = int64(noGo), int64(goes), ok
	b, err := json.MarshalIndent(rec, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, name+".json"), append(b, '\n'), 0o644)
	}
	return rec, err
}

// ErrUnqualified: no record qualifies the route.
var ErrUnqualified = errors.New("qualify: the reviewer route is not qualified")

// Check is nil when a record under dir qualifies route for the suite s and
// the current prompt; otherwise ErrUnqualified, with each record's reason.
func Check(dir string, route types.QualifiedRoute, s Suite) error {
	names, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}
	var why []string
	for _, p := range names {
		if err := check(p, route, s); err != nil {
			why = append(why, filepath.Base(p)+": "+err.Error())
		} else {
			return nil
		}
	}
	if len(why) == 0 {
		why = []string{"no record"}
	}
	return fmt.Errorf("%w: %s", ErrUnqualified, strings.Join(why, "; "))
}

func check(p string, route types.QualifiedRoute, s Suite) error {
	raw, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	rec, err := contract.Decode[types.ReviewQualification]("config/review-qualification", raw)
	switch {
	case err != nil:
		return err
	case rec.Route != route:
		return errors.New("another route")
	case rec.PromptDigest != review.PromptDigest:
		return errors.New("another prompt")
	case rec.SuiteDigest != s.Digest:
		return errors.New("another suite")
	case len(rec.Cases) != len(s.Cases):
		return errors.New("not the suite's cases")
	}
	decided := map[string]string{}
	for i, c := range s.Cases {
		rc := rec.Cases[i]
		if rc.ID != c.ID || string(rc.Expect) != c.Expect {
			return fmt.Errorf("case %d is not the suite's %s", i, c.ID)
		}
		b, err := os.ReadFile(filepath.Join(strings.TrimSuffix(p, ".json"), c.ID+".review-report.json"))
		if err != nil || digest(b) != rc.ReportDigest {
			return fmt.Errorf("case %s's report is missing or not its digest", c.ID)
		}
		if rc.Decision != types.QualifiedCaseDecisionNone {
			decided[c.ID] = string(rc.Decision)
		}
	}
	noGo, goes, ok := s.Score(decided)
	switch {
	case int64(noGo) != rec.NoGoOnDefects || int64(goes) != rec.GoOnControls || ok != rec.Qualified:
		return errors.New("its counts are not its cases'")
	case !ok:
		return fmt.Errorf("under the bar: %d of %d defects no_go, %d go", noGo, s.Pass.NoGo, goes)
	}
	return nil
}

func digest(b []byte) types.Digest {
	sum := sha256.Sum256(b)
	return types.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
