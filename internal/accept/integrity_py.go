// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"cmp"
	"errors"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
)

// pySkipDir: pytest never walks dir (its default norecursedirs).
func pySkipDir(d string) bool {
	return strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".egg") ||
		slices.Contains([]string{"_darcs", "build", "CVS", "dist", "node_modules", "venv", "{arch}"}, d)
}

// pyOn: pytest's default patterns collect p (test_*.py, *_test.py).
func pyOn(p string) []string {
	b := path.Base(p)
	if strings.HasSuffix(b, ".py") && (strings.HasPrefix(b, "test_") || strings.HasSuffix(b, "_test.py")) && walked(p, pySkipDir) {
		return []string{"pytest"}
	}
	return nil
}

// ptok is a Python code token: a name ('n'), a string ('s'), a number
// ('1') or an operator ('o', one byte).
type ptok struct {
	kind byte
	text string
}

// pline is a logical line: its indentation and its tokens.
type pline struct {
	indent int
	toks   []ptok
}

var errLex = errors.New("not tokenizable as Python")

// maxNest bounds nested f-string fields (the scanner recurses on them).
const maxNest = 64

var strPrefixes = strings.Fields("r u b br rb f fr rf t tr rt")

func word(c byte) bool {
	return c == '_' || c >= 0x80 || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// pyLex splits src into logical lines of code tokens as Python's tokenizer
// does: comments dropped; a string (any prefix, single or triple quoted,
// f- and t-strings with nested fields) one token; brackets and a trailing
// backslash joining physical lines. What it cannot tokenize is an error
// (the scanners panic with errLex): the guard never guesses where code is.
func pyLex(src []byte) (lines []pline, err error) {
	defer func() {
		if r := recover(); r != nil {
			lines, err = nil, r.(error)
		}
	}()
	s := strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(string(src), "\ufeff"), "\r\n", "\n"), "\r", "\n") + "\n"
	cur, depth, start := pline{}, 0, true
	for i := 0; i < len(s); {
		if start { // indentation: a tab to the next multiple of 8, a form feed resets
			col := 0
			for ; strings.IndexByte(" \t\f", s[i]) >= 0; i++ {
				col = map[byte]int{' ': col + 1, '\t': col/8*8 + 8, '\f': 0}[s[i]]
			}
			cur.indent, start = col, false
			continue
		}
		switch c := s[i]; {
		case c == '\n' && depth == 0:
			if i++; len(cur.toks) > 0 {
				lines = append(lines, cur)
			}
			cur, start = pline{}, true
		case strings.IndexByte(" \t\f\n", c) >= 0:
			i++
		case c == '#':
			i += eol(s[i:])
		case c == '\\':
			need(strings.HasPrefix(s[i:], "\\\n"))
			i += 2
		default:
			k, n := pyToken(s[i:], 0)
			if t := s[i : i+n]; k == 'o' && strings.Contains("([{", t) {
				depth++
			} else if k == 'o' && strings.Contains(")]}", t) {
				depth--
				need(depth >= 0)
			}
			cur.toks, i = append(cur.toks, ptok{k, s[i : i+n]}), i+n
		}
	}
	need(depth == 0)
	return lines, nil
}

func need(ok bool) {
	if !ok {
		panic(errLex)
	}
}

// eol is the length of s's first line.
func eol(s string) int {
	if n := strings.IndexByte(s, '\n'); n >= 0 {
		return n
	}
	return len(s)
}

// pyToken scans the token at the start of s: its kind and length.
func pyToken(s string, depth int) (byte, int) {
	switch c := s[0]; {
	case c == '"' || c == '\'':
		return 's', pyString(s, "", depth)
	case word(c):
		n, num := 1, c <= '9'
		for n < len(s) && (word(s[n]) || num && s[n] == '.') {
			n++
		}
		if pre := strings.ToLower(s[:n]); n < len(s) && (s[n] == '"' || s[n] == '\'') && slices.Contains(strPrefixes, pre) {
			return 's', n + pyString(s[n:], pre, depth)
		}
		return map[bool]byte{true: '1', false: 'n'}[num], n
	}
	return 'o', 1
}

