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
export type DecisionHandling = Readonly<{ turn_id: string; attempt_id: string; consumed_at: string }>;
export type DecisionAnswerView = Readonly<{ receipt: DecisionReceipt; handling: DecisionHandling | null }>;
export type DecisionReference = Readonly<Pick<DecisionReceipt, "request_id" | "group_id" | "topic_id" | "version" | "task_id" | "task_revision">>;
export type DecisionAttempt = Readonly<{ ref: DecisionReference; scope: "me" | "group"; key: string; digest: string }>;
export type DecisionHistory = Readonly<{ ref: DecisionReference; receiptDigest: string }>;
export type DecisionJournalEntry = DecisionAttempt | DecisionHistory;
type DecisionPage = { items: PendingDecision[]; next_cursor: string };

const record = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);
const nonblank = (value: unknown): value is string => typeof value === "string" && /\P{White_Space}/u.test(value);
const id = (value: unknown): value is string => nonblank(value) && !/[\0\r\n\ud800-\udfff]/u.test(value);
const pathId = (value: unknown): value is string => id(value) && value !== "." && value !== "..";
const positive = (value: unknown): value is number => typeof value === "number" && Number.isSafeInteger(value) && value > 0;
export const decisionReference = (item: PendingDecision): DecisionReference => ({
  request_id: item.id, group_id: item.group_id, topic_id: item.topic_id, version: item.version,
  task_id: item.task?.task_id ?? "", task_revision: item.task?.task_revision ?? 0,
});
const binding = (ref: DecisionReference) => [ref.group_id, ref.topic_id, ref.request_id, ref.task_id, ref.task_revision];
export const requestKey = (ref: DecisionReference) => JSON.stringify(binding(ref));
export const decisionKey = (ref: DecisionReference) => JSON.stringify([...binding(ref), ref.version]);
// A write lock is not a receipt binding. The backend allows one answer even
// if the version or its underlying task revision becomes stale.
export const decisionWriteSlot = (ref: DecisionReference) => JSON.stringify([ref.group_id, ref.request_id]);
export const validDecisionReference = (value: unknown): value is DecisionReference => record(value)
  && Object.keys(value).length === 6 && pathId(value.request_id) && pathId(value.group_id)
  && typeof value.topic_id === "string" && positive(value.version)
  && (value.task_id === "" ? value.task_revision === 0 : pathId(value.task_id) && positive(value.task_revision));
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
  init.signal?.throwIfAborted();
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

async function boundedGet<T>(path: string, signal: AbortSignal, valid: (value: unknown) => value is T) {
  signal.throwIfAborted();
  const controller = new AbortController();
  const stop = () => controller.abort();
  const timeout = setTimeout(stop, 5000);
  signal.addEventListener("abort", stop, { once: true });
  try {
    return await request(path, { method: "GET", signal: controller.signal }, valid);
  } finally {
    clearTimeout(timeout);
    signal.removeEventListener("abort", stop);
  }
}

type GroupIdentity = { group_id: string; workspace_id: string; project_id: string; channel_id: string };
type ProjectProof = { id: string; workspace_id: string; name: string };
export type DecisionProject = Readonly<Pick<ProjectProof, "id" | "name">>;

// This resolver belongs to one active view scope. Optional failures are cached in
// that scope too, so repeated list loads don't fan out another set of reads.
export function decisionProjects(signal: AbortSignal, resolved: (groupId: string, project: DecisionProject) => void) {
  const groups = new Set<string>();
  const queue: string[] = [];
  const projects = new Map<string, Promise<ProjectProof>>();
  let running = 0;
  async function lookup(groupId: string) {
    const identity = await boundedGet(`/api/v1/groups/${encodeURIComponent(groupId)}/identity`, signal,
      (value): value is GroupIdentity => record(value) && value.group_id === groupId
        && id(value.workspace_id) && pathId(value.project_id) && id(value.channel_id));
    if (signal.aborted) return;
    const { project_id, workspace_id } = identity;
    const key = JSON.stringify([workspace_id, project_id]);
    let proof = projects.get(key);
    if (!proof) {
      proof = boundedGet(`/api/v1/projects/${encodeURIComponent(project_id)}`, signal,
        (value): value is ProjectProof => record(value) && value.id === project_id
          && value.workspace_id === workspace_id && nonblank(value.name));
      projects.set(key, proof);
    }
    const { id: projectId, name } = await proof;
    if (!signal.aborted) resolved(groupId, { id: projectId, name });
  }
  function drain() {
    while (!signal.aborted && running < 4 && queue.length) {
      running++;
      void lookup(queue.shift()!).catch(() => { /* A label is not required to answer. */ }).finally(() => { running--; drain(); });
    }
  }
  return (groupIds: Iterable<string>) => {
    for (const groupId of groupIds) {
      if (signal.aborted || queue.length >= 100) break;
      if (groups.has(groupId)) continue;
      groups.add(groupId);
      queue.push(groupId);
    }
    drain();
  };
}

