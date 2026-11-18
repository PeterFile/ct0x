# Opt-in typed Pi investigation transport

This C candidate is disabled by default. Its three typed wakes use B's exact v30 launch transaction at `2b942bf8b3f2f19b7e3e3c71700360ba45f8aa6c`, in the same scheduler and registry as the first investigation. The CLI opt-in is not task or policy authority.

For a coordinator's isolated trial, start from the repository root with a separate non-symlink `0700` data directory and a qualified Pi 1.4.0 executable. Use these `workspace` arguments (`BRIDGE` is the absolute physical path to this checkout's `extensions/pi-bridge`):

```sh
--listen 127.0.0.1:0 --data-dir "$DATA_DIR" --pi-path "$PI_140" \
  --enable-execution --execution-max-active 1 --enable-member-tools \
  --enable-coordination-roles --enable-investigation-execution \
  --pi-member-tools-extension "$BRIDGE/index.ts" \
  --pi-fresh-investigation-work-extension "$BRIDGE/fresh-investigation-work.ts"
```

Use the printed loopback port. Keep `index.ts` with its matched `bridge.mjs`, and `fresh-investigation-work.ts` with `fresh-task-work.mjs`. Use this checkout's new pair for combined intake, and also pass `--pi-fresh-task-work-extension "$BRIDGE/fresh-task-work.ts"` to exercise the existing `continue_work` intake. The legacy qualification tests do not certify the new config with an old pair. The scheduler selects and claims the private per-delivery CWD; do not substitute an external workspace. This trial adds neither an owner delegation tool nor a task handback tool.

## Pass the profile and digest from the claimed launch

- Map C's `FreshTaskWorkProfile` (`Action`, `SourceKind`, `Operations`) directly to `provider.PiFreshInvestigationProfile`. `NewPiFreshInvestigationAdapter` takes the absolute `fresh-investigation-work.ts` path and a timeout. `Supports(selected, profile)` only checks exact installed Pi `1.4.0` executor metadata, bridge bytes, and profile shape; it doesn't discover providers, probe a CLI, or prove authority.
- In the typed launcher, after issuing the capability, call `provider.PiFreshInvestigationConfig(endpoint, token, runID, profile)`. Write the returned bytes to a managed, current-owner `0600` regular file in the private config root. Pass its absolute path, the **returned** SHA-256, the same claimed Turn ID, and the same profile in `PiFreshInvestigationBinding` to `adapter.NewExecutor(ctx, selected, binding)`. Don't digest readback or let `Construct` create credentials without access to the launcher's cleanup path: `Construct` can succeed and the final launch check can still deny execution.
- The config is compact, newline-terminated UTF-8 JSON with exactly `endpoint`, `token`, `run_id`, `action`, `source_kind`, `operations` in that order. It permits only the explicitly ported numeric loopback member-tool base, requires `action: read_only_investigation`, and carries no Task, report, policy, resource, or artifact-version selections. The launcher supplies the file path and SHA-256, not the token, in `AGENT_WORKSPACE_MEMBER_TOOL_CONFIG` and `AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256`. Go checks the digest, canonical bytes, Turn/profile, and file identity; Pi hashes and parses the same bounded buffer and never rereads it for registration.
- Keep `fresh-task-work.mjs` beside the new entry: it supplies the shared work-input checks and write-uncertainty metadata format. Go checks both files' exact pins at construction and invocation; it doesn't load the ordinary bridge or the pure continuation entry. Keep the actual Pi executable, loader, dependencies, paths, and ancestors trusted and stable. The adapter reuses the ordinary JSON-mode member executor, with `--no-session` and the exact `--session-id` for a one-shot in-memory session. It has no capture, resume, or append path. At session start, the extension requires the exact ID, JSON mode, and no stored session or prior work before registering the issued names. Built-in host tools, ambient extensions, MCP, skills, templates, and context files aren't enabled by this invocation. This is not a host sandbox.

## Register only the issued operations

