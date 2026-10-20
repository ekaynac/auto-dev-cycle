// SPDX-License-Identifier: Apache-2.0

// Package qualify is reviewer qualification (D56; P31): a reviewer route is
// qualified when, on the suite under fixtures/m1/review-qualification/, it
// returns no_go on every planted defect and go on at least three of the
// four clean controls, and the latest committed record of a run on that
// route (adc.review-qualification/v1) still binds the route, the prompt
// (review.PromptDigest and each case's rendered prompt), the adapter
// (AdapterDigest) and the suite. P22a's activation of the review
// capability calls Check (the I-inventory's obligation P22a-6).
//
// Decisions (the lead, 2026-10-20, and Opus #306):
//   - Check fails closed: no record of the route, a record that is not
//     one, a stale route, prompt, adapter or suite digest, a report missing,
//     not its digest, or not agreeing with its case (its decision, its
//     roster, its charge links), and a score under the bar each leave the
//     route unqualified. The score is recomputed from the cases; the
//     record's own counts and verdict must agree with it.
//   - Only the latest run of a route counts (by ran_at): a failing run
//     after a passing one takes the qualification away.
//   - The run goes through the real http-chat adapter and gateway
//     (Route.Review, adapterhost.Host.Run); a live run is the lead's,
//     committed as a record. CI runs the harness against a scripted
//     upstream only: CI cannot qualify a model.
//   - A reviewer sees review.Prompt only: the task's requirement, the
//     candidate's diff and the gate's summary. Never a case's id, the
//     suite's notes, the sealed probes or the task's hidden tests.
//   - Any change to the http-chat adapter's source or to the reviewer's
//     limits makes every record stale (a false block accepted: the
//     adapter's system message and decision schema need no export).
package qualify

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/review"
)

// Dir is the suite's directory, from the repository root.
const Dir = "fixtures/m1/review-qualification"

// The reviewer's limits a qualified route runs with: Route.Review's, and
// P22a's for a qualified route (P22a-6). AdapterDigest binds them.
const (
	MaxTurns        = 1
	MaxOutputTokens = 8000
)

const (
	recordSchema = "adc.review-qualification/v1"
	reportSchema = "adc.review-report/v1"
)

// adapterPaths are what AdapterDigest covers, from the repository root.
var adapterPaths = []string{"adapters/http-chat/src", "adapters/http-chat/package.json", "adapters/pnpm-lock.yaml"}

