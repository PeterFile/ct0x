import { ApiError } from "./api";

// Experimental local-user HTTP authority. A receipt is not execution readiness or an OS sandbox.
export type WorkspaceGrantInput = Readonly<{ root: string; scope: "read_only" | "read_write" }>;
export type WorkspaceGrantReceipt = Readonly<{
  id: string; group_id: string; member_id: string; root: string; resolved_root: string; resource_key: string;
  scope: WorkspaceGrantInput["scope"]; node_id: "local"; authorized_by: "user"; created_at: string;
}>;
const record = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);
const text = (value: unknown): value is string => typeof value === "string" && !/[\ud800-\udfff]/u.test(value);
const identifier = (value: unknown): value is string => text(value) && value !== "" && !/^\p{White_Space}|\p{White_Space}$/u.test(value)
  && !/[\0\r\n]/u.test(value) && new TextEncoder().encode(value).length <= 4096;
const pathId = (value: unknown): value is string => identifier(value) && value !== "." && value !== "..";
const absoluteRoot = (value: unknown): value is string => text(value) && value.startsWith("/") && !value.includes("\0");
function evidenceTime(value: unknown): value is string {
  if (!text(value)) return false;
  const parts = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?Z$/.exec(value);
  if (!parts) return false;
  const [year, month, day, hour, minute, second] = parts.slice(1, 7).map(Number);
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  return year > 0 && month >= 1 && month <= 12 && day >= 1 && day <= days[month - 1] && hour < 24 && minute < 60 && second < 60
    && !(year === 1 && month === 1 && day === 1 && hour === 0 && minute === 0 && second === 0 && Number(parts[7] ?? 0) === 0);
}
const receiptFields = ["id", "group_id", "member_id", "root", "resolved_root", "resource_key", "scope", "node_id", "authorized_by", "created_at"];
function isReceipt(value: unknown, groupId: string, memberId: string): value is WorkspaceGrantReceipt {
  return record(value) && Object.keys(value).length === receiptFields.length && receiptFields.every(field => Object.hasOwn(value, field))
    && pathId(value.id) && value.group_id === groupId && value.member_id === memberId
    && absoluteRoot(value.root) && absoluteRoot(value.resolved_root) && identifier(value.resource_key)
    && value.resource_key.startsWith("workspace:") && value.resource_key !== "workspace:"
    && (value.scope === "read_only" || value.scope === "read_write") && value.node_id === "local"
    && value.authorized_by === "user" && evidenceTime(value.created_at);
}
function decode(body: ArrayBuffer): unknown {
  const source = new TextDecoder("utf-8", { fatal: true }).decode(body), value: unknown = JSON.parse(source);
  // Preserve ambiguity detection before JSON.parse's last-key-wins projection can become evidence.
  const stack: { keys: Set<string> | null; expectingKey: boolean }[] = [];
  for (const token of source.matchAll(/"(?:[^"\\]|\\.)*"|[{}\[\],:]/g)) {
    const part = token[0], top = stack.at(-1);
    if (part === "{" || part === "[") stack.push({ keys: part === "{" ? new Set() : null, expectingKey: part === "{" });
    else if (part === "}" || part === "]") stack.pop();
    else if (part === "," && top?.keys) top.expectingKey = true;
    else if (part === ":" && top) top.expectingKey = false;
    else if (part.startsWith('"') && top?.keys && top.expectingKey) {
      const key: string = JSON.parse(part);
      if (top.keys.has(key)) throw new Error("Duplicate JSON key");
      top.keys.add(key); top.expectingKey = false;
    }
  }
  return value;
}
class GrantRejection extends ApiError {}
class GrantNotFound extends GrantRejection {}
export const isWorkspaceGrantDefinitiveRejection = (error: unknown) => error instanceof GrantRejection;
const invalid = () => new GrantRejection("目录授权输入无效，请明确指定成员、已有绝对目录与读写范围。", 400, "invalid_input");
const refusals: Record<number, readonly string[]> = { 400: ["invalid_input", "invalid_json"], 403: ["invalid_host", "invalid_origin"],
  404: ["not_found"], 405: ["method_not_allowed"], 409: ["conflict"], 413: ["body_too_large"], 415: ["unsupported_media_type"] };
const reasons: Record<number, string> = { 400: "目录或授权范围未通过服务端校验，目录可能不存在、受保护或当前实例不支持该范围",
  403: "本地访问校验失败", 404: "目录授权入口、群组或成员记录不可用", 405: "当前实例不支持此目录授权入口",
  409: "该成员已有不可变目录授权，或同一幂等键的原请求内容冲突", 413: "目录授权请求内容过大", 415: "目录授权入口仅接受 JSON" };
async function request<T>(path: string, init: RequestInit, status: number, valid: (value: unknown) => value is T): Promise<T> {
  let response: Response;
  try { response = await fetch(path, { ...init, redirect: "error" }); }
  catch { throw new ApiError("目录授权读取或保存结果未确认，请保留原操作后重试。", 0, "disconnected"); }
  let value: unknown = null;
  try { value = decode(await response.arrayBuffer()); } catch { /* Malformed or ambiguous evidence remains unknown. */ }
  if (response.status === status && valid(value)) return value;
  if (!response.ok && record(value) && Object.keys(value).length === 1 && record(value.error) && Object.keys(value.error).length === 2
    && identifier(value.error.code) && text(value.error.message) && /\P{White_Space}/u.test(value.error.message)) {
    const definitive = refusals[response.status]?.includes(value.error.code) ?? false;
    const ErrorType = response.status === 404 && value.error.code === "not_found" ? GrantNotFound : definitive ? GrantRejection : ApiError;
    throw new ErrorType(`${definitive ? reasons[response.status] : "目录授权读取或保存结果未确认"}（HTTP ${response.status}）。`, response.status, value.error.code);
  }
  throw new ApiError(`目录授权响应不符合约定，读取或保存结果未确认（HTTP ${response.status}）。`, response.status, "invalid_response");
}
function grantPath(groupId: string, memberId: string) {
  if (!pathId(groupId) || !pathId(memberId)) throw invalid();
  return `/api/v1/groups/${encodeURIComponent(groupId)}/members/${encodeURIComponent(memberId)}/workspace-grant`;
}
export async function getWorkspaceGrant(groupId: string, memberId: string, signal?: AbortSignal): Promise<WorkspaceGrantReceipt | null> {
  const path = grantPath(groupId, memberId);
  try { return await request(path, { signal }, 200, (value): value is WorkspaceGrantReceipt => isReceipt(value, groupId, memberId)); }
  catch (error) { if (error instanceof GrantNotFound) return null; throw error; }
}
export async function authorizeWorkspaceGrant(groupId: string, memberId: string, input: WorkspaceGrantInput, key: string): Promise<WorkspaceGrantReceipt> {
  const path = grantPath(groupId, memberId);
  if (!record(input) || !absoluteRoot(input.root) || (input.scope !== "read_only" && input.scope !== "read_write")
    || !key || key.length > 128 || /[^\x21-\x7e]|,/.test(key)) throw invalid();
  const sent = Object.freeze({ root: input.root, scope: input.scope });
  return request(path, { method: "POST", headers: { "Content-Type": "application/json", "Idempotency-Key": key }, body: JSON.stringify(sent) }, 201,
    (value): value is WorkspaceGrantReceipt => isReceipt(value, groupId, memberId) && value.root === sent.root && value.scope === sent.scope);
}
