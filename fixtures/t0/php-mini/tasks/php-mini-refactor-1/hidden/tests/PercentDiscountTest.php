<?php
// SPDX-License-Identifier: Apache-2.0

declare(strict_types=1);

namespace PhpMini\Shop\Tests;

use InvalidArgumentException;
use PhpMini\Shop\Cart;
use PhpMini\Shop\LineItem;
use PhpMini\Shop\Money;
use PhpMini\Shop\PercentDiscount;
use PHPUnit\Framework\TestCase;

final class PercentDiscountTest extends TestCase
{
    public function testAppliesToAnAmountRoundingTheDiscountHalfUp(): void
    {
        $discount = new PercentDiscount(10);

        self::assertSame(1799, $discount->applyTo(Money::ofCents(1999, 'EUR'))->cents());
        self::assertSame(90, $discount->applyTo(Money::ofCents(100, 'EUR'))->cents());
        self::assertSame('EUR', $discount->applyTo(Money::ofCents(100, 'EUR'))->currency());
    }

    public function testExposesItsPercentage(): void
    {
        self::assertSame(25, (new PercentDiscount(25))->percent());
    }

    public function testTheBoundsAreAllowed(): void
    {
        self::assertSame(500, (new PercentDiscount(0))->applyTo(Money::ofCents(500, 'EUR'))->cents());
        self::assertSame(0, (new PercentDiscount(100))->applyTo(Money::ofCents(500, 'EUR'))->cents());
    }

    public function testRejectsANegativePercentage(): void
    {
        $this->expectException(InvalidArgumentException::class);

        new PercentDiscount(-1);
    }

    public function testRejectsAPercentageAboveOneHundred(): void
    {
        $this->expectException(InvalidArgumentException::class);

        new PercentDiscount(101);
    }

    public function testTheCartUsesTheSameRule(): void
    {
        $cart = Cart::empty('EUR')
            ->add(new LineItem('book', Money::ofCents(1999, 'EUR'), 2))
            ->add(new LineItem('pen', Money::ofCents(155, 'EUR'), 3));

        foreach ([0, 7, 10, 33, 50, 100] as $percent) {
            self::assertTrue(
                (new PercentDiscount($percent))->applyTo($cart->subtotal())
                    ->equals($cart->totalWithPercentDiscount($percent)),
                "Cart and PercentDiscount disagree at {$percent}%",
            );
        }
    }
}
