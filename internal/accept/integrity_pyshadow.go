// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"cmp"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
)

// P14c-2a-shadow (Mythos #266 r2 M3, Opus and Mythos #267): modules
// shadowing what the tests import, and changed modules a base test's
// skipif or xfail condition reads.

// pyBase is what the shadow rules read of the base, once per run
// (Mythos #267 L3): its sys.path entries, its modules' dotted names, its
// files read on demand.
type pyBase struct {
	r            *run
	entries      []string
	basedirs     []string // the test files' basedirs below the root, in either tree
	paths        []string // the base's paths, sorted
	read         bool
	err          error
	mods         map[string]string      // a base module's dotted name, to its path
	alts         map[string]int         // how many base files answer to a dotted name (Astra #272 H1)
	files        map[string]*pyBaseFile // the base files read, by path
	tests        []*pyBaseFile          // the judged ones (Opus #270 H2, M4)
	holders      []*pyBaseFile          // the application modules they import, one hop (Mythos #270 M2)
	byName       map[string][]string    // a name, to the base modules binding it (Opus #272 M3)
	holderUnread string                 // a module a judged file imports, unread on the way to its holder (the review of 1ceb79c)
	sysUnread    bool                   // a judged file changes sys.path in a way the guard does not read (#270 M1)
	tree         map[string]string      // the files file reads: nil, the base; the candidate for pyStarCandidate
	cand         *pyBase                // the candidate's files, for its package __init__.py stars
	dirs         map[string][]string    // a directory's direct entries, built once (Astra #279 M1)
	bad          map[string]bool        // a base closure module that is not inert, writes, imports dynamically or uses sys
	linkTargets  []string               // a base symlink's target in the root; "" when out of it (Opus #281 H4)
	opaque       map[string]bool        // a module name under a base symlink or gitlink (Opus #279 M1)
	other        map[string]string      // a module name's non-source form (.pyc, an extension), its path (Astra #281 r4 H3)
	changed      map[string]bool        // the changes' paths: a changed judged file's own code is P14c-2a-allow's, a changed module's warns

	// entries are main's (the root, pythonpath, the basedirs) and resolve; extra are the roots an install
	// adds (pyInstall), which name modules for pyShadow only (Opus #275 H2)
	extra         []string
	shadowEntries []string
	shadowMods    map[string]string // a dotted name over every root, to a base path
}

// pyBaseFile is what a base Python file imports, and its conditions.
type pyBaseFile struct {
	path      string
	unread    bool
	mods      []string          // the dotted names it imports (a from-import's as module.name)
	from      map[string]string // a name it binds by import, to that dotted name (import a.b: a to a.b)
	named     map[string]bool   // the names a from-import binds
	locals    map[string]bool   // the names its module-level statements bind
	stars     []string          // the modules it star-imports
	dynamic   bool              // a module-level statement not pyStatic: it may bind names at run time
	conds     [][]ptok          // its condition lines, and the bindings they read (Opus #270 H2)
	units     [][]ptok          // its module-level statements, def and class bodies (Mythos #270 M3)
	binders   map[string][]int  // a name, to the units binding it
	sysUnread bool              // it names sys.path, or sys in a way pySysUse does not read

	plain   bool     // a package whose submodules resolve in its own directory: read, every module-level statement pyInert (Astra #275 H6)
	starSrc []string // x of each exact "from .x import *" at module level (P14c-2a-star)
	badStar bool     // any other import followed by * anywhere in its tokens
	starWhy *string  // starCheck's verdict, cached (Astra #279 M1)
	active  bool     // a write word at import: setattr, __dict__, vars, globals, … (Opus #281 H2)
	impure  bool     // a statement run at import outside pyAllowed (P14c-2a-allow judges a test file's)
	runs    bool     // a dynamic import or code from a string: import_module, __import__, importlib, runpy, exec, eval, compile (Opus #281 H3)
	writes  []string // its attribute, subscript and del targets at import, whatever their head (the split)
}

// pyEntries are the sys.path entries the tests import from (Mythos #267
// r2 M1): the root, each base pythonpath entry, and each test file's
// basedir, in the base and in the candidate (the security review).
func (r *run) pyEntries(paths []string) (entries, basedirs []string) {
	for _, tree := range []map[string]string{r.files, r.cand} {
		for q := range tree {
			if d := pyBasedir(tree, q); pyTested(q) && d != "." {
				basedirs = append(basedirs, d)
			}
		}
	}
	slices.Sort(basedirs)
	basedirs = slices.Compact(basedirs)
	entries = slices.Concat([]string{"."}, paths, basedirs)
	slices.Sort(entries)
	return slices.Compact(entries), basedirs
}

