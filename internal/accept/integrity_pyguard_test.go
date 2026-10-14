// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	pyT = "from money import add\n\ndef test_add():\n    assert add(1, 2) == 3\n\ndef test_zero():\n    assert add(0, 0) == 0\n"
	// forgeHook is Opus #255 C2's forgery: every test reads passed.
	forgeHook = "import pytest\n\n@pytest.hookimpl(hookwrapper=True)\ndef pytest_runtest_makereport(item, call):\n" +
		"    outcome = yield\n    outcome.get_result().outcome = \"passed\"\n"
	pyFix  = "import pytest\n\n@pytest.fixture(autouse=True)\ndef forge(request):\n"
	pyFail = "python integrity not verified (P14c)"
)

// pyOwn are the blocks besides P14b's fail-closed rule (until P14c-2b).
func pyOwn(bs []Block) []Block {
	return slices.DeleteFunc(slices.Clone(bs), func(b Block) bool { return b.What == pyFail })
}

// pyFinds: a finding "path: what" holds why.
func pyFinds(bs []Block, why string) bool {
	return slices.ContainsFunc(whats(bs), func(s string) bool { return strings.Contains(s, why) })
}

func pyBlocks(t *testing.T, f fixture, name string, files map[string]string, why string) {
	t.Helper()
	if v := f.guarded(t, files); v.Reason() != vocab.ReasonTestIntegrityViolation || !pyFinds(pyOwn(v.Blocks), why) {
		t.Errorf("%s: %q, want test_integrity_violation (%s)", name, whats(v.Blocks), why)
	}
}

// pyClean: no block but P14b's fail-closed rule, and exactly the warning
// why (none when empty).
func pyClean(t *testing.T, f fixture, name string, files map[string]string, why string) {
	t.Helper()
	v := f.guarded(t, files)
	if own := pyOwn(v.Blocks); len(own) > 0 || why == "" && len(v.Warnings) > 0 || why != "" && (len(v.Warnings) != 1 || !pyFinds(v.Warnings, why)) {
		t.Errorf("%s: blocks %q, warnings %q; want none but %q (warning %q)", name, whats(own), whats(v.Warnings), pyFail, why)
	}
}

