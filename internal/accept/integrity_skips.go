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
// name skipping. Past the bound it stops; a parameter's type it cannot
// resolve makes the package's facts unknown (skipBad: its tests
// unverifiable, never "not testing"); a done context is the run's error.
func (s *pkgScan) skipFacts() error {
	for _, ds := range s.same {
		for _, d := range ds {
			if d.Body == nil || s.skipping[d] {
				continue
			}
			params, why := s.testingParams(d)
			if why != "" {
				s.skipBad = why
				return nil
			} else if len(params) == 0 {
				continue
			} else if !s.skipCharge(int(d.Body.End() - d.Body.Pos())) {
				return nil
			}
			skips, err := s.skipsDirectly(d, params)
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

// skipCharge charges n to the skip layer's bound; past it, the package's
// tests are unverifiable.
func (s *pkgScan) skipCharge(n int) bool {
	if s.r.skipWork += n; s.r.skipWork > maxSkipWork {
		s.skipBad = "skipping helpers past the guard's bound"
		return false
	}
	return true
}

// testingParams is d's parameters declared of a testing type (*testing.T,
// B, F or TB), by the declaration's syntax; why, if one's type is not
// resolved.
func (s *pkgScan) testingParams(d *ast.FuncDecl) (params map[types.Object]bool, why string) {
	params = map[types.Object]bool{}
	for _, fl := range d.Type.Params.List {
		q, why := s.testingType(fl.Type)
		if why != "" {
			return nil, why
		} else if q != "" {
			for _, id := range fl.Names {
				if o := s.info.Defs[id]; o != nil {
					params[o] = true
				}
			}
		}
	}
	return params, ""
}

// binding is a generic alias's type parameter bound to its argument, read
// in the bindings of the instantiation (env).
type binding struct {
	arg ast.Expr
	env map[types.Object]binding
}

// testingType is the testing type ("testing.T", B, F or TB) t names,
// through parentheses, pointers and the package's own type aliases
// resolved to their end (Opus #350 M1, Astra #350 H1): a generic alias
// instantiated (type Ptr[P any] = *P; Ptr[testing.T]) with its arguments
// substituted by position. Each step is charged to the skip bound. why is
// set, never "not testing", when an alias is not resolved: a cycle, the
// bound, or an instantiation whose arguments do not match.
func (s *pkgScan) testingType(t ast.Expr) (q, why string) {
	env, seen := map[types.Object]binding{}, map[[2]any]bool{}
	for {
		if !s.skipCharge(1) {
			return "", s.skipBad
		}
		t = ast.Unparen(t)
		if st, ok := t.(*ast.StarExpr); ok {
			t = st.X
			continue
		}
		if id, ok := t.(*ast.Ident); ok {
			if b, ok := env[s.info.Uses[id]]; ok { // a type parameter of the alias being read
				t, env = b.arg, b.env
				continue
			}
		}
		f, args := fn(t)
		o := s.info.Uses[ident(f)]
		if id := ident(f); o == nil && id != nil && s.pkg != nil { // a type error left it unrecorded: by name, in the package
			o = s.pkg.Scope().Lookup(id.Name)
		}
		rhs := s.aliases[o]
		if o == nil || rhs == nil {
			if q := s.qual(t); slices.Contains(testTypes[:4], q) {
				return q, ""
			}
			return "", ""
		}
		key := [2]any{o, nil}
		if len(args) > 0 {
			key[1] = args[0]
		}
		if seen[key] {
			return "", "unverifiable: a testing parameter's type alias not resolved (a cycle: " + o.Name() + ")"
		}
		seen[key] = true
		a, _ := o.Type().(*types.Alias)
		if n := 0; a != nil {
			n = a.TypeParams().Len()
			if n != len(args) {
				return "", "unverifiable: a testing parameter's type alias not resolved (" + o.Name() + "'s arguments)"
			}
			inner := map[types.Object]binding{}
			for i := range n {
				inner[a.TypeParams().At(i).Obj()] = binding{args[i], env}
			}
			t, env = rhs, inner
		} else if len(args) > 0 {
			return "", "unverifiable: a testing parameter's type alias not resolved (" + o.Name() + ")"
		} else {
			t, env = rhs, map[types.Object]binding{}
		}
	}
}

// skipsDirectly: d's body calls Skip, SkipNow or Skipf on one of d's own
// parameters declared of a testing type. The reading checks the run's
// context every skipCheckEvery nodes.
func (s *pkgScan) skipsDirectly(d *ast.FuncDecl, params map[types.Object]bool) (found bool, err error) {
	skipReads.Add(1)
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
