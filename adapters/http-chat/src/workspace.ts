// SPDX-License-Identifier: Apache-2.0
// The builder's workspace tools, confined to the prepared workspace
// (docs/design/m1/00-slice-plan.md P13). A path is relative, has no ".."
// and no .git look-alike component. It is resolved one directory at a time
// through descriptors (/proc/self/fd/<dir>/<name>, in effect openat), never
// following a symlink, and the last directory's real path must be under
// the root's: a component swapped after it was opened cannot redirect a
// create, write or unlink. A change never reaches a file with another hard
// link. The first change to a path records its original content, so the
// patch is cumulative over the run and names only the paths it touched.

import { constants } from "node:fs";
import { lstat, mkdir, open, opendir, realpath, rmdir, unlink, type FileHandle } from "node:fs/promises";
import { sep } from "node:path";

/** ConfinementError: a path the workspace refuses; nothing was touched. */
export class ConfinementError extends Error {}

const maxPathBytes = 1024;
/** The bytes read_file returns and write_file accepts. */
export const maxFileBytes = 256 << 10;
const maxEntries = 1000;
const segment = /^[^\x00-\x1f\x7f"\\]+$/;
const utf8 = new TextDecoder("utf-8", { fatal: true });
const { O_RDONLY, O_WRONLY, O_CREAT, O_EXCL, O_NOFOLLOW, O_NONBLOCK, O_DIRECTORY } = constants;
const dirFlags = O_RDONLY | O_DIRECTORY | O_NOFOLLOW;
/** HFS+ ignores these code points in names (git's is_hfs_dotgit). */
const hfsIgnorable = /[\u200c-\u200f\u202a-\u202e\u206a-\u206f\ufeff]/g;

/** gitLike is git's verify_path view of a component that names .git on
 *  some filesystem: case-folded, without HFS-ignorable code points and
 *  trailing dots or spaces (NTFS), .git or its 8.3 name git~1. */
export function gitLike(p: string): boolean {
  const n = p.replace(hfsIgnorable, "").toLowerCase().replace(/[. ]+$/, "");
  return n === ".git" || n === "git~1";
}

/** segments is rel as checked path components ([] is the root). */
export function segments(rel: unknown): string[] {
  if (typeof rel !== "string" || Buffer.byteLength(rel) > maxPathBytes) throw new ConfinementError("a path is a string of at most 1024 bytes");
  if (rel.startsWith("/")) throw new ConfinementError(`${JSON.stringify(rel)} is absolute; paths are relative to the workspace`);
  const parts = rel.split("/").filter((p) => p !== "" && p !== ".");
  for (const p of parts) {
    if (p === "..") throw new ConfinementError(`${JSON.stringify(rel)} leaves the workspace`);
    if (gitLike(p)) throw new ConfinementError(`${JSON.stringify(rel)} names .git`);
    if (!segment.test(p)) throw new ConfinementError(`${JSON.stringify(rel)} holds a control, quote or backslash character`);
  }
  return parts;
}

const code = (e: unknown) => (e as NodeJS.ErrnoException)?.code;
/** at names name inside the directory open as d, resolved from d itself. */
const at = (d: FileHandle, name: string) => `/proc/self/fd/${d.fd}/${name}`;

/** within: the directory open as d is root (a real path) or under it, by
 *  its descriptor's real path; without /proc it is not. */
export async function within(root: string, d: FileHandle): Promise<boolean> {
  const real = await realpath(`/proc/self/fd/${d.fd}`).catch(() => "");
  return real === root || real.startsWith(root + sep);
}

/** Walk holds a path's directories open, root first; made are the ones it created. */
class Walk {
  readonly dirs: FileHandle[] = [];
  readonly made: [FileHandle, string][] = [];
  get dir() { return this.dirs[this.dirs.length - 1]; }
  /** undo removes the directories this walk created, deepest first. */
  async undo() { for (const [p, n] of this.made.reverse()) await rmdir(at(p, n)).catch(() => undefined); }
  async close() { for (const d of this.dirs) await d.close().catch(() => undefined); }
}

export class Workspace {
  readonly root: string;
  readonly writable: boolean;
  /** The original content of each changed path, null when it did not exist. */
  private readonly originals = new Map<string, string | null>();

  constructor(root: string, writable: boolean) {
    this.root = root;
    this.writable = writable;
  }

  /** walk opens dirs one by one from the root, creating missing ones when
   *  create is set, and checks the last one's real path (/proc; none fails
   *  closed) is under the root's. */
  private async walk(dirs: string[], create: boolean): Promise<Walk> {
    const w = new Walk();
    try {
      const root = await realpath(this.root);
      w.dirs.push(await open(root, dirFlags));
      for (const p of dirs) {
        let d = await open(at(w.dir, p), dirFlags).catch((e) => (code(e) === "ENOENT" && create ? null : Promise.reject(e)));
        if (d === null) {
          await mkdir(at(w.dir, p));
          w.made.push([w.dir, p]);
          d = await open(at(w.dir, p), dirFlags);
        }
        w.dirs.push(d);
      }
      if (!(await within(root, w.dir))) throw new ConfinementError("the directory resolves outside the workspace");
      return w;
    } catch (e) {
      await w.undo();
      await w.close();
      if (e instanceof ConfinementError) throw e;
      if (code(e) === "ELOOP" || code(e) === "ENOTDIR") throw new ConfinementError(`${dirs.join("/")} passes through a symlink or a non-directory, never followed`);
      if (code(e) === "ENOENT") throw new ConfinementError(`${dirs.join("/")} does not exist`);
      throw e;
    }
  }

  /** inside runs f on parts' last component in its walked directory, with
   *  its lstat (null when absent); a refusal undoes the walk's directories. */
  private async inside<T>(parts: string[], create: boolean, f: (w: Walk, name: string, st: Awaited<ReturnType<typeof lstat>> | null) => Promise<T>): Promise<T> {
    const w = await this.walk(parts.slice(0, -1), create);
    try {
      const name = parts[parts.length - 1];
      const st = await lstat(at(w.dir, name)).catch((e) => (code(e) === "ENOENT" ? null : Promise.reject(e)));
      if (st?.isSymbolicLink()) throw new ConfinementError(`${parts.join("/")} is a symlink, which is never followed`);
      return await f(w, name, st);
    } catch (e) {
      await w.undo();
      throw e;
    } finally {
      await w.close();
    }
  }

  /** pin opens name in w's directory, never blocking or following a link,
   *  and checks it is a regular file and, for a change, its only link. */
  private async pin(w: Walk, name: string, flags: number, change: boolean) {
    const f = await open(at(w.dir, name), flags | O_NOFOLLOW | O_NONBLOCK, 0o644);
    try {
      const st = await f.stat();
      if (!st.isFile()) throw new ConfinementError("not a regular file");
      if (change && st.nlink > 1) throw new ConfinementError("the file has another hard link, which a change would reach");
      return { f, st };
    } catch (e) {
      await f.close();
      throw e;
    }
  }

  private async readText(w: Walk, name: string): Promise<string> {
    const { f, st } = await this.pin(w, name, O_RDONLY, false);
    try {
      if (st.size > maxFileBytes) throw new ConfinementError(`the file is over ${maxFileBytes} bytes`);
      try {
        return utf8.decode(await f.readFile());
      } catch {
        throw new ConfinementError("the file is not UTF-8 text");
      }
    } finally {
      await f.close();
    }
  }

  async read(rel: unknown): Promise<string> {
    const parts = segments(rel);
    if (parts.length === 0) throw new ConfinementError("the workspace root is not a file");
    return this.inside(parts, false, async (w, name, st) => {
      if (!st?.isFile()) throw new ConfinementError(`${parts.join("/")} is not a regular file`);
      return this.readText(w, name);
    });
  }

  async list(rel: unknown): Promise<string> {
    const w = await this.walk(segments(rel), false);
    try {
      const names: string[] = [];
      for await (const d of await opendir(`/proc/self/fd/${w.dir.fd}`)) {
        if (d.name !== ".git") names.push(d.name + (d.isDirectory() ? "/" : ""));
        if (names.length >= maxEntries) break; // closes the directory
      }
      return names.sort().join("\n");
    } finally {
      await w.close();
    }
  }

  /** change runs f on a writable path's last component, after recording
   *  its original content. */
  private async change(rel: unknown, create: boolean, f: (w: Walk, name: string, existed: boolean) => Promise<void>): Promise<void> {
    if (!this.writable) throw new ConfinementError("the workspace is read-only for this role");
    const parts = segments(rel);
    if (parts.length === 0) throw new ConfinementError("the workspace root is not a file");
    const key = parts.join("/");
    await this.inside(parts, create, async (w, name, st) => {
      if (st && !st.isFile()) throw new ConfinementError(`${key} is not a regular file`);
      if (!this.originals.has(key)) this.originals.set(key, st ? await this.readText(w, name) : null);
      await f(w, name, st !== null);
    });
  }

  async write(rel: unknown, content: unknown): Promise<void> {
    if (typeof content !== "string" || Buffer.byteLength(content) > maxFileBytes) throw new ConfinementError(`content is a string of at most ${maxFileBytes} bytes`);
    await this.change(rel, true, async (w, name, existed) => {
      // A new file is created exclusively, so an existing one is never adopted;
      // only a file this call created is removed on a refusal.
      const { f } = await this.pin(w, name, existed ? O_WRONLY : O_WRONLY | O_CREAT | O_EXCL, true).catch(async (e) => {
        if (!existed && code(e) !== "EEXIST") await unlink(at(w.dir, name)).catch(() => undefined);
        throw code(e) === "EEXIST" ? new ConfinementError("the file appeared during the write") : e;
      });
      try {
        await f.truncate(0);
        await f.writeFile(content);
      } finally {
        await f.close();
      }
    });
  }

  async remove(rel: unknown): Promise<void> {
    await this.change(rel, false, async (w, name, existed) => {
      if (!existed) throw new ConfinementError("the file does not exist");
      const { f } = await this.pin(w, name, O_RDONLY, true);
      await f.close();
      await unlink(at(w.dir, name));
    });
  }

  /** patch is the run's cumulative change as a unified diff ("" for none). */
  async patch(): Promise<string> {
    let out = "";
    for (const key of [...this.originals.keys()].sort()) {
      const now = await this.inside(key.split("/"), false, async (w, name, st) => (st?.isFile() ? this.readText(w, name) : null)).catch((e) =>
        e instanceof ConfinementError ? null : Promise.reject(e),
      );
      out += fileDiff(key, this.originals.get(key) ?? null, now);
    }
    return out;
  }
}

const tokens = (s: string | null) => (s === null ? [] : (s.match(/[^\n]*\n|[^\n]+$/g) ?? []));
const context = 3;

/** fileDiff is one file's change as a git-style diff: one hunk spanning
 *  everything between the common leading and trailing lines, with up to
 *  three lines of context. "" when nothing changed. */
export function fileDiff(path: string, before: string | null, after: string | null): string {
  if (before === after) return "";
  const a = tokens(before);
  const b = tokens(after);
  let pre = 0;
  while (pre < a.length && pre < b.length && a[pre] === b[pre]) pre++;
  let suf = 0;
  while (suf < a.length - pre && suf < b.length - pre && a[a.length - 1 - suf] === b[b.length - 1 - suf]) suf++;
  const start = Math.max(0, pre - context);
  const tail = Math.min(suf, context);
  const line = (sign: string, t: string) => sign + (t.endsWith("\n") ? t : t + "\n\\ No newline at end of file\n");
  const body = [
    ...a.slice(start, pre).map((t) => line(" ", t)),
    ...a.slice(pre, a.length - suf).map((t) => line("-", t)),
    ...b.slice(pre, b.length - suf).map((t) => line("+", t)),
    ...a.slice(a.length - suf, a.length - suf + tail).map((t) => line(" ", t)),
  ].join("");
  const range = (n: number) => `${n === 0 ? start : start + 1},${n}`;
  const oldLen = a.length - suf + tail - start;
  const newLen = b.length - suf + tail - start;
  const head = [`diff --git a/${path} b/${path}`];
  if (before === null) head.push("new file mode 100644");
  if (after === null) head.push("deleted file mode 100644");
  head.push(before === null ? "--- /dev/null" : `--- a/${path}`, after === null ? "+++ /dev/null" : `+++ b/${path}`);
  // An empty file created or deleted has no hunk, as git writes it.
  return body === "" ? `${head.slice(0, 2).join("\n")}\n` : `${head.join("\n")}\n@@ -${range(oldLen)} +${range(newLen)} @@\n${body}`;
}
