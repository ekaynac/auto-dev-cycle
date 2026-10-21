// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"go/ast"
	"go/types"
	"slices"
	"strings"
	"sync/atomic"
)

// P14d-a1 (#226 entries 74, 95, 99, 112, 115, 116; Astra #255 H6, Opus #255
// M5; the reviews of #292, #315, #350 and #354), on the package scan of
// each version and platform: direct forms only.
//   - A package function or method is skipping when any of its
//     declarations (race, !race: Opus #292 M3) calls Skip, SkipNow or Skipf
//     on one of its own parameters that may be of a testing type. A later
//     assignment of the parameter does not hide it (Astra #315 H1:
//     t.Skip(…); t = nil); any other variable (a copy, a nested literal's
//     own parameter) is none.
//   - Its type is classified conservatively, never resolved exactly (#226
//     entries 115, 116): parentheses and one * stripped, then testing
//     (testing.T, B, F or TB), provably none (a builtin, a composite type
//     literal, the package's own defined type), the package's own
//     non-generic alias followed, or else possibly testing (any imported
//     type but testing's, the standard library's included; a generic
//     alias's instantiation; a type parameter; anything unresolved), whose
//     skips count: a false block, stated, when such a type's Skip is not
//     testing's (an xml.Decoder's). No module metadata is read.
//   - Any use of one in a test is a propagated skip: a call whatever its
//     arguments, or a value (a slice, a field, a runner's argument, a local
//     alias). Propagated skips are compared apart from main's direct ones
//     (Astra #292 r2 1): one fewer never offsets a new direct skip.
//   - Each body is read once a scan and each alias followed once, charged
//     to the skip layer's own bound (maxSkipWork, never the run's maxWork:
//     Opus #315 H1); the reading checks for cancellation as it goes. Past
//     the bound, every test file of the package is unverifiable.
//   - Not followed here (P14d-a2): a copy of a testing parameter, package
//     variables holding a helper, containers, tuple assignments, held
//     results, a helper calling a helper, an interface's method, and
//     helpers of other packages (the gate's go_test_json adapter refuses a
//     skip at head).

// maxSkipWork bounds the skip layer's work in a run, its own: the bytes it
// reads and each type it classifies. A body is charged in every scan that
// reads it: each touched directory, both versions, both platforms. maxRead
// charges a blob once, so identical files repeated across directories can
// reach this bound within maxRead (Astra #350 M3): four identical scans,
// body only, reach it at 128 MiB of such bodies counted once per directory.
// A var: tests lower it.
var maxSkipWork = 4 * len(platforms) * int(maxRead)

// skipReads counts the bodies the skip layer reads, and skipCharged its
// work in all (tests read them: counts, never a clock).
var skipReads, skipCharged atomic.Int64

// skipCheckEvery: the reading checks for cancellation every this many nodes.
const skipCheckEvery = 256

// skipUse: id names a skipping package function or method.
func (s *pkgScan) skipUse(id *ast.Ident) bool {
	if len(s.skipping) == 0 { // none in the package (or not yet read): no lookup
		return false
	}
	o := s.info.Uses[id]
	if f, ok := o.(*types.Func); ok {
		return s.skipping[s.decls[f.Origin()]]
	}
	// Unbound in this import-free pass (h[time.Time]{}.need: the receiver's
	// type is not resolved), or bound to an invalid type: by name, any
	// skipping helper's (Astra #356 H1; a false block, stated, when the name
	// is another's).
	return (o == nil && s.info.Defs[id] == nil || o != nil && o.Type() == types.Typ[types.Invalid]) && s.skipNames[id.Name]
}

// skipFacts reads, once and charged, the body of each declaration with a
// testing parameter: one that skips directly makes every declaration of its
// name skipping. Past the bound it stops (skipBad: the package's tests
// unverifiable); a done context is the run's error.
func (s *pkgScan) skipFacts() error {
	for _, ds := range s.same {
		for _, d := range ds {
			if d.Body == nil || s.skipping[d] {
				continue
			}
			params := s.testingParams(d)
			if s.skipErr != nil {
				return s.skipErr
			} else if s.skipBad != "" {
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
					s.skipping[e], s.skipNames[e.Name.Name] = true, true
				}
			}
		}
	}
	return nil
}

