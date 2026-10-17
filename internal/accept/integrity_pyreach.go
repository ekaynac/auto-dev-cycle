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

// P14c-2a-reach (Mythos #270 M1–M4, r2 L4) and P14c-2a-roots: conditions
// that reach a changed module through an application module, across
// modules or through a third-party proxy; the roots an install adds, read
// only where they can be read exactly, and only to name shadows (Opus #275
// H2). A test file's sys.path change leaves the base unread, as on main
// (literal sys.path roots: P14c-2a-syspath, Astra #275 H4, H5).

var (
	pySection    = regexp.MustCompile(`(?m)^[ \t]*\[([^\]\n]*)\][ \t]*$`)
	pyInstallKey = regexp.MustCompile(`(?mi)^(package[_-]dir|where|build-backend)[ \t]*[:=]`)                     // no indented key (Opus #275 H3)
	pyInstallAny = regexp.MustCompile(`(?i)package[_-]dir|(^|[^\w])where["']?\s*[:=]|build-backend|backend-path`) // any backend-path unread (Opus #275 H4)
	pyFindPkgs   = regexp.MustCompile(`(?m)^packages[ \t]*[:=][ \t]*find(_namespace)?:[ \t]*$`)
	pyBackend    = regexp.MustCompile(`(?m)^[ \t]*build-backend[ \t]*=[ \t]*["']setuptools[."']`) // main's: a setuptools backend is no root
	pyToolOther  = regexp.MustCompile(`(?i)tool["'\s]*\.["'\s]*(poetry|hatch|pdm|flit)`)
	pyPkgDir     = regexp.MustCompile(`^\{\s*""\s*=\s*"([\w./-]+)"\s*\}$`)                              // package-dir = {"" = "src"}
	pyWhere      = regexp.MustCompile(`^(?:\[\s*"([\w./-]+)"\s*\]|\[\s*'([\w./-]+)'\s*\]|([\w./-]+))$`) // where = DIR, or ["DIR"] in TOML
	pyDir        = regexp.MustCompile(`^[\w./-]+$`)                                                     // a directory read exactly: no %(interpolation)s, attr:, file:, quote or escape (Astra #275 H3)
	pySysAttrs   = strings.Fields("argv platform version_info version implementation executable exit stdout stderr stdin maxsize byteorder flags " +
		"getrecursionlimit setrecursionlimit exc_info prefix base_prefix float_info hexversion getdefaultencoding getfilesystemencoding") // sys attributes read as no sys.path change
	// pyInstallKeys: the key each setuptools section is read for (Astra #272 H1).
	pyInstallKeys = map[string]string{"options": "package_dir", "options.packages.find": "where", "tool.setuptools": "package-dir",
		"tool.setuptools.packages.find": "where", "build-system": "build-backend"}
)

// pyInstallRootsRead gates pyInstall's reader: off, a base with an install
// key keeps main's unread verdict (Astra #275 r3: a package's own __path__
// would send a read root's module elsewhere). P14c-2a-install turns it on,
// once P14c-2a-widen models imports and code parked at run time (Opus
// #277 H1, H2). A var only
// so that the reader's own tests can run it. A package variable, not safe
// to change concurrently: a test that sets it must not run in parallel.
var pyInstallRootsRead = false

