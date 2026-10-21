// SPDX-License-Identifier: Apache-2.0

// Package qualify is reviewer qualification (D56; P31): a reviewer route is
// qualified when, on the suite under fixtures/m1/review-qualification/, it
// returns no_go on every planted defect and go on at least three of the
// four clean controls in each of its latest two committed runs, which
// have different seeds, and each of their records
// (adc.review-qualification/v1) still binds the route, the prompt
// (review.PromptDigest and each case's rendered prompt), the adapter
// (AdapterDigest) and the suite. P22a's activation of the review
// capability calls Check (the I-inventory's obligation P22a-6).
//
// Decisions (the lead, 2026-10-20 and #226 entry 110, Opus #306 and
// Astra #306):
//   - Check fails closed: no record of the route, a record that is not
//     one, a stale route, prompt, adapter or suite digest, a report missing,
//     not its digest, or not agreeing with its case (its decision, its
//     roster, its charge links), a case's evidence not binding its report
//     to the record's run, route and adapter, the case's prompt and the
//     run's order, and a score under the bar each leave the route
//     unqualified. The score is recomputed from the cases; the record's
//     own counts and verdict must agree with it.
//   - Each case's evidence is Route.Review's: the run's id, the case's
//     place in the run's order, the digest of the prompt it sent, the
//     registry's route it sent it on, the run's adapter and suite, and
//     the model call its report charges. Record takes it from the outcome
//     and never makes it: an outcome Route.Review did not produce, or one
//     of another run, route or prompt, is refused (Astra #306 H1).
//   - Only the latest two runs of a route count (by ran_at; the lead,
//     #226 entry 110): both must pass, and their seeds must differ (two
//     records of one seed are one run). One passing run is not enough,
//     and a failing run after passing ones takes the qualification away
//     until two more pass. Every run at the latest time is judged.
//   - A record and a report are each read as a regular file, never
//     through a link, within a byte bound checked before reading, and a
//     records directory holds at most maxRecords records: past a bound,
//     the route is unqualified (Astra #306 M4).
//   - A run's cases run in an order its random seed decides, recorded
//     (Astra #306 M5). The bar's chance figure assumes decisions that are
//     independent and identically distributed across cases; a reviewer
//     that ignores the case and answers by the request's place in the run
//     is outside it. Under the seeded order, no_go to the first four
//     requests passes with probability 1/70 per run, and no_go to exactly
//     five, the best such policy, with 1/14; over the latest two runs, of
//     fresh orders, with at most 1/196 (about 0.5%), the worst case.
//   - The run goes through the real http-chat adapter and gateway
//     (Route.Review, adapterhost.Host.Run); a live run is the lead's,
//     committed as a record. CI runs the harness against a scripted
//     upstream only: CI cannot qualify a model.
//   - A reviewer sees review.Prompt only: the task's requirement, the
//     candidate's diff and the gate's summary. Never a case's id, the
//     suite's notes, the sealed probes or the task's hidden tests.
//   - Any change to the http-chat adapter's runtime closure or to the
//     reviewer's limits makes every record stale (a false block accepted:
//     the adapter's system message and decision schema need no export).
package qualify

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
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

// The bounds Check reads within (Astra #306 M4).
const (
	maxRecords     = 1024    // records in a records directory
	maxEntries     = 4096    // entries of a records directory: each record's file and reports directory, and more
	maxRecordBytes = 1 << 20 // bytes of a record
)

// adapterPaths are what AdapterDigest covers, from the repository root: the
// http-chat adapter's local runtime closure (Astra #306 M3). Its source
// and manifest; the contract package it imports (@adc/contract), its
// source and manifest; the generated validators that package loads and
// the formats they load (@adc/contract-gen); the workspace's manifests and
// lockfile, which link the packages and pin every registry dependency.
// TestAdapterClosure walks the adapter's imports from its entry point and
// requires every local file it reaches to be under one of them.
var adapterPaths = []string{"adapters/http-chat/src", "adapters/http-chat/package.json", "adapters/contract/src", "adapters/contract/package.json",
	"gen/ts/validators.cjs", "gen/ts/formats.cjs", "gen/ts/package.json", "adapters/package.json", "adapters/pnpm-workspace.yaml", "adapters/pnpm-lock.yaml"}

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

// AdapterDigest is the digest of the http-chat adapter's runtime closure
// under root (adapterPaths) and of the reviewer's limits (Opus #306 M6;
// Astra #306 M3).
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

// Run is one run of the suite on a route (Astra #306 H1, M5): a random id
// every case's evidence carries, a random seed that orders its cases, its
// time, and the route, adapter and suite it runs on.
type Run struct {
	ID      types.QualificationRunID
	Seed    string
	At      time.Time
	Route   types.QualifiedRoute
	Adapter types.Digest
	Suite   types.Digest
}

// NewRun is a new run of s on route, at at.
func (s Suite) NewRun(route types.QualifiedRoute, at time.Time) Run {
	id, seed := make([]byte, 16), make([]byte, 32)
	_, _ = rand.Read(id) // crypto/rand.Read never fails
	_, _ = rand.Read(seed)
	return Run{ID: hex.EncodeToString(id), Seed: hex.EncodeToString(seed), At: at.UTC(), Route: route, Adapter: s.Adapter, Suite: s.Digest}
}

