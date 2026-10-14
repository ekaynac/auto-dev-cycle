// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"cmp"
	"errors"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// The Python guard (P14c; #226 entries 68, 73 and 75) reads changed Python
// files with a Go-side tokenizer (the controller runs no Python) and keys
// a test by file, class with its bases, and the compound statements around
// it. It blocks a test lost, uncollected, skipped or weakened; a skip
// outside a test; a pytest hook, hookimpl or pytest_plugins added or
// changed in any file; a changed conftest.py holding more than fixtures
// and plain data; a changed pytest configuration, virtual-environment
// marker or compiled module; in a test module or conftest, a new dynamic
// namespace or pytest-internals reference, or an exit or output at import.
// What it cannot read, decode as UTF-8 or tokenize is unverifiable, and so
// is every changed Python file of a base whose pytest settings move
// collection off the defaults. Other exit, output, namespace and pytest
// control references warn, naming file and function (decision A).
var (
	pyConfs   = []string{"pytest.ini", ".pytest.ini", "pytest.toml", ".pytest.toml", "tox.ini", "setup.cfg", "pyproject.toml"}
	pyCollect = []string{"python_files", "python_classes", "python_functions", "norecursedirs"}
	pyBinary  = []string{".pyc", ".pyo", ".so", ".pyd", ".pth"} // loaded before, or instead of, a source
	cookie    = regexp.MustCompile(`^[ \t\f]*#.*?coding[:=][ \t]*([-\w.]+)`)
	// splitLines breaks lines where str.splitlines does (iniconfig's lines).
	splitLines = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n", "\x1c", "\n", "\x1d", "\n", "\x1e", "\n",
		"\u0085", "\n", "\u2028", "\n", "\u2029", "\n")
	// pySkips skip, xfail or uncollect; a bare skip counts only as an
	// attribute (pytest.skip, mark.skip) or once imported.
	pySkips = []string{"skipif", "skipIf", "skipUnless", "skipTest", "importorskip", "xfail", "expectedFailure", "SkipTest",
		"__test__", "__unittest_skip__", "__unittest_expecting_failure__"}
	pyForge  = []string{"_pytest", "pluginmanager", "__dict__", "__builtins__", "builtins", "__getattr__", "importlib", "modules", "__globals__", "__code__"}
	pyBare   = []string{"globals", "locals", "vars", "setattr", "delattr", "exec", "eval", "compile", "__import__"} // builtins, not attributes
	pyS      = []string{"exit", "_exit", "abort", "kill", "killpg", "system", "popen", "Popen", "fork", "forkpty", "subprocess", "signal", "atexit", "ctypes", "faulthandler", "stdout", "stderr", "__stdout__", "__stderr__", "print", "config", "session"}
	pyValues = []string{"True", "False", "None", "not", "and", "or", "in", "is", "if", "else"} // names an assert of constants may hold
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

