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
	pyInstallKey = regexp.MustCompile(`(?mi)^[ \t]*(package[_-]dir|where)[ \t]*[:=]`)
	pyInstallAny = regexp.MustCompile(`(?mi)package[_-]dir|(^|[^\w])where["']?\s*[:=]|\[tool\.(poetry|hatch|pdm|flit)|build-backend`)
	pyBackend    = regexp.MustCompile(`(?m)^[ \t]*build-backend[ \t]*=[ \t]*["']setuptools[."']`)
	pyPkgDir     = regexp.MustCompile(`^\{\s*""\s*=\s*"([^"\\]+)"\s*\}$`) // package-dir = {"" = "src"}
	pySysNames   = strings.Fields("sys path insert append os join dirname abspath realpath __file__ str Path parent resolve")
	pySysAttrs   = strings.Fields("argv platform version_info version implementation executable exit stdout stderr stdin maxsize byteorder flags " +
		"getrecursionlimit setrecursionlimit exc_info prefix base_prefix float_info hexversion getdefaultencoding getfilesystemencoding") // sys attributes read as no sys.path change
)

// pyInstall reads the entries an install adds (Mythos #270 M1), as an
// allowlist (Opus #272 (c)): setup.cfg's package_dir = =DIR (any case, = or
// :), pyproject's package-dir = {"" = "DIR"}, a where = (packages.find), a
// setuptools build-backend, a .py under src/. Any other package_dir, where,
// [tool.poetry|hatch|pdm|flit] or build-backend, or an entry outside the
// root, is unread.
func (r *run) pyInstall() (entries []string, unread bool, err error) {
	for _, p := range slices.Sorted(maps.Keys(r.files)) {
		switch base := path.Base(p); {
		case strings.HasPrefix(p, "src/") && path.Ext(p) == ".py":
			entries = append(entries, "src")
		case base == "setup.cfg" || base == "pyproject.toml" || base == "tox.ini":
			b, why, err := r.file(r.files[p])
			if err != nil || why != "" {
				return nil, true, err
			}
			s := strings.ReplaceAll(string(b), "\r\n", "\n")
			parsed := len(pyBackend.FindAllString(s, -1))
			for _, m := range pyInstallKey.FindAllStringSubmatchIndex(s, -1) {
				v, key := strings.TrimSpace(pyKey.Split(s[m[1]:], 2)[0]), strings.ToLower(s[m[2]:m[3]])
				words := pyWords.Split(v, -1)
				if key != "where" {
					if g := pyPkgDir.FindStringSubmatch(v); g != nil {
						words = g[1:]
					} else if d, ok := strings.CutPrefix(v, "="); ok && !strings.ContainsAny(d, "=:\n") {
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
			if parsed != len(pyInstallAny.FindAllString(s, -1)) {
				return nil, true, nil
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

// pySysPath reads a line of base file q naming sys.path (Mythos #270 M1):
// sys.path.insert or append with only literals, os.path's join, dirname,
// abspath, realpath, __file__ or Path(__file__).parent in it, read as q's
// directory and its ancestors, each joined with the literals in order
// (Opus #272 (c)). Anything else is unread (an allowlist).
func pySysPath(q string, t []ptok) (entries []string, ok bool) {
	if len(t) < 6 || pyJoin(t[:4]) != "sys.path." || t[4].text != "insert" && t[4].text != "append" {
		return nil, false
	}
	var lits []string
	for _, k := range t {
		switch {
		case k.kind == 's' && pyStr(k) != "":
			lits = append(lits, pyStr(k))
		case k.kind == 's' || k.kind == 'n' && !slices.Contains(pySysNames, k.text) || k.kind == 'o' && !strings.Contains(".,()", k.text):
			return nil, false
		}
	}
	for d := path.Dir(q); ; d = path.Dir(d) {
		entries = append(entries, d)
		if j := path.Join(append([]string{d}, lits...)...); !slices.ContainsFunc(lits, path.IsAbs) && j != ".." && !strings.HasPrefix(j, "../") {
			entries = append(entries, j)
		}
		if d == "." {
			return entries, true
		}
	}
}

// across follows chain d into the base module it stops in (Mythos #270
// M3): the module-level statements and def bodies binding the name it
// reads, to a fixed point, and the dotted names they import, resolved
// (Opus #272 H1), or a star import binds (#272 M2), until one reaches the
// changed module (hit). A chain through no base module is a third-party
// proxy: each of its names is matched by name, and followed into the base
// modules binding it (Mythos #270 M4, Opus #272 M3).
func (pb *pyBase) across(d string, hit func(string, bool) string, rebound func(string) bool) (y, unread string, err error) {
	seen, queue := map[string]bool{}, []string{d}
	follow := func(x string) string { // x resolved: c's attribute it reaches, or queued
		if err != nil || unread != "" {
			return ""
		} else if x, unread, err = pb.resolve(x); err == nil && unread == "" {
			if y := hit(x, true); y != "" {
				return y
			}
			queue = append(queue, x)
		}
		return ""
	}
	for ; len(queue) > 0 && err == nil && unread == ""; queue = queue[1:] {
		if seen[queue[0]] {
			continue
		}
		seen[queue[0]] = true
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
					queue = append(queue, m+"."+n)
				}
			}
			continue
		}
		f, err := pb.file(pb.mods[strings.Join(parts[:i], ".")])
		if err != nil {
			return "", "", err
		} else if f.unread {
			return "", f.path, nil
		}
		names, taken := []string{parts[i]}, map[string]bool{parts[i]: true}
		for k := 0; k < len(names); k++ {
			if n := names[k]; len(f.binders[n]) == 0 && f.from[n] == "" {
				for _, s := range f.stars { // through a star import (Opus #272 M2)
					if pb.mods[s] == "" {
						return "", "the star import " + s, nil
					} else if y := follow(s + "." + n); y != "" {
						return y, "", nil
					}
				}
			}
			for _, u := range f.binders[names[k]] {
				if pb.r.work += len(f.units[u]); pb.r.work > maxWork {
					return "", "the base conditions' analysis is over the guard's work bound", nil
				}
				for j, t := range f.units[u] {
					if t.kind != 'n' || j > 0 && f.units[u][j-1].text == "." || slices.Contains(pyKeywords, t.text) || slices.Contains([]string{"True", "False", "None"}, t.text) {
						continue
					} else if !taken[t.text] {
						taken[t.text], names = true, append(names, t.text) // its module-level binding, and its import too (Opus #272 L3)
					}
					if to := f.from[t.text]; to != "" {
						if !f.named[t.text] && strings.HasPrefix(to, t.text+".") {
							to = t.text // import a.b binds a
						}
						for k := j; k+2 < len(f.units[u]) && f.units[u][k+1].text == "." && f.units[u][k+2].kind == 'n'; k += 2 {
							to += "." + f.units[u][k+2].text // u.d(): u.d (Opus #272 H1)
						}
						if y := follow(to); y != "" {
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
// dotted name each (Opus #272 M3), indexed once.
func (pb *pyBase) binding(n string) ([]string, string, error) {
	if pb.byName == nil {
		pb.byName = map[string][]string{}
		for name, q := range pb.mods {
			if f, err := pb.file(q); err != nil {
				return nil, "", err
			} else if f.unread {
				return nil, q, nil
			} else if pyModNames(q, pb.entries, false)[0] == name {
				for l := range f.locals {
					pb.byName[l] = append(pb.byName[l], name)
				}
			}
		}
	}
	return pb.byName[n], "", nil
}
