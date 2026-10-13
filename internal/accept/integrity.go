// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The integrity guard (01 §2.5 [A6]) blocks a deleted test file; fewer
// tests or assertions, or more skips, counted across the changed test
// files (a test moved between them is no loss); and a Go file gaining a
// way to forge test2json results (#226 entry 32). It warns the reviewer of
// an exit or collection hook added to a test. M1 has no approved rows.
var (
	skipMarkers   = []string{"pytest.mark.skip", "pytest.skip(", "unittest.skip", "pytest.mark.xfail", "t.Skip(", "t.Skipf(", "t.SkipNow(", "b.Skip", ".skip(", " xit(", " xdescribe("}
	testMarkers   = []string{"def test", "func Test", "func Fuzz", "func Example"}
	assertMarkers = []string{"assert", "t.Error", "t.Fatal", "require.", "expect(", "self.fail(", "pytest.raises("}
	forgedMarkers = []string{"testing.Testing(", "\x16", `\x16`, `\026`, `\u0016`, `\U00000016`}
	warnMarkers   = []string{"os.Exit(", "sys.exit(", "os._exit(", "pytest_collection_modifyitems", "pytest_runtest_", "func TestMain("}
)

// isTest: a test file by the conventions of M1's recipes (Go, pytest) and
// the common JavaScript ones, or any file under a tests directory.
func isTest(p string) bool {
	base := path.Base(p)
	switch {
	case strings.HasSuffix(base, "_test.go"), base == "conftest.py",
		strings.HasSuffix(base, ".py") && (strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py")),
		strings.Contains(base, ".test."), strings.Contains(base, ".spec."):
		return true
	}
	for _, c := range strings.Split(path.Dir(p), "/") {
		if c == "tests" || c == "test" || c == "__tests__" {
			return true
		}
	}
	return false
}

func count(b []byte, markers []string) int {
	n := 0
	for _, m := range markers {
		n += bytes.Count(b, []byte(m))
	}
	return n
}

// guard runs the integrity guard over changes, reading at most maxRead
// bytes of blobs in all. A change it cannot read whole is not counted,
// and goes to the reviewer.
func guard(ctx context.Context, g index, changes []change) (blocks, warnings []Block, err error) {
	budget := int64(maxRead)
	var before, after [3]int // tests, assertions, skips over the changed test files
	tally := func(t *[3]int, b []byte) {
		t[0], t[1], t[2] = t[0]+count(b, testMarkers), t[1]+count(b, assertMarkers), t[2]+count(b, skipMarkers)
	}
	finding := func(to *[]Block, p, what string) {
		*to = append(*to, Block{vocab.ReasonTestIntegrityViolation, p, what})
	}
	for _, c := range changes {
		srcTest, dstTest := isTest(c.src), isTest(c.dst)
		if srcTest && (c.status == "D" || c.status == "R" && !dstTest) {
			finding(&blocks, c.src, "a test file deleted")
		}
		srcRead := c.status != "A" && c.status != "C" && (srcTest || strings.HasSuffix(c.src, ".go"))
		dstRead := c.status != "D" && (dstTest || strings.HasSuffix(c.dst, ".go"))
		old, okOld, err := g.blob(ctx, srcRead, c.srcBlob, &budget)
		cur, okCur, err2 := g.blob(ctx, dstRead, c.dstBlob, &budget)
		switch {
		case err != nil || err2 != nil:
			return nil, nil, errors.Join(err, err2)
		case !okOld || !okCur:
			finding(&warnings, c.dst, "not read by the integrity guard (over its bound): review it")
			continue
		}
		if srcTest && srcRead {
			tally(&before, old)
		}
		if dstTest && dstRead {
			tally(&after, cur)
		}
		if dstRead && strings.HasSuffix(c.dst, ".go") && count(cur, forgedMarkers) > count(old, forgedMarkers) {
			finding(&blocks, c.dst, "can forge go test2json results (testing.Testing or a 0x16 byte)")
		}
		if dstTest && count(cur, warnMarkers) > count(old, warnMarkers) {
			finding(&warnings, c.dst, "adds a process exit or a collection hook to a test")
		}
	}
	for i, what := range []string{"fewer tests", "fewer assertions"} {
		if after[i] < before[i] {
			finding(&blocks, "", fmt.Sprintf("%s across the changed test files (%d → %d)", what, before[i], after[i]))
		}
	}
	if after[2] > before[2] {
		finding(&blocks, "", fmt.Sprintf("a test skipped (%d more skip markers)", after[2]-before[2]))
	}
	return blocks, warnings, nil
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