// pyString scans a string literal at s (its quote), with its prefix: its
// length. In an f- or t-string, a {field} is code (pyField).
func pyString(s, prefix string, depth int) int {
	need(depth <= maxNest)
	q := s[:1]
	if strings.HasPrefix(s, q+q+q) {
		q = s[:3]
	}
	raw, fstr := strings.Contains(prefix, "r"), strings.ContainsAny(prefix, "ft")
	for i := len(q); i < len(s); {
		switch c := s[i]; {
		case strings.HasPrefix(s[i:], q):
			return i + len(q)
		case c == '\\' && fstr && !raw && strings.HasPrefix(s[i:], `\N{`): // a named character, not a field
			j := strings.IndexByte(s[i:], '}')
			need(j > 0)
			i += j + 1
		case c == '\\' && !(fstr && strings.HasPrefix(s[i:], `\{`)):
			i += 2
		case fstr && (strings.HasPrefix(s[i:], "{{") || strings.HasPrefix(s[i:], "}}")):
			i += 2
		case fstr && c == '{':
			i += 1 + pyField(s[i+1:], depth+1, false)
		default:
			need(c != '\n' || len(q) == 3)
			i++
		}
	}
	panic(errLex)
}

// pyField scans an f-string field after its '{', up to its '}': code,
// then a conversion (!r) and a format spec (spec), which is text that may
// hold nested fields.
func pyField(s string, depth int, spec bool) int {
	nest := 0
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case spec && c == '{':
			i += 1 + pyField(s[i+1:], depth+1, false)
		case c == '}' && nest == 0:
			return i + 1
		case spec || strings.IndexByte(" \t\n\\", c) >= 0:
			i++
		case c == '#':
			i += eol(s[i:])
		case c == ':' && nest == 0:
			return i + 1 + pyField(s[i+1:], depth, true)
		case strings.IndexByte("([{", c) >= 0:
			nest, i = nest+1, i+1
		case strings.IndexByte(")]}", c) >= 0:
			nest, i = nest-1, i+1
			need(nest >= 0)
		default:
			_, n := pyToken(s[i:], depth)
			i += n
		}
	}
	panic(errLex)
}

var (
	// pySkips mark a test skipped or expected to fail; a bare skip counts
	// only as an attribute (pytest.skip, mark.skip) or once imported.
	pySkips = map[string]bool{"skipif": true, "skipIf": true, "skipUnless": true, "skipTest": true, "importorskip": true,
		"xfail": true, "expectedFailure": true, "SkipTest": true}
	// pyControls drop collected tests from a conftest.py.
	pyControls = map[string]bool{"collect_ignore": true, "collect_ignore_glob": true, "pytest_collection_modifyitems": true,
		"pytest_ignore_collect": true, "pytest_collect_file": true, "pytest_pycollect_makeitem": true, "pytest_collect_directory": true}
)

// pyCount tallies a run of code tokens.
func pyCount(t []ptok, bare bool) (asserts, skips, exits, controls int) {
	for i, k := range t {
		dot := i > 0 && t[i-1].text == "."
		switch {
		case k.kind != 'n':
		case strings.HasPrefix(k.text, "assert"), dot && (k.text == "raises" || k.text == "warns" || k.text == "fail"):
			asserts++
		case pySkips[k.text], k.text == "skip" && (dot || bare):
			skips++
		case dot && (k.text == "exit" || k.text == "_exit"):
			exits++
		case pyControls[k.text]:
			controls++
		}
	}
	return
}

// trivial: an assert statement that cannot fail (assert True, assert x or True).
func trivial(t []ptok) bool {
	return t[0].text == "assert" && t[len(t)-1].text == "True" && (len(t) == 2 || named(t[1:len(t)-1], "or"))
}

func named(t []ptok, name string) bool {
	return slices.ContainsFunc(t, func(k ptok) bool { return k.kind == 'n' && k.text == name })
}

// defOf: a def or class statement's keyword and name, and async.
func defOf(t []ptok) (kw, name string, async bool) {
	if len(t) > 1 && t[0].text == "async" {
		t, async = t[1:], true
	}
	if len(t) > 1 && (t[0].text == "def" || t[0].text == "class") && t[1].kind == 'n' {
		return t[0].text, t[1].text, async
	}
	return "", "", false
}

