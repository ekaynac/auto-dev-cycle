// SPDX-License-Identifier: Apache-2.0

package results

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// The testdata documents are real pytest 9.1.1 --junitxml output (xunit2,
// and xunit1 for pytest-xunit1.xml), its paths and host name rewritten.
func junit(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func outcomes(r Report) map[string]Outcome {
	m := map[string]Outcome{}
	for _, x := range r.Results {
		m[x.Package+"::"+x.Name] = x.Outcome
	}
	return m
}

// TestJUnitPytest: pytest's own documents. Only a testcase with no
// failure, error or skipped child passes; an error (setup, teardown,
// collection) is a failure, and a skip or xfail a skip; pytest's second
// testcase for a teardown error makes its test fail.
func TestJUnitPytest(t *testing.T) {
	r, err := ParseJUnitXML(strings.NewReader(junit(t, "pytest-mix.xml")), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := "tests.test_mix::"
	want := map[string]Outcome{m + "test_pass": Pass, m + "test_fail": Fail, m + "test_skip": Skip, m + "test_xfail": Skip,
		m + "test_setup_error": Fail, m + "test_fail_and_teardown": Fail, m + "test_param[1]": Pass, m + "test_param[2]": Pass,
		"tests.test_mix.TestK::test_m": Pass, "tests.test_ok::test_ok": Pass}
	got := outcomes(r)
	if len(got) != len(want) || r.Clean() || r.Green() {
		t.Fatalf("mix: %v", got)
	}
	for k, o := range want {
		if got[k] != o {
			t.Errorf("%s: %s, want %s", k, got[k], o)
		}
	}
	if b, _ := json.Marshal(r); strings.Contains(string(b), "CANARY") {
		t.Errorf("the report keeps failure text")
	}
	for _, name := range []string{"pytest-ok.xml", "pytest-xunit1.xml"} {
		if r, err := ParseJUnitXML(strings.NewReader(junit(t, name)), nil, nil); err != nil || !r.Clean() || !r.Green() {
			t.Errorf("%s: %v, %v", name, outcomes(r), err)
		}
	}
	r, err = ParseJUnitXML(strings.NewReader(junit(t, "pytest-collect-error.xml")), nil, nil)
	if err != nil || outcomes(r)["::tests.test_broken"] != Fail || r.Clean() {
		t.Errorf("a collection error: %v, %v", outcomes(r), err)
	}
	// Against a base and an expected test: what the document does not
	// report is Missing.
	base, err := ParseJUnitXML(strings.NewReader(junit(t, "pytest-mix.xml")), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err = ParseJUnitXML(strings.NewReader(junit(t, "pytest-ok.xml")), []Test{{"tests.test_hidden", "test_acceptance"}}, &base)
	got = outcomes(r)
	if err != nil || got[m+"test_pass"] != Missing || got["tests.test_hidden::test_acceptance"] != Missing || got["tests.test_ok::test_ok"] != Pass || r.Clean() {
		t.Errorf("against a base: %v, %v", got, err)
	}
}

// TestJUnitStrict: everything but a well-formed pytest-shaped document
// within bounds is unreadable, never a pass.
func TestJUnitStrict(t *testing.T) {
	suite := func(attrs, body string) string {
		return `<?xml version="1.0" encoding="utf-8"?><testsuites name="pytest tests"><testsuite name="pytest" ` + attrs + `>` + body + `</testsuite></testsuites>`
	}
	pass := `<testcase classname="m" name="a"/>`
	mix := junit(t, "pytest-mix.xml")
	many := func(n int) string { // n distinct passing testcases, each with its output
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, `<testcase classname="m" name="t%d"><system-out/></testcase>`, i)
		}
		return b.String()
	}
	defer func(e, d, n int) { maxElements, maxDepth, maxName = e, d, n }(maxElements, maxDepth, maxName)
	maxElements, maxDepth, maxName = 64, 8, 64
	for name, doc := range map[string]string{
		"empty":                        "",
		"malformed":                    suite(`tests="1"`, `<testcase classname="m" name="a">`),
		"partial":                      mix[:len(mix)/2],
		"a DTD":                        `<?xml version="1.0"?><!DOCTYPE x [<!ENTITY e "x">]>` + suite(`tests="1"`, pass),
		"an entity":                    suite(`tests="1"`, `<testcase classname="m" name="&e;"/>`),
		"an unknown element":           suite(`tests="1"`, `<testcase classname="m" name="a"><rerunFailure/></testcase>`),
		"a namespaced element":         suite(`tests="1"`, `<x:testcase xmlns:x="u" classname="m" name="a"/>`),
		"a second root":                suite(`tests="1"`, pass) + `<testsuite tests="0"/>`,
		"no testcase":                  suite(`tests="0"`, ``),
		"a testcase with no name":      suite(`tests="1"`, `<testcase classname="m"/>`),
		"one identity twice":           suite(`tests="2"`, pass+pass),
		"one identity, then a failure": suite(`tests="2" failures="1"`, pass+`<testcase classname="m" name="a"><failure/></testcase>`),
		"tests miscounted":             suite(`tests="2"`, pass),
		"failures miscounted":          suite(`tests="1" failures="0"`, `<testcase classname="m" name="a"><failure/></testcase>`),
		"errors miscounted":            suite(`tests="1" errors="0"`, `<testcase classname="m" name="a"><error/></testcase>`),
		"skipped miscounted":           suite(`tests="1" skipped="0"`, `<testcase classname="m" name="a"><skipped/></testcase>`),
		"a count that is no number":    suite(`tests="one"`, pass),
		"a root count miscounted":      strings.Replace(suite(`tests="1"`, pass), `<testsuites name="pytest tests">`, `<testsuites tests="3">`, 1),
		"a name beyond its bound":      suite(`tests="1"`, `<testcase classname="m" name="`+strings.Repeat("n", 65)+`"/>`),
		"levels beyond their bound":    strings.Repeat("<testsuite>", 9) + pass + strings.Repeat("</testsuite>", 9),
		"elements beyond their bound":  suite(`tests="35"`, many(35)),
	} {
		if r, err := ParseJUnitXML(strings.NewReader(doc), nil, nil); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%s: %v, %v; want unreadable", name, outcomes(r), err)
		}
	}
	// Past MaxStream bytes, a whole document then whitespace to the bound
	// and past it: unreadable, never the part read.
	big := suite(`tests="1"`, pass) + strings.Repeat(" ", MaxStream)
	if r, err := ParseJUnitXML(strings.NewReader(big), nil, nil); !errors.Is(err, ErrUnreadable) {
		t.Errorf("a document past MaxStream: %v, %v; want unreadable", outcomes(r), err)
	}
	// Within the lowered bounds, the same shapes read.
	if r, err := ParseJUnitXML(strings.NewReader(suite(`tests="2" failures="0" errors="1" skipped="0"`, pass+`<testcase classname="m" name="a"><error/></testcase>`)), nil, nil); err != nil || outcomes(r)["m::a"] != Fail {
		t.Errorf("pytest's teardown shape: %v, %v", outcomes(r), err)
	}
	if r, err := ParseJUnitXML(strings.NewReader(suite(`tests="30"`, many(30))), nil, nil); err != nil || !r.Clean() {
		t.Errorf("elements within their bound: %v", err)
	}
}
