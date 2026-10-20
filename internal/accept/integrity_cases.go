// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"slices"
	"strconv"
	"strings"
)

// P14d (#226 entry 74; Astra #255 H5, H6, M9; #226 entry 85), on the
// package scan of each version and platform:
//   - A test's cases are identities: each row of a literal table (slice,
//     array or map; inline, or a local the test defines) a loop ranges over
//     when its body asserts, skips or runs subtests (the row's map key, else
//     its first string literal, else its text), and each t.Run with a
//     literal name, nested names joined by "/". A base identity the
//     candidate's test lacks blocks; a loop over another source, or a
//     t.Run named by neither, is a case source the guard cannot follow
//     (opaque), named in that block.
//   - A package function whose every declaration skips (directly, or
//     through another, to a bound) is skipping: a call passing it a testing
//     value is a skip (Astra #255 H6, Opus #255 M5), through a variable
//     holding it too (f := requireDocker; var f = h.require).
//   - A test's print is its assertions and skips, in order, each with the
//     conditions around it; with equal counts, a changed print, or a
//     changed row under its identity, warns (Astra #255 M9).
//   - An assertion or skip in a branch a constant condition never takes
//     (const strict = false; if strict && …) is not counted, and a
//     variable ever assigned a value that is no testing value is no
//     testing value to an assertion (var x E = t; x = fake{}).
//   - Residual: helpers of other packages receiving the testing value (the
//     gate's go_test_json adapter refuses a skip at head), testify suites
//     (no M1 profile uses testify), a table built by appends or a helper.

// deadRanges are the branches a constant condition never takes, in n.
func (s *pkgScan) deadRanges(n ast.Node) {
	ast.Inspect(n, func(n ast.Node) bool {
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
	return slices.ContainsFunc(s.dead, func(r [2]token.Pos) bool { return p >= r[0] && p < r[1] })
}

// live: e is a testing value an assertion may use (not M, never rebound).
func (s *pkgScan) live(e ast.Expr) bool {
	t := s.tval(e)
	if u, ok := ast.Unparen(e).(*ast.UnaryExpr); ok {
		e = u.X
	}
	return t != "" && t != "testing.M" && !s.rebound[s.info.Uses[ident(e)]]
}

// skipCall: call passes a testing value to a skipping function, or to a
// variable holding one.
func (s *pkgScan) skipCall(call *ast.CallExpr) bool {
	if !slices.ContainsFunc(call.Args, s.live) {
		return false
	} else if d := s.callee(call); d != nil && s.skipping[d] {
		return true
	}
	f, _ := fn(call.Fun)
	return slices.ContainsFunc(s.funcVals[s.info.Uses[ident(f)]], func(o types.Object) bool { return s.skipping[s.decls[o]] })
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

// drives: a loop body asserts, skips or runs subtests.
func (s *pkgScan) drives(body ast.Node) bool {
	c := s.count(body)
	return c.asserts+c.skips > 0 || s.runs(body)
}

func (s *pkgScan) runs(n ast.Node) (found bool) {
	ast.Inspect(n, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && s.runCall(c) {
			found = true
		}
		return !found
	})
	return found
}

func (s *pkgScan) runCall(c *ast.CallExpr) bool {
	sel, ok := ast.Unparen(c.Fun).(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Run" && len(c.Args) == 2 && slices.Contains([]string{"testing.T", "testing.B"}, s.tval(sel.X))
}

// testCases reads a test body's case identities (counted: a name may
// repeat), their rows' text, and whether it has a case source the guard
// cannot follow.
func (s *pkgScan) testCases(body ast.Node, text func(ast.Node) string) (ids map[string]int, rows map[string]string, opaque bool) {
	ids, rows = map[string]int{}, map[string]string{}
	lits, traced := map[types.Object]*ast.CompositeLit{}, map[types.Object]bool{} // a local table's literal; a table's range key and value
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, l := range x.Lhs {
				if lit, ok := ast.Unparen(x.Rhs[min(i, len(x.Rhs)-1)]).(*ast.CompositeLit); ok && x.Tok == token.DEFINE && ident(l) != nil {
					lits[s.info.Defs[ident(l)]] = lit
				}
			}
		case *ast.ValueSpec:
			for i, id := range x.Names {
				if i < len(x.Values) {
					if lit, ok := ast.Unparen(x.Values[i]).(*ast.CompositeLit); ok {
						lits[s.info.Defs[id]] = lit
					}
				}
			}
		}
		return true
	})
	var walk func(n ast.Node, prefix string)
	walk = func(n ast.Node, prefix string) {
		ast.Inspect(n, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.RangeStmt:
				if !s.drives(x.Body) {
					return true
				}
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
					k := "row " + prefix + rowName(key, text)
					ids[k]++
					rows[k] += text(e) + "\n"
				}
				s.r.work += len(lit.Elts)
			case *ast.CallExpr:
				if !s.runCall(x) {
					return true
				}
				name := "*" // a traced row's name: its row is the identity
				if lit, ok := ast.Unparen(x.Args[0]).(*ast.BasicLit); ok && lit.Kind == token.STRING {
					name, _ = strconv.Unquote(lit.Value)
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

// rowName is a row's identity: a string key or value, else its first
// string literal field, else its text.
func rowName(e ast.Expr, text func(ast.Node) string) string {
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
	if lit, ok := ast.Unparen(e).(*ast.CompositeLit); ok {
		for _, f := range lit.Elts {
			if kv, ok := f.(*ast.KeyValueExpr); ok {
				f = kv.Value
			}
			if v, ok := str(f); ok {
				return strconv.Quote(v)
			}
		}
	}
	return text(e)
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

// testPrint is a test's assertions and skips, in order, each with the
// conditions of the statements around it (a range over a table by its
// name only: an added row changes no print).
func (s *pkgScan) testPrint(body ast.Node) string {
	var b strings.Builder
	var stack []ast.Node
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if call, ok := n.(*ast.CallExpr); ok && !s.isDead(call.Pos()) && (s.assertion(call) || s.skipCall(call) || s.skipSel(call)) {
			for i, a := range stack {
				switch x := a.(type) {
				case *ast.IfStmt:
					in := "if "
					if i+1 < len(stack) && stack[i+1] == x.Else {
						in = "else of if "
					}
					b.WriteString(in + types.ExprString(x.Cond) + " | ")
				case *ast.ForStmt:
					if x.Cond != nil {
						b.WriteString("for " + types.ExprString(x.Cond) + " | ")
					}
				case *ast.SwitchStmt:
					if x.Tag != nil {
						b.WriteString("switch " + types.ExprString(x.Tag) + " | ")
					}
				case *ast.CaseClause:
					for _, e := range x.List {
						b.WriteString("case " + types.ExprString(e) + " ")
					}
					b.WriteString("| ")
				}
			}
			b.WriteString(types.ExprString(call) + "\n")
		}
		stack = append(stack, n)
		return true
	})
	return b.String()
}

// skipSel: call is a Skip, Skipf or SkipNow method call.
func (s *pkgScan) skipSel(call *ast.CallExpr) bool {
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	return ok && slices.Contains(goSkips, sel.Sel.Name)
}

// casesRemoved is the first base case identity the candidate's test
// holds fewer of, and how many are fewer.
func casesRemoved(b, a *stat) (first string, n int) {
	for _, k := range sortedKeys(b.cases) {
		if d := b.cases[k] - a.cases[k]; d > 0 {
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
