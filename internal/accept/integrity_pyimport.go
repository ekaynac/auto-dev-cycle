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
	t, _ := r.pyTestWalk(p)
	return t
}

// pyTestWalk is pyTestDir's verdict, and how many directories it walked
// that had no verdict yet: each directory's verdict, its ancestors'
// included, is cached once, so a directory is walked once (Astra #281 r4
// M2, r5 M1).
func (r *run) pyTestWalk(p string) (bool, int) {
	if r.pyTests == nil {
		r.pyTests = map[string]bool{}
	}
	t, n, todo := false, 0, []string(nil)
	for d := path.Dir(p); d != "."; d = path.Dir(d) {
		cached, ok := r.pyTests[d]
		if b := path.Base(d); ok {
			t = cached
			break
		} else if n++; b == "test" || b == "tests" || slices.ContainsFunc(r.baseDirs[d], pyTested) || slices.ContainsFunc(r.candDirs[d], pyTested) {
			t, r.pyTests[d] = true, true
			break
		}
		todo = append(todo, d)
	}
	for _, d := range todo { // below a test directory, or not, as its parent
		r.pyTests[d] = t
	}
	return t, n
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
	if pat := pySplit(t, ":"); t[0].text == "case" && len(pat) > 1 && len(pyAssign(t)) == 1 { // a case pattern's captures (Opus #270 M2)
		for i, k := range pat[0][1:] { // a name not after ., nor before (, . or = (a class, a value, a keyword)
			if k.kind == 'n' && pat[0][i].text != "." && (i+2 >= len(pat[0]) || !slices.Contains([]string{"(", ".", "="}, pat[0][i+2].text)) {
				names = append(names, k.text)
			}
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
		if end := pyDepthEnd(params, 0); len(params) > 0 && params[0].text == "[" && end > 0 && pyElemsBy(pySplit(params[1:end], ","), func(p []ptok) bool { return len(p) == 1 && p[0].kind == 'n' }) {
			params = params[end+1:] // def f[T, U](…): bare type parameters, no bound to evaluate (Mythos #266 L6)
		}
		for j := 1; j < len(params); j++ {
			if params[j].text == "*" && strings.Contains("(,*", params[j-1].text) {
				params, j = slices.Delete(params, j, j+1), j-1
			}
		}
		return ok(pyConst(params))
	case d[0].text == "class" && len(h[0]) > 1:
		return ok(pyConst(h[0][2:]) && !named(h[0], "metaclass") && !slices.ContainsFunc(h[0], func(k ptok) bool { return k.text == "*" }) &&
			len(t) == len(h[0])+1+b2i(t[len(t)-1].text == "pass"))
	case d[0].text == "type" && len(d) > 3 && d[1].kind == 'n' && d[2].text == "=": // type X = …: its value is evaluated lazily (Mythos #266 L6)
		return ok(pyConst(d[3:]) && !slices.Contains(pyFixtures, d[1].text) && !(conftest && strings.HasPrefix(d[1].text, "collect_ignore")))
	case len(d) == 1 && d[0].text == "pass" || !slices.ContainsFunc(d, func(k ptok) bool {
		return k.kind != 's' || strings.ContainsAny(k.text[:strings.IndexAny(k.text, `"'`)], "fFtT")
	}):
		return "" // pass, or a docstring, its strings concatenated too (Mythos #266 L6)
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

// pyCompound are the keywords heading a compound statement whose inline
// suite runs at import (try: M.x = v).
var pyCompound = strings.Fields("try except finally else elif if while for with")

// pyStmt: statement st, run at import, is inert by a positive grammar, and
// nothing else is (Astra #281 r4): an import without a star, a docstring,
// pass, ONE bare name = a pyExpr (annotated by a pyExpr), or a def or class
// whose header is inert (pyHeader). A decorator is pyDeco's.
func pyStmt(st []ptok) bool {
	d := st[b2i(st[0].text == "async"):]
	switch {
	case len(d) == 0:
		return false
	case d[0].text == "import" || d[0].text == "from":
		return !slices.ContainsFunc(d, func(k ptok) bool { return k.text == "*" })
	case len(d) == 1 && (d[0].text == "pass" || d[0].kind == 's'):
		return d[0].text == "pass" || pyExpr(d)
	case d[0].text == "def" || d[0].text == "class":
		return pyHeader(d)
	}
	parts := pyAssign(st) // all of st: pyAssign stops only at a lambda, and pyOpaque makes any lambda risky (Astra #281 r5 H1)
	tg := pySplit(parts[0], ":")
	return len(parts) == 2 && len(tg[0]) == 1 && pyName(tg[0]) && (len(tg) == 1 || len(tg) == 2 && pyExpr(tg[1])) && pyExpr(parts[1])
}

// pyOpaque: t holds a lambda, or a for past its first token (a
// comprehension or generator, anywhere; an async one too, whose async is
// always followed by for): code run at import no recognizer reads, risky
// at token level (Astra #281 r5 H1).
func pyOpaque(t []ptok) bool {
	return slices.ContainsFunc(t, func(k ptok) bool { return k.kind == 'n' && k.text == "lambda" }) ||
		slices.ContainsFunc(t[min(1, len(t)):], func(k ptok) bool { return k.kind == 'n' && k.text == "for" })
}

// pyExpr: t is an inert expression: a number, -number, strings (no f- or
// t-string), True, False, None, a bare name, or a tuple, list, set or dict
// display of pyExprs. Nothing else is: no dotted name (a module's
// __getattr__ runs), comprehension, call, subscript, operator, lambda,
// conditional, walrus or starred (r4 H1, the lead's pre-check).
func pyExpr(t []ptok) bool {
	if es := pySplit(t, ","); len(es) > 1 {
		return pyElems(es, false)
	} else if len(t) == 0 {
		return false
	}
	switch k := t[0]; {
	case len(t) == 1 && k.kind == '1', len(t) == 2 && k.text == "-" && t[1].kind == '1':
		return true
	case k.kind == 's':
		return !slices.ContainsFunc(t, func(k ptok) bool {
			return k.kind != 's' || strings.ContainsAny(k.text[:strings.IndexAny(k.text, `"'`)], "fFtT")
		})
	case k.kind == 'o' && strings.Contains("([{", k.text) && pyClosed(t):
		return pyElems(pySplit(t[1:len(t)-1], ","), k.text == "{")
	}
	return len(t) == 1 && pyName(t)
}

// pyElems: each element of a display is a pyExpr (key: value in a dict).
func pyElems(es [][]ptok, dict bool) bool {
	return pyElemsBy(es, func(e []ptok) bool {
		kv := pySplit(e, ":")
		return !dict && pyExpr(e) || dict && len(kv) <= 2 && pyExpr(kv[0]) && (len(kv) == 1 || pyExpr(kv[1]))
	})
}

// pyName: t is a name or a dotted name: no keyword (try: X = 1 is no
// annotated name try), True, False and None aside.
func pyName(t []ptok) bool {
	for i, k := range t {
		if i%2 == 0 && (k.kind != 'n' || slices.Contains(pyHard, k.text)) || i%2 == 1 && k.text != "." {
			return false
		}
	}
	return len(t)%2 == 1
}

// pyHard are Python's keywords but True, False and None.
var pyHard = strings.Fields("and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield")

// pyClosed: t's first bracket closes at its last token.
func pyClosed(t []ptok) bool {
	depth := 0
	for i, k := range t {
		if k.kind == 'o' && strings.Contains("([{", k.text) {
			depth++
		} else if k.kind == 'o' && strings.Contains(")]}", k.text) {
			depth--
		}
		if depth == 0 {
			return i == len(t)-1
		}
	}
	return false
}

// pyHeader: def or class statement d evaluates only pyExprs at import: each
// default and annotation, the return one too; a class has no base but
// object and no keyword (a base's __init_subclass__ or metaclass runs: the
// lead's pre-check; the file may not bind object, file's dunder rule).
func pyHeader(d []ptok) bool {
	h := pySplit(d, ":")
	if len(d) < 3 || d[1].kind != 'n' || len(h) < 2 {
		return false
	} else if d[0].text == "class" {
		return slices.Contains([]string{"", "()", "(object)"}, pyJoin(h[0][2:]))
	}
	sig := pySplit(h[0][2:], "-")
	if len(sig) > 2 || len(sig[0]) < 2 || sig[0][0].text != "(" || !pyClosed(sig[0]) || len(sig) == 2 && (len(sig[1]) < 2 || sig[1][0].text != ">" || !pyExpr(sig[1][1:])) {
		return false
	}
	return pyElemsBy(pySplit(sig[0][1:len(sig[0])-1], ","), func(p []ptok) bool {
		for len(p) > 0 && p[0].text == "*" { // *args, **kwargs, a bare *
			p = p[1:]
		}
		if len(p) == 0 || len(p) == 1 && p[0].text == "/" {
			return true
		}
		kv := pySplit(p[1:], "=")
		return pyName(p[:1]) && len(kv) <= 2 && (len(kv[0]) == 0 || kv[0][0].text == ":" && pyExpr(kv[0][1:])) && (len(kv) == 1 || pyExpr(kv[1]))
	})
}

// pyElemsBy: each of es passes ok; a trailing comma leaves one empty.
func pyElemsBy(es [][]ptok, ok func([]ptok) bool) bool {
	if len(es[len(es)-1]) == 0 {
		es = es[:len(es)-1]
	}
	return !slices.ContainsFunc(es, func(e []ptok) bool { return !ok(e) })
}

// pyDeco: decorator st calls a trusted pytest chain (pyPytest without
// patch: rooted at pytest or fixture) with pyExpr arguments, or a dotted
// name as one (a skipif's condition); its root, whose binding pyTrusted
// must prove (Astra #281 r4 H2), and each dotted argument's root, which
// must be a repository module's (r5 H4). A call with exactly one
// positional argument and no keyword applies that argument as a function
// (pytest's MarkDecorator.__call__), so it must be a pyLiteral (r6 M1).
func pyDeco(st []ptok) (root string, args []string, ok bool) {
	end := slices.IndexFunc(st, func(k ptok) bool { return k.text == "(" })
	if end < 0 {
		end = len(st)
	}
	c := pyJoin(st[1:end])
	root, _, _ = strings.Cut(c, ".")
	ok = pyName(st[1:end]) && pyPytest(c, false) && (end == len(st) || pyClosed(st[end:]) &&
		pyElemsBy(pySplit(st[end+1:len(st)-1], ","), func(a []ptok) bool {
			if len(a) > 2 && a[0].kind == 'n' && a[1].text == "=" { // a keyword argument
				a = a[2:]
			}
			if !pyExpr(a) && pyName(a) {
				args = append(args, a[0].text)
				return true
			}
			return pyExpr(a)
		}))
	if ok && end < len(st) && !strings.HasSuffix(c, ".with_args") { // with_args stores its arguments, never applies them
		as := pySplit(st[end+1:len(st)-1], ",")
		if len(as[len(as)-1]) == 0 {
			as = as[:len(as)-1] // () or a trailing comma
		}
		ok = len(as) != 1 || len(as[0]) > 1 && as[0][1].text == "=" || pyLiteral(as[0])
	}
	return root, args, ok
}

// pyBindings is, in one scan of the file's tokens (its caller charges them
// first: Astra #281 r6 M2), for each trusted name r whether it is bound
// only by import pytest (r is pytest) or from pytest import r (r4 H2), and
// for each imported name whether it is bound only by imports (r5 H4): the
// file has no star import, and every other r not after . is r. or @r.
func pyBindings(lines []pline, trusted, imported []string) (map[string]bool, map[string]bool) {
	type use struct {
		trusted bool
		r       string
	}
	names, proved, rebound, star := map[string][]use{}, map[use]bool{}, map[use]bool{}, false
	for _, r := range trusted {
		names[r] = append(names[r], use{true, r})
	}
	for _, r := range imported {
		names[r] = append(names[r], use{false, r})
	}
	for _, l := range lines {
		for _, st := range pySplit(slices.Concat(l.toks, l.inner), ";") {
			imp := len(st) > 1 && (st[0].text == "import" || st[0].text == "from")
			for i, k := range st {
				if star = star || k.text == "*" && i > 0 && st[i-1].text == "import"; k.kind != 'n' || len(names[k.text]) == 0 {
					continue
				}
				free := (i == 0 || st[i-1].text != "." && st[i-1].text != "@") && (i+1 == len(st) || st[i+1].text != ".")
				for _, u := range names[k.text] { // an import proving r binds it however it names it (import r.x)
					if imp && (!u.trusted || pyTrustProof(st, u.r)) {
						proved[u] = true
					} else if free {
						rebound[u] = true
					}
				}
			}
		}
	}
	verdicts := func(ns []string, trusted bool) map[string]bool {
		v := map[string]bool{}
		for _, r := range ns {
			v[r] = proved[use{trusted, r}] && !rebound[use{trusted, r}] && !star
		}
		return v
	}
	return verdicts(trusted, true), verdicts(imported, false)
}

// pyTrustProof: st is import pytest (r is pytest) or from pytest import r.
func pyTrustProof(st []ptok, r string) bool {
	return (st[0].text == "import" && r == "pytest" || pyJoin(st[:min(3, len(st))]) == "frompytestimport" && r != "pytest") &&
		slices.ContainsFunc(pyImportParts(st), func(p []ptok) bool { return len(p) == 1 && p[0].text == r })
}

// pyMaxRoots bounds the distinct names a file's binding analysis proves:
// past it, the file is not inert (Astra #281 r6 M2).
const pyMaxRoots = 16

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
