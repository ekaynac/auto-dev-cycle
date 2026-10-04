// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// Glob bounds: a protected glob is short, and the coverage checks of a whole
// profile together cost at most maxCoverageWork byte comparisons (see
// matchWork), so validation stays fast whatever the profile.
const (
	maxGlobBytes    = 256
	maxGlobSegments = 32
	maxCoverageWork = 1 << 22
)

// checkRepoPath: a relative, slash-separated, clean path inside the
// repository.
func checkRepoPath(p string) error {
	switch {
	case strings.ContainsRune(p, 0):
		return errors.New("a path has no NUL byte")
	case strings.ContainsRune(p, '\\'):
		return errors.New("use forward slashes")
	case strings.HasPrefix(p, "/"):
		return errors.New("must be relative to the repository root")
	case p == "." || path.Clean(p) != p:
		return fmt.Errorf("must be clean (%q)", path.Clean(p))
	case p == ".." || strings.HasPrefix(p, "../"):
		return errors.New("must stay inside the repository")
	}
	return nil
}

// compileGlob splits a repository glob into segments, each "**" (any number
// of segments) or a path.Match pattern.
func compileGlob(g string) ([]string, error) {
	if len(g) > maxGlobBytes {
		return nil, fmt.Errorf("a glob is at most %d bytes", maxGlobBytes)
	}
	if strings.ContainsRune(g, 0) {
		return nil, errors.New("a glob has no NUL byte")
	}
	if strings.ContainsRune(g, '\\') || strings.HasPrefix(g, "/") || path.Clean(g) != g {
		return nil, errors.New("a glob is relative, clean and slash-separated")
	}
	segs := strings.Split(g, "/")
	if len(segs) > maxGlobSegments {
		return nil, fmt.Errorf("a glob has at most %d segments", maxGlobSegments)
	}
	for _, s := range segs {
		switch {
		case s == "..":
			return nil, errors.New("a glob stays inside the repository")
		case s == "**":
		case strings.Contains(s, "**"):
			return nil, fmt.Errorf("%q: ** must be a whole segment", s)
		default:
			if _, err := path.Match(s, ""); err != nil {
				return nil, fmt.Errorf("%q: %w", s, err)
			}
		}
	}
	return segs, nil
}

// match reports whether the glob segments g match the segments s, where one
// decides a single non-** glob segment against a single segment of s. A
// "**" in g matches any number of segments of s. It fills one table of
// (len(g)+1)·(len(s)+1) positions, bottom-up, so its work is exactly
// matchWork(g, s), whatever the "**"s.
func match(g, s []string, one func(g, s string) bool) bool {
	w := len(s) + 1
	at := make([]bool, (len(g)+1)*w) // at[i*w+j]: g[i:] matches s[j:]
	at[len(g)*w+len(s)] = true
	for i := len(g) - 1; i >= 0; i-- {
		for j := len(s); j >= 0; j-- {
			switch {
			case g[i] == "**":
				at[i*w+j] = at[(i+1)*w+j] || (j < len(s) && at[i*w+j+1])
			case j < len(s):
				at[i*w+j] = at[(i+1)*w+j+1] && one(g[i], s[j])
			}
		}
	}
	return at[0]
}

// matchWork bounds the cost of match(g, s) in byte comparisons. The table
// has (len(g)+1)·(len(s)+1) cells, and the cell for g[i] against s[j] calls
// path.Match, which never backtracks across stars and so costs at most
// about (len(g[i])+1)·(len(s[j])+1). Summed over the table, that is at most
// weight(g)·weight(s), where weight adds one per segment and per byte.
func matchWork(g, s []string) int { return weight(g) * weight(s) }

func weight(segs []string) int {
	n := 1
	for _, x := range segs {
		n += len(x) + 1
	}
	return n
}

// matchesPath: the glob matches the literal repository path segments.
func matchesPath(g, segs []string) bool {
	return match(g, segs, func(g, s string) bool {
		ok, _ := path.Match(g, s)
		return ok
	})
}

// covers: every path the glob d matches is matched by the glob g. It is
// conservative: a d segment that is a pattern is covered only by "*", "**" or
// the identical pattern, never because g matches d's spelling.
func covers(g, d []string) bool {
	return match(g, d, func(g, s string) bool {
		switch {
		case s == "**":
			return false // only a ** in g covers a ** in d
		case g == s, g == "*":
			return true
		case strings.ContainsAny(s, `*?[\`):
			return false
		}
		ok, _ := path.Match(g, s)
		return ok
	})
}

func anyCovers(globs [][]string, d []string) bool {
	for _, g := range globs {
		if covers(g, d) {
			return true
		}
	}
	return false
}

func anyMatches(globs [][]string, segs []string) bool {
	for _, g := range globs {
		if matchesPath(g, segs) {
			return true
		}
	}
	return false
}