// pyRows are the literal argvalues rows of each parametrize in t, as
// their token text: the elements of a list or tuple second argument.
func pyRows(t []ptok) (rows []string) {
	for i := range t {
		if t[i].text != "parametrize" || i+1 == len(t) || t[i+1].text != "(" {
			continue
		}
		depth, arg, row := 0, 0, ""
		for _, k := range t[i+1:] {
			if k.kind == 'o' && strings.Contains(")]}", k.text) {
				depth--
			}
			if arg == 1 && (depth == 1 || depth == 2 && k.text == ",") && row != "" {
				rows, row = append(rows, row), ""
			} else if arg == 1 && depth >= 2 {
				row += k.text + " "
			}
			if k.kind == 'o' && strings.Contains("([{", k.text) {
				depth++
			}
			if depth == 1 && k.text == "," {
				arg++
			}
			if depth == 0 {
				break
			}
		}
	}
	return rows
}

// pyFacts reads a Python module as pytest collects it: test* functions at
// module level, test* methods of Test* (or TestCase) classes, nested ones
// too, by logical line and indentation. A test's decorators and body are
// its own; skips anywhere else are ambient. __test__ (not True) at module
// or test class level, or a test class constructor, uncollects its tests.
func pyFacts(p string, src []byte, fa *facts) error {
	lines, err := pyLex(src)
	if err != nil {
		return err
	}
	type block struct {
		indent      int
		class, hook string // a test class's name within the file; a pytest hook's
		test        *stat
	}
	stack, deco, off, exits := []block{{indent: -1, class: "."}}, []ptok(nil), []string(nil), 0
	bare := slices.ContainsFunc(lines, func(l pline) bool {
		return (l.toks[0].text == "from" || l.toks[0].text == "import") && named(l.toks, "skip")
	})
	for _, l := range lines {
		for l.indent <= stack[len(stack)-1].indent {
			stack = stack[:len(stack)-1]
		}
		t, top := l.toks, stack[len(stack)-1]
		if t[0].text == "@" {
			deco = append(deco, t...)
			continue
		}
		scope := strings.TrimPrefix(top.class+"::", ".::") // "" at module level
		if kw, name, async := defOf(t); kw != "" {
			b := block{indent: l.indent}
			switch {
			case kw == "def" && top.class != "" && strings.HasPrefix(name, "test"):
				b.test = &stat{local: scope + name, rows: pyRows(deco)}
				if _, b.test.skips, _, _ = pyCount(deco, bare); async { // unsupported without a plugin: skipped
					b.test.skips++
				}
				fa.tests[p+"::"+scope+name] = b.test
			case kw == "class" && top.class != "" && (strings.HasPrefix(name, "Test") || named(t, "TestCase")):
				b.class = scope + name
			case kw == "def" && scope != "" && (name == "__init__" || name == "__new__"):
				off = append(off, p+"::"+scope)
			case kw == "def" && strings.HasPrefix(name, "pytest_"):
				b.hook = name
			}
			if b.test == nil {
				_, s, _, _ := pyCount(deco, bare)
				fa.ambient += s
			}
			deco, stack = nil, append(stack, b)
		} else if top.class != "" && len(t) > 1 { // del or rebinding of a test, or __test__
			for i, k := range t {
				if k.kind == 'n' && (t[0].text == "del" || i == 0 && (t[1].text == "=" || t[1].text == ":")) {
					delete(fa.tests, p+"::"+scope+k.text)
				}
			}
			if t[0].text == "__test__" && !(len(t) == 3 && t[2].text == "True") {
				off = append(off, p+"::"+scope)
			}
		}
		var test *stat
		hook := ""
		if named(t, "pytest_plugins") {
			hook = "pytest_plugins"
		}
		for _, b := range slices.Backward(stack) {
			test, hook = cmp.Or(test, b.test), cmp.Or(hook, b.hook)
		}
		a, s, x, c := pyCount(t, bare)
		if test != nil {
			test.asserts, test.skips = test.asserts+a, test.skips+s
		} else {
			fa.ambient += s
		}
		if hook != "" {
			for _, k := range t {
				fa.warn["the pytest hook "+hook] += k.text + " "
			}
		}
		fa.controls, exits = fa.controls+c, exits+x
		if trivial(t) {
			fa.trivial++
		}
	}
	maps.DeleteFunc(fa.tests, func(k string, _ *stat) bool {
		return slices.ContainsFunc(off, func(o string) bool { return strings.HasPrefix(k, o) })
	})
	if exits > 0 {
		fa.warn["sys.exit or os._exit calls"] = strconv.Itoa(exits)
	}
	return nil
}
