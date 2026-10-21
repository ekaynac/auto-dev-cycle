// SPDX-License-Identifier: Apache-2.0

package qualify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
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
	perTask := map[string][2]int{} // Opus #306 r2 H1: one defect and one control per task
	for _, c := range s.Cases {
		n := perTask[c.Task]
		n[b2i(c.Expect == review.Go)]++
		perTask[c.Task] = n
	}
	if len(perTask) != 4 {
		t.Errorf("the cases cover %d tasks, want 4", len(perTask))
	}
	for task, n := range perTask {
		if n != [2]int{1, 1} {
			t.Errorf("%s has %d defects and %d controls; want one of each, or the requirement alone tells them apart", task, n[0], n[1])
		}
	}
	for name, c := range map[string]struct {
		decided map[string]string
		ok      bool
	}{
		"every case right":    {all(s, ""), true},
		"one control missed":  {with(all(s, ""), "control-sku-package", review.NoGo), true},
		"two controls missed": {with(with(all(s, ""), "control-sku-package", review.NoGo), "control-lowstock", ""), false},
		"one defect passed":   {with(all(s, ""), "defect-sku-package-test-weakened", review.Go), false},
		"one defect a miss":   {with(all(s, ""), "defect-transfer-keeps-destination-price", ""), false},
		"every case go":       {all(s, review.Go), false},
		"every case no_go":    {all(s, review.NoGo), false}, // Opus #306 L1
	} {
		if _, _, ok := s.Score(c.decided); ok != c.ok {
			t.Errorf("%s: qualified %v", name, ok)
		}
	}
}

// TestBlindReviewers is the exhaustive count behind the residuals (Astra
// #306 M5; Opus #306 r2 H1), over all 8! orders of the suite's cases:
//   - a reviewer that reads only the requirement (the task) and the
//     request's place among its task's requests, deciding each by any
//     rule: the best of the 4^4 rules passes the bar in 1/8 of the
//     orders. A stateful reviewer that conditions on the whole sequence
//     does no better: the sequence of tasks says nothing of which of a
//     task's two requests is its defect;
//   - a reviewer that reads nothing, answering by the request's place in
//     the run: the best of the 2^8 answer sets passes in 1/14.
func TestBlindReviewers(t *testing.T) {
	s := suite(t)
	n := len(s.Cases)
	tasks, defect := map[string]int{}, make([]bool, n)
	task := make([]int, n)
	for i, c := range s.Cases {
		if _, ok := tasks[c.Task]; !ok {
			tasks[c.Task] = len(tasks)
		}
		task[i], defect[i] = tasks[c.Task], c.Expect == review.NoGo
	}
	passes := func(noGo func(seq, c int, nth int) bool, perm []int) bool {
		var seen [8]int
		missed := 0
		for seq, c := range perm {
			ng := noGo(seq, c, seen[task[c]])
			seen[task[c]]++
			if defect[c] && !ng {
				return false
			}
			missed += b2i(!defect[c] && ng)
		}
		return n-s.Pass.NoGo-missed >= s.Pass.Go
	}
	orders := permutations(n)
	best := func(rules int, noGo func(rule, seq, c, nth int) bool) int {
		top := 0
		for rule := range rules {
			k := 0
			for _, perm := range orders {
				k += b2i(passes(func(seq, c, nth int) bool { return noGo(rule, seq, c, nth) }, perm))
			}
			top = max(top, k)
		}
		return top
	}
	byRequirement := best(1<<(2*len(tasks)), func(rule, _, c, nth int) bool { return rule>>(2*task[c]+nth)&1 == 1 })
	byPlace := best(1<<n, func(rule, seq, _, _ int) bool { return rule>>seq&1 == 1 })
	if len(orders) != 40320 || byRequirement*8 != len(orders) || byPlace*14 != len(orders) {
		t.Errorf("of %d orders, the best requirement-only rule passes %d (want 1/8), the best place-only rule %d (want 1/14): update the residuals",
			len(orders), byRequirement, byPlace)
	}
}

