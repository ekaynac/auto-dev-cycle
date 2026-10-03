# SPDX-License-Identifier: Apache-2.0
# Hidden acceptance tests for py-mini-failing-then-green-1.
from decimal import Decimal

import pytest

from invoicekit import Invoice, LineItem
from invoicekit.money import format_amount, parse_amount


@pytest.mark.parametrize(
    ("amount", "expected"),
    [
        ("999.99", "EUR 999.99"),
        ("1000", "EUR 1,000.00"),
        ("-1000", "-EUR 1,000.00"),
        ("-9876543.215", "-EUR 9,876,543.22"),
        ("1000000000", "EUR 1,000,000,000.00"),
    ],
)
def test_grouping(amount, expected):
    assert format_amount(Decimal(amount), "EUR") == expected


def test_formatted_amounts_parse_back():
    text = format_amount(Decimal("1234567.5"), "EUR").removeprefix("EUR ")
    assert parse_amount(text) == Decimal("1234567.50")


def test_rendered_invoice_groups_totals():
    invoice = Invoice(number="INV-9").add_line(
        LineItem(sku="S-9", description="Server", quantity=2, unit_price=Decimal("1500"))
    )
    assert invoice.render("USD").splitlines()[-1] == "Total    USD 3,000.00"
