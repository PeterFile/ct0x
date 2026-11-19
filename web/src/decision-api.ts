import { ApiError } from "./api";

type TaskOrigin = Readonly<{
  task_id: string; task_title: string; task_revision: number; assignment_id: string;
  mandate_id: string; authority_generation: number; owner_member_id: string; origin_trigger_id: string;
}>;
type ChatOrigin = Readonly<{ source_id: string; candidate_id: string; assessment_id: string; notice_message_id: string }>;
export type PendingDecision = Readonly<{
  id: string; version: number; group_id: string; topic_id: string; group_name: string; recipient_id: "user";
  source_message_id: string; source_sequence: number; author_member_id: string; author_turn_id: string; author_attempt_id?: string;
  facts: string; question: string; recommendation: string; choices?: readonly string[];
  authorization?: Readonly<{ scope: string; impact: string }>;
  created_at: string; updated_at: string; expires_at?: string;
} & ({ kind: "decision" | "authorization" | "unblock"; relay: "task_owner"; task: TaskOrigin; chat?: never }
  | { kind: "clarification"; relay: "origin_thread"; chat: ChatOrigin; task?: never })>;
export type DecisionSelection = Readonly<{ option_index: number; text?: never } | { text: string; option_index?: never }>;
export type DecisionReceipt = Readonly<{
  id: string; request_id: string; group_id: string; topic_id: string; version: number; actor_id: "user";
  task_id: string; task_revision: number; option_index?: number; text: string;
  reply_message_id: string; wake_delivery_id: string; wake_message_id: string; created_at: string;
}>;
type DecisionPage = { items: PendingDecision[]; next_cursor: string };

const record = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);
const nonblank = (value: unknown): value is string => typeof value === "string" && /\P{White_Space}/u.test(value);
const id = (value: unknown): value is string => nonblank(value) && !/[\0\r\n\ud800-\udfff]/u.test(value);
const pathId = (value: unknown): value is string => id(value) && value !== "." && value !== "..";
const positive = (value: unknown): value is number => typeof value === "number" && Number.isSafeInteger(value) && value > 0;
const timestamp = (value: unknown): value is string => typeof value === "string"
  && /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z$/.test(value)
  && !/^0001-01-01T00:00:00(?:\.0+)?Z$/.test(value) && Number.isFinite(Date.parse(value));
const taskOrigin = (value: unknown): value is TaskOrigin => record(value) && pathId(value.task_id) && nonblank(value.task_title)
  && positive(value.task_revision) && positive(value.authority_generation)
  && [value.assignment_id, value.mandate_id, value.owner_member_id, value.origin_trigger_id].every(id);
const chatOrigin = (value: unknown): value is ChatOrigin => record(value)
  && [value.source_id, value.candidate_id, value.assessment_id, value.notice_message_id].every(id);
function pendingDecision(value: unknown): value is PendingDecision {
  if (!record(value) || !pathId(value.id) || !pathId(value.group_id) || typeof value.topic_id !== "string"
    || !nonblank(value.group_name) || value.recipient_id !== "user" || !positive(value.version) || !positive(value.source_sequence)
    || ![value.source_message_id, value.author_member_id, value.author_turn_id].every(id)
    || (value.author_attempt_id !== undefined && !id(value.author_attempt_id))
    || !timestamp(value.created_at) || !timestamp(value.updated_at) || (value.expires_at !== undefined && !timestamp(value.expires_at))
    || ![value.facts, value.question, value.recommendation].every(nonblank)
    || (value.choices !== undefined && (!Array.isArray(value.choices) || value.choices.length > 4 || !value.choices.every(nonblank)))) return false;
  if (value.kind === "authorization") {
    if (!record(value.authorization) || !nonblank(value.authorization.scope) || !nonblank(value.authorization.impact)) return false;
  } else if (value.authorization !== undefined) return false;
  return value.kind === "clarification"
    ? value.relay === "origin_thread" && value.task === undefined && chatOrigin(value.chat)
    : (value.kind === "decision" || value.kind === "authorization" || value.kind === "unblock")
      && value.relay === "task_owner" && value.chat === undefined && taskOrigin(value.task);
}

