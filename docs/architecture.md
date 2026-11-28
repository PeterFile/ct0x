# Target architecture: Project and Channel collaboration

> **Status & Dual-State Notice**: This document defines the **sole target engineering contract** for all subsequent development work under the converged 39-table `Workspace -> Project -> Channel` architecture (frozen decisions in [ADR-0001](adr/0001-architecture-convergence.md); phased execution plan in [architecture-convergence-roadmap.md](plans/architecture-convergence-roadmap.md)). On current `origin/master` (`0188361`), running runtime code remains the legacy `Group/Conversation` implementation until architecture convergence merges into `master`.

The product is a local, single-user collaboration workspace for Darwin and Linux. [PRD.md](../PRD.md) is the product authority. The converged domain hierarchy is `Workspace -> Project -> Channel`.

An empty Channel can begin a discussion without a profile, Task, Git repository, or external working directory. The backend is one local Go process with embedded SQLite and no microservices or external queues. The web client is React with TypeScript and Vite, served locally by the backend in the packaged build via the single [Openbot-derived shell](openbot-shell.md).

The coordination model adapts the pinned dsh-superagent-prototype architecture. [Conversation-led orchestration](orchestration.md) defines the domain mapping, Engine boundaries, two-tier crash reconciliation, and atomic schema migration rules.

## Boundaries

- `internal/domain`: shared value types, explicit states (`open`/`handover_pending`/`submitted`/`accepted`/`cancelled` Tasks, `unknown` Turns, `handover` Decisions), and zero-dual-track domain models.
- `internal/store`: SQLite WAL writer/reader pools, 39-table `target_v1` schema (`user_version=100`), atomic `v48`-to-`target_v1` migration, indexed bounded reads, and single-pass `BEGIN IMMEDIATE` write transactions.
- `internal/provider`: zero-paid-probe CLI discovery (`Pi >= 1.5.2`, `Codex >= 0.172.0` by protocol capability), per-run isolated `0700` `PI_CODING_AGENT_DIR` handling, embedded 24-LOC `pi_thin_mcp.mjs`, AppServer transport, and process-group/boot-id anti-PID-reuse evidence.
- `internal/platform`: shared platform operations, server-bound actor/project/channel identity validation (`404`/`403`/`409`), discussion routing, three-tier Project policy templates with Channel/Thread overrides, task lifecycle, artifacts, skill snapshots, and bounded context assembly.
- `internal/coordination`: unified transactional Engine for source intake, versioned Policy admission, immutable Mandates, Assignment handover, follow-up ownership, and structured attention without ephemeral scratchpad tables or process-level feature flags.
- `internal/scheduler`: durable delivery/wakeup separation, fair inbox/work scheduling, two-layer concurrency locking (DB partial unique active-turn indexes + exclusive write `resource_claims`), cancellation, and crash recovery.
- `internal/httpapi`, `cmd/workspace`: loopback-only HTTP/SSE server (`127.0.0.1:7310` default), single Session-Bound Loopback Streamable HTTP MCP endpoint (`POST /api/v1/member-tools/mcp`), two-tier crash reconciliation endpoint, and converged 8-flag default startup profile.
- `web`: single-shell `OpenbotShell` (`ProjectChannelNav`, `ChannelPolicyPanel`, `Discussion`, `Members`, `PendingDecisions`, `Tasks`, `UnknownReconcilePanel`), central CSS tokens, and accessible server-backed panels.

## Storage, schema, and concurrency

SQLite operates in `WAL` mode with `synchronous=FULL` (with `--sqlite-sync=full|normal` escape hatch defaulting to `full`), `foreign_keys=1`, `busy_timeout=5000`, `wal_autocheckpoint=1000`, `_txlock=immediate`, writer pool `MaxOpenConns=1`, separate read-only pool (`mode=ro`, `query_only=1`, `MaxOpenConns=4`), and `PRAGMA wal_checkpoint(TRUNCATE)` on clean shutdown.

The converged `target_v1` (`user_version=100`, revision `r2`) schema contains **39 strict tables**, **9 partial unique structural guard indexes**, **27 targeted covering indexes** (49 auto + 36 explicit = 85 indexes total), **10 single-table immutability/no-unrevoke triggers**, **0 multi-table JOIN triggers**, and **0 views**. It eliminates all dual-track shadow tables and cross-table validation triggers, enforcing multi-entity invariants inside single-pass `BEGIN IMMEDIATE` Go transactions.

