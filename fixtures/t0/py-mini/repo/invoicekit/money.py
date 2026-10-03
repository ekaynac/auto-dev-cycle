# SPDX-License-Identifier: Apache-2.0
"""Money amounts as Decimal values rounded to the cent."""

from decimal import ROUND_HALF_UP, Decimal, InvalidOperation

CENT = Decimal("0.01")


def round_money(amount: Decimal) -> Decimal:
    """Round to the cent, halves away from zero (commercial rounding)."""
    return Decimal(amount).quantize(CENT, rounding=ROUND_HALF_UP)


def parse_amount(text: str) -> Decimal:
    """Parse a user-entered amount such as " 1,250.5 " into Decimal("1250.50")."""
    cleaned = text.strip().replace(",", "")
    if not cleaned:
        raise ValueError("empty amount")
    try:
        value = Decimal(cleaned)
    except InvalidOperation as exc:
        raise ValueError(f"not an amount: {text!r}") from exc
    if not value.is_finite():
        raise ValueError(f"not a finite amount: {text!r}")
    return round_money(value)


def format_amount(amount: Decimal, currency: str) -> str:
    """Format an amount for display, e.g. "EUR 12.50" or "-EUR 3.00"."""
    if not currency:
        raise ValueError("currency is required")
    value = round_money(amount)
    sign = "-" if value < 0 else ""
    return f"{sign}{currency} {abs(value):.2f}"
