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
path. Preparation keeps the original Task, Mandate and Assignment, not the
original conversation directory or its contents. A separate 0700 directory
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

Keep the original call ID, derived key and request if a write outcome is
unknown. The bridge does not resend it and blocks subsequent writes for that
client, even after a successful read. This is not durable reconciliation. Remove
the fresh flag on restart to disable new launches. That does not settle previous
claims, release unknown resources or authorize retry of in-flight work.
