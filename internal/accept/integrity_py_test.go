// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The goldens in testdata/pytoken are CPython 3.12.3's own tokens of each
// .pysrc, made once with
//
//	for f in testdata/pytoken/*.pysrc; do python3 -I -m tokenize "$f" > "${f%.pysrc}.tok"; done
//
// except cr.tok: the importer reads a lone CR as a line end, the tokenize
// module's byte reader does not, so cr.pysrc is decoded first:
//
//	python3 -I -c 'import sys; sys.stdout.write(open(sys.argv[1], encoding="utf-8").read())' cr.pysrc | python3 -I -m tokenize > cr.tok
//
// Every .pysrc compiles under that CPython. The test needs no Python.
var tokLine = regexp.MustCompile(`^(\d+),(\d+)-(\d+),(\d+):\s+([A-Z_]+)\s`)

// golden is CPython's tokens of src (its tokenize output tok) in pyLex's
// form: logical lines of names, strings (an f-string whole), numbers and
// one-byte operators, with each line's indentation level.
func golden(t *testing.T, src, tok string) (levels []int, lines [][]ptok) {
	t.Helper()
	s := strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(src, "\ufeff"), "\r\n", "\n"), "\r", "\n")
	rows := strings.SplitAfter(s, "\n")
	off := func(row, col string) int { // a 1-based row and a column in characters, as a byte offset
		r, _ := strconv.Atoi(row)
		c, _ := strconv.Atoi(col)
		n := 0
		for _, l := range rows[:r-1] {
			n += len(l)
		}
		return n + len(string([]rune(rows[r-1])[:c]))
	}
	var cur []ptok
	level, at, fdepth, fstart := 0, 0, 0, 0
	for _, l := range strings.Split(tok, "\n") {
		m := tokLine.FindStringSubmatch(l)
		if m == nil || m[5] == "ENCODING" || m[5] == "ENDMARKER" {
			continue
		}
		from, to := off(m[1], m[2]), off(m[3], m[4])
		if len(cur) == 0 {
			at = level
		}
		switch typ := m[5]; {
		case typ == "FSTRING_START":
			if fdepth++; fdepth == 1 {
				fstart = from
			}
		case typ == "FSTRING_END":
			if fdepth--; fdepth == 0 {
				cur = append(cur, ptok{'s', s[fstart:to]})
			}
		case fdepth > 0: // inside an f-string
		case typ == "INDENT":
			level++
		case typ == "DEDENT":
			level--
		case typ == "NEWLINE":
			if len(cur) > 0 {
				lines, levels, cur = append(lines, cur), append(levels, at), nil
			}
		case typ == "NAME", typ == "NUMBER", typ == "STRING":
			cur = append(cur, ptok{map[string]byte{"NAME": 'n', "NUMBER": '1', "STRING": 's'}[typ], s[from:to]})
		case typ == "OP":
			for i := from; i < to; i++ {
				cur = append(cur, ptok{'o', s[i : i+1]})
			}
		}
	}
	return levels, lines
}

// levelsOf turns pyLex's indentation columns into nesting levels.
func levelsOf(lines []pline) (levels []int) {
	stack := []int{0}
	for _, l := range lines {
		for l.indent < stack[len(stack)-1] {
			stack = stack[:len(stack)-1]
		}
		if l.indent > stack[len(stack)-1] {
			stack = append(stack, l.indent)
		}
		levels = append(levels, len(stack)-1)
	}
	return levels
}

// F (P14c-1, Opus #255 H5): the tokenizer reads each golden as CPython
// does: strings (prefixed, raw, bytes, triple-quoted, PEP 701 f-strings
// nested and reusing quotes, comments in their fields), comments,
// backslash and bracket joining, tabs and form feeds in indentation,
// numbers before names (1if, 1or), cookies on lines 1 to 3, a BOM, CRLF
// and a lone CR.
func TestPyTokensMatchCPython(t *testing.T) {
	srcs, err := filepath.Glob(filepath.Join("testdata", "pytoken", "*.pysrc"))
	if err != nil || len(srcs) < 10 {
		t.Fatalf("goldens: %v %v", srcs, err)
	}
	for _, p := range srcs {
		src, err := os.ReadFile(p)
		tok, err2 := os.ReadFile(strings.TrimSuffix(p, ".pysrc") + ".tok")
		if err != nil || err2 != nil {
			t.Fatal(err, err2)
		}
		levels, want := golden(t, string(src), string(tok))
		got, why, err := pyTokens(context.Background(), src)
		if err != nil || why != "" {
			t.Errorf("%s: %q %v", p, why, err)
			continue
		}
		if len(got) != len(want) {
			t.Errorf("%s: %d logical lines, CPython %d", p, len(got), len(want))
		}
		for i := range min(len(got), len(want)) {
			if g, w := ptexts(got[i].toks), ptexts(want[i]); g != w || len(got[i].toks) != len(want[i]) {
				t.Errorf("%s line %d:\n got  %s\n want %s", p, i+1, g, w)
			}
		}
		if g := levelsOf(got); !slices.Equal(g, levels) {
			t.Errorf("%s: indentation levels %v, CPython %v", p, g, levels)
		}
	}
}

