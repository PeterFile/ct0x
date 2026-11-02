import { ApiError } from "./api";

export type NewTaskRequest = Readonly<{
  title: string; description?: string; acceptance: string; assignee_id?: string;
  dependency_ids?: readonly string[] | null;
}>;
export type RequestWorkInput = Readonly<{
  topic_id: string; message_id: string; mode: "create_new"; reason: string;
  new_task: NewTaskRequest; existing_task?: never;
}>;
export type TaskScope = Readonly<{
  objective: string; description: string; acceptance: string; assignee_id: string; dependency_ids: readonly string[];
}>;
export type RoutingDecision = Readonly<{
  id: string; trigger_id: string; group_id: string; topic_id: string; kind: "create_new";
  actor_id: "user"; policy_revision: "local-user-explicit/v1"; reason: string; task_id: string;
  task_revision_at_admission: number; mandate_id: string; created_at: string;
}>;
export type Mandate = Readonly<{
  id: string; decision_id: string; origin_trigger_id: string; group_id: string; topic_id: string; task_id: string;
  task_revision_at_admission: number; authority_kind: "local_user_explicit"; authorized_by: "user";
  strength: "explicit"; policy_revision: "local-user-explicit/v1"; scope: TaskScope; created_at: string;
}>;
export type TaskRef = Readonly<{ id: string; group_id: string; revision_at_admission: number }>;
export type AdmissionReceipt = Readonly<{ decision: RoutingDecision; mandate: Mandate; task: TaskRef }>;

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);
const exactRecord = (value: unknown, fields: readonly string[]): value is Record<string, unknown> => isRecord(value)
  && Object.keys(value).length === fields.length && fields.every(field => Object.hasOwn(value, field));
// Go strings.TrimSpace uses Unicode White_Space: NEL is space; BOM is not.
const goSpace = "[\\u0009-\\u000d \\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000]";
const goBlank = new RegExp(`^${goSpace}*$`, "u");
const goEdgeSpace = new RegExp(`^${goSpace}|${goSpace}$`, "u");
// Go JSON decoding replaces lone JS surrogates, so they cannot round-trip exact text.
const validText = (value: unknown): value is string => typeof value === "string" && !/[\ud800-\udfff]/u.test(value);
const nonblank = (value: unknown): value is string => validText(value) && !goBlank.test(value);
const encoder = new TextEncoder();
const validIdentifier = (value: unknown): value is string => validText(value) && value !== ""
  && !goEdgeSpace.test(value) && !/[\u0000\r\n]/u.test(value) && encoder.encode(value).length <= 4096;
const validKey = (value: unknown): value is string => typeof value === "string" && value.length >= 1 && value.length <= 128
  && !/[^\x21-\x7e]|,/.test(value);
const invalidInput = () => new ApiError("工作请求输入无效，请检查来源、任务内容及幂等键。", 0, "invalid_input");

function requestSnapshot(input: RequestWorkInput): RequestWorkInput {
  if (!isRecord(input) || input.mode !== "create_new" || "existing_task" in input || !isRecord(input.new_task)) throw invalidInput();
  const task = input.new_task;
  // Copy only known fields before fetch; caller mutations cannot change receipt association.
  const sent: RequestWorkInput = {
    topic_id: input.topic_id, message_id: input.message_id, mode: input.mode, reason: input.reason,
    new_task: { title: task.title, description: task.description, acceptance: task.acceptance, assignee_id: task.assignee_id,
      dependency_ids: Array.isArray(task.dependency_ids) ? [...task.dependency_ids] : task.dependency_ids },
  };
  const n = sent.new_task;
  if (!validText(sent.topic_id) || !validIdentifier(sent.message_id) || !nonblank(sent.reason)
    || !nonblank(n.title) || !nonblank(n.acceptance)
    || (n.description !== undefined && !validText(n.description))
    || (n.assignee_id !== undefined && n.assignee_id !== "" && !nonblank(n.assignee_id))
    || (n.dependency_ids != null && (!Array.isArray(n.dependency_ids) || !n.dependency_ids.every(nonblank)))) throw invalidInput();
  return sent;
}

function writerTime(value: unknown): value is string {
  if (typeof value !== "string") return false;
  const match = /^(\d{4})-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.(\d{1,9}))?Z$/.exec(value);
  if (!match || match[0] !== value || match[1] === "0000") return false;
  const date = new Date(value);
  return Number.isFinite(date.valueOf()) && date.toISOString().slice(0, 19) === value.slice(0, 19)
    && !(value.startsWith("0001-01-01T00:00:00") && !/[1-9]/.test(match[2] ?? ""));
}

const decisionFields = ["id", "trigger_id", "group_id", "topic_id", "kind", "actor_id", "policy_revision", "reason",
  "task_id", "task_revision_at_admission", "mandate_id", "created_at"];
