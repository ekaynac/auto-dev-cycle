// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"fmt"
	"go/ast"
	"go/types"
	"maps"
	"slices"
)

// P14d-w (#226 entry 122): assertion weakening in test helpers. Main
// counts a test's assertions, an asserting helper's call as one whatever
// it holds; a helper or a named t.Run callback could lose assertions with
// no block (traced: lead/p14d2-sizing.md). So every function and method
// declared in a package's _test.go files that a test reaches (tests,
// helpers, named callbacks) has its assertions compared, base against
// head:
//   - A helper's own count is main's (count's), with an assertion under a
//     branch a constant condition never takes (markDead's: if, for,
//     switch) removed, and the calls count counted as one taken out; each
//     call to a function of the test files then adds that function's
//     total (a call within a cycle of calls adds nothing), and any other
//     call count counted adds one, as main counts it. The calls are
//     every call to such a function, read from the syntax, never from
//     main's asserting fixed point, whose depth depends on map order (Opus
//     #369 H2). A body extracted into a new helper it calls keeps its
//     count (the replay's openEffectStore → openEffectStoreAt). A function
//     declared again (race, !race) counts its weakest declaration.
//   - At the head, a callee that exists at the base adds the lesser of its
//     base and head totals (Opus #369 M1): a caller's lost assertion is
//     never offset by a callee's gain. A new callee adds its head total.
//   - A decrease blocks ("a helper's assertions weakened"). A gain is
//     clean. A total past maxHelperTotal, in either version, is never
//     compared: the helper is unverifiable (Opus #369 H1).
//   - Only functions a test (Test, Fuzz, Example) reaches through calls
//     and references (a named t.Run callback) at the base are compared
//     (Opus #369 L1). A test itself is main's to compare (Opus #369 L2):
//     here it blocks only when an assertion of its own body is made dead
//     (any declaration's: the weakest live count, the most dead; Opus #369
//     r2 H1), or when its set of callees among the test files' functions
//     changed and its total fell (a weaker body inlined; r2 M1, r3 M2).
//   - A function with assertions at the base that the head no longer
//     declares blocks when its name is still used in the directory's test
//     files ("a helper with assertions removed while still used").
//   - Each test file's declarations are read once a scan, each node charged
//     to the case layer's bound (r.caseWork) before it is read, a dead
//     branch's bytes too; past it, or cancelled, the file is unverifiable.
//     Totals are memoized: each call is read once a comparison.

// maxHelperTotal bounds a helper's total, which duplicated calls can
// multiply: past it the helper is unverifiable. A var: tests lower it.
var maxHelperTotal = 1 << 40

// fnStat is one declaration's assertions on one platform: its own (count's,
// less dead ones and the calls count counted as one), dead (count's under
// branches never taken), the functions it calls (their keys, once a call),
// the calls count counted (ones) and the functions it refers to, and the
// name a use of it spells (a method's, without its receiver).
type fnStat struct {
	own, dead         int
	calls, ones, refs []string
	plat, short       string
	test              bool
}

// helperFacts records d, a declaration of a test file, on platform plat:
// a function's assertions (asserts, count's of it), and every name d uses.
func (s *pkgScan) helperFacts(fa *pkgFacts, plat, dir, name, path string, d ast.Decl, asserts int) {
	if s.cs.dead == nil {
		s.cs.dead = map[ast.Node]bool{}
	}
	key := func(c *ast.FuncDecl) string { return plat + " " + dir + ":" + name + "." + declKey(c) }
	dead := 0
	var calls, ones, refs []string
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
				if f, ok := s.info.Uses[x].(*types.Func); ok && s.decls[f.Origin()] != nil {
					refs = append(refs, key(s.decls[f.Origin()]))
				}
			case *ast.CallExpr:
				if c := s.callee(x); c != nil {
					calls = append(calls, key(c))
					if s.asserting[c] && s.assertion(x) { // count's one for it
						ones = append(ones, key(c))
					}
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
		test := fn.Recv == nil && goTest(fn.Name.Name)
		fa.helpers[key(fn)] = append(fa.helpers[key(fn)], fnStat{asserts - dead - len(ones), dead, calls, ones, refs, plat, fn.Name.Name, test})
	}
}

