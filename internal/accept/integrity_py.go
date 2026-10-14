// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// The Python tokenizer (P14c-1, Opus #255 H5 and H8): the controller runs
// no Python, so the Python guard (P14c-2) reads Python source through this
// Go-side tokenizer. It assumes CPython 3.12 (its goldens' interpreter)
// and splits source as it does (PEP 701 f-strings included; a 3.14
// t-string is read as an f-string, which 3.12 does not compile). It
// refuses what it could read differently: source not UTF-8, a NUL, a
// coding cookie naming another encoding (PEP 263), a non-ASCII name
// (CPython folds names by NFKC), a line starting with a backslash
// continuation (CPython's cont_line_col), a newline, escaped or not, in a
// format spec of a single-quoted f-string, brackets nested past CPython's
// 200, f-strings nested past 64 (about 32 through format specs).
// Tokenizable is not compilable: it accepts some source CPython rejects
// (Opus #260 L3; a quote in a single-quoted spec, unlike a newline). Its
// work and recursion are bounded by the source, and it stops when its
// context is cancelled. A panic of its own is unverifiable too, and named.
// Until P14c-2, P14b's rule fails closed.

// ptok is a Python code token: a name ('n'), a string ('s'), a number
// ('1') or an operator ('o', one byte).
type ptok struct {
	kind byte
	text string
}

// pline is a logical line: its indentation (tabs to the next multiple of
// 8, a form feed resetting it), its tokens, and the code tokens in its
// f-strings' fields (inner; the guard reads them as code, P14c-2).
type pline struct {
	indent int
	toks   []ptok
	inner  []ptok
}

var (
	errLex      = errors.New("not tokenizable as Python")
	cookie      = regexp.MustCompile(`^[ \t\f]*#.*?coding[:=][ \t]*([-\w.]+)`)
	strPrefixes = strings.Fields("r u b br rb f fr rf t tr rt")
)

const (
	maxNest     = 64  // f-string field and format-spec descents (the scanner recurses on them)
	maxBrackets = 200 // CPython's MAXLEVEL: "too many nested parentheses"
)

// pyTokens is src's logical lines as CPython tokenizes them, or why the
// guard cannot read src as CPython does (unverifiable). Its error is only
// ctx's.
func pyTokens(ctx context.Context, src []byte) ([]pline, string, error) {
	if why := pyCookie(src); why != "" {
		return nil, why, nil
	}
	lines, err := pyLex(ctx, src)
	if cerr := ctx.Err(); cerr != nil {
		return nil, "", cerr
	} else if err != nil {
		return nil, err.Error(), nil
	}
	return lines, "", nil
}

// pyCookie: why CPython may decode src other than as UTF-8: it is not
// UTF-8, holds a NUL, or a PEP 263 cookie on line 1 or 2 names another
// encoding.
func pyCookie(src []byte) string {
	if !utf8.Valid(src) || slices.Contains(src, 0) {
		return "not UTF-8 source"
	}
	lines := strings.SplitN(pyNorm(src), "\n", 3)
	for _, l := range lines[:min(2, len(lines))] {
		if m := cookie.FindStringSubmatch(l); m != nil && !slices.Contains([]string{"utf-8", "utf8", "ascii", "us-ascii"}, strings.ReplaceAll(strings.ToLower(m[1]), "_", "-")) {
			return "a coding cookie other than utf-8 or ascii (" + m[1] + ")"
		}
	}
	return ""
}

// pyNorm is src as CPython's decoder hands it to the tokenizer: a BOM
// stripped, CRLF and a lone CR read as LF. The gate and the lexer share it
// (Mythos #260 L3).
func pyNorm(src []byte) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(string(src), "\ufeff"), "\r\n", "\n"), "\r", "\n")
}

func word(c byte) bool {
	return c == '_' || c >= 0x80 || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func digit(c byte) bool { return c >= '0' && c <= '9' }

// pyLex splits src into logical lines of code tokens: comments dropped; a
// string (any prefix, single or triple quoted, f- and t-strings with
// nested fields) one token; brackets and a trailing backslash joining
// physical lines. What it cannot tokenize is an error (the scanners panic
// with errLex, or ctx's error at a line end): it never guesses.
func pyLex(ctx context.Context, src []byte) (lines []pline, err error) {
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(error)
			if !ok || e != errLex && e != ctx.Err() { // a bug: still unverifiable, named
				e = fmt.Errorf("an internal tokenizer error: %v", r)
			}
			lines, err = nil, e
		}
	}()
	check := func() {
		if err := ctx.Err(); err != nil {
			panic(err)
		}
	}
	s := pyNorm(src) + "\n"
	cur, depth, start := pline{}, 0, true
	for i := 0; i < len(s); {
		if start {
			col := 0
			for ; strings.IndexByte(" \t\f", s[i]) >= 0; i++ {
				switch s[i] {
				case ' ':
					col++
				case '\t':
					col = col/8*8 + 8
				default:
					col = 0
				}
			}
			cur.indent, start = col, false
			need(s[i] != '\\') // CPython indents it by cont_line_col: fail closed
			continue
		}
		switch c := s[i]; {
		case c == '\n' && depth == 0:
			if i++; len(cur.toks) > 0 {
				lines = append(lines, cur)
			}
			cur, start = pline{}, true
			check()
		case strings.IndexByte(" \t\f\n", c) >= 0:
			if i++; c == '\n' {
				check()
			}
		case c == '#':
			i += eol(s[i:])
		case c == '\\':
			need(strings.HasPrefix(s[i:], "\\\n"))
			i += 2
			check()
		default:
			k, n := pyToken(s[i:], 0, &cur.inner)
			if t := s[i : i+n]; k == 'o' && strings.Contains("([{", t) {
				depth++
				need(depth <= maxBrackets)
			} else if k == 'o' && strings.Contains(")]}", t) {
				depth--
				need(depth >= 0)
			}
			cur.toks, i = append(cur.toks, ptok{k, s[i : i+n]}), i+n
		}
	}
	need(depth == 0)
	return lines, nil
}