// pyBasedir is test file q's basedir in tree: its first directory up with
// no __init__.py.
func pyBasedir(tree map[string]string, q string) string {
	d := path.Dir(q)
	for ; d != "." && tree[d+"/__init__.py"] != ""; d = path.Dir(d) {
	}
	return d
}

// pyRel is p relative to the sys.path entry e, if under it.
func pyRel(p, e string) (string, bool) {
	if e == "." {
		return p, p != "."
	}
	return strings.CutPrefix(p, e+"/")
}

// pyModNames are the dotted names the module at p answers to: relative
// to each sys.path entry holding it (Mythos #267 r2 H1, M1); in a judged
// file, every trailing suffix (pySuffixes).
func pyModNames(p string, entries []string, judged bool) (names []string) {
	if judged {
		return pySuffixes(p)
	}
	m := strings.TrimSuffix(strings.TrimSuffix(p, ".py"), "/__init__")
	for _, e := range entries {
		if rel, ok := pyRel(m, e); ok {
			names = append(names, strings.ReplaceAll(rel, "/", "."))
		}
	}
	return names
}

// pySuffixes are the dotted names the module at p (dir/m) may answer to.
func pySuffixes(p string) (s []string) {
	for d, rest := "", strings.Split(strings.TrimSuffix(strings.TrimSuffix(p, ".py"), "/__init__"), "/"); len(rest) > 0; rest = rest[:len(rest)-1] {
		d = strings.TrimSuffix(rest[len(rest)-1]+"."+d, ".")
		s = append(s, d)
	}
	return s
}

// load names the base's modules and reads its judged files.
func (pb *pyBase) load() error {
	if pb.read {
		return pb.err
	}
	pb.read, pb.mods, pb.alts, pb.files, pb.paths = true, map[string]string{}, map[string]int{}, map[string]*pyBaseFile{}, slices.Sorted(maps.Keys(pb.r.files))
	for _, q := range pb.paths {
		if pb.err = pb.r.ctx.Err(); pb.err != nil { // Mythos #270 M5
			return pb.err
		} else if path.Ext(q) == ".py" && (pyTested(q) || pb.r.pyTestDir(q)) {
			f, err := pb.file(q)
			if pb.err = err; err != nil {
				return err
			}
			pb.tests, pb.sysUnread = append(pb.tests, f), pb.sysUnread || f.sysUnread
		}
	}
	pb.shadowEntries, pb.shadowMods = slices.Concat(pb.entries, pb.extra), map[string]string{}
	slices.Sort(pb.shadowEntries)
	pb.shadowEntries = slices.Compact(pb.shadowEntries)
	for _, q := range pb.paths {
		if path.Ext(q) != ".py" {
			continue
		} else if pb.err = pb.r.ctx.Err(); pb.err != nil { // main's indexing, uncharged (its work-bound rows), but cancellable (Astra #281 r3 M1)
			return pb.err
		}
		for _, n := range pyModNames(q, pb.entries, false) {
			pb.mods[n] = cmp.Or(pb.mods[n], q)
		}
		for _, n := range pyModNames(q, pb.shadowEntries, false) { // a name an added root gives a second file is unread (Opus #275 H2)
			pb.shadowMods[n], pb.alts[n] = cmp.Or(pb.shadowMods[n], q), pb.alts[n]+1
		}
	}
	if err := pb.closure(); err != nil {
		pb.err = err
		return err
	}
	for _, f := range slices.Clone(pb.tests) { // the application modules a judged file imports, one hop (Mythos #270 M2)
		for _, m := range f.mods {
			if strings.Count(m, ".") >= pyMaxDepth { // the closure made the base unread (Astra #281 M1)
				pb.holderUnread = cmp.Or(pb.holderUnread, "the module "+f.path+" imports a name past the depth bound")
				continue
			}
			r, unread, err := pb.resolve(m) // and the module a re-export lands in (Opus #272 M1); unread: no holder known
			pb.holderUnread = cmp.Or(pb.holderUnread, unread)
			for _, d := range []string{m, r} {
				parts := strings.Split(d, ".")
				i, why, perr := pb.prefix(parts, len(parts)) // feature.py beside feature/__init__.py, a namespace decoy (Astra #272 r2 H1)
				if err = cmp.Or(err, perr); err != nil {
					pb.err = err
					return err
				} else if why != "" {
					pb.holderUnread = cmp.Or(pb.holderUnread, why)
					continue
				} else if d = strings.Join(parts[:max(i, 0)], "."); i < 1 {
					continue
				} else if h, err := pb.file(pb.mods[d]); err != nil {
					pb.err = err
					return err
				} else if !slices.Contains(pb.tests, h) && !slices.Contains(pb.holders, h) {
					pb.holders, pb.sysUnread = append(pb.holders, h), pb.sysUnread || h.sysUnread // a module a judged file reaches: its sys.path unread (Opus #272 (b))
				}
			}
		}
	}
	return nil
}