// permutations is every order of 0..n-1.
func permutations(n int) [][]int {
	if n == 0 {
		return [][]int{{}}
	}
	var out [][]int
	for _, p := range permutations(n - 1) {
		for i := 0; i <= len(p); i++ {
			q := append(append(append(make([]int, 0, n), p[:i]...), n-1), p[i:]...)
			out = append(out, q)
		}
	}
	return out
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

// TestAdapterDigest: the digest moves with the http-chat adapter's runtime
// closure (its source and package.json, the contract package's source and
// package.json, the generated validators and formats, the workspace's
// manifests and lockfile) and the limits, and not with the packages' tests
// (Opus #306 M6; Astra #306 M3).
func TestAdapterDigest(t *testing.T) {
	dir := t.TempDir()
	for _, p := range append(slices.Clone(adapterPaths), "adapters/http-chat/test", "adapters/contract/test") {
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
		"the adapter's source":            {"adapters/http-chat/src/adapter.ts", true},
		"a new source file":               {"adapters/http-chat/src/extra.ts", true},
		"package.json":                    {"adapters/http-chat/package.json", true},
		"the lockfile":                    {"adapters/pnpm-lock.yaml", true},
		"the contract's source (Astra)":   {"adapters/contract/src/adapter.ts", true},
		"the contract's package.json":     {"adapters/contract/package.json", true},
		"the generated validators":        {"gen/ts/validators.cjs", true},
		"the generated formats":           {"gen/ts/formats.cjs", true},
		"the generated package.json":      {"gen/ts/package.json", true},
		"the workspace's package.json":    {"adapters/package.json", true},
		"the workspace's package list":    {"adapters/pnpm-workspace.yaml", true},
		"the adapter's own test":          {"adapters/http-chat/test/x.test.ts", false},
		"the contract package's own test": {"adapters/contract/test/x.test.ts", false},
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

// TestAdapterClosure: every local file the http-chat adapter loads at run
// time (its value imports, re-exports and requires, from its entry point;
// type-only imports are erased) is under one of adapterPaths, and so is
// each local package's manifest (Astra #306 M3).
func TestAdapterClosure(t *testing.T) {
	packages := map[string]string{"@adc/contract": "adapters/contract", "@adc/contract-gen": "gen/ts"} // the workspace's local packages the adapter reaches
	loads := []*regexp.Regexp{
		regexp.MustCompile(`(?s)(?:^|\n)\s*(?:import|export)(\s+type)?\s[^;]*?\bfrom\s+"([^"]+)"`),
		regexp.MustCompile(`(?:^|\n)\s*import()\s+"([^"]+)"`),
		regexp.MustCompile(`\brequire\(()\s*"([^"]+)"\s*\)`),
		regexp.MustCompile(`createRequire\([^)]*\)\(()\s*"([^"]+)"\s*\)`),
	}
	covered := func(rel string) bool {
		return slices.ContainsFunc(adapterPaths, func(p string) bool { return rel == p || strings.HasPrefix(rel, p+"/") })
	}
	resolve := func(from, spec string) string {
		switch {
		case strings.HasPrefix(spec, "."):
			return filepath.ToSlash(filepath.Join(filepath.Dir(from), spec))
		case packages[spec] != "": // the package's "." export
			var m struct {
				Exports map[string]string `json:"exports"`
			}
			b, err := os.ReadFile(filepath.Join(root, packages[spec], "package.json"))
			if err != nil || json.Unmarshal(b, &m) != nil || m.Exports["."] == "" {
				t.Fatalf("%s has no export", spec)
			}
			return filepath.ToSlash(filepath.Join(packages[spec], m.Exports["."]))
		}
		for name, dir := range packages {
			if rest, ok := strings.CutPrefix(spec, name+"/"); ok {
				return dir + "/" + rest
			}
		}
		return "" // node: built-ins and registry packages, which the lockfile pins
	}
	seen, queue := map[string]bool{}, []string{AdapterEntry} // the command Route.Review runs
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		if seen[f] {
			continue
		}
		seen[f] = true
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range loads {
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				if m[1] == "" { // a type-only import or export loads nothing
					if next := resolve(f, m[2]); next != "" {
						queue = append(queue, next)
					}
				}
			}
		}
	}
	for _, want := range []string{"adapters/contract/src/adapter.ts", "gen/ts/validators.cjs", "gen/ts/formats.cjs"} {
		if !seen[want] {
			t.Errorf("the walk never reached %s: its patterns are broken", want)
		}
	}
	for f := range seen {
		if !covered(f) {
			t.Errorf("the adapter loads %s, which AdapterDigest does not cover", f)
		}
	}
	for _, dir := range append(slices.Collect(maps.Values(packages)), "adapters/http-chat", "adapters") {
		if !covered(dir + "/package.json") {
			t.Errorf("%s/package.json is not covered", dir)
		}
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

// finish reviews the rest of run x's cases, from its cursor, as decided,
// as Route.Review would: the run admits each in turn (take) and keeps its
// evidence (log); its outcome has its review report (review.Report), with
// the route's reviewer as its roster and a call of its own. out holds the
// cases reviewed already.
func finish(t *testing.T, s Suite, x *Run, decided map[string]string, out map[string]Outcome) map[string]Outcome {
	t.Helper()
	for _, c := range s.Order(x.Seed)[x.st.cursor:] {
		seq, err := x.take(c)
		if err != nil {
			t.Fatal(err)
		}
		call := newCall(t)
		o := outcome(t, decided[c.ID], call, x.Route)
		o.evidence = &types.QualifiedEvidence{RunID: x.ID, RanAt: types.Time(x.At.Format(time.RFC3339)), Seq: int64(seq), PromptDigest: c.PromptDigest,
			Route: x.Route, AdapterDigest: x.Adapter, SuiteDigest: x.Suite, Call: call}
		x.log(c.ID, *o.evidence)
		out[c.ID] = o
	}
	return out
}

// outcomes is every case of run x reviewed, as decided.
func outcomes(t *testing.T, s Suite, x *Run, decided map[string]string) map[string]Outcome {
	t.Helper()
	return finish(t, s, x, decided, map[string]Outcome{})
}

// begin is s.Begin, which must not fail.
func begin(t *testing.T, s Suite, dir, name string, r types.QualifiedRoute, at time.Time) *Run {
	t.Helper()
	x, err := s.Begin(dir, name, r, at)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func newCall(t *testing.T) types.CallID {
	t.Helper()
	id, err := ids.New("cal")
	if err != nil {
		t.Fatal(err)
	}
	return types.CallID(id)
}

// outcome is a decision d's report, with call and r's reviewer, and no
// evidence.
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

// Check fails closed (the lead's decision 3; Opus #306; Astra #306): no
// record, a record that is not one, another route, prompt, adapter or
// suite, cases that are not the suite's, a report missing, not its digest,
// not one or not agreeing with its case, evidence not binding a case to
// the record's run, route and adapter, its prompt and the run's order,
// counts that are not the cases', a score under the bar, and a latest run
// that fails each leave the route unqualified.
func TestCheck(t *testing.T) {
	s := suite(t)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := Check(t.TempDir(), route, s); !errors.Is(err, ErrUnqualified) {
		t.Errorf("no record: %v", err)
	}
	record := func(dir, name string, r types.QualifiedRoute, when time.Time, decided map[string]string) {
		x := begin(t, s, dir, name, r, when)
		if _, err := s.Record(x, outcomes(t, s, x, decided)); err != nil {
			t.Fatal(err)
		}
	}
	fresh := func(decided map[string]string) string { // a passing run, then run.json, the latest: the rows change it
		dir := t.TempDir()
		record(dir, "earlier", route, at.Add(-time.Hour), all(s, ""))
		record(dir, "run", route, at, decided)
		return dir
	}
	if err := Check(fresh(all(s, "")), route, s); err != nil {
		t.Fatalf("a qualifying record: %v", err)
	}
	if err := Check(fresh(with(all(s, ""), "control-transfer", "")), route, s); err != nil {
		t.Errorf("one control a miss: %v", err)
	}
	load := func(dir string) types.ReviewQualification {
		var rec types.ReviewQualification
		raw, err := os.ReadFile(filepath.Join(dir, "run.json"))
		if err == nil {
			err = json.Unmarshal(raw, &rec)
		}
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	edit := func(dir string, fn func(*types.ReviewQualification)) {
		rec := load(dir)
		fn(&rec)
		raw, err := json.Marshal(rec)
		if err == nil {
			err = os.WriteFile(filepath.Join(dir, "run.json"), raw, 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	report := func(dir, id string, b []byte) func(*types.ReviewQualification) { // a case's report replaced, its digest updated
		if err := os.WriteFile(filepath.Join(dir, "run", id+".review-report.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
		return func(r *types.ReviewQualification) {
			for i := range r.Cases {
				if r.Cases[i].ID == id {
					r.Cases[i].ReportDigest = digest(b)
				}
			}
		}
	}
	callOf := func(dir, id string) types.CallID {
		for _, c := range load(dir).Cases {
			if c.ID == id {
				return c.Evidence.Call
			}
		}
		t.Fatalf("no case %s", id)
		return ""
	}
	recount := func(r *types.ReviewQualification) { // the counts and verdict made the cases'
		m := map[string]string{}
		for _, c := range r.Cases {
			m[c.ID] = string(c.Decision)
		}
		noGo, goes, ok := s.Score(m)
		r.NoGoOnDefects, r.GoOnControls, r.Qualified = int64(noGo), int64(goes), ok
	}
	other := route
	other.Model = "another-model"
	endpoint := route
	endpoint.Endpoint = "https://elsewhere.example/v1"
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
		"a lying verdict": {with(all(s, ""), "defect-sku-package-test-weakened", review.Go), route, func(d string) {
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
		// Opus H1: a record's decisions edited, its real reports unchanged.
		"decisions edited, reports unchanged": {all(s, review.Go), route, func(d string) {
			edit(d, func(r *types.ReviewQualification) {
				for i := range r.Cases {
					r.Cases[i].Decision = types.QualifiedCaseDecision(r.Cases[i].Expect)
				}
				r.NoGoOnDefects, r.GoOnControls, r.Qualified = 4, 4, true
			})
		}, "report decides"},
		"a report that is not one": {all(s, ""), route, func(d string) {
			edit(d, report(d, first, []byte(`{"schema":"adc.review-report/v1","decision":"no_go"}`)))
		}, "is not an adc.review-report/v1"},
		"another case's report": {all(s, ""), route, func(d string) {
			b, _ := os.ReadFile(filepath.Join(d, "run", first+".review-report.json")) // a no_go for a no_go, but the first case's call
			edit(d, report(d, second, b))
		}, "no call of its own"},
		"another reviewer's report": {all(s, ""), route, func(d string) {
			stranger := route
			stranger.Executor, stranger.Family = "exe_01m4tm99haemqt8es2qfbwj8pa", "stub-builder"
			edit(d, report(d, first, outcome(t, review.NoGo, callOf(d, first), stranger).Report))
		}, "another reviewer's"},
		"an outcome not its decision": {all(s, ""), route, func(d string) {
			b := outcome(t, review.NoGo, callOf(d, first), route).Report
			edit(d, report(d, first, bytes.Replace(b, []byte(`"outcome":"found"`), []byte(`"outcome":"empty"`), 1)))
		}, "an outcome or reason not its decision's"},
		"not the schema's": {all(s, ""), route, func(d string) {
			os.WriteFile(filepath.Join(d, "run.json"), []byte(`{"schema":"adc.review-qualification/v1"}`), 0o644)
		}, "is not a record"},
		// Astra H2: an all-go run's defect reports keep "decision":"go" and
		// gain case-variant "Decision":"no_go" and "Outcome":"found"; their
		// digests, decisions and counts updated.
		"case-variant aliases (Astra H2)": {all(s, review.Go), route, func(d string) {
			for _, c := range s.Cases {
				if c.Expect != review.NoGo {
					continue
				}
				b, _ := os.ReadFile(filepath.Join(d, "run", c.ID+".review-report.json"))
				b = append(bytes.TrimSuffix(bytes.TrimSpace(b), []byte("}")), []byte(`,"Decision":"no_go","Outcome":"found"}`)...)
				edit(d, report(d, c.ID, b))
				edit(d, func(r *types.ReviewQualification) {
					for i := range r.Cases {
						if r.Cases[i].ID == c.ID {
							r.Cases[i].Decision = types.QualifiedCaseDecisionNoGo
						}
					}
					recount(r)
				})
			}
		}, "not a report: its keys"},
		// Astra H1: a passing record's route changed, its evidence not.
		"another model": {all(s, ""), other, func(d string) { edit(d, func(r *types.ReviewQualification) { r.Route = other }) }, "ran on another route"},
		"another endpoint": {all(s, ""), endpoint, func(d string) {
			edit(d, func(r *types.ReviewQualification) { r.Route = endpoint })
		}, "ran on another route"},
		"a case of another run": {all(s, ""), route, func(d string) {
			edit(d, func(r *types.ReviewQualification) { r.RunID = strings.Repeat("0", 32) })
		}, "ran in another run"},
		"a case sent another prompt": {all(s, ""), route, func(d string) {
			edit(d, func(r *types.ReviewQualification) { r.Cases[5].Evidence.PromptDigest = digest([]byte("x")) })
		}, "was sent another prompt"},
		"a case on another adapter": {all(s, ""), route, func(d string) {
			edit(d, func(r *types.ReviewQualification) { r.Cases[6].Evidence.AdapterDigest = digest([]byte("x")) })
		}, "ran on another adapter or suite"},
		"a case on another suite": {all(s, ""), route, func(d string) {
			edit(d, func(r *types.ReviewQualification) { r.Cases[6].Evidence.SuiteDigest = digest([]byte("x")) })
		}, "ran on another adapter or suite"},
		"a case out of the seed's order (Astra M5)": {all(s, ""), route, func(d string) {
			edit(d, func(r *types.ReviewQualification) {
				r.Cases[0].Evidence.Seq, r.Cases[1].Evidence.Seq = r.Cases[1].Evidence.Seq, r.Cases[0].Evidence.Seq
			})
		}, "out of its run's order"},
		"a report charging another call": {all(s, ""), route, func(d string) {
			edit(d, func(r *types.ReviewQualification) { r.Cases[3].Evidence.Call = newCall(t) })
		}, "no call of its own"},
		"a call shared by two cases": {all(s, ""), route, func(d string) {
			call := callOf(d, first)
			edit(d, report(d, second, outcome(t, s.Cases[1].Expect, call, route).Report))
			edit(d, func(r *types.ReviewQualification) { r.Cases[1].Evidence.Call = call })
		}, "no call of its own"},
		// Astra M4: a record or a report that is a link, or a reports
		// directory that is one.
		"a record linking to /dev/zero": {all(s, ""), route, func(d string) { os.Symlink("/dev/zero", filepath.Join(d, "poison.json")) }, "poison.json is not a record"},
		"a record that is a link to one": {all(s, ""), route, func(d string) {
			later := t.TempDir()
			record(later, "later", route, at.Add(time.Hour), all(s, ""))
			os.Symlink(filepath.Join(later, "later.json"), filepath.Join(d, "later.json"))
			os.Symlink(filepath.Join(later, "later"), filepath.Join(d, "later"))
		}, "later.json is not a record"},
		"a report linking to /dev/zero": {all(s, ""), route, func(d string) {
			p := filepath.Join(d, "run", first+".review-report.json")
			os.Remove(p)
			os.Symlink("/dev/zero", p)
		}, "report is missing"},
		"a reports directory that is a link": {all(s, ""), route, func(d string) {
			os.Rename(filepath.Join(d, "run"), filepath.Join(d, "elsewhere"))
			os.Symlink(filepath.Join(d, "elsewhere"), filepath.Join(d, "run"))
		}, "its reports are not a directory"},
		"a record over its bound": {all(s, ""), route, func(d string) {
			os.WriteFile(filepath.Join(d, "huge.json"), bytes.Repeat([]byte(" "), maxRecordBytes+1), 0o644)
		}, "huge.json is not a record: is 1048577 bytes, over"},
		"too many records": {all(s, ""), route, func(d string) {
			for i := range maxRecords {
				os.WriteFile(filepath.Join(d, fmt.Sprintf("x%04d.json", i)), nil, 0o644)
			}
		}, "past its bounds"},
		"too many entries": {all(s, ""), route, func(d string) {
			for i := range maxEntries {
				os.WriteFile(filepath.Join(d, fmt.Sprintf("x%04d", i)), nil, 0o644)
			}
		}, "past its bounds"},
		// Opus r2 M2: a run begun and never ended is the latest, and fails.
		"an incomplete latest run (r2 M2)": {all(s, ""), route, func(d string) { begin(t, s, d, "aborted", route, at.Add(time.Hour)) }, "aborted.json (a latest run): an incomplete run"},
		"a record marked complete, without its cases": {all(s, ""), route, func(d string) {
			begin(t, s, d, "aborted", route, at.Add(time.Hour))
			p := filepath.Join(d, "aborted.json")
			b, _ := os.ReadFile(p)
			os.WriteFile(p, bytes.Replace(b, []byte(`"complete": false`), []byte(`"complete": true`), 1), 0o644)
		}, "aborted.json (a latest run): its reports are not a directory"},
		// Opus r2 L2: a passing record's ran_at moved past a newer failing
		// run, its evidence unchanged.
		"ran_at moved (r2 L2)": {all(s, ""), route, func(d string) {
			record(d, "newer", route, at.Add(time.Hour), all(s, review.Go))
			edit(d, func(r *types.ReviewQualification) { r.RanAt = types.Time(at.Add(2 * time.Hour).Format(time.RFC3339)) })
		}, "run.json (a latest run): case defect-transfer-keeps-destination-price's run started at another time"},
	} {
		dir := fresh(c.decided)
		if c.change != nil {
			c.change(dir)
		}
		if err := Check(dir, c.route, s); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: %v, want unqualified (%s)", name, err, c.why)
		}
	}
	// Astra H1, the reviewer's input: the four unchanged defect reports of
	// an all-no_go run and the four unchanged control reports of an all-go
	// run, under one record with its decisions and counts made to agree.
	goes, noGos := fresh(all(s, review.Go)), fresh(all(s, review.NoGo))
	edit(goes, func(r *types.ReviewQualification) {
		donor := load(noGos)
		for i, c := range s.Cases {
			if c.Expect == review.NoGo {
				b, _ := os.ReadFile(filepath.Join(noGos, "run", c.ID+".review-report.json"))
				report(goes, c.ID, b)
				r.Cases[i] = donor.Cases[i]
			}
		}
		recount(r)
	})
	if err := Check(goes, route, s); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), "ran in another run") {
		t.Errorf("two runs' reports as one record: %v", err)
	}
	// Record makes no evidence, and completes only a run that reviewed every
	// case once, each outcome the one the run logged (Astra H1; Opus r2
	// M1). It refuses, and writes nothing but leaves the run's record
	// incomplete.
	type refusal struct {
		x   *Run
		out map[string]Outcome
		why string
	}
	rows := map[string]func(dir string) refusal{
		"a run Begin did not start": func(string) refusal {
			return refusal{&Run{ID: strings.Repeat("0", 32)}, map[string]Outcome{}, "not a run Begin started"}
		},
		"reports without evidence": func(dir string) refusal {
			x := begin(t, s, dir, "run", route, at)
			out := outcomes(t, s, x, all(s, ""))
			for id, o := range out {
				out[id] = Outcome{Decision: o.Decision, Report: o.Report}
			}
			return refusal{x, out, "has no outcome of the run"}
		},
		"another run's outcomes": func(dir string) refusal {
			x := begin(t, s, dir, "run", route, at)
			outcomes(t, s, x, all(s, ""))
			return refusal{x, outcomes(t, s, begin(t, s, t.TempDir(), "other", route, at), all(s, "")), "has no outcome of the run"}
		},
		"a run not ended": func(dir string) refusal {
			return refusal{begin(t, s, dir, "run", route, at), outcomes(t, s, begin(t, s, t.TempDir(), "other", route, at), all(s, "")), "reviewed 0 of its 8 cases"}
		},
		"a review that never ended": func(dir string) refusal { // admitted, then its adapter failed: no outcome
			x := begin(t, s, dir, "run", route, at)
			if _, err := x.take(s.Order(x.Seed)[0]); err != nil {
				t.Fatal(err)
			}
			return refusal{x, finish(t, s, x, all(s, ""), map[string]Outcome{}), "was reviewed 0 times"}
		},
		"a case reviewed twice (r2 M1)": func(dir string) refusal {
			x := begin(t, s, dir, "run", route, at)
			out := outcomes(t, s, x, all(s, ""))
			again := *out[first].evidence
			again.Call = newCall(t)
			x.log(first, again)
			return refusal{x, out, "was reviewed 2 times"}
		},
		"outcomes on another route": func(dir string) refusal {
			x := begin(t, s, dir, "run", route, at)
			out := outcomes(t, s, x, all(s, ""))
			for id, o := range out { // as the run logged them: Record's own check of the evidence refuses
				o.evidence.Route = other
				x.st.done[id] = []types.QualifiedEvidence{*o.evidence}
			}
			return refusal{x, out, "ran on another route"}
		},
	}
	for name, mk := range rows {
		dir := t.TempDir()
		c := mk(dir)
		if _, err := s.Record(c.x, c.out); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: recorded (%v), want refused (%s)", name, err, c.why)
		}
		entries, _ := os.ReadDir(dir)
		if c.x.st != nil && (len(entries) != 1 || entries[0].Name() != "run.json" || load(dir).Complete) || c.x.st == nil && len(entries) > 0 {
			t.Errorf("%s: Record wrote %d entries, or completed the record", name, len(entries))
		}
	}
	taken := t.TempDir()
	begin(t, s, taken, "run", route, at)
	if _, err := s.Begin(taken, "run", route, at); err == nil || !errors.Is(err, os.ErrExist) {
		t.Errorf("Begin over an existing record: %v, want refused", err)
	}
	// Opus M3 and the lead's #226 entry 110: the latest two runs, of
	// different seeds, must both pass.
	dir := t.TempDir()
	record(dir, "a", route, at, all(s, ""))
	if err := Check(dir, route, s); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), "one run of the route") {
		t.Errorf("one passing run: %v, want unqualified", err)
	}
	record(dir, "b", route, at.Add(24*time.Hour), with(all(s, ""), "control-lowstock", review.NoGo))
	if err := Check(dir, route, s); err != nil {
		t.Errorf("two passing runs: %v", err)
	}
	record(dir, "c", route, at.Add(48*time.Hour), all(s, review.Go))
	if err := Check(dir, route, s); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), "c.json (a latest run)") {
		t.Errorf("pass, then fail: %v", err)
	}
	record(dir, "d", route, at.Add(72*time.Hour), all(s, ""))
	if err := Check(dir, route, s); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), "c.json (a latest run)") {
		t.Errorf("fail, then one pass: %v, want unqualified", err)
	}
	record(dir, "e", route, at.Add(96*time.Hour), all(s, ""))
	if err := Check(dir, route, s); err != nil {
		t.Errorf("fail, then two passes: %v", err)
	}
	record(dir, "f", route, at.Add(96*time.Hour), all(s, review.NoGo))
	if err := Check(dir, route, s); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), "f.json (a latest run)") {
		t.Errorf("a failing run beside a passing one at the same time: %v", err)
	}
	record(dir, "g", other, at.Add(120*time.Hour), all(s, review.Go))
	if err := Check(dir, other, s); !errors.Is(err, ErrUnqualified) { // another route's run is its own
		t.Errorf("another route's failing run: %v", err)
	}
	ties := t.TempDir() // every run at the latest time is judged, past the latest two
	record(ties, "p", route, at, all(s, ""))
	record(ties, "q", route, at, all(s, ""))
	record(ties, "r", route, at, all(s, review.Go))
	if err := Check(ties, route, s); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), "r.json (a latest run)") {
		t.Errorf("a third run at the latest time failing: %v", err)
	}
	// Opus r2 L1: and every run at the second-latest time, whatever the
	// names.
	for _, names := range [][3]string{{"z-latest", "b-pass", "c-fail"}, {"z-latest", "c-pass", "b-fail"}} {
		dir := t.TempDir()
		record(dir, names[0], route, at.Add(time.Hour), all(s, ""))
		record(dir, names[1], route, at, all(s, ""))
		record(dir, names[2], route, at, all(s, review.Go))
		if err := Check(dir, route, s); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), names[2]+".json (a latest run)") {
			t.Errorf("a failing run at the second-latest time, beside a passing one (%v): %v", names, err)
		}
	}
	// Two passing records of one seed are one run, not two.
	same := t.TempDir()
	x1 := begin(t, s, same, "one", route, at)
	if _, err := s.Record(x1, outcomes(t, s, x1, all(s, ""))); err != nil {
		t.Fatal(err)
	}
	x2 := begin(t, s, same, "two", route, at.Add(time.Hour))
	x2.Seed, x2.st.order = x1.Seed, s.Order(x1.Seed)
	if _, err := s.Record(x2, outcomes(t, s, x2, all(s, ""))); err != nil {
		t.Fatal(err)
	}
	if err := Check(same, route, s); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), "share a seed") {
		t.Errorf("two runs of one seed: %v, want unqualified", err)
	}
	ties = t.TempDir() // and at the second-latest time, a copy of the latest run's seed
	x3 := begin(t, s, ties, "c", route, at.Add(time.Hour))
	if _, err := s.Record(x3, outcomes(t, s, x3, all(s, ""))); err != nil {
		t.Fatal(err)
	}
	record(ties, "a", route, at, all(s, ""))
	x4 := begin(t, s, ties, "b", route, at)
	x4.Seed, x4.st.order = x3.Seed, s.Order(x3.Seed)
	if _, err := s.Record(x4, outcomes(t, s, x4, all(s, ""))); err != nil {
		t.Fatal(err)
	}
	if err := Check(ties, route, s); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), "c.json and b.json share a seed") {
		t.Errorf("a judged run of the latest run's seed: %v, want unqualified", err)
	}
	// The suite or the adapter changed after the run: the record and its
	// evidence agree with each other, not with the current digests.
	tampered := s
	tampered.Digest = digest([]byte("another suite"))
	if err := Check(fresh(all(s, "")), route, tampered); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), "run.json (a latest run): another suite") {
		t.Errorf("a changed suite: %v", err)
	}
	tampered = s
	tampered.Adapter = digest([]byte("another adapter"))
	if err := Check(fresh(all(s, "")), route, tampered); !errors.Is(err, ErrUnqualified) || !strings.Contains(err.Error(), "run.json (a latest run): another adapter") {
		t.Errorf("a changed adapter: %v", err)
	}
}

