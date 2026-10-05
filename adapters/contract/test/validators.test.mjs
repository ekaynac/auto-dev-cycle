// SPDX-License-Identifier: Apache-2.0
// The generated validators classify the shared corpus exactly as the Go
// validators do (schemas/shared_cases_test.go runs the same file).
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import { createRequire } from "node:module";
import { join, relative } from "node:path";

const root = new URL("../../../", import.meta.url).pathname;
const validators = createRequire(import.meta.url)(join(root, "gen/ts/validators.cjs"));

test("every shared case is classified as the Go validator classifies it", () => {
  const cases = JSON.parse(readFileSync(join(root, "fixtures/contract/cases.json"), "utf8"));
  assert.ok(cases.length > 0);
  for (const c of cases) {
    const validate = validators[c.schema];
    assert.equal(typeof validate, "function", `${c.name}: no validator for ${c.schema}`);
    assert.equal(validate(c.doc), c.valid, `${c.name}: ${JSON.stringify(validate.errors)}`);
  }
});

test("every schema file has a validator and a types module", () => {
  const walk = (d) => readdirSync(d, { withFileTypes: true }).flatMap((e) =>
    e.isDirectory() ? walk(join(d, e.name)) : e.name.endsWith(".json") ? [join(d, e.name)] : []);
  const schemas = walk(join(root, "schemas")).map((p) => relative(join(root, "schemas"), p).replace(/\.json$/, "")).sort();
  assert.deepEqual(Object.keys(validators).sort(), schemas);
  for (const s of schemas) readFileSync(join(root, "gen/ts/types", `${s}.ts`));
});