// python is the guard for a base holding Python.
func (r *run) python(changes []Change) error {
	custom, err := r.pyCustom()
	if err != nil {
		return err
	}
	before, after := map[string]*pyTest{}, map[string]*pyTest{}
	for _, c := range changes {
		src, dst := c.Status != "A" && c.Status != "C", c.Status != "D" // a copy's source is unchanged
		for _, p := range slices.Compact([]string{c.Src, c.Dst}) {
			switch {
			case path.Base(p) == "pyvenv.cfg" || p == "conda-meta/history" || strings.HasSuffix(p, "/conda-meta/history"):
				add(&r.blocks, p, "a virtual-environment marker changed (pytest stops walking its directory)")
			case slices.Contains(pyBinary, path.Ext(p)):
				add(&r.blocks, p, "unverifiable: compiled Python, an extension module or a .pth file (it may shadow a source)")
			case slices.Contains(pyConfs, path.Base(p)):
				b, ok, err := r.pyRead(src && p == c.Src, p, c.SrcBlob)
				a, ok2, err2 := r.pyRead(dst && p == c.Dst, p, c.DstBlob)
				if err = cmp.Or(err, err2); err != nil {
					return err
				} else if ok && ok2 && pyConf(p, b, src && p == c.Src) != pyConf(p, a, dst && p == c.Dst) {
					add(&r.blocks, p, "a pytest configuration changed (collection, options or plugins)")
				}
			}
		}
		if path.Ext(c.Src) != ".py" && path.Ext(c.Dst) != ".py" {
			continue
		} else if custom {
			add(&r.blocks, c.Dst, "unverifiable: a pytest configuration of the base moves collection off its defaults (python_files, python_classes, python_functions, norecursedirs) or is unread")
			continue
		}
		b, a := &pyFile{}, &pyFile{}
		ok, ok2 := true, true
		if src && path.Ext(c.Src) == ".py" {
			if b, ok, err = r.pySide(c.Src, c.SrcBlob); err != nil {
				return err
			}
		}
		if dst && path.Ext(c.Dst) == ".py" {
			if a, ok2, err = r.pySide(c.Dst, c.DstBlob); err != nil {
				return err
			}
		}
		if !ok || !ok2 {
			continue
		}
		maps.Copy(before, b.tests)
		maps.Copy(after, a.tests)
		if c.Status == "R" && path.Base(c.Dst) == "conftest.py" { // a conftest moved reaches other tests
			b = &pyFile{}
		}
		r.pyFileCompare(c.Dst, b, a)
	}
	r.pyCompare(before, after)
	return nil
}

// pyCustom: a pytest configuration of the base sets what moves collection
// off pytest's defaults (Opus #255 M3), or is unread (and may).
func (r *run) pyCustom() (bool, error) {
	for _, p := range slices.Sorted(maps.Keys(r.files)) {
		if !slices.Contains(pyConfs, path.Base(p)) {
			continue
		}
		b, why, err := r.file(r.files[p])
		if err != nil || why != "" || slices.ContainsFunc(pyCollect, func(s string) bool { return strings.Contains(string(b), s) }) {
			return err == nil, err
		}
	}
	return false, nil
}

// pyRead reads p's blob when want; unread, it blocks p.
func (r *run) pyRead(want bool, p, blob string) ([]byte, bool, error) {
	if !want {
		return nil, true, nil
	}
	b, why, err := r.file(blob)
	if err == nil && why != "" {
		add(&r.blocks, p, "unverifiable: "+why)
	}
	return b, err == nil && why == "", err
}

// pySide reads one version of the Python file p; unverifiable, it blocks p.
func (r *run) pySide(p, blob string) (*pyFile, bool, error) {
	b, ok, err := r.pyRead(true, p, blob)
	if !ok || err != nil {
		return nil, false, err
	}
	fa, why, err := r.pyFacts(p, b)
	if err == nil && why != "" {
		add(&r.blocks, p, "unverifiable: "+why)
	}
	return fa, err == nil && why == "", err
}

