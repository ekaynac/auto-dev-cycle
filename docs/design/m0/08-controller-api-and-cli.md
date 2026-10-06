# 08 — Controller API and CLI

Implements plan §2.11, D23 and M0 review [M5][M13], Q3. One controller API; the CLI and the dashboard are both clients;
the CLI is a superset.

## Transport [Q3]

| Client | Transport | Auth |
|---|---|---|
| Local CLI, local operator agent | HTTP/1.1 + JSON over a Unix socket `<state_dir>/adcd.sock`, **owner-only (0600) by default** | bearer token |
| Local supervisor under another OS user (the recommended isolation, D31) | a **separate** socket `<run_dir>/adcd-shared.sock`, enabled explicitly, group-accessible, outside the private state directory | bearer token (a supervisor token) |
| Dashboard, remote CLI | HTTPS on a configured TCP address | browser: session cookie (`Secure`, `HttpOnly`, `SameSite=Strict`) + CSRF; CLI: bearer token over **verified TLS** (pinned controller certificate or a CA the operator configured; no insecure mode) |
| adc-agent (M3) | outbound WebSocket over TLS from the agent | agent identity credential (M3) |

`schemas/openapi.yaml` is the single API description. Bodies are `$ref`s into `schemas/`.

## Reads, commands, events

- **Reads** are resources (`GET /v1/projects`, `/v1/tasks/{id}`, `/v1/holds`, …). They are authorized server-side
  against the actor's read scope (04). Blobs are served only through a referencing record the actor may read.
- **Commands:** `POST /v1/commands/{action_kind}`, one per action in the registry. The body carries
  `idempotency_key`, `expected` (object version or digest), `scope` and `reason`. The response is a discriminated
  union [M13]:
  - `{disposition: "applied", approval|event, effects: [{id, state}]}`
  - `{disposition: "proposed", proposal, policy, binding_digest}`
  - `{disposition: "refused", outcome, reason, detail}`
- **Events:** `GET /v1/events?after=<seq>&kinds=…&project=…` as Server-Sent Events, resumable by `seq`, filtered
  server-side by read scope. The stream is closed with `failed/authority_revoked` when the actor's grant changes in a
  way that removes access [M5]. `adc events --follow --json` is this stream.
  - **Read scope.** An event's resource kind, matched against a grant's `reads.kinds`, is its kind's first segment
    (`lease.granted/v1` is `lease`); an event outside any project is read only under an every-project read. Empty
    kinds or projects read nothing.
  - **Re-check.** M0 re-checks on a timed poll, at most one second apart, in the same read snapshot as the events it
    loads, and again before each frame it sends; each check is due one bound after the previous one, whatever writing
    took. Every frame is written and flushed under a deadline at the next due check, cleared once the frame is out
    (an idle stream holds no deadline), so a client that stalls longer is cut off and no frame is sent after a change
    of authority. On HTTP/1.1 the cutoff closes the connection hard, beneath TLS (servers register connections with
    `api.ConnContext`); on HTTP/2 it resets only that stream. The client resumes by `seq`. A store that cannot be read closes the stream with
    `could_not_run/store_unreadable`, never as a revocation. The stream closes with `failed/authority_revoked` when any grant it opened under is revoked, expires or is
    narrowed (a narrowing is a revocation and a new grant), and when the caller's token or actor is revoked. A
    supervisor with no grant in force is refused with `failed/action_not_granted`; a read-specific reason may follow.
  - **No credentials.** The stream never carries credential material. Each streamed kind has an explicit allow-list
    of payload paths (`internal/api`); a kind without one streams with its payload withheld.

## CLI rules

- `adc <noun> <verb>`. Families: `project`, `task`, `holds`, `approve`, `propose`, `grant`, `seat`, `packet`,
  `digest`, `notes`, `events`, `sources`, `doctor`, `admin`, `dev`.
- Every command supports `--json` (stdout = one JSON document or NDJSON for streams; logs on stderr).
- **One exit-code table** [M13]:

  | Exit | Meaning |
  |---|---|
  | 0 | command applied, or a read succeeded (result outcome `found` or `empty`) |
  | 1 | refused, or result outcome `failed` |
  | 2 | `could_not_run` (controller or dependency could not run it) |
  | 3 | proposed: waiting for confirmation |
  | 4 | stale: `stale_expected_state`, `stale_attempt`, `idempotency_key_reused` |
  | 64 | usage error (unknown flag, bad arguments) |

- **Explicit identity for approvals** [M13]. Approving, confirming or adopting anything requires `--expect
  <version|digest>` naming the exact revision the caller inspected. In `--json` mode it is mandatory, never
  auto-fetched. Interactive mode may fetch and display the current revision and ask the human to confirm it, and then
  sends that exact value.
- `human`-policy confirmations cannot be completed from the CLI. The CLI prints the proposal, its effective change and
  the dashboard link.
- Unknown flags are refused.

## Dashboard parity (D23)

- The dashboard is a static TS app served by `adcd`, using the generated API client.
- **Parity test:** for every action in `actions.yaml`, there is a CLI command and a dashboard control calling the same
  endpoint. The test enumerates the registry and fails on a missing mapping. The CLI may have extra dev/admin commands;
  the dashboard may not have actions the CLI lacks. The one asymmetry is WebAuthn confirmation, which exists only on
  the dashboard by design (D37).

## Supervisor seat (02-supervisor-role §5)

- `adc seat take [--force-after <duration>] | release | status`.
- Supervisor write actions require holding the seat (checked in the writer transaction). Operator actions do not. A
  conflicting supervisor write is refused with `failed/seat_not_held`.
- Taking a held seat emits `seat.handover_requested` to the holder's stream. After `force-after` the request wins.

## What M0 delivers

The OpenAPI document for the M0 resources and commands (projects, authority, holds, events, seat, notes), the two
sockets, the CLI skeleton with `--json`, the exit-code table and `--expect` rules, server-side read authorization, the
SSE stream with revocation, and the parity harness (dashboard side stubbed until M5).
