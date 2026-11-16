# Conversation-led orchestration

[PRD.md](../PRD.md) is the product authority for the accepted `Workspace → Project → Channel` target. This document retains the coordination direction selected on 2026-11-01 and defines its compatibility migration. Current runtime and persistence remain `Group/Conversation`, one Go process, SQLite, and the existing CLI adapters.

Coordination storage and role-scoped primitives already exist, including local Source/admission and group-bound coordinator configuration. The private intent pass remains unwired; production coordinator/worker composition and Project migration are not enabled. Inspect is diagnostic, not execution authorization. The flows and interfaces below are target contracts unless explicitly described as current.

The principal reference is [dsh-superagent-prototype at 6337fc8](https://github.com/deepseek-ai/dsh-superagent-prototype/tree/6337fc823ce20251c8de8085948165c81aaa701e). Its implementation provides a domain Engine, versioned admission policy, durable effects, persistent worker inputs, and source-specific replies. Its underlying Sessions, Inbox, Jobs, and Subagents are services of DeepSeek Harness; this repository does not independently implement that runtime. Our Go application adapts the orchestration responsibilities instead of embedding the DSH plugin.

## Product and execution flow

The conversation is the primary interface. Ordinary discussion can produce an Agent Turn without creating a Task. A request becomes authorized work only through a recorded admission decision. Follow-ups for existing work stay with that work unless its executor hands them back.

```mermaid
flowchart TD
    IM[Local IM Source] --> Intake[Engine: receive and classify]
    Intake --> Discussion[Ordinary group discussion]
    Intake --> Follow[Existing work's subscribed inputs]
    Intake --> Gate[Deterministic Policy gate]
    Gate --> Assess[Coordinator assessment]
    Assess --> Decision[RoutingDecision and immutable Mandate]
    Decision --> Task[Authorized Task and Assignment]
    Follow --> Task
    Task --> Intent[Durable execution or notice intent]
    Discussion --> Queue[Existing persistent delivery queue]
    Intent --> Queue
    Queue --> Runtime[Existing claim, Prepare and provider execution]
    Runtime --> Receipt[Trusted execution receipt]
    Receipt --> Report[Worker report or question]
    Report --> Engine[Engine: apply report and route attention]
    Engine --> Outbox[Reply intent and receipt]
    Outbox --> IM
    Report --> Submission[Immutable Submission]
    Submission --> Verify[Independent verification evidence]
    Verify --> Review[Separate local-user ReviewDecision]
```

The initial Source is the local IM. **Reply destination** means the authorized response destination, called Channel in the pinned DSH reference; it is not the PRD's Project Channel. Local replies use the existing group/topic scope; Project Channel mapping is not yet implemented.

GitHub and Feishu adapters may later implement the same contracts; adding their descriptions or UI labels does not enable them or authorize external messages.

## Domain mapping

| Reference concept | Go application meaning | Required distinction |
| --- | --- | --- |
| Source / Trigger | A source-owned immutable event and its verified metadata; local message events reference the existing Message | A Trigger can also be a command or system fact. Do not duplicate message text or infer identity from its body. |
| Thread | Shared topic within a target Project Channel; legacy identity is group/topic | Existing Conversation is private to one group/member/topic. It is not the shared Thread. |
| Channel (DSH reference) | Reply destination for an authorized response | Not the target Project Channel, a Pi/Codex provider, a model, or a Session. |
| Policy / Eval | Deterministic eligibility followed by a version-bound assessment of one Thread batch | Model judgment cannot bypass the gate, choose the caller, or grant permissions. |
| Decision / Mandate | RoutingDecision plus immutable authority, origin, scope, strength, and policy revision | RoutingDecision is not the existing deliverable ReviewDecision. Scope text is not a filesystem grant. |
| Ticket | An existing Task with explicit coordination and Mandate records | An ordinary or manually recorded Task does not automatically authorize execution. Do not create a second unrelated task system. |
| Assignment | A durable executor binding for authorized work, independent of physical Turns | Current TaskAttempt is an immutable generation/evidence association, often bound to one Turn. It must not silently become a multi-turn Assignment. |
| Subscription / Input | Work-to-Thread follow-up ownership and durable inputs | A subscription supplies context, not wider authority. Delivery remains the physical member-conversation delivery record. |
| Report / Attention | Structured progress, outcome, or one concrete question | A report of readiness creates or points to a Submission; it never accepts its own result. |
| Effect / Outbox | A durable intent and its independently tracked reply receipt | An intent is not proof of execution or external delivery. |

During compatibility, coordinator bindings and execution conversations remain group-isolated. A coordinator is a server-bound role, not a member name or free-form responsibility string. This adapts the reference's one-repository main Session to multiple isolated groups. Worker roles remain bound to their Assignment and live execution scope.

An Assignment may eventually span multiple Turns. That requires an explicit, reviewed association and generation check. Existing same-Turn submission fences continue to apply until that association is implemented; no adapter may weaken them merely to obtain continuity.

## Modules and their interfaces

Runtime is the composition root. It owns the process lock, Store, private roots, adapters, HTTP, startup, and ordered shutdown. It contains no admission rules.

Engine is the only writer of the new coordination aggregates. Its small role-scoped interfaces receive commands and adapter facts. Intake, admission, follow-up ownership, work transitions, and replies are internal parts of the same module, rather than parallel orchestration services.

| Caller | Interface responsibilities |
| --- | --- |
| Verified Source | Receive a source event, associate its Threads, report an external close |
| Local user | Explicitly request or attach work, stop authorized work, resolve an attention request, review a concrete Submission |
| Bound coordinator | Assess a current candidate, record a permitted decision, route work or a follow-up, send an allowed reply |
| Bound worker | Report on its Assignment, acknowledge only evidenced inputs, submit a deliverable, request help, hand back an unrelated follow-up |
| Runtime adapter | Report start, input delivery, terminal receipts, and reconciliation facts for its bound execution generation |

The Store supplies a transactional seam through which one Engine command changes all affected records and queues its intents. Extract and reuse existing in-transaction helpers; do not call public Store operations that open a second transaction from inside an Engine transaction. File copies, model calls, native process launches, and network writes happen after commit.

Policy is deterministic and versioned. Assessment proposes a decision under a captured Policy revision and candidate generation. A stale result cannot accept work after either changes. Explicit local-user requests are supported first. Inferred work begins in observation mode and requires an enabled written rule before automatic execution.

A trusted Policy change invalidates stale evaluations and notifies the work it affects. An ended Task's coordination may resume only after its original Mandate is revalidated against the current Policy. A human stop persistently withdraws that Mandate: ordinary follow-up, attach, or retry cannot restore it; newly authorized work requires a new decision and Mandate. This authorization transition does not release an uncertain native Turn's resource claims.

Assessor and Executor are different responsibilities. The assessor decides whether and where work belongs. The executor uses the existing scheduler, claim fencing, provider adapters, and receipts to perform it. There is one persistent execution queue, not an additional scheduler hidden behind Engine.

## Persistence and recovery contracts

- Source event identity is stable and idempotent. Replaying an accepted event cannot create another Task, Mandate, Assignment, or acknowledgement.
- A Thread has at most one current evaluation generation. Events that arrive while assessment runs remain available for the next batch.
- Domain changes and their durable intents commit together. Workers run intents after commit with bounded scans and backoff. A generation comparison prevents an old completion from deleting a newer payload under the same effect key.
- Effects are at-least-once scheduling. Local replies reuse the existing message transaction and idempotency receipt. Any external reply destination must supply its own idempotency or reconciliation behavior; the queue alone cannot promise external exactly-once delivery.
- Existing deliveries remain the source of execution scheduling. Effects may create a stable delivery or a coordinator notice; they must not launch a second CLI for an already claimed or uncertain delivery.
- Input queued, adapter accepted, demonstrably consumed, processed, and task accepted are distinct facts. Pi/Codex paths without trustworthy native-history evidence must not manufacture an input read receipt.
- Human follow-up ownership is explicit. Context-only subscriptions neither widen a Mandate nor resume stopped work. A worker can hand back a message, but another owner must not lose it as a side effect.
- A structured question is a single attention record with a stable identity. Routine progress updates do not wake every member or append repetitive IM notifications.

All migrations remain append-only. Existing durable messages, idempotency receipts, Task versions, artifact references, and claim evidence are retained. Timestamp/ID cursors remain display pagination; a durable message sequence, where available, is the basis for commit-position processing. No cursor is silently reinterpreted as another kind of acknowledgement.

## Project migration and rollback

Implement the [ownership contract](architecture.md#project-compatibility-migration) in separately reviewable slices. Each slice must close historical backfill and every corresponding new-write path transactionally before dependent APIs or UI are enabled. New and legacy entry points share transaction helpers and one authorization/idempotency authority, not a background synchronizer or a second Engine, Store, or queue.

| Slice | Required closure |
| --- | --- |
| Group ownership | Backfill each Group into its distinct Project and Channel in the default Workspace. Legacy `CreateGroup` must create its Project and Channel in the same Group transaction; identical key/payload replay must reuse the saved mapping. Historical backfill alone is insufficient. |
| Task ownership | Later, backfill Project and origin-Channel associations and maintain them in every Task insertion transaction, including RequestWork. Preserve legacy group/version/provenance authority; explicit Thread links grant no access. |
| Member ownership | Later, map each old member independently and maintain every member-creation path in the same transaction. Do not merge same-name/provider members or enable cross-Channel instance reuse. |
| Session compatibility | Later, backfill immutable explicit 1:1 Conversation-to-Session bindings and maintain every Conversation insertion in the same transaction. Preserve group/member/topic, delivery matching, uniqueness, and concurrency. This is neither a final product 1:1 rule nor evidence of native resume. |

Mappings and migration version must commit atomically. Missing or inconsistent mappings fail closed; reads must not silently repair them, and background backfill must not expose partial ownership. Channel-independent Project Tasks require a later compatibility slice; do not simulate them with hidden Channels or Groups.

Apply append-only, versioned SQLite migrations under the existing startup lock, before scheduler activity or new business writes. Do not rewrite old records or published migrations, rename/rebuild legacy tables, or weaken foreign keys, grants, claim, or provenance constraints. The existing schema gate makes an old binary reject a newer schema; API compatibility does not imply binary downgrade compatibility.

Before migration, follow existing drain/receipt rules and take a consistent SQLite backup accounting for WAL. Record schema version, database hash, exact binary/source version and binary hash, and runtime/external execution state. Preserve unknown executions and claims.

Restore that snapshot with its old binary only if no business writes or external effects occurred after the snapshot; retain the failed/new database as evidence. Otherwise preserve all subsequent facts and forward-repair/reconcile. Never erase unknown outcomes or claims, or force unsafe replay. There is no automatic downmigration; never delete migration rows to bypass the schema gate.

Implementation acceptance covers empty and historical databases, atomic migration failure, reopen, idempotent replay and new-write closure, isolation including two same-name old groups, unknown/cancellation protection, Session exclusivity, resource locks, and late-receipt generation fences.

Before altering established performance paths, record a comparable baseline for the exact base, environment, and data scale. Assess affected migration and creation costs; preserve bounded/indexed reads, transaction integrity, and unchanged hot paths where possible. Keep performance comparisons scoped to affected paths.

## Safety and compatibility retained from this application

The reference is research code. Its product-specific choices do not replace these established contracts:

1. A business stop requests cancellation; it does not prove that a native process or background operation stopped. Unknown and cancel-requested executions retain capacity and resource claims until an authoritative terminal or reconciliation receipt.
2. Restart does not blindly create a replacement execution for an uncertain start. A new Assignment or a recovery prompt cannot erase old external uncertainty.
3. Task submission and acceptance remain separate. Worker readiness, a merged external Thread, native exit zero, or an ended Assignment cannot fabricate a local-user ReviewDecision.
4. Group isolation, runtime-bound tool identity, same-transaction authorization fences, and explicit resource grants remain mandatory. Prompt scope is explanatory text, not an operating-system sandbox.
5. CLI credentials remain with their original CLI. Installation, account state, connectivity, and supported capabilities remain independent evidence. A stable platform Session identifier does not imply native resume, steering, or compaction support.
6. Bounded event/frame queues, SQL range scans, shutdown drain, and the existing skills allocation regression gate are preserved. No per-native-event durable-effect fan-out or unbounded history scan is introduced.

In particular, do not adopt the reference's timeout-based worker-slot release while background subagents may still run, or its automatic restart of interrupted work, as substitutes for this application's receipts.

## IM interaction contract

The UI uses a conversation list, a central chronological message timeline, and a composer that remains reachable. Members, Tasks, deliverables, search, and conversation details are contextual panels or drawers. They do not replace the conversation with a dashboard or a collection of administrative pages.

Use the existing charcoal, warm-white, and vermilion tokens. Display the local author as the user and a member by its real known identity. Put technical identifiers and detailed execution evidence in expandable details. Keep source identity separate from the selected Agent provider.

Panel or conversation changes must preserve pending and uncertain mutations, their original input, and their idempotency keys. Hidden, unused reads may stop; shared roster data still needed by the active composer must remain available. Closing a panel must not cancel a submitted write or change the chosen recipients.

Message visibility and wakeups stay separate: default routing, explicitly public-only, and explicitly selected recipients retain their existing meanings. Textual mentions do not silently alter delivery. Receipts distinguish saved, accepted by the CLI, terminal, submitted, and accepted result; no unproven read or typing indicator is shown.

Task and attention cards require durable typed references and real actions. Their immutable event history is separate from any clearly labeled current-state projection. Until the backend supplies those references, show the actual Task in a contextual panel rather than inventing an event in the chat.

Unread counts, last-message previews, coordinator badges, external-source labels, and activity indicators require authoritative data. Responsive IM navigation must preserve drafts, scroll position where possible, keyboard focus, and unknown-result recovery across conversation list, chat, and details views.

## Incremental acceptance

Completing the existing Group workflow establishes a reusable execution baseline, not Project compatibility or full PRD acceptance.

1. Retain the IM shell on the existing working message flow, with desktop, short-window, narrow-screen, keyboard, and delayed-response validation.
2. Complete trusted group-scoped execution and coordinator/worker composition over the implemented admission and coordination primitives, using the existing queue and receipts. Explicit work admission remains the first executable path.
3. Verify real local CLI coordinator and worker collaboration, including follow-up during execution, structured reports, an attention question, an actual versioned Submission, independent verification, and a separate local-user ReviewDecision. Include controlled restart and unknown/cancellation/resource-claim protection.
4. Verify each Project/Channel and later Task/member/Session compatibility slice's integrity, isolation, and recovery before exposing dependent product operations. Hierarchy labels alone do not satisfy migration or real product acceptance.
5. Extend provider continuity and external Sources one adapter at a time, only after their actual capabilities and authority are verified.

Each slice is reviewed independently and tested through its module interface. A UI mockup, fixture, CI result, adapter smoke test, or merge alone does not establish real CLI collaboration or full product acceptance.

## Pinned design provenance

- [Domain model and rejected concepts](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/6337fc823ce20251c8de8085948165c81aaa701e/docs/001-domain.md)
- [Engine and role interfaces](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/6337fc823ce20251c8de8085948165c81aaa701e/src/core/engine.ts)
- [Transactional intake](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/6337fc823ce20251c8de8085948165c81aaa701e/src/core/intake.ts)
- [Effects and generation fencing](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/6337fc823ce20251c8de8085948165c81aaa701e/src/app/effects.ts)
- [Outbox receipts](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/6337fc823ce20251c8de8085948165c81aaa701e/src/app/outbox.ts)
- [Persistent Session input delivery](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/6337fc823ce20251c8de8085948165c81aaa701e/src/app/delivery.ts)
- [Executor and background lifecycle](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/6337fc823ce20251c8de8085948165c81aaa701e/src/executor/dsh.ts)
- [Low-attention philosophy](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/6337fc823ce20251c8de8085948165c81aaa701e/docs/000-philosophy.md)
- [The reference supervision panel](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/6337fc823ce20251c8de8085948165c81aaa701e/docs/005-panel.md)

The reference is MIT licensed. Preserve its license and attribution when adapting implementation code. Source-specific defaults, account identities, repository paths, and deployment policy are not copied into this application.