func ptexts(t []ptok) string {
	var b strings.Builder
	for _, k := range t {
		b.WriteString(string(k.kind) + ":" + k.text + " ")
	}
	return b.String()
}

// F (P14c-1, Opus #255 H5): what CPython may read differently from the
// guard, or not at all, is refused: a coding cookie naming another
// encoding (H5's utf-7 test hidden in a string), invalid UTF-8, a NUL, a
// non-ASCII name (CPython folds ｔest_zero to test_zero), an unterminated
// string or field, unbalanced or too deeply nested brackets, a stray
// backslash. Cookies naming utf-8 or ascii, and non-ASCII text in strings
// and comments, are read.
func TestPyTokensRefuses(t *testing.T) {
	deep := func(n int) string { return "x = " + strings.Repeat("(", n) + "1" + strings.Repeat(")", n) + "\n" }
	fdeep := func(n int) string { return "x = " + strings.Repeat("f'{", n) + "1" + strings.Repeat("}'", n) + "\n" }
	for src, want := range map[string]string{
		"# coding: utf-7\nX = +ACIAIgAi-\ndef test_zero():\n    assert 1\n+ACIAIgAi-\n": "coding cookie other than utf-8 or ascii (utf-7)",
		"#!/usr/bin/env python3\n# -*- coding: latin-1 -*-\nx = 1\n":                    "coding cookie other than utf-8 or ascii (latin-1)",
		"#!/usr/bin/env python3\r# vim: fileencoding=cp1252\rx = 1\r":                   "coding cookie",
		"x = '\xff'\n":            "not UTF-8 source",
		"x = 1\x00\n":             "not UTF-8 source",
		"caf\u00e9 = 1\n":         "not tokenizable",
		"\uff54est_zero = None\n": "not tokenizable",
		"x = 'abc\n":              "not tokenizable",
		"x = '''abc\n":            "not tokenizable",
		"x = f'{abc'\n":           "not tokenizable",
		"x = f'{abc:{d}'\n":       "not tokenizable",
		"x = (1,\n":               "not tokenizable",
		"x = )\n":                 "not tokenizable",
		"x = 1 \\ + 2\n":          "not tokenizable",
		deep(201):                 "not tokenizable",
		fdeep(maxNest + 2):        "not tokenizable",
	} {
		if _, why, err := pyTokens(context.Background(), []byte(src)); err != nil || !strings.Contains(why, want) {
			t.Errorf("%q: %q %v, want %q", src, why, err, want)
		}
	}
	for _, src := range []string{"# -*- coding: UTF_8 -*-\nx = 1\n", "# coding=ascii\n", "x = 'caf\u00e9'  # \u00e9\n", deep(200), fdeep(maxNest + 1), "x = 1\ny = 2\n# coding: latin-1\n"} {
		if _, why, err := pyTokens(context.Background(), []byte(src)); err != nil || why != "" {
			t.Errorf("%q: %q %v, want read", src, why, err)
		}
	}
}

// F (P14c-1, Opus #255 H8): about 4 MiB of hostile input tokenizes in
// time linear in its size, and a cancelled context stops it within a
// line, returning ctx's error.
func TestPyTokensBounded(t *testing.T) {
	for name, gen := range map[string]func(n int) string{
		"an unterminated triple-quoted string": func(n int) string { return "x = '''" + strings.Repeat("ab\n", n/3) },
		"an unterminated f-string field":       func(n int) string { return "x = f'{" + strings.Repeat("a + ", n/4) },
		"deep brackets": func(n int) string {
			return strings.Repeat("x = "+strings.Repeat("([{", 66)+strings.Repeat("}])", 66)+"\n", n/400)
		},
		"a long continuation chain":       func(n int) string { return "x = 1" + strings.Repeat(" + \\\n 1", n/7) + "\n" },
		"one logical line of many tokens": func(n int) string { return "x = [\n" + strings.Repeat("1,\n", n/3) + "]\n" },
		"nested f-strings":                func(n int) string { return strings.Repeat(`x = f"{f'{f"{y!r:>{w}}"}'}"`+"\n", n/28) },
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		var took [2]time.Duration
		for i, size := range []int{1 << 20, 4 << 20} {
			start := time.Now()
			if _, _, err := pyTokens(ctx, []byte(gen(size))); err != nil {
				t.Fatalf("%s, %d bytes: %v (not linear)", name, size, err)
			}
			took[i] = time.Since(start)
		}
		cancel()
		if took[1] > 8*took[0]+250*time.Millisecond {
			t.Errorf("%s: 1 MiB in %v, 4 MiB in %v: not linear", name, took[0], took[1])
		}
	}
	src := []byte(strings.Repeat("def test_a():\n    assert 1\n", 4<<20/27))
	ctx := &countCtx{Context: context.Background()}
	ctx.n.Store(100)
	if _, _, err := pyTokens(ctx, src); !errors.Is(err, context.Canceled) || ctx.calls.Load() > 102 {
		t.Errorf("cancelled: %v after %d checks", err, ctx.calls.Load())
	}
}
