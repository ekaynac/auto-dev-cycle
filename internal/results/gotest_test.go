// SPDX-License-Identifier: Apache-2.0

package results

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// Streams below are trimmed from real go1.27.1 `go test -json` output (Time
// and Elapsed dropped; the adapter reads neither).
const (
	okStream = `{"Action":"start","Package":"m/ok"}
{"Action":"run","Package":"m/ok","Test":"TestA"}
{"Action":"output","Package":"m/ok","Test":"TestA","Output":"--- PASS: TestA (0.00s)\n","OutputType":"frame"}
{"Action":"pass","Package":"m/ok","Test":"TestA"}
{"Action":"run","Package":"m/ok","Test":"TestB"}
{"Action":"run","Package":"m/ok","Test":"TestB/sub"}
{"Action":"pass","Package":"m/ok","Test":"TestB/sub"}
{"Action":"pass","Package":"m/ok","Test":"TestB"}
{"Action":"output","Package":"m/ok","Output":"ok  \tm/ok\t0.004s\n"}
{"Action":"pass","Package":"m/ok"}
`
	// A skipped test: the package passes and go test exits 0.
	skipStream = `{"Action":"start","Package":"m/skip"}
{"Action":"run","Package":"m/skip","Test":"TestA"}
{"Action":"pass","Package":"m/skip","Test":"TestA"}
{"Action":"run","Package":"m/skip","Test":"TestS"}
{"Action":"output","Package":"m/skip","Test":"TestS","Output":"--- SKIP: TestS (0.00s)\n","OutputType":"frame"}
{"Action":"skip","Package":"m/skip","Test":"TestS"}
{"Action":"output","Package":"m/skip","Output":"PASS\n","OutputType":"frame"}
{"Action":"pass","Package":"m/skip"}
`
	panicStream = `{"Action":"start","Package":"m/panic"}
{"Action":"run","Package":"m/panic","Test":"TestA"}
{"Action":"pass","Package":"m/panic","Test":"TestA"}
{"Action":"run","Package":"m/panic","Test":"TestP"}
{"Action":"output","Package":"m/panic","Test":"TestP","Output":"panic: boom [recovered, repanicked]\n"}
{"Action":"fail","Package":"m/panic","Test":"TestP"}
{"Action":"fail","Package":"m/panic"}
`
	buildFailStream = `{"ImportPath":"m/broken [m/broken.test]","Action":"build-output","Output":"broken/b_test.go:3:27: undefined: undefined\n"}
{"ImportPath":"m/broken [m/broken.test]","Action":"build-fail"}
{"Action":"start","Package":"m/broken"}
{"Action":"output","Package":"m/broken","Output":"FAIL\tm/broken [build failed]\n","OutputType":"frame"}
{"Action":"fail","Package":"m/broken","FailedBuild":"m/broken [m/broken.test]"}
`
	noTestsStream = `{"Action":"start","Package":"m/notests"}
{"Action":"output","Package":"m/notests","Output":"?   \tm/notests\t[no test files]\n"}
{"Action":"skip","Package":"m/notests"}
`
)

func parse(t *testing.T, stream string, expected ...Test) Report {
	t.Helper()
	return parseOn(t, nil, stream, expected...)
}

