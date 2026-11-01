import { ApiError } from "./api";

export type Task = {
  id: string; group_id: string; title: string; description: string; acceptance: string;
  assignee_id: string; dependency_ids: string[]; status: "open" | "submitted" | "accepted";
  revision: number; active_attempt_id: string; current_submission_id: string; created_at: string;
};
export type TaskInput = Readonly<{
  title: string; description?: string; acceptance: string; assignee_id?: string; dependency_ids?: readonly string[];
}>;
type TaskPage = { items: Task[]; next_cursor: string };
const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);
const taskIn = (groupId: string) => (value: unknown): value is Task => isRecord(value)
  && ["id", "group_id", "title", "description", "acceptance", "assignee_id", "active_attempt_id", "current_submission_id", "created_at"]
    .every(key => typeof value[key] === "string")
  && typeof value.id === "string" && value.id.trim() !== "" && value.group_id === groupId
  && typeof value.active_attempt_id === "string" && (value.active_attempt_id === "" || value.active_attempt_id.trim() !== "")
  && typeof value.current_submission_id === "string"
  && ((value.status === "open" && value.current_submission_id === "")
    || (value.status === "submitted" && value.active_attempt_id !== "" && value.current_submission_id.trim() !== "")
    || (value.status === "accepted" && value.active_attempt_id === "" && value.current_submission_id.trim() !== ""))
  && typeof value.revision === "number" && Number.isSafeInteger(value.revision) && value.revision > 0
  && Array.isArray(value.dependency_ids) && value.dependency_ids.every(id => typeof id === "string");

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

const tasksPath = (groupId: string) => `/api/v1/groups/${encodeURIComponent(groupId)}/tasks`;
export function listTasks(groupId: string, before = "", signal?: AbortSignal) {
  const query = new URLSearchParams({ limit: "50" });
  if (before) query.set("before", before);
  return request(`${tasksPath(groupId)}?${query}`, { signal }, (value): value is TaskPage => isRecord(value)
    && Array.isArray(value.items) && value.items.every(taskIn(groupId)) && typeof value.next_cursor === "string");
}
export function getTask(groupId: string, taskId: string, signal?: AbortSignal) {
  return request(`${tasksPath(groupId)}/${encodeURIComponent(taskId)}`, { signal },
    (value): value is Task => taskIn(groupId)(value) && value.id === taskId);
}
export function createTask(groupId: string, input: TaskInput, key: string, signal?: AbortSignal) {
  return request(tasksPath(groupId), {
    method: "POST", signal,
    headers: { "Content-Type": "application/json", "Idempotency-Key": key },
    body: JSON.stringify({ title: input.title, description: input.description, acceptance: input.acceptance,
      assignee_id: input.assignee_id, dependency_ids: input.dependency_ids }),
  }, taskIn(groupId), 201);
}
