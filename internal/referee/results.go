// SPDX-License-Identifier: Apache-2.0

package referee

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/internal/results"
)

// outcome is one test's result in one run, from best to worst.
type outcome int

const (
	pass outcome = iota + 1
	skip
	fail
)

// tests are one run's results by test id, and whether they report any
// failure (a failing test, or a go package that failed or never ended).
type tests struct {
	by       map[string]outcome
	failures bool
}

// add records id's outcome, the worst one when it is reported more than
// once, so a later pass never hides an earlier failure.
func (t *tests) add(id string, o outcome) {
	t.by[id] = max(t.by[id], o)
	t.failures = t.failures || o == fail
}

// id joins a test's parts with " > ", empty parts left out.
func id(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " > ")
}

var errUnreadable = errors.New("referee: the results are unreadable")

// parse reads a run's results in format (the item's results.format).
func parse(format string, b []byte) (tests, error) {
	if format == "go_test_json" {
		return parseGo(b)
	}
	return parseJUnit(b)
}

// parseGo reads a go test -json stream through internal/results: a test
// id is its package and its name.
func parseGo(b []byte) (tests, error) {
	rep, err := results.ParseGoTestJSON(bytes.NewReader(b), nil, nil)
	if err != nil {
		return tests{}, errUnreadable
	}
	t := tests{by: map[string]outcome{}}
	for _, x := range rep.Results {
		o := fail // missing or incomplete: not a pass
		switch x.Outcome {
		case results.Pass:
			o = pass
		case results.Skip:
			o = skip
		}
		t.add(id(x.Package, x.Name), o)
	}
	t.failures = t.failures || len(rep.BlockingPackages()) > 0
	return t, nil
}

// parseJUnit reads JUnit XML (a testsuites or testsuite root): a test id
// is its enclosing testsuite names, its classname and its name. A testcase
// with a failure or an error fails; one with skipped skips; any other
// passes.
func parseJUnit(b []byte) (tests, error) {
	t := tests{by: map[string]outcome{}}
	d := xml.NewDecoder(bytes.NewReader(b))
	var suites []string
	var cur string // the open testcase's id
	var o outcome
	seen := false
	for {
		tok, err := d.Token()
		if err == io.EOF && seen {
			return t, nil
		}
		if err != nil {
			return tests{}, errUnreadable
		}
		switch e := tok.(type) {
		case xml.StartElement:
			if !seen && e.Name.Local != "testsuites" && e.Name.Local != "testsuite" {
				return tests{}, errUnreadable
			}
			seen = true
			switch e.Name.Local {
			case "testsuite":
				suites = append(suites, attr(e, "name"))
			case "testcase":
				if cur != "" {
					return tests{}, errUnreadable
				}
				cur, o = id(slices.Concat(suites, []string{attr(e, "classname"), attr(e, "name")})...), pass
				if cur == "" {
					return tests{}, errUnreadable
				}
			case "failure", "error":
				o = fail
			case "skipped":
				o = max(o, skip)
			}
		case xml.EndElement:
			switch {
			case e.Name.Local == "testsuite" && len(suites) > 0:
				suites = suites[:len(suites)-1]
			case e.Name.Local == "testcase" && cur != "":
				t.add(cur, o)
				cur = ""
			}
		}
	}
}

func attr(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if a.Name.Space == "" && a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}
