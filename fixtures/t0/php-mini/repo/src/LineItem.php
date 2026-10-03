<?php
// SPDX-License-Identifier: Apache-2.0

declare(strict_types=1);

namespace PhpMini\Shop;

use InvalidArgumentException;

/**
 * One product line in a cart: a SKU, a unit price and a quantity.
 */
final class LineItem
{
    public function __construct(
        public readonly string $sku,
        public readonly Money $unitPrice,
        public readonly int $quantity,
    ) {
        if ($sku === '') {
            throw new InvalidArgumentException('SKU must not be empty');
        }
        if ($quantity < 1) {
            throw new InvalidArgumentException("Quantity must be at least 1, got {$quantity}");
        }
    }

    public function withQuantity(int $quantity): self
    {
        return new self($this->sku, $this->unitPrice, $quantity);
    }

    public function total(): Money
    {
        return $this->unitPrice->multiply($this->quantity);
    }
}
