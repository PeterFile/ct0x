# Pi fresh text Task Work (opt-in)

`--pi-fresh-task-work-extension /trusted/bridge/fresh-task-work.ts` is off by
default. It requires `--enable-execution`, `--enable-member-tools`,
`--enable-coordination-roles`, a separately configured ordinary Pi extension,
and a supported Pi 1.4.0 executable. Keep `fresh-task-work.mjs` beside the
entry. The executable, loader, configured files and ancestors must be trusted
and stable. Both typed files must match the built-in pair; do not replace
ordinary `index.ts` or `bridge.mjs`. The ordinary native digest profile, when
also configured, uses its own published bytes and pins.

With the flag enabled, a bound chat intake can use `continue_work` in its
existing `send_message` assessment. B checks the current user source and, when
eligible, records a WorkInput and queues a typed wake for the original owner.
An optional Task reference must appear in the source, and a constraint must
quote it verbatim. A receipt isn't proof of execution. If B denies the request
or the typed wake isn't ready to run, there's no generic fallback. This flag
doesn't enable the legacy unpaged work-input preview.

The Pi intake with that schema is fresh-only: the ordinary native resume
profile doesn't bind the optional schema. Other eligible native resumes keep
their existing profile and digest checks.

Typed worker preparation keeps the original Task, Mandate and Assignment, not
the original conversation directory or its contents. A separate 0700 directory
under the trusted private workspace root is selected by typed delivery. It
contains only a private delivery/inode seal, never task data or a capability.
Inspection does not create it. An unsealed, replaced or non-dedicated entry
blocks with `typed_private_cwd_unavailable`; a held key is not reused. Startup
pins the directory and verifies the new Turn's exact sole
scheduler resource and the new bound Attempt. This does not bypass an unknown
or cancel-requested predecessor. Trust the directory namespace through launch;
the CWD is not a host filesystem sandbox.

The final Store commit is the launch authorization point. It checks current
member configuration, B authority, and the resource claim while consuming the
runtime's one-shot invocation right. Provider invocation happens outside the
transaction. Changes committed before this point can block launch. Later
changes are enforced by live tool checks and cancellation; the gate doesn't
prevent every change between commit and process start.

Only four pinned native HTTP tools are installed: `read_work_inputs` (directory
or bounded UTF-8 byte segment), `ack_work_input`, `hand_back_work_input` (one
input, not the Task) and `submit_result` (text only). A segment reads compact
JSON `{content,constraint,disposition}`. `constraint` is empty unless this input
advanced B's proposal revision with a contiguous verbatim quote from `content`.
`pending` and `confirmed` do not extend the Mandate; `returned` includes neither
content nor quote. After handling changes, start reading from offset zero.
Reads do not acknowledge input, and submission is not acceptance. There is no
report, delegation, investigation, file tool, MCP or generic fallback.

If a write's receipt can't be confirmed, the bridge refuses replays and all
later writes in that client, even after a successful read. Only the first
unconfirmed write adds `result.details.agent_workspace_write_unknown_v1` to
Pi's native `tool_execution_end`. The record holds the bound role and RunID,
operation, original native call ID (at most 256 visible ASCII bytes), the
idempotency key, and the SHA-256 of the exact HTTP request bytes. Non-proposal
writes keep `SHA256(operation + call ID)`; proposals use the Run-scoped key
below. The marker holds no request body or capability. Checked successes and
definite denials don't set it.

The pinned typed adapter requires a native `tool_execution_start` and matching
`tool_execution_end` (same call ID and bound write name), even for a declined
write. Ends may arrive in a different order from starts. Missing, mismatched,
or duplicate write frames and exceeding the 256-ID limit per invocation
(including closed writes) prevent settlement. This check can't see a write if
both native lifecycle frames are omitted; assistant messages and tool results
aren't substitute lifecycle proof.

