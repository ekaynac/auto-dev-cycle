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
}

export function start(o: StartOptions): Promise<StubProvider>;
export function body(r: object, id: string): object;
