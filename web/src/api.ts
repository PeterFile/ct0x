export type Group = { id: string; name: string; description: string; created_at: string };
export type GroupInput = Readonly<{ name: string; description?: string }>;
export type Member = {
  id: string; group_id: string; name: string; role: string; provider_id: string; model: string;
  creator_id: string; owner_id: string; status: string; workspace: string; created_at: string;
};
export type MemberInput = Readonly<{ name: string; role: string; provider_id?: string; model?: string; workspace?: string }>;
export type Provider = {
  id: string; name: string; path: string; version: string; installation: string; account: string; connectivity: string;
  capabilities: { one_shot: boolean; resume: boolean; tools: boolean };
  evidence: { at: string; type: string; reason: string }[];
};
export type Delivery = {
  id: string; message_id: string; conversation_id: string; recipient_id: string;
  state: string; turn_id: string; error: string; created_at: string;
};
export type Message = {
  id: string; group_id: string; topic_id: string; sender_id: string; content: string; created_at: string; deliveries: Delivery[];
};
export type MessageInput = Readonly<{ content: string; recipient_ids?: readonly string[]; topic_id?: string }>;
type Page<T> = { items: T[]; next_cursor: string };
export class ApiError extends Error {
  constructor(message: string, readonly status = 0, readonly code = "disconnected") { super(message); }
}
export const isDefinitiveRejection = (error: unknown) => error instanceof ApiError
  && [400, 404, 409, 413, 415].includes(error.status);
const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null;
const isGroup = (value: unknown): value is Group => isRecord(value)
  && ["id", "name", "description", "created_at"].every(key => typeof value[key] === "string")
  && value.id !== "" && value.name !== "";
const pageOf = <T>(valid: (value: unknown) => value is T) => (value: unknown): value is Page<T> => isRecord(value)
  && Array.isArray(value.items) && value.items.every(valid) && typeof value.next_cursor === "string";
const memberIn = (groupId: string) => (value: unknown): value is Member => isRecord(value)
  && ["id", "group_id", "name", "role", "provider_id", "model", "creator_id", "owner_id", "status", "workspace", "created_at"]
    .every(key => typeof value[key] === "string") && value.id !== "" && value.group_id === groupId;
const messageIn = (groupId: string) => (value: unknown): value is Message => isRecord(value)
  && ["id", "group_id", "topic_id", "sender_id", "content", "created_at"].every(key => typeof value[key] === "string")
  && value.id !== "" && value.group_id === groupId && Array.isArray(value.deliveries)
  && value.deliveries.every(delivery => isRecord(delivery)
    && ["id", "message_id", "conversation_id", "recipient_id", "state", "turn_id", "error", "created_at"]
      .every(key => typeof delivery[key] === "string") && delivery.id !== "" && delivery.message_id === value.id);
function isProvider(value: unknown): value is Provider {
  if (!isRecord(value) || !["id", "name", "path", "version", "installation", "account", "connectivity"]
    .every(key => typeof value[key] === "string") || value.id === "") return false;
  const capabilities = value.capabilities;
  return isRecord(capabilities) && ["one_shot", "resume", "tools"].every(key => typeof capabilities[key] === "boolean")
    && Array.isArray(value.evidence) && value.evidence.every(entry => isRecord(entry)
      && ["at", "type", "reason"].every(key => typeof entry[key] === "string"));
}

async function request<T>(path: string, init: RequestInit, valid: (value: unknown) => value is T, expectedStatus = 200): Promise<T> {
  let response: Response;
  try { response = await fetch(path, init); }
  catch (error) {
    if (init.signal?.aborted) throw error;
    throw new ApiError("无法连接本地服务，请确认服务已启动。");
  }
  const data: unknown = await response.json().catch(() => null);
  if (!response.ok) {
    const error = isRecord(data) && isRecord(data.error) ? data.error : {};
    const reason = response.status === 400 ? "输入未通过验证，请检查输入信息"
      : response.status === 404 ? "未找到请求的服务或记录"
      : response.status === 409 ? "幂等键与已有请求冲突"
      : response.status === 413 ? "请求内容过大，请缩短输入后重试"
      : response.status === 415 ? "服务仅接受 JSON 内容，请检查请求格式"
      : "服务暂时无法完成请求";
    throw new ApiError(`${reason}（HTTP ${response.status}）。`, response.status,
      typeof error.code === "string" ? error.code : "http_error");
  }
  if (response.status !== expectedStatus || !valid(data)) {
    throw new ApiError(`服务响应不符合约定（HTTP ${response.status}），请重试。`, response.status, "invalid_response");
  }
  return data;
}
function pageQuery(before: string) {
  const query = new URLSearchParams({ limit: "50" });
  if (before) query.set("before", before);
  return `?${query}`;
}
function post<T>(path: string, input: object, key: string, valid: (value: unknown) => value is T, signal?: AbortSignal) {
  return request(path, {
    method: "POST", signal,
    headers: { "Content-Type": "application/json", "Idempotency-Key": key },
    body: JSON.stringify(input),
  }, valid, 201);
}
export function listGroups(before = "", signal?: AbortSignal) {
  return request(`/api/v1/groups${pageQuery(before)}`, { signal }, pageOf(isGroup));
}
export function createGroup(input: GroupInput, key: string, signal?: AbortSignal) {
  return post("/api/v1/groups", input, key, isGroup, signal);
}
const membersPath = (groupId: string) => `/api/v1/groups/${encodeURIComponent(groupId)}/members`;
export function listMembers(groupId: string, before = "", signal?: AbortSignal) {
  return request(`${membersPath(groupId)}${pageQuery(before)}`, { signal }, pageOf(memberIn(groupId)));
}
export function createMember(groupId: string, input: MemberInput, key: string, signal?: AbortSignal) {
  return post(membersPath(groupId), input, key, memberIn(groupId), signal);
}
export function sendMessage(groupId: string, input: MessageInput, key: string, signal?: AbortSignal) {
  return post(`/api/v1/groups/${encodeURIComponent(groupId)}/messages`, {
    content: input.content, recipient_ids: input.recipient_ids, topic_id: input.topic_id,
  }, key, messageIn(groupId), signal);
}
export function listProviders(before = "", signal?: AbortSignal) {
  return request(`/api/v1/providers${pageQuery(before)}`, { signal }, pageOf(isProvider));
}
