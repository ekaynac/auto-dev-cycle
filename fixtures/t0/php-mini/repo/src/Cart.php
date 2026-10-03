<?php
// SPDX-License-Identifier: Apache-2.0

declare(strict_types=1);

namespace PhpMini\Shop;

use InvalidArgumentException;

/**
 * An immutable shopping cart in a single currency.
 */
final class Cart
{
    /**
     * @param array<string, LineItem> $lines keyed by SKU
     */
    private function __construct(
        private readonly string $currency,
        private readonly array $lines,
    ) {
    }

    public static function empty(string $currency): self
    {
        Money::zero($currency);

        return new self($currency, []);
    }

    public function currency(): string
    {
        return $this->currency;
    }

    /**
     * Adds a line; adding a SKU already in the cart increases its quantity.
     */
    public function add(LineItem $item): self
    {
        if ($item->unitPrice->currency() !== $this->currency) {
            throw new InvalidArgumentException(
                "Cart is in {$this->currency}, item {$item->sku} is in {$item->unitPrice->currency()}",
            );
        }

        $lines = $this->lines;
        $existing = $lines[$item->sku] ?? null;
        $lines[$item->sku] = $existing === null
            ? $item
            : $existing->withQuantity($existing->quantity + $item->quantity);

        return new self($this->currency, $lines);
    }

    /**
     * @return list<LineItem>
     */
    public function lines(): array
    {
        return array_values($this->lines);
    }

    /**
     * The number of units in the cart, summed over all lines.
     */
    public function itemCount(): int
    {
        return count($this->lines);
    }

    public function subtotal(): Money
    {
        $sum = Money::zero($this->currency);
        foreach ($this->lines as $line) {
            $sum = $sum->add($line->total());
        }

        return $sum;
    }

    /**
     * The subtotal reduced by a whole-number percentage (0-100); the discount is rounded half up to the cent.
     */
    public function totalWithPercentDiscount(int $percent): Money
    {
        if ($percent < 0 || $percent > 100) {
            throw new InvalidArgumentException("Discount must be between 0 and 100, got {$percent}");
        }

        $subtotal = $this->subtotal();
        $discount = intdiv($subtotal->cents() * $percent + 50, 100);

        return $subtotal->subtract(Money::ofCents($discount, $this->currency));
    }
}
