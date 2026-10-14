// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// guardBase adds to baseFiles a TestMain, an example, a root test, an
// external test package, an ordinary test named TestMain, a test file
// under a constraint the guard does not model, and a root go.mod.
var guardBase = map[string]string{
	"pkg/main_test.go":    "package pkg\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) { os.Exit(m.Run()) }\n",
	"pkg/example_test.go": "package pkg\n\nimport \"fmt\"\n\nfunc ExampleAdd() {\n\tfmt.Println(Add(1, 2))\n\t// Output: 3\n}\n",
	"pkg/ext_test.go":     "package pkg_test\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif 1+2 != 3 {\n\t\tt.Fatal(\"add\")\n\t}\n}\n",
	"other/a_test.go":     "package other\n\nimport \"testing\"\n\nfunc TestMain(t *testing.T) { t.Fatal() }\n\nfunc TestOther(t *testing.T) { t.Fatal() }\n",
	"race/r_test.go":      "//go:build !race\n\npackage race\n\nimport \"testing\"\n\nfunc TestR(t *testing.T) { t.Fatal() }\n",
	"go.mod":              "module example.com/m\n\ngo 1.22\n",
	"root_test.go":        "package m\n\nimport \"testing\"\n\nfunc TestRoot(t *testing.T) { t.Fatal() }\n",
}

// The forgeries of the #255 reviews: a failing TestAdd reads pass under
// go test -json. linkGo is Astra's (Critical): no recognised exit, no
// marker spelling.
const (
	forgeGo = "package pkg\n\nimport (\n\t\"os\"\n\t\"strings\"\n\t\"syscall\"\n)\n\nfunc init() {\n" +
		"\tif strings.HasSuffix(os.Args[0], \".test\") {\n\t\tm := string(rune(22))\n" +
		"\t\tos.Stdout.WriteString(m + \"=== RUN   TestAdd\\n\" + m + \"--- PASS: TestAdd (0.00s)\\n\" + m + \"PASS\\n\")\n" +
		"\t\tsyscall.Exit(0)\n\t}\n}\n"
	linkGo = "package pkg\n\nimport (\n\t\"os\"\n\t_ \"unsafe\"\n)\n\n//go:linkname terminate syscall.Exit\nfunc terminate(code int)\n\n" +
		"func init() {\n\tm := string([]byte{22})\n\t_, _ = os.Stdout.WriteString(m + \"--- PASS: TestAdd (0.00s)\\n\" + m + \"PASS\\n\")\n\tterminate(0)\n}\n"
)

func newGuardFixture(t *testing.T) fixture {
	return newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		plain(t, src, "add", "--all")
	})
}

