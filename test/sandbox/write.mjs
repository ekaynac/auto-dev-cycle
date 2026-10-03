// SPDX-License-Identifier: Apache-2.0
// Tries a TypeScript/Node-style direct write (fs.writeFileSync) to the path in
// argv[2]. Exit 3 = the write was refused (the sandbox held); exit 0 = it landed.
import { writeFileSync } from "node:fs";

try {
  writeFileSync(process.argv[2], "x");
  process.exit(0);
} catch {
  process.exit(3);
}
