<?php
// SPDX-License-Identifier: Apache-2.0

declare(strict_types=1);

namespace PhpMini\Shop\Tests;

use InvalidArgumentException;
use PhpMini\Shop\Money;
use PHPUnit\Framework\TestCase;

final class MoneyTest extends TestCase
{
    public function testAddsAmountsInTheSameCurrency(): void
    {
        $sum = Money::ofCents(250, 'EUR')->add(Money::ofCents(199, 'EUR'));

        self::assertSame(449, $sum->cents());
        self::assertSame('EUR', $sum->currency());
    }

    public function testSubtractsAmounts(): void
    {
        $diff = Money::ofCents(1000, 'USD')->subtract(Money::ofCents(1, 'USD'));

        self::assertSame(999, $diff->cents());
    }

    public function testMultipliesByAnInteger(): void
    {
        self::assertSame(1497, Money::ofCents(499, 'EUR')->multiply(3)->cents());
    }

    public function testIsImmutable(): void
    {
        $a = Money::ofCents(100, 'EUR');
        $a->add(Money::ofCents(50, 'EUR'));

        self::assertSame(100, $a->cents());
    }

    public function testRejectsMixedCurrencies(): void
    {
        $this->expectException(InvalidArgumentException::class);

        Money::ofCents(100, 'EUR')->add(Money::ofCents(100, 'USD'));
    }

    public function testRejectsAnInvalidCurrencyCode(): void
    {
        $this->expectException(InvalidArgumentException::class);

        Money::ofCents(100, 'euro');
    }

    public function testEquality(): void
    {
        self::assertTrue(Money::ofCents(5, 'EUR')->equals(Money::ofCents(5, 'EUR')));
        self::assertFalse(Money::ofCents(5, 'EUR')->equals(Money::ofCents(5, 'USD')));
        self::assertFalse(Money::ofCents(5, 'EUR')->equals(Money::ofCents(6, 'EUR')));
    }

    public function testFormatsPositiveAmounts(): void
    {
        self::assertSame('EUR 12.05', Money::ofCents(1205, 'EUR')->format());
        self::assertSame('USD 0.99', Money::ofCents(99, 'USD')->format());
        self::assertSame('GBP 0.00', Money::zero('GBP')->format());
    }
}