// guarded accepts a patch of the base through the integrity guard.
func (f fixture) guarded(t *testing.T, files map[string]string, gone ...string) Verdict {
	t.Helper()
	v, err := Accept(context.Background(), In{Repo: f.repo, Base: f.base, Patch: f.patch(t, files, gone...), Policy: f.policy,
		Guard: Integrity(f.repo, f.base)})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func whats(bs []Block) []string {
	var out []string
	for _, b := range bs {
		out = append(out, b.Path+": "+b.What)
	}
	return out
}

func finds(bs []Block, what string) bool {
	return slices.ContainsFunc(bs, func(b Block) bool { return strings.Contains(b.Path+" "+b.What, what) })
}

type guardCase struct {
	files map[string]string
	gone  []string
	why   string // the finding the row must raise
}

// F (P14b, #226 entry 68; #255 rounds 2 and 3): each way to lose, skip or
// weaken a Go test, or to forge its result, that the reviews of #252 and
// #255 found is refused, failed/test_integrity_violation, with the rule
// that discriminates it. A Python change in a base holding Python is
// refused until P14c.
func TestIntegrityBlocks(t *testing.T) {
	f := newGuardFixture(t)
	goT, calc := baseFiles["pkg/calc_test.go"], baseFiles["pkg/calc.go"]
	goRep := func(old, new string) map[string]string {
		return map[string]string{"pkg/calc_test.go": strings.Replace(goT, old, new, 1)}
	}
	for name, c := range map[string]guardCase{
		// Collection (Astra #252 H1; Opus #255 H3; Astra #255 H3, H4).
		"_calc_test.go":         {map[string]string{"pkg/_calc_test.go": goT}, []string{"pkg/calc_test.go"}, "no longer collected"},
		"calc_windows_test.go":  {map[string]string{"pkg/calc_windows_test.go": goT}, []string{"pkg/calc_test.go"}, "no longer collected"},
		"calc_amd64_test.go":    {map[string]string{"pkg/calc_amd64_test.go": goT}, []string{"pkg/calc_test.go"}, "linux/arm64 pkg:pkg.TestAdd"},
		"calc.test.go":          {map[string]string{"pkg/calc.test.go": goT}, []string{"pkg/calc_test.go"}, "no longer collected"},
		"into testdata":         {map[string]string{"pkg/testdata/calc_test.go": goT}, []string{"pkg/calc_test.go"}, "no longer collected"},
		"a root test renamed":   {map[string]string{"_root_test.go": guardBase["root_test.go"]}, []string{"root_test.go"}, "no longer collected"},
		"go:build ignore":       {map[string]string{"pkg/calc_test.go": "//go:build ignore\n\n" + goT}, nil, "no longer surely collected"},
		"+build windows":        {map[string]string{"pkg/calc_test.go": "// +build windows\n\n" + goT}, nil, "no longer collected"},
		"go:build integration":  {map[string]string{"pkg/calc_test.go": "//go:build integration\n\n" + goT}, nil, "no longer surely collected"},
		"!goexperiment":         {map[string]string{"pkg/calc_test.go": "//go:build !goexperiment.greenteagc\n\n" + goT}, nil, "no longer surely collected"},
		"package documentation": {goRep("package pkg", "package documentation"), nil, "no longer collected"},
		"moved to amd64 only":   {map[string]string{"other/a_test.go": strings.SplitAfter(guardBase["other/a_test.go"], "t.Fatal() }\n")[0], "other/b_amd64_test.go": "package other\n\nimport \"testing\"\n\nfunc TestOther(t *testing.T) { t.Fatal() }\n"}, nil, "linux/arm64 other:other.TestOther"},
		"!race renamed amd64":   {map[string]string{"race/r_amd64_test.go": guardBase["race/r_test.go"]}, []string{"race/r_test.go"}, "linux/arm64 race:race.TestR"},
		"pkg and pkg_test":      {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "t.Fatal(\"add\")", "_ = 0", 1), "pkg/ext_test.go": strings.Replace(guardBase["pkg/ext_test.go"], "t.Fatal(\"add\")", "t.Fatal(\"add\")\n\t\tt.Error(\"again\")", 1)}, nil, "pkg:pkg.TestAdd"},
		"TestMain(*testing.T)":  {map[string]string{"other/a_test.go": "package other\n\nimport \"testing\"\n\nfunc TestOther(t *testing.T) { t.Fatal() }\n"}, nil, "other:other.TestMain"},
		"a nested go.mod":       {map[string]string{"pkg/go.mod": "module example.com/pkg\n"}, nil, "go.mod added over tests"},
		"root go.mod ignore":    {map[string]string{"go.mod": guardBase["go.mod"] + "\nignore ./pkg // gone\n"}, nil, "ignore directives changed"},
		// Tests and assertions hidden in comments and strings (Astra #252 H2).
		"go test commented out": {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "func TestAdd", "/*\nfunc TestAdd", 1) + "*/\n"}, nil, "a test deleted"},
		"go test in a string":   {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "func TestAdd", "var _ = `\nfunc TestAdd", 1) + "`\n"}, nil, "a test deleted"},
		"go assert commented":   {goRep("t.Fatal", "// t.Fatal"), nil, "fewer assertions"},
		// Assertion look-alikes (Opus #255 H7, Astra #255 H2).
		"err.Error()":      {goRep("t.Fatal(\"add\")", "_ = err.Error()"), nil, "fewer assertions"},
		"t.Failed()":       {goRep("t.Fatal(\"add\")", "_ = t.Failed()"), nil, "fewer assertions"},
		"a shadowed t":     {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "t.Fatal(\"add\")", "t := fake{}\n\t\tt.Fatal(\"add\")", 1) + "\ntype fake struct{}\n\nfunc (fake) Fatal(...any) {}\n"}, nil, "fewer assertions"},
		"a no-op check":    {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "t.Fatal(\"add\")", "checkNothing(false)", 1) + "\nfunc checkNothing(bool) {}\n"}, nil, "fewer assertions"},
		"a helper given t": {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "t.Fatal(\"add\")", "checkAdd(t)", 1) + "\nfunc checkAdd(t *testing.T) { t.Helper() }\n"}, nil, "fewer assertions"},
		"output not last":  {map[string]string{"pkg/example_test.go": strings.Replace(guardBase["pkg/example_test.go"], "// Output: 3\n", "// Output: 3\n\t_ = 0\n\t// done\n", 1)}, nil, "fewer assertions"},
		// Skips (Opus #255 H2).
		"go skip added":       {goRep("{\n\tif", "{\n\tt.Skip()\n\tif"), nil, "a test skipped"},
		"go skip by value":    {goRep("{\n\tif", "{\n\tskip := t.Skip\n\tskip()\n\tif"), nil, "a test skipped"},
		"go skip in a helper": {map[string]string{"pkg/calc.go": calc + "\nfunc skip(t interface{ Skip(...any) }) { t.Skip() }\n"}, nil, "a skip added outside a test"},
		"MethodByName":        {goRep("{\n\tif", "{\n\treflect.ValueOf(t).MethodByName(\"SkipNow\").Call(nil)\n\tif"), nil, "reflection on a testing value"},
		// Forgery (#226 entry 32; Opus #255 C1; Astra #255 Critical).
		"testing.Testing":           {map[string]string{"pkg/calc.go": "package pkg\n\nimport tt \"testing\"\n\nvar _ = tt.Testing()\n"}, nil, "forge"},
		"testing.Testing a value":   {map[string]string{"pkg/calc.go": "package pkg\n\nimport \"testing\"\n\nvar isT = testing.Testing\n"}, nil, "forge"},
		"0x16 written":              {map[string]string{"pkg/calc.go": calc + "\nconst x = \"\\026\"\n"}, nil, "forge"},
		"forge.go":                  {map[string]string{"pkg/forge.go": forgeGo}, nil, "initialization (tests included) reaches an exit (syscall.Exit)"},
		"linkname forgery":          {map[string]string{"pkg/forge.go": linkGo}, nil, "a //go:linkname directive"},
		"an unsafe import":          {map[string]string{"pkg/u.go": "package pkg\n\nimport \"unsafe\"\n\nvar _ = unsafe.Sizeof(0)\n"}, nil, "an import of unsafe"},
		"linkname reached":          {map[string]string{"pkg/forge.go": linkGo}, nil, "a call it cannot resolve (terminate)"},
		"init in a _test.go":        {map[string]string{"pkg/zz_test.go": "package pkg\n\nimport \"os\"\n\nfunc init() { os.Stdout.WriteString(\"PASS\\n\"); os.Exit(0) }\n"}, nil, "reaches output (os.Stdout)"},
		"split across files":        {map[string]string{"pkg/a.go": "package pkg\n\nfunc init() { quit() }\n", "pkg/b.go": "package pkg\n\nimport \"log\"\n\nfunc quit() { log.Fatal(\"PASS\") }\n"}, nil, "reaches an exit (log.Fatal)"},
		"init through an interface": {map[string]string{"pkg/a.go": "package pkg\n\ntype q interface{ quit() }\n\nvar Q q\n\nfunc init() { Q.quit() }\n"}, nil, "a call it cannot resolve (Q.quit)"},
		"init through an imported interface": {map[string]string{"pkg/a.go": "package pkg\n\nimport (\n\t\"io\"\n\t\"os\"\n)\n\ntype exiter struct{}\n\n" +
			"func (exiter) Write(p []byte) (int, error) { os.Exit(0); return 0, nil }\n\nvar w io.Writer = exiter{}\n\nfunc init() { w.Write(nil) }\n"}, nil, "a call it cannot resolve (w.Write)"},
		"assembly": {map[string]string{"pkg/exit_amd64.s": "TEXT ·terminate(SB),$0\n\tRET\n"}, nil, "assembly"},
		"a forged init through a changed package": {map[string]string{"pkg/a.go": "package pkg\n\nimport \"example.com/m/util\"\n\nfunc init() { util.Prepare() }\n", "util/u.go": "package util\n\nfunc Prepare() {}\n"}, nil, "a call it cannot resolve (util.Prepare)"},
		// Read bounds fail closed for every Go file (Opus #255 H1).
		"go test not parsed":  {map[string]string{"pkg/calc_test.go": goT + "func {\n"}, nil, "unverifiable"},
		"a helper not parsed": {map[string]string{"pkg/calc.go": calc + "func {\n"}, nil, "unverifiable"},
		"a large helper":      {map[string]string{"pkg/big.go": "package pkg\n\n//" + strings.Repeat("x", 5<<20) + "\n"}, nil, "unverifiable"},
		// Deletions.
		"test removed":      {map[string]string{"pkg/calc_test.go": "package pkg\n"}, nil, "a test deleted"},
		"test file deleted": {nil, []string{"pkg/calc_test.go"}, "a test deleted"},
		// Python until P14c (Opus and Astra #255).
		"python helper":     {map[string]string{"money.py": "def add(a, b):\n    return 0\n"}, nil, "python integrity not verified (P14c)"},
		"python test gone":  {nil, []string{"tests/test_money.py"}, "P14c"},
		"a nested conftest": {map[string]string{"tests/conftest.py": "collect_ignore = ['test_money.py']\n"}, nil, "P14c"},
		"pytest.toml":       {map[string]string{"pytest.toml": "[pytest]\naddopts = ['--ignore=tests']\n"}, nil, "P14c"},
		"conda-meta":        {map[string]string{"tests/conda-meta/history": "x\n"}, nil, "P14c"},
	} {
		v := f.guarded(t, c.files, c.gone...)
		if v.Reason() != vocab.ReasonTestIntegrityViolation || !finds(v.Blocks, c.why) {
			t.Errorf("%s: %q, want test_integrity_violation (%s)", name, whats(v.Blocks), c.why)
		}
	}
	// Astra #255 H7: a base whose only Python tests are named by a custom
	// pattern (python_files = check_*.py) is a base holding Python.
	g := newFixtureWith(t, func(src string) {
		plain(t, src, "mv", "tests/test_money.py", "tests/check_money.py")
		write(t, src, map[string]string{"pytest.ini": "[pytest]\npython_files = check_*.py\n"})
		plain(t, src, "add", "--all")
	})
	if v := g.guarded(t, map[string]string{"tests/check_money.py": "def test_add():\n    pass\n"}); !finds(v.Blocks, "P14c") {
		t.Errorf("custom python_files: %q", whats(v.Blocks))
	}
}

