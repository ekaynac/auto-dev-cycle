// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/constant"
	"go/printer"
	"go/token"
	"go/types"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
)

// P14d (#226 entry 74; Astra #255 H5, H6, M9; #226 entry 85), on the
// package scan of each version and platform. Decision A: what the guard
// cannot follow warns or refuses, never modelled further.
//   - A test's cases are identities: each row of a literal table (slice,
//     array or map; inline, or a local the test defines and uses only as a
//     range's expression: an alias, a copy, delete, clear, an argument, an
//     address or a write makes it opaque, Opus #292 H1, Astra #292 H1) a loop
//     ranges over when its body asserts, skips or runs subtests (itself, or
//     through a local closure it calls: M4). A row's identity is its loop's
//     (its position among the test's, and whether it checks or only runs
//     subtests: a row moved under another loop is removed, H1, but one whose
//     loop only ran subtests and now checks is kept, Astra M6; the loop's
//     print would make any edit of its assertions a removal, as in the
//     replay's c09ac49) and its own: the row's map key, else its first string
//     literal (or a call's, append(id, "commit")), else its canonical text
//     (go/printer, no comments: gofmt changes none, M2). Each t.Run with a
//     constant name is one too, nested names joined by "/". A base identity
//     the candidate's test lacks blocks; a loop over another source, or a
//     t.Run named by neither, is a source the guard cannot follow (opaque),
//     named in that block. What a constant condition never runs names none.
//   - A package function any declaration of which skips (Skip, SkipNow or
//     Skipf of a testing.T, B, F or TB, never another type's: Astra M7;
//     directly, or through another, to a bound; M3), and a package variable holding a
//     literal that skips, are skipping: any use of one is a skip, a call
//     whatever its arguments or a value (a slice, a field, a runner's
//     argument, an alias: H2), and so is a call of an interface method a
//     skipping package method's name.
//   - A test's print is its assertions and skips, in order, each a hash of
//     its canonical text and a rolling hash of every control statement and
//     branch around it (ifs with their Init, else, for, switch, select and
//     their cases), charged to the work bound (H3, M1, Astra M8). With equal
//     counts, a changed print warns; a changed row under its identity warns
//     whatever the counts, when nothing blocks (Astra #255 M9).
//   - An assertion or skip in a branch a constant condition never takes
//     (const strict = false; if strict && …) is not counted, and a
//     variable ever assigned a value that is no testing value is no
//     testing value to an assertion (var x E = t; x = fake{}). Rows and
//     assertions are charged by their canonical text, loop bodies and the
//     skipping helpers by their size; past the bound, reading stops at once
//     and the test's file is unverifiable (Astra H5).
//   - Residual: helpers of other packages receiving the testing value (the
//     gate's go_test_json adapter refuses a skip at head), testify suites
//     (no M1 profile uses testify), a table built by appends or a helper; a
//     testing value rebound through a call mentioning it (x = wrap(t)) stays
//     one (Opus #292 L3). P14d-2 (#226 entry 95): skipping facts through a
//     package variable's alias of an alias (var second = first: Astra #292
//     H2), literal subtests in a same-package helper the test calls (H3), and
//     identities of one build variant only, which the merge of variants
//     drops (H4).

// deadRanges are the branches a constant condition never takes, in files,
// sorted and merged for isDead's binary search (Opus #292 M5).
func (s *pkgScan) deadRanges(files []*ast.File) {
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if x, ok := n.(*ast.IfStmt); ok {
				if s.constCond(x.Cond, false) {
					s.dead = append(s.dead, [2]token.Pos{x.Body.Pos(), x.Body.End()})
				} else if s.constCond(x.Cond, true) && x.Else != nil {
					s.dead = append(s.dead, [2]token.Pos{x.Else.Pos(), x.Else.End()})
				}
			}
			return true
		})
	}
	slices.SortFunc(s.dead, func(a, b [2]token.Pos) int { return cmp.Compare(a[0], b[0]) })
	merged := s.dead[:0]
	for _, r := range s.dead {
		if n := len(merged); n > 0 && r[0] <= merged[n-1][1] {
			merged[n-1][1] = max(merged[n-1][1], r[1])
		} else {
			merged = append(merged, r)
		}
	}
	s.dead = merged
}

// constCond: e is the constant v, or (for false) a conjunction one of
// whose operands is false.
func (s *pkgScan) constCond(e ast.Expr, v bool) bool {
	e = ast.Unparen(e)
	if tv, ok := s.info.Types[e]; ok && tv.Value != nil && tv.Value.Kind() == constant.Bool {
		return constant.BoolVal(tv.Value) == v
	}
	b, ok := e.(*ast.BinaryExpr)
	return ok && !v && b.Op == token.LAND && (s.constCond(b.X, false) || s.constCond(b.Y, false))
}

