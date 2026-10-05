// SPDX-License-Identifier: Apache-2.0
// The adapter wire corpus, read as the Go host reads it
// (internal/adapterhost/wire runs the same file): framing, strict JSON and
// the envelope give the same verdict per line on both sides.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { FramingError, StrictJSONError, classify, lines, parseStrict } from "../src/index.ts";

const root = new URL("../../../", import.meta.url).pathname;

interface WireCase {
  name: string;
  text?: string;
  hex?: string;
  nest?: number;
  pad_to?: number;
  want: string[];
}

function bytes(c: WireCase): Uint8Array {
  if (c.hex !== undefined) return Uint8Array.from(Buffer.from(c.hex, "hex"));
  if (c.nest) return new TextEncoder().encode("[".repeat(c.nest) + "]".repeat(c.nest) + "\n");
  assert.ok(c.text !== undefined, `${c.name}: no bytes`);
  if (c.pad_to) {
    const nl = c.text.indexOf("\n");
    const first = nl < 0 ? c.text : c.text.slice(0, nl);
    const rest = nl < 0 ? "" : c.text.slice(nl + 1);
    const size = Buffer.byteLength(first, "utf8");
    assert.ok(first.startsWith("{") && size <= c.pad_to, `${c.name}: cannot pad`);
    return Buffer.from("{" + " ".repeat(c.pad_to - size) + first.slice(1) + "\n" + rest, "utf8");
  }
  return new TextEncoder().encode(c.text);
}

async function* chunked(b: Uint8Array, size: number) {
  for (let i = 0; i < b.length; i += size) yield b.subarray(i, i + size);
}

async function verdicts(input: AsyncIterable<Uint8Array>): Promise<string[]> {
  const got: string[] = [];
  try {
    for await (const line of lines(input)) {
      let m: unknown;
      try {
        m = parseStrict(line);
      } catch (e) {
        if (!(e instanceof StrictJSONError)) throw e;
        got.push("json");
        continue;
      }
      const c = classify(m);
      got.push(c.kind === "invalid" ? "envelope" : c.kind);
    }
  } catch (e) {
    if (!(e instanceof FramingError)) throw e;
    got.push("framing");
  }
  return got;
}

test("every wire case gets the verdicts the Go host gives it", async () => {
  const { cases } = JSON.parse(readFileSync(join(root, "fixtures/contract/wire-cases.json"), "utf8")) as { cases: WireCase[] };
  assert.ok(cases.length > 0);
  for (const c of cases) {
    const b = bytes(c);
    // Whole, and one byte per chunk where that is cheap, so line boundaries fall anywhere.
    for (const size of b.length > 1 << 20 ? [b.length || 1, 65536] : [b.length || 1, 1]) {
      assert.deepEqual(await verdicts(chunked(b, size)), c.want, `${c.name} (chunks of ${size})`);
    }
  }
});