// file reads base file q once: every import statement, after ; or a
// compound header's : too (Opus #267 M3), relative ones from q's package,
// constant dynamic ones; and its conditions, a string one's code too,
// closed over the module-level statements and def or class bodies binding
// a name they read, to a fixed point (Opus #270 H2).
func (pb *pyBase) file(q string) (*pyBaseFile, error) {
	if f := pb.files[q]; f != nil {
		return f, nil
	}
	f := &pyBaseFile{path: q, from: map[string]string{}, named: map[string]bool{}, locals: map[string]bool{}}
	pb.files[q] = f
	tree := pb.r.files
	if pb.tree != nil {
		tree = pb.tree
	}
	b, why, err := pb.r.file(tree[q])
	lines, why2, err2 := pyTokens(pb.r.ctx, b)
	if err = cmp.Or(err, err2, pb.r.ctx.Err()); err != nil {
		return nil, err
	}
	f.unread = why+why2 != "" || pb.tree == nil && pb.r.links[q] != "" // a base symlink is read as its link text (Opus #281 H4); the candidate's closure blocks on reaching one
	pure, aliases := pyPure(pb.r.ctx, lines)
	rel, in := pyRel(path.Dir(q), pyBasedir(tree, q))
	pkg := strings.Split(rel, "/") // q's package (a relative import's base)
	if !in {
		pkg = nil
	}
	var units [][]ptok            // module-level statements, def and class bodies
	binders := map[string][]int{} // a name, to the units binding it
	owner, indent, deco, bodies := -1, 0, []ptok(nil), []int(nil)
	future, inert, starSeen := false, true, map[string]bool{} // pyInert, every module-level statement
	conftest, defIn := path.Base(q) == "conftest.py", -1      // a def's body does not run at import
	trust := map[string]bool{}                                // the decorators' roots: pyTrusted proves each binding (Astra #281 r4 H2)
	stmt := func(st []ptok) bool {                            // inert: main's allowlist and the positive grammar (Astra #281 r4)
		r, ok := pyDeco(st)
		trust[r] = trust[r] || st[0].text == "@"
		return st[0].text == "@" && ok || st[0].text != "@" && pyStmt(st)
	}
	for _, l := range lines {
		line := slices.Concat(l.toks, l.inner)
		cond := pySkipString(line, len(l.toks), pure, aliases)
		f.sysUnread = f.sysUnread || !pySysUse(l.toks)
		if defIn >= 0 && l.indent <= defIn {
			defIn = -1
		}
		atImport := defIn < 0    // this line's header expressions run at import, a def's defaults too (Astra #281 r3 H2)
		for i, k := range line { // the words of a write by call or of code run from a string, anywhere (Opus #281 H2, H3)
			bare := i == 0 || line[i-1].text != "."
			f.active = f.active || atImport && (slices.Contains(pyWriteWords, k.text) || slices.Contains(pyImportPath, pyStr(k))) // getattr(M, '__path__')
			f.runs = f.runs || slices.Contains(pyRunWords, k.text) && !pyExactImport(line, i) || bare && slices.Contains([]string{"exec", "eval", "compile"}, k.text)
		}
		for _, st := range pySplit(l.toks, ";") { // a statement run at import: pyAllowed and pyStmt only (Opus #281 H1, Astra #281 r4)
			if len(st) == 0 || defIn >= 0 || len(st) == 5 && st[0].text == "from" && st[1].text == "." && st[4].text == "*" {
				continue // an exact relative star is P14c-2a-star's
			} else if pyMain(st) {
				defIn = l.indent // the exact __main__ guard's body does not run at import
				continue
			} else if names, _ := pyBinds(st); pyAllowed(st, conftest) != "" || !stmt(st) || !pyConst(l.inner) || slices.ContainsFunc(names, func(n string) bool {
				return strings.HasPrefix(n, "__") && strings.HasSuffix(n, "__") && n != "__all__" && n != "__slots__" || n == "object" // def __getattr__, __init_subclass__, …; a class(object) base rebound
			}) {
				f.impure = true
			}
			if d := st[b2i(st[0].text == "async"):]; d[0].text == "def" {
				defIn = l.indent
			}
		}
		for _, st := range pySplit(line, ";") { // any attribute, subscript or del target at import, whatever its head (the split)
			var targets [][]ptok
			for i := range st { // a.b.__path__.insert(…), a.__spec__…: the package's import path, moved in place, in a header too
				if i+2 < len(st) && st[i+1].text == "." && slices.Contains(pyImportPath, st[i+2].text) {
					targets = append(targets, st[:i+3])
				}
			}
			for len(st) > 0 && slices.Contains(pyCompound, st[0].text) { // an inline suite runs too (Astra #281 r3 H1)
				st = st[min(len(pySplit(st, ":")[0])+1, len(st)):]
			}
			parts := pyAssign(st)
			for _, tg := range parts[:len(parts)-1] {
				if len(tg) > 1 && tg[1].text == ":" {
					tg = tg[:1] // NAME: annotation = value binds NAME only
				}
				targets = append(targets, tg)
			}
			if len(st) > 1 && st[0].text == "del" {
				targets = append(targets, st[1:])
			}
			for _, tg := range targets {
				if atImport && slices.ContainsFunc(tg, func(k ptok) bool { return k.text == "." || k.text == "[" || k.text == "(" }) {
					f.writes = append(f.writes, ptext(tg)) // its first tokens only
				}
			}
		}
		for i := 1; i < len(l.toks); i++ { // a star import at token level, in any form (Astra #277 r2 H1)
			if l.toks[i].text == "*" && l.toks[i-1].text == "import" {
				if t := l.toks; l.indent == 0 && len(t) == 5 && t[0].text == "from" && t[1].text == "." && t[2].kind == 'n' {
					if !starSeen[t[2].text] { // each source once (Astra #279 M1)
						starSeen[t[2].text], f.starSrc = true, append(f.starSrc, t[2].text)
					}
				} else {
					f.badStar = true
				}
			}
		}
		if l.toks[0].text == "pytest_plugins" { // its modules are reached too (Opus #272 (b))
			for _, k := range line {
				if pyStr(k) != "" {
					f.mods = append(f.mods, pyStr(k))
				}
			}
		}
		for i, k := range line {
			if s := pyStr(k); cond && s != "" { // a string condition's code too (the security review)
				sl, _, err := pyTokens(pb.r.ctx, []byte(s))
				if err != nil {
					return nil, err
				}
				for _, sk := range sl {
					line = append(line, sk.toks...)
				}
			} else if i+2 < len(line) && slices.Contains([]string{"__import__", "import_module", "importorskip"}, k.text) && line[i+1].text == "(" && pyStr(line[i+2]) != "" {
				f.mods = append(f.mods, strings.TrimLeft(pyStr(line[i+2]), ".")) // a constant one (the security review)
			}
		}
		if cond {
			f.conds = append(f.conds, line)
		}
		for i, k := range l.toks {
			if i > 0 && l.toks[i-1].text != ";" && l.toks[i-1].text != ":" || k.text != "import" && k.text != "from" {
				continue
			}
			st := pySplit(l.toks[i:], ";")[0]
			for _, part := range pyImportParts(st) {
				d, as := pyJoin(pySplit(part, "as")[0]), part[len(part)-1].text
				if k.text == "from" { // a relative import from q's package (Mythos #267 r2 H3 i, the security review)
					mod := pyJoin(st[1:max(slices.IndexFunc(st, func(k ptok) bool { return k.text == "import" }), 1)])
					if rest := strings.TrimLeft(mod, "."); rest != mod {
						mod = strings.Trim(strings.Join(pkg[:max(len(pkg)-len(mod)+len(rest)+1, 0)], ".")+"."+rest, ".")
					}
					d, f.named[as] = strings.Trim(mod+"."+d, "."), true
				} else if !named(part, "as") {
					as = part[0].text // import a.b binds a, and keeps a.b (Mythos #267 r2 H2)
				}
				if star, ok := strings.CutSuffix(d, ".*"); ok {
					f.stars = append(f.stars, star)
				}
				f.mods, f.from[as] = append(f.mods, d), d
			}
		}
		switch {
		case owner >= 0 && l.indent > indent: // a def's or class's body
			units[owner] = append(units[owner], line...)
			if cond { // its unit is a condition (Mythos #270 H2); pyPure made a def holding one an alias
				bodies = append(bodies, owner)
			}
		case l.toks[0].text == "@":
			owner, deco, f.dynamic, inert = -1, append(deco, line...), true, false
		default:
			owner = -1
			for _, st := range pySplit(line, ";") {
				if len(st) == 0 {
					continue
				}
				f.dynamic = f.dynamic || !pyStatic(st)
				future = future || pyJoin(st) == "from__future__importannotations"
				inert = inert && pyInert(st, future)
				if bound, imported := pyBinds(st); !imported {
					if len(st) > 2 && st[0].kind == 'n' && st[1].text == "." && len(pyAssign(st)) == 1 {
						bound = append(bound, st[0].text) // FLAGS.update(…) changes FLAGS (Mythos #270 r2 L4)
					}
					for _, n := range bound {
						binders[n], f.locals[n] = append(binders[n], len(units)), true
					}
					if d := st[b2i(st[0].text == "async"):]; d[0].text == "def" || d[0].text == "class" {
						owner, indent = len(units), l.indent
					}
					units, deco = append(units, slices.Concat(deco, st)), nil
				}
			}
		}
	}
	taken, seen := make([]bool, len(units)), map[string]bool{}
	for _, u := range bodies {
		if !taken[u] {
			taken[u], f.conds = true, append(f.conds, units[u])
		}
	}
	for i := 0; i < len(f.conds); i++ { // the closure: f.conds grows with each unit taken
		for _, k := range f.conds[i] {
			if k.kind != 'n' || seen[k.text] {
				continue
			}
			seen[k.text] = true
			for _, u := range binders[k.text] {
				if !taken[u] {
					taken[u], f.conds = true, append(f.conds, units[u])
				}
			}
		}
	}
	for r, used := range trust {
		f.impure = f.impure || used && !pyTrusted(lines, r)
	}
	f.units, f.binders = units, binders
	f.plain = inert && !f.unread
	return f, nil
}