// pyInstall reads the entries an install adds (Mythos #270 M1), as an
// allowlist (Opus #272 (c)), each key only in its section (Astra #272 H1):
// [options] package_dir = =DIR (any case, = or :), [options.packages.find]
// where, [tool.setuptools] package-dir = {"" = "DIR"},
// [tool.setuptools.packages.find] where, a setuptools [build-system]
// build-backend, a .py under src/; a directory only as exactly [\w./-]
// (Astra #275 H3). Any other such key anywhere, a
// tool.poetry, hatch, pdm or flit table, an entry outside the root, or a
// .pth anywhere (Astra #272 M2) is unread, as on main.
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
			if pyToolOther.MatchString(s) || !pyInstallRootsRead && len(pyInstallAny.FindAllString(string(b), -1)) > len(pyBackend.FindAllString(string(b), -1)) {
				return nil, true, nil // the gate off: main's verdict, a key not a setuptools backend
			}
			parsed, heads, toml := 0, append([][]int{{0, 0, 0, 0}}, pySection.FindAllStringSubmatchIndex(s, -1)...), base == "pyproject.toml"
			if p != base || base == "tox.ini" {
				heads = nil // only the root setup.cfg or pyproject.toml is read (Opus #275 H3)
			}
			for i, h := range heads {
				end, name := len(s), strings.TrimSpace(s[h[2]:h[3]]) // exact case (Opus #275 H3)
				if toml {
					name = strings.NewReplacer(`"`, "", "'", "", " ", "", "\t", "", "[", "", "]", "").Replace(name)
				}
				if i+1 < len(heads) {
					end = heads[i+1][0]
				}
				for _, m := range pyInstallKey.FindAllStringSubmatchIndex(s[h[0]:end], -1) {
					sec := s[h[0]:end]
					v, key := strings.TrimSpace(pyKey.Split(sec[m[1]:], 2)[0]), strings.ToLower(strings.ReplaceAll(sec[m[2]:m[3]], "_", "-"))
					words := slices.DeleteFunc(pyWords.Split(v, -1), func(w string) bool { return w == "" })
					switch {
					case key != strings.ReplaceAll(pyInstallKeys[name], "_", "-") || toml && sec[m[2]:m[3]] != pyInstallKeys[name]:
						return nil, true, nil // a key outside its section, or a TOML key in another case, stays unread, as on main
					case key == "build-backend":
						if words = nil; !slices.Contains([]string{`"setuptools.build_meta"`, `"setuptools.build_meta:__legacy__"`, `'setuptools.build_meta'`, `'setuptools.build_meta:__legacy__'`}, v) {
							return nil, true, nil // exactly setuptools' (Opus #275 H4)
						}
					case key == "where":
						g := pyWhere.FindStringSubmatch(v)
						if g == nil || toml == (g[3] != "") || !toml && !pyFindPkgs.MatchString(s) {
							return nil, true, nil // one directory, read exactly, with packages = find: (Opus #275 H3, Astra #275 H3)
						}
						words = []string{g[1] + g[2] + g[3]}
					case key == "package-dir":
						if g := pyPkgDir.FindStringSubmatch(v); g != nil && name == "tool.setuptools" {
							words = g[1:]
						} else if d, ok := strings.CutPrefix(v, "="); ok && name == "options" && pyDir.MatchString(strings.TrimSpace(d)) {
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
			}
			if parsed != len(pyInstallAny.FindAllString(s, -1)) { // a mention not read exactly stays unread
				return nil, true, nil
			}
		}
	}
	return entries, false, nil
}

