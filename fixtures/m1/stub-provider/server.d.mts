// SPDX-License-Identifier: Apache-2.0
// The stub provider's types, for TypeScript callers (the http-chat adapter's tests).

export interface StubProvider {
  /** The base URL; requests go to base + "/chat/completions". */
  base: string;
  close(): Promise<void>;
}

export interface StartOptions {
  scenario: object;
  listen?: string;
  log?: string;
  barriers?: string;
  /** The log's bound in bytes (default 256 MiB). */
  maxLog?: number;
}

/** Starts after check(scenario); rejects a non-loopback listen. */
export function start(o: StartOptions): Promise<StubProvider>;
/** Throws on a scenario with an unknown key or a malformed reply. */
export function check(scenario: object): void;
export function body(r: object, id: string): object;
