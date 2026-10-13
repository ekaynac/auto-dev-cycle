// SPDX-License-Identifier: Apache-2.0

// Package results turns a gate command's output into per-test results (the
// profile's gate.result_adapter, 07). The gate's verdict comes from these
// results, never from the command's exit code or its free text (I13).
//
// Decisions for go_test_json (00 §8 item 1, ledger L4):
//   - Per test, only a pass is a pass. A skipped test is Skip; a test that
//     started and never ended (a panic that killed the binary, a timeout, a
//     cut stream) is Incomplete; a package that failed to build is Fail.
//   - go test ./... has no end-of-run event, so a stream alone cannot show
//     that every package ran. The verdict is base-aware: given the base's
//     report (the measured baseline, P15), every package and test the base
//     reported and the head does not is Missing, as is every expected test
//     the head does not report.
//   - A test counts as passed (Counts) only if it passed at head, or it
//     skipped at head and also skipped at the base (the tests that skip as
//     the sandbox's uid 0). A test that passed at the base and skips at head
//     does not count, and neither does an expected test that skipped.
//     Without a base, any skip fails Clean.
//   - A test reported more than once (-count, or a stream with a forged
//     line appended) keeps its worst outcome, so a later pass never hides an
//     earlier failure.
//   - Only the Action field is read; Output is never parsed. The events are
//     not proof by themselves: the test binary writes the stream test2json
//     reads, so it can forge events (framing lines it prints on stdout,
//     \x16-marked ones included, or non-test code that sees
//     testing.Testing() and prints a fake run before os.Exit(0)). That is
//     for accept's integrity guard (P14) to stop, not for this adapter.
//   - A stream that is not go test -json (a line that is not one event, an
//     unknown action, a test event without its package, no event at all, or
//     beyond the bounds) is unreadable: an error, never an empty report.
package results

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// The adapter's name in a profile's gate.result_adapter.
const GoTestJSON = "go_test_json"

// Bounds on a stream: its size and one event line.
const (
	MaxStream = 64 << 20
	MaxLine   = 1 << 20
)

// ErrUnreadable: the stream is not a go test -json stream within bounds.
var ErrUnreadable = errors.New("results: unreadable go test -json stream")

// Outcome is one test's or one package's result.
type Outcome string

const (
	Pass       Outcome = "pass"
	Skip       Outcome = "skip"
	Missing    Outcome = "missing"    // at the base or expected, never reported
	Incomplete Outcome = "incomplete" // started, never ended
	Fail       Outcome = "fail"
)

// rank orders outcomes from best to worst.
var rank = map[Outcome]int{Pass: 0, Skip: 1, Missing: 2, Incomplete: 3, Fail: 4}

func worse(a, b Outcome) Outcome {
	if a == "" || rank[b] > rank[a] {
		return b
	}
	return a
}

// Test names a test in its package's import path. A subtest's name is the
// one go test reports, after its rewriting: "TestX/a_b" for t.Run("a b").
type Test struct {
	Package string `json:"package"`
	Name    string `json:"name"`
}

// Result is one test's outcome at head, and at the base when one was given
// and reported it.
type Result struct {
	Test
	Outcome  Outcome `json:"outcome"`
	Base     Outcome `json:"base,omitempty"`
	Expected bool    `json:"expected"`
}

// Counts: the test passed, or it skipped at head and at the base and is
// not expected.
func (x Result) Counts() bool {
	return x.Outcome == Pass || x.Outcome == Skip && x.Base == Skip && !x.Expected
}

// Report is a parsed stream. Packages holds each package's own outcome: a
// package with no test files is Skip, one that failed to build is Fail, one
// the base reported and the head did not is Missing.
type Report struct {
	Results  []Result           `json:"results"`
	Packages map[string]Outcome `json:"packages"`
}

// Clean: some test passed, every test Counts, and every package passed or
// had no test files. It is only as complete as the base the report was
// parsed against: without a base, a package that never ran is not seen
// (and any skip fails), so the gate parses against its measured baseline.
func (r Report) Clean() bool {
	for _, o := range r.Packages {
		if o != Pass && o != Skip {
			return false
		}
	}
	return slices.ContainsFunc(r.Results, func(x Result) bool { return x.Outcome == Pass }) &&
		!slices.ContainsFunc(r.Results, func(x Result) bool { return !x.Counts() })
}

// Blocking lists every result that does not count.
func (r Report) Blocking() []Result {
	return slices.DeleteFunc(slices.Clone(r.Results), Result.Counts)
}

type event struct {
	Action, Package, Test, ImportPath string
}

