// SPDX-License-Identifier: Apache-2.0

package qualify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/review"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const root = "../.."

func suite(t *testing.T) Suite {
	t.Helper()
	s, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// goEnv is an offline, local toolchain's environment, as the go-mini gate
// test runs it; it skips without go or git.
func goEnv(t *testing.T) (string, []string) {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go on PATH")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on PATH")
	}
	cache := t.TempDir()
	return gobin, append(testguard.HomeEnv(t), "PATH="+filepath.Dir(gobin)+":"+filepath.Dir(git)+":/usr/bin:/bin", "GOCACHE="+filepath.Join(cache, "build"),
		"GOPATH="+filepath.Join(cache, "path"), "GOTOOLCHAIN=local", "GOPROXY=off", "GOFLAGS=-mod=mod", "GOWORK=off")
}

func TestSuite(t *testing.T) {
	s := suite(t)
	defects, controls := 0, 0
	for _, c := range s.Cases {
		defects, controls = defects+b2i(c.Expect == review.NoGo), controls+b2i(c.Expect == review.Go)
	}
	if defects != 4 || controls != 4 || s.Pass.NoGo != 4 || s.Pass.Go != 3 {
		t.Errorf("%d defects, %d controls, bar %+v; want 4, 4 and 4 no_go, 3 go (P31)", defects, controls, s.Pass)
	}
	for name, c := range map[string]struct {
		decided map[string]string
		ok      bool
	}{
		"every case right":    {all(s, ""), true},
		"one control missed":  {with(all(s, ""), "control-sku-package", review.NoGo), true},
		"two controls missed": {with(with(all(s, ""), "control-sku-package", review.NoGo), "control-lowstock", ""), false},
		"one defect passed":   {with(all(s, ""), "defect-formatcents-test-weakened", review.Go), false},
		"one defect a miss":   {with(all(s, ""), "defect-transfer-keeps-destination-price", ""), false},
		"every case go":       {all(s, review.Go), false},
		"every case no_go":    {all(s, review.NoGo), false}, // Opus #306 L1
	} {
		if _, _, ok := s.Score(c.decided); ok != c.ok {
			t.Errorf("%s: qualified %v", name, ok)
		}
	}
}

// all is every case decided as expected, or each as d when d is set.
func all(s Suite, d string) map[string]string {
	m := map[string]string{}
	for _, c := range s.Cases {
		m[c.ID] = c.Expect
		if d != "" {
			m[c.ID] = d
		}
	}
	return m
}

func with(m map[string]string, id, d string) map[string]string {
	m[id] = d
	return m
}

var failed = regexp.MustCompile(`--- FAIL: (\S+)`)

// TestSuiteDiscriminates is the suite's positive control (§2.7): every
// candidate applies and passes the visible gate, so the gate discriminates
// nothing, and the sealed probes fail every planted defect, exactly in its
// named probe (Opus #306 L3), and pass every clean control and each
// task's reference change.
func TestSuiteDiscriminates(t *testing.T) {
	s := suite(t)
	gobin, env := goEnv(t)
	ctx := context.Background()
	probe := func(dir, task string) ([]string, string) {
		if err := copyTree(filepath.Join(root, Dir, "sealed", task), dir); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, gobin, "test", "-count=1", "-run", "TestQualProbe", "./...")
		cmd.Dir, cmd.Env = dir, env
		out, _ := cmd.CombinedOutput()
		var names []string
		for _, m := range failed.FindAllStringSubmatch(string(out), -1) {
			names = append(names, m[1])
		}
		if len(names) == 0 && !bytes.Contains(out, []byte("ok  \t")) {
			names = []string{"(the probes did not run)"}
		}
		return names, string(out)
	}
	for _, c := range s.Cases {
		dir := t.TempDir()
		if err := s.Workspace(ctx, dir, c, env); err != nil {
			t.Fatal(err)
		}
		if g, err := Gate(ctx, gobin, dir, env); err != nil || !g.Green || g.Failed+g.Skipped != 0 {
			t.Errorf("%s: the visible gate %+v, %v; want green", c.ID, g, err)
		}
		want := []string{}
		if c.Probe != "" {
			want = []string{c.Probe}
		}
		if got, out := probe(dir, c.Task); !slices.Equal(append([]string{}, got...), want) {
			t.Errorf("%s: the probes failed %v, want %v:\n%s", c.ID, got, want, out)
		}
		if g, err := Gate(ctx, gobin, dir, env); err != nil || g.Green != (c.Expect == review.Go) || (g.Failed > 0) == g.Green {
			t.Errorf("%s: the gate with the probes %+v, %v", c.ID, g, err) // Gate's own control: a failing test is red
		}
	}
	for _, task := range []string{"go-mini-feature-1", "go-mini-bugfix-1", "go-mini-failing-then-green-1", "go-mini-refactor-1"} {
		dir, c := t.TempDir(), Case{Task: task}
		if err := copyTree(filepath.Join(root, s.Project, "repo"), dir); err != nil {
			t.Fatal(err)
		}
		if v := filepath.Join(s.taskDir(c), "visible"); exists(v) {
			if err := copyTree(v, dir); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.CommandContext(ctx, "git", "apply", filepath.Join(s.taskDir(c), "reference.patch"))
		cmd.Dir, cmd.Env = dir, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s's reference: %v %s", task, err, out)
		}
		if got, out := probe(dir, task); len(got) > 0 {
			t.Errorf("%s's reference change fails the probes %v:\n%s", task, got, out)
		}
	}
}