// pyConf is what pytest may read of the configuration file p: all of a
// pytest.ini or pytest.toml (present or not), a TOML file naming pytest
// (or escaping a key), and an INI file's pytest sections as iniconfig
// splits them (a header only at column 0).
func pyConf(p string, b []byte, present bool) string {
	s := string(b)
	switch {
	case !present:
		return ""
	case strings.HasPrefix(strings.TrimPrefix(path.Base(p), "."), "pytest."):
		return "present\n" + s
	case path.Ext(p) == ".toml":
		if strings.Contains(strings.ToLower(s), "pytest") || strings.Contains(s, `\`) {
			return s
		}
		return ""
	}
	var out strings.Builder
	in := false
	for _, l := range strings.Split(splitLines.Replace(s), "\n") {
		if strings.HasPrefix(l, "[") {
			in = strings.Contains(strings.ToLower(l), "pytest")
		}
		if in {
			out.WriteString(l + "\n")
		}
	}
	return out.String()
}

// pyCookie: why CPython may read src other than as the guard does (PEP 263).
func pyCookie(src []byte) string {
	if !utf8.Valid(src) || slices.Contains(src, 0) {
		return "not UTF-8 source"
	}
	s := strings.ReplaceAll(strings.ReplaceAll(string(src), "\r\n", "\n"), "\r", "\n")
	lines := strings.SplitN(s, "\n", 3)
	for _, l := range lines[:min(2, len(lines))] {
		if m := cookie.FindStringSubmatch(l); m != nil && !slices.Contains([]string{"utf-8", "utf8", "ascii", "us-ascii"}, strings.ReplaceAll(strings.ToLower(m[1]), "_", "-")) {
			return "a coding cookie other than utf-8 or ascii (" + m[1] + ")"
		}
	}
	return ""
}

// pyTest is one test's evidence: sig, its decorators and signature; text,
// its assertions; uses, the names (and strings) its decorators and
// signature mention.
type pyTest struct {
	asserts, skips int
	rows           []string
	local, ns      string // its key within its file; its namespace name
	uses           []string
	sig, text      strings.Builder
}

// pyFile is what the guard reads of one version of a Python file.
type pyFile struct {
	tests   map[string]*pyTest          // "path::Class(bases)::[if x :]::name"
	names   map[string]bool             // test and class names bound, by namespace
	hooks   map[string]*strings.Builder // pytest hooks and plugin lines, whole
	refs    map[string]int              // "kind\x00reference\x00function"
	ambient int                         // skips and uncollection outside tests
	plain   string                      // a conftest's first top-level line not a fixture or plain data
}

type pyBlock struct {
	indent        int
	label, ns, fn string // key and namespace prefixes; the outermost function
	hook          string
	test          *pyTest
	coll, class   bool // a collecting scope (module, class, compound under them); a class body
}

func isHook(k ptok) bool {
	return k.kind == 'n' && (strings.HasPrefix(k.text, "pytest_") || k.text == "hookimpl" || k.text == "hookwrapper")
}

// pyKind classifies the name t[i]: 'f' a dynamic namespace or pytest's
// internals, 'x' an exit, output, process or pytest control, else 0.
func pyKind(t []ptok, i int) byte {
	n, dot := t[i].text, i > 0 && t[i-1].text == "."
	switch {
	case t[i].kind != 'n':
	case slices.Contains(pyForge, n), !dot && slices.Contains(pyBare, n):
		return 'f'
	case slices.Contains(pyS, n), dot && (strings.HasPrefix(n, "exec") || strings.HasPrefix(n, "spawn")):
		return 'x'
	}
	return 0
}

func ptext(t []ptok) string {
	var b strings.Builder
	for _, k := range t {
		b.WriteString(k.text + " ")
	}
	return b.String()
}

// pyFacts reads one version of the Python file p as pytest collects it,
// over-approximated (test* methods of every class), or says why it cannot.
// A test's decorators and body are its own; skips anywhere else are
// ambient. A name bound again at a collecting scope (=, del, import, for,
// with, def, class, global) drops the tests it named.
func (r *run) pyFacts(p string, src []byte) (*pyFile, string, error) {
	if why := pyCookie(src); why != "" {
		return nil, why, nil
	}
	lines, err := pyLex(src)
	if err != nil {
		return nil, "not tokenizable as Python", nil
	}
	fa := &pyFile{tests: map[string]*pyTest{}, names: map[string]bool{}, hooks: map[string]*strings.Builder{}, refs: map[string]int{}}
	conftest, collected := path.Base(p) == "conftest.py", pyOn(p) != nil
	stack, deco, fixture, skippers := []pyBlock{{indent: -1, coll: true}}, []ptok(nil), true, map[string]bool{}
	bare := slices.ContainsFunc(lines, func(l pline) bool {
		return (l.toks[0].text == "from" || l.toks[0].text == "import") && named(l.toks, "skip")
	})
	for _, l := range lines {
		if r.work += len(l.toks); r.work > maxWork {
			return nil, "its analysis is over the guard's work bound", nil
		} else if err := r.ctx.Err(); err != nil {
			return nil, "", err
		}
		for l.indent <= stack[len(stack)-1].indent {
			stack = stack[:len(stack)-1]
		}
		t, top := l.toks, stack[len(stack)-1]
		if t[0].text == "@" {
			fixture = fixture && len(t) > 1 && (t[1].text == "fixture" || len(t) > 3 && t[1].text == "pytest" && t[3].text == "fixture")
			deco = append(deco, t...)
			continue
		}
		kw, name, async := defOf(t)
		if conftest && len(stack) == 1 && fa.plain == "" && !pyPlain(t, kw == "def" && fixture && len(deco) > 0) {
			fa.plain = ptext(t[:min(len(t), 12)])
		}
		for i, k := range t { // rebinding: only the defined name of a def or class
			if top.coll && k.kind == 'n' && (i == 0 || t[i-1].text != ".") && (kw == "" || k.text == name) {
				r.unbind(fa, top.ns, k.text)
			} else if t[0].text == "global" && k.kind == 'n' {
				r.unbind(fa, "", k.text)
			}
		}
		full, b := append(deco, t...), top
		b.indent = l.indent
		switch {
		case kw == "def":
			b.coll, b.class, b.fn = false, false, cmp.Or(top.fn, name)
			if top.coll && collected && strings.HasPrefix(name, "test") {
				b.test = &pyTest{local: top.label + name, ns: top.ns + name}
				b.test.rows, b.test.skips = pyRows(full)
				b.test.skips += b2i(async) // unsupported without a plugin: skipped
				b.test.sig.WriteString(ptext(full))
				fa.tests[p+"::"+b.test.local], fa.names[top.ns+name] = b.test, true
				for i, k := range full {
					if k.kind == 's' || k.kind == 'n' && (i == 0 || full[i-1].text != ".") {
						b.test.uses = append(b.test.uses, strings.Trim(k.text, `"'`))
					}
				}
			}
			if strings.HasPrefix(name, "pytest_") || slices.ContainsFunc(deco, isHook) {
				b.hook = top.ns + name
			}
			fa.ambient += b2i(top.class && (conftest || collected) && (name == "__init__" || name == "__new__")) // uncollects the class
		case kw == "class":
			cls := name + "(" + ptext(t[2:]) + ")::"
			b.label, b.ns, b.class = top.label+cls, top.ns+cls, true
			fa.names[top.ns+name] = fa.names[top.ns+name] || top.coll
		case t[len(t)-1].text == ":":
			b.label = top.label + "[" + ptext(t) + "]::"
		}
		if kw != "" || t[len(t)-1].text == ":" {
			stack = append(stack, b)
		}
		hk := b.hook
		if i := slices.IndexFunc(full, isHook); hk == "" && i >= 0 {
			hk = full[i].text
		}
		if hk != "" {
			fa.hooks[hk] = cmp.Or(fa.hooks[hk], &strings.Builder{})
			fa.hooks[hk].WriteString(ptext(full) + "\n")
		}
		a, s := pyCount(full, bare)
		if b.test != nil {
			b.test.asserts, b.test.skips = b.test.asserts+a, b.test.skips+s
			if a > 0 {
				b.test.text.WriteString(ptext(t) + "\n")
			}
		} else if s > 0 { // a fixture or helper that skips, or an alias of a skip mark
			fa.ambient, skippers[b.fn] = fa.ambient+s, true
			for i, k := range t {
				if i+1 < len(t) && (i == 0 && t[1].text == "=" || k.text == "as") {
					skippers[t[i+b2i(k.text == "as")].text] = true
				}
			}
		}
		for i, k := range full {
			if kind := pyKind(full, i); kind != 0 {
				fa.refs[string(kind)+"\x00"+k.text+"\x00"+b.fn]++
			}
		}
		deco, fixture = nil, true
	}
	for _, t := range fa.tests {
		for _, u := range t.uses {
			t.skips += b2i(skippers[u])
		}
	}
	return fa, "", nil
}

