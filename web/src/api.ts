export type Group = { id: string; name: string; description: string; created_at: string };
export type GroupInput = Readonly<{ name: string; description?: string }>;
type GroupPage = { items: Group[]; next_cursor: string };
export class ApiError extends Error {
  constructor(message: string, readonly status = 0, readonly code = "disconnected") { super(message); }
}
const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null;
const isGroup = (value: unknown): value is Group => isRecord(value)
  && ["id", "name", "description", "created_at"].every(key => typeof value[key] === "string")
  && value.id !== "" && value.name !== "";
const isPage = (value: unknown): value is GroupPage => isRecord(value)
  && Array.isArray(value.items) && value.items.every(isGroup) && typeof value.next_cursor === "string";

async function request<T>(query: string, init: RequestInit, valid: (value: unknown) => value is T, expectedStatus = 200): Promise<T> {
  let response: Response;
  try { response = await fetch(`/api/v1/groups${query}`, init); }
  catch (error) {
    if (init.signal?.aborted) throw error;
    throw new ApiError("无法连接本地服务，请确认服务已启动。");
  }
  const data: unknown = await response.json().catch(() => null);
  if (!response.ok) {
    const error = isRecord(data) && isRecord(data.error) ? data.error : {};
    const reason = response.status === 400 ? "输入未通过验证，请检查群组信息"
      : response.status === 404 ? "未找到请求的服务或群组"
      : response.status === 409 ? "幂等键与已有请求冲突"
      : "服务暂时无法完成请求";
    throw new ApiError(`${reason}（HTTP ${response.status}）。`, response.status,
      typeof error.code === "string" ? error.code : "http_error");
  }
  if (response.status !== expectedStatus || !valid(data)) {
    throw new ApiError(`服务响应不符合约定（HTTP ${response.status}），请重试。`, response.status, "invalid_response");
  }
  return data;
}
export function listGroups(before = "", signal?: AbortSignal): Promise<GroupPage> {
  const query = new URLSearchParams({ limit: "50" });
  if (before) query.set("before", before);
  return request(`?${query}`, { signal }, isPage);
}
export function createGroup(input: GroupInput, key: string, signal?: AbortSignal): Promise<Group> {
  return request("", {
    method: "POST", signal,
    headers: { "Content-Type": "application/json", "Idempotency-Key": key },
    body: JSON.stringify(input),
  }, isGroup, 201);
}
