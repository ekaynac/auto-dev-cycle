// SPDX-License-Identifier: Apache-2.0

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { formatCents } from "../src/money.ts";

describe("formatCents negative amounts (acceptance)", () => {
  it("keeps the sign for amounts under one unit", () => {
    assert.equal(formatCents(-5), "-$0.05");
    assert.equal(formatCents(-99), "-$0.99");
  });

  it("works for every currency", () => {
    assert.equal(formatCents(-100, "EUR"), "-€1.00");
    assert.equal(formatCents(-250050, "GBP"), "-£2500.50");
  });

  it("leaves zero and positive amounts unsigned", () => {
    assert.equal(formatCents(0), "$0.00");
    assert.equal(formatCents(-0), "$0.00");
    assert.equal(formatCents(1234), "$12.34");
  });
});