// The controls: ordinary changes the guard accepts with no warning, and
// the heuristics it warns the reviewer of (Astra #252 M7, #255 H4).
func TestIntegrityAcceptsAndWarns(t *testing.T) {
	f := newGuardFixture(t)
	goT, calc := baseFiles["pkg/calc_test.go"], baseFiles["pkg/calc.go"]
	for name, c := range map[string]guardCase{
		"go test renamed":     {map[string]string{"pkg/add_test.go": goT}, []string{"pkg/calc_test.go"}, ""},
		"go test moved":       {map[string]string{"pkg/calc_test.go": "package pkg\n", "pkg/more_test.go": goT}, nil, ""},
		"go:build linux":      {map[string]string{"pkg/calc_test.go": "//go:build linux && !windows && go1.20\n\n" + goT}, nil, ""},
		"an assertion added":  {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "{\n\tif", "{\n\tif Add(0, 0) != 0 {\n\t\tt.Error(\"zero\")\n\t}\n\tif", 1)}, nil, ""},
		"a helper that fails": {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "if Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}", "checkAdd(t, Add(1, 2))", 1) + "\nfunc checkAdd(t *testing.T, got int) {\n\tif got != 3 {\n\t\tt.Fatal(\"add\")\n\t}\n}\n"}, nil, ""},
		"a subtest":           {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "t.Fatal(\"add\")", "t.Run(\"x\", func(t *testing.T) { t.Fatal(\"add\") })", 1)}, nil, ""},
		"a comment and a doc": {map[string]string{"pkg/calc.go": calc + "\n// t.Skip() in a comment\nvar doc = \"t.Skip()\"\n"}, nil, ""},
		"an ordinary init":    {map[string]string{"pkg/calc.go": calc + "\nvar table = map[string]int{}\n\nfunc init() { table[\"one\"] = Add(0, 1) }\n"}, nil, ""},
		"a new go file":       {map[string]string{"pkg/x.go": "package pkg\n"}, nil, ""},
		"TestMain changed":    {map[string]string{"pkg/main_test.go": strings.Replace(guardBase["pkg/main_test.go"], "m.Run()", "0", 1)}, nil, "TestMain(*testing.M) of pkg:pkg changed or deleted"},
		"TestMain deleted":    {nil, []string{"pkg/main_test.go"}, "TestMain(*testing.M) of pkg:pkg changed or deleted"},
		"an exit added":       {map[string]string{"pkg/calc.go": "package pkg\n\nimport xos \"os\"\n\nfunc quit() { xos.Exit(1) }\n"}, nil, "its exits changed"},
		"rune(22)":            {map[string]string{"pkg/calc.go": calc + "\nvar m = string(rune(0x16))\n"}, nil, "converts 22"},
	} {
		v := f.guarded(t, c.files, c.gone...)
		if !v.Accepted() || c.why == "" && len(v.Warnings) > 0 || c.why != "" && !finds(v.Warnings, c.why) {
			t.Errorf("%s: blocks %q, warnings %q; want accepted (warning %q)", name, whats(v.Blocks), whats(v.Warnings), c.why)
		}
	}
	// A base holding no Python: a Python file is not P14c's to verify.
	g := newFixtureWith(t, func(src string) {
		plain(t, src, "rm", "-q", "--cached", "tests/test_money.py", "money.py", "conftest.py")
	})
	if v := g.guarded(t, map[string]string{"tool.py": "x = 1\n"}); !v.Accepted() {
		t.Errorf("python in a base without python: %q", whats(v.Blocks))
	}
}

