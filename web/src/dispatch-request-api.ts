import { ApiError } from "./api";
import type { AdmissionReceipt } from "./coordination-api";
import type { Task } from "./task-api";

export type TaskCoordinationView = Readonly<{
  coordination: Readonly<{ task_id: string; group_id: string; topic_id: string; current_mandate_id: string; authority_generation: number }>;
  admission: Omit<AdmissionReceipt, "decision"> & Readonly<{
    decision: Omit<AdmissionReceipt["decision"], "kind"> & { kind: "create_new" | "link_existing" };
  }>;
}>;
export type DispatchRequestInput = Readonly<{ expected_task_revision: number; expected_authority_generation: number; reason: string }>;
export type DispatchRequest = Readonly<{
  id: string; group_id: string; topic_id: string; task_id: string; task_revision: number; authority_generation: number;
  mandate_id: string; coordinator_member_id: string; binding_revision: number; policy_revision: number;
  requested_by: "user"; reason: string; created_at: string;
}>;
export type DispatchDelivery = Readonly<{
  message_id: string; delivery_id: string; conversation_id: string; coordinator_member_id: string;
  materialized_from_generation: number; created_at: string;
}>;
export type DispatchState = Readonly<{
  request_id: string; generation: number; status: "pending" | "enqueued" | "held" | "assigned" | "declined";
  attempt_count: number; next_attempt_at: string; last_error_code: string; held_reason: string; delivery: DispatchDelivery | null;
}>;
export type DispatchView = Readonly<{ request: DispatchRequest; state: DispatchState }>;

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);
const exactRecord = (value: unknown, fields: readonly string[]): value is Record<string, unknown> => isRecord(value)
  && Object.keys(value).length === fields.length && fields.every(field => Object.hasOwn(value, field));
const validText = (value: unknown): value is string => typeof value === "string" && !/[\ud800-\udfff]/u.test(value);
const nonblank = (value: unknown): value is string => validText(value) && /\P{White_Space}/u.test(value);
const goEdgeSpace = /^\p{White_Space}|\p{White_Space}$/u;
const encoder = new TextEncoder();
const validIdentifier = (value: unknown): value is string => validText(value) && value !== ""
  && !goEdgeSpace.test(value) && !/[\0\r\n]/u.test(value) && encoder.encode(value).length <= 4096;
// fetch normalizes exact dot segments before sending; reject them rather than changing the scope.
const pathIdentifier = (value: unknown): value is string => validIdentifier(value) && value !== "." && value !== "..";
// Topics are opaque text, not bounded/trimmed path identifiers.
const validTopic = validText;
const validKey = (value: unknown): value is string => typeof value === "string" && value.length >= 1
  && value.length <= 128 && !/[^\x21-\x7e]|,/.test(value);
