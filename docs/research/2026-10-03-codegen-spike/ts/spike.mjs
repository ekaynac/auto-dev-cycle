// SPDX-License-Identifier: Apache-2.0
// Codegen spike, TypeScript side: json-schema-to-typescript types + Ajv
// standalone validators from ../schemas, then the shared cases in ../cases.json.
// Run from this directory after `pnpm add json-schema-to-typescript@16.0.0 ajv@8.20.0`.
import { compileFromFile } from "json-schema-to-typescript";
import Ajv2020 from "ajv/dist/2020.js";
import standalone from "ajv/dist/standalone/index.js";
import { readFileSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";

const base = "https://github.com/ekaynac/auto-dev-cycle/schemas/";
const files = ["common/defs", "records/receipt", "records/approval", "adapter/event"];
const ajv = new Ajv2020({ code: { source: true }, strict: true, allErrors: true });
for (const f of files) ajv.addSchema(JSON.parse(readFileSync(`../schemas/${f}.json`, "utf8")));

const ids = {};
for (const f of files.slice(1)) {
  const name = f.split("/")[1];
  ids[name] = `${base}${f}.json`;
  const ts = await compileFromFile(`../schemas/${f}.json`, { cwd: `../schemas/${f.split("/")[0]}`, bannerComment: "" });
  writeFileSync(`out-${name}.ts`, ts);
}
writeFileSync("out-validators.cjs", standalone(ajv, ids)); // CommonJS: Ajv's ESM output still require()s runtime helpers

const v = createRequire(import.meta.url)("./out-validators.cjs");
let bad = 0;
for (const c of JSON.parse(readFileSync("../cases.json", "utf8"))) {
  const got = v[c.Schema](c.Doc);
  if (got !== c.Want) bad++;
  console.log(got === c.Want ? "ok  " : "BAD ", c.Name, got);
}
process.exit(bad ? 1 : 0);
