// SPDX-License-Identifier: Apache-2.0

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { discountCents, totals } from "../src/cart.ts";

describe("fixed discounts larger than the subtotal", () => {
  it("cap the discount at the subtotal", () => {
    assert.equal(discountCents(300, { kind: "fixed", cents: 500 }), 300);
    assert.equal(discountCents(0, { kind: "fixed", cents: 100 }), 0);
  });

  it("never produce a negative total or negative tax", () => {
    const items = [{ sku: "sticker", unitCents: 300, quantity: 1 }];
    assert.deepEqual(totals(items, { discount: { kind: "fixed", cents: 500 }, taxBasisPoints: 825 }), {
      subtotal: 300,
      discount: 300,
      tax: 0,
      total: 0,
    });
  });

  it("leave discounts within the subtotal unchanged", () => {
    assert.equal(discountCents(300, { kind: "fixed", cents: 300 }), 300);
    assert.equal(discountCents(300, { kind: "fixed", cents: 299 }), 299);
  });
});
