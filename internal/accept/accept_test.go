// SPDX-License-Identifier: Apache-2.0

package accept

import (
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
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// gitEnv: a plain git for the test's own repositories (never the
// controller's), with an identity and no user configuration.
var gitEnv = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "PATH=/usr/bin:/bin",
	"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}

func plain(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, append(testguard.HomeEnv(t), gitEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q: %v: %s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		f := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// baseFiles is a py-mini-like base with a Go package beside it.
var baseFiles = map[string]string{
	"money.py":             "def add(a, b):\n    return a + b\n",
	"conftest.py":          "# shared fixtures\n",
	"requirements-dev.txt": "pytest==8.0.0\n",
	"tests/test_money.py":  "from money import add\n\ndef test_add():\n    assert add(1, 2) == 3\n\ndef test_zero():\n    assert add(0, 0) == 0\n",
	"pkg/calc.go":          "package pkg\n\nfunc Add(a, b int) int { return a + b }\n",
	"pkg/calc_test.go":     "package pkg\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}\n}\n",
}

type fixture struct {
	src, base string
	repo      sandbox.Repo
	policy    Policy
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	src := filepath.Join(t.TempDir(), "src")
	plain(t, filepath.Dir(src), "init", "--quiet", "--initial-branch=main", src)
	write(t, src, baseFiles)
	plain(t, src, "add", "--all")
	plain(t, src, "commit", "--quiet", "-m", "base")
	f := fixture{src: src, base: strings.TrimSpace(plain(t, src, "rev-parse", "HEAD"))}
	var err error
	f.repo, err = sandbox.Host{Root: t.TempDir()}.Clone(context.Background(), src, "refs/heads/main", filepath.Join(t.TempDir(), "ctl"))
	if err != nil {
		t.Fatal(err)
	}
	f.policy, err = PolicyOf(types.ProjectProfile{ProtectedPaths: []types.Glob{"conftest.py", "pyproject.toml"},
		Toolchain: types.ProjectProfileToolchain{DependencyRecipe: types.ProjectProfileToolchainDependencyRecipe{Lockfiles: []types.RepoPath{"requirements-dev.txt"}}}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// patch is git's own patch of the base with files written over it and
// gone removed: what a builder's cumulative patch looks like.
func (f fixture) patch(t *testing.T, files map[string]string, gone ...string) []byte {
	t.Helper()
	w := filepath.Join(t.TempDir(), "w")
	plain(t, filepath.Dir(w), "clone", "--quiet", f.src, w)
	write(t, w, files)
	for _, p := range gone {
		if err := os.Remove(filepath.Join(w, p)); err != nil {
			t.Fatal(err)
		}
	}
	plain(t, w, "add", "--all")
	return []byte(plain(t, w, "diff", "--cached", "--binary", "HEAD"))
}

func (f fixture) accept(t *testing.T, patch []byte) Verdict {
	t.Helper()
	v, err := Accept(context.Background(), In{Repo: f.repo, Base: f.base, Patch: patch, Policy: f.policy})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func reasons(bs []Block) []string {
	var out []string
	for _, b := range bs {
		out = append(out, b.Reason.String()+" "+b.Path)
	}
	return out
}

// The ordinary path: a feature with its test is accepted, and its tree is
// the base with the patch applied.
func TestAcceptsAnOrdinaryChange(t *testing.T) {
	f := newFixture(t)
	v := f.accept(t, f.patch(t, map[string]string{
		"money.py":            "def add(a, b):\n    return a + b\n\ndef sub(a, b):\n    return a - b\n",
		"tests/test_money.py": baseFiles["tests/test_money.py"] + "\ndef test_sub():\n    assert True\n",
	}))
	if !v.Accepted() || v.Files != 2 || len(v.Tree) != 40 || !strings.HasPrefix(string(v.TreeDigest), "sha256:") {
		t.Fatalf("verdict %+v, want accepted with its tree", v)
	}
	// A test moved between the changed files is no loss.
	split := f.patch(t, map[string]string{"tests/test_money.py": strings.SplitAfter(baseFiles["tests/test_money.py"], "== 3\n")[0],
		"tests/test_zero.py": "from money import add\n\ndef test_zero():\n    assert add(0, 0) == 0\n"})
	if v := f.accept(t, split); !v.Accepted() {
		t.Errorf("a test moved to a new file: %v", reasons(v.Blocks))
	}
	// A test file renamed, assertions kept.
	moved := f.patch(t, map[string]string{"tests/test_money2.py": baseFiles["tests/test_money.py"]}, "tests/test_money.py")
	if v := f.accept(t, moved); !v.Accepted() {
		t.Errorf("a renamed test file: %v", reasons(v.Blocks))
	}
}

// F (00 §5.2 P14): a builder editing a protected path is refused,
// failed/protected_path: a profile glob, a lockfile, and every git control
// file or look-alike.
func TestRefusesProtectedPaths(t *testing.T) {
	f := newFixture(t)
	for _, files := range []map[string]string{
		{"conftest.py": "import os\n"},
		{"pyproject.toml": "[project]\n"},
		{"requirements-dev.txt": "pytest==8.0.0\nevil==1\n"},
		{".gitattributes": "* filter=lfs\n"},
		{".gitmodules": "[submodule \"x\"]\n\turl = file:///tmp/x\n"},
		{".githooks/post-checkout": "#!/bin/sh\ntouch /tmp/canary\n"},
		{"sub/.gitattributes": "* filter=evil\n"},
		{"sub/.GITMODULES": "x\n"},
	} {
		v := f.accept(t, f.patch(t, files))
		if v.Accepted() || v.Reason() != vocab.ReasonProtectedPath {
			t.Errorf("%v: %v, want protected_path", files, reasons(v.Blocks))
		}
	}
	// Deleting one is touching it too.
	if v := f.accept(t, f.patch(t, nil, "conftest.py")); v.Reason() != vocab.ReasonProtectedPath {
		t.Errorf("a deleted protected path: %v", reasons(v.Blocks))
	}
}

// Accept's own path rules hold on git's file list whatever git refuses
// first: an unsafe path, a symlink or a gitlink outranks a protected one.
func TestCheckPaths(t *testing.T) {
	var pol Policy
	for _, c := range []struct {
		c    change
		want vocab.Reason
	}{
		{change{src: "docs/../../outside.txt", dst: "docs/../../outside.txt", dstMode: "100644"}, vocab.ReasonUnsafeOutput},
		{change{src: "/etc/passwd", dst: "/etc/passwd", dstMode: "100644"}, vocab.ReasonUnsafeOutput},
		{change{src: "a/./b", dst: "a/./b", dstMode: "100644"}, vocab.ReasonUnsafeOutput},
		{change{src: "a\\..\\b", dst: "a\\..\\b", dstMode: "100644"}, vocab.ReasonUnsafeOutput},
		{change{src: ".gitmodules", dst: ".gitmodules", dstMode: "120000"}, vocab.ReasonUnsafeOutput},
		{change{src: "a.txt", dst: ".git/config", dstMode: "100644", status: "R"}, vocab.ReasonProtectedPath},
		{change{src: "x/.GIT./hooks/pre-commit", dst: "x/.GIT./hooks/pre-commit", dstMode: "100755"}, vocab.ReasonProtectedPath},
	} {
		got := pol.check(c.c)
		if len(got) == 0 || got[0].Reason != c.want {
			t.Errorf("%+v: %v, want %s", c.c, reasons(got), c.want)
		}
	}
	if got := pol.check(change{src: "docs/key", dst: "docs/key", srcMode: "120000", dstMode: "000000", status: "D"}); len(got) != 0 {
		t.Errorf("deleting a symlink: %v", reasons(got))
	}
}

func TestGitControlLookAlikes(t *testing.T) {
	for _, c := range []string{".git", ".GIT", ".git.", ".git ", ".git::$INDEX_ALLOCATION", "git~1", ".g\u200cit",
		".gitattributes", ".GitAttributes", "gitatt~1", ".gitmodules", "gitmod~1", ".githooks", ".gitmodules\ufeff",
		"gi7eba~1", "gi7d29~1", "GI7EBA~9", "gitmod~4", "gitatt~2 .", "githoo~1", "gi7e~123", "gi7d29~1:x"} {
		if !gitControl(c) {
			t.Errorf("%q is not a git control name", c)
		}
	}
	for _, c := range []string{".gitignore", "git", ".gitx", "x.git", "github", "git~2", "gitmod~5", "gi7eba~0", "gi7eba~1x", "gi7ebb~1"} {
		if gitControl(c) {
			t.Errorf("%q is a git control name", c)
		}
	}
}

// 00 §2.7 hostile-git-dir and hostile-symlink(-path): a patch writing
// .git, a symlink, a ".." path or an absolute one is refused as unsafe
// output, and nothing is written anywhere: accept only reads an index.
func TestRefusesUnsafeOutput(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	newFile := func(p, mode, body string) []byte {
		return []byte("diff --git a/" + p + " b/" + p + "\nnew file mode " + mode + "\n--- /dev/null\n+++ b/" + p + "\n@@ -0,0 +1 @@\n+" + body + "\n")
	}
	for name, patch := range map[string][]byte{
		"git config": newFile(".git/config", "100644", "[core] fsmonitor = touch "+outside),
		"git hook":   newFile(".git/hooks/post-checkout", "100755", "touch "+outside),
		"look-alike": newFile(".GIT/config", "100644", "x"),
		"dot-dot":    newFile("docs/../../outside.txt", "100644", "x"),
		"absolute":   newFile(outside, "100644", "x"),
		"symlink":    newFile("docs/key", "120000", "/home/u/.ssh/id_ed25519"),
		"gitlink":    []byte("diff --git a/sub b/sub\nnew file mode 160000\nindex 0000000..1111111\n--- /dev/null\n+++ b/sub\n@@ -0,0 +1 @@\n+Subproject commit 1111111111111111111111111111111111111111\n"),
		"garbage":    []byte("not a patch\n"),
	} {
		v := f.accept(t, patch)
		if v.Reason() != vocab.ReasonUnsafeOutput {
			t.Errorf("%s: %v, want unsafe_output", name, reasons(v.Blocks))
		}
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("accept wrote outside the index: %v", err)
	}
	if strings.Contains(plain(t, f.repo.Dir, "status", "--porcelain"), "outside") {
		t.Error("the controller's repository was written")
	}
}

// The integrity guard's conclusive findings (00 §2.6 fault-test-deletion,
// -skip; #226 entry 32) block; its heuristics warn the reviewer.
func TestIntegrityGuard(t *testing.T) {
	f := newFixture(t)
	test, goTest := baseFiles["tests/test_money.py"], baseFiles["pkg/calc_test.go"]
	// why is the block each row must raise (the rule it discriminates).
	why := map[string]string{"deleted test file": "no longer collected", "skip added": "skipped", "go skip added": "skipped",
		"assertion removed": "fewer assertions", "test removed": "a test deleted", "testing.Testing": "forge", "0x16 written": "forge",
		"renamed to non-test": "no longer collected", "go:build ignore": "build constraint", "renamed, uncollected": "no longer collected",
		"go test renamed .test": "no longer collected", "importorskip": "skip", "deleted, comment pad": "a test deleted",
		"assert moved to new": "fewer assertions", "padded over the bound": "unverifiable", "go skip in a helper": "skip"}
	for name, c := range map[string]struct {
		files map[string]string
		gone  []string
	}{
		"deleted test file":   {gone: []string{"tests/test_money.py"}},
		"skip added":          {files: map[string]string{"tests/test_money.py": strings.Replace(test, "def test_add", "@pytest.mark.skip\ndef test_add", 1)}},
		"go skip added":       {files: map[string]string{"pkg/calc_test.go": strings.Replace(baseFiles["pkg/calc_test.go"], "{\n\tif", "{\n\tt.Skip()\n\tif", 1)}},
		"assertion removed":   {files: map[string]string{"tests/test_money.py": strings.Replace(test, "assert add(0, 0) == 0", "pass", 1)}},
		"test removed":        {files: map[string]string{"tests/test_money.py": strings.SplitAfter(test, "== 3\n")[0]}},
		"testing.Testing":     {files: map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\nvar _ = testing.Testing()\n"}},
		"0x16 written":        {files: map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\nconst x = \"\\x16\"\n"}},
		"renamed to non-test": {files: map[string]string{"tests_old.txt": test}, gone: []string{"tests/test_money.py"}},
		// The Opus pre-review's probes (#252).
		"go:build ignore":       {files: map[string]string{"pkg/calc_test.go": "//go:build ignore\n\n" + goTest}},
		"renamed, uncollected":  {files: map[string]string{"tests/money_cases.py": test}, gone: []string{"tests/test_money.py"}},
		"go test renamed .test": {files: map[string]string{"pkg/calc.test.go": goTest}, gone: []string{"pkg/calc_test.go"}},
		"importorskip":          {files: map[string]string{"tests/test_money.py": "import pytest\npytest.importorskip('nope')\n" + test}},
		"deleted, comment pad":  {files: map[string]string{"tests/test_money.py": strings.SplitAfter(test, "== 3\n")[0] + "\n# def test assert\n"}},
		"assert moved to new":   {files: map[string]string{"tests/test_money.py": strings.Replace(test, "assert add(0, 0) == 0", "pass", 1), "tests/test_x.py": "def test_x():\n    assert True\n"}},
		"padded over the bound": {files: map[string]string{"tests/test_money.py": strings.Replace(test, "assert add(0, 0) == 0", "pass", 1) + "#" + strings.Repeat("x", 5<<20) + "\n"}},
		"go skip in a helper":   {files: map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\nfunc skip(t interface{ Skip(...any) }) { t.Skip() }\n"}},
	} {
		v := f.accept(t, f.patch(t, c.files, c.gone...))
		found := slices.ContainsFunc(v.Blocks, func(b Block) bool { return strings.Contains(b.What, why[name]) })
		if v.Reason() != vocab.ReasonTestIntegrityViolation || !found {
			t.Errorf("%s: %+v, want test_integrity_violation (%s)", name, v.Blocks, why[name])
		}
	}
	for name, files := range map[string]map[string]string{
		"an added TestMain":            {"pkg/main_test.go": "package pkg\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) { os.Exit(m.Run()) }\n"},
		"os.Exit in init beside tests": {"pkg/calc.go": baseFiles["pkg/calc.go"] + "\nfunc init() { os.Exit(0) }\n"},
		"a large fixture under tests":  {"tests/data.json": strings.Repeat("x", 5<<20)},
	} {
		v := f.accept(t, f.patch(t, files))
		if !v.Accepted() || len(v.Warnings) == 0 {
			t.Errorf("%s: blocks %v, warnings %v; want accepted with a warning", name, reasons(v.Blocks), reasons(v.Warnings))
		}
	}
}

// The build's diff is the controller's, of the output tree; a symlink in
// it reaches accept and is refused. The diff round-trips through accept.
func TestDiffOfTheOutputTree(t *testing.T) {
	f := newFixture(t)
	tree := filepath.Join(t.TempDir(), "tree")
	write(t, tree, baseFiles)
	if d, _, err := Diff(context.Background(), f.repo, f.base, tree); err != nil || len(d) != 0 {
		t.Fatalf("an unchanged tree: %q, %v", d, err)
	}
	write(t, tree, map[string]string{"money.py": "def add(a, b):\n    return b + a\n", ".gitignore": "*.log\n", "out.log": "x\n"})
	d, ignored, err := Diff(context.Background(), f.repo, f.base, tree)
	if err != nil || !strings.Contains(string(d), "b/money.py") || strings.Contains(string(d), "out.log") || !slices.Equal(ignored, []string{"out.log"}) {
		t.Fatalf("diff %q, ignored %q, %v", d, ignored, err)
	}
	v, err := Accept(context.Background(), In{Repo: f.repo, Base: f.base, Patch: d, Policy: f.policy, Ignored: ignored})
	if err != nil || !v.Accepted() || len(v.Warnings) != 1 {
		t.Fatalf("the controller's own diff: %v, warnings %v, %v", reasons(v.Blocks), reasons(v.Warnings), err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(tree, "key")); err != nil {
		t.Fatal(err)
	}
	d, _, err = Diff(context.Background(), f.repo, f.base, tree)
	if err != nil {
		t.Fatal(err)
	}
	if v := f.accept(t, d); v.Reason() != vocab.ReasonUnsafeOutput {
		t.Fatalf("a symlink in the tree: %v", reasons(v.Blocks))
	}
}

// A .git look-alike in the output tree is the candidate's unsafe output,
// refused before git reads the tree, never a controller fault.
func TestDiffRefusesGitDirectories(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{".git/config", "sub/.GIT/config", "x/Git~1/HEAD", "y/.git. /x"} {
		tree := filepath.Join(t.TempDir(), "tree")
		write(t, tree, map[string]string{"money.py": "x = 1\n", p: "[core]\n"})
		if _, _, err := Diff(context.Background(), f.repo, f.base, tree); !errors.Is(err, ErrUnsafeTree) {
			t.Errorf("%s: %v, want ErrUnsafeTree", p, err)
		}
	}
	// A path git itself refuses to add (a .gitmodules symlink) is unsafe
	// too, never a controller fault.
	tree := filepath.Join(t.TempDir(), "tree")
	write(t, tree, map[string]string{"money.py": "x = 1\n"})
	if err := os.Symlink("money.py", filepath.Join(tree, ".gitmodules")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Diff(context.Background(), f.repo, f.base, tree); !errors.Is(err, ErrUnsafeTree) {
		t.Errorf("a .gitmodules symlink: %v, want ErrUnsafeTree", err)
	}
}

// L4: only PolicyOf's policy is accepted, and a bad glob gives none.
func TestPolicyOfOnly(t *testing.T) {
	f := newFixture(t)
	if _, err := Accept(context.Background(), In{Repo: f.repo, Base: f.base, Policy: Policy{}}); err == nil {
		t.Error("a zero policy was accepted")
	}
	if pol, err := PolicyOf(types.ProjectProfile{ProtectedPaths: []types.Glob{"../x"}}); err == nil || pol.ok {
		t.Errorf("a bad glob: %v, %+v", err, pol)
	}
}

// L2: the report's lists are bounded, counted beyond, and never null.
func TestReportBounded(t *testing.T) {
	b, err := Verdict{Blocks: slices.Repeat([]Block{{vocab.ReasonProtectedPath, "p", "w"}}, maxReported+5)}.Report()
	var rep struct {
		Blocks, Warnings []json.RawMessage
		Truncated        int
	}
	if err != nil || json.Unmarshal(b, &rep) != nil || len(rep.Blocks) != maxReported || rep.Truncated != 5 ||
		rep.Warnings == nil || !strings.Contains(string(b), `"warnings":[]`) {
		t.Fatalf("report: %d blocks, truncated %d, %v", len(rep.Blocks), rep.Truncated, err)
	}
}

// D55: the result holds the report, with the elapsed time and the
// blocks; accepted, the tree first.
func TestResultAndReport(t *testing.T) {
	f := newFixture(t)
	clock := time.Unix(0, 0)
	now := func() time.Time { clock = clock.Add(1500 * time.Millisecond); return clock }
	key := types.ComputationKey{Key: types.Digest("sha256:" + strings.Repeat("a", 64)), Generation: 1}
	report := types.Digest("sha256:" + strings.Repeat("b", 64))
	project := types.ProjectID("prj_01h455vb4pex5vsknk084sn02q")
	for _, c := range []struct {
		files   map[string]string
		outcome types.Outcome
		kinds   []types.RefType
	}{
		{map[string]string{"money.py": "x = 1\n"}, types.OutcomeEmpty, []types.RefType{types.RefTypeGitTree, types.RefTypeBlob}},
		{map[string]string{"conftest.py": "x = 1\n"}, types.OutcomeFailed, []types.RefType{types.RefTypeBlob}},
	} {
		v, err := Accept(context.Background(), In{Repo: f.repo, Base: f.base, Patch: f.patch(t, c.files), Policy: f.policy, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		b, err := v.Report()
		var rep struct {
			ElapsedMS int64             `json:"elapsed_ms"`
			Blocks    []json.RawMessage `json:"blocks"`
		}
		if err != nil || json.Unmarshal(b, &rep) != nil || rep.ElapsedMS != 1500 || len(rep.Blocks) != len(v.Blocks) {
			t.Fatalf("report %s, %v", b, err)
		}
		r, err := Result(key, v, project, report)
		if err == nil {
			err = graph.Check(r)
		}
		if err != nil || r.Outcome != c.outcome || len(r.Value) != len(c.kinds) || !r.Completed {
			t.Fatalf("result %+v, %v", r, err)
		}
		for i, k := range c.kinds {
			if r.Value[i].Type != k {
				t.Errorf("value %d is %s, want %s", i, r.Value[i].Type, k)
			}
		}
	}
}
