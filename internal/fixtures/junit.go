// SPDX-License-Identifier: Apache-2.0

package fixtures

// The junit_xml result adapter's fixtures (M1 P15b; slice plan §2.6's
// fault-lying-runner and fault-unreadable-results, on the adapter): each
// step is a pytest --junitxml document, read as the gate reads it. A
// readable report is the gate's verdict on it (clean: empty; anything
// else: found/gate_test_failed); an unreadable one is
// could_not_run/runner_fault, never a pass.

import (
	"context"
	"errors"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/internal/results"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// junitDoc is a pytest-shaped document (xunit1, as the gate runs pytest): one suite holding body, with the
// counts attrs declares.
func junitDoc(attrs, body string) string {
	return `<?xml version="1.0" encoding="utf-8"?><testsuites name="pytest tests"><testsuite name="pytest" ` + attrs + `>` + body + `</testsuite></testsuites>`
}

const (
	junitPass   = `<testcase file="tests/test_money.py" classname="tests.test_money" name="test_add" time="0.001" />`
	junitPass2  = `<testcase file="tests/test_money.py" classname="tests.test_money" name="test_sub" time="0.001" />`
	junitFail   = `<testcase file="tests/test_money.py" classname="tests.test_money" name="test_sub" time="0.001"><failure message="assert 1 == 2">assert 1 == 2</failure></testcase>`
	junitErrors = `<testcase file="tests/test_money.py" classname="tests.test_money" name="test_sub" time="0.000"><error message="failed on setup with &quot;RuntimeError&quot;">RuntimeError</error></testcase>`
)

// junitStep reads doc against expected as the gate does.
func junitStep(doc string, expected []results.Test) (Step, error) {
	r, err := results.ParseJUnitXML(strings.NewReader(doc), expected, nil)
	switch {
	case errors.Is(err, results.ErrUnreadable):
		return Step{vocab.CouldNotRun, vocab.ReasonRunnerFault.String()}, nil
	case err != nil:
		return Step{}, err
	case r.Clean():
		return Step{vocab.Empty, vocab.ReasonCompletedClean.String()}, nil
	}
	return Step{vocab.Found, vocab.ReasonGateTestFailed.String()}, nil
}

func junitSteps(docs []string, expected []results.Test) ([]Step, error) {
	var steps []Step
	for _, d := range docs {
		st, err := junitStep(d, expected)
		if err != nil {
			return nil, err
		}
		steps = append(steps, st)
	}
	return steps, nil
}

// junitLyingRunner: a runner that prints "5 passed" and exits 0 is judged
// by its results alone: a failure, an error in setup (an error is a
// failure) and a missing acceptance test each fail the gate, each step on
// that fault alone; a clean document passes.
func junitLyingRunner(_ context.Context, _ string) ([]Step, []string, error) {
	acceptance := []results.Test{{Package: "tests/test_hidden.py::tests.test_hidden", Name: "test_acceptance"}}
	accepted := `<testcase file="tests/test_hidden.py" classname="tests.test_hidden" name="test_acceptance" time="0.001" />`
	steps, err := junitSteps([]string{
		junitDoc(`errors="0" failures="1" skipped="0" tests="3"`, junitPass+junitFail+accepted),
		junitDoc(`errors="1" failures="0" skipped="0" tests="3"`, junitPass+junitErrors+accepted),
		junitDoc(`errors="0" failures="0" skipped="0" tests="2"`, junitPass+junitPass2),
		junitDoc(`errors="0" failures="0" skipped="0" tests="3"`, junitPass+junitPass2+accepted),
	}, acceptance)
	return steps, nil, err
}

// junitUnreadableResults: results never written, a partial document, counts
// that disagree with the testcases and two testcases of one identity are
// each could_not_run/runner_fault, never a pass; a whole one reads.
func junitUnreadableResults(_ context.Context, _ string) ([]Step, []string, error) {
	whole := junitDoc(`errors="0" failures="0" skipped="0" tests="2"`, junitPass+junitPass2)
	steps, err := junitSteps([]string{
		"",
		whole[:len(whole)/2],
		junitDoc(`errors="0" failures="0" skipped="0" tests="3"`, junitPass+junitPass2),
		junitDoc(`errors="0" failures="0" skipped="0" tests="2"`, junitPass+junitPass),
		whole,
	}, nil)
	return steps, nil, err
}
