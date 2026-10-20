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
// and round 2's 2, 3, 5 to 8; Opus #318), on the package scan of each
// version and platform. It only adds blocks and warnings to main's
// comparison. Decision A: what the guard cannot follow is named in a
// block, never modelled further; opacity never hides a removal: a base
// identity the candidate lacks blocks, as unverifiable when opaque.
//   - A test's cases are identities, each counted (a name may repeat):
//   - each row of a literal table (slice, array or map; inline, or a local
//     the test uses only as a range's expression: any other use, an alias,
//     a copy, delete, clear, an argument, an address or a write, makes it
//     opaque) a loop ranges over when its body asserts or skips (checks)
//     or runs subtests (runs): itself, through a local closure it calls,
//     or a helper running a subtest whatever its name (Opus #318 H3). A
//     row's identity is its loop's (its position among the test's or the
//     helper's, and runs, checks, or runs checks: a row moved under
//     another loop is removed) and its own: its map key, else its first
//     string literal (or a call's: append(id, "commit")); a row with
//     neither is counted per loop, "(nameless)", its text in its hash
//     (Opus #318 M1). A row whose loop ran subtests and now runs them and
//     checks is kept (a strengthening); any other change of the loop's
//     kind removes it (Opus #318 H1); a subtest's name is never so read
//     (Astra r2 2).
//   - each t.Run with a constant name, nested names joined by "/" (a
//     prefix past maxPrefix bytes is a digest: Opus #318 H5), read through
//     its callback: a literal, a local closure, or a package function (a
//     named callback: Astra H3).
//   - A function literal names cases only where it runs (Opus #318 H2):
//     called on the spot (defer and go too), a testing value's Run or
//     Cleanup callback, or an argument of a followed helper. A local
//     closure never assigned again nor addressed is read where it is
//     called, given to t.Cleanup or as t.Run's callback (Astra r2 8); one
//     also used as a value is followed so, and opaque (Opus #318 M2). Any
//     other literal names no case, and makes its summary opaque.
//   - A same-package helper called with a testing value, and a followed
//     closure, contribute their cases where they are called, under the
//     caller's subtest name: each one's summary is read once and composed
//     per call (Astra H3): a summary is read from its own function, at no
//     caller's depth, so no count depends on the order of the calls
//     (Astra r2 5). A loop whose drives' reading the depth cut is opaque,
//     labelled deep when nothing else is known (Opus #318 M3). A helper
//     declared more than once (race, !race) whose declarations name or run
//     cases is one identity carrying a digest of every declaration's
//     canonical text (Opus #318 H4, the lead's decision). A cycle, a t.Run
//     named by neither a constant nor a row, and a loop over another
//     source are sources the guard cannot follow (opaque). A branch a
//     constant condition never takes names no case.
//   - A test's cases are kept per build-constraint alternative (Astra H4):
//     a base alternative is compared with the candidate's of the same
//     constraint, else with what every candidate alternative holds.
//   - A test's print is its assertions and skips, in order, each a hash of
//     its canonical text and a rolling hash of every control statement and
//     branch around it (an if with its Init, an else with the if it holds,
//     for, switch and type switch with Init, select, case and comm
//     clauses), each control's text built once (Astra r2 3, 7). With equal
//     counts, a changed print warns; a changed row under its identity (a
//     running hash of its text: Opus #318 H5) warns whatever the counts,
//     when nothing blocks the test.
//   - Work: canonical text, bodies, each identity as it is built, and
//     summaries are charged to the case layer's own bound per package scan
//     (maxCaseWork, apart from the run's r.work: Opus #318 H6), checked
//     right after each charge (Astra r2 6), with the run's cancellation;
//     past it, reading stops and the test's file is unverifiable. drives'
//     answers are cached per body, mode and depth (Astra r2 5).
//   - New blocks main has none of, by design: a removed row or subtest
//     (the purpose); and, as costs: a driving loop inserted before an
//     existing one (its position shifts); a table used anywhere but its
//     range; a loop's kind changed other than runs to runs checks; a row's
//     first string literal edited when it is not its name; a closure never
//     called whose literal held identities; any change to a helper of
//     several declarations that names or runs cases.
//   - Residual: helpers of other packages, testify suites, a table built by
//     appends or a helper, a row skipped by its loop's own code (if tc.name
//     == "zero" { continue }: Opus #318 L1).