The ordered prefix is `read_investigation_channel`, then `read_investigation_artifact`, omitting each when the source doesn't grant it. The suffix is exact:

| B source kind | Operations after the optional material reads |
| --- | --- |
| `followup` | `read_work_inputs`, `ack_work_input`, `hand_back_work_input`, `submit_result` |
| `delegation` | `report_work` (including a one-tool profile when no materials are granted) |
| `report` | `read_work_report`, `read_work_inputs`, `ack_work_input`, `hand_back_work_input`, `submit_result` |

| Operation | Request bounds |
| --- | --- |
| `read_investigation_channel` | `{limit?,before?}`; limit 1–200, cursor up to 2048 bytes. No topic selector. |
| `read_investigation_artifact` | `{artifact_id,version_id,offset?,max_bytes?}`; each handle up to 128 UTF-8 bytes, offset 0–64 MiB, fragment 1–16384 bytes. C/B still decides whether the exact immutable tuple is authorized. |
| `read_work_inputs` | Directory `{limit?,before?}` (limit 1–20, cursor up to 512 bytes) or segment `{input_id,offset?,max_bytes?}` (nonnegative safe-integer byte offset, fragment 4–8192 bytes). The bridge doesn't reinterpret the segment's `text`. |
| `ack_work_input`, `hand_back_work_input` | `{input_id}` or `{input_id,reason}`, respectively. Input handles up to 256 UTF-8 bytes; reason up to 2048. |
| `read_work_report` | `{}` only, even if the caller knows the bound report ID. Checks the report projection without treating the report or native reply as authority. |
| `report_work` | `{kind,summary}`; `progress`, `question`, `blocked`, or `delivered`. Summary up to 8192 UTF-8 bytes; no `answer_to`. |
| `submit_result` | `{summary}` only, up to 8192 UTF-8 bytes. No artifact versions. |

Requests are POSTs to C's registered `/api/v1/member-tools/<operation>` routes with the issued bearer. Writes keep C's existing `SHA256(operation + nativeCallID)` `Idempotency-Key`; reads don't send one. If the first write receipt can't be confirmed, the bridge emits the same [native result details as pure Task Work](fresh-task-work.md), with the role from the checked `source_kind`. The adapter preserves the call ID, key, and exact HTTP body digest in private stdout and returns `OutcomeUnknown`, without overwriting the native Terminal or Wait. Later writes, including replays, are refused. Reads, checked successes, and definite denials don't add a write-unknown marker. Neither successful Pi output nor this private evidence is a report receipt, Submission, or acceptance.

## Refuse JSON input if the loaded extension can't bind

Pi 1.4.0 catches a `session_start` handler error, and its print mode still calls `prompt()`. This extension consumes input with the `input` hook in JSON mode unless startup verified the session and exact active tools. Pi handles that input before model authentication or an agent run. The hook doesn't intercept TUI input or exit the host process. The parent can't settle from a session header alone.

## Offline proof and remaining acceptance

The SQLite fixtures run the real claim/resource/Attempt and B commit path with an in-process executor. The Pi 1.4.0 loader and in-memory SDK fixtures check the three profiles, intercepted HTTP, and JSON print startup refusal with a mismatched, nonempty, or persistent session. With cached SDK dependencies installed, point `TEST_PI_TYPED_PRINT_FIXTURES` at a new private directory and run from the repository root:

```sh
node --test --test-name-pattern='Pi JSON print' extensions/pi-bridge/tests/fresh-{task,investigation}-work.test.mjs
go test -run '^TestPiFresh(TaskWork|Investigation)SDKPrintLifecycle$' ./internal/provider
```

This replays synthetic Pi JSONL through the pinned Go adapters, including `report_work` without either native lifecycle frame. It does not exercise malformed bootstrap on the deployed CLI, real Pi/account/model startup, or a deployed report-to-owner round trip. Unknown executions still require reconciliation; the opt-in does not release their claims.
