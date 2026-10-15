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
	pyInstallKey = regexp.MustCompile(`(?m)^[ \t]*(package_dir|package-dir|where)[ \t]*=`)
	pyPkgDir     = regexp.MustCompile(`^\{\s*""\s*=\s*"([^"\\]+)"\s*\}$`) // package-dir = {"" = "src"}
	pySysNames   = strings.Fields("sys path insert append os join dirname abspath realpath __file__ str Path parent resolve")
)

// pyInstall reads the entries an install adds (Mythos #270 M1): setup.cfg's
// package_dir = =DIR, pyproject's package-dir = {"" = "DIR"}, any where =
// (packages.find), a src/ package. Another package_dir mapping, or an entry
// outside the root, is unread. A setup.py or a PYTHONPATH in a
// configuration is an entry the guard does not model (M6).
func (r *run) pyInstall() (entries []string, unmodelled, unread bool, err error) {
	for _, p := range slices.Sorted(maps.Keys(r.files)) {
		switch base := path.Base(p); {
		case base == "setup.py":
			unmodelled = true
		case strings.HasPrefix(p, "src/") && strings.Count(p, "/") == 2 && base == "__init__.py":
			entries = append(entries, "src")
		case base == "setup.cfg" || base == "pyproject.toml" || base == "tox.ini":
			b, why, err := r.file(r.files[p])
			if err != nil || why != "" {
				return nil, true, true, err
			}
			s := strings.ReplaceAll(string(b), "\r\n", "\n")
			unmodelled = unmodelled || strings.Contains(s, "PYTHONPATH")
			for _, m := range pyInstallKey.FindAllStringSubmatchIndex(s, -1) {
				v, key := strings.TrimSpace(pyKey.Split(s[m[1]:], 2)[0]), s[m[2]:m[3]]
				words := pyWords.Split(v, -1)
				if key != "where" {
					if g := pyPkgDir.FindStringSubmatch(v); g != nil {
						words = g[1:]
					} else if d, ok := strings.CutPrefix(v, "="); ok && !strings.ContainsAny(d, "=\n") {
						words = []string{strings.TrimSpace(d)}
					} else {
						return nil, true, true, nil
					}
				}
				for _, w := range words {
					if j := path.Join(path.Dir(p), w); w != "" && (path.IsAbs(w) || j == ".." || strings.HasPrefix(j, "../")) {
						return nil, true, true, nil
					} else if w != "" {
						entries = append(entries, j)
					}
				}
			}
		}
	}
	return entries, unmodelled, false, nil
}

// pySysPath reads a line of base file q naming sys.path (Mythos #270 M1):
// sys.path.insert or append with only literals, os.path's join, dirname,
// abspath, realpath, __file__ or Path(__file__).parent in it, read as q's
// directory and its ancestors, each joined with each literal. Anything else
// is unread (an allowlist).
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
		for _, l := range lits {
			if j := path.Join(d, l); !path.IsAbs(l) && j != ".." && !strings.HasPrefix(j, "../") {
				entries = append(entries, j)
			}
		}
		if d == "." {
			return entries, true
		}
	}
}

// across follows chain d into the base module it stops in (Mythos #270
// M3): the module-level statements and def bodies binding the name it
// reads, to a fixed point, and the names they import, resolved, until one
// reaches the changed module (hit). A chain through no base module is a
// third-party proxy: its last name is matched by name (M4).
func (pb *pyBase) across(d string, hit func(string, bool) string, rebound func(string) bool) (y, unread string, err error) {
	seen := map[string]bool{}
	for queue := []string{d}; len(queue) > 0; queue = queue[1:] {
		if seen[queue[0]] {
			continue
		}
		seen[queue[0]] = true
		parts := strings.Split(queue[0], ".")
		i := len(parts) - 1
		for ; i >= 1 && pb.mods[strings.Join(parts[:i], ".")] == ""; i-- {
		}
		if i < 1 {
			if y := parts[len(parts)-1]; len(parts) > 1 && rebound(y) {
				return y, "", nil
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
			for _, u := range f.binders[names[k]] {
				if pb.r.work += len(f.units[u]); pb.r.work > maxWork {
					return "", "the base conditions' analysis is over the guard's work bound", nil
				}
				for _, t := range f.units[u] {
					if t.kind != 'n' || taken[t.text] {
						continue
					} else if taken[t.text] = true; f.from[t.text] == "" {
						names = append(names, t.text)
						continue
					}
					to := f.from[t.text]
					if !f.named[t.text] && strings.HasPrefix(to, t.text+".") {
						to = t.text // import a.b binds a
					}
					if to, unread, err = pb.resolve(to); err != nil || unread != "" {
						return "", unread, err
					} else if y := hit(to, true); y != "" {
						return y, "", nil
					}
					queue = append(queue, to)
				}
			}
		}
	}
	return "", "", nil
}
