// SPDX-License-Identifier: Apache-2.0
// The adapter wire corpus, read as the Go host reads it
// (internal/adapterhost/wire runs the same file): framing, strict JSON and
// the envelope give the same verdict per line on both sides, and the strict
// reader the same value for every line it accepts.
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
  nest?: { prefix?: string; open: string; times: number; inner?: string; close: string; suffix?: string };
  pad_to?: number;
  want: string[];
  decoded?: unknown[];
}

function bytes(c: WireCase): Uint8Array {
  if (c.hex !== undefined) return Uint8Array.from(Buffer.from(c.hex, "hex"));
  const n = c.nest;
  if (n) return Buffer.from((n.prefix ?? "") + n.open.repeat(n.times) + (n.inner ?? "") + n.close.repeat(n.times) + (n.suffix ?? "") + "\n", "utf8");
  assert.ok(c.text !== undefined, `${c.name}: no bytes`);
  if (c.pad_to) {
    const nl = c.text.indexOf("\n");
    const first = nl < 0 ? c.text : c.text.slice(0, nl);
    const rest = nl < 0 ? "" : c.text.slice(nl + 1);
    const size = Buffer.byteLength(first, "utf8");
    assert.ok(first.startsWith("{") && size <= c.pad_to, `${c.name}: cannot pad`);
    return Buffer.from("{" + " ".repeat(c.pad_to - size) + first.slice(1) + "\n" + rest, "utf8");
  }
  return Buffer.from(c.text, "utf8");
}

async function* chunked(b: Uint8Array, size: number) {
  for (let i = 0; i < b.length; i += size) yield b.subarray(i, i + size);
}

/** read gives each line's verdict, the strict value of each line the strict
 *  reader accepts (with the line), and checks what classify returns against
 *  that value. */
async function read(input: AsyncIterable<Uint8Array>) {
  const verdicts: string[] = [];
  const values: { value: unknown; line: string }[] = [];
  try {
    for await (const line of lines(input)) {
      let m: unknown;
      try {
        m = parseStrict(line);
      } catch (e) {
        if (!(e instanceof StrictJSONError)) throw e;
        verdicts.push("json");
        continue;
      }
      values.push({ value: m, line });
      const c = classify(m);
      if (c.kind === "event") assert.equal(c.params, (m as { params: unknown }).params);
      if (c.kind === "response") assert.equal(c.reply, m);
      verdicts.push(c.kind === "invalid" ? "envelope" : c.kind);
    }
  } catch (e) {
    if (!(e instanceof FramingError)) throw e;
    verdicts.push("framing"); // lines() ends here: nothing after it is read
  }
  return { verdicts, values };
}

test("every wire case gets the verdicts and values the Go host gives it", async () => {
  const { cases } = JSON.parse(readFileSync(join(root, "fixtures/contract/wire-cases.json"), "utf8")) as { cases: WireCase[] };
  assert.ok(cases.length > 0);
  for (const c of cases) {
    const b = bytes(c);
    // Whole, and in pieces of 1 and 3 bytes where that is cheap, so line
    // boundaries and multibyte sequences fall across chunks.
    for (const size of b.length > 1 << 20 ? [b.length, 65536] : [b.length || 1, 1, 3]) {
      const { verdicts, values } = await read(chunked(b, size));
      assert.deepEqual(verdicts, c.want, `${c.name} (chunks of ${size})`);
      if (c.nest) {
        // Generated: the ordinary parser is the reference.
        for (const v of values) assert.deepEqual(v.value, JSON.parse(v.line), c.name);
        continue;
      }
      assert.deepEqual(values.map((v) => v.value), c.decoded ?? [], `${c.name}: decoded (chunks of ${size})`);
    }
  }
});
