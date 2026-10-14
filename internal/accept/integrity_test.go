// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// guardBase adds to baseFiles a TestMain, an example, a root test and a
// root go.mod.
var guardBase = map[string]string{
	"pkg/main_test.go":    "package pkg\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) { os.Exit(m.Run()) }\n",
	"pkg/example_test.go": "package pkg\n\nimport \"fmt\"\n\nfunc ExampleAdd() {\n\tfmt.Println(Add(1, 2))\n\t// Output: 3\n}\n",
	"go.mod":              "module example.com/m\n\ngo 1.22\n",
	"root_test.go":        "package m\n\nimport \"testing\"\n\nfunc TestRoot(t *testing.T) { t.Fatal() }\n",
}

// forgeGo is the Opus probe of #255 (C1): a failing TestAdd reads pass
// under go test -json.
const forgeGo = "package pkg\n\nimport (\n\t\"os\"\n\t\"strings\"\n\t\"syscall\"\n)\n\nfunc init() {\n" +
	"\tif strings.HasSuffix(os.Args[0], \".test\") {\n\t\tm := string(rune(22))\n" +
	"\t\tos.Stdout.WriteString(m + \"=== RUN   TestAdd\\n\" + m + \"--- PASS: TestAdd (0.00s)\\n\" + m + \"PASS\\n\")\n" +
	"\t\tsyscall.Exit(0)\n\t}\n}\n"

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

func finds(bs []Block, path, what string) bool {
	return slices.ContainsFunc(bs, func(b Block) bool { return (path == "" || b.Path == path) && strings.Contains(b.What, what) })
}

type guardCase struct {
	files map[string]string
	gone  []string
	why   string // the finding the row must raise
}

