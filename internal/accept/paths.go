// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"strings"
	"unicode"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/profile"
)

// Policy is what accept refuses as protected in a candidate's file list.
type Policy struct {
	protected func(path string) bool
	lockfiles map[string]bool
}

// PolicyOf is a checked profile's policy: its protected_paths, and its
// dependency recipe's lockfiles, which no candidate changes until P10's
// preparation exists (00 §8 item 2).
func PolicyOf(p types.ProjectProfile) (Policy, error) {
	m, err := profile.Matcher(p.ProtectedPaths)
	locks := map[string]bool{}
	for _, l := range p.Toolchain.DependencyRecipe.Lockfiles {
		locks[string(l)] = true
	}
	return Policy{protected: m, lockfiles: locks}, err
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

// gitControl: c names, on some filesystem git supports, a git directory,
// hooks directory, attributes or modules file (#226 entry 63). Look-alikes
// count, as git's is_ntfs_dotgit and is_hfs_dotgit read them: case,
// ignorable code points, trailing dots and spaces, a stream suffix, 8.3
// short names.
func gitControl(c string) bool {
	n := strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return unicode.ToLower(r)
	}, c)
	n, _, _ = strings.Cut(n, ":")
	switch n = strings.TrimRight(n, ". "); n {
	case ".git", ".gitattributes", ".gitmodules", ".githooks":
		return true
	}
	return strings.HasPrefix(n, "git~") || strings.HasPrefix(n, "gitatt~") || strings.HasPrefix(n, "gitmod~")
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
