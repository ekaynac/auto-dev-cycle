// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
)

// P14c-2a-reach (Mythos #270 M1–M4, M6, r2 L4): the sys.path entries an
// install or a test file adds, and conditions that reach a changed module
// through an application module or across modules.

var (
	pySection    = regexp.MustCompile(`(?m)^[ \t]*\[([^\]\n]*)\][ \t]*$`)
	pyInstallKey = regexp.MustCompile(`(?mi)^[ \t]*(package[_-]dir|where|build-backend)[ \t]*[:=]`)
	pyInstallAny = regexp.MustCompile(`(?mi)package[_-]dir|(^|[^\w])where["']?\s*[:=]|build-backend`)
	pyPkgDir     = regexp.MustCompile(`^\{\s*""\s*=\s*"([^"\\]+)"\s*\}$`) // package-dir = {"" = "src"}
	pySysAttrs   = strings.Fields("argv platform version_info version implementation executable exit stdout stderr stdin maxsize byteorder flags " +
		"getrecursionlimit setrecursionlimit exc_info prefix base_prefix float_info hexversion getdefaultencoding getfilesystemencoding") // sys attributes read as no sys.path change
	// pyInstallKeys: the key each setuptools section is read for (Astra #272 H1).
	pyInstallKeys = map[string]string{"options": "package_dir", "options.packages.find": "where", "tool.setuptools": "package-dir",
		"tool.setuptools.packages.find": "where", "build-system": "build-backend"}
)

// pyInstall reads the entries an install adds (Mythos #270 M1), as an
// allowlist (Opus #272 (c)), each key only in its section (Astra #272 H1):
// [options] package_dir = =DIR (any case, = or :), [options.packages.find]
// where, [tool.setuptools] package-dir = {"" = "DIR"},
// [tool.setuptools.packages.find] where, a setuptools build-backend, a .py
// under src/. Any other such key in a setuptools section or at the top, a
// [tool.poetry|hatch|pdm|flit] section, an entry outside the root, or a
// .pth anywhere (Astra #272 M2) is unread; other sections are not read.
func (r *run) pyInstall() (entries []string, unread bool, err error) {
	for _, p := range slices.Sorted(maps.Keys(r.files)) {
		switch base := path.Base(p); {
		case path.Ext(p) == ".pth":
			return nil, true, nil
		case strings.HasPrefix(p, "src/") && path.Ext(p) == ".py":
			entries = append(entries, "src")
		case base == "setup.cfg" || base == "pyproject.toml" || base == "tox.ini":
			b, why, err := r.file(r.files[p])
			if err != nil || why != "" {
				return nil, true, err
			}
			s := strings.ReplaceAll(string(b), "\r\n", "\n")
			heads := append([][]int{{0, 0, 0, 0}}, pySection.FindAllStringSubmatchIndex(s, -1)...)
			for i, h := range heads {
				end, name := len(s), strings.ToLower(strings.TrimSpace(s[h[2]:h[3]]))
				if i+1 < len(heads) {
					end = heads[i+1][0]
				}
				sec := s[h[0]:end]
				if slices.ContainsFunc([]string{"tool.poetry", "tool.hatch", "tool.pdm", "tool.flit"}, func(t string) bool { return strings.HasPrefix(name, t) }) {
					return nil, true, nil
				} else if !(name == "" || strings.HasPrefix(name, "options") || strings.HasPrefix(name, "tool.setuptools") || name == "build-system") {
					continue // an unrelated section is not read
				}
				parsed := 0
				for _, m := range pyInstallKey.FindAllStringSubmatchIndex(sec, -1) {
					v, key := strings.TrimSpace(pyKey.Split(sec[m[1]:], 2)[0]), strings.ToLower(strings.ReplaceAll(sec[m[2]:m[3]], "_", "-"))
					words := pyWords.Split(v, -1)
					switch {
					case key != strings.ReplaceAll(pyInstallKeys[name], "_", "-"):
						return nil, true, nil
					case key == "build-backend":
						if words = nil; !strings.HasPrefix(strings.Trim(v, `"'`), "setuptools") {
							return nil, true, nil
						}
					case key == "package-dir":
						if g := pyPkgDir.FindStringSubmatch(v); g != nil && name == "tool.setuptools" {
							words = g[1:]
						} else if d, ok := strings.CutPrefix(v, "="); ok && name == "options" && !strings.ContainsAny(d, "=:\n") {
							words = []string{strings.TrimSpace(d)}
						} else {
							return nil, true, nil
						}
					}
					for _, w := range words {
						if j := path.Join(path.Dir(p), w); w != "" && (path.IsAbs(w) || j == ".." || strings.HasPrefix(j, "../")) {
							return nil, true, nil
						} else if w != "" {
							entries = append(entries, j)
						}
					}
					parsed++
				}
				if parsed != len(pyInstallAny.FindAllString(sec, -1)) {
					return nil, true, nil
				}
			}
		}
	}
	return entries, false, nil
}