// F (P14b, #226 entry 68; #255 round 2): each way to lose, skip or weaken
// a Go test, or to forge its result, that the reviews of #252 and #255
// found is refused, failed/test_integrity_violation, with the rule that
// discriminates it. A Python change in a base with Python tests is
// refused until P14c.
func TestIntegrityBlocks(t *testing.T) {
	f := newGuardFixture(t)
	goT, calc := baseFiles["pkg/calc_test.go"], baseFiles["pkg/calc.go"]
	goRep := func(old, new string) map[string]string {
		return map[string]string{"pkg/calc_test.go": strings.Replace(goT, old, new, 1)}
	}
	for name, c := range map[string]guardCase{
		// Astra #252 H1, Opus #255 H3: Go tests renamed or moved out of execution.
		"_calc_test.go":         {map[string]string{"pkg/_calc_test.go": goT}, []string{"pkg/calc_test.go"}, "no longer collected"},
		"calc_windows_test.go":  {map[string]string{"pkg/calc_windows_test.go": goT}, []string{"pkg/calc_test.go"}, "no longer collected"},
		"calc_amd64_test.go":    {map[string]string{"pkg/calc_amd64_test.go": goT}, []string{"pkg/calc_test.go"}, "no longer surely collected (linux/arm64)"},
		"calc.test.go":          {map[string]string{"pkg/calc.test.go": goT}, []string{"pkg/calc_test.go"}, "no longer collected"},
		"into testdata":         {map[string]string{"pkg/testdata/calc_test.go": goT}, []string{"pkg/calc_test.go"}, "no longer collected"},
		"a root test renamed":   {map[string]string{"_root_test.go": guardBase["root_test.go"]}, []string{"root_test.go"}, "no longer collected"},
		"go:build ignore":       {map[string]string{"pkg/calc_test.go": "//go:build ignore\n\n" + goT}, nil, "no longer surely collected"},
		"+build windows":        {map[string]string{"pkg/calc_test.go": "// +build windows\n\n" + goT}, nil, "no longer surely collected"},
		"go:build integration":  {map[string]string{"pkg/calc_test.go": "//go:build integration\n\n" + goT}, nil, "no longer surely collected"},
		"!goexperiment":         {map[string]string{"pkg/calc_test.go": "//go:build !goexperiment.greenteagc\n\n" + goT}, nil, "no longer surely collected"},
		"package documentation": {goRep("package pkg", "package documentation"), nil, "no longer surely collected"},
		"a nested go.mod":       {map[string]string{"pkg/go.mod": "module example.com/pkg\n"}, nil, "go.mod added over tests"},
		"root go.mod ignore":    {map[string]string{"go.mod": guardBase["go.mod"] + "\nignore ./pkg // gone\n"}, nil, "ignore directives changed"},
		// Astra #252 H2: tests and assertions hidden in comments and strings.
		"go test commented out": {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "func TestAdd", "/*\nfunc TestAdd", 1) + "*/\n"}, nil, "a test deleted"},
		"go test in a string":   {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "func TestAdd", "var _ = `\nfunc TestAdd", 1) + "`\n"}, nil, "a test deleted"},
		"go assert commented":   {goRep("t.Fatal", "// t.Fatal"), nil, "fewer assertions"},
		// Opus #255 H7: assertions replaced by look-alikes.
		"err.Error()":     {goRep("t.Fatal(\"add\")", "_ = err.Error()"), nil, "fewer assertions"},
		"a no-op check":   {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "t.Fatal(\"add\")", "checkNothing(false)", 1) + "\nfunc checkNothing(bool) {}\n"}, nil, "fewer assertions"},
		"output not last": {map[string]string{"pkg/example_test.go": strings.Replace(guardBase["pkg/example_test.go"], "// Output: 3\n", "// Output: 3\n\t_ = 0\n\t// done\n", 1)}, nil, "fewer assertions"},
		// Skips: in a test, through a method value (#255 H2), in a helper.
		"go skip added":       {goRep("{\n\tif", "{\n\tt.Skip()\n\tif"), nil, "a test skipped"},
		"go skip by value":    {goRep("{\n\tif", "{\n\tskip := t.Skip\n\tskip()\n\tif"), nil, "a test skipped"},
		"go skip in a helper": {map[string]string{"pkg/calc.go": calc + "\nfunc skip(t interface{ Skip(...any) }) { t.Skip() }\n"}, nil, "a skip added outside a test"},
		// #226 entry 32 and Opus #255 C1: test2json forgery.
		"testing.Testing":         {map[string]string{"pkg/calc.go": "package pkg\n\nimport tt \"testing\"\n\nvar _ = tt.Testing()\n"}, nil, "forge"},
		"testing.Testing a value": {map[string]string{"pkg/calc.go": "package pkg\n\nimport \"testing\"\n\nvar isT = testing.Testing\n"}, nil, "forge"},
		"0x16 written":            {map[string]string{"pkg/calc.go": calc + "\nconst x = \"\\026\"\n"}, nil, "forge"},
		"forge.go":                {map[string]string{"pkg/forge.go": forgeGo}, nil, "forge"},
		"forge through a helper": {map[string]string{"pkg/forge.go": "package pkg\n\nimport (\n\t\"flag\"\n\txos \"os\"\n)\n\nvar _ = setup()\n\n" +
			"func setup() int {\n\tif flag.Lookup(\"test.v\") != nil {\n\t\tquit()\n\t}\n\treturn 0\n}\n\nfunc quit() { xos.Exit(0) }\n"}, nil, "forge"},
		// Read bounds fail closed for every Go file (Opus #255 H1).
		"go test not parsed":  {map[string]string{"pkg/calc_test.go": goT + "func {\n"}, nil, "unverifiable"},
		"a helper not parsed": {map[string]string{"pkg/calc.go": calc + "func {\n"}, nil, "unverifiable"},
		"a large helper":      {map[string]string{"pkg/big.go": "package pkg\n\n//" + strings.Repeat("x", 5<<20) + "\n"}, nil, "unverifiable"},
		// Deletions.
		"test removed":      {map[string]string{"pkg/calc_test.go": "package pkg\n"}, nil, "a test deleted"},
		"test file deleted": {nil, []string{"pkg/calc_test.go"}, "a test deleted"},
		// Python until P14c (Opus #255 split).
		"python helper":     {map[string]string{"money.py": "def add(a, b):\n    return 0\n"}, nil, "python integrity not verified (P14c)"},
		"python test gone":  {nil, []string{"tests/test_money.py"}, "python integrity not verified (P14c)"},
		"a nested conftest": {map[string]string{"tests/conftest.py": "collect_ignore = ['test_money.py']\n"}, nil, "P14c"},
		"pytest.toml":       {map[string]string{"pytest.toml": "[pytest]\naddopts = ['--ignore=tests']\n"}, nil, "P14c"},
	} {
		v := f.guarded(t, c.files, c.gone...)
		if v.Reason() != vocab.ReasonTestIntegrityViolation || !finds(v.Blocks, "", c.why) {
			t.Errorf("%s: %q, want test_integrity_violation (%s)", name, whats(v.Blocks), c.why)
		}
	}
}

