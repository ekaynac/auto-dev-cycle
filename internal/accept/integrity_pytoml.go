// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"strings"
)

// pyTOML is what pytest may read of a TOML file other than pytest.toml:
// each statement whose key path, its table's included, names pytest in a
// component ([tool.pytest.ini_options], [tool.pytest], a dotted or quoted
// key under [tool] or at the root, a pytest11 entry point), and each whose
// value holds an inline table naming pytest or escaping. The file is lexed
// as TOML (strings of all four kinds, arrays and inline tables across
// lines, comments), so free text elsewhere (a classifier, another tool's
// options) is never read as pytest's (the widen-b baseline: networkx,
// setuptools, arrow). false: a form it does not lex (an escaped or
// unterminated key, a stray character); the caller then reads the file
// whole, as main does.
func pyTOML(s string) (string, bool) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var out strings.Builder
	var head []string
	for i := 0; i < len(s); {
		if c := s[i]; c == ' ' || c == '\t' || c == '\n' {
			i++
			continue
		} else if c == '#' {
			i += tomlEOL(s[i:])
			continue
		}
		start, inline, keys, ok := i, false, []string(nil), false
		if s[i] == '[' {
			arr := strings.HasPrefix(s[i:], "[[")
			if keys, i, ok = tomlKey(s, i+1+b2i(arr)); !ok || !strings.HasPrefix(s[i:], "]"+strings.Repeat("]", b2i(arr))) {
				return "", false
			}
			i, head = i+1+b2i(arr), keys
		} else {
			if keys, i, ok = tomlKey(s, i); !ok || i >= len(s) || s[i] != '=' {
				return "", false
			}
			if i, inline, ok = tomlValue(s, i+1, 0); !ok {
				return "", false
			}
			keys = append(append([]string(nil), head...), keys...)
		}
		i = tomlBlank(s, i, false)
		if i < len(s) && s[i] == '#' {
			i += tomlEOL(s[i:])
		}
		if i < len(s) && s[i] != '\n' {
			return "", false // a second statement on the line
		}
		st := s[start:i]
		named := false
		for _, k := range keys {
			named = named || strings.Contains(strings.ToLower(k), "pytest")
		}
		if named || inline && (strings.Contains(strings.ToLower(st), "pytest") || strings.Contains(st, `\`)) {
			out.WriteString(st + "\n")
		}
	}
	return out.String(), true
}

// tomlEOL: the length of s up to its first newline.
func tomlEOL(s string) int {
	if n := strings.IndexByte(s, '\n'); n >= 0 {
		return n
	}
	return len(s)
}

// tomlBlank skips spaces and tabs from i; with lines, newlines and
// comments too (inside an array or an inline table).
func tomlBlank(s string, i int, lines bool) int {
	for i < len(s) {
		switch c := s[i]; {
		case c == ' ' || c == '\t' || lines && c == '\n':
			i++
		case lines && c == '#':
			i += tomlEOL(s[i:])
		default:
			return i
		}
	}
	return i
}

// tomlKey reads a dotted key from i: bare parts, or quoted ones without an
// escape (an escape may spell pytest: false, and the file is read whole).
func tomlKey(s string, i int) ([]string, int, bool) {
	var keys []string
	for {
		i = tomlBlank(s, i, false)
		j := i
		switch {
		case i < len(s) && (s[i] == '"' || s[i] == '\''):
			n := strings.IndexAny(s[i+1:], string(s[i])+"\n\\")
			if n < 0 || s[i+1+n] != s[i] {
				return nil, i, false
			}
			keys, i = append(keys, s[i+1:i+1+n]), i+n+2
		default:
			for j < len(s) && (s[j] == '_' || s[j] == '-' || s[j] >= '0' && s[j] <= '9' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z') {
				j++
			}
			if j == i {
				return nil, i, false
			}
			keys, i = append(keys, s[i:j]), j
		}
		if i = tomlBlank(s, i, false); i >= len(s) || s[i] != '.' {
			return keys, i, true
		}
		i++
	}
}

// tomlValue reads one value from i; inline: it holds an inline table.
func tomlValue(s string, i, depth int) (int, bool, bool) {
	if i = tomlBlank(s, i, false); i >= len(s) || depth > maxNest {
		return i, false, false
	}
	switch c := s[i]; {
	case strings.HasPrefix(s[i:], `"""`) || strings.HasPrefix(s[i:], `'''`):
		q := s[i : i+3]
		for j := i + 3; j < len(s); j++ {
			if c == '"' && s[j] == '\\' {
				j++
			} else if strings.HasPrefix(s[j:], q) {
				j += 3
				for k := 0; k < 2 && j < len(s) && s[j] == c; k++ {
					j++ // up to two quotes before the closing three are content
				}
				return j, false, true
			}
		}
		return i, false, false
	case c == '"' || c == '\'':
		for j := i + 1; j < len(s) && s[j] != '\n'; j++ {
			if c == '"' && s[j] == '\\' {
				j++
			} else if s[j] == c {
				return j + 1, false, true
			}
		}
		return i, false, false
	case c == '[' || c == '{':
		inline, end := c == '{', map[byte]byte{'[': ']', '{': '}'}[c]
		for i++; ; {
			if i = tomlBlank(s, i, true); i < len(s) && s[i] == end {
				return i + 1, inline, true
			}
			if c == '{' {
				var ok bool
				if _, i, ok = tomlKey(s, i); !ok || i >= len(s) || s[i] != '=' {
					return i, false, false
				}
				i++
			}
			j, in, ok := tomlValue(s, i, depth+1)
			if !ok {
				return i, false, false
			}
			if i, inline = tomlBlank(s, j, true), inline || in; i < len(s) && s[i] == ',' {
				i++
			} else if i >= len(s) || s[i] != end {
				return i, false, false
			}
		}
	default: // a number, a boolean or a date: up to a separator
		j := i
		for j < len(s) && !strings.ContainsRune(",]}#\n", rune(s[j])) {
			j++
		}
		if strings.TrimRight(s[i:j], " \t") == "" {
			return i, false, false
		}
		return j, false, true
	}
}
