// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"fmt"
	"maps"
	"strings"
	"testing"
)

// TestPyGuardWidenB2: P14c-2a-widen-b PR 2, the stricter fixes. S1 main's
// base index is charged; S2 pyPure is never vacuous; S3 names that differ
// only by case collide (on every host: a stated false block); S4 an
// f-string's debug = binds nothing; S5 main's allowlist reads bare type
// parameters, type aliases and concatenated docstrings (Mythos #266 L6).
func TestPyGuardWidenB2(t *testing.T) {
	base := func(extra map[string]string) map[string]string {
		m := map[string]string{"money.py": "def add(a, b):\n    return a + b\n", "cfg.py": "SKIP = False\n", "feature/__init__.py": "", "feature/flags.py": "from cfg import SKIP\n",
			"tests/test_money.py": "import pytest\nimport feature.flags\nfrom money import add\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_add():\n    assert add(1, 2) == 3\n"}
		maps.Copy(m, extra)
		return m
	}
	const read = "cfg.py: a skipif or xfail condition in tests/test_money.py reads SKIP, which"
	const test = "import pytest\nimport feature.flags\nfrom money import add\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_add():\n    assert add(1, 2) == 3\n"
	for name, c := range map[string]struct {
		base, change map[string]string
		want         string // "": no block of the guard's own
	}{
		"control: the base is read": {base(nil), map[string]string{"cfg.py": "SKIP = 'True'\n"}, read},
		// S2: pytest never imported, a star may bind it.
		"S2 pytest.xfail after a star import in a module": {base(nil), map[string]string{"money.py": "from fake import *\n\ndef add(a, b):\n    pytest.xfail('a string')\n    return a + b\n"}, "money.py: unverifiable: a skipif or xfail condition that may be a string"},
		"S2 pytest.xfail with no pytest import":           {base(nil), map[string]string{"money.py": "def add(a, b):\n    pytest.xfail('a string')\n    return a + b\n"}, "money.py: unverifiable: a skipif or xfail condition that may be a string"},
		"S2 control: import pytest stays pure":            {base(nil), map[string]string{"money.py": "import pytest\n\ndef add(a, b):\n    pytest.xfail('a string')\n    return a + b\n"}, ""},
		"S2 a star import after import pytest":            {base(nil), map[string]string{"money.py": "import pytest\nfrom fake import *\n\ndef add(a, b):\n    pytest.xfail('a string')\n    return a + b\n"}, "money.py: unverifiable: a skipif or xfail condition that may be a string"},
		// S3: on every host, a stated false block.
		"S3 a reached name another differs from only by case":      {base(map[string]string{"Feature/other.py": "X = 1\n"}), map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module name feature differs from another only by case)"},
		"S3 an added module named like a base module but for case": {base(nil), map[string]string{"Cfg.py": "X = 1\n"}, "Cfg.py: an added module's name differs from the base module cfg only by case"},
		"S3 the floor folds case":                                  {base(map[string]string{"lib/helpers.py": "X = 1\n"}), map[string]string{"tests/Helpers.py": "X = 1\n"}, "tests/Helpers.py: an added module shadows the base module lib/helpers.py"},
		"S3 a changed module newly reaching a name two files share but for case": {base(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n", "parker.py": "X = 1\n", "Parker.py": "X = 2\n"}),
			map[string]string{"helper.py": "import parker\nX = 1\n"}, "helper.py: a module the tests now import runs code at import"},
		"S3 control: a name no other differs from only by case": {base(map[string]string{"other/x.py": "X = 1\n"}), map[string]string{"cfg.py": "SKIP = 'True'\n"}, read},
		// S4: the debug specifier.
		"S4 an f-string's debug = binds nothing":          {base(nil), map[string]string{"tests/test_money.py": test + "\nX = f\"{__name__=}\"\nY = f\"{__name__ = !r:>10}\"\n"}, ""},
		"S4 control: a walrus in an f-string still binds": {base(nil), map[string]string{"tests/test_money.py": test + "\nX = f\"{(__name__ := 'x')}\"\n"}, "tests/test_money.py: unverifiable: a dunder or an allowlisted name bound at import (__name__)"},
		"S4 control: == in a field is no debug =":         {base(nil), map[string]string{"tests/test_money.py": test + "\nX = f\"{__name__ == 'x'}\"\nY = f\"{__name__ != 'x'}\"\n"}, ""},
		// S5: main's allowlist.
		"S5 bare type parameters":                   {base(nil), map[string]string{"tests/test_money.py": test + "\ndef f[T, U](x: T) -> U:\n    pass\n"}, ""},
		"S5 a type alias":                           {base(nil), map[string]string{"tests/test_money.py": test + "\ntype Pair = tuple[int, int]\n"}, ""},
		"S5 a concatenated docstring":               {base(nil), map[string]string{"tests/test_money.py": "\"a\" \"b\"\n" + test}, ""},
		"S5 control: a type parameter bound":        {base(nil), map[string]string{"tests/test_money.py": test + "\ndef f[T: g()](x):\n    pass\n"}, "tests/test_money.py: unverifiable: code run at import outside the allowlist"},
		"S5 control: a type alias's call value":     {base(nil), map[string]string{"tests/test_money.py": test + "\ntype Pair = g()\n"}, "tests/test_money.py: unverifiable: code run at import outside the allowlist"},
		"S5 control: a type alias naming a fixture": {base(nil), map[string]string{"tests/test_money.py": test + "\ntype capsys = int\n"}, "tests/test_money.py: unverifiable: "},
		"S5 control: an f-string among the strings": {base(nil), map[string]string{"tests/test_money.py": "\"a\" f\"{X}\"\n" + test}, "tests/test_money.py: unverifiable: code run at import outside the allowlist"},
	} {
		g := newFixtureWith(t, func(src string) {
			write(t, src, c.base)
			plain(t, src, "add", "--all")
		})
		if v := g.guarded(t, c.change); c.want == "" && len(pyOwn(v.Blocks)) > 0 || c.want != "" && !pyFinds(pyOwn(v.Blocks), c.want) {
			t.Errorf("%s: %q, want %q", name, whats(pyOwn(v.Blocks)), c.want)
		}
	}
	paths := "" // S1: 2,000 pythonpath roots over 20 base modules, main's index past a test-only bound (the closure itself fits)
	for i := range 2000 {
		paths += fmt.Sprintf(" p%d", i)
	}
	mods := base(map[string]string{"pytest.ini": "[pytest]\npythonpath =" + paths + "\n"})
	for i := range 20 {
		mods[fmt.Sprintf("m%d.py", i)] = "X = 1\n"
	}
	g := newFixtureWith(t, func(src string) {
		write(t, src, mods)
		plain(t, src, "add", "--all")
	})
	old := maxWork
	t.Cleanup(func() { maxWork = old })
	maxWork = 60000 // main's index: 25 modules × (1 + 2 × 2,003 roots), about 100,000
	if v := g.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); !pyFinds(pyOwn(v.Blocks), "cfg.py: a module a condition reads is unread (the base conditions' analysis is over the guard's work bound)") {
		t.Errorf("S1 main's base index charged: %q", whats(pyOwn(v.Blocks)))
	}
	maxWork = old
	if v := g.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); !strings.Contains(strings.Join(whats(pyOwn(v.Blocks)), "\n"), read) {
		t.Errorf("S1 control: within the bound the base is read: %q", whats(pyOwn(v.Blocks)))
	}
}
