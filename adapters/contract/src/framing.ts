// SPDX-License-Identifier: Apache-2.0
// Newline-delimited JSON-RPC framing over stdio (docs/design/m0/09 "Process
// model"): one message per line, UTF-8, at most 16 MiB.

export const maxMessageBytes = 16 * 1024 * 1024;

export class FramingError extends Error {}

const newline = 0x0a;

/** lines yields each complete line of input, strictly decoded as UTF-8. A
 *  line over max bytes, invalid UTF-8, or a final line without its newline
 *  (a truncated message) is a FramingError. */
export async function* lines(input: AsyncIterable<Uint8Array>, max = maxMessageBytes): AsyncGenerator<string> {
  const decoder = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });
  let pending: Uint8Array[] = [];
  let size = 0;
  const take = (last: Uint8Array): string => {
    const buf = new Uint8Array(size + last.length);
    let at = 0;
    for (const p of pending) {
      buf.set(p, at);
      at += p.length;
    }
    buf.set(last, at);
    pending = [];
    size = 0;
    try {
      return decoder.decode(buf);
    } catch {
      throw new FramingError("a message is not valid UTF-8");
    }
  };
  for await (const chunk of input) {
    let start = 0;
    for (let nl = chunk.indexOf(newline); nl >= 0; nl = chunk.indexOf(newline, start)) {
      if (size + nl - start > max) throw new FramingError(`a message exceeds ${max} bytes`);
      yield take(chunk.subarray(start, nl));
      start = nl + 1;
    }
    const rest = chunk.subarray(start);
    size += rest.length;
    if (size > max) throw new FramingError(`a message exceeds ${max} bytes`);
    if (rest.length > 0) pending.push(rest.slice());
  }
  if (size > 0) throw new FramingError("the input ended inside a message");
}

/** frame serializes one message as a line, refusing one over max bytes. */
export function frame(message: unknown, max = maxMessageBytes): string {
  const text = JSON.stringify(message);
  if (Buffer.byteLength(text, "utf8") > max) throw new FramingError(`a message exceeds ${max} bytes`);
  return text + "\n";
}