// TestGate: a candidate that does not build is red, never "green with no
// failure" (Opus #306 L2).
func TestGate(t *testing.T) {
	s := suite(t)
	gobin, env := goEnv(t)
	dir := t.TempDir()
	if err := s.Workspace(context.Background(), dir, s.Cases[len(s.Cases)-1], env); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "inventory", "broken.go"), []byte("package inventory\n\nfunc broken( {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if g, err := Gate(context.Background(), gobin, dir, env); err != nil || g.Green {
		t.Errorf("a tree that does not build: %+v, %v; want red", g, err)
	}
}

// The prompt holds the task's requirement, the diff and the gate's
// summary, and never the task's hidden tests, a sealed probe, the suite's
// notes or a case's id (the lead's decision 1; I18). A restricted line is
// one no file of the base the reviewer may see holds (repo/ and the
// task's visible overlay): neither the prompt nor, apart, its sources (the
// patch, the requirement) hold one (Opus #306 M4). Each rendered prompt is
// the one suite.json pins (M2).
func TestPromptWithholds(t *testing.T) {
	s := suite(t)
	gobin, env := goEnv(t)
	suiteJSON, err := os.ReadFile(filepath.Join(root, Dir, "suite.json"))
	if err != nil {
		t.Fatal(err)
	}
	files := func(dirs ...string) (out [][]byte) {
		for _, d := range dirs {
			_ = filepath.WalkDir(d, func(p string, e os.DirEntry, err error) error {
				if err == nil && !e.IsDir() {
					b, _ := os.ReadFile(p)
					out = append(out, b)
				}
				return nil
			})
		}
		return out
	}
	for _, c := range s.Cases {
		p, err := s.Prompt(context.Background(), c, gobin, t.TempDir(), env)
		if err != nil {
			t.Fatal(err)
		}
		if digest(p) != c.PromptDigest {
			t.Errorf("%s: the prompt is %s, suite.json pins %s", c.ID, digest(p), c.PromptDigest)
		}
		req, _ := s.Requirement(c)
		diff, _ := s.Patch(c)
		if !bytes.Contains(p, []byte(req)) || !bytes.Contains(p, bytes.TrimRight(diff, "\n")) || !bytes.Contains(p, []byte("green: every test it ran passed")) {
			t.Errorf("%s: the prompt lacks the requirement, the diff or the gate's summary", c.ID)
		}
		restricted := files(filepath.Join(s.taskDir(c), "hidden"), filepath.Join(root, Dir, "sealed"))
		if len(restricted) == 0 {
			t.Fatalf("%s: no restricted file found", c.ID)
		}
		shown := bytes.Join(files(filepath.Join(root, s.Project, "repo"), filepath.Join(s.taskDir(c), "visible")), nil)
		named := func(l []byte) bool { // an import path the requirement names: the task's own package
			path, ok := bytes.CutPrefix(l, []byte(`"`))
			path, ok2 := bytes.CutSuffix(path, []byte(`"`))
			return ok && ok2 && !bytes.ContainsAny(path, "\" \t") && strings.Contains(req, string(path))
		}
		leaks := func(text []byte) (out []string) {
			for _, b := range restricted {
				for _, l := range bytes.Split(b, []byte("\n")) {
					if l = bytes.TrimSpace(l); len(l) >= 12 && !bytes.Contains(shown, l) && !named(l) && bytes.Contains(text, l) {
						out = append(out, string(l))
					}
				}
			}
			return out
		}
		for name, text := range map[string][]byte{"prompt": p, "patch": diff, "requirement": []byte(req)} {
			if l := leaks(text); len(l) > 0 {
				t.Errorf("%s: the %s holds restricted lines %q", c.ID, name, l)
			}
		}
		for _, b := range restricted { // the check's control: each restricted file, shown, is caught
			if len(leaks(append(slices.Clone(p), b...))) == 0 {
				t.Errorf("%s: a restricted file in the prompt goes unseen", c.ID)
			}
		}
		for _, x := range s.Cases {
			if bytes.Contains(p, []byte(x.ID)) || x.Defect != "" && bytes.Contains(p, []byte(x.Defect)) {
				t.Errorf("%s: the prompt names case %s or its defect", c.ID, x.ID)
			}
		}
		if bytes.Contains(p, suiteJSON) {
			t.Errorf("%s: the prompt holds suite.json", c.ID)
		}
	}
	moved := s // a case whose pinned digest is not its prompt's: Prompt refuses it
	moved.Cases = slices.Clone(s.Cases)
	moved.Cases[0].PromptDigest = digest([]byte("another prompt"))
	if _, err := moved.Prompt(context.Background(), moved.Cases[0], gobin, t.TempDir(), env); !errors.Is(err, ErrPromptDrift) {
		t.Errorf("a drifted prompt: %v", err)
	}
}