// F (P14c-2a; Opus #255 C2, H4, H5, M3; #226 entry 84): each forgery form
// is refused with the rule that discriminates it.
func TestPyGuardBlocks(t *testing.T) {
	f := newGuardFixture(t)
	for name, c := range map[string]guardCase{
		// Hooks (C2; entry 84's validator bypasses on hook detection).
		"F a conftest hook forging passes": {map[string]string{"tests/conftest.py": forgeHook}, nil, "tests/conftest.py: a file holding a pytest hook"},
		"a hook in a plugin module":        {map[string]string{"tests/plug.py": "def pytest_runtest_logreport(report):\n    report.outcome = 'passed'\n"}, nil, "pytest hook"},
		"hookimpl through an alias":        {map[string]string{"tests/helpers.py": "import pytest\n\nh = pytest.hookimpl\n"}, nil, "pytest hook"},
		"a hook named by a string":         {map[string]string{"tests/helpers.py": "def f(r):\n    pass\n\nglobals()['pytest_' + 'runtest_logreport'] = f\n"}, nil, "pytest hook"},
		"a hook in an f-string field":      {map[string]string{"money.py": baseFiles["money.py"] + "x = f\"{globals().__setitem__('pytest_runtest_logreport', print)}\"\n"}, nil, "money.py: a file holding a pytest hook"},
		"pytest_plugins":                   {map[string]string{"tests/conftest.py": "pytest_plugins = ['tests.helpers']\n"}, nil, "pytest hook"},
		"a pluggy marker":                  {map[string]string{"tests/helpers.py": "import pluggy\n\nimpl = pluggy.HookimplMarker('pytest')\n"}, nil, "pytest hook"},
		// The conftest allowlist (entry 84).
		"a conftest helper":       {map[string]string{"tests/conftest.py": "def helper():\n    return 1\n"}, nil, "plain data ([def helper"},
		"collect_ignore":          {map[string]string{"tests/conftest.py": "collect_ignore = ['test_money.py']\n"}, nil, "holding more than fixtures"},
		"collect_ignore_glob +=":  {map[string]string{"tests/conftest.py": "X = []\nX += ['*.py']\n"}, nil, "plain data ([X + ="},
		"an f-string constant":    {map[string]string{"tests/conftest.py": "X = f\"{__import__('os')._exit(0)}\"\n"}, nil, "holding more than fixtures"},
		"fixture from elsewhere":  {map[string]string{"tests/conftest.py": "from helpers import register as fixture\n\n@fixture\ndef db():\n    return 1\n"}, nil, "plain data ([from helpers"},
		"a star import":           {map[string]string{"tests/conftest.py": "from helpers import *\n"}, nil, "holding more than fixtures"},
		"a top-level if":          {map[string]string{"tests/conftest.py": "if True:\n    X = 1\n"}, nil, "plain data ([if True"},
		"a statement after a ';'": {map[string]string{"tests/conftest.py": "X = 1; import os\n"}, nil, "holding more than fixtures"},
		// Configuration (H4) and markers.
		"pytest.ini added":       {map[string]string{"pytest.ini": "[pytest]\n"}, nil, "pytest.ini: a pytest configuration changed"},
		"tox.ini [pytest]":       {map[string]string{"tox.ini": "[testenv]\ncommands = pytest\n[pytest]\naddopts = --ignore=tests\n"}, nil, "tox.ini: a pytest configuration changed"},
		"pytest.toml":            {map[string]string{"pytest.toml": "[pytest]\naddopts = ['--ignore=tests']\n"}, nil, "pytest.toml: a pytest configuration changed"},
		"a pyproject for pytest": {map[string]string{"tests/pyproject.toml": "[tool.pytest.ini_options]\naddopts = '-p x'\n"}, nil, "a pytest configuration changed"},
		"a TOML key escaped":     {map[string]string{"tests/pyproject.toml": "[tool.\"\\u0070ytest\".ini_options]\naddopts = '-p x'\n"}, nil, "a pytest configuration changed"},
		"pyvenv.cfg":             {map[string]string{"tests/pyvenv.cfg": "home = /usr\n"}, nil, "a virtual-environment marker"},
		"conda-meta":             {map[string]string{"tests/conda-meta/history": "x\n"}, nil, "a virtual-environment marker"},
		"a .pyc":                 {map[string]string{"tests/__pycache__/test_money.cpython-312.pyc": "x\n"}, nil, "compiled Python"},
		"an extension module":    {map[string]string{"money.cpython-312-x86_64-linux-gnu.so": "x\n"}, nil, "compiled Python"},
		"a .pth":                 {map[string]string{"x.pth": "import os\n"}, nil, "compiled Python"},
		"sitecustomize.py":       {map[string]string{"sitecustomize.py": "x = 1\n"}, nil, "compiled Python"},
		"entry points":           {map[string]string{"money.egg-info/entry_points.txt": "[pytest11]\nx = x\n"}, nil, "compiled Python"},
		// Encoding and tokens (H5, end to end through P14c-1's gate).
		"F a test hidden by a coding cookie": {map[string]string{"tests/test_money.py": "# coding: utf-7\nX = +ACIAIgAi-\ndef test_zero():\n    assert 1\n+ACIAIgAi-\n"}, nil,
			"tests/test_money.py: unverifiable: a coding cookie other than utf-8 or ascii (utf-7)"},
		"not tokenizable": {map[string]string{"tests/test_money.py": pyT + "def test_x(:\n"}, nil, "tests/test_money.py: unverifiable: not tokenizable"},
		// Decision A's known forms.
		"F os._exit in a test":            {map[string]string{"tests/test_money.py": pyT + "\ndef test_exit():\n    import os\n    os._exit(0)\n"}, nil, "tests/test_money.py: _exit in test_exit"},
		"subprocess at a helper's import": {map[string]string{"money.py": baseFiles["money.py"] + "import subprocess\nsubprocess.run(['true'])\n"}, nil, "money.py: subprocess in module level"},
		"os.execv at a helper's import":   {map[string]string{"money.py": baseFiles["money.py"] + "import os\nos.execv('/bin/true', [])\n"}, nil, "money.py: execv in module level"},
		"exec in an f-string at import":   {map[string]string{"money.py": baseFiles["money.py"] + "X = f\"{exec('import os')}\"\n"}, nil, "money.py: exec in module level"},
		"_pytest in a helper function":    {map[string]string{"money.py": baseFiles["money.py"] + "def f():\n    from _pytest import runner\n    return runner\n"}, nil, "money.py: _pytest in f"},
		"monkeypatch naming _pytest":      {map[string]string{"tests/test_money.py": pyT + "\ndef test_x(monkeypatch):\n    monkeypatch.setattr('_pytest.runner.call_and_report', None)\n"}, nil, "a string naming _pytest or pluggy in test_x"},
		"globals() in a test":             {map[string]string{"tests/test_money.py": pyT + "\ndef test_g():\n    globals()['add'] = max\n"}, nil, "tests/test_money.py: globals in test_g"},
		"sys.settrace in a test":          {map[string]string{"tests/test_money.py": pyT + "\ndef test_s():\n    import sys\n    sys.settrace(None)\n"}, nil, "settrace in test_s"},
		"gc.get_objects in a fixture":     {map[string]string{"tests/conftest.py": pyFix + "    import gc\n    gc.get_objects()\n"}, nil, "get_objects in forge"},
		"__eq__ from a conftest fixture":  {map[string]string{"tests/conftest.py": pyFix + "    class A:\n        def __eq__(self, o):\n            return True\n    return A()\n"}, nil, "tests/conftest.py: __eq__ in forge"},
		"print at a test's import":        {map[string]string{"tests/test_money.py": pyT + "print('PASSED')\n"}, nil, "tests/test_money.py: print in module level"},
		"a builtin fixture overridden":    {map[string]string{"tests/conftest.py": "import pytest\n\n@pytest.fixture\ndef capsys():\n    return None\n"}, nil, "a builtin fixture overridden (capsys) in module level"},
		// Opus #263 H2: an exact __main__ guard only; decorators, def lines and class bodies run at import (MD, ME).
		"MD a guard not exact":  {map[string]string{"money.py": baseFiles["money.py"] + "import os\nif __name__ != '__main__':\n    os._exit(0)\n"}, nil, "money.py: _exit in module level"},
		"MD a guard or True":    {map[string]string{"money.py": baseFiles["money.py"] + "import sys\nif __name__ == '__main__' or True:\n    sys.modules['money'].__dict__\n"}, nil, "money.py: modules in module level"},
		"a default at import":   {map[string]string{"money.py": baseFiles["money.py"] + "import os\ndef _f(x=os._exit(0)):\n    pass\n"}, nil, "money.py: _exit in module level"},
		"a decorator at import": {map[string]string{"money.py": baseFiles["money.py"] + "import os\n@(lambda f: os._exit(0))\ndef _g():\n    pass\n"}, nil, "money.py: _exit in module level"},
		"ME a class body":       {map[string]string{"money.py": baseFiles["money.py"] + "import os\nclass C:\n    os._exit(0)\n"}, nil, "money.py: _exit in module level"},
		"a thread in a test's default": {map[string]string{"tests/test_money.py": pyT + "import threading\n\ndef test_t(_=threading.Thread(target=print).start()):\n    pass\n"}, nil,
			"tests/test_money.py: Thread in module level"},
		// Opus #263 H3 (by name=, in any file) and M3 (pytest aliased in a conftest).
		"H3 a builtin fixture by name=":     {map[string]string{"tests/conftest.py": "import pytest\n\n@pytest.fixture(name='capsys')\ndef _c():\n    return None\n"}, nil, "a builtin fixture overridden by name="},
		"H3 a builtin fixture in a helper":  {map[string]string{"tests/helpers.py": "import pytest\n\n@pytest.fixture\ndef capsys():\n    return None\n"}, nil, "tests/helpers.py: a builtin fixture overridden (capsys) in module level"},
		"M3 pytest aliased in a conftest":   {map[string]string{"tests/conftest.py": "import helpers as pytest\n\n@pytest.fixture\ndef db():\n    return 1\n"}, nil, "plain data ([import helpers as pytest"},
		"M3 import pytest as in a conftest": {map[string]string{"tests/conftest.py": "import pytest as pt\n"}, nil, "plain data ([import pytest as pt"},
		"L1 a pyproject added":              {map[string]string{"tests/pyproject.toml": "[tool.black]\nline-length = 100\n"}, nil, "a pyproject.toml added or removed in a base without pytest configuration"},
		"pytest.main":                       {map[string]string{"tests/test_money.py": pyT + "\nimport pytest\n\ndef test_m():\n    pytest.main(['-q'])\n"}, nil, "pytest.main in test_m"},
		"record_xml_attribute":              {map[string]string{"tests/test_money.py": pyT + "\ndef test_r(record_xml_attribute):\n    record_xml_attribute('name', 'test_add')\n"}, nil, "record_xml_attribute in test_r"},
	} {
		pyBlocks(t, f, name, c.files, c.why)
	}
}