// skipStep counts a classification step, and checks the run's context
// every skipCheckEvery: false once it is done (skipErr, the run's error).
func (s *pkgScan) skipStep() bool {
	if s.skipSteps++; s.skipErr == nil && s.skipSteps%skipCheckEvery == 1 {
		s.skipErr = s.r.ctx.Err()
	}
	return s.skipErr == nil
}

// skipCharge charges n to the skip layer's bound; past it, the package's
// tests are unverifiable.
func (s *pkgScan) skipCharge(n int) bool {
	if skipCharged.Add(int64(n)); s.skipBad != "" {
		return false
	} else if s.r.skipWork += n; s.r.skipWork > maxSkipWork {
		s.skipBad = "skipping helpers past the guard's bound"
		return false
	}
	return true
}

// testingParams is d's parameters that may be of a testing type
// (possiblyTesting), by the declaration's syntax.
func (s *pkgScan) testingParams(d *ast.FuncDecl) map[types.Object]bool {
	params := map[types.Object]bool{}
	for _, fl := range d.Type.Params.List {
		if !s.skipCharge(len(fl.Names)) || !s.skipStep() { // each name charged before it is read (Astra #356 M2)
			return nil
		} else if s.possiblyTesting(fl.Type) {
			for _, id := range fl.Names {
				if o := s.info.Defs[id]; o != nil {
					params[o] = true
				}
			}
		}
	}
	return params
}

// possiblyTesting classifies t, a parameter's declared type (#226 entry
// 115): parentheses and one * stripped, a testing type (testing.T, B, F,
// TB) is one; provably none are a builtin, a composite type literal and
// the package's own defined type (an instantiation of one too); any other
// imported type is possibly testing; the package's own non-generic alias is followed
// (memoized per alias for the scan, charged once; a cycle is possibly
// testing); anything else is possibly testing. Each call is charged one.
func (s *pkgScan) possiblyTesting(t ast.Expr) bool {
	if !s.skipCharge(1) || !s.skipStep() {
		return false // the bound (skipBad) or a done context (skipErr): the package's facts unknown
	}
	t = ast.Unparen(t)
	if st, ok := t.(*ast.StarExpr); ok {
		t = ast.Unparen(st.X)
	}
	switch x := t.(type) {
	case *ast.ArrayType, *ast.MapType, *ast.FuncType, *ast.ChanType, *ast.StructType, *ast.InterfaceType:
		return false
	case *ast.SelectorExpr:
		q := s.qual(x)
		if q == "" {
			return true // unresolved
		}
		path := q[:strings.LastIndex(q, ".")]
		return path != "testing" || slices.Contains(testTypes[:4], q) // any other import's type possibly testing
	case *ast.Ident:
		o := s.info.Uses[x]
		if o == nil && s.pkg != nil { // a type error left it unrecorded: by name, in the package
			o = s.pkg.Scope().Lookup(x.Name)
		}
		tn, ok := o.(*types.TypeName)
		switch {
		case !ok:
			return true // unresolved
		case tn.Parent() == types.Universe:
			return false // a builtin
		case s.aliases[tn] != nil:
			return s.aliasTesting(tn)
		case tn.Pkg() != s.pkg: // a dot import
			return tn.Pkg() == nil || tn.Pkg().Path() != "testing" || slices.Contains(testTypes[:4], "testing."+tn.Name())
		}
		_, param := tn.Type().(*types.TypeParam)
		return param || tn.IsAlias() // a type parameter; an alias the scan did not record
	case *ast.IndexExpr, *ast.IndexListExpr: // an instantiation: of the package's own defined type, none; else (a generic alias) possibly
		f, _ := fn(x)
		id, _ := f.(*ast.Ident)
		if id == nil {
			return true
		}
		tn, ok := s.info.Uses[id].(*types.TypeName)
		return !ok || tn.Pkg() != s.pkg || tn.IsAlias() || s.aliases[tn] != nil
	}
	return true
}

// aliasTesting is possiblyTesting of the package's own alias tn's
// right-hand side (a generic alias is only ever instantiated: possibly
// testing, above); a cycle is possibly testing; memoized for the scan
// (each alias followed once).
func (s *pkgScan) aliasTesting(tn *types.TypeName) bool {
	if v, ok := s.aliasMemo[tn]; ok {
		return v != 2 // 1 testing, 2 not, 3 being followed: a cycle, possibly
	}
	s.aliasMemo[tn] = 3
	v := s.possiblyTesting(s.aliases[tn])
	s.aliasMemo[tn] = 2 - int8(b2i(v))
	return v
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