// pyStatic: st is a statement a star module may hold and stay read: an
// import, a def or class (not __getattr__), a docstring, NAME = value, or
// if TYPE_CHECKING: (an allowlist: the review of b5a634d).
func pyStatic(st []ptok) bool {
	d, parts := st[b2i(st[0].text == "async"):], pyAssign(st)
	switch {
	case d[0].text == "import" || d[0].text == "from" || len(st) == 1 && st[0].kind == 's':
		return true
	case d[0].text == "def" || d[0].text == "class":
		return len(d) > 1 && d[1].text != "__getattr__"
	}
	if len(parts) == 2 && len(parts[0]) == 1 && parts[0][0].kind == 'n' {
		return !slices.ContainsFunc(parts[1], func(k ptok) bool { return k.text == "(" }) // no call in the value: X = globals().update(…) (the review of d859e60)
	}
	return slices.Contains([]string{"ifTYPE_CHECKING:", "iftyping.TYPE_CHECKING:"}, pyJoin(st))
}

var (
	pyWriteWords = strings.Fields("__dict__ __setattr__ __setitem__ __delattr__ setattr delattr setitem vars globals") // a write by call (Opus #281 H2)
	pyRunWords   = strings.Fields("import_module __import__ importlib runpy run_module run_path")                      // an import the closure cannot follow (Opus #281 H3)
	pyDotted     = regexp.MustCompile(`^(?:'[A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*'|"[A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*")$`)     // one plain literal of an absolute dotted name (Astra #281 H3)
)

