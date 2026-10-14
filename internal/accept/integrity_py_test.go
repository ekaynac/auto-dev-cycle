// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The goldens in testdata/pytoken are CPython 3.12.3's own tokens of each
// .pysrc, which compiles under it:
//
//	python3 -I -m tokenize x.pysrc > x.tok
//
// except cr.tok: the importer reads a lone CR as a line end, the tokenize
// module's byte reader does not, so cr.pysrc is decoded first:
//
//	python3 -I -c 'import sys; sys.stdout.write(open(sys.argv[1], encoding="utf-8").read())' cr.pysrc | python3 -I -m tokenize > cr.tok
//
// TestPyTokensDifferential, where python3 is 3.12, checks that every
// .pysrc compiles and its .tok is reproduced. The other tests need no
// Python.
var tokLine = regexp.MustCompile(`^(\d+),(\d+)-(\d+),(\d+):\s+([A-Z_]+)\s`)

// pyHelper reads a JSON list of sources on stdin and prints, per source,
// "#CASE" and either "#SKIP" (it does not compile) or its tokens as
// python -m tokenize prints them.
const pyHelper = `import io, json, sys, tokenize, warnings
warnings.simplefilter("ignore")
for src in json.load(sys.stdin):
    print("#CASE")
    try:
        compile(src, "case", "exec")
        toks = list(tokenize.generate_tokens(io.StringIO(src).readline))
    except Exception:
        print("#SKIP")
        continue
    for t in toks:
        print("%d,%d-%d,%d:\t%s\t%r" % (t.start + t.end + (tokenize.tok_name[t.type], t.string)))
`

// golden is CPython's tokens of src (its tokenize output tok) in pyLex's
// form: logical lines of names, strings (an f-string whole), numbers and
// one-byte operators, with each line's indentation level. A line that is
// not a token, an ERRORTOKEN or a type it does not know fails the test.
func golden(t *testing.T, src, tok string) (levels []int, lines [][]ptok) {
	t.Helper()
	s := strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(src, "\ufeff"), "\r\n", "\n"), "\r", "\n")
	rows := strings.SplitAfter(s, "\n")
	off := func(row, col string) int { // a 1-based row and a column in characters, as a byte offset
		r, _ := strconv.Atoi(row)
		c, _ := strconv.Atoi(col)
		if r < 1 || r > len(rows) { // ENCODING, ENDMARKER
			return 0
		}
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
		if m == nil && strings.TrimSpace(l) != "" || m != nil && m[5] == "ERRORTOKEN" {
			t.Fatalf("not a token: %q", l)
		} else if m == nil {
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
		case typ == "ENCODING", typ == "ENDMARKER", typ == "NL", typ == "COMMENT":
		default:
			t.Fatalf("an unknown token type: %q", l)
		}
	}
	return levels, lines
}

