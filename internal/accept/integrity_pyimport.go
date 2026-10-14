// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"path"
	"slices"
	"strings"
)

// The import-time allowlist (P14c-2a-allow; Opus #263 C1, H1, H3, M1): in
// test modules, conftests and changed modules under a test directory, code
// run at import may only import, define, bind constants to plain names,
// hold a docstring, or guard __main__ exactly; a constant calls only
// pytest.fixture, pytest.param, pytest.mark.NAME (and patch as a decorator).
// Findings unchanged from the base pass; application modules warn.
var (
	pyKeywords = strings.Fields("and or not in is if else elif for while async await lambda return yield assert del from import as with")
	// pyBound: names an import may bind only from these modules.
	pyBound = map[string]string{"pytest": "pytest", "fixture": "pytest.fixture", "mock": "unittest.mock", "patch": "unittest.mock.patch", "unittest": "unittest"}
)

// pyTestDir: p is under a test directory: one named test or tests, or one
// below the root holding a test module or conftest (base or candidate).
func (r *run) pyTestDir(p string) bool {
	for d := path.Dir(p); d != "."; d = path.Dir(d) {
		if b := path.Base(d); b == "test" || b == "tests" || slices.ContainsFunc(append(slices.Clone(r.baseDirs[d]), r.candDirs[d]...), pyTested) {
			return true
		}
	}
	return false
}

// chain is the attribute chain ending t ("pytest.mark.x").
func chain(t []ptok) string {
	j := len(t)
	for j > 0 && (t[j-1].kind == 'n' && !slices.Contains(pyKeywords, t[j-1].text) || t[j-1].text == ".") {
		j--
	}
	return pyJoin(t[j:])
}

// pyPytest: c is a chain the allowlist may call: exactly pytest.fixture,
// fixture, pytest.param, or pytest.mark.NAME (with_args); decorator adds
// unittest.mock's patch (patch, patch.object, mock.patch.dict, …). No
// segment may be private or a dunder (pytest.mark.__getattr__.…).
func pyPytest(c string, decorator bool) bool {
	s := strings.Split(c, ".")
	if slices.ContainsFunc(s, func(n string) bool { return strings.HasPrefix(n, "_") }) {
		return false
	}
	switch {
	case c == "fixture" || c == "pytest.fixture" || c == "pytest.param":
		return true
	case len(s) >= 3 && s[0] == "pytest" && s[1] == "mark":
		return len(s) == 3 || len(s) == 4 && s[3] == "with_args"
	}
	i := slices.Index(s, "patch")
	return decorator && i >= 0 && slices.Contains([]string{"", "mock", "unittest.mock"}, strings.Join(s[:i], ".")) && len(s) <= i+2
}

// pyCallAt: t[i] opens a call.
func pyCallAt(t []ptok, i int) bool {
	return t[i].text == "(" && t[i].kind == 'o' && i > 0 && (t[i-1].kind == 's' || t[i-1].kind == 'n' && !slices.Contains(pyKeywords, t[i-1].text) ||
		t[i-1].text == ")" || t[i-1].text == "]")
}

// pyConst: t evaluates no code of its own; ** unpacks only a literal dict
// ({**K} and f(**K) call K.keys(): Opus #266 H1).
func pyConst(t []ptok) bool {
	for i, k := range t {
		if pyCallAt(t, i) && !pyPytest(chain(t[:i]), false) || k.text == "await" || k.text == "yield" ||
			k.text == "*" && i+1 < len(t) && t[i+1].text == "*" && (i == 0 || strings.Contains("(,[{", t[i-1].text)) && (i+2 >= len(t) || t[i+2].text != "{") {
			return false
		}
	}
	return true
}

// pyCalls are the roots of what t calls ("": not a plain name).
func pyCalls(t []ptok) (calls []string) {
	for i := range t {
		if pyCallAt(t, i) && !strings.HasPrefix(chain(t[:i]), ".") {
			calls = append(calls, strings.Split(chain(t[:i]), ".")[0])
		} else if i > 0 && i+1 < len(t) && t[i].text == "*" && t[i-1].text == "*" {
			calls = append(calls, t[i+1].text)
		}
	}
	if t[0].text == "@" && len(t) > 1 {
		calls = append(calls, t[1].text)
	}
	return calls
}

// pyBinds are the names t binds: by an import (imported), def, class, = or :=.
func pyBinds(t []ptok) (names []string, imported bool) {
	target := false // for … in, and as: bindings too (Mythos #267 M3)
	for i, k := range t {
		target = k.text == "for" || target && k.text != "in"
		if k.kind == 'n' && (i+2 < len(t) && t[i+1].text == ":" && t[i+2].text == "=" || target && k.text != "for" || i > 0 && t[i-1].text == "as") {
			names = append(names, k.text)
		}
	}
	switch d := t[b2i(t[0].text == "async"):]; {
	case len(d) > 1 && (d[0].text == "def" || d[0].text == "class"):
		return append(names, d[1].text), false
	case len(d) > 1 && (d[0].text == "import" || d[0].text == "from"):
		for _, part := range pyImportParts(d) {
			names = append(names, part[len(part)-1].text, part[0].text)
		}
		return names, true
	}
	parts := pyAssign(t)
	for _, target := range parts[:len(parts)-1] {
		for _, k := range target {
			if k.kind == 'n' {
				names = append(names, k.text)
			}
		}
	}
	return names, false
}

