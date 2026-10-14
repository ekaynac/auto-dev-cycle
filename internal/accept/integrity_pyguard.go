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

// The Python guard's forgery blocks (P14c-2a; #226 entries 73, 75, 84):
// hooks in either version of a file; a conftest past fixtures and plain
// data; configuration, markers, compiled files; base collection settings
// (M3); decision A's references (pytest internals and builtin fixtures
// anywhere; namespace, exit and process ones in test files and at import;
// others warn); the import-time allowlist (integrity_pyimport.go). Until
// 2a3 and 2b, P14b's rule still blocks .py.
var (
	pyConfs   = []string{"pytest.ini", ".pytest.ini", "pytest.toml", ".pytest.toml", "tox.ini", "setup.cfg", "pyproject.toml"}
	pyCollect = strings.Fields("python_files python_classes python_functions norecursedirs testpaths confcutdir " +
		"collect_imported_tests consider_namespace_packages usefixtures empty_parameter_set_mark")
	// pyOpt: the options a base may set (collection and results untouched).
	pyOpt   = regexp.MustCompile(`^-(q+|v+|r\w*|l|s|W|-(strict|strict-markers|strict-config|showlocals|tb|color|durations|capture))$`)
	pyWords = regexp.MustCompile(`[\s"',\[\]=]+`)
	pyKey   = regexp.MustCompile(`\n(\[|[A-Za-z_][\w.-]*[ \t]*[=:])`) // the next key or section
	pyNote  = regexp.MustCompile(`(?m)^[ \t]*[#;].*$`)                // a comment line
	// splitLines breaks lines where str.splitlines does (iniconfig's lines).
	splitLines = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n", "\x1c", "\n", "\x1d", "\n", "\x1e", "\n",
		"\u0085", "\n", "\u2028", "\n", "\u2029", "\n")
	pyLoaded   = []string{".pyc", ".pyo", ".so", ".pyd", ".pth", "sitecustomize.py", "usercustomize.py", "entry_points.txt"}
	pyBare     = strings.Fields("globals locals vars setattr delattr exec eval compile __import__") // builtins, not attributes
	pyExec     = regexp.MustCompile(`^(exec|spawn)[lv]p?e?$`)                                       // os.exec*, os.spawn*
	pyFixtures = strings.Fields("cache capfd capfdbinary caplog capsys capsysbinary capteesys doctest_namespace monkeypatch " +
		"pytestconfig pytester record_property record_testsuite_property record_xml_attribute recwarn subtests testdir " +
		"tmp_path tmp_path_factory tmpdir tmpdir_factory")
	// pyClass: 'p' pytest internals; 'f' namespace, frame, comparison; 'e' exit, process; 'o' output, thread.
	pyClass = map[string]byte{}
)

func init() {
	for c, names := range map[byte]string{
		'p': "_pytest pluggy pluginmanager HookspecMarker ihook record_xml_attribute _xml",
		'f': "__dict__ __builtins__ builtins __getattr__ __getattribute__ __setattr__ __delattr__ __setitem__ __delitem__ importlib modules __globals__ " +
			"__code__ __subclasses__ __bases__ settrace setprofile _getframe currentframe f_locals f_globals f_back tb_frame gi_frame get_objects " +
			"get_referrers get_referents __eq__ __ne__ __bool__ __contains__ __instancecheck__ __subclasscheck__ __lt__ __le__ __gt__ __ge__",
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
	return strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".egg") || slices.Contains([]string{"_darcs", "build", "CVS", "dist", "node_modules", "venv", "{arch}"}, d)
}

// pyTested: p is a conftest.py or a test_*.py or *_test.py pytest walks.
func pyTested(p string) bool {
	b := path.Base(p)
	return b == "conftest.py" || strings.HasSuffix(b, ".py") && (strings.HasPrefix(b, "test_") || strings.HasSuffix(b, "_test.py")) && walked(p, pySkipDir)
}

