// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/constant"
	"go/printer"
	"go/token"
	"go/types"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
)

// P14d-b (#226 entries 74, 85, 99; Astra #255 H5, M9; Astra #292 H3, H4
// and round 2's 2, 3, 5 to 8), on the package scan of each version and
// platform. It only adds blocks and warnings to main's comparison.
// Decision A: what the guard cannot follow is named in a block, never
// modelled further.
//   - A test's cases are identities, each counted (a name may repeat):
//   - each row of a literal table (slice, array or map; inline, or a local
//     the test uses only as a range's expression: any other use, an alias,
//     a copy, delete, clear, an argument, an address or a write, makes it
//     opaque) a loop ranges over when its body asserts, skips or runs
//     subtests (itself, through a local closure it calls, or a helper's
//     subtests). A row's identity is its loop's (its position among the
//     test's or the helper's, and whether it checks or only runs subtests:
//     a row moved under another loop is removed) and its own: its map key,
//     else its first string literal (or a call's: append(id, "commit")),
//     else its canonical text (go/printer, no comments: gofmt changes
//     none). A row whose loop only ran subtests and now checks is kept
//     (a strengthening); a subtest's name never is so read (Astra r2 2).
//   - each t.Run with a constant name, nested names joined by "/", read
//     through its callback: a literal, a local closure, or a package
//     function (a named callback: Astra H3).
//   - A same-package helper called with a testing value, and a local
//     closure called, deferred, given to t.Cleanup or as t.Run's callback
//     (Astra r2 8), contribute their cases where they are called, under the
//     caller's subtest name: each one's summary is read once and composed
//     per call (Astra H3). A summary whose followed closures and helpers
//     nest maxCaseDepth deep is not followed, the same wherever it is
//     called, so no count depends on the order of the calls (Astra r2 5).
//     A cycle, a helper of several declarations (race, !race), a summary
//     too deep, a t.Run named by neither a constant nor a row, and a loop
//     over another source are sources the guard cannot follow (opaque): a
//     removal is then named unverifiable. A branch a constant condition
//     never takes names no case.
//   - A test's cases are kept per build-constraint alternative (Astra H4):
//     a base alternative is compared with the candidate's of the same
//     constraint, else with what every candidate alternative holds.
//   - A test's print is its assertions and skips, in order, each a hash of
//     its canonical text and a rolling hash of every control statement and
//     branch around it (an if with its Init, an else with the if it holds,
//     for, switch and type switch with Init, select, case and comm
//     clauses), each control's text built once (Astra r2 3, 7). With equal
//     counts, a changed print warns; a changed row under its identity
//     warns whatever the counts, when nothing blocks the test.
//   - Work: canonical text, loop bodies, helper bodies and summaries are
//     charged to the guard's work bound, which is checked right after each
//     charge (Astra r2 6); past it, reading stops and the test's file is
//     unverifiable. drives' answers are cached per body and depth (Astra
//     r2 5): a depth-limited answer is never reused at another depth.
//   - Residual: helpers of other packages, testify suites, a table built by
//     appends or a helper, the cases of a helper of several declarations.

// maxCaseDepth bounds closures and helpers followed within one another.
const maxCaseDepth = 8

// caseScan is a package scan's case state, made on its first test.
type caseScan struct {
	driven map[driveKey]bool      // drives' answers
	sums   map[ast.Node]*caseSum  // helpers' and closures' summaries
	on     map[*ast.FuncDecl]bool // helpers being summarized (a cycle)
	dead   map[ast.Node]bool      // branches a constant condition never takes
}

// caseAlts is a test's cases per build-constraint alternative.
type caseAlts = map[string]*caseSum

// caseSum is a test's or a helper's cases: identities ("row …" or "run …")
// counted, each row's canonical text, whether a case source is one the
// guard cannot follow, whether a helper's cycle cut it (never cached), the
// height of the closures and helpers it follows within one another, and (a
// test's) its print.
type caseSum struct {
	ids         map[string]int
	rows        map[string]string
	opaque, cyc bool
	height      int
	print       string
}

type driveKey struct {
	body  ast.Node
	runs  bool
	depth int
}

// caseVisits counts drives' readings and caseWork the case code's charged
// work (tests read them: bounds, never a clock).
var caseVisits, caseWork atomic.Int64

// charge adds n to the work bound; false past it.
func (s *pkgScan) charge(n int) bool {
	s.r.work += n
	caseWork.Add(int64(n))
	return s.r.work <= maxWork
}

