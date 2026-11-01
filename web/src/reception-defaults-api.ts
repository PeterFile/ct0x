import { ApiError } from "./api";

export type ReceptionConfiguration = Readonly<{
  id: string; revision: number; provider_id: string; model: string; role: string; created_at: string;
}>;
export type ReceptionDefaultsInput = Readonly<{ provider_id: string; model: string; role: string; expected_revision: number }>;
export type ReceptionDefaultsState = Readonly<{
  configuration: ReceptionConfiguration | null;
  availability: { code: "not_configured" | "configured" | "provider_unavailable"; message: string };
  execution_enabled: boolean;
}>;
const endpoint = "/api/v1/settings/reception-defaults";
const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);
const nonblank = (value: unknown): value is string => typeof value === "string" && value.trim() !== "";
const wellFormed = (value: string) => !Array.from(value).some(character => character.length === 1
  && character.charCodeAt(0) >= 0xd800 && character.charCodeAt(0) <= 0xdfff);
const byteLength = (value: string) => new TextEncoder().encode(value).length;

export function receptionDefaultsInputError(input: ReceptionDefaultsInput): string {
  if (!nonblank(input.provider_id)) return "请从已载入目录中选择运行器。";
  if (!nonblank(input.model)) return "请显式填写模型，不会自动猜测模型。";
  if (!wellFormed(input.model)) return "模型必须是合法 UTF-8 文本。";
  if (/[\0\r\n]/.test(input.model)) return "模型不能包含 NUL、回车或换行等控制字符。";
  if (byteLength(input.model) > 256) return "模型不能超过 256 个 UTF-8 字节。";
  if (!nonblank(input.role)) return "请填写非空白的职责。";
  if (!wellFormed(input.role)) return "职责必须是合法 UTF-8 文本。";
  if (byteLength(input.role) > 4096) return "职责不能超过 4096 个 UTF-8 字节。";
  if (!Number.isSafeInteger(input.expected_revision) || input.expected_revision < 0 || input.expected_revision >= Number.MAX_SAFE_INTEGER) {
    return "设置版本无效或已达到上限，请重新载入设置。";
  }
  return "";
}
function isConfiguration(value: unknown): value is ReceptionConfiguration {
  return isRecord(value) && nonblank(value.id) && typeof value.revision === "number"
    && Number.isSafeInteger(value.revision) && value.revision > 0
    && typeof value.created_at === "string"
    && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|\+00:00)$/.test(value.created_at)
    && Number.isFinite(Date.parse(value.created_at))
    && receptionDefaultsInputError({ provider_id: value.provider_id as string, model: value.model as string,
      role: value.role as string, expected_revision: 0 }) === "";
}
function isState(value: unknown): value is ReceptionDefaultsState {
  if (!isRecord(value) || typeof value.execution_enabled !== "boolean" || !isRecord(value.availability)
    || typeof value.availability.message !== "string") return false;
  return value.configuration === null ? value.availability.code === "not_configured"
    : isConfiguration(value.configuration) && (value.availability.code === "configured" || value.availability.code === "provider_unavailable");
}
function rejectionText(status: number, code: string) {
  if (status === 409) {
    switch (code) {
      case "reception_defaults_required": return "请先在默认接待设置中选择运行器、填写模型与职责，或将消息设为仅公开可见";
      case "reception_provider_unavailable": return "已选运行器当前不受支持，请重新选择运行器，不会自动改用其他配置";
      case "reception_member_required": return "群内没有活跃成员，请手动添加或恢复成员，不会再次自动创建接待者";
      case "reception_revision_conflict": return "默认接待设置已被更新，请重新载入最新设置，保留草稿并确认选择后再保存";
      case "conflict": return "同一保存标识已用于不同内容，请重新确认后保存";
      default: return "保存被拒绝，发生冲突，请检查设置后重试";
    }
  }
  return status === 400 ? "输入未通过验证，请检查运行器、模型和职责"
    : status === 404 ? "未找到默认接待设置服务，请确认本地服务版本"
    : status === 413 ? "请求内容过大，请缩短输入后重试"
    : status === 415 ? "服务仅接受 JSON 内容，请检查请求格式"
    : "服务暂时无法完成请求，请重试";
}
async function request<T>(init: RequestInit, valid: (value: unknown) => value is T): Promise<T> {
  let response: Response;
  try { response = await fetch(endpoint, init); }
  catch (error) {
    if (init.signal?.aborted) throw error;
    throw new ApiError("无法连接本地服务，请确认服务已启动。");
  }
  const data: unknown = await response.json().catch(() => null);
  if (response.status >= 400) {
    const error = isRecord(data) && isRecord(data.error) ? data.error : {};
    const code = typeof error.code === "string" ? error.code : "http_error";
    throw new ApiError(`${rejectionText(response.status, code)}（HTTP ${response.status}）。`, response.status, code);
  }
  if (response.status !== 200 || !valid(data)) {
    throw new ApiError(`服务响应不符合约定（HTTP ${response.status}），请重试。`, response.status, "invalid_response");
  }
  return data;
}
export function getReceptionDefaults(signal?: AbortSignal) {
  return request({ signal }, isState);
}
export function setReceptionDefaults(input: ReceptionDefaultsInput, key: string, signal?: AbortSignal) {
  const error = receptionDefaultsInputError(input);
  if (error || !key.trim()) return Promise.reject(new ApiError(error || "缺少保存标识。", 400, "invalid_input"));
  // Serialize only the owned DTO; identities, scope and workspace are never client choices.
  const body = JSON.stringify({ provider_id: input.provider_id, model: input.model, role: input.role, expected_revision: input.expected_revision });
  return request({ method: "PUT", signal, headers: { "Content-Type": "application/json", "Idempotency-Key": key }, body },
    (value): value is ReceptionConfiguration => isConfiguration(value) && value.provider_id === input.provider_id
      && value.model === input.model && value.role === input.role && value.revision === input.expected_revision + 1);
}
