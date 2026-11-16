import { ApiError } from "./api";
import type { DispatchState } from "./dispatch-request-api";

export type ExecutionTurnState = "starting" | "running" | "cancel_requested" | "unknown" | "succeeded" | "failed" | "aborted" | "truncated" | "not_started";
export type ExecutionOutcome = Exclude<ExecutionTurnState, "starting" | "running" | "cancel_requested">;
export type ExecutionReceipt = Readonly<{
  started: boolean; accepted: boolean; outcome: ExecutionOutcome; terminal: Exclude<ExecutionOutcome, "unknown" | "not_started"> | ""; delivery_failed: boolean;
  native_session_id: string; direct_child_reaped: boolean; output_complete: boolean; exit_code: number; logs_truncated: boolean;
}>;
export type ExecutionTurn = Readonly<{
  id: string; group_id: string; delivery_id: string; conversation_id: string; member_id: string; state: ExecutionTurnState; native_session_id: string;
  accepted_at: string | null; cancel_requested: boolean; cancel_requested_at: string | null; created_at: string; updated_at: string; receipt: ExecutionReceipt | null;
}>;
export type DispatchExecution = Readonly<{
  group_id: string; task_id: string; request_id: string; dispatch_status: DispatchState["status"]; coordinator_turn: ExecutionTurn | null;
  decision: Readonly<{ id: string; kind: "assign" | "decline"; coordinator_turn_id: string; created_at: string }> | null;
  assignment: Readonly<{ id: string; request_id: string; decision_id: string; task_id: string; group_id: string; worker_member_id: string; created_at: string }> | null;
  assignment_dispatch_status: "pending" | "enqueued" | "held" | null;
  binding: Readonly<{ assignment_id: string; group_id: string; task_id: string; worker_member_id: string; turn_id: string; attempt_id: string; task_revision: number; created_at: string }> | null;
  worker_turn: ExecutionTurn | null;
}>;
const record = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v);
const exact = (v: unknown, fields: string): v is Record<string, unknown> => record(v)
  && Object.keys(v).length === fields.split(" ").length && fields.split(" ").every(key => Object.hasOwn(v, key));
const text = (v: unknown): v is string => typeof v === "string" && !/[\ud800-\udfff]/u.test(v);
const encoder = new TextEncoder();
const identifier = (v: unknown): v is string => text(v) && v !== "" && v !== "." && v !== ".."
  && !/^\p{White_Space}|\p{White_Space}$/u.test(v) && !/[\0\r\n]/u.test(v) && encoder.encode(v).length <= 4096;
