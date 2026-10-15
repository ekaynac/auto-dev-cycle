// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"cmp"
	"maps"
	"path"
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
	r        *run
	entries  []string
	basedirs []string // the test files' basedirs below the root, in either tree
	paths    []string // the base's paths, sorted
	read     bool
	err      error
	mods     map[string]string      // a base module's dotted name, to its path
	files    map[string]*pyBaseFile // the base files read, by path
	tests    []*pyBaseFile          // the judged ones (Opus #270 H2, M4)
}

// pyBaseFile is what a base Python file imports, and its conditions.
type pyBaseFile struct {
	path   string
	unread bool
	mods   []string          // the dotted names it imports (a from-import's as module.name)
	from   map[string]string // a name it binds by import, to that dotted name (import a.b: a to a.b)
	named  map[string]bool   // the names a from-import binds
	locals map[string]bool   // the names its module-level statements bind
	stars  []string          // the modules it star-imports
	conds  [][]ptok          // its condition lines, and the bindings they read (Opus #270 H2)
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
	pb.read, pb.mods, pb.files, pb.paths = true, map[string]string{}, map[string]*pyBaseFile{}, slices.Sorted(maps.Keys(pb.r.files))
	for _, q := range pb.paths {
		if pb.err = pb.r.ctx.Err(); pb.err != nil { // Mythos #270 M5
			return pb.err
		} else if path.Ext(q) != ".py" {
			continue
		}
		for _, n := range pyModNames(q, pb.entries, false) {
			if pb.mods[n] == "" {
				pb.mods[n] = q
			}
		}
		if pyTested(q) || pb.r.pyTestDir(q) {
			f, err := pb.file(q)
			if pb.err = err; err != nil {
				return err
			}
			pb.tests = append(pb.tests, f)
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
	b, why, err := pb.r.file(pb.r.files[q])
	lines, why2, err2 := pyTokens(pb.r.ctx, b)
	if err = cmp.Or(err, err2, pb.r.ctx.Err()); err != nil {
		return nil, err
	}
	f.unread = why+why2 != ""
	pure, aliases := pyPure(pb.r.ctx, lines)
	rel, in := pyRel(path.Dir(q), pyBasedir(pb.r.files, q))
	pkg := strings.Split(rel, "/") // q's package (a relative import's base)
	if !in {
		pkg = nil
	}
	var units [][]ptok            // module-level statements, def and class bodies
	binders := map[string][]int{} // a name, to the units binding it
	owner, indent, deco, def, bodies := -1, 0, []ptok(nil), "", []int(nil)
	for _, l := range lines {
		line := slices.Concat(l.toks, l.inner)
		cond := pySkipString(line, len(l.toks), pure, aliases)
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
			if cond { // its unit is a condition; a def's callers pass conditions (Mythos #270 H2)
				if bodies = append(bodies, owner); def != "" {
					aliases[def] = true
				}
			}
		case l.toks[0].text == "@":
			owner, deco = -1, append(deco, line...)
		default:
			owner = -1
			for _, st := range pySplit(line, ";") {
				if len(st) == 0 {
					continue
				}
				if bound, imported := pyBinds(st); !imported {
					for _, n := range bound {
						binders[n], f.locals[n] = append(binders[n], len(units)), true
					}
					if d := st[b2i(st[0].text == "async"):]; d[0].text == "def" || d[0].text == "class" {
						owner, indent, def = len(units), l.indent, ""
						if d[0].text == "def" {
							def = d[1].text
						}
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
	return f, nil
}

// resolve follows the dotted name d through the base modules' own
// imports (Opus #270 H2): pkg.PSLOW2, pkg/__init__.py holding from .conf
// import REAL as PSLOW2, is pkg.conf.REAL. Unread: the module's path.
func (pb *pyBase) resolve(d string) (string, string, error) {
	seen := map[string]bool{}
	for !seen[d] && len(seen) < 16 { // a cycle or over 16 steps: unread (the security review of 9e748fc)
		seen[d] = true
		parts, next := strings.Split(d, "."), d
		for i := len(parts) - 1; i >= 1; i-- { // the longest module prefix
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
					for _, s := range f.stars { // through a star import that binds it (Mythos #270 H3); an unknown one is unread
						if pb.mods[s] == "" {
							return d, "the star import " + s, nil
						} else if fs, err := pb.file(pb.mods[s]); err != nil || fs.unread {
							return d, "the star import " + s, err
						} else if fs.locals[parts[i]] || fs.from[parts[i]] != "" || len(fs.stars) > 0 {
							next = strings.Join(append([]string{s}, parts[i:]...), ".")
							break
						}
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
	names := pyModNames(c.Dst, pb.entries, judged)
	for _, n := range names {
		if q := pb.mods[n]; q != "" {
			return "an added module shadows the base module " + q, nil
		}
	}
	split := func(p string) (string, string) {
		m := strings.TrimSuffix(strings.TrimSuffix(p, ".py"), "/__init__")
		return path.Dir(m), path.Base(m)
	}
	floor := judged || slices.ContainsFunc(pb.basedirs, func(e string) bool { _, ok := pyRel(c.Dst, e); return ok }) // under a test basedir too
	for _, q := range pb.paths {
		if d, m := split(c.Dst); floor && path.Ext(q) == ".py" {
			if qd, qm := split(q); qm == m && qd != d {
				return "an added module shadows the base module " + q, nil
			}
		}
	}
	for _, bt := range pb.tests {
		if bt.unread {
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
	for _, f := range pb.tests {
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
				if n.kind != 'n' || j > 0 && line[j-1].text == "." {
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
					}
					for e, i := strings.Split(d, "."), 1; i <= len(e); i++ { // i == len(e): c read whole, getattr(c, …) (the security review)
						if y := e[min(i, len(e)-1)]; slices.Contains(suffixes, strings.Join(e[:i], ".")) &&
							(i == len(e) && via || i+1 < len(e) || i < len(e) && (rebound(y) || pyMoved(y, b, a))) {
							return why(f, y), nil
						}
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
			if seen[n] = true; a.binds[n] != b.binds[n] || pyMoving(a.binds[n]) {
				return true
			}
			queue = append(queue, a.reads[n]...)
		}
	}
	return false
}

// pyMoving: a binding that may change under its line: a def's or class's
// body (Mythos #267 H1), or a function's global statement (r2 M2).
func pyMoving(binds string) bool {
	return slices.ContainsFunc([]string{"[def ", "[class ", "[async ", "[global "}, func(s string) bool { return strings.Contains(binds, s) })
}
