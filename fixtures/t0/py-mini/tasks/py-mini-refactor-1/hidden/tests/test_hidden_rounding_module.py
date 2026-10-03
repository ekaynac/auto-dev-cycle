# SPDX-License-Identifier: Apache-2.0
# Hidden acceptance tests for py-mini-refactor-1. The new module is imported
# inside each test so that its absence is a test failure, not a collection error.
import importlib
import importlib.util
from decimal import Decimal

import invoicekit
from invoicekit import money


def rounding():
    return importlib.import_module("invoicekit.rounding")


def test_round_money_lives_in_the_rounding_module():
    module = rounding()
    assert module.round_money.__module__ == "invoicekit.rounding"
    assert module.CENT == Decimal("0.01")


def test_old_import_paths_still_work():
    module = rounding()
    assert money.round_money is module.round_money
    assert money.CENT is module.CENT
    assert invoicekit.round_money is module.round_money


def test_behaviour_is_unchanged():
    round_money = rounding().round_money
    assert round_money(Decimal("2.345")) == Decimal("2.35")
    assert round_money(Decimal("-2.345")) == Decimal("-2.35")
    assert round_money(Decimal("7")) == Decimal("7.00")


def test_money_module_no_longer_defines_rounding():
    source = importlib.util.find_spec("invoicekit.money").origin
    with open(source, encoding="utf-8") as handle:
        text = handle.read()
    assert "def round_money" not in text
    assert "ROUND_HALF_UP" not in text
