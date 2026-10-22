// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"fmt"
	"go/ast"
)

// P14d-w1b (#226 entries 122, 129, 134; supersedes #369 and #376):
// assertion weakening in test helpers. Main counts a test's assertions, an
// asserting helper's call as one whatever it holds; a helper or a named
// t.Run callback could lose assertions with no block (traced:
// lead/p14d2-sizing.md). So every function and method declared in a
// package's _test.go files, and every package-level var of them (its whole
// value: a table of closures sums them), is compared by its own direct
// assertions, base against head:
//   - Its own direct assertions are the assertion calls in its body, read
//     from the syntax (direct: a failing method on an identifier bound to
//     a testing value, main's tval). A call to a helper, or testify's, is
//     none here, and main's asserting fixed point is never read: no
//     totals, no call graph.
//   - An assertion under a branch a constant condition never takes
//     (main's markDead, unchanged) is removed.
//   - A helper's live count falling blocks ("a helper's assertions
//     weakened"); a gain is clean. A function declared again (race,
//     !race) counts its weakest declaration.
//   - A test is main's to compare: here it blocks only when an assertion
//     of its own body is made dead (the weakest declaration's live count
//     fell, the most dead of any declaration rose).
//   - A helper with assertions at the base that the head no longer
//     declares blocks while its name is still used in the directory's test
//     files ("a helper with assertions removed while still used").
//   - Each test file's declarations are read in one pass, each node charged
//     to the case layer's bound (r.caseWork) before it is read; past it, or
//     cancelled, the file is unverifiable.

// fnStat is one declaration's own direct assertions on one platform: live,
// and dead (under branches never taken); the name a use of it spells (a
// method's, without its receiver); and whether it is a test.
type fnStat struct {
	live, dead  int
	test        bool
	plat, short string
}

// helperFacts records d, a declaration of a test file, on platform plat:
// each function's own direct assertions, each package var's (its whole
// value), and every name d uses.
func (s *pkgScan) helperFacts(fa *pkgFacts, plat, dir, name, path string, d ast.Decl) {
	if s.cs.dead == nil {
		s.cs.dead = map[ast.Node]bool{}
	}
	key := func(short string) string { return plat + " " + dir + ":" + name + "." + short }
	var walk func(root ast.Node, dead bool, st *fnStat)
	walk = func(root ast.Node, dead bool, st *fnStat) {
		ast.Inspect(root, func(n ast.Node) bool {
			switch {
			case n == nil || !s.caseCharge(1): // each node charged before it is read
				return false
			case n != root && s.cs.dead[n]: // a branch never taken: its assertions dead
				walk(n, true, st)
				return false
			}
			s.markDead(n)
			switch x := n.(type) {
			case *ast.Ident:
				if s.info.Defs[x] == nil {
					fa.names[plat+" "+x.Name] = true
				}
			case *ast.CallExpr:
				if s.direct(x) {
					st.live, st.dead = st.live+b2i(!dead), st.dead+b2i(dead)
				}
			}
			return true
		})
	}
	stats := map[string]fnStat{}
	switch x := d.(type) {
	case *ast.FuncDecl:
		st := fnStat{plat: plat, short: x.Name.Name, test: x.Recv == nil && goTest(x.Name.Name)}
		walk(x, false, &st)
		if x.Body != nil {
			stats[key(declKey(x))] = st
		}
	case *ast.GenDecl:
		for _, sp := range x.Specs {
			vs, ok := sp.(*ast.ValueSpec)
			if !ok {
				continue
			}
			sts := make([]fnStat, len(vs.Values))
			for i, v := range vs.Values {
				walk(v, false, &sts[i])
			}
			for i, id := range vs.Names {
				if len(sts) > 0 && id.Name != "_" { // var a, b = f(): each name the one value's
					st := sts[min(i, len(sts)-1)]
					st.plat, st.short = plat, id.Name
					stats[key(id.Name)] = st
				}
			}
		}
	}
	if s.over() {
		if fa.bad[path] == "" { // another layer's reason kept
			fa.bad[path] = "test helpers' assertions past the guard's work bound"
		}
		return
	}
	for _, k := range keys(stats) {
		fa.helpers[k] = append(fa.helpers[k], stats[k])
	}
}

// liveDead is a function's live assertions, its weakest declaration's,
// and its dead ones, its most's (race, !race).
func liveDead(sts []fnStat) (live, dead int) {
	live = sts[0].live
	for _, st := range sts {
		live, dead = min(live, st.live), max(dead, st.dead)
	}
	return live, dead
}

// compareHelpers blocks a function of the test files whose own live
// assertions the head reduced, or that the head removed while its name is
// still used; a test whose own assertions the head made dead.
func compareHelpers(add func(*[]Block, string, string), blocks *[]Block, b, a pkgFacts) {
	for _, k := range keys(b.helpers) {
		bs := b.helpers[k]
		bl, bd := liveDead(bs)
		as, ok := a.helpers[k]
		switch {
		case !ok:
			if !bs[0].test && bl > 0 && a.names[bs[0].plat+" "+bs[0].short] {
				add(blocks, k, "a helper with assertions removed while still used")
			}
		case bs[0].test:
			if al, ad := liveDead(as); ad > bd && al < bl {
				add(blocks, k, fmt.Sprintf("a test's assertions made dead (%d → %d live)", bl, al))
			}
		default:
			if al, _ := liveDead(as); al < bl {
				add(blocks, k, fmt.Sprintf("a helper's assertions weakened (%d → %d)", bl, al))
			}
		}
	}
}