func parseOn(t *testing.T, base *Report, stream string, expected ...Test) Report {
	t.Helper()
	r, err := ParseGoTestJSON(strings.NewReader(stream), expected, base)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func outcome(r Report, pkg, name string) Outcome {
	i := slices.IndexFunc(r.Results, func(x Result) bool { return x.Package == pkg && x.Name == name })
	if i < 0 {
		return ""
	}
	return r.Results[i].Outcome
}

// The positive control: an all-green stream passes, with the expected tests
// present, subtests included, and a package without tests beside it.
func TestGreenStreamPasses(t *testing.T) {
	r := parse(t, okStream+noTestsStream, Test{"m/ok", "TestA"}, Test{"m/ok", "TestB/sub"})
	if !r.Clean() || len(r.Results) != 3 || len(r.Blocking()) != 0 {
		t.Fatalf("a green stream: %+v", r)
	}
	if r.Packages["m/ok"] != Pass || r.Packages["m/notests"] != Skip {
		t.Fatalf("packages: %v", r.Packages)
	}
}

// F (P32): a skipped test is never a pass, though the package passed and go
// test exited 0. Without a base any skip fails; an expected test that
// skipped fails against any base.
func TestSkippedTestIsNotAPass(t *testing.T) {
	for _, exp := range [][]Test{nil, {{"m/skip", "TestS"}}} {
		r := parse(t, skipStream, exp...)
		if r.Clean() || outcome(r, "m/skip", "TestS") != Skip || outcome(r, "m/skip", "TestA") != Pass {
			t.Fatalf("expected %v: a skipped test read as a pass: %+v", exp, r)
		}
	}
	base := parse(t, skipStream)
	if r := parseOn(t, &base, skipStream, Test{"m/skip", "TestS"}); r.Clean() {
		t.Fatalf("an expected skipped test read as a pass: %+v", r)
	}
	// Skips alone are no pass, even when they skipped at the base.
	only := strings.ReplaceAll(skipStream, `"Action":"pass","Package":"m/skip","Test":"TestA"`, `"Action":"skip","Package":"m/skip","Test":"TestA"`)
	ob := parse(t, only)
	if r := parseOn(t, &ob, only); r.Clean() {
		t.Fatalf("a stream of skips passed: %+v", r)
	}
}

// A test that skipped at the base and skips at head (root-only tests in the
// sandbox) counts; it is still reported as Skip.
func TestSkipAtBaseAndHeadCounts(t *testing.T) {
	base := parse(t, skipStream)
	r := parseOn(t, &base, skipStream)
	if !r.Clean() || outcome(r, "m/skip", "TestS") != Skip || len(r.Blocking()) != 0 {
		t.Fatalf("a base skip: %+v", r)
	}
}

// F (H2): a test that passed at the base and skips at head does not count.
func TestPassAtBaseSkipAtHeadFails(t *testing.T) {
	green := strings.ReplaceAll(skipStream, `"Action":"skip","Package":"m/skip","Test":"TestS"`, `"Action":"pass","Package":"m/skip","Test":"TestS"`)
	base := parse(t, green)
	r := parseOn(t, &base, skipStream)
	if r.Clean() || len(r.Blocking()) != 1 || r.Blocking()[0].Name != "TestS" || r.Blocking()[0].Base != Pass {
		t.Fatalf("a pass turned skip read as a pass: %+v", r)
	}
}

// F (H1): go test ./... has no end-of-run event. A stream cut at a package
// boundary, or a TestMain that narrows -test.run, passes on its own; against
// the base, the packages and tests that never ran are Missing.
func TestNeverRanAgainstBaseFails(t *testing.T) {
	base := parse(t, okStream+skipStream)
	cut := okStream // the run died before m/skip started
	if r := parse(t, cut); !r.Clean() {
		t.Fatalf("control: the cut stream alone: %+v", r)
	}
	r := parseOn(t, &base, cut)
	if r.Clean() || r.Packages["m/skip"] != Missing || outcome(r, "m/skip", "TestA") != Missing {
		t.Fatalf("a package that never ran read as a pass: %+v", r)
	}
	// TestMain narrowed the run: TestB never ran, the package passed.
	narrowed := strings.Join(slices.DeleteFunc(strings.SplitAfter(okStream, "\n"), func(l string) bool {
		return strings.Contains(l, `"Test":"TestB`)
	}), "")
	ob := parse(t, okStream)
	r = parseOn(t, &ob, narrowed)
	if r.Clean() || outcome(r, "m/ok", "TestB") != Missing || outcome(r, "m/ok", "TestB/sub") != Missing {
		t.Fatalf("a narrowed run read as a pass: %+v", r)
	}
}

// F (P32): an expected test the stream never reports is Missing, and fails
// the report, though every reported test passed.
func TestMissingExpectedTestFails(t *testing.T) {
	r := parse(t, okStream, Test{"m/ok", "TestHidden"}, Test{"m/other", "TestA"})
	if r.Clean() || outcome(r, "m/ok", "TestHidden") != Missing || outcome(r, "m/other", "TestA") != Missing {
		t.Fatalf("a missing test read as a pass: %+v", r)
	}
	if got := r.Blocking(); len(got) != 2 || !got[0].Expected || !got[1].Expected {
		t.Fatalf("failed: %+v", got)
	}
}

func TestPanicFails(t *testing.T) {
	r := parse(t, panicStream)
	if r.Clean() || outcome(r, "m/panic", "TestP") != Fail || r.Packages["m/panic"] != Fail {
		t.Fatalf("a panic: %+v", r)
	}
}

// A build failure has no test events; the package fails, and an expected
// test in it is Missing.
func TestBuildFailureFails(t *testing.T) {
	r := parse(t, okStream+buildFailStream, Test{"m/broken", "TestA"})
	if r.Clean() || r.Packages["m/broken"] != Fail || outcome(r, "m/broken", "TestA") != Missing {
		t.Fatalf("a build failure: %+v", r)
	}
	// build-fail alone, with the package's own events cut, still fails it.
	r = parse(t, okStream+strings.SplitAfter(buildFailStream, "\n")[1])
	if r.Clean() || r.Packages["m/broken"] != Fail {
		t.Fatalf("a bare build failure: %+v", r)
	}
}

// A stream cut mid-test (the binary killed, a timeout) leaves the test and
// its package Incomplete, never a pass.
func TestCutStreamIsIncomplete(t *testing.T) {
	lines := strings.SplitAfter(okStream, "\n")
	r := parse(t, strings.Join(lines[:5], ""))
	if r.Clean() || outcome(r, "m/ok", "TestB") != Incomplete || outcome(r, "m/ok", "TestA") != Pass ||
		r.Packages["m/ok"] != Incomplete {
		t.Fatalf("a cut stream: %+v", r)
	}
}

// A failure reported once stays, whatever passes are appended after it (a
// lying runner, or -count=2); output text claiming a pass is never read.
func TestWorstOutcomeWins(t *testing.T) {
	lie := `{"Action":"run","Package":"m/panic","Test":"TestP"}
{"Action":"output","Package":"m/panic","Test":"TestP","Output":"--- PASS: TestP (0.00s)\n"}
{"Action":"pass","Package":"m/panic","Test":"TestP"}
{"Action":"pass","Package":"m/panic"}
`
	r := parse(t, panicStream+lie)
	if r.Clean() || outcome(r, "m/panic", "TestP") != Fail || r.Packages["m/panic"] != Fail {
		t.Fatalf("a later pass hid a failure: %+v", r)
	}
}

func TestNoTestsIsNotAPass(t *testing.T) {
	if r := parse(t, noTestsStream); r.Clean() {
		t.Fatalf("no test ran, yet passed: %+v", r)
	}
}

func TestUnreadableStreams(t *testing.T) {
	for name, s := range map[string]string{
		"empty":          "",
		"blank lines":    "\n\n",
		"plain text":     "ok  \tm/ok\t0.004s\n",
		"text after":     okStream + "5 passed\n",
		"unknown action": `{"Action":"won","Package":"m/ok","Test":"TestA"}` + "\n",
		"no package":     `{"Action":"pass","Test":"TestA"}` + "\n",
		"build no pkg":   `{"Action":"build-fail"}` + "\n",
		"long line":      `{"Action":"output","Package":"m","Output":"` + strings.Repeat("x", MaxLine) + `"}` + "\n",
	} {
		if _, err := ParseGoTestJSON(strings.NewReader(s), nil, nil); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%s: got %v, want ErrUnreadable", name, err)
		}
	}
	if _, err := ParseGoTestJSON(strings.NewReader(okStream), []Test{{Package: "m/ok"}}, nil); err == nil {
		t.Error("an expected test without a name was accepted")
	}
}
