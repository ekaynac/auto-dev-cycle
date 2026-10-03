// SPDX-License-Identifier: Apache-2.0

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { formatCents } from "../src/money.ts";

describe("formatCents with negative amounts", () => {
  it("puts the minus sign before the currency symbol", () => {
    assert.equal(formatCents(-1234), "-$12.34");
  });
});