// python is the guard for a base holding Python.
func (r *run) python(changes []Change) error {
	custom, configured, paths, err := r.pyCustom()
	if err != nil {
		return err
	}
	var cache []pyBaseTest // the base's test files, read once (Mythos #267 L3)
	var cacheErr error
	tests := func() ([]pyBaseTest, error) {
		if cache == nil && cacheErr == nil {
			cache, cacheErr = r.pyBaseTests()
			cache = append(cache, pyBaseTest{}) // read, even when empty
		}
		return cache, cacheErr
	}
	var changed []string // the changed modules' paths
	for _, c := range changes {
		for _, q := range []string{c.Src, c.Dst} {
			if path.Ext(q) == ".py" {
				changed = append(changed, strings.TrimSuffix(strings.TrimSuffix(q, ".py"), "/__init__"))
			}
		}
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
				} else if base == "pyproject.toml" && !configured && (src && p == c.Src) != (dst && p == c.Dst) { // Opus #263 L1
					add(&r.blocks, p, "a pyproject.toml added or removed in a base without pytest configuration (rootdir moves)")
				}
			}
		}
		if path.Ext(c.Src) != ".py" && path.Ext(c.Dst) != ".py" {
			continue
		} else if custom {
			add(&r.blocks, c.Dst, "unverifiable: a pytest configuration of the base moves collection off its defaults or is unread")
			continue
		}
		judged := pyTested(c.Dst) || r.pyTestDir(c.Dst)
		if why, err := r.pyShadow(c, judged, paths, tests); err != nil {
			return err
		} else if why != "" {
			add(&r.blocks, c.Dst, why)
		}
		b, ok, err := r.pyScan(src && path.Ext(c.Src) == ".py", c.Src, c.SrcBlob, judged)
		a, ok2, err2 := r.pyScan(dst && path.Ext(c.Dst) == ".py", c.Dst, c.DstBlob, judged)
		if err = cmp.Or(err, err2); err != nil {
			return err
		} else if ok && ok2 {
			if src && dst && path.Ext(c.Src) == ".py" && path.Ext(c.Dst) == ".py" { // a changed module a base condition reads (Mythos #267 H4)
				if all, err := tests(); err != nil {
					return err
				} else if why := pyReach(c, b, a, all); why != "" {
					add(&r.blocks, c.Dst, why)
				}
			}
			r.pyFileCompare(c.Dst, b, a, judged, changed)
		}
	}
	return nil
}

