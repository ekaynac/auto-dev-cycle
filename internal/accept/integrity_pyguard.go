// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"cmp"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
)

// The Python guard's forgery blocks (P14c-2a; #226 entries 73, 75, 84)
// read changed Python files, f-string fields included, and block: a hook,
// hookimpl or pytest_plugins in either version of a file; a conftest.py
// past fixtures, imports and plain data; a changed pytest configuration,
// environment marker, compiled or startup file; any Python change when the
// base's pytest settings move collection (M3). Decision A: pytest
// internals and builtin fixtures overridden block anywhere; namespace,
// frame, comparison, exit and process references in test modules and
// conftests and at import; output at their import; other changes warn.
// Test identities are P14c-2b's: until then P14b's rule blocks every .py.
var (
	pyConfs   = []string{"pytest.ini", ".pytest.ini", "pytest.toml", ".pytest.toml", "tox.ini", "setup.cfg", "pyproject.toml"}
	pyCollect = strings.Fields("python_files python_classes python_functions norecursedirs testpaths confcutdir " +
		"collect_imported_tests consider_namespace_packages usefixtures empty_parameter_set_mark")
	// pyOpt: the options a base may set (collection and results untouched).
	pyOpt   = regexp.MustCompile(`^-(q+|v+|r\w*|l|s|W|-(strict|strict-markers|strict-config|showlocals|tb|color|durations|capture))$`)
	pyWords = regexp.MustCompile(`[\s"',\[\]=]+`)
	// splitLines breaks lines where str.splitlines does (iniconfig's lines).
	splitLines = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n", "\x1c", "\n", "\x1d", "\n", "\x1e", "\n",
		"\u0085", "\n", "\u2028", "\n", "\u2029", "\n")
	pyLoaded   = []string{".pyc", ".pyo", ".so", ".pyd", ".pth", "sitecustomize.py", "usercustomize.py", "entry_points.txt"}
	pyBare     = strings.Fields("globals locals vars setattr delattr exec eval compile __import__") // builtins, not attributes
	pyExec     = regexp.MustCompile(`^(exec|spawn)[lv]p?e?$`)                                       // os.exec*, os.spawn*
	pyFixtures = strings.Fields("cache capfd capfdbinary caplog capsys capsysbinary capteesys doctest_namespace monkeypatch " +
		"pytestconfig pytester record_property record_testsuite_property record_xml_attribute recwarn subtests testdir " +
		"tmp_path tmp_path_factory tmpdir tmpdir_factory")
	// pyClass: 'p' pytest internals, 'f' a namespace, a frame or a
	// comparison override, 'e' an exit or a process, 'o' output or a thread.
	pyClass = map[string]byte{}
)

func init() {
	for c, names := range map[byte]string{
		'p': "_pytest pluggy pluginmanager HookspecMarker ihook record_xml_attribute _xml",
		'f': "__dict__ __builtins__ builtins __getattr__ __getattribute__ __setattr__ __delattr__ __setitem__ __delitem__ " +
			"importlib modules __globals__ __code__ __subclasses__ __bases__ settrace setprofile _getframe currentframe f_locals " +
			"f_globals f_back tb_frame gi_frame get_objects get_referrers get_referents __eq__ __ne__ __bool__ __contains__ " +
			"__instancecheck__ __subclasscheck__",
		'e': "exit _exit abort kill killpg system popen Popen fork forkpty subprocess ctypes atexit posix_spawn posix_spawnp",
		'o': "print stdout stderr __stdout__ __stderr__ signal faulthandler argv threading Thread _thread multiprocessing",
	} {
		for _, n := range strings.Fields(names) {
			pyClass[n] = c
		}
	}
}

// pySkipDir: pytest never walks dir (its default norecursedirs).
func pySkipDir(d string) bool {
	return strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".egg") ||
		slices.Contains([]string{"_darcs", "build", "CVS", "dist", "node_modules", "venv", "{arch}"}, d)
}

// pyTested: p is a conftest.py or a module pytest's default patterns
// collect (test_*.py, *_test.py).
func pyTested(p string) bool {
	b := path.Base(p)
	return b == "conftest.py" || strings.HasSuffix(b, ".py") && (strings.HasPrefix(b, "test_") || strings.HasSuffix(b, "_test.py")) && walked(p, pySkipDir)
}