// TestOrder: a run's order is a permutation of the suite that its seed
// alone decides, and seeds order the cases differently (Astra #306 M5).
func TestOrder(t *testing.T) {
	s := suite(t)
	idsOf := func(cs []Case) (out []string) {
		for _, c := range cs {
			out = append(out, c.ID)
		}
		return out
	}
	suiteOrder := idsOf(s.Cases)
	firsts := map[string]bool{}
	for i := range 64 {
		seed := fmt.Sprintf("%064x", i)
		got := idsOf(s.Order(seed))
		if !slices.Equal(got, idsOf(s.Order(seed))) || !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(suiteOrder))) {
			t.Fatalf("seed %d: %v is not one permutation of the suite", i, got)
		}
		firsts[got[0]] = true
	}
	if len(firsts) < len(s.Cases)/2 {
		t.Errorf("64 seeds put only %d cases first: the order does not follow the seed", len(firsts))
	}
	dir := t.TempDir()
	a, b := begin(t, s, dir, "a", route, time.Now()), begin(t, s, dir, "b", route, time.Now())
	if a.ID == b.ID || a.Seed == b.Seed || len(a.ID) != 32 || len(a.Seed) != 64 || !slices.Equal(idsOf(a.st.order), idsOf(s.Order(a.Seed))) {
		t.Errorf("two runs %+v and %+v: not random ids and seeds, or not in their seed's order", a, b)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "a.json")) // Begin's record: incomplete, and a record (Opus #306 r2 M2)
	if err != nil {
		t.Fatal(err)
	}
	if rec, err := contract.Decode[types.ReviewQualification]("config/review-qualification", raw); err != nil || rec.Complete || rec.Qualified || len(rec.Cases) != 0 || rec.Seed != a.Seed {
		t.Errorf("Begin's record %+v, %v", rec, err)
	}
}