// caseFacts records fn's cases under the test key k and its build
// constraint cons; past the work bound, its file is unverifiable.
func (s *pkgScan) caseFacts(fa *pkgFacts, k, path, cons string, fn *ast.FuncDecl) {
	if s.cs.driven == nil {
		s.cs = caseScan{map[driveKey]bool{}, map[ast.Node]*caseSum{}, map[*ast.FuncDecl]bool{}, map[ast.Node]bool{}}
	}
	c := s.cases(fn.Body, "")
	c.print = s.testPrint(fn.Body)
	if fa.cases[k] == nil {
		fa.cases[k] = caseAlts{}
	}
	if old := fa.cases[k][cons]; old != nil { // declared twice under one constraint: what both hold
		c = common(old, c)
	}
	if fa.cases[k][cons] = c; s.r.work > maxWork {
		fa.bad[path] = "test cases or assertions past the guard's work bound"
	}
}

// caseWalk is one body's reading: its summary, its local tables and the
// closures followed, the rows' traced names, its loops' label and count,
// and its function's (the test's or the helper's: its closures' labels).
type caseWalk struct {
	sum      *caseSum
	lits     map[types.Object]*ast.CompositeLit
	closures map[types.Object]*ast.FuncLit
	defs     map[*ast.FuncLit]bool
	traced   map[types.Object]bool
	self     string
	loops    int
	root     string
}

// cases reads body's cases; self labels its loops ("" a test's, "name#" a
// helper's).
func (s *pkgScan) cases(body *ast.BlockStmt, self string) *caseSum {
	w := &caseWalk{sum: &caseSum{ids: map[string]int{}, rows: map[string]string{}}, traced: map[types.Object]bool{}, self: self, root: self, defs: map[*ast.FuncLit]bool{}}
	if !s.charge(int(body.End() - body.Pos())) {
		return w.sum
	}
	w.lits, w.closures = s.locals(body)
	for _, fl := range w.closures {
		w.defs[fl] = true
	}
	s.walk(w, body, "")
	return w.sum
}