func (s *pkgScan) isDead(p token.Pos) bool {
	i, found := slices.BinarySearchFunc(s.dead, p, func(r [2]token.Pos, p token.Pos) int {
		return cmp.Compare(r[0], p+1) // the last range starting at or before p
	})
	return !found && i > 0 && p < s.dead[i-1][1]
}

// live: e is a testing value an assertion may use (not M, never rebound).
func (s *pkgScan) live(e ast.Expr) bool {
	t := s.tval(e)
	if u, ok := ast.Unparen(e).(*ast.UnaryExpr); ok {
		e = u.X
	}
	return t != "" && t != "testing.M" && !s.rebound[s.info.Uses[ident(e)]]
}

// skipUse: id names a skipping function or method (called or not), a
// variable holding one, a package variable holding a literal that skips,
// or an interface's method a skipping package method's name.
func (s *pkgScan) skipUse(id *ast.Ident) bool {
	switch o := s.info.Uses[id].(type) {
	case nil:
		return false
	case *types.Func:
		sig, _ := o.Type().(*types.Signature)
		return s.skipping[s.decls[o.Origin()]] || sig != nil && sig.Recv() != nil && types.IsInterface(sig.Recv().Type()) && s.skipNames[o.Name()]
	case *types.Var:
		return s.skipVars[o] || slices.ContainsFunc(s.funcVals[o], func(f types.Object) bool { return s.skipping[s.decls[f]] })
	}
	return false
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

// skipsIn: body skips a test (Astra #292 M7): Skip, SkipNow or Skipf of a
// testing.T, B, F or TB (by its type, or a testing value the scan knows:
// the scan's imports may be unresolved), or a use of a skipping function;
// its reading charged.
func (s *pkgScan) skipsIn(body ast.Node) (found bool) {
	s.r.work += int(body.End() - body.Pos())
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			t := s.info.TypeOf(x.X)
			if p, ok := t.(*types.Pointer); ok {
				t = p.Elem()
			}
			nt, _ := types.Unalias(t).(*types.Named)
			typed := nt != nil && nt.Obj().Pkg() != nil && nt.Obj().Pkg().Path() == "testing" && slices.Contains([]string{"T", "B", "F", "TB"}, nt.Obj().Name())
			found = found || slices.Contains(goSkips, x.Sel.Name) && (typed || slices.Contains([]string{"testing.T", "testing.B", "testing.F", "testing.TB"}, s.tval(x.X))) && !s.isDead(x.Pos())
		case *ast.Ident:
			found = found || s.skipUse(x) && !s.isDead(x.Pos())
		}
		return !found
	})
	return found
}

// drives: a loop body asserts or skips (checks), or else runs subtests,
// itself or through a local closure it calls (to a depth), each body's
// answer cached and its reading charged; past the work bound, false at
// once (the caller's test is then unverifiable: Astra #292 H5).
func (s *pkgScan) drives(body ast.Node, closures map[types.Object]*ast.FuncLit, runs bool, depth int) (found bool) {
	key := driveKey{body, runs}
	if v, ok := s.driven[key]; ok {
		return v
	}
	defer func() { s.driven[key] = found }()
	caseVisits.Add(1)
	if s.r.work += int(body.End() - body.Pos()); depth > 8 { // the bound: checked by the walk before, and below during
		return false
	}
	if c := s.count(body); c.asserts+c.skips > 0 {
		return true
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if runs && s.runCall(x) {
				found = true
			} else if fl := closures[s.info.Uses[ident(x.Fun)]]; fl != nil {
				found = s.drives(fl.Body, closures, runs, depth+1)
			}
		}
		return !found && s.r.work <= maxWork
	})
	return found
}

type driveKey struct {
	body ast.Node
	runs bool
}

// caseVisits counts drives' readings (tests read it: a bound, never a clock).
var caseVisits atomic.Int64

