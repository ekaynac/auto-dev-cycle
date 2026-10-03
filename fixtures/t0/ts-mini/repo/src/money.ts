// SPDX-License-Identifier: Apache-2.0

/** Amounts are integer cents throughout; this module formats and rounds them. */

export const CURRENCY_SYMBOLS: Readonly<Record<string, string>> = {
  USD: "$",
  EUR: "€",
  GBP: "£",
};

export function assertCents(value: number, label = "amount"): void {
  if (!Number.isSafeInteger(value)) {
    throw new TypeError(`${label} must be an integer number of cents, got ${value}`);
  }
}

/** Rounds a fractional cent value half away from zero. */
export function roundCents(value: number): number {
  return Math.sign(value) * Math.round(Math.abs(value));
}

/** Formats cents as a currency string, e.g. 1234 -> "$12.34". */
export function formatCents(cents: number, currency = "USD"): string {
  assertCents(cents);
  const symbol = CURRENCY_SYMBOLS[currency];
  if (symbol === undefined) {
    throw new RangeError(`unknown currency: ${currency}`);
  }
  const whole = Math.trunc(cents / 100);
  const fraction = Math.abs(cents % 100).toString().padStart(2, "0");
  return `${symbol}${whole}.${fraction}`;
}
