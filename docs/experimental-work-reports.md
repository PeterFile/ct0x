# Experimental B3 work reports

B3 uses the original read-only investigation Assignment. Progress is visible in the Task's `work-reports` history without making a message or delivery. Questions, blockers and delivery reports each have one typed wake directed to the original owner. The owner decides what to do next; a report is neither a formal Submission nor a local-user Review. Handback notices do not wake a Turn. This candidate adds no runtime wiring and is closed for execution by default.

A child may overlap its own known-running owner, but siblings are serial. A cancellation request or unknown Turn blocks replacement; only a durable settlement receipt and released resource claims count as settlement. A new owner Submission also waits for queued children and fails on a damaged association. Exact same-key Submission replay and local-user Review are not new submissions.

The staged registry appends provisional v30 after B1's v28 inputs and v29 typed delivery. Existing rows and tables are not rebuilt or backfilled. v30 extends the delivery source trigger to admit the two new kinds only with their matching logical rows and adds report foreign keys and queue identity guards.

## B1 integration for `answer_to`

This slice rejects nonempty `answer_to`: B1's immutable inputs are installed, but no child input grant exists. If a later slice adds that grant, resolve it as `task_work_inputs.id`, not a message, trigger, source, or delivery ID. Use B1's verified input projection (`workInputSelect` and the `chat_followup_routes` evidence) in the live report transaction. Match group, topic, Task, Task revision, Mandate, Assignment, and its owner to the verified delegation, including the Assignment's dispatch authority generation. The current B1 inbox is owner-bound: a matching Assignment by itself does not grant the child's Turn access to that input. Require an explicit, immutable grant of that exact input to the verified delegation and its source Turn before recording the answer.

Only then persist the verified input reference in the immutable report, compare it on callback readback and replay, and project it only with the scoped report. A child answer is not B1 owner acknowledgement and must not create a `task_work_input_consumptions` row. B1 followup delivery proof remains the responsibility of the B1 producer; never infer it from a B3 report or a notice body.
