// SPDX-License-Identifier: Apache-2.0

package qualify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/review"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
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
		m[c.ID] = cmpOr(d, c.Expect)
	}
	return m
}

func with(m map[string]string, id, d string) map[string]string {
	m[id] = d
	return m
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// TestSuiteDiscriminates is the suite's positive control (§2.7): every
// candidate applies and passes the visible gate, so the gate discriminates
// nothing, and the sealed probes fail every planted defect, for its stated
// reason, and pass every clean control and each task's reference change.
func TestSuiteDiscriminates(t *testing.T) {
	s := suite(t)
	gobin, env := goEnv(t)
	ctx := context.Background()
	probe := func(dir, task string) (bool, string) {
		sealed := filepath.Join(root, Dir, "sealed", task)
		if err := copyTree(sealed, dir); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, gobin, "test", "-count=1", "-run", "TestQualProbe", "./...")
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		return err == nil, string(out)
	}
	for _, c := range s.Cases {
		dir := t.TempDir()
		if err := s.Workspace(ctx, dir, c, env); err != nil {
			t.Fatal(err)
		}
		if g, err := Gate(ctx, gobin, dir, env); err != nil || !g.Green || g.Failed+g.Skipped != 0 {
			t.Errorf("%s: the visible gate %+v, %v; want green", c.ID, g, err)
		}
		if ok, out := probe(dir, c.Task); ok != (c.Expect == review.Go) {
			t.Errorf("%s: the probe passes = %v, want %v:\n%s", c.ID, ok, c.Expect == review.Go, out)
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
		if ok, out := probe(dir, task); !ok {
			t.Errorf("%s's reference change fails the probe:\n%s", task, out)
		}
	}
}

// The prompt holds the task's requirement, the diff and the gate's
// summary, and never the task's hidden tests, a sealed probe, the suite's
// notes or a case's id (the lead's decision 1; I18).
func TestPromptWithholds(t *testing.T) {
	s := suite(t)
	gobin, env := goEnv(t)
	suiteJSON, err := os.ReadFile(filepath.Join(root, Dir, "suite.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Cases {
		p, err := s.Prompt(context.Background(), c, gobin, t.TempDir(), env)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := s.Requirement(c)
		diff, _ := s.Patch(c)
		if !bytes.Contains(p, []byte(req)) || !bytes.Contains(p, bytes.TrimRight(diff, "\n")) || !bytes.Contains(p, []byte("green: every test it ran passed")) {
			t.Errorf("%s: the prompt lacks the requirement, the diff or the gate's summary", c.ID)
		}
		var restricted [][]byte
		for _, d := range []string{filepath.Join(s.taskDir(c), "hidden"), filepath.Join(root, Dir, "sealed")} {
			_ = filepath.WalkDir(d, func(p string, e os.DirEntry, err error) error {
				if err == nil && !e.IsDir() {
					b, _ := os.ReadFile(p)
					restricted = append(restricted, b)
				}
				return nil
			})
		}
		if len(restricted) == 0 {
			t.Fatalf("%s: no restricted file found", c.ID)
		}
		var shown []byte // what the reviewer may see: the candidate's tree and its patch
		for _, d := range []string{filepath.Join(root, s.Project, "repo"), filepath.Join(s.taskDir(c), "visible")} {
			_ = filepath.WalkDir(d, func(p string, e os.DirEntry, err error) error {
				if err == nil && !e.IsDir() {
					b, _ := os.ReadFile(p)
					shown = append(shown, b...)
				}
				return nil
			})
		}
		shown = append(shown, diff...)
		leaks := func(p []byte) (out []string) { // every distinctive line of a restricted file: one the candidate's tree and patch do not hold
			for _, b := range restricted {
				for _, l := range bytes.Split(b, []byte("\n")) {
					if l = bytes.TrimSpace(l); len(l) >= 12 && !bytes.Contains(shown, l) && bytes.Contains(p, l) {
						out = append(out, string(l))
					}
				}
			}
			return out
		}
		if l := leaks(p); len(l) > 0 {
			t.Errorf("%s: the prompt holds restricted lines %q", c.ID, l)
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
}

func TestPrompt(t *testing.T) {
	in := review.PromptInput{Requirement: "Add Transfer.", Diff: "diff --git a/x b/x\n+x\n", Gate: review.Gate{Passed: 3, Failed: 1}}
	p, err := review.Prompt(in)
	if err != nil || !bytes.Contains(p, []byte("red: not every test passed (3 passed, 1 failed, 0 skipped)")) {
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
	if !strings.HasPrefix(string(review.PromptDigest), "sha256:") {
		t.Error(review.PromptDigest)
	}
}

var route = types.QualifiedRoute{Executor: "exe_01m4tm99haemr8ajyf09v4ytzb", Family: "stub-reviewer", Adapter: "http-chat", Model: "stub-reviewer-1",
	Destination: "stub", Endpoint: "http://127.0.0.1:18080/v1"}

// outcomes is every case's outcome as decided, each with a report.
func outcomes(decided map[string]string) map[string]Outcome {
	out := map[string]Outcome{}
	for id, d := range decided {
		out[id] = Outcome{Decision: d, Report: []byte(`{"schema":"adc.review-report/v1","case":"` + id + `"}`)}
	}
	return out
}

// Check fails closed (the lead's decision 3): no record, another route,
// prompt or suite, a report missing or not its digest, counts that are
// not the cases', and a score under the bar each leave the route
// unqualified.
func TestCheck(t *testing.T) {
	s := suite(t)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := Check(t.TempDir(), route, s); !errors.Is(err, ErrUnqualified) {
		t.Errorf("no record: %v", err)
	}
	fresh := func(decided map[string]string) string {
		dir := t.TempDir()
		if _, err := s.Record(dir, "run", route, at, outcomes(decided)); err != nil {
			t.Fatal(err)
		}
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
	other := route
	other.Model = "another-model"
	for name, c := range map[string]struct {
		decided map[string]string
		route   types.QualifiedRoute
		change  func(dir string)
		why     string
	}{
		"under the bar":    {with(all(s, ""), "defect-lowstock-negative-threshold-panics", review.Go), route, nil, "under the bar"},
		"every case go":    {all(s, review.Go), route, nil, "under the bar"},
		"another route":    {all(s, ""), other, nil, "another route"},
		"a stale prompt":   {all(s, ""), route, func(d string) { edit(d, func(r *types.ReviewQualification) { r.PromptDigest = digest([]byte("x")) }) }, "another prompt"},
		"a stale suite":    {all(s, ""), route, func(d string) { edit(d, func(r *types.ReviewQualification) { r.SuiteDigest = digest([]byte("x")) }) }, "another suite"},
		"a lying verdict":  {with(all(s, ""), "defect-formatcents-test-weakened", review.Go), route, func(d string) { edit(d, func(r *types.ReviewQualification) { r.Qualified = true }) }, "counts are not"},
		"a lying decision": {all(s, ""), route, func(d string) { edit(d, func(r *types.ReviewQualification) { r.Cases[0].Decision = "go" }) }, "counts are not"},
		"cases reordered": {all(s, ""), route, func(d string) {
			edit(d, func(r *types.ReviewQualification) { r.Cases[0], r.Cases[1] = r.Cases[1], r.Cases[0] })
		}, "not the suite's"},
		"a case dropped":   {all(s, ""), route, func(d string) { edit(d, func(r *types.ReviewQualification) { r.Cases = r.Cases[1:] }) }, "not the suite's cases"},
		"a report missing": {all(s, ""), route, func(d string) { os.Remove(filepath.Join(d, "run", "control-lowstock.review-report.json")) }, "report is missing"},
		"a report changed": {all(s, ""), route, func(d string) {
			os.WriteFile(filepath.Join(d, "run", "control-lowstock.review-report.json"), []byte("{}"), 0o644)
		}, "report is missing"},
		"not the schema's": {all(s, ""), route, func(d string) {
			os.WriteFile(filepath.Join(d, "run.json"), []byte(`{"schema":"adc.review-qualification/v1"}`), 0o644)
		}, ""},
	} {
		dir := fresh(c.decided)
		if c.change != nil {
			c.change(dir)
		}
		if err := Check(dir, c.route, s); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: %v, want unqualified (%s)", name, err, c.why)
		}
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
	base, err := digestDir(dir)
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
		if d, err := digestDir(dir); err != nil || (d != base) != c.moves {
			t.Errorf("%s changed: the digest moves %v, want %v (%v)", name, d != base, c.moves, err)
		}
		if old == nil {
			err = os.Remove(p)
		} else {
			err = os.WriteFile(p, old, 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}
