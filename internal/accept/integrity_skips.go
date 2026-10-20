// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"go/ast"
	"go/types"
	"slices"
	"sync/atomic"
)

// P14d-a (#226 entries 74, 95, 99; Astra #255 H6, Opus #255 M5; #292's
// reviews; Opus #315), on the package scan of each version and platform:
//   - A package function is skipping when any of its declarations (race,
//     !race) skips a test directly, or uses a skipping function or
//     variable. A direct skip is Skip, SkipNow or Skipf of a testing.T, B,
//     F or TB: by its type when the scan knows it, never over a known other
//     type (xml.Decoder.Skip); where the type is unknown (the scan resolves
//     no import), of a testing parameter or a variable every assignment of
//     which is, through copies, one (u := t), never a value a call returned.
//   - A package variable is skipping when a value assigned to it, directly
//     or to a field or element of it (var f = requireDocker; var needs =
//     []func(*testing.T){requireDocker}; holder.f = requireDocker in init),
//     holds a direct skip or uses a skipping function or variable.
//   - Any use of one in a test is a propagated skip: a call whatever its
//     arguments, or a value (a slice, a field, a runner's argument, a local
//     alias); and so is a call of an interface method a skipping package
//     method's name. Propagated skips are compared apart from main's direct
//     ones: one fewer never offsets a new direct skip.
//   - Each body and value is read once a scan, charged to the skip layer's
//     own bound (maxSkipWork: it never spends the run's maxWork), and the
//     facts propagate over reverse references with a worklist (Opus #315
//     H1). Past the bound, every test of the package is unverifiable.
//   - Residual: helpers of other packages receiving the testing value (the
//     gate's go_test_json adapter refuses a skip at head); a testing value
//     reached through a call, a field, an element or a range variable
//     (wrap(t), e.t, range []*testing.T{t}) is no testing receiver where the
//     scan has no type for it.

// maxSkipWork bounds the skip layer's work in a run, its own: the bytes it
// reads and each step of its copy search. Every scan reads at most the
// bytes of its version's touched packages, so a run's reading stays under
// len(platforms) × maxRead; twice that again is headroom for the steps, and
// the read bound always binds first (Opus #315 r2 L2). A var: tests lower it.
var maxSkipWork = 4 * len(platforms) * int(maxRead)

// skipReads counts the skip layer's readings and skipCharged its work in
// all (tests read them: a bound and a count, never a clock).
var skipReads, skipCharged atomic.Int64

// charge adds n to the skip layer's work; false past its bound.
func (s *pkgScan) charge(n int) bool {
	skipCharged.Add(int64(n))
	s.r.skipWork += n
	return s.r.skipWork <= maxSkipWork
}

// skipUse: id names a skipping function or method, a skipping variable, or
// an interface's method a skipping package method's name.
func (s *pkgScan) skipUse(id *ast.Ident) bool {
	if len(s.skipping)+len(s.skipVars) == 0 { // none in the package (or not yet computed): no lookup
		return false
	}
	switch o := s.info.Uses[id].(type) {
	case *types.Func:
		sig, _ := o.Type().(*types.Signature)
		return s.skipping[s.decls[o.Origin()]] || sig != nil && sig.Recv() != nil && types.IsInterface(sig.Recv().Type()) && s.skipNames[o.Name()]
	case *types.Var:
		return s.skipVars[o]
	}
	return false
}

// held records that package variable o's root is assigned v (pass two).
func (s *pkgScan) held(lhs, v ast.Expr) {
	id := ident(s.root(lhs))
	if id == nil {
		return
	}
	if o, ok := cmpObj(s.info.Defs[id], s.info.Uses[id]).(*types.Var); ok && o.Parent() == s.pkg.Scope() {
		s.varVals[o] = append(s.varVals[o], v)
	}
}

