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

// Opus #263 C1's inputs: a hook plugin built under a computed name and
// registered from a test's import (A), or built in a test helper a
// conftest imports, the manager reached by getattr in a fixture (B).
const (
	pyC1A = "import inspect\nfrom pytest import TestReport\nfrom money import add\n\ndef _mk(item, call):\n" +
		"    return TestReport(item.nodeid, item.location, {}, \"passed\", None, call.when)\n\n" +
		"_P = type(\"_P\", (), {\"PYTEST_RUNTEST_MAKEREPORT\".lower(): staticmethod(_mk)})\n\n" +
		"for _fi in inspect.stack():\n    _pm = inspect.getargvalues(_fi.frame).locals.get(\"self\")\n" +
		"    if type(_pm).__name__ == \"PytestPluginManager\":\n        _pm.register(_P)\n        break\n\n" +
		"def test_add():\n    assert add(1, 2) == 3\n\ndef test_zero():\n    assert add(0, 1) == 1\n"
	pyC1BHelpers = "from pytest import TestReport\n\ndef _mk(item, call):\n" +
		"    return TestReport(item.nodeid, item.location, {}, \"passed\", None, call.when)\n\n" +
		"Plugin = type(\"Plugin\", (), {\"pytest\" \"_runtest_makereport\": staticmethod(_mk)})\n"
	pyC1BConftest = "import pytest\nfrom helpers import Plugin\n\n@pytest.fixture(autouse=True, scope=\"session\")\n" +
		"def _setup(request):\n    getattr(request.config, \"plugin\" \"manager\").register(Plugin)\n"
)