// Order is the suite's cases in the order a run with seed runs them, by
// sha256(seed, case id): a permutation the seed alone decides (Astra #306
// M5).
func (s Suite) Order(seed string) []Case {
	key := func(c Case) []byte {
		sum := sha256.Sum256([]byte(seed + "\x00" + c.ID))
		return sum[:]
	}
	out := slices.Clone(s.Cases)
	slices.SortFunc(out, func(a, b Case) int { return bytes.Compare(key(a), key(b)) })
	return out
}

// Outcome is one case's run: the decision (review.Go, review.NoGo or "",
// a miss), its D55 report and the evidence of its execution, which only
// Route.Review makes.
type Outcome struct {
	Decision string
	Report   []byte
	evidence *types.QualifiedEvidence
}

// Record is the record of the suite's run, with each case's outcome; it
// writes each report beside it, under dir/<name>/. Every outcome must be
// one Route.Review returned in run, for its case's prompt, on run's route,
// with run's adapter and suite the current ones; Record refuses any other
// and writes nothing (Astra #306 H1).
func (s Suite) Record(dir, name string, run Run, out map[string]Outcome) (types.ReviewQualification, error) {
	rec := types.ReviewQualification{Schema: recordSchema, RunID: run.ID, Seed: run.Seed, Route: run.Route, PromptDigest: review.PromptDigest, AdapterDigest: run.Adapter,
		SuiteDigest: run.Suite, RanAt: types.Time(run.At.UTC().Format(time.RFC3339)), Cases: []types.QualifiedCase{}}
	decided := map[string]string{}
	for _, c := range s.Cases {
		o, ok := out[c.ID]
		if !ok || len(o.Report) == 0 || o.evidence == nil {
			return rec, fmt.Errorf("qualify: case %s has no outcome of a run", c.ID)
		}
		decided[c.ID] = o.Decision
		rec.Cases = append(rec.Cases, types.QualifiedCase{ID: c.ID, Expect: types.QualifiedCaseExpect(c.Expect), PromptDigest: c.PromptDigest,
			Decision: types.QualifiedCaseDecision(cmp.Or(o.Decision, "none")), ReportDigest: digest(o.Report), Evidence: *o.evidence})
	}
	noGo, goes, ok := s.Score(decided)
	rec.NoGoOnDefects, rec.GoOnControls, rec.Qualified = int64(noGo), int64(goes), ok
	if err := s.verify(rec, func(id string) ([]byte, error) { return out[id].Report, nil }); err != nil {
		return rec, fmt.Errorf("qualify: the outcomes are not the run's: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		return rec, err
	}
	for _, c := range s.Cases {
		if err := os.WriteFile(filepath.Join(dir, name, c.ID+".review-report.json"), out[c.ID].Report, 0o644); err != nil {
			return rec, err
		}
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, name+".json"), append(b, '\n'), 0o644)
	}
	return rec, err
}

// ErrUnqualified: no record qualifies the route.
var ErrUnqualified = errors.New("qualify: the reviewer route is not qualified")

// Check is nil when the latest two records (by ran_at) of route under dir,
// of different seeds, each qualify it for the suite s, the current prompt
// and the current adapter; otherwise ErrUnqualified, with the reason. A
// record that is not one, or a records directory past its bounds, fails
// the whole check.
func Check(dir string, route types.QualifiedRoute, s Suite) error {
	names, err := recordNames(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: no record of the route", ErrUnqualified)
	case err != nil:
		return fmt.Errorf("%w: %v", ErrUnqualified, err)
	}
	type run struct {
		path string
		rec  types.ReviewQualification
		at   time.Time
	}
	var runs []run
	for _, n := range names {
		p := filepath.Join(dir, n)
		raw, err := readRegular(p, maxRecordBytes)
		var rec types.ReviewQualification
		if err == nil {
			rec, err = contract.Decode[types.ReviewQualification]("config/review-qualification", raw)
		}
		var at time.Time
		if err == nil {
			at, err = time.Parse(time.RFC3339Nano, string(rec.RanAt))
		}
		if err != nil {
			return fmt.Errorf("%w: %s is not a record: %v", ErrUnqualified, n, err)
		}
		if rec.Route == route {
			runs = append(runs, run{p, rec, at})
		}
	}
	if len(runs) == 0 {
		return fmt.Errorf("%w: no record of the route", ErrUnqualified)
	}
	// The latest first (names break ties). The latest two are judged, and
	// every run at the latest time (Opus #306 M3; #226 entry 110).
	slices.SortStableFunc(runs, func(a, b run) int { return b.at.Compare(a.at) })
	for i, r := range runs {
		if i > 1 && !r.at.Equal(runs[0].at) {
			break
		}
		if err := check(r.path, r.rec, s); err != nil {
			return fmt.Errorf("%w: %s (a latest run): %v", ErrUnqualified, filepath.Base(r.path), err)
		}
	}
	if len(runs) < 2 {
		return fmt.Errorf("%w: one run of the route: qualification takes its latest two runs, of different seeds, passing", ErrUnqualified)
	}
	if runs[0].rec.Seed == runs[1].rec.Seed {
		return fmt.Errorf("%w: %s and %s share a seed: they are one run, not two", ErrUnqualified, filepath.Base(runs[0].path), filepath.Base(runs[1].path))
	}
	return nil
}