// pyMaxDepth bounds a dotted name's components: past it, the name is unread (Astra #281 M1).
const pyMaxDepth = 64

// pyExactImport: line[i], a dynamic-import word, is a form the closure
// follows exactly: `import importlib`, importlib.import_module, and
// import_module('mod') or __import__('mod') with one plain string literal
// of an absolute dotted name, no prefix, escape or concatenation.
func pyExactImport(line []ptok, i int) bool {
	at := func(j int, s string) bool { return j < len(line) && line[j].text == s }
	switch line[i].text {
	case "importlib":
		return line[0].text == "import" && len(line) == 2 || at(i+1, ".") && at(i+2, "import_module")
	case "import_module", "__import__":
		return at(i+1, "(") && i+3 < len(line) && line[i+2].kind == 's' && pyDotted.MatchString(line[i+2].text) && at(i+3, ")")
	}
	return false
}

// pyInert: module-level statement st of a package runs nothing at import
// that can move its submodules (Astra #275 H6), an allowlist: an import
// binding no dunder name (a star import as main treats it: its source's
// exports are not read, P14c-2a-star); a docstring;
// NAME = a pyLiteral (NAME no dunder but __all__ and __version__); or a
// def, not a dunder, whose defaults are pyLiterals and whose annotations
// are strings (from __future__ import annotations). A class body runs: not
// inert. The statements are pyLex's logical lines split at ";" outside
// brackets and strings, as CPython splits them; what pyLex cannot read as
// CPython does leaves the file unread, and so not plain.
func pyInert(st []ptok, future bool) bool {
	dunder := func(n string) bool { return strings.HasPrefix(n, "__") && strings.HasSuffix(n, "__") }
	d, parts := st[b2i(st[0].text == "async"):], pyAssign(st)
	switch {
	case d[0].text == "import" || d[0].text == "from":
		names, _ := pyBinds(st)
		return !slices.ContainsFunc(names, dunder)
	case len(parts) == 1 && !slices.ContainsFunc(st, func(k ptok) bool { return k.kind != 's' }):
		return pyLiteral(st) // a docstring
	case len(parts) == 2 && len(parts[0]) == 1 && parts[0][0].kind == 'n':
		n := parts[0][0].text
		return (!dunder(n) || n == "__all__" || n == "__version__") && pyLiteral(parts[1])
	case len(d) < 4 || d[0].text != "def" || d[1].kind != 'n' || dunder(d[1].text) || d[2].text != "(":
		return false
	}
	end := pyDepthEnd(d, 2)
	if end < 0 || end+1 < len(d) && d[end+1].text != ":" && !future { // a return annotation runs
		return false
	}
	for _, p := range pySplit(d[3:end], ",") {
		for len(p) > 0 && strings.Trim(p[0].text, "*") == "" { // *args, **kwargs, a bare *
			p = p[1:]
		}
		if len(p) == 0 || len(p) == 1 && (p[0].kind == 'n' || p[0].text == "/") {
			continue
		} else if p[0].kind != 'n' {
			return false
		}
		eq := slices.IndexFunc(p, func(k ptok) bool { return k.text == "=" })
		if p[1].text == ":" && !future || eq < 0 && p[1].text != ":" || eq >= 0 && !pyLiteral(p[eq+1:]) {
			return false // an annotation runs; a default runs
		}
	}
	return true
}