// unbind drops the tests the name bound again in the namespace ns named:
// a test, or a class's tests. Its work is bounded.
func (r *run) unbind(fa *pyFile, ns, name string) {
	if !fa.names[ns+name] {
		return
	}
	delete(fa.names, ns+name)
	r.work += len(fa.tests)
	maps.DeleteFunc(fa.tests, func(_ string, t *pyTest) bool { return t.ns == ns+name || strings.HasPrefix(t.ns, ns+name+"(") })
}

// pyPlain: a conftest's top-level line is a fixture (fixture: its def under
// fixture decorators only), an import (not *), a docstring, or a name bound
// to constants other than collect_ignore*.
func pyPlain(t []ptok, fixture bool) bool {
	constant := func(k ptok) bool {
		return k.kind == 's' || k.kind == '1' || slices.Contains([]string{"True", "False", "None"}, k.text) ||
			k.kind == 'o' && strings.Contains("[](){},:-+", k.text)
	}
	switch {
	case slices.ContainsFunc(t, func(k ptok) bool { return k.text == ";" }):
		return false
	case fixture:
		return true
	case t[0].text == "import" || t[0].text == "from":
		return !slices.ContainsFunc(t, func(k ptok) bool { return k.text == "*" })
	}
	return !slices.ContainsFunc(t, func(k ptok) bool { return k.kind != 's' }) ||
		len(t) > 2 && t[0].kind == 'n' && t[1].text == "=" && !strings.HasPrefix(t[0].text, "collect_ignore") && !slices.ContainsFunc(t[2:], func(k ptok) bool { return !constant(k) })
}

