# SPDX-License-Identifier: Apache-2.0
# Hidden acceptance tests for py-mini-feature-1. Names are resolved inside each
# test so that a missing function is a test failure, not a collection error.
from decimal import Decimal

import pytest

import invoicekit
from invoicekit import money


def test_even_split():
    assert money.split_evenly(Decimal("9.00"), 3) == [Decimal("3.00")] * 3


def test_remainder_cents_go_to_the_first_shares():
    assert money.split_evenly(Decimal("10.00"), 3) == [
        Decimal("3.34"),
        Decimal("3.33"),
        Decimal("3.33"),
    ]
    assert money.split_evenly(Decimal("0.05"), 3) == [
        Decimal("0.02"),
        Decimal("0.02"),
        Decimal("0.01"),
    ]


@pytest.mark.parametrize(
    ("amount", "parts"),
    [("100.00", 7), ("0.01", 4), ("1234.567", 6), ("-10.00", 3), ("0", 2)],
)
def test_shares_sum_to_the_rounded_amount_and_differ_by_at_most_a_cent(amount, parts):
    shares = money.split_evenly(Decimal(amount), parts)
    assert len(shares) == parts
    assert sum(shares, Decimal("0")) == money.round_money(Decimal(amount))
    assert all(share == money.round_money(share) for share in shares)
    assert max(shares) - min(shares) <= Decimal("0.01")


def test_negative_amounts_put_the_larger_magnitudes_first():
    assert money.split_evenly(Decimal("-10.00"), 3) == [
        Decimal("-3.34"),
        Decimal("-3.33"),
        Decimal("-3.33"),
    ]


def test_single_part_is_the_rounded_amount():
    assert money.split_evenly(Decimal("5.555"), 1) == [Decimal("5.56")]


@pytest.mark.parametrize("parts", [0, -2])
def test_parts_must_be_positive(parts):
    with pytest.raises(ValueError):
        money.split_evenly(Decimal("1.00"), parts)


def test_exported_from_the_package():
    assert invoicekit.split_evenly is money.split_evenly
    assert "split_evenly" in invoicekit.__all__