// pyLiteral: value t is strings (no f- or t-string, Astra #275 r2 H6a),
// numbers, True, False, None, or tuples, lists and dicts of them: no name,
// attribute, subscript, call or operator (Astra #275 H6).
func pyLiteral(t []ptok) bool {
	value := false // the token before closes a value: ( or [ after it is a call or a subscript
	for _, k := range t {
		switch {
		case k.kind == 's':
			if q := strings.IndexAny(k.text, `"'`); q < 0 || strings.ContainsAny(k.text[:q], "fFtT") {
				return false
			}
			value = true
		case k.kind == '1' || slices.Contains([]string{"True", "False", "None"}, k.text):
			value = true
		case k.text == "(" || k.text == "[":
			if value {
				return false
			}
		case slices.Contains([]string{")", "]", "}"}, k.text):
			value = true
		case k.text == "{" || k.text == "," || k.text == ":":
			value = false
		default:
			return false
		}
	}
	return len(t) > 0
}

// resolve follows the dotted name d through the base modules' own
// imports (Opus #270 H2): pkg.PSLOW2, pkg/__init__.py holding from .conf
// import REAL as PSLOW2, is pkg.conf.REAL. Unread: the module's path.
func (pb *pyBase) resolve(d string) (string, string, error) {
	seen := map[string]bool{}
	for !seen[d] && len(seen) < 16 { // a cycle or over 16 steps: unread (the security review of 9e748fc)
		seen[d] = true
		parts, next := strings.Split(d, "."), d
		i, why, err := pb.prefix(parts, len(parts)-1) // the module prefix Python imports (Astra #272 r2 H1)
		if err != nil || why != "" {
			return d, why, err
		}
		for ; i >= 1; i = 0 {
			if q := pb.mods[strings.Join(parts[:i], ".")]; q != "" {
				f, err := pb.file(q)
				if err != nil || f.unread {
					return d, cmp.Or(q, ""), err
				}
				if to := f.from[parts[i]]; to != "" {
					if !f.named[parts[i]] && strings.HasPrefix(to, parts[i]+".") {
						to = parts[i] // import a.b binds a
					}
					next = strings.Join(append([]string{to}, parts[i+1:]...), ".")
				} else if !f.locals[parts[i]] {
					unknown := "" // through a star import that binds it (Mythos #270 H3); else an unknown, unread or run-time one is unread (r2 M3)
					for _, s := range f.stars {
						if pb.mods[s] == "" {
							unknown = cmp.Or(unknown, s)
						} else if fs, err := pb.file(pb.mods[s]); err != nil {
							return d, s, err
						} else if !fs.unread && (fs.locals[parts[i]] || fs.from[parts[i]] != "" || len(fs.stars) > 0) {
							next = strings.Join(append([]string{s}, parts[i:]...), ".")
							break
						} else if fs.unread || fs.dynamic {
							unknown = cmp.Or(unknown, s)
						}
					}
					if next == d && (unknown != "" || f.dynamic) { // a run-time module itself too: a __getattr__ (r2 L1)
						return d, cmp.Or(unknown, q) + ", unknown, unread or binding names at run time", nil
					}
				}
				break
			}
		}
		if next == d {
			return d, "", nil
		}
		d = next
	}
	return d, "a cycle or over 16 steps: " + d, nil
}

