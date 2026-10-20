// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"go/ast"
	"go/types"
	"maps"
	"slices"
	"sync/atomic"
)

// P14d-a (#226 entries 74, 95, 99; Astra #255 H6, Opus #255 M5; #292's
// reviews), on the package scan of each version and platform:
//   - A package function is skipping when any of its declarations (race,
//     !race) skips a test: Skip, SkipNow or Skipf of a testing.T, B, F or
//     TB (by its type; by the scan's testing values only where the type is
//     unknown, never over a known other type: xml.Decoder.Skip), or a use
//     of another skipping function or variable. A package variable is
//     skipping when it holds a skipping function or method (var f =
//     requireDocker; var g = h{}.require), a literal that skips, or another
//     skipping variable (var second = first; late = second in init).
//   - Any use of one in a test is a propagated skip: a call whatever its
//     arguments, or a value (a slice, a field, a runner's argument, a local
//     alias); and so is a call of an interface method a skipping package
//     method's name. Propagated skips are compared apart from main's direct
//     ones: one fewer never offsets a new direct skip.
//   - The facts are a fixed point, one hop an iteration from the last one's
//     facts (a deterministic count), each body's reading charged to the work
//     bound, which is checked after each charge and between summaries.
//     Still changing at 32 iterations, or past the bound, the scan is
//     exhausted and every test of the package unverifiable: never a chain
//     left uncounted.
//   - Residual: helpers of other packages receiving the testing value (the
//     gate's go_test_json adapter refuses a skip at head).

// skipReads counts skipsIn's readings (tests read it: a bound, never a clock).
var skipReads atomic.Int64

// skipUse: id names a skipping function or method, a skipping variable,
// or an interface's method a skipping package method's name.
func (s *pkgScan) skipUse(id *ast.Ident) bool {
	if len(s.skipping)+len(s.skipVars) == 0 { // none in the package (or not yet computed): no lookup
		return false
	}
	switch o := s.info.Uses[id].(type) {
	case *types.Func:
		sig, _ := o.Type().(*types.Signature)
		return s.skipping[s.decls[o.Origin()]] || sig != nil && sig.Recv() != nil && types.IsInterface(sig.Recv().Type()) && s.skipNames[o.Name()]
	case *types.Var:
		return s.varSkips(o)
	}
	return false
}

// varSkips: variable o holds a skipping function, a literal that skips, or
// a skipping variable.
func (s *pkgScan) varSkips(o types.Object) bool {
	return s.skipVars[o] || slices.ContainsFunc(s.funcVals[o], func(f types.Object) bool { return s.skipping[s.decls[f]] })
}

// assign records that package variable o is assigned v: a package function
// or method (funcVals), another variable (varSrcs), or a literal (litVars).
func (s *pkgScan) assign(o types.Object, v ast.Expr) {
	if o == nil {
		return
	}
	if f := s.funcOf(v); f != nil && !slices.Contains(s.funcVals[o], f) {
		s.funcVals[o] = append(s.funcVals[o], f)
	}
	if src, ok := s.info.Uses[ident(v)].(*types.Var); ok && types.Object(src) != o && !slices.Contains(s.varSrcs[o], types.Object(src)) {
		s.varSrcs[o] = append(s.varSrcs[o], src)
	}
	if fl, ok := ast.Unparen(v).(*ast.FuncLit); ok {
		s.litVars[o] = append(s.litVars[o], fl)
	}
}

// funcOf is the package function or method e names, if one (f, h.m, f[int]).
func (s *pkgScan) funcOf(e ast.Expr) types.Object {
	f, _ := fn(e)
	id := ident(f)
	if sel, ok := f.(*ast.SelectorExpr); ok {
		id = sel.Sel
	}
	if o, ok := s.info.Uses[id].(*types.Func); ok {
		return o.Origin()
	}
	return nil
}

