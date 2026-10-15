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

// pyBaseTest is what a base test module or conftest imports, and its
// lines holding a condition: read once per run (Mythos #267 L3).
type pyBaseTest struct {
	path   string
	unread bool
	mods   []string          // the dotted names it imports (a from-import's as module.name)
	from   map[string]string // a name it binds by import, to that dotted name
	conds  [][]ptok          // its lines pySkipString flags
}

// pyBaseTests reads the base's test files: every import statement, after
// ; or a compound header's : too (Opus #267 M3).
func (r *run) pyBaseTests() (tests []pyBaseTest, err error) {
	for _, q := range slices.Sorted(maps.Keys(r.files)) {
		if !pyTested(q) {
			continue
		}
		bt := pyBaseTest{path: q, from: map[string]string{}}
		b, why, err := r.file(r.files[q])
		lines, why2, err2 := pyTokens(r.ctx, b)
		if err = cmp.Or(err, err2); err != nil {
			return nil, err
		}
		pure, aliases := pyPure(lines)
		for _, l := range lines {
			if line := slices.Concat(l.toks, l.inner); pySkipString(line, len(l.toks), pure, aliases) {
				bt.conds = append(bt.conds, line)
			}
			for i, k := range l.toks {
				if i > 0 && l.toks[i-1].text != ";" && l.toks[i-1].text != ":" || k.text != "import" && k.text != "from" {
					continue
				}
				st := pySplit(l.toks[i:], ";")[0]
				for _, part := range pyImportParts(st) {
					d, as := pyJoin(pySplit(part, "as")[0]), part[len(part)-1].text
					if k.text == "from" {
						d = pyJoin(st[1:max(slices.IndexFunc(st, func(k ptok) bool { return k.text == "import" }), 1)]) + "." + d
					} else if !named(part, "as") {
						as, d = part[0].text, part[0].text
					}
					bt.mods, bt.from[as] = append(bt.mods, d), d
				}
			}
		}
		bt.unread = why+why2 != ""
		tests = append(tests, bt)
	}
	return tests, nil
}

// pySuffixes are the dotted names the module at p (dir/m) may answer to.
func pySuffixes(p string) (s []string) {
	for d, rest := "", strings.Split(strings.TrimSuffix(strings.TrimSuffix(p, ".py"), "/__init__"), "/"); len(rest) > 0; rest = rest[:len(rest)-1] {
		d = strings.TrimSuffix(rest[len(rest)-1]+"."+d, ".")
		s = append(s, d)
	}
	return s
}

// pyShadow is why change c may shadow a module the tests import, or "":
// an __init__.py added, removed or renamed in a test directory (the
// basedir moves); a module added under a test directory, the root, or a
// base pythonpath directory or its subdirectories, whose module path is a
// base module's or ends with one, or named as a base module in a directory
// holding a test file or a pythonpath one; or a module a base test file
// imports, by any dotted suffix (a third-party one too). Unread blocks.
func (r *run) pyShadow(c Change, judged bool, paths []string, tests func() ([]pyBaseTest, error)) (string, error) {
	dir, m := path.Dir(c.Dst), strings.TrimSuffix(path.Base(c.Dst), ".py")
	switch {
	case path.Base(c.Src) == "__init__.py" && (c.Status == "D" || c.Src != c.Dst) && r.pyTestDir(c.Src) || judged && m == "__init__" && c.Status != "M" && c.Status != "D":
		return "an __init__.py added, removed or renamed in a test directory (the basedir moves)", nil
	case c.Status == "M" || c.Status == "D" || path.Ext(c.Dst) != ".py" || pyTested(c.Dst):
		return "", nil
	case m == "__init__":
		dir, m = path.Dir(dir), path.Base(dir)
	}
	in := func(d string) bool {
		return slices.ContainsFunc(paths, func(p string) bool { return d == p || strings.HasPrefix(d, p+"/") })
	}
	if !judged && dir != "." && !in(dir) { // the root is on sys.path (Mythos #267 H2), a pythonpath's tree too (M1)
		return "", nil
	}
	mp, suffixes := path.Join(dir, m), pySuffixes(c.Dst)
	for _, q := range slices.Sorted(maps.Keys(r.files)) {
		if qm := strings.TrimSuffix(strings.TrimSuffix(q, "/__init__.py"), ".py"); path.Ext(q) == ".py" && (qm == mp || strings.HasSuffix(mp, "/"+qm) ||
			path.Base(qm) == m && path.Dir(qm) != dir && (slices.Contains(paths, path.Dir(qm)) || slices.ContainsFunc(r.baseDirs[path.Dir(qm)], pyTested))) {
			return "a module added under a test directory shadows the base module " + q, nil
		}
	}
	all, err := tests()
	for _, bt := range all {
		if bt.unread {
			return "a module added under a test directory may shadow one " + bt.path + " imports (unread)", err
		}
		for _, d := range bt.mods {
			for ; d != "" && d != "."; d = d[:max(strings.LastIndex(d, "."), 0)] {
				if slices.Contains(suffixes, d) {
					return "a module added under a test directory shadows a module " + bt.path + " imports", nil
				}
			}
		}
	}
	return "", err
}

// pyReach is why changed module c may change a base test's condition, or
// "": a base test file's condition reads a name imported from c (or c.name
// through a module import) that c binds differently now (Mythos #267 H4).
func pyReach(c Change, b, a *pyFile, all []pyBaseTest) string {
	suffixes := pySuffixes(c.Dst)
	for _, bt := range all {
		if bt.unread {
			return "a base test file is unread (" + bt.path + "): its conditions are not verified"
		}
		for _, line := range bt.conds {
			for j, n := range line {
				src, attr := bt.from[n.text], ""
				if j+2 < len(line) && line[j+1].text == "." && slices.Contains(suffixes, src) {
					attr = line[j+2].text
				} else if i := strings.LastIndex(src, "."); i > 0 && slices.Contains(suffixes, src[:i]) {
					attr = src[i+1:]
				}
				if attr != "" && a.binds[attr] != b.binds[attr] {
					return "a skipif or xfail condition in " + bt.path + " reads " + attr + ", which this change rebinds"
				}
			}
		}
	}
	return ""
}
