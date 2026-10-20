// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pyRealTOML are three real pyproject.toml files without a pytest table, the
// widen-b baseline's false-block reader (Opus #296 M1): networkx 3.3
// (7fdddfa2ca, BSD-3-Clause) and setuptools 72.1.0 (441799f8b4, MIT), whose
// classifier "5 - Production/Stable" main read as the option -, and arrow
// 17.0.0's python/ (6a2e19a852, Apache-2.0), whose setuptools-scm "git
// describe --dirty" main read as the option --dirty. Each is its project's
// file verbatim, under its project's licence: .txt, so that no SPDX header
// of this repository's is added to it.
var pyRealTOML = []string{"networkx-3.3.pyproject.txt", "setuptools-72.1.0.pyproject.txt", "arrow-17.0.0.pyproject.txt"}

func pyRealFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "pyconf", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// pyTOMLForges: pytest options in every TOML form pytest reads, each to be
// read; "<" prepends (a root key), otherwise appended.
var pyTOMLForges = map[string]string{
	"the ini_options table":                "[tool.pytest.ini_options]\naddopts = \"-p forge\"\n",
	"pytest 9's native table":              "[tool.pytest]\naddopts = [\"-p\", \"forge\"]\n",
	"a root dotted key":                    "<tool.pytest.ini_options.addopts = \"-p forge\"\n",
	"a dotted key under [tool]":            "[tool]\npytest.ini_options.addopts = \"-p forge\"\n",
	"a root inline table":                  "<tool = { pytest = { ini_options = { addopts = \"-p forge\" } } }\n",
	"quoted parts and blanks":              "[ tool . \"pytest\" . 'ini_options' ]\naddopts = \"-p forge\"\n",
	"an escaped table name":                "[tool.\"\\u0070ytest\".ini_options]\naddopts = \"-p forge\"\n",
	"an escaped inline key":                "<tool = { \"\\u0070ytest\" = { ini_options = { addopts = \"-p forge\" } } }\n",
	"after a literal string ending in \\":  "x = 'C:\\'\n[tool.pytest.ini_options]\naddopts = \"-p forge\"\n",
	"after an escaped quote":               "x = \"a\\\"[b]\"\n[tool.pytest.ini_options]\naddopts = \"-p forge\"\n",
	"past a header in a multi-line string": "[tool.pytest.ini_options]\nmarkers = \"\"\"\n[tool.other]\n\"\"\"\naddopts = \"-p forge\"\n",
	"an array of tables":                   "[[tool.pytest.ini_options.x]]\naddopts = \"-p forge\"\n",
	"comments after the header and value":  "[tool.pytest.ini_options] # x\naddopts = \"-p forge\" # y\n",
	"CRLF lines":                           "[tool.pytest.ini_options]\r\naddopts = \"-p forge\"\r\n",
	"a multi-line array with a comment":    "[tool.pytest.ini_options]\naddopts = [\n  \"-p\", # c\n  \"forge\",\n]\n",
	"a collection setting":                 "[tool.pytest.ini_options]\ntestpaths = [\"tests\"]\n",
	"a stray character after a header":     "[tool.black] x\n[tool.pytest.ini_options]\naddopts = \"-p forge\"\n",
}

func pyForge(real, forge string) string {
	if f, ok := strings.CutPrefix(forge, "<"); ok {
		return f + real
	}
	return real + "\n" + forge
}