// pyCustom: a base configuration moves collection off the defaults (a
// setting, an option not pyOpt, an addopts path), or is unread (M3).
func (r *run) pyCustom() (custom, configured bool, paths []string, err error) {
	for _, p := range slices.Sorted(maps.Keys(r.files)) {
		if !slices.Contains(pyConfs, path.Base(p)) {
			continue
		}
		b, why, err := r.file(r.files[p])
		conf := pyConf(p, b, true)
		if err != nil || why != "" || slices.ContainsFunc(pyCollect, func(s string) bool { return strings.Contains(conf, s) }) ||
			slices.ContainsFunc(pyWords.Split(conf, -1), func(w string) bool { return strings.HasPrefix(w, "-") && !pyOpt.MatchString(w) }) {
			return err == nil, true, nil, err
		}
		configured = configured || conf != ""
		for _, v := range strings.Split(conf, "pythonpath")[1:] { // its directories, from the file's (Mythos #266 r2 L2, Opus #267 L1)
			if v = pyKey.Split(v, 2)[0]; path.Ext(p) == ".toml" && strings.Contains(v, `\`) {
				return true, true, nil, nil // an escape: unread
			}
			for _, w := range pyWords.Split(strings.TrimLeft(v, " \t=:"), -1) {
				if w != "" {
					paths = append(paths, path.Join(path.Dir(p), w))
				}
			}
		}
		for _, v := range strings.Split(conf, "addopts")[1:] { // Opus #263 L3
			prev := ""
			for _, w := range pyWords.Split(strings.TrimLeft(pyNote.ReplaceAllString(pyKey.Split(v, 2)[0], ""), " \t=:"), -1) {
				if w != "" && !strings.HasPrefix(w, "-") && !slices.Contains([]string{"--tb", "--color", "--durations", "--capture", "-W"}, prev) {
					return true, true, nil, nil
				}
				prev = w
			}
		}
	}
	return false, configured, paths, nil
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

// pyConf is what pytest may read of p (H4): pytest.ini or pytest.toml
// whole; TOML naming pytest or escaping; iniconfig's pytest sections (a '['
// line naming pytest opens one, only a surely closed other header ends it).
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
	hooks bool               // it mentions a pytest hook, hookimpl or pytest_plugins
	finds map[string]*pyFind // findings at import, by statement
	// its import statements; the names they bind, and the names defined at import
	imports         string
	imported, local map[string]bool
	mods            []string          // the names its imports bind or name
	binds           map[string]string // the statements binding each name at import
	refs            map[string]int    // "kind\x00reference\x00function"
}

// pyFind is one import-time finding: strict ones never pass on the base.
type pyFind struct {
	why    string
	strict bool
	n      int
	calls  []string // the names its statement calls ("": not a plain name); a condition's: the names it reads
	cond   bool     // a skipif or xfail condition (Opus #267 H1)
	always bool     // blocks in every file: a condition, __path__ (Mythos #267 H3)
}

// passes: a's finding k is unchanged from the base b, its imports
// unchanged, and it calls only imported names (Opus #266 widening 1).
func (a *pyFile) passes(k string, b *pyFile, changed []string) bool {
	f, base := a.finds[k], b.finds[k]
	if base == nil || base.n < f.n || f.strict || a.imports != b.imports {
		return false
	} else if f.cond { // each name bound at import as in the base, no import naming a changed module (Opus #267 M4)
		return !slices.ContainsFunc(f.calls, func(n string) bool { // a def's or class's body may change under its line
			return (!a.imported[n] || a.local[n]) && (!a.local[n] || a.binds[n] != b.binds[n] || strings.Contains(a.binds[n], "[def ") || // Mythos #267 H1
				strings.Contains(a.binds[n], "[class ") || strings.Contains(a.binds[n], "[async "))
		}) &&
			!slices.ContainsFunc(changed, func(q string) bool { return slices.Contains(a.mods, path.Base(q)) })
	}
	return !slices.ContainsFunc(f.calls, func(c string) bool { return !a.imported[c] || a.local[c] })
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

// pyScan reads one version of p when want (unverifiable: blocks p). Only a
// def body or an exact __main__ guard is not import code (Opus #263 H2);
// __init_subclass__, __set_name__ and a type subclass's methods run when a
// class is made, so are (Mythos #263 L1).
func (r *run) pyScan(want bool, p, blob string, judged bool) (fa *pyFile, ok bool, err error) {
	fa = &pyFile{refs: map[string]int{}, finds: map[string]*pyFind{}, imported: map[string]bool{}, local: map[string]bool{}, binds: map[string]string{}}
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
		fn     string // the outermost function, "" at import
		meta   bool   // a class that may be a metaclass (Mythos #263 L1, Opus #266 L1)
	}
	stack, deco, conftest, tested := []block{{indent: -1}}, []ptok(nil), path.Base(p) == "conftest.py", pyTested(p)
	rebound, classes := pyRebound(lines), map[string]bool{} // Opus #266 M2: a pre-pass
	pure, aliases := pyPure(lines)
	find := func(st []ptok, why string, strict bool) {
		if f := fa.finds[fmt.Sprint(st)]; f != nil {
			f.n, f.strict = f.n+1, f.strict || strict // Mythos #266 L2
		} else {
			fa.finds[fmt.Sprint(st)] = &pyFind{why, strict, 1, pyCalls(st), false, false}
		}
	}
	for _, l := range lines {
		if r.work += 2 * (len(l.toks) + len(l.inner)); r.work > maxWork {
			break
		} else if err := r.ctx.Err(); err != nil {
			return nil, false, err
		}
		for l.indent <= stack[len(stack)-1].indent {
			stack = stack[:len(stack)-1]
		}
		t, top := l.toks, stack[len(stack)-1]
		fn, b, code := top.fn, block{l.indent, top.fn, false}, append(append(deco, t...), l.inner...)
		if fn == "" && !(pyMain(t) && !rebound) {
			for _, st := range pySplit(t, ";") {
				if why := pyAllowed(st, conftest); why != "" {
					find(st, why, strings.HasPrefix(why, "an import binding")) // Mythos #266 L1
				}
				binds, imported := pyBinds(slices.Concat(st, l.inner)) // a walrus in an f-string binds too (Mythos #266 H1)
				for _, n := range binds {
					if slices.Contains([]string{"__path__", "__spec__", "__loader__"}, n) { // in every file (Mythos #267 H3)
						find(st, "a package's import path, spec or loader bound at import ("+n+")", false)
						fa.finds[fmt.Sprint(st)].always = true
					}
					if judged && (strings.HasPrefix(n, "__") && strings.HasSuffix(n, "__") && n != "__all__" && n != "__slots__" || !imported && pyBound[n] != "") {
						find(st, "a dunder or an allowlisted name bound at import ("+n+")", true) // Opus #266 H1
					}
					fa.imported[n], fa.local[n], fa.binds[n] = fa.imported[n] || imported, fa.local[n] || !imported, fa.binds[n]+fmt.Sprint(st)+"\n"
				}
				if imported {
					fa.imports = fa.imports + fmt.Sprint(st) + "\n"
					for _, k := range st {
						fa.mods = append(fa.mods, k.text)
					}
				}
			}
			if !pyConst(l.inner) {
				find(slices.Concat(t, l.inner), "code run at import in an f-string field ("+ptext(t)+")", false)
			}
		}
		if line := slices.Concat(t, l.inner); pySkipString(line, pure, aliases) { // in every file (Opus #267 H1)
			if f := fa.finds["cond "+fmt.Sprint(t)]; f != nil {
				f.n++
			} else {
				fa.finds["cond "+fmt.Sprint(t)] = &pyFind{"a skipif or xfail condition that may be a string, code pytest runs (" + ptext(t) + ")", false, 1, pyNames(line), true, true}
			}
		}
		if why := pyFixtureName(t); (t[0].text == "@" || fn == "") && named(t, "fixture") && why != "" {
			fa.refs["p\x00"+why+"\x00"+fn]++ // Opus #263 H3, Mythos #263 H2
		}
		if t[0].text == "@" {
			deco = code
			continue
		}
		kw, name, d := "", "", t[b2i(t[0].text == "async"):]
		if len(d) > 1 && (d[0].text == "def" || d[0].text == "class") {
			kw, name = d[0].text, d[1].text
		}
		dunder := strings.HasPrefix(name, "__") && strings.HasSuffix(name, "__")
		if kw == "def" && !(top.meta && dunder) && !slices.Contains(strings.Fields("__init_subclass__ __set_name__ __class_getitem__ __mro_entries__ __prepare__"), name) {
			b.fn = cmp.Or(fn, name)
		} else if kw == "class" {
			b.meta, classes[name] = pyMeta(pySplit(t, ":")[0], classes), true
		} else if pyMain(t) && !rebound {
			b.fn = cmp.Or(fn, "__main__")
		}
		if kw != "" || t[len(t)-1].text == ":" {
			stack = append(stack, b)
		}
		if fn == "" && (t[0].text == "import" || t[0].text == "from") {
			fn = "(import)"
		}
		fa.hooks = fa.hooks || slices.ContainsFunc(code, isHook)
		for i := range code {
			if kind, what := pyKind(code, i); kind != 0 {
				fa.refs[string(kind)+"\x00"+what+"\x00"+fn]++
			}
		}
		if fn == "" && len(t) > 2 && slices.Contains(pyFixtures, t[0].text) && t[1].text == "=" && t[2].text != "=" && (tested || named(code, "fixture")) {
			fa.refs["p\x00a builtin fixture overridden by assignment ("+t[0].text+")\x00"]++ // Mythos #263 M1
		}
		if kw == "def" && (tested || named(deco, "fixture")) && slices.Contains(pyFixtures, name) { // in any file (H3)
			fa.refs["p\x00a builtin fixture overridden ("+name+")\x00"+fn]++
		}
		if judged && (kw == "def" && dunder || slices.ContainsFunc(pySplit(t, "="), func(s []ptok) bool { return len(s) > 0 && s[len(s)-1].text == "__class__" })) {
			find(t, "a dunder method or __class__ bound in a test file ("+ptext(t)+")", true) // Opus #263 M1
		}
		deco = nil
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

func ptext(t []ptok) string { return fmt.Sprint(t[:min(len(t), 12)]) }

// pyStr is the string literal k, prefix and quotes stripped; "" for an
// f- or t-string, an escape or not a string (not read as a literal).
func pyStr(k ptok) string {
	if q := strings.IndexAny(k.text, `"'`); k.kind == 's' && !strings.ContainsAny(k.text[:q], "fFtT") && !strings.Contains(k.text, `\`) {
		return strings.Trim(k.text[q:], `"'`)
	}
	return ""
}

// pyFixtureName: on a line naming fixture, a builtin fixture named by
// name=, or a name not one string literal (a variable, **): Mythos #263 H2.
func pyFixtureName(t []ptok) string {
	for i := range t {
		if i+1 < len(t) && t[i].text == "*" && t[i+1].text == "*" {
			return "a fixture name not a literal (**)"
		} else if t[i].text != "name" || i+2 >= len(t) || t[i+1].text != "=" || t[i+2].text == "=" {
			continue
		} else if v := pyStr(t[i+2]); v == "" || i+3 >= len(t) || t[i+3].text != "," && t[i+3].text != ")" {
			return "a fixture name not a literal"
		} else if slices.Contains(pyFixtures, v) {
			return "a builtin fixture overridden by name= (" + v + ")"
		}
	}
	return ""
}

// pyMain: t is exactly if __name__ == "__main__": (either way round).
func pyMain(t []ptok) bool {
	main := func(n, s ptok) bool { return n.text == "__name__" && pyStr(s) == "__main__" }
	return len(t) == 6 && t[0].text == "if" && t[2].text == "=" && t[3].text == "=" && t[5].text == ":" && (main(t[1], t[4]) || main(t[4], t[1]))
}

// pyFileCompare: the change at p from b to a; a reference added blocks by class and place.
func (r *run) pyFileCompare(p string, b, a *pyFile, judged bool, changed []string) {
	for _, k := range slices.Sorted(maps.Keys(a.finds)) { // new findings: blocks where judged, else warnings (Opus #266 M1)
		if f := a.finds[k]; !a.passes(k, b, changed) && (judged || f.always) {
			add(&r.blocks, p, "unverifiable: "+f.why)
		} else if !a.passes(k, b, changed) {
			add(&r.warnings, p, f.why+": review it (decision A)")
		} else if slices.ContainsFunc(changed, func(q string) bool { return slices.Contains(a.mods, path.Base(q)) }) { // Mythos #266 L3
			add(&r.warnings, p, f.why+": unchanged, but its imports name a changed module: review it")
		}
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