// skipsIn: body skips a test (above), its reading charged; past the work
// bound, false at once.
func (s *pkgScan) skipsIn(body ast.Node) (found bool) {
	skipReads.Add(1)
	if s.r.work += int(body.End() - body.Pos()); s.r.work > maxWork {
		return false
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if !slices.Contains(goSkips, x.Sel.Name) {
				break
			}
			if t := s.info.TypeOf(x.X); t != nil && t != types.Typ[types.Invalid] {
				if p, ok := t.(*types.Pointer); ok {
					t = p.Elem()
				}
				nt, _ := types.Unalias(t).(*types.Named)
				found = nt != nil && nt.Obj().Pkg() != nil && nt.Obj().Pkg().Path() == "testing" && slices.Contains([]string{"T", "B", "F", "TB"}, nt.Obj().Name())
			} else {
				found = s.direct(x.X) // no type (the scan resolves no import): a testing parameter, or a copy of one
			}
		case *ast.Ident:
			found = s.skipUse(x)
		}
		return !found
	})
	return found
}

// direct: e is a testing parameter, or a variable every assignment of
// which is a copy of one (u := t; var u testing.TB = t): never a value a
// call returned, though it mentions one (d := xml.NewDecoder(…t.Name()…):
// Astra #292 r2 4).
func (s *pkgScan) direct(e ast.Expr) bool {
	o := s.varOf(e)
	return o != nil && s.directObj(o)
}

// directObj is direct for a variable.
func (s *pkgScan) directObj(o types.Object) bool {
	if !slices.Contains([]string{"testing.T", "testing.B", "testing.F", "testing.TB"}, s.tvals[o]) || s.indirect[o] {
		return false
	} else if s.directs[o] == 0 {
		s.directs[o] = 1
		s.directs[o] = 2 + int8(b2i(!slices.ContainsFunc(slices.Collect(maps.Keys(s.copies[o])), func(src types.Object) bool { return !s.directObj(src) })))
	}
	return s.directs[o] == 3
}

// varOf is the variable e names, through &, * and parentheses.
func (s *pkgScan) varOf(e ast.Expr) types.Object {
	for {
		switch x := ast.Unparen(e).(type) {
		case *ast.UnaryExpr:
			e = x.X
			continue
		case *ast.StarExpr:
			e = x.X
			continue
		}
		break
	}
	if o, ok := s.info.Uses[ident(e)].(*types.Var); ok {
		return o
	}
	return nil
}

// set records that variable o is assigned v: a copy of another variable,
// or anything else (direct).
func (s *pkgScan) set(o types.Object, v ast.Expr) {
	if src := s.varOf(v); o != nil && src != nil {
		if s.copies[o] == nil {
			s.copies[o] = map[types.Object]bool{}
		}
		s.copies[o][src] = true
	} else if o != nil {
		s.indirect[o] = true
	}
}

// skipFacts computes the skipping functions and variables (above). An
// exhaustion another layer caused is that layer's to report.
func (s *pkgScan) skipFacts() error {
	if s.r.work > maxWork {
		return nil
	}
	changed := true
	for i := 0; changed && i < 32 && s.r.work <= maxWork; i++ {
		if err := s.r.ctx.Err(); err != nil {
			return err
		}
		var decls [][]*ast.FuncDecl
		var vars []types.Object
		for _, ds := range s.same {
			if !s.skipping[ds[0]] && slices.ContainsFunc(ds, func(d *ast.FuncDecl) bool { return s.r.work <= maxWork && d.Body != nil && s.skipsIn(d.Body) }) {
				decls = append(decls, ds)
			}
		}
		for o, fls := range s.litVars {
			if !s.skipVars[o] && slices.ContainsFunc(fls, func(fl *ast.FuncLit) bool { return s.r.work <= maxWork && s.skipsIn(fl.Body) }) {
				vars = append(vars, o)
			}
		}
		for o, srcs := range s.varSrcs {
			if !s.skipVars[o] && slices.ContainsFunc(srcs, s.varSkips) {
				vars = append(vars, o)
			}
		}
		for _, ds := range decls {
			for _, d := range ds {
				s.skipping[d] = true
			}
			s.skipNames[ds[0].Name.Name] = s.skipNames[ds[0].Name.Name] || ds[0].Recv != nil
		}
		for _, o := range vars {
			s.skipVars[o] = true
		}
		changed = len(decls)+len(vars) > 0
	}
	s.exhausted = changed || s.r.work > maxWork
	return nil
}
