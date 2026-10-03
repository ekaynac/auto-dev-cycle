# SPDX-License-Identifier: Apache-2.0
"""Invoices and their lines. Values are immutable; adding a line returns a new invoice."""

from dataclasses import dataclass, replace
from decimal import Decimal

from invoicekit.discounts import bulk_discount_rate
from invoicekit.money import format_amount, round_money

ZERO = Decimal("0")


@dataclass(frozen=True)
class LineItem:
    sku: str
    description: str
    quantity: int
    unit_price: Decimal

    def __post_init__(self) -> None:
        if not self.sku:
            raise ValueError("sku is required")
        if self.quantity <= 0:
            raise ValueError("quantity must be positive")
        if self.unit_price < 0:
            raise ValueError("unit price must not be negative")

    @property
    def gross(self) -> Decimal:
        return round_money(self.unit_price * self.quantity)

    @property
    def discount(self) -> Decimal:
        return round_money(self.gross * bulk_discount_rate(self.quantity))

    @property
    def net(self) -> Decimal:
        return self.gross - self.discount


@dataclass(frozen=True)
class Invoice:
    number: str
    lines: tuple[LineItem, ...] = ()
    tax_rate: Decimal = ZERO

    def __post_init__(self) -> None:
        if not self.number:
            raise ValueError("invoice number is required")
        if self.tax_rate < 0:
            raise ValueError("tax rate must not be negative")

    def add_line(self, line: LineItem) -> "Invoice":
        return replace(self, lines=self.lines + (line,))

    @property
    def subtotal(self) -> Decimal:
        return sum((line.net for line in self.lines), ZERO)

    @property
    def tax(self) -> Decimal:
        return round_money(self.subtotal * self.tax_rate)

    @property
    def total(self) -> Decimal:
        return self.subtotal + self.tax

    def render(self, currency: str) -> str:
        rows = [f"Invoice {self.number}"]
        for line in self.lines:
            rows.append(
                f"{line.sku:<8} {line.quantity:>4} x {format_amount(line.unit_price, currency)}"
                f"  {format_amount(line.net, currency)}"
            )
        rows.append(f"Subtotal {format_amount(self.subtotal, currency)}")
        rows.append(f"Tax      {format_amount(self.tax, currency)}")
        rows.append(f"Total    {format_amount(self.total, currency)}")
        return "\n".join(rows)
