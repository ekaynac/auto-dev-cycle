// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The integrity guard (01 §2.5 [A6]) blocks, without an approved row (M1
// has none): a collected test file deleted or made uncollected; a test
// name gone from the changed collected files; fewer assertions or more
// skips in a test (per name), or more skips across the changed files; a
// _test.go build constraint gained or changed; a collected test source it
// cannot read whole; a Go file gaining a way to forge test2json results
// (#226 entry 32). It warns the reviewer of an exit or collection hook.
var (
	skipMarkers   = []string{"pytest.mark.skip", "pytest.mark.skipif", "pytest.skip(", "importorskip", "unittest.skip", "pytest.mark.xfail", "t.Skip(", "t.Skipf(", "t.SkipNow(", "b.Skip", ".skip(", " xit(", " xdescribe("}
	assertMarkers = []string{"assert", "t.Error", "t.Fatal", "require.", "expect(", "self.fail(", "pytest.raises("}
	forgedMarkers = []string{"testing.Testing(", "\x16", `\x16`, `\026`, `\u0016`, `\U00000016`}
	warnMarkers   = []string{"os.Exit(", "sys.exit(", "os._exit(", "pytest_collection_modifyitems", "pytest_runtest_", "func TestMain("}
	pyTest        = regexp.MustCompile(`^(\s*)(?:async\s+)?def\s+(test\w*)\s*\(`)
	goTest        = regexp.MustCompile(`^func\s+((?:Test|Fuzz|Example)\w*)\s*\(`)
	constraint    = regexp.MustCompile(`(?m)^//(?:go:build|\s*\+build)\b.*$`)
	jsExt         = []string{".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".mts", ".cts"}
)

// collected: the recipes' runners collect p as a test file (go test,
// pytest's default patterns, the common JavaScript runners).
func collected(p string) bool {
	base, ext := path.Base(p), path.Ext(p)
	switch {
	case strings.HasSuffix(base, "_test.go"):
		return true
	case ext == ".py":
		return strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py")
	}
	return slices.Contains(jsExt, ext) && (strings.Contains(base, ".test.") || strings.Contains(base, ".spec."))
}

// isTest: collected, a pytest conftest, or any file under a tests
// directory (its changes are read and counted for skips and hooks).
func isTest(p string) bool {
	if collected(p) || path.Base(p) == "conftest.py" {
		return true
	}
	return slices.ContainsFunc(strings.Split(path.Dir(p), "/"), func(c string) bool { return c == "tests" || c == "test" || c == "__tests__" })
}

func count(b []byte, markers []string) int {
	n := 0
	for _, m := range markers {
		n += bytes.Count(b, []byte(m))
	}
	return n
}

// stat is one test's assertions and skips (its decorators', its body's).
type stat struct{ asserts, skips int }

// tests adds the Go and Python tests of src to into, by name: a test is a
// line-anchored definition, and its body runs to the closing "}" (Go) or
// to the next line indented no deeper than the def (Python). Comment
// lines count for nothing.
func tests(src []byte, into map[string]stat) {
	name, indent, deco := "", -1, 0
	for _, line := range strings.Split(string(src), "\n") {
		t := strings.TrimSpace(line)
		if m := goTest.FindStringSubmatch(line); m != nil {
			name, indent = m[1], -1
		} else if m := pyTest.FindStringSubmatch(line); m != nil {
			name, indent = m[2], len(m[1])
			into[name] = stat{into[name].asserts, into[name].skips + deco}
		} else if indent < 0 && line == "}" || indent >= 0 && t != "" && len(line)-len(strings.TrimLeft(line, " \t")) <= indent {
			name, indent = "", -1
		}
		if strings.HasPrefix(t, "@") {
			deco += count([]byte(t), skipMarkers)
		} else if t != "" {
			deco = 0
		}
		if name == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "//") {
			continue
		}
		s := into[name]
		into[name] = stat{s.asserts + count([]byte(line), assertMarkers), s.skips + count([]byte(line), skipMarkers)}
	}
}

