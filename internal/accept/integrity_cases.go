// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/constant"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
)

// P14d-b1 (#226 entries 74, 85, 99, 106; Astra #255 H5, M9; Astra #292 H4
// and round 2's 2, 3, 6, 7; Opus #318; Astra #318 1), on the package scan
// of each version and platform: the direct forms of a test's cases only.
// It only adds blocks and warnings to main's comparison. Decision A: what
// the guard cannot follow is named in a block, never modelled further;
// opacity never hides a removal: a base identity the candidate lacks
// blocks, as unverifiable when opaque.
//   - A test's cases are identities, each counted (a name may repeat):
//   - each row of a literal table (slice, array or map; inline, or a local
//     the test uses only as a range's expression: any other use, an alias,
//     a copy, delete, clear, an argument, an address or a write, makes it
//     opaque) a loop ranges over when its own body, outside any function
//     literal, asserts or skips (checks) or calls t.Run (runs): a check or
//     a t.Run inside a literal, a helper or a closure is no evidence (Astra
//     #318 1). A row's identity is its loop's (its position among the
//     test's driving loops, and runs, checks, or runs checks: a row moved
//     under another loop is removed) and its own: its map key, else its
//     first string literal (or a call's: append(id, "commit")); a row with
//     neither is counted per loop, "(nameless)", its text in its hash. A
//     row whose loop ran subtests and now runs them and checks is kept (a
//     strengthening); any other change of the loop's kind removes it (Opus
//     #318 H1); a subtest's name is never so read (Astra r2 2).
//   - each t.Run with a constant name, nested names joined by "/" (a
//     prefix past maxPrefix bytes is a digest: Opus #318 H5), read through
//     its callback when that is a function literal; a t.Run named by
//     neither a constant nor a row is a source the guard cannot follow.
//   - Only the test's own body is read, and a t.Run's literal callback:
//     any other function literal (called, deferred, given to Cleanup or to
//     a helper, or never run) names no case and makes the summary opaque; a
//     helper, a named callback or a closure is not followed. Following them
//     is P14d-b2 (#226 entry 106). A branch a constant condition never
//     takes names no case: an if's, a for false's, and a switch clause a
//     constant condition or tag never selects (Mythos #344 H1).
//   - A test's cases are kept per build-constraint alternative (Astra #292
//     H4): a base alternative is compared with the candidate's of the same
//     constraint, else with what every candidate alternative holds.
//   - A test's print is its assertions and skips, in order, each a hash of
//     its canonical text and a rolling hash of every control statement and
//     branch around it (an if with its Init, an else with the if it holds,
//     for, switch and type switch with Init, select, case and comm
//     clauses), each control's text built once (Astra r2 3, 7). With equal
//     counts, a changed print warns; a changed row under its identity (a
//     running hash of its text: Opus #318 H5), an early exit (return,
//     break, continue, goto) and a direct t.Run (its name's value and its
//     callback: Mythos #344 M1) warn whatever the counts, when nothing
//     blocks the test. The text of an event and of a control reads the
//     bodies of the function literals in it as a marker: each byte once.
//   - Work: canonical text, bodies and each identity as it is built are
//     charged to the case layer's own bound, one counter per run
//     (r.caseWork against maxCaseWork, scaled to maxRead as P14d-a's skip
//     bound, apart from the run's r.work: Opus #318 H6, r2 M1); every
//     visitor checks it, and the run's cancellation, before each node
//     (Astra r2 6; Astra #318 3); past it, reading stops and the test's
//     file is unverifiable.
//   - New blocks main has none of, by design: a removed row or subtest
//     (the purpose); and, as costs:
//   - a driving loop inserted before an existing one (its position
//     shifts);
//   - a table used anywhere but as its range's expression, or moved to a
//     package variable;
//   - a loop's kind changed other than runs to runs checks, its check or
//     its t.Run moved out of its own body (into a literal, a closure or a
//     helper) included;
//   - a row's first string literal edited (a []string value included),
//     and a name added to nameless rows;
//   - a subtest moved into anything but a t.Run's literal callback (a
//     literal, a closure, a helper);
//   - a local table ranged by many loops, each hashing its rows, past the
//     work bound (Mythos #344 L3: pathological, bounded).
//   - Residual (P14d-b2): the cases of helpers, named callbacks, closures
//     and literals; helpers of other packages, testify suites, a method
//     whose receiver holds the testing value, and a table built by appends
//     or a helper. A row its loop's own code skips (if tc.name == "zero" {
//     continue }) warns, the exit an event (Mythos #344 residual).

