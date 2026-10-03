# SPDX-License-Identifier: Apache-2.0
from decimal import Decimal

import pytest

from invoicekit.money import format_amount, parse_amount, round_money


def test_round_money_rounds_halves_away_from_zero():
    assert round_money(Decimal("2.345")) == Decimal("2.35")
    assert round_money(Decimal("-2.345")) == Decimal("-2.35")
    assert round_money(Decimal("2.344")) == Decimal("2.34")


def test_parse_amount_accepts_separators_and_whitespace():
    assert parse_amount(" 1,250.5 ") == Decimal("1250.50")
    assert parse_amount("0") == Decimal("0.00")
    assert parse_amount("-3.999") == Decimal("-4.00")


@pytest.mark.parametrize("text", ["", "   ", "abc", "NaN", "Infinity"])
def test_parse_amount_rejects_non_amounts(text):
    with pytest.raises(ValueError):
        parse_amount(text)


def test_format_amount_small_values():
    assert format_amount(Decimal("12.5"), "EUR") == "EUR 12.50"
    assert format_amount(Decimal("-3"), "USD") == "-USD 3.00"
    assert format_amount(Decimal("0.004"), "EUR") == "EUR 0.00"


def test_format_amount_requires_currency():
    with pytest.raises(ValueError):
        format_amount(Decimal("1"), "")
