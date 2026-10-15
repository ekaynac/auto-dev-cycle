// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
)

// P14c-2a-reach (Mythos #270 M1–M4, r2 L4): conditions that reach a
// changed module through an application module, across modules or through
// a third-party proxy; and the sys.path entries the guard does not read,
// which leave the base unread. Reading them as roots is P14c-2a-roots'
// (#272 split per 04 §2).

var (
	pyInstallAny = regexp.MustCompile(`(?i)package[_-]dir|(^|[^\w])where["']?\s*[:=]|build-backend`)
	pyBackend    = regexp.MustCompile(`(?m)^[ \t]*build-backend[ \t]*=[ \t]*["']setuptools[."']`)
	pyToolOther  = regexp.MustCompile(`(?i)tool["'\s]*\.["'\s]*(poetry|hatch|pdm|flit)`)
	pySysAttrs   = strings.Fields("argv platform version_info version implementation executable exit stdout stderr stdin maxsize byteorder flags " +
		"getrecursionlimit setrecursionlimit exc_info prefix base_prefix float_info hexversion getdefaultencoding getfilesystemencoding") // sys attributes read as no sys.path change
)

// pyInstall: the base adds sys.path entries the guard does not read
// (Mythos #270 M1, Opus and Astra #272): a .pth anywhere (Astra #272 M2);
// in setup.cfg, pyproject.toml or tox.ini any package_dir, package-dir or
// where key, a [tool.poetry|hatch|pdm|flit] table, or a build-backend not
// setuptools'. Unread: every Python change blocks.
func (r *run) pyInstall() (unread bool, err error) {
	for _, p := range slices.Sorted(maps.Keys(r.files)) {
		switch base := path.Base(p); {
		case path.Ext(p) == ".pth":
			return true, nil
		case base == "setup.cfg" || base == "pyproject.toml" || base == "tox.ini":
			b, why, err := r.file(r.files[p])
			if err != nil || why != "" {
				return true, err
			} else if s := string(b); len(pyInstallAny.FindAllString(s, -1)) > len(pyBackend.FindAllString(s, -1)) || pyToolOther.MatchString(s) {
				return true, nil
			}
		}
	}
	return false, nil
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
	over := func() bool { return pb.r.work > maxWork || pb.r.ctx.Err() != nil }
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
		for ; i >= 1 && pb.mods[strings.Join(parts[:i], ".")] == "" && !over(); i-- {
			pb.r.work++
		}
		if i < 1 {
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
				if pb.r.work += len(f.units[u]); over() {
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