// maxCaseDepth bounds drives' reading of closures within one another;
// maxPrefix, an identity's subtest prefix, past which it is a digest.
const maxCaseDepth, maxPrefix = 8, 256

// maxCaseWork is the case layer's own work bound per package scan (Opus
// #318 H6): canonical text, bodies, identities and summaries, kept apart
// from the run's r.work, as the skip and out layers keep theirs. Var:
// tests lower it.
var maxCaseWork = 1 << 24

// caseScan is a package scan's case state, made on its first test: its
// work, drives' answers, the summaries, the helpers being summarized and
// the branches a constant condition never takes.
type caseScan struct {
	work   int
	driven map[driveKey]drive
	sums   map[ast.Node]*caseSum
	on     map[*ast.FuncDecl]bool
	dead   map[ast.Node]bool
}

// caseAlts is a test's cases per build-constraint alternative.
type caseAlts = map[string]*caseSum

// caseSum is a test's or a helper's cases: identities ("row …", "run …"
// or "hlp …") counted, a running hash of each row's canonical text,
// whether a case source is one the guard cannot follow, whether a helper's
// cycle cut it (never cached), whether it runs a subtest whatever its name
// (Opus #318 H3), and (a test's) its print.
type caseSum struct {
	ids               map[string]int
	rows              map[string][32]byte
	opaque, cyc, runs bool
	print             string
}

func newSum() *caseSum { return &caseSum{ids: map[string]int{}, rows: map[string][32]byte{}} }

// drive is drives' answer: no, yes, or cut by the depth (Opus #318 M3).
type drive int8

const (
	driveNo drive = iota + 1 // ordered: max is the strongest answer
	driveCut
	driveYes
)

type driveKey struct {
	body  ast.Node
	runs  bool
	depth int
}

// caseVisits counts drives' readings and caseWork the case code's charged
// work (tests read them: bounds, never a clock).
var caseVisits, caseWork atomic.Int64

// charge adds n to the case layer's work; false past its bound.
func (s *pkgScan) charge(n int) bool {
	s.cs.work += n
	caseWork.Add(int64(n))
	return !s.over()
}

// over: the case layer is past its bound, or the run is cancelled.
func (s *pkgScan) over() bool { return s.cs.work > maxCaseWork || s.r.ctx.Err() != nil }

// caseFacts records fn's cases under the test key k and its build
// constraint cons; past the case layer's bound, its file is unverifiable.
func (s *pkgScan) caseFacts(fa *pkgFacts, k, path, cons string, fn *ast.FuncDecl) {
	if s.cs.driven == nil {
		s.cs = caseScan{0, map[driveKey]drive{}, map[ast.Node]*caseSum{}, map[*ast.FuncDecl]bool{}, map[ast.Node]bool{}}
	}
	c := s.cases(fn.Body, "")
	c.print = s.testPrint(fn.Body)
	if fa.cases[k] == nil {
		fa.cases[k] = caseAlts{}
	}
	if old := fa.cases[k][cons]; old != nil { // declared twice under one constraint: what both hold
		c = common(old, c)
	}
	if fa.cases[k][cons] = c; s.over() && fa.bad[path] == "" { // another layer's reason kept
		fa.bad[path] = "test cases or assertions past the guard's work bound"
	}
}

// caseWalk is one body's reading: its summary, its local tables and the
// closures followed (valued: also used as a value, so opaque), the rows'
// traced names, its loops' label and count, and its function's (the
// test's or the helper's: its closures' labels).
type caseWalk struct {
	sum      *caseSum
	lits     map[types.Object]*ast.CompositeLit
	closures map[types.Object]*ast.FuncLit
	valued   map[types.Object]bool
	defs     map[*ast.FuncLit]bool // followed closures' literals: read where called
	unread   map[*ast.FuncLit]bool // valued ones' literals: they may run unread
	traced   map[types.Object]bool
	self     string
	loops    int
	root     string
}

