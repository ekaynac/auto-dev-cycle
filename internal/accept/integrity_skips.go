// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"go/ast"
	"go/types"
	"slices"
	"sync/atomic"
)

// P14d-a1 (#226 entries 74, 95, 99, 112; Astra #255 H6, Opus #255 M5; the
// reviews of #292 and #315), on the package scan of each version and
// platform: direct forms only.
//   - A package function or method is skipping when any of its
//     declarations (race, !race: Opus #292 M3) calls Skip, SkipNow or Skipf
//     on one of its own parameters declared of a testing type (*testing.T,
//     B, F or TB), by the declaration's syntax. A later assignment of the
//     parameter does not hide it (Astra #315 H1: t.Skip(…); t = nil); a
//     value of another type (an xml.Decoder's Skip) or any other variable
//     (a copy, a nested literal's own parameter) is none.
//   - Any use of one in a test is a propagated skip: a call whatever its
//     arguments, or a value (a slice, a field, a runner's argument, a local
//     alias). Propagated skips are compared apart from main's direct ones
//     (Astra #292 r2 1): one fewer never offsets a new direct skip.
//   - Each body is read once a scan, charged to the skip layer's own bound
//     (maxSkipWork, never the run's maxWork: Opus #315 H1), and the reading
//     checks for cancellation as it goes. Past the bound, every test file of
//     the package is unverifiable.
//   - Not followed here (P14d-a2): a copy of a testing parameter, package
//     variables holding a helper, aliases, containers, tuple assignments,
//     held results, a helper calling a helper, an interface's method, and
//     helpers of other packages (the gate's go_test_json adapter refuses a
//     skip at head).

// maxSkipWork bounds the bytes the skip layer reads in a run, its own. A
// body is read once a scan, and each platform scans both versions, a blob
// unchanged between them charged to maxRead once: a run reads at most
// 2 × len(platforms) × maxRead bytes; the bound is twice that, so maxRead
// binds first. A var: tests lower it.
var maxSkipWork = 4 * len(platforms) * int(maxRead)

// skipReads counts the bodies the skip layer reads (tests read it: a
// count, never a clock).
var skipReads atomic.Int64

// skipCheckEvery: the reading checks for cancellation every this many nodes.
const skipCheckEvery = 256

// skipUse: id names a skipping package function or method.
func (s *pkgScan) skipUse(id *ast.Ident) bool {
	if len(s.skipping) == 0 { // none in the package (or not yet read): no lookup
		return false
	}
	o, ok := s.info.Uses[id].(*types.Func)
	return ok && s.skipping[s.decls[o.Origin()]]
}

// skipFacts reads, once and charged, the body of each declaration with a
// testing parameter: one that skips directly makes every declaration of its
// name skipping. Past the bound it stops (exhausted); a done context is the
// run's error.
func (s *pkgScan) skipFacts() error {
	for _, ds := range s.same {
		for _, d := range ds {
			if d.Body == nil || s.skipping[d] || len(s.testingParams(d)) == 0 {
				continue
			} else if s.r.skipWork += int(d.Body.End() - d.Body.Pos()); s.r.skipWork > maxSkipWork {
				s.exhausted = true
				return nil
			}
			skips, err := s.skipsDirectly(d)
			if err != nil {
				return err
			} else if skips {
				for _, e := range ds {
					s.skipping[e] = true
				}
			}
		}
	}
	return nil
}

// testingParams is d's parameters declared of a testing type (*testing.T,
// B, F or TB), by the declaration's syntax: through parentheses, a pointer
// and the scan's own type aliases (type TT = testing.T), as mentions reads
// them (Opus #350 M1).
func (s *pkgScan) testingParams(d *ast.FuncDecl) map[types.Object]bool {
	params := map[types.Object]bool{}
	for _, fl := range d.Type.Params.List {
		t := ast.Unparen(fl.Type)
		for range 8 { // a bound: an alias of a pointer of an alias…
			if st, ok := t.(*ast.StarExpr); ok {
				t = ast.Unparen(st.X)
			} else if a := s.alias(t); a != nil {
				t = ast.Unparen(a)
			} else {
				break
			}
		}
		if slices.Contains(testTypes[:4], s.qual(t)) {
			for _, id := range fl.Names {
				if o := s.info.Defs[id]; o != nil {
					params[o] = true
				}
			}
		}
	}
	return params
}

// skipsDirectly: d's body calls Skip, SkipNow or Skipf on one of d's own
// parameters declared of a testing type. The reading checks the run's
// context every skipCheckEvery nodes.
func (s *pkgScan) skipsDirectly(d *ast.FuncDecl) (found bool, err error) {
	skipReads.Add(1)
	params := s.testingParams(d)
	nodes := 0
	ast.Inspect(d.Body, func(n ast.Node) bool {
		if found || err != nil {
			return false
		} else if nodes++; nodes%skipCheckEvery == 1 {
			if err = s.r.ctx.Err(); err != nil {
				return false
			}
		}
		if x, ok := n.(*ast.SelectorExpr); ok && slices.Contains(goSkips, x.Sel.Name) {
			recv := ast.Unparen(x.X)
			if st, ok := recv.(*ast.StarExpr); ok {
				recv = ast.Unparen(st.X)
			}
			id, _ := recv.(*ast.Ident)
			found = id != nil && params[s.info.Uses[id]]
		}
		return true
	})
	return found, err
}