// Match the Go text contract in UTF-8 bytes without trimming the statement.
export const validDecisionText = (value: unknown): value is string => nonblank(value) && value.length <= 4096
  && !/[\0\ud800-\udfff]/u.test(value) && new TextEncoder().encode(value).length <= 4096;

const answerPath = (ref: DecisionReference) => `/decisions/${encodeURIComponent(ref.request_id)}/answer`;
function answerBody({ group_id, topic_id, version }: DecisionReference, text: string, option_index?: number) {
  const base = { group_id, topic_id, version };
  return JSON.stringify(option_index === undefined ? { ...base, text } : { ...base, option_index });
}
const matchesReference = (value: unknown, ref: DecisionReference): value is DecisionReceipt => record(value)
  && value.request_id === ref.request_id && value.group_id === ref.group_id && value.topic_id === ref.topic_id
  && value.version === ref.version && value.task_id === ref.task_id && value.task_revision === ref.task_revision
  && value.actor_id === "user" && validDecisionText(value.text) && timestamp(value.created_at)
  && (value.option_index === undefined || (positive(value.option_index) && value.option_index <= 4))
  && [value.id, value.reply_message_id, value.wake_delivery_id, value.wake_message_id].every(id);
async function digest(fields: unknown[]) {
  const bytes = new TextEncoder().encode(JSON.stringify(fields));
  const hash = await crypto.subtle.digest("SHA-256", bytes);
  return Array.from(new Uint8Array(hash), byte => byte.toString(16).padStart(2, "0")).join("");
}
const answerDigest = (ref: DecisionReference, scope: DecisionAttempt["scope"], key: string, body: string, text: string) =>
  digest(["decision-attempt", decisionKey(ref), scope, key, body, text]);
const receiptFields = (receipt: DecisionReceipt) => [receipt.id, receipt.reply_message_id, receipt.wake_delivery_id,
  receipt.wake_message_id, receipt.created_at, receipt.option_index, receipt.text];
const receiptDigest = (ref: DecisionReference, receipt: DecisionReceipt) => digest(["decision-receipt", decisionKey(ref), ...receiptFields(receipt)]);
export async function decisionHistory(ref: DecisionReference, receipt: DecisionReceipt): Promise<DecisionHistory> {
  return { ref, receiptDigest: await receiptDigest(ref, receipt) };
}

export async function prepareDecisionAnswer(item: PendingDecision, selection: DecisionSelection, key: string, groupId?: string) {
  const ref = decisionReference(item);
  const option_index = selection.option_index;
  const text = option_index === undefined ? selection.text : item.choices?.[option_index - 1];
  if (!validDecisionReference(ref) || (groupId !== undefined && groupId !== ref.group_id)
    || !validDecisionText(text) || (option_index !== undefined && (!positive(option_index) || selection.text !== undefined))) {
    throw new ApiError("答复内容或来源不符合约定。", 400);
  }
  const scope = groupId === undefined ? "me" : "group";
  const path = scope === "me" ? "/api/v1/me" : `/api/v1/groups/${encodeURIComponent(ref.group_id)}`;
  const body = answerBody(ref, text, option_index);
  const attempt: DecisionAttempt = { ref, scope, key, digest: await answerDigest(ref, scope, key, body, text) };
  return {
    attempt,
    post: async () => Object.freeze(await request(`${path}${answerPath(ref)}`, {
      method: "POST", headers: { "Content-Type": "application/json", "Idempotency-Key": key }, body,
    }, (value): value is DecisionReceipt => matchesReference(value, ref) && value.option_index === option_index && value.text === text)),
  };
}

// The historical local-user GET is the same for either POST alias. An exact 404
// is not a positive result or permission to start a different answer.
export async function readDecisionAnswer(entry: DecisionJournalEntry, signal: AbortSignal, saved?: DecisionReceipt): Promise<DecisionAnswerView | null> {
  const { ref } = entry, { group_id, topic_id, version } = ref;
  const query = new URLSearchParams({ group_id, topic_id, version: String(version) });
  let view: DecisionAnswerView;
  try {
    view = await boundedGet(`/api/v1/me${answerPath(ref)}?${query}`, signal,
      (value): value is DecisionAnswerView => {
        if (!record(value) || !matchesReference(value.receipt, ref)) return false;
        const { handling } = value;
        return handling === null || (record(handling) && id(handling.turn_id) && id(handling.attempt_id) && timestamp(handling.consumed_at));
      });
  } catch (error) {
    if (!signal.aborted && error instanceof ApiError && error.status === 404 && error.code === "not_found") return null;
    throw error;
  }
  const { receipt, handling } = view;
  const matches = saved ? JSON.stringify(receiptFields(receipt)) === JSON.stringify(receiptFields(saved))
    : "key" in entry ? await answerDigest(ref, entry.scope, entry.key, answerBody(ref, receipt.text, receipt.option_index), receipt.text) === entry.digest
    : await receiptDigest(ref, receipt) === entry.receiptDigest;
  signal.throwIfAborted();
  if (!matches) throw new ApiError("服务返回的答复与本地记录不一致。", 200, "invalid_response");
  return { receipt: Object.freeze(receipt), handling: handling && Object.freeze(handling) };
}
