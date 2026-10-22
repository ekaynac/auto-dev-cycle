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
//   - A package's blank initializers (var _ = …) are one entry, their sum
//     (Astra #384 H1): a fall blocks ("blank package initializers'
//     assertions fell").
//   - Each test file's declarations are read in one pass, each node charged
//     to the case layer's bound (r.caseWork) before it is read; past it, or
//     cancelled, the file is unverifiable.
//
// Residuals (P14d-w2): a helper renamed while weakened; assertions on a
// field or an embedded testing value, or on a testing value of a type
// parameter ([T testing.TB]); assertions under a default clause never
// selected; totals across calls; testify's. False blocks (main accepts
// them): a body extracted into a new helper; an unused helper weakened; a
// blank initializer moved to another package; a clause reached by
// fallthrough from any reachable predecessor, read as dead by main's
// markDead; a helper moved whole, under its name, into a package
// (non-test) file, or between pkg and its external pkg_test; a value
// derived from t (main's tval) whose Error method call is removed.

// fnStat is one declaration's own direct assertions on one platform: live,
// and dead (under branches never taken); the name a use of it spells (a
// method's, without its receiver); and whether it is a test.
type fnStat struct {
	live, dead  int
	test        bool
	plat, short string
}

// fnStats is a package and platform's functions, by short identity, each
// one's declarations; nameSet, the names a platform's test files use.
type (
	fnStats map[string][]fnStat
	nameSet map[string]bool
)

// blankKey is a package's blank initializers (var _ = …), one entry, their
// sum (Astra #384 H1). A space: no identifier spells it.
const blankKey = "var _"

// helperFacts records d, a declaration of a test file, on platform plat:
// each function's own direct assertions, each package var's (its whole
// value), the blank initializers' sum, and every name d uses. The
// package and platform's namespace is built once a scan and charged; a
// declaration is keyed in it by its short identity (Astra #384 M2).
func (s *pkgScan) helperFacts(fa *pkgFacts, plat, dir, name, path string, d ast.Decl) {
	if s.cs.dead == nil {
		s.cs.dead = map[ast.Node]bool{}
	}
	if s.helperNS == "" {
		s.helperNS = plat + " " + dir + ":" + name
		s.caseCharge(len(s.helperNS))
	}
	if fa.helpers[s.helperNS] == nil {
		fa.helpers[s.helperNS] = fnStats{}
	}
	if fa.names[plat] == nil {
		fa.names[plat] = nameSet{}
	}
	fns, names := fa.helpers[s.helperNS], fa.names[plat]
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
					names[x.Name] = true
				}
			case *ast.CallExpr:
				if s.direct(x) {
					st.live, st.dead = st.live+b2i(!dead), st.dead+b2i(dead)
				}
			}
			return true
		})
	}
	stats, blank := map[string]fnStat{}, fnStat{plat: plat, short: "_"}
	switch x := d.(type) {
	case *ast.FuncDecl:
		st := fnStat{plat: plat, short: x.Name.Name, test: x.Recv == nil && goTest(x.Name.Name)}
		walk(x, false, &st)
		if short := declKey(x); x.Body != nil && s.caseCharge(len(short)) {
			stats[short] = st
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
				st := fnStat{}
				if len(sts) > 0 { // var a, b = f(): each name the one value's
					st = sts[min(i, len(sts)-1)]
				}
				switch {
				case len(sts) == 0:
				case id.Name == "_":
					blank.live, blank.dead = blank.live+st.live, blank.dead+st.dead
				default:
					st.plat, st.short = plat, id.Name
					stats[id.Name] = st
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
		fns[k] = append(fns[k], stats[k])
	}
	if blank.live+blank.dead > 0 { // summed over the package's files on plat
		if fns[blankKey] == nil {
			fns[blankKey] = []fnStat{{plat: plat, short: "_"}}
		}
		fns[blankKey][0].live += blank.live
		fns[blankKey][0].dead += blank.dead
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
// still used; a test whose own assertions the head made dead; a package's
// blank initializers whose live assertions fell. Each entry, and each
// finding's bytes, are charged to the case layer's bound; past it, or
// cancelled, the namespace is unverifiable.
func (r *run) compareHelpers(add func(*[]Block, string, string), b, a pkgFacts) {
	charge := func(n int) bool {
		r.caseWork += n
		caseCharged.Add(int64(n))
		return r.caseWork <= maxCaseWork && r.ctx.Err() == nil
	}
	for _, ns := range keys(b.helpers) {
		bh, ah := b.helpers[ns], a.helpers[ns]
		for _, short := range keys(bh) {
			if msg := helperFinding(short, bh[short], ah, a.names); !charge(1) || msg != "" && !charge(len(ns)+len(short)+len(msg)) {
				add(&r.blocks, ns, "unverifiable: test helpers' findings past the guard's work bound")
				return
			} else if msg != "" {
				add(&r.blocks, ns+"."+short, msg)
			}
		}
	}
}

// helperFinding is what bs, a function's base declarations, show against
// the head's ah ("": nothing).
func helperFinding(short string, bs []fnStat, ah fnStats, names map[string]nameSet) string {
	bl, bd := liveDead(bs)
	as, ok := ah[short]
	switch {
	case short == blankKey: // all of them gone: none
		al := 0
		if ok {
			al, _ = liveDead(as)
		}
		if al < bl {
			return fmt.Sprintf("blank package initializers' assertions fell (%d → %d)", bl, al)
		}
	case !ok:
		if !bs[0].test && bl > 0 && names[bs[0].plat][bs[0].short] {
			return "a helper with assertions removed while still used"
		}
	case bs[0].test:
		if al, ad := liveDead(as); ad > bd && al < bl {
			return fmt.Sprintf("a test's assertions made dead (%d → %d live)", bl, al)
		}
	default:
		if al, _ := liveDead(as); al < bl {
			return fmt.Sprintf("a helper's assertions weakened (%d → %d)", bl, al)
		}
	}
	return ""
}
