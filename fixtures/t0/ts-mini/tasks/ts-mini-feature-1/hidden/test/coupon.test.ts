// SPDX-License-Identifier: Apache-2.0

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { parseCoupon, totals } from "../src/index.ts";

describe("parseCoupon", () => {
  it("parses percent coupons", () => {
    assert.deepEqual(parseCoupon("SAVE10"), { kind: "percent", percent: 10 });
    assert.deepEqual(parseCoupon("SAVE100"), { kind: "percent", percent: 100 });
  });

  it("parses fixed coupons in whole currency units", () => {
    assert.deepEqual(parseCoupon("TAKE5"), { kind: "fixed", cents: 500 });
    assert.deepEqual(parseCoupon("TAKE1250"), { kind: "fixed", cents: 125_000 });
  });

  it("ignores case and surrounding whitespace", () => {
    assert.deepEqual(parseCoupon("  save15 "), { kind: "percent", percent: 15 });
    assert.deepEqual(parseCoupon("Take2"), { kind: "fixed", cents: 200 });
  });

  it("rejects unknown, empty and out-of-range codes", () => {
    for (const code of ["", "SAVE", "SAVE0", "SAVE101", "TAKE0", "TAKE-5", "FREE10", "SAVE 10", "SAVE10X"]) {
      assert.throws(() => parseCoupon(code), RangeError, `expected RangeError for ${JSON.stringify(code)}`);
    }
  });

  it("feeds totals", () => {
    const items = [{ sku: "mug", unitCents: 1200, quantity: 2 }];
    assert.equal(totals(items, { discount: parseCoupon("SAVE25") }).total, 1800);
    assert.equal(totals(items, { discount: parseCoupon("TAKE4") }).total, 2000);
  });
});
