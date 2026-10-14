// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
)

// Writes into pytest objects (P14c-2a; #226 entry 84's monkeypatch and
// forgery-in-fixtures flag): pytest.Function.runtest replaced,
// request.node.obj rebound, an xfail marker added from a fixture. They
// block as pytest internals (decision A). Bound by a flow-sensitive pass
// over plain attribute chains: a pytest object reached through another
// module, a call result (getattr included), a with or walrus target, or a
// lambda is a stated residual.
var (
	// pySeeds root the pytest objects; request and pytestconfig join them
	// once a function takes them. pyUserData reaches the test's own values.
	pySeeds    = []string{"pytest", "_pytest", "pluggy", "unittest"}
	pyUserData = []string{"param", "cls", "instance", "module", "function"}
	pyMutators = strings.Fields("append extend insert remove pop clear update setdefault add discard popitem sort reverse " +
		"register unregister add_marker applymarker")
	pySetters = strings.Fields("setattr delattr setitem delitem patch object")
	pyTarget  = regexp.MustCompile(`^[rRbBuU]*['"]+(_?pytest|pluggy|unittest)\b`) // a setter's string target
)

func pyRoots() map[string]bool {
	roots := map[string]bool{}
	for _, s := range pySeeds {
		roots[s] = true
	}
	return roots
}

// pyWrites updates roots, the names bound to a pytest object so far: a
// seed; an import, an assignment or a for target of a plain attribute
// chain from one; a class based on one; request and pytestconfig once a
// function takes them. It returns the roots line t (code: with its
// decorators and f-string fields) writes into or hands out: an assignment
// or del target (a bare name only if a seed), a mutating call's object, a
// setter's first argument, a chain returned or yielded.
func pyWrites(t, code []ptok, roots map[string]bool, kw, name string, work *int) (out []string) {
	write := func(part []ptok, store bool) {
		if root := pyRooted(part, roots); root != "" && (!store || len(part) > 1 || slices.Contains(pySeeds, root)) {
			out = append(out, root)
		}
	}
	bind := func(from []ptok, to ...[]ptok) {
		for _, part := range to {
			if root := pyChain(from); root != "" && roots[root] && len(part) > 0 {
				roots[part[len(part)-1].text] = true
			}
		}
	}
	switch h := pySplit(t, ":")[0]; {
	case t[0].text == "import" || t[0].text == "from":
		mod, i := "", slices.IndexFunc(t, func(k ptok) bool { return k.text == "import" })
		if t[0].text == "from" && len(t) > 1 {
			mod = t[1].text
		}
		for _, part := range pySplit(slices.DeleteFunc(slices.Clone(t[i+1:]), func(k ptok) bool { return k.text == "(" || k.text == ")" }), ",") {
			if len(part) > 0 {
				from, to := cmp.Or(mod, part[0].text), part[len(part)-1].text
				if !named(part, "as") {
					to = part[0].text
				}
				if from != to && slices.Contains(pySeeds, to) { // a seed rebound
					out = append(out, to)
				}
				roots[to] = roots[to] || roots[from] || roots[part[0].text]
			}
		}
	case kw == "def" && (named(h, "request") || named(h, "pytestconfig")):
		roots["request"], roots["pytestconfig"] = true, true
	case kw == "class":
		roots[name] = roots[name] || slices.ContainsFunc(h, func(k ptok) bool { return roots[k.text] })
	case t[0].text == "for":
		if p := pySplit(h[1:], "in"); len(p) == 2 {
			bind(p[1], pySplit(p[0], ",")...)
		}
	}
	for _, st := range pySplit(t, ";") {
		if i := slices.IndexFunc(st, func(k ptok) bool { return k.text == "return" || k.text == "yield" || k.text == "del" }); i >= 0 && st[i].text == "del" {
			for _, part := range pySplit(st[i+1:], ",") {
				write(part, true)
			}
		} else if root := pyChain(st[i+1:]); i >= 0 && roots[root] { // handed out
			out = append(out, root)
		}
		parts := pyAssign(st)
		for _, target := range parts[:len(parts)-1] {
			for _, part := range pySplit(target, ",") {
				write(part, true)
			}
			bind(parts[len(parts)-1], pySplit(target, ",")...)
		}
	}
	for i, k := range code {
		switch {
		case i+1 >= len(code) || code[i+1].text != "(" || k.kind != 'n':
		case i > 1 && code[i-1].text == "." && slices.Contains(pyMutators, k.text):
			write(pyBack(code, i-2, work), false)
		case slices.Contains(pySetters, k.text):
			j, depth := i+2, 0
			for ; j < len(code) && *work <= maxWork && (depth > 0 || code[j].text != "," && code[j].text != ")"); j++ {
				*work, depth = *work+1, depth+strings.Count("([{", code[j].text)-strings.Count(")]}", code[j].text)
			}
			if arg := code[i+2 : j]; len(arg) == 1 && arg[0].kind == 's' && pyTarget.MatchString(arg[0].text) {
				out = append(out, "a string target")
			} else {
				write(arg, false)
			}
		}
	}
	return out
}

