// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"strings"
	"unicode"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/profile"
)

// Policy is what accept refuses as protected in a candidate's file list.
// Only PolicyOf makes one; Accept refuses any other.
type Policy struct {
	protected func(path string) bool
	lockfiles map[string]bool
	ok        bool
}

// PolicyOf is a checked profile's policy: its protected_paths, and its
// dependency recipe's lockfiles, which no candidate changes until P10's
// preparation exists (00 §8 item 2). Globs match case-sensitively, as
// paths do on the Linux hosts that run the gate: Conftest.py is not
// conftest.py, and pytest does not load it.
func PolicyOf(p types.ProjectProfile) (Policy, error) {
	m, err := profile.Matcher(p.ProtectedPaths)
	if err != nil {
		return Policy{}, err
	}
	locks := map[string]bool{}
	for _, l := range p.Toolchain.DependencyRecipe.Lockfiles {
		locks[string(l)] = true
	}
	return Policy{protected: m, lockfiles: locks, ok: true}, nil
}

// Protected: p has a git control component (gitControl), or is a lockfile
// or a profile glob's.
func (pol Policy) Protected(p string) bool {
	for _, c := range strings.Split(p, "/") {
		if gitControl(c) {
			return true
		}
	}
	return pol.lockfiles[p] || pol.protected != nil && pol.protected(p)
}

// fold is c as NTFS and HFS+ compare it: lower case, without ignorable
// code points.
func fold(c string) string {
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return unicode.ToLower(r)
	}, c)
}

// ntfsTail: s is only spaces and periods, up to its end or a ':' (NTFS
// drops them, or names a stream).
func ntfsTail(s string) bool {
	head, _, _ := strings.Cut(s, ":")
	return strings.Trim(head, ". ") == ""
}

// dotGit: c is, on some filesystem, git's own directory (is_ntfs_dotgit,
// is_hfs_dotgit).
func dotGit(c string) bool {
	n := fold(c)
	return strings.HasPrefix(n, ".git") && ntfsTail(n[4:]) || strings.HasPrefix(n, "git~1") && ntfsTail(n[5:])
}

// dotGeneric mirrors git's is_ntfs_dot_generic for ".name": the name, its
// regular 8.3 short name (first six characters, ~1 to ~4) or its hashed
// fall-back (a prefix of hashed, ~1 to ~9, digits, eight characters).
func dotGeneric(n, name, hashed string) bool {
	switch {
	case strings.HasPrefix(n, "."+name) && ntfsTail(n[len(name)+1:]):
		return true
	case len(n) >= 8 && n[:6] == name[:6] && n[6] == '~' && n[7] >= '1' && n[7] <= '4' && ntfsTail(n[8:]):
		return true
	case hashed == "" || len(n) < 8:
		return false
	}
	t := strings.IndexByte(n[:7], '~')
	if t < 0 || n[:t] != hashed[:t] || n[t+1] < '1' || n[t+1] > '9' || strings.Trim(n[t+2:8], "0123456789") != "" {
		return false
	}
	return ntfsTail(n[8:])
}

// gitControl: c names, on some filesystem git supports, a git directory,
// hooks directory, attributes or modules file (#226 entry 63).
func gitControl(c string) bool {
	n := fold(c)
	return dotGit(c) || dotGeneric(n, "gitattributes", "gi7d29") || dotGeneric(n, "gitmodules", "gi7eba") || dotGeneric(n, "githooks", "")
}

// unsafePath: p is not one plain repository path: empty, absolute, with an
// empty, "." or ".." component, or a backslash (a separator elsewhere).
func unsafePath(p string) bool {
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." || c == ".." || strings.ContainsAny(c, "\\\x00") {
			return true
		}
	}
	return false
}