// recordNames are the names of the records (*.json) in dir, sorted, read
// within maxEntries and maxRecords.
func recordNames(dir string) ([]string, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(maxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	if len(entries) > maxEntries || len(names) > maxRecords {
		return nil, fmt.Errorf("the records directory is past its bounds (%d entries, %d records)", maxEntries, maxRecords)
	}
	slices.Sort(names)
	return names, nil
}

// readRegular is the regular file at p, opened never through a link and
// never a special file, of at most limit bytes: its size is checked before
// it is read, and the read stops past the limit (Astra #306 M4).
func readRegular(p string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	switch {
	case err != nil:
		return nil, err
	case !fi.Mode().IsRegular():
		return nil, errors.New("not a regular file")
	case fi.Size() > limit:
		return nil, fmt.Errorf("is %d bytes, over %d", fi.Size(), limit)
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1)) // a file that grows, or whose size lies (/proc), is still read within the bound
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("grew past %d bytes", limit)
	}
	return b, err
}

// check is whether the record at p, with its reports beside it, qualifies
// for s.
func check(p string, rec types.ReviewQualification, s Suite) error {
	reports := strings.TrimSuffix(p, ".json")
	if fi, err := os.Lstat(reports); err != nil || !fi.IsDir() {
		return errors.New("its reports are not a directory")
	}
	if err := s.verify(rec, func(id string) ([]byte, error) {
		return readRegular(filepath.Join(reports, id+".review-report.json"), review.MaxReport)
	}); err != nil {
		return err
	}
	if !rec.Qualified {
		return fmt.Errorf("under the bar: %d of %d defects no_go, %d go", rec.NoGoOnDefects, s.Pass.NoGo, rec.GoOnControls)
	}
	return nil
}

// verify is whether rec is a run of s on the current prompt and adapter:
// its cases the suite's, each report (from report) its digest, a report
// agreeing with its case, each case's evidence binding it to the record's
// run, route, adapter and suite, the case's prompt and the run's order,
// and the counts the cases'.
func (s Suite) verify(rec types.ReviewQualification, report func(id string) ([]byte, error)) error {
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
	seq := map[string]int64{}
	for i, c := range s.Order(rec.Seed) {
		seq[c.ID] = int64(i)
	}
	decided, calls := map[string]string{}, map[types.CallID]bool{}
	roster := []types.RosterMember{{Executor: rec.Route.Executor, Family: rec.Route.Family}}
	for i, c := range s.Cases {
		rc := rec.Cases[i]
		ev := rc.Evidence
		if rc.ID != c.ID || string(rc.Expect) != c.Expect || rc.PromptDigest != c.PromptDigest {
			return fmt.Errorf("case %d is not the suite's %s", i, c.ID)
		}
		switch { // the case's execution is this run's, on this route, of this prompt (Astra #306 H1)
		case ev.RunID != rec.RunID:
			return fmt.Errorf("case %s ran in another run", c.ID)
		case ev.Route != rec.Route:
			return fmt.Errorf("case %s ran on another route", c.ID)
		case ev.PromptDigest != c.PromptDigest:
			return fmt.Errorf("case %s was sent another prompt", c.ID)
		case ev.AdapterDigest != rec.AdapterDigest || ev.SuiteDigest != rec.SuiteDigest:
			return fmt.Errorf("case %s ran on another adapter or suite", c.ID)
		case ev.Seq != seq[c.ID]:
			return fmt.Errorf("case %s ran out of its run's order", c.ID)
		}
		b, err := report(c.ID)
		if err != nil || digest(b) != rc.ReportDigest {
			return fmt.Errorf("case %s's report is missing or not its digest", c.ID)
		}
		if rc.Decision != types.QualifiedCaseDecisionNone {
			decided[c.ID] = string(rc.Decision)
		}
		switch r, err := review.ReadReport(b); { // the report agrees with its case (Opus #306 H1; Astra #306 H2)
		case err != nil:
			return fmt.Errorf("case %s's report is not an %s: %v", c.ID, reportSchema, err)
		case r.Decision != decided[c.ID]:
			return fmt.Errorf("case %s's report decides %q, its record %q", c.ID, r.Decision, rc.Decision)
		case !slices.Equal(r.Roster, roster):
			return fmt.Errorf("case %s's report is another reviewer's", c.ID)
		case !slices.Equal(r.ChargeLinks, []types.CallID{ev.Call}) || calls[ev.Call]:
			return fmt.Errorf("case %s's report charges no call of its own execution", c.ID)
		}
		calls[ev.Call] = true
	}
	if noGo, goes, ok := s.Score(decided); int64(noGo) != rec.NoGoOnDefects || int64(goes) != rec.GoOnControls || ok != rec.Qualified {
		return errors.New("its counts are not its cases'")
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
