// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"cmp"
	"maps"
	"path"
	"slices"
	"strings"
)

// P14c-2a-cond (Opus and Mythos #266 H2, Mythos #266 r2): a skipif or
// xfail condition pytest would run as code, and modules shadowing the code
// under test.

// pySkipString: a skipif or xfail mark (pytest.mark.NAME(…) or
// .with_args(…), not pytest.xfail()) whose positional arguments or
// condition= may be a string: each must surely be a bool.
func pySkipString(t []ptok, pure bool) bool {
	for i := 0; i+1 < len(t); i++ {
		open := i + 1
		if i+3 < len(t) && t[i+1].text == "." && t[i+2].text == "with_args" {
			open = i + 3
		}
		if t[i].text != "skipif" && t[i].text != "xfail" || t[open].text != "(" || pure && i > 1 && t[i-1].text == "." && t[i-2].text == "pytest" {
			continue
		}
		end, depth := open, 0
		for ; end < len(t); end++ {
			if depth += strings.Count("([{", t[end].text) - strings.Count(")]}", t[end].text); depth == 0 {
				break
			}
		}
		for _, a := range pySplit(t[open+1:min(end, len(t))], ",") {
			if kw := len(a) > 1 && a[0].kind == 'n' && a[1].text == "=" && (len(a) < 3 || a[2].text != "="); kw && a[0].text == "condition" && !pyBool(a[2:]) || !kw && len(a) > 0 && !pyBool(a) {
				return true
			}
		}
	}
	return false
}

// pyBool: True, False, or and/or of operands each holding a comparison
// (not a walrus's =) or led by not, with no if or else, at level 0.
func pyBool(t []ptok) bool {
	for _, op := range pySplit(t, "and", "or") {
		depth, sure := 0, len(op) == 1 && (op[0].text == "True" || op[0].text == "False") || len(op) > 1 && op[0].text == "not"
		for i, k := range op {
			if depth -= strings.Count(")]}", k.text); depth == 0 && (k.text == "if" || k.text == "else") {
				return false
			}
			cmp := strings.Contains("<>=!", k.text) && k.kind == 'o' && !(k.text == "=" && i > 0 && op[i-1].text == ":") &&
				!(strings.Contains("<>", k.text) && (i > 0 && op[i-1].text == k.text || i+1 < len(op) && op[i+1].text == k.text)) // not << or >> (Opus #267 M1)
			sure, depth = sure || depth == 0 && (cmp || k.text == "is" || k.text == "in"), depth+strings.Count("([{", k.text)
		}
		if !sure {
			return false
		}
	}
	return len(t) > 0
}

// pyShadow is why change c may shadow a module the tests import, or "":
// an __init__.py added, removed or renamed in a test directory (the
// basedir moves); a module added under a test directory, or in a base
// pythonpath directory, whose path ends with a base module's (one in the
// root included), or named as a base module in a directory holding a test
// file or a pythonpath one; or a module a base test file imports, by any
// dotted suffix (a third-party one too: Opus #267 H2). Unread blocks.
func (r *run) pyShadow(c Change, judged bool, paths []string) (string, error) {
	dir, m := path.Dir(c.Dst), strings.TrimSuffix(path.Base(c.Dst), ".py")
	switch {
	case path.Base(c.Src) == "__init__.py" && (c.Status == "D" || c.Src != c.Dst) && r.pyTestDir(c.Src) || judged && m == "__init__" && c.Status != "M" && c.Status != "D":
		return "an __init__.py added, removed or renamed in a test directory (the basedir moves)", nil
	case c.Status == "M" || c.Status == "D" || path.Ext(c.Dst) != ".py" || pyTested(c.Dst):
		return "", nil
	case m == "__init__":
		dir, m = path.Dir(dir), path.Base(dir)
	}
	if !judged && !slices.Contains(paths, dir) {
		return "", nil
	}
	onPath := func(d string) bool {
		return slices.Contains(paths, d) || slices.ContainsFunc(r.baseDirs[d], pyTested)
	}
	var suffixes []string // the dotted names the added module may answer to
	for s, rest := "", strings.Split(path.Join(dir, m), "/"); len(rest) > 0; rest = rest[:len(rest)-1] {
		s = strings.TrimSuffix(rest[len(rest)-1]+"."+s, ".")
		suffixes = append(suffixes, s)
	}
	for _, q := range slices.Sorted(maps.Keys(r.files)) {
		if strings.HasSuffix(c.Dst, "/"+q) || path.Dir(q) != dir && (path.Base(q) == m+".py" && onPath(path.Dir(q)) ||
			path.Base(q) == "__init__.py" && path.Base(path.Dir(q)) == m && onPath(path.Dir(path.Dir(q)))) {
			return "a module added under a test directory shadows the base module " + q, nil
		} else if !pyTested(q) {
			continue
		}
		b, why, err := r.file(r.files[q])
		lines, why2, err2 := pyTokens(r.ctx, b)
		if err = cmp.Or(err, err2); err != nil || why+why2 != "" {
			return "a module added under a test directory may shadow one " + q + " imports (unread)", err
		}
		for _, l := range lines { // every import statement, after ; or a compound header's : too (Opus #267 M3)
			for i, k := range l.toks {
				if i > 0 && l.toks[i-1].text != ";" && l.toks[i-1].text != ":" || k.text != "import" && k.text != "from" {
					continue
				}
				st := pySplit(l.toks[i:], ";")[0]
				names := pySplit(st[1:], ",")
				if k.text == "from" {
					j := slices.IndexFunc(st, func(k ptok) bool { return k.text == "import" })
					names = nil
					for _, part := range pyImportParts(st) {
						names = append(names, append(slices.Clone(st[1:max(j, 1)]), ptok{'o', "."}, part[0]))
					}
				}
				for _, n := range names {
					for d := pyJoin(pySplit(n, "as")[0]); d != "" && d != "."; d = d[:max(strings.LastIndex(d, "."), 0)] {
						if slices.Contains(suffixes, d) {
							return "a module added under a test directory shadows a module " + q + " imports", nil
						}
					}
				}
			}
		}
	}
	return "", nil
}

// pyPure: the file binds pytest only by import pytest: no parameter,
// target, attribute or alias named pytest (Opus #267 M2).
func pyPure(lines []pline) bool {
	for _, l := range lines {
		t, d := slices.Concat(l.toks, l.inner), l.toks[b2i(l.toks[0].text == "async"):]
		for i, k := range t {
			if k.text == "pytest" && (i > 0 && slices.Contains([]string{".", "as", "for", "def", "class", "lambda"}, t[i-1].text) ||
				i+1 < len(t) && (t[i+1].text == ":" || t[i+1].text == "=" && (i+2 >= len(t) || t[i+2].text != "=")) ||
				d[0].text == "def" && i < len(pySplit(d, ":")[0])) {
				return false
			}
		}
	}
	return true
}

// pyNames are the plain names t reads (not attributes, keywords or
// keyword-argument names).
func pyNames(t []ptok) (names []string) {
	for i, k := range t {
		if k.kind == 'n' && (i == 0 || t[i-1].text != ".") && !slices.Contains(pyKeywords, k.text) && (i+1 >= len(t) || t[i+1].text != "=") {
			names = append(names, k.text)
		}
	}
	return names
}
