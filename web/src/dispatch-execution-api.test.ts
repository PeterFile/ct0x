// @vitest-environment node
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { getDispatchExecution } from "./dispatch-execution-api";
import type { DispatchExecution, ExecutionTurn } from "./dispatch-execution-api";
import { json } from "./test/groups";

const stamp = "2026-11-16T08:00:00.123456789Z";
const groupId = "群/one?x#%+", taskId = "任务/two?x#%+", requestId = "请求/three?x#%+";
const turn = (id = "lead-turn", memberId = "lead"): ExecutionTurn => ({
  id, group_id: groupId, delivery_id: `${id}-delivery`, conversation_id: `${id}-conversation`, member_id: memberId,
  state: "starting", native_session_id: "", accepted_at: null, cancel_requested: false, cancel_requested_at: null,
  created_at: stamp, updated_at: stamp, receipt: null,
});
function assigned(): { -readonly [K in keyof DispatchExecution]: DispatchExecution[K] } {
  return { group_id: groupId, task_id: taskId, request_id: requestId, dispatch_status: "assigned", coordinator_turn: turn(),
    decision: { id: "decision", kind: "assign", coordinator_turn_id: "lead-turn", created_at: stamp },
    assignment: { id: "assignment", request_id: requestId, decision_id: "decision", task_id: taskId, group_id: groupId, worker_member_id: "worker", created_at: stamp },
    assignment_dispatch_status: "enqueued",
    binding: { assignment_id: "assignment", group_id: groupId, task_id: taskId, worker_member_id: "worker", turn_id: "worker-turn", attempt_id: "attempt", task_revision: 7, created_at: stamp },
    worker_turn: turn("worker-turn", "worker") };
}
const transport = vi.fn<typeof fetch>();
const read = (signal?: AbortSignal) => getDispatchExecution(groupId, taskId, requestId, signal);
beforeEach(() => { transport.mockReset(); vi.stubGlobal("fetch", transport); });
afterEach(() => vi.unstubAllGlobals());

it("reads only the exact encoded execution endpoint with abort, no-store and no mutation key", async () => {
  const value = assigned(), signal = new AbortController().signal;
  transport.mockResolvedValueOnce(json(value));
  await expect(read(signal)).resolves.toEqual(value);
  expect(transport.mock.calls).toEqual([[`/api/v1/groups/${encodeURIComponent(groupId)}/tasks/${encodeURIComponent(taskId)}/dispatch-requests/${encodeURIComponent(requestId)}/execution`,
    { method: "GET", signal, redirect: "error", cache: "no-store" }]]);
});
it.each(["starting", "running", "cancel_requested", "unknown", "succeeded", "failed", "aborted", "truncated", "not_started"] as const)(
  "preserves the actual %s Turn independently of null receipt", async state => {
    const value = assigned(), canceled = state === "cancel_requested";
    value.worker_turn = { ...turn("worker-turn", "worker"), state, cancel_requested: canceled, cancel_requested_at: canceled ? stamp : null };
    transport.mockResolvedValueOnce(json(value));
    await expect(read()).resolves.toEqual(value);
  },
);
it("preserves unknown with a native succeeded terminal, false observations, negative exit and cancellation intent", async () => {
  const value = assigned();
  value.worker_turn = { ...turn("worker-turn", "worker"), state: "unknown", native_session_id: "native-worker", cancel_requested: true, cancel_requested_at: stamp,
    receipt: { started: true, accepted: false, outcome: "unknown", terminal: "succeeded", delivery_failed: true, native_session_id: "native-worker",
      direct_child_reaped: false, output_complete: false, exit_code: -1, logs_truncated: true } };
  transport.mockResolvedValueOnce(json(value));
  await expect(read()).resolves.toEqual(value);
});
it.each(["pending", "held", "enqueued"] as const)("keeps %s without any execution explicitly unobserved", async dispatch_status => {
  const value: DispatchExecution = { group_id: groupId, task_id: taskId, request_id: requestId, dispatch_status,
    coordinator_turn: null, decision: null, assignment: null, assignment_dispatch_status: null, binding: null, worker_turn: null };
  transport.mockResolvedValueOnce(json(value));
  await expect(read()).resolves.toEqual(value);
});
it("accepts decline and assignment before a Turn or attempt exists without fabricating either", async () => {
  const value = assigned();
  value.worker_turn = null; value.binding = null; value.assignment_dispatch_status = "pending";
  transport.mockResolvedValueOnce(json(value)); await expect(read()).resolves.toEqual(value);
  value.assignment = null; value.assignment_dispatch_status = null; value.dispatch_status = "declined";
  value.decision = { ...value.decision!, kind: "decline" };
  transport.mockResolvedValueOnce(json(value)); await expect(read()).resolves.toEqual(value);
});

