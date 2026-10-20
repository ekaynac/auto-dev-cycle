// SPDX-License-Identifier: Apache-2.0

package gate

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/results"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// junit is an xunit1 pytest --junitxml document of tests in
// tests/test_money.py, each "name=pass|fail|error".
func junit(tests ...string) string {
	var b strings.Builder
	failures, errs := 0, 0
	for _, x := range tests {
		name, o, _ := strings.Cut(x, "=")
		b.WriteString(`<testcase classname="tests.test_money" name="` + name + `" file="tests/test_money.py" line="1" time="0.001">`)
		switch o {
		case "fail":
			failures++
			b.WriteString(`<failure message="assert 1 == 2">CANARY-FAILURE-TEXT</failure>`)
		case "error":
			errs++
			b.WriteString(`<error message="failed on setup">RuntimeError</error>`)
		}
		b.WriteString(`</testcase>`)
	}
	return doc(errs, failures, len(tests), b.String())
}

func doc(errs, failures, tests int, body string) string {
	return `<?xml version="1.0" encoding="utf-8"?><testsuites name="pytest tests"><testsuite name="pytest" errors="` + strconv.Itoa(errs) +
		`" failures="` + strconv.Itoa(failures) + `" skipped="0" tests="` + strconv.Itoa(tests) + `">` + body + `</testsuite></testsuites>`
}

// pytestStub stands in for pytest (python3 -m pytest finds it at the
// checkout root): it refuses to run without the controller's xunit1, and
// with --junitxml=PATH copies report.xml there, if the checkout has one;
// it prints "5 passed" and exits 0 whatever the report holds.
const pytestStub = `import os, shutil, sys
if "junit_family=xunit1" not in sys.argv:
    sys.exit(3)
for a in sys.argv[1:]:
    if a.startswith("--junitxml=") and os.path.exists("report.xml"):
        shutil.copy("report.xml", a[len("--junitxml="):])
print("5 passed")
`