// countCtx is cancelled at its n-th Err call: a cancellation mid-analysis.
type countCtx struct {
	context.Context
	calls, n atomic.Int64
}

func (c *countCtx) Err() error {
	if c.calls.Add(1) >= c.n.Load() {
		return context.Canceled
	}
	return nil
}

// Opus #255 H1, L2, L3 and Astra #255 H8: the read bounds fail closed for
// Go files, an unread file is reported once, a base list over its bound is
// the base's (not the candidate's unsafe output), constraint evaluation is
// bounded, and analysis stops when cancelled midway.
func TestIntegrityBounds(t *testing.T) {
	f := newGuardFixture(t)
	oldRead, oldFile, oldWork := maxRead, maxFile, maxWork
	t.Cleanup(func() { maxRead, maxFile, maxWork = oldRead, oldFile, oldWork })
	maxRead = 1 << 10 // the base's pkg files are read, then pkg/zz.go is past the budget
	v := f.guarded(t, map[string]string{"pkg/a.go": "package pkg\n\n//" + strings.Repeat("x", 600) + "\n", "pkg/zz.go": "package pkg\n\nvar _ = 1\n"})
	if !finds(v.Blocks, "unverifiable: over the guard's read bound") {
		t.Errorf("past the budget: %q", whats(v.Blocks))
	}
	maxRead, maxFile = oldRead, 40 // pkg/calc.go is over it in both versions
	v = f.guarded(t, map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "//" + strings.Repeat("x", 100) + "\n"})
	if n := len(slices.DeleteFunc(slices.Clone(v.Blocks), func(b Block) bool { return b.Path != "pkg/calc.go" })); n != 1 {
		t.Errorf("an unread file: %q, want pkg/calc.go once", whats(v.Blocks))
	}
	maxFile = oldFile

	// Astra's H8 probe, scaled: tautologies over twelve unmodelled tags.
	heavy := strings.Repeat("// +build a !a b !b c !c d !d e !e f !f g !g h !h i !i j !j k !k l !l\n", 3) + "\npackage pkg\n"
	maxWork = 1 << 12
	if v := f.guarded(t, map[string]string{"pkg/h_test.go": heavy}); !finds(v.Blocks, "work bound") {
		t.Errorf("over the work bound: %q", whats(v.Blocks))
	}
	maxWork = oldWork
	ctx := &countCtx{Context: context.Background()}
	ctx.n.Store(1 << 40)
	in := In{Repo: f.repo, Base: f.base, Patch: f.patch(t, map[string]string{"pkg/h_test.go": heavy}), Policy: f.policy, Guard: Integrity(f.repo, f.base)}
	start := time.Now()
	if _, err := Accept(ctx, in); err != nil {
		t.Fatal(err)
	}
	total := ctx.calls.Load()
	ctx.calls.Store(0)
	ctx.n.Store(total / 2)
	if _, err := Accept(ctx, in); err == nil || total < 1000 {
		t.Errorf("cancelled midway (%d checks, %v): %v", total, time.Since(start), err)
	}

	h := newFixtureWith(t, func(src string) {
		for i := range 300 {
			write(t, src, map[string]string{fmt.Sprintf("many/%s%d.txt", strings.Repeat("x", 60), i): "x\n"})
		}
		plain(t, src, "add", "--all")
	})
	old := maxPatch
	maxPatch = 8 << 10
	t.Cleanup(func() { maxPatch = old })
	v = h.guarded(t, map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n"})
	if v.Reason() != vocab.ReasonTestIntegrityViolation || !finds(v.Blocks, "file list is over its bound") {
		t.Errorf("a base list over its bound: %s %q", v.Reason(), whats(v.Blocks))
	}
}

