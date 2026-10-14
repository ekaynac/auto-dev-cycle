// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// guardBase adds to baseFiles a parametrized test, a test class, a
// TestMain and a root go.mod.
var guardBase = map[string]string{
	"tests/test_cases.py": "import pytest\nfrom money import add\n\n\n@pytest.mark.parametrize(\"a,b,want\", [\n    (1, 2, 3),\n    (2, 2, 4),\n])\n" +
		"def test_table(a, b, want):\n    assert add(a, b) == want\n\n\nclass TestAdd:\n    def test_method(self):\n        assert add(1, 1) == 2\n",
	"pkg/main_test.go": "package pkg\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) { os.Exit(m.Run()) }\n",
	"go.mod":           "module example.com/m\n\ngo 1.22\n",
	"root_test.go":     "package m\n\nimport \"testing\"\n\nfunc TestRoot(t *testing.T) { t.Fatal() }\n",
}

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

type guardCase struct {
	files map[string]string
	gone  []string
	why   string // the finding the row must raise
}

// F (P14b, #226 entry 68): each way to lose a test, a skip or an assertion
// that Astra's and Opus's reviews of #252 found is refused,
// failed/test_integrity_violation, with the rule that discriminates it.
func TestIntegrityBlocks(t *testing.T) {
	f := newGuardFixture(t)
	py, goT, cases := baseFiles["tests/test_money.py"], baseFiles["pkg/calc_test.go"], guardBase["tests/test_cases.py"]
	pyRep := func(old, new string) map[string]string {
		return map[string]string{"tests/test_money.py": strings.Replace(py, old, new, 1)}
	}
	for name, c := range map[string]guardCase{
		// Astra H1: Go tests renamed or moved out of execution.
		"_calc_test.go":         {map[string]string{"pkg/_calc_test.go": goT}, []string{"pkg/calc_test.go"}, "no longer collected"},
		"calc_windows_test.go":  {map[string]string{"pkg/calc_windows_test.go": goT}, []string{"pkg/calc_test.go"}, "no longer collected"},
		"calc_amd64_test.go":    {map[string]string{"pkg/calc_amd64_test.go": goT}, []string{"pkg/calc_test.go"}, "no longer collected (linux/arm64)"},
		"calc.test.go":          {map[string]string{"pkg/calc.test.go": goT}, []string{"pkg/calc_test.go"}, "no longer collected"},
		"into testdata":         {map[string]string{"pkg/testdata/calc_test.go": goT}, []string{"pkg/calc_test.go"}, "no longer collected"},
		"go:build ignore":       {map[string]string{"pkg/calc_test.go": "//go:build ignore\n\n" + goT}, nil, "no longer collected"},
		"+build windows":        {map[string]string{"pkg/calc_test.go": "// +build windows\n\n" + goT}, nil, "no longer collected"},
		"go:build integration":  {map[string]string{"pkg/calc_test.go": "//go:build integration\n\n" + goT}, nil, "no longer collected"},
		"a nested go.mod":       {map[string]string{"pkg/go.mod": "module example.com/pkg\n"}, nil, "go.mod added over tests"},
		"go test commented out": {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "func TestAdd", "/*\nfunc TestAdd", 1) + "*/\n"}, nil, "a test deleted"},
		"go test in a string":   {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "func TestAdd", "var _ = `\nfunc TestAdd", 1) + "`\n"}, nil, "a test deleted"},
		"go assert commented":   {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "t.Fatal", "// t.Fatal", 1)}, nil, "fewer assertions"},
		"go skip added":         {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "{\n\tif", "{\n\tt.Skip()\n\tif", 1)}, nil, "a test skipped"},
		"go skip in a helper":   {map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\nfunc skip(t interface{ Skip(...any) }) { t.Skip() }\n"}, nil, "a skip added outside a test"},
		"testing.Testing":       {map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\nvar _ = tt.Testing()\n"}, nil, "forge"},
		"0x16 written":          {map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\nconst x = \"\\026\"\n"}, nil, "forge"},
		// Astra H2: tests and assertions hidden in strings and comments.
		"tests in a triple-quoted string": {map[string]string{"tests/test_money.py": "\"\"\"\n" + py + "\"\"\"\n"}, nil, "a test deleted"},
		"tests in an Rb''' string":        {map[string]string{"tests/test_money.py": "x = Rb'''\n" + py + "'''\n"}, nil, "a test deleted"},
		"pass # assert x":                 {pyRep("assert add(0, 0) == 0", "pass  # assert add(0, 0) == 0"), nil, "fewer assertions"},
		"assert in a string":              {pyRep("assert add(0, 0) == 0", "'assert add(0, 0) == 0'"), nil, "fewer assertions"},
		"deleted, comment pad":            {map[string]string{"tests/test_money.py": strings.SplitAfter(py, "== 3\n")[0] + "\n# def test_zero(): assert\n"}, nil, "a test deleted"},
		"shadowed by a redefinition":      {map[string]string{"tests/test_money.py": py + "\n\ndef test_zero():\n    pass\n"}, nil, "fewer assertions"},
		// Astra H3: pytest collection controls, helpers, parametrize rows.
		"__test__ = False":           {map[string]string{"tests/test_money.py": "__test__ = False\n" + py}, nil, "no longer collected"},
		"a test class constructor":   {map[string]string{"tests/test_cases.py": strings.Replace(cases, "class TestAdd:\n", "class TestAdd:\n    def __init__(self):\n        pass\n\n", 1)}, nil, "no longer collected"},
		"nested conftest ignore":     {map[string]string{"tests/conftest.py": "collect_ignore = [\"test_money.py\"]\n"}, nil, "collection control"},
		"conftest modifyitems":       {map[string]string{"tests/sub/conftest.py": "def pytest_collection_modifyitems(items):\n    items.clear()\n"}, nil, "collection control"},
		"a plugin dropping tests":    {map[string]string{"tests/plugin.py": "def pytest_collection_modifyitems(items):\n    items.clear()\n"}, nil, "collection control"},
		"a test deleted by del":      {map[string]string{"tests/test_money.py": py + "\ndel test_zero\n"}, nil, "a test deleted"},
		"a test rebound":             {map[string]string{"tests/test_money.py": py + "\ntest_zero = None\n"}, nil, "a test deleted"},
		"a root test renamed":        {map[string]string{"_root_test.go": "package m\n\nimport \"testing\"\n\nfunc TestRoot(t *testing.T) { t.Fatal() }\n"}, []string{"root_test.go"}, "no longer collected"},
		"skip in a helper":           {map[string]string{"money.py": "import pytest\n\ndef add(a, b):\n    pytest.skip('later')\n    return a + b\n"}, nil, "a skip added outside a test"},
		"bare skip in a helper":      {map[string]string{"money.py": "from pytest import skip\n\ndef add(a, b):\n    skip()\n    return a + b\n"}, nil, "a skip added outside a test"},
		"importorskip":               {map[string]string{"tests/test_money.py": "import pytest\npytest.importorskip('nope')\n" + py}, nil, "a skip added outside a test"},
		"pytestmark":                 {map[string]string{"tests/test_money.py": "import pytest\npytestmark = pytest.mark.skip\n" + py}, nil, "a skip added outside a test"},
		"a parametrize row removed":  {map[string]string{"tests/test_cases.py": strings.Replace(cases, "    (2, 2, 4),\n", "", 1)}, nil, "a parametrize case removed"},
		"a parametrize row replaced": {map[string]string{"tests/test_cases.py": strings.Replace(cases, "(2, 2, 4)", "(0, 0, 0)", 1)}, nil, "a parametrize case removed"},
		"pytest.ini added":           {map[string]string{"pytest.ini": "[pytest]\naddopts = --ignore=tests\n"}, nil, "a pytest configuration changed"},
		"setup.cfg section":          {map[string]string{"setup.cfg": "[metadata]\nname = m\n\n[tool:pytest]\ntestpaths = docs\n"}, nil, "a pytest configuration changed"},
		// Opus round 1 and the ec00fee rows.
		"deleted test file":     {nil, []string{"tests/test_money.py"}, "a test deleted"},
		"skip added":            {pyRep("def test_add", "@pytest.mark.skip\ndef test_add"), nil, "a test skipped"},
		"xfail added":           {pyRep("def test_add", "@pytest.mark.xfail\ndef test_add"), nil, "a test skipped"},
		"made async":            {pyRep("def test_add", "async def test_add"), nil, "a test skipped"},
		"a class skipped":       {map[string]string{"tests/test_cases.py": strings.Replace(cases, "class TestAdd", "@pytest.mark.skip\nclass TestAdd", 1)}, nil, "a skip added outside a test"},
		"a param marked skip":   {map[string]string{"tests/test_cases.py": strings.Replace(cases, "(2, 2, 4)", "pytest.param(2, 2, 4, marks=pytest.mark.skip)", 1)}, nil, "a test skipped"},
		"method skipTest":       {map[string]string{"tests/test_cases.py": strings.Replace(cases, "(self):\n", "(self):\n        self.skipTest('x')\n", 1)}, nil, "a test skipped"},
		"assertion removed":     {pyRep("assert add(0, 0) == 0", "pass"), nil, "fewer assertions"},
		"test removed":          {map[string]string{"tests/test_money.py": strings.SplitAfter(py, "== 3\n")[0]}, nil, "a test deleted"},
		"renamed to non-test":   {map[string]string{"tests_old.txt": py}, []string{"tests/test_money.py"}, "no longer collected"},
		"renamed, uncollected":  {map[string]string{"tests/money_cases.py": py}, []string{"tests/test_money.py"}, "no longer collected"},
		"into a build dir":      {map[string]string{"tests/build/test_money.py": py}, []string{"tests/test_money.py"}, "no longer collected"},
		"assert moved to new":   {map[string]string{"tests/test_money.py": strings.Replace(py, "assert add(0, 0) == 0", "pass", 1), "tests/test_x.py": "def test_x():\n    assert 1 == 1\n"}, nil, "fewer assertions"},
		"padded over the bound": {map[string]string{"tests/test_money.py": strings.Replace(py, "assert add(0, 0) == 0", "pass", 1) + "#" + strings.Repeat("x", 5<<20) + "\n"}, nil, "unverifiable"},
		"not tokenizable":       {map[string]string{"tests/test_money.py": py + "x = 'unterminated\n"}, nil, "unverifiable"},
		"go not parsed":         {map[string]string{"pkg/calc_test.go": goT + "func {\n"}, nil, "unverifiable"},
	} {
		v := f.guarded(t, c.files, c.gone...)
		found := slices.ContainsFunc(v.Blocks, func(b Block) bool { return strings.Contains(b.What, c.why) })
		if v.Reason() != vocab.ReasonTestIntegrityViolation || !found {
			t.Errorf("%s: %q, want test_integrity_violation (%s)", name, whats(v.Blocks), c.why)
		}
	}
}

