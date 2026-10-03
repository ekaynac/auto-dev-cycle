<?php
// SPDX-License-Identifier: Apache-2.0

declare(strict_types=1);

namespace PhpMini\Shop\Tests;

use PhpMini\Shop\Money;
use PHPUnit\Framework\TestCase;

final class MoneyNegativeFormatTest extends TestCase
{
    public function testFormatsANegativeAmountBelowOneUnit(): void
    {
        self::assertSame('EUR -0.05', Money::ofCents(-5, 'EUR')->format());
        self::assertSame('EUR -0.50', Money::ofCents(-50, 'EUR')->format());
    }

    public function testFormatsANegativeAmountAboveOneUnit(): void
    {
        self::assertSame('EUR -1.50', Money::ofCents(-150, 'EUR')->format());
        self::assertSame('USD -12.05', Money::ofCents(-1205, 'USD')->format());
    }

    public function testFormatsAWholeNegativeAmount(): void
    {
        self::assertSame('EUR -1.00', Money::ofCents(-100, 'EUR')->format());
    }

    public function testFormatsARefund(): void
    {
        $refund = Money::ofCents(300, 'EUR')->subtract(Money::ofCents(499, 'EUR'));

        self::assertSame('EUR -1.99', $refund->format());
    }
}