| Domain Group | Exact Target Tables (39) |
| --- | --- |
| Metadata & Top-Level Scopes (6) | `schema_meta`, `workspaces`, `projects`, `channels`, `channel_members`, `channel_policies` |
| Explicit Resource & Artifact Grants (2) | `workspace_grants`, `artifact_read_grants` |
| Threads, Messaging, Skills & Execution (10) | `threads`, `thread_state_revisions`, `messages`, `deliveries`, `wakeups`, `sessions`, `skill_snapshots`, `turns`, `turn_skill_bindings`, `run_explanations` |
| Tasks, Mandates, Dependencies & Collaboration (12) | `tasks`, `task_thread_links`, `mandates`, `task_dependencies`, `task_lineage`, `assignments`, `attempts`, `task_work_inputs`, `task_constraint_impacts`, `submissions`, `verifications`, `reviews` |
| Artifacts & Pending User Decisions (4) | `artifacts`, `artifact_versions`, `submission_artifacts`, `decisions` |
| Resource Locking, Ops, Experiences, Idempotency & Audit (5) | `resource_claims`, `workspace_operations`, `project_experiences`, `idempotency_keys`, `audit_events` |

Historical databases (`v1..v47` normalized to `v48` first, then `v48 -> target_v1`) migrate via a one-shot offline/startup atomic temp-file transaction gated by `PRAGMA integrity_check == ok`, `PRAGMA foreign_key_check == 0`, SHA-256 equivalence across all 37 business tables, and zero unaccounted non-empty source tables before `fsync` + atomic `rename`. See [ADR-0001](adr/0001-architecture-convergence.md) for the full 100-source-table disposition audit.

## Stable invariants

`projects.root_path` is deliberately non-unique: multiple Projects may point to the same directory with isolated identities and independent grants. Opening a folder never grants filesystem permissions; `projects.default_policy_tier` defaults to `chat_only` (`read_only_collab` and `authorized_write` require explicit authorization, and Channel/Thread overrides live in versioned `channel_policies` with shadow preview approval for over-tier requests).

Channel membership (`channel_members`) and lifecycle ownership are distinct from creator and working directory. Threads (`(project_id, channel_id, topic_key)`) and Tasks have true many-to-many links (`task_thread_links`); ordinary discussion creates a Turn without a Task. When a temporary member leaves (`status = 'left'`), active `assignments` become `cancelled` (preserving history), open Tasks freeze in `handover_pending`, and a `decisions(kind='handover')` card enters the user's global pending queue with an optional one-click coordinator handover suggestion or task cancellation (revoking the Mandate non-restorably).

A message (with optional inline `reply_to_message_id` / `quote_excerpt`) and its recipient deliveries commit in one transaction before notification; `deliveries` and `wakeups` remain strictly decoupled. Repeating an idempotency key with the same payload returns the same result; a different payload conflicts (`409`). Queued, accepted by CLI, terminal turn, submitted task, independent `verifications` outcome, and local-user `reviews` acceptance are distinct facts.

Two-layer concurrency protection prevents data loss: partial unique indexes (`idx_turns_active_session` and `idx_turns_active_thread_member`) allow at most one active turn per session and per `(project_id, channel_id, thread_id, member_id)`, while mutable directory writes acquire exclusive `resource_claims` until terminal receipt or formal reconciliation (read-only operations on the same `cwd` run concurrently).

A cancelling or `unknown` turn retains its capacity slot and `resource_claims` until a trustworthy terminal or formal two-tier reconciliation receipt (`412` on `external_pgid`/`started_at`/`boot_id` mismatch; Tier 1 one-click `200` for pure-text/read-only crashes; Tier 2 `428` blocking one-click release until secondary SHA-256 preimage vs disk observation confirmation returns `200` and appends `audit_events`).

Local user operations and member tools share platform semantics, with runtime `claim_token` hard-binding caller identity, `project_id`, `channel_id`, and grants. Cross-project mismatches return `404`, cross-scope capabilities return `403`, and duplicate intra-project channel names return `409`.

## Unified Loopback MCP and provider execution

Pi (`>= 1.5.2`) and Codex (`>= 0.172.0`) connect through a single Session-Bound Loopback Streamable HTTP MCP endpoint (`2025-06-18`) at `POST /api/v1/member-tools/mcp`, eliminating legacy multi-extension TypeScript bridges and per-operation HTTP member endpoints.

