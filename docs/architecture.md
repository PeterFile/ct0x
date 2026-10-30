# Local group collaboration

The product is a local, single-user collaboration workspace. An empty group can begin a discussion without a profile, task, Git repository, or external working directory. The backend is one Go process with SQLite. The web client is React with TypeScript and Vite, served locally by the backend in the packaged build.

## Boundaries

- `internal/domain`: shared value types and explicit states.
- `internal/store`: SQLite transactions, migrations, indexed bounded reads, identity and resource constraints.
- `internal/provider`: CLI discovery, capability contracts, native/one-shot transport, execution receipts. No group-specific routing.
- `internal/platform`: shared operations, actor binding, discussion routing, task lifecycle, artifacts, skills, and context.
- `internal/scheduler`: durable claims, fair inbox/work scheduling, cancellation, and recovery.
- `internal/httpapi`, `cmd/workspace`: loopback HTTP, SSE, process lifecycle, and platform CLI entry.
- `web`: design tokens, accessible interaction, and server-backed views.

## Stable invariants

Group membership and lifecycle ownership are distinct from the creator and working directory. Conversations belong to one group/member/topic. Ordinary discussion creates a Turn without a Task.

A message and its recipient deliveries commit in one transaction before notification. Repeating an idempotency key with the same payload returns the same message; a different payload conflicts. Recipient membership is checked within the transaction. Queued, accepted by the CLI, terminal turn, submitted task, and accepted result are different states.

Only one active turn holds a conversation. Shared write resources are exclusive. A cancelling or unknown turn retains its resource claims until a trustworthy terminal or reconciliation result. A restarted process does not repeat an uncertain external start. Generation or claim tokens fence stale acknowledgements.

Local user operations and member tools call the same platform methods. A member's actor is bound to the runtime session, never read from arbitrary request payloads. Cross-group history, artifacts, skills, and resources require explicit authorization.

The UI does not interpret `installed` as `authenticated`, configuration presence as account proof, or process exit success as accepted deliverable. Capability flags default to unsupported. Every probe records timestamp, type, and sanitized evidence. Discovery has timeouts and never invokes a paid model request.

## HTTP contract, version 1

Use `/api/v1`, JSON snake_case, opaque string IDs, UTC RFC3339 timestamps, and errors shaped as `{ "error": { "code": "conflict", "message": "..." } }`. Every list responds with `{ "items": [], "next_cursor": "" }`; arrays are never null, and an empty cursor means the end. Bound all list queries with `limit` and an opaque `before` cursor (newest first, stable ordering by timestamp and ID); history increments may additionally accept `after`. Default limit is 50 and maximum 200. A mutation returns the created/updated object directly; creation uses 201. Mutation requests can supply `Idempotency-Key`; reusing a key with different input returns 409. Invalid inputs return 400 and missing or inaccessible records return 404.

The default local address is `127.0.0.1:7310`, configurable by an explicit CLI flag. The frontend uses relative URLs and the development proxy targets this loopback port. Enforce same-origin browser mutations and JSON request bodies; do not expose wildcard CORS or bind to all interfaces by default. A local member tool transport needs a run-bound capability and must not inherit the user HTTP actor.

Initial endpoints:

| Route | Operations |
| --- | --- |
| `/healthz` | GET process readiness |
| `/api/v1/groups` | GET list, POST `{name, description?}` |
| `/api/v1/groups/{group}/members` | GET list, POST `{name, role, provider_id?, model?, workspace?}` |
| `/api/v1/groups/{group}/messages` | GET bounded history, POST `{content, recipient_ids?, topic_id?}` |
| `/api/v1/groups/{group}/tasks` | GET, POST `{title, description, acceptance, assignee_id?, dependency_ids?}` |
| `/api/v1/groups/{group}/artifacts` | GET versioned outputs, POST publish |
| `/api/v1/groups/{group}/context` | GET knowledge and summaries |
| `/api/v1/groups/{group}/history?q=...` | GET scoped search |
| `/api/v1/providers` | GET independent installation/account/connectivity/capability states |
| `/api/v1/skills` | GET metadata, explicit load reads body |
| `/api/v1/events` | GET SSE notifications; reconnect always re-reads durable state |

Initial Group fields: `id, name, description, created_at`. Member fields: `id, group_id, name, role, provider_id, model, creator_id, owner_id, status, workspace, created_at`. Message fields: `id, group_id, topic_id, sender_id, content, created_at, deliveries`. The local human sender is `user`; member messages use the member ID. An empty topic denotes the main channel. Delivery fields: `id, message_id, conversation_id, recipient_id, state, turn_id, error, created_at`. Provider fields: `id, name, path, version, installation, account, connectivity, capabilities, evidence`.

The first provider implementation targets installed Pi and Codex. Capability tiers remain truthful: richer native protocols are separate slices, and PTY-only integrations do not claim machine collaboration. Live provider checks are recorded separately from fixture tests.

## Visual language

Reference inspected on 2026-10-30: [claude.dev](https://claude.dev/). Borrow its restrained charcoal canvas, warm off-white text, vermilion highlights, monospaced navigation, thin rules, and generous editorial spacing. Create original product identity; do not copy logos or illustrations.

Central CSS tokens own all colors, typography, spacing, borders, focus, status, and motion. Initial direction: canvas `#131412`, panel `#1b1c19`, text `#eeede6`, muted `#a5a69c`, line `#34362f`, accent `#dc7958`, success `#9eaf87`. Use a serif display accent, a readable sans UI stack, and a monospace metadata stack with local fallbacks. Support keyboard navigation, visible focus, reduced motion, and a usable narrow layout.

The primary screen puts groups on the left, discussion in the center, and members/activity on the right. Tasks, artifacts, skills, and connections are real server-backed destinations. Empty, loading, disconnected, error, queued, and unknown states must be explicit. Never show fabricated activity or pretend a button succeeded.

## Verified design sources

- [OpenBot channel context](https://github.com/nightly-labs/openbot/blob/f3e3f8ade9c146e96d9d68d65770d2a9126a7856/docs/architecture/channels-and-messaging.md): shared channel history and independent member sessions.
- [Orca durable mailbox](https://github.com/stablyai/orca/blob/9470b4618c7a494254ab9e1ec81c52bd5994256b/src/main/runtime/orchestration/db/messages/role-mailbox-delivery.ts): FIFO claims, replay, idempotent acknowledgement, and consumer-generation fencing. Atomic message-plus-recipient creation is our PRD requirement, not a claim inferred from this file.
- [Paseo](https://github.com/getpaseo/paseo): adapter-owned runtime and uncertain-start reconciliation; exact contracts are checked against the installed/source version before use.
- [Agent Skills specification](https://agentskills.io/specification): metadata first, selected body next, supporting resources on demand.
- [Go database/sql](https://go.dev/doc/database/execute-transactions): transactions own one connection; all operations inside a transaction use that transaction.

## Delivery and regression gates

Deliver independently reviewable contracts, storage, provider discovery, execution, UI, scheduler, resources, context, and acceptance slices. Aim for 300–500 effective lines per review; record justified exceptions. Each slice needs behavior-specific tests, independent review, and a rollback commit. No remote PR exists until a remote is selected.

Required validation grows with the slice: Go tests and race detector, web type checks/unit tests/build, database reopen/recovery tests, real browser flows, and two real CLI executions. Benchmark durable message insertion and bounded history retrieval before optimization. Store raw evidence locally; do not replace evidence with successful mocks.
