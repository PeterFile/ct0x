import { hasMessageSourceIdentityConflict, hasValidMessageSource } from "./message-source";
import type { MessageSource } from "./message-source";

export type Group = { id: string; name: string; description: string; created_at: string };
export type GroupInput = Readonly<{ name: string; description?: string }>;
export type Member = {
  id: string; group_id: string; name: string; role: string; provider_id: string; model: string;
  creator_id: string; owner_id: string; status: string; workspace: string; created_at: string;
};
export type MemberInput = Readonly<{ name: string; role: string; provider_id?: string; model?: string; workspace?: string }>;
export type AdapterProtocol = "one_shot" | "pi_extension" | "codex_app_server";
export type AdapterSupport = "supported" | "unsupported" | "unknown";
export type ProviderAdapterCapabilities = {
  platform_tools: AdapterSupport; native_resume: AdapterSupport; permission_interaction: AdapterSupport;
  cancel: AdapterSupport; terminal_evidence: AdapterSupport; steer: AdapterSupport;
  evidence: { source: "builtin_contract"; provider_id: string; name: string; path: string; version: string; protocol: AdapterProtocol };
};
export type ProviderAdapter = {
  id: string; protocol: AdapterProtocol; enabled: boolean; capabilities?: ProviderAdapterCapabilities;
};
export type Provider = {
  id: string; name: string; path: string; version: string; installation: string; account: string; connectivity: string;
  capabilities: { one_shot: boolean; resume: boolean; tools: boolean };
  evidence: { at: string; type: string; reason: string }[];
  adapter?: ProviderAdapter;
};
export type Delivery = {
  id: string; message_id: string; conversation_id: string; recipient_id: string;
  state: string; turn_id: string; error: string; created_at: string;
};
export type MessageDiscussion = Readonly<{
  kind: "invite" | "reply" | "resolve";
  root_message_id: string; invitation_message_id: string; parent_invitation_message_id?: string;
  inviter_id: string; assignee_id: string; expected_reply: string;
}>;
export type Message = MessageSource & {
  id: string; group_id: string; topic_id: string; sender_id: string; content: string; created_at: string; deliveries: Delivery[];
  discussion?: MessageDiscussion | null;
  discussion_closure_reason?: string;
};
export type WireMessage = Omit<Message, "discussion" | "discussion_closure_reason"> & {
  discussion?: unknown; discussion_closure_reason?: unknown;
};
export type MessageInput = Readonly<{ content: string; recipient_ids?: readonly string[]; topic_id?: string }>;
type Page<T> = { items: T[]; next_cursor: string };
export class ApiError extends Error {
  constructor(message: string, readonly status = 0, readonly code = "disconnected") { super(message); }
}
// Only the exact Message read's client-side identity check can create this signal.
class MessageSourceIdentityConflict extends ApiError {}
export const isMessageSourceIdentityConflict = (error: unknown) => error instanceof MessageSourceIdentityConflict;
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
export const messageIn = (groupId: string, topicId?: string) => (value: unknown): value is WireMessage => isRecord(value) && !Array.isArray(value)
  && ["id", "group_id", "topic_id", "sender_id", "content", "created_at"].every(key => typeof value[key] === "string")
  && value.id !== "" && value.group_id === groupId && (topicId === undefined || value.topic_id === topicId)
  && hasValidMessageSource(value) && Array.isArray(value.deliveries)
  && value.deliveries.every(delivery => isRecord(delivery) && !Array.isArray(delivery)
    && ["id", "message_id", "conversation_id", "recipient_id", "state", "turn_id", "error", "created_at"]
      .every(key => typeof delivery[key] === "string") && delivery.id !== "" && delivery.message_id === value.id);
const boundedDiscussionText = (value: unknown, maxBytes: number): value is string => typeof value === "string"
  && value.length <= maxBytes && !/[\0\ud800-\udfff]/u.test(value) && new TextEncoder().encode(value).length <= maxBytes;
const discussionId = (value: unknown): value is string => boundedDiscussionText(value, 4096)
  && value !== "" && !/^\p{White_Space}|\p{White_Space}$|[\r\n]/u.test(value);