// cases reads body's cases; self labels its loops ("" a test's, "name#" a
// helper's).
func (s *pkgScan) cases(body *ast.BlockStmt, self string) *caseSum {
	w := &caseWalk{sum: newSum(), traced: map[types.Object]bool{}, self: self, root: self, defs: map[*ast.FuncLit]bool{}, unread: map[*ast.FuncLit]bool{}}
	if !s.charge(int(body.End() - body.Pos())) {
		return w.sum
	}
	w.lits, w.closures, w.valued = s.locals(body)
	for o, fl := range w.closures {
		w.defs[fl], w.unread[fl] = true, w.valued[o]
	}
	s.walk(w, body, "")
	return w.sum
}

// locals are body's local tables and closures the guard follows: a table
// whose every use is a range's expression (Astra #292 H1); a closure never
// assigned again nor addressed, valued when it has another use than a
// call or a testing value's Cleanup or Run callback (Opus #318 M2: it is
// followed where called, and opaque).
func (s *pkgScan) locals(body ast.Node) (map[types.Object]*ast.CompositeLit, map[types.Object]*ast.FuncLit, map[types.Object]bool) {
	lits, closures, valued := map[types.Object]*ast.CompositeLit{}, map[types.Object]*ast.FuncLit{}, map[types.Object]bool{}
	define := func(id *ast.Ident, v ast.Expr) {
		switch x := ast.Unparen(v).(type) {
		case *ast.CompositeLit:
			lits[s.info.Defs[id]] = x
		case *ast.FuncLit:
			closures[s.info.Defs[id]] = x
		}
	}
	uses, kept, changed := map[types.Object]int{}, map[types.Object]int{}, map[types.Object]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, l := range x.Lhs {
				if id := ident(l); id != nil && s.info.Defs[id] != nil && len(x.Lhs) == len(x.Rhs) {
					define(id, x.Rhs[i])
				} else if id != nil {
					changed[s.info.Uses[id]] = true
				}
			}
		case *ast.UnaryExpr:
			if id := ident(x.X); id != nil && x.Op == token.AND {
				changed[s.info.Uses[id]] = true
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
			valued[o] = true
		}
		if changed[o] {
			delete(closures, o)
		}
	}
	return lits, closures, valued
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

// walk reads n's cases under prefix. A function literal is read where it
// runs (Opus #318 H2): called on the spot (under defer or go too), a
// testing value's Run or Cleanup callback, or an argument of a followed
// helper; a followed closure where it is called; any other literal names
// no case, and makes the summary opaque.
func (s *pkgScan) walk(w *caseWalk, n ast.Node, prefix string) {
	inPlace := map[*ast.FuncLit]bool{}
	ast.Inspect(n, func(n ast.Node) bool {
		if n == nil || s.over() || s.cs.dead[n] {
			return false
		}
		switch x := n.(type) {
		case *ast.IfStmt:
			s.markDead(x)
		case *ast.FuncLit:
			if !inPlace[x] && (!w.defs[x] || w.unread[x]) {
				w.sum.opaque = true
			}
			return inPlace[x]
		case *ast.RangeStmt:
			s.rows(w, x, prefix)
		case *ast.CallExpr:
			if s.runCall(x) {
				s.run(w, x, prefix)
				return false
			}
			if fl, ok := ast.Unparen(x.Fun).(*ast.FuncLit); ok {
				inPlace[fl] = true
			}
			if cb := s.callback(x); cb != nil { // Cleanup
				if fl, ok := ast.Unparen(cb).(*ast.FuncLit); ok {
					inPlace[fl] = true
				} else {
					s.follow(w, cb, prefix)
				}
			}
			if w.closures[s.info.Uses[ident(x.Fun)]] != nil {
				s.follow(w, x.Fun, prefix)
			} else if d := s.helper(x); d != nil {
				s.compose(w, s.helperSum(d), prefix)
				for _, a := range x.Args {
					if fl, ok := ast.Unparen(a).(*ast.FuncLit); ok {
						inPlace[fl] = true
					}
				}
			}
		}
		return true
	})
}

