// SPDX-License-Identifier: Apache-2.0

package gate

import (
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/results"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// junit is a pytest --junitxml document of tests in tests/test_money.py,
// each "name=pass|fail|error".
func junit(tests ...string) string {
	var b strings.Builder
	failures, errs := 0, 0
	for _, x := range tests {
		name, o, _ := strings.Cut(x, "=")
		b.WriteString(`<testcase classname="tests.test_money" name="` + name + `" time="0.001">`)
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
	return `<?xml version="1.0" encoding="utf-8"?><testsuites name="pytest tests"><testsuite name="pytest" errors="` + strconv.Itoa(errs) +
		`" failures="` + strconv.Itoa(failures) + `" skipped="0" tests="` + strconv.Itoa(len(tests)) + `">` + b.String() + `</testsuite></testsuites>`
}

// pytestStub stands in for pytest: it loads PYTEST_PLUGINS (exit 4 if it
// cannot, as pytest does) and calls its configure hook, runs COLLECT,
// calls the collection hook, runs RUN (a test's code), then with
// --junitxml=PATH writes the document named by REPORT (report.xml) there.
const pytestStub = `import importlib, os, shutil, sys
report, plugin = os.path.abspath(os.environ.get("REPORT", "report.xml")), None
if os.environ.get("PYTEST_PLUGINS"):
    try:
        plugin = importlib.import_module(os.environ["PYTEST_PLUGINS"])
    except ImportError:
        sys.exit(4)
    plugin.pytest_configure(None)
exec(os.environ.get("COLLECT", ""))
if plugin:
    plugin.pytest_collection_finish(None)
exec(os.environ.get("RUN", ""))
for a in sys.argv[1:]:
    if a.startswith("--junitxml="):
        shutil.copy(report, a[len("--junitxml="):])
`

// pyFixture is newFixture for a pytest gate (junit_xml), its baseline
// test_add and test_sub passing.
func pyFixture(t *testing.T, files map[string]string, command ...string) fixture {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("the pytest gate's rows need python3")
	}
	files["pytest_stub.py"] = pytestStub
	f := newFixture(t, files, command...)
	base, err := results.ParseJUnitXML(strings.NewReader(junit("test_add=pass", "test_sub=pass")), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.in.Gate.ResultAdapter, f.in.Baseline.Report = results.JUnitXML, base
	return f
}

// TestGateFixtures (slice plan §2.6, through the gate): fault-lying-runner,
// a pytest that prints "5 passed" and exits 0 while its results show a
// failure, is found/gate_test_failed; fault-unreadable-results, a pytest
// run without --junitxml, is could_not_run/runner_fault, never empty.
func TestGateFixtures(t *testing.T) {
	files := map[string]string{"failing.xml": junit("test_add=pass", "test_sub=fail"), "errored.xml": junit("test_add=pass", "test_sub=error"),
		"report.xml": junit("test_add=pass", "test_sub=pass"), "money.py": "def add(a, b):\n    return a + b\n"}
	for name, c := range map[string]struct {
		script, report, want string
	}{
		"fault-lying-runner":               {`python3 pytest_stub.py -q --junitxml="$0"; echo "5 passed"; exit 0`, "failing.xml", "found/gate_test_failed"},
		"fault-lying-runner, setup errors": {`python3 pytest_stub.py -q --junitxml="$0"; echo "5 passed"; exit 0`, "errored.xml", "found/gate_test_failed"},
		"fault-unreadable-results":         {`python3 pytest_stub.py -q; echo "5 passed"; exit 0`, "report.xml", "runner_fault: the run never wrote its results"},
		"a clean pytest run":               {`python3 pytest_stub.py -q --junitxml="$0"`, "report.xml", "empty/completed_clean"},
		"a half-written document":          {`python3 pytest_stub.py --junitxml="$0" && head -c 100 "$0" > half && cp half "$0"`, "report.xml", "runner_fault: the run's results are not a JUnit XML document"},
	} {
		f := pyFixture(t, files, "sh", "-c", "REPORT="+c.report+" "+c.script, "{results_path}")
		if v := gated(t, f.in); !strings.Contains(v.String(), c.want) {
			t.Errorf("%s: %v, want %s", name, v, c.want)
		}
	}
	// A test of a changed module missing from the results is the candidate's;
	// of an untouched one, the environment's. Opus #290 M3: a changed
	// conftest.py or __init__.py names its directory's modules, and a
	// collection error (pytest's testcase with no classname) names the
	// module the change broke, whatever was changed.
	collect := `<?xml version="1.0" encoding="utf-8"?><testsuites name="pytest tests"><testsuite name="pytest" errors="1" failures="0" skipped="0" tests="1">` +
		`<testcase classname="" name="tests.test_money" time="0.000"><error message="collection failure">ImportError</error></testcase></testsuite></testsuites>`
	files["only.xml"], files["collect.xml"] = junit("test_add=pass"), collect
	for name, c := range map[string]struct{ report, changed, want string }{
		"test_sub missing, its module changed":        {"only.xml", "tests/test_money.py", "found/gate_test_failed"},
		"test_sub missing, money.py changed":          {"only.xml", "money.py", "runner_fault: 1 results missing"},
		"test_sub missing, tests/conftest.py changed": {"only.xml", "tests/conftest.py", "found/gate_test_failed"},
		"test_sub missing, tests/__init__.py changed": {"only.xml", "tests/__init__.py", "found/gate_test_failed"},
		"test_sub missing, lib/conftest.py changed":   {"only.xml", "lib/conftest.py", "runner_fault: 1 results missing"},
		"a collection error, money.py changed":        {"collect.xml", "money.py", "found/gate_test_failed"},
	} {
		f := pyFixture(t, files, "sh", "-c", `REPORT=`+c.report+` python3 pytest_stub.py --junitxml="$0"`, "{results_path}")
		f.in.Changed = []string{c.changed}
		if v := gated(t, f.in); !strings.Contains(v.String(), c.want) {
			t.Errorf("%s: %v, want %s", name, v, c.want)
		}
	}
}

// TestGateCwd (#275): under junit_xml every pytest process (the runner, a
// worker, a wrapper that execs pytest) starts in the checkout root and none
// changes directory before pytest has collected. The runner/
// counterexample, a worker started elsewhere, a wrapper started elsewhere,
// an early chdir, a log over its bound, and no record at all (no pytest
// loaded the probe, or the log deleted) invalidate the run; a test's own
// subprocess in another directory, and a chdir after collection, do not
// (Opus #290 M4). A Go gate is never probed (Opus #286 M3).
func TestGateCwd(t *testing.T) {
	files := map[string]string{"report.xml": junit("test_add=pass", "test_sub=pass"), "conftest.py": "import sys\nsys.path.insert(0, 'lib')\n",
		"lib/feature.py": "FLAG = False\n", "runner/lib/feature.py": "FLAG = True\n", "runner/README": "runner\n"}
	pytest := `python3 pytest_stub.py --junitxml="$0"`
	wrapper := `cd runner && python3 -c 'import os, sys; os.chdir(".."); os.execv(sys.executable, [sys.executable, "pytest_stub.py", "--junitxml=" + sys.argv[1]])' "$0"`
	for name, c := range map[string]struct {
		script string
		want   string
	}{
		"pytest at the root":                {pytest, "empty/completed_clean"},
		"cd runner && pytest .. (#275)":     {`export REPORT="$PWD/report.xml"; cd runner && python3 ../pytest_stub.py --junitxml="$0"`, "the cwd contract"},
		"a pytest worker started in runner": {`python3 -c 'import subprocess; subprocess.run(["python3", "../pytest_stub.py"], cwd="runner", check=True)' && ` + pytest, "the cwd contract"},
		"a wrapper in runner execs pytest":  {wrapper, "the cwd contract"},
		"a chdir before collection":         {`COLLECT='import os; os.chdir("runner")' ` + pytest, "the cwd contract"},
		"a chdir after collection":          {`RUN='import os; os.chdir("runner")' ` + pytest, "empty/completed_clean"},
		"a test's subprocess in tmp_path":   {`RUN='import subprocess, sys, tempfile; subprocess.run([sys.executable, "-c", "import os; os.chdir(\"/\")"], cwd=tempfile.mkdtemp(), check=True)' ` + pytest, "empty/completed_clean"},
		"a test's python3 in runner":        {`RUN='import subprocess; subprocess.run(["python3", "-c", "pass"], cwd="runner", check=True)' ` + pytest, "empty/completed_clean"},
		"the probe's log over its bound":    {pytest + ` && head -c 1100000 /dev/zero >> "$ADC_GATE_LOG"`, "the cwd probe's log is over its bound"},
		"python3 -S: the plugin loads it":   {`python3 -S pytest_stub.py --junitxml="$0"`, "empty/completed_clean"},
		"python3 -I: no plugin, no pytest":  {`python3 -I pytest_stub.py --junitxml="$0"`, "of 0 recorded (the cwd contract)"},
		"no record: no pytest":              {`python3 -c pass && cp report.xml "$0"`, "of 0 recorded (the cwd contract)"},
		"no record: the log deleted":        {pytest + ` && rm "$ADC_GATE_LOG"`, "of 0 recorded (the cwd contract)"},
	} {
		f := pyFixture(t, files, "sh", "-c", c.script, "{results_path}")
		if v := gated(t, f.in); !strings.Contains(v.String(), c.want) {
			t.Errorf("%s: %v, want %s", name, v, c.want)
		}
	}
	// L4 (Opus #290): the probe goes first on the image's PYTHONPATH, never
	// in place of it.
	res := t.TempDir()
	cmd := argv([]string{"sh", "-c", `printf %s "$PYTHONPATH" >"$0"`, "{results_path}"}, adapters[results.JUnitXML])
	for i := range cmd {
		cmd[i] = strings.ReplaceAll(cmd[i], sandbox.ResultsDir, res)
	}
	x := exec.Command(cmd[0], cmd[1:]...)
	x.Env = []string{"PATH=/usr/bin:/bin", "PYTHONPATH=/image"}
	if err := x.Run(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(res, "junit.xml")); string(b) != res+"/"+probeDir+":/image" {
		t.Errorf("the image's PYTHONPATH: %q", b)
	}
	f := newFixture(t, map[string]string{"clean.json": stream["clean.json"], "runner/README": "runner\n"}, "sh", "-c",
		`python3 -c 'import subprocess; subprocess.run(["python3", "-c", "pass"], cwd="runner", check=True)' && cat clean.json`)
	if v := gated(t, f.in); !is(v, vocab.Empty, vocab.ReasonCompletedClean) {
		t.Errorf("a Go gate whose test runs Python in its package directory: %v", v)
	}
}

// TestGateCwdPytest: the cwd contract under real pytest, when the host
// has it (ADC_GATE_PYTEST: a python with pytest >= 9.1; skipped
// otherwise): a test's monkeypatch.chdir and its subprocess in tmp_path
// are clean; a conftest's chdir at import, and pytest started in tests/,
// are invalidated.
func TestGateCwdPytest(t *testing.T) {
	py := os.Getenv("ADC_GATE_PYTEST")
	if py == "" {
		t.Skip("set ADC_GATE_PYTEST to a python with pytest to run the cwd contract under real pytest")
	}
	test := "import os, subprocess, sys\n\ndef test_add(monkeypatch, tmp_path):\n    monkeypatch.chdir(tmp_path)\n" +
		"    subprocess.run([sys.executable, '-c', 'pass'], cwd=tmp_path, check=True)\n\ndef test_sub():\n    assert 1 - 1 == 0\n"
	run := py + ` -m pytest -q -p no:cacheprovider --junitxml="$0"`
	for name, c := range map[string]struct {
		files  map[string]string
		script string
		want   string
	}{
		"chdir and a subprocess in a test": {nil, run, "empty/completed_clean"},
		"a conftest's chdir at import":     {map[string]string{"conftest.py": "import os\nos.chdir('tests')\n"}, run, "the cwd contract"},
		"pytest started in tests/ (#275)":  {nil, `cd tests && ` + py + ` -m pytest -q -p no:cacheprovider --rootdir=.. --junitxml="$0" ..`, "the cwd contract"},
	} {
		files := map[string]string{"tests/test_money.py": test}
		maps.Copy(files, c.files)
		f := pyFixture(t, files, "sh", "-c", c.script, "{results_path}")
		if v := gated(t, f.in); !strings.Contains(v.String(), c.want) {
			t.Errorf("%s: %v, want %s", name, v, c.want)
		}
	}
}

// TestGateArchAny: runtime.arch "any" (py-mini) relaxes only the
// environment's check; an image of another architecture than the host's
// is still refused.
func TestGateArchAny(t *testing.T) {
	f := pyFixture(t, map[string]string{"report.xml": junit("test_add=pass", "test_sub=pass")}, "sh", "-c", `python3 pytest_stub.py --junitxml="$0"`, "{results_path}")
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