// pyFixture is newFixture for a pytest gate (junit_xml), its baseline
// test_add and test_sub passing, its pytest the stub unless real.
func pyFixture(t *testing.T, files map[string]string, real bool, command ...string) fixture {
	t.Helper()
	if !real {
		files["pytest.py"] = pytestStub
	}
	f := newFixture(t, files, command...)
	base, err := results.ParseJUnitXML(strings.NewReader(junit("test_add=pass", "test_sub=pass")), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.in.Gate.ResultAdapter, f.in.Baseline.Report = results.JUnitXML, base
	return f
}

var pytest = []string{"python3", "-m", "pytest", "-q"}

// TestGateFixtures (slice plan §2.6, through the gate): fault-lying-runner,
// a pytest that prints "5 passed" and exits 0 while its results show a
// failure, is found/gate_test_failed; fault-unreadable-results, a pytest
// that writes no results, is could_not_run/runner_fault, never empty.
// Ownership: any failure or error in the report (a collection error of a
// module or a class too: Astra #290 M5) makes a missing test the
// candidate's; else its file, or a conftest.py or __init__.py above it,
// changed.
func TestGateFixtures(t *testing.T) {
	classErr := doc(1, 0, 2, `<testcase classname="tests.test_money" name="test_add" file="tests/test_money.py" line="1" time="0.001"/>`+
		`<testcase classname="tests.test_money" name="TestRates" file="tests/test_money.py" time="0.000"><error message="collection failure">TypeError</error></testcase>`)
	modErr := doc(1, 0, 1, `<testcase classname="" name="tests.test_money" file="tests/test_money.py" time="0.000"><error message="collection failure">ImportError</error></testcase>`)
	whole := junit("test_add=pass", "test_sub=pass")
	failing := doc(0, 1, 1, `<testcase classname="tests.test_money" name="test_add" file="tests/test_money.py" line="1" time="0.001"><failure message="x"/></testcase>`)
	for name, c := range map[string]struct {
		report  string // "": none written
		changed string
		want    string
	}{
		"fault-lying-runner":                     {junit("test_add=pass", "test_sub=fail"), "", "found/gate_test_failed"},
		"fault-lying-runner, setup errors":       {junit("test_add=pass", "test_sub=error"), "", "found/gate_test_failed"},
		"fault-unreadable-results":               {"", "", "runner_fault: the run never wrote its results"},
		"a clean pytest run":                     {whole, "", "empty/completed_clean"},
		"a half-written document":                {whole[:100], "", "runner_fault: the run's results are not a JUnit XML document"},
		"a test defined in a shared helper":      {strings.Replace(whole, `name="test_sub" file="tests/test_money.py"`, `name="test_sub" file="helpers.py"`, 1), "", "runner_fault: the run's results are not a JUnit XML document"}, // Astra #290 r2 H1
		"test_sub missing, its file changed":     {junit("test_add=pass"), "tests/test_money.py", "found/gate_test_failed"},
		"test_sub missing, money.py changed":     {junit("test_add=pass"), "money.py", "runner_fault: 1 results missing"},
		"test_sub missing, tests/conftest.py":    {junit("test_add=pass"), "tests/conftest.py", "found/gate_test_failed"},
		"test_sub missing, tests/__init__.py":    {junit("test_add=pass"), "tests/__init__.py", "found/gate_test_failed"},
		"test_sub missing, the root conftest.py": {junit("test_add=pass"), "conftest.py", "found/gate_test_failed"},
		"test_sub missing, lib/conftest.py":      {junit("test_add=pass"), "lib/conftest.py", "runner_fault: 1 results missing"},
		"a module's collection error, money.py":  {modErr, "money.py", "found/gate_test_failed"},
		"a class's collection error, money.py":   {classErr, "money.py", "found/gate_test_failed"},
		"another test failing, test_sub missing": {failing, "", "found/gate_test_failed"},
	} {
		files := map[string]string{"money.py": "CASES = [1]\n"}
		if c.report != "" {
			files["report.xml"] = c.report
		}
		f := pyFixture(t, files, false, slices.Concat(pytest, []string{"--junitxml={results_path}"})...)
		if c.changed != "" {
			f.in.Changed = []string{c.changed}
		}
		if v := gated(t, f.in); !strings.Contains(v.String(), c.want) {
			t.Errorf("%s: %v, want %s", name, v, c.want)
		}
	}
	// Astra #290 H4: the base's tests/test_a.py::TestB_test::test_x,
	// replaced by tests/test_a/TestB_test.py::test_x (one classname and
	// name): missing, never present.
	at := func(file string) string {
		return doc(0, 0, 1, `<testcase classname="tests.test_a.TestB_test" name="test_x" file="`+file+`" line="1" time="0.001"/>`)
	}
	f := pyFixture(t, map[string]string{"report.xml": at("tests/test_a/TestB_test.py")}, false, pytest...)
	base, err := results.ParseJUnitXML(strings.NewReader(at("tests/test_a.py")), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.in.Baseline.Report = base
	if v := gated(t, f.in); is(v, vocab.Empty, vocab.ReasonCompletedClean) {
		t.Errorf("a test replaced by one of its classname and name: %v", v)
	}
	// Astra #290 r3 H1: the same identity, file="tests/test_a.py", with
	// tests/test_a/TestB_test.py beside the module (an ancestor's helper
	// imported as the test): ambiguous, never present; measured too. With
	// one module file (the control), it reads.
	for name, c := range map[string]struct {
		files map[string]string
		want  string
	}{
		"an ancestor's helper": {map[string]string{"tests/test_a.py": "def check():\n    pass\n", "tests/test_a/TestB_test.py": "from tests.test_a import check as test_x\n"}, "runner_fault: the run's results name a test two module files could have reported"},
		"one module file":      {map[string]string{"tests/test_a.py": "def check():\n    pass\n"}, "empty/completed_clean"},
		// Astra #290 r4 H1: tests/test_a -> ../.payload holds the module the
		// walk does not reach; any directory link is refused. A leaf .py
		// link is walked (the control).
		"a directory link": {map[string]string{"tests/test_a.py": "def check():\n    pass\n", "tests/test_a": "symlink:../.payload", ".payload/TestB_test.py": "from tests.test_a import check as test_x\n"}, "runner_fault: the tree holds a directory link"},
		"a leaf .py link":  {map[string]string{"tests/test_a.py": "def check():\n    pass\n", "tests/helper.py": "symlink:test_a.py"}, "empty/completed_clean"},
	} {
		c.files["report.xml"] = at("tests/test_a.py")
		f := pyFixture(t, c.files, false, pytest...)
		f.in.Baseline.Report = base
		if v := gated(t, f.in); !strings.Contains(v.String(), c.want) {
			t.Errorf("%s: %v, want %s", name, v, c.want)
		}
		f.in.Baseline = nil
		if b, v, err := Measure(context.Background(), f.in); err != nil || strings.Contains(c.want, "runner_fault") && (b.Results != nil || !strings.Contains(v.String(), c.want)) {
			t.Errorf("%s, measured: %v, %v", name, v, err)
		}
	}
}

// TestGateCommand (#275, Astra #290): a junit_xml gate runs pytest
// directly, with allowlisted arguments, in the checkout root; any other
// command is refused before it runs. The controller appends
// --junitxml={results_path} (unless named) and -o junit_family=xunit1.
func TestGateCommand(t *testing.T) {
	report := map[string]string{"report.xml": junit("test_add=pass", "test_sub=pass")}
	for name, command := range map[string][]string{
		"sh -c":                       {"sh", "-c", "python3 -m pytest -q"},
		"a wrapper script":            {"python3", "run_gate.py"},
		"cd runner &&":                {"cd", "runner", "&&", "python3", "-m", "pytest"},
		"env":                         {"env", "PYTHONPATH=src", "python3", "-m", "pytest"},
		"--rootdir":                   {"python3", "-m", "pytest", "--rootdir=.."},
		"-c":                          {"pytest", "-c", "other.ini"},
		"-p a plugin":                 {"pytest", "-p", "myplugin"},
		"-o pythonpath":               {"pytest", "-o", "pythonpath=src"},
		"--confcutdir":                {"pytest", "--confcutdir=tests"},
		"--pyargs":                    {"pytest", "--pyargs", "money"},
		"an absolute path":            {"pytest", "/workspace/tests"},
		"..":                          {"pytest", "../tests"},
		"an argument file":            {"python3", "-m", "pytest", "@args.txt"}, // Astra #290 r2 H2
		"an argument file in a value": {"pytest", "-k", "@args.txt"},
		"another --junitxml":          {"pytest", "--junitxml=out.xml"},
		"-n with no count":            {"pytest", "-n", "auto"},
		"python, not -m":              {"python3", "pytest.py"},
		"python -m another":           {"python3", "-m", "unittest"},
		"no command":                  {},
	} {
		f := pyFixture(t, maps.Clone(report), false, command...)
		if _, err := Run(context.Background(), f.in); !errors.Is(err, ErrSpec) {
			t.Errorf("%s: %v, want ErrSpec", name, err)
		}
	}
	valid := []string{"python3", "-m", "pytest", "-q", "-x", "-p", "no:cacheprovider", "-k", "add or sub", "-m", "not slow", "--maxfail=3",
		"-o", "xfail_strict=true", "tests/test_money.py::test_add", "tests", "--junitxml={results_path}"}
	f := pyFixture(t, maps.Clone(report), false, valid...)
	if v := gated(t, f.in); !strings.Contains(v.String(), "empty/completed_clean") {
		t.Errorf("a valid command: %v", v)
	}
	for _, c := range [][]string{pytest, {"pytest", "-q"}} {
		a := argv(c, adapters[results.JUnitXML])
		if tail := a[len(a)-3:]; !slices.Equal(tail, []string{"--junitxml=" + "/results/junit.xml", "-o", "junit_family=xunit1"}) {
			t.Errorf("%q: argv %q", c, a)
		}
	}
	if a := argv(valid, adapters[results.JUnitXML]); strings.Count(strings.Join(a, " "), "--junitxml=") != 1 || a[len(a)-1] != "junit_family=xunit1" {
		t.Errorf("a command naming its results: argv %q", a)
	}
}

// TestGatePytest: real pytest through the gate, when the host has it
// (ADC_GATE_PYTEST: a python with pytest >= 9.1, its directory first on
// PATH; skipped otherwise): a clean run reads its xunit1 identities; a
// class's collection error with money.py changed is the candidate's; the
// H4 replacement is missing.
func TestGatePytest(t *testing.T) {
	py := os.Getenv("ADC_GATE_PYTEST")
	if py == "" {
		t.Skip("set ADC_GATE_PYTEST to a python with pytest to run the gate under real pytest")
	}
	money := "import os\n\n\ndef test_add(monkeypatch, tmp_path):\n    monkeypatch.chdir(tmp_path)\n    assert 1 + 1 == 2\n\n\ndef test_sub():\n    assert 1 - 1 == 0\n"
	rates := "import pytest\nimport money\n\n\nclass TestRates:\n    @pytest.mark.parametrize(\"r\", money.CASES)\n    def test_rate(self, r):\n        pass\n"
	for name, c := range map[string]struct {
		files   map[string]string
		changed string
		want    string
	}{
		"a clean run":                            {map[string]string{"tests/test_money.py": money, "money.py": "CASES = [1]\n"}, "", "empty/completed_clean"},
		"a class's collection error":             {map[string]string{"tests/test_money.py": money, "tests/test_rates.py": rates, "money.py": "CASES = None\n"}, "money.py", "found/gate_test_failed"},
		"a test defined in a shared helper":      {map[string]string{"tests/test_money.py": money, "helpers.py": "def check():\n    pass\n", "tests/test_a.py": "from helpers import check\n\n\nclass TestB_test:\n    test_x = staticmethod(check)\n", "money.py": "CASES = [1]\n"}, "", "runner_fault: the run's results are not a JUnit XML document"},
		"an ancestor's helper beside the module": {map[string]string{"tests/test_money.py": money, "tests/__init__.py": "", "tests/test_a.py": "def check():\n    pass\n", "tests/test_a/TestB_test.py": "from tests.test_a import check as test_x\n", "money.py": "CASES = [1]\n"}, "", "runner_fault: the run's results name a test two module files could have reported"},
		"test_sub moved to tests/test_money/":    {map[string]string{"tests/test_money.py": money[:strings.Index(money, "\n\n\ndef test_sub")] + "\n", "tests/test_money/sub_test.py": "def test_sub():\n    pass\n", "money.py": "CASES = [1]\n"}, "", "runner_fault: the run's results name a test two module files could have reported"}, // tests.test_money.sub_test: tests/test_money.py's class, or this module
	} {
		f := pyFixture(t, c.files, true, "python3", "-m", "pytest", "-q", "-p", "no:cacheprovider")
		f.in.Host.Runner = &local{path: filepath.Dir(py)}
		if c.changed != "" {
			f.in.Changed = []string{c.changed}
		}
		if v := gated(t, f.in); !strings.Contains(v.String(), c.want) {
			t.Errorf("%s: %v, want %s", name, v, c.want)
		}
	}
}

// TestGateArchAny: runtime.arch "any" (py-mini) relaxes only the
// environment's check; an image of another architecture than the host's
// is still refused.
func TestGateArchAny(t *testing.T) {
	f := pyFixture(t, map[string]string{"report.xml": junit("test_add=pass", "test_sub=pass")}, false, pytest...)
	f.in.Gate.Environments[0].Facts.Runtime.Arch = "any"
	if v := gated(t, f.in); !is(v, vocab.Empty, vocab.ReasonCompletedClean) {
		t.Errorf("arch any, the host's image: %v", v)
	}
	other := map[string]string{"arm64": "amd64"}[runtime.GOARCH]
	if other == "" {
		other = "arm64"
	}
	f.in.Image.Arch, f.in.Baseline.Arch = other, other
	if _, err := Run(context.Background(), f.in); !errors.Is(err, ErrSpec) {
		t.Errorf("arch any, an image of another architecture than the host's: %v", err)
	}
}
