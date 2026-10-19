// SPDX-License-Identifier: Apache-2.0

package gate

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/results"
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

// pytestStub stands in for pytest: with --junitxml=PATH it writes the
// document named by REPORT (report.xml) there, and prints nothing useful.
const pytestStub = "import os, shutil, sys\nfor a in sys.argv[1:]:\n    if a.startswith('--junitxml='):\n        shutil.copy(os.environ.get('REPORT', 'report.xml'), a[len('--junitxml='):])\n"

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
	// of an untouched one, the environment's.
	for changed, want := range map[string]string{"tests/test_money.py": "found/gate_test_failed", "money.py": "runner_fault: 1 results missing"} {
		files["only.xml"] = junit("test_add=pass")
		f := pyFixture(t, files, "sh", "-c", `REPORT=only.xml python3 pytest_stub.py --junitxml="$0"`, "{results_path}")
		f.in.Changed = []string{changed}
		if v := gated(t, f.in); !strings.Contains(v.String(), want) {
			t.Errorf("test_sub missing, %s changed: %v, want %s", changed, v, want)
		}
	}
}

// TestGateCwd (#275): under junit_xml every Python process starts in the
// checkout root and none changes directory before pytest has collected;
// the runner/ counterexample, a worker started elsewhere, an early chdir, a
// log over its bound, and no record at all (Python that never loaded the
// probe: -S, -I, or none) invalidate the run. A Go gate is never probed
// (Opus #286 M3).
func TestGateCwd(t *testing.T) {
	files := map[string]string{"report.xml": junit("test_add=pass", "test_sub=pass"), "conftest.py": "import sys\nsys.path.insert(0, 'lib')\n",
		"lib/feature.py": "FLAG = False\n", "runner/lib/feature.py": "FLAG = True\n", "runner/README": "runner\n"}
	py, pytest := `python3 -c 'import sys; sys.path.insert(0, "lib"); import feature'`, ` && python3 pytest_stub.py --junitxml="$0"`
	worker := `python3 -c 'import subprocess; subprocess.run(["python3", "-c", "pass"], cwd="runner", check=True)'`
	for name, c := range map[string]struct {
		script string
		want   string
	}{
		"python at the root":             {py + pytest, "empty/completed_clean"},
		"cd runner && pytest .. (#275)":  {`cd runner && ` + py + ` && cd ..` + pytest, "the cwd contract"},
		"a worker started in runner":     {worker + pytest, "the cwd contract"},
		"a chdir before collection":      {`python3 -c 'import os; os.chdir("runner")'` + pytest, "the cwd contract"},
		"the probe's log over its bound": {`python3 -c pass && head -c 1100000 /dev/zero >> "$ADC_GATE_LOG"` + pytest, "the cwd probe's log is over its bound"},
		"no record: python3 -S":          {`python3 -S pytest_stub.py --junitxml="$0"`, "of 0 recorded (the cwd contract)"},
		"no record: python3 -I":          {`python3 -I pytest_stub.py --junitxml="$0"`, "of 0 recorded (the cwd contract)"},
		"no record: no Python":           {`cp report.xml "$0"`, "of 0 recorded (the cwd contract)"},
		"the log deleted":                {`python3 -c pass && rm "$ADC_GATE_LOG" && cp report.xml "$0"`, "of 0 recorded (the cwd contract)"},
	} {
		f := pyFixture(t, files, "sh", "-c", c.script, "{results_path}")
		if v := gated(t, f.in); !strings.Contains(v.String(), c.want) {
			t.Errorf("%s: %v, want %s", name, v, c.want)
		}
	}
	f := newFixture(t, map[string]string{"clean.json": stream["clean.json"], "runner/README": "runner\n"}, "sh", "-c", worker+" && cat clean.json")
	if v := gated(t, f.in); !is(v, vocab.Empty, vocab.ReasonCompletedClean) {
		t.Errorf("a Go gate whose test runs Python in its package directory: %v", v)
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