// locals are body's local tables and closures the guard follows: a table
// whose every use is a range's expression, a closure whose every use is a
// call or a testing value's Cleanup or Run callback (Astra #292 H1; r2 8).
func (s *pkgScan) locals(body ast.Node) (map[types.Object]*ast.CompositeLit, map[types.Object]*ast.FuncLit) {
	lits, closures := map[types.Object]*ast.CompositeLit{}, map[types.Object]*ast.FuncLit{}
	define := func(id *ast.Ident, v ast.Expr) {
		switch x := ast.Unparen(v).(type) {
		case *ast.CompositeLit:
			lits[s.info.Defs[id]] = x
		case *ast.FuncLit:
			closures[s.info.Defs[id]] = x
		}
	}
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
			if cb := s.callback(x); cb != nil {
				if id := ident(cb); id != nil {
					kept[s.info.Uses[id]]++
				}
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
	return lits, closures
}

// callback is the function a testing value's Run (its second argument) or
// Cleanup (its one) runs, if call is one.
func (s *pkgScan) callback(c *ast.CallExpr) ast.Expr {
	sel, ok := ast.Unparen(c.Fun).(*ast.SelectorExpr)
	switch {
	case s.runCall(c):
		return c.Args[1]
	case ok && sel.Sel.Name == "Cleanup" && len(c.Args) == 1 && s.tval(sel.X) != "" && s.tval(sel.X) != "testing.M":
		return c.Args[0]
	}
	return nil
}

func (s *pkgScan) runCall(c *ast.CallExpr) bool {
	sel, ok := ast.Unparen(c.Fun).(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Run" && len(c.Args) == 2 && slices.Contains([]string{"testing.T", "testing.B"}, s.tval(sel.X))
}

// helper is the same-package function call reaches with a testing value,
// if one.
func (s *pkgScan) helper(c *ast.CallExpr) *ast.FuncDecl {
	if d := s.callee(c); d != nil && d.Body != nil && slices.ContainsFunc(c.Args, func(a ast.Expr) bool {
		t := s.tval(a)
		return t != "" && t != "testing.M"
	}) {
		return d
	}
	return nil
}

// declOf is the package function or method e names, if one (a named
// callback: f, h.m, f[int]).
func (s *pkgScan) declOf(e ast.Expr) *ast.FuncDecl {
	f, _ := fn(e)
	id := ident(f)
	if sel, ok := f.(*ast.SelectorExpr); ok {
		id = sel.Sel
	}
	if o, ok := s.info.Uses[id].(*types.Func); ok {
		if d := s.decls[o.Origin()]; d != nil && d.Body != nil {
			return d
		}
	}
	return nil
}

// markDead records the branch of x a constant condition never takes
// (before ast.Inspect reaches it: pre-order).
func (s *pkgScan) markDead(x *ast.IfStmt) {
	if s.constCond(x.Cond, false) {
		s.cs.dead[x.Body] = true
	} else if s.constCond(x.Cond, true) && x.Else != nil {
		s.cs.dead[x.Else] = true
	}
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

func (s *pkgScan) walk(w *caseWalk, n ast.Node, prefix string) {
	ast.Inspect(n, func(n ast.Node) bool {
		if n == nil || s.r.work > maxWork || s.cs.dead[n] {
			return false
		}
		switch x := n.(type) {
		case *ast.IfStmt:
			s.markDead(x)
		case *ast.FuncLit:
			return !w.defs[x] // a followed closure is read where it runs
		case *ast.RangeStmt:
			s.rows(w, x, prefix)
		case *ast.CallExpr:
			if !s.charge(1) {
				return false
			}
			if s.runCall(x) {
				s.run(w, x, prefix)
				return false
			}
			if _, lit := ast.Unparen(s.callback(x)).(*ast.FuncLit); s.callback(x) != nil && !lit { // Cleanup: a literal is read in place
				s.follow(w, s.callback(x), prefix)
			} else if fl := w.closures[s.info.Uses[ident(x.Fun)]]; fl != nil {
				s.follow(w, x.Fun, prefix)
			} else if d := s.helper(x); d != nil {
				s.compose(w, s.helperSum(d), prefix)
			}
		}
		return true
	})
}

// rows records a driving loop's rows.
func (s *pkgScan) rows(w *caseWalk, x *ast.RangeStmt, prefix string) {
	if !s.drives(x.Body, w.closures, true, 0) {
		return
	}
	w.loops++
	loop := w.self + strconv.Itoa(w.loops) + map[bool]string{true: " checks", false: " runs"}[s.drives(x.Body, w.closures, false, 0)]
	lit, _ := ast.Unparen(x.X).(*ast.CompositeLit)
	if id := ident(x.X); id != nil && lit == nil {
		lit = w.lits[s.info.Uses[id]]
	}
	if lit == nil {
		w.sum.opaque = true
		return
	}
	for _, v := range []ast.Expr{x.Key, x.Value} {
		if id := ident(v); id != nil && s.info.Defs[id] != nil {
			w.traced[s.info.Defs[id]] = true
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
		w.sum.ids[k]++
		w.sum.rows[k] += s.canon(e) + "\n"
		if s.r.work > maxWork {
			return
		}
	}
}

// run records a t.Run's identity and reads its callback under its name.
func (s *pkgScan) run(w *caseWalk, x *ast.CallExpr, prefix string) {
	name := "*" // a traced row's name: its row is the identity
	if tv := s.info.Types[x.Args[0]]; tv.Value != nil && tv.Value.Kind() == constant.String {
		name = constant.StringVal(tv.Value) // a literal or a constant
		w.sum.ids["run "+prefix+name]++
	} else if !mentionsAny(x.Args[0], s.info, w.traced) {
		w.sum.opaque, name = true, "?"
	}
	s.follow(w, x.Args[1], prefix+name+"/")
}

// follow reads the function cb runs, under prefix: a literal in place, a
// followed local closure or a package function by its summary; else it is
// opaque.
func (s *pkgScan) follow(w *caseWalk, cb ast.Expr, prefix string) {
	if fl, ok := ast.Unparen(cb).(*ast.FuncLit); ok {
		s.walk(w, fl.Body, prefix)
		return
	}
	id := ident(cb)
	if fl := w.closures[s.info.Uses[id]]; fl != nil {
		s.compose(w, s.closureSum(w, id.Name, fl), prefix)
	} else if d := s.declOf(cb); d != nil {
		s.compose(w, s.helperSum(d), prefix)
	} else {
		w.sum.opaque = true
	}
}

// closureSum is a local closure's cases, read once: its loops are
// labelled by its function's label and its name.
func (s *pkgScan) closureSum(w *caseWalk, name string, fl *ast.FuncLit) *caseSum {
	if c := s.cs.sums[fl]; c != nil {
		return c
	}
	sub := *w // its tables, closures and traced names
	sub.sum, sub.self, sub.loops = &caseSum{ids: map[string]int{}, rows: map[string]string{}}, w.root+name+"#", 0
	s.walk(&sub, fl.Body, "")
	s.keep(fl, sub.sum)
	return sub.sum
}

// helperSum is d's cases, read once per declaration.
func (s *pkgScan) helperSum(d *ast.FuncDecl) *caseSum {
	if h := s.cs.sums[d]; h != nil {
		return h
	}
	switch {
	case len(s.same[declKey(d)]) > 1: // race, !race: not followed
		return &caseSum{opaque: true}
	case s.cs.on[d]:
		return &caseSum{opaque: true, cyc: true}
	}
	s.cs.on[d] = true
	h := s.cases(d.Body, declKey(d)+"#")
	delete(s.cs.on, d)
	s.keep(d, h)
	return h
}

// keep caches a summary, unless a cycle cut it (its answer then depends on
// where it was first read), or past the work bound.
func (s *pkgScan) keep(fn ast.Node, h *caseSum) {
	if !h.cyc && s.r.work <= maxWork {
		s.cs.sums[fn] = h
	}
}

// compose adds h's cases to w's under prefix, charged. A summary of height
// maxCaseDepth is not followed (opaque): the same wherever it is read, so
// the order of calls never changes a count (Astra #292 r2 5).
func (s *pkgScan) compose(w *caseWalk, h *caseSum, prefix string) {
	w.sum.opaque, w.sum.cyc = w.sum.opaque || h.opaque, w.sum.cyc || h.cyc
	if h.height >= maxCaseDepth {
		w.sum.opaque, w.sum.height = true, maxCaseDepth
		return
	}
	w.sum.height = max(w.sum.height, h.height+1)
	if !s.charge(len(h.ids)) {
		return
	}
	for id, n := range h.ids {
		w.sum.ids[id[:4]+prefix+id[4:]] += n
	}
	for id, t := range h.rows {
		if !s.charge(len(t)) {
			return
		}
		w.sum.rows[id[:4]+prefix+id[4:]] += t
	}
}

// drives: a loop body asserts or skips (checks), or else runs subtests,
// itself, through a local closure it calls or a helper's subtests, to a
// depth. Each body's answer is cached with its depth; the bound is checked
// right after each charge.
func (s *pkgScan) drives(body ast.Node, closures map[types.Object]*ast.FuncLit, runs bool, depth int) (found bool) {
	key := driveKey{body, runs, depth}
	if v, ok := s.cs.driven[key]; ok {
		return v
	}
	if !s.charge(int(body.End()-body.Pos())) || depth > maxCaseDepth {
		return false
	}
	defer func() { s.cs.driven[key] = found }()
	caseVisits.Add(1) // a reading: none past the bound
	if c := s.count(body); c.asserts+c.skips > 0 {
		return true
	}
	ast.Inspect(body, func(n ast.Node) bool {
		if found || s.r.work > maxWork {
			return false
		}
		if x, ok := n.(*ast.CallExpr); ok {
			switch fl, d := closures[s.info.Uses[ident(x.Fun)]], s.helper(x); {
			case runs && s.runCall(x):
				found = true
			case fl != nil:
				found = s.drives(fl.Body, closures, runs, depth+1)
			case runs && d != nil:
				found = len(s.helperSum(d).ids) > 0
			}
		}
		return !found
	})
	return found
}

// canon is n's canonical text: go/printer's, without positions (gofmt's
// layout) or comments; charged to the work bound.
func (s *pkgScan) canon(n ast.Node) string {
	var b strings.Builder
	s.canonTo(&b, n)
	return b.String()
}

func (s *pkgScan) canonTo(b *strings.Builder, n ast.Node) {
	l := b.Len()
	_ = printer.Fprint(b, token.NewFileSet(), n)
	s.charge(b.Len() - l)
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
	case *ast.CallExpr: // append(id, "commit", …)
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
// canonical text and a rolling hash of every control around it, each
// control's text built once into one builder, charged per expression
// (Astra r2 3); an else is hashed with the if it holds (r2 7). A range is
// a level, never its expression (an added row changes no print).
func (s *pkgScan) testPrint(n ast.Node) string {
	var out, ctl strings.Builder
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
		if s.r.work > maxWork || s.cs.dead[n] {
			return false
		}
		top := stack[len(stack)-1]
		ctl.Reset()
		if ifs, ok := top.node.(*ast.IfStmt); ok && n == ifs.Else {
			ctl.WriteString("else ")
		}
		switch x := n.(type) {
		case *ast.IfStmt:
			s.markDead(x)
			s.control(&ctl, "if", x.Init, x.Cond)
		case *ast.ForStmt:
			s.control(&ctl, "for", x.Init, x.Cond, x.Post)
		case *ast.RangeStmt:
			ctl.WriteString("range")
		case *ast.SwitchStmt:
			s.control(&ctl, "switch", x.Init, x.Tag)
		case *ast.TypeSwitchStmt:
			s.control(&ctl, "type switch", x.Init, x.Assign)
		case *ast.SelectStmt:
			ctl.WriteString("select")
		case *ast.CaseClause:
			ctl.WriteString("case")
			for _, e := range x.List {
				if s.control(&ctl, "", e); s.r.work > maxWork {
					return false
				}
			}
		case *ast.CommClause:
			s.control(&ctl, "comm", x.Comm)
		}
		sum := top.sum
		if ctl.Len() > 0 {
			h := sha256.New()
			h.Write(top.sum[:])
			_, _ = io.WriteString(h, ctl.String())
			h.Sum(sum[:0])
		}
		if x, ok := n.(*ast.CallExpr); ok {
			if sel, isSel := ast.Unparen(x.Fun).(*ast.SelectorExpr); s.assertion(x) || isSel && slices.Contains(goSkips, sel.Sel.Name) {
				e := sha256.Sum256(append(sum[:], s.canon(x)...))
				out.WriteString(hex.EncodeToString(e[:8]))
			}
		}
		stack = append(stack, level{n, sum})
		return true
	})
	return out.String()
}

// control writes a control's keyword and its parts' canonical text into b.
func (s *pkgScan) control(b *strings.Builder, kw string, parts ...ast.Node) {
	b.WriteString(kw)
	for _, p := range parts {
		b.WriteString("; ")
		if p != nil { // an absent Init, Cond, Post, Tag or Comm
			s.canonTo(b, p)
		}
	}
}

// compareCases blocks a base case identity the candidate's test lacks, per
// build-constraint alternative, and warns of a changed print or row when
// nothing blocks the test.
func (r *run) compareCases(add func(*[]Block, string, string), b, a pkgFacts) {
	for _, k := range slices.Sorted(maps.Keys(b.cases)) {
		bt, at, alts := b.tests[k], a.tests[k], a.cases[k]
		if bt == nil || at == nil || len(alts) == 0 {
			continue // main's comparison reads the test's absence
		}
		warn := false
		for _, c := range slices.Sorted(maps.Keys(b.cases[k])) {
			bs, as := b.cases[k][c], alts[c]
			if as == nil { // its constraint changed: what every alternative holds
				as = common(slices.Collect(maps.Values(alts))...)
			} else {
				warn = warn || bs.print != as.print && bt.asserts == at.asserts && bt.skips == at.skips || rowsChanged(bs, as)
			}
			switch first, n := casesRemoved(bs, as); {
			case n > 0 && as.opaque:
				add(&r.blocks, k, fmt.Sprintf("unverifiable: %d test cases no longer where the guard can follow them (%.80s)", n, first))
			case n > 0:
				add(&r.blocks, k, fmt.Sprintf("%d test cases removed (table rows or subtests: %.80s)", n, first))
			}
		}
		if warn && !slices.ContainsFunc(r.blocks, func(x Block) bool { return x.Path == k }) {
			add(&r.warnings, k, "an assertion, its condition or a test case changed: review it")
		}
	}
}

// common is what every summary holds: each identity's least count; opaque
// when any is. It has no rows and no print.
func common(cs ...*caseSum) *caseSum {
	out := &caseSum{ids: maps.Clone(cs[0].ids), rows: map[string]string{}}
	for _, c := range cs {
		out.opaque = out.opaque || c.opaque
		for id, n := range out.ids {
			if out.ids[id] = min(n, c.ids[id]); out.ids[id] == 0 {
				delete(out.ids, id)
			}
		}
	}
	return out
}

// casesRemoved is the first base case identity the candidate holds fewer
// of, and how many are fewer. A row whose loop only ran subtests and now
// checks, at its position, is kept (a strengthening); the reverse is
// removed, and a subtest's name is compared exactly (Astra r2 2).
func casesRemoved(b, a *caseSum) (first string, n int) {
	for _, k := range slices.Sorted(maps.Keys(b.ids)) {
		have := a.ids[k]
		if c, ok := strings.CutSuffix(k, " runs"); ok && strings.HasPrefix(k, "row ") {
			have += max(0, a.ids[c+" checks"]-b.ids[c+" checks"])
		}
		if d := b.ids[k] - have; d > 0 {
			if first == "" {
				first = k
			}
			n += d
		}
	}
	return first, n
}

// rowsChanged: a case both hold has another row text.
func rowsChanged(b, a *caseSum) bool {
	for k, t := range b.rows {
		if u, ok := a.rows[k]; ok && u != t {
			return true
		}
	}
	return false
}
