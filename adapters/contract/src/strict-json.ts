// SPDX-License-Identifier: Apache-2.0
// A strict JSON reader for the adapter boundary: the byte-level rules the Go
// reader (internal/contract) applies before validation, which JSON.parse
// does not. Refused: duplicate keys, any number that is not a safe integer
// spelled canonically (no fraction, exponent, leading zero or -0; every
// contract number is an integer), lone surrogates in escapes, raw control
// characters, nesting deeper than maxDepth, and anything after the value.

export class StrictJSONError extends Error {}

const maxDepth = 128;
/** A plain run at least this long is appended as one slice; shorter runs and
 *  escapes collect as char codes, decoded in batches of codeBatch. */
const longRun = 64;
const codeBatch = 4096;
const integer = /^-?(0|[1-9][0-9]*)/;

/** parseStrict reads one JSON text. maxValues, when given, bounds how many
 *  values (containers and scalars) it builds, so a reader that must survive
 *  hostile input (the conformance kit) fails before allocating them; the
 *  default accepts any count, as the Go reader does. */
export function parseStrict(text: string, maxValues = Infinity): unknown {
  let i = 0;
  let values = 0;
  const fail = (why: string): never => {
    throw new StrictJSONError(`${why} at offset ${i}`);
  };
  const ws = () => {
    while (i < text.length && " \t\n\r".includes(text[i])) i++;
  };
  const literal = (word: string, v: unknown) => {
    if (text.startsWith(word, i)) {
      i += word.length;
      return v;
    }
    return fail("unexpected token");
  };
  const str = (): string => {
    i++; // the opening quote
    // Building a string one piece at a time costs a heap node per piece, so
    // long plain runs are appended as slices and everything else collects as
    // char codes, decoded a batch at a time.
    let out = "";
    let codes: number[] = [];
    const flush = () => {
      if (codes.length) out += String.fromCharCode(...codes);
      codes = [];
    };
    const push = (...cs: number[]) => {
      codes.push(...cs);
      if (codes.length >= codeBatch) flush();
    };
    for (;;) {
      if (i >= text.length) return fail("unterminated string");
      const c = text.charCodeAt(i);
      if (c === 0x22) {
        i++;
        flush();
        return out;
      }
      if (c < 0x20) return fail("raw control character in a string");
      if (c !== 0x5c) {
        // A raw lone surrogate cannot come from strictly decoded UTF-8.
        const from = i;
        while (i < text.length) {
          const d = text.charCodeAt(i);
          if (d === 0x22 || d === 0x5c || d < 0x20) break;
          i++;
        }
        if (i - from >= longRun) {
          flush();
          out += text.slice(from, i);
        } else for (let k = from; k < i; k++) push(text.charCodeAt(k));
        continue;
      }
      const e = text[i + 1];
      i += 2;
      const simple: Record<string, string> = { '"': '"', "\\": "\\", "/": "/", b: "\b", f: "\f", n: "\n", r: "\r", t: "\t" };
      if (e in simple) {
        push(simple[e].charCodeAt(0));
        continue;
      }
      if (e !== "u") return fail("invalid escape");
      const unit = (at: number) => {
        const h = text.slice(at, at + 4);
        return /^[0-9A-Fa-f]{4}$/.test(h) ? parseInt(h, 16) : fail("invalid \\u escape");
      };
      const u = unit(i);
      i += 4;
      if (u >= 0xdc00 && u <= 0xdfff) return fail("lone low surrogate");
      if (u >= 0xd800 && u <= 0xdbff) {
        if (text.slice(i, i + 2) !== "\\u") return fail("lone high surrogate");
        const lo = unit(i + 2);
        if (lo < 0xdc00 || lo > 0xdfff) return fail("lone high surrogate");
        i += 6;
        push(u, lo);
        continue;
      }
      push(u);
    }
  };
  const num = (): number => {
    const m = integer.exec(text.slice(i, i + 24));
    if (!m) return fail("invalid number");
    const after = text[i + m[0].length];
    if (after === "." || after === "e" || after === "E" || (after !== undefined && after >= "0" && after <= "9")) {
      return fail("not an integer spelling");
    }
    if (m[0] === "-0") return fail("-0");
    const n = Number(m[0]);
    if (!Number.isSafeInteger(n)) return fail("integer outside ±(2^53−1)");
    i += m[0].length;
    return n;
  };
  const value = (depth: number): unknown => {
    if (depth >= maxDepth) return fail("nesting too deep"); // at most maxDepth levels
    if (++values > maxValues) return fail(`more than ${maxValues} values`);
    ws();
    const c = text[i];
    if (c === "{") {
      i++;
      const obj: Record<string, unknown> = Object.create(null);
      ws();
      if (text[i] === "}") {
        i++;
        return { ...obj };
      }
      for (;;) {
        ws();
        if (text[i] !== '"') return fail("expected a key");
        const k = str();
        if (Object.hasOwn(obj, k)) return fail(`duplicate key ${JSON.stringify(k)}`);
        ws();
        if (text[i++] !== ":") return fail("expected :");
        obj[k] = value(depth + 1);
        ws();
        if (text[i] === ",") {
          i++;
          continue;
        }
        if (text[i++] === "}") return { ...obj };
        return fail("expected , or }");
      }
    }
    if (c === "[") {
      i++;
      const arr: unknown[] = [];
      ws();
      if (text[i] === "]") {
        i++;
        return arr;
      }
      for (;;) {
        arr.push(value(depth + 1));
        ws();
        if (text[i] === ",") {
          i++;
          continue;
        }
        if (text[i++] === "]") return arr;
        return fail("expected , or ]");
      }
    }
    if (c === '"') return str();
    if (c === "t") return literal("true", true);
    if (c === "f") return literal("false", false);
    if (c === "n") return literal("null", null);
    return num();
  };
  const v = value(0);
  ws();
  if (i !== text.length) fail("trailing data");
  return v;
}
