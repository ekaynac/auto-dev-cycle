// SPDX-License-Identifier: Apache-2.0
// The fake adapter's process: the scenario comes from ADC_FAKE_SCENARIO
// (default patch), the stream-gap pause from ADC_FAKE_GAP_MS. An unknown
// scenario exits 2 before reading any input.

import { run } from "@adc/contract";
import { FakeAdapter, type Scenario } from "./fake.ts";

let adapter: FakeAdapter;
try {
  const gap = process.env.ADC_FAKE_GAP_MS;
  adapter = new FakeAdapter({ scenario: (process.env.ADC_FAKE_SCENARIO ?? "patch") as Scenario, gapMs: gap === undefined ? undefined : Number(gap) });
} catch (e) {
  process.stderr.write(`adc fake adapter: ${(e as Error).message}\n`);
  process.exit(2);
}
await run(adapter);