const states = ["starting", "running", "cancel_requested", "unknown", "succeeded", "failed", "aborted", "truncated", "not_started"];
const outcomes = ["unknown", "not_started", "succeeded", "failed", "aborted", "truncated"], terminals = ["", "succeeded", "failed", "aborted", "truncated"];
const oneOf = (v: unknown, values: readonly string[]) => typeof v === "string" && values.includes(v);
function time(v: unknown): v is string {
  if (typeof v !== "string" || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$/.test(v) || v.startsWith("0000-")) return false;
  const date = new Date(v);
  return Number.isFinite(date.valueOf()) && date.toISOString().slice(0, 19) === v.slice(0, 19) && date.valueOf() > -62135596800000;
}
function isTurn(v: unknown, groupId: string): v is ExecutionTurn | null {
  if (v === null) return true;
  if (!exact(v, "id group_id delivery_id conversation_id member_id state native_session_id accepted_at cancel_requested cancel_requested_at created_at updated_at receipt")
    || ![v.id, v.delivery_id, v.conversation_id, v.member_id].every(identifier) || v.group_id !== groupId || !oneOf(v.state, states)
    || !text(v.native_session_id) || !time(v.created_at) || !time(v.updated_at) || !(v.accepted_at === null || time(v.accepted_at))
    || !(v.cancel_requested_at === null || time(v.cancel_requested_at)) || v.cancel_requested !== (v.cancel_requested_at !== null)) return false;
  const r = v.receipt;
  return r === null || (exact(r, "started accepted outcome terminal delivery_failed native_session_id direct_child_reaped output_complete exit_code logs_truncated")
    && [r.started, r.accepted, r.delivery_failed, r.direct_child_reaped, r.output_complete, r.logs_truncated].every(v => typeof v === "boolean")
    && oneOf(r.outcome, outcomes) && oneOf(r.terminal, terminals) && text(r.native_session_id) && Number.isSafeInteger(r.exit_code));
}
function isExecution(v: unknown, groupId: string, taskId: string, requestId: string): v is DispatchExecution {
  if (!exact(v, "group_id task_id request_id dispatch_status coordinator_turn decision assignment assignment_dispatch_status binding worker_turn")
    || v.group_id !== groupId || v.task_id !== taskId || v.request_id !== requestId
    || !oneOf(v.dispatch_status, ["pending", "held", "enqueued", "assigned", "declined"])
    || !isTurn(v.coordinator_turn, groupId) || !isTurn(v.worker_turn, groupId)) return false;
  const d = v.decision, a = v.assignment, b = v.binding, lead = v.coordinator_turn, worker = v.worker_turn;
  if (v.dispatch_status === "pending" || v.dispatch_status === "held") {
    if (lead !== null) return false;
  }
  if (v.dispatch_status !== "assigned" && v.dispatch_status !== "declined") {
    return d === null && a === null && b === null && worker === null && v.assignment_dispatch_status === null;
  }
  if (!lead || !exact(d, "id kind coordinator_turn_id created_at") || !identifier(d.id) || !time(d.created_at) || d.coordinator_turn_id !== lead.id) return false;
  if (v.dispatch_status === "declined") return d.kind === "decline" && a === null && b === null && worker === null && v.assignment_dispatch_status === null;
  if (d.kind !== "assign" || !exact(a, "id request_id decision_id task_id group_id worker_member_id created_at")
    || !identifier(a.id) || !identifier(a.worker_member_id) || a.worker_member_id === lead.member_id || a.request_id !== requestId || a.decision_id !== d.id
    || a.group_id !== groupId || a.task_id !== taskId || !time(a.created_at) || !oneOf(v.assignment_dispatch_status, ["pending", "held", "enqueued"])) return false;
  if (worker !== null && (worker.member_id !== a.worker_member_id || v.assignment_dispatch_status !== "enqueued")) return false;
  return b === null || (worker !== null && exact(b, "assignment_id group_id task_id worker_member_id turn_id attempt_id task_revision created_at")
    && b.assignment_id === a.id && b.group_id === groupId && b.task_id === taskId && b.worker_member_id === a.worker_member_id
    && b.turn_id === worker.id && identifier(b.attempt_id) && typeof b.task_revision === "number" && Number.isSafeInteger(b.task_revision) && b.task_revision > 0 && time(b.created_at));
}
function decode(buffer: ArrayBuffer): unknown {
  const source = new TextDecoder("utf-8", { fatal: true }).decode(buffer);
  // Validate integer wire tokens before JSON.parse can round revisions or exit codes.
  const tokens = /"(?:[^"\\]|\\.)*"|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?/uy;
  for (let index = 0; index < source.length; index++) {
    if (!/["\d-]/u.test(source[index])) continue;
    tokens.lastIndex = index;
    const token = tokens.exec(source)?.[0];
    if (token === undefined) return null;
    index = tokens.lastIndex - 1;
    if (!token.startsWith('"') && (!/^-?(?:0|[1-9]\d*)$/.test(token) || token === "-0" || !Number.isSafeInteger(Number(token)))) return null;
  }
  return JSON.parse(source);
}
export async function getDispatchExecution(groupId: string, taskId: string, requestId: string, signal?: AbortSignal): Promise<DispatchExecution> {
  if (![groupId, taskId, requestId].every(identifier)) throw new ApiError("执行回执查询标识无效。", 0, "invalid_input");
  let response: Response;
  try {
    response = await fetch(`/api/v1/groups/${encodeURIComponent(groupId)}/tasks/${encodeURIComponent(taskId)}/dispatch-requests/${encodeURIComponent(requestId)}/execution`,
      { method: "GET", signal, redirect: "error", cache: "no-store" });
  } catch {
    throw new ApiError("执行回执未确认，请重新读取。", 0, signal?.aborted ? "request_canceled" : "disconnected");
  }
  const value: unknown = await response.arrayBuffer().then(decode).catch(() => null);
  if (response.status === 200 && isExecution(value, groupId, taskId, requestId)) return value;
  throw new ApiError(`执行回执未确认，请重新读取（HTTP ${response.status}）。`, response.status, response.ok ? "invalid_response" : "http_error");
}