// pyRebound: the file rebinds __name__ or star-imports (Opus #266 M2).
func pyRebound(lines []pline) bool {
	for _, l := range lines {
		t := slices.Concat(l.toks, l.inner)
		for i := range t {
			if t[i].text == "*" && (t[0].text == "from" || t[0].text == "import") || t[i].text == "__name__" && t[0].text == "case" || t[i].text == "__name__" && !pyMain(l.toks) &&
				!(i > 1 && i+1 < len(t) && t[i-1].text == "(" && t[i+1].text == ")" && t[i-2].kind == 'n' && !slices.Contains(pyKeywords, t[i-2].text)) {
				return true
			}
		}
	}
	return false
}

// pyMeta: a keyword or a foreign base may make a metaclass (dunders run at import).
func pyMeta(h []ptok, classes map[string]bool) bool {
	for i := 2; i < len(h); i++ {
		if h[i].text == "=" || h[i].kind == 'n' && h[i-1].text != "." && !classes[h[i].text] && h[i].text != "object" {
			return true
		}
	}
	return false
}

// pyAllowed is why the statement t, run at import, is outside the
// allowlist, or "".
func pyAllowed(t []ptok, conftest bool) string {
	d, why := t[b2i(t[0].text == "async"):], "code run at import outside the allowlist ("+ptext(t)+")"
	if len(d) == 0 {
		return why
	}
	ok := func(b bool) string { return map[bool]string{true: "", false: why}[b] }
	switch h := pySplit(d, ":"); {
	case d[0].text == "@":
		end := slices.IndexFunc(d, func(k ptok) bool { return k.text == "(" })
		if end < 0 {
			end = len(d)
		}
		c, depth, last := chain(d[:end]), 0, end
		for ; last < len(d); last++ { // exactly one balanced ( … ), then nothing (Opus #266 H1)
			if depth += strings.Count("([{", d[last].text) - strings.Count(")]}", d[last].text); depth == 0 {
				break
			}
		}
		return ok(c == pyJoin(d[1:end]) && pyPytest(c, true) && pyConst(d[end:]) && last >= len(d)-1)
	case (d[0].text == "import" || d[0].text == "from") && len(d) > 1:
		if slices.ContainsFunc(d, func(k ptok) bool { return k.text == "*" }) {
			return "a star import (" + ptext(t) + ")"
		}
		for _, part := range pyImportParts(d) {
			bound, src := part[len(part)-1].text, pyJoin(part[:len(part)-2*b2i(named(part, "as"))]) // the module or name it binds
			if d[0].text == "from" {
				src = pyJoin(d[1:max(slices.IndexFunc(d, func(k ptok) bool { return k.text == "import" }), 1)]) + "." + part[0].text
			} else if !named(part, "as") {
				bound, src = part[0].text, part[0].text
			}
			if want, kept := pyBound[bound]; kept && src != want || src == "pytest" && bound != "pytest" || slices.Contains(pyFixtures, bound) {
				return "an import binding pytest, fixture, mock, patch or a builtin fixture's name otherwise (" + ptext(t) + ")"
			}
		}
		return ""
	case d[0].text == "def" && len(h[0]) > 1:
		params := slices.Clone(h[0][2:]) // *args and **kwargs are parameters, not unpacking (Mythos #266 M2)
		for j := 1; j < len(params); j++ {
			if params[j].text == "*" && strings.Contains("(,*", params[j-1].text) {
				params, j = slices.Delete(params, j, j+1), j-1
			}
		}
		return ok(pyConst(params))
	case d[0].text == "class" && len(h[0]) > 1:
		return ok(pyConst(h[0][2:]) && !named(h[0], "metaclass") && !slices.ContainsFunc(h[0], func(k ptok) bool { return k.text == "*" }) &&
			len(t) == len(h[0])+1+b2i(t[len(t)-1].text == "pass"))
	case len(d) == 1 && (d[0].text == "pass" || d[0].kind == 's' && !strings.ContainsAny(d[0].text[:strings.IndexAny(d[0].text, `"'`)], "fFtT")):
		return ""
	}
	parts := pyAssign(t)
	for _, target := range parts[:len(parts)-1] {
		if !pyConst(target) {
			return why
		}
		for _, n := range pySplit(pySplit(target, ":")[0], ",") {
			if len(n) != 1 || n[0].kind != 'n' || slices.Contains(pyFixtures, n[0].text) || conftest && strings.HasPrefix(n[0].text, "collect_ignore") {
				return why
			}
		}
	}
	return ok(len(parts) > 1 && pyConst(t)) // the whole statement: a lambda cut pyAssign short
}

func pyJoin(t []ptok) string {
	var b strings.Builder
	for _, k := range t {
		b.WriteString(k.text)
	}
	return b.String()
}

// pyImportParts are the names an import statement binds, one per part.
func pyImportParts(t []ptok) (parts [][]ptok) {
	i := slices.IndexFunc(t, func(k ptok) bool { return k.text == "import" })
	for _, part := range pySplit(slices.DeleteFunc(slices.Clone(t[i+1:]), func(k ptok) bool { return k.text == "(" || k.text == ")" }), ",") {
		if len(part) > 0 {
			parts = append(parts, part)
		}
	}
	return parts
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
		} else if depth == 0 && k.text == "=" && next != "=" && (prev == "" || !strings.Contains("=!:", prev)) &&
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