// The guard reads at most maxRead bytes in all: past the budget, a
// collected test source is unverifiable, never skipped.
func TestIntegrityBudget(t *testing.T) {
	f := newGuardFixture(t)
	old := maxRead
	maxRead = 200
	t.Cleanup(func() { maxRead = old })
	v := f.guarded(t, map[string]string{"money.py": baseFiles["money.py"] + "# " + strings.Repeat("x", 300) + "\n",
		"tests/test_money.py": baseFiles["tests/test_money.py"] + "\n"})
	if !slices.ContainsFunc(v.Blocks, func(b Block) bool { return b.Path == "tests/test_money.py" && strings.Contains(b.What, "unverifiable") }) {
		t.Fatalf("over the budget: %q", whats(v.Blocks))
	}
}

// The controls: ordinary refactors the guard must accept, and the
// heuristics it only warns the reviewer of (M7).
func TestIntegrityAcceptsAndWarns(t *testing.T) {
	f := newGuardFixture(t)
	py, goT, cases := baseFiles["tests/test_money.py"], baseFiles["pkg/calc_test.go"], guardBase["tests/test_cases.py"]
	for name, c := range map[string]guardCase{
		"go test renamed":       {map[string]string{"pkg/add_test.go": goT}, []string{"pkg/calc_test.go"}, ""},
		"go test moved":         {map[string]string{"pkg/calc_test.go": "package pkg\n", "pkg/more_test.go": goT}, nil, ""},
		"go:build linux":        {map[string]string{"pkg/calc_test.go": "//go:build linux && !windows\n\n" + goT}, nil, ""},
		"go assertion added":    {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "{\n\tif", "{\n\tif Add(0, 0) != 0 {\n\t\tt.Error(\"zero\")\n\t}\n\tif", 1)}, nil, ""},
		"python file renamed":   {map[string]string{"tests/test_money2.py": py}, []string{"tests/test_money.py"}, ""},
		"python test split":     {map[string]string{"tests/test_money.py": strings.SplitAfter(py, "== 3\n")[0], "tests/test_zero.py": "from money import add\n\ndef test_zero():\n    assert add(0, 0) == 0\n"}, nil, ""},
		"class test moved":      {map[string]string{"tests/test_cases.py": strings.SplitAfter(cases, "== want\n")[0], "tests/test_more.py": "class TestAdd:\n    def test_method(self):\n        assert 1 + 1 == 2\n"}, nil, ""},
		"parametrize row added": {map[string]string{"tests/test_cases.py": strings.Replace(cases, "    (2, 2, 4),\n", "    (2, 2, 4),\n    (0, 0, 0),\n", 1)}, nil, ""},
		"comments and strings":  {map[string]string{"tests/test_money.py": py + "\n# def test_x(): pytest.skip()\nDOC = \"\"\"def test_y(): pytest.skip()\"\"\"\n"}, nil, ""},
		"a helper named skip":   {map[string]string{"money.py": baseFiles["money.py"] + "\ndef skip(n):\n    return n\n"}, nil, ""},
		"a conftest fixture":    {map[string]string{"tests/conftest.py": "import pytest\n\n@pytest.fixture\ndef money():\n    return 1\n"}, nil, ""},
		"other setup.cfg":       {map[string]string{"setup.cfg": "[metadata]\nname = m\n"}, nil, ""},
		// Warnings (Astra M7): the candidate is accepted, the reviewer told.
		"TestMain changed":   {map[string]string{"pkg/main_test.go": strings.Replace(guardBase["pkg/main_test.go"], "m.Run()", "0", 1)}, nil, "TestMain changed"},
		"init exits":         {map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\nfunc init() { os.Exit(0) }\n"}, nil, "an init calling os.Exit changed"},
		"assert x or True":   {map[string]string{"tests/test_money.py": strings.Replace(py, "== 3", "== 3 or True", 1)}, nil, "cannot fail"},
		"a pytest hook":      {map[string]string{"tests/plugin.py": "def pytest_runtest_setup(item):\n    pass\n"}, nil, "the pytest hook pytest_runtest_setup changed"},
		"sys.exit added":     {map[string]string{"tests/conftest.py": "import sys\nsys.exit(0)\n"}, nil, "sys.exit"},
		"a large helper":     {map[string]string{"money.py": baseFiles["money.py"] + "#" + strings.Repeat("x", 5<<20) + "\n"}, nil, "not read"},
		"assert True placed": {map[string]string{"tests/test_x.py": "def test_x():\n    assert True\n"}, nil, "cannot fail"},
	} {
		v := f.guarded(t, c.files, c.gone...)
		warned := c.why == "" || slices.ContainsFunc(v.Warnings, func(b Block) bool { return strings.Contains(b.What, c.why) })
		if !v.Accepted() || !warned {
			t.Errorf("%s: blocks %q, warnings %q; want accepted (warning %q)", name, whats(v.Blocks), whats(v.Warnings), c.why)
		}
	}
}