// Opus #255 M3 and H4 on a base's own configuration: collection off the
// defaults makes every Python change unverifiable; a pytest section is
// read as iniconfig reads it, other sections are not pytest's.
func TestPyGuardBaseConfig(t *testing.T) {
	for conf, custom := range map[string]bool{
		"pytest.ini\x00[pytest]\npython_files = check_*.py\n":                                                           true,
		"setup.cfg\x00[tool:pytest]\naddopts = -p myplugin\n":                                                           true,
		"tox.ini\x00[pytest]\naddopts = --deselect tests/test_money.py::test_add\n":                                     true,
		"tox.ini\x00[pytest]\ntestpaths = tests\n":                                                                      true,
		"pytest.ini\x00[pytest]\naddopts = -q tests/unit\n":                                                             true, // Opus #263 L3
		"setup.cfg\x00[tool:pytest]\naddopts = -ra -q --strict-markers --tb=short\n\n[flake8]\nmax-line-length = 100\n": false,
	} {
		p, body, _ := strings.Cut(conf, "\x00")
		f := newFixtureWith(t, func(src string) {
			write(t, src, map[string]string{p: body})
			plain(t, src, "add", "--all")
		})
		if v := f.guarded(t, map[string]string{"money.py": baseFiles["money.py"] + "\n"}); finds(v.Blocks, "moves collection off its defaults") != custom {
			t.Errorf("%s: %q, custom %t", conf, whats(v.Blocks), custom)
		}
		if custom {
			continue
		}
		pyClean(t, f, "a flake8 section changed", map[string]string{p: strings.Replace(body, "100", "120", 1)}, "")
		pyBlocks(t, f, "a pytest option changed", map[string]string{p: strings.Replace(body, "-ra", "-p no:python", 1)}, "setup.cfg: a pytest configuration changed")
		// iniconfig reads a column-0 '[' line not closed by ']' as a value's
		// continuation, not a header: the section goes on.
		pyBlocks(t, f, "a '[' line not a header", map[string]string{p: strings.Replace(body, "\n\n[flake8]", "\n\n[x\npython_files = check_*.py\n[flake8]", 1)}, "a pytest configuration changed")
	}
	// A base hook's helper beside it changes the hook (entry 84).
	h := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"tests/plug.py": "def pytest_runtest_logreport(report):\n    check(report)\n\ndef check(report):\n    pass\n"})
		plain(t, src, "add", "--all")
	})
	pyBlocks(t, h, "a hook's helper", map[string]string{"tests/plug.py": "def pytest_runtest_logreport(report):\n    check(report)\n\ndef check(report):\n    report.outcome = 'passed'\n"},
		"tests/plug.py: a file holding a pytest hook")
}