// Go's own collection rules, per gate platform: 2 surely collected, 1
// depending on tags the guard does not model, 0 never (#255 H3).
func TestGoOn(t *testing.T) {
	r := &run{ctx: context.Background()}
	on := func(p, src string) []int {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Base(p), src, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		o, _, err := r.goOn(p, f)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	for p, want := range map[string][]int{
		"a_test.go": {2, 2}, "p/a_test.go": {2, 2}, "p/a_linux_test.go": {2, 2}, "p/a_arm64_test.go": {0, 2}, "p/a_darwin_test.go": {0, 0},
		"p/_a_test.go": {0, 0}, "p/.a_test.go": {0, 0}, "p/testdata/a_test.go": {0, 0}, "vendor/x/a_test.go": {0, 0}, "_x/a_test.go": {0, 0}, "p/a.go": {0, 0},
	} {
		if got := on(p, "package p\n"); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", p, got, want)
		}
	}
	for c, want := range map[string][]int{"//go:build linux": {2, 2}, "//go:build !linux": {0, 0}, "//go:build go1.1": {2, 2},
		"//go:build go1.999": {0, 0}, "//go:build cgo": {1, 1}, "//go:build !integration": {1, 1}, "//go:build amd64 || ignore": {2, 1},
		"// +build linux,arm64": {0, 2}, "/*\n//go:build ignore\n*/": {2, 2}, "//go:build !arm64.v8.0": {1, 1}, "//go:build windows": {0, 0}} {
		if got := on("p/a_test.go", c+"\n\npackage p\n"); !slices.Equal(got, want) {
			t.Errorf("%q: %v, want %v", c, got, want)
		}
	}
	if got := on("p/a_test.go", "package documentation\n"); !slices.Equal(got, []int{0, 0}) {
		t.Errorf("package documentation: %v", got)
	}
}
