// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"slices"
	"strings"
)

// P14c-2a-cond (Opus and Mythos #266 H2, Mythos #266 r2, Opus and Mythos
// #267): a skipif or xfail condition pytest would run as code. The shadow
// rules are P14c-2a-shadow's (#267 round 4, split per 04 §2).

// pySkipString: a skipif or xfail mark (pytest.mark.NAME(…), .with_args(…),
// .__call__(…), or a name bound to such a mark; not pytest.xfail() in a
// pure file) whose positional arguments or condition= may be a string:
// each must surely be a bool, and the call is not called again. t's first
// code tokens are its line's, the rest its f-string fields'.
func pySkipString(t []ptok, code int, pure bool, aliases map[string]bool) bool {
	for i := 0; i+1 < len(t); i++ {
		open := i + 1
		if i+3 < len(t) && t[i+1].text == "." && (t[i+2].text == "with_args" || t[i+2].text == "__call__") {
			open = i + 3
		}
		for open+1 < len(t) && t[open].text == ")" { // (pytest.mark.xfail)(…) (#267 round 7)
			open++
		}
		if t[i].text != "skipif" && t[i].text != "xfail" && !aliases[t[i].text] || t[open].text != "(" || pure && i > 1 && t[i-1].text == "." && t[i-2].text == "pytest" {
			continue
		}
		end, depth := open, 0
		for ; end < len(t); end++ {
			if depth += strings.Count("([{", t[end].text) - strings.Count(")]}", t[end].text); depth == 0 {
				break
			}
		}
		next := end + 1
		for next < len(t) && t[next].text == ")" { // closes a group opened before the mark: (xfail(False))('…'), (m := …)('…') (#267 round 7)
			next++
		}
		if next < len(t) && t[next].text == "(" && (i < code) == (next < code) { // xfail(False)("…") appends a condition (Mythos #267 M2), the mark and the call in one segment (r2 L2, r3 H2)
			return true
		}
		for _, a := range pySplit(t[open+1:min(end, len(t))], ",") {
			if kw := len(a) > 1 && a[0].kind == 'n' && a[1].text == "=" && (len(a) < 3 || a[2].text != "="); kw && (a[0].text == "condition" || t[i].text != "skipif" && t[i].text != "xfail" && !slices.Contains(pyMarkKw, a[0].text)) && !pyBool(a[2:]) || !kw && len(a) > 0 && !pyBool(a) {
				return true
			}
		}
	}
	return false
}

// pyMarkKw: a mark's keywords that are no condition; a factory's others are (Mythos #270 r2 M6).
var pyMarkKw = []string{"reason", "strict", "raises", "run"}

// pyBool: True, False, or and/or of operands each holding a comparison
// (not a walrus's =, << or >>) or led by not, with no if or else, at level 0.
func pyBool(t []ptok) bool {
	for _, op := range pySplit(t, "and", "or") {
		depth, sure := 0, len(op) == 1 && (op[0].text == "True" || op[0].text == "False") || len(op) > 1 && op[0].text == "not"
		for i, k := range op {
			if depth -= strings.Count(")]}", k.text); depth == 0 && (k.text == "if" || k.text == "else") {
				return false
			}
			cmp := strings.Contains("<>=!", k.text) && k.kind == 'o' && !(k.text == "=" && i > 0 && op[i-1].text == ":") &&
				!(strings.Contains("<>", k.text) && (i > 0 && op[i-1].text == k.text || i+1 < len(op) && op[i+1].text == k.text))
			sure, depth = sure || depth == 0 && (cmp || k.text == "is" || k.text == "in"), depth+strings.Count("([{", k.text)
		}
		if !sure {
			return false
		}
	}
	return len(t) > 0
}

