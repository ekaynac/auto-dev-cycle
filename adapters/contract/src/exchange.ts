// SPDX-License-Identifier: Apache-2.0
// The adapter side of the blob exchange (docs/design/m0/09 "Blob exchange"):
// an output is one fresh regular file directly in out/, named exactly by its
// handle, complete on disk before its handle is returned.

import { createHash } from "node:crypto";
import { constants } from "node:fs";
import { open, unlink } from "node:fs/promises";
import { isAbsolute, join } from "node:path";
import type { Handle } from "./adapter.ts";

/** handlePattern: a handle name (schemas/adapter/event.json). It also rules
 *  out "", ".", ".." and any "/". */
export const handlePattern = /^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$/;
const attemptPattern = /^att_[0-7][0-9a-hjkmnp-tv-z]{25}$/;

/** digestOf is the contract's digest of bytes: sha256, lowercase hex. */
export function digestOf(bytes: Uint8Array): string {
  return "sha256:" + createHash("sha256").update(bytes).digest("hex");
}

/** writeOutput writes content to out/<name> and returns its attempt-scoped
 *  handle. out is the prepared exchange.out. The file is created exclusively
 *  (an existing name, a symlink included, is refused, never followed),
 *  written whole, fsynced and closed before the handle is returned; a failed
 *  write, sync or close removes it and throws the first error. content is
 *  copied before anything is awaited: the handle describes the bytes as they
 *  were at the call. */
export async function writeOutput(out: string, attempt: string, name: string, content: Uint8Array | string): Promise<Handle> {
  if (!isAbsolute(out)) throw new Error(`the exchange out directory ${JSON.stringify(out)} is not an absolute path`);
  if (!attemptPattern.test(attempt)) throw new Error(`${JSON.stringify(attempt)} is not an attempt id`);
  if (!handlePattern.test(name)) throw new Error(`${JSON.stringify(name)} is not a handle name: one file name matching ${handlePattern.source}`);
  // A snapshot taken before the first await: the caller may reuse its buffer.
  const bytes = typeof content === "string" ? Buffer.from(content) : Buffer.from(content); // Buffer.from copies a Uint8Array
  const handle = { attempt, handle: name, digest: digestOf(bytes), size: bytes.length };
  const path = join(out, name);
  const f = await open(path, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o644);
  const failures: unknown[] = [];
  try {
    await f.writeFile(bytes);
    await f.sync();
  } catch (e) {
    failures.push(e);
  }
  try {
    await f.close();
  } catch (e) {
    failures.push(e);
  }
  if (failures.length === 0) return handle;
  // Every failure after the create removes the file; the first error is the one thrown.
  for (const later of failures.slice(1)) process.stderr.write(`adc adapter: also, while writing ${path}: ${(later as Error)?.message ?? String(later)}\n`);
  await unlink(path).catch((u) => process.stderr.write(`adc adapter: could not remove the partial output ${path}: ${(u as Error).message}\n`));
  throw failures[0];
}