// maxPrefix is an identity's subtest prefix, past which it is a digest.
const maxPrefix = 256

// maxCaseWork is the case layer's own work bound per run (Opus #318 H6;
// r2 M1), scaled as P14d-a's maxSkipWork: one counter, r.caseWork, apart
// from the run's r.work, across every scan, version and platform. Var:
// tests lower it.
var maxCaseWork = 16 * len(platforms) * int(maxRead)

// caseScan is a package scan's case state: the branches a constant
// condition never takes, and the file being read with its source (canon's).
type caseScan struct {
	dead map[ast.Node]bool
	file *token.File
	src  []byte
}

// loopKind is what a range statement's own body does, outside any function
// literal: asserts or skips (checks), calls t.Run (runs).
type loopKind struct{ checks, runs bool }

// caseAlts is a test's cases per build-constraint alternative.
type caseAlts = map[string]*caseSum

// caseSum is a test's cases: identities ("row …" or "run …") counted, a
// running hash of each row's canonical text, whether a case source is one
// the guard cannot follow, and its print.
type caseSum struct {
	ids    map[string]int
	rows   map[string][32]byte
	opaque bool
	print  string
	exits  string // the print's early exits and subtests, compared whatever the counts (Opus #344 r2 L2; Mythos #344 M1)
}

// caseVisits counts the loops whose kind was read and caseCharged the
// case code's charged work (tests read them: bounds, never a clock).
var caseVisits, caseCharged atomic.Int64

// caseCharge adds n to the case layer's work; false past its bound.
func (s *pkgScan) caseCharge(n int) bool {
	s.r.caseWork += n
	caseCharged.Add(int64(n))
	return !s.over()
}

// over: the case layer is past its bound, or the run is cancelled.
func (s *pkgScan) over() bool { return s.r.caseWork > maxCaseWork || s.r.ctx.Err() != nil }

// caseFacts records fn's cases under the test key k and its build
// constraint cons; past the case layer's bound, its file is unverifiable.
func (s *pkgScan) caseFacts(fa *pkgFacts, k, path, cons string, fn *ast.FuncDecl, file *token.File, src []byte) {
	if s.cs.dead == nil {
		s.cs.dead = map[ast.Node]bool{}
	}
	s.cs.file, s.cs.src = file, src
	c := s.cases(fn.Body)
	c.print, c.exits = s.testPrint(fn.Body)
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

// caseWalk is one test's reading: its summary, its local tables, the
// rows' traced names, and its driving loops' count.
type caseWalk struct {
	sum    *caseSum
	kinds  map[*ast.RangeStmt]loopKind
	lits   map[types.Object]*ast.CompositeLit
	traced map[types.Object]bool
	loops  int
}

// cases reads a test body's cases.
func (s *pkgScan) cases(body *ast.BlockStmt) *caseSum {
	w := &caseWalk{sum: &caseSum{ids: map[string]int{}, rows: map[string][32]byte{}}, traced: map[types.Object]bool{}}
	if !s.caseCharge(int(body.End() - body.Pos())) {
		return w.sum
	}
	w.lits, w.kinds = s.locals(body), s.kinds(body)
	s.walk(w, body, "")
	return w.sum
}

// locals are body's local tables the guard follows: a table whose every
// use is a range's expression (Astra #292 H1).
func (s *pkgScan) locals(body ast.Node) map[types.Object]*ast.CompositeLit {
	lits := map[types.Object]*ast.CompositeLit{}
	define := func(id *ast.Ident, v ast.Expr) {
		if x, ok := ast.Unparen(v).(*ast.CompositeLit); ok {
			lits[s.info.Defs[id]] = x
		}
	}
	uses, kept := map[types.Object]int{}, map[types.Object]int{}
	ast.Inspect(body, func(n ast.Node) bool {
		if s.over() {
			return false
		}
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
		case *ast.Ident:
			uses[s.info.Uses[x]]++
		}
		return true
	})
	for o, n := range uses {
		if n != kept[o] {
			delete(lits, o)
		}
	}
	return lits
}