func need(ok bool) {
	if !ok {
		panic(errLex)
	}
}

// eol is the length of s's first line.
func eol(s string) int {
	if n := strings.IndexByte(s, '\n'); n >= 0 {
		return n
	}
	return len(s)
}

// pyToken scans the token at the start of s: its kind and length. A name
// must be ASCII. Code in an f-string's fields goes to inner.
func pyToken(s string, depth int, inner *[]ptok) (byte, int) {
	switch c := s[0]; {
	case c == '"' || c == '\'':
		return 's', pyString(s, "", depth, inner)
	case digit(c) || c == '.' && len(s) > 1 && digit(s[1]):
		return '1', pyNumber(s)
	case word(c):
		n := 1
		for n < len(s) && word(s[n]) {
			n++
		}
		if pre := strings.ToLower(s[:n]); n < len(s) && (s[n] == '"' || s[n] == '\'') && slices.Contains(strPrefixes, pre) {
			return 's', n + pyString(s[n:], pre, depth, inner)
		}
		need(!strings.ContainsFunc(s[:n], func(r rune) bool { return r >= utf8.RuneSelf }))
		return 'n', n
	}
	return 'o', 1
}

// pyNumber is the length of the number at s: a hex, octal or binary body;
// else digits, a fraction, an exponent and j. A name after it is its own
// token, as CPython reads 1if and 0or.
func pyNumber(s string) int {
	n := 0
	if len(s) > 1 && s[0] == '0' && strings.IndexByte("xXoObB", s[1]) >= 0 {
		body := "0123456789_" // octal, binary: CPython ends them at a letter (0b1for)
		if s[1] == 'x' || s[1] == 'X' {
			body += "abcdefABCDEF"
		}
		for n = 2; n < len(s) && strings.IndexByte(body, s[n]) >= 0; n++ {
		}
		return n
	}
	digits := func() {
		for n < len(s) && (digit(s[n]) || s[n] == '_') {
			n++
		}
	}
	if digits(); n < len(s) && s[n] == '.' {
		n++
		digits()
	}
	if m := n + 1; n < len(s) && (s[n] == 'e' || s[n] == 'E') {
		if m < len(s) && (s[m] == '+' || s[m] == '-') {
			m++
		}
		if m < len(s) && digit(s[m]) {
			n = m
			digits()
		}
	}
	if n < len(s) && (s[n] == 'j' || s[n] == 'J') {
		n++
	}
	return n
}

// pyString scans a string literal at s (its quote), with its prefix: its
// length. In an f- or t-string, a {field} is code (pyField).
func pyString(s, prefix string, depth int, inner *[]ptok) int {
	need(depth <= maxNest)
	q := s[:1]
	if strings.HasPrefix(s, q+q+q) {
		q = s[:3]
	}
	raw, fstr := strings.Contains(prefix, "r"), strings.ContainsAny(prefix, "ft")
	for i := len(q); i < len(s); {
		switch c := s[i]; {
		case strings.HasPrefix(s[i:], q):
			return i + len(q)
		case c == '\\' && fstr && !raw && strings.HasPrefix(s[i:], `\N{`): // a named character, not a field
			j := strings.IndexByte(s[i:], '}')
			need(j > 0)
			i += j + 1
		case c == '\\' && !(fstr && strings.HasPrefix(s[i:], `\{`)):
			i += 2
		case fstr && (strings.HasPrefix(s[i:], "{{") || strings.HasPrefix(s[i:], "}}")):
			i += 2
		case fstr && c == '{':
			i += 1 + pyField(s[i+1:], depth+1, false, len(q) == 1, inner)
		default:
			need(c != '\n' || len(q) == 3)
			i++
		}
	}
	panic(errLex)
}

// pyField scans an f-string field after its '{', up to its '}': code,
// then a conversion (!r) and a format spec (spec), which is text that may
// hold nested fields. Every descent counts toward maxNest (Opus #260 H3).
// In a single-quoted f-string (single), a newline in a spec sends CPython
// back to code mode: refused, escaped or not (Mythos #260 H1 and r2 L1;
// a D55 false block where CPython 3.12 reads an escaped one).
func pyField(s string, depth int, spec, single bool, inner *[]ptok) int {
	need(depth <= maxNest)
	nest := 0
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case spec && c == '{':
			i += 1 + pyField(s[i+1:], depth+1, false, single, inner)
		case spec && c == '\n':
			need(!single)
			i++
		case c == '}' && nest == 0:
			return i + 1
		case spec || strings.IndexByte(" \t\n\\", c) >= 0:
			i++
		case c == '#':
			i += eol(s[i:])
		case c == ':' && nest == 0:
			return i + 1 + pyField(s[i+1:], depth+1, true, single, inner)
		case strings.IndexByte("([{", c) >= 0:
			nest, i, *inner = nest+1, i+1, append(*inner, ptok{'o', s[i : i+1]})
		case strings.IndexByte(")]}", c) >= 0:
			nest, i, *inner = nest-1, i+1, append(*inner, ptok{'o', s[i : i+1]})
			need(nest >= 0)
		default:
			k, n := pyToken(s[i:], depth, inner)
			i, *inner = i+n, append(*inner, ptok{k, s[i : i+n]})
		}
	}
	panic(errLex)
}
