# The supervisor role — proposal

Status: **approved** by the operator 2026-10-01 (D33). It follows the operator's rulings D31–D32 (see
`00-brainstorm-decisions.md`) and the question the operator asked: *what exactly will the unattended supervisor do,
will it do something functionally meaningful, will it report, and how does it relate to the developer's attended
agent?* It defines the role both for the agent playing it and for us.

---

## 1. Premises

1. **The supervisor is not part of auto-dev-cycle.** It is an external AI agent (Claude Code, Codex or any harness)
   that operates and develops auto-dev-cycle through its CLI. auto-dev-cycle never depends on it for liveness: retries,
   holds with signals, paging and recovery all work with no supervisor present (nöbetçi lesson 2.4: "an unattended
   system cannot depend on an interactive session for liveness").
2. **There are two ways to play the role:**
   - **Attended:** the developer's own agent, working while the developer is present. It holds the developer's
     credentials. Nothing is closed to it.
   - **Unattended:** a backgrounded agent on an always-on host, working while no one watches. This reproduces the
     operator's nöbetçi practice: a background agent on a server that the operator foregrounds to inspect when they
     are at work. It runs on a **separate supervisor token** under a grant. Sensitive actions become proposals that
     need confirmation (D31, D32).
3. **Enforcement is honest.** The supervisor token's limits are real only if the unattended agent cannot read the
   operator's credentials: a different OS user, a container or another host. On the operator's own account they are
   labels, and the docs say so.

---

## 2. Why have one: the value it must add

The cycle is designed to run alone. The supervisor exists for what code deliberately does not decide. Five jobs, each
with a measurable result:

| # | Job | What it does | Measured by |
|---|---|---|---|
| J1 | **Hold resolution** | Investigates holds whose resume signal is a human act or decision. Resolves those inside its grant: carry a P2 per policy, amend a contract, add a prerequisite, re-queue, quarantine/unquarantine a source, reorder a fallback chain within the profile. For the rest, prepares a **decision packet**: evidence, options, a recommended default, the exact command. | Time in human-signal holds; packets accepted unchanged |
| J2 | **Plan stewardship** | Tracks plan progress against requirements. Proposes typed revisions: split a stuck task, add a prerequisite, defer with a record. Compiles the next phase's plan when the current one closes. Keeps the backlog. | Tasks unblocked; revisions accepted; phases opened on time |
| J3 | **Framework evolution** (after self-host, M6) | Turns incidents into **tasks for the cycle's own backlog**, with reproduction and acceptance rows. The cycle builds them. Lands them via self-install after an independent-review GO. It does not hand-edit the framework or product; the builder owns builds. | Framework fixes delivered by the cycle; repeat-incident rate |
| J4 | **Capacity stewardship** | Reviews source health, quota use and cost trends. Proposes overrides within the active usage profile. Investigates provider anomalies from per-turn evidence (the Relace analysis is the model). Prepares enrolment and profile-widening proposals for a human. | Share of work on free/local capacity; quarantines with evidence; spend per delivered row |
| J5 | **Reporting** | Writes the shift digest and handover notes (§4). Answers "what is the state?" from data. | Digest delivered on schedule; claims in it traceable to records |

What it does **not** do:
- keep the loop alive (the cycle and watchdog do);
- decide gate verdicts, severities or independence (code does);
- approve its own authority;
- touch the operator's own workloads on its own interpretation (nöbetçi lesson: give data + command, the decision is
  the operator's).

---

## 3. Limits (unattended supervisor token)

**Allowed under a grant** (proposed default grant):
- every read: status, holds, evidence, ledgers, telemetry, cost;
- hold resolution actions in J1;
- plan revisions in J2: proposals; low-risk kinds (prerequisite, hold/release, carry per policy) applied directly;
- filing framework tasks (J3);
- source quarantine and fallback reordering within the profile (J4);
- digests and notes (J5);
- self-install, with `independent_review` confirmation (D32).

**Proposal + human confirmation** (a passkey/WebAuthn assertion on the dashboard; the pager only notifies and links,
D32, D37):
- widening a data policy or allowed destinations;
- allowing paid fallback;
- enrolling agents;
- creating or extending grants (the supervisor cannot widen its own authority).

**Never, for any actor** (core invariants, not grant settings):
- resetting counters, charges, candidates or authority;
- editing evidence or records;
- disabling the watchdog or pager;
- bypassing the merge queue.

**Working rules carried from nöbetçi:**
- measure before claiming;
- write times from the clock;
- never retry a refused action with a different command;
- one change in flight at a time for framework installs;
- every claim in a report traceable to a record.

---

## 4. Reporting

- **Shift digest.** Cadence configurable; default once per day plus at every handover. Delivered through the pager
  channel and stored in auto-dev-cycle. Contents:
  - delivered rows;
  - open holds with cause and owner;
  - actions taken under the grant;
  - decisions awaiting a human (packets);
  - spend by cost basis;
  - capacity utilisation by source kind;
  - anomalies.

  Numbers, not adjectives. Healthy days are short.
- **Decision packets** are pushed when created, not batched, if they block work.
- **Incident notes** for anything that stopped work longer than the project threshold: timeline from records, root
  cause with evidence, the task filed.

---

## 5. Relationship with the attended agent: one seat, handed over

- **One supervisor seat per controller.** The seat is a lease held through the CLI (`adc seat take|release|status`).
  Only the seat holder performs supervisor write actions. This avoids two supervisors deciding the same thing ("one
  decision, one place").
- **Taking over.** When the developer starts work, their attended agent takes the seat:
  - the unattended supervisor is notified;
  - it finishes or parks its current action;
  - it writes a handover note;
  - it switches to read-only observation.
- **Handing back.** When the developer leaves, the attended agent writes a handover note and releases the seat. The
  unattended supervisor resumes.

  This replaces "foreground the background agent". The developer may still inspect it directly.
- **Continuity lives in auto-dev-cycle, not in an agent's memory.** Supervisor notes, open threads, handover notes and
  pending packets are stored by the controller (`adc notes …`). Any agent (Claude, Codex, another harness) can take
  the seat and continue. The death of a session loses nothing (nöbetçi lesson: a supervisor session death killed a
  pending rollout).
- **Waking up.** `adc events --follow --json` is a blocking event stream (holds, packets answered, seat requests,
  digest due). A backgrounded agent waits on it instead of polling or re-arming monitors. This fixes nöbetçi's "the
  monitor must never close" problem structurally.

---

## 5a. Classifiers in supervisor work (D34)

The unattended supervisor may call the classifier port where a structural, clear-cut decision helps. Candidates:
- triaging a hold into J1 categories;
- whether a decision packet is needed;
- which digest section an event belongs to.

These are classifier surfaces like any other (plan §2.8): they start in **shadow mode**, are evaluated against the
supervisor's own choices, and are promoted only by an approved record citing an eval report. They never grant
authority or confirm a proposal.

## 6. What v1 ships for the role

- This role definition as `docs/operating/supervisor.md`, written for the agent as much as for the human.
- CLI support:
  - `adc seat`;
  - `adc events --follow`;
  - `adc holds explain <id>` (evidence + resume signal + options);
  - `adc packet create|list|answer`;
  - `adc digest`;
  - `adc notes`;
  - every command with `--json`.
- Token kinds (`operator`, `supervisor`) with grants and per-action confirmation policies; passkey confirmation on the
  dashboard (the only human-confirmation channel, D37); the pager sends a notification with a link.
- **A supervisor kit:** instruction templates for Claude Code (`CLAUDE.md`) and Codex (`AGENTS.md`) that load this
  role, and a reference systemd unit for running an unattended supervisor under its own OS user.
- **Exit check (in M5 / M6):**
  - an unattended supervisor resolves a seeded human-signal hold within its grant;
  - it produces a packet for an out-of-grant action, which a human confirms with a passkey;
  - it hands the seat to an attended agent and back with notes;
  - it writes a digest whose every number is traceable to a record.