// info carries no result: output and the events that annotate a test.
var info = map[string]bool{"output": true, "pause": true, "cont": true, "attr": true, "artifacts": true, "build-output": true}

// terminal maps an action that ends a test or a package to its outcome. A
// benchmark that logged output and did not fail ends with "bench".
var terminal = map[string]Outcome{"pass": Pass, "bench": Pass, "skip": Skip, "fail": Fail}

type state struct {
	outcome Outcome
	open    bool // started and not yet ended
}

// ParseGoTestJSON reads a go test -json stream against base, the base's
// report (nil for none, which makes the report strict and blind to packages
// that never ran). Each expected test, and each package and test of base,
// that the stream does not report is Missing.
func ParseGoTestJSON(stream io.Reader, expected []Test, base *Report) (Report, error) {
	tests := map[Test]*state{}
	pkgs := map[string]*state{}
	testState := func(k Test) *state {
		if tests[k] == nil {
			tests[k] = &state{}
		}
		return tests[k]
	}
	pkgState := func(p string) *state {
		if pkgs[p] == nil {
			pkgs[p] = &state{}
		}
		return pkgs[p]
	}

	sc := bufio.NewScanner(io.LimitReader(stream, MaxStream+1))
	sc.Buffer(make([]byte, 0, 64<<10), MaxLine)
	var read, events int
	for sc.Scan() {
		read += len(sc.Bytes()) + 1
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev event
		if err := json.Unmarshal(line, &ev); err != nil {
			return Report{}, fmt.Errorf("%w: line %d is not an event", ErrUnreadable, events+1)
		}
		events++
		switch o, end := terminal[ev.Action]; {
		case ev.Action == "build-fail":
			// ImportPath is "pkg" or "pkg [pkg.test]"; the package's own
			// fail event follows, and this keeps it failed whatever follows.
			p, _, _ := strings.Cut(ev.ImportPath, " ")
			if p == "" {
				return Report{}, fmt.Errorf("%w: a build failure names no package", ErrUnreadable)
			}
			pkgState(p).outcome = Fail
		case info[ev.Action]:
		case ev.Package == "":
			return Report{}, fmt.Errorf("%w: a %q event names no package", ErrUnreadable, ev.Action)
		case ev.Action == "start" && ev.Test == "":
			pkgState(ev.Package).open = true
		case ev.Action == "run" && ev.Test != "":
			pkgState(ev.Package)
			testState(Test{ev.Package, ev.Test}).open = true
		case end && ev.Test == "":
			s := pkgState(ev.Package)
			s.outcome, s.open = worse(s.outcome, o), false
		case end:
			pkgState(ev.Package)
			s := testState(Test{ev.Package, ev.Test})
			s.outcome, s.open = worse(s.outcome, o), false
		default:
			return Report{}, fmt.Errorf("%w: unknown action %q", ErrUnreadable, ev.Action)
		}
	}
	if err := sc.Err(); err != nil || read > MaxStream {
		return Report{}, fmt.Errorf("%w: beyond its bounds or unreadable", ErrUnreadable)
	}
	if events == 0 {
		return Report{}, fmt.Errorf("%w: no events", ErrUnreadable)
	}

	rep := Report{Packages: map[string]Outcome{}}
	for p, s := range pkgs {
		if s.open || s.outcome == "" {
			s.outcome = worse(s.outcome, Incomplete)
		}
		rep.Packages[p] = s.outcome
	}
	at := map[Test]Outcome{}
	if base != nil {
		for p := range base.Packages {
			if _, ok := rep.Packages[p]; !ok {
				rep.Packages[p] = Missing
			}
		}
		for _, x := range base.Results {
			at[x.Test] = x.Outcome
			if tests[x.Test] == nil {
				tests[x.Test] = &state{outcome: Missing}
			}
		}
	}
	want := map[Test]bool{}
	for _, t := range expected {
		if t.Package == "" || t.Name == "" {
			return Report{}, errors.New("results: an expected test names its package and its name")
		}
		want[t] = true
		if tests[t] == nil {
			tests[t] = &state{outcome: Missing}
		}
	}
	for _, t := range slices.SortedFunc(maps.Keys(tests), func(a, b Test) int {
		return cmp.Or(cmp.Compare(a.Package, b.Package), cmp.Compare(a.Name, b.Name))
	}) {
		s := tests[t]
		if s.open || s.outcome == "" {
			s.outcome = worse(s.outcome, Incomplete)
		}
		rep.Results = append(rep.Results, Result{Test: t, Outcome: s.outcome, Base: at[t], Expected: want[t]})
	}
	return rep, nil
}
