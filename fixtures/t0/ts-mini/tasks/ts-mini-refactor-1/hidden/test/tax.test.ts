// SPDX-License-Identifier: Apache-2.0

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import * as tax from "../src/tax.ts";
import * as cart from "../src/cart.ts";
import * as index from "../src/index.ts";

describe("tax module", () => {
  it("owns taxCents, and the old exports are the same function", () => {
    assert.equal(typeof tax.taxCents, "function");
    assert.equal(cart.taxCents, tax.taxCents);
    assert.equal(index.taxCents, tax.taxCents);
  });

  it("keeps the tax behaviour", () => {
    assert.equal(tax.taxCents(1000, 825), 83);
    assert.equal(tax.taxCents(200, 825), 17);
    assert.equal(tax.taxCents(0, 825), 0);
    assert.throws(() => tax.taxCents(100, -1), RangeError);
    assert.throws(() => tax.taxCents(100, 8.25), RangeError);
  });
});