// totals is each function's total (above), saturated at maxHelperTotal;
// base, when given, is the base's: a callee it holds adds the lesser of
// its two totals. A call within a strongly connected component of calls
// adds nothing, whatever the order the map is read in.
func totals(fa pkgFacts, base map[string]int) map[string]int {
	comp := sccs(fa)
	memo := map[string]int{}
	var total func(k string) int
	total = func(k string) int {
		if n, ok := memo[k]; ok {
			return n
		}
		n := -1
		for _, st := range fa.helpers[k] { // the weakest declaration
			t := st.own
			for _, c := range st.calls {
				if _, ok := fa.helpers[c]; !ok || comp[c] == comp[k] {
					continue // no function of the test files, or within the cycle
				}
				add := total(c)
				if b, ok := base[c]; ok && b < add {
					add = b
				}
				t = min(t+add, maxHelperTotal+1)
			}
			for _, c := range st.ones { // another package file's helper: main's one
				if _, ok := fa.helpers[c]; !ok {
					t = min(t+1, maxHelperTotal+1)
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

// reached is the functions of the test files a test reaches through calls
// and references (a named t.Run callback), the tests included.
func reached(fa pkgFacts) map[string]bool {
	seen := map[string]bool{}
	var work []string
	for _, k := range keys(fa.helpers) {
		if slices.ContainsFunc(fa.helpers[k], func(st fnStat) bool { return st.test }) {
			seen[k], work = true, append(work, k)
		}
	}
	for len(work) > 0 {
		k := work[len(work)-1]
		work = work[:len(work)-1]
		for _, st := range fa.helpers[k] {
			for _, c := range slices.Concat(st.calls, st.refs) {
				if _, ok := fa.helpers[c]; ok && !seen[c] {
					seen[c], work = true, append(work, c)
				}
			}
		}
	}
	return seen
}

// compareHelpers blocks a function of the test files a test reaches whose
// assertions the head weakened, or removed while its name is still used;
// a test whose own assertions the head made dead, or whose total fell
// with its set of callees changed.
func compareHelpers(add func(*[]Block, string, string), blocks *[]Block, b, a pkgFacts) {
	bt := totals(b, nil)
	at, live := totals(a, bt), reached(b)
	for _, k := range keys(b.helpers) {
		bh := b.helpers[k][0]
		switch ah, ok := a.helpers[k]; {
		case !live[k]:
		case bh.test && ok:
			bl, bd := liveDead(b.helpers[k])
			al, ad := liveDead(ah)
			switch {
			case ad > bd && al < bl: // its own live assertions fewer, its dead ones more (its calls are main's)
				add(blocks, k, fmt.Sprintf("a test's assertions made dead (%d → %d live)", bl, al))
			case maps.Equal(callees(b, b.helpers[k]), callees(a, ah)): // Opus #369 r2 M1, r3 M2
			case bt[k] > maxHelperTotal || at[k] > maxHelperTotal:
				add(blocks, k, "unverifiable: test helpers' totals past the guard's bound")
			case at[k] < bt[k]:
				add(blocks, k, fmt.Sprintf("a test's assertions weakened, its callees changed (%d → %d)", bt[k], at[k]))
			}
		case bh.test:
		case bt[k] > maxHelperTotal || ok && at[k] > maxHelperTotal:
			add(blocks, k, "unverifiable: test helpers' totals past the guard's bound")
		case ok && at[k] < bt[k]:
			add(blocks, k, fmt.Sprintf("a helper's assertions weakened (%d → %d)", bt[k], at[k]))
		case !ok && bt[k] > 0 && a.names[bh.plat+" "+bh.short]:
			add(blocks, k, "a helper with assertions removed while still used")
		}
	}
}

// callees is the functions of the test files sts's declarations call.
func callees(fa pkgFacts, sts []fnStat) map[string]bool {
	set := map[string]bool{}
	for _, st := range sts {
		for _, c := range st.calls {
			if _, ok := fa.helpers[c]; ok {
				set[c] = true
			}
		}
	}
	return set
}

// liveDead is a test's own live assertions, its weakest declaration's,
// and its dead ones, its most's: every declaration (race, !race) is
// compared (Opus #369 r2 H1).
func liveDead(sts []fnStat) (live, dead int) {
	live = sts[0].own
	for _, st := range sts {
		live, dead = min(live, st.own), max(dead, st.dead)
	}
	return live, dead
}
