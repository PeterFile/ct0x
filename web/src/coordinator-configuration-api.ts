import { ApiError } from "./api";

export type CoordinatorBinding = Readonly<{
  group_id: string;
  member_id: string;
  revision: number;
}>;
export type DispatchPolicy = Readonly<{
  group_id: string;
  revision: number;
  allowed_worker_ids: readonly string[];
}>;
export type CoordinatorInput = Readonly<{
  member_id: string;
  expected_revision: number;
}>;
export type DispatchPolicyInput = Readonly<{
  allowed_worker_ids: readonly string[];
  expected_revision: number;
}>;

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);
const exactRecord = (value: unknown, fields: readonly string[]): value is Record<string, unknown> =>
  isRecord(value) && Object.keys(value).length === fields.length && fields.every(field => Object.hasOwn(value, field));
const validText = (value: unknown): value is string => typeof value === "string" && !/[\ud800-\udfff]/u.test(value);
// Go strings.TrimSpace uses Unicode White_Space: NEL is space; BOM is not.
const goSpace = "[\\u0009-\\u000d \\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000]";
const goEdgeSpace = new RegExp(`^${goSpace}|${goSpace}$`, "u");
const encoder = new TextEncoder();
const validIdentifier = (value: unknown): value is string => validText(value) && value !== ""
  && !goEdgeSpace.test(value) && !/[\0\r\n]/u.test(value) && encoder.encode(value).length <= 4096;
const validKey = (value: unknown): value is string => typeof value === "string" && value.length >= 1
  && value.length <= 128 && !/[^\x21-\x7e]|,/.test(value);
const validRevision = (value: unknown): value is number => typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
const validExpectedRevision = (value: unknown): value is number => validRevision(value) && value < Number.MAX_SAFE_INTEGER;
const validWorkers = (value: unknown): value is readonly string[] => Array.isArray(value) && value.every(validIdentifier);

function isBinding(value: unknown, groupId: string): value is CoordinatorBinding {
  return exactRecord(value, ["group_id", "member_id", "revision"]) && value.group_id === groupId
    && validIdentifier(value.member_id) && validRevision(value.revision) && value.revision > 0;
}
function isPolicy(value: unknown, groupId: string): value is DispatchPolicy {
  return exactRecord(value, ["group_id", "revision", "allowed_worker_ids"]) && value.group_id === groupId
    && validRevision(value.revision) && validWorkers(value.allowed_worker_ids)
    && new Set(value.allowed_worker_ids).size === value.allowed_worker_ids.length
    && (value.revision > 0 || value.allowed_worker_ids.length === 0);
}
function isErrorEnvelope(value: unknown): value is { error: { code: string; message: string } } {
  if (!exactRecord(value, ["error"]) || !exactRecord(value.error, ["code", "message"])) return false;
  return validText(value.error.code) && value.error.code.trim() !== ""
    && validText(value.error.message) && value.error.message.trim() !== "";
}

const rejectionMessages: Readonly<Record<number, string>> = {
  400: "协调配置输入未通过验证，请检查群组、成员和版本",
  404: "未找到协调配置或关联记录，或记录不可访问",
  409: "协调配置与当前版本或成员配置冲突",
  413: "协调配置请求内容过大",
  415: "服务仅接受 JSON 内容，请检查请求格式",
};
const unknownResult = "协调配置结果未知，请保留原请求和幂等键";
// Mark validated rejection provenance, not an arbitrary HTTP status or server code.
class ConfigurationRejection extends ApiError {}
const invalidInput = () => new ConfigurationRejection("协调配置输入无效，请检查群组、成员、版本和幂等键。", 0, "invalid_input");

// This classifies one response only; an earlier unknown outcome remains the caller's responsibility.
export function isCoordinatorConfigurationDefinitiveRejection(error: unknown): boolean {
  return error instanceof ConfigurationRejection;
}