// pyG is a test module holding g, a same-file function (Opus #266 H1's
// payload; the end-to-end runs register C1 A's plugin in it), then the
// trigger body, then the two tests.
func pyG(body string) map[string]string {
	return map[string]string{"tests/test_money.py": "from money import add\n\ndef g(*a):\n    return a\n\n" + body +
		"\ndef test_add():\n    assert add(1, 2) == 3\n"}
}

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
		"collect_ignore":          {map[string]string{"tests/conftest.py": "collect_ignore = ['test_money.py']\n"}, nil, "[collect_ignore ="},
		"collect_ignore_glob +=":  {map[string]string{"tests/conftest.py": "X = []\nX += ['*.py']\n"}, nil, "[X + ="},
		"an f-string constant":    {map[string]string{"tests/conftest.py": "X = f\"{__import__('os')._exit(0)}\"\n"}, nil, "code run at import in an f-string field"},
		"fixture from elsewhere":  {map[string]string{"tests/conftest.py": "from helpers import register as fixture\n\n@fixture\ndef db():\n    return 1\n"}, nil, "an import binding pytest, fixture, mock, patch or a builtin fixture"},
		"a star import":           {map[string]string{"tests/conftest.py": "from helpers import *\n"}, nil, "a star import"},
		"a top-level if":          {map[string]string{"tests/conftest.py": "if True:\n    X = 1\n"}, nil, "[if True :]"},
		"a statement after a ';'": {map[string]string{"tests/conftest.py": "X = 1; os.system('x')\n"}, nil, "[os . system ("},
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
		"M3 pytest aliased in a conftest":   {map[string]string{"tests/conftest.py": "import helpers as pytest\n\n@pytest.fixture\ndef db():\n    return 1\n"}, nil, "an import binding pytest, fixture, mock, patch or a builtin fixture"},
		"M3 import pytest as in a conftest": {map[string]string{"tests/conftest.py": "import pytest as pt\n"}, nil, "an import binding pytest, fixture, mock, patch or a builtin fixture"},
		// Mythos #263: H1 (__name__ rebound), H2 (name= not a literal), M1 (by assignment), M2 (pytest from elsewhere).
		"H2 __name__ rebound":        {map[string]string{"money.py": baseFiles["money.py"] + "import os\n__name__ = '__main__'\nif __name__ == '__main__':\n    os._exit(0)\n"}, nil, "money.py: _exit in module level"},
		"H2 __name__ by a for":       {map[string]string{"money.py": baseFiles["money.py"] + "import os\nfor __name__ in ('__main__',):\n    pass\nif __name__ == '__main__':\n    os._exit(0)\n"}, nil, "money.py: _exit in module level"},
		"H2 __name__ by a walrus":    {map[string]string{"money.py": baseFiles["money.py"] + "import os\n_ = (__name__ := '__main__')\nif __name__ == '__main__':\n    os._exit(0)\n"}, nil, "money.py: _exit in module level"},
		"H2 __name__ imported":       {map[string]string{"money.py": baseFiles["money.py"] + "import os\nfrom __main__ import __name__\nif __name__ == '__main__':\n    os._exit(0)\n"}, nil, "money.py: _exit in module level"},
		"H3 name= with a prefix":     {map[string]string{"tests/conftest.py": "import pytest\n\n@pytest.fixture(name=r'capsys')\ndef _c():\n    return None\n"}, nil, "a builtin fixture overridden by name= (capsys)"},
		"H3 name= a variable":        {map[string]string{"tests/conftest.py": "import pytest\nNAME = 'capsys'\n\n@pytest.fixture(name=NAME)\ndef _c():\n    return None\n"}, nil, "a fixture name not a literal"},
		"H3 name= through **":        {map[string]string{"tests/conftest.py": "import pytest\nKW = {'name': 'capsys'}\n\n@pytest.fixture(**KW)\ndef _c():\n    return None\n"}, nil, "a fixture name not a literal (**)"},
		"H3 name= through a partial": {map[string]string{"tests/test_money.py": pyT + "import functools\nimport pytest\nfx = functools.partial(pytest.fixture, name='capsys')\n"}, nil, "a builtin fixture overridden by name= (capsys) in module level"},
		"M1 a fixture by assignment": {map[string]string{"tests/test_money.py": pyT + "import pytest\n\ndef _c():\n    return None\ncapsys = pytest.fixture(_c)\n"}, nil, "a builtin fixture overridden by assignment (capsys)"},
		"M2 pytest from a helper":    {map[string]string{"tests/conftest.py": "from helpers import pytest\n\n@pytest.fixture\ndef db():\n    return 1\n"}, nil, "an import binding pytest, fixture, mock, patch or a builtin fixture"},
		// P14c-2a-allow: the import-time allowlist (Opus #263 C1, H1, H3, M1; Mythos #263 L1).
		"F C1 A: a plugin registered at a test's import": {map[string]string{"tests/test_money.py": pyC1A}, nil, "tests/test_money.py: unverifiable: code run at import outside the allowlist ([_P = type ("},
		"F C1 B: a plugin built in a test helper":        {map[string]string{"tests/helpers.py": pyC1BHelpers, "tests/conftest.py": pyC1BConftest}, nil, "tests/helpers.py: unverifiable: code run at import outside the allowlist ([Plugin = type ("},
		"H1 runpy at a test's import":                    {map[string]string{"tests/test_money.py": pyT + "import runpy\nrunpy.run_path('tests/data/forge.txt')\n", "tests/data/forge.txt": "x = 1\n"}, nil, "[runpy . run_path ("},
		"a call in a def's default":                      {map[string]string{"tests/test_money.py": pyT + "import os\n\ndef test_t(_=os.getpid()):\n    pass\n"}, nil, "[def test_t ("},
		"a decorator not pytest's or mock's":             {map[string]string{"tests/test_money.py": pyT + "import functools\n\n@functools.lru_cache\ndef test_m():\n    pass\n"}, nil, "[@ functools . lru_cache]"},
		"a patch not unittest.mock's":                    {map[string]string{"tests/test_money.py": pyT + "from helpers import patch\n"}, nil, "an import binding pytest, fixture, mock, patch or a builtin fixture"},
		"a dunder in a pytest chain":                     {map[string]string{"tests/test_money.py": pyT + "import pytest\nX = pytest.mark.__getattribute__('foo')\n"}, nil, "[X = pytest . mark ."},
		"a metaclass keyword":                            {map[string]string{"tests/test_money.py": pyT + "class T(metaclass=M):\n    pass\n"}, nil, "[class T ("},
		"a class body call":                              {map[string]string{"tests/test_money.py": pyT + "class TestX:\n    x = print('PASSED')\n"}, nil, "[x = print ("},
		"an attribute bound":                             {map[string]string{"tests/test_money.py": pyT + "import money\nmoney.add = max\n"}, nil, "[money . add ="},
		"a helper beside a test module":                  {map[string]string{"lib/test_lib.py": "def test_l():\n    pass\n", "lib/helpers.py": "import os\nX = os.getpid()\n"}, nil, "lib/helpers.py: unverifiable: code run at import outside the allowlist"},
		"H3 a builtin fixture imported":                  {map[string]string{"tests/conftest.py": "from helpers import capsys\n"}, nil, "an import binding pytest, fixture, mock, patch or a builtin fixture"},
		"M1 a dunder method":                             {map[string]string{"tests/test_money.py": pyT + "class Big:\n    def __len__(self):\n        return 0\n"}, nil, "a dunder or an allowlisted name bound at import (__len__)"},
		"A1 a lambda hiding a call":                      {map[string]string{"tests/test_money.py": pyT + "X = (lambda: print('PASSED'))()\n"}, nil, "[X = ( lambda"},
		"A3 a dunder a constant reaches":                 {map[string]string{"tests/helpers.py": "class K:\n    def __class_getitem__(cls, i):\n        return i\n\nX = K[int]\n"}, nil, "tests/helpers.py: unverifiable: a dunder or an allowlisted name bound at import (__class_getitem__)"},
		"M1 __class__ bound":                             {map[string]string{"tests/conftest.py": pyFix + "    o = object()\n    o.__class__ = int\n"}, nil, "a dunder method or __class__ bound"},
		"L1 a metaclass's method":                        {map[string]string{"money.py": baseFiles["money.py"] + "import os\n\nclass Meta(type):\n    def __new__(m, *a):\n        os._exit(0)\n"}, nil, "money.py: _exit in module level"},
		"L1 __init_subclass__":                           {map[string]string{"money.py": baseFiles["money.py"] + "import os\n\nclass Base:\n    def __init_subclass__(cls):\n        os._exit(0)\n"}, nil, "money.py: _exit in module level"},
		// Opus #266 H1: import code calls a same-file function without call syntax (a1–a18).
		"a1 a decorator's tail":           {pyG("import pytest\n\n@pytest.mark.parametrize('x', [1]) and g\ndef test_a(x):\n    pass\n"), nil, "[@ pytest . mark"},
		"a2 a decorator's conditional":    {pyG("import pytest\n\n@pytest.fixture() if 0 else g\ndef f():\n    pass\n"), nil, "[@ pytest . fixture"},
		"a3 __init_subclass__ assigned":   {pyG("class Base:\n    __init_subclass__ = g\n\nclass T(Base):\n    pass\n"), nil, "a dunder or an allowlisted name bound at import (__init_subclass__)"},
		"a4 __class_getitem__ assigned":   {pyG("class K:\n    __class_getitem__ = g\n\nX = K[int]\n"), nil, "a dunder or an allowlisted name bound at import (__class_getitem__)"},
		"a5 a metaclass through **":       {pyG("class T(**{'metaclass': g}):\n    pass\n"), nil, "[class T ("},
		"a6 a def named fixture":          {pyG("def fixture(fn):\n    return g(fn)\n\n@fixture\ndef f():\n    pass\n"), nil, "a dunder or an allowlisted name bound at import (fixture)"},
		"a7 fixture by a walrus":          {pyG("X = (fixture := g)\n"), nil, "a dunder or an allowlisted name bound at import (fixture)"},
		"a8 a class named pytest":         {pyG("class pytest:\n    class mark:\n        x = g\n"), nil, "a dunder or an allowlisted name bound at import (pytest)"},
		"a9 a dunder imported":            {pyG("class Base:\n    from tests.trig import g2 as __init_subclass__\n"), nil, "a dunder or an allowlisted name bound at import (__init_subclass__)"},
		"a11 a dunder by a walrus":        {pyG("class Base:\n    X = (__init_subclass__ := g)\n"), nil, "a dunder or an allowlisted name bound at import (__init_subclass__)"},
		"a13 an Enum's __init__ assigned": {pyG("import enum\n\nclass E(enum.Enum):\n    __init__ = g\n    A = 1\n"), nil, "a dunder or an allowlisted name bound at import (__init__)"},
		"a18 ** calling keys()":           {pyG("class K:\n    def keys():\n        return []\n\nX = {**K}\n"), nil, "[X = { * * K }]"},
		"C1 B through __class_getitem__": {map[string]string{"tests/helpers.py": "def _plug(*a):\n    return type('Plugin', (), {})\n\nclass _B:\n    __class_getitem__ = _plug\n\nPlugin = _B[0]\n"}, nil,
			"tests/helpers.py: unverifiable: a dunder or an allowlisted name bound at import (__class_getitem__)"},
		"H1 behind a3": {map[string]string{"tests/test_money.py": pyT + "import runpy\n\ndef g(*a, **k):\n    runpy.run_path('tests/data/forge.txt')\n\nclass Base:\n    __init_subclass__ = g\n\nclass T(Base):\n    pass\n"}, nil, "a dunder or an allowlisted name bound at import (__init_subclass__)"},
		// Opus #266 M2 (#263's code): a later or hidden rebinding of __name__.
		"M2 __name__ rebound after the guard": {map[string]string{"money.py": baseFiles["money.py"] + "import os\nfor _ in range(2):\n    if __name__ == '__main__':\n        os._exit(0)\n    __name__ = '__main__'\n"}, nil, "money.py: _exit in module level"},
		"M2 a star import":                    {map[string]string{"money.py": baseFiles["money.py"] + "from helper import *\nimport os\nif __name__ == '__main__':\n    os._exit(0)\n"}, nil, "money.py: _exit in module level"},
		// Opus #266 L1: metaclasses by an imported base or an alias; more class-creation methods.
		"L1 a metaclass on ABCMeta":  {map[string]string{"money.py": baseFiles["money.py"] + "import abc\nimport os\n\nclass M(abc.ABCMeta):\n    def __new__(m, *a):\n        os._exit(0)\n"}, nil, "money.py: _exit in module level"},
		"L1 a metaclass on an alias": {map[string]string{"money.py": baseFiles["money.py"] + "import os\nT = type\n\nclass M(T):\n    def __call__(c, *a):\n        os._exit(0)\n"}, nil, "money.py: _exit in module level"},
		"L1 def __class_getitem__":   {map[string]string{"money.py": baseFiles["money.py"] + "import os\n\nclass K:\n    def __class_getitem__(cls, i):\n        os._exit(0)\n"}, nil, "money.py: _exit in module level"},
		// Mythos #266: H1 (a walrus in an f-string), M1 (for (__name__)), M3 (a shadow). The condition rule (H2) is P14c-2a-cond's.
		"H1m fixture by an f-string walrus":           {pyG("X = f\"{(fixture := g)}\"\n\n@fixture\ndef f():\n    pass\n"), nil, "a dunder or an allowlisted name bound at import (fixture)"},
		"H1m __init_subclass__ by an f-string walrus": {pyG("class Base:\n    X = f\"{(__init_subclass__ := g)}\"\n\nclass T(Base):\n    pass\n"), nil, "a dunder or an allowlisted name bound at import (__init_subclass__)"},
		"M1m for (__name__)":                          {map[string]string{"money.py": baseFiles["money.py"] + "import os\nfor (__name__) in ('__main__',):\n    pass\nif __name__ == '__main__':\n    os._exit(0)\n"}, nil, "money.py: _exit in module level"},
		"M1m with … as (__name__)":                    {map[string]string{"money.py": baseFiles["money.py"] + "import os\nwith open('x') as (__name__):\n    pass\nif __name__ == '__main__':\n    os._exit(0)\n"}, nil, "money.py: _exit in module level"},
		"M3 a shadow of money":                        {map[string]string{"tests/money.py": "def add(a, b):\n    return a + b\n"}, nil, "tests/money.py: a module added under a test directory shadows a base module"},
		"M3 tests/__init__.py added":                  {map[string]string{"tests/__init__.py": ""}, nil, "tests/__init__.py: a module added under a test directory shadows a base module, or its __init__.py moved"},
		// P14c-2a-cond: the condition rule (Opus and Mythos #266 H2) with Mythos r2's forms.
		"H2 a skipif string":              {pyG("import pytest\n\n@pytest.mark.skipif(\"config.pluginmanager.register(1) and False\", reason='r')\ndef test_s():\n    pass\n"), nil, "a skipif or xfail condition that may be a string"},
		"H2 xfail(condition=)":            {pyG("import pytest\n\n@pytest.mark.xfail(condition='x', reason='r')\ndef test_s():\n    pass\n"), nil, "a skipif or xfail condition that may be a string"},
		"H2 pytestmark":                   {pyG("import pytest\n\npytestmark = pytest.mark.skipif('x')\n"), nil, "a skipif or xfail condition that may be a string"},
		"H2 pytest.param(marks=)":         {pyG("import pytest\n\n@pytest.mark.parametrize('x', [pytest.param(1, marks=pytest.mark.xfail('x'))])\ndef test_s(x):\n    pass\n"), nil, "a skipif or xfail condition that may be a string"},
		"H2 applied in a body":            {pyG("import pytest\n\ndef test_s(request):\n    request.applymarker(pytest.mark.xfail('x'))\n"), nil, "a skipif or xfail condition that may be a string"},
		"H2m skipif(COND)":                {pyG("import pytest\nCOND = 'x'\n\n@pytest.mark.skipif(COND, reason='r')\ndef test_s():\n    pass\n"), nil, "a skipif or xfail condition that may be a string"},
		"H2m skipif((string))":            {pyG("import pytest\n\n@pytest.mark.skipif(('x'), reason='r')\ndef test_s():\n    pass\n"), nil, "a skipif or xfail condition that may be a string"},
		"H2m an or operand":               {pyG("import pytest\nimport sys\n\n@pytest.mark.skipif(sys.platform == 'nope' or 'x', reason='r')\ndef test_s():\n    pass\n"), nil, "a skipif or xfail condition that may be a string"},
		"H2m a conditional":               {pyG("import pytest\nX = 0\n\n@pytest.mark.skipif(1 == 1 if X else 'x', reason='r')\ndef test_s():\n    pass\n"), nil, "a skipif or xfail condition that may be a string"},
		"H2m skipif(*ARGS)":               {pyG("import pytest\nARGS = ('x',)\n\n@pytest.mark.skipif(*ARGS, reason='r')\ndef test_s():\n    pass\n"), nil, "a skipif or xfail condition that may be a string"},
		"H2m skipif(**{condition})":       {pyG("import pytest\n\n@pytest.mark.skipif(**{'condition': 'x'})\ndef test_s():\n    pass\n"), nil, "a skipif or xfail condition that may be a string"},
		"H2m xfail(condition=COND)":       {pyG("import pytest\nCOND = 'x'\n\n@pytest.mark.xfail(condition=COND, reason='r')\ndef test_s():\n    pass\n"), nil, "a skipif or xfail condition that may be a string"},
		"H2m pytestmark = skipif(C)":      {pyG("import pytest\nCOND = 'x'\npytestmark = pytest.mark.skipif(COND, reason='r')\n"), nil, "a skipif or xfail condition that may be a string"},
		"H2m param marks=xfail(C)":        {pyG("import pytest\nCOND = 'x'\n\n@pytest.mark.parametrize('x', [pytest.param(1, marks=pytest.mark.xfail(COND))])\ndef test_s(x):\n    pass\n"), nil, "a skipif or xfail condition that may be a string"},
		"r2 skipif(p := string)":          {pyG("import pytest\n\n@pytest.mark.skipif(p := 'x', reason='r')\ndef test_s():\n    pass\n"), nil, "a skipif or xfail condition that may be a string"},
		"r2 pytestmark = skipif(p := …)":  {pyG("import pytest\n\npytestmark = pytest.mark.skipif(p := 'x', reason='r')\n"), nil, "a skipif or xfail condition that may be a string"},
		"r2 skipif.with_args(string)":     {pyG("import pytest\n\n@pytest.mark.skipif.with_args('x', reason='r')\ndef test_s():\n    pass\n"), nil, "a skipif or xfail condition that may be a string"},
		"r2 xfail.with_args(condition=…)": {pyG("import pytest\n\n@pytest.mark.xfail.with_args(condition='x', reason='r')\ndef test_s():\n    pass\n"), nil, "a skipif or xfail condition that may be a string"},
		"a bare name (a D55 false block)": {pyG("import pytest\nIS_WINDOWS = False\n\n@pytest.mark.skipif(IS_WINDOWS, reason='w')\ndef test_s():\n    pass\n"), nil, "a skipif or xfail condition that may be a string"},
		"r2 M1 case str(__name__)":        {map[string]string{"money.py": baseFiles["money.py"] + "import os\nmatch '__main__':\n    case str(__name__):\n        pass\nif __name__ == '__main__':\n    os._exit(0)\n"}, nil, "money.py: _exit in module level"},
		// Opus #267: H1 (a mark built in an application module), M1 (<<), M2 (pytest rebound).
		"H1 a mark built in money.py": {map[string]string{"money.py": "import pytest\n" + baseFiles["money.py"] + "SKIP = pytest.mark.skipif('x', reason='r')\n",
			"tests/test_money.py": "from money import add, SKIP\npytestmark = SKIP\n\ndef test_add():\n    assert add(1, 2) == 3\n"}, nil, "money.py: unverifiable: a skipif or xfail condition that may be a string"},
		"H1 marks=SKIP and a class pytestmark": {map[string]string{"money.py": "import pytest\n" + baseFiles["money.py"] + "SKIP = pytest.mark.xfail(condition='x')\n",
			"tests/test_money.py": "import pytest\nfrom money import add, SKIP\n\nclass TestA:\n    pytestmark = SKIP\n\n@pytest.mark.parametrize('x', [pytest.param(1, marks=SKIP)])\ndef test_add(x):\n    assert add(1, 2) == 3\n"}, nil, "money.py: unverifiable: a skipif or xfail condition that may be a string"},
		"M1 skipif(1 << ZERO)":        {pyG("import pytest\nZERO = 0\n\n@pytest.mark.skipif(1 << ZERO, reason='r')\ndef test_s():\n    pass\n"), nil, "a skipif or xfail condition that may be a string"},
		"M2 a parameter named pytest": {map[string]string{"tests/conftest.py": "import pytest\n\n@pytest.fixture(autouse=True)\ndef fx(request, pytest=pytest.mark):\n    request.applymarker(pytest.xfail('x'))\n"}, nil, "a skipif or xfail condition that may be a string"},
		// Mythos #267: H2 (the root, module paths), M2 (renamed marks in an application module).
		"M2 an alias of skipif":      {map[string]string{"money.py": "import pytest\n" + baseFiles["money.py"] + "sk = pytest.mark.skipif\nSKIP = sk('x', reason='r')\n"}, nil, "money.py: unverifiable: a skipif or xfail condition that may be a string"},
		"M2 pytest bound by a tuple": {map[string]string{"money.py": "import pytest\n" + baseFiles["money.py"] + "pytest, _ = pytest.mark, 0\nSKIP = pytest.xfail('x')\n"}, nil, "money.py: unverifiable: a skipif or xfail condition that may be a string"},
		"M2 .__call__":               {map[string]string{"money.py": "import pytest\n" + baseFiles["money.py"] + "SKIP = pytest.mark.skipif.__call__('x')\n"}, nil, "money.py: unverifiable: a skipif or xfail condition that may be a string"},
		"M2 xfail(False)(string)":    {map[string]string{"money.py": "import pytest\n" + baseFiles["money.py"] + "SKIP = pytest.mark.xfail(False)('x')\n"}, nil, "money.py: unverifiable: a skipif or xfail condition that may be a string"},
		"r3 H1 pytest rebound after import pytest;": {map[string]string{"money.py": "import pytest; pytest, _ = pytest.mark, 0\n" + baseFiles["money.py"] + "SKIP = pytest.xfail('x')\n",
			"tests/test_money.py": "from money import add, SKIP\npytestmark = SKIP\n\ndef test_add():\n    assert add(1, 2) == 3\n"}, nil, "money.py: unverifiable: a skipif or xfail condition"},
		"r3 H2 a second call inside an f-string field": {map[string]string{"tests/conftest.py": "import pytest\n\n@pytest.fixture(autouse=True)\ndef fx(request):\n    x = f\"{request.applymarker(pytest.mark.xfail(False)('x'))}\"\n"}, nil,
			"tests/conftest.py: unverifiable: a skipif or xfail condition"},
		"r4 H1 an alias of a partially applied mark": {map[string]string{"tests/conftest.py": "import pytest\n\n@pytest.fixture(autouse=True)\ndef fx(request):\n    m = pytest.mark.xfail(False)\n    request.applymarker(m('x'))\n"}, nil,
			"tests/conftest.py: unverifiable: a skipif or xfail condition"},
		"r7 1a (mark(False))(x)":        {map[string]string{"tests/conftest.py": "import pytest\n\n@pytest.fixture(autouse=True)\ndef fx(request):\n    request.applymarker((pytest.mark.xfail(False))('x'))\n"}, nil, "tests/conftest.py: unverifiable: a skipif or xfail condition"},
		"r7 1b ((mark(False)))(x)":      {map[string]string{"tests/conftest.py": "import pytest\n\n@pytest.fixture(autouse=True)\ndef fx(request):\n    request.applymarker(((pytest.mark.xfail(False)))('x'))\n"}, nil, "tests/conftest.py: unverifiable: a skipif or xfail condition"},
		"r7 1c (m := mark(False))(x)":   {map[string]string{"tests/conftest.py": "import pytest\n\n@pytest.fixture(autouse=True)\ndef fx(request):\n    request.applymarker((m := pytest.mark.xfail(False))('x'))\n"}, nil, "tests/conftest.py: unverifiable: a skipif or xfail condition"},
		"r7 1d (mark)(False)(x)":        {map[string]string{"tests/conftest.py": "import pytest\n\n@pytest.fixture(autouse=True)\ndef fx(request):\n    request.applymarker((pytest.mark.xfail)(False)('x'))\n"}, nil, "tests/conftest.py: unverifiable: a skipif or xfail condition"},
		"r7 2a m = (mark)":              {map[string]string{"tests/conftest.py": "import pytest\n\n@pytest.fixture(autouse=True)\ndef fx(request):\n    m = (pytest.mark.xfail)\n    request.applymarker(m(False)('x'))\n"}, nil, "tests/conftest.py: unverifiable: a skipif or xfail condition"},
		"r7 2b m = (mark(False))":       {map[string]string{"tests/conftest.py": "import pytest\n\n@pytest.fixture(autouse=True)\ndef fx(request):\n    m = (pytest.mark.xfail(False))\n    request.applymarker(m('x'))\n"}, nil, "tests/conftest.py: unverifiable: a skipif or xfail condition"},
		"r7 2c a walrus alias":          {map[string]string{"tests/conftest.py": "import pytest\n\n@pytest.fixture(autouse=True)\ndef fx(request):\n    (m := pytest.mark.xfail(False))\n    request.applymarker(m('x'))\n"}, nil, "tests/conftest.py: unverifiable: a skipif or xfail condition"},
		"r7 1e SKIP = (mark(False))(x)": {map[string]string{"money.py": "import pytest\n" + baseFiles["money.py"] + "SKIP = (pytest.mark.xfail(False))('x')\n"}, nil, "money.py: unverifiable: a skipif or xfail condition"},
		"r2 L4 an ordering dunder":      {map[string]string{"money.py": baseFiles["money.py"] + "class K:\n    def __lt__(self, o):\n        return 'x'\n"}, nil, "money.py: __lt__ in module level"},
		"M2 pytest from the package":    {map[string]string{"tests/conftest.py": "from . import pytest\n"}, nil, "an import binding pytest, fixture, mock, patch or a builtin fixture"},
		"L1 a pyproject added":          {map[string]string{"tests/pyproject.toml": "[tool.black]\nline-length = 100\n"}, nil, "a pyproject.toml added or removed in a base without pytest configuration"},
		"pytest.main":                   {map[string]string{"tests/test_money.py": pyT + "\nimport pytest\n\ndef test_m():\n    pytest.main(['-q'])\n"}, nil, "pytest.main in test_m"},
		"record_xml_attribute":          {map[string]string{"tests/test_money.py": pyT + "\ndef test_r(record_xml_attribute):\n    record_xml_attribute('name', 'test_add')\n"}, nil, "record_xml_attribute in test_r"},
	} {
		pyBlocks(t, f, name, c.files, c.why)
	}
	// Mythos #267 r4 H1 control: @m, m an alias of a partially applied
	// mark, stays the one allowlist block it was (no condition finding).
	m := map[string]string{"tests/test_money.py": pyT + "\nimport pytest\nm = pytest.mark.xfail(False)\n\n@m\ndef test_x():\n    pass\n"}
	if own := pyOwn(f.guarded(t, m).Blocks); len(own) != 1 || !pyFinds(own, "tests/test_money.py: unverifiable: code run at import outside the allowlist ([@ m])") {
		t.Errorf("r4 H1 control @m: %q", whats(own))
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
	// Mythos #263 L3: a -W value and a comment line in addopts are not positional.
	for _, conf := range []string{"[pytest]\naddopts = -W error::DeprecationWarning -q\n", "[pytest]\naddopts = -q\n    # a note\n    -ra\n"} {
		g := newFixtureWith(t, func(src string) {
			write(t, src, map[string]string{"pytest.ini": conf})
			plain(t, src, "add", "--all")
		})
		if v := g.guarded(t, map[string]string{"money.py": baseFiles["money.py"] + "\n"}); finds(v.Blocks, "moves collection off its defaults") {
			t.Errorf("%q: %q, want the defaults", conf, whats(v.Blocks))
		}
	}
	// A base hook's helper beside it changes the hook (entry 84).
	h := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"tests/plug.py": "def pytest_runtest_logreport(report):\n    check(report)\n\ndef check(report):\n    pass\n"})
		plain(t, src, "add", "--all")
	})
	pyBlocks(t, h, "a hook's helper", map[string]string{"tests/plug.py": "def pytest_runtest_logreport(report):\n    check(report)\n\ndef check(report):\n    report.outcome = 'passed'\n"},
		"tests/plug.py: a file holding a pytest hook")
}

