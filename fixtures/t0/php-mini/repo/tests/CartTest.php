<?php
// SPDX-License-Identifier: Apache-2.0

declare(strict_types=1);

namespace PhpMini\Shop\Tests;

use InvalidArgumentException;
use PhpMini\Shop\Cart;
use PhpMini\Shop\LineItem;
use PhpMini\Shop\Money;
use PHPUnit\Framework\TestCase;

final class CartTest extends TestCase
{
    private static function item(string $sku, int $cents, int $quantity = 1): LineItem
    {
        return new LineItem($sku, Money::ofCents($cents, 'EUR'), $quantity);
    }

    public function testAnEmptyCartHasAZeroSubtotal(): void
    {
        self::assertSame(0, Cart::empty('EUR')->subtotal()->cents());
    }

    public function testSubtotalSumsLineTotals(): void
    {
        $cart = Cart::empty('EUR')
            ->add(self::item('apple', 120, 3))
            ->add(self::item('pear', 95));

        self::assertSame(455, $cart->subtotal()->cents());
    }

    public function testAddingTheSameSkuMergesTheLine(): void
    {
        $cart = Cart::empty('EUR')
            ->add(self::item('apple', 120, 2))
            ->add(self::item('apple', 120, 1));

        self::assertCount(1, $cart->lines());
        self::assertSame(3, $cart->lines()[0]->quantity);
    }

    public function testAddingReturnsANewCart(): void
    {
        $empty = Cart::empty('EUR');
        $empty->add(self::item('apple', 120));

        self::assertSame([], $empty->lines());
    }

    public function testRejectsAnItemInAnotherCurrency(): void
    {
        $this->expectException(InvalidArgumentException::class);

        Cart::empty('EUR')->add(new LineItem('apple', Money::ofCents(120, 'USD'), 1));
    }

    public function testRejectsANonPositiveQuantity(): void
    {
        $this->expectException(InvalidArgumentException::class);

        self::item('apple', 120, 0);
    }

    public function testAppliesAPercentDiscountRoundedHalfUp(): void
    {
        $cart = Cart::empty('EUR')->add(self::item('book', 1999));

        // 10% of 19.99 is 1.999, rounded to 2.00.
        self::assertSame(1799, $cart->totalWithPercentDiscount(10)->cents());
        self::assertSame(1999, $cart->totalWithPercentDiscount(0)->cents());
        self::assertSame(0, $cart->totalWithPercentDiscount(100)->cents());
    }

    public function testRejectsADiscountAboveOneHundredPercent(): void
    {
        $this->expectException(InvalidArgumentException::class);

        Cart::empty('EUR')->totalWithPercentDiscount(101);
    }
}