function discussionFor(value: unknown, message: Omit<Message, "discussion">): MessageDiscussion | undefined {
  if (!isRecord(value) || Array.isArray(value)) return;
  const { kind, root_message_id, invitation_message_id, parent_invitation_message_id, inviter_id, assignee_id, expected_reply } = value;
  if ((kind !== "invite" && kind !== "reply" && kind !== "resolve")
    || !discussionId(root_message_id) || !discussionId(invitation_message_id)
    || !discussionId(inviter_id) || !discussionId(assignee_id) || !boundedDiscussionText(expected_reply, 16 * 1024)
    || inviter_id === assignee_id || inviter_id === "user" || assignee_id === "user"
    || message.sender_id !== (kind === "reply" ? assignee_id : inviter_id)
    || (message.source_kind !== undefined && message.source_kind !== "unknown" && message.source_kind !== "member")
    || root_message_id === message.id || root_message_id === invitation_message_id
    || (kind === "invite") !== (invitation_message_id === message.id)
    || (parent_invitation_message_id !== undefined && (!discussionId(parent_invitation_message_id)
      || parent_invitation_message_id === root_message_id || parent_invitation_message_id === invitation_message_id
      || parent_invitation_message_id === message.id))) return;
  return { kind, root_message_id, invitation_message_id, inviter_id, assignee_id, expected_reply,
    ...(parent_invitation_message_id === undefined ? {} : { parent_invitation_message_id }) };
}
function discussionClosureFor(value: unknown, message: Omit<Message, "discussion">, relation: MessageDiscussion): string | undefined {
  if (relation.kind === "resolve" || message.source_kind !== "member" || !discussionId(message.source_id) || message.deliveries.length !== 1
    || !boundedDiscussionText(value, 256) || value === "" || value.trim() !== value || /[\p{Cc}\p{Cf}\u2028\u2029]/u.test(value)) return;
  const [delivery] = message.deliveries;
  const recipient = relation.kind === "invite" ? relation.assignee_id : relation.inviter_id;
  if (delivery.message_id === message.id && delivery.recipient_id === recipient
    && (delivery.state === "accepted" || delivery.state === "failed")) return value;
}
export function decodeMessage({ discussion, discussion_closure_reason, ...message }: WireMessage): Message {
  // Invalid optional evidence must not conceal an otherwise valid public row or change its receipt.
  if (discussion === null) return { ...message, discussion };
  const relation = discussionFor(discussion, message);
  if (!relation) return message;
  const reason = discussionClosureFor(discussion_closure_reason, message, relation);
  return { ...message, discussion: relation, ...(reason === undefined ? {} : { discussion_closure_reason: reason }) };
}
function validProviderAdapter(value: unknown, current: Record<string, unknown>): value is ProviderAdapter {
  if (!isRecord(value) || typeof value.id !== "string" || !value.id || value.id.trim() !== value.id || value.id.includes("\0")
    || !["one_shot", "pi_extension", "codex_app_server"].some(protocol => protocol === value.protocol)
    || typeof value.enabled !== "boolean") return false;
  if (!("capabilities" in value)) return true;
  if (typeof current.version !== "string" || !current.version.trim()) return false;
  const capabilities = value.capabilities;
  if (!isRecord(capabilities) || !["platform_tools", "native_resume", "permission_interaction", "cancel", "terminal_evidence", "steer"]
    .every(key => ["supported", "unsupported", "unknown"].some(state => state === capabilities[key]))) return false;
  const evidence = capabilities.evidence;
  return isRecord(evidence) && evidence.source === "builtin_contract" && evidence.provider_id === current.id
    && ["name", "path", "version"].every(key => evidence[key] === current[key]) && evidence.protocol === value.protocol;
}
function isProvider(value: unknown): value is Provider {
  if (!isRecord(value) || !["id", "name", "path", "version", "installation", "account", "connectivity"]
    .every(key => typeof value[key] === "string") || value.id === "") return false;
  const capabilities = value.capabilities;
  return isRecord(capabilities) && ["one_shot", "resume", "tools"].every(key => typeof capabilities[key] === "boolean")
    && Array.isArray(value.evidence) && value.evidence.every(entry => isRecord(entry)
      && ["at", "type", "reason"].every(key => typeof entry[key] === "string"))
    && (!("adapter" in value) || validProviderAdapter(value.adapter, value));
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
    method: "POST", redirect: "error", signal,
    headers: { "Content-Type": "application/json", "Idempotency-Key": key },
    body: JSON.stringify(input),
  }, valid, 201);
}
export function listGroups(before = "", signal?: AbortSignal) {
  return request(`/api/v1/groups${pageQuery(before)}`, { signal }, pageOf(isGroup));
}
export function createGroup(input: GroupInput, key: string, signal?: AbortSignal) {
  const sent = Object.freeze({ ...input });
  return post("/api/v1/groups", sent, key, (value): value is Group => isGroup(value)
    && value.name === sent.name && value.description === (sent.description ?? ""), signal);
}
const membersPath = (groupId: string) => `/api/v1/groups/${encodeURIComponent(groupId)}/members`;
export function listMembers(groupId: string, before = "", signal?: AbortSignal) {
  return request(`${membersPath(groupId)}${pageQuery(before)}`, { signal }, pageOf(memberIn(groupId)));
}
export function createMember(groupId: string, input: MemberInput, key: string, signal?: AbortSignal) {
  const sent = Object.freeze({ ...input });
  return post(membersPath(groupId), sent, key, (value): value is Member => memberIn(groupId)(value)
    && value.name === sent.name && value.role === sent.role && value.provider_id === (sent.provider_id ?? "")
    && value.model === (sent.model ?? "") && value.workspace === (sent.workspace ?? ""), signal);
}
const messagesPath = (groupId: string) => `/api/v1/groups/${encodeURIComponent(groupId)}/messages`;
export function sendMessage(groupId: string, input: MessageInput, key: string, signal?: AbortSignal) {
  const sent = Object.freeze({ content: input.content,
    recipient_ids: Array.isArray(input.recipient_ids) ? Object.freeze([...input.recipient_ids]) : input.recipient_ids, topic_id: input.topic_id });
  return post(messagesPath(groupId), sent, key, (value): value is WireMessage => messageIn(groupId, sent.topic_id ?? "")(value)
    && value.content === sent.content, signal).then(decodeMessage);
}
export async function listMessages(groupId: string, before = "", signal?: AbortSignal): Promise<Page<Message>> {
  const result = await request(`${messagesPath(groupId)}${pageQuery(before)}`, { signal }, pageOf(messageIn(groupId)));
  return { ...result, items: result.items.map(decodeMessage) };
}
export function getMessage(groupId: string, id: string, signal?: AbortSignal) {
  return request(`${messagesPath(groupId)}/${encodeURIComponent(id)}`, { signal },
    (value): value is WireMessage => {
      if (isRecord(value) && value.group_id === groupId && value.id === id && hasMessageSourceIdentityConflict(value)) {
        throw new MessageSourceIdentityConflict("来源消息的来源类型与发送者身份矛盾，读取未确认。", 200, "invalid_response");
      }
      return messageIn(groupId)(value) && value.id === id;
    }).then(decodeMessage);
}
export function listProviders(before = "", signal?: AbortSignal) {
  return request(`/api/v1/providers${pageQuery(before)}`, { signal }, pageOf(isProvider));
}