// Opus #266 widening 1: a finding unchanged from the base passes when it
// calls only imported names and the imports are unchanged; a same-file
// callee, or a changed import, still blocks.
func TestPyGuardBaseFindings(t *testing.T) {
	path := pyT + "from pathlib import Path\n\nHERE = Path(__file__).parent\n\ndef helper():\n    return 1\n\nX = helper()\n"
	f := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"tests/test_money.py": path})
		plain(t, src, "add", "--all")
	})
	if v := f.guarded(t, map[string]string{"tests/test_money.py": strings.Replace(path, "X = helper()", "X = helper()\nY = 2", 1)}); pyFinds(pyOwn(v.Blocks), "HERE") || !pyFinds(pyOwn(v.Blocks), "[X = helper ( )]") {
		t.Errorf("an unchanged finding: %q, want HERE passed and X = helper() blocked (a same-file callee)", whats(pyOwn(v.Blocks)))
	}
	if v := f.guarded(t, map[string]string{"tests/test_money.py": "import os\n" + path}); !pyFinds(pyOwn(v.Blocks), "[HERE = Path") {
		t.Errorf("imports changed: %q, want HERE blocked", whats(pyOwn(v.Blocks)))
	}
	// Mythos #266 H1: a walrus in an f-string field makes Path local.
	if v := f.guarded(t, map[string]string{"tests/test_money.py": path + "Y = f\"{(Path := len)}\"\n"}); !pyFinds(pyOwn(v.Blocks), "[HERE = Path") {
		t.Errorf("Path rebound in an f-string: %q, want HERE blocked", whats(pyOwn(v.Blocks)))
	}
	// Mythos #266 L1, L2 (strict findings never pass) and L3 (a warning
	// when an unchanged finding's imports name a changed module).
	strict := "from helpers import pytest\nfrom os import getpid\n\nclass Base:\n    __init_subclass__ = getpid()\n"
	g := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"tests/conftest.py": strict, "tests/helpers.py": "def make():\n    return 1\n",
			"tests/test_money.py": pyT + "from helpers import make\n\nCASES = make()\n"})
		plain(t, src, "add", "--all")
	})
	if v := g.guarded(t, map[string]string{"tests/conftest.py": strict + "X = 1\n"}); !pyFinds(pyOwn(v.Blocks), "an import binding") || !pyFinds(pyOwn(v.Blocks), "[__init_subclass__ = getpid") {
		t.Errorf("strict base findings: %q, want both blocked", whats(pyOwn(v.Blocks)))
	}
	if v := g.guarded(t, map[string]string{"tests/helpers.py": "def make():\n    return 2\n", "tests/test_money.py": pyT + "from helpers import make\n\nCASES = make()\nY = 1\n"}); len(pyOwn(v.Blocks)) > 0 || !pyFinds(v.Warnings, "its imports name a changed module") {
		t.Errorf("a changed module under an unchanged finding: blocks %q, warnings %q", whats(pyOwn(v.Blocks)), whats(v.Warnings))
	}
}