// The controls: ordinary Python changes raise nothing of P14c-2a's but
// the warnings decision A names (Astra #255 finding 10's exact warnings).
func TestPyGuardAcceptsAndWarns(t *testing.T) {
	f := newGuardFixture(t)
	for name, c := range map[string]guardCase{
		"a test added": {map[string]string{"tests/test_money.py": pyT + "\ndef test_sub():\n    assert add(2, 1) == 3\n"}, nil, ""},
		"fixtures in a nested conftest": {map[string]string{"tests/conftest.py": "\"\"\"Fixtures.\"\"\"\nimport pytest\nfrom pytest import fixture\n\nLIMIT = 3\nNAMES = ['a', \"b\"]\n\n" +
			"@pytest.fixture(params=[1, 2])\ndef n(request):\n    return request.param\n\n@fixture\ndef db(tmp_path, request):\n    request.cls.db = tmp_path\n    yield tmp_path\n\n" +
			"@pytest.fixture(autouse=True)\ndef opt(request):\n    return request.config.getoption('-q')\n"}, nil, ""},
		"a __main__ guard":             {map[string]string{"money.py": baseFiles["money.py"] + "\nif __name__ == '__main__':\n    import sys\n    sys.exit(0)\n"}, nil, "money.py: exit in __main__"},
		"print in a test":              {map[string]string{"tests/test_money.py": pyT + "\ndef test_p():\n    print('x')\n    assert add(1, 1) == 2\n"}, nil, "tests/test_money.py: print in test_p"},
		"subprocess in a helper":       {map[string]string{"money.py": baseFiles["money.py"] + "\ndef run():\n    import subprocess\n    return subprocess.run(['true'])\n"}, nil, "money.py: subprocess in run"},
		"an import of subprocess":      {map[string]string{"money.py": "import subprocess\n" + baseFiles["money.py"]}, nil, "money.py: subprocess in (import)"},
		"mock":                         {map[string]string{"tests/test_money.py": pyT + "\nfrom unittest import mock\n\n@mock.patch('money.add')\ndef test_m(m):\n    m.return_value = 1\n    assert m(1, 2) == 1\n"}, nil, ""},
		"raises and params":            {map[string]string{"tests/test_money.py": pyT + "\nimport pytest\n\ncases = [pytest.param(1)]\ncases.append(pytest.param(2))\n\n@pytest.mark.parametrize('x', cases)\ndef test_c(x):\n    with pytest.raises(ZeroDivisionError) as e:\n        1 / x\n    assert e.value\n"}, nil, ""},
		"a unittest case":              {map[string]string{"tests/test_money.py": pyT + "\nimport unittest\n\nclass T(unittest.TestCase):\n    def setUp(self):\n        self.x = 1\n\n    def test_t(self):\n        self.assertEqual(self.x, 1)\n"}, nil, ""},
		"walked: an exit under build/": {map[string]string{"build/test_x.py": "import os\n\ndef test_x():\n    os._exit(0)\n"}, nil, "build/test_x.py: _exit in test_x"},
		"an exact __main__ guard":      {map[string]string{"money.py": baseFiles["money.py"] + "\nif '__main__' == __name__:\n    import sys\n    sys.exit(0)\n"}, nil, "money.py: exit in __main__"},
		"f-string fields":              {map[string]string{"money.py": baseFiles["money.py"] + "\ndef fmt(x, w):\n    return f'{x!r:>{w}} {x=}'\n"}, nil, ""},
		"a test deleted":               {nil, []string{"tests/test_money.py"}, ""},
	} {
		if c.gone != nil {
			v := f.guarded(t, nil, c.gone...)
			if len(pyOwn(v.Blocks)) > 0 || len(v.Warnings) > 0 {
				t.Errorf("%s: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
			}
			continue
		}
		pyClean(t, f, name, c.files, c.why)
	}
}

// F (P14c-2a, Opus #255 H8): a 1 MiB hook body is read in time linear in
// its size and blocked; past the work bound it is unverifiable; a
// cancelled context stops the guard midway.
func TestPyGuardBounded(t *testing.T) {
	f := newGuardFixture(t)
	body := func(n int) map[string]string {
		return map[string]string{"tests/conftest.py": "def pytest_runtest_makereport(item, call):\n" + strings.Repeat("    x = item.obj.y(z, f'{w!r}')\n", n/32)}
	}
	var took [2]time.Duration
	for i, n := range []int{1 << 20, 3 << 20} {
		start := time.Now()
		pyBlocks(t, f, "a large hook body", body(n), "a file holding a pytest hook")
		took[i] = time.Since(start)
	}
	if took[1] > 6*took[0]+time.Second {
		t.Errorf("1 MiB in %v, 3 MiB in %v: not linear", took[0], took[1])
	}
	old := maxWork
	t.Cleanup(func() { maxWork = old })
	maxWork = 1 << 12
	pyBlocks(t, f, "over the work bound", body(1<<20), "tests/conftest.py: unverifiable: its analysis is over the guard's work bound")
	maxWork = old
	ctx := &countCtx{Context: context.Background()}
	ctx.n.Store(1 << 40)
	in := In{Repo: f.repo, Base: f.base, Patch: f.patch(t, body(1<<20)), Policy: f.policy, Guard: Integrity(f.repo, f.base)}
	if _, err := Accept(ctx, in); err != nil {
		t.Fatal(err)
	}
	total := ctx.calls.Load()
	ctx.calls.Store(0)
	ctx.n.Store(total / 2)
	if _, err := Accept(ctx, in); err == nil || total < 1000 {
		t.Errorf("cancelled midway (%d checks): %v", total, err)
	}
}
