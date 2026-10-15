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
// test files.
type pyBase struct {
	r       *run
	entries []string
	read    bool
	err     error
	mods    map[string]string // a base module's dotted name, to its path
	tests   []pyBaseTest
}

// pyBaseTest is what a base test module or conftest imports, and its
// lines holding a condition.
type pyBaseTest struct {
	path   string
	unread bool
	mods   []string          // the dotted names it imports (a from-import's as module.name; no relative dots)
	from   map[string]string // a name it binds by import, to that dotted name (import a.b: a to a.b)
	named  map[string]bool   // the names a from-import binds
	stars  []string          // the modules it star-imports
	conds  [][]ptok          // its lines pySkipString flags
}

// pyEntries are the sys.path entries the tests import from (Mythos #267
// r2 M1): the root, each base pythonpath entry, and each test file's
// basedir, in the base and in the candidate (the security review).
func (r *run) pyEntries(paths []string) []string {
	entries := append([]string{"."}, paths...)
	for _, tree := range []map[string]string{r.files, r.cand} {
		for q := range tree {
			if pyTested(q) {
				entries = append(entries, pyBasedir(tree, q))
			}
		}
	}
	slices.Sort(entries)
	return slices.Compact(entries)
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

// load reads the base's modules' names and its test files: every import
// statement, after ; or a compound header's : too (Opus #267 M3).
func (pb *pyBase) load() error {
	if pb.read {
		return pb.err
	}
	pb.read, pb.mods = true, map[string]string{}
	for _, q := range slices.Sorted(maps.Keys(pb.r.files)) {
		for _, n := range pyModNames(q, pb.entries, false) {
			if pb.mods[n] == "" && path.Ext(q) == ".py" {
				pb.mods[n] = q
			}
		}
		if !pyTested(q) {
			continue
		}
		bt := pyBaseTest{path: q, from: map[string]string{}, named: map[string]bool{}}
		b, why, err := pb.r.file(pb.r.files[q])
		lines, why2, err2 := pyTokens(pb.r.ctx, b)
		if pb.err = cmp.Or(err, err2); pb.err != nil {
			return pb.err
		}
		pure, aliases := pyPure(lines)
		rel, in := pyRel(path.Dir(q), pyBasedir(pb.r.files, q))
		pkg := strings.Split(rel, "/") // q's package (a relative import's base)
		if !in {
			pkg = nil
		}
		for _, l := range lines {
			line := slices.Concat(l.toks, l.inner)
			cond := pySkipString(line, len(l.toks), pure, aliases)
			for i, k := range line {
				if s := pyStr(k); cond && s != "" { // a string condition's code too (the security review)
					sl, _, err := pyTokens(pb.r.ctx, []byte(s))
					if pb.err = err; err != nil {
						return err
					}
					for _, sk := range sl {
						line = append(line, sk.toks...)
					}
				} else if i+2 < len(line) && slices.Contains([]string{"__import__", "import_module", "importorskip"}, k.text) && line[i+1].text == "(" && pyStr(line[i+2]) != "" {
					bt.mods = append(bt.mods, strings.TrimLeft(pyStr(line[i+2]), ".")) // a constant one (the security review)
				}
			}
			if cond {
				bt.conds = append(bt.conds, line)
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
						d, bt.named[as] = strings.Trim(mod+"."+d, "."), true
					} else if !named(part, "as") {
						as = part[0].text // import a.b binds a, and keeps a.b (Mythos #267 r2 H2)
					}
					if star, ok := strings.CutSuffix(d, ".*"); ok {
						bt.stars = append(bt.stars, star)
					}
					bt.mods, bt.from[as] = append(bt.mods, d), d
				}
			}
		}
		bt.unread = why+why2 != ""
		pb.tests = append(pb.tests, bt)
	}
	return nil
}

// pyShadow is why change c may shadow a module the tests import, or "":
// an __init__.py removed or renamed, or added in a test directory (a
// basedir or a package moves); or a module added under a sys.path entry
// whose dotted name (pyModNames) is a base module's, or one a base test
// file imports or a prefix of it (a third-party one too). Unread blocks.
func (r *run) pyShadow(c Change, judged bool, pb *pyBase) (string, error) {
	switch {
	case path.Base(c.Src) == "__init__.py" && (c.Status == "D" || c.Src != c.Dst) || judged && path.Base(c.Dst) == "__init__.py" && c.Status != "M" && c.Status != "D":
		return "an __init__.py added, removed or renamed (a basedir or a package moves)", nil
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

// pyReach is why changed module c may change a base test's condition, or
// "" (Mythos #267 H4, r2 H3). A condition's plain name n reaches c's
// attribute y: n bound by a from-import and y = n's original name, c
// binding y differently (by name: a re-export); or n's dotted chain, from
// its import (or a star import of c), passing through c's dotted name to
// y, and c binding y differently, by def, class or global (pyMoving), or
// the condition reading past y.
func pyReach(c Change, b, a *pyFile, all []pyBaseTest) string {
	suffixes := slices.Concat(pySuffixes(c.Src), pySuffixes(c.Dst))
	why := func(bt pyBaseTest, y string) string {
		return "a skipif or xfail condition in " + bt.path + " reads " + y + ", which this change rebinds"
	}
	for _, bt := range all {
		if bt.unread {
			return "a base test file is unread (" + bt.path + "): its conditions are not verified"
		}
		for _, line := range bt.conds {
			for j, n := range line {
				if n.kind != 'n' || j > 0 && line[j-1].text == "." {
					continue
				}
				src, e := bt.from[n.text], []string{n.text}
				for k := j; k+2 < len(line) && line[k+1].text == "." && line[k+2].kind == 'n'; k += 2 {
					e = append(e, line[k+2].text)
				}
				if y := src[strings.LastIndex(src, ".")+1:]; bt.named[n.text] && a.binds[y] != b.binds[y] {
					return why(bt, y)
				}
				star := slices.IndexFunc(bt.stars, func(s string) bool { return slices.Contains(suffixes, s) })
				if src == "" && star >= 0 {
					e = slices.Concat(strings.Split(bt.stars[star], "."), e)
				} else if src != "" && !strings.HasPrefix(src, n.text+".") {
					e = slices.Concat(strings.Split(src, "."), e[1:])
				}
				for i := 1; i <= len(e); i++ { // i == len(e): c read whole, getattr(c, …) (the security review)
					if y := e[min(i, len(e)-1)]; slices.Contains(suffixes, strings.Join(e[:i], ".")) &&
						(i == len(e) && (src != "" || star >= 0) || i+1 < len(e) || i < len(e) && (a.binds[y] != b.binds[y] || pyMoving(a.binds[y]))) {
						return why(bt, y)
					}
				}
			}
		}
	}
	return ""
}

// pyMoving: a binding that may change under its line: a def's or class's
// body (Mythos #267 H1), or a function's global statement (r2 M2).
func pyMoving(binds string) bool {
	return slices.ContainsFunc([]string{"[def ", "[class ", "[async ", "[global "}, func(s string) bool { return strings.Contains(binds, s) })
}