// guard runs the integrity guard over changes, reading at most maxRead
// bytes of blobs in all.
func guard(ctx context.Context, g index, changes []change) (blocks, warnings []Block, err error) {
	budget := int64(maxRead)
	before, after := map[string]stat{}, map[string]stat{}
	var skipsBefore, skipsAfter int
	finding := func(to *[]Block, p, what string) {
		*to = append(*to, Block{vocab.ReasonTestIntegrityViolation, p, what})
	}
	for _, c := range changes {
		srcC, dstC := collected(c.src), collected(c.dst)
		if srcC && (c.status == "D" || c.status == "R" && !dstC) {
			finding(&blocks, c.src, "a test file deleted or no longer collected")
		}
		goSrc, goDst := strings.HasSuffix(c.src, ".go"), strings.HasSuffix(c.dst, ".go")
		srcRead := c.status != "A" && c.status != "C" && (isTest(c.src) || goSrc)
		dstRead := c.status != "D" && (isTest(c.dst) || goDst)
		old, okOld, err := g.blob(ctx, srcRead, c.srcBlob, &budget)
		cur, okCur, err2 := g.blob(ctx, dstRead, c.dstBlob, &budget)
		switch {
		case err != nil || err2 != nil:
			return nil, nil, errors.Join(err, err2)
		case srcC && !okOld || dstC && !okCur:
			finding(&blocks, c.dst, "unverifiable: over the guard's read bound")
			continue
		case !okOld || !okCur:
			finding(&warnings, c.dst, "not read by the integrity guard (over its bound): review it")
			continue
		}
		if srcRead && srcC {
			tests(old, before)
		}
		if dstRead && dstC {
			tests(cur, after)
		}
		skipsBefore, skipsAfter = skipsBefore+count(old, skipMarkers), skipsAfter+count(cur, skipMarkers)
		if srcRead && dstRead && strings.HasSuffix(c.dst, "_test.go") &&
			!slices.EqualFunc(constraint.FindAll(old, -1), constraint.FindAll(cur, -1), bytes.Equal) {
			finding(&blocks, c.dst, "a build constraint gained or changed")
		}
		if dstRead && goDst && count(cur, forgedMarkers) > count(old, forgedMarkers) {
			finding(&blocks, c.dst, "can forge go test2json results (testing.Testing or a 0x16 byte)")
		}
		gains := count(cur, warnMarkers) > count(old, warnMarkers)
		if gains && isTest(c.dst) || gains && goDst && g.hasTests(ctx, path.Dir(c.dst)) {
			finding(&warnings, c.dst, "adds a process exit or a collection hook beside tests")
		}
	}
	for _, name := range slices.Sorted(maps.Keys(before)) {
		b, a := before[name], after[name]
		_, kept := after[name]
		switch {
		case !kept:
			finding(&blocks, name, "a test deleted")
		case a.asserts < b.asserts:
			finding(&blocks, name, fmt.Sprintf("fewer assertions in a test (%d → %d)", b.asserts, a.asserts))
		case a.skips > b.skips:
			finding(&blocks, name, "a test skipped")
		}
	}
	if skipsAfter > skipsBefore {
		finding(&blocks, "", fmt.Sprintf("a test skipped (%d more skip markers)", skipsAfter-skipsBefore))
	}
	return blocks, warnings, nil
}

// hasTests: the candidate's package dir holds a _test.go file.
func (g index) hasTests(ctx context.Context, dir string) bool {
	out, err := g.run(ctx, nil, "ls-files", "--", dir+"/")
	return err == nil && slices.ContainsFunc(strings.Split(string(out), "\n"), func(p string) bool {
		return path.Dir(p) == dir && strings.HasSuffix(p, "_test.go")
	})
}

// blob reads blob id when want, within the budget: ok is false, and
// nothing is read, when it is over maxFile or the budget left.
func (g index) blob(ctx context.Context, want bool, id string, budget *int64) ([]byte, bool, error) {
	if !want {
		return nil, true, nil
	}
	size, err := g.run(ctx, nil, "cat-file", "-s", id)
	if err != nil {
		return nil, false, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(size)), 10, 64)
	if err != nil || n > maxFile || n > *budget {
		return nil, false, err
	}
	*budget -= n
	b, err := g.run(ctx, nil, "cat-file", "blob", id)
	return b, err == nil, err
}
