# SPDX-License-Identifier: Apache-2.0
"""Bulk discount policy.

A line of at least 50 units gets 5% off; a line of at least 100 units gets 10%.
"""

from decimal import Decimal

# Highest threshold first: the first tier the quantity reaches wins.
BULK_TIERS: tuple[tuple[int, Decimal], ...] = (
    (100, Decimal("0.10")),
    (50, Decimal("0.05")),
)
NO_DISCOUNT = Decimal("0")


def bulk_discount_rate(quantity: int) -> Decimal:
    """Return the discount rate for a line of `quantity` units."""
    if quantity < 0:
        raise ValueError("quantity must not be negative")
    for threshold, rate in BULK_TIERS:
        if quantity > threshold:
            return rate
    return NO_DISCOUNT