// The controls: ordinary changes the guard accepts, and the heuristics it
// only warns the reviewer of (Astra #252 M7, Opus #255 C1).
func TestIntegrityAcceptsAndWarns(t *testing.T) {
	f := newGuardFixture(t)
	goT, calc := baseFiles["pkg/calc_test.go"], baseFiles["pkg/calc.go"]
	for name, c := range map[string]guardCase{
		"go test renamed":     {map[string]string{"pkg/add_test.go": goT}, []string{"pkg/calc_test.go"}, ""},
		"go test moved":       {map[string]string{"pkg/calc_test.go": "package pkg\n", "pkg/more_test.go": goT}, nil, ""},
		"go:build linux":      {map[string]string{"pkg/calc_test.go": "//go:build linux && !windows && go1.20\n\n" + goT}, nil, ""},
		"an assertion added":  {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "{\n\tif", "{\n\tif Add(0, 0) != 0 {\n\t\tt.Error(\"zero\")\n\t}\n\tif", 1)}, nil, ""},
		"a helper passed t":   {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "t.Fatal(\"add\")", "checkAdd(t)", 1) + "\nfunc checkAdd(t *testing.T) { t.Helper() }\n"}, nil, ""},
		"a subtest":           {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "t.Fatal(\"add\")", "t.Run(\"x\", func(t *testing.T) { t.Fatal(\"add\") })", 1)}, nil, ""},
		"a comment and a doc": {map[string]string{"pkg/calc.go": calc + "\n// t.Skip() in a comment\nvar doc = \"t.Skip()\"\n"}, nil, ""},
		"TestMain changed":    {map[string]string{"pkg/main_test.go": strings.Replace(guardBase["pkg/main_test.go"], "m.Run()", "0", 1)}, nil, "TestMain changed"},
		"init exits":          {map[string]string{"pkg/calc.go": "package pkg\n\nimport \"os\"\n\nfunc init() { os.Exit(0) }\n"}, nil, "an init calling an exit changed"},
		"an aliased exit":     {map[string]string{"pkg/calc.go": "package pkg\n\nimport xos \"os\"\n\nfunc quit() { xos.Exit(1) }\n"}, nil, "exits the process"},
		"rune(22)":            {map[string]string{"pkg/calc.go": calc + "\nvar m = string(rune(0x16))\n"}, nil, "converts 22"},
	} {
		v := f.guarded(t, c.files, c.gone...)
		if !v.Accepted() || c.why != "" && !finds(v.Warnings, "", c.why) {
			t.Errorf("%s: blocks %q, warnings %q; want accepted (warning %q)", name, whats(v.Blocks), whats(v.Warnings), c.why)
		}
	}
	// A base without Python tests: Python changes are not P14c's to verify.
	g := newFixtureWith(t, func(src string) { plain(t, src, "rm", "-q", "--cached", "tests/test_money.py") })
	if v := g.guarded(t, map[string]string{"money.py": "x = 1\n"}); !v.Accepted() {
		t.Errorf("python without python tests: %q", whats(v.Blocks))
	}
}

// Opus #255 H1, L2, L3, H8: the read bounds fail closed for Go files, an
// unread file is reported once, a base list over its bound is the base's
// (not the candidate's unsafe output), and a cancelled guard stops.
func TestIntegrityBounds(t *testing.T) {
	f := newGuardFixture(t)
	oldRead, oldFile := maxRead, maxFile
	t.Cleanup(func() { maxRead, maxFile = oldRead, oldFile })
	maxRead = 300 // pkg/a.go is read whole, then pkg/zz.go is past the budget
	v := f.guarded(t, map[string]string{"pkg/a.go": "package pkg\n\n//" + strings.Repeat("x", 280) + "\n", "pkg/zz.go": "package pkg\n\nvar _ = 1\n"})
	if !finds(v.Blocks, "pkg/zz.go", "unverifiable") {
		t.Errorf("past the budget: %q", whats(v.Blocks))
	}
	maxRead, maxFile = oldRead, 10
	if v := f.guarded(t, map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n"}); len(v.Blocks) != 1 {
		t.Errorf("an unread file: %q, want one block", whats(v.Blocks))
	}
	maxFile = oldFile

	h := newFixtureWith(t, func(src string) {
		for i := range 300 {
			write(t, src, map[string]string{fmt.Sprintf("many/%s%d.txt", strings.Repeat("x", 60), i): "x\n"})
		}
		plain(t, src, "add", "--all")
	})
	old := maxPatch
	maxPatch = 8 << 10
	t.Cleanup(func() { maxPatch = old })
	v = h.guarded(t, map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\nfunc skip(t interface{ Skip() }) { t.Skip() }\n"})
	if v.Reason() != vocab.ReasonTestIntegrityViolation || !finds(v.Blocks, "", "file list is over its bound") {
		t.Errorf("a base list over its bound: %s %q", v.Reason(), whats(v.Blocks))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	id := strings.Repeat("a", 40)
	read := func(context.Context, string, int64) ([]byte, bool, error) { return []byte("package a\n"), true, nil }
	if _, _, err := Integrity(sandbox.Repo{}, id)(ctx, []Change{{Status: "M", Src: "a.go", Dst: "a.go", SrcBlob: id, DstBlob: id}}, read); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled guard: %v", err)
	}
}

// Go's own collection rules, per gate platform: 2 surely collected, 1
// depending on tags the guard does not model, 0 never (#255 H3).
func TestGoOn(t *testing.T) {
	on := func(p, src string) []int {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Base(p), src, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		o, _ := goOn(p, f)
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
