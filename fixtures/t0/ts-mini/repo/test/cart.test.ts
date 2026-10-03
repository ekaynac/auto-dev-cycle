// SPDX-License-Identifier: Apache-2.0

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { discountCents, subtotal, taxCents, totals, type LineItem } from "../src/cart.ts";

const items: LineItem[] = [
  { sku: "pen", unitCents: 150, quantity: 4 },
  { sku: "notebook", unitCents: 899, quantity: 1 },
];

describe("subtotal", () => {
  it("sums line totals", () => {
    assert.equal(subtotal(items), 1499);
    assert.equal(subtotal([]), 0);
  });

  it("rejects negative or fractional quantities", () => {
    assert.throws(() => subtotal([{ sku: "x", unitCents: 1, quantity: -1 }]), RangeError);
    assert.throws(() => subtotal([{ sku: "x", unitCents: 1, quantity: 1.5 }]), RangeError);
  });
});

describe("discountCents", () => {
  it("applies percent and fixed discounts", () => {
    assert.equal(discountCents(1499, { kind: "percent", percent: 10 }), 150);
    assert.equal(discountCents(1499, { kind: "fixed", cents: 200 }), 200);
    assert.equal(discountCents(1499, undefined), 0);
  });

  it("rejects out-of-range discounts", () => {
    assert.throws(() => discountCents(100, { kind: "percent", percent: 101 }), RangeError);
    assert.throws(() => discountCents(100, { kind: "fixed", cents: -1 }), RangeError);
  });
});

describe("taxCents", () => {
  it("applies basis points with rounding", () => {
    assert.equal(taxCents(1000, 825), 83);
    assert.equal(taxCents(0, 825), 0);
    assert.throws(() => taxCents(100, -1), RangeError);
  });
});

describe("totals", () => {
  it("combines subtotal, discount and tax", () => {
    assert.deepEqual(totals(items, { discount: { kind: "percent", percent: 10 }, taxBasisPoints: 1000 }), {
      subtotal: 1499,
      discount: 150,
      tax: 135,
      total: 1484,
    });
  });

  it("defaults to no discount and no tax", () => {
    assert.deepEqual(totals(items), { subtotal: 1499, discount: 0, tax: 0, total: 1499 });
  });
});
