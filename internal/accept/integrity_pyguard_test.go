// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
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
		"M3 a shadow of money":                        {map[string]string{"tests/money.py": "def add(a, b):\n    return a + b\n"}, nil, "tests/money.py: an added module shadows the base module money.py"},
		"M3 tests/__init__.py added":                  {map[string]string{"tests/__init__.py": ""}, nil, "tests/test_money.py: a test file's basedir moves"},
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
		"H2 a package beside a root module": {map[string]string{"money/__init__.py": "def add(a, b):\n    return a + b\n"}, nil, "money/__init__.py: an added module shadows the base module money.py"},
		"M2 an alias of skipif":             {map[string]string{"money.py": "import pytest\n" + baseFiles["money.py"] + "sk = pytest.mark.skipif\nSKIP = sk('x', reason='r')\n"}, nil, "money.py: unverifiable: a skipif or xfail condition that may be a string"},
		"M2 pytest bound by a tuple":        {map[string]string{"money.py": "import pytest\n" + baseFiles["money.py"] + "pytest, _ = pytest.mark, 0\nSKIP = pytest.xfail('x')\n"}, nil, "money.py: unverifiable: a skipif or xfail condition that may be a string"},
		"M2 .__call__":                      {map[string]string{"money.py": "import pytest\n" + baseFiles["money.py"] + "SKIP = pytest.mark.skipif.__call__('x')\n"}, nil, "money.py: unverifiable: a skipif or xfail condition that may be a string"},
		"M2 xfail(False)(string)":           {map[string]string{"money.py": "import pytest\n" + baseFiles["money.py"] + "SKIP = pytest.mark.xfail(False)('x')\n"}, nil, "money.py: unverifiable: a skipif or xfail condition that may be a string"},
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
		"r2 M3 an alias of an alias":    {map[string]string{"money.py": "import pytest\n" + baseFiles["money.py"] + "sk = pytest.mark.skipif\nsk2 = sk\nSKIP = sk2('x')\n"}, nil, "money.py: unverifiable: a skipif or xfail condition"},
		"r2 M3 a tuple alias":           {map[string]string{"money.py": "import pytest\n" + baseFiles["money.py"] + "sk, _ = pytest.mark.skipif, 0\nSKIP = sk('x')\n"}, nil, "money.py: unverifiable: a skipif or xfail condition"},
		"r2 M3 an alias after a ;":      {map[string]string{"money.py": "import pytest\n" + baseFiles["money.py"] + "sk = pytest.mark.skipif; SKIP = sk('x')\n"}, nil, "money.py: unverifiable: a skipif or xfail condition"},
		"r2 M3 an alias of an alias in a fixture": {map[string]string{"tests/conftest.py": "import pytest\n\n@pytest.fixture(autouse=True)\ndef fx(request):\n    m = pytest.mark.xfail(False)\n    k = m\n    request.applymarker(k('x'))\n"}, nil,
			"tests/conftest.py: unverifiable: a skipif or xfail condition"},
		"r2 L4 an ordering dunder":   {map[string]string{"money.py": baseFiles["money.py"] + "class K:\n    def __lt__(self, o):\n        return 'x'\n"}, nil, "money.py: __lt__ in module level"},
		"M2 pytest from the package": {map[string]string{"tests/conftest.py": "from . import pytest\n"}, nil, "an import binding pytest, fixture, mock, patch or a builtin fixture"},
		"L1 a pyproject added":       {map[string]string{"tests/pyproject.toml": "[tool.black]\nline-length = 100\n"}, nil, "a pyproject.toml added or removed in a base without pytest configuration"},
		"pytest.main":                {map[string]string{"tests/test_money.py": pyT + "\nimport pytest\n\ndef test_m():\n    pytest.main(['-q'])\n"}, nil, "pytest.main in test_m"},
		"record_xml_attribute":       {map[string]string{"tests/test_money.py": pyT + "\ndef test_r(record_xml_attribute):\n    record_xml_attribute('name', 'test_add')\n"}, nil, "record_xml_attribute in test_r"},
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
	if v := g.guarded(t, map[string]string{"tests/conftest.py": strict + "X = 1\n"}); !pyFinds(pyOwn(v.Blocks), "tests/conftest.py: a module a condition reads is unread (the module tests/test_money.py runs code at import outside the allowlist)") { // round 5: an unchanged judged file must be inert
		t.Errorf("strict base findings: %q, want blocked", whats(pyOwn(v.Blocks)))
	}
	if v := g.guarded(t, map[string]string{"tests/helpers.py": "def make():\n    return 2\n", "tests/test_money.py": pyT + "from helpers import make\n\nCASES = make()\nY = 1\n"}); !pyFinds(pyOwn(v.Blocks), "tests/helpers.py: a module a condition reads is unread (the module tests/conftest.py runs code at import outside the allowlist)") { // round 5: a stated false block (an unchanged conftest that is not inert)
		t.Errorf("a changed module under an unchanged finding: blocks %q, warnings %q", whats(pyOwn(v.Blocks)), whats(v.Warnings))
	}
}

