import { ApiError } from "./api";
import type { Message } from "./api";

export type HistoryQuery = Readonly<{ q: string; topic_id?: string; before?: string }>;
export type HistoryPage = { items: Message[]; next_cursor: string };
const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);
const messageIn = (groupId: string, topicId?: string) => (value: unknown): value is Message => isRecord(value)
  && ["id", "group_id", "topic_id", "sender_id", "content", "created_at"].every(key => typeof value[key] === "string")
  && value.id !== "" && value.group_id === groupId && (topicId === undefined || value.topic_id === topicId)
  && Array.isArray(value.deliveries) && value.deliveries.every(delivery => isRecord(delivery)
    && ["id", "message_id", "conversation_id", "recipient_id", "state", "turn_id", "error", "created_at"]
      .every(key => typeof delivery[key] === "string") && delivery.id !== "" && delivery.message_id === value.id);

async function request<T>(path: string, signal: AbortSignal | undefined, valid: (value: unknown) => value is T): Promise<T> {
  let response: Response;
  try { response = await fetch(path, { signal }); }
  catch (error) {
    if (signal?.aborted) throw error;
    throw new ApiError("无法连接本地服务，请确认服务已启动。");
  }
  const data: unknown = await response.json().catch(() => null);
  if (!response.ok) {
    const error = isRecord(data) && isRecord(data.error) ? data.error : {};
    const reason = response.status === 400 ? "输入未通过验证，请检查关键词和话题 ID"
      : response.status === 404 ? "未找到请求的服务或消息"
      : "服务暂时无法完成请求";
    throw new ApiError(`${reason}（HTTP ${response.status}）。`, response.status,
      typeof error.code === "string" ? error.code : "http_error");
  }
  if (response.status !== 200 || !valid(data)) {
    throw new ApiError(`服务响应不符合约定（HTTP ${response.status}），请重试。`, response.status, "invalid_response");
  }
  return data;
}

const historyPath = (groupId: string) => `/api/v1/groups/${encodeURIComponent(groupId)}/history`;
export function searchHistory(groupId: string, query: HistoryQuery, signal?: AbortSignal) {
  const params = new URLSearchParams({ q: query.q, limit: "50" });
  if (query.topic_id !== undefined) params.set("topic_id", query.topic_id);
  if (query.before) params.set("before", query.before);
  return request(`${historyPath(groupId)}?${params}`, signal, (value): value is HistoryPage => isRecord(value)
    && Array.isArray(value.items) && value.items.every(messageIn(groupId, query.topic_id)) && typeof value.next_cursor === "string");
}
export function readHistory(groupId: string, messageId: string, topicId?: string, signal?: AbortSignal) {
  const params = new URLSearchParams();
  if (topicId !== undefined) params.set("topic_id", topicId);
  return request(`${historyPath(groupId)}/${encodeURIComponent(messageId)}${params.size ? `?${params}` : ""}`, signal,
    (value): value is Message => messageIn(groupId, topicId)(value) && value.id === messageId);
}