it.each([
  (v: DispatchExecution) => ({ ...v, group_id: "foreign" }),
  (v: DispatchExecution) => ({ ...v, task_id: "foreign" }),
  (v: DispatchExecution) => ({ ...v, request_id: "foreign" }),
  (v: DispatchExecution) => ({ ...v, dispatch_status: "completed" }),
  (v: DispatchExecution) => ({ ...v, coordinator_turn: { ...v.coordinator_turn, group_id: "foreign" } }),
  (v: DispatchExecution) => ({ ...v, coordinator_turn: { ...v.coordinator_turn, id: "different-turn" } }),
  (v: DispatchExecution) => ({ ...v, coordinator_turn: { ...v.coordinator_turn, state: "accepted" } }),
  (v: DispatchExecution) => ({ ...v, coordinator_turn: { ...v.coordinator_turn, cancel_requested: true } }),
  (v: DispatchExecution) => ({ ...v, coordinator_turn: { ...v.coordinator_turn, created_at: "2026-02-30T00:00:00Z" } }),
  (v: DispatchExecution) => ({ ...v, decision: { ...v.decision, kind: "decline" } }),
  (v: DispatchExecution) => ({ ...v, assignment: { ...v.assignment, request_id: "foreign" } }),
  (v: DispatchExecution) => ({ ...v, assignment: { ...v.assignment, group_id: "foreign" } }),
  (v: DispatchExecution) => ({ ...v, assignment: { ...v.assignment, task_id: "foreign" } }),
  (v: DispatchExecution) => ({ ...v, assignment: { ...v.assignment, decision_id: "foreign" } }),
  (v: DispatchExecution) => ({ ...v, binding: { ...v.binding, assignment_id: "foreign" } }),
  (v: DispatchExecution) => ({ ...v, binding: { ...v.binding, task_id: "foreign" } }),
  (v: DispatchExecution) => ({ ...v, binding: { ...v.binding, turn_id: "foreign" } }),
  (v: DispatchExecution) => ({ ...v, binding: { ...v.binding, task_revision: 0 } }),
  (v: DispatchExecution) => ({ ...v, worker_turn: { ...v.worker_turn, member_id: "foreign" } }),
  (v: DispatchExecution) => ({ ...v, assignment_dispatch_status: "pending" }),
  (v: DispatchExecution) => ({ ...v, worker_turn: null }),
  (v: DispatchExecution) => ({ ...v, assignment: null }),
  (v: DispatchExecution) => ({ ...v, coordinator_turn: null }),
  (v: DispatchExecution) => ({ ...v, coordinator_turn: { ...v.coordinator_turn, receipt: {} } }),
])("rejects malformed or mismatched evidence instead of showing stale execution %#", async change => {
  transport.mockResolvedValueOnce(json(change(assigned())));
  await expect(read()).rejects.toMatchObject({ code: "invalid_response", message: expect.stringContaining("未确认") });
});
it.each(["reason", "stdout_path", "stderr_path", "claim_token", "prompt", "token", "raw_receipt"])("rejects a non-whitelisted %s receipt field", async field => {
  const value = assigned(), receipt = { started: true, accepted: true, outcome: "succeeded", terminal: "succeeded", delivery_failed: false,
    native_session_id: "native", direct_child_reaped: true, output_complete: true, exit_code: 0, logs_truncated: false, [field]: "PRIVATE" };
  transport.mockResolvedValueOnce(json({ ...value, worker_turn: { ...value.worker_turn, receipt } }));
  await expect(read()).rejects.toMatchObject({ code: "invalid_response" });
});
it.each(["7.0000000000000001", "9007199254740991.4", "9007199254740992", "7e0", "-0"])("rejects rounded wire integer %s", async token => {
  transport.mockResolvedValueOnce(new Response(JSON.stringify(assigned()).replace('"task_revision":7', `"task_revision":${token}`)));
  await expect(read()).rejects.toMatchObject({ code: "invalid_response" });
});
it.each(["", ".", "..", " padded", "bad\n", "bad\0", "\ud800", "x".repeat(4097)])("rejects unaddressable identity %# before transport", async id => {
  for (const ids of [[id, taskId, requestId], [groupId, id, requestId], [groupId, taskId, id]]) {
    await expect(getDispatchExecution(ids[0], ids[1], ids[2])).rejects.toMatchObject({ code: "invalid_input" });
  }
  expect(transport).not.toHaveBeenCalled();
});
it.each([404, 500, 200, 202])("sanitizes failed/malformed HTTP %s and never exposes server details", async status => {
  transport.mockResolvedValueOnce(json({ error: { code: "internal_error", message: "PRIVATE path prompt" } }, status));
  await expect(read()).rejects.toMatchObject({ message: expect.not.stringContaining("PRIVATE") });
});
it("rejects invalid UTF-8 and disconnected reads without leaking transport details", async () => {
  transport.mockResolvedValueOnce(new Response(new Uint8Array([0xff])));
  await expect(read()).rejects.toMatchObject({ code: "invalid_response" });
  transport.mockRejectedValueOnce(new TypeError("PRIVATE"));
  await expect(read()).rejects.toMatchObject({ code: "disconnected", message: expect.not.stringContaining("PRIVATE") });
});