// P14c-2a-shadow (Mythos #266 r2 M3, L1, L2, L5): a module added under a
// test directory, or in a base pythonpath directory, shadowing what the
// tests import; tests' __init__.py renamed out.
func TestPyGuardShadows(t *testing.T) {
	f := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"tests/test_requests.py": "import os; import requests as rq\ntry: import yaml\nexcept ImportError: pass\nimport json, \\\n    toml\nfrom lib.core import add\nfrom google.protobuf import message\nimport pkgx.sub\nimport importlib\nimportlib.import_module('dynmod')\n__import__('dyn2')\npytest.importorskip('dyn3')\n\ndef test_r():\n    assert rq\n",
			"tests/__init__.py": "X = 1\n", "app/utils.py": "X = 1\n", "pytest.ini": "[pytest]\npythonpath = src\n", "lib/core.py": "X = 1\n", "bank.py": "X = 1\n",
			"lib3/test_x.py": "def test_x():\n    pass\n", "lib3/helpers3.py": "X = 1\n", "pkgb/__init__.py": "X = 1\n",
			"src/nsp/core.py": "X = 1\n", "src/mypkg/__init__.py": "", "lib3/utils3.py": "X = 1\n",
			"src/nsp/deep/core.py": "X = 1\n", "pkgr/__init__.py": "", "pkgr/tests/__init__.py": "", "pkgr/tests/test_r.py": "from .. import core\n\ndef test_r():\n    assert core\n",
			"pkgz/__init__.py": "", "pkgz/tests/__init__.py": "", "pkgz/tests/test_z.py": "def test_z():\n    pass\n",
			"pkgm/tests/__init__.py": "", "pkgm/tests/test_m.py": "def test_m():\n    pass\n", "tests/hp/__init__.py": "", "tests/hp/h.py": "X = 1\n", "tests/helpers4.py": "from deepdiff import DeepDiff\n"})
		plain(t, src, "add", "--all")
		plain(t, src, "rm", "-q", "--cached", "conftest.py") // no root conftest: the root's own clause (Opus #267 L2)
	})
	for name, c := range map[string]guardCase{
		"a pythonpath shadow":     {map[string]string{"src/money.py": "def add(a, b):\n    return a + b\n"}, nil, "src/money.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"__init__.py renamed out": {map[string]string{"tests/x.py": "X = 1\n"}, []string{"tests/__init__.py"}, "tests/test_requests.py: a test file's basedir moves"},
		"a module beside an unrelated one (main's floor, Opus #272 C1)": {map[string]string{"tests/utils.py": "X = 1\n"}, nil, "tests/utils.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		// Opus #267 H2 (dotted suffixes), M3 (statements, not lines), L2 (onPath's clauses).
		"H2 a namespace submodule shadow":               {map[string]string{"tests/lib/core.py": "def add(a, b):\n    return a + b\n"}, nil, "tests/lib/core.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"H2 a third-party submodule":                    {map[string]string{"tests/google/protobuf.py": "X = 1\n"}, nil, "tests/google/protobuf.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"M3 after a ;":                                  {map[string]string{"tests/requests.py": "X = 1\n"}, nil, "tests/requests.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"M3 after try:":                                 {map[string]string{"tests/yaml.py": "X = 1\n"}, nil, "tests/yaml.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"M3 a continued import":                         {map[string]string{"tests/toml.py": "X = 1\n"}, nil, "tests/toml.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"L2 a root module":                              {map[string]string{"tests/bank.py": "X = 1\n"}, nil, "tests/bank.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"M1 under a pythonpath tree":                    {map[string]string{"src/google/protobuf.py": "X = 1\n"}, nil, "src/google/protobuf.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"H2 an unimported package beside a root module": {map[string]string{"bank/__init__.py": "X = 1\n"}, nil, "bank/__init__.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"M4 a package against a root module":            {map[string]string{"src/bank/__init__.py": "X = 1\n"}, nil, "src/bank/__init__.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"M4 a module against a root package":            {map[string]string{"tests/pkgb.py": "X = 1\n"}, nil, "tests/pkgb.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		// Mythos #267 r2: H1 (the root's namespace portions), H2 (import a.b), M1 (names relative to sys.path entries).
		"r2 H1 a root namespace portion":                    {map[string]string{"nsp/core.py": "X = 1\n"}, nil, "nsp/core.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"r2 H1 a root third-party namespace":                {map[string]string{"google/protobuf.py": "X = 1\n"}, nil, "google/protobuf.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"r2 H2 import a.b":                                  {map[string]string{"tests/pkgx/sub.py": "X = 1\n"}, nil, "tests/pkgx/sub.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"r2 M1 src/mypkg/json.py (a stated false block)":    {map[string]string{"src/mypkg/json.py": "X = 1\n"}, nil, "a module a condition reads is unread (the module tests/test_requests.py runs code at import"},
		"r2 M1 src/mypkg/utils3.py (a stated false block)":  {map[string]string{"src/mypkg/utils3.py": "X = 1\n"}, nil, "a module a condition reads is unread (the module tests/test_requests.py runs code at import"},
		"r2 M1 src/pkg/sub/money.py (a stated false block)": {map[string]string{"src/pkg/sub/money.py": "X = 1\n"}, nil, "a module a condition reads is unread (the module tests/test_requests.py runs code at import"},
		// The security review's flags on 713eb2e: scope, import reading, case and suffixes.
		"flag a depth-two namespace portion":                                        {map[string]string{"nsp/deep/core.py": "X = 1\n"}, nil, "nsp/deep/core.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"flag a candidate test package's basedir":                                   {map[string]string{"q/w/__init__.py": "", "q/w/test_x.py": "def test_x():\n    pass\n", "q/money.py": "X = 1\n"}, nil, "q/money.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"flag import_module with a constant":                                        {map[string]string{"tests/dynmod.py": "X = 1\n"}, nil, "tests/dynmod.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"flag __import__ with a constant":                                           {map[string]string{"tests/dyn2.py": "X = 1\n"}, nil, "tests/dyn2.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"flag importorskip with a constant":                                         {map[string]string{"tests/dyn3.py": "X = 1\n"}, nil, "tests/dyn3.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"flag from .. import core":                                                  {map[string]string{"pkgr/core.py": "X = 1\n"}, nil, "pkgr/core.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"flag an __init__.py removed outside tests":                                 {nil, []string{"pkgz/__init__.py"}, "pkgz/tests/test_z.py: a test file's basedir moves"},
		"flag a case-only name (imports are case-sensitive) (a stated false block)": {map[string]string{"Money.py": "X = 1\n"}, nil, "a module a condition reads is unread (the module tests/test_requests.py runs code at import"},
		"flag a .pyi beside a .py (never imported; round 8: the base's lib is a namespace package, a stated false block)": {map[string]string{"money.pyi": "def add(a: int, b: int) -> int: ...\n"}, nil, "tests/test_requests.py: a module the tests now import runs code at import"},
		"flag a .so beside a .py": {map[string]string{"money.cpython-312-x86_64-linux-gnu.so": "x"}, nil, "unverifiable: compiled Python, an extension module"},
		// Opus #270: M3 (a basedir moved up), M4 (a judged helper's imports).
		"o M3 an __init__.py added above a test package":            {map[string]string{"pkgm/__init__.py": ""}, nil, "pkgm/tests/test_m.py: a test file's basedir moves"},
		"o M4 a test helper's import":                               {map[string]string{"deepdiff.py": "X = 1\n"}, nil, "deepdiff.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"s2 a test package added (main's rule)":                     {map[string]string{"tests/newpkg/__init__.py": "", "tests/newpkg/test_n.py": "def test_n():\n    pass\n"}, nil, "tests/newpkg/__init__.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"s2 a helper package's __init__.py removed (main's rule)":   {nil, []string{"tests/hp/__init__.py"}, "tests/hp/__init__.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"s4 an import shadowed under a basedir the candidate makes": {map[string]string{"q3/w/__init__.py": "", "q3/w/test_x.py": "def test_x():\n    pass\n", "q3/yaml.py": "X = 1\n"}, nil, "q3/w/__init__.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"s4 the floor under a basedir the candidate makes":          {map[string]string{"q2/w/__init__.py": "", "q2/w/test_x.py": "def test_x():\n    pass\n", "q2/utils.py": "X = 1\n"}, nil, "q2/utils.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
		"L2 a module beside a test file":                            {map[string]string{"tests/helpers3.py": "X = 1\n"}, nil, "tests/helpers3.py: a module a condition reads is unread (the module tests/test_requests.py runs code at import outside the allowlist)"},
	} {
		v := f.guarded(t, c.files, c.gone...)
		if own := pyOwn(v.Blocks); c.why == "" && len(own) > 0 || c.why != "" && !pyFinds(own, c.why) {
			t.Errorf("%s: %q, want %q", name, whats(own), c.why)
		}
	}
}

// Opus #270 H1: main's #266 basename rule is a floor for judged added
// files: sys.path entries the guard does not read (a conftest's
// sys.path.insert, setup.cfg's package_dir). M1: an ini pythonpath with
// a quote or a backslash is unread; python_paths and site_dirs are read.
func TestPyGuardShadowFloor(t *testing.T) {
	for name, base := range map[string]map[string]string{
		"S1 a conftest's sys.path.insert": {"conftest.py": "import sys\nsys.path.insert(0, 'app')\n", "app/utils.py": "X = 1\n", "app/money2.py": "import utils\n\ndef total():\n    return utils.X\n",
			"tests/test_total.py": "from money2 import total\n\ndef test_t():\n    assert total() == 1\n"},
		"S2 setup.cfg's package_dir": {"setup.cfg": "[options]\npackage_dir =\n    =lib\n", "lib/utils.py": "X = 1\n", "lib/money2.py": "import utils\n\ndef total():\n    return utils.X\n", "tests/test_total.py": "from money2 import total\n\ndef test_t():\n    assert total() == 1\n"},
	} {
		f := newFixtureWith(t, func(src string) {
			write(t, src, base)
			plain(t, src, "add", "--all")
		})
		if v := f.guarded(t, map[string]string{"tests/utils.py": "X = 2\n"}); !pyFinds(pyOwn(v.Blocks), "tests/utils.py: ") { // unread since #272's split
			t.Errorf("%s: %q, want blocked", name, whats(pyOwn(v.Blocks)))
		}
	}
	src := newFixtureWith(t, func(src string) { // Mythos #270 r2 M2: in-package tests, src/ their basedir
		write(t, src, map[string]string{"src/pkg/__init__.py": "", "src/pkg/tests/__init__.py": "", "src/pkg/tests/test_a.py": "def test_a():\n    pass\n", "app/utils.py": "X = 1\n"})
		plain(t, src, "add", "--all")
	})
	if v := src.guarded(t, map[string]string{"src/pkg/utils.py": "X = 2\n"}); len(pyOwn(v.Blocks)) > 0 {
		t.Errorf("r4 M2 src/pkg/utils.py: %q, want none", whats(pyOwn(v.Blocks)))
	}
	for _, v := range []string{`s\rc`, `"sr"c`, `'src'`, "src\rlib", "src\f"} { // a lone CR or a form feed: unread (the review of b5a634d)
		f := newFixtureWith(t, func(src string) {
			write(t, src, map[string]string{"pytest.ini": "[pytest]\npythonpath = " + v + "\n"})
			plain(t, src, "add", "--all")
		})
		if got := f.guarded(t, map[string]string{"money.py": baseFiles["money.py"] + "\n"}); !finds(got.Blocks, "moves collection off its defaults") {
			t.Errorf("an ini pythonpath %s: %q, want unread", v, whats(got.Blocks))
		}
	}
	h := newFixtureWith(t, func(src string) { // Mythos #270 L2: a comment naming pythonpath is no key
		write(t, src, map[string]string{"pytest.ini": "[pytest]\n# see the pythonpath docs, it's long\nminversion = 7\n"})
		plain(t, src, "add", "--all")
	})
	if v := h.guarded(t, map[string]string{"money.py": baseFiles["money.py"] + "\n"}); finds(v.Blocks, "moves collection off its defaults") {
		t.Errorf("m L2 a comment: %q, want read", whats(v.Blocks))
	}
	for name, conf := range map[string]map[string]string{ // the security review of 8f2a20c: line breaks and keys iniconfig and TOML read
		"a quoted TOML key":     {"pyproject.toml": "[tool.pytest.ini_options]\n\"pythonpath\" = [\"src\"]\n"},
		"r4 M4 an inline table": {"pyproject.toml": "[tool.pytest]\nini_options = { pythonpath = [\"src\"] }\n"},
		"r4 M4 a dotted key":    {"pyproject.toml": "[tool.pytest]\nini_options.pythonpath = [\"src\"]\n"},
	} {
		conf["src/zz/core.py"] = "X = 1\n"
		f := newFixtureWith(t, func(src string) {
			write(t, src, conf)
			plain(t, src, "add", "--all")
		})
		if v := f.guarded(t, map[string]string{"zz/core.py": "X = 2\n"}); !pyFinds(pyOwn(v.Blocks), "zz/core.py: an added module shadows the base module src/zz/core.py") {
			t.Errorf("t1 %s: %q, want blocked", name, whats(pyOwn(v.Blocks)))
		}
	}
	for _, key := range []string{"python_paths", "site_dirs"} {
		f := newFixtureWith(t, func(src string) {
			write(t, src, map[string]string{"pytest.ini": "[pytest]\n" + key + " = src\n", "src/zz/core.py": "X = 1\n"})
			plain(t, src, "add", "--all")
		})
		if v := f.guarded(t, map[string]string{"zz/core.py": "X = 2\n"}); !pyFinds(pyOwn(v.Blocks), "zz/core.py: an added module shadows the base module src/zz/core.py") {
			t.Errorf("%s: %q, want blocked", key, whats(pyOwn(v.Blocks)))
		}
	}
}

// Opus #267 M4: an unchanged condition passes when each name it reads is
// bound as in the base; a changed binding blocks. L1: pythonpath entries
// from the config file's directory; an escaped TOML entry is unread.
func TestPyGuardConditionBase(t *testing.T) {
	test := pyT + "import pytest\nIS_WINDOWS = False\n\n@pytest.mark.skipif(IS_WINDOWS, reason='w')\ndef test_w():\n    pass\n"
	f := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"tests/test_money.py": test, "tests/pytest.ini": "[pytest]\npythonpath = ../lib2\n", "lib2/x.py": "X = 1\n"})
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
	if v := f.guarded(t, map[string]string{"lib2/money.py": "X = 1\n"}); !pyFinds(pyOwn(v.Blocks), "lib2/money.py: an added module shadows the base module money.py") {
		t.Errorf("a pythonpath from tests/pytest.ini: %q", whats(pyOwn(v.Blocks)))
	}
	g := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"tests/pytest.toml": "[pytest]\npythonpath = ['\\x73rc']\n"})
		plain(t, src, "add", "--all")
	})
	if v := g.guarded(t, map[string]string{"money.py": baseFiles["money.py"] + "\n"}); !finds(v.Blocks, "moves collection off its defaults") {
		t.Errorf("an escaped TOML pythonpath: %q, want unread", whats(v.Blocks))
	}
	for _, out := range []string{"..", "/opt/lib"} { // the security review's flag: an entry outside the root is unread
		h := newFixtureWith(t, func(src string) {
			write(t, src, map[string]string{"pytest.ini": "[pytest]\npythonpath = " + out + "\n"})
			plain(t, src, "add", "--all")
		})
		if v := h.guarded(t, map[string]string{"money.py": baseFiles["money.py"] + "\n"}); !finds(v.Blocks, "moves collection off its defaults") {
			t.Errorf("a pythonpath outside the root (%s): %q, want unread", out, whats(v.Blocks))
		}
	}
}

// Mythos #267 H1, H2 (input B), H3, H4 and M3: a condition's names and
// the modules they come from; __path__; the src layout.
func TestPyGuardConditionReach(t *testing.T) {
	test := "import pytest\nimport money\nfrom money import add, FAST\n\n@pytest.mark.skipif(FAST, reason='s')\ndef test_add():\n    assert add(1, 2) == 3\n\n@pytest.mark.xfail(money.SLOW, reason='s')\ndef test_zero():\n    pass\n"
	marks := "import pytest\nIS_WINDOWS = False\nSKIP = pytest.mark.skipif(IS_WINDOWS, reason='w')\n"
	f := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"money.py": baseFiles["money.py"] + "SLOW = False\nFAST = False\n", "tests/test_money.py": test, "marks.py": marks,
			"lib/core.py": baseFiles["money.py"], "pkg/__init__.py": "import pkgutil\n__path__ = pkgutil.extend_path(__path__, __name__)\n"})
		plain(t, src, "add", "--all")
	})
	for name, c := range map[string]guardCase{
		"H1 an imported name rebound in the test":             {map[string]string{"tests/test_money.py": strings.Replace(test, "from money import add, FAST\n", "from money import add, FAST\nFAST = 'x'\n", 1)}, nil, "tests/test_money.py: unverifiable: a skipif or xfail condition"},
		"H4 only money.py rebinds FAST":                       {map[string]string{"money.py": baseFiles["money.py"] + "SLOW = False\nFAST = 'x'\n"}, nil, "money.py: a skipif or xfail condition in tests/test_money.py reads FAST"},
		"H4 only money.py rebinds SLOW":                       {map[string]string{"money.py": baseFiles["money.py"] + "SLOW = 'x'\nFAST = False\n"}, nil, "money.py: a skipif or xfail condition in tests/test_money.py reads SLOW"},
		"r2 M2 global rebinds SLOW":                           {map[string]string{"money.py": baseFiles["money.py"] + "SLOW = False\nFAST = False\n\ndef _f():\n    global SLOW\n    SLOW = 'x'\n\n_f()\n"}, nil, "money.py: a skipif or xfail condition in tests/test_money.py reads SLOW"},
		"r2 M2 __path__ by a call":                            {map[string]string{"lib/__init__.py": "__path__.insert(0, 'lib/alt')\n", "lib/alt/core.py": "def add(a, b):\n    return a + b\n"}, nil, "lib/__init__.py: unverifiable: a package's import path, spec or loader bound at import (__path__)"},
		"o M2 a case capture":                                 {map[string]string{"money.py": baseFiles["money.py"] + "SLOW = False\nFAST = False\nmatch 1:\n    case SLOW:\n        pass\n"}, nil, "money.py: a skipif or xfail condition in tests/test_money.py reads SLOW"},
		"o M2 an added star import":                           {map[string]string{"money.py": baseFiles["money.py"] + "SLOW = False\nFAST = False\nfrom other import *\n", "other.py": "FAST = 'x'\n"}, nil, "money.py: a skipif or xfail condition in tests/test_money.py reads FAST"},
		"o L2 (__path__).insert":                              {map[string]string{"lib/__init__.py": "(__path__).insert(0, 'lib/alt')\n"}, nil, "lib/__init__.py: unverifiable: a package's import path, spec or loader bound at import (__path__)"},
		"o L2 del __path__[:]":                                {map[string]string{"lib/__init__.py": "del __path__[:]\n"}, nil, "lib/__init__.py: unverifiable: a package's import path, spec or loader bound at import (__path__)"},
		"s1 a package's __path__ changed from a plain module": {map[string]string{"money.py": baseFiles["money.py"] + "SLOW = False\nFAST = False\nimport lib\nlib.__path__.append('lib/alt')\n"}, nil, "money.py: unverifiable: a package's import path, spec or loader bound at import (__path__)"},
		"s1 __path__ bound in a plain module (main's rule)":   {map[string]string{"money.py": baseFiles["money.py"] + "SLOW = False\nFAST = False\n__path__ = ['alt']\n"}, nil, "money.py: unverifiable: a package's import path, spec or loader bound at import (__path__)"},
		"H3 __path__ in an application package":               {map[string]string{"lib/__init__.py": "__path__ = ['lib/alt']\n", "lib/alt/core.py": "def add(a, b):\n    return a + b\n"}, nil, "lib/__init__.py: unverifiable: a package's import path, spec or loader bound at import (__path__)"},
		"M3 a for target rebinds a condition's name":          {map[string]string{"marks.py": strings.Replace(marks, "SKIP =", "for IS_WINDOWS in ['x']:\n    pass\nSKIP =", 1)}, nil, "marks.py: unverifiable: a skipif or xfail condition"},
		"an unchanged extend_path passes":                     {map[string]string{"pkg/__init__.py": "import pkgutil\n__path__ = pkgutil.extend_path(__path__, __name__)\nX = 1\n"}, nil, ""},
	} {
		v := f.guarded(t, c.files, c.gone...)
		if own := pyOwn(v.Blocks); c.why == "" && len(own) > 0 || c.why != "" && !pyFinds(own, c.why) {
			t.Errorf("%s: %q, want %q", name, whats(own), c.why)
		}
	}
	cyc := newFixtureWith(t, func(src string) { // the security review of 9e748fc: a resolution cycle is unread
		write(t, src, map[string]string{"cyca/__init__.py": "from cycb import CV\n", "cycb/__init__.py": "from cyca import CV\n", "other.py": "X = 1\n",
			"tests/test_c.py": "import pytest\nfrom cyca import CV\n\n@pytest.mark.skipif(CV, reason='r')\ndef test_c():\n    pass\n"})
		plain(t, src, "add", "--all")
	})
	if v := cyc.guarded(t, map[string]string{"other.py": "X = 2\n"}); !pyFinds(pyOwn(v.Blocks), "other.py: a module a condition reads is unread") {
		t.Errorf("s3 a resolution cycle: %q, want blocked", whats(pyOwn(v.Blocks)))
	}
	dyn := newFixtureWith(t, func(src string) { // the security review of 8f2a20c: a star module binding names at run time
		write(t, src, map[string]string{"spk3/__init__.py": "from .dyn import *\n", "spk3/dyn.py": "def __getattr__(name):\n    return False\n",
			"tests/test_d.py": "import pytest\nimport spk3\n\n@pytest.mark.skipif(spk3.PSLOW6, reason='r')\ndef test_d():\n    pass\n"})
		plain(t, src, "add", "--all")
	})
	if v := dyn.guarded(t, map[string]string{"spk3/dyn.py": "def __getattr__(name):\n    return 'x'\n"}); !pyFinds(pyOwn(v.Blocks), "spk3/dyn.py: a module a condition reads is unread") {
		t.Errorf("t3 a star module's __getattr__: %q, want blocked", whats(pyOwn(v.Blocks)))
	}
	kw := newFixtureWith(t, func(src string) { // Mythos #270 r2 M3 (b): a keyword is no chain head
		write(t, src, map[string]string{"starh.py": "from numpy.testing import *\nHAS_X = True\n", "other.py": "X = 1\n",
			"tests/test_k.py": "import pytest\nfrom starh import *\n\n@pytest.mark.skipif(not HAS_X, 1 + 1)\ndef test_k():\n    pass\n"})
		plain(t, src, "add", "--all")
	})
	if v := kw.guarded(t, map[string]string{"other.py": "X = 2\n"}); !pyFinds(pyOwn(v.Blocks), "other.py: a module a condition reads is unread (the module tests/test_k.py has a star import other than from .x import * in a package __init__.py)") { // round 7: a star outside a package __init__.py
		t.Errorf("r4 M3 not through an unknown star: %q, want unread", whats(pyOwn(v.Blocks)))
	}
	lazy := newFixtureWith(t, func(src string) { // Mythos #270 r2 L1: a chain stopping at a module __getattr__
		write(t, src, map[string]string{"lazypkg/__init__.py": "def __getattr__(name):\n    return False\n", "other.py": "X = 1\n",
			"tests/test_l.py": "import pytest\nimport lazypkg\n\n@pytest.mark.skipif(lazypkg.LSLOW, reason='r')\ndef test_l():\n    pass\n"})
		plain(t, src, "add", "--all")
	})
	if v := lazy.guarded(t, map[string]string{"other.py": "X = 2\n"}); !pyFinds(pyOwn(v.Blocks), "other.py: a module a condition reads is unread") {
		t.Errorf("r4 L1 a module __getattr__: %q, want blocked", whats(pyOwn(v.Blocks)))
	}
	for name, dyn := range map[string]string{"exec": "exec('PSLOW7 = False')\n", "a call in an assignment's value": "X = globals().update({'PSLOW7': False})\n", "locals()": "locals()['PSLOW7'] = False\n"} { // the review of b5a634d: an allowlist
		u := newFixtureWith(t, func(src string) {
			write(t, src, map[string]string{"spk4/__init__.py": "from .dyn import *\n", "spk4/dyn.py": dyn, "other.py": "X = 1\n",
				"tests/test_u.py": "import pytest\nimport spk4\n\n@pytest.mark.skipif(spk4.PSLOW7, reason='r')\ndef test_u():\n    pass\n"})
			plain(t, src, "add", "--all")
		})
		if v := u.guarded(t, map[string]string{"other.py": "X = 2\n"}); !pyFinds(pyOwn(v.Blocks), "other.py: a module a condition reads is unread") {
			t.Errorf("u1 a star module binding by %s: %q, want blocked", name, whats(pyOwn(v.Blocks)))
		}
	}
	g := newFixtureWith(t, func(src string) { // Mythos #267 H2, input B: the src layout
		plain(t, src, "mv", "money.py", "src_money.py")
		write(t, src, map[string]string{"pytest.ini": "[pytest]\npythonpath = src\n", "src/money.py": baseFiles["money.py"]})
		plain(t, src, "rm", "-q", "--cached", "src_money.py")
		plain(t, src, "add", "--all")
	})
	if v := g.guarded(t, map[string]string{"money.py": "def add(a, b):\n    return a + b\n"}); !pyFinds(pyOwn(v.Blocks), "money.py: an added module shadows the base module src/money.py") {
		t.Errorf("the src layout: %q", whats(pyOwn(v.Blocks)))
	}
}

// Mythos #267 r2 H3: the four base shapes a condition reads a changed
// module through: a relative import, a re-export through an unchanged
// __init__.py, a def- or class-bound name, a dotted or star import.
func TestPyGuardConditionShapes(t *testing.T) {
	test := "import pytest\nimport cfgmod\nimport clsmod\nimport objmod\nimport getmod\nimport strmod\nimport dotted.config\nfrom pkg import PSLOW\nfrom . import flags\nfrom starmod import *\n" +
		"import lmod\nimport fmod\nfrom .marks import SKIPH\nfrom pkg import PSLOW2, settings\nfrom pkgs import SSLOW\nfrom gpu1 import HAS_GPU\nimport gpu2\nimport gpu3\nimport gpu4\nimport gpu5\nimport gpu6\nimport spk\nfrom spk2 import *\nfrom extmod import EXSLOW\nLSLOW = lmod.SLOW\n\ndef _slow():\n    return fmod.FSLOW\n\n" +
		"@pytest.mark.skipif(LSLOW, reason='r')\n@pytest.mark.skipif(_slow(), reason='r')\n@SKIPH\n@pytest.mark.skipif(PSLOW2, reason='r')\n@pytest.mark.skipif(settings.S4, reason='r')\n@pytest.mark.skipif(SSLOW, reason='r')\n@pytest.mark.skipif(HAS_GPU, reason='r')\n@pytest.mark.skipif(gpu2.PSLOW3, reason='r')\n@pytest.mark.skipif(gpu3.CSLOW, reason='r')\n@pytest.mark.skipif(gpu4.HAS_GPU4, reason='r')\n@pytest.mark.skipif(gpu5.HAS5, reason='r')\n@pytest.mark.skipif(gpu6.HAS6, reason='r')\n@pytest.mark.skipif(spk.PSLOW4, reason='r')\n@pytest.mark.skipif(PSLOW5, reason='r')\n@pytest.mark.skipif(EXSLOW, reason='r')\n" +
		"@pytest.mark.skipif(flags.RSLOW, reason='r')\n@pytest.mark.skipif(PSLOW, reason='r')\n@pytest.mark.skipif(cfgmod.flag(), reason='r')\n" +
		"@pytest.mark.skipif(clsmod.Cfg.SLOW, reason='r')\n@pytest.mark.skipif(objmod.cfg.SLOW, reason='r')\n@pytest.mark.skipif(getattr(getmod, 'X'), reason='r')\n@pytest.mark.skipif('strmod.check()', reason='r')\n@pytest.mark.skipif(dotted.config.DSLOW, reason='r')\n@pytest.mark.skipif(STAR, reason='r')\ndef test_s():\n    pass\n"
	cfg, cls, obj := "def flag():\n    return False\n", "class Cfg:\n    SLOW = False\n", "import types\ncfg = types.SimpleNamespace(SLOW=False)\n"
	gpu1 := "def _detect():\n    return False\n\nHAS_GPU = _detect()\n"
	gpu2 := "import functools\n\ndef _g(x):\n    return False\n\nPSLOW3 = functools.partial(_g, 1)\n"
	gpu3 := "class _Cfg:\n    slow = False\n\nCSLOW = _Cfg().slow\n"
	gpu5 := "def _probe():\n    return None\n\ntry:\n    _probe()\nexcept Exception:\n    HAS5 = False\n"
	gpu6 := "def _p6(x):\n    return False\n\nfor x in [1]:\n    if _p6(x):\n        break\nelse:\n    HAS6 = False\n"
	gpu4 := "def _detect():\n    return False\n\nif _detect():\n    pass\nelse:\n    HAS_GPU4 = False\n"
	f := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"tests/__init__.py": "", "tests/flags.py": "RSLOW = False\n", "tests/test_shapes.py": test, "pkg/__init__.py": "from .config import PSLOW\nfrom .conf import REAL as PSLOW2\nfrom . import conf4 as settings\n", "pkg/conf.py": "REAL = False\n", "pkgs/__init__.py": "from .sconf import *\n", "gpu1.py": gpu1, "gpu2.py": gpu2, "gpu3.py": gpu3, "gpu4.py": gpu4, "gpu5.py": gpu5, "gpu6.py": gpu6,
			"spk/__init__.py": "from .sconf2 import *\n", "spk/sconf2.py": "PSLOW4 = False\n", "spk2/__init__.py": "from .sconf3 import *\n", "spk2/sconf3.py": "PSLOW5 = False\n", "exsrc.py": "EXSLOW = False\n", "pkgs/sconf.py": "SSLOW = False\n", "pkg/conf4.py": "S4 = False\n",
			"lmod.py": "SLOW = False\n", "fmod.py": "FSLOW = False\n", "hmod.py": "HSLOW = False\n", "tests/marks.py": "import pytest\nfrom hmod import HSLOW\nSKIPH = pytest.mark.skipif(HSLOW, reason='r')\n",
			"pkg/config.py": "PSLOW = False\n", "cfgmod.py": cfg, "clsmod.py": cls, "objmod.py": obj, "getmod.py": "X = False\n", "strmod.py": "def check():\n    return False\n", "dotted/__init__.py": "", "dotted/config.py": "DSLOW = False\n", "starmod.py": "STAR = False\n"})
		plain(t, src, "add", "--all")
	})
	for name, c := range map[string]guardCase{
		"i a relative import":                                  {map[string]string{"tests/flags.py": "RSLOW = 'x'\n"}, nil, "tests/flags.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"ii a re-export":                                       {map[string]string{"pkg/config.py": "PSLOW = 'x'\n"}, nil, "pkg/config.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"iii a def body":                                       {map[string]string{"cfgmod.py": strings.Replace(cfg, "return False", "return 'x'", 1)}, nil, "cfgmod.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"iii a class body":                                     {map[string]string{"clsmod.py": strings.Replace(cls, "SLOW = False", "SLOW = 'x'", 1)}, nil, "clsmod.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"iii an attribute read past its name":                  {map[string]string{"objmod.py": obj + "\ndef _f():\n    cfg.SLOW = 'x'\n\n_f()\n"}, nil, "objmod.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"flag a module read whole":                             {map[string]string{"getmod.py": "X = 'x'\n"}, nil, "getmod.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"flag a string condition":                              {map[string]string{"strmod.py": "def check():\n    return 'x'\n"}, nil, "strmod.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"o H2 R1 a local binding":                              {map[string]string{"lmod.py": "SLOW = 'x'\n"}, nil, "lmod.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"o H2 R8 a same-file function":                         {map[string]string{"fmod.py": "FSLOW = 'x'\n"}, nil, "fmod.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"o H2 R2 a condition in a helper":                      {map[string]string{"hmod.py": "HSLOW = 'x'\n"}, nil, "hmod.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"o H2 R10 a renamed re-export":                         {map[string]string{"pkg/conf.py": "REAL = 'x'\n"}, nil, "pkg/conf.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"o H2 R11 a module re-exported":                        {map[string]string{"pkg/conf4.py": "S4 = 'x'\n"}, nil, "pkg/conf4.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"ii a re-export through a star import (the name rule)": {map[string]string{"pkgs/sconf.py": "SSLOW = 'x'\n"}, nil, "pkgs/sconf.py: a star source of pkgs changed (its exports may move the package's submodules)"},
		// Mythos #270: H1 (a name bound by a same-file call), H3 (resolve through star imports).
		"m H1 a same-file def's body":                                        {map[string]string{"gpu1.py": strings.Replace(gpu1, "return False", "return 'x'", 1)}, nil, "gpu1.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"r4 M5 an unchanged def body (pyReach)":                              {map[string]string{"gpu1.py": gpu1 + "OTHER = 1\n"}, nil, "gpu1.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"m H1 functools.partial":                                             {map[string]string{"gpu2.py": strings.Replace(gpu2, "return False", "return 'x'", 1)}, nil, "gpu2.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"m H1 _Cfg().slow":                                                   {map[string]string{"gpu3.py": strings.Replace(gpu3, "slow = False", "slow = 'x'", 1)}, nil, "gpu3.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"u2 a flag set in an except":                                         {map[string]string{"gpu5.py": strings.Replace(gpu5, "return None", "raise ValueError", 1)}, nil, "gpu5.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"u2 a flag set in a for's else":                                      {map[string]string{"gpu6.py": strings.Replace(gpu6, "return False", "return True", 1)}, nil, "gpu6.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"t2 a flag bound under an if":                                        {map[string]string{"gpu4.py": strings.Replace(gpu4, "return False", "return 'x'", 1)}, nil, "gpu4.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"m H3 pkg.PSLOW4 through a star":                                     {map[string]string{"spk/sconf2.py": "PSLOW4 = 'x'\n"}, nil, "spk/sconf2.py: a star source of spk changed (its exports may move the package's submodules)"},
		"ii a name re-exported by a module not in the base (the name rule)":  {map[string]string{"exsrc.py": "EXSLOW = 'x'\n"}, nil, "exsrc.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"m H3 a two-level star":                                              {map[string]string{"spk2/sconf3.py": "PSLOW5 = 'x'\n"}, nil, "spk2/sconf3.py: a star source of spk2 changed (its exports may move the package's submodules)"},
		"iv a dotted import":                                                 {map[string]string{"dotted/config.py": "DSLOW = 'x'\n"}, nil, "dotted/config.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"iv a star import":                                                   {map[string]string{"starmod.py": "STAR = 'x'\n"}, nil, "starmod.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
		"an unrelated change to a star one is unread (a stated false block)": {map[string]string{"starmod.py": "STAR = False\nOTHER = 1\n"}, nil, "starmod.py: a module a condition reads is unread (the module tests/marks.py runs code at import outside the allowlist)"},
	} {
		v := f.guarded(t, c.files, c.gone...)
		if own := pyOwn(v.Blocks); c.why == "" && len(own) > 0 || c.why != "" && !pyFinds(own, c.why) {
			t.Errorf("%s: %q, want %q", name, whats(own), c.why)
		}
	}
}

// Main's r4 M3 ("an unknown star before a binding one") in its own base: an
// absolute star in a package __init__.py is not inert (P14c-2a-star), so a
// condition through pkgn is unread. It was in TestPyGuardConditionShapes'
// shared base, where that unread condition would block every row.
func TestPyGuardUnknownStar(t *testing.T) {
	g := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"pkgn/__init__.py": "from numpy import *\nfrom .config import *\n", "pkgn/config.py": "SLOWN = False\n",
			"tests/test_n.py": "import pytest\nimport pkgn\n\n@pytest.mark.skipif(pkgn.SLOWN, reason='r')\ndef test_n():\n    pass\n"})
		plain(t, src, "add", "--all")
	})
	if v := g.guarded(t, map[string]string{"pkgn/config.py": "SLOWN = 'x'\n"}); !pyFinds(pyOwn(v.Blocks), "pkgn/config.py: a module a condition reads is unread") {
		t.Errorf("r4 M3 an unknown star before a binding one: %q", whats(pyOwn(v.Blocks)))
	}
}

// Mythos #270 H1 in passes: a flag a same-file def computes; H2: a
// condition in a def or class body reading a local name or a parameter.
func TestPyGuardConditionBodies(t *testing.T) {
	marks := "import pytest\n\ndef _detect():\n    return False\n\nFLAG = _detect()\nSKIP = pytest.mark.skipif(FLAG, reason='r')\n"
	m := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"marks2.py": marks})
		plain(t, src, "add", "--all")
	})
	if v := m.guarded(t, map[string]string{"marks2.py": strings.Replace(marks, "return False", "return 'x'", 1)}); !pyFinds(pyOwn(v.Blocks), "marks2.py: unverifiable: a skipif or xfail condition") {
		t.Errorf("m H1 passes: FLAG = _detect(), the body changed: %q, want blocked", whats(pyOwn(v.Blocks)))
	}
	xf := "import pytest\n\ndef _xf():\n    return pytest.mark.xfail\n" // Mythos #270 r2 H1: a factory in the base conftest, a fixture added calling it
	h := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"tests/conftest.py": xf})
		plain(t, src, "add", "--all")
	})
	if v := h.guarded(t, map[string]string{"tests/conftest.py": xf + "\n@pytest.fixture(autouse=True)\ndef fx(request):\n    request.applymarker(_xf()('x'))\n"}); !pyFinds(pyOwn(v.Blocks), "tests/conftest.py: unverifiable: a skipif or xfail condition") {
		t.Errorf("r4 H1 a two-line factory in a fixture: %q, want blocked", whats(pyOwn(v.Blocks)))
	}
	m3 := "import functools\nimport pytest\n\n@functools.lru_cache\ndef _detect():\n    return False\n\nFLAG = _detect()\nSKIP = pytest.mark.skipif(FLAG, reason='r')\n\ndef other():\n    return 1\n"
	k := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"marks3.py": m3})
		plain(t, src, "add", "--all")
	})
	if v := k.guarded(t, map[string]string{"marks3.py": strings.Replace(m3, "return 1", "return 2", 1)}); len(pyOwn(v.Blocks)) > 0 {
		t.Errorf("r4 M5 an unchanged def body passes: %q", whats(pyOwn(v.Blocks)))
	}
	if v := k.guarded(t, map[string]string{"marks3.py": strings.Replace(m3, "@functools.lru_cache", "@functools.cache", 1)}); !pyFinds(pyOwn(v.Blocks), "marks3.py: unverifiable: a skipif or xfail condition") {
		t.Errorf("r4 M5 a changed decorator blocks: %q", whats(pyOwn(v.Blocks)))
	}
	f := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"b1.py": "SLOW = False\n", "b2.py": "SLOW = False\n", "b3.py": "SLOW = False\n", "b4.py": "SLOW = False\n", "b5.py": "SLOW = False\n", "b6.py": "SLOW = False\n", "b7.py": "SLOW = False\n",
			"tests/conftest.py": "import pytest\nimport b1\nimport b2\nimport b5\n\n@pytest.fixture(autouse=True)\ndef fx5(request):\n    request.applymarker(_xf(b5.SLOW))\n\ndef _xf(c):\n    return pytest.mark.xfail(c)\n\n@pytest.fixture(autouse=True)\ndef fx(request):\n    s = b1.SLOW\n    request.applymarker(pytest.mark.xfail(s))\n\n" +
				"def pytest_collection_modifyitems(items):\n    flag = b2.SLOW\n    for item in items:\n        item.add_marker(pytest.mark.skipif(flag, reason='r'))\n",
			"tests/test_b.py": "import pytest\nimport b3\nimport b4\n\nclass TestX:\n    slow = b3.SLOW\n    pytestmark = pytest.mark.skipif(slow, reason='r')\n\n    def test_x(self):\n        pass\n\n" +
				"def _mk(flag):\n    return pytest.mark.skipif(flag, reason='r')\n\nSKIP = _mk(b4.SLOW)\n\n@SKIP\ndef test_y():\n    pass\n\nimport b6\nSKIP6 = _mk(flag=b6.SLOW)\n\nimport b7\n\ndef outer():\n    def inner(c):\n        return pytest.mark.xfail(c)\n    return inner\n\n_mk7 = outer()\nSKIP7 = _mk7(b7.SLOW)\n"})
		plain(t, src, "add", "--all")
	})
	for name, c := range map[string]guardCase{
		"m H2 a fixture's local":             {map[string]string{"b1.py": "SLOW = 'x'\n"}, nil, "b1.py: a module a condition reads is unread (the module tests/test_b.py runs code at import outside the allowlist)"},
		"m H2 a hook's local":                {map[string]string{"b2.py": "SLOW = 'x'\n"}, nil, "b2.py: a module a condition reads is unread (the module tests/test_b.py runs code at import outside the allowlist)"},
		"m H2 a class's pytestmark":          {map[string]string{"b3.py": "SLOW = 'x'\n"}, nil, "b3.py: a module a condition reads is unread (the module tests/test_b.py runs code at import outside the allowlist)"},
		"r4 M1 a caller above its factory":   {map[string]string{"b5.py": "SLOW = 'x'\n"}, nil, "b5.py: a module a condition reads is unread (the module tests/test_b.py runs code at import outside the allowlist)"},
		"r4 M6 a factory's keyword argument": {map[string]string{"b6.py": "SLOW = 'x'\n"}, nil, "b6.py: a module a condition reads is unread (the module tests/test_b.py runs code at import outside the allowlist)"},
		"r4 L6 a factory a factory returns":  {map[string]string{"b7.py": "SLOW = 'x'\n"}, nil, "b7.py: a module a condition reads is unread (the module tests/test_b.py runs code at import outside the allowlist)"},
		"m H2 a mark factory":                {map[string]string{"b4.py": "SLOW = 'x'\n"}, nil, "b4.py: a module a condition reads is unread (the module tests/test_b.py runs code at import outside the allowlist)"},
	} {
		v := f.guarded(t, c.files, c.gone...)
		if own := pyOwn(v.Blocks); !pyFinds(own, c.why) {
			t.Errorf("%s: %q, want %q", name, whats(own), c.why)
		}
	}
	old := maxWork // M5: the base conditions' analysis counts toward the work bound
	t.Cleanup(func() { maxWork = old })
	maxWork = 120
	if v := f.guarded(t, map[string]string{"b4.py": "SLOW = False\nX = 1\n"}); !pyFinds(pyOwn(v.Blocks), "over the guard's work bound") {
		t.Errorf("m M5 the work bound: %q", whats(pyOwn(v.Blocks)))
	}
	maxWork = old
}

