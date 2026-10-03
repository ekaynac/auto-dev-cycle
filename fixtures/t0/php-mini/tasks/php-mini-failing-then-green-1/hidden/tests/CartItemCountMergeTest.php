<?php
// SPDX-License-Identifier: Apache-2.0

declare(strict_types=1);

namespace PhpMini\Shop\Tests;

use PhpMini\Shop\Cart;
use PhpMini\Shop\LineItem;
use PhpMini\Shop\Money;
use PHPUnit\Framework\TestCase;

final class CartItemCountMergeTest extends TestCase
{
    public function testCountsUnitsOfAMergedLine(): void
    {
        $cart = Cart::empty('EUR')
            ->add(new LineItem('apple', Money::ofCents(120, 'EUR'), 2))
            ->add(new LineItem('apple', Money::ofCents(120, 'EUR'), 3));

        self::assertSame(5, $cart->itemCount());
    }

    public function testASingleUnitCountsAsOne(): void
    {
        $cart = Cart::empty('EUR')->add(new LineItem('pear', Money::ofCents(95, 'EUR'), 1));

        self::assertSame(1, $cart->itemCount());
    }

    public function testCountingDoesNotChangeTheCart(): void
    {
        $cart = Cart::empty('EUR')->add(new LineItem('apple', Money::ofCents(120, 'EUR'), 4));
        $cart->itemCount();

        self::assertSame(4, $cart->itemCount());
        self::assertSame(480, $cart->subtotal()->cents());
    }
}