const nonnegative = (value: unknown): value is number => typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
const positive = (value: unknown): value is number => nonnegative(value) && value > 0;
function writerTime(value: unknown): value is string {
  if (typeof value !== "string") return false;
  const match = /^(\d{4})-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.(\d{1,9}))?Z$/.exec(value);
  if (!match || match[0] !== value || match[1] === "0000") return false;
  const date = new Date(value);
  return Number.isFinite(date.valueOf()) && date.toISOString().slice(0, 19) === value.slice(0, 19)
    && !(value.startsWith("0001-01-01T00:00:00") && !/[1-9]/.test(match[2] ?? ""));
}
function decodeResponse(buffer: ArrayBuffer): unknown {
  const source = new TextDecoder("utf-8", { fatal: true }).decode(buffer);
  const maximum = String(Number.MAX_SAFE_INTEGER);
  // All numeric fields in these DTOs are nonnegative int64. Validate whole wire tokens
  // before JSON.parse can round a fraction or an out-of-range integer into a safe number.
  const tokens = /"(?:[^"\\]|\\.)*"|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?/uy;
  for (let index = 0; index < source.length; index++) {
    if (!/["\d-]/u.test(source[index])) continue;
    tokens.lastIndex = index;
    const token = tokens.exec(source)?.[0];
    if (token === undefined) return null;
    index = tokens.lastIndex - 1;
    if (token.startsWith('"')) continue;
    if (!/^(?:0|[1-9]\d*)$/.test(token) || token.length > maximum.length
      || (token.length === maximum.length && token > maximum)) return null;
  }
  return JSON.parse(source);
}
const heldReasons = ["task_stale", "authority_stale", "binding_stale", "policy_stale", "coordinator_inactive", "no_eligible_worker", "payload_too_large", "payload_not_encodable"];
const requestFields = ["id", "group_id", "topic_id", "task_id", "task_revision", "authority_generation", "mandate_id",
  "coordinator_member_id", "binding_revision", "policy_revision", "requested_by", "reason", "created_at"];
// Preserve task-api's status/pointer shape without using its replacement-decoding JSON reader.
function isTask(value: unknown, groupId: string, taskId: string): value is Task {
  return isRecord(value) && ["id", "group_id", "title", "description", "acceptance", "assignee_id", "active_attempt_id", "current_submission_id", "created_at"]
    .every(field => validText(value[field])) && value.id === taskId && value.group_id === groupId
    && validText(value.active_attempt_id) && (value.active_attempt_id === "" || value.active_attempt_id.trim() !== "")
    && validText(value.current_submission_id)
    && ((value.status === "open" && value.current_submission_id === "")
      || (value.status === "submitted" && value.active_attempt_id !== "" && value.current_submission_id.trim() !== "")
      || (value.status === "accepted" && value.active_attempt_id === "" && value.current_submission_id.trim() !== ""))
    && positive(value.revision) && Array.isArray(value.dependency_ids) && value.dependency_ids.every(validText);
}
function isRequest(value: unknown, groupId: string, taskId: string): value is DispatchRequest {
  return exactRecord(value, requestFields) && pathIdentifier(value.id) && value.group_id === groupId && value.task_id === taskId
    && validTopic(value.topic_id) && positive(value.task_revision) && positive(value.authority_generation)
    && validIdentifier(value.mandate_id) && validIdentifier(value.coordinator_member_id)
    && positive(value.binding_revision) && positive(value.policy_revision) && value.requested_by === "user"
    && nonblank(value.reason) && writerTime(value.created_at);
}
function isAuthority(value: unknown, groupId: string, taskId: string): value is TaskCoordinationView {
  if (!exactRecord(value, ["coordination", "admission"])) return false;
  const c = value.coordination, a = value.admission;
  if (!exactRecord(c, ["task_id", "group_id", "topic_id", "current_mandate_id", "authority_generation"])
    || c.task_id !== taskId || c.group_id !== groupId || !validTopic(c.topic_id) || !positive(c.authority_generation)
    || !validIdentifier(c.current_mandate_id) || !exactRecord(a, ["decision", "mandate", "task"])) return false;
  const d = a.decision, m = a.mandate, t = a.task;
  if (!exactRecord(d, ["id", "trigger_id", "group_id", "topic_id", "kind", "actor_id", "policy_revision", "reason",
    "task_id", "task_revision_at_admission", "mandate_id", "created_at"])
    || !exactRecord(m, ["id", "decision_id", "origin_trigger_id", "group_id", "topic_id", "task_id", "task_revision_at_admission",
      "authority_kind", "authorized_by", "strength", "policy_revision", "scope", "created_at"])
    || !exactRecord(t, ["id", "group_id", "revision_at_admission"])
    || !exactRecord(m.scope, ["objective", "description", "acceptance", "assignee_id", "dependency_ids"])) return false;
  const scope = m.scope;
  return validIdentifier(d.id) && validIdentifier(d.trigger_id) && m.id === c.current_mandate_id && d.mandate_id === m.id
    && m.decision_id === d.id && m.origin_trigger_id === d.trigger_id
    && d.group_id === groupId && m.group_id === groupId && t.group_id === groupId
    && d.task_id === taskId && m.task_id === taskId && t.id === taskId
    && d.topic_id === c.topic_id && m.topic_id === c.topic_id && (d.kind === "create_new" || d.kind === "link_existing")
    && d.actor_id === "user" && m.authorized_by === "user" && m.authority_kind === "local_user_explicit" && m.strength === "explicit"
    && d.policy_revision === "local-user-explicit/v1" && m.policy_revision === d.policy_revision
    && positive(d.task_revision_at_admission) && m.task_revision_at_admission === d.task_revision_at_admission
    && t.revision_at_admission === d.task_revision_at_admission && (d.kind !== "create_new" || t.revision_at_admission === 1)
    && nonblank(d.reason) && writerTime(d.created_at) && m.created_at === d.created_at
    && nonblank(scope.objective) && validText(scope.description) && nonblank(scope.acceptance)
    && (scope.assignee_id === "" || validIdentifier(scope.assignee_id)) && Array.isArray(scope.dependency_ids)
    && scope.dependency_ids.every(validIdentifier) && new Set(scope.dependency_ids).size === scope.dependency_ids.length;
}
function isView(value: unknown, groupId: string, taskId: string, requestId: string): value is DispatchView {
  if (!exactRecord(value, ["request", "state"]) || !isRequest(value.request, groupId, taskId) || value.request.id !== requestId) return false;
  const s = value.state, r = value.request;
  if (!exactRecord(s, ["request_id", "generation", "status", "attempt_count", "next_attempt_at", "last_error_code", "held_reason", "delivery"])
    || s.request_id !== requestId || !positive(s.generation) || !nonnegative(s.attempt_count) || !writerTime(s.next_attempt_at)
    || !validText(s.last_error_code) || !validText(s.held_reason)) return false;
  // Without a retry, materialization and decisions preserve the initial next-at.
  if (s.attempt_count === 0 && s.next_attempt_at !== r.created_at) return false;
  // Store starts at (generation=1, count=0); only pending retries increment both.
  // Materialization adds one generation, and a later decision adds one more, without changing count.
  if (s.status === "pending") return s.delivery === null && s.held_reason === "" && s.attempt_count === s.generation - 1
    && (s.attempt_count === 0 ? s.last_error_code === "" : ["sqlite_busy", "sqlite_locked"].includes(s.last_error_code));
  if (s.status === "held") return s.delivery === null && heldReasons.includes(s.held_reason) && s.last_error_code === ""
    && s.attempt_count === s.generation - 2;
  if (typeof s.status !== "string" || !["enqueued", "assigned", "declined"].includes(s.status) || s.held_reason !== "" || s.last_error_code !== "") return false;
  const d = s.delivery, fromGeneration = s.generation - (s.status === "enqueued" ? 1 : 2);
  if (s.attempt_count !== fromGeneration - 1) return false;
  return exactRecord(d, ["message_id", "delivery_id", "conversation_id", "coordinator_member_id", "materialized_from_generation", "created_at"])
    && validIdentifier(d.message_id) && validIdentifier(d.delivery_id) && validIdentifier(d.conversation_id)
    && d.coordinator_member_id === r.coordinator_member_id && positive(d.materialized_from_generation)
    && d.materialized_from_generation === fromGeneration && writerTime(d.created_at);
}
const refusalCodes: Readonly<Record<number, readonly string[]>> = {
  400: ["invalid_input", "invalid_json"], 403: ["invalid_host", "invalid_origin"], 404: ["not_found"],
  405: ["method_not_allowed"], 409: ["conflict"], 413: ["body_too_large"], 415: ["unsupported_media_type"],
};
const rejectionMessages: Readonly<Record<number, string>> = {
  400: "分派请求输入未通过验证", 403: "本地访问校验失败，无法登记分派请求", 404: "未找到分派请求或关联记录，或记录不可访问",
  405: "服务尚不支持此分派请求接口", 409: "分派请求与当前任务、授权或协调配置冲突",
  413: "分派请求内容过大", 415: "服务仅接受 JSON 内容，请检查请求格式",
};
class DispatchRejection extends ApiError {}
const invalidInput = () => new DispatchRejection("分派请求输入无效，请检查标识、版本、理由和幂等键。", 0, "invalid_input");
// Provenance of this response only; the controller must retain any earlier unknown outcome.
export const isDispatchRequestDefinitiveRejection = (error: unknown): boolean => error instanceof DispatchRejection;
async function request<T>(path: string, init: RequestInit, expectedStatus: number, valid: (value: unknown) => value is T): Promise<T> {
  const unknownResult = init.method === "POST" ? "分派登记结果未确认，请保留原请求和幂等键" : "当前分派信息未确认，请重新读取";
  let response: Response;
  try { response = await fetch(path, { ...init, redirect: "error" }); }
  catch { throw new ApiError(`${unknownResult}。`, 0, init.signal?.aborted ? "request_canceled" : "disconnected"); }
  const data: unknown = await response.arrayBuffer().then(decodeResponse).catch(() => null);
  if (response.status === expectedStatus && valid(data)) return data;
  if (!response.ok && exactRecord(data, ["error"]) && exactRecord(data.error, ["code", "message"])
    && nonblank(data.error.code) && nonblank(data.error.message)) {
    const definitive = refusalCodes[response.status]?.includes(data.error.code) ?? false;
    const ErrorType = definitive ? DispatchRejection : ApiError;
    throw new ErrorType(`${definitive ? rejectionMessages[response.status] : unknownResult}（HTTP ${response.status}）。`, response.status, data.error.code);
  }
  throw new ApiError(`服务响应不符合约定，${unknownResult}（HTTP ${response.status}）。`, response.status, "invalid_response");
}
const taskPath = (groupId: string, taskId: string) => `/api/v1/groups/${encodeURIComponent(groupId)}/tasks/${encodeURIComponent(taskId)}`;
export async function getDispatchTask(groupId: string, taskId: string, signal?: AbortSignal): Promise<Task> {
  if (!pathIdentifier(groupId) || !pathIdentifier(taskId)) throw invalidInput();
  return request(taskPath(groupId, taskId), { signal }, 200, (value): value is Task => isTask(value, groupId, taskId));
}
export async function getTaskCoordination(groupId: string, taskId: string, signal?: AbortSignal): Promise<TaskCoordinationView> {
  if (!pathIdentifier(groupId) || !pathIdentifier(taskId)) throw invalidInput();
  return request(`${taskPath(groupId, taskId)}/coordination`, { signal }, 200,
    (value): value is TaskCoordinationView => isAuthority(value, groupId, taskId));
}
export async function requestDispatch(groupId: string, taskId: string, input: DispatchRequestInput, key: string): Promise<DispatchRequest> {
  if (!pathIdentifier(groupId) || !pathIdentifier(taskId) || !validKey(key) || !isRecord(input)) throw invalidInput();
  const sent = { expected_task_revision: input.expected_task_revision, expected_authority_generation: input.expected_authority_generation, reason: input.reason };
  if (!positive(sent.expected_task_revision) || !positive(sent.expected_authority_generation) || !nonblank(sent.reason)) throw invalidInput();
  return request(`${taskPath(groupId, taskId)}/dispatch-requests`, {
    method: "POST", headers: { "Content-Type": "application/json", "Idempotency-Key": key }, body: JSON.stringify(sent),
  }, 201, (value): value is DispatchRequest => isRequest(value, groupId, taskId)
    && value.task_revision === sent.expected_task_revision && value.authority_generation === sent.expected_authority_generation && value.reason === sent.reason);
}
export async function getDispatchRequest(groupId: string, taskId: string, requestId: string, signal?: AbortSignal): Promise<DispatchView> {
  if (!pathIdentifier(groupId) || !pathIdentifier(taskId) || !pathIdentifier(requestId)) throw invalidInput();
  return request(`/api/v1/groups/${encodeURIComponent(groupId)}/coordination/dispatch-requests/${encodeURIComponent(requestId)}`, { signal }, 200,
    (value): value is DispatchView => isView(value, groupId, taskId, requestId));
}