- Go embeds a 24-LOC `pi_thin_mcp.mjs` shell via `//go:embed`, materializes it into a private `0700` directory with SHA-256 verification, and launches Pi with `-e builtin:mcp` (omitting `--no-mcp` so `registerMcpServer` stays active).
- Before each Pi run, Go creates an isolated `0700` `PI_CODING_AGENT_DIR` containing read-only symlinks to host `auth.json`, `models.json`, and `settings.json`, plus an empty `mcp.json` (`{"mcpServers":{}}`) so host MCP servers never spawn in the background.
- Forged scope arguments (`caller`, `project_id`, `channel_id`, `grants`, etc.) are rejected with JSON-RPC `-32602`; reconnect replays require explicit session-level `idempotency_key` with write-unknown circuit breaking.
- See [provider-compatibility.md](provider-compatibility.md) for capability discovery, credential boundaries, and out-of-scope CLI exclusions (experimental `agy` ACP is removed in Phase 0).

## Startup configuration

`cmd/workspace` converges from 24 experimental toggles to a single default startup profile with **8 operational flags** (runtime capability boundaries are governed by Project templates and `channel_policies`, not CLI feature flags):

| Flag | Default / Domain | Purpose |
| --- | --- | --- |
| `--listen` | `127.0.0.1:7310` | Loopback-only HTTP listen address |
| `--data-dir` | User config `agent-workspace` (`0700`) | Private SQLite and runtime artifact root |
| `--web-dir` | Embedded/dist default | Static web UI asset override |
| `--dev-origin` | Empty | Explicit local development CORS/origin allowance |
| `--max-active` | `4` (range `1..8`) | Global maximum concurrent active executions |
| `--sqlite-sync` | `full` (`full\|normal`) | SQLite `synchronous` mode escape hatch (echoed in `/healthz`) |
| `--pi-path` | `PATH` discovery | Optional explicit Pi executable path |
| `--codex-path` | `PATH` discovery | Optional explicit Codex executable path |

## HTTP contract, version 1

Use `/api/v1`, JSON snake_case, opaque string IDs, UTC RFC3339 timestamps, and errors shaped as `{ "error": { "code": "...", "message": "..." } }`. Lists return `{ "items": [], "next_cursor": "" }` bounded by `limit` (default 50, max 200) and opaque `before` cursors (`created_at, id` descending); mutations accept `Idempotency-Key` (`409` on payload mismatch). Enforce same-origin browser mutations and JSON bodies; never expose wildcard CORS or bind non-loopback by default.

Target routes (legacy Group routes are removed in the same Phase 4 cutover wave per Decision 4A):

| Domain | Target Routes |
| --- | --- |
| System & Discovery | `GET /healthz` (echoes active `sqlite_sync`), `GET /api/v1/providers`, `GET /api/v1/skills`, `GET /api/v1/events` |
| Projects & Channels | `GET/POST /api/v1/projects` (`default_policy_tier`), `GET /api/v1/projects/{project}`, `GET/POST /api/v1/projects/{project}/channels`, `GET /.../channels/{channel}`, `GET /.../channels/by-name/{name}` |
| Members, Threads, Messages & History | `GET/POST/DELETE /.../channels/{channel}/members`, `GET /.../threads`, `GET /.../threads/{thread}/state`, `GET/POST /.../messages` (`reply_to_message_id`), `GET /.../history?q=...` |
| Policy & Explicit Grants | `GET/PUT /.../channels/{channel}/policy`, `GET/POST/DELETE /.../channels/{channel}/workspace-grants`, `POST /.../channels/{channel}/artifact-read-grants` |
| Tasks, Dependencies, Verification & Artifacts | `GET/POST /.../channels/{channel}/tasks`, `POST /.../tasks/{task}/cancel`, `POST /.../tasks/{task}/dependencies/{dep}/rebind`, `GET/POST /.../tasks/{task}/{attempts,work-inputs,submissions,verifications,reviews}`, `GET/POST /.../artifacts`, `GET /.../artifacts/{artifact}/versions/{version}/content` |
| Cancellation & Tiered Reconciliation | `POST /api/v1/projects/{project}/attempts/{attempt}/cancel` (`202`, holds claims), `POST /api/v1/projects/{project}/attempts/{attempt}/reconcile` (`412` identity mismatch; Tier 1 `200`; Tier 2 `428` -> SHA-256 confirm `200`) |
| Project Experiences | `GET/POST /api/v1/projects/{project}/project-experiences`, `POST /.../project-experiences/{experience}/retire` |
| Personal Inbox & Run Observability | `GET /api/v1/me/pending-reviews`, `GET /api/v1/me/pending-decisions` (includes `handover`), `POST /api/v1/me/decisions/{decision}/answer`, `GET /api/v1/me/unknown-attempts`, `GET /api/v1/me/agent-runs`, `GET /api/v1/me/agent-runs/{run}` (timeline includes `dispatch_decision`, `submission`, and immutable `public_explanation` from `run_explanations`) |
| Unified Member Tools | `POST /api/v1/member-tools/mcp` (Loopback Streamable HTTP MCP `2025-06-18`) |