// The tokenizer finds tests and assertions in code only, whatever the
// strings and comments around them hold.
func TestPyFacts(t *testing.T) {
	for src, want := range map[string]map[string]int{
		"x = f\"{'\"'}\"\ndef test_a():\n    assert 1\n":                                                             {"test_a": 1}, // PEP 701 nesting
		"x = f'{{'\ndef test_a():\n    assert x\n":                                                                   {"test_a": 1},
		"x = f\"\\N{BULLET} {y:{w}.{p}}\"\ndef test_a():\n    assert x\n":                                            {"test_a": 1},
		"x = rf'\\{y}'\ndef test_a():\n    assert x\n":                                                               {"test_a": 1},
		"# \"\"\"\ndef test_a():\n    assert \\\n        x\n":                                                        {"test_a": 1},
		"x = '# not a comment'\ndef test_a():\n    assert x  # assert y\n":                                           {"test_a": 1},
		"x = bR\"\"\"\ndef test_a():\n    assert x\n\"\"\"\n":                                                        {},
		"x = f'''{y}\ndef test_a(): assert x'''\n":                                                                   {},
		"def test_a():\n    def test_inner():\n        assert 1\n    assert 2\n":                                     {"test_a": 2},
		"class Helper:\n    def test_a(self):\n        assert 1\n":                                                   {},
		"class TestA:\n    class TestB:\n        def test_c(self):\n            self.assertEqual(1, 1)\n":            {"TestA::TestB::test_c": 1},
		"class Case(unittest.TestCase):\n    def test_a(self):\n        self.assertTrue(1)\n":                        {"Case::test_a": 1},
		"class TestA:\n    __test__ = False\n    def test_a(self):\n        assert 1\ndef test_b():\n    assert 1\n": {"test_b": 1},
		"\ufeffdef test_a():\n    assert 1\n":                                                                        {"test_a": 1},
		"def test_a():\n\tassert 1\n\x0cdef test_b(): assert 2\n":                                                    {"test_a": 1, "test_b": 1},
	} {
		fa := facts{tests: map[string]*stat{}, warn: map[string]string{}}
		if err := pyFacts("t.py", []byte(src), &fa); err != nil {
			t.Errorf("%q: %v", src, err)
			continue
		}
		got := map[string]int{}
		for k, s := range fa.tests {
			got[strings.TrimPrefix(k, "t.py::")] = s.asserts
		}
		if !maps.Equal(got, want) {
			t.Errorf("%q: tests %v, want %v", src, got, want)
		}
	}
	for _, src := range []string{"x = 'a\n", "x = \"\"\"a\n", "x = f'{a'\n", "x = (1\n", "x = 1)\n", "x = 1 \\ y\n",
		"x = " + strings.Repeat("f'{", maxNest+2) + "1" + strings.Repeat("}'", maxNest+2) + "\n"} {
		if _, err := pyLex([]byte(src)); err == nil {
			t.Errorf("%q tokenized", src)
		}
	}
}

// Go's own collection rules, per gate platform.
func TestGoOn(t *testing.T) {
	src := []byte("package p\n")
	for p, want := range map[string]int{
		"a_test.go": 2, "p/a_test.go": 2, "p/a_linux_test.go": 2, "p/a_arm64_test.go": 1, "p/a_darwin_test.go": 0, "p/_a_test.go": 0,
		"p/.a_test.go": 0, "p/testdata/a_test.go": 0, "vendor/x/a_test.go": 0, "_x/a_test.go": 0, "p/a.go": 0,
	} {
		if got := len(goOn(p, src)); got != want {
			t.Errorf("%s: collected on %d platforms, want %d", p, got, want)
		}
	}
	for c, want := range map[string]int{"//go:build linux": 2, "//go:build !linux": 0, "//go:build go1.1": 2, "//go:build cgo": 0,
		"//go:build amd64 || ignore": 1, "// +build linux,arm64": 1, "/*\n//go:build ignore\n*/": 2} {
		if got := len(goOn("p/a_test.go", []byte(c+"\n\npackage p\n"))); got != want {
			t.Errorf("%q: collected on %d platforms, want %d", c, got, want)
		}
	}
}