func (s *pkgScan) runCall(c *ast.CallExpr) bool {
	sel, ok := ast.Unparen(c.Fun).(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Run" && len(c.Args) == 2 && slices.Contains([]string{"testing.T", "testing.B"}, s.tval(sel.X))
}

// markDead records the branch of x a constant condition never takes
// (before ast.Inspect reaches it: pre-order).
//
// An if's branch; a for whose condition is constant false; in a tagless
// switch, a clause whose every expression is constant false and every
// clause after one that is constant true; with a constant tag, a clause
// whose expressions are all constants unequal to it and every clause after
// one equal to it (Mythos #344 H1). A switch it cannot so decide names its
// cases as written: what it reads as dead names no case, never more.
func (s *pkgScan) markDead(n ast.Node) {
	switch x := n.(type) {
	case *ast.IfStmt:
		if s.constCond(x.Cond, false) {
			s.cs.dead[x.Body] = true
		} else if s.constCond(x.Cond, true) && x.Else != nil {
			s.cs.dead[x.Else] = true
		}
	case *ast.ForStmt:
		if x.Cond != nil && s.constCond(x.Cond, false) {
			s.cs.dead[x.Body] = true
		}
	case *ast.SwitchStmt:
		var tag constant.Value
		if x.Tag != nil {
			if tag = s.info.Types[x.Tag].Value; tag == nil {
				return // not constant: every clause may run
			}
		}
		taken := false
		for _, st := range x.Body.List {
			cc, ok := st.(*ast.CaseClause)
			if !ok || cc.List == nil { // default: taken whenever no clause is
				continue
			}
			if taken || !slices.ContainsFunc(cc.List, func(e ast.Expr) bool { return !s.never(e, tag) }) {
				s.cs.dead[cc] = true
			}
			taken = taken || slices.ContainsFunc(cc.List, func(e ast.Expr) bool { return s.always(e, tag) })
		}
	}
}

// never: a switch clause's expression e is a constant that never selects
// it (false with no tag; unequal to the constant tag).
func (s *pkgScan) never(e ast.Expr, tag constant.Value) bool {
	if tag == nil {
		return s.constCond(e, false)
	}
	v := s.info.Types[e].Value
	return v != nil && v.Kind() == tag.Kind() && !constant.Compare(v, token.EQL, tag)
}

// always: e is a constant that always selects its clause.
func (s *pkgScan) always(e ast.Expr, tag constant.Value) bool {
	if tag == nil {
		return s.constCond(e, true)
	}
	v := s.info.Types[e].Value
	return v != nil && v.Kind() == tag.Kind() && constant.Compare(v, token.EQL, tag)
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

// walk reads n's cases under prefix: a t.Run's literal callback is read
// in place; any other function literal names no case, and makes the
// summary opaque (Astra #318 1).
func (s *pkgScan) walk(w *caseWalk, n ast.Node, prefix string) {
	ast.Inspect(n, func(n ast.Node) bool {
		if n == nil || s.over() || s.cs.dead[n] {
			return false
		}
		s.markDead(n)
		switch x := n.(type) {
		case *ast.FuncLit:
			w.sum.opaque = true
			return false
		case *ast.RangeStmt:
			s.rows(w, x, prefix)
		case *ast.CallExpr:
			if s.runCall(x) {
				s.run(w, x, prefix)
				return false
			}
		}
		return true
	})
}

// kinds is, for each range statement of body, what its own body does,
// outside any function literal (Astra #318 1): asserts or skips on a
// testing value (checks), calls t.Run (runs); a loop holds what the loops
// inside it hold. One bottom-up pass, each node read once and the body
// charged once (Opus #344 L1).
func (s *pkgScan) kinds(body ast.Node) map[*ast.RangeStmt]loopKind {
	out := map[*ast.RangeStmt]loopKind{}
	if !s.caseCharge(int(body.End() - body.Pos())) {
		return out
	}
	type frame struct {
		loop *ast.RangeStmt // nil: a function literal, which no loop outside it holds
		k    loopKind
	}
	var nodes []ast.Node
	var frames []frame
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			top := nodes[len(nodes)-1]
			nodes = nodes[:len(nodes)-1]
			switch top.(type) {
			case *ast.RangeStmt, *ast.FuncLit:
				f := frames[len(frames)-1]
				frames = frames[:len(frames)-1]
				if f.loop == nil {
					break
				}
				out[f.loop] = f.k
				caseVisits.Add(1) // a loop read: none past the bound
				if p := len(frames) - 1; p >= 0 && frames[p].loop != nil {
					frames[p].k = loopKind{frames[p].k.checks || f.k.checks, frames[p].k.runs || f.k.runs}
				}
			}
			return true
		}
		if s.over() || s.cs.dead[n] {
			return false
		}
		s.markDead(n)
		switch x := n.(type) {
		case *ast.RangeStmt:
			frames = append(frames, frame{loop: x})
		case *ast.FuncLit:
			frames = append(frames, frame{})
		case *ast.CallExpr:
			if p := len(frames) - 1; p >= 0 && frames[p].loop != nil && x.Pos() >= frames[p].loop.Body.Pos() {
				sel, isSel := ast.Unparen(x.Fun).(*ast.SelectorExpr)
				k := &frames[p].k
				k.runs = k.runs || s.runCall(x) // its literal callback is the subtest's (a function literal)
				k.checks = k.checks || s.assertion(x) || isSel && slices.Contains(goSkips, sel.Sel.Name) && s.tval(sel.X) != ""
			}
		}
		nodes = append(nodes, n)
		return true
	})
	return out
}

