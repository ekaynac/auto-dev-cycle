# SPDX-License-Identifier: Apache-2.0
from decimal import Decimal

import pytest

from invoicekit.discounts import bulk_discount_rate


@pytest.mark.parametrize(
    ("quantity", "rate"),
    [(0, "0"), (10, "0"), (60, "0.05"), (150, "0.10")],
)
def test_bulk_discount_rate_by_tier(quantity, rate):
    assert bulk_discount_rate(quantity) == Decimal(rate)


def test_bulk_discount_rate_rejects_negative_quantity():
    with pytest.raises(ValueError):
        bulk_discount_rate(-1)
