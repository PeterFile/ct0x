# Experimental Pi whole-work return (A7)

This is the C native entry for the v31 A7/B composite, **not** an A/B migration, a new queue, or an ordinary Pi discussion-tool integration. It needs the corrected A/B schema guard before release. C1 supplies the ordinary structured discussion apply/read tools separately.

## Startup

Use a new private, non-symlink `0700` data directory and the explicitly selected Pi 1.4.0 installation. Keep each trusted extension beside its checked dependencies. Do not enable Codex AppServer.

```text
workspace --data-dir /absolute/new-private-data \
  --enable-execution --enable-member-tools --enable-coordination-roles \
  --pi-path /absolute/pi --pi-member-tools-extension /absolute/pi-bridge/index.ts \
  --enable-investigation-execution \
  --pi-fresh-investigation-work-extension /absolute/pi-bridge/fresh-investigation-work.ts \
  --enable-discussion-work-return \
  --pi-fresh-task-work-extension /absolute/pi-bridge/fresh-task-work.ts
```

The last flag enables normal Pi chat intake of a local-user supplement (`continue_work`); trusted fixtures can instead seed that assessment via B. Keep `--enable-investigation-owner-management` **off** for the handback-only experiment. Enabling it separately keeps the existing direct-local management profile but still does not add delegation or initial-owner management for structured origins. Native session resumption may be configured for otherwise eligible text turns, but does not include the typed return.

## Acceptance fixture

1. Configure a coordinator and eligible worker; select the same Pi installation and explicit models for participant members. The local user authorizes one scoped read-only investigation on one topic with the written investigation policy. Send a new local-user source; the first member uses a real structured invitation. Prefer two invitation hops with four distinct identities: original inviter, intermediate inviter (also the worker), receiving assessor, and coordinator. Ordinary member sends, structured replies, and approvals do not qualify as this slice's intake.
2. The exact receiving Turn admits `prepare_work` / `read_only_investigation` under the current written scope. Its persisted invitation (not the original user message) is the Mandate's origin. The original user source and same-source limits are independently checked in B. The scheduler runs the formal coordinator and first worker with their scoped reads and `decide_dispatch` or `submit_result`, respectively. Leave work open if incomplete; don't submit a placeholder result or delegate from the initial worker. Let the receiving and initial worker Turns settle with trusted native evidence.
3. The local user asks to continue the same Task (include its exact ID) with a verbatim supplemental constraint. On the real B followup, the current owner binds a new Attempt, reads the exact pending input, and may issue `handback_work` when the whole investigation cannot continue. `hand_back_work_input` only returns one input. The native HTTP response remains the existing B non-waking notice receipt; it doesn't assert a wake, resource release, assignment change, or consumption. On an unknown write result preserve the original key and reconcile, not automatically retry.
4. Check the separate A7 receipt and its one queued delivery to the original inviter, not the intermediate inviter or coordinator. Preparation requires the typed receipt's bound group, topic, message, sender, recipient, delivery and conversation plus the ordinary queued role. A damaged typed read is not an untyped message. No typed reply goes through intake, continuation, role tooling, or resumption. If this entry isn't opted in, the typed delivery stays queued; ordinary messages still take their usual path.
5. After the usual scheduler claim, the selected original inviter gets a bounded prompt containing the exact return, no history or tools, and explicit system prompts rather than ambient `SYSTEM.md` / `APPEND_SYSTEM.md`. The ordinary Pi adapter's accepted/terminal evidence and scheduler receipt must yield a terminal Turn with one public plain reply from that member to the same topic, without a new delivery or work effect. Storage of the return, a candidate response, or even a terminal without a persisted reply doesn't prove this acceptance case. An unknown execution remains unknown with its claim held pending reconciliation.

The C fixture uses the actual Store and HTTP tool handlers with synthetic native output (no model). Only the invitation path and supplement are seeded via trusted operations; the Pi intake/role entries and the final text settlement use the normal scheduler. The typed followup uses the pinned profile/config and B launch transaction with a synthetic executor. There are no new provider capabilities or changes to A/B persisted contracts.