const mandateFields = ["id", "decision_id", "origin_trigger_id", "group_id", "topic_id", "task_id", "task_revision_at_admission",
  "authority_kind", "authorized_by", "strength", "policy_revision", "scope", "created_at"];
const scopeFields = ["objective", "description", "acceptance", "assignee_id", "dependency_ids"];

function receiptMatches(value: unknown, groupId: string, sent: RequestWorkInput): value is AdmissionReceipt {
  if (!exactRecord(value, ["decision", "mandate", "task"])) return false;
  const { decision: d, mandate: m, task: t } = value;
  if (!exactRecord(d, decisionFields) || !exactRecord(m, mandateFields)
    || !exactRecord(t, ["id", "group_id", "revision_at_admission"]) || !exactRecord(m.scope, scopeFields)) return false;
  const scope = m.scope, n = sent.new_task;
  // Compare sets, not JS UTF-16 sorting; Go canonicalizes by UTF-8 bytes.
  const dependencies = new Set(n.dependency_ids ?? []);
  return validIdentifier(d.id) && validIdentifier(d.trigger_id) && validIdentifier(m.id) && validIdentifier(t.id)
    && d.group_id === groupId && m.group_id === groupId && t.group_id === groupId
    && d.topic_id === sent.topic_id && m.topic_id === sent.topic_id
    && d.kind === "create_new" && d.actor_id === "user" && m.authorized_by === "user"
    && d.policy_revision === "local-user-explicit/v1" && m.policy_revision === "local-user-explicit/v1"
    && m.authority_kind === "local_user_explicit" && m.strength === "explicit"
    && d.task_revision_at_admission === 1 && m.task_revision_at_admission === 1 && t.revision_at_admission === 1
    && d.mandate_id === m.id && m.decision_id === d.id && d.trigger_id === m.origin_trigger_id
    && d.task_id === t.id && m.task_id === t.id && d.reason === sent.reason
    && writerTime(d.created_at) && d.created_at === m.created_at
    && scope.objective === n.title && scope.description === (n.description ?? "") && scope.acceptance === n.acceptance
    && scope.assignee_id === (n.assignee_id ?? "") && Array.isArray(scope.dependency_ids)
    && scope.dependency_ids.length === dependencies.size && new Set(scope.dependency_ids).size === dependencies.size
    && scope.dependency_ids.every(id => typeof id === "string" && dependencies.has(id));
}

const rejectionMessages: Readonly<Record<number, string>> = {
  400: "工作请求输入未通过验证，请检查输入信息",
  403: "本地访问校验失败，无法提交工作请求",
  404: "未找到工作请求的来源或关联记录",
  405: "服务不支持此工作请求方式",
  409: "工作请求与已有记录或任务状态冲突",
  413: "工作请求内容过大，请缩短输入",
  415: "服务仅接受 JSON 内容，请检查请求格式",
};
const unknownResult = "工作请求结果未知，请保留原请求和幂等键";
// Only these responses (or local preflight validation) permit releasing a frozen operation.
export const isRequestWorkDefinitiveRejection = (error: unknown) => error instanceof ApiError
  && ((error.status === 0 && error.code === "invalid_input") || Object.hasOwn(rejectionMessages, error.status));

// A single atomic admission command. Key generation, retries and live Task reads belong to the controller.
export async function requestWork(groupId: string, input: RequestWorkInput, key: string, signal?: AbortSignal): Promise<AdmissionReceipt> {
  if (!validKey(key) || !validIdentifier(groupId)) throw invalidInput();
  const sent = requestSnapshot(input);
  let response: Response;
  try {
    response = await fetch(`/api/v1/groups/${encodeURIComponent(groupId)}/coordination/requests`, {
      method: "POST", signal, headers: { "Content-Type": "application/json", "Idempotency-Key": key }, body: JSON.stringify(sent),
    });
  } catch (error) {
    const canceled = signal?.aborted || (isRecord(error) && error.name === "AbortError");
    throw new ApiError(`${unknownResult}。`, 0, canceled ? "request_canceled" : "disconnected");
  }
  const data: unknown = await response.json().catch(() => null);
  if (!response.ok) {
    const error = isRecord(data) && isRecord(data.error) ? data.error : {};
    throw new ApiError(`${rejectionMessages[response.status] ?? unknownResult}（HTTP ${response.status}）。`, response.status,
      typeof error.code === "string" ? error.code : "http_error");
  }
  if (response.status !== 201 || !receiptMatches(data, groupId, sent)) {
    throw new ApiError(`工作请求回执不符合约定，结果未知，请保留原请求和幂等键（HTTP ${response.status}）。`, response.status, "invalid_response");
  }
  return data;
}