// rows records a driving loop's rows. Its label is its position and what
// it does: runs, checks, or runs checks (Opus #318 H1); when the depth cut
// drives' reading, opaque, and deep if nothing else is known (M3).
func (s *pkgScan) rows(w *caseWalk, x *ast.RangeStmt, prefix string) {
	checks, runs := s.drives(x.Body, w.closures, false, 0), s.drives(x.Body, w.closures, true, 0)
	if checks == driveNo && runs == driveNo {
		return
	}
	w.loops++
	var kind []string
	if runs == driveYes {
		kind = append(kind, "runs")
	}
	if checks == driveYes {
		kind = append(kind, "checks")
	}
	if checks == driveCut || runs == driveCut { // what the depth hid is unknown: opaque, and deep when nothing else is known
		w.sum.opaque = true
		if kind == nil {
			kind = []string{"deep"}
		}
	}
	loop := w.self + strconv.Itoa(w.loops) + " " + strings.Join(kind, " ")
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
		name, named := s.rowName(key)
		if isMap && !named {
			name = s.canon(key) // a map's key names its row
		} else if !named {
			name = "(nameless)" // counted (Opus #318 M1): its text is the row's hash's
		}
		k := "row " + prefix + name + " @" + loop
		if !s.charge(len(k)) {
			return
		}
		w.sum.ids[k]++
		w.sum.rows[k] = rowChain(w.sum.rows[k], s.canon(e))
	}
}

// rowChain is h extended by text: a row's running hash (Opus #318 H5: never
// its text concatenated).
func rowChain(h [32]byte, text string) [32]byte {
	return sha256.Sum256(append(h[:], text...))
}

// run records a t.Run's identity and reads its callback under its name.
func (s *pkgScan) run(w *caseWalk, x *ast.CallExpr, prefix string) {
	w.sum.runs = true
	name := "*" // a traced row's name: its row is the identity
	if tv := s.info.Types[x.Args[0]]; tv.Value != nil && tv.Value.Kind() == constant.String {
		name = constant.StringVal(tv.Value) // a literal or a constant
		id := "run " + prefix + name
		if !s.charge(len(id)) {
			return
		}
		w.sum.ids[id]++
	} else if !mentionsAny(x.Args[0], s.info, w.traced) {
		w.sum.opaque, name = true, "?"
	}
	next := prefix + name + "/"
	if len(next) > maxPrefix { // a digest of a long one: identities grow linearly in their names (Opus #318 H5)
		d := sha256.Sum256([]byte(next))
		next = "#" + hex.EncodeToString(d[:8]) + "/"
	}
	s.follow(w, x.Args[1], next)
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
		w.sum.opaque = w.sum.opaque || w.valued[s.info.Uses[id]]
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
	sub.sum, sub.self, sub.loops = newSum(), w.root+name+"#", 0
	if !s.charge(int(fl.Body.End() - fl.Body.Pos())) { // each reading paid for
		return sub.sum
	}
	s.walk(&sub, fl.Body, "")
	s.keep(fl, sub.sum)
	return sub.sum
}

// helperSum is d's cases, read once per declaration. A helper declared
// more than once (race, !race), whose declarations name or run cases, is
// one identity carrying a digest of every declaration's canonical text
// (Opus #318 H4, the lead's decision): any change of one reads as an
// unverifiable removal.
func (s *pkgScan) helperSum(d *ast.FuncDecl) *caseSum {
	if h := s.cs.sums[d]; h != nil {
		return h
	}
	if s.cs.on[d] {
		return &caseSum{ids: map[string]int{}, rows: map[string][32]byte{}, opaque: true, cyc: true}
	}
	ds := s.same[declKey(d)]
	for _, x := range ds {
		s.cs.on[x] = true
	}
	var h *caseSum
	if len(ds) > 1 {
		h = &caseSum{ids: map[string]int{}, rows: map[string][32]byte{}, opaque: true}
		sum := sha256.New()
		for _, x := range ds {
			if x.Body == nil {
				continue
			}
			c := s.cases(x.Body, declKey(x)+"#")
			h.runs, h.cyc = h.runs || c.runs || len(c.ids) > 0, h.cyc || c.cyc
			text := *x
			text.Doc = nil // a comment changes no case
			_, _ = io.WriteString(sum, s.canon(&text)+"\x00")
		}
		if h.runs {
			h.ids["hlp "+declKey(d)+" "+hex.EncodeToString(sum.Sum(nil)[:8])] = 1
		}
	} else {
		h = s.cases(d.Body, declKey(d)+"#")
	}
	for _, x := range ds {
		delete(s.cs.on, x)
	}
	s.keep(d, h)
	return h
}