// pyShadow is why change c may shadow a module the tests import, or "":
// a module added under a sys.path entry whose dotted name (pyModNames) is
// a base module's, or one a base judged file imports or a prefix of it (a
// third-party one too); in a judged file or under a test basedir, a base
// module's in another directory by its last name too (main's #266 rule, a
// floor: Opus #270 H1); or main's __init__.py rule. Unread blocks.
func (r *run) pyShadow(c Change, judged bool, pb *pyBase) (string, error) {
	switch { // main's #266 rule, a floor beside the basedir rule (the security review of 9e748fc)
	case path.Base(c.Src) == "__init__.py" && (c.Status == "D" || c.Src != c.Dst) && r.pyTestDir(c.Src) || judged && path.Base(c.Dst) == "__init__.py" && c.Status != "M" && c.Status != "D":
		return "an __init__.py added, removed or renamed in a test directory (the basedir moves)", nil
	case c.Status == "M" || c.Status == "D" || path.Ext(c.Dst) != ".py" || pyTested(c.Dst):
		return "", nil
	}
	if err := pb.load(); err != nil {
		return "", err
	}
	names := pyModNames(c.Dst, pb.shadowEntries, judged)
	for _, n := range names {
		if q := pb.shadowMods[n]; q != "" {
			return "an added module shadows the base module " + q, nil
		}
	}
	split := func(p string) (string, string) {
		m := strings.TrimSuffix(strings.TrimSuffix(p, ".py"), "/__init__")
		return path.Dir(m), path.Base(m)
	}
	d, m := split(c.Dst)
	floor := judged || slices.Contains(pb.basedirs, d) // directly under a test basedir too (Mythos #270 r2 M2); main's floor (Opus #272 C1)
	for _, q := range pb.paths {
		if err := r.ctx.Err(); err != nil { // Mythos #270 r2 L2
			return "", err
		} else if floor && path.Ext(q) == ".py" {
			if qd, qm := split(q); qm == m && qd != d {
				return "an added module shadows the base module " + q, nil
			}
		}
	}
	for _, bt := range slices.Concat(pb.tests, pb.holders) { // a holder's imports too (Astra #275 H2)
		if err := r.ctx.Err(); err != nil {
			return "", err
		} else if bt.unread {
			return "an added module may shadow one " + bt.path + " imports (unread)", nil
		}
		for _, d := range bt.mods {
			for ; d != ""; d = d[:max(strings.LastIndex(d, "."), 0)] {
				if slices.Contains(names, d) {
					return "an added module shadows a module " + bt.path + " imports", nil
				}
			}
		}
	}
	return "", nil
}