// Opus #267 M4: an unchanged condition passes when each name it reads is
// bound as in the base; a changed binding blocks.
func TestPyGuardConditionBase(t *testing.T) {
	test := pyT + "import pytest\nIS_WINDOWS = False\n\n@pytest.mark.skipif(IS_WINDOWS, reason='w')\ndef test_w():\n    pass\n"
	f := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"tests/test_money.py": test})
		plain(t, src, "add", "--all")
	})
	if v := f.guarded(t, map[string]string{"tests/test_money.py": test + "\ndef test_more():\n    pass\n"}); pyFinds(pyOwn(v.Blocks), "skipif or xfail") {
		t.Errorf("an unchanged condition: %q, want it passed", whats(pyOwn(v.Blocks)))
	}
	if v := f.guarded(t, map[string]string{"tests/test_money.py": strings.Replace(test, "IS_WINDOWS = False", "IS_WINDOWS = 'x'", 1)}); !pyFinds(pyOwn(v.Blocks), "skipif or xfail") {
		t.Errorf("a changed binding: %q, want the condition blocked", whats(pyOwn(v.Blocks)))
	}
	app := "import pytest\n\ndef flag():\n    return False\n\nSKIP = pytest.mark.skipif(flag(), reason='f')\n"
	h := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"marks.py": app})
		plain(t, src, "add", "--all")
	})
	if v := h.guarded(t, map[string]string{"marks.py": strings.Replace(app, "return False", "return 'x'", 1)}); !pyFinds(pyOwn(v.Blocks), "marks.py: unverifiable: a skipif or xfail condition") {
		t.Errorf("a same-file function's body changed under an unchanged condition: %q, want blocked", whats(pyOwn(v.Blocks)))
	}
}