// rows records a driving loop's rows. Its label is its position and what
// it does: runs, checks, or runs checks (Opus #318 H1).
func (s *pkgScan) rows(w *caseWalk, x *ast.RangeStmt, prefix string) {
	k := w.kinds[x]
	checks, runs := k.checks, k.runs
	if !checks && !runs {
		return
	}
	w.loops++
	var kind []string
	if runs {
		kind = append(kind, "runs")
	}
	if checks {
		kind = append(kind, "checks")
	}
	loop := strconv.Itoa(w.loops) + " " + strings.Join(kind, " ")
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
		if tv := s.info.Types[key]; tv.Value != nil && tv.Value.Kind() == constant.String {
			name, named = strconv.Quote(constant.StringVal(tv.Value)), true // a constant key by its value (Mythos #344 L2)
		}
		if isMap && !named {
			name = s.canon(key) // a map's key names its row
		} else if !named {
			name = "(nameless)" // counted (Opus #318 M1): its text is the row's hash's
		}
		k := "row " + prefix + name + " @" + loop
		if !s.caseCharge(len(k)) {
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

// run records a t.Run's identity and reads its callback under its name,
// when that is a function literal.
func (s *pkgScan) run(w *caseWalk, x *ast.CallExpr, prefix string) {
	name := "*" // a traced row's name: its row is the identity
	if tv := s.info.Types[x.Args[0]]; tv.Value != nil && tv.Value.Kind() == constant.String {
		name = constant.StringVal(tv.Value) // a literal or a constant
		id := "run " + prefix + name
		if !s.caseCharge(len(id)) {
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
	if fl, ok := ast.Unparen(x.Args[1]).(*ast.FuncLit); ok {
		s.walk(w, fl.Body, next)
	}
}

// canon is n's canonical text: go/printer's, without positions (gofmt's
// layout) or comments; charged to the work bound.
func (s *pkgScan) canon(n ast.Node) string {
	var b strings.Builder
	s.canonTo(&b, n)
	return b.String()
}

// eventText is a print event's canonical text: canon's, the bodies of the
// function literals in it read as a marker, as their assertions are events
// of their own (Opus #344 r2 L1: each byte read once, however deeply
// asserting literals nest).
func (s *pkgScan) eventText(n ast.Node) string {
	var bodies []ast.Node
	ast.Inspect(n, func(x ast.Node) bool {
		if fl, ok := x.(*ast.FuncLit); ok {
			bodies = append(bodies, fl.Body)
			return false
		}
		return !s.over()
	})
	var b strings.Builder
	s.canonSkip(&b, n, bodies)
	return b.String()
}

// It is n's source span read by go/scanner, each token once, separated by
// one space (canonTokens: what gofmt changes is normalized); its work is linear in the span, which is charged
// before it is read (Opus #344 H1: go/printer's grows with the nesting of
// function literals). A node of another file than the one read is opaque
// text.
func (s *pkgScan) canonTo(b *strings.Builder, n ast.Node) { s.canonSkip(b, n, nil) }

// canonSkip is canonTo with the spans of skip (nodes within n, in source
// order, none within another) read as a marker, "{…}", and not charged.
func (s *pkgScan) canonSkip(b *strings.Builder, n ast.Node, skip []ast.Node) {
	f := s.cs.file
	if f == nil || f.Size() != len(s.cs.src) || f.Base() > int(n.Pos()) || int(n.End()) > f.Base()+f.Size() {
		b.WriteString("?")
		return
	}
	t := canonTokens{b: b}
	at := n.Pos()
	for _, k := range append(skip, nil) {
		end := n.End()
		if k != nil {
			end = k.Pos()
		}
		span := s.cs.src[f.Offset(at):f.Offset(end)]
		if !s.caseCharge(len(span)) {
			break
		}
		t.scan(span)
		if k == nil {
			break
		}
		t.toks = append(t.toks, canonToken{token.ILLEGAL, "{…}"})
		at = k.End()
	}
	t.flush()
}

// canonTokens are a canonical text's tokens, written by flush one space
// apart: comments and semicolons (written, or a newline's) dropped; a
// comma before a closing bracket dropped; doubled parentheses collapsed
// (((x)) is (x)); a number's prefix and exponent letters lower case, and an
// integer imaginary literal's leading zeros trimmed. So gofmt changes none
// (Opus #344 r2 M1; Mythos #344 L1).
type canonTokens struct {
	b    *strings.Builder
	toks []canonToken
}

type canonToken struct {
	tok token.Token
	lit string
}

func (t *canonTokens) scan(span []byte) {
	var sc scanner.Scanner
	sc.Init(token.NewFileSet().AddFile("", -1, len(span)), span, nil, 0)
	for {
		_, tok, lit := sc.Scan()
		switch tok {
		case token.EOF:
			return
		case token.SEMICOLON: // a newline's or a written one: gofmt moves between them
			continue
		case token.INT, token.FLOAT, token.IMAG:
			lit = number(lit)
		}
		if lit == "" {
			lit = tok.String()
		}
		t.toks = append(t.toks, canonToken{tok, lit})
	}
}

func (t *canonTokens) flush() {
	n := len(t.toks)
	match, open, drop := make([]int, n), []int{}, make([]bool, n)
	for i, x := range t.toks {
		match[i] = -1
		switch x.tok {
		case token.LPAREN:
			open = append(open, i)
		case token.RPAREN:
			if k := len(open); k > 0 {
				match[open[k-1]], match[i] = i, open[k-1]
				open = open[:k-1]
			}
		}
	}
	for i := 0; i+1 < n; i++ { // ( ( … ) ): the outer pair goes
		if t.toks[i].tok == token.LPAREN && t.toks[i+1].tok == token.LPAREN && match[i] >= 0 && match[i] == match[i+1]+1 {
			drop[i], drop[match[i]] = true, true
		}
	}
	first := true
	for i, x := range t.toks {
		if drop[i] {
			continue
		}
		if x.tok == token.COMMA {
			next := i + 1
			for next < n && drop[next] {
				next++
			}
			if next < n && (t.toks[next].tok == token.RBRACE || t.toks[next].tok == token.RPAREN || t.toks[next].tok == token.RBRACK) {
				continue
			}
		}
		if !first {
			t.b.WriteByte(' ')
		}
		first = false
		t.b.WriteString(x.lit)
	}
}

// number is lit with its base prefix's letter and its exponent's in lower
// case, as gofmt writes them; a hexadecimal mantissa's digits are kept.
func number(lit string) string {
	if t := strings.TrimLeft(strings.TrimSuffix(lit, "i"), "0"); strings.HasSuffix(lit, "i") && t != "" && strings.Trim(t, "0123456789_") == "" {
		lit = t + "i" // an integer imaginary literal: gofmt trims its leading zeros (Mythos #344 L1)
	}
	b := []byte(lit)
	hex := len(b) > 1 && b[0] == '0' && (b[1] == 'x' || b[1] == 'X')
	for i, c := range b {
		switch {
		case i == 1 && b[0] == '0' && (c == 'X' || c == 'B' || c == 'O'):
			b[i] = c + 'a' - 'A'
		case hex && c == 'P', !hex && c == 'E':
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
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
func (s *pkgScan) testPrint(n ast.Node) (string, string) {
	var out, ctl, exits strings.Builder
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
		s.markDead(n)
		switch x := n.(type) {
		case *ast.IfStmt:
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
		event, run := false, false
		switch x := n.(type) {
		case *ast.CallExpr:
			sel, isSel := ast.Unparen(x.Fun).(*ast.SelectorExpr)
			event = s.assertion(x) || isSel && slices.Contains(goSkips, sel.Sel.Name)
			run = s.runCall(x) // a subtest: its name and callback (Mythos #344 M1)
			event = event || run
		case *ast.ReturnStmt, *ast.BranchStmt: // an early exit, which may skip what follows (Opus #344 M1)
			event = true
		}
		if event {
			text := ""
			if x, ok := n.(*ast.CallExpr); ok && run {
				if tv := s.info.Types[x.Args[0]]; tv.Value != nil && tv.Value.Kind() == constant.String {
					text = "run " + strconv.Quote(constant.StringVal(tv.Value)) + " " // a constant name by its value, as its identity
				} else {
					text = "run " + s.canon(x.Args[0]) + " "
				}
				text += s.eventText(x.Args[1])
			} else {
				text = s.eventText(n)
			}
			e := sha256.Sum256(append(sum[:], text...))
			out.WriteString(hex.EncodeToString(e[:8]))
			if _, call := n.(*ast.CallExpr); !call || run {
				exits.WriteString(hex.EncodeToString(e[:8]))
			}
		}
		stack = append(stack, level{n, sum})
		return true
	})
	return out.String(), exits.String()
}

// control writes a control's keyword and its parts' canonical text into b.
func (s *pkgScan) control(b *strings.Builder, kw string, parts ...ast.Node) {
	b.WriteString(kw)
	for _, p := range parts {
		b.WriteString("; ")
		if p != nil { // an absent Init, Cond, Post, Tag or Comm
			b.WriteString(s.eventText(p)) // its literals' bodies a marker: their controls are levels of their own (Mythos #344 L3)
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
				warn = warn || bs.print != as.print && bt.asserts == at.asserts && bt.skips == at.skips || bs.exits != as.exits || rowsChanged(bs, as)
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
