# SPDX-License-Identifier: Apache-2.0
# Hidden acceptance tests for py-mini-bugfix-1: the tier thresholds are inclusive.
from decimal import Decimal

import pytest

from invoicekit import LineItem
from invoicekit.discounts import bulk_discount_rate


@pytest.mark.parametrize(
    ("quantity", "rate"),
    [(49, "0"), (50, "0.05"), (51, "0.05"), (99, "0.05"), (100, "0.10"), (101, "0.10")],
)
def test_thresholds_are_inclusive(quantity, rate):
    assert bulk_discount_rate(quantity) == Decimal(rate)


def test_line_of_exactly_fifty_units_is_discounted():
    line = LineItem(sku="W-1", description="Widget", quantity=50, unit_price=Decimal("2.00"))
    assert line.gross == Decimal("100.00")
    assert line.discount == Decimal("5.00")
    assert line.net == Decimal("95.00")
