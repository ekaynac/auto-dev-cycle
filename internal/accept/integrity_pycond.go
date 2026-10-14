// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
)

// P14c-2a-cond (Opus and Mythos #266 H2, Mythos #266 r2): a skipif or
// xfail condition pytest would run as code, and modules shadowing the code
// under test.
var pyImportLine = regexp.MustCompile(`(?m)^[ \t]*(?:from[ \t]+([\w.]+)[ \t]+import|import[ \t]+([\w., \t]+))`)

// pySkipString: a skipif or xfail mark (pytest.mark.NAME(…) or
// .with_args(…), not pytest.xfail()) whose positional arguments or
// condition= may be a string: each must surely be a bool.
func pySkipString(t []ptok) bool {
	for i := 0; i+1 < len(t); i++ {
		open := i + 1
		if i+3 < len(t) && t[i+1].text == "." && t[i+2].text == "with_args" {
			open = i + 3
		}
		if t[i].text != "skipif" && t[i].text != "xfail" || t[open].text != "(" || i > 1 && t[i-1].text == "." && t[i-2].text == "pytest" {
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
			cmp := strings.Contains("<>=!", k.text) && k.kind == 'o' && !(k.text == "=" && i > 0 && op[i-1].text == ":")
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
// pythonpath directory, named as a base module importable from the root,
// a directory holding a test file or a pythonpath one; or named as a
// module a base test file imports (a third-party one too). Unread blocks.
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
		return d == "." || slices.Contains(paths, d) || slices.ContainsFunc(r.baseDirs[d], pyTested)
	}
	for _, q := range slices.Sorted(maps.Keys(r.files)) {
		if path.Dir(q) != dir && (path.Base(q) == m+".py" && onPath(path.Dir(q)) || path.Base(q) == "__init__.py" && path.Base(path.Dir(q)) == m && onPath(path.Dir(path.Dir(q)))) {
			return "a module added under a test directory shadows the base module " + q, nil
		} else if !pyTested(q) {
			continue
		}
		b, why, err := r.file(r.files[q])
		if err != nil || why != "" {
			return "a module added under a test directory may shadow one " + q + " imports (unread)", err
		}
		for _, g := range pyImportLine.FindAllStringSubmatch(string(b), -1) {
			for _, n := range strings.Split(g[1]+g[2], ",") {
				if strings.Split(strings.Fields(n + " \x00")[0], ".")[0] == m {
					return "a module added under a test directory shadows a module " + q + " imports", nil
				}
			}
		}
	}
	return "", nil
}
