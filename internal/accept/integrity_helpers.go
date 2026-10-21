// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"fmt"
	"go/ast"
	"slices"
)

// P14d-w1 (#226 entries 122, 129; supersedes #369): assertion weakening in
// test helpers. Main counts a test's assertions, an asserting helper's call
// as one whatever it holds; a helper or a named t.Run callback could lose
// assertions with no block (traced: lead/p14d2-sizing.md). So every
// function and method declared in a package's _test.go files, and every
// function literal a package-level var of them holds, is compared by its
// own direct assertions, base against head:
//   - Its own direct assertions are the assertion calls in its body, read
//     from the syntax (direct: a testing value's failing method, or
//     testify's). A call to a helper is none here, and main's
//     asserting fixed point is never read: no totals, no call graph.
//   - An assertion under a branch a constant condition never takes
//     (markDead's: if, for, a switch clause, a default another clause must
//     match) is removed. A clause fallen into is kept live, and warns.
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
// and dead (under branches never taken); fell, a clause never selected but
// fallen into, kept live; the name a use of it spells (a method's, without
// its receiver); and whether it is a test.
type fnStat struct {
	live, dead  int
	fell, test  bool
	plat, short string
}

// helperFacts records d, a declaration of a test file, on platform plat:
// each function's own direct assertions, and every name d uses.
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
	type found struct {
		k  string
		st *fnStat
	}
	var fns []found
	fell := s.cs.fell
	switch x := d.(type) {
	case *ast.FuncDecl:
		st := &fnStat{plat: plat, short: x.Name.Name, test: x.Recv == nil && goTest(x.Name.Name)}
		walk(x, false, st)
		if x.Body != nil {
			st.fell = s.cs.fell > fell
			fns = append(fns, found{key(declKey(x)), st})
		}
	case *ast.GenDecl:
		for _, sp := range x.Specs {
			vs, ok := sp.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, v := range vs.Values {
				fell, st := s.cs.fell, &fnStat{plat: plat}
				walk(v, false, st)
				if _, lit := ast.Unparen(v).(*ast.FuncLit); lit && i < len(vs.Names) && vs.Names[i].Name != "_" {
					st.short, st.fell = vs.Names[i].Name, s.cs.fell > fell
					fns = append(fns, found{key(st.short), st})
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
	for _, f := range fns {
		fa.helpers[f.k] = append(fa.helpers[f.k], *f.st)
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
// still used; a test whose own assertions the head made dead. A clause
// kept live because it is fallen into warns.
func compareHelpers(add func(*[]Block, string, string), blocks, warnings *[]Block, b, a pkgFacts) {
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
	fell := func(sts []fnStat) bool { return slices.ContainsFunc(sts, func(st fnStat) bool { return st.fell }) }
	for _, k := range keys(a.helpers) {
		if fell(a.helpers[k]) && !fell(b.helpers[k]) {
			add(warnings, k, "a switch clause never selected but fallen into, kept live: review it")
		}
	}
}