// TestPyConfTOML: of a TOML file pyConf reads only what pytest may (the
// widen-b baseline's false blocks): nothing of the three real files, each
// forged option in every form.
func TestPyConfTOML(t *testing.T) {
	for _, name := range pyRealTOML {
		if c := pyConf("pyproject.toml", []byte(pyRealFile(t, name)), true); c != "" {
			t.Errorf("%s: read %q, want nothing", name, c)
		}
	}
	real := pyRealFile(t, pyRealTOML[0])
	for name, forge := range pyTOMLForges {
		if c := pyConf("pyproject.toml", []byte(pyForge(real, forge)), true); !strings.Contains(c, "forge") && !strings.Contains(c, "testpaths") {
			t.Errorf("%s: read %q, want the option", name, c)
		}
	}
	for name, c := range map[string]struct{ in, want string }{
		"a pytest11 entry point":             {"[project.entry-points.pytest11]\nforge = \"forge_mod\"\n", "forge = \"forge_mod\""},
		"an inline pytest11 entry point":     {"[project]\nentry-points = { pytest11 = { forge = \"forge_mod\" } }\n", "pytest11"},
		"a dependency string is no key":      {"[project]\ndependencies = [\"pytest-forge\"]\n", ""},
		"an unterminated string":             {"x = \"pytest\n", "x = \"pytest\n"},
		"another tool's option text":         {"[tool.x]\nargs = \"-p pytest\"\n", ""},
		"an inline table naming pytest":      {"[tool.x]\nargs = { a = \"pytest\" }\n", "args"},
		"an inline table with an escape":     {"[tool.x]\nargs = { a = \"\\u0070\" }\n", "args"},
		"two statements on one line":         {"x = 1 [tool.pytest.ini_options]\n", "x = 1 [tool.pytest.ini_options]\n"},
		"a key with no value":                {"[tool.pytest.ini_options]\naddopts =\n", "[tool.pytest.ini_options]\naddopts =\n"},
		"over the nesting bound, read whole": {"x = " + strings.Repeat("[", maxNest+2) + strings.Repeat("]", maxNest+2) + "\n# pytest\n", "# pytest"},
		// Free text in each string form is no option (a lexing slip would read the file whole: "-" an option).
		"a multi-line string":                  {"[project]\ndescription = \"\"\"\nA pytest plugin - fast\n\"\"\"\n", ""},
		"an escaped quote in a string":         {"[project]\ndescription = \"say \\\"pytest - fast\\\"\"\n", ""},
		"an escaped quote in a multi-line one": {"[project]\ndescription = \"\"\"a \\\"\"\" pytest - b\"\"\"\n", ""},
		"a literal string ending in \\":        {"[tool.x]\npath = 'C:\\'\nnote = \"pytest - x\"\n", ""},
		"a statement after a header, whole":    {"[tool.x] y = 1\n# pytest\n", "y = 1"},
		"a key with no value elsewhere, whole": {"[tool.x]\nk =\n# pytest\n", "k ="},
	} {
		if got := pyConf("pyproject.toml", []byte(c.in), true); c.want == "" && got != "" || !strings.Contains(got, c.want) {
			t.Errorf("%s: read %q, want %q", name, got, c.want)
		}
	}
}

// TestPyGuardConfTOML, end to end (tests/pyproject.toml: the root one is a
// protected path): a base holding a real file is read and a change outside
// pytest's tables passes, with no block of the guard's own; each forged option in a base is
// unread, and added by a change blocks.
func TestPyGuardConfTOML(t *testing.T) {
	for _, name := range pyRealTOML {
		real := pyRealFile(t, name)
		f := newFixtureWith(t, func(src string) {
			write(t, src, map[string]string{"tests/pyproject.toml": real})
			plain(t, src, "add", "--all")
		})
		if v := f.guarded(t, map[string]string{"money.py": baseFiles["money.py"] + "\n"}); finds(v.Blocks, "moves collection off its defaults") {
			t.Errorf("%s in the base: %q, want read", name, whats(v.Blocks))
		}
		if v := f.guarded(t, map[string]string{"tests/pyproject.toml": real + "\n[tool.black]\nline-length = 100\n"}); len(pyOwn(v.Blocks)) > 0 {
			t.Errorf("%s changed outside pytest's tables: %q, want no block of the guard's own", name, whats(pyOwn(v.Blocks)))
		}
	}
	real := pyRealFile(t, pyRealTOML[0])
	f := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"tests/pyproject.toml": real})
		plain(t, src, "add", "--all")
	})
	for name, forge := range pyTOMLForges {
		g := newFixtureWith(t, func(src string) {
			write(t, src, map[string]string{"tests/pyproject.toml": pyForge(real, forge)})
			plain(t, src, "add", "--all")
		})
		if v := g.guarded(t, map[string]string{"money.py": baseFiles["money.py"] + "\n"}); !finds(v.Blocks, "moves collection off its defaults") {
			t.Errorf("%s in the base: %q, want unread", name, whats(v.Blocks))
		}
		if v := f.guarded(t, map[string]string{"tests/pyproject.toml": pyForge(real, forge)}); !pyFinds(v.Blocks, "tests/pyproject.toml: a pytest configuration changed") {
			t.Errorf("%s added: %q, want blocked", name, whats(v.Blocks))
		}
	}
	if v := f.guarded(t, map[string]string{"tests/pyproject.toml": real + "\n[project.entry-points.pytest11]\nforge = \"forge_mod\"\n"}); !pyFinds(v.Blocks, "tests/pyproject.toml: a pytest configuration changed") {
		t.Errorf("a pytest11 entry point added: %q, want blocked", whats(v.Blocks))
	}
}