func (s *pkgScan) runCall(c *ast.CallExpr) bool {
	sel, ok := ast.Unparen(c.Fun).(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Run" && len(c.Args) == 2 && slices.Contains([]string{"testing.T", "testing.B"}, s.tval(sel.X))
}

// testCases reads a test body's case identities (counted: a name may
// repeat), their rows' canonical text, and whether it has a case source the
// guard cannot follow.
func (s *pkgScan) testCases(body ast.Node) (ids map[string]int, rows map[string]string, opaque bool) {
	ids, rows = map[string]int{}, map[string]string{}
	lits, closures := map[types.Object]*ast.CompositeLit{}, map[types.Object]*ast.FuncLit{} // a local table; a local closure
	traced := map[types.Object]bool{}                                                       // a table's range key and value
	define := func(id *ast.Ident, v ast.Expr) {
		switch x := ast.Unparen(v).(type) {
		case *ast.CompositeLit:
			lits[s.info.Defs[id]] = x
		case *ast.FuncLit:
			closures[s.info.Defs[id]] = x
		}
	}
	// A table is followed only where its every use is a range's expression,
	// a closure only where its every use is a call: any other use (an alias,
	// a copy, delete, clear, an argument, an address, a write) may change
	// it, so it is not followed (Astra #292 H1).
	uses, kept := map[types.Object]int{}, map[types.Object]int{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, l := range x.Lhs {
				if id := ident(l); id != nil && s.info.Defs[id] != nil && len(x.Lhs) == len(x.Rhs) {
					define(id, x.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			for i, id := range x.Names {
				if len(x.Values) == len(x.Names) {
					define(id, x.Values[i])
				}
			}
		case *ast.RangeStmt:
			if id := ident(x.X); id != nil {
				kept[s.info.Uses[id]]++
			}
		case *ast.CallExpr:
			if id := ident(x.Fun); id != nil {
				kept[s.info.Uses[id]]++
			}
		case *ast.Ident:
			uses[s.info.Uses[x]]++
		}
		return true
	})
	for o, n := range uses {
		if n != kept[o] {
			delete(lits, o)
			delete(closures, o)
		}
	}
	loops := 0 // the test's driving loops, in order
	var walk func(n ast.Node, prefix string)
	walk = func(n ast.Node, prefix string) {
		ast.Inspect(n, func(n ast.Node) bool {
			if n != nil && s.isDead(n.Pos()) || s.r.work > maxWork {
				return false // a branch never taken names no case (Opus #292 L4); past the bound, nothing more is read
			}
			switch x := n.(type) {
			case *ast.RangeStmt:
				if !s.drives(x.Body, closures, true, 0) {
					return true
				}
				loops++
				loop := strconv.Itoa(loops) + map[bool]string{true: " checks", false: " runs"}[s.drives(x.Body, closures, false, 0)] // a row moved to another loop, or to one only running subtests, is removed (Opus #292 H1)
				lit, _ := ast.Unparen(x.X).(*ast.CompositeLit)
				if id := ident(x.X); id != nil && lit == nil {
					lit = lits[s.info.Uses[id]]
				}
				if lit == nil {
					opaque = true
					return true
				}
				for _, v := range []ast.Expr{x.Key, x.Value} {
					if id := ident(v); id != nil && s.info.Defs[id] != nil {
						traced[s.info.Defs[id]] = true
					}
				}
				_, isMap := types.Unalias(s.info.TypeOf(lit)).Underlying().(*types.Map)
				for _, e := range lit.Elts {
					key := e
					if kv, ok := e.(*ast.KeyValueExpr); ok && isMap {
						key = kv.Key
					} else if ok {
						key = kv.Value // an array's index
					}
					k := "row " + prefix + s.rowName(key) + " @" + loop
					ids[k]++
					rows[k] += s.canon(e) + "\n"
				}
			case *ast.CallExpr:
				if !s.runCall(x) {
					return true
				}
				name := "*" // a traced row's name: its row is the identity
				if tv := s.info.Types[x.Args[0]]; tv.Value != nil && tv.Value.Kind() == constant.String {
					name = constant.StringVal(tv.Value) // a literal or a constant (Opus #292 M2)
					ids["run "+prefix+name]++
				} else if !mentionsAny(x.Args[0], s.info, traced) {
					opaque, name = true, "?"
				}
				s.r.work++
				if fl, ok := x.Args[1].(*ast.FuncLit); ok {
					walk(fl.Body, prefix+name+"/")
					return false
				}
			}
			return true
		})
	}
	walk(body, "")
	return ids, rows, opaque
}

// canon is n's canonical text: go/printer's, without positions (gofmt's
// layout) or comments; charged to the work bound.
func (s *pkgScan) canon(n ast.Node) string {
	var b strings.Builder
	_ = printer.Fprint(&b, token.NewFileSet(), n)
	s.r.work += b.Len()
	return b.String()
}

// rowName is a row's identity: a string key or value, else its first
// string literal field (or a call's first string literal argument, as in
// append(id, "commit")), else its canonical text.
func (s *pkgScan) rowName(e ast.Expr) string {
	str := func(e ast.Expr) (string, bool) {
		lit, ok := ast.Unparen(e).(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(lit.Value)
		return v, err == nil
	}
	if v, ok := str(e); ok {
		return strconv.Quote(v)
	}
	var fields []ast.Expr
	switch x := ast.Unparen(e).(type) {
	case *ast.CompositeLit:
		fields = x.Elts
	case *ast.CallExpr: // append(id, "commit", …): its first string literal argument
		fields = x.Args
	}
	for _, f := range fields {
		if kv, ok := f.(*ast.KeyValueExpr); ok {
			f = kv.Value
		}
		if v, ok := str(f); ok {
			return strconv.Quote(v)
		}
	}
	return s.canon(e)
}

func mentionsAny(e ast.Expr, info *types.Info, objs map[types.Object]bool) (found bool) {
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && objs[info.Uses[id]] {
			found = true
		}
		return !found
	})
	return found
}

// testPrint is n's assertions and skips, in order, each a hash of its
// canonical text and a rolling hash of every control statement around it:
// each if (its Init and condition, and which branch), for, switch and type
// switch (Init and tag), select, and case or comm clause, each read once on
// entry (linear: Opus #292 H3; Astra #292 M8); charged to the work bound,
// and cut past it. A range is a level, never its expression (an added row
// changes no print).
func (s *pkgScan) testPrint(n ast.Node) string {
	var b strings.Builder
	type level struct {
		node ast.Node
		sum  [32]byte
	}
	stack := []level{{}}
	ast.Inspect(n, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if s.r.work > maxWork {
			return false
		}
		top := stack[len(stack)-1]
		ctl := ""
		switch x := n.(type) {
		case *ast.IfStmt:
			ctl = "if " + s.canonOpt(x.Init) + "; " + s.canon(x.Cond)
		case *ast.ForStmt:
			ctl = "for " + s.canonOpt(x.Init) + "; " + s.canonOpt(x.Cond) + "; " + s.canonOpt(x.Post)
		case *ast.RangeStmt:
			ctl = "range"
		case *ast.SwitchStmt:
			ctl = "switch " + s.canonOpt(x.Init) + "; " + s.canonOpt(x.Tag)
		case *ast.TypeSwitchStmt:
			ctl = "type switch " + s.canonOpt(x.Init) + "; " + s.canon(x.Assign)
		case *ast.SelectStmt:
			ctl = "select"
		case *ast.CaseClause:
			ctl = "case"
			for _, e := range x.List {
				ctl += " " + s.canon(e)
			}
		case *ast.CommClause:
			ctl = "comm " + s.canonOpt(x.Comm)
		}
		if ifs, ok := top.node.(*ast.IfStmt); ok && n == ifs.Else {
			ctl = "else"
		}
		sum := top.sum
		if ctl != "" {
			sum = sha256.Sum256(append(top.sum[:], ctl...))
		}
		event := ""
		switch x := n.(type) {
		case *ast.CallExpr:
			if sel, ok := ast.Unparen(x.Fun).(*ast.SelectorExpr); s.assertion(x) || ok && slices.Contains(goSkips, sel.Sel.Name) {
				event = s.canon(x)
			}
		case *ast.Ident:
			if s.skipUse(x) {
				event = "use " + x.Name
			}
		}
		if event != "" && !s.isDead(n.Pos()) {
			e := sha256.Sum256(append(sum[:], event...))
			b.WriteString(hex.EncodeToString(e[:8]))
		}
		stack = append(stack, level{n, sum})
		return true
	})
	return b.String()
}

func (s *pkgScan) canonOpt(n ast.Node) string {
	if n == nil {
		return ""
	}
	return s.canon(n)
}

// casesRemoved is the first base case identity the candidate's test
// holds fewer of, and how many are fewer.
// A row whose loop only ran subtests and now checks, at its position,
// is kept (Astra #292 M6: a strengthening); the reverse is removed.
func casesRemoved(b, a *stat) (first string, n int) {
	for _, k := range sortedKeys(b.cases) {
		have := a.cases[k]
		if c, ok := strings.CutSuffix(k, " runs"); ok {
			have += max(0, a.cases[c+" checks"]-b.cases[c+" checks"])
		}
		if d := b.cases[k] - have; d > 0 {
			first, n = cmpOrS(first, k), n+d
		}
	}
	return first, n
}

// rowsChanged: a case both hold has another row text.
func rowsChanged(b, a *stat) bool {
	for k, t := range b.rows {
		if u, ok := a.rows[k]; ok && u != t {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]int) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

func cmpOrS(a, b string) string {
	if a == "" {
		return b
	}
	return a
}