func TestPrompt(t *testing.T) {
	in := review.PromptInput{Requirement: "Add Transfer.", Diff: "diff --git a/x b/x\n+x\n", Gate: review.Gate{Passed: 3, Failed: 1}}
	p, err := review.Prompt(in)
	if err != nil || !bytes.Contains(p, []byte("red: not every test passed (3 passed, 1 failed, 0 skipped)")) || !bytes.Contains(p, []byte("```diff\n")) {
		t.Errorf("%s, %v", p, err)
	}
	for name, bad := range map[string]review.PromptInput{
		"no requirement": {Diff: in.Diff},
		"no diff":        {Requirement: in.Requirement},
		"a huge diff":    {Requirement: in.Requirement, Diff: strings.Repeat("x", 1<<20)},
		"not UTF-8":      {Requirement: in.Requirement, Diff: "\xff"},
	} {
		if _, err := review.Prompt(bad); err == nil {
			t.Errorf("%s: a prompt", name)
		}
	}
	// L5: a diff line holding a fence never closes the prompt's.
	in.Diff = "diff --git a/x b/x\n ```\n+``` ``````\n"
	if p, err := review.Prompt(in); err != nil || !bytes.Contains(p, []byte("\n```````diff\n")) || !bytes.HasSuffix(bytes.TrimSpace(p), []byte("\n```````")) {
		t.Errorf("a diff holding fences: %s, %v", p, err)
	}
	// M1: the digest is the rendered prompt's, pinned: a change to the
	// template or the rendering must update it, and every record goes stale.
	if review.PromptDigest != "sha256:"+pinnedPrompt {
		t.Errorf("review.PromptDigest is %s; update pinnedPrompt deliberately (every qualification record goes stale)", review.PromptDigest)
	}
}

const pinnedPrompt = "b73d979dfc3d748a3c3601cc714b14d1a221e575c96bcfd1cfb646a0c310f6fa"

