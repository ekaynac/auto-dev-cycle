#!/usr/bin/env node
// SPDX-License-Identifier: Apache-2.0
// The http-chat adapter's process. ADC_GATEWAY_SOCKET names the gateway's
// Unix socket inside the sandbox (an absolute path); unset, the gateway is
// reached at its base_url. No other environment is read: the adapter holds
// no credential.

import { isAbsolute } from "node:path";
import { run } from "@adc/contract";
import { HttpChatAdapter } from "./adapter.ts";

const socketPath = process.env.ADC_GATEWAY_SOCKET;
if (socketPath !== undefined && !isAbsolute(socketPath)) {
  process.stderr.write(`adc http-chat adapter: ADC_GATEWAY_SOCKET ${JSON.stringify(socketPath)} is not an absolute path\n`);
  process.exit(2);
}
await run(new HttpChatAdapter({ socketPath }));
