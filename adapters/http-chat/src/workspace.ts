// SPDX-License-Identifier: Apache-2.0
// The builder's workspace tools, confined to the prepared workspace
// (docs/design/m1/00-slice-plan.md P13). A path is relative, has no ".."
// and no ".git" component, and no component may be a symlink or (except
// the last) anything but a directory: every write lands inside the
// workspace, whatever links the repository holds. The first change to a
// path records its original content, so the patch is cumulative over the
// whole run and names only the paths the run touched.

import { constants } from "node:fs";
import { lstat, mkdir, open, readdir, realpath, unlink } from "node:fs/promises";
import { join, sep } from "node:path";

/** ConfinementError: a path the workspace refuses; nothing was touched. */
export class ConfinementError extends Error {}

const maxPathBytes = 1024;
/** The bytes read_file returns and write_file accepts. */
export const maxFileBytes = 256 << 10;
const maxEntries = 1000;
const segment = /^[^\x00-\x1f\x7f"\\]+$/;
const utf8 = new TextDecoder("utf-8", { fatal: true });

/** segments is rel as checked path components ([] is the root). */
export function segments(rel: unknown): string[] {
  if (typeof rel !== "string" || Buffer.byteLength(rel) > maxPathBytes) throw new ConfinementError("a path is a string of at most 1024 bytes");
  if (rel.startsWith("/")) throw new ConfinementError(`${JSON.stringify(rel)} is absolute; paths are relative to the workspace`);
  const parts = rel.split("/").filter((p) => p !== "" && p !== ".");
  for (const p of parts) {
    if (p === "..") throw new ConfinementError(`${JSON.stringify(rel)} leaves the workspace`);
    if (p === ".git") throw new ConfinementError(`${JSON.stringify(rel)} is inside .git`);
    if (!segment.test(p)) throw new ConfinementError(`${JSON.stringify(rel)} holds a control, quote or backslash character`);
  }
  return parts;
}

const missing = (e: unknown) => (e as NodeJS.ErrnoException)?.code === "ENOENT";

export class Workspace {
  readonly root: string;
  readonly writable: boolean;
  /** The original content of each changed path, null when it did not exist. */
  private readonly originals = new Map<string, string | null>();

  constructor(root: string, writable: boolean) {
    this.root = root;
    this.writable = writable;
  }

  /** walk resolves parts under the root: every parent a real directory
   *  (created when create is set), never a symlink. It returns the path and
   *  the last component's lstat, null when it does not exist. */
  private async walk(parts: string[], create: boolean) {
    let dir = this.root;
    for (const p of parts.slice(0, -1)) {
      dir = join(dir, p);
      const st = await lstat(dir).catch((e) => (missing(e) ? null : Promise.reject(e)));
      if (st === null && create) await mkdir(dir);
      else if (st === null) throw new ConfinementError(`${parts.join("/")} does not exist`);
      else if (!st.isDirectory()) throw new ConfinementError(`${parts.join("/")} passes through ${p}, which is not a directory (a symlink is never followed)`);
    }
    const path = join(this.root, ...parts);
    const st = parts.length === 0 ? await lstat(path) : await lstat(path).catch((e) => (missing(e) ? null : Promise.reject(e)));
    if (st?.isSymbolicLink()) throw new ConfinementError(`${parts.join("/")} is a symlink, which is never followed`);
    return { path, st };
  }

  /** pin opens path and checks what it opened, since a parent may have been
   *  swapped for a symlink after the walk: a regular file whose real path,
   *  read from the descriptor (/proc; none fails closed), is under the
   *  root's; for a change, its only link, so a write never reaches a hard
   *  link outside. On a refusal the descriptor is closed. */
  private async pin(path: string, flags: number, change: boolean) {
    const f = await open(path, flags | constants.O_NOFOLLOW, 0o644);
    try {
      const root = await realpath(this.root);
      const real = await realpath(`/proc/self/fd/${f.fd}`).catch(() => "");
      const st = await f.stat();
      if (!real.startsWith(root + sep)) throw new ConfinementError("the file resolves outside the workspace");
      if (!st.isFile()) throw new ConfinementError("not a regular file");
      if (change && st.nlink > 1) throw new ConfinementError("the file has another hard link, which a change would reach");
      return { f, real, st };
    } catch (e) {
      await f.close();
      throw e;
    }
  }

  private async readText(path: string): Promise<string> {
    const { f, st } = await this.pin(path, constants.O_RDONLY, false);
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
    const { path, st } = await this.walk(parts, false);
    if (!st) throw new ConfinementError(`${parts.join("/")} does not exist`);
    return this.readText(path);
  }

  async list(rel: unknown): Promise<string> {
    const { path, st } = await this.walk(segments(rel), false);
    if (!st?.isDirectory()) throw new ConfinementError("not a directory");
    const names = (await readdir(path, { withFileTypes: true })).filter((d) => d.name !== ".git");
    return names.slice(0, maxEntries).map((d) => d.name + (d.isDirectory() ? "/" : "")).sort().join("\n");
  }

  /** remember records path's original content before its first change. */
  private async remember(key: string, path: string, exists: boolean): Promise<void> {
    if (this.originals.has(key)) return;
    this.originals.set(key, exists ? await this.readText(path) : null);
  }

  private async changeable(rel: unknown, create: boolean) {
    if (!this.writable) throw new ConfinementError("the workspace is read-only for this role");
    const parts = segments(rel);
    if (parts.length === 0) throw new ConfinementError("the workspace root is not a file");
    const w = await this.walk(parts, create);
    if (w.st && !w.st.isFile()) throw new ConfinementError(`${parts.join("/")} is not a regular file`);
    await this.remember(parts.join("/"), w.path, w.st !== null);
    return w;
  }

  async write(rel: unknown, content: unknown): Promise<void> {
    if (typeof content !== "string" || Buffer.byteLength(content) > maxFileBytes) throw new ConfinementError(`content is a string of at most ${maxFileBytes} bytes`);
    const { path, st } = await this.changeable(rel, true);
    // Truncated only once pinned: a refused file is never changed, and one this call created is removed.
    const { f } = await this.pin(path, constants.O_WRONLY | constants.O_CREAT, true).catch(async (e) => {
      if (!st) await unlink(path).catch(() => undefined);
      throw e;
    });
    try {
      await f.truncate(0);
      await f.writeFile(content);
    } finally {
      await f.close();
    }
  }

  async remove(rel: unknown): Promise<void> {
    const { path, st } = await this.changeable(rel, false);
    if (!st) throw new ConfinementError("the file does not exist");
    const { f, real } = await this.pin(path, constants.O_RDONLY, true);
    await f.close();
    await unlink(real);
  }

  /** patch is the run's cumulative change as a unified diff ("" for none). */
  async patch(): Promise<string> {
    let out = "";
    for (const key of [...this.originals.keys()].sort()) {
      const { path, st } = await this.walk(key.split("/"), false).catch(() => ({ path: "", st: null }));
      const now = st?.isFile() ? await this.readText(path) : null;
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
