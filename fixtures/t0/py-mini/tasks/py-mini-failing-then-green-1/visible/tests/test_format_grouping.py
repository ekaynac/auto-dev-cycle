# SPDX-License-Identifier: Apache-2.0
# Visible test that fails at base for py-mini-failing-then-green-1.
from decimal import Decimal

from invoicekit.money import format_amount


def test_large_amounts_use_thousands_separators():
    assert format_amount(Decimal("1234567.5"), "EUR") == "EUR 1,234,567.50"