// Mythos #267 H1, H3 and M3: a condition's names rebound; __path__.
func TestPyGuardConditionReach(t *testing.T) {
	test := "import pytest\nimport money\nfrom money import add, FAST\n\n@pytest.mark.skipif(FAST, reason='s')\ndef test_add():\n    assert add(1, 2) == 3\n\n@pytest.mark.xfail(money.SLOW, reason='s')\ndef test_zero():\n    pass\n"
	marks := "import pytest\nIS_WINDOWS = False\nSKIP = pytest.mark.skipif(IS_WINDOWS, reason='w')\n"
	f := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"money.py": baseFiles["money.py"] + "SLOW = False\nFAST = False\n", "tests/test_money.py": test, "marks.py": marks,
			"lib/core.py": baseFiles["money.py"], "pkg/__init__.py": "import pkgutil\n__path__ = pkgutil.extend_path(__path__, __name__)\n"})
		plain(t, src, "add", "--all")
	})
	for name, c := range map[string]guardCase{
		"H1 an imported name rebound in the test":    {map[string]string{"tests/test_money.py": strings.Replace(test, "from money import add, FAST\n", "from money import add, FAST\nFAST = 'x'\n", 1)}, nil, "tests/test_money.py: unverifiable: a skipif or xfail condition"},
		"H3 __path__ in an application package":      {map[string]string{"lib/__init__.py": "__path__ = ['lib/alt']\n", "lib/alt/core.py": "def add(a, b):\n    return a + b\n"}, nil, "lib/__init__.py: unverifiable: a package's import path, spec or loader bound at import (__path__)"},
		"M3 a for target rebinds a condition's name": {map[string]string{"marks.py": strings.Replace(marks, "SKIP =", "for IS_WINDOWS in ['x']:\n    pass\nSKIP =", 1)}, nil, "marks.py: unverifiable: a skipif or xfail condition"},
		"an unchanged extend_path passes":            {map[string]string{"pkg/__init__.py": "import pkgutil\n__path__ = pkgutil.extend_path(__path__, __name__)\nX = 1\n"}, nil, ""},
	} {
		v := f.guarded(t, c.files, c.gone...)
		if own := pyOwn(v.Blocks); c.why == "" && len(own) > 0 || c.why != "" && !pyFinds(own, c.why) {
			t.Errorf("%s: %q, want %q", name, whats(own), c.why)
		}
	}
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
		"a __main__ guard":                    {map[string]string{"money.py": baseFiles["money.py"] + "\nif __name__ == '__main__':\n    import sys\n    sys.exit(0)\n"}, nil, "money.py: exit in __main__"},
		"print in a test":                     {map[string]string{"tests/test_money.py": pyT + "\ndef test_p():\n    print('x')\n    assert add(1, 1) == 2\n"}, nil, "tests/test_money.py: print in test_p"},
		"subprocess in a helper":              {map[string]string{"money.py": baseFiles["money.py"] + "\ndef run():\n    import subprocess\n    return subprocess.run(['true'])\n"}, nil, "money.py: subprocess in run"},
		"an import of subprocess":             {map[string]string{"money.py": "import subprocess\n" + baseFiles["money.py"]}, nil, "money.py: subprocess in (import)"},
		"mock":                                {map[string]string{"tests/test_money.py": pyT + "\nfrom unittest import mock\n\n@mock.patch('money.add')\ndef test_m(m):\n    m.return_value = 1\n    assert m(1, 2) == 1\n"}, nil, ""},
		"a helper def in a conftest":          {map[string]string{"tests/conftest.py": "def helper():\n    return 1\n"}, nil, ""},
		"a def taking *args and **kwargs":     {map[string]string{"tests/conftest.py": "def helper(*args, **kwargs):\n    return args\n"}, nil, ""},
		"r2 L1 import pytest, os":             {map[string]string{"tests/test_money.py": pyT + "\nimport pytest, os\n\ndef test_x():\n    pytest.xfail('not yet')\n"}, nil, ""},
		"r2 L2 an f-string after a condition": {map[string]string{"tests/test_money.py": pyT + "\nimport pytest\nX = 1\n\n@pytest.mark.skipif(X == 1, reason=f'{(X)}')\ndef test_x():\n    pass\n"}, nil, ""},
		"r4 L a trailing ;":                   {map[string]string{"tests/test_money.py": pyT + "\nX = 1;\n\ndef test_x():\n    x = 1;\n    assert x\n"}, nil, ""},
		"comparison decorators":               {map[string]string{"tests/test_money.py": pyT + "\nimport sys\nimport pytest\n\n@pytest.mark.skipif(sys.platform == 'win32' or sys.version_info < (3, 8), reason='old')\n@pytest.mark.xfail(not True, reason='x', strict=True)\ndef test_b():\n    pytest.xfail('not yet')\n"}, nil, ""},
		"mock and patch decorators":           {map[string]string{"tests/test_money.py": pyT + "\nimport unittest.mock\nfrom unittest import mock\nfrom unittest.mock import patch\n\n@mock.patch('money.add')\n@patch.object(mock, 'ANY')\n@unittest.mock.patch.dict('os.environ', {'X': '1'})\ndef test_p(a, b):\n    pass\n"}, nil, ""},
		"raises and params":                   {map[string]string{"tests/test_money.py": pyT + "\nimport pytest\n\npytestmark = pytest.mark.slow\nCASES = [(1, 2), (3, 4)]\n\n@pytest.mark.parametrize('x', [pytest.param(1, id='one'), 2])\n@pytest.mark.skipif(1 > 2, reason='never')\ndef test_c(x):\n    with pytest.raises(ZeroDivisionError) as e:\n        1 / (x - x)\n    assert e.value\n\nclass TestK:\n    \"\"\"Doc.\"\"\"\n    k = 1\n\n    def test_k(self):\n        assert self.k\n"}, nil, ""},
		"a unittest case":                     {map[string]string{"tests/test_money.py": pyT + "\nimport unittest\n\nclass T(unittest.TestCase):\n    def setUp(self):\n        self.x = 1\n\n    def test_t(self):\n        self.assertEqual(self.x, 1)\n"}, nil, ""},
		"walked: an exit under build/":        {map[string]string{"build/test_x.py": "import os\n\ndef test_x():\n    os._exit(0)\n"}, nil, "build/test_x.py: _exit in test_x"},
		"an exact __main__ guard":             {map[string]string{"money.py": baseFiles["money.py"] + "\nif '__main__' == __name__:\n    import sys\n    sys.exit(0)\n"}, nil, "money.py: exit in __main__"},
		"__name__ as a call argument":         {map[string]string{"money.py": "import logging\n\ndef log():\n    return logging.getLogger(__name__)\n\n" + baseFiles["money.py"] + "\nif __name__ == '__main__':\n    import sys\n    sys.exit(0)\n"}, nil, "money.py: exit in __main__"},
		"f-string fields":                     {map[string]string{"money.py": baseFiles["money.py"] + "\ndef fmt(x, w):\n    return f'{x!r:>{w}} {x=}'\n"}, nil, ""},
		"a test deleted":                      {nil, []string{"tests/test_money.py"}, ""},
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
	// Opus #266 M1: an application module's new import-time code warns,
	// naming the file, and never blocks; an unchanged one is silent.
	app := map[string]string{"money.py": "import logging\ntry:\n    import json\nexcept ImportError:\n    json = None\n\nlogger = logging.getLogger(__name__)\n\n" + baseFiles["money.py"]}
	if v := f.guarded(t, app); len(pyOwn(v.Blocks)) > 0 || len(v.Warnings) != 3 || !pyFinds(v.Warnings, "money.py: code run at import outside the allowlist ([logger = logging") {
		t.Errorf("an application module's top level: blocks %q, warnings %q", whats(pyOwn(v.Blocks)), whats(v.Warnings))
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
	in := In{Repo: f.repo, Base: f.base, Patch: f.patch(t, body(1<<20)), Policy: f.policy, Guard: Integrity(f.repo, f.base, testLister(t))}
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
