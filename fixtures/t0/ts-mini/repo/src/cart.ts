// SPDX-License-Identifier: Apache-2.0

import { assertCents, roundCents } from "./money.ts";

export interface LineItem {
  sku: string;
  unitCents: number;
  quantity: number;
}

export type Discount =
  | { kind: "percent"; percent: number }
  | { kind: "fixed"; cents: number };

export interface Totals {
  subtotal: number;
  discount: number;
  tax: number;
  total: number;
}

export function lineTotal(item: LineItem): number {
  assertCents(item.unitCents, `unitCents of ${item.sku}`);
  if (!Number.isInteger(item.quantity) || item.quantity < 0) {
    throw new RangeError(`quantity of ${item.sku} must be a non-negative integer`);
  }
  return item.unitCents * item.quantity;
}

export function subtotal(items: readonly LineItem[]): number {
  return items.reduce((sum, item) => sum + lineTotal(item), 0);
}

/** Returns the discount amount in cents for the given subtotal. */
export function discountCents(amount: number, discount: Discount | undefined): number {
  if (discount === undefined) {
    return 0;
  }
  if (discount.kind === "percent") {
    if (discount.percent < 0 || discount.percent > 100) {
      throw new RangeError(`percent discount must be within 0..100, got ${discount.percent}`);
    }
    return roundCents((amount * discount.percent) / 100);
  }
  assertCents(discount.cents, "fixed discount");
  if (discount.cents < 0) {
    throw new RangeError("fixed discount must not be negative");
  }
  return discount.cents;
}

/** Tax on an amount at a rate given in basis points (825 = 8.25%). */
export function taxCents(amount: number, rateBasisPoints: number): number {
  if (!Number.isInteger(rateBasisPoints) || rateBasisPoints < 0) {
    throw new RangeError(`tax rate must be a non-negative integer of basis points, got ${rateBasisPoints}`);
  }
  return roundCents((amount * rateBasisPoints) / 10_000);
}

export function totals(
  items: readonly LineItem[],
  options: { discount?: Discount; taxBasisPoints?: number } = {},
): Totals {
  const sub = subtotal(items);
  const discount = discountCents(sub, options.discount);
  const taxable = sub - discount;
  const tax = taxCents(taxable, options.taxBasisPoints ?? 0);
  return { subtotal: sub, discount, tax, total: taxable + tax };
}
