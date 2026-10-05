#!/usr/bin/env node
// SPDX-License-Identifier: Apache-2.0
// The fake adapter's process: the scenario comes from ADC_FAKE_SCENARIO
// (default patch), the stream-gap pause from ADC_FAKE_GAP_MS (decimal
// digits only). An unknown scenario or a malformed gap exits 2 before any
// input is read.

import { run } from "@adc/contract";
import { FakeAdapter, type Scenario } from "./fake.ts";

let adapter: FakeAdapter;
try {
  const gap = process.env.ADC_FAKE_GAP_MS;
  if (gap !== undefined && !/^[0-9]{1,10}$/.test(gap)) throw new Error(`ADC_FAKE_GAP_MS ${JSON.stringify(gap)} is not a number of milliseconds`);
  adapter = new FakeAdapter({ scenario: (process.env.ADC_FAKE_SCENARIO ?? "patch") as Scenario, gapMs: gap === undefined ? undefined : Number(gap) });
} catch (e) {
  process.stderr.write(`adc fake adapter: ${(e as Error).message}\n`);
  process.exit(2);
}
await run(adapter);