// keep caches a summary, unless a cycle cut it (its answer then depends on
// where it was first read), or past the bound.
func (s *pkgScan) keep(fn ast.Node, h *caseSum) {
	if !h.cyc && !s.over() {
		s.cs.sums[fn] = h
	}
}

// compose adds h's cases to w's under prefix, each identity charged as it
// is built (Opus #318 H5). A summary is the same wherever it is read
// (each read from its own function, at no caller's depth), so the order of
// calls never changes a count (Astra #292 r2 5).
func (s *pkgScan) compose(w *caseWalk, h *caseSum, prefix string) {
	w.sum.opaque, w.sum.cyc, w.sum.runs = w.sum.opaque || h.opaque, w.sum.cyc || h.cyc, w.sum.runs || h.runs
	for id, n := range h.ids {
		k := id[:4] + prefix + id[4:]
		if !s.charge(len(k)) {
			return
		}
		w.sum.ids[k] += n
		if t, ok := h.rows[id]; ok {
			w.sum.rows[k] = rowChain(w.sum.rows[k], string(t[:]))
		}
	}
}

// drives: a loop body asserts or skips (checks, runs false), or runs
// subtests (runs true): itself, through a local closure it calls, or (runs)
// a helper running a subtest whatever its name (Opus #318 H3), to a depth;
// past it, cut. Each body's answer is cached with its mode and depth; the
// bound is checked right after the body's charge, which pays for its
// reading.
func (s *pkgScan) drives(body ast.Node, closures map[types.Object]*ast.FuncLit, runs bool, depth int) (found drive) {
	key := driveKey{body, runs, depth}
	if v, ok := s.cs.driven[key]; ok {
		return v
	}
	if depth > maxCaseDepth {
		return driveCut
	}
	if !s.charge(int(body.End() - body.Pos())) {
		return driveNo
	}
	defer func() { s.cs.driven[key] = found }()
	caseVisits.Add(1) // a reading: none past the bound
	if c := s.count(body); !runs && c.asserts+c.skips > 0 {
		return driveYes
	}
	found = driveNo
	ast.Inspect(body, func(n ast.Node) bool {
		if x, ok := n.(*ast.CallExpr); ok {
			switch fl, d := closures[s.info.Uses[ident(x.Fun)]], s.helper(x); {
			case runs && s.runCall(x):
				found = driveYes
			case fl != nil:
				found = max(found, s.drives(fl.Body, closures, runs, depth+1))
			case runs && d != nil:
				if h := s.helperSum(d); h.runs || len(h.ids) > 0 {
					found = driveYes
				}
			}
		}
		return found != driveYes
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

// rowName is a row's name: a string key or value, else its first string
// literal field (or a call's first string literal argument, as in
// append(id, "commit")); else it has none.
func (s *pkgScan) rowName(e ast.Expr) (string, bool) {
	str := func(e ast.Expr) (string, bool) {
		lit, ok := ast.Unparen(e).(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(lit.Value)
		return v, err == nil
	}
	if v, ok := str(e); ok {
		return strconv.Quote(v), true
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
			return strconv.Quote(v), true
		}
	}
	return "", false
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
		if s.over() || s.cs.dead[n] {
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
				if s.control(&ctl, "", e); s.over() {
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
				var all []*caseSum
				for _, c := range slices.Sorted(maps.Keys(alts)) { // in order: the result never depends on a map's
					all = append(all, alts[c])
				}
				as = common(all...)
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
	out := &caseSum{ids: maps.Clone(cs[0].ids), rows: map[string][32]byte{}}
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
// runs them and checks, at its position, is kept (a strengthening); any
// other change of its loop's kind is a removal (Opus #318 H1), and a
// subtest's name is compared exactly (Astra r2 2).
func casesRemoved(b, a *caseSum) (first string, n int) {
	for _, k := range slices.Sorted(maps.Keys(b.ids)) {
		have := a.ids[k]
		if c, ok := strings.CutSuffix(k, " runs"); ok && strings.HasPrefix(k, "row ") {
			have += max(0, a.ids[c+" runs checks"]-b.ids[c+" runs checks"])
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