`GET /api/v1/me/agent-runs/{run}` returns `timeline.items` newest first by `(created_at, id)` descending; `public_explanation:<turn_id>` carries an optional immutable member-published text (non-blank UTF-8, `<= 1024` bytes, no NUL) stored in `run_explanations`, distinct from receipts, authorization, or chat messages. `task_dependencies` pins exact `(pinned_submission_id, pinned_artifact_id, pinned_version_id)` tuples that do not follow artifact heads and require explicit `local-user` approval (`rebind_decision_id`) to rebind.

## Visual language

The sole UI target is the single [Openbot-derived shell](openbot-shell.md) (Phase 4 retires legacy fallback layouts and unused reception defaults). It preserves PolyForm Noncommercial 1.0.0 attribution, NOTICE, and source mappings alongside the central CSS palette (`#131412` canvas, `#1b1c19` panel, `#eeede6` text, `#a5a69c` muted, `#34362f` line, `#dc7958` accent, `#9eaf87` success), keyboard navigation, visible focus, reduced motion, and narrow-screen usability.

The primary screen remains an IM with left navigation (`ProjectChannelNav`), central message timeline with selective quote/reply and composer (`Discussion`), and contextual server-backed panels (`ChannelPolicyPanel`, `Members`, `PendingDecisions`, `Tasks` with `Verification` and dependency rebind, and `UnknownReconcilePanel`). Never fabricate activity, read receipts, or unverified completion.

## Verified design sources

- [DSH Super-agent prototype, pinned architecture](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/6337fc823ce20251c8de8085948165c81aaa701e/docs/007-architecture.md): the primary coordination reference; adaptations and deliberate safety differences are recorded in [orchestration.md](orchestration.md).
- [OpenBot channel context](https://github.com/nightly-labs/openbot/blob/f3e3f8ade9c146e96d9d68d65770d2a9126a7856/docs/architecture/channels-and-messaging.md): shared channel history and independent member sessions.
- [Orca durable mailbox](https://github.com/stablyai/orca/blob/9470b4618c7a494254ab9e1ec81c52bd5994256b/src/main/runtime/orchestration/db/messages/role-mailbox-delivery.ts): FIFO claims, replay, idempotent acknowledgement, and consumer-generation fencing. Atomic message-plus-recipient creation is our PRD requirement, not a claim inferred from this file.
- [Paseo](https://github.com/getpaseo/paseo): adapter-owned runtime and uncertain-start reconciliation; exact contracts are checked against the installed/source version before use.
- [Agent Skills specification](https://agentskills.io/specification): metadata first, selected body next, supporting resources on demand.
- [Go database/sql](https://go.dev/doc/database/execute-transactions): transactions own one connection; all operations inside a transaction use that transaction.

## Delivery and regression gates

Independent code review applies to repository development. For a product Task, its acceptance agreement or a user-enabled Policy determines whether independent verification is required.

Deliver independently reviewable slices inside isolated worktrees under [architecture-convergence-roadmap.md](plans/architecture-convergence-roadmap.md), with behavior-specific tests, zero `SQLITE_BUSY` under `-race`, and rollback safety. Do not commit `.local/`, databases, credentials, or mocks pretending to be real CLI/browser acceptance. Keep raw evidence under `.local/` and satisfy all four release gates (lossless 5-sample migration, zero `SQLITE_BUSY` & DDL structural parity, per-package `-race` speedup calibrated against Phase 0 local baseline, and 6 real Playwright + dual CLI end-to-end loops on the default `127.0.0.1:7310` startup profile).