// pyRooted is the first root t names, not one reaching the test's own
// values (request.param).
func pyRooted(t []ptok, roots map[string]bool) string {
	for i, k := range t {
		if k.kind == 'n' && roots[k.text] && !(i+2 < len(t) && t[i+1].text == "." && slices.Contains(pyUserData, t[i+2].text)) {
			return k.text
		}
	}
	return ""
}

// pyChain is the root of t when t is a plain attribute chain (a.b.c) not
// reaching the test's own values.
func pyChain(t []ptok) string {
	for i, k := range t {
		if i%2 == 0 && k.kind != 'n' || i%2 == 1 && k.text != "." {
			return ""
		}
	}
	if len(t)%2 == 0 || len(t) > 2 && slices.Contains(pyUserData, t[2].text) {
		return ""
	}
	return t[0].text
}

// pyBack is the chain ending at t[j], back over names, dots and balanced
// brackets; its work is counted.
func pyBack(t []ptok, j int, work *int) []ptok {
	depth, i := 0, j
	for ; i >= 0 && *work <= maxWork; i-- {
		*work++
		if k := t[i]; k.kind == 'o' && strings.Contains(")]}", k.text) {
			depth++
		} else if k.kind == 'o' && strings.Contains("([{", k.text) && depth == 0 || depth == 0 && k.kind != 'n' && k.text != "." {
			break
		} else if k.kind == 'o' && strings.Contains("([{", k.text) {
			depth--
		}
	}
	return t[i+1 : j+1]
}

// pyAssign splits t at its statement-level assignment operators (before
// any lambda): the targets, then the value. An augmented operator stays on
// its target.
func pyAssign(t []ptok) (parts [][]ptok) {
	if i := slices.IndexFunc(t, func(k ptok) bool { return k.text == "lambda" }); i >= 0 {
		t = t[:i]
	}
	depth, from := 0, 0
	for i, k := range t {
		prev, next := "", ""
		if i > 0 {
			prev = t[i-1].text
		}
		if i+1 < len(t) {
			next = t[i+1].text
		}
		if k.kind == 'o' && strings.Contains("([{", k.text) {
			depth++
		} else if k.kind == 'o' && strings.Contains(")]}", k.text) {
			depth--
		} else if depth == 0 && k.text == "=" && next != "=" && !strings.Contains("=!:", cmp.Or(prev, "x")) &&
			(prev != "<" && prev != ">" || i > 1 && t[i-2].text == prev) {
			parts, from = append(parts, t[from:i]), i+1
		}
	}
	return append(parts, t[from:])
}

// pySplit splits t at the bracket-level-0 tokens named by at.
func pySplit(t []ptok, at ...string) (parts [][]ptok) {
	depth, from := 0, 0
	for i, k := range t {
		if k.kind == 'o' && strings.Contains("([{", k.text) {
			depth++
		} else if k.kind == 'o' && strings.Contains(")]}", k.text) {
			depth--
		} else if depth == 0 && slices.Contains(at, k.text) {
			parts, from = append(parts, t[from:i]), i+1
		}
	}
	return append(parts, t[from:])
}