// pySysUse reads base file q's line t for the sys.path it changes (Mythos
// #270 M1), as an allowlist (Opus #272 (b)): sys only after import sys or
// before a dot, sys.path only in what pySysPath reads; from sys import
// path, an alias of sys, getattr(sys, …), site.addsitedir, meta_path or
// path_hooks are unread.
func pySysUse(q string, t []ptok) (entries []string, ok bool) {
	if len(t) > 1 && t[0].text == "from" && t[1].text == "sys" || len(t) > 2 && t[0].text == "import" && named(t, "as") && named(t, "sys") {
		return nil, !slices.ContainsFunc(t, func(k ptok) bool { return k.text == "path" || k.text == "*" || k.text == "meta_path" || k.text == "as" })
	}
	for i, k := range t {
		switch {
		case slices.Contains([]string{"addsitedir", "meta_path", "path_hooks", "path_importer_cache"}, k.text) || pyStr(k) == "sys":
			return nil, false // importlib.import_module('sys') too (the review of 1ceb79c)
		case k.text != "sys" || k.kind != 'n' || i > 0 && t[i-1].text == ".":
		case t[0].text == "import":
		case i+2 >= len(t) || t[i+1].text != ".":
			return nil, false
		case t[i+2].text == "path":
			es, ok := pySysPath(q, t)
			if !ok {
				return nil, false
			}
			entries = append(entries, es...)
		case !slices.Contains(pySysAttrs, t[i+2].text):
			return nil, false // sys.__dict__['path'] (the review of 1ceb79c)
		}
	}
	return entries, true
}

// pySysPath reads base file q's line t, exactly sys.path.insert(N, E) or
// sys.path.append(E), E a shape pyPathExpr evaluates (Astra #272 H2).
func pySysPath(q string, t []ptok) ([]string, bool) {
	if len(t) < 7 || pyJoin(t[:4]) != "sys.path." || t[5].text != "(" || t[len(t)-1].text != ")" {
		return nil, false
	}
	args := pySplit(t[6:len(t)-1], ",")
	if t[4].text == "insert" && len(args) == 2 && len(args[0]) == 1 && args[0][0].kind == '1' {
		args = args[1:]
	} else if t[4].text != "append" || len(args) != 1 {
		return nil, false
	}
	ps, anchored, ok := pyPathExpr(q, args[0])
	if !anchored && ok { // a literal: relative to q's directory, and to the root (the cwd pytest runs in)
		ps = append([]string{path.Join(path.Dir(q), ps[0])}, ps...)
	}
	return ps, ok && !slices.ContainsFunc(ps, func(p string) bool { return p == ".." || strings.HasPrefix(p, "../") || path.IsAbs(p) })
}