// pyReach is why changed module c may change a base judged file's
// condition, or "" (Mythos #267 H4, r2 H3, Opus #270 H2). A condition's
// plain name n reaches c's attribute y: n bound by a from-import whose
// target, resolved through the base modules' imports, ends in y, c
// binding y differently (by name: a re-export); or n's dotted chain, from
// its import (or a star import of c) and so resolved, passing through c's
// dotted name to y, and c binding y differently, by def, class or global
// (pyMoving), or the condition reading past y or c whole. A star import
// added or removed in c rebinds every name (Opus #270 M2).
func pyReach(c Change, b, a *pyFile, pb *pyBase) (string, error) {
	suffixes := slices.Concat(pySuffixes(c.Src), pySuffixes(c.Dst))
	stars := func(f *pyFile) []string {
		return slices.DeleteFunc(strings.Split(f.imports, "\n"), func(s string) bool { return !strings.HasSuffix(s, "*]") })
	}
	rebound := func(y string) bool { return a.binds[y] != b.binds[y] || !slices.Equal(stars(a), stars(b)) }
	why := func(f *pyBaseFile, y string) string {
		return "a skipif or xfail condition in " + f.path + " reads " + y + ", which this change rebinds"
	}
	hit := func(d string, via bool) string { // c's attribute chain d reaches, or ""
		for e, i := strings.Split(d, "."), 1; i <= len(e); i++ { // i == len(e): c read whole, getattr(c, …) (the security review)
			if y := e[min(i, len(e)-1)]; slices.Contains(suffixes, strings.Join(e[:i], ".")) &&
				(i == len(e) && via || i+1 < len(e) || i < len(e) && (rebound(y) || pyMoved(y, b, a))) {
				return y
			}
		}
		return ""
	}
	for _, f := range slices.Concat(pb.tests, pb.holders) {
		if f.unread {
			return "a base test file is unread (" + f.path + "): its conditions are not verified", nil
		}
		for _, line := range f.conds {
			if pb.r.work += len(line); pb.r.work > maxWork { // Mythos #270 M5
				return "unverifiable: the base conditions' analysis is over the guard's work bound", nil
			} else if err := pb.r.ctx.Err(); err != nil {
				return "", err
			}
			for j, n := range line {
				if n.kind != 'n' || j > 0 && line[j-1].text == "." || slices.Contains(pyKeywords, n.text) || slices.Contains([]string{"True", "False", "None"}, n.text) ||
					j+1 < len(line) && line[j+1].text == "=" && (j+2 >= len(line) || line[j+2].text != "=") { // not a read (Mythos #270 r2 M3)
					continue
				}
				src, e := f.from[n.text], []string{n.text}
				for k := j; k+2 < len(line) && line[k+1].text == "." && line[k+2].kind == 'n'; k += 2 {
					e = append(e, line[k+2].text)
				}
				if f.named[n.text] {
					if ds, unread, err := pb.resolve(src); err != nil || unread != "" {
						return "a module a condition reads is unread (" + unread + ")", err
					} else if y := ds[strings.LastIndex(ds, ".")+1:]; rebound(y) {
						return why(f, y), nil
					}
				}
				chains, via := [][]string{e}, src != "" // n's chain: from its import, or through each star import (Mythos #270 H3)
				if src != "" && !strings.HasPrefix(src, n.text+".") {
					chains = [][]string{slices.Concat(strings.Split(src, "."), e[1:])}
				} else if src == "" && !f.locals[n.text] && len(f.stars) > 0 {
					chains, via = nil, true
					for _, s := range f.stars {
						chains = append(chains, slices.Concat(strings.Split(s, "."), e))
					}
				}
				for _, ch := range chains {
					d, unread, err := pb.resolve(strings.Join(ch, "."))
					if err != nil || unread != "" {
						return "a module a condition reads is unread (" + unread + ")", err
					} else if y := hit(d, via); y != "" {
						return why(f, y), nil
					} else if y, unread, err := pb.across(d, hit, func(y string) bool { return a.binds[y] != b.binds[y] || pyMoved(y, b, a) }); err != nil || unread != "" {
						return "a module a condition reads is unread (" + unread + ")", err
					} else if y != "" {
						return why(f, y), nil
					}
				}
			}
		}
	}
	return "", nil
}

// pyMoved: y, or a same-file name its statements read, transitively, is
// bound differently, or may change under its line (Mythos #270 H1:
// HAS_GPU = _detect(), _detect's body changed).
func pyMoved(y string, b, a *pyFile) bool {
	seen := map[string]bool{}
	for queue := []string{y}; len(queue) > 0; queue = queue[1:] {
		if n := queue[0]; !seen[n] {
			if seen[n] = true; a.binds[n] != b.binds[n] || !slices.Equal(a.bodies[n], b.bodies[n]) || pyMoving(a.binds[n]) { // a def's body as it reads (r2 M5)
				return true
			}
			queue = append(queue, a.reads[n]...)
		}
	}
	return false
}

// pyMoving: a binding that may change under its line: a class's body
// (Mythos #267 H1), or a function's global statement (r2 M2); a def's body
// is compared (#270 r2 M5).
func pyMoving(binds string) bool {
	return strings.Contains(binds, "[class ") || strings.Contains(binds, "[global ")
}