// TestReadRegular: a record or report is a regular file read within its
// bound: never through a link, never a device, never past the bound, even
// when its size says otherwise (Astra #306 M4).
func TestReadRegular(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok.json")
	if err := os.WriteFile(ok, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ok, filepath.Join(dir, "link.json")); err != nil {
		t.Fatal(err)
	}
	if b, err := readRegular(ok, 10); err != nil || string(b) != "0123456789" {
		t.Errorf("a file at its bound: %q, %v", b, err)
	}
	for name, c := range map[string]struct {
		path  string
		limit int64
		why   string
	}{
		"a link to a regular file": {filepath.Join(dir, "link.json"), 10, "symbolic link"},
		"a link to /dev/zero":      {filepath.Join(dir, "zero.json"), 10, "symbolic link"},
		"/dev/zero":                {"/dev/zero", 10, "not a regular file"},
		"/dev/null":                {"/dev/null", 10, "not a regular file"},
		"a directory":              {dir, 10, "not a regular file"},
		"a file past its bound":    {ok, 9, "is 10 bytes, over 9"},
		"a file whose size lies":   {"/proc/self/status", 10, "grew past 10 bytes"},
	} {
		if name == "a link to /dev/zero" {
			if err := os.Symlink("/dev/zero", c.path); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := os.Lstat(c.path); err != nil {
			t.Logf("%s: %v; skipped", name, err)
			continue
		}
		if b, err := readRegular(c.path, c.limit); err == nil || !strings.Contains(err.Error(), c.why) || int64(len(b)) > c.limit+1 {
			t.Errorf("%s: %d bytes, %v; want %q, and never past the bound", name, len(b), err, c.why)
		}
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
