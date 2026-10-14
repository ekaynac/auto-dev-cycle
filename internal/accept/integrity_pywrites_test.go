// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"strings"
	"testing"
	"time"
)

// F (P14c-2a; #226 entry 84's monkeypatch and forgery-in-fixtures flag):
// a write into a pytest object, or one handed out, from a fixture, a test
// or a module's import, blocks as pytest internals.
func TestPyWritesBlock(t *testing.T) {
	f := newGuardFixture(t)
	conf := func(body string) map[string]string { return map[string]string{"tests/conftest.py": pyFix + body} }
	test := func(body string) map[string]string {
		return map[string]string{"tests/test_money.py": pyT + "\n" + body}
	}
	for name, c := range map[string]guardCase{
		"F request.node.obj replaced":   {conf("    request.node.obj = lambda *a, **k: None\n"), nil, "tests/conftest.py: a pytest object written or handed out (request) in forge"},
		"through an alias":              {conf("    n = request.node\n    n.obj = None\n"), nil, "(n) in forge"},
		"over the session's items":      {conf("    for item in request.session.items:\n        item.obj = None\n"), nil, "(item) in forge"},
		"an xfail marker added":         {conf("    request.node.add_marker(pytest.mark.xfail)\n"), nil, "(request) in forge"},
		"in a one-liner":                {conf("    if True: request.node.obj = None\n"), nil, "(request) in forge"},
		"deleted":                       {conf("    del request.node.obj\n"), nil, "(request) in forge"},
		"a session handed out":          {conf("    return request.session\n"), nil, "(request) in forge"},
		"pytestconfig":                  {map[string]string{"tests/conftest.py": "import pytest\n\n@pytest.fixture\ndef c(pytestconfig):\n    pytestconfig.option.x = 1\n"}, nil, "(pytestconfig) in c"},
		"F pytest.Function.runtest":     {test("import pytest\npytest.Function.runtest = lambda self: None\n"), nil, "tests/test_money.py: a pytest object written or handed out (pytest) in module level"},
		"from pytest import Function":   {test("from pytest import Function as F\nF.runtest = lambda self: None\n"), nil, "(F) in module level"},
		"a class based on one":          {test("import pytest\n\nclass G(pytest.Function):\n    pass\n\nG.runtest = None\n"), nil, "(G) in module level"},
		"unittest.TestCase.run":         {test("import unittest\nunittest.TestCase.run = lambda *a: None\n"), nil, "(unittest) in module level"},
		"in a helper's function":        {map[string]string{"money.py": baseFiles["money.py"] + "\ndef patch():\n    import pytest\n    pytest.Item.runtest = None\n"}, nil, "money.py: a pytest object written or handed out (pytest) in patch"},
		"monkeypatch.setattr(pytest..)": {test("import pytest\n\ndef test_x(monkeypatch):\n    monkeypatch.setattr(pytest.Function, 'runtest', lambda s: None)\n"), nil, "(pytest) in test_x"},
		"mock.patch a pytest path":      {test("from unittest import mock\n\n@mock.patch('pytest.Function.runtest')\ndef test_y(m):\n    pass\n"), nil, "(a string target) in test_y"},
		"a seed rebound":                {test("import types as pytest\n"), nil, "(pytest) in (import)"},
		"in an f-string field":          {test("def test_f(request):\n    x = f\"{request.node.add_marker('xfail')}\"\n"), nil, "(request) in test_f"},
	} {
		pyBlocks(t, f, name, c.files, c.why)
	}
	// Bounded: a chain of a million mutating calls on one line.
	start := time.Now()
	pyBlocks(t, f, "a long call chain", map[string]string{"money.py": "x = []" + strings.Repeat(".append(1)", 1<<20/10) + "\n"}, "money.py: unverifiable: its analysis is over the guard's work bound")
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("a long call chain: %v", d)
	}
}
