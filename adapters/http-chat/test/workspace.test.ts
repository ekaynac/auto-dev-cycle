// SPDX-License-Identifier: Apache-2.0
import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, existsSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { ConfinementError, fileDiff, segments, Workspace } from "../src/workspace.ts";

const scratch = (t: { after: (f: () => void) => void }) => {
  const d = mkdtempSync(join(tmpdir(), "adc-http-chat-ws-"));
  t.after(() => rmSync(d, { recursive: true, force: true }));
  return d;
};

test("segments refuses every path that could leave the workspace", () => {
  for (const p of ["/etc/passwd", "..", "a/../../b", "a/..", ".git/config", "a/.git/hooks/x", "a\u0000b", 'a"b', "a\\b", 7, null]) {
    assert.throws(() => segments(p), ConfinementError, String(p));
  }
  assert.deepEqual(segments("./a//b/"), ["a", "b"]);
  assert.deepEqual(segments(""), []);
});

// Each case: the file before and after (null: absent). The diff must apply with git to give "after".
const cases: [string, string | null, string | null][] = [
  ["a middle line changed", "1\n2\n3\n4\n5\n6\n7\n8\n9\n", "1\n2\n3\n4\nFIVE\n6\n7\n8\n9\n"],
  ["lines appended", "a\nb\n", "a\nb\nc\nd\n"],
  ["lines prepended", "a\nb\n", "z\na\nb\n"],
  ["a new file", null, "new\nfile\n"],
  ["a deleted file", "gone\n", null],
  ["no newline at the end, before", "a\nb", "a\nb\n"],
  ["no newline at the end, after", "a\nb\n", "a\nc"],
  ["an empty new file", null, ""],
  ["emptied", "a\n", ""],
];

for (const [name, before, after] of cases) {
  test(`fileDiff applies with git: ${name}`, (t) => {
    const d = scratch(t);
    const f = join(d, "dir", "f.txt");
    mkdirSync(dirname(f));
    if (before !== null) writeFileSync(f, before);
    execFileSync("git", ["apply", "-"], { cwd: d, input: fileDiff("dir/f.txt", before, after) });
    if (after === null) assert.ok(!existsSync(f));
    else assert.equal(readFileSync(f, "utf8"), after);
  });
}

test("fileDiff is empty when nothing changed", () => {
  assert.equal(fileDiff("x", "a\n", "a\n"), "");
  assert.equal(fileDiff("x", null, null), "");
});

test("a write and its undo leave no patch; a read-only workspace refuses writes", async (t) => {
  const d = scratch(t);
  writeFileSync(join(d, "f"), "a\n");
  const w = new Workspace(d, true);
  await w.write("f", "b\n");
  await w.write("f", "a\n");
  assert.equal(await w.patch(), "");
  await assert.rejects(new Workspace(d, false).write("f", "b\n"), ConfinementError);
  assert.equal(readFileSync(join(d, "f"), "utf8"), "a\n");
});

test("a binary file is never edited", async (t) => {
  const d = scratch(t);
  writeFileSync(join(d, "b.bin"), Buffer.from([0xff, 0xfe, 0x00]));
  await assert.rejects(new Workspace(d, true).write("b.bin", "text"), /not UTF-8/);
  assert.deepEqual([...readFileSync(join(d, "b.bin"))], [0xff, 0xfe, 0x00]);
});