// pyPathExpr evaluates the path expression t of base file q, as a whole
// shape (Astra #272 H2): a literal; __file__ through os.path.dirname,
// abspath, realpath, os.path.join with literals, str, Path(…), .parent,
// .resolve() or .absolute(). anchored: derived from __file__. dirname,
// abspath or join of a literal (cwd-dependent), or any other shape: !ok.
func pyPathExpr(q string, t []ptok) (paths []string, anchored, ok bool) {
	n := len(t)
	switch {
	case n == 1 && pyStr(t[0]) != "":
		return []string{path.Clean(pyStr(t[0]))}, false, true
	case n == 1 && t[0].text == "__file__":
		return []string{q}, true, true
	case n > 2 && t[n-2].text == "." && t[n-1].text == "parent":
		p, a, ok := pyPathExpr(q, t[:n-2])
		return []string{path.Dir(p[0])}, a, ok && a
	case n > 4 && t[n-4].text == "." && (t[n-3].text == "resolve" || t[n-3].text == "absolute") && t[n-2].text == "(" && t[n-1].text == ")":
		p, a, ok := pyPathExpr(q, t[:n-4])
		return p, a, ok && a
	}
	open := slices.IndexFunc(t, func(k ptok) bool { return k.text == "(" })
	if open < 1 || pyDepthEnd(t, open) != n-1 {
		return nil, false, false
	}
	args := pySplit(t[open+1:n-1], ",")
	if len(args) == 0 {
		return nil, false, false
	}
	p, a, ok := pyPathExpr(q, args[0])
	switch callee := pyJoin(t[:open]); {
	case !ok || !a:
		return nil, false, false
	case callee == "os.path.dirname" && len(args) == 1:
		return []string{path.Dir(p[0])}, true, true
	case slices.Contains([]string{"os.path.abspath", "os.path.realpath", "str", "Path", "pathlib.Path"}, callee) && len(args) == 1:
		return p, true, true
	case callee == "os.path.join":
		j := p[0]
		for _, l := range args[1:] {
			if len(l) != 1 || pyStr(l[0]) == "" {
				return nil, false, false
			}
			j = path.Join(j, pyStr(l[0]))
		}
		return []string{j}, true, true
	}
	return nil, false, false
}

// pyDepthEnd is the index of the bracket closing t[open].
func pyDepthEnd(t []ptok, open int) int {
	depth := 0
	for i := open; i < len(t); i++ {
		if depth += strings.Count("([{", t[i].text) - strings.Count(")]}", t[i].text); depth == 0 {
			return i
		}
	}
	return -1
}

