// SPDX-License-Identifier: Apache-2.0
import { test } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { lstatSync, mkdtempSync, readdirSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { open } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { digestOf, handlePattern, writeOutput } from "../src/index.ts";

const att = "att_01m3250vnfeyrv67x6w0ddqr6a";
const scratch = () => mkdtempSync(join(tmpdir(), "adc-exchange-"));
const sha = (b: Buffer | string) => "sha256:" + createHash("sha256").update(b).digest("hex");

test("writeOutput writes one regular file named by the handle, and returns its digest and size", async (t) => {
  const out = scratch();
  t.after(() => rmSync(out, { recursive: true, force: true }));
  const h = await writeOutput(out, att, "patch.diff", "--- a\n+++ b\n");
  const bytes = readFileSync(join(out, "patch.diff"));
  assert.deepEqual(h, { attempt: att, handle: "patch.diff", digest: sha(bytes), size: bytes.length });
  const st = lstatSync(join(out, "patch.diff"));
  assert.ok(st.isFile() && st.nlink === 1);
  const raw = Uint8Array.from([0, 255, 10]);
  assert.deepEqual(await writeOutput(out, att, "b.bin", raw), { attempt: att, handle: "b.bin", digest: sha(Buffer.from(raw)), size: 3 });
  assert.deepEqual(await writeOutput(out, att, "empty", ""), { attempt: att, handle: "empty", digest: sha(""), size: 0 });
  assert.equal(digestOf(raw), sha(Buffer.from(raw)));
});

test("writeOutput never replaces or follows an existing name", async (t) => {
  const out = scratch();
  const elsewhere = scratch();
  t.after(() => [out, elsewhere].forEach((d) => rmSync(d, { recursive: true, force: true })));
  writeFileSync(join(out, "taken"), "old");
  await assert.rejects(writeOutput(out, att, "taken", "new"), { code: "EEXIST" });
  assert.equal(readFileSync(join(out, "taken"), "utf8"), "old");
  symlinkSync(join(elsewhere, "target"), join(out, "link"));
  await assert.rejects(writeOutput(out, att, "link", "new"), { code: "EEXIST" });
  assert.deepEqual(readdirSync(elsewhere), [], "the dangling symlink's target was not created");
});

test("writeOutput refuses a name that is not one file, a relative out and a non-attempt", async (t) => {
  const out = scratch();
  t.after(() => rmSync(out, { recursive: true, force: true }));
  for (const name of ["", ".", "..", "../x", "a/b", ".hidden", "-x", "a b", "x".repeat(256)]) {
    await assert.rejects(writeOutput(out, att, name, "x"), /not a handle name/, JSON.stringify(name));
  }
  await assert.rejects(writeOutput("out", att, "x", "x"), /not an absolute path/);
  await assert.rejects(writeOutput(out, "cal_01m3250v9xft08ssr4ygq6g2np", "x", "x"), /not an attempt id/);
  await assert.rejects(writeOutput(join(out, "missing"), att, "x", "x"), { code: "ENOENT" });
  assert.deepEqual(readdirSync(out), []);
});

test("a failed write, sync or close leaves no file and throws the first error", async (t) => {
  const out = scratch();
  t.after(() => rmSync(out, { recursive: true, force: true }));
  const probe = await open(join(out, "probe"), "w");
  const proto = Object.getPrototypeOf(probe);
  await probe.close();
  rmSync(join(out, "probe"));
  const fail = (what: string) => () => Promise.reject(new Error(`injected ${what} failure`));
  // close is an own property of each FileHandle: a mocked writeFile writes,
  // then replaces that handle's close with one that closes and then fails.
  const realWrite = proto.writeFile;
  const failingClose = () => t.mock.method(proto, "writeFile", async function (this: { close: () => Promise<void> }, ...args: unknown[]) {
    await realWrite.apply(this, args);
    const close = this.close.bind(this);
    this.close = async () => {
      await close();
      throw new Error("injected close failure");
    };
  });
  const cases: [string, () => void][] = [
    ["write", () => t.mock.method(proto, "writeFile", fail("write"))],
    ["sync", () => t.mock.method(proto, "sync", fail("sync"))],
    ["close", failingClose],
    ["sync", () => {
      // sync and close both fail: the sync error is thrown, the file still removed.
      t.mock.method(proto, "sync", fail("sync"));
      failingClose();
    }],
  ];
  for (const [what, inject] of cases) {
    inject();
    try {
      await assert.rejects(writeOutput(out, att, "x.diff", "bytes"), new RegExp(`injected ${what} failure`), what);
    } finally {
      t.mock.restoreAll();
    }
    assert.deepEqual(readdirSync(out), [], `${what}: no file left`);
  }
});

test("the handle describes the bytes at the call, whatever the caller does to its buffer later", async (t) => {
  const out = scratch();
  t.after(() => rmSync(out, { recursive: true, force: true }));
  const buf = Buffer.from("original bytes");
  const pending = writeOutput(out, att, "x.bin", buf);
  buf.fill(0x41);
  const h = await pending;
  buf.fill(0x42);
  assert.deepEqual(h, { attempt: att, handle: "x.bin", digest: sha("original bytes"), size: 14 });
  assert.equal(readFileSync(join(out, "x.bin"), "utf8"), "original bytes");
});

test("the handle pattern is the schema's", () => {
  const schema = readFileSync(new URL("../../../schemas/adapter/event.json", import.meta.url), "utf8");
  const m = schema.match(/"handle": \{"type": "string", "pattern": "([^"]+)"\}/);
  assert.equal(m?.[1], handlePattern.source);
});