// TestAdapterDigest: the digest moves with the http-chat adapter's source,
// its package.json, the lockfile and the limits, and not with its tests
// (Opus #306 M6).
func TestAdapterDigest(t *testing.T) {
	dir := t.TempDir()
	for _, p := range append(slices.Clone(adapterPaths), "adapters/http-chat/test") {
		src := filepath.Join(root, p)
		fi, err := os.Stat(src)
		if err != nil {
			t.Fatal(err)
		} else if fi.IsDir() {
			err = copyTree(src, filepath.Join(dir, p))
		} else if err = os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err == nil {
			var b []byte
			if b, err = os.ReadFile(src); err == nil {
				err = os.WriteFile(filepath.Join(dir, p), b, 0o644)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	base, err := AdapterDigest(dir)
	if real, err2 := AdapterDigest(root); err != nil || err2 != nil || real != base {
		t.Fatalf("a copy's digest %s, the repository's %s: %v %v", base, real, err, err2)
	}
	for name, d := range map[string][2]int{"more turns": {MaxTurns + 1, MaxOutputTokens}, "fewer tokens": {MaxTurns, MaxOutputTokens - 1}} {
		if moved, err := adapterDigest(dir, d[0], d[1]); err != nil || moved == base {
			t.Errorf("%s: the digest does not move (%v)", name, err)
		}
	}
	for name, c := range map[string]struct {
		path  string
		moves bool
	}{
		"the adapter's source":   {"adapters/http-chat/src/adapter.ts", true},
		"a new source file":      {"adapters/http-chat/src/extra.ts", true},
		"package.json":           {"adapters/http-chat/package.json", true},
		"the lockfile":           {"adapters/pnpm-lock.yaml", true},
		"the adapter's own test": {"adapters/http-chat/test/x.test.ts", false},
	} {
		p := filepath.Join(dir, c.path)
		old, _ := os.ReadFile(p)
		if err := os.WriteFile(p, append(slices.Clone(old), '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		if d, err := AdapterDigest(dir); err != nil || (d != base) != c.moves {
			t.Errorf("%s changed: the digest moves %v, want %v (%v)", name, d != base, c.moves, err)
		}
		restore(t, p, old)
	}
}

func restore(t *testing.T, p string, old []byte) {
	t.Helper()
	var err error
	if old == nil {
		err = os.Remove(p)
	} else {
		err = os.WriteFile(p, old, 0o644)
	}
	if err != nil {
		t.Fatal(err)
	}
}

var route = types.QualifiedRoute{Executor: "exe_01m4tm99haemr8ajyf09v4ytzb", Family: "stub-reviewer", Adapter: "http-chat", Model: "stub-reviewer-1",
	Destination: "stub", Endpoint: "http://127.0.0.1:18080/v1"}

// outcomes is every case's outcome as decided, each with its review report
// (review.Report): its own call, the route's reviewer as its roster.
func outcomes(t *testing.T, decided map[string]string) map[string]Outcome {
	t.Helper()
	out := map[string]Outcome{}
	for id, d := range decided {
		out[id] = outcome(t, d, types.CallID(fmt.Sprintf("cal_01m4tm99haemss9tvzd438v%03d", len(out))), route)
	}
	return out
}

func outcome(t *testing.T, d string, call types.CallID, r types.QualifiedRoute) Outcome {
	t.Helper()
	res := types.Result{Outcome: types.OutcomeCouldNotRun, Reason: vocab.ReasonDegenerateOutput}
	switch d {
	case review.Go:
		res.Outcome, res.Reason = types.OutcomeEmpty, vocab.ReasonCompletedClean
	case review.NoGo:
		res.Outcome, res.Reason = types.OutcomeFound, vocab.ReasonCompletedWithFindings
	}
	b, err := review.Report(review.Facts{CandidateRound: 1, Roster: []types.RosterMember{{Executor: r.Executor, Family: r.Family}}, Calls: []types.CallID{call}},
		res, review.Decision{Decision: d})
	if err != nil {
		t.Fatal(err)
	}
	return Outcome{Decision: d, Report: b}
}

// Check fails closed (the lead's decision 3; Opus #306): no record, a
// record that is not one, another route, prompt, adapter or suite, cases
// that are not the suite's, a report missing, not its digest or not
// agreeing with its case, counts that are not the cases', a score under
// the bar, and a latest run that fails each leave the route unqualified.
func TestCheck(t *testing.T) {
	s := suite(t)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := Check(t.TempDir(), route, s); !errors.Is(err, ErrUnqualified) {
		t.Errorf("no record: %v", err)
	}
	record := func(dir, name string, when time.Time, decided map[string]string) {
		if _, err := s.Record(dir, name, route, when, outcomes(t, decided)); err != nil {
			t.Fatal(err)
		}
	}
	fresh := func(decided map[string]string) string {
		dir := t.TempDir()
		record(dir, "run", at, decided)
		return dir
	}
	if err := Check(fresh(all(s, "")), route, s); err != nil {
		t.Fatalf("a qualifying record: %v", err)
	}
	if err := Check(fresh(with(all(s, ""), "control-transfer", "")), route, s); err != nil {
		t.Errorf("one control a miss: %v", err)
	}
	edit := func(dir string, fn func(*types.ReviewQualification)) {
		p := filepath.Join(dir, "run.json")
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var rec types.ReviewQualification
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		fn(&rec)
		if raw, err = json.Marshal(rec); err == nil {
			err = os.WriteFile(p, raw, 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	report := func(dir, id string, o Outcome) func(*types.ReviewQualification) { // a case's report replaced, its digest updated
		if err := os.WriteFile(filepath.Join(dir, "run", id+".review-report.json"), o.Report, 0o644); err != nil {
			t.Fatal(err)
		}
		return func(r *types.ReviewQualification) {
			for i := range r.Cases {
				if r.Cases[i].ID == id {
					r.Cases[i].ReportDigest = digest(o.Report)
				}
			}
		}
	}
	other := route
	other.Model = "another-model"
	first, second := s.Cases[0].ID, s.Cases[1].ID
	for name, c := range map[string]struct {
		decided map[string]string
		route   types.QualifiedRoute
		change  func(dir string)
		why     string
	}{
		"under the bar":    {with(all(s, ""), "defect-lowstock-negative-threshold-panics", review.Go), route, nil, "under the bar"},
		"every case go":    {all(s, review.Go), route, nil, "under the bar"},
		"every case no_go": {all(s, review.NoGo), route, nil, "under the bar"},
		"another route":    {all(s, ""), other, nil, "no record of the route"},
		"a stale prompt":   {all(s, ""), route, func(d string) { edit(d, func(r *types.ReviewQualification) { r.PromptDigest = digest([]byte("x")) }) }, "another prompt"},
		"a stale adapter":  {all(s, ""), route, func(d string) { edit(d, func(r *types.ReviewQualification) { r.AdapterDigest = digest([]byte("x")) }) }, "another adapter"},
		"a stale suite":    {all(s, ""), route, func(d string) { edit(d, func(r *types.ReviewQualification) { r.SuiteDigest = digest([]byte("x")) }) }, "another suite"},
		"a stale case prompt": {all(s, ""), route, func(d string) {
			edit(d, func(r *types.ReviewQualification) { r.Cases[2].PromptDigest = digest([]byte("x")) })
		}, "not the suite's"},
		"a lying verdict": {with(all(s, ""), "defect-formatcents-test-weakened", review.Go), route, func(d string) {
			edit(d, func(r *types.ReviewQualification) { r.Qualified = true })
		}, "counts are not"},
		"an expect flipped (L4)": {all(s, ""), route, func(d string) { edit(d, func(r *types.ReviewQualification) { r.Cases[4].Expect = "no_go" }) }, "not the suite's"},
		"an id renamed":          {all(s, ""), route, func(d string) { edit(d, func(r *types.ReviewQualification) { r.Cases[3].ID = "another-case" }) }, "not the suite's"},
		"cases reordered": {all(s, ""), route, func(d string) {
			edit(d, func(r *types.ReviewQualification) { r.Cases[0], r.Cases[1] = r.Cases[1], r.Cases[0] })
		}, "not the suite's"},
		"a case dropped":   {all(s, ""), route, func(d string) { edit(d, func(r *types.ReviewQualification) { r.Cases = r.Cases[1:] }) }, "not the suite's cases"},
		"a report missing": {all(s, ""), route, func(d string) { os.Remove(filepath.Join(d, "run", "control-lowstock.review-report.json")) }, "report is missing"},
		"a report changed": {all(s, ""), route, func(d string) {
			os.WriteFile(filepath.Join(d, "run", "control-lowstock.review-report.json"), []byte("{}"), 0o644)
		}, "report is missing"},
		// H1: a record's decisions edited, its real reports unchanged.
		"decisions edited, reports unchanged": {all(s, review.Go), route, func(d string) {
			edit(d, func(r *types.ReviewQualification) {
				for i := range r.Cases {
					r.Cases[i].Decision = types.QualifiedCaseDecision(r.Cases[i].Expect)
				}
				r.NoGoOnDefects, r.GoOnControls, r.Qualified = 4, 4, true
			})
		}, "report decides"},
		"a report that is not one": {all(s, ""), route, func(d string) {
			edit(d, report(d, first, Outcome{Report: []byte(`{"schema":"adc.review-report/v1","decision":"no_go"}`)}))
		}, "is not an adc.review-report/v1"},
		"another case's report": {all(s, ""), route, func(d string) {
			b, _ := os.ReadFile(filepath.Join(d, "run", first+".review-report.json")) // a no_go for a no_go, but the first case's call
			edit(d, report(d, second, Outcome{Report: b}))
		}, "no call of its own"},
		"another reviewer's report": {all(s, ""), route, func(d string) {
			stranger := route
			stranger.Executor, stranger.Family = "exe_01m4tm99haemqt8es2qfbwj8pa", "stub-builder"
			edit(d, report(d, first, outcome(t, review.NoGo, "cal_01m4tm99haemss9tvzd438vzzz", stranger)))
		}, "another reviewer's"},
		"an outcome not its decision": {all(s, ""), route, func(d string) {
			o := outcome(t, review.NoGo, "cal_01m4tm99haemss9tvzd438vyyy", route)
			edit(d, report(d, first, Outcome{Report: bytes.Replace(o.Report, []byte(`"outcome":"found"`), []byte(`"outcome":"empty"`), 1)}))
		}, "report decides"},
		"not the schema's": {all(s, ""), route, func(d string) {
			os.WriteFile(filepath.Join(d, "run.json"), []byte(`{"schema":"adc.review-qualification/v1"}`), 0o644)
		}, "is not a record"},
	} {
		dir := fresh(c.decided)
		if c.change != nil {
			c.change(dir)
		}
		if err := Check(dir, c.route, s); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: %v, want unqualified (%s)", name, err, c.why)
		}
	}
	// M3: only the latest run of the route counts.
	dir := t.TempDir()
	record(dir, "a", at, all(s, ""))
	record(dir, "b", at.Add(24*time.Hour), with(all(s, ""), "control-lowstock", review.NoGo))
	if err := Check(dir, route, s); err != nil {
		t.Errorf("a passing run after a passing one: %v", err)
	}
	record(dir, "c", at.Add(48*time.Hour), all(s, review.Go))
	if err := Check(dir, route, s); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), "c.json (the latest run)") {
		t.Errorf("a failing run after a passing one: %v", err)
	}
	record(dir, "d", at.Add(72*time.Hour), all(s, ""))
	if err := Check(dir, route, s); err != nil {
		t.Errorf("a passing run after a failing one: %v", err)
	}
	record(dir, "e", at.Add(72*time.Hour), all(s, review.NoGo))
	if err := Check(dir, route, s); !errors.Is(err, ErrUnqualified) {
		t.Errorf("a failing run beside a passing one at the same time: %v", err)
	}
	if _, err := s.Record(dir, "f", other, at.Add(96*time.Hour), outcomes(t, all(s, review.Go))); err != nil {
		t.Fatal(err)
	}
	if err := Check(dir, other, s); !errors.Is(err, ErrUnqualified) { // another route's run is its own
		t.Errorf("another route's failing run: %v", err)
	}
	tampered := s // a suite changed after the run: its digest moves
	tampered.Digest = digest([]byte("another suite"))
	if err := Check(fresh(all(s, "")), route, tampered); !errors.Is(err, ErrUnqualified) {
		t.Errorf("a changed suite: %v", err)
	}
}

// The suite's digest covers suite.json, every patch and every sealed file,
// and neither the records nor the README.
func TestSuiteDigest(t *testing.T) {
	dir := t.TempDir()
	if err := copyTree(filepath.Join(root, Dir), dir); err != nil {
		t.Fatal(err)
	}
	base, err := suiteDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		path  string
		moves bool
	}{
		"suite.json":     {"suite.json", true},
		"a patch":        {"cases/control-lowstock/candidate.patch", true},
		"a sealed probe": {"sealed/go-mini-bugfix-1/inventory/qual_probe_test.go", true},
		"the fence":      {"go.mod", true},
		"the README":     {"README.md", false},
		"a record":       {"records/run.json", false},
	} {
		p := filepath.Join(dir, c.path)
		old, _ := os.ReadFile(p)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, append(slices.Clone(old), '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		if d, err := suiteDigest(dir); err != nil || (d != base) != c.moves {
			t.Errorf("%s changed: the digest moves %v, want %v (%v)", name, d != base, c.moves, err)
		}
		restore(t, p, old)
	}
}
