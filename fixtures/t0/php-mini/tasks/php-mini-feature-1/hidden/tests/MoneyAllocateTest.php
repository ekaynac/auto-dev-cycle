<?php
// SPDX-License-Identifier: Apache-2.0

declare(strict_types=1);

namespace PhpMini\Shop\Tests;

use InvalidArgumentException;
use PhpMini\Shop\Money;
use PHPUnit\Framework\TestCase;

final class MoneyAllocateTest extends TestCase
{
    /**
     * @param list<Money> $parts
     * @return list<int>
     */
    private static function cents(array $parts): array
    {
        return array_map(static fn (Money $m): int => $m->cents(), $parts);
    }

    public function testSplitsEvenlyAndGivesLeftoverCentsToTheFirstParts(): void
    {
        $parts = Money::ofCents(1000, 'EUR')->allocate([1, 1, 1]);

        self::assertSame([334, 333, 333], self::cents($parts));
    }

    public function testSplitsByRatio(): void
    {
        self::assertSame([4, 1], self::cents(Money::ofCents(5, 'EUR')->allocate([70, 30])));
        self::assertSame([750, 250], self::cents(Money::ofCents(1000, 'EUR')->allocate([3, 1])));
    }

    public function testNeverLosesOrCreatesACent(): void
    {
        $parts = Money::ofCents(10001, 'USD')->allocate([2, 3, 5, 7]);

        self::assertSame(10001, array_sum(self::cents($parts)));
        foreach ($parts as $part) {
            self::assertSame('USD', $part->currency());
        }
    }

    public function testAZeroRatioGetsNothing(): void
    {
        self::assertSame([0, 100], self::cents(Money::ofCents(100, 'EUR')->allocate([0, 1])));
    }

    public function testRejectsAnEmptyRatioList(): void
    {
        $this->expectException(InvalidArgumentException::class);

        Money::ofCents(100, 'EUR')->allocate([]);
    }

    public function testRejectsANegativeRatio(): void
    {
        $this->expectException(InvalidArgumentException::class);

        Money::ofCents(100, 'EUR')->allocate([2, -1]);
    }

    public function testRejectsRatiosThatSumToZero(): void
    {
        $this->expectException(InvalidArgumentException::class);

        Money::ofCents(100, 'EUR')->allocate([0, 0]);
    }
}