var (
	caseID      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,95}$`)
	probeName   = regexp.MustCompile(`^TestQualProbe[A-Za-z0-9]+$`)
	digestValue = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Case is one candidate of the suite: a patch of its task's repository.
type Case struct {
	ID     string `json:"id"`
	Task   string `json:"task"`   // the project's task the candidate is for
	Expect string `json:"expect"` // go (a clean control) or no_go (a planted defect)
	Defect string `json:"defect,omitempty"`
	// Probe is the one sealed probe a defect fails (Opus #306 L3).
	Probe string `json:"probe,omitempty"`
	// PromptDigest is the digest of the case's rendered review.Prompt.
	PromptDigest types.Digest `json:"prompt_digest"`
}

// Suite is the qualification suite, read from root's Dir.
type Suite struct {
	Project string `json:"project"` // the project's fixture directory, from the root
	Pass    struct {
		NoGo int `json:"no_go"`
		Go   int `json:"go"`
	} `json:"pass"`
	Cases   []Case       `json:"cases"`
	Root    string       `json:"-"`
	Digest  types.Digest `json:"-"` // suite.json, every case's patch and every sealed file
	Adapter types.Digest `json:"-"` // AdapterDigest
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
		defect := c.Expect == review.NoGo
		if !caseID.MatchString(c.ID) || seen[c.ID] || defect != (c.Defect != "") || defect != probeName.MatchString(c.Probe) || !defect && c.Probe != "" ||
			c.Expect != review.Go && !defect || !digestValue.MatchString(string(c.PromptDigest)) {
			return Suite{}, fmt.Errorf("qualify: case %q is not one", c.ID)
		} else if _, err := os.Stat(filepath.Join(s.taskDir(c), "task.json")); err != nil {
			return Suite{}, fmt.Errorf("qualify: case %s: %w", c.ID, err)
		}
		seen[c.ID] = true
		defects, controls = defects+b2i(defect), controls+b2i(!defect)
	}
	if s.Pass.NoGo < 1 || defects != s.Pass.NoGo || s.Pass.Go < 1 || controls < s.Pass.Go {
		return Suite{}, fmt.Errorf("qualify: the bar (%d no_go, %d go) does not fit %d defects and %d controls", s.Pass.NoGo, s.Pass.Go, defects, controls)
	}
	if s.Digest, err = suiteDigest(dir); err != nil {
		return Suite{}, err
	}
	if s.Adapter, err = AdapterDigest(root); err != nil {
		return Suite{}, err
	}
	return s, nil
}

// suiteDigest is the digest of every file under the suite's dir but its
// records and README.
func suiteDigest(dir string) (types.Digest, error) {
	return digestOf(dir, []string{"."}, func(rel string, isDir bool) bool {
		return isDir && rel == "records" || !isDir && rel == "README.md"
	})
}

// AdapterDigest is the digest of the http-chat adapter's source, its
// package.json and the workspace lockfile under root, and of the
// reviewer's limits (Opus #306 M6).
func AdapterDigest(root string) (types.Digest, error) {
	return adapterDigest(root, MaxTurns, MaxOutputTokens)
}

func adapterDigest(root string, turns, tokens int) (types.Digest, error) {
	return digestOf(root, adapterPaths, func(string, bool) bool { return false }, fmt.Sprintf("max_turns=%d\x00max_output_tokens=%d", turns, tokens))
}

// digestOf is the digest of every regular file under each of rels from
// base (a file, or a directory walked whole) but those skip names, by path
// and content, after extra.
func digestOf(base string, rels []string, skip func(rel string, isDir bool) bool, extra ...string) (types.Digest, error) {
	h := sha256.New()
	for _, x := range extra {
		fmt.Fprintf(h, "%d\x00%s", len(x), x)
	}
	for _, r := range rels {
		if err := walkInto(h, base, r, skip); err != nil {
			return "", err
		}
	}
	return types.Digest("sha256:" + hex.EncodeToString(h.Sum(nil))), nil
}

func walkInto(h hash.Hash, base, rel string, skip func(string, bool) bool) error {
	return filepath.WalkDir(filepath.Join(base, rel), func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		r, _ := filepath.Rel(base, p)
		switch {
		case skip(r, e.IsDir()) && e.IsDir():
			return filepath.SkipDir
		case e.IsDir() || skip(r, false):
			return nil
		case !e.Type().IsRegular():
			return fmt.Errorf("qualify: %s is not a regular file", r)
		}
		b, err := os.ReadFile(p)
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(r), len(b))
		h.Write(b)
		return err
	})
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
	rec := types.ReviewQualification{Schema: recordSchema, Route: route, PromptDigest: review.PromptDigest, AdapterDigest: s.Adapter, SuiteDigest: s.Digest,
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
		rec.Cases = append(rec.Cases, types.QualifiedCase{ID: c.ID, Expect: types.QualifiedCaseExpect(c.Expect), PromptDigest: c.PromptDigest,
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

// Check is nil when the latest record (by ran_at) of route under dir
// qualifies it for the suite s, the current prompt and the current
// adapter; otherwise ErrUnqualified, with the reason. A record that is not
// one fails the whole check.
func Check(dir string, route types.QualifiedRoute, s Suite) error {
	names, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}
	type run struct {
		path string
		rec  types.ReviewQualification
		at   time.Time
	}
	var runs []run
	for _, p := range names {
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rec, err := contract.Decode[types.ReviewQualification]("config/review-qualification", raw)
		var at time.Time
		if err == nil {
			at, err = time.Parse(time.RFC3339Nano, string(rec.RanAt))
		}
		if err != nil {
			return fmt.Errorf("%w: %s is not a record: %v", ErrUnqualified, filepath.Base(p), err)
		}
		if rec.Route == route {
			runs = append(runs, run{p, rec, at})
		}
	}
	if len(runs) == 0 {
		return fmt.Errorf("%w: no record of the route", ErrUnqualified)
	}
	latest := slices.MaxFunc(runs, func(a, b run) int { return a.at.Compare(b.at) }).at
	for _, r := range runs { // every run at the latest time must qualify (Opus #306 M3)
		if !r.at.Equal(latest) {
			continue
		}
		if err := check(r.path, r.rec, s); err != nil {
			return fmt.Errorf("%w: %s (the latest run): %v", ErrUnqualified, filepath.Base(r.path), err)
		}
	}
	return nil
}

// report is an adc.review-report/v1 (review.Report), read strictly.
type report struct {
	Schema      string               `json:"schema"`
	ElapsedMS   int64                `json:"elapsed_ms"`
	Round       int64                `json:"candidate_round"`
	Roster      []types.RosterMember `json:"roster"`
	Outcome     types.Outcome        `json:"outcome"`
	Reason      string               `json:"reason"`
	Decision    string               `json:"decision"`
	Findings    []json.RawMessage    `json:"findings"`
	RepairRound *int64               `json:"repair_round"`
	ChargeLinks []types.CallID       `json:"charge_links"`
}

// readReport reads b into r strictly: every key of the report, each once,
// no other (the decoder refuses one), and no trailing data.
func readReport(b []byte, r *report) error {
	v, err := contract.ReadStrict(b)
	if err != nil {
		return err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return errors.New("not a report")
	}
	for _, k := range reportKeys {
		if _, ok := m[k]; !ok {
			return errors.New("not a report's keys")
		}
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(r)
}

var reportKeys = []string{"schema", "elapsed_ms", "candidate_round", "roster", "outcome", "reason", "decision", "findings", "repair_round", "charge_links"}

// outcomeOf is the review result's outcome for a decision (review.Result).
var outcomeOf = map[string]types.Outcome{review.Go: types.OutcomeEmpty, review.NoGo: types.OutcomeFound, "": types.OutcomeCouldNotRun}

func check(p string, rec types.ReviewQualification, s Suite) error {
	switch {
	case rec.PromptDigest != review.PromptDigest:
		return errors.New("another prompt")
	case rec.AdapterDigest != s.Adapter:
		return errors.New("another adapter")
	case rec.SuiteDigest != s.Digest:
		return errors.New("another suite")
	case len(rec.Cases) != len(s.Cases):
		return errors.New("not the suite's cases")
	}
	decided, links := map[string]string{}, map[types.CallID]bool{}
	roster := []types.RosterMember{{Executor: rec.Route.Executor, Family: rec.Route.Family}}
	for i, c := range s.Cases {
		rc := rec.Cases[i]
		if rc.ID != c.ID || string(rc.Expect) != c.Expect || rc.PromptDigest != c.PromptDigest {
			return fmt.Errorf("case %d is not the suite's %s", i, c.ID)
		}
		b, err := os.ReadFile(filepath.Join(strings.TrimSuffix(p, ".json"), c.ID+".review-report.json"))
		if err != nil || digest(b) != rc.ReportDigest {
			return fmt.Errorf("case %s's report is missing or not its digest", c.ID)
		}
		var r report
		if rc.Decision != types.QualifiedCaseDecisionNone {
			decided[c.ID] = string(rc.Decision)
		}
		switch err := readReport(b, &r); { // the report agrees with its case (Opus #306 H1)
		case err != nil || r.Schema != reportSchema:
			return fmt.Errorf("case %s's report is not an %s", c.ID, reportSchema)
		case r.Decision != decided[c.ID] || r.Outcome != outcomeOf[r.Decision]:
			return fmt.Errorf("case %s's report decides %q, its record %q", c.ID, r.Decision, rc.Decision)
		case !slices.Equal(r.Roster, roster):
			return fmt.Errorf("case %s's report is another reviewer's", c.ID)
		case len(r.ChargeLinks) == 0 || slices.ContainsFunc(r.ChargeLinks, func(l types.CallID) bool { return links[l] }):
			return fmt.Errorf("case %s's report has no call of its own", c.ID)
		}
		for _, l := range r.ChargeLinks {
			links[l] = true
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