// pySysUse: base file line t leaves sys.path as the guard reads it (Mythos
// #270 M1), an allowlist (Opus #272 (b)): sys only after import sys or
// before a dot and an attribute of pySysAttrs. Any sys.path use, from sys
// import path, an alias of sys, getattr(sys, …) or of 'path', a 'sys'
// string, a path changed on something not named sys, site.addsitedir,
// meta_path or path_hooks is not.
func pySysUse(t []ptok) bool {
	if len(t) > 1 && t[0].text == "from" && t[1].text == "sys" || len(t) > 2 && t[0].text == "import" && named(t, "as") && named(t, "sys") {
		return !slices.ContainsFunc(t, func(k ptok) bool { return k.text == "path" || k.text == "*" || k.text == "meta_path" || k.text == "as" })
	}
	for i, k := range t {
		switch {
		case slices.Contains([]string{"addsitedir", "meta_path", "path_hooks", "path_importer_cache"}, k.text) || pyStr(k) == "sys":
			return false // importlib.import_module('sys') too (the review of 1ceb79c)
		case k.text == "path" && i > 0 && t[i-1].text == "." && i+2 < len(t) && t[i+1].text == "." && slices.Contains([]string{"insert", "append", "extend", "remove", "clear", "pop"}, t[i+2].text) && (i < 2 || t[i-2].text != "sys"),
			pyStr(k) == "path" && named(t[:i], "getattr"):
			return false // a path changed on something not named sys: __import__('s' + 'ys') (the review of 9d12467)
		case k.text != "sys" || k.kind != 'n' || i > 0 && t[i-1].text == ".":
		case t[0].text == "import":
		case i+2 >= len(t) || t[i+1].text != "." || !slices.Contains(pySysAttrs, t[i+2].text):
			return false // sys.path in any form, sys.__dict__ (the review of 1ceb79c, #272 split)
		}
	}
	return true
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

// starCheck: why package q's star imports are not inert (P14c-2a-star), an
// allowlist. Each is exactly "from .x import *" at module level, x.py in
// q's own directory is read, plain (pyInert), imports nothing and binds no
// __all__, and no form CPython's FileFinder may load before it, or in its
// place, exists: a package x/__init__.*, an extension x.*.so or x.*.pyd,
// x.pyc, or __pycache__/x.*. A source imports nothing, so it is a leaf.
// The verdict is cached on f, and the competing forms come from three
// directories' entries, charged to the work bound (Astra #279 M1).
func (pb *pyBase) starCheck(q string, f *pyBaseFile) (string, error) {
	if f.starWhy != nil {
		return *f.starWhy, nil
	}
	why, err := pb.starVerdict(q, f)
	if err == nil {
		f.starWhy = &why
	}
	return why, err
}

func (pb *pyBase) starVerdict(q string, f *pyBaseFile) (string, error) {
	if f.badStar {
		return "has a star import other than from .x import * at module level", nil
	}
	tree, d := pb.r.files, path.Dir(q)
	if pb.tree != nil {
		tree = pb.tree
	}
	if pb.dirs == nil {
		pb.dirs, pb.r.work = map[string][]string{}, pb.r.work+len(pb.paths)
		for _, p := range pb.paths {
			pb.dirs[path.Dir(p)] = append(pb.dirs[path.Dir(p)], p)
		}
	}
	for _, x := range f.starSrc {
		src := d + "/" + x + ".py"
		near := slices.Concat(pb.dirs[d], pb.dirs[d+"/"+x], pb.dirs[d+"/__pycache__"])
		if pb.r.work += len(near) + 1; pb.r.work > maxWork || pb.r.ctx.Err() != nil {
			return "is over the guard's work bound", pb.r.ctx.Err()
		} else if tree[src] == "" || slices.ContainsFunc(near, func(p string) bool { return pyStarForm(p, d, x) && p != src && path.Ext(p) != ".pyi" }) {
			return "star-imports ." + x + ", which is not one plain module file", nil
		} else if fs, err := pb.file(src); err != nil {
			return "", err
		} else if !fs.plain || len(fs.mods) > 0 || fs.locals["__all__"] {
			return "star-imports ." + x + ", which may run code or bind __path__", nil
		}
	}
	return "", nil
}

// starCandidate: why the candidate's package __init__.py q has a star import
// that is not inert, judged on the candidate's files (P14c-2a-star).
func (pb *pyBase) starCandidate(q string) (string, error) {
	f, err := pb.candFile(q)
	if err != nil {
		return "", err
	} else if f.unread {
		return "a package __init__.py the guard cannot read", nil
	}
	why, err := pb.cand.starCheck(q, f)
	if err == nil && why == "" && len(f.starSrc) > 0 && !f.plain {
		why = "star-imports and is not plain" // its other statements may move its submodules (Opus #279 L2)
	}
	if err != nil || why == "" {
		return "", err
	}
	return "a package __init__.py that " + why, nil
}

// starSourceOf: the base package directory whose __init__.py star-imports
// the module that path p is a form of (pyStarForm), or "". Only the
// __init__.py of p's own package is read, never all of them (Opus #279 L3).
func (pb *pyBase) starSourceOf(p string) (string, error) {
	d, base := path.Dir(p), path.Base(p)
	cands := [][2]string{{d, strings.SplitN(base, ".", 2)[0]}} // d/x.*
	if strings.HasPrefix(base, "__init__.") {
		cands = append(cands, [2]string{path.Dir(d), path.Base(d)}) // d/x/__init__.*
	} else if path.Base(d) == "__pycache__" {
		cands = append(cands, [2]string{path.Dir(d), strings.SplitN(base, ".", 2)[0]}) // d/__pycache__/x.*
	}
	for _, c := range cands {
		if q := path.Join(c[0], "__init__.py"); pb.r.files[q] == "" {
			continue
		} else if f, err := pb.file(q); err != nil {
			return "", err
		} else if slices.Contains(f.starSrc, c[1]) {
			return c[0], nil
		}
	}
	return "", nil
}

// closure: the base modules that run because a judged file imports them,
// transitively, under every root (P14c-2a-widen). In each, a sys use
// pySysUse does not read leaves the base unread, as in a judged file; so
// does any signal risky names, and a statement run at import outside
// pyAllowed in an application module (a judged file's is P14c-2a-allow's).
// No write is attributed to a module (the split: widen-b). Base symlinks
// and gitlinks name opaque modules.
func (pb *pyBase) closure() error {
	pb.opaque, pb.bad = map[string]bool{}, map[string]bool{}
	for p, mode := range pb.r.links {
		for _, n := range pyModNames(p, pb.shadowEntries, false) {
			pb.opaque[n] = true
		}
		if mode != "120000" {
			continue // a gitlink has no target in the tree
		}
		b, why, err := pb.r.file(pb.r.files[p])
		if err != nil {
			return err
		}
		// out of the root, the root itself or a .. past the first component: every change (Opus #281 H4, Astra
		// #281 H4). A target through another link is at or under that link's own target, already matched.
		if t := path.Join(path.Dir(p), string(b)); why != "" || path.IsAbs(string(b)) || slices.Contains(strings.Split(string(b), "/")[1:], "..") || t == "." || t == ".." || strings.HasPrefix(t, "../") {
			pb.linkTargets = append(pb.linkTargets, "")
		} else {
			pb.linkTargets = append(pb.linkTargets, t)
		}
	}
	seen, names, queue := map[*pyBaseFile]bool{}, map[string]bool{}, slices.Clone(pb.tests)
	for ; len(queue) > 0; queue = queue[1:] {
		f := queue[0]
		if pb.r.work++; pb.r.work > maxWork || pb.r.ctx.Err() != nil {
			pb.holderUnread = cmp.Or(pb.holderUnread, "the base conditions' analysis is over the guard's work bound")
			return pb.r.ctx.Err()
		} else if seen[f] {
			continue
		}
		seen[f], pb.sysUnread = true, pb.sysUnread || f.sysUnread
		if why := pb.risky(f); why != "" || f.sysUnread {
			pb.holderUnread, pb.bad[f.path] = cmp.Or(pb.holderUnread, why), true
		} else if f.impure && !pyTested(f.path) && !pb.r.pyTestDir(f.path) {
			pb.holderUnread = cmp.Or(pb.holderUnread, "the module "+f.path+" runs code at import outside the allowlist")
		}
		tried := map[string]bool{}
		for _, m := range f.mods { // every package on the way runs too; each attempt charged, a new name by its bytes, before it is split (Astra #281 M1)
			if pb.r.work += 1 + len(m)*b2i(!tried[m]); pb.r.work > maxWork || pb.r.ctx.Err() != nil {
				pb.holderUnread = cmp.Or(pb.holderUnread, "the base conditions' analysis is over the guard's work bound")
				return pb.r.ctx.Err()
			} else if tried[m] {
				continue
			} else if tried[m] = true; strings.Count(m, ".") >= pyMaxDepth {
				pb.holderUnread = cmp.Or(pb.holderUnread, "the module "+f.path+" imports a name past the depth bound")
				continue
			}
			for parts, k := strings.Split(m, "."), 1; k <= len(parts); k++ {
				if n := strings.Join(parts[:k], "."); names[n] {
					continue // each name once
				} else if pb.opaque[n] {
					pb.holderUnread = cmp.Or(pb.holderUnread, "the module "+n+" is a symlink or a gitlink in the base") // Opus #281 H4
				} else if names[n] = true; cmp.Or(pb.mods[n], pb.shadowMods[n]) != "" {
					g, err := pb.file(cmp.Or(pb.mods[n], pb.shadowMods[n]))
					if err != nil {
						return err
					}
					queue = append(queue, g)
				}
			}
		}
	}
	return nil
}

// risky: why f, a closure module, may change what a condition reads, or
// "": a dynamic import, an unread file or a base link (Astra #281 H5), a
// write word or a target the recognizer cannot read, or any attribute,
// subscript or del target run at import, whatever its head (the split:
// attribution is widen-b's). In the candidate's closure, a statement run
// at import outside pyAllowed alone stays main's warning (Opus #266 M1).
func (pb *pyBase) risky(f *pyBaseFile) string {
	switch {
	case f.runs:
		return "the module " + f.path + " imports or runs code dynamically"
	case f.unread:
		return "the module " + f.path + " is unread"
	case f.active:
		return "the module " + f.path + " writes by a call or through a target the guard cannot read"
	case len(f.impWrites) > 0:
		return "the module " + f.path + " writes " + f.impWrites[0] + " at import"
	}
	return ""
}

// candClosure is the first module the candidate's closure (from its judged
// files and the changed files) reaches that is not inert, writes, imports
// dynamically, uses sys or is unread, and that the base closure did not
// reach as one (Opus #281 M1, Astra #281 H5). Each import is charged by its
// bytes before it is split, and each path queued once (Astra #281 M1).
func (pb *pyBase) candClosure(changed []string) (string, error) {
	mods := map[string]string{}
	for _, p := range slices.Sorted(maps.Keys(pb.r.cand)) {
		for _, n := range pyModNames(p, pb.shadowEntries, false) {
			if path.Ext(p) == ".py" && mods[n] == "" {
				mods[n] = p
			}
		}
	}
	queue := slices.Clone(changed)
	for p := range pb.r.cand {
		if path.Ext(p) == ".py" && (pyTested(p) || pb.r.pyTestDir(p)) {
			queue = append(queue, p)
		}
	}
	slices.Sort(queue)
	queue = slices.Compact(queue)
	queued := map[string]bool{}
	for _, p := range queue {
		queued[p] = true
	}
	for ; len(queue) > 0; queue = queue[1:] {
		if pb.r.work++; pb.r.work > maxWork || pb.r.ctx.Err() != nil {
			return "the candidate's closure is over the guard's work bound", pb.r.ctx.Err()
		} else if p := queue[0]; pb.r.cand[p] == "" {
			continue
		} else if f, err := pb.candFile(p); err != nil {
			return "", err
		} else if (pb.risky(f) != "" || f.sysUnread) && !pb.bad[p] {
			return p, nil
		} else {
			for _, m := range f.mods {
				if pb.r.work += 1 + len(m); pb.r.work > maxWork || pb.r.ctx.Err() != nil {
					return "the candidate's closure is over the guard's work bound", pb.r.ctx.Err()
				} else if strings.Count(m, ".") >= pyMaxDepth {
					return p, nil // a name past the depth bound
				}
				for parts, k := strings.Split(m, "."), 1; k <= len(parts); k++ {
					if n := strings.Join(parts[:k], "."); pb.opaque[n] {
						return p, nil // a base link or gitlink newly reached (Astra #281 r2 H5)
					} else if q := mods[n]; q != "" && !queued[q] {
						queued[q], queue = true, append(queue, q)
					}
				}
			}
		}
	}
	return "", nil
}

// candFile is the candidate's version of q, read with the candidate's tree.
func (pb *pyBase) candFile(q string) (*pyBaseFile, error) {
	if pb.cand == nil {
		pb.cand = &pyBase{r: pb.r, tree: pb.r.cand, files: map[string]*pyBaseFile{}, paths: slices.Sorted(maps.Keys(pb.r.cand))}
	}
	return pb.cand.file(q)
}

// pyStarForm: path p is a form of module x in directory d that CPython may
// load: x.py, a package x/__init__.*, x.<anything> (extensions, .pyc), or
// __pycache__/x.*.
func pyStarForm(p, d, x string) bool {
	return slices.ContainsFunc([]string{d + "/" + x + ".", d + "/" + x + "/__init__.", d + "/__pycache__/" + x + "."}, func(pre string) bool { return strings.HasPrefix(p, pre) })
}

// prefix is how many of parts name the base module Python imports, at most
// top: main's longest module prefix, or unread (strictly additive, Astra
// #275 r3). Unread: a shorter prefix that is a plain module, or a package
// whose directory does not hold the longer one (Astra #272 r2 H1);
// any ancestor only an added root names, or two files answer to (Astra
// #275 H1, Opus #275 H1); a package not plain (pyInert: its own __path__,
// Opus #275 M1, Astra #275 H6), or binding the next name otherwise than as
// its submodule.
func (pb *pyBase) prefix(parts []string, top int) (int, string, error) {
	i := min(top, len(parts))
	for ; i >= 1 && pb.mods[strings.Join(parts[:i], ".")] == ""; i-- {
		if pb.r.work++; pb.r.work > maxWork || pb.r.ctx.Err() != nil { // Opus #275 L3
			return 0, "the base conditions' analysis is over the guard's work bound", pb.r.ctx.Err()
		}
	}
	for k := 1; k <= min(top, len(parts)); k++ { // every ancestor, a namespace portion's too (Astra #275 H1)
		if name := strings.Join(parts[:k], "."); pb.mods[name] == "" && pb.shadowMods[name] != "" {
			return 0, "the module " + name + " is under a root the guard reads for shadows only", nil // main reads no added root
		} else if pb.alts[name] > 1 { // at any depth (Opus #275 H1); a module and a package (Astra #272 r2 H1)
			return 0, "the module name " + name + " in two roots", nil
		}
	}
	for k := 1; k <= i; k++ {
		name, q := strings.Join(parts[:k], "."), pb.mods[strings.Join(parts[:k], ".")]
		if q == "" {
			continue // a namespace portion
		} else if path.Base(q) == "__init__.py" && k < len(parts) {
			f, err := pb.file(q)
			if err != nil {
				return 0, "", err
			} else if !f.plain {
				return 0, "the package " + name + " is not plain (its submodules may resolve elsewhere)", nil // Opus #275 M1, Astra #275 H6
			} else if why, err := pb.starCheck(q, f); err != nil {
				return 0, "", err
			} else if why != "" {
				return 0, "the package " + name + " " + why, nil // P14c-2a-star
			} else if k < i && (f.locals[parts[k]] || f.from[parts[k]] != "" && f.from[parts[k]] != name+"."+parts[k]) {
				return 0, "the package " + name + " binds " + parts[k] + " beside its submodule", nil
			}
		}
		if k < i && (path.Base(q) != "__init__.py" || !strings.HasPrefix(pb.mods[strings.Join(parts[:i], ".")], path.Dir(q)+"/")) {
			return 0, "the module " + name + " holds no submodule " + parts[k], nil // main takes the longer one; Python the shorter: unread
		}
	}
	return i, "", nil
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
	push := func(x string) { // every attempt charged; nothing allocated past the bound (Astra #272 r2 M1)
		if pb.r.work++; !seen[x] && pb.r.work <= maxWork {
			seen[x], queue = true, append(queue, x)
		}
	}
	over := func() bool { // exhausted: the bound or ctx, propagated at once (Astra #272 r3 M1)
		if err = pb.r.ctx.Err(); err == nil && pb.r.work > maxWork {
			unread = "the base conditions' analysis is over the guard's work bound"
		}
		return err != nil || unread != ""
	}
	follow := func(x string) string { // x resolved: c's attribute it reaches, or queued
		if pb.r.work++; err != nil || unread != "" || over() {
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
		i, why, perr := pb.prefix(parts, len(parts)-1)
		if perr != nil || why != "" {
			return "", why, perr
		} else if i < 1 {
			names := map[string]bool{}
			for _, n := range parts[1:] { // every head: a value may be parked on pytest or os (the review of 1ceb79c)
				if names[n] || over() {
					continue // each name once (Astra #272 r2 M1)
				} else if names[n] = true; rebound(n) {
					return n, "", nil
				}
				mods, why, err := pb.binding(n)
				if err != nil || why != "" {
					return "", why, err
				}
				for _, m := range mods {
					if push(m + "." + n); over() {
						break
					}
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
		star := func(chain string) string { // a name neither bound nor imported, through each star import
			for _, s := range f.stars {
				if over() {
					return ""
				} else if pb.mods[s] == "" {
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
				if pb.r.work += len(f.units[u]); over() {
					return "", "the base conditions' analysis is over the guard's work bound", nil
				}
				unit := f.units[u]
				for j, t := range unit {
					if over() {
						return "", unread, err
					} else if t.kind != 'n' || j > 0 && unit[j-1].text == "." || slices.Contains(pyKeywords, t.text) || slices.Contains([]string{"True", "False", "None"}, t.text) {
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