// pyCompare is where pyTokens' reading of src differs from CPython's
// tokens tok, or "".
func pyDiff(t *testing.T, src, tok string) string {
	t.Helper()
	levels, want := golden(t, src, tok)
	got, why, err := pyTokens(context.Background(), []byte(src))
	switch {
	case err != nil || why != "":
		return fmt.Sprintf("refused: %q %v", why, err)
	case len(got) != len(want):
		return fmt.Sprintf("%d logical lines, CPython %d", len(got), len(want))
	case !slices.Equal(levelsOf(got), levels):
		return fmt.Sprintf("indentation levels %v, CPython %v", levelsOf(got), levels)
	}
	for i := range got {
		if g, w := ptexts(got[i].toks), ptexts(want[i]); g != w {
			return fmt.Sprintf("line %d:\n got  %s\n want %s", i+1, g, w)
		}
	}
	return ""
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
// numbers before names (1if, 1or; 0b1for, 0o1and: Opus #260 H2), cookies
// on lines 1 to 3, a BOM, CRLF and a lone CR, and the review's clean
// inputs (U+2028 and U+0085 in strings and comments, a form feed inside
// an indentation, brackets in comments inside brackets).
func TestPyTokensMatchCPython(t *testing.T) {
	srcs := goldens(t)
	for _, p := range srcs {
		src, err := os.ReadFile(p)
		tok, err2 := os.ReadFile(strings.TrimSuffix(p, ".pysrc") + ".tok")
		if err != nil || err2 != nil {
			t.Fatal(err, err2)
		}
		if d := pyDiff(t, string(src), string(tok)); d != "" {
			t.Errorf("%s: %s", p, d)
		}
	}
}

func goldens(t *testing.T) []string {
	srcs, err := filepath.Glob(filepath.Join("testdata", "pytoken", "*.pysrc"))
	if err != nil || len(srcs) < 12 {
		t.Fatalf("goldens: %v %v", srcs, err)
	}
	return srcs
}

// Opus #260 M1, M2 (dev-only: where python3 is CPython 3.12): every golden
// compiles and its .tok is reproduced (ADC_PYTOKEN_REGEN=1 rewrites it);
// and pyTokens reads every generated input that compiles as CPython does,
// or refuses it: string prefixes and bodies, number and keyword joins,
// backslashes in and after indentation, nested f-string specs.
func TestPyTokensDifferential(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	} else if v, err := exec.Command(py, "-I", "-c", "import sys; print(sys.version_info[:2] == (3, 12))").Output(); err != nil || strings.TrimSpace(string(v)) != "True" {
		t.Skip("python3 is not CPython 3.12")
	}
	for _, p := range goldens(t) {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(py, "-I", "-c", "import sys; compile(sys.stdin.buffer.read(), sys.argv[1], 'exec')", p)
		if cmd.Stdin = bytes.NewReader(src); cmd.Run() != nil {
			t.Errorf("%s does not compile", p)
			continue
		}
		cmd = exec.Command(py, "-I", "-m", "tokenize", p)
		if filepath.Base(p) == "cr.pysrc" { // the importer's universal newlines: decoded first
			dec, err := exec.Command(py, "-I", "-c", "import sys; sys.stdout.write(open(sys.argv[1], encoding='utf-8').read())", p).Output()
			if err != nil {
				t.Fatal(err)
			}
			cmd = exec.Command(py, "-I", "-m", "tokenize")
			cmd.Stdin = bytes.NewReader(dec)
		}
		tok, err := cmd.Output()
		want := strings.TrimSuffix(p, ".pysrc") + ".tok"
		if err != nil {
			t.Fatal(err)
		} else if old, err := os.ReadFile(want); err != nil || string(tok) != string(old) {
			t.Errorf("%s: not CPython's tokens now", want)
		}
	}
	srcs := pyGenerated()
	in, err := json.Marshal(srcs)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, "-I", "-c", pyHelper)
	cmd.Stdin = strings.NewReader(string(in))
	out, err := cmd.Output()
	cases := strings.Split(string(out), "#CASE\n")[1:]
	if err != nil || len(cases) != len(srcs) {
		t.Fatalf("python3: %v, %d of %d cases", err, len(cases), len(srcs))
	}
	compared, refused := 0, 0
	for i, c := range cases {
		if strings.HasPrefix(c, "#SKIP") {
			continue
		}
		triple := strings.Contains(srcs[i], "'''") || strings.Contains(srcs[i], `"""`)
		switch d := pyDiff(t, srcs[i], c); {
		case strings.Contains(srcs[i], ":d\n") && !triple && !strings.HasPrefix(d, "refused"):
			t.Errorf("%q: a newline in a single-quoted spec read (%s), want refused", srcs[i], d)
		case strings.HasPrefix(d, "refused"):
			refused++
		case d != "":
			t.Errorf("%q: %s", srcs[i], d)
		default:
			compared++
		}
	}
	t.Logf("%d generated inputs: %d read as CPython does, %d refused, the rest do not compile", len(srcs), compared, refused)
	if compared < 500 {
		t.Errorf("only %d inputs compared", compared)
	}
}

