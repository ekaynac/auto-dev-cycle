// SPDX-License-Identifier: Apache-2.0
// @adc/contract: the adapter SDK (docs/adapters/AUTHORING.md §2).

export { Adapter, run, serve, heartbeatPeriod, eventMethod, ParseError, InvalidRequest, MethodNotFound, InvalidParams, InternalError } from "./adapter.ts";
export type { EventBody, Handle, RunTools, ServeOptions } from "./adapter.ts";
export { frame, lines, maxMessageBytes, FramingError } from "./framing.ts";
export { parseStrict, StrictJSONError } from "./strict-json.ts";