// P14c-2a-reach (Mythos #270 M2, M3, M4, r2 L4): a condition in an
// application module a judged file imports; the closure across modules; a
// third-party proxy; a container a method call fills.
func TestPyGuardReach(t *testing.T) {
	gate := pyInstallRootsRead // the reader's rows: the gate on, as P14c-2a-plain will turn it (Astra #275 r3); TestPyGuardInstallGate runs the default
	t.Cleanup(func() { pyInstallRootsRead = gate })
	pyInstallRootsRead = true
	test := "import pytest\nfrom testing_utils import require_gpu\nfrom lmod3 import SLOW\nfrom django.conf import settings\nimport lmod4\nfrom lmod5 import FLAG5\nfrom m2 import W\nfrom testing_utils2 import require_gpu2\nfrom pkgm2 import require_gpu3\nfrom lmod6 import SLOW6\nfrom lmod7 import SLOW7\nfrom flags8 import SLOW8\n\n" +
		"@pytest.mark.skipif(SLOW8, reason='r')\n" +
		"@pytest.mark.skipif(pytest.FLAGP, reason='r')\n" +
		"@require_gpu\n@require_gpu2\n@require_gpu3\n@pytest.mark.skipif(SLOW, reason='r')\n@pytest.mark.skipif(W, reason='r')\n@pytest.mark.skipif(SLOW6, reason='r')\n@pytest.mark.skipif(SLOW7, reason='r')\n@pytest.mark.skipif(settings.SKIP2, reason='r')\n@pytest.mark.skipif(settings.FEATURES.gpu, reason='r')\n@pytest.mark.skipif(settings.SKIP3, reason='r')\n@pytest.mark.skipif(FLAG5, reason='r')\n@pytest.mark.skipif(settings.SKIP_SLOW, reason='r')\n@pytest.mark.skipif(lmod4.FLAGS['gpu'], reason='r')\ndef test_r():\n    pass\n"
	u, calc := "def d():\n    return False\n", "def compute():\n    return False\n"
	settings := "from myproject.calc import compute\nSKIP_SLOW = False\n\ndef _d():\n    return False\n\nSKIP2 = _d()\n\nclass FEATURES:\n    gpu = False\n\nSKIP3 = compute()\n"
	util, lmod4 := "def detect():\n    return False\n", "def _detect():\n    return False\n\nFLAGS = {}\nFLAGS.update(gpu=_detect())\n"
	f := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"cfg.py": "NO_GPU = False\n", "testing_utils.py": "import pytest\nimport cfg\nrequire_gpu = pytest.mark.skipif(cfg.NO_GPU, reason='g')\n",
			"util.py": util, "lmod3.py": "from util import detect\nSLOW = detect()\n", "myproject/__init__.py": "", "myproject/settings.py": settings, "myproject/calc.py": calc,
			"lmod4.py": lmod4, "tests/test_r.py": test,
			"lmod5.py": "from mid5 import flag5\nFLAG5 = flag5\n", "mid5.py": "from util5 import detect5\nflag5 = detect5()\n", "util5.py": "def detect5():\n    return False\n",
			"u.py": u, "m1.py": "import u\nV = u.d()\n", "m2.py": "import m1\nW = m1.V\n",
			"testing_utils2.py": "from marks2 import require_gpu2\n", "marks2.py": "import pytest\nimport cfg2\nrequire_gpu2 = pytest.mark.skipif(cfg2.NO_GPU2, reason='g')\n", "cfg2.py": "NO_GPU2 = False\n",
			"pkgm2/__init__.py": "from .marks import require_gpu3\n", "pkgm2/marks.py": "import pytest\nimport cfg3\nrequire_gpu3 = pytest.mark.skipif(cfg3.NO_GPU3, reason='g')\n", "cfg3.py": "NO_GPU3 = False\n",
			"lmod6.py": "from util6 import *\nSLOW6 = detect6()\n", "util6.py": "def detect6():\n    return False\n", "lmod7.py": "from cfg7 import *\nSLOW7 = NO_GPU7\n", "cfg7.py": "NO_GPU7 = False\n",
			"conftest.py": "import pytest\nimport cfgp\npytest.FLAGP = cfgp.PVAL\n", "cfgp.py": "PVAL = False\n",
			"util8.py": "def detect8():\n    return False\n", "leaf8.py": "from util8 import detect8\nV8 = detect8()\n", "bridge8.py": "import leaf8\n", "flags8.py": "from bridge8 import *\nSLOW8 = leaf8.V8\n"})
		plain(t, src, "add", "--all")
	})
	// P14c-2a-widen r2 (Opus #281, decision A): m1.py, lmod3.py, settings.py, lmod4.py and flags8.py run
	// code at import outside pyAllowed, so each may write every base module it imports: the conditions are unread.
	for name, c := range map[string]guardCase{
		"M2 a condition in an application module":      {map[string]string{"cfg.py": "NO_GPU = 'x'\n"}, nil, "cfg.py: a module a condition reads is unread"},
		"M3 the closure across modules":                {map[string]string{"util.py": strings.Replace(util, "return False", "return 'x'", 1)}, nil, "util.py: a module a condition reads is unread"},
		"M3 two modules away":                          {map[string]string{"util5.py": "def detect5():\n    return 'x'\n"}, nil, "util5.py: a module a condition reads is unread"},
		"r-12 an attribute after an imported module":   {map[string]string{"u.py": "def d():\n    return 'x'\n"}, nil, "u.py: a module a condition reads is unread"},
		"r-01 a condition re-exported":                 {map[string]string{"cfg2.py": "NO_GPU2 = 'x'\n"}, nil, "cfg2.py: a module a condition reads is unread"},
		"r-02 re-exported by a package":                {map[string]string{"cfg3.py": "NO_GPU3 = 'x'\n"}, nil, "cfg3.py: a module a condition reads is unread"},
		"r-03 across a star import":                    {map[string]string{"util6.py": "def detect6():\n    return 'x'\n"}, nil, "util6.py: a module a condition reads is unread"},
		"r-24 a name across a star import":             {map[string]string{"cfg7.py": "NO_GPU7 = 'x'\n"}, nil, "cfg7.py: a module a condition reads is unread"},
		"r-13 a proxy name a def computes":             {map[string]string{"myproject/settings.py": strings.Replace(settings, "    return False", "    return 'x'", 1)}, nil, "myproject/settings.py: a module a condition reads is unread"},
		"r-14 a proxy chain's inner name":              {map[string]string{"myproject/settings.py": strings.Replace(settings, "gpu = False", "gpu = 'x'", 1)}, nil, "myproject/settings.py: a module a condition reads is unread"},
		"r-15 a proxy name computed in another module": {map[string]string{"myproject/calc.py": "def compute():\n    return 'x'\n"}, nil, "myproject/calc.py: a module a condition reads is unread"},
		"v1 a value parked on pytest":                  {map[string]string{"cfgp.py": "PVAL = 'x'\n"}, nil, "cfgp.py: a module a condition reads is unread"},
		"a-H3 a star import keeps the whole chain":     {map[string]string{"util8.py": "def detect8():\n    return 'True'\n"}, nil, "util8.py: a module a condition reads is unread"},
		"M3 an unrelated change passes":                {map[string]string{"util.py": util + "OTHER = 1\n"}, nil, "util.py: a module a condition reads is unread"},
		"M4 a third-party proxy":                       {map[string]string{"myproject/settings.py": strings.Replace(settings, "SKIP_SLOW = False", "SKIP_SLOW = 'x'", 1)}, nil, "myproject/settings.py: a module a condition reads is unread"},
		"r2 L4 a container a method call fills":        {map[string]string{"lmod4.py": strings.Replace(lmod4, "gpu=_detect()", "gpu='x'", 1)}, nil, "lmod4.py: a module a condition reads is unread"},
	} {
		v := f.guarded(t, c.files, c.gone...)
		if own := pyOwn(v.Blocks); c.why == "" && len(own) > 0 || c.why != "" && !pyFinds(own, c.why) {
			t.Errorf("%s: %q, want %q", name, whats(own), c.why)
		}
	}
	// M1: sys.path entries the guard did not model; an unread one blocks every Python change.
	for name, base := range map[string]map[string]string{
		"M1 setup.cfg package_dir":                   {"setup.cfg": "[options]\npackage_dir =\n    =lib\n"},
		"M1 setup.cfg packages.find where":           {"setup.cfg": "[options]\npackages = find:\n\n[options.packages.find]\nwhere = lib\n"},
		"M1 pyproject package-dir":                   {"pyproject.toml": "[tool.setuptools]\npackage-dir = {\"\" = \"lib\"}\n"},
		"M1 pyproject packages.find where":           {"pyproject.toml": "[tool.setuptools.packages.find]\nwhere = [\"lib\"]\n"},
		"M1 a conftest's sys.path.insert":            {"conftest.py": "import sys\nsys.path.insert(0, 'lib')\n"},
		"M1 a conftest's sys.path from its __file__": {"tests/conftest.py": "import os\nimport sys\nsys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'lib'))\n"},
	} {
		base["lib/x.py"], base["tests/test_y.py"] = "X = 1\n", "import yaml\n\ndef test_y():\n    assert yaml\n"
		g := newFixtureWith(t, func(src string) {
			write(t, src, base)
			plain(t, src, "add", "--all")
		})
		want := "lib/yaml.py: an added module shadows a module tests/test_y.py imports" // an install root, read exactly (P14c-2a-roots)
		if strings.Contains(name, "sys.path") {
			want = "lib/yaml.py: unverifiable: the base adds sys.path entries the guard does not read" // as on main (Astra #275: P14c-2a-syspath)
		}
		if v := g.guarded(t, map[string]string{"lib/yaml.py": "X = 1\n"}); !pyFinds(pyOwn(v.Blocks), want) {
			t.Errorf("%s: %q, want blocked", name, whats(pyOwn(v.Blocks)))
		}
	}
	for name, base := range map[string]map[string]string{ // the readers (Opus #272 (b), (c)): read, or unread
		"rd pyproject package-dir as a table":      {"pyproject.toml": "[tool.setuptools.package-dir]\n\"\" = \"lib\"\n"},
		"rd pyproject package-dir as a dotted key": {"pyproject.toml": "tool.setuptools.package-dir = {\"\" = \"lib\"}\n"},
		"rd pyproject package-dir quoted":          {"pyproject.toml": "[tool.setuptools]\n\"package-dir\" = {\"\" = \"lib\"}\n"},
		"rd setup.cfg package_dir with a colon":    {"setup.cfg": "[options]\npackage_dir:\n    =lib\n"},
		"rd setup.cfg Package_Dir":                 {"setup.cfg": "[options]\nPackage_Dir =\n    =lib\n"},
		"rd poetry":                                {"pyproject.toml": "[tool.poetry]\npackages = [{include = \"x\", from = \"lib\"}]\n"},
		"rd hatch":                                 {"pyproject.toml": "[tool.hatch.build.targets.wheel.sources]\n\"lib\" = \"\"\n"},
		"rd another build backend":                 {"pyproject.toml": "[build-system]\nbuild-backend = \"flit_core.buildapi\"\n"},
		"rd site.addsitedir":                       {"conftest.py": "import site\nsite.addsitedir('lib')\n"},
		"rd from sys import path":                  {"conftest.py": "from sys import path\npath.insert(0, 'lib')\n"},
		"rd import sys as":                         {"conftest.py": "import sys as _s\n_s.path.insert(0, 'lib')\n"},
		"rd getattr(sys":                           {"conftest.py": "import sys\ngetattr(sys, 'path').insert(0, 'lib')\n"},
		"rd sys.meta_path":                         {"conftest.py": "import sys\nimport finder\nsys.meta_path.insert(0, finder.F())\n"},
		"v2 import_module('sys')":                  {"conftest.py": "import importlib\nimportlib.import_module('sys').path[0:0] = ['lib']\n"},
		"v2 sys.__dict__":                          {"conftest.py": "import sys\nsys.__dict__['path'].insert(0, 'lib')\n"},
		"a-H2 dirname of a literal":                {"conftest.py": "import os\nimport sys\nsys.path.insert(0, os.path.dirname('lib/anchor'))\n"},
		"a-H2 abspath of a literal":                {"conftest.py": "import os\nimport sys\nsys.path.insert(0, os.path.abspath('lib'))\n"},
		"a-M2 a .pth in the tree":                  {"vendor/local.pth": "../lib\n"},
		"w2 a quoted TOML table name":              {"pyproject.toml": "[tool.\"setuptools\"]\npackage-dir = {\"\" = \"lib\"}\n"},
		"w2 a key dotted under [tool]":             {"pyproject.toml": "[tool]\nsetuptools.package-dir = {\"\" = \"lib\"}\n"},
		"w3 sys built from a string":               {"conftest.py": "__import__('s' + 'ys').path.insert(0, 'lib')\n"},
		"w3 path through getattr":                  {"conftest.py": "import importlib\ngetattr(importlib.import_module('s' + 'ys'), 'path').insert(0, 'lib')\n"},
		"rd a module a conftest imports":           {"conftest.py": "import helper\n", "helper.py": "import sys\nsys.path.insert(0, 'lib')\n"},
		"rd a pytest_plugins module":               {"conftest.py": "pytest_plugins = ['myplugin']\n", "myplugin.py": "import sys\nsys.path.insert(0, 'lib')\n"},
	} {
		base["lib/x.py"], base["tests/test_y.py"] = "X = 1\n", "import yaml\n\ndef test_y():\n    assert yaml\n"
		g := newFixtureWith(t, func(src string) {
			write(t, src, base)
			plain(t, src, "add", "--all")
		})
		if v := g.guarded(t, map[string]string{"lib/yaml.py": "X = 1\n"}); !pyFinds(pyOwn(v.Blocks), "lib/yaml.py: ") {
			t.Errorf("%s: %q, want blocked", name, whats(pyOwn(v.Blocks)))
		}
	}
	for name, base := range map[string]map[string]string{ // the src/ layout, a root (P14c-2a-roots)
		"rt a src layout of modules": {"src/x.py": "X = 1\n"},
		"rt a src namespace package": {"src/ns/x.py": "X = 1\n"},
		"rt an editable src layout":  {"pyproject.toml": "[project]\nname = 'x'\n", "src/mypkg/__init__.py": ""},
	} {
		base["tests/test_y.py"] = "import yaml\n\ndef test_y():\n    assert yaml\n"
		g := newFixtureWith(t, func(src string) {
			write(t, src, base)
			plain(t, src, "add", "--all")
		})
		if v := g.guarded(t, map[string]string{"src/yaml.py": "X = 1\n"}); !pyFinds(pyOwn(v.Blocks), "src/yaml.py: an added module shadows a module tests/test_y.py imports") {
			t.Errorf("%s: %q, want blocked", name, whats(pyOwn(v.Blocks)))
		}
	}
	vend := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"tests/conftest.py": "import os\nimport sys\nsys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'vendor', 'lib'))\n",
			"vendor/lib/x.py": "X = 1\n", "tests/test_y.py": "import yaml\n\ndef test_y():\n    assert yaml\n"})
		plain(t, src, "add", "--all")
	})
	if v := vend.guarded(t, map[string]string{"vendor/lib/yaml.py": "X = 1\n"}); !pyFinds(pyOwn(v.Blocks), "vendor/lib/yaml.py: unverifiable: the base adds sys.path entries the guard does not read") {
		t.Errorf("rd a path joined from several literals: %q, want blocked", whats(pyOwn(v.Blocks)))
	}
	for name, c := range map[string][2]string{ // Astra #272 H1: a root never replaces main's binding
		"a-H1 a where in an unrelated section": {"[unrelated]\nwhere = a\n", "cfg.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"a-H1 a module name in two roots":      {"[options]\npackages = find:\n\n[options.packages.find]\nwhere = a\n", "cfg.py: a module a condition reads is unread (the module name feature has more than one file)"},
	} {
		h1 := newFixtureWith(t, func(src string) {
			write(t, src, map[string]string{"setup.cfg": c[0], "a/feature.py": "SKIP = False\n", "feature.py": "from cfg import SLOW as SKIP\n", "cfg.py": "SLOW = False\n",
				"tests/test_x.py": "import pytest, feature\n\n@pytest.mark.skipif(feature.SKIP, reason='r')\ndef test_x():\n    pass\n"})
			plain(t, src, "add", "--all")
		})
		if v := h1.guarded(t, map[string]string{"cfg.py": "SLOW = 'True'\n"}); !pyFinds(pyOwn(v.Blocks), c[1]) {
			t.Errorf("%s: %q, want blocked", name, whats(pyOwn(v.Blocks)))
		}
	}
	for name, c := range map[string][2]map[string]string{ // Astra #272 r2: a namespace prefix under a where root; a module beside its package
		"a2-H1 a namespace-prefix decoy": {{"setup.cfg": "[options]\npackages = find:\n\n[options.packages.find]\nwhere = a\n", "a/feature/flags.py": "SKIP = False\n", "feature.py": "import cfg as flags\n", "cfg.py": "SKIP = False\n",
			"tests/test_x.py": "import pytest, feature\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, {"cfg.py": "SKIP = 'True'\n"}},
		"a2-H1 a holder module beside its package": {{"feature.py": "X = False\n", "feature/__init__.py": "import pytest\nimport cfg\nrequire_gpu = pytest.mark.skipif(cfg.SLOW, reason='r')\n", "cfg.py": "SLOW = False\n",
			"tests/test_x.py": "import feature\n\n@feature.require_gpu\ndef test_x():\n    pass\n"}, {"cfg.py": "SLOW = 'True'\n"}},
		"a2-H1 a collision reached across modules": {{"feature.py": "X = False\n", "feature/__init__.py": "X = False\n", "lmod11.py": "import feature\nV11 = feature.X\n", "cfg.py": "SLOW = False\n",
			"tests/test_x.py": "import pytest\nfrom lmod11 import V11\n\n@pytest.mark.skipif(V11, reason='r')\ndef test_x():\n    pass\n"}, {"cfg.py": "SLOW = 'True'\n"}},
		"rt-H1 a package binding a name beside its submodule": {{"feature/__init__.py": "import cfg as flags\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n",
			"tests/test_x.py": "import pytest, feature\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, {"cfg.py": "SKIP = 'True'\n"}},
		"a2-H2 an absolute join argument": {{"conftest.py": "import os\nimport sys\nsys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), '/work/repo/vendor/lib'))\n",
			"vendor/lib/x.py": "X = 1\n", "tests/test_y.py": "import yaml\n\ndef test_y():\n    assert yaml\n"}, {"vendor/lib/yaml.py": "X = 1\n"}},
	} {
		g := newFixtureWith(t, func(src string) {
			write(t, src, c[0])
			plain(t, src, "add", "--all")
		})
		if v := g.guarded(t, c[1]); !pyFinds(pyOwn(v.Blocks), "unverifiable: ") && !pyFinds(pyOwn(v.Blocks), "is unread") { // the decoy: o2-a2
			t.Errorf("%s: %q, want blocked", name, whats(pyOwn(v.Blocks)))
		}
	}
	for name, c := range map[string]struct {
		base, change map[string]string
		want         string
	}{ // Opus #275: resolution on main's roots (H2), collisions at any depth (H1), the readers (H3, H4); __path__ (M1) moves to P14c-2a-plain
		"o2-P1d a package decoy under src":                            {map[string]string{"src/util/__init__.py": "", "src/util/flags.py": "SKIP = False\n", "util/__init__.py": "import cfg as flags\n", "cfg.py": "SKIP = False\n", "helper.py": "import util\nV = util.flags.SKIP\n", "tests/test_x.py": "import pytest\nfrom helper import V\n\n@pytest.mark.skipif(V, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: "},
		"o2-P1e a package decoy under a where root":                   {map[string]string{"setup.cfg": "[options]\npackages = find:\n\n[options.packages.find]\nwhere = a\n", "a/util/__init__.py": "", "a/util/flags.py": "SKIP = False\n", "util/__init__.py": "import cfg as flags\n", "cfg.py": "SKIP = False\n", "helper.py": "import util\nV = util.flags.SKIP\n", "tests/test_x.py": "import pytest\nfrom helper import V\n\n@pytest.mark.skipif(V, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: "},
		"o2-P1f a package decoy under a sys.path root":                {map[string]string{"conftest.py": "import sys\nsys.path.insert(0, 'a')\n", "a/util/__init__.py": "", "a/util/flags.py": "SKIP = False\n", "util/__init__.py": "import cfg as flags\n", "cfg.py": "SKIP = False\n", "helper.py": "import util\nV = util.flags.SKIP\n", "tests/test_x.py": "import pytest\nfrom helper import V\n\n@pytest.mark.skipif(V, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: "},
		"o2-P2 an assumed src root against the proxy rule":            {map[string]string{"src/feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module tests/test_x.py reads a decorator argument from feature, not a repository module)"},
		"o2-P4 a sourceless module beside a src package":              {map[string]string{"feature.pyc": "\x00", "src/feature/__init__.py": "", "src/feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module name feature has more than one file)"},
		"o2-P2h a value parked on a decoy's name":                     {map[string]string{"src/feature/flags.py": "SKIP = False\n", "cfg.py": "PV = False\n", "conftest.py": "import cfg\nimport feature.flags\nfeature.flags.SKIP = cfg.PV\n", "tests/test_x.py": "import pytest, feature\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "PV = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module conftest.py writes [feature . flags . SKIP] at import)"},
		"o2-P10 another backend with a backend-path":                  {map[string]string{"pyproject.toml": "[build-system]\nbuild-backend = \"setuptools_x\"\nbackend-path = [\"_build\"]\n", "tests/test_y.py": "import yaml\n\ndef test_y():\n    assert yaml\n"}, map[string]string{"vendor/yaml.py": "X = 1\n"}, "vendor/yaml.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"o2-P10b setuptools' backend with a backend-path":             {map[string]string{"pyproject.toml": "[build-system]\nbuild-backend = \"setuptools.build_meta\"\nbackend-path = [\"_build\"]\n", "tests/test_y.py": "import yaml\n\ndef test_y():\n    assert yaml\n"}, map[string]string{"vendor/yaml.py": "X = 1\n"}, "vendor/yaml.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"o2-P2b a where in tox.ini":                                   {map[string]string{"tox.ini": "[options]\npackages = find:\n\n[options.packages.find]\nwhere = a\n", "tests/test_y.py": "import yaml\n\ndef test_y():\n    assert yaml\n"}, map[string]string{"a/yaml.py": "X = 1\n"}, "a/yaml.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"o2-P2f a where without packages = find:":                     {map[string]string{"setup.cfg": "[options.packages.find]\nwhere = a\n", "tests/test_y.py": "import yaml\n\ndef test_y():\n    assert yaml\n"}, map[string]string{"a/yaml.py": "X = 1\n"}, "a/yaml.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"o2-P12 a where of two words":                                 {map[string]string{"setup.cfg": "[options]\npackages = find:\n\n[options.packages.find]\nwhere = a b\n", "tests/test_y.py": "import yaml\n\ndef test_y():\n    assert yaml\n"}, map[string]string{"a/yaml.py": "X = 1\n"}, "a/yaml.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"o2-P2c an upper-case section name":                           {map[string]string{"setup.cfg": "[options]\npackages = find:\n\n[Options.Packages.Find]\nwhere = a\n", "tests/test_y.py": "import yaml\n\ndef test_y():\n    assert yaml\n"}, map[string]string{"a/yaml.py": "X = 1\n"}, "a/yaml.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"o2-P2d a nested setup.cfg":                                   {map[string]string{"sub/setup.cfg": "[options]\npackages = find:\n\n[options.packages.find]\nwhere = a\n", "tests/test_y.py": "import yaml\n\ndef test_y():\n    assert yaml\n"}, map[string]string{"sub/a/yaml.py": "X = 1\n"}, "sub/a/yaml.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"o2-P2g a TOML Where key":                                     {map[string]string{"pyproject.toml": "[tool.setuptools.packages.find]\nWhere = [\"a\"]\n", "tests/test_y.py": "import yaml\n\ndef test_y():\n    assert yaml\n"}, map[string]string{"a/yaml.py": "X = 1\n"}, "a/yaml.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"o2-P9 an indented continuation line":                         {map[string]string{"setup.cfg": "[options]\npackages = find:\n\n[options.packages.find]\nexclude =\n    where = a\n", "tests/test_y.py": "import yaml\n\ndef test_y():\n    assert yaml\n"}, map[string]string{"a/yaml.py": "X = 1\n"}, "a/yaml.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"o2-P10c another backend named setuptools_x":                  {map[string]string{"pyproject.toml": "[build-system]\nbuild-backend = \"setuptools_x\"\n", "tests/test_y.py": "import yaml\n\ndef test_y():\n    assert yaml\n"}, map[string]string{"vendor/yaml.py": "X = 1\n"}, "vendor/yaml.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"o2-P1g a package in two roots above a unique submodule":      {map[string]string{"src/util/__init__.py": "", "util/__init__.py": "", "util/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nfrom util.flags import SKIP\n\n@pytest.mark.skipif(SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module name util has more than one file)"},
		"o2-a2 a module beside a namespace portion in a test basedir": {map[string]string{"feature.py": "import cfg as flags\n", "tests/feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module feature holds no submodule flags)"},
		"o3-B1 a module only a sys.path root names":                   {map[string]string{"conftest.py": "import sys\nsys.path.insert(0, 'lib')\n", "lib/flags.py": "import cfg\nSKIP = cfg.PV\n", "cfg.py": "PV = False\n", "tests/test_x.py": "import pytest, flags\n\n@pytest.mark.skipif(flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "PV = 'True'\n"}, "cfg.py: "},
		"o3-B2 a module only a package-dir root names":                {map[string]string{"pyproject.toml": "[tool.setuptools]\npackage-dir = {\"\" = \"lib\"}\n", "lib/flags.py": "import cfg\nSKIP = cfg.PV\n", "cfg.py": "PV = False\n", "tests/test_x.py": "import pytest, flags\n\n@pytest.mark.skipif(flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "PV = 'True'\n"}, "cfg.py: "},
		"o3-B3 a module binding the name by import":                   {map[string]string{"pyproject.toml": "[tool.setuptools]\npackage-dir = {\"\" = \"lib\"}\n", "lib/flags.py": "from cfg import PV as SKIP\n", "cfg.py": "PV = False\n", "tests/test_x.py": "import pytest, flags\n\n@pytest.mark.skipif(flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "PV = 'True'\n"}, "cfg.py: "},
		"o4-H1 a src module above a namespace portion":                {map[string]string{"src/feature.py": "import cfg as flags\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "helper.py": "import feature\nV = feature.flags.SKIP\n", "tests/test_x.py": "import pytest, helper\n\n@pytest.mark.skipif(helper.V, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread"},
		"o4-H1 a src module above a namespace portion, through an inert holder (round 6: a dotted value is not inert)": {map[string]string{"src/feature.py": "import cfg as flags\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "helper.py": "from feature.flags import SKIP as V\n", "tests/test_x.py": "import pytest, helper\n\n@pytest.mark.skipif(helper.V, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module feature is under a root the guard reads for shadows only)"},
		"o4-H2 a src helper importing yaml":                                  {map[string]string{"src/helper.py": "import yaml\nSKIP = yaml.safe_load('false')\n", "tests/test_x.py": "import pytest\nimport helper\n\n@pytest.mark.skipif(helper.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"src/yaml.py": "def safe_load(v):\n    return True\n"}, "src/yaml.py: a module a condition reads is unread"},
		"o4-H2 a root helper importing yaml":                                 {map[string]string{"helper.py": "import yaml\nSKIP = yaml.safe_load('false')\n", "tests/test_x.py": "import pytest\nimport helper\n\n@pytest.mark.skipif(helper.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"yaml.py": "def safe_load(v):\n    return True\n"}, "yaml.py: a module a condition reads is unread (the module helper.py runs code at import outside the allowlist)"},
		"o4-H2 a root helper, yaml added under src":                          {map[string]string{"src/other.py": "X = 1\n", "helper.py": "import yaml\nSKIP = yaml.safe_load('false')\n", "tests/test_x.py": "import pytest\nimport helper\n\n@pytest.mark.skipif(helper.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"src/yaml.py": "def safe_load(v):\n    return True\n"}, "src/yaml.py: a module a condition reads is unread (the module helper.py runs code at import outside the allowlist)"},
		"o4-H3 a package_dir interpolated from [DEFAULT]":                    {map[string]string{"setup.cfg": "[DEFAULT]\nroot = lib\n\n[metadata]\nname = example\nversion = 0.0.0\n\n[options]\npackage_dir = =%(root)s\npy_modules = flags\n", "lib/flags.py": "from cfg import PV as SKIP\n", "cfg.py": "PV = False\n", "tests/test_x.py": "import pytest, flags\n\n@pytest.mark.skipif(flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "PV = 'True'\n"}, "cfg.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"o4-H3 a where interpolated":                                         {map[string]string{"setup.cfg": "[DEFAULT]\nroot = lib\n\n[options]\npackages = find:\n\n[options.packages.find]\nwhere = %(root)s\n", "tests/test_y.py": "import yaml\n\ndef test_y():\n    assert yaml\n"}, map[string]string{"lib/yaml.py": "X = 1\n"}, "lib/yaml.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"o4-H5 a where with a quote":                                         {map[string]string{"setup.cfg": "[options]\npackages = find:\n\n[options.packages.find]\nwhere = lib'\n", "tests/test_y.py": "import yaml\n\ndef test_y():\n    assert yaml\n"}, map[string]string{"lib'/yaml.py": "X = 1\n"}, "lib'/yaml.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"o3-D1 a package binding __spec__ from an import":                    {map[string]string{"feature/__init__.py": "from types import SimpleNamespace as __spec__\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "feature/impl.py": "from cfg import SKIP\n", "tests/test_x.py": "import pytest, feature\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module feature/__init__.py runs code at import outside the allowlist)"},
		"o3-D2 a package assigning __class__":                                {map[string]string{"feature/__init__.py": "__class__ = object\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module feature/__init__.py runs code at import outside the allowlist)"},
		"o3-D3 a package setting an attribute at run time":                   {map[string]string{"feature/__init__.py": "import cfg\nglobals().update(flags=cfg)\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module feature/__init__.py writes by a call or through a target the guard cannot read)"},
		"o3-D4 a package binding __path__ in a walrus":                       {map[string]string{"feature/__init__.py": "X = [__path__ := ['feature/impl']]\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module feature/__init__.py runs code at import outside the allowlist)"},
		"o4-H6 a class body":                                                 {map[string]string{"feature/__init__.py": "class Redirect:\n    __path__.insert(0, 'feature/impl')\n", "feature/flags.py": "SKIP = False\n", "feature/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module feature/__init__.py runs code at import outside the allowlist)"},
		"o4-H6 a subscript":                                                  {map[string]string{"feature/__init__.py": "from ._r import redirect\nX = redirect[0]\n", "feature/flags.py": "SKIP = False\n", "feature/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module feature/__init__.py runs code at import outside the allowlist)"},
		"o4-H6 a subscript of a literal":                                     {map[string]string{"feature/__init__.py": "X = ['feature/impl'][0]\n", "feature/flags.py": "SKIP = False\n", "feature/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module feature/__init__.py runs code at import outside the allowlist)"},
		"o4-H6 a def default with a call":                                    {map[string]string{"feature/__init__.py": "from ._r import g\n\ndef f(x=g()):\n    pass\n", "feature/flags.py": "SKIP = False\n", "feature/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread"},
		"o4-H6 a def annotation with a call":                                 {map[string]string{"feature/__init__.py": "from ._r import g\n\ndef f(x: g()):\n    pass\n", "feature/flags.py": "SKIP = False\n", "feature/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread"},
		"o4-H6 a return annotation with a call":                              {map[string]string{"feature/__init__.py": "from ._r import g\n\ndef f() -> g():\n    pass\n", "feature/flags.py": "SKIP = False\n", "feature/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread"},
		"o4-H6 a decorated def":                                              {map[string]string{"feature/__init__.py": "from ._r import g\n\n@g\ndef f():\n    pass\n", "feature/flags.py": "SKIP = False\n", "feature/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread"},
		"o4-H6 an f-string":                                                  {map[string]string{"feature/__init__.py": "from ._r import g\nX = f\"{g()}\"\n", "feature/flags.py": "SKIP = False\n", "feature/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread"},
		"o4-H6 a plain package with a call is unread (a stated false block)": {map[string]string{"feature/__init__.py": "\"\"\"Doc.\"\"\"\nfrom __future__ import annotations\nimport os\n__all__ = ['flags']\n__version__ = '1.0'\nX = {'a': [1, 0.5, None, True], 'b': [b'x', 'y' 'z']}\n\ndef f(x: int = 1, *a, k='v', **kw) -> str:\n    return g()\n", "feature/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module feature/__init__.py runs code at import outside the allowlist)"},
		"o2-P3c a package setting its own __path__":                          {map[string]string{"feature/__init__.py": "__path__ = ['feature/impl']\n", "feature/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module feature/__init__.py runs code at import outside the allowlist)"},
		"o2-P3d a package changing __path__ in a try":                        {map[string]string{"feature/__init__.py": "try:\n    __path__.insert(0, 'feature/impl')\nexcept NameError:\n    pass\n", "feature/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module feature/__init__.py runs code at import outside the allowlist)"},
		"p-H6a an f-string in a def default":                                 {map[string]string{"feature/__init__.py": "def f(x=f\"{__path__.insert(0, 'feature/impl')}\"):\n    pass\n", "feature/flags.py": "SKIP = False\n", "feature/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module feature/__init__.py runs code at import outside the allowlist)"},
		"p-H6a an f-string concatenated in a def default":                    {map[string]string{"feature/__init__.py": "def f(x=\"prefix\" f\"{__path__.insert(0, 'feature/impl')}\"):\n    pass\n", "feature/flags.py": "SKIP = False\n", "feature/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module feature/__init__.py runs code at import outside the allowlist)"},
		"p-H6a an f-string in a container":                                   {map[string]string{"feature/__init__.py": "X = ['a', (f\"{__path__.insert(0, 'feature/impl')}\",)]\n", "feature/flags.py": "SKIP = False\n", "feature/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module feature/__init__.py runs code at import outside the allowlist)"},
		"p-c a backslash continuation hiding a statement":                    {map[string]string{"feature/__init__.py": "X = 1 \\\n; __path__.insert(0, 'feature/impl')\n", "feature/flags.py": "SKIP = False\n", "feature/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module feature/__init__.py runs code at import outside the allowlist)"},
		"p-c a statement across lines in brackets":                           {map[string]string{"feature/__init__.py": "X = [\n    1,\n    __path__.insert(0, 'feature/impl'),\n]\n", "feature/flags.py": "SKIP = False\n", "feature/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module feature/__init__.py runs code at import outside the allowlist)"},
		"p-c a ; in a string and a quote in a comment stay plain":            {map[string]string{"feature/__init__.py": "X = 'a;__path__.insert(0, \"feature/impl\")'  # it's plain\nY = [\n    1,  # a ; and a \" here\n]\n", "feature/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest, feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    pass\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a skipif or xfail condition in tests/test_x.py reads SKIP, which"},
	} {
		g := newFixtureWith(t, func(src string) {
			write(t, src, c.base)
			plain(t, src, "add", "--all")
		})
		if v := g.guarded(t, c.change); !pyFinds(pyOwn(v.Blocks), c.want) {
			t.Errorf("%s: %q, want %q", name, whats(pyOwn(v.Blocks)), c.want)
		}
	}
	h4 := newFixtureWith(t, func(src string) { // Astra #272 H4: a holder's __getattr__
		write(t, src, map[string]string{"holder10.py": "import marks10\n\ndef __getattr__(name):\n    if name == 'require_gpu10':\n        return marks10.require_gpu10\n    raise AttributeError(name)\n",
			"marks10.py": "import pytest\nimport cfg10\nrequire_gpu10 = pytest.mark.skipif(cfg10.SLOW10, reason='r')\n", "cfg10.py": "SLOW10 = False\n",
			"tests/test_h.py": "from holder10 import require_gpu10\n\n@require_gpu10\ndef test_h():\n    pass\n"})
		plain(t, src, "add", "--all")
	})
	if v := h4.guarded(t, map[string]string{"cfg10.py": "SLOW10 = 'True'\n"}); !pyFinds(pyOwn(v.Blocks), "cfg10.py: a module a condition reads is unread") {
		t.Errorf("a-H4 a holder's __getattr__: %q, want blocked", whats(pyOwn(v.Blocks)))
	}
	big := func(t *testing.T) fixture { // Astra #272 M1: 2000 modules binding X, a chain of 5000 .X
		files := map[string]string{"other.py": "Y = 1\n", "tests/test_b.py": "import pytest\nimport ext\n\n@pytest.mark.skipif(ext" + strings.Repeat(".X", 5000) + ", reason='r')\ndef test_b():\n    pass\n"}
		for i := range 2000 {
			files[fmt.Sprintf("m%d.py", i)] = "X = False\n"
		}
		return newFixtureWith(t, func(src string) {
			write(t, src, files)
			plain(t, src, "add", "--all")
		})
	}(t)
	old := maxWork
	t.Cleanup(func() { maxWork = old })
	for bound, want := range map[int]string{200000: "other.py: a module a condition reads is unread (the module tests/test_b.py reads a decorator argument from ext, not a repository module)", 10000: "other.py: a module a condition reads is unread"} { // round 7: ext is no repository module
		maxWork = bound
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		v := big.guarded(t, map[string]string{"other.py": "Y = 2\n"})
		runtime.ReadMemStats(&m1)
		if mb := (m1.TotalAlloc - m0.TotalAlloc) >> 20; mb > 200 || want == "" && len(pyOwn(v.Blocks)) > 0 || want != "" && !pyFinds(pyOwn(v.Blocks), want) {
			t.Errorf("a-M1 a bounded expansion (%d, %d MB): %q, want %q", bound, (m1.TotalAlloc-m0.TotalAlloc)>>20, whats(pyOwn(v.Blocks)), want)
		}
	}
	maxWork = old
	stars := func(t *testing.T) fixture { // Astra #272 r3 M1: 10000 star imports binding Z, a binding reading Z 10000 times
		var b strings.Builder
		files := map[string]string{"other.py": "Y = 1\n", "tests/test_s.py": "import pytest\nfrom big import V\n\n@pytest.mark.skipif(V, reason='r')\ndef test_s():\n    pass\n"}
		for i := range 10000 {
			fmt.Fprintf(&b, "from s%d import *\n", i)
			files[fmt.Sprintf("s%d.py", i)] = "Z = False\n"
		}
		files["big.py"] = b.String() + "V = Z" + strings.Repeat(" + Z", 9999) + "\n"
		return newFixtureWith(t, func(src string) {
			write(t, src, files)
			plain(t, src, "add", "--all")
		})
	}(t)
	maxWork = 100000
	start := time.Now()
	v := stars.guarded(t, map[string]string{"other.py": "Y = 2\n"})
	if took := time.Since(start); took > 10*time.Second || !pyFinds(pyOwn(v.Blocks), "over the guard's work bound") && !pyFinds(pyOwn(v.Blocks), "other.py: a module a condition reads is unread") { // big.py is not inert (P14c-2a-widen r2): unread before the expansion
		t.Errorf("r5-M1 a star expansion bounded: %v, %q", took, whats(pyOwn(v.Blocks)))
	}
	maxWork = old
	hu := newFixtureWith(t, func(src string) { // the review of 1ceb79c: a re-export behind an unknown star is unread
		write(t, src, map[string]string{"helpers9.py": "from numpy import *\n", "other.py": "X = 1\n",
			"tests/test_h.py": "from helpers9 import REQ9\n\n@REQ9\ndef test_h():\n    pass\n"})
		plain(t, src, "add", "--all")
	})
	if v := hu.guarded(t, map[string]string{"other.py": "X = 2\n"}); !pyFinds(pyOwn(v.Blocks), "other.py: a module a condition reads is unread") {
		t.Errorf("v3 a holder behind an unknown star: %q, want blocked", whats(pyOwn(v.Blocks)))
	}
	for name, base := range map[string]map[string]string{
		"w1 .parent of an unknown shape (a nil index)": {"conftest.py": "import sys\nimport foo\nsys.path.append(foo.parent)\n"},
		"M1 sys.path from a call":                      {"conftest.py": "import sys\nimport helper\nsys.path.insert(0, helper.where())\n"},
		"M1 setup.cfg package_dir a mapping":           {"setup.cfg": "[options]\npackage_dir =\n    mypkg = lib/mypkg\n"},
	} {
		g := newFixtureWith(t, func(src string) {
			write(t, src, base)
			plain(t, src, "add", "--all")
		})
		if v := g.guarded(t, map[string]string{"money.py": baseFiles["money.py"] + "\n"}); !pyFinds(pyOwn(v.Blocks), "money.py: unverifiable: the base adds sys.path entries the guard does not read") {
			t.Errorf("%s: %q, want unread", name, whats(pyOwn(v.Blocks)))
		}
	}
	// Opus #272 C1: main's floor holds for every unmodelled entry (m6-01..m6-18, m6-20; the review has no m6-19).
	for name, base := range map[string]map[string]string{
		"m6-01 package-dir as a table":        {"pyproject.toml": "[tool.setuptools.package-dir]\n\"\" = \"lib\"\n"},
		"m6-02 package-dir as a dotted key":   {"pyproject.toml": "tool.setuptools.package-dir = {\"\" = \"lib\"}\n"},
		"m6-03 package-dir quoted":            {"pyproject.toml": "[tool.setuptools]\n\"package-dir\" = {\"\" = \"lib\"}\n"},
		"m6-04 package_dir:":                  {"setup.cfg": "[options]\npackage_dir:\n    =lib\n"},
		"m6-05 Package_Dir":                   {"setup.cfg": "[options]\nPackage_Dir =\n    =lib\n"},
		"m6-06 poetry from":                   {"pyproject.toml": "[tool.poetry]\npackages = [{include = \"x\", from = \"lib\"}]\n"},
		"m6-07 hatch sources":                 {"pyproject.toml": "[tool.hatch.build.targets.wheel.sources]\n\"lib\" = \"\"\n"},
		"m6-08 a src layout of modules":       {"src/x.py": "X = 1\n"},
		"m6-09 a src namespace package":       {"src/ns/x.py": "X = 1\n"},
		"m6-10 PYTHONPATH in CI":              {".github/workflows/ci.yml": "env:\n  PYTHONPATH: lib\n"},
		"m6-11 PYTHONPATH in a Makefile":      {"Makefile": "test:\n\tPYTHONPATH=lib pytest\n"},
		"m6-12 PYTHONPATH in a noxfile":       {"noxfile.py": "import nox\n\n@nox.session\ndef tests(session):\n    session.run('pytest', env={'PYTHONPATH': 'lib'})\n"},
		"m6-13 site.addsitedir":               {"conftest.py": "import site\nsite.addsitedir('lib')\n"},
		"m6-14 from sys import path":          {"conftest.py": "from sys import path\npath.insert(0, 'lib')\n"},
		"m6-15 import sys as _s":              {"conftest.py": "import sys as _s\n_s.path.insert(0, 'lib')\n"},
		"m6-16 getattr(sys, 'path')":          {"conftest.py": "import sys\ngetattr(sys, 'path').insert(0, 'lib')\n"},
		"m6-17 a path from several literals":  {"tests/conftest.py": "import os\nimport sys\nsys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'vendor', 'lib'))\n"},
		"m6-18 a module the conftest imports": {"conftest.py": "import helper\n", "helper.py": "import sys\nsys.path.insert(0, 'lib')\n"},
		"m6-20 a pytest_plugins module":       {"conftest.py": "pytest_plugins = ['myplugin']\n", "myplugin.py": "import sys\nsys.path.insert(0, 'lib')\n"},
	} {
		base["lib/utils.py"], base["lib/money2.py"], base["tests/test_total.py"] = "X = 1\n", "import utils\n\ndef total():\n    return utils.X\n", "from money2 import total\n\ndef test_t():\n    assert total() == 1\n"
		g := newFixtureWith(t, func(src string) {
			write(t, src, base)
			plain(t, src, "add", "--all")
		})
		if v := g.guarded(t, map[string]string{"tests/utils.py": "X = 2\n"}); !pyFinds(pyOwn(v.Blocks), "tests/utils.py: ") {
			t.Errorf("%s: %q, want blocked", name, whats(pyOwn(v.Blocks)))
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
	chain := func(n int) map[string]string { // Opus #270 L1: one statement binding n aliases
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "a%d = ", i)
		}
		return map[string]string{"tests/conftest.py": "import pytest\n" + b.String() + "pytest.mark.skipif\n"}
	}
	for i, n := range []int{10000, 40000} {
		start := time.Now()
		f.guarded(t, chain(n))
		took[i] = time.Since(start)
	}
	if took[1] > 6*took[0]+time.Second {
		t.Errorf("an alias chain of 10000 in %v, of 40000 in %v: not linear", took[0], took[1])
	}
	old := maxWork
	t.Cleanup(func() { maxWork = old })
	maxWork = 1 << 12
	pyBlocks(t, f, "over the work bound", body(1<<20), "tests/conftest.py: unverifiable: its analysis is over the guard's work bound")
	maxWork = old
	ctx := &countCtx{Context: context.Background()}
	ctx.n.Store(1 << 40)
	in := In{Repo: f.repo, Base: f.base, Patch: f.patch(t, body(1<<20)), Policy: f.policy, Guard: integrity(f.repo, f.base, testLister(t))}
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

// TestPyGuardInstallGate: the gate off (the default), a base with an install
// key keeps main's unread verdict, and nothing else this PR adds passes what
// main blocks (Astra #275 r3; strictly additive). "": no block, as on main.
func TestPyGuardInstallGate(t *testing.T) {
	for name, c := range map[string]struct {
		base, change map[string]string
		want         string
	}{
		"g setup.cfg package_dir":                                                 {map[string]string{"setup.cfg": "[options]\npackage_dir =\n    =lib\n", "tests/test_y.py": "import yaml\n\ndef test_y():\n    assert yaml\n"}, map[string]string{"lib/yaml.py": "X = 1\n"}, "lib/yaml.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"g setup.cfg packages.find where":                                         {map[string]string{"setup.cfg": "[options]\npackages = find:\n\n[options.packages.find]\nwhere = lib\n", "tests/test_y.py": "import yaml\n\ndef test_y():\n    assert yaml\n"}, map[string]string{"lib/yaml.py": "X = 1\n"}, "lib/yaml.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"g pyproject package-dir":                                                 {map[string]string{"pyproject.toml": "[tool.setuptools]\npackage-dir = {\"\" = \"lib\"}\n", "tests/test_y.py": "import yaml\n\ndef test_y():\n    assert yaml\n"}, map[string]string{"lib/yaml.py": "X = 1\n"}, "lib/yaml.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"g pyproject packages.find where":                                         {map[string]string{"pyproject.toml": "[tool.setuptools.packages.find]\nwhere = [\"lib\"]\n", "tests/test_y.py": "import yaml\n\ndef test_y():\n    assert yaml\n"}, map[string]string{"lib/yaml.py": "X = 1\n"}, "lib/yaml.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"g Astra #275 r3: an install root and a package's own __path__":           {map[string]string{"setup.cfg": "[metadata]\nname = example\nversion = 0.0.0\n\n[options]\npackage_dir = =lib\npackages = find:\n\n[options.packages.find]\nwhere = lib\n", "feature/__init__.py": "__path__ = [\"lib/impl\"]\n", "feature/flags.py": "SKIP = False\n", "lib/impl/__init__.py": "", "lib/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"g Opus #277 A02: a plain package importing a module that moves __path__": {map[string]string{"setup.cfg": "[metadata]\nname = example\nversion = 0.0.0\n\n[options]\npackage_dir = =lib\npackages = find:\n\n[options.packages.find]\nwhere = lib\n", "feature/__init__.py": "from . import _r\n", "feature/_r.py": "import feature\nfeature.__path__.insert(0, 'lib/impl')\n", "feature/flags.py": "SKIP = False\n", "lib/impl/__init__.py": "", "lib/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"g Opus #277 S07: a star source running a def default":                    {map[string]string{"setup.cfg": "[metadata]\nname = example\nversion = 0.0.0\n\n[options]\npackage_dir = =lib\npackages = find:\n\n[options.packages.find]\nwhere = lib\n", "feature/__init__.py": "from helper import *\n", "helper.py": "import sys\n\ndef f(x=sys.modules['feature'].__path__.insert(0, 'lib/impl')):\n    pass\n", "feature/flags.py": "SKIP = False\n", "lib/impl/__init__.py": "", "lib/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"g Opus #277 S08: a star source running a class body":                     {map[string]string{"setup.cfg": "[metadata]\nname = example\nversion = 0.0.0\n\n[options]\npackage_dir = =lib\npackages = find:\n\n[options.packages.find]\nwhere = lib\n", "feature/__init__.py": "from helper import *\n", "helper.py": "import sys\n\nclass K:\n    sys.modules['feature'].__path__.insert(0, 'lib/impl')\n", "feature/flags.py": "SKIP = False\n", "lib/impl/__init__.py": "", "lib/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"g a setuptools backend only stays read, as on main":                      {map[string]string{"pyproject.toml": "[build-system]\nbuild-backend = \"setuptools.build_meta\"\n", "cfg.py": "SKIP = False\n", "tests/test_y.py": "import yaml\n\ndef test_y():\n    assert yaml\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, ""},
		"g src/ and a package's own __path__: not plain (P14c-2a-plain)":          {map[string]string{"feature/__init__.py": "__path__ = [\"src/impl\"]\n", "feature/flags.py": "SKIP = False\n", "src/impl/__init__.py": "", "src/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, "cfg.py: a module a condition reads is unread (the module feature/__init__.py runs code at import outside the allowlist)"},
		"g src/ and a package's own __path__: the decoy changed blocks":           {map[string]string{"feature/__init__.py": "__path__ = [\"src/impl\"]\n", "feature/flags.py": "SKIP = False\n", "src/impl/__init__.py": "", "src/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"feature/flags.py": "SKIP = 'True'\n"}, "feature/flags.py: a module a condition reads is unread (the module feature/__init__.py runs code at import outside the allowlist)"},
	} {
		g := newFixtureWith(t, func(src string) {
			write(t, src, c.base)
			plain(t, src, "add", "--all")
		})
		if v := g.guarded(t, c.change); c.want == "" && len(pyOwn(v.Blocks)) > 0 || c.want != "" && !pyFinds(pyOwn(v.Blocks), c.want) {
			t.Errorf("%s: %q, want %q", name, whats(pyOwn(v.Blocks)), c.want)
		}
	}
}

// TestPyGuardInstallPending runs the install reader on (test-only: it sets a
// package variable, so it must not run in parallel). Astra #275 r3 blocks
// (not plain). Opus #277 A02 and S07 must block before P14c-2a-install
// turns the reader on; they pass until P14c-2a-widen models imports and
// P14c-2a-star star imports, so the test is skipped while one passes.
func TestPyGuardInstallPending(t *testing.T) {
	gate := pyInstallRootsRead
	t.Cleanup(func() { pyInstallRootsRead = gate })
	pyInstallRootsRead = true
	var pending []string
	for name, c := range map[string]struct {
		base    map[string]string
		pending bool
	}{
		"Astra #275 r3: an install root and a package's own __path__":           {map[string]string{"setup.cfg": "[metadata]\nname = example\nversion = 0.0.0\n\n[options]\npackage_dir = =lib\npackages = find:\n\n[options.packages.find]\nwhere = lib\n", "feature/__init__.py": "__path__ = [\"lib/impl\"]\n", "feature/flags.py": "SKIP = False\n", "lib/impl/__init__.py": "", "lib/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, false},
		"Opus #277 A02: a plain package importing a module that moves __path__": {map[string]string{"setup.cfg": "[metadata]\nname = example\nversion = 0.0.0\n\n[options]\npackage_dir = =lib\npackages = find:\n\n[options.packages.find]\nwhere = lib\n", "feature/__init__.py": "from . import _r\n", "feature/_r.py": "import feature\nfeature.__path__.insert(0, 'lib/impl')\n", "feature/flags.py": "SKIP = False\n", "lib/impl/__init__.py": "", "lib/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, true},
		"Opus #277 S07: a star source running a def default":                    {map[string]string{"setup.cfg": "[metadata]\nname = example\nversion = 0.0.0\n\n[options]\npackage_dir = =lib\npackages = find:\n\n[options.packages.find]\nwhere = lib\n", "feature/__init__.py": "from helper import *\n", "helper.py": "import sys\n\ndef f(x=sys.modules['feature'].__path__.insert(0, 'lib/impl')):\n    pass\n", "feature/flags.py": "SKIP = False\n", "lib/impl/__init__.py": "", "lib/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, true},
	} {
		g := newFixtureWith(t, func(src string) {
			write(t, src, c.base)
			plain(t, src, "add", "--all")
		})
		if v := g.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); pyFinds(pyOwn(v.Blocks), "cfg.py: ") {
			continue
		} else if c.pending {
			pending = append(pending, name)
			continue
		}
		t.Errorf("%s: passes with the reader on, want blocked", name)
	}
	if len(pending) > 0 {
		slices.Sort(pending)
		t.Skipf("pass with the reader on, must block before P14c-2a-install (P14c-2a-widen): %q", pending)
	}
}

// TestPyGuardStar: P14c-2a-star, the allowlist. A star import in a package
// __init__.py is inert only as "from .x import *" at module level over one
// plain module file x.py with no competing form, and a change to a star
// source blocks. "": no block of the guard's own.
func TestPyGuardStar(t *testing.T) {
	for name, c := range map[string]struct {
		base, change map[string]string
		gone         []string
		want         string
	}{
		"H6b an absolute star exporting __path__":                        {map[string]string{"feature/__init__.py": "from redirect import *\n", "redirect.py": "__all__ = [\"__path__\"]\n__path__ = [\"feature/impl\"]\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, []string{}, "cfg.py: a module a condition reads is unread (the module feature/__init__.py has a star import other than from .x import * in a package __init__.py)"},
		"r1 H1 an absolute package star exporting its implicit __path__": {map[string]string{"feature/__init__.py": "from redirect import *\n", "redirect/__init__.py": "__all__ = [\"__path__\"]\n", "redirect/flags.py": "from cfg import SKIP\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, []string{}, "cfg.py: a module a condition reads is unread (the module feature/__init__.py has a star import other than from .x import * in a package __init__.py)"},
		"r1 H1 a relative package star source":                           {map[string]string{"feature/__init__.py": "from .redirect import *\n", "feature/redirect/__init__.py": "__all__ = [\"__path__\"]\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, []string{}, "cfg.py: a module a condition reads is unread (the package feature star-imports .redirect, which is not one plain module file)"},
		"r1 H1 the candidate adds __all__ to a star source":              {map[string]string{"feature/__init__.py": "from .redirect import *\n", "feature/redirect.py": "X = 1\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"feature/redirect.py": "__all__ = [\"__path__\"]\n"}, []string{}, "feature/redirect.py: a star source of feature changed"},
		"r1 H1 the candidate adds a package beside a star source":        {map[string]string{"feature/__init__.py": "from .redirect import *\n", "feature/redirect.py": "X = 1\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"feature/redirect/__init__.py": "__all__ = [\"__path__\"]\n"}, []string{}, "feature/redirect/__init__.py: a star source of feature changed"},
		"r2 H1 an inline if suite":                                       {map[string]string{"feature/__init__.py": "", "feature/flags.py": "", "redirect/flags.py": "import pytest\npytest.skip(\"redirected\", allow_module_level=True)\n", "tests/test_x.py": "import feature.flags\n\ndef test_x():\n    assert False\n", "tests/test_control.py": "def test_control():\n    pass\n"}, map[string]string{"feature/__init__.py": "if True: from redirect import *\n", "redirect/__init__.py": "__all__ = [\"__path__\"]\n"}, []string{}, "feature/__init__.py: a package __init__.py that has a star import other than"},
		"r2 H1 a try suite":                                              {map[string]string{"feature/__init__.py": "", "feature/flags.py": "", "tests/test_x.py": "import feature.flags\n\ndef test_x():\n    assert False\n", "tests/test_control.py": "def test_control():\n    pass\n"}, map[string]string{"feature/__init__.py": "try:\n    from redirect import *\nexcept ImportError:\n    pass\n"}, []string{}, "feature/__init__.py: a package __init__.py that has a star import other than"},
		"r2 H1 a backslash continuation":                                 {map[string]string{"feature/__init__.py": "", "feature/flags.py": "", "tests/test_x.py": "import feature.flags\n\ndef test_x():\n    assert False\n", "tests/test_control.py": "def test_control():\n    pass\n"}, map[string]string{"feature/__init__.py": "if True: from redirect \\\n    import *\n"}, []string{}, "feature/__init__.py: a package __init__.py that has a star import other than"},
		"r2 H2 a compiled package beside the source":                     {map[string]string{"feature/__init__.py": "from .redirect import *\n", "feature/redirect.py": "X = 1\n", "feature/redirect/__init__.pyc": "\x00", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, []string{}, "cfg.py: a module a condition reads is unread (the package feature star-imports .redirect, which is not one plain module file)"},
		"r2 H2 an absolute star with a compiled package":                 {map[string]string{"feature/__init__.py": "from redirect import *\n", "redirect.py": "X = 1\n", "redirect/__init__.pyc": "\x00", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, []string{}, "cfg.py: a module a condition reads is unread (the module feature/__init__.py has a star import other than from .x import * in a package __init__.py)"},
		"r2 H3 a dotted source":                                          {map[string]string{"feature/__init__.py": "from .sources.redirect import *\n", "feature/sources/__init__.py": "__path__ = [\"real\"]\n", "feature/sources/redirect.py": "X = 1\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, []string{}, "cfg.py: a module a condition reads is unread"},
		"S07 a source running a def default":                             {map[string]string{"feature/__init__.py": "from .helper import *\n", "feature/helper.py": "import sys\n\ndef f(x=sys.modules['feature'].__path__.insert(0, 'impl')):\n    pass\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, []string{}, "cfg.py: unverifiable: the base adds sys.path entries the guard does not read"},
		"S08 a source with a class":                                      {map[string]string{"feature/__init__.py": "from .helper import *\n", "feature/helper.py": "class K:\n    X = 1\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, []string{}, "cfg.py: a module a condition reads is unread (the package feature star-imports .helper, which may run code or bind __path__"},
		"P33l a source importing":                                        {map[string]string{"feature/__init__.py": "from .helper import *\n", "feature/helper.py": "import helper2\n", "helper2.py": "X = 1\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, []string{}, "cfg.py: a module a condition reads is unread (the package feature star-imports .helper, which may run code or bind __path__"},
		"6ba a source only src/ names":                                   {map[string]string{"feature/__init__.py": "from .redirect import *\n", "src/feature/redirect.py": "X = 1\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, []string{}, "cfg.py: a module a condition reads is unread (the package feature star-imports .redirect, which is not one plain module file"},
		"6ba a source two files answer to":                               {map[string]string{"feature/__init__.py": "from .redirect import *\n", "feature/redirect.py": "X = 1\n", "feature/redirect/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, []string{}, "cfg.py: a module a condition reads is unread (the package feature star-imports .redirect, which is not one plain module file)"},
		"6ba an extension sibling":                                       {map[string]string{"feature/__init__.py": "from .redirect import *\n", "feature/redirect.py": "X = 1\n", "feature/redirect.cpython-312-x86_64-linux-gnu.so": "\x00", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, []string{}, "cfg.py: a module a condition reads is unread (the package feature star-imports .redirect, which is not one plain module file)"},
		"6ba a cached bytecode sibling":                                  {map[string]string{"feature/__init__.py": "from .redirect import *\n", "feature/redirect.py": "X = 1\n", "feature/__pycache__/redirect.cpython-312.pyc": "\x00", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, []string{}, "cfg.py: a module a condition reads is unread (the package feature star-imports .redirect, which is not one plain module file)"},
		"6ba a star source deleted":                                      {map[string]string{"feature/__init__.py": "from .redirect import *\n", "feature/redirect.py": "X = 1\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{}, []string{"feature/redirect.py"}, "feature/redirect.py: a star source of feature changed"},
		"6ba a star source changed":                                      {map[string]string{"feature/__init__.py": "from .redirect import *\n", "feature/redirect.py": "X = 1\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"feature/redirect.py": "X = 2\n"}, []string{}, "feature/redirect.py: a star source of feature changed"},
		"an ancestor with a star that is not inert":                      {map[string]string{"a/__init__.py": "from os import *\n", "a/b/__init__.py": "from .redirect import *\n", "a/b/redirect.py": "X = 1\n", "a/b/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport a.b.flags\n\n@pytest.mark.skipif(a.b.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, []string{}, "cfg.py: a module a condition reads is unread (the module a/__init__.py has a star import other than from .x import * in a package __init__.py)"},
		"the candidate star-imports a new source that imports":           {map[string]string{"feature/__init__.py": "", "feature/flags.py": "", "tests/test_x.py": "import feature.flags\n\ndef test_x():\n    assert False\n", "tests/test_control.py": "def test_control():\n    pass\n"}, map[string]string{"feature/__init__.py": "from .redirect import *\n", "feature/redirect.py": "import os\n"}, []string{}, "feature/__init__.py: a package __init__.py that star-imports .redirect, which may run code"},
		"the candidate's relative star indented in a suite":              {map[string]string{"feature/__init__.py": "", "feature/redirect.py": "X = 1\n", "feature/flags.py": "", "tests/test_x.py": "import feature.flags\n\ndef test_x():\n    assert False\n", "tests/test_control.py": "def test_control():\n    pass\n"}, map[string]string{"feature/__init__.py": "if True:\n    from .redirect import *\n"}, []string{}, "feature/__init__.py: a package __init__.py that has a star import other than"},
		"a source exporting a dunder through __all__":                    {map[string]string{"feature/__init__.py": "from .redirect import *\n", "feature/redirect.py": "__all__ = [\"__spec__\"]\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, []string{}, "cfg.py: a module a condition reads is unread (the package feature star-imports .redirect, which may run code or bind __path__)"},
		"a stub beside an inert source stays plain":                      {map[string]string{"feature/__init__.py": "from .redirect import *\n", "feature/redirect.py": "X = 1\n", "feature/redirect.pyi": "X: int\n", "feature/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, []string{}, "cfg.py: a skipif or xfail condition in tests/test_x.py reads SKIP, which"},
		"Opus #279 L1 the candidate adds a .pyw form of a star source":   {map[string]string{"feature/__init__.py": "from .redirect import *\n", "feature/redirect.py": "X = 1\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"feature/redirect.pyw": "X = 1\n"}, []string{}, "feature/redirect.pyw: a star source of feature changed"},
		"Opus #279 L2 the candidate star-imports and is not plain":       {map[string]string{"feature/__init__.py": "", "feature/redirect.py": "X = 1\n", "feature/flags.py": "", "tests/test_x.py": "import feature.flags\n\ndef test_x():\n    assert False\n", "tests/test_control.py": "def test_control():\n    pass\n"}, map[string]string{"feature/__init__.py": "from .redirect import *\nY = len('a')\n"}, []string{}, "feature/__init__.py: a package __init__.py that star-imports and is not plain"},
		"Opus #279 L1 the candidate adds a cached form of a star source": {map[string]string{"feature/__init__.py": "from .redirect import *\n", "feature/redirect.py": "X = 1\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"feature/__pycache__/redirect.cpython-312.pyc": "\x00"}, []string{}, "feature/__pycache__/redirect.cpython-312.pyc: a star source of feature changed"},
		"an inert star stays plain":                                      {map[string]string{"feature/__init__.py": "from .redirect import *\n", "feature/redirect.py": "X = 1\n", "feature/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, map[string]string{"cfg.py": "SKIP = 'True'\n"}, []string{}, "cfg.py: a skipif or xfail condition in tests/test_x.py reads SKIP, which"},
		"the candidate adds an inert star":                               {map[string]string{"feature/__init__.py": "", "feature/redirect.py": "X = 1\n", "feature/flags.py": "", "tests/test_x.py": "import feature.flags\n\ndef test_x():\n    assert False\n", "tests/test_control.py": "def test_control():\n    pass\n"}, map[string]string{"feature/__init__.py": "from .redirect import *\n"}, []string{}, ""},
	} {
		g := newFixtureWith(t, func(src string) {
			write(t, src, c.base)
			plain(t, src, "add", "--all")
		})
		if v := g.guarded(t, c.change, c.gone...); c.want == "" && len(pyOwn(v.Blocks)) > 0 || c.want != "" && !pyFinds(pyOwn(v.Blocks), c.want) {
			t.Errorf("%s: %q, want %q", name, whats(pyOwn(v.Blocks)), c.want)
		}
	}
}

// TestPyGuardStarWork: Astra #279 M1, scaled. 2,000 repetitions of
// "from .x import *" beside 2,000 sibling files, read by 100 conditions:
// deduplicated, cached and indexed by directory, the star check stays well
// inside a work bound that the uncached (about 100 x 2,005) or repeated
// (about 2,000 x 2,005) scan would pass; and the scan is charged, so a
// small bound stops it as unread. No wall-clock bound.
func TestPyGuardStarWork(t *testing.T) {
	base := map[string]string{"feature/__init__.py": strings.Repeat("from .x import *\n", 2000), "feature/x.py": "X = 1\n", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "other.py": "O = 1\n"}
	for i := range 2000 {
		base[fmt.Sprintf("feature/m%d.py", i)] = "M = 1\n"
	}
	test := "import pytest\nimport feature.flags\n\n"
	for i := range 100 {
		test += fmt.Sprintf("@pytest.mark.skipif(feature.flags.SKIP, reason='r%d')\ndef test_%d():\n    pass\n\n", i, i)
	}
	base["tests/test_x.py"] = test
	g := newFixtureWith(t, func(src string) {
		write(t, src, base)
		plain(t, src, "add", "--all")
	})
	old := maxWork
	t.Cleanup(func() { maxWork = old })
	maxWork = 52000 // round 8: the binding analysis charges its one scan of the test file (Astra #281 r6 M2)
	if v := g.guarded(t, map[string]string{"other.py": "O = 2\n"}); len(pyOwn(v.Blocks)) > 0 {
		t.Errorf("repeated stars within the work bound: %q, want no block", whats(pyOwn(v.Blocks)))
	}
	maxWork = 3000
	if v := g.guarded(t, map[string]string{"other.py": "O = 2\n"}); !pyFinds(pyOwn(v.Blocks), "other.py: a module a condition reads is unread (the package feature is over the guard's work bound)") {
		t.Errorf("the star check charged to the work bound: %q", whats(pyOwn(v.Blocks)))
	}
}

// TestPyGuardImportTime: P14c-2a-widen (A), code that runs because a module
// is imported. The acceptance rows of Opus #277 (A02-A02d, G01, G02) block
// or are unread with the install reader on (test-only: it sets a package
// variable, not in parallel) and without install keys; ordinary closure
// modules stay read.
func TestPyGuardImportTime(t *testing.T) {
	gate := pyInstallRootsRead
	t.Cleanup(func() { pyInstallRootsRead = gate })
	rows := map[string]struct {
		base map[string]string
		want string
	}{
		"A02 a plain package importing a module that moves __path__":  {map[string]string{"feature/__init__.py": "from . import _r\n", "feature/_r.py": "import feature\nfeature.__path__.insert(0, 'lib/impl')\n", "feature/flags.py": "SKIP = False\n", "lib/impl/__init__.py": "", "lib/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, "cfg.py: a module a condition reads is unread (the module feature/_r.py writes [feature . __path__] at import)"},
		"A02b sys.modules in an imported module":                      {map[string]string{"feature/__init__.py": "import feature._r\n", "feature/_r.py": "import sys\nsys.modules['feature'].__path__.insert(0, 'lib/impl')\n", "feature/flags.py": "SKIP = False\n", "lib/impl/__init__.py": "", "lib/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, "cfg.py: "},
		"A02c a root helper rebinding __path__":                       {map[string]string{"feature/__init__.py": "import helper\n", "helper.py": "import feature\nfeature.__path__[:] = ['lib/impl']\n", "feature/flags.py": "SKIP = False\n", "lib/impl/__init__.py": "", "lib/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, "cfg.py: a module a condition reads is unread (the module helper.py writes [feature . __path__] at import)"},
		"A02d the helper under the install root":                      {map[string]string{"feature/__init__.py": "import helper\n", "lib/helper.py": "import feature\nfeature.__path__[:] = ['lib/impl']\n", "feature/flags.py": "SKIP = False\n", "lib/impl/__init__.py": "", "lib/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, "cfg.py: "},
		"G01 a conftest parking a value on a base module":             {map[string]string{"conftest.py": "import cfg\nimport feature.flags\nfeature.flags.SKIP = cfg.SKIP\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "lib/impl/__init__.py": "", "lib/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, "cfg.py: a module a condition reads is unread (the module conftest.py writes [feature . flags . SKIP] at import)"},
		"G02 an imported sibling parking a value":                     {map[string]string{"feature/__init__.py": "from . import _park\n", "feature/_park.py": "import cfg\nfrom . import flags\nflags.SKIP = cfg.SKIP\n", "feature/flags.py": "SKIP = False\n", "lib/impl/__init__.py": "", "lib/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, "cfg.py: a module a condition reads is unread (the module feature"},
		"G03 a setattr on a base module":                              {map[string]string{"conftest.py": "import cfg\nimport feature.flags\nsetattr(feature.flags, 'SKIP', cfg.SKIP)\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "lib/impl/__init__.py": "", "lib/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, "cfg.py: a module a condition reads is unread"},
		"G04 a del on a base module":                                  {map[string]string{"conftest.py": "import feature.flags\ndel feature.flags.SKIP\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "lib/impl/__init__.py": "", "lib/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, "cfg.py: a module a condition reads is unread (the module conftest.py writes [feature . flags . SKIP] at import)"},
		"an ordinary closure module is unread (a stated false block)": {map[string]string{"feature/__init__.py": "from . import util\n", "feature/util.py": "import os\nX = os.path.join('a', 'b')\nos.environ['MODE'] = '1'\n", "feature/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, "cfg.py: a module a condition reads is unread (the module feature/util.py writes [os . environ [ 'MODE' ]] at import)"},
		"a __main__ guard stays read":                                 {map[string]string{"feature/__init__.py": "from . import util\n", "feature/util.py": "from . import flags\n\ndef main():\n    pass\n\nif __name__ == \"__main__\":\n    main()\n", "feature/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, "cfg.py: a skipif or xfail condition in tests/test_x.py reads SKIP, which"},
		"a method writing self stays read":                            {map[string]string{"feature/__init__.py": "from . import util\n", "feature/util.py": "class C:\n    def set(self):\n        self.x = 1\n", "feature/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}, "cfg.py: a skipif or xfail condition in tests/test_x.py reads SKIP, which"},
	}
	for _, on := range []bool{true, false} {
		pyInstallRootsRead = on
		for name, c := range rows {
			base := maps.Clone(c.base)
			if !on && strings.HasPrefix(name, "A02d") {
				continue // lib/ is a root only through the install key
			} else if on {
				base["setup.cfg"] = "[metadata]\nname = example\nversion = 0.0.0\n\n[options]\npackage_dir = =lib\npackages = find:\n\n[options.packages.find]\nwhere = lib\n"
			}
			g := newFixtureWith(t, func(src string) {
				write(t, src, base)
				plain(t, src, "add", "--all")
			})
			if v := g.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); !pyFinds(pyOwn(v.Blocks), c.want) {
				t.Errorf("%s (the reader on: %v): %q, want %q", name, on, whats(pyOwn(v.Blocks)), c.want)
			}
		}
	}
	pyInstallRootsRead = gate
	for name, c := range map[string]struct {
		base, change map[string]string
		want         string
	}{
		"the candidate makes a package __init__.py not plain": {map[string]string{"feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "tests/test_y.py": "def test_y():\n    pass\n"}, map[string]string{"feature/__init__.py": "import logging\nlogging.getLogger(__name__)\n"}, "feature/__init__.py: a package __init__.py that is not plain changed"},
		"the candidate writes onto a base module":             {map[string]string{"helper.py": "X = 1\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "tests/test_y.py": "def test_y():\n    pass\n"}, map[string]string{"helper.py": "import feature.flags\nfeature.flags.SKIP = True\n"}, "helper.py: a module the tests now import runs code at import, writes a module or uses sys (P14c-2a-widen)"},
		"the candidate uses sys.modules":                      {map[string]string{"helper.py": "X = 1\n", "tests/test_y.py": "def test_y():\n    pass\n"}, map[string]string{"helper.py": "import sys\nsys.modules['feature'] = object()\n"}, "helper.py: modules in module level: unverifiable"},
	} {
		g := newFixtureWith(t, func(src string) {
			write(t, src, c.base)
			plain(t, src, "add", "--all")
		})
		if v := g.guarded(t, c.change); !pyFinds(pyOwn(v.Blocks), c.want) {
			t.Errorf("%s: %q, want %q", name, whats(pyOwn(v.Blocks)), c.want)
		}
	}
	links := newFixtureWith(t, func(src string) { // a base .py symlink and a gitlink (Opus #279 M1, Astra #279 M2)
		write(t, src, map[string]string{"feature/__init__.py": "", "real/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "sub/x.py": "",
			"tests/test_x.py": "import pytest\nimport feature.flags\nfrom vendored.flags import SKIP as VSKIP\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\n@pytest.mark.skipif(VSKIP, reason='r')\ndef test_x():\n    pass\n"})
		if err := os.Symlink("../real/flags.py", filepath.Join(src, "feature", "flags.py")); err != nil {
			t.Fatal(err)
		}
		plain(t, src, "add", "--all")
		plain(t, src, "update-index", "--add", "--cacheinfo", "160000,1111111111111111111111111111111111111111,vendored")
	})
	if v := links.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); !pyFinds(pyOwn(v.Blocks), "cfg.py: a module a condition reads is unread (the module feature.flags is a symlink or a gitlink in the base)") {
		t.Errorf("a base .py symlink: %q", whats(pyOwn(v.Blocks)))
	}
	gitlink := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"cfg.py": "SKIP = False\n", "tests/test_v.py": "import pytest\nfrom vendored.flags import SKIP\n\n@pytest.mark.skipif(SKIP, reason='r')\ndef test_v():\n    pass\n"})
		plain(t, src, "add", "--all")
		plain(t, src, "update-index", "--add", "--cacheinfo", "160000,1111111111111111111111111111111111111111,vendored")
	})
	if v := gitlink.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); !pyFinds(pyOwn(v.Blocks), "cfg.py: a module a condition reads is unread (the module vendored is a symlink or a gitlink in the base)") {
		t.Errorf("a base gitlink: %q", whats(pyOwn(v.Blocks)))
	}
}

// TestPyGuardImportTimeR2: Opus #281's probes (H1-H3, L1), each blocking or
// unread: a closure module is inert only when every statement it runs at
// import is pyAllowed; any other writes every base module it imports; a
// dynamic-import name leaves the base unread.
func TestPyGuardImportTimeR2(t *testing.T) {
	for name, base := range map[string]map[string]string{
		"w02 a tuple target":                  {"conftest.py": "import cfg\nimport feature.flags\n_, feature.flags.SKIP = 0, cfg.SKIP\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"},
		"w04 an inline if suite":              {"conftest.py": "import cfg\nimport feature.flags\nif True: feature.flags.SKIP = cfg.SKIP\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"},
		"w27b a parenthesised head":           {"conftest.py": "import cfg\nimport feature.flags\n(feature).flags.SKIP = cfg.SKIP\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"},
		"w11 a computed head":                 {"conftest.py": "import cfg\nimport importlib\nimportlib.import_module('feature.flags').SKIP = cfg.SKIP\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"},
		"a04 a parenthesised __path__ move":   {"conftest.py": "import feature\n(feature).__path__.insert(0, 'lib/impl')\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "lib/impl/__init__.py": "", "lib/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"},
		"w05 __setattr__":                     {"conftest.py": "import cfg\nimport feature.flags\nfeature.flags.__setattr__('SKIP', cfg.SKIP)\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"},
		"w07 __dict__.update":                 {"conftest.py": "import cfg\nimport feature.flags\nfeature.flags.__dict__.update(SKIP=cfg.SKIP)\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"},
		"w16 exec":                            {"conftest.py": "import cfg\nimport feature.flags\nexec('feature.flags.SKIP = cfg.SKIP')\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"},
		"a02 getattr __path__":                {"conftest.py": "import feature\ngetattr(feature, '__path__').insert(0, 'lib/impl')\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "lib/impl/__init__.py": "", "lib/impl/flags.py": "from cfg import SKIP\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"},
		"i02 import_module by keyword":        {"conftest.py": "import importlib\nimportlib.import_module(name='helper')\n", "helper.py": "import cfg\nimport feature.flags\nfeature.flags.SKIP = cfg.SKIP\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"},
		"i04 a relative constant":             {"conftest.py": "import importlib\nimportlib.import_module('._park', 'feature')\n", "feature/_park.py": "import cfg\nfrom . import flags\nflags.SKIP = cfg.SKIP\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"},
		"i21 fromlist":                        {"conftest.py": "__import__('feature', fromlist=['_park'])\n", "feature/_park.py": "import cfg\nfrom . import flags\nflags.SKIP = cfg.SKIP\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"},
		"m1 a head import of a dotted module": {"conftest.py": "import cfg\nimport feature.flags\nfeature.X = cfg.SKIP\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature\n\n@pytest.mark.skipif(feature.X, reason='r')\ndef test_x():\n    assert False\n"},
		"m2 a setattr on a package":           {"conftest.py": "import cfg\nimport feature\nsetattr(feature, 'X', cfg.SKIP)\n", "feature/__init__.py": "", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature\n\n@pytest.mark.skipif(feature.X, reason='r')\ndef test_x():\n    assert False\n"},
		"m3 a setattr not at the head":        {"conftest.py": "import cfg\nimport feature.flags\nx = setattr(feature.flags, 'SKIP', cfg.SKIP)\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"},
	} {
		g := newFixtureWith(t, func(src string) {
			write(t, src, base)
			plain(t, src, "add", "--all")
		})
		if v := g.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); !pyFinds(pyOwn(v.Blocks), "cfg.py: ") {
			t.Errorf("%s: %q, want blocked or unread", name, whats(pyOwn(v.Blocks)))
		}
	}
	for name, c := range map[string]struct {
		base, change map[string]string
		want         string
	}{ // the candidate side (Opus #281 H1 k04, M1 k01-k03)
		"kp1 the candidate gives a package a decorated def":          {map[string]string{"feature/__init__.py": "", "feature/_r.py": "def g(f):\n    return f\n", "feature/sources.py": "X = 1\n", "tests/test_y.py": "def test_y():\n    pass\n"}, map[string]string{"feature/__init__.py": "from ._r import g\n\n@g\ndef f():\n    pass\n"}, "feature/__init__.py: a package __init__.py that is not plain changed"},
		"kp2 the candidate gives a package an annotation call":       {map[string]string{"feature/__init__.py": "", "feature/_r.py": "def g(f):\n    return f\n", "feature/sources.py": "X = 1\n", "tests/test_y.py": "def test_y():\n    pass\n"}, map[string]string{"feature/__init__.py": "from ._r import g\n\ndef f(x: g()):\n    pass\n"}, "feature/__init__.py: a package __init__.py that is not plain changed"},
		"kp3 the candidate gives a package a return annotation call": {map[string]string{"feature/__init__.py": "", "feature/_r.py": "def g(f):\n    return f\n", "feature/sources.py": "X = 1\n", "tests/test_y.py": "def test_y():\n    pass\n"}, map[string]string{"feature/__init__.py": "from ._r import g\n\ndef f() -> g():\n    pass\n"}, "feature/__init__.py: a package __init__.py that is not plain changed"},
		"kp4 the candidate gives a package a dotted relative star":   {map[string]string{"feature/__init__.py": "", "feature/_r.py": "def g(f):\n    return f\n", "feature/sources.py": "X = 1\n", "tests/test_y.py": "def test_y():\n    pass\n"}, map[string]string{"feature/__init__.py": "from .sources.redirect import *\n"}, "feature/__init__.py: a package __init__.py that has a star import other than"},
		"k04 a changed module with a tuple target": {map[string]string{"helper.py": "X = 1\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "tests/test_y.py": "def test_y():\n    pass\n"},
			map[string]string{"helper.py": "import feature.flags\n_, feature.flags.SKIP = 0, True\n"}, "helper.py: "},
		"k01 a changed helper newly importing a writer": {map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n", "parker.py": "import feature.flags\nfeature.flags.SKIP = True\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"},
			map[string]string{"helper.py": "import parker\nX = 1\n"}, "a module the tests now import runs code at import"},
		"k02 a new test importing a writer": {map[string]string{"parker.py": "import feature.flags\nfeature.flags.SKIP = True\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"},
			map[string]string{"tests/test_a.py": "import parker\n\ndef test_a():\n    pass\n"}, "a module the tests now import runs code at import"},
		"k03 a package already not plain newly importing a writer": {map[string]string{"conftest.py": "import other\n", "other/__init__.py": "import logging\nlogging.getLogger(__name__)\n", "parker.py": "import feature.flags\nfeature.flags.SKIP = True\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"},
			map[string]string{"other/__init__.py": "import logging\nimport parker\nlogging.getLogger(__name__)\n"}, "a module the tests now import runs code at import"},
	} {
		g := newFixtureWith(t, func(src string) {
			write(t, src, c.base)
			plain(t, src, "add", "--all")
		})
		if v := g.guarded(t, c.change); !pyFinds(pyOwn(v.Blocks), c.want) {
			t.Errorf("%s: %q, want %q", name, whats(pyOwn(v.Blocks)), c.want)
		}
	}
	links := map[string]func(src string){ // Opus #281 H4: s08, s03, c08
		"s08 a base symlink's target changed": func(src string) {
			write(t, src, map[string]string{"feature/__init__.py": "", "real_flags.txt": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"})
			if err := os.Symlink("../real_flags.txt", filepath.Join(src, "feature", "flags.py")); err != nil {
				t.Fatal(err)
			}
		},
		"s03 a symlinked test file": func(src string) {
			write(t, src, map[string]string{"feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "impl_test.txt": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"})
			if err := os.MkdirAll(filepath.Join(src, "tests"), 0o755); err != nil {
				t.Fatal(err)
			} else if err := os.Symlink("../impl_test.txt", filepath.Join(src, "tests", "test_x.py")); err != nil {
				t.Fatal(err)
			}
		},
		"s09 a base symlink out of the checkout": func(src string) {
			write(t, src, map[string]string{"feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"})
			if err := os.Symlink("../../outside.txt", filepath.Join(src, "feature", "data.txt")); err != nil {
				t.Fatal(err)
			}
		},
		"c10 a gitlink two hops away": func(src string) {
			write(t, src, map[string]string{"conftest.py": "import helper0\n", "helper0.py": "import vendored\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n",
				"tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"})
		},
		"c08 a symlinked closure module two hops away": func(src string) {
			write(t, src, map[string]string{"conftest.py": "import helper0\n", "helper0.py": "import helper\n", "real_helper.txt": "import cfg\nimport feature.flags\nfeature.flags.SKIP = cfg.SKIP\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n",
				"tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"})
			if err := os.Symlink("real_helper.txt", filepath.Join(src, "helper.py")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, prep := range links {
		g := newFixtureWith(t, func(src string) {
			prep(src)
			plain(t, src, "add", "--all")
			if strings.HasPrefix(name, "c10") {
				plain(t, src, "update-index", "--add", "--cacheinfo", "160000,1111111111111111111111111111111111111111,vendored")
			}
		})
		change := map[string]string{"cfg.py": "SKIP = 'True'\n"}
		if strings.HasPrefix(name, "s08") {
			change = map[string]string{"real_flags.txt": "SKIP = True\n"}
		} else if strings.HasPrefix(name, "s09") {
			change = map[string]string{"notes.txt": "x\n"} // any change, while a base symlink leaves the checkout
		}
		if v := g.guarded(t, change); len(pyOwn(v.Blocks)) == 0 {
			t.Errorf("%s: no block", name)
		}
	}
	old := maxWork // the closure charged to the work bound
	t.Cleanup(func() { maxWork = old })
	maxWork = 40
	chain := map[string]string{"conftest.py": "import m0\n", "cfg.py": "SKIP = False\n", "feature/__init__.py": "", "feature/flags.py": "SKIP = False\n",
		"tests/test_x.py": "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_x():\n    assert False\n"}
	for i := range 60 {
		chain[fmt.Sprintf("m%d.py", i)] = fmt.Sprintf("import m%d\n", i+1)
	}
	g := newFixtureWith(t, func(src string) {
		write(t, src, chain)
		plain(t, src, "add", "--all")
	})
	if v := g.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); !pyFinds(pyOwn(v.Blocks), "over the guard's work bound") {
		t.Errorf("a long import chain over the work bound: %q", whats(pyOwn(v.Blocks)))
	}
}

// TestPyGuardImportTimeR3: Astra #281's inputs (H1-H5, M1), each unread or
// blocking; the positives (a def's or literal's attribute, self.x in a
// method, an import cycle) stay read or pass.
func TestPyGuardImportTimeR3(t *testing.T) {
	const flags = "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_z():\n    assert False\n"
	astra := func(extra map[string]string) map[string]string {
		return maps.Collect(func(yield func(string, string) bool) {
			for k, v := range map[string]string{"feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_z.py": flags} {
				_ = yield(k, v)
			}
			for k, v := range extra {
				_ = yield(k, v)
			}
		})
	}
	read := "cfg.py: a skipif or xfail condition in tests/test_z.py reads SKIP, which"
	for name, c := range map[string]struct {
		base map[string]string
		want string
	}{
		"H1 a conftest writing through an alias":                              {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\n\nm = feature.flags\nm.SKIP = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py writes [m . SKIP] at import)"},
		"H1 a subscript through an alias":                                     {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\n\nm = feature.flags.__dict__\nm['SKIP'] = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread"},
		"H1 an alias also bound by a literal":                                 {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\n\nm = {}\nm = feature.flags\nm.SKIP = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py writes [m . SKIP] at import)"},
		"H1 a global rebinding a literal":                                     {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\n\nm = {}\n\ndef f():\n    global m\n    m = feature.flags\n\nf()\nm.SKIP = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py writes [m . SKIP] at import)"},
		"H1 a decorated def":                                                  {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\n\n@lambda f: feature.flags\ndef m():\n    pass\n\nm.SKIP = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread"},
		"H2 a re-export":                                                      {astra(map[string]string{"conftest.py": "import writer\n", "bridge.py": "import feature.flags as target\n", "writer.py": "import cfg\nfrom bridge import target\ntarget.SKIP = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread"},
		"H2 a re-export's attribute in a conftest":                            {astra(map[string]string{"conftest.py": "import cfg\nimport bridge\nbridge.target.SKIP = cfg.SKIP\n", "bridge.py": "import feature.flags as target\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py writes [bridge . target . SKIP] at import)"},
		"H2 a re-exported value in a conftest":                                {astra(map[string]string{"conftest.py": "import cfg\nfrom bridge import target\ntarget['skip'] = cfg.SKIP\n", "bridge.py": "from feature.flags import CONFIG as target\n", "feature/flags.py": "CONFIG = {'skip': False}\nSKIP = False\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py writes [target [ 'skip' ]] at import)"},
		"H2 an impure writer through a re-export":                             {astra(map[string]string{"conftest.py": "import writer\n", "bridge.py": "import feature.flags as target\n", "writer.py": "import cfg\nimport bridge\nsetattr(bridge.target, 'SKIP', cfg.SKIP)\n"}), "cfg.py: a module a condition reads is unread (the module writer.py writes by a call or through a target the guard cannot read)"},
		"H2 an impure writer through two re-exports":                          {astra(map[string]string{"conftest.py": "import writer\n", "bridge.py": "import bridge2\n", "bridge2.py": "import feature.flags as target\n", "writer.py": "import cfg\nimport bridge\nsetattr(bridge.bridge2.target, 'SKIP', cfg.SKIP)\n"}), "cfg.py: a module a condition reads is unread (the module writer.py writes by a call or through a target the guard cannot read)"},
		"H3 a quoted dynamic-import name":                                     {astra(map[string]string{"conftest.py": "__import__(\"'parker'\")\n", "parker.py": "X = 1\n", "'parker'.py": "import cfg\nimport feature.flags\nfeature.flags.SKIP = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py imports or runs code dynamically)"},
		"H5 an unread module two hops away":                                   {astra(map[string]string{"conftest.py": "import helper0\n", "helper0.py": "import parker\n", "parker.py": "# coding: latin-1\nimport cfg\nimport feature.flags\nfeature.flags.SKIP = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread (the module parker.py is unread)"},
		"a def's attribute is unread (a stated false block)":                  {astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "conftest.py": "def f():\n    pass\n\nclass C:\n    pass\n\nf.x = 1\nC.y = 2\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py writes [f . x] at import)"},
		"a literal's subscript is unread (a stated false block)":              {astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "conftest.py": "import os\nCFG = {}\nCFG['x'] = 1\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py writes [CFG [ 'x' ]] at import)"},
		"r3 H1 an inline try suite":                                           {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\n\ntry: feature.flags.SKIP = cfg.SKIP\nexcept Exception: pass\n"}), "cfg.py: a module a condition reads is unread"},
		"r3 H1 an inline else suite":                                          {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\n\nif cfg.SKIP is None: pass\nelse: feature.flags.SKIP = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread"},
		"r3 H1 an inline finally suite":                                       {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\n\ntry: pass\nfinally: feature.flags.SKIP = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread"},
		"r3 H1 an inline bare except suite":                                   {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\n\ntry: pass\nexcept: feature.flags.SKIP = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread"},
		"r3 H1 an inline del":                                                 {astra(map[string]string{"conftest.py": "import feature.flags\n\nif True: del feature.flags.SKIP\n"}), "cfg.py: a module a condition reads is unread"},
		"r3 H2 a def default moving __path__":                                 {astra(map[string]string{"conftest.py": "import feature\n\ndef unused(x=feature.__path__.insert(0, \"impl\")):\n    pass\n", "impl/flags.py": "from cfg import SKIP\n"}), "cfg.py: a module a condition reads is unread"},
		"r3 H3 a module beside its package":                                   {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "import parker\n", "parker.py": "X = 1\n", "parker/__init__.py": "import cfg\nimport feature.flags\nfeature.flags.SKIP = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread (the module name parker has more than one file)"},
		"r3 H3 a module beside an extension":                                  {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "import parker\n", "parker.py": "X = 1\n", "parker.cpython-312-x86_64-linux-gnu.so": "\x00"}), "cfg.py: a module a condition reads is unread (the module name parker has more than one file)"},
		"r3 H3 a module beside cached bytecode":                               {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "import parker\n", "parker.py": "X = 1\n", "__pycache__/parker.cpython-312.pyc": "\x00"}), "cfg.py: a module a condition reads is unread (the module name parker has more than one file)"},
		"r3 a decorator outside the allowlist":                                {astra(map[string]string{"conftest.py": "import functools\n\n@functools.lru_cache\ndef f():\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r3 a default that is a dotted name is unread (a stated false block)": {astra(map[string]string{"conftest.py": "import os\n\ndef f(x=os.sep):\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r3 a default that is a call":                                         {astra(map[string]string{"conftest.py": "import os\n\ndef f(x=os.getcwd()):\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r3 an annotation that is not a name":                                 {astra(map[string]string{"conftest.py": "def f(x: list[int]):\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r3 a return annotation that is not a name":                           {astra(map[string]string{"conftest.py": "def f() -> list[int]:\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r3 a class keyword":                                                  {astra(map[string]string{"conftest.py": "class B:\n    pass\n\nclass C(B, flag=1):\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r3 a subscripted base":                                               {astra(map[string]string{"conftest.py": "from typing import Generic, TypeVar\n\nclass C(Generic[int]):\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r3 an unchanged test module's call":                                  {astra(map[string]string{"tests/test_y.py": "import os\nX = os.getpid()\n\ndef test_y():\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module tests/test_y.py runs code at import outside the allowlist)"},
		"r3 inert headers stay read":                                          {astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "conftest.py": "import pytest\n\nclass B(object):\n    pass\n\n@pytest.fixture\ndef f(x: int = 1, *a: str, y=None, **k) -> None:\n    pass\n"}), read},
		"r3 an inline try binding a name is unread (a stated false block)":    {astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "conftest.py": "try: X = 1\nfinally: Y = 2\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r3 a middle prefix two roots answer to":                              {astra(map[string]string{"tests/test_z.py": "import pytest\nimport feature\n\n@pytest.mark.skipif(feature.sub.flags.SKIP, reason='r')\ndef test_z():\n    assert False\n", "feature/sub/__init__.py": "", "feature/sub/flags.py": "SKIP = False\n", "src/feature/sub.py": "X = 1\n"}), "cfg.py: a module a condition reads is unread (the module name feature.sub in two roots)"},
		"r4 H1 a list comprehension writing an attribute":                     {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\n\nPARK = [0 for feature.flags.SKIP in [cfg.SKIP]]\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py holds a lambda, a comprehension or a generator at import)"},
		"r4 H1 a set comprehension writing an attribute":                      {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\n\nPARK = {0 for feature.flags.SKIP in [cfg.SKIP]}\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py holds a lambda, a comprehension or a generator at import)"},
		"r4 H1 a dict comprehension writing an attribute":                     {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\n\nPARK = {0: 0 for feature.flags.SKIP in [cfg.SKIP]}\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py holds a lambda, a comprehension or a generator at import)"},
		"r4 H2 a local fixture decorator":                                     {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\n\ndef fixture(f):\n    feature.flags.SKIP = cfg.SKIP\n    return f\n\n@fixture\ndef unused():\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r4 H2 a rebound patch":                                               {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\nfrom unittest.mock import patch\n\ndef patch(f):\n    feature.flags.SKIP = cfg.SKIP\n    return f\n\n@patch\ndef unused():\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r4 H2 a locally built attribute chain":                               {astra(map[string]string{"conftest.py": "import pytest\nimport cfg\nimport feature.flags\n\nclass pytest:\n    class mark:\n        def skip(f):\n            feature.flags.SKIP = cfg.SKIP\n            return f\n\n@pytest.mark.skip\ndef unused():\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r4 H2 pytest rebound through global":                                 {astra(map[string]string{"conftest.py": "import pytest\n\ndef g():\n    global pytest\n    pytest = None\n\n@pytest.fixture\ndef f():\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r4 H2 a proven fixture stays read":                                   {astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "conftest.py": "from pytest import fixture\n\n@fixture(scope='session')\ndef f():\n    pass\n"}), read},
		"r4 inert displays stay read":                                         {astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "conftest.py": "A = -1\nB = (1, 'a', None)\nC = {'k': [1, 2], 'j': {3}}\nD: int = A\n__all__ = ['A', 'B']\n"}), read},
		"r4 a skipif with an operator is unread (a stated false block)":       {astra(map[string]string{"tests/test_y.py": "import pytest\nimport sys\n\n@pytest.mark.skipif(sys.version_info < (3, 8), reason='old')\ndef test_y():\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module tests/test_y.py runs code at import outside the allowlist)"},
		"r4 H3 a sole compiled module":                                        {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "import parker\n", "parker.pyc": "\x00"}), "cfg.py: a module a condition reads is unread (the module parker is not one plain source (parker.pyc))"},
		"r4 H3 a sole extension module":                                       {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "import parker\n", "parker.cpython-312-x86_64-linux-gnu.so": "\x00"}), "cfg.py: a module a condition reads is unread (the module parker is not one plain source (parker.cpython-312-x86_64-linux-gnu.so))"},
		"r4 H3 a package's sole compiled __init__":                            {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "import parker\n", "parker/__init__.pyc": "\x00"}), "cfg.py: a module a condition reads is unread (the module parker is not one plain source (parker/__init__.pyc))"},
		"r4 H4 a module under __pycache__":                                    {astra(map[string]string{"conftest.py": "from __pycache__ import parker\n", "__pycache__/parker.py": "X = 1\n"}), "cfg.py: a module a condition reads is unread (the module __pycache__ is not one plain source (__pycache__/))"},
		"pre-check a module __getattr__ behind a dotted default":              {astra(map[string]string{"conftest.py": "import cfg\n\ndef f(x=cfg.FOO):\n    pass\n", "cfg.py": "def __getattr__(name):\n    import feature.flags\n    feature.flags.SKIP = True\n    return 1\n\nSKIP = False\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"pre-check a dotted default on an inert module":                       {astra(map[string]string{"conftest.py": "import cfg\n\ndef f(x=cfg.SKIP):\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"pre-check a dotted value":                                            {astra(map[string]string{"conftest.py": "import cfg\nX = cfg.FOO\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"pre-check a dotted annotation":                                       {astra(map[string]string{"conftest.py": "import cfg\nX: cfg.T = 1\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"pre-check a dotted display element":                                  {astra(map[string]string{"conftest.py": "import cfg\nX = [cfg.SKIP]\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"pre-check a base's __init_subclass__":                                {astra(map[string]string{"base.py": "class B:\n    def __init_subclass__(cls, **k):\n        import feature.flags\n        feature.flags.SKIP = True\n", "conftest.py": "from base import B\n\nclass A(B):\n    pass\n"}), "cfg.py: a module a condition reads is unread"},
		"pre-check a class base on an inert module":                           {astra(map[string]string{"base.py": "class B:\n    pass\n", "conftest.py": "from base import B\n\nclass A(B):\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"pre-check object rebound":                                            {astra(map[string]string{"base.py": "class B:\n    pass\n", "conftest.py": "from base import B as object\n\nclass A(object):\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"pre-check a module-level __dir__":                                    {astra(map[string]string{"conftest.py": "def __dir__():\n    return []\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"pre-check a skipif reading a module with __getattr__":                {astra(map[string]string{"feature/flags.py": "def __getattr__(name):\n    import cfg\n    return cfg.SKIP\n\nSKIP = False\n"}), "cfg.py: a module a condition reads is unread (the module feature/flags.py runs code at import outside the allowlist)"},
		"pre-check a dotted name nested in a decorator argument":              {astra(map[string]string{"conftest.py": "import pytest\nimport cfg\n\n@pytest.mark.parametrize('x', [cfg.SKIP])\ndef f(x):\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r5 H1 a lambda default with a list comprehension":                    {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\n\nPARK = 0, lambda x=[0 for feature.flags.SKIP in [cfg.SKIP]]: 0\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py holds a lambda, a comprehension or a generator at import)"},
		"r5 H1 a lambda default with a set comprehension":                     {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\n\nPARK = 0, lambda x={0 for feature.flags.SKIP in [cfg.SKIP]}: 0\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py holds a lambda, a comprehension or a generator at import)"},
		"r5 H1 a lambda default with a dict comprehension":                    {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\n\nPARK = 0, lambda x={0: 0 for feature.flags.SKIP in [cfg.SKIP]}: 0\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py holds a lambda, a comprehension or a generator at import)"},
		"r5 H1 a trailing-comma prefix before a lambda":                       {astra(map[string]string{"conftest.py": "PARK = 0, lambda: 0\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py holds a lambda, a comprehension or a generator at import)"},
		"r5 H1 a lambda value":                                                {astra(map[string]string{"conftest.py": "PARK = lambda: 0\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py holds a lambda, a comprehension or a generator at import)"},
		"r5 H1 an async comprehension":                                        {astra(map[string]string{"conftest.py": "PARK = [0 async for x in y]\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py holds a lambda, a comprehension or a generator at import)"},
		"r5 H2 a star in a module that is no package __init__.py":             {astra(map[string]string{"conftest.py": "import pkg.helper\n", "pkg/__init__.py": "", "pkg/helper.py": "from .x import *\n", "pkg/x/__init__.py": "__all__ = [\"parker\"]\n", "pkg/x/parker.py": "import cfg\nimport feature.flags\nfeature.flags.SKIP = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread (the module pkg/helper.py has a star import other than from .x import * in a package __init__.py)"},
		"r5 H2 a package star from a package with __all__":                    {astra(map[string]string{"conftest.py": "import pkg\n", "pkg/__init__.py": "from .x import *\n", "pkg/x/__init__.py": "__all__ = [\"parker\"]\n", "pkg/x/parker.py": "import cfg\nimport feature.flags\nfeature.flags.SKIP = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread (the package pkg star-imports .x, which is not one plain module file)"},
		"r5 H3 a class-private import in a class body":                        {astra(map[string]string{"conftest.py": "class C:\n    import __writer\n", "__writer.py": "X = 1\n", "_C__writer.py": "import cfg\nimport feature.flags\nfeature.flags.SKIP = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py imports in a class body or imports a class-private name)"},
		"r5 H3 an import in a class body":                                     {astra(map[string]string{"conftest.py": "class C:\n    import os\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py imports in a class body or imports a class-private name)"},
		"r5 H3 a class-private name in a method's from-import":                {astra(map[string]string{"conftest.py": "class C:\n    def m(self):\n        from helper import __x\n", "helper.py": "X = 1\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py imports in a class body or imports a class-private name)"},
		"r5 H4 a decorator argument from an external module":                  {astra(map[string]string{"conftest.py": "import pytest\nimport unittest\n\n@pytest.mark.slow(tag=unittest.IsolatedAsyncioTestCase)\ndef unused():\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py reads a decorator argument from unittest, not a repository module)"},
		"r5 H4 Astra's asyncio.py behind unittest":                            {astra(map[string]string{"conftest.py": "import pytest\nimport unittest\n\n@pytest.mark.slow(tag=unittest.IsolatedAsyncioTestCase)\ndef unused():\n    pass\n", "asyncio.py": "import cfg\nimport feature.flags\nfeature.flags.SKIP = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread"},
		"r5 H4 a base asyncio.py with an import of unittest's lazy name":      {astra(map[string]string{"conftest.py": "from unittest import IsolatedAsyncioTestCase\n", "asyncio.py": "import cfg\nimport feature.flags\nfeature.flags.SKIP = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread (the module asyncio.py shadows the standard library's asyncio)"},
		"r5 H4 a stdlib name under a pythonpath root":                         {astra(map[string]string{"pytest.ini": "[pytest]\npythonpath = lib\n", "lib/asyncio/__init__.py": ""}), "cfg.py: a module a condition reads is unread (the module lib/asyncio/__init__.py shadows the standard library's asyncio)"},
		"r5 H4 a decorator argument root bound twice":                         {astra(map[string]string{"conftest.py": "import pytest\nimport feature.flags\n\n@pytest.mark.slow(tag=feature.flags.SKIP)\ndef unused():\n    import os as feature\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py reads a decorator argument through feature, which one import does not bind)"},
		"r5 H4 a decorator argument root rebound":                             {astra(map[string]string{"conftest.py": "import pytest\nimport feature.flags\n\n@pytest.mark.slow(tag=feature.flags.SKIP)\ndef unused(feature):\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py reads a decorator argument through feature, which one import does not bind)"},
		"r5 H4 a repository module's dotted decorator argument stays read":    {astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "conftest.py": "import pytest\nimport feature.flags\n\n@pytest.mark.slow(tag=feature.flags.SKIP)\ndef unused():\n    pass\n"}), read},
		"r5 H4 a module name no stdlib module has stays read":                 {astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "conftest.py": "import myhelpers\n", "myhelpers.py": "X = 1\n"}), read},
		"r5 M2 a named Unicode escape":                                        {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "\"\"\" \\N{LATIN CAPITAL LETTER A} \"\"\"\n"}), "cfg.py: a module a condition reads is unread (the module helper.py names a character by"},
		"r5 M2 a named Unicode escape in a function body":                     {astra(map[string]string{"conftest.py": "def f():\n    return '\\N{BULLET}'\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py names a character by"},
		"r5 L1 a module beside a data directory stays read":                   {astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "conftest.py": "import data\n", "data.py": "X = 1\n", "data/x.txt": "x\n"}), read},
		"r5 L1 a namespace-only import":                                       {astra(map[string]string{"conftest.py": "import ns\n", "ns/data.txt": "x\n"}), "cfg.py: a module a condition reads is unread (the module ns is not one plain source (ns/))"},
		"r5 a package star rebinding pytest":                                  {astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "conftest.py": "import pkg\n", "pkg/__init__.py": "import pytest\nfrom .x import *\n\n@pytest.fixture\ndef f():\n    pass\n", "pkg/x.py": "pytest = 1\n"}), "cfg.py: a module a condition reads is unread (the module pkg/__init__.py runs code at import outside the allowlist)"},
		"r6 M1 a mark applied to a repository function":                       {map[string]string{"cfg.py": "ENABLE = False\n", "flags.py": "SKIP = False\n", "conftest.py": "import pytest\nimport cfg\nimport flags\n\ndef park(f):\n    flags.SKIP = cfg.ENABLE\n    return f\n\n@pytest.mark.usefixtures(park)\ndef unused():\n    pass\n", "tests/test_z.py": "import pytest\nimport flags\n\n@pytest.mark.skipif(flags.SKIP, reason='r')\ndef test_z():\n    assert False\n"}, "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r6 M1 a mark with one dotted positional argument":                    {astra(map[string]string{"conftest.py": "import pytest\nimport feature.flags\n\n@pytest.mark.usefixtures(feature.flags.SKIP)\ndef unused():\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r6 M1 a mark with one literal argument stays read":                   {astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "conftest.py": "import pytest\n\n@pytest.mark.usefixtures('db')\ndef unused():\n    pass\n"}), read},
		"r6 M1 with_args stays read":                                          {astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "conftest.py": "import pytest\n\nX = 1\n\n@pytest.mark.slow.with_args(X)\ndef unused():\n    pass\n"}), read},
		"r6 L1 a namespace with a module":                                     {astra(map[string]string{"conftest.py": "import ns\n", "ns/leaf.py": "X = 1\n"}), "cfg.py: a module a condition reads is unread (the module ns is not one plain source (ns/))"},
		"r6 L1 a namespace with only a nested directory":                      {astra(map[string]string{"conftest.py": "import ns\n", "ns/sub/data.txt": "x\n"}), "cfg.py: a module a condition reads is unread (the module ns is not one plain source (ns/))"},
		"r6 L1 a reached namespace ancestor":                                  {astra(map[string]string{"conftest.py": "import ns.leaf\n", "ns/leaf.py": "X = 1\n"}), "cfg.py: a module a condition reads is unread (the module ns is not one plain source (ns/))"},
		"r6 L1 a stdlib-named directory with no __init__ stays read":          {astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "cmd/main.go": "package main\n", "email/x.py": "X = 1\n"}), read},
		"r6 L1 a reached stdlib-named namespace is unread":                    {astra(map[string]string{"conftest.py": "import email.x\n", "email/x.py": "X = 1\n"}), "cfg.py: a module a condition reads is unread (the module email is not one plain source (email/))"},
		"r6 M1 a trailing comma after one positional argument":                {astra(map[string]string{"conftest.py": "import pytest\n\ndef park(f):\n    return f\n\n@pytest.mark.usefixtures(park,)\ndef unused():\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r6 a module under a regular __pycache__ package":                     {astra(map[string]string{"conftest.py": "from __pycache__ import parker\n", "__pycache__/__init__.py": "", "__pycache__/parker.py": "X = 1\n"}), "cfg.py: a module a condition reads is unread (the module __pycache__/__init__.py is under __pycache__ or is pytest's)"},
		"r4 a base module named pytest":                                       {astra(map[string]string{"pytest.py": "X = 1\n"}), "cfg.py: a module a condition reads is unread (the module pytest.py is under __pycache__ or is pytest's)"},
		"r4 an inert package that is not plain":                               {astra(map[string]string{"feature/__init__.py": "class C:\n    pass\n"}), "cfg.py: a module a condition reads is unread (the package feature is not plain"},
		"r4 a default with an operator":                                       {astra(map[string]string{"conftest.py": "def f(x=1 + 2):\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r4 a unary minus on a name":                                          {astra(map[string]string{"conftest.py": "Y = 1\nX = -Y\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r4 a dict value with an operator":                                    {astra(map[string]string{"conftest.py": "X = {'k': 1 + 2}\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r4 a bare tuple with an operator":                                    {astra(map[string]string{"conftest.py": "X = 1, 2 + 3\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r4 an f-string value":                                                {astra(map[string]string{"conftest.py": "X = f'{1}'\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r4 a chained assignment":                                             {astra(map[string]string{"conftest.py": "X = Y = 1\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r4 a decorator argument with an operator":                            {astra(map[string]string{"conftest.py": "import pytest\n\n@pytest.fixture(scope='s' + 'ession')\ndef f():\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r4 a decorator with no pytest import":                                {astra(map[string]string{"conftest.py": "@pytest.fixture\ndef f():\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r4 strings and an operator":                                          {astra(map[string]string{"conftest.py": "X = 'a' + 'b'\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"r4 a source under __pycache__ is no form of its namesake":            {astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "conftest.py": "import parker\n", "parker.py": "X = 1\n", "__pycache__/parker.py": "X = 2\n"}), read},
		"r4 pytest rebound by a relative star":                                {astra(map[string]string{"tests/__init__.py": "", "tests/helpers.py": "class pytest:\n    def fixture(f):\n        return f\n", "tests/conftest.py": "import pytest\nfrom .helpers import *\n\n@pytest.fixture\ndef f():\n    pass\n"}), "cfg.py: a module a condition reads is unread (the module tests/conftest.py has a star import other than from .x import * in a package __init__.py)"},
		"r2 H1 a rebound import head":                                         {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\nimport os as m\n\nm = feature.flags\nm.SKIP = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread"},
		"r2 H1 a class attribute holding a module":                            {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\n\nclass C:\n    target = feature.flags\n\nC.target.SKIP = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread"},
		"r2 H1 a literal list holding a module":                               {astra(map[string]string{"conftest.py": "import cfg\nimport feature.flags\n\nbox = []\nbox[:] = [feature.flags]\nbox[0].SKIP = cfg.SKIP\n"}), "cfg.py: a module a condition reads is unread"},
		"r2 H2 a re-export shadowed by a local import":                        {astra(map[string]string{"conftest.py": "import writer\n", "bridge.py": "import feature.flags as target\n\ndef unused():\n    import os as target\n", "writer.py": "import cfg\nimport bridge\nsetattr(bridge.target, \"SKIP\", cfg.SKIP)\n"}), "cfg.py: a module a condition reads is unread"},
		"os.environ in a conftest is unread (a stated false block)":           {astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "conftest.py": "import os\nos.environ['MODE'] = '1'\n"}), "cfg.py: a module a condition reads is unread"},
		"a fixture's monkeypatch.setattr is read":                             {astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "conftest.py": "import pytest\n\n@pytest.fixture\ndef env(monkeypatch):\n    monkeypatch.setattr('os.sep', '/')\n"}), read},
		"a name annotated by a name in a conftest is read":                    {astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "conftest.py": "X: int = 1\n"}), read},
		"an annotated name in a conftest is unread (a stated false block)":    {astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "conftest.py": "X: dict[str, int] = {}\n"}), "cfg.py: a module a condition reads is unread (the module conftest.py runs code at import outside the allowlist)"},
		"a method writing self in a conftest is read":                         {astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "conftest.py": "class C:\n    def set(self):\n        self.x = 1\n"}), read},
	} {
		g := newFixtureWith(t, func(src string) {
			write(t, src, c.base)
			plain(t, src, "add", "--all")
		})
		if v := g.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); !pyFinds(pyOwn(v.Blocks), c.want) {
			t.Errorf("%s: %q, want %q", name, whats(pyOwn(v.Blocks)), c.want)
		}
	}
	for name, c := range map[string]struct {
		base, change map[string]string
		want         string
	}{
		"r3 H3 a new test reaching a module beside its package": {astra(map[string]string{"helper.py": "import parker\n", "parker.py": "X = 1\n", "parker/__init__.py": "import feature.flags\nfeature.flags.SKIP = True\n"}),
			map[string]string{"tests/test_a.py": "import helper\n\ndef test_a():\n    pass\n"}, "helper.py: a module the tests now import runs code at import"},
		"r3 a new test reaching a module that is not inert (a stated false block)": {astra(map[string]string{"helper.py": "import logging\nlogging.getLogger(__name__)\n"}),
			map[string]string{"tests/test_a.py": "import helper\n\ndef test_a():\n    pass\n"}, "helper.py: a module the tests now import runs code at import"},
		"r3 a changed module's own call stays main's warning": {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n"}),
			map[string]string{"helper.py": "import logging\nlogging.getLogger(__name__)\nX = 1\n"}, ""},
		"r3 H1 a changed module's inline try write": {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n"}),
			map[string]string{"helper.py": "import feature.flags\ntry: feature.flags.SKIP = True\nexcept Exception: pass\n"}, "helper.py: a module the tests now import runs code at import"},
		"r3 H1 a changed module's inline del": {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n"}),
			map[string]string{"helper.py": "import feature.flags\nif True: del feature.flags.SKIP\n"}, "helper.py: a module the tests now import runs code at import"},
		"r3 a changed module running exec": {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n"}),
			map[string]string{"helper.py": "exec('X = 1')\n"}, "helper.py: a module the tests now import runs code at import"},
		"r3 H2 a changed module's def default moving __path__": {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n"}),
			map[string]string{"helper.py": "import feature\n\ndef unused(x=feature.__path__.insert(0, 'impl')):\n    pass\n"}, "helper.py: a module the tests now import runs code at import"},
		"r3 a new star beside a package form": {astra(map[string]string{"feature/redirect.py": "X = 1\n", "feature/redirect/__init__.py": ""}),
			map[string]string{"feature/__init__.py": "from .redirect import *\n"}, "feature/__init__.py: a package __init__.py that star-imports .redirect, which is not one plain module file"},
		"r3 a new star beside cached bytecode": {astra(map[string]string{"feature/redirect.py": "X = 1\n", "feature/__pycache__/redirect.cpython-312.pyc": "\x00"}),
			map[string]string{"feature/__init__.py": "from .redirect import *\n"}, "feature/__init__.py: a package __init__.py that star-imports .redirect, which is not one plain module file"},
		"r4 H1 a new test reaching a comprehension writer": {astra(map[string]string{"parker.py": "import feature.flags\nPARK = [0 for feature.flags.SKIP in [True]]\n"}),
			map[string]string{"tests/test_a.py": "import parker\n\ndef test_a():\n    pass\n"}, "parker.py: a module the tests now import runs code at import"},
		"r4 H3 a changed helper newly importing a sole compiled module": {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n", "parker.pyc": "\x00"}),
			map[string]string{"helper.py": "import parker\nX = 1\n"}, "helper.py: a module the tests now import runs code at import"},
		"r4 H3 a changed helper newly importing a sole extension module": {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n", "parker.cpython-312-x86_64-linux-gnu.so": "\x00"}),
			map[string]string{"helper.py": "import parker\nX = 1\n"}, "helper.py: a module the tests now import runs code at import"},
		"r4 H4 a changed helper newly importing __pycache__/parker.py": {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n", "__pycache__/parker.py": "import feature.flags\nfeature.flags.SKIP = True\n"}),
			map[string]string{"helper.py": "import __pycache__.parker\nX = 1\n"}, "helper.py: a module the tests now import runs code at import"},
		"r4 M1 class metaclass blocks as on main": {astra(nil),
			map[string]string{"tests/test_a.py": "class metaclass:\n    pass\n\ndef test_a():\n    pass\n"}, "tests/test_a.py: unverifiable: code run at import outside the allowlist ([class metaclass :])"},
		"r5 H1 a new test reaching a lambda writer": {astra(map[string]string{"parker.py": "import feature.flags\nPARK = 0, lambda x=[0 for feature.flags.SKIP in [True]]: 0\n"}),
			map[string]string{"tests/test_a.py": "import parker\n\ndef test_a():\n    pass\n"}, "parker.py: a module the tests now import runs code at import"},
		"r5 H2 a new test reaching a star outside a package __init__.py": {astra(map[string]string{"pkg/__init__.py": "", "pkg/helper.py": "from .x import *\n", "pkg/x/__init__.py": "__all__ = [\"parker\"]\n", "pkg/x/parker.py": "import feature.flags\nfeature.flags.SKIP = True\n"}),
			map[string]string{"tests/test_a.py": "import pkg.helper\n\ndef test_a():\n    pass\n"}, "pkg/helper.py: a module the tests now import runs code at import"},
		"r5 H3 a changed helper's class-private import": {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n", "_C__writer.py": "import feature.flags\nfeature.flags.SKIP = True\n"}),
			map[string]string{"helper.py": "class C:\n    import __writer\n"}, "helper.py: a module the tests now import runs code at import"},
		"r5 H4 a changed test module's external decorator argument": {astra(map[string]string{"tests/test_y.py": "def test_y():\n    pass\n"}),
			map[string]string{"tests/test_y.py": "import pytest\nimport unittest\n\n@pytest.mark.slow(tag=unittest.IsolatedAsyncioTestCase)\ndef test_y():\n    pass\n"}, "tests/test_y.py: a module the tests now import runs code at import"},
		"r5 H4 the candidate adds asyncio.py": {astra(nil),
			map[string]string{"asyncio.py": "X = 1\n"}, "asyncio.py: a module the tests now import runs code at import"},
		"r5 M2 a changed helper's named Unicode escape": {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n"}),
			map[string]string{"helper.py": "X = '\\N{BULLET}'\n"}, "helper.py: a module the tests now import runs code at import"},
		"r5 L1 a changed helper newly importing a namespace-only name": {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n", "ns/data.txt": "x\n"}),
			map[string]string{"helper.py": "import ns\nX = 1\n"}, "helper.py: a module the tests now import runs code at import"},
		"r5 H1 a changed helper's comprehension writer": {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n"}),
			map[string]string{"helper.py": "import feature.flags\nPARK = [0 for feature.flags.SKIP in [True]]\n"}, "helper.py: a module the tests now import runs code at import"},
		"r5 H1 a changed helper's lambda": {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n"}),
			map[string]string{"helper.py": "PARK = lambda: 0\n"}, "helper.py: a module the tests now import runs code at import"},
		"r5 a changed helper's import path in a string": {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n"}),
			map[string]string{"helper.py": "import feature\nX = getattr(feature, '__path__')\n"}, "helper.py: a module the tests now import runs code at import"},
		"r5 H2 a new test reaching a package star the base never judged": {astra(map[string]string{"pkg/__init__.py": "from .x import *\n", "pkg/x/__init__.py": "__all__ = ['parker']\n", "pkg/x/parker.py": "import feature.flags\nfeature.flags.SKIP = True\n"}),
			map[string]string{"tests/test_a.py": "import pkg\n\ndef test_a():\n    pass\n"}, "pkg/__init__.py: a module the tests now import runs code at import"},
		"r6 M1 a new test reaching a mark applied to a function": {astra(map[string]string{"helper.py": "import pytest\nimport feature.flags\n\ndef park(f):\n    feature.flags.SKIP = True\n    return f\n\n@pytest.mark.usefixtures(park)\ndef unused():\n    pass\n"}),
			map[string]string{"tests/test_a.py": "import helper\n\ndef test_a():\n    pass\n"}, "helper.py: a module the tests now import runs code at import"},
		"r6 L1 a changed helper newly reaching a namespace with a module": {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n", "ns/leaf.py": "X = 1\n"}),
			map[string]string{"helper.py": "import ns\nX = 1\n"}, "helper.py: a module the tests now import runs code at import"},
		"r6 an unchanged impure test stays the base's verdict": {astra(map[string]string{"tests/test_y.py": "import os\nX = os.getpid()\n\ndef test_y():\n    pass\n"}),
			map[string]string{"notes.txt": "x\n"}, ""},
		"r6 a changed helper newly reaching a module under a regular __pycache__ package": {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n", "__pycache__/__init__.py": "", "__pycache__/parker.py": "import feature.flags\nfeature.flags.SKIP = True\n"}),
			map[string]string{"helper.py": "import __pycache__.parker\nX = 1\n"}, "helper.py: a module the tests now import runs code at import"},
		"r6 a source under __pycache__ is no form of its namesake, newly reached": {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n", "parker.py": "X = 1\n", "__pycache__/parker.py": "X = 2\n"}),
			map[string]string{"helper.py": "import parker\nX = 1\n"}, ""},
		"H1 a candidate helper newly importing an alias writer": {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n", "parker.py": "import feature.flags\nm = feature.flags\nm.SKIP = True\n"}),
			map[string]string{"helper.py": "import parker\nX = 1\n"}, "parker.py: a module the tests now import runs code at import"},
		"H5 a new test importing an unread module": {astra(map[string]string{"parker.py": "# coding: latin-1\nimport feature.flags\nfeature.flags.SKIP = True\n"}),
			map[string]string{"tests/test_a.py": "import parker\n\ndef test_a():\n    pass\n"}, "parker.py: a module the tests now import runs code at import"},
		"M1 a new test with a 100,000-component import":             {astra(nil), map[string]string{"tests/test_a.py": "__import__('" + strings.TrimSuffix(strings.Repeat("a.", 100000), ".") + "')\n\ndef test_a():\n    pass\n"}, "tests/test_a.py: a module the tests now import runs code at import"},
		"kp5 the candidate gives a package a call in a default":     {astra(map[string]string{"feature/__init__.py": "", "feature/_r.py": "def g(f):\n    return f\n", "feature/flags.py": "SKIP = False\n", "tests/test_y.py": "def test_y():\n    pass\n"}), map[string]string{"feature/__init__.py": "from ._r import g\n\ndef f(x=g(1)):\n    pass\n"}, "feature/__init__.py: a package __init__.py that is not plain changed"},
		"kp6 the candidate gives a package a dunder import":         {astra(map[string]string{"feature/__init__.py": "", "feature/_r.py": "def g(f):\n    return f\n", "feature/flags.py": "SKIP = False\n", "tests/test_y.py": "def test_y():\n    pass\n"}), map[string]string{"feature/__init__.py": "from ._r import g as __getattr__\n"}, "feature/__init__.py: a package __init__.py that is not plain changed"},
		"kp7 the candidate gives a package its own __path__":        {astra(map[string]string{"feature/__init__.py": "", "feature/_r.py": "def g(f):\n    return f\n", "feature/flags.py": "SKIP = False\n", "tests/test_y.py": "def test_y():\n    pass\n"}), map[string]string{"feature/__init__.py": "__path__ = [\"elsewhere\"]\n"}, "feature/__init__.py: a package __init__.py that is not plain changed"},
		"kp8 the candidate gives a package an f-string":             {astra(map[string]string{"feature/__init__.py": "", "feature/_r.py": "def g(f):\n    return f\n", "feature/flags.py": "SKIP = False\n", "tests/test_y.py": "def test_y():\n    pass\n"}), map[string]string{"feature/__init__.py": "X = f\"{1}\"\n"}, "feature/__init__.py: a package __init__.py that is not plain changed"},
		"kp9 the candidate star-imports a source binding __all__":   {astra(map[string]string{"feature/__init__.py": "", "feature/redirect.py": "X = 1\n", "feature/flags.py": "", "tests/test_y.py": "def test_y():\n    pass\n"}), map[string]string{"feature/__init__.py": "from .redirect import *\n", "feature/redirect.py": "__all__ = ['X']\nX = 1\n"}, "feature/__init__.py: a package __init__.py that star-imports .redirect"},
		"r4 the candidate gives a package a subscript of a literal": {astra(map[string]string{"feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "tests/test_y.py": "def test_y():\n    pass\n"}), map[string]string{"feature/__init__.py": "X = ['feature/impl'][0]\n"}, "feature/__init__.py: a package __init__.py that is not plain changed"},
		"a pure holder's import shadowed":                           {astra(map[string]string{"helper.py": "import yaml\nX = 1\n", "tests/test_y.py": "import helper\n\ndef test_y():\n    pass\n"}), map[string]string{"yaml.py": "X = 1\n"}, "yaml.py: an added module shadows a module helper.py imports"},
		"an import cycle in the candidate passes": {astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n", "a.py": "import b\n", "b.py": "import a\n"}),
			map[string]string{"helper.py": "import a\nX = 1\n"}, ""},
	} {
		g := newFixtureWith(t, func(src string) {
			write(t, src, c.base)
			plain(t, src, "add", "--all")
		})
		if v := g.guarded(t, c.change); c.want == "" && len(pyOwn(v.Blocks)) > 0 || c.want != "" && !pyFinds(pyOwn(v.Blocks), c.want) {
			t.Errorf("%s: %q, want %q", name, whats(pyOwn(v.Blocks)), c.want)
		}
	}
	for name, c := range map[string]struct {
		links  map[string]string // a link, to its text
		change string
	}{
		"H4 a .. after a linked directory": {map[string]string{"feature/flags.py": "../alias/../real_flags.txt", "alias": "real/sub"}, "real/real_flags.txt"},
		"H4 a target through another link": {map[string]string{"feature/flags.py": "../alias/real_flags.txt", "alias": "real"}, "real/real_flags.txt"},
		"H4 a target that is the root":     {map[string]string{"loop": "."}, "notes.txt"},
	} {
		g := newFixtureWith(t, func(src string) {
			write(t, src, astra(map[string]string{"feature/flags.py": "", "real/sub/.keep": "", "real/real_flags.txt": "SKIP = False\n", "notes.txt": "x\n"}))
			for l, to := range c.links {
				if err := os.Remove(filepath.Join(src, l)); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				} else if err := os.Symlink(to, filepath.Join(src, l)); err != nil {
					t.Fatal(err)
				}
			}
			plain(t, src, "add", "--all")
		})
		if v := g.guarded(t, map[string]string{c.change: "SKIP = True\n"}); !pyFinds(pyOwn(v.Blocks), c.change+": a change at or under a base symlink's target") {
			t.Errorf("%s: %q", name, whats(pyOwn(v.Blocks)))
		}
	}
	deep := astra(map[string]string{"conftest.py": "import " + strings.TrimSuffix(strings.Repeat("a.", 100000), ".") + "\n", "a.py": ""}) // M1: 100,000 components, in an inert import (round 5: __import__ is not inert)
	g := newFixtureWith(t, func(src string) {
		write(t, src, deep)
		plain(t, src, "add", "--all")
	})
	start := time.Now()
	if v := g.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); time.Since(start) > 20*time.Second || !pyFinds(pyOwn(v.Blocks), "cfg.py: a module a condition reads is unread (the module conftest.py imports a name past the depth bound)") {
		t.Errorf("M1 a 100,000-component import (%v): %q", time.Since(start), whats(pyOwn(v.Blocks)))
	}
	old := maxWork // M1: a 60 KB name charged by its bytes, in either closure; the rest of each run is far below the bound
	t.Cleanup(func() { maxWork = old })
	maxWork = 40000
	name := strings.TrimSuffix(strings.Repeat(strings.Repeat("b", 1000)+".", 60), ".")
	g = newFixtureWith(t, func(src string) {
		write(t, src, astra(map[string]string{"conftest.py": "import " + name + "\n"}))
		plain(t, src, "add", "--all")
	})
	if v := g.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); !pyFinds(pyOwn(v.Blocks), "cfg.py: a module a condition reads is unread (the base conditions' analysis is over the guard's work bound)") {
		t.Errorf("M1 a long name charged by its bytes: %q", whats(pyOwn(v.Blocks)))
	}
	g = newFixtureWith(t, func(src string) {
		write(t, src, astra(map[string]string{"conftest.py": "import helper\n", "helper.py": "X = 1\n"}))
		plain(t, src, "add", "--all")
	})
	if v := g.guarded(t, map[string]string{"helper.py": "__import__('" + name + "')\nX = 1\n"}); !pyFinds(pyOwn(v.Blocks), "the candidate's closure is over the guard's work bound") {
		t.Errorf("M1 a long candidate name charged by its bytes: %q", whats(pyOwn(v.Blocks)))
	}
	g = newFixtureWith(t, func(src string) { // Astra #281 r2 H5: a base symlink the candidate newly reaches
		write(t, src, astra(map[string]string{"payload.txt": "import feature.flags\nfeature.flags.SKIP = True\n"}))
		if err := os.Symlink("payload.txt", filepath.Join(src, "parker.py")); err != nil {
			t.Fatal(err)
		}
		plain(t, src, "add", "--all")
	})
	if v := g.guarded(t, map[string]string{"tests/test_a.py": "import parker\n\ndef test_a():\n    pass\n"}); !pyFinds(pyOwn(v.Blocks), "tests/test_a.py: a module the tests now import runs code at import") {
		t.Errorf("r2 H5 a newly reached base symlink: %q", whats(pyOwn(v.Blocks)))
	}
	g = newFixtureWith(t, func(src string) { // a judged base symlink whose link text is an inert statement: unread, not read as that text
		write(t, src, astra(nil))
		if err := os.Symlink("X=1", filepath.Join(src, "tests", "test_y.py")); err != nil {
			t.Fatal(err)
		}
		plain(t, src, "add", "--all")
	})
	if v := g.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); !pyFinds(pyOwn(v.Blocks), "cfg.py: a module a condition reads is unread (the module tests/test_y.py is unread)") {
		t.Errorf("a judged base symlink: %q", whats(pyOwn(v.Blocks)))
	}
	maxWork = old
	fan := map[string]string{"conftest.py": "import writer\n", "writer.py": "import fanout\nlen(())\n"} // Astra #281 r2 M1: 60 aliases of one 60,000-character name
	long := strings.Repeat("n", 60000)
	fan["leaf.py"] = long + " = 1\n"
	for i := range 60 {
		fan["fanout.py"] += fmt.Sprintf("from leaf import %s as a%d\n", long, i)
	}
	g = newFixtureWith(t, func(src string) {
		write(t, src, astra(fan))
		plain(t, src, "add", "--all")
	})
	start = time.Now()
	if v := g.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); time.Since(start) > 20*time.Second || !pyFinds(pyOwn(v.Blocks), "cfg.py: a module a condition reads is unread") {
		t.Errorf("r2 M1 a 60-alias fanout (%v): %q", time.Since(start), whats(pyOwn(v.Blocks)))
	}
	paths, links := "", []string{"update-index", "--add"} // Astra #281 r3 M1: 2,000 pythonpath roots and 5,000 gitlinks, a test-only bound
	for i := range 2000 {
		paths += fmt.Sprintf(" p%d", i)
	}
	for i := range 5000 {
		links = append(links, "--cacheinfo", fmt.Sprintf("160000,1111111111111111111111111111111111111111,vendor/g%d", i))
	}
	g = newFixtureWith(t, func(src string) {
		write(t, src, astra(map[string]string{"pytest.ini": "[pytest]\npythonpath =" + paths + "\n"}))
		plain(t, src, "add", "--all")
		plain(t, src, links...)
	})
	maxWork = 20000
	start = time.Now()
	if v := g.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); time.Since(start) > 20*time.Second || !pyFinds(pyOwn(v.Blocks), "cfg.py: a module a condition reads is unread (the base conditions' analysis is over the guard's work bound)") {
		t.Errorf("r3 M1 roots and gitlinks (%v): %q", time.Since(start), whats(pyOwn(v.Blocks)))
	}
	forms := map[string]string{"pytest.ini": "[pytest]\npythonpath =" + paths + "\n"} // and 20 compiled forms over the same roots
	for i := range 20 {
		forms[fmt.Sprintf("__pycache__/m%d.cpython-312.pyc", i)] = "\x00"
	}
	g = newFixtureWith(t, func(src string) {
		write(t, src, astra(forms))
		plain(t, src, "add", "--all")
	})
	if v := g.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); !pyFinds(pyOwn(v.Blocks), "cfg.py: a module a condition reads is unread (the base conditions' analysis is over the guard's work bound)") {
		t.Errorf("r3 M1 roots and compiled forms: %q", whats(pyOwn(v.Blocks)))
	}
	nsdirs := map[string]string{"pytest.ini": "[pytest]\npythonpath =" + paths + "\n"} // and 20 data directories over the same roots (Astra #281 r5 L1)
	for i := range 20 {
		nsdirs[fmt.Sprintf("data%d/x.txt", i)] = "x\n"
	}
	g = newFixtureWith(t, func(src string) {
		write(t, src, astra(nsdirs))
		plain(t, src, "add", "--all")
	})
	if v := g.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); !pyFinds(pyOwn(v.Blocks), "cfg.py: a module a condition reads is unread (the base conditions' analysis is over the guard's work bound)") {
		t.Errorf("r5 M1 roots and namespace directories: %q", whats(pyOwn(v.Blocks)))
	}
	pkg := astra(nil) // Astra #281 r4 M2: 2,000 unchanged modules in one directory (her 20,000, scaled), a test-only bound
	for i := range 2000 {
		pkg[fmt.Sprintf("pkg/m%d.py", i)] = ""
	}
	g = newFixtureWith(t, func(src string) {
		write(t, src, pkg)
		plain(t, src, "add", "--all")
	})
	maxWork = 13000 // the seeding charges 2,000 visits and 4,000 directory entries: without either, this run fits
	start = time.Now()
	if v := g.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); time.Since(start) > 20*time.Second || !pyFinds(pyOwn(v.Blocks), "the candidate's closure is over the guard's work bound") {
		t.Errorf("r4 M2 the candidate's seeding charged (%v): %q", time.Since(start), whats(pyOwn(v.Blocks)))
	}
	many := "import pytest\nimport feature.flags\n\n" // Astra #281 r6 M2: 2,000 decorators reading one dotted root (her 10,000, scaled), a test-only bound
	for i := range 2000 {
		many += fmt.Sprintf("@pytest.mark.slow(tag=feature.flags.SKIP)\ndef f%05d():\n    pass\n\n", i)
	}
	g = newFixtureWith(t, func(src string) {
		write(t, src, astra(map[string]string{"feature/flags.py": "from cfg import SKIP\n", "conftest.py": many}))
		plain(t, src, "add", "--all")
	})
	maxWork = 60000 // one binding scan per file (about 44,000 in all): one per decorator would pass it
	start = time.Now()
	if v := g.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); time.Since(start) > 20*time.Second || !pyFinds(pyOwn(v.Blocks), "cfg.py: a skipif or xfail condition in tests/test_z.py reads SKIP") {
		t.Errorf("r6 M2 one binding scan per file (%v): %q", time.Since(start), whats(pyOwn(v.Blocks)))
	}
	roots := "import pytest\n" // and 17 distinct roots: past pyMaxRoots, not inert
	for i := range 17 {
		roots += fmt.Sprintf("import m%d\n\n@pytest.mark.slow(tag=m%d.X)\ndef f%d():\n    pass\n\n", i, i, i)
	}
	g = newFixtureWith(t, func(src string) {
		write(t, src, astra(map[string]string{"conftest.py": roots}))
		plain(t, src, "add", "--all")
	})
	maxWork = old
	if v := g.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); !pyFinds(pyOwn(v.Blocks), "cfg.py: a module a condition reads is unread (the module conftest.py reads decorator arguments through too many names)") {
		t.Errorf("r6 M2 too many roots: %q", whats(pyOwn(v.Blocks)))
	}
	deep = astra(nil) // Astra #281 r5 M1: 2,000 modules beneath 200 one-letter directories (her 20,000 and 1,000, scaled), a test-only bound
	dir := strings.Repeat("d/", 200)
	for i := range 2000 {
		deep[fmt.Sprintf("%sm%d.py", dir, i)] = ""
	}
	g = newFixtureWith(t, func(src string) {
		write(t, src, deep)
		plain(t, src, "add", "--all")
	})
	maxWork = 24000 // each directory walked once: 200 walks per file would pass it
	start = time.Now()
	if v := g.guarded(t, map[string]string{"cfg.py": "SKIP = 'True'\n"}); time.Since(start) > 20*time.Second || len(pyOwn(v.Blocks)) > 0 {
		t.Errorf("r5 M1 a deep directory walked once (%v): %q", time.Since(start), whats(pyOwn(v.Blocks)))
	}
	maxWork = old
}
