// SPDX-License-Identifier: Apache-2.0

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { formatCents, roundCents } from "../src/money.ts";

describe("formatCents", () => {
  it("formats whole and fractional amounts", () => {
    assert.equal(formatCents(1234), "$12.34");
    assert.equal(formatCents(5), "$0.05");
    assert.equal(formatCents(0), "$0.00");
  });

  it("uses the currency symbol", () => {
    assert.equal(formatCents(250, "EUR"), "€2.50");
    assert.equal(formatCents(99, "GBP"), "£0.99");
  });

  it("rejects unknown currencies and fractional cents", () => {
    assert.throws(() => formatCents(100, "XYZ"), RangeError);
    assert.throws(() => formatCents(1.5), TypeError);
  });
});

describe("roundCents", () => {
  it("rounds half away from zero", () => {
    assert.equal(roundCents(2.5), 3);
    assert.equal(roundCents(-2.5), -3);
    assert.equal(roundCents(2.4), 2);
  });
});