// across follows chain d into the base module it stops in (Mythos #270
// M3): the module-level statements and def bodies binding the name it
// reads, to a fixed point, and the dotted chains they import, resolved
// (Opus #272 H1), or a star import binds, the whole chain kept (#272 M2,
// Astra #272 H3), until one reaches the changed module (hit). A chain
// through no base module is a third-party proxy: each of its names is
// matched by name, and followed into the base modules binding it (Mythos
// #270 M4, Opus #272 M3). Each chain is queued once and charged, ctx read
// (Astra #272 M1); a module name in two roots is unread (#272 H1).
func (pb *pyBase) across(d string, hit func(string, bool) string, rebound func(string) bool) (y, unread string, err error) {
	seen, queue := map[string]bool{d: true}, []string{d}
	push := func(x string) {
		if !seen[x] {
			seen[x], queue, pb.r.work = true, append(queue, x), pb.r.work+1
		}
	}
	follow := func(x string) string { // x resolved: c's attribute it reaches, or queued
		if err != nil || unread != "" {
			return ""
		} else if x, unread, err = pb.resolve(x); err == nil && unread == "" {
			if y := hit(x, true); y != "" {
				return y
			}
			push(x)
		}
		return ""
	}
	for ; len(queue) > 0 && err == nil && unread == ""; queue = queue[1:] {
		if err = pb.r.ctx.Err(); err != nil {
			break
		} else if pb.r.work > maxWork {
			return "", "the base conditions' analysis is over the guard's work bound", nil
		}
		parts := strings.Split(queue[0], ".")
		i := len(parts) - 1
		for ; i >= 1 && pb.mods[strings.Join(parts[:i], ".")] == ""; i-- {
		}
		if i < 1 {
			for _, n := range parts[1:] { // every head: a value may be parked on pytest or os (the review of 1ceb79c)
				if rebound(n) {
					return n, "", nil
				}
				mods, why, err := pb.binding(n)
				if err != nil || why != "" {
					return "", why, err
				}
				for _, m := range mods {
					push(m + "." + n)
				}
			}
			continue
		}
		if mod := strings.Join(parts[:i], "."); pb.alts[mod] > 1 {
			return "", "the module name " + mod + " in two roots", nil
		}
		f, err := pb.file(pb.mods[strings.Join(parts[:i], ".")])
		if err != nil {
			return "", "", err
		} else if f.unread {
			return "", f.path, nil
		}
		star := func(chain string) string { // a name neither bound nor imported, through each star import
			for _, s := range f.stars {
				if pb.mods[s] == "" {
					unread = "the star import " + s
				} else if y := follow(s + "." + chain); y != "" {
					return y
				}
			}
			return ""
		}
		if len(f.binders[parts[i]]) == 0 && f.from[parts[i]] == "" {
			if y := star(strings.Join(parts[i:], ".")); y != "" {
				return y, "", nil
			}
		}
		names, taken := []string{parts[i]}, map[string]bool{parts[i]: true}
		for k := 0; k < len(names); k++ {
			for _, u := range f.binders[names[k]] {
				if pb.r.work += len(f.units[u]); pb.r.work > maxWork {
					return "", "the base conditions' analysis is over the guard's work bound", nil
				}
				unit := f.units[u]
				for j, t := range unit {
					if t.kind != 'n' || j > 0 && unit[j-1].text == "." || slices.Contains(pyKeywords, t.text) || slices.Contains([]string{"True", "False", "None"}, t.text) {
						continue
					} else if !taken[t.text] {
						taken[t.text], names = true, append(names, t.text) // its module-level binding, and its import too (Opus #272 L3)
					}
					chain := t.text
					for k := j; k+2 < len(unit) && unit[k+1].text == "." && unit[k+2].kind == 'n'; k += 2 {
						chain += "." + unit[k+2].text // u.d(): u.d (Opus #272 H1)
					}
					if to := f.from[t.text]; to != "" {
						if !f.named[t.text] && strings.HasPrefix(to, t.text+".") {
							to = t.text // import a.b binds a
						}
						if y := follow(to + strings.TrimPrefix(chain, t.text)); y != "" {
							return y, "", nil
						}
					} else if len(f.binders[t.text]) == 0 {
						if y := star(chain); y != "" {
							return y, "", nil
						}
					}
				}
			}
		}
	}
	return "", unread, err
}

// binding: the base modules whose module-level statements bind n, by a
// dotted name each (Opus #272 M3), indexed once, charged and ctx read
// (Astra #272 M1).
func (pb *pyBase) binding(n string) ([]string, string, error) {
	if pb.byName == nil {
		pb.byName = map[string][]string{}
		for _, name := range slices.Sorted(maps.Keys(pb.mods)) {
			q := pb.mods[name]
			if pb.r.work++; pb.r.work > maxWork {
				return nil, "the base conditions' analysis is over the guard's work bound", nil
			} else if err := pb.r.ctx.Err(); err != nil {
				return nil, "", err
			} else if pyModNames(q, pb.entries, false)[0] != name {
				continue
			} else if f, err := pb.file(q); err != nil {
				return nil, "", err
			} else if f.unread {
				return nil, q, nil
			} else {
				pb.r.work += len(f.locals)
				for l := range f.locals {
					pb.byName[l] = append(pb.byName[l], name)
				}
			}
		}
	}
	return pb.byName[n], "", nil
}