func cmpObj(a, b types.Object) types.Object {
	if a != nil {
		return a
	}
	return b
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

// direct: e is a testing parameter, or a variable no copy chain of which
// reaches a variable assigned anything else or no testing value (u := t;
// a cycle of copies of one too: Opus #315 L1). A map lookup: notDirect is
// computed once a scan.
func (s *pkgScan) direct(e ast.Expr) bool {
	o := s.varOf(e)
	return o != nil && slices.Contains(testTypes[:4], s.tvals[o]) && !s.notDirect[o]
}

// notDirects computes the variables no direct receiver: those assigned
// anything else or no testing value, then, over the inverted copy edges in
// one search, every variable copying one (Opus #315 r2 H1). Each edge and
// step is charged; false past the bound.
func (s *pkgScan) notDirects() bool {
	copiedBy := map[types.Object][]types.Object{}
	var queue []types.Object
	mark := func(o types.Object) {
		if !s.notDirect[o] && (s.indirect[o] || !slices.Contains(testTypes[:4], s.tvals[o])) {
			s.notDirect[o], queue = true, append(queue, o)
		}
	}
	for o, srcs := range s.copies {
		if !s.charge(1 + len(srcs)) {
			return false
		}
		mark(o)
		for src := range srcs {
			copiedBy[src] = append(copiedBy[src], o)
			mark(src)
		}
	}
	for o := range s.indirect {
		mark(o)
	}
	for len(queue) > 0 {
		x := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if !s.charge(1 + len(copiedBy[x])) {
			return false
		}
		for _, o := range copiedBy[x] {
			if !s.notDirect[o] {
				s.notDirect[o], queue = true, append(queue, o)
			}
		}
	}
	return true
}

// bodyFacts is what one body or value holds: a direct skip, and the
// package functions, variables and interface method names it uses.
type bodyFacts struct {
	direct bool
	refs   []types.Object
	ifaces []string
}

// skipRead reads n once, charged; past the bound, nothing. A package
// variable's value (value) holds what it names, not what it calls: outside
// a function literal, a function only called there is not counted unless
// the call's result can hold a function (pick()), so shared =
// requireServer(t) holds a server, not a skip (Opus #315 r2 M1).
func (s *pkgScan) skipRead(n ast.Node, value bool) (r bodyFacts) {
	skipReads.Add(1)
	if !s.charge(int(n.End() - n.Pos())) {
		return r
	}
	var walk func(n ast.Node, value bool)
	walk = func(n ast.Node, value bool) {
		ast.Inspect(n, func(n ast.Node) bool { return s.skipVisit(n, value, &r, walk) })
	}
	walk(n, value)
	return r
}

// skipVisit is skipRead's visitor.
func (s *pkgScan) skipVisit(n ast.Node, value bool, r *bodyFacts, walk func(ast.Node, bool)) bool {
	if value {
		switch x := n.(type) {
		case *ast.FuncLit:
			walk(x.Body, false)
			return false
		case *ast.CallExpr:
			if t := s.info.TypeOf(x); t == nil || !plainData(t, map[types.Type]bool{}) {
				return true // the result can hold a function, or is unknown: all of it
			}
			callee := ast.Unparen(x.Fun)
			switch c := callee.(type) {
			case *ast.IndexExpr:
				callee = c.X
			case *ast.IndexListExpr:
				callee = c.X
			}
			switch c := ast.Unparen(callee).(type) {
			case *ast.Ident: // called only: not held
			case *ast.SelectorExpr:
				walk(c.X, true)
			default:
				walk(c, true)
			}
			for _, a := range x.Args {
				walk(a, true)
			}
			return false
		}
	}
	switch x := n.(type) {
	case *ast.SelectorExpr:
		if !slices.Contains(goSkips, x.Sel.Name) || r.direct {
			break
		}
		if t := s.info.TypeOf(x.X); t != nil && t != types.Typ[types.Invalid] {
			if p, ok := t.(*types.Pointer); ok {
				t = p.Elem()
			}
			nt, _ := types.Unalias(t).(*types.Named)
			r.direct = nt != nil && nt.Obj().Pkg() != nil && nt.Obj().Pkg().Path() == "testing" && slices.Contains([]string{"T", "B", "F", "TB"}, nt.Obj().Name())
		} else {
			r.direct = s.direct(x.X) // no type (the scan resolves no import): a testing parameter, or a copy of one
		}
	case *ast.Ident:
		switch o := s.info.Uses[x].(type) {
		case *types.Func:
			if sig, _ := o.Type().(*types.Signature); sig != nil && sig.Recv() != nil && types.IsInterface(sig.Recv().Type()) {
				r.ifaces = append(r.ifaces, o.Name())
			} else {
				r.refs = append(r.refs, o.Origin())
			}
		case *types.Var:
			if o.Parent() == s.pkg.Scope() {
				r.refs = append(r.refs, o)
			}
		}
	}
	return true
}

// skipFacts computes the skipping functions and variables (above): each
// declaration's body and each package variable's values read once, then a
// worklist over the reverse references; past the skip layer's bound, the
// reading stops and the package's tests are unverifiable.
func (s *pkgScan) skipFacts() error {
	type item struct {
		key   any // the *ast.FuncDecl, or the package variable
		node  ast.Node
		value bool // a package variable's value
	}
	var items []item
	for _, ds := range s.same {
		for _, d := range ds {
			if d.Body != nil {
				items = append(items, item{d, d.Body, false})
			}
		}
	}
	for o, vals := range s.varVals {
		for _, v := range vals {
			items = append(items, item{o, v, true})
		}
	}
	if !s.notDirects() {
		s.exhausted = true
		return nil
	}
	rev, revIface := map[types.Object][]any{}, map[string][]any{} // what is used → the declarations and variables using it
	var work []any
	for _, it := range items {
		if err := s.r.ctx.Err(); err != nil {
			return err
		} else if s.r.skipWork > maxSkipWork {
			s.exhausted = true
			return nil
		}
		r := s.skipRead(it.node, it.value)
		if r.direct {
			work = append(work, it.key)
		}
		for _, o := range r.refs {
			rev[o] = append(rev[o], it.key)
		}
		for _, m := range r.ifaces {
			revIface[m] = append(revIface[m], it.key)
		}
	}
	if s.r.skipWork > maxSkipWork {
		s.exhausted = true
		return nil
	}
	for len(work) > 0 {
		k := work[len(work)-1]
		work = work[:len(work)-1]
		switch k := k.(type) {
		case *ast.FuncDecl:
			if s.skipping[k] {
				continue
			}
			for _, d := range s.same[declKey(k)] { // any declaration of a name (race, !race: Opus #292 M3)
				s.skipping[d] = true
				if o := s.info.Defs[d.Name]; o != nil {
					work = append(work, rev[o]...)
				}
			}
			if k.Recv != nil && !s.skipNames[k.Name.Name] {
				s.skipNames[k.Name.Name] = true
				work = append(work, revIface[k.Name.Name]...)
			}
		case types.Object:
			if !s.skipVars[k] {
				s.skipVars[k] = true
				work = append(work, rev[k]...)
			}
		}
	}
	return nil
}