// pyPure: the file binds pytest by import pytest and only so: no parameter,
// target, attribute or alias named pytest (Opus #267 M2, Mythos #267 M2), and
// no star import that may bind it (Mythos #267 r3: never vacuously);
// aliases are the names bound to a chain ending in .skipif or .xfail.
func pyPure(ctx context.Context, lines []pline) (pure bool, aliases map[string]bool) {
	pure, aliases = true, map[string]bool{}
	imported := false
	defer func() { pure = pure && imported }()
	var binds [][]string // each statement's bindings
	var owners []string  // each statement's def, by indent, and whether it holds a mark (Mythos #270 r2 H1)
	var marks []bool
	uses := map[string][]int{} // a name, to the statements holding it
	type def struct {
		indent int
		name   string
	}
	defs := []def{{-1, ""}}
	for _, l := range lines {
		if ctx.Err() != nil { // the caller reads ctx (Opus #270 L1)
			return pure, aliases
		}
		t, d := slices.Concat(l.toks, l.inner), l.toks[b2i(l.toks[0].text == "async"):]
		for l.indent <= defs[len(defs)-1].indent {
			defs = defs[:len(defs)-1]
		}
		owner := defs[len(defs)-1].name
		if len(d) > 1 && d[0].text == "def" {
			defs = append(defs, def{l.indent, d[1].text})
		}
		for _, st := range pySplit(t, ";") { // per statement: import pytest, os stays pure (r2 L1), import pytest; pytest = … does not (r3 H1)
			if len(st) == 0 { // x = 1; (r4 L)
				continue
			}
			b, imp := pyBinds(st)
			if slices.Contains(b, "pytest") && !(imp && st[0].text == "import" && !named(st, "as")) || imp && slices.ContainsFunc(st, func(k ptok) bool { return k.text == "*" }) {
				pure = false
			}
			imported = imported || imp && st[0].text == "import" && !named(st, "as") && slices.Contains(b, "pytest")
			for _, k := range st {
				uses[k.text] = append(uses[k.text], len(binds))
			}
			binds, owners = append(binds, b), append(owners, owner)
			marks = append(marks, slices.ContainsFunc(st, func(k ptok) bool { return k.text == "mark" }))
		}
		for i, k := range t {
			pure = pure && !(k.text == "pytest" && (i > 0 && slices.Contains([]string{".", "lambda"}, t[i-1].text) ||
				i+1 < len(t) && (t[i+1].text == ":" || t[i+1].text == "=" && (i+2 >= len(t) || t[i+2].text != "=")) ||
				d[0].text == "def" && i < len(pySplit(d, ":")[0])))
		}
	}
	// aliases: what a statement holding skipif, xfail or an alias binds, to a
	// fixed point (Mythos #267 r2 M3: an alias of an alias, a tuple, a ;),
	// each statement taken once (Opus #270 L1); and the def owning it, a
	// mark factory (Mythos #270 r2 H1, M1, L6), not pytest.xfail()'s
	taken := make([]bool, len(binds))
	for queue := []string{"skipif", "xfail"}; len(queue) > 0; queue = queue[1:] {
		for _, i := range uses[queue[0]] {
			if taken[i] {
				continue
			}
			taken[i] = true
			names := binds[i]
			if owners[i] != "" && (marks[i] || queue[0] != "skipif" && queue[0] != "xfail") {
				names = append(slices.Clone(names), owners[i])
			}
			for _, n := range names {
				if !aliases[n] {
					aliases[n], queue = true, append(queue, n)
				}
			}
		}
	}
	return pure, aliases
}

// pyNames are the plain names t reads (not attributes, keywords or
// keyword-argument names; Mythos #267 L4: == is not a keyword's =).
func pyNames(t []ptok) (names []string) {
	for i, k := range t {
		if k.kind == 'n' && (i == 0 || t[i-1].text != ".") && !slices.Contains(pyKeywords, k.text) &&
			(i+1 >= len(t) || t[i+1].text != "=" || i+2 < len(t) && t[i+2].text == "=") {
			names = append(names, k.text)
		}
	}
	return names
}
