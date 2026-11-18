# Pi fresh text Task Work (opt-in)

`--pi-fresh-task-work-extension /trusted/bridge/fresh-task-work.ts` is off by
default. It requires `--enable-execution`, `--enable-member-tools`,
`--enable-coordination-roles`, a separately configured ordinary Pi extension,
and a supported Pi 1.4.0 executable. Keep `fresh-task-work.mjs` beside the
entry. The executable, loader, configured files and ancestors must be trusted
and stable. Both typed files must match the built-in pair; do not replace
ordinary `index.ts` or `bridge.mjs`. The ordinary native digest profile, when
also configured, uses its own published bytes and pins.

This profile consumes an existing B v29 typed followup for a plain-text Task.
It does not turn on any new intake schema or provide a second authorization
path. Preparation uses the original Mandate and private conversation workspace.
After claim, it checks the original Assignment, the new bound Attempt and the
same Turn's sole resource. Only the four pinned native HTTP tools are installed:
`read_work_inputs` (directory or bounded UTF-8 byte segment), `ack_work_input`,
`hand_back_work_input` (one input, not the Task) and `submit_result` (text only).
Reads do not acknowledge input, and submission is not acceptance. There is no
report, delegation, investigation, file authority, MCP or generic fallback
for a typed run. The private directory is not a host filesystem sandbox.

Keep the original call ID, derived key and request if a write outcome is
unknown. The bridge does not resend it and blocks subsequent writes for that
client, even after a successful read. This is not durable reconciliation. Remove
the fresh flag on restart to disable new launches. That does not settle previous
claims, release unknown resources or authorize retry of in-flight work.