The adapter retains a matched marker in the private `stdout.jsonl` and returns
`OutcomeUnknown`, even if Pi reports a successful terminal and exits normally.
It preserves the observed Terminal and child wait facts, not a native resume
reference. Assistant text and tool arguments can't set or clear the marker.
A damaged stream or clipped typed stdout log can't authorize settlement.
If a valid marker was observed despite clipping, a reserved tail in the same
1 MiB log holds a parent-written copy of just the identity, with type
`agent_workspace_write_unknown_v1`. A lost frame or failed log can still leave
no identity to recover. The log helps same-key reconciliation; it doesn't
supply the original request, reconcile the write, or authorize replay.

## Task decision proposals (separate opt-in)

`--enable-pi-task-decision-proposals` adds only `propose_task_decision` to the
exact fresh Pi 1.4.0 **text Task continuation** above. It requires the current
pinned `fresh-task-work.ts`/`fresh-task-work.mjs` pair, the existing fresh launch
flag and dependencies, and F's installed **decision request and answer** schemas.
Startup and proposal grant issuance use the same bounded pending Task read as
`GET /api/v1/me/pending-decisions`. A v30 database without F's schemas, or one
with only the request schema, rejects this opt-in; fresh Task Work remains
available with the flag off. Older pinned fresh pairs remain usable only for the
four-tool profile. This option does not install or register migrations.

The initial native text Worker is excluded: its Store run-profile and session
binding require the exact existing Worker tool set, which would need separate
Store/receipt compatibility work. Coordinator, intake, ordinary, file, Codex,
readonly investigation, and delegate/report profiles get no proposal authority.
This path does not select native resume or add a new scheduler wake.

After the final continuation launch gate **and** a durably accepted native
run, the bridge sends the question to F. The loopback request needs the issued
bearer, a distinct proof from the same digest-bound private config, and the
exact native call ID. Before sending, Pi checks the live SDK session ID against
the Run ID in that config. The proposal's `Idempotency-Key` is lowercase hex
`SHA256(operation + NUL + Run ID + NUL + call ID)`, using literal zero bytes as
separators. The server checks it against the actual bound Turn before calling F;
the native receipt parser uses the same identity for unknown markers. Reusing a
call ID in a new Turn yields a different key, without using the bearer or proof.
A bearer-only caller or the preflight cannot obtain this grant; this is not an
attestation against an actor that can steal the private config or impersonate
the trusted executable.

The tool supplies only `kind` (`decision`, `authorization`, `unblock`),
`expected_version`, `question`, `facts`, `recommendation`, optional `choices`
and (only for `authorization`) the proposed `scope` and `impact`. No expiry,
identity or grant fields are exposed. `0` opens or coalesces an identical slot;
an amendment requires the current version. The fixed receipt body contains only
`decision_request_id`, `kind` and `version`. Pi also requires `Idempotency-Key`
in the success headers to match the request exactly; missing or mismatched
receipt evidence is unknown. F binds the owner and generation and records a
question, not an answer, grant, message, acknowledgment or submission.

F #221 does not prove rollback on any proposal error. After the Engine is
called, even a stale/idempotency conflict is reported as unknown rather than a
safe rejection. The client keeps the exact call ID, original key and payload
hash and refuses further writes or replay; a successful read does not reopen
writes. The server also fences this capability's writes and preserves an
unknown Turn on Engine uncertainty, even if Pi reports success. No automatic
reconciliation or replay is provided.

Remove the proposal flag to disable new grants, or the fresh flag to disable
fresh launches. Neither settles previous claims, releases unknown resources or
authorizes retry of in-flight work.

For the offline SDK-to-Go regression, export `TEST_PI_TYPED_PRINT_FIXTURES`
pointing to an empty private directory and run these from the repository root
with the pinned SDK dependencies installed:

```sh
node --test --test-name-pattern='Pi JSON print' extensions/pi-bridge/tests/fresh-task-work.test.mjs
go test -p 1 -race -run '^TestPiFreshTask(Work|Decision)SDKPrintLifecycle$' ./internal/provider
```

Node uses an in-memory SDK session, synthetic assistant turns and mocked HTTP.
Go drives the fixed adapter with that raw JSONL and also tests single-frame
omissions without changing the tool results or final terminal.
