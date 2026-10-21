// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"
)

// P14d-w1 (#226 entries 122, 129; supersedes #369): assertion weakening in
// test helpers. Main counts a test's assertions, an asserting helper's call
// as one whatever it holds; a helper or a named t.Run callback could lose
// assertions with no block (traced: lead/p14d2-sizing.md). So every
// function and method declared in a package's _test.go files, and every
// package-level var of them (its whole value: a table of closures sums
// them), is compared by its own direct assertions, base against head:
//   - Its own direct assertions are the assertion calls in its body, read
//     from the syntax (direct: a testing value's failing method, or
//     testify's), and from the types a testing value's failing method
//     reached through a field or an embedding (typedFail, this layer's
//     only). A call to a helper is none here, and main's asserting fixed
//     point is never read: no totals, no call graph.
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
//     files ("a helper with assertions removed while still used"). One
//     whose name is gone blocks when the new functions (absent at the
//     base) that its referrers, still declared, refer to at the head hold
//     fewer live assertions together ("a helper renamed or replaced,
//     weaker"); with no such new function it is clean (inlined, deleted).
//   - Each test file's declarations are read in one pass, each node charged
//     to the case layer's bound (r.caseWork) before it is read; past it, or
//     cancelled, the file is unverifiable.

// fnStat is one declaration's own direct assertions on one platform: live,
// and dead (under branches never taken); fell, a clause never selected but
// fallen into, kept live; the name a use of it spells (a method's, without
// its receiver); whether it is a test; and the functions and package vars
// of the package it refers to (their keys, Opus #376 H1).
type fnStat struct {
	live, dead  int
	fell, test  bool
	plat, short string
	refs        []string
}

// helperFacts records d, a declaration of a test file, on platform plat:
// each function's own direct assertions, each package var's (its whole
// value, every function literal in it: Opus #376 M2), and every name d
// uses.
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
				switch o := s.info.Uses[x].(type) {
				case *types.Func:
					if fd := s.decls[o.Origin()]; fd != nil {
						st.refs = append(st.refs, key(declKey(fd)))
					}
				case *types.Var:
					if o.Parent() == s.pkg.Scope() {
						st.refs = append(st.refs, key(o.Name()))
					}
				}
			case *ast.CallExpr:
				if s.direct(x) || s.typedFail(x) {
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
			var sts []*fnStat
			for _, v := range vs.Values {
				fell, st := s.cs.fell, &fnStat{plat: plat}
				walk(v, false, st)
				st.fell = s.cs.fell > fell
				sts = append(sts, st)
			}
			if x.Tok != token.VAR || len(sts) == 0 {
				continue
			}
			for i, id := range vs.Names {
				st := sts[min(i, len(sts)-1)]
				if len(sts) != len(vs.Names) { // var a, b = f(): each name the whole spec
					st = &fnStat{plat: plat}
					for _, o := range sts {
						st.live, st.dead, st.fell, st.refs = st.live+o.live, st.dead+o.dead, st.fell || o.fell, append(st.refs, o.refs...)
					}
				}
				if id.Name != "_" {
					c := *st
					c.short = id.Name
					fns = append(fns, found{key(id.Name), &c})
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

// typedFail: call is a testing value's failing method reached through a
// field of a testing type (s.t.Fatal), or through one embedded in the
// receiver's struct (e.Fatal) when nothing else resolves it (Opus #376
// M1). The import-free pass types no testing value: the fields are read
// from the syntax (s.fieldT). Main's count is unchanged: this layer's only.
func (s *pkgScan) typedFail(call *ast.CallExpr) bool {
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok || !slices.Contains(goFails, sel.Sel.Name) {
		return false
	}
	if x, ok := ast.Unparen(sel.X).(*ast.SelectorExpr); ok && s.fieldT[s.info.Uses[x.Sel]] != "" {
		return true
	}
	t := s.info.TypeOf(sel.X)
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	st, ok := t.(*types.Named)
	if !ok || s.info.Selections[sel] != nil {
		return false
	}
	fs, ok := st.Underlying().(*types.Struct)
	for i := 0; ok && i < fs.NumFields(); i++ {
		if f := fs.Field(i); f.Embedded() && s.fieldT[f] != "" {
			return true
		}
	}
	return false
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
// still used, or renamed or replaced by weaker new functions; a test whose
// own assertions the head made dead. A clause kept live because it is
// fallen into warns.
func compareHelpers(add func(*[]Block, string, string), blocks, warnings *[]Block, b, a pkgFacts) {
	referrers := map[string][]string{} // the base's: who refers to each function
	for _, k := range keys(b.helpers) {
		for _, st := range b.helpers[k] {
			for _, c := range st.refs {
				if r := referrers[c]; len(r) == 0 || r[len(r)-1] != k {
					referrers[c] = append(r, k)
				}
			}
		}
	}
	for _, k := range keys(b.helpers) {
		bs := b.helpers[k]
		bl, bd := liveDead(bs)
		as, ok := a.helpers[k]
		switch {
		case !ok && (bs[0].test || bl == 0):
		case !ok && a.names[bs[0].plat+" "+bs[0].short]:
			add(blocks, k, "a helper with assertions removed while still used")
		case !ok:
			if n, news := replaced(referrers[k], b, a); news && n < bl {
				add(blocks, k, fmt.Sprintf("a helper renamed or replaced, weaker (%d → %d)", bl, n))
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

// replaced is the live assertions of the new functions (absent at the
// base) that a removed function's referrers, those still declared, refer
// to at the head, each counted once; news, whether there is any (Opus #376
// H1: a helper renamed while weakened).
func replaced(referrers []string, b, a pkgFacts) (n int, news bool) {
	seen := map[string]bool{}
	for _, r := range referrers {
		for _, st := range a.helpers[r] {
			for _, c := range st.refs {
				_, at := a.helpers[c]
				if _, before := b.helpers[c]; at && !before && !seen[c] {
					seen[c], news = true, true
					l, _ := liveDead(a.helpers[c])
					n += l
				}
			}
		}
	}
	return n, news
}
