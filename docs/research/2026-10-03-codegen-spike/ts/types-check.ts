// SPDX-License-Identifier: Apache-2.0
// Compile-time assertions on the generated types (`npx tsc -p .`). Every line
// marked as an expected error must fail to compile; tsc fails if one compiles.
import type { AdapterEvent, Envelope } from "./out-event.js";
import type { Receipt, Subject, Demand } from "./out-receipt.js";
import type { Approval, PlanApproveScope, InputAnswerScope } from "./out-approval.js";
import type { Grant, ProjectSet } from "./out-grant.js";

const id = "cal_01h455vb4pex5vsknk084sn002";
const env: Envelope = { attempt: id, fencing_token: 1, call: id };

// Literal discriminators and narrowing.
export function subjectId(s: Subject): string {
  switch (s.type) {
    case "tree": return s.tree;
    case "proposal": return s.proposal;
    case "call": return s.call;
  }
}
export function onlyCall(s: Subject): string {
  if (s.type === "call") {
    // @ts-expect-error a call subject has no proposal field
    return s.proposal;
  }
  return "";
}
// @ts-expect-error unknown discriminator value
export const badEvent: AdapterEvent = { ...env, event: "coverag" };
// @ts-expect-error the envelope's call is required
export const noCall: AdapterEvent = { attempt: id, fencing_token: 1, event: "heartbeat" };
export const beat: AdapterEvent = { ...env, event: "heartbeat" };

// Nullable unions are required and accept null, never undefined.
export const noDemand: Demand = null;
// @ts-expect-error demand is null or an object with key and run
export const halfDemand: Demand = { key: "sha256:x" };
export function demandRequired(r: Receipt): Demand {
  return r.subject.demand;
}

// Keyed union and per-action scope types exist.
export const byVersion: Approval["expected_state"] = { version: 3 };
export const scope: PlanApproveScope = { plan_revision: 3 };
// @ts-expect-error plan_revision is required
export const noRevision: PlanApproveScope = {};
export const answerScope: InputAnswerScope = { owed_input: id, owed_revision: 1, answer: {} };

// Array-or-literal union and typed maps.
export const all: ProjectSet = "*";
export const some: ProjectSet = [id];
// @ts-expect-error only "*" is allowed as a literal
export const other: ProjectSet = "all";
// @ts-expect-error at least one project in the list form
export const none: ProjectSet = [];
export const overrides: Grant["confirmation_overrides"] = { "plan.approve": "human" };
// @ts-expect-error overrides may only name independent_review or human
export const weaker: Grant["confirmation_overrides"] = { "plan.approve": "none" };
