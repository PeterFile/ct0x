import { ApiError, decodeMessage, messageIn } from "./api";
import type { Message, WireMessage } from "./api";

export type HistoryQuery = Readonly<{ q: string; topic_id?: string; before?: string }>;
type EffectiveTopic = { effective_topic_id?: string };
export type HistoryMessage = Message & EffectiveTopic;
type WireHistoryMessage = WireMessage & EffectiveTopic;
export type HistoryPage = { items: HistoryMessage[]; next_cursor: string };
const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);
const topicEncoder = new TextEncoder();
const maxTopicBytes = 256;

function historyMessageIn(groupId: string, topicId?: string) {
  const inGroup = messageIn(groupId);
  return (value: unknown): value is WireHistoryMessage => {
    if (!inGroup(value)) return false;
    const effective = "effective_topic_id" in value ? value.effective_topic_id : undefined;
    // Exact UTF-8 selectors may be empty (main) and are never trimmed or normalized.
    if (effective !== undefined && (typeof effective !== "string" || effective.length > maxTopicBytes
      || /[\0\ud800-\udfff]/u.test(effective) || topicEncoder.encode(effective).length > maxTopicBytes)) return false;
    return topicId === undefined || (effective ?? value.topic_id) === topicId;
  };
}
function decodeHistoryMessage({ effective_topic_id, ...wire }: WireHistoryMessage): HistoryMessage {
  const message = decodeMessage(wire);
  return effective_topic_id === undefined ? message : { ...message, effective_topic_id };
}

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
export async function searchHistory(groupId: string, { q, topic_id: topicId, before }: HistoryQuery, signal?: AbortSignal): Promise<HistoryPage> {
  const params = new URLSearchParams({ q, limit: "50" });
  if (topicId !== undefined) params.set("topic_id", topicId);
  if (before) params.set("before", before);
  const inScope = historyMessageIn(groupId, topicId);
  const result = await request(`${historyPath(groupId)}?${params}`, signal, (value): value is { items: WireHistoryMessage[]; next_cursor: string } => isRecord(value)
    && Array.isArray(value.items) && value.items.every(inScope) && typeof value.next_cursor === "string");
  return { ...result, items: result.items.map(decodeHistoryMessage) };
}
// A result's recorded topic is an identity check on the reply, not a read selector.
export function readHistory(groupId: string, messageId: string, topicId?: string, signal?: AbortSignal, recordedTopicId?: string) {
  const params = new URLSearchParams();
  if (topicId !== undefined) params.set("topic_id", topicId);
  const inScope = historyMessageIn(groupId, topicId);
  return request(`${historyPath(groupId)}/${encodeURIComponent(messageId)}${params.size ? `?${params}` : ""}`, signal,
    (value): value is WireHistoryMessage => inScope(value) && value.id === messageId
      && (recordedTopicId === undefined || value.topic_id === recordedTopicId)).then(decodeHistoryMessage);
}
