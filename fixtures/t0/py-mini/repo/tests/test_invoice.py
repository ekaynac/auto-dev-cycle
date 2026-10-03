# SPDX-License-Identifier: Apache-2.0
from decimal import Decimal

import pytest

from invoicekit import Invoice, LineItem


def widget(quantity, price="2.50"):
    return LineItem(sku="W-1", description="Widget", quantity=quantity, unit_price=Decimal(price))


def test_line_without_bulk_discount():
    line = widget(4)
    assert line.gross == Decimal("10.00")
    assert line.discount == Decimal("0.00")
    assert line.net == Decimal("10.00")


def test_line_with_bulk_discount():
    line = widget(60)
    assert line.gross == Decimal("150.00")
    assert line.discount == Decimal("7.50")
    assert line.net == Decimal("142.50")


def test_add_line_returns_a_new_invoice():
    empty = Invoice(number="INV-1")
    one = empty.add_line(widget(4))
    assert empty.lines == ()
    assert len(one.lines) == 1


def test_totals_with_tax():
    invoice = (
        Invoice(number="INV-2", tax_rate=Decimal("0.20"))
        .add_line(widget(4))
        .add_line(LineItem("G-7", "Gadget", 3, Decimal("1.99")))
    )
    assert invoice.subtotal == Decimal("15.97")
    assert invoice.tax == Decimal("3.19")
    assert invoice.total == Decimal("19.16")


def test_render_lists_lines_and_totals():
    invoice = Invoice(number="INV-3").add_line(widget(4))
    assert invoice.render("EUR").splitlines() == [
        "Invoice INV-3",
        "W-1         4 x EUR 2.50  EUR 10.00",
        "Subtotal EUR 10.00",
        "Tax      EUR 0.00",
        "Total    EUR 10.00",
    ]


@pytest.mark.parametrize(
    "kwargs",
    [
        {"sku": "", "description": "x", "quantity": 1, "unit_price": Decimal("1")},
        {"sku": "A", "description": "x", "quantity": 0, "unit_price": Decimal("1")},
        {"sku": "A", "description": "x", "quantity": 1, "unit_price": Decimal("-1")},
    ],
)
def test_line_item_validation(kwargs):
    with pytest.raises(ValueError):
        LineItem(**kwargs)
