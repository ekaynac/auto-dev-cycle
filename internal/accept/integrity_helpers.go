// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"fmt"
	"go/ast"
)

// P14d-w (#226 entry 122): assertion weakening in test helpers. Main
// counts a test's assertions, an asserting helper's call as one whatever
// it holds; a helper or a named t.Run callback could lose assertions with
// no block (traced: lead/p14d2-sizing.md). So every function and method
// declared in a package's _test.go files (tests, helpers, named
// callbacks) has its assertions compared, base against head:
//   - Its count is main's (count's), with an assertion under a branch a
//     constant condition never takes (markDead's: if, for, switch) removed,
//     and each asserting helper's call (main's one) expanded to that
//     helper's own count when it is a function of the test files: a body
//     extracted into a new helper it calls keeps its count (the replay's
//     openEffectStore → openEffectStoreAt, home → homeAt). A call within a
//     cycle of calls counts one, as main counts it. A function declared
//     again (race, !race) counts its weakest declaration.
//   - A decrease blocks ("a helper's assertions weakened").
//   - A function with assertions at the base that the head no longer
//     declares blocks when its name is still used in the directory's test
//     files ("a helper with assertions removed while still used"): a
//     rename whose old name is gone is no block here (a direct caller's
//     test loses an asserting call, main's block; a swapped t.Run callback
//     warns, P14d-b1's print).
//   - Each test file's declarations are read once a scan, each node charged
//     to the case layer's bound (r.caseWork) before it is read, a dead
//     branch's bytes too; past it, or cancelled, the file is unverifiable.
//     Totals are memoized per component: each call is read once a
//     comparison.

// fnStat is one declaration's assertions on one platform (count's, less
// dead ones and its asserting helpers' calls), the asserting helpers it
// calls (their keys, once a call), and the name a use of it spells (a
// method's, without its receiver).
type fnStat struct {
	asserts     int
	calls       []string
	plat, short string
}

// helperFacts records d, a declaration of a test file, on platform plat:
// a function's assertions (asserts, count's of it), and every name d uses.
func (s *pkgScan) helperFacts(fa *pkgFacts, plat, dir, name, path string, d ast.Decl, asserts int) {
	if s.cs.dead == nil {
		s.cs.dead = map[ast.Node]bool{}
	}
	dead := 0
	var calls []string
	if !s.over() {
		ast.Inspect(d, func(n ast.Node) bool {
			switch {
			case n == nil || !s.caseCharge(1): // each node charged before it is read
				return false
			case s.cs.dead[n]: // a branch never taken: its assertions are none, read once more, charged
				if s.caseCharge(int(n.End() - n.Pos())) {
					dead += s.count(n).asserts
				}
				return false
			}
			s.markDead(n)
			switch x := n.(type) {
			case *ast.Ident:
				if s.info.Defs[x] == nil {
					fa.names[plat+" "+x.Name] = true
				}
			case *ast.CallExpr: // an asserting helper's call, which count counts one
				if c := s.callee(x); c != nil && s.asserting[c] && s.assertion(x) {
					calls = append(calls, plat+" "+dir+":"+name+"."+declKey(c))
				}
			}
			return true
		})
	}
	if s.over() {
		if fa.bad[path] == "" { // another layer's reason kept
			fa.bad[path] = "test helpers' assertions past the guard's work bound"
		}
		return
	}
	if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil {
		k := plat + " " + dir + ":" + name + "." + declKey(fn)
		fa.helpers[k] = append(fa.helpers[k], fnStat{asserts - dead - len(calls), calls, plat, fn.Name.Name})
	}
}

// totals is each function's assertions (above). A call to a function of
// the same strongly connected component (a cycle of calls) counts one, as
// main counts it; any other call to a function of the test files counts
// that function's total: each component is summed once, whatever the
// order the map is read in.
func totals(fa pkgFacts) map[string]int {
	comp := sccs(fa)
	memo := map[string]int{}
	var total func(k string) int
	total = func(k string) int {
		if n, ok := memo[k]; ok {
			return n
		}
		n := -1
		for _, st := range fa.helpers[k] { // the weakest declaration
			t := st.asserts
			for _, c := range st.calls {
				if _, ok := fa.helpers[c]; !ok || comp[c] == comp[k] {
					t++ // no function of the test files, or within the cycle: main's one
				} else {
					t += total(c)
				}
			}
			if n < 0 || t < n {
				n = t
			}
		}
		memo[k] = n
		return n
	}
	for k := range fa.helpers {
		total(k)
	}
	return memo
}

// sccs numbers the strongly connected components of the functions' calls
// (Tarjan's).
func sccs(fa pkgFacts) map[string]int {
	index, low, comp, on := map[string]int{}, map[string]int{}, map[string]int{}, map[string]bool{}
	var stack []string
	next, n := 0, 0
	var visit func(k string)
	visit = func(k string) {
		index[k], low[k], next = next, next, next+1
		stack, on[k] = append(stack, k), true
		for _, st := range fa.helpers[k] {
			for _, c := range st.calls {
				if _, ok := fa.helpers[c]; !ok {
					continue
				} else if _, seen := index[c]; !seen {
					visit(c)
					low[k] = min(low[k], low[c])
				} else if on[c] {
					low[k] = min(low[k], index[c])
				}
			}
		}
		if low[k] == index[k] {
			for {
				c := stack[len(stack)-1]
				stack, on[c], comp[c] = stack[:len(stack)-1], false, n
				if c == k {
					break
				}
			}
			n++
		}
	}
	for _, k := range keys(fa.helpers) {
		if _, seen := index[k]; !seen {
			visit(k)
		}
	}
	return comp
}

// compareHelpers blocks a function of the test files whose assertions
// the head weakened, or removed while its name is still used.
func compareHelpers(add func(*[]Block, string, string), blocks *[]Block, b, a pkgFacts) {
	bt, at := totals(b), totals(a)
	for _, k := range keys(b.helpers) {
		bh := b.helpers[k][0]
		switch an, ok := at[k]; {
		case ok && an < bt[k]:
			add(blocks, k, fmt.Sprintf("a helper's assertions weakened (%d → %d)", bt[k], an))
		case !ok && bt[k] > 0 && a.names[bh.plat+" "+bh.short]:
			add(blocks, k, "a helper with assertions removed while still used")
		}
	}
}