async function request<T>(
  groupId: string, resource: "coordinator" | "dispatch-policy", init: RequestInit,
  valid: (value: unknown) => value is T,
): Promise<T> {
  let response: Response;
  try {
    response = await fetch(`/api/v1/groups/${encodeURIComponent(groupId)}/coordination/${resource}`, {
      ...init, redirect: "error",
    });
  } catch (error) {
    const canceled = init.signal?.aborted || (isRecord(error) && error.name === "AbortError");
    throw new ApiError(`${unknownResult}。`, 0, canceled ? "request_canceled" : "disconnected");
  }
  const data: unknown = await response.json().catch(() => null);
  if (response.status === 200 && valid(data)) return data;
  if (response.status !== 200 && isErrorEnvelope(data)) {
    const ErrorType = Object.hasOwn(rejectionMessages, response.status) ? ConfigurationRejection : ApiError;
    throw new ErrorType(`${rejectionMessages[response.status] ?? unknownResult}（HTTP ${response.status}）。`,
      response.status, data.error.code);
  }
  throw new ApiError(`协调配置响应不符合约定，${unknownResult}（HTTP ${response.status}）。`, response.status, "invalid_response");
}

export async function getCoordinator(groupId: string, signal?: AbortSignal): Promise<CoordinatorBinding> {
  if (!validIdentifier(groupId)) throw invalidInput();
  return request(groupId, "coordinator", { signal }, (value): value is CoordinatorBinding => isBinding(value, groupId));
}
export async function getDispatchPolicy(groupId: string, signal?: AbortSignal): Promise<DispatchPolicy> {
  if (!validIdentifier(groupId)) throw invalidInput();
  return request(groupId, "dispatch-policy", { signal }, (value): value is DispatchPolicy => isPolicy(value, groupId));
}

export async function setCoordinator(
  groupId: string, input: CoordinatorInput, key: string, signal?: AbortSignal,
): Promise<CoordinatorBinding> {
  if (!validIdentifier(groupId) || !validKey(key) || !isRecord(input)) throw invalidInput();
  const sent: CoordinatorInput = { member_id: input.member_id, expected_revision: input.expected_revision };
  if (!validIdentifier(sent.member_id) || !validExpectedRevision(sent.expected_revision)) throw invalidInput();
  return request(groupId, "coordinator", {
    method: "PUT", signal, headers: { "Content-Type": "application/json", "Idempotency-Key": key },
    body: JSON.stringify(sent),
  }, (value): value is CoordinatorBinding => isBinding(value, groupId)
    && value.member_id === sent.member_id && value.revision === sent.expected_revision + 1);
}
export async function setDispatchPolicy(
  groupId: string, input: DispatchPolicyInput, key: string, signal?: AbortSignal,
): Promise<DispatchPolicy> {
  if (!validIdentifier(groupId) || !validKey(key) || !isRecord(input)) throw invalidInput();
  const workers = input.allowed_worker_ids;
  // Copy the actual sent DTO before fetch; caller-owned arrays never participate in receipt matching.
  const sent: DispatchPolicyInput = {
    allowed_worker_ids: Array.isArray(workers) ? [...workers] : workers,
    expected_revision: input.expected_revision,
  };
  if (!validWorkers(sent.allowed_worker_ids) || !validExpectedRevision(sent.expected_revision)) throw invalidInput();
  const expectedWorkers = new Set(sent.allowed_worker_ids);
  return request(groupId, "dispatch-policy", {
    method: "PUT", signal, headers: { "Content-Type": "application/json", "Idempotency-Key": key },
    body: JSON.stringify(sent),
  }, (value): value is DispatchPolicy => isPolicy(value, groupId) && value.revision === sent.expected_revision + 1
    // Go sorts by UTF-8 bytes, unlike JS's UTF-16 sort; compare the deduplicated set instead.
    && value.allowed_worker_ids.length === expectedWorkers.size && value.allowed_worker_ids.every(id => expectedWorkers.has(id)));
}