// pyGenerated are the differential's inputs (Opus #260 M2's list).
func pyGenerated() (srcs []string) {
	for _, n := range []string{"0", "1", "0x1f", "0XaB", "0xE", "0o17", "0O7", "0b1", "0B0", "0b0", "0o1", "1.5", "1.", ".5", "1e5", "1E-5", "1_0", "1j", "1.5j", "0b_1", "0x_f"} {
		for _, f := range []string{"x = %sif 1 else 2\n", "x = %sor 1\n", "x = %sand 1\n", "x = [%sfor i in ()]\n", "x = %sin ()\n", "x = %snot in ()\n", "x = %sis 1\n", "assert f() if %selse True\n"} {
			srcs = append(srcs, fmt.Sprintf(f, n))
		}
	}
	for _, p := range []string{"", "r", "u", "R", "U", "b", "B", "br", "Rb", "bR", "f", "F", "fr", "Rf", "rF", "fR"} {
		for _, q := range []string{"'", `"`, "'''", `"""`} {
			for _, b := range []string{"a", `\\`, `\'`, `\"`, "{x}", "{{x}}", `\N{BULLET}`, "# h", "{x!r:>{w}}", "{x:{w}.{p}}", "{x=}", `{'a' if x else "b"}`, "a\\\nb", `\{x}`, "{x:{'>'}{10}}", "{ {'a': 1}['a'] }", "{x:#x}", "{x:d\n}", "{x:d\n#}\n}", `{x:{"}"}}`} {
				srcs = append(srcs, "x = "+p+q+b+q+"\n")
			}
		}
	}
	for _, s := range []string{"f'{a:{b:{c}}}'", "f'{a:{b}{c}}'", "f'{a!r:{b!s:{c}}}'", "f'{a:{f\"{b:{c}}\"}}'", `f"{a:{'{'}}}"`, "f'{a:=^{w}}'", "f'{a:{w}{\"x\"}}'"} {
		srcs = append(srcs, "x = "+s+"\n")
	}
	return append(srcs,
		"if x:\n    y = 1\n\\\n    z = 2\n", "if x:\n  \f\\\n  y = 1\n", "if x:\n    \\\n    y = 1\n", "\\\nx = 1\n",
		"x = 1 + \\\n    2\n", "if x and \\\n        y:\n    pass\n", "def f():\n    return 1 + \\\n2\n", "x = (1 +\\\n 2)\n",
		"if x:\n\ty = 1\n\tif y:\n\t\tz = 2\n", "if x:\n\f    y = 1\n", "if x:\n  \f  y = 1\n  z = 2\n", "if x:\n        y = 1\n\tz = 2\n")
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
	specDeep := func(n int) string { return "x = f'" + strings.Repeat("{a:", n) + "1" + strings.Repeat("}", n) + "'\n" }
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
		fdeep(maxNest + 1):        "not tokenizable",
		specDeep(maxNest/2 + 1):   "not tokenizable",
		// Mythos #260 H1: a newline in a single-quoted f-string's spec sends
		// CPython back to code mode (here, test_zero rebound after a comment
		// the spec would hide): refused, a D55 false block. Triple quotes read.
		"x = 1\ndef test_zero():\n    assert 1 == 2\ny = [f\"{x:d\n#}\"\n}\"]; test_zero = None  # \"\n": "not tokenizable",
		"x = 1\ny = f\"{x:d\n}\"\n":        "not tokenizable",
		"\ufeff# coding: latin-1\nx = 1\n": "coding cookie other than utf-8 or ascii (latin-1)", // Mythos #260 L1
		"x = 1\ndef test_zero():\n    assert 1 == 2\ny = [f\"\"\"{f\"{x:d\n#}\"\n}\"}\"\"\"]; test_zero = None  # \"\"\"\n": "not tokenizable",
		// Opus #260 H1: CPython nests a line after a backslash at column 0
		// (cont_line_col) where a reading by column would not: refused, as any
		// line starting with a backslash continuation (a D55 false block).
		"if False:\n    pass\n\\\n    def test_zero():\n        assert 1 == 2\n":     "not tokenizable",
		"if False:\n    pass\n  \f\\\n    def test_zero():\n        assert 1 == 2\n": "not tokenizable",
		"if x:\n    \\\n    y = 1\n": "not tokenizable",
	} {
		if _, why, err := pyTokens(context.Background(), []byte(src)); err != nil || !strings.Contains(why, want) {
			t.Errorf("%q: %q %v, want %q", src, why, err, want)
		}
	}
	for _, src := range []string{"# -*- coding: UTF_8 -*-\nx = 1\n", "# coding=ascii\n", "x = 'caf\u00e9'  # \u00e9\n", deep(200), fdeep(maxNest), specDeep(maxNest / 2), "x = 1\ny = 2\n# coding: latin-1\n", "x = 1 + \\\n    2\n",
		"x = 1\ny = f\"\"\"{x:d\n}\"\"\"\n", "\ufeff# coding: utf-8\nx = 1\n"} {
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
		var took [2]time.Duration // the median of three runs (Mythos #260 L4)
		for i, size := range []int{1 << 20, 4 << 20} {
			src, runs := []byte(gen(size)), []time.Duration{}
			for range 3 {
				start := time.Now()
				if _, _, err := pyTokens(ctx, src); err != nil {
					t.Fatalf("%s, %d bytes: %v (not linear)", name, size, err)
				}
				runs = append(runs, time.Since(start))
			}
			slices.Sort(runs)
			took[i] = runs[1]
		}
		cancel()
		if took[1] > 8*took[0]+500*time.Millisecond {
			t.Errorf("%s: 1 MiB in %v, 4 MiB in %v: not linear", name, took[0], took[1])
		}
	}
	// Opus #260 H3: f-string format specs nested 2M deep at the 4 MiB blob
	// bound, unterminated, are refused at maxNest, at once: before the scan
	// reaches the end, so no slower than a flat unterminated f-string of the
	// same size, which must be read to its end (about 5 ms against 25 ms;
	// without the bound, about 440 ms). Relative, so a loaded runner slows
	// both sides (a fixed 100 ms bound flaked in CI on #261).
	refused := func(src []byte) time.Duration {
		runs := []time.Duration{}
		for range 3 {
			start := time.Now()
			if _, why, err := pyTokens(context.Background(), src); err != nil || why == "" {
				t.Fatalf("%.12q…: %q %v, want refused", src, why, err)
			}
			runs = append(runs, time.Since(start))
		}
		slices.Sort(runs)
		return runs[1]
	}
	flat := refused([]byte("x = f'" + strings.Repeat("ab", 2<<20-4)))
	if nested := refused([]byte("x = f'" + strings.Repeat("{:", 2<<20-4))); nested > flat {
		t.Errorf("nested specs refused in %v, a flat f-string of the same size in %v: not at once", nested, flat)
	}
	src := []byte(strings.Repeat("def test_a():\n    assert 1\n", 4<<20/27))
	ctx := &countCtx{Context: context.Background()}
	ctx.n.Store(100)
	if _, _, err := pyTokens(ctx, src); !errors.Is(err, context.Canceled) || ctx.calls.Load() > 102 {
		t.Errorf("cancelled: %v after %d checks", err, ctx.calls.Load())
	}
}

// panicCtx panics with a value that is not an error: a bug's panic.
type panicCtx struct{ context.Context }

func (panicCtx) Err() error { panic("boom") }

// Mythos #260 L2: a panic that is neither errLex nor ctx's error is still
// an error (unverifiable), named, and never re-panics in the recover.
func TestPyLexInternalPanic(t *testing.T) {
	if _, err := pyLex(panicCtx{context.Background()}, []byte("x\n")); err == nil || !strings.Contains(err.Error(), "internal tokenizer error: boom") {
		t.Errorf("an internal panic: %v", err)
	}
}
