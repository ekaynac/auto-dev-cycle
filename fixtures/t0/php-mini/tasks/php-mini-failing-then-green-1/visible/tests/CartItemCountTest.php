<?php
// SPDX-License-Identifier: Apache-2.0

declare(strict_types=1);

namespace PhpMini\Shop\Tests;

use PhpMini\Shop\Cart;
use PhpMini\Shop\LineItem;
use PhpMini\Shop\Money;
use PHPUnit\Framework\TestCase;

final class CartItemCountTest extends TestCase
{
    public function testAnEmptyCartHasNoItems(): void
    {
        self::assertSame(0, Cart::empty('EUR')->itemCount());
    }

    public function testCountsUnitsNotLines(): void
    {
        $cart = Cart::empty('EUR')
            ->add(new LineItem('apple', Money::ofCents(120, 'EUR'), 3))
            ->add(new LineItem('pear', Money::ofCents(95, 'EUR'), 2));

        self::assertSame(5, $cart->itemCount());
    }
}