async function request<T>(path: string, init: RequestInit, valid: (value: unknown) => value is T): Promise<T> {
  let response: Response;
  try { response = await fetch(path, { ...init, cache: "no-store", redirect: "error" }); }
  catch (error) {
    if (init.signal?.aborted) throw error;
    throw new ApiError("无法连接本地服务，请重试。");
  }
  const value: unknown = await response.json().catch(() => null);
  if (!response.ok) {
    const failure = record(value) && record(value.error) ? value.error : {};
    const reason = response.status === 400 ? "答复内容未通过验证"
      : response.status === 404 || response.status === 409 ? "事项当前不可答复或已有不同答复"
      : response.status === 413 ? "答复内容过长" : "服务暂时无法完成请求";
    throw new ApiError(`${reason}（HTTP ${response.status}）。`, response.status,
      typeof failure.code === "string" ? failure.code : "http_error");
  }
  if (response.status !== 200 || !valid(value)) {
    throw new ApiError("服务响应不符合约定，请重试。", response.status, "invalid_response");
  }
  return value;
}

export function listPendingDecisions(before: string, signal: AbortSignal) {
  const query = new URLSearchParams({ limit: "50" });
  if (before) query.set("before", before);
  return request(`/api/v1/me/pending-decisions?${query}`, { signal }, (value): value is DecisionPage => record(value)
    && Array.isArray(value.items) && value.items.length <= 50 && value.items.every(pendingDecision)
    && new Set(value.items.map(item => item.id)).size === value.items.length
    && typeof value.next_cursor === "string" && (value.next_cursor === "" || value.next_cursor !== before));
}

type GroupIdentity = { group_id: string; workspace_id: string; project_id: string; channel_id: string };
type ProjectLabel = { id: string; name: string };
export type DecisionProject = Readonly<{ id: string; name?: string }>;
function coalesce<T>(cache: Map<string, Promise<T>>, key: string, read: () => Promise<T>) {
  const previous = cache.get(key);
  if (previous) return previous;
  const pending = read();
  cache.set(key, pending);
  return pending;
}

// Only an exact Group -> Project relationship authorizes a project label.
export function decisionProjects(signal: AbortSignal) {
  const groups = new Map<string, Promise<DecisionProject | undefined>>();
  const projects = new Map<string, Promise<ProjectLabel | undefined>>();
  return (groupId: string) => coalesce(groups, groupId, async () => {
    const identity = await request(`/api/v1/groups/${encodeURIComponent(groupId)}/identity`, { signal }, (value): value is GroupIdentity =>
      record(value) && value.group_id === groupId && id(value.workspace_id) && pathId(value.project_id) && id(value.channel_id))
      .catch(() => undefined);
    if (!identity || signal.aborted) return;
    const projectId = identity.project_id;
    const project = await coalesce(projects, projectId, () => request(`/api/v1/projects/${encodeURIComponent(projectId)}`, { signal },
      (value): value is ProjectLabel => record(value) && value.id === projectId && nonblank(value.name)).catch(() => undefined));
    return { id: projectId, name: project?.name };
  });
}

// Match the Go text contract in UTF-8 bytes without trimming the statement.
export const validDecisionText = (value: unknown): value is string => nonblank(value) && value.length <= 4096
  && !/[\0\ud800-\udfff]/u.test(value) && new TextEncoder().encode(value).length <= 4096;

export function prepareDecisionAnswer(item: PendingDecision, selection: DecisionSelection, key: string, groupId?: string) {
  const { id: requestId, group_id, topic_id, version, recipient_id, task } = item;
  const option_index = selection.option_index;
  const text = option_index === undefined ? selection.text : item.choices?.[option_index - 1];
  if (!pathId(requestId) || !pathId(group_id) || (groupId !== undefined && groupId !== group_id)
    || !positive(version) || !validDecisionText(text)
    || (option_index !== undefined && (!positive(option_index) || selection.text !== undefined))) {
    throw new ApiError("答复内容或来源不符合约定。", 400);
  }
  const taskId = task?.task_id ?? "", taskRevision = task?.task_revision ?? 0;
  const base = { group_id, topic_id, version };
  const body = JSON.stringify(option_index === undefined ? { ...base, text } : { ...base, option_index });
  const scope = groupId === undefined ? "/api/v1/me" : `/api/v1/groups/${encodeURIComponent(groupId)}`;
  const path = `${scope}/decisions/${encodeURIComponent(requestId)}/answer`;
  // Snapshot the envelope and receipt binding once. A caller can only replay it as-is.
  return async () => Object.freeze(await request(path, {
    method: "POST", headers: { "Content-Type": "application/json", "Idempotency-Key": key }, body,
  }, (value): value is DecisionReceipt => record(value)
    && value.request_id === requestId && value.group_id === group_id && value.topic_id === topic_id && value.version === version
    && value.actor_id === recipient_id && value.task_id === taskId && value.task_revision === taskRevision
    && value.option_index === option_index && value.text === text && timestamp(value.created_at)
    && [value.id, value.reply_message_id, value.wake_delivery_id, value.wake_message_id].every(id)));
}
