// SPDX-License-Identifier: Apache-2.0

package results

// Decisions for junit_xml (P15b; pytest's --junitxml, xunit1 and xunit2):
//   - A test is a testcase, named by its file, classname and name (Package:
//     "file::classname", pytest's file, dotted module and class; Name, with
//     its parameter ids). The gate forces junit_family=xunit1, whose
//     testcases carry their file: without it, a testcase is unreadable
//     (Astra #290 H4: tests/test_a.py::TestB_test::test_x and
//     tests/test_a/TestB_test.py::test_x share their classname and name). A
//     collection error is a testcase with an error, its file the module's.
//   - The file must be the module classname names (Astra #290 r2 H1):
//     tests/test_a.py for tests.test_a, or tests.test_a.TestB_test (its
//     class), or tests/test_a/TestB_test.py for module
//     tests.test_a.TestB_test; a module's collection error (no classname)
//     names its module. pytest's file is where the function is defined: a
//     test defined elsewhere (a shared helper's, file="helpers.py") is
//     unreadable, never another test's identity.
//   - Only a testcase with no failure, error or skipped child is a pass.
//     An error (setup, teardown, collection) counts as a failure (Fail); a
//     skipped one (skip, xfail) is Skip; the worst child decides. Packages
//     are empty: JUnit has no package outcome of its own.
//   - The file is read strictly, failing closed (ErrUnreadable, never a
//     pass): a malformed or partial document, a DTD or entity, an element
//     this schema does not know, content after the root, no testcase, a
//     count attribute (tests, failures, errors, skipped) that is not the
//     number of testcases (tests) or child elements (failures, errors,
//     skipped: pytest >= 9.1 counts each child, a setup error and a
//     teardown error of one testcase two; older pytest subtracts double
//     failures, which reads as unreadable, Opus #290 M1) below it, a
//     namespaced or repeated attribute, text or a comment outside the root,
//     a processing instruction but the declaration (first, version, then
//     encoding and standalone, each once at most: Astra #290 r2 L6), and any bound
//     passed: MaxStream bytes, maxElements elements, maxDepth levels,
//     maxName bytes in an attribute read (file, classname, name, counts). Free text (messages, tracebacks,
//     output) is read past, never kept: MaxStream bounds it.
//   - Two testcases of one identity are unreadable, but for pytest's own
//     shape: a test that passes or fails and then errors in teardown is a
//     second testcase holding one error alone, after a testcase holding no
//     error or skipped child (Astra #290 r2 L5). It is merged, as Fail: it
//     can only make the test fail; a third, or another shape, is unreadable.

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
)

// The adapter's name in a profile's gate.result_adapter.
const JUnitXML = "junit_xml"

var maxElements, maxDepth, maxName = 1 << 20, 32, 4096 // var: tests lower them

// known are the elements a JUnit document may hold, by their parent.
var known = map[string][]string{
	"":           {"testsuites", "testsuite"},
	"testsuites": {"testsuite", "properties"},
	"testsuite":  {"testsuite", "testcase", "properties", "system-out", "system-err"},
	"testcase":   {"failure", "error", "skipped", "properties", "system-out", "system-err"},
	"properties": {"property"},
}

// decl is the XML declaration's grammar (XML 1.0 §2.8, VersionNum 1.x).
var decl = regexp.MustCompile(`^\s*version\s*=\s*("1\.[0-9]+"|'1\.[0-9]+')(\s+encoding\s*=\s*("[A-Za-z][A-Za-z0-9._-]*"|'[A-Za-z][A-Za-z0-9._-]*'))?` +
	`(\s+standalone\s*=\s*("(yes|no)"|'(yes|no)'))?\s*$`)

// read are the attributes the adapter reads, each within maxName.
var read = []string{"file", "classname", "name", "tests", "failures", "errors", "skipped"}

// suite counts the testcases below an open testsuite (or testsuites).
type suite struct {
	tests, failures, errors, skipped int
	declared                         map[string]int // its count attributes
}

