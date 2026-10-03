<?php
// SPDX-License-Identifier: Apache-2.0

declare(strict_types=1);

namespace PhpMini\Shop;

use InvalidArgumentException;

/**
 * An immutable amount of money held in minor units (cents) of one currency.
 */
final class Money
{
    private function __construct(
        private readonly int $cents,
        private readonly string $currency,
    ) {
    }

    public static function ofCents(int $cents, string $currency): self
    {
        if (preg_match('/^[A-Z]{3}$/', $currency) !== 1) {
            throw new InvalidArgumentException("Invalid currency code: {$currency}");
        }

        return new self($cents, $currency);
    }

    public static function zero(string $currency): self
    {
        return self::ofCents(0, $currency);
    }

    public function cents(): int
    {
        return $this->cents;
    }

    public function currency(): string
    {
        return $this->currency;
    }

    public function add(self $other): self
    {
        $this->assertSameCurrency($other);

        return new self($this->cents + $other->cents, $this->currency);
    }

    public function subtract(self $other): self
    {
        $this->assertSameCurrency($other);

        return new self($this->cents - $other->cents, $this->currency);
    }

    public function multiply(int $factor): self
    {
        return new self($this->cents * $factor, $this->currency);
    }

    public function equals(self $other): bool
    {
        return $this->currency === $other->currency && $this->cents === $other->cents;
    }

    /**
     * Formats as "<CUR> <units>.<cents>", for example "EUR 12.05" or "EUR -0.50".
     */
    public function format(): string
    {
        $units = intdiv($this->cents, 100);
        $rest = $this->cents % 100;

        return sprintf('%s %d.%02d', $this->currency, $units, $rest);
    }

    private function assertSameCurrency(self $other): void
    {
        if ($this->currency !== $other->currency) {
            throw new InvalidArgumentException(
                "Currency mismatch: {$this->currency} and {$other->currency}",
            );
        }
    }
}