// pyCount tallies a run of code tokens: assertions (the assert keyword
// unless trivial; assert*, raises, warns and fail calls) and skips.
func pyCount(t []ptok, bare bool) (asserts, skips int) {
	for i, k := range t {
		dot, call := i > 0 && t[i-1].text == ".", i+1 < len(t) && t[i+1].text == "("
		switch {
		case k.kind != 'n':
		case k.text == "assert", call && (strings.HasPrefix(k.text, "assert") || dot && slices.Contains([]string{"raises", "warns", "fail"}, k.text)):
			asserts++
		case slices.Contains(pySkips, k.text), k.text == "skip" && (dot || bare):
			skips++
		}
	}
	if asserts > 0 {
		for _, st := range pySplit(t, ";") {
			if j := slices.IndexFunc(st, func(k ptok) bool { return k.text == "assert" }); j >= 0 && trivial(st[j+1:]) {
				asserts--
			}
		}
	}
	return
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

// trivial: an assert's expression (to its message) cannot fail or checks
// nothing: it names only constants (assert 1, assert not False), has a
// constant or-operand (x or True), or identical sides (x == x, x is x).
func trivial(t []ptok) bool {
	e := pySplit(t, ",")[0]
	if slices.ContainsFunc(pySplit(e, "or"), func(o []ptok) bool {
		return !slices.ContainsFunc(o, func(k ptok) bool { return k.kind == 'n' && !slices.Contains(pyValues, k.text) })
	}) {
		return true
	}
	if s := pySplit(e, "is"); len(s) == 2 && ptext(s[0]) == ptext(s[1]) {
		return true
	}
	s := pySplit(e, "=") // == is two operator tokens: x, "", x
	return len(s) == 3 && len(s[1]) == 0 && ptext(s[0]) == ptext(s[2])
}

// pyRows are the literal argvalues rows of each parametrize in t (not
// nested in another), as token text, and how many argvalues are an empty
// literal (pytest skips the test: "got empty parameter set").
func pyRows(t []ptok) (rows []string, empty int) {
	for i := 0; i+1 < len(t); i++ {
		if t[i].text != "parametrize" || t[i+1].text != "(" {
			continue
		}
		end, depth := i+1, 0
		for ; end < len(t); end++ { // its closing bracket: lines are balanced
			if k := t[end]; k.kind == 'o' && strings.Contains("([{", k.text) {
				depth++
			} else if k.kind == 'o' && strings.Contains(")]}", k.text) {
				if depth--; depth == 0 {
					break
				}
			}
		}
		args := pySplit(t[i+2:min(end, len(t))], ",")
		if i = end; len(args) < 2 {
			continue
		}
		v := args[1]
		if len(v) > 2 && v[0].text == "argvalues" && v[1].text == "=" {
			v = v[2:]
		}
		if t := ptext(v); t == "[ ] " || t == "( ) " {
			empty++
		} else if len(v) > 1 && (v[0].text == "[" || v[0].text == "(") {
			for _, row := range pySplit(v[1:len(v)-1], ",") {
				rows = append(rows, ptext(row))
			}
		}
	}
	return rows, empty
}

// pyFileCompare applies the per-file rules to one Python change at p:
// its base version b (empty when added) and its candidate a.
func (r *run) pyFileCompare(p string, b, a *pyFile) {
	tested := pyOn(p) != nil || path.Base(p) == "conftest.py"
	if a.plain != "" {
		add(&r.blocks, p, "unverifiable: a conftest.py holding more than fixtures and plain data ("+a.plain+")")
	}
	for _, k := range keys(b.hooks, a.hooks) {
		if a.hooks[k] == nil {
			add(&r.warnings, p+": "+k, "a pytest hook or plugin removed: review it")
		} else if b.hooks[k] == nil || b.hooks[k].String() != a.hooks[k].String() {
			add(&r.blocks, p+": "+k, "a pytest hook or plugin added or changed (pytest_*, hookimpl, pytest_plugins)")
		}
	}
	if a.ambient > b.ambient {
		add(&r.blocks, p, "a skip added outside a test (a helper, a fixture, a mark, __test__ or a constructor)")
	}
	for _, k := range keys(b.refs, a.refs) {
		f := strings.Split(k, "\x00") // kind, reference, function
		at := p + ": " + f[1] + " in " + cmp.Or(f[2], "module level")
		if a.refs[k] > b.refs[k] && tested && (f[0] == "f" || f[2] == "") {
			add(&r.blocks, at, "unverifiable: a new dynamic-namespace or pytest-internals reference in a test module or conftest, or an exit, output or pytest control at import")
		} else if a.refs[k] != b.refs[k] {
			add(&r.warnings, at, "an exit, output, process, namespace or pytest-control reference added, removed or changed: review it")
		}
	}
}

// pyCompare blocks a test gone or weakened. A test gone from one file may
// reappear, by its key within the file, as a new test of another; a moved
// test whose assertions changed, and a test whose decorators or signature
// changed, warn.
func (r *run) pyCompare(before, after map[string]*pyTest) {
	moved := map[string][]string{}
	for _, k := range slices.Sorted(maps.Keys(after)) {
		if before[k] == nil {
			moved[after[k].local] = append(moved[after[k].local], k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(before)) {
		b, a := before[k], after[k]
		if m := moved[b.local]; a == nil && len(m) > 0 {
			if a, moved[b.local] = after[m[0]], m[1:]; a.text.String() != b.text.String() {
				add(&r.warnings, k, "a moved test's assertions changed: review it")
			}
		} else if a != nil && a.sig.String() != b.sig.String() {
			add(&r.warnings, k, "a test's decorators or signature changed: review it")
		}
		switch {
		case a == nil:
			add(&r.blocks, k, "a test deleted or no longer collected")
		case a.asserts < b.asserts:
			add(&r.blocks, k, "fewer assertions in a test")
		case a.skips > b.skips:
			add(&r.blocks, k, "a test skipped")
		case lost(b.rows, a.rows):
			add(&r.blocks, k, "a parametrize case removed")
		}
	}
}

// lost: a row of b is not in a (as multisets).
func lost(b, a []string) bool {
	left := map[string]int{}
	for _, row := range a {
		left[row]++
	}
	for _, row := range b {
		if left[row]--; left[row] < 0 {
			return true
		}
	}
	return false
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

// pyToken scans the token at the start of s: its kind and length. A name
// must be ASCII (Python folds others by NFKC: test_zero by another spelling).
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
		need(!strings.ContainsFunc(s[:n], func(r rune) bool { return r >= utf8.RuneSelf }))
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