// python is the guard for a base holding Python.
func (r *run) python(changes []Change) error {
	custom, err := r.pyCustom()
	if err != nil {
		return err
	}
	for _, c := range changes {
		src, dst := c.Status != "A" && c.Status != "C", c.Status != "D" // a copy's source is unchanged
		for _, p := range slices.Compact([]string{c.Src, c.Dst}) {
			switch base := path.Base(p); {
			case base == "pyvenv.cfg" || p == "conda-meta/history" || strings.HasSuffix(p, "/conda-meta/history"):
				add(&r.blocks, p, "a virtual-environment marker changed (pytest stops walking its directory)")
			case slices.Contains(pyLoaded, path.Ext(p)) || slices.Contains(pyLoaded, base):
				add(&r.blocks, p, "unverifiable: compiled Python, an extension module, a .pth, startup or entry-points file")
			case slices.Contains(pyConfs, base):
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
			add(&r.blocks, c.Dst, "unverifiable: a pytest configuration of the base moves collection off its defaults or is unread")
			continue
		}
		b, ok, err := r.pyScan(src && path.Ext(c.Src) == ".py", c.Src, c.SrcBlob)
		a, ok2, err2 := r.pyScan(dst && path.Ext(c.Dst) == ".py", c.Dst, c.DstBlob)
		if err = cmp.Or(err, err2); err != nil {
			return err
		} else if ok && ok2 {
			r.pyFileCompare(c.Dst, b, a)
		}
	}
	return nil
}

// pyCustom: a base configuration moves collection or selection off the
// defaults (Opus #255 M3: a setting, an option outside pyOpt), or is unread.
func (r *run) pyCustom() (bool, error) {
	for _, p := range slices.Sorted(maps.Keys(r.files)) {
		if !slices.Contains(pyConfs, path.Base(p)) {
			continue
		} else if b, why, err := r.file(r.files[p]); err != nil || why != "" {
			return err == nil, err
		} else if conf := pyConf(p, b, true); slices.ContainsFunc(pyCollect, func(s string) bool { return strings.Contains(conf, s) }) ||
			slices.ContainsFunc(pyWords.Split(conf, -1), func(w string) bool { return strings.HasPrefix(w, "-") && !pyOpt.MatchString(w) }) {
			return true, nil
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

// pyConf is what pytest may read of p (Opus #255 H4): a pytest.ini or
// pytest.toml whole, present or not; a TOML file naming pytest or escaping
// a key; INI pytest sections as iniconfig splits them: a column-0 '[' line
// naming pytest opens one, only a surely closed other header ends it.
func pyConf(p string, b []byte, present bool) string {
	s := string(b)
	switch {
	case !present:
		return ""
	case strings.HasPrefix(strings.TrimPrefix(path.Base(p), "."), "pytest."):
		return "present\n" + s
	case path.Ext(p) == ".toml" && (strings.Contains(strings.ToLower(s), "pytest") || strings.Contains(s, `\`)):
		return s
	case path.Ext(p) == ".toml":
		return ""
	}
	var out strings.Builder
	in := false
	for _, l := range strings.Split(splitLines.Replace(s), "\n") {
		if h, _, _ := strings.Cut(strings.Split(l, "#")[0], ";"); strings.HasPrefix(l, "[") {
			in = strings.Contains(strings.ToLower(l), "pytest") || in && !strings.HasSuffix(strings.TrimRight(h, " \t\r\n\v\f"), "]")
		}
		if in {
			out.WriteString(l + "\n")
		}
	}
	return out.String()
}

// pyFile is what the guard reads of one version of a Python file.
type pyFile struct {
	hooks bool           // it mentions a pytest hook, hookimpl or pytest_plugins
	plain string         // a conftest's first top-level line not a fixture, an import or plain data
	refs  map[string]int // "kind\x00reference\x00function"
}

func isHook(k ptok) bool {
	return k.kind == 'n' && (strings.HasPrefix(k.text, "pytest_") || slices.Contains([]string{"hookimpl", "hookwrapper", "hookspec", "HookimplMarker"}, k.text)) ||
		k.kind == 's' && (strings.Contains(k.text, "pytest_") || strings.Contains(k.text, "hookimpl"))
}

// pyKind classifies the token t[i] (pyClass), and names it.
func pyKind(t []ptok, i int) (byte, string) {
	k, dot := t[i], i > 0 && t[i-1].text == "."
	switch {
	case k.kind == 's' && (strings.Contains(k.text, "_pytest") || strings.Contains(k.text, "pluggy")):
		return 'p', "a string naming _pytest or pluggy"
	case k.kind != 'n':
	case k.text == "main" && dot && i > 1 && t[i-2].text == "pytest": // a session in the session
		return 'p', "pytest.main"
	case !dot && slices.Contains(pyBare, k.text):
		return 'f', k.text
	case dot && pyExec.MatchString(k.text):
		return 'e', k.text
	}
	return pyClass[k.text], k.text
}

func named(t []ptok, name string) bool {
	return slices.ContainsFunc(t, func(k ptok) bool { return k.kind == 'n' && k.text == name })
}

// pyScan reads one version of p when want; unverifiable, it blocks p. A
// def or a __main__ guard is a scope; the rest, class bodies too, is import.
func (r *run) pyScan(want bool, p, blob string) (fa *pyFile, ok bool, err error) {
	fa = &pyFile{refs: map[string]int{}}
	src, ok, err := r.pyRead(want, p, blob)
	if !want || !ok || err != nil {
		return fa, ok, err
	}
	lines, why, err := pyTokens(r.ctx, src)
	if err != nil {
		return nil, false, err
	}
	defer func() { // a panic of the guard's own: unverifiable, named
		if e := recover(); e != nil {
			add(&r.blocks, p, fmt.Sprintf("unverifiable: an internal guard error: %v", e))
			ok = false
		}
	}()
	type block struct {
		indent int
		fn     string // the outermost function: "" at import
	}
	stack, deco, fixture := []block{{indent: -1}}, []ptok(nil), true
	for _, l := range lines {
		if r.work += 2 * (len(l.toks) + len(l.inner)); r.work > maxWork {
			break
		} else if err := r.ctx.Err(); err != nil {
			return nil, false, err
		}
		for l.indent <= stack[len(stack)-1].indent {
			stack = stack[:len(stack)-1]
		}
		t, b := l.toks, block{l.indent, stack[len(stack)-1].fn}
		if t[0].text == "@" {
			fixture = fixture && len(t) > 1 && (t[1].text == "fixture" || len(t) > 3 && t[1].text == "pytest" && t[3].text == "fixture")
			deco = append(append(deco, t...), l.inner...)
			continue
		}
		kw, name, d := "", "", t[b2i(t[0].text == "async"):]
		if len(d) > 1 && (d[0].text == "def" || d[0].text == "class") {
			kw, name = d[0].text, d[1].text
		}
		if path.Base(p) == "conftest.py" && len(stack) == 1 && fa.plain == "" && !pyPlain(t, kw == "def" && fixture && len(deco) > 0) {
			fa.plain = fmt.Sprint(t[:min(len(t), 12)])
		}
		if kw == "def" {
			b.fn = cmp.Or(b.fn, name)
		} else if len(t) > 4 && t[0].text == "if" && t[1].text == "__name__" && strings.Trim(t[4].text, `"'`) == "__main__" {
			b.fn = cmp.Or(b.fn, "__main__")
		}
		if kw != "" || t[len(t)-1].text == ":" {
			stack = append(stack, b)
		}
		fn, code := b.fn, append(append(deco, t...), l.inner...)
		if fn == "" && (t[0].text == "import" || t[0].text == "from") {
			fn = "(import)"
		}
		fa.hooks = fa.hooks || slices.ContainsFunc(code, isHook)
		for i := range code {
			if kind, what := pyKind(code, i); kind != 0 {
				fa.refs[string(kind)+"\x00"+what+"\x00"+fn]++
			}
		}
		if kw == "def" && pyTested(p) && slices.Contains(pyFixtures, name) {
			fa.refs["p\x00a builtin fixture overridden ("+name+")\x00"+fn]++
		}
		deco, fixture = nil, true
	}
	if r.work > maxWork {
		why = "its analysis is over the guard's work bound"
	}
	if why != "" {
		add(&r.blocks, p, "unverifiable: "+why)
	}
	return fa, why == "", nil
}

func (k ptok) String() string { return k.text }

// pyPlain: a conftest's top-level line is a fixture (fixture: under fixture
// decorators only), an import (not *; fixture only from pytest), a
// docstring, or constants (not f-strings) bound to a name not collect_ignore*.
func pyPlain(t []ptok, fixture bool) bool {
	constant := func(k ptok) bool {
		return k.kind == 's' && !strings.ContainsAny(k.text[:strings.IndexAny(k.text, `"'`)], "fFtT") || k.kind == '1' ||
			slices.Contains([]string{"True", "False", "None"}, k.text) || k.kind == 'o' && strings.Contains("[](){},:-+", k.text)
	}
	switch {
	case slices.ContainsFunc(t, func(k ptok) bool { return k.text == ";" }):
		return false
	case fixture:
		return true
	case t[0].text == "import" || t[0].text == "from":
		return !slices.ContainsFunc(t, func(k ptok) bool { return k.text == "*" }) &&
			(!named(t, "fixture") || t[0].text == "from" && len(t) > 1 && t[1].text == "pytest" && !named(t, "as"))
	}
	return !slices.ContainsFunc(t, func(k ptok) bool { return k.kind != 's' || !constant(k) }) || len(t) > 2 && t[0].kind == 'n' &&
		t[1].text == "=" && !strings.HasPrefix(t[0].text, "collect_ignore") && !slices.ContainsFunc(t[2:], func(k ptok) bool { return !constant(k) })
}

// pyFileCompare applies the rules to the change at p: base b (empty when
// added), candidate a. A reference added blocks by class and place.
func (r *run) pyFileCompare(p string, b, a *pyFile) {
	if a.plain != "" {
		add(&r.blocks, p, "unverifiable: a conftest.py holding more than fixtures, imports and plain data ("+a.plain+")")
	}
	if a.hooks || b.hooks {
		add(&r.blocks, p, "a file holding a pytest hook, hookimpl or pytest_plugins changed")
	}
	for _, k := range keys(b.refs, a.refs) {
		f := strings.Split(k, "\x00") // kind, reference, function
		at, top, tested := p+": "+f[1]+" in "+cmp.Or(f[2], "module level"), f[2] == "", pyTested(p)
		if a.refs[k] > b.refs[k] && (f[0] == "p" || (f[0] == "f" || f[0] == "e") && (tested || top) || f[0] == "o" && tested && top) {
			add(&r.blocks, at, "unverifiable: a new pytest-internals, namespace, exit or output reference where it can forge results (decision A)")
		} else if a.refs[k] != b.refs[k] {
			add(&r.warnings, at, "an exit, output, process, namespace or pytest reference added, removed or changed: review it")
		}
	}
}