// ParseJUnitXML reads a JUnit XML document against base and expected, as
// ParseGoTestJSON reads a stream.
func ParseJUnitXML(doc io.Reader, expected []Test, base *Report) (Report, error) {
	unreadable := func(why string, args ...any) (Report, error) {
		return Report{}, fmt.Errorf("%w (junit_xml): %s", ErrUnreadable, fmt.Sprintf(why, args...))
	}
	lr := &io.LimitedReader{R: doc, N: MaxStream + 1}
	d := xml.NewDecoder(lr)
	d.Strict, d.Entity = true, map[string]string{}
	tests := map[Test]Outcome{}
	var path []string       // the open elements
	var suites []*suite     // the open testsuites
	var test *Test          // the open testcase
	has := map[string]int{} // its failure, error and skipped children
	elements, rootDone := 0, false
	dups := map[Test]bool{}  // the identities that had their one successor
	clean := map[Test]bool{} // a first testcase with no error or skipped child: a successor may follow
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil || lr.N <= 0 {
			return unreadable("malformed, partial or beyond %d bytes", MaxStream)
		}
		switch t := tok.(type) {
		case xml.Directive:
			return unreadable("a DTD or directive")
		case xml.CharData:
			if (rootDone || elements == 0) && len(bytes.TrimSpace(t)) > 0 {
				return unreadable("text outside the root")
			}
		case xml.Comment:
			if rootDone || elements == 0 {
				return unreadable("a comment outside the root")
			}
		case xml.ProcInst: // the declaration, first, alone
			if t.Target != "xml" || d.InputOffset() != int64(len("<?xml ?>")+len(t.Inst)) || !decl.Match(t.Inst) {
				return unreadable("a processing instruction other than the declaration, first")
			}
		case xml.StartElement:
			parent := ""
			if len(path) > 0 {
				parent = path[len(path)-1]
			}
			if elements++; elements > maxElements || len(path) >= maxDepth || rootDone ||
				t.Name.Space != "" || !slices.Contains(known[parent], t.Name.Local) {
				return unreadable("an element %q under %q, past the root or beyond the bounds", t.Name.Local, parent)
			}
			path = append(path, t.Name.Local)
			attr := map[string]string{}
			for _, a := range t.Attr {
				if _, twice := attr[a.Name.Local]; twice || a.Name.Space != "" {
					return unreadable("an attribute %q repeated or namespaced", a.Name.Local)
				} else if len(a.Value) > maxName && slices.Contains(read, a.Name.Local) {
					return unreadable("an attribute beyond %d bytes", maxName)
				}
				attr[a.Name.Local] = a.Value
			}
			switch t.Name.Local {
			case "testsuites", "testsuite":
				s := &suite{declared: map[string]int{}}
				for _, k := range []string{"tests", "failures", "errors", "skipped"} {
					if v, ok := attr[k]; ok {
						n, err := strconv.Atoi(v)
						if err != nil {
							return unreadable("a %s count %q", k, v)
						}
						s.declared[k] = n
					}
				}
				suites = append(suites, s)
			case "testcase": // xunit1's file, its path: one classname may be two modules' (Astra #290 H4)
				test = &Test{Package: attr["file"] + "::" + attr["classname"], Name: attr["name"]}
				clear(has)
				if test.Name == "" || !module(attr["file"], cmpOr(attr["classname"], test.Name)) { // no file: xunit2
					return unreadable("a testcase with no name, or whose file is not its classname's module (%.64q, %.64q)", attr["file"], attr["classname"])
				}
			case "failure", "error", "skipped":
				has[t.Name.Local]++
			}
		case xml.EndElement:
			switch path = path[:len(path)-1]; t.Name.Local {
			case "testcase":
				o := Pass
				if has["skipped"] > 0 {
					o = Skip
				}
				if has["failure"] > 0 || has["error"] > 0 && !mutation.Is(mutation.JUnitErrorPasses) {
					o = Fail
				}
				// A second testcase of one identity is unreadable, but for
				// pytest's own shape: one teardown error after a pass or a
				// failure, its one error alone, which can only make it fail.
				if prior, dup := tests[*test]; !dup {
					tests[*test], clean[*test] = o, has["error"]+has["skipped"] == 0
				} else if !dups[*test] && clean[*test] && has["error"] == 1 && has["failure"]+has["skipped"] == 0 {
					tests[*test], dups[*test] = worse(prior, Fail), true
				} else {
					return unreadable("two testcases %s %s", test.Package, test.Name)
				}
				for _, s := range suites {
					s.tests++
					s.failures += has["failure"]
					s.errors += has["error"]
					s.skipped += has["skipped"]
				}
			case "testsuites", "testsuite":
				s := suites[len(suites)-1]
				suites = suites[:len(suites)-1]
				got := map[string]int{"tests": s.tests, "failures": s.failures, "errors": s.errors, "skipped": s.skipped}
				for k, n := range s.declared {
					if got[k] != n && !mutation.Is(mutation.JUnitCountsUnchecked) {
						return unreadable("%s declares %d %s, holds %d", t.Name.Local, n, k, got[k])
					}
				}
				rootDone = len(path) == 0
			}
		}
	}
	if len(tests) == 0 { // a partial document is the decoder's error
		return unreadable("no testcase")
	}
	return assemble(tests, map[string]Outcome{}, expected, base)
}

// module: file is the module classname names, or its module's (classname's
// prefix, a class below it): tests/test_a.py for tests.test_a or
// tests.test_a.TestB_test.
func module(file, classname string) bool {
	mod, ok := strings.CutSuffix(file, ".py")
	mod = strings.ReplaceAll(mod, "/", ".")
	return ok && mod != "" && (classname == mod || strings.HasPrefix(classname, mod+"."))
}

func cmpOr(a, b string) string {
	if a == "" {
		return b
	}
	return a
}
