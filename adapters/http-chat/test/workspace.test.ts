// SPDX-License-Identifier: Apache-2.0
import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { constants, existsSync, linkSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, realpathSync, renameSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { open } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { ConfinementError, fileDiff, segments, within, Workspace } from "../src/workspace.ts";

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

test("F: a file hard-linked to one outside is never written or removed", async (t) => {
  const d = scratch(t);
  mkdirSync(join(d, "ws"));
  writeFileSync(join(d, "outside.txt"), "outside\n");
  linkSync(join(d, "outside.txt"), join(d, "ws", "h"));
  const w = new Workspace(join(d, "ws"), true);
  await assert.rejects(w.write("h", "pwned"), /hard link/);
  await assert.rejects(w.remove("h"), /hard link/);
  assert.equal(readFileSync(join(d, "outside.txt"), "utf8"), "outside\n");
  assert.ok(existsSync(join(d, "ws", "h")));
});

test("F: a parent swapped for a symlink after the walk redirects nothing: no write, read or delete reaches outside", async (t) => {
  const d = scratch(t);
  for (const p of ["ws/sub", "outside"]) mkdirSync(join(d, p), { recursive: true });
  writeFileSync(join(d, "outside", "victim.txt"), "outside\n");
  writeFileSync(join(d, "ws", "sub", "victim.txt"), "inside\n");
  const w = new Workspace(join(d, "ws"), true);
  // The race, made deterministic: right after each walk, sub/ moves aside (still inside) and a link to outside/ takes its name.
  const walk = (w as any).walk.bind(w);
  (w as any).walk = async (dirs: string[], create: boolean) => {
    const r = await walk(dirs, create);
    if (dirs[0] === "sub") {
      rmSync(join(d, "ws", "sub-old"), { recursive: true, force: true });
      renameSync(join(d, "ws", "sub"), join(d, "ws", "sub-old"));
      symlinkSync(join(d, "outside"), join(d, "ws", "sub"));
    }
    return r;
  };
  const unswap = () => {
    rmSync(join(d, "ws", "sub"));
    renameSync(join(d, "ws", "sub-old"), join(d, "ws", "sub"));
  };
  assert.equal(await w.read("sub/victim.txt"), "inside\n");
  unswap();
  await w.remove("sub/victim.txt");
  unswap();
  // The file is gone: the write creates it exclusively, in the held directory, and a refusal removes only its own.
  await w.write("sub/victim.txt", "new\n");
  assert.equal(readFileSync(join(d, "ws", "sub-old", "victim.txt"), "utf8"), "new\n");
  assert.deepEqual(readdirSync(join(d, "outside")), ["victim.txt"]);
  assert.equal(readFileSync(join(d, "outside", "victim.txt"), "utf8"), "outside\n");
});

test("F: a directory whose descriptor resolves outside the root is not within it", async (t) => {
  const d = scratch(t);
  for (const p of ["ws/sub", "outside"]) mkdirSync(join(d, p), { recursive: true });
  const root = realpathSync(join(d, "ws"));
  for (const [p, want] of [["ws", true], ["ws/sub", true], ["outside", false], ["", false]] as const) {
    const h = await open(join(d, p), constants.O_RDONLY | constants.O_DIRECTORY);
    t.after(() => h.close());
    assert.equal(await within(root, h), want, p);
  }
});

test("F: a refused write leaves no directory it created", async (t) => {
  const d = scratch(t);
  writeFileSync(join(d, "keep"), "x");
  const w = new Workspace(d, true);
  // The open-time check refuses the new file: the walk made n1/ and n1/n2/ for it, and both go.
  (w as any).pin = async () => {
    throw new ConfinementError("refused");
  };
  await assert.rejects(w.write("n1/n2/f.txt", "x"), /refused/);
  assert.deepEqual(readdirSync(d), ["keep"]);
});

test("F: a file that appears during a write is never adopted, nor removed", async (t) => {
  const d = scratch(t);
  const w = new Workspace(d, true);
  const pin = (w as any).pin.bind(w);
  (w as any).pin = async (...a: unknown[]) => {
    writeFileSync(join(d, "f.txt"), "theirs\n"); // after the walk saw no file
    return pin(...a);
  };
  await assert.rejects(w.write("f.txt", "pwned"), /appeared/);
  assert.equal(readFileSync(join(d, "f.txt"), "utf8"), "theirs\n");
});

test("list_dir stops at 1000 entries", async (t) => {
  const d = scratch(t);
  for (let i = 0; i < 1005; i++) writeFileSync(join(d, `f${i}`), "");
  assert.equal((await new Workspace(d, false).list("")).split("\n").length, 1000);
});

test("read_file never blocks on a FIFO", async (t) => {
  const d = scratch(t);
  execFileSync("mkfifo", [join(d, "fifo")]);
  await assert.rejects(new Workspace(d, false).read("fifo"), /not a regular file/);
});

test("git's .git look-alikes are refused", () => {
  for (const p of [".Git/config", "sub/.GIT/hooks/x", ".git./config", ".git /x", "git~1/config", "GIT~1", ".g‌it/config"]) {
    assert.throws(() => segments(p), /names .git/, p);
  }
  for (const p of [".gitignore", ".github/x", "git", ".gitattributes"]) assert.doesNotThrow(() => segments(p), p);
});


test("a binary file is never edited", async (t) => {
  const d = scratch(t);
  writeFileSync(join(d, "b.bin"), Buffer.from([0xff, 0xfe, 0x00]));
  await assert.rejects(new Workspace(d, true).write("b.bin", "text"), /not UTF-8/);
  assert.deepEqual([...readFileSync(join(d, "b.bin"))], [0xff, 0xfe, 0x00]);
});
