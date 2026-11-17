import { ApiError } from "./api";
import { isMessageSourceKind, sourceMatchesSender } from "./message-source";
import type { MessageSourceKind } from "./message-source";

// Experimental local chat-authority contract; absent legacy metadata is never promoted into authority.
export type ChatPolicy = Readonly<{
  group_id: string; topic_id: string; enabled: boolean; allow_preparation: boolean;
  preauthorization_message_id: string; action: "" | "text_submission"; max_admissions: number;
  revision: number; authorized_by: "" | "user"; preauthorization_text?: string;
  preparation_scope_kind?: string; preparation_supported?: boolean; configuration_source_id?: string;
}>;
export type ChatPolicyInput = Readonly<{
  enabled: boolean; allow_preparation: boolean; preparation_scope_kind?: "topic_summary";
  // Retained only for an already-submitted v16 operation; new UI commands never populate this field.
  preauthorization_message_id?: string;
  action: "text_submission"; max_admissions: number; expected_revision: number;
}>;
export type ChatAdmission = Readonly<{
  id: string; candidate_id: string; group_id: string; topic_id: string; message_id: string;
  source_sender_id: string; assessed_by: string; turn_id: string; policy_revision: number; candidate_generation: number;
  disposition: "request_work" | "prepare_work"; authority: "explicit_request" | "written_preauthorization";
  authorization_message_id: string; action: "text_submission"; task_id: string; mandate_id: string;
  dispatch_request_id: string; notice_message_id: string; created_at: string;
  source_id?: string; source_kind?: MessageSourceKind; purpose?: string; authority_evidence_verified?: boolean;
  outcome?: "" | "admitted"; reason_code?: string; question?: string;
}>;
const record = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);
const text = (value: unknown): value is string => typeof value === "string" && !/[\ud800-\udfff]/u.test(value);
const nonblank = (value: unknown): value is string => text(value) && /\P{White_Space}/u.test(value);
const identifier = (value: unknown): value is string => text(value) && value !== "" && !/^\p{White_Space}|\p{White_Space}$/u.test(value)
  && !/[\0\r\n]/u.test(value) && new TextEncoder().encode(value).length <= 4096;
const pathId = (value: unknown): value is string => identifier(value) && value !== "." && value !== "..";
const integer = (value: unknown): value is number => typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
const positive = (value: unknown): value is number => integer(value) && value > 0;
const budget = (value: unknown): value is number => integer(value) && value >= 1 && value <= 100;
const keyValid = (value: string) => value.length > 0 && value.length <= 128 && !/[^\x21-\x7e]|,/.test(value);
const policyFields = ["group_id", "topic_id", "enabled", "allow_preparation", "preauthorization_message_id", "action", "max_admissions", "revision", "authorized_by"];
export const hasChatPolicyMetadata = (policy: ChatPolicy) => typeof policy.preparation_supported === "boolean"
  && typeof policy.configuration_source_id === "string";
const policyMetadata = ["preauthorization_text", "preparation_scope_kind", "preparation_supported", "configuration_source_id"];
function isPolicy(value: unknown, groupId: string, topicId: string): value is ChatPolicy {
  if (!record(value) || !policyFields.every(field => Object.hasOwn(value, field))
    || Object.keys(value).some(field => !policyFields.includes(field) && !policyMetadata.includes(field))
    || value.group_id !== groupId || value.topic_id !== topicId || !integer(value.revision)
    || typeof value.enabled !== "boolean" || typeof value.allow_preparation !== "boolean"
    || !text(value.preauthorization_message_id) || (value.preauthorization_text !== undefined && !text(value.preauthorization_text))) return false;
  const metadata = ["preparation_scope_kind", "preparation_supported", "configuration_source_id"].some(field => Object.hasOwn(value, field));
  if (metadata && (typeof value.preparation_supported !== "boolean" || !text(value.configuration_source_id)
    || (value.preparation_scope_kind !== undefined && !text(value.preparation_scope_kind)))) return false;
  const scope = value.preparation_scope_kind ?? "";
  if (value.revision === 0) return !value.enabled && !value.allow_preparation && value.preauthorization_message_id === ""
    && value.action === "" && value.max_admissions === 0 && value.authorized_by === "" && !value.preauthorization_text
    && (!metadata || (!value.preparation_supported && value.configuration_source_id === "" && scope === ""));
  if (value.action !== "text_submission" || value.authorized_by !== "user" || !budget(value.max_admissions)
    || (value.allow_preparation && !identifier(value.preauthorization_message_id))
    || (value.preauthorization_message_id === "" ? Boolean(value.preauthorization_text)
      : !identifier(value.preauthorization_message_id) || !nonblank(value.preauthorization_text) || value.preauthorization_text.includes("\0")
        || new TextEncoder().encode(value.preauthorization_text).length > 8192)) return false;
  if (!metadata) return true;
  if (value.configuration_source_id === "") return !value.preparation_supported && scope === "";
  if (!identifier(value.configuration_source_id)) return false;
  if (!value.allow_preparation) return !value.preparation_supported && scope === "" && value.preauthorization_message_id === "";
  return value.preparation_supported === (scope === "topic_summary");
}
const admissionFields = ["id", "candidate_id", "group_id", "topic_id", "message_id", "source_sender_id", "assessed_by", "turn_id",
  "policy_revision", "candidate_generation", "disposition", "authority", "authorization_message_id", "action", "task_id", "mandate_id",
  "dispatch_request_id", "notice_message_id", "created_at"];
function isAdmission(value: unknown, groupId: string, taskId: string): value is ChatAdmission {
  const metadata = ["source_id", "source_kind", "purpose", "authority_evidence_verified", "outcome", "reason_code", "question"];
  if (!record(value) || Object.keys(value).some(field => !admissionFields.includes(field) && !metadata.includes(field))
    || !admissionFields.every(field => Object.hasOwn(value, field))
    || value.group_id !== groupId || value.task_id !== taskId || !text(value.topic_id) || value.action !== "text_submission"
    || !positive(value.policy_revision) || !positive(value.candidate_generation)
    || !["id", "candidate_id", "message_id", "source_sender_id", "assessed_by", "turn_id", "authorization_message_id", "mandate_id", "notice_message_id"].every(field => identifier(value[field]))
    || !pathId(value.dispatch_request_id) || !text(value.created_at)
    || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$/.test(value.created_at)
    || !Number.isFinite(Date.parse(value.created_at))) return false;
  if ((value.source_kind !== undefined && !isMessageSourceKind(value.source_kind))
    || (value.source_id !== undefined && value.source_id !== "" && !identifier(value.source_id))
    || (value.purpose !== undefined && value.purpose !== "" && value.purpose !== "chat_intake/v1")
    || (value.authority_evidence_verified !== undefined && typeof value.authority_evidence_verified !== "boolean")
    || (value.outcome !== undefined && value.outcome !== "" && value.outcome !== "admitted")
    || (value.reason_code !== undefined && value.reason_code !== "") || (value.question !== undefined && value.question !== "")) return false;
  const knownSource = value.source_kind !== undefined && value.source_kind !== "unknown";
  if (knownSource ? !identifier(value.source_id) : value.source_id !== undefined && value.source_id !== "") return false;
  if (!sourceMatchesSender(value.source_kind, value.source_sender_id)) return false;
  if (value.authority_evidence_verified === true && (value.purpose !== "chat_intake/v1"
    || (value.source_kind !== "local_user_chat" && value.source_kind !== "member")
    || (value.disposition === "request_work" && value.source_kind !== "local_user_chat"))) return false;
  return value.disposition === "request_work" ? value.authority === "explicit_request" && value.source_sender_id === "user"
    && value.authorization_message_id === value.message_id
    : value.disposition === "prepare_work" && value.authority === "written_preauthorization";
}
function decode(buffer: ArrayBuffer): unknown {
  const source = new TextDecoder("utf-8", { fatal: true }).decode(buffer), maximum = String(Number.MAX_SAFE_INTEGER);
  // Every numeric wire field in these contracts is a nonnegative integer. Reject rounding before parse.
  const tokens = /"(?:[^"\\]|\\.)*"|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?/uy;
  for (let index = 0; index < source.length; index++) {
    if (!/["\d-]/u.test(source[index])) continue;
    tokens.lastIndex = index;
    const token = tokens.exec(source)?.[0];
    if (token === undefined) return null;
    index = tokens.lastIndex - 1;
    if (token.startsWith('"')) continue;
    if (!/^(?:0|[1-9]\d*)$/.test(token) || token.length > maximum.length || (token.length === maximum.length && token > maximum)) return null;
  }
  return JSON.parse(source);
}
class ChatRejection extends ApiError {}
class LegacyPlainNotFound extends ApiError {}
function isLegacyPlainNotFound(response: Response, body: ArrayBuffer | null): boolean {
  const mediaType = response.headers.get("Content-Type")?.split(";")[0].trim().toLowerCase();
  if (response.status !== 404 || body === null || (mediaType && mediaType !== "text/plain")) return false;
  try { return /^404 page not found(?:\r?\n)?$/.test(new TextDecoder("utf-8", { fatal: true }).decode(body)); }
  catch { return false; }
}
export const isChatPolicyDefinitiveRejection = (error: unknown) => error instanceof ChatRejection;
const invalid = () => new ChatRejection("聊天策略输入无效，请检查范围、版本、任务预算和授权来源。", 400, "invalid_input");
const refusals: Record<number, readonly string[]> = { 400: ["invalid_input", "invalid_json"], 403: ["invalid_host", "invalid_origin"],
  404: ["not_found"], 405: ["method_not_allowed"], 409: ["conflict"], 413: ["body_too_large"], 415: ["unsupported_media_type"] };
const rejectionText: Record<number, string> = {
  400: "聊天策略输入未通过验证，请检查任务预算、授权来源和版本", 403: "本地访问校验失败，无法保存聊天策略",
  404: "当前实例的聊天配置能力或关联记录不可用", 405: "当前服务不支持此聊天请求接口",
  409: "聊天策略版本或原请求发生冲突，请读取真实当前配置并核对草稿",
  413: "聊天策略请求内容过大", 415: "服务仅接受 JSON 格式的聊天策略请求",
};
async function request<T>(path: string, init: RequestInit, valid: (value: unknown) => value is T): Promise<T> {
  let response: Response;
  try { response = await fetch(path, { ...init, redirect: "error" }); }
  catch { throw new ApiError("聊天配置信息或保存结果未确认，请保留原操作后重试。", 0, "disconnected"); }
  const body = await response.arrayBuffer().catch(() => null);
  let value: unknown = null;
  if (body !== null) { try { value = decode(body); } catch { /* Invalid bodies remain unconfirmed. */ } }
  if (response.status === 200 && valid(value)) return value;
  if (!response.ok && record(value) && Object.keys(value).length === 1 && record(value.error) && Object.keys(value.error).length === 2
    && nonblank(value.error.code) && nonblank(value.error.message)) {
    const definitive = refusals[response.status]?.includes(value.error.code) ?? false;
    const ErrorType = definitive ? ChatRejection : ApiError;
    const reason = definitive ? rejectionText[response.status] : "聊天请求未完成，配置或保存结果未确认";
    throw new ErrorType(`${reason}（HTTP ${response.status}）。`, response.status, value.error.code);
  }
  const ErrorType = isLegacyPlainNotFound(response, body) ? LegacyPlainNotFound : ApiError;
  throw new ErrorType(`聊天响应不符合约定，读取或保存结果未确认（HTTP ${response.status}）。`, response.status, "invalid_response");
}
function policyPath(groupId: string, topicId: string) {
  return `/api/v1/groups/${encodeURIComponent(groupId)}/coordination/chat-policy${topicId ? `?${new URLSearchParams({ topic_id: topicId })}` : ""}`;
}
export async function getChatPolicy(groupId: string, signal?: AbortSignal, topicId = ""): Promise<ChatPolicy> {
  if (!pathId(groupId) || !text(topicId)) throw invalid();
  return request(policyPath(groupId, topicId), { signal }, (value): value is ChatPolicy => isPolicy(value, groupId, topicId));
}
export async function setChatPolicy(groupId: string, input: ChatPolicyInput, key: string, topicId = ""): Promise<ChatPolicy> {
  if (!pathId(groupId) || !text(topicId) || !keyValid(key) || !record(input)) throw invalid();
  // This branch must preserve the old wire order and DTO for an unknown v16 write's same-key replay.
  // The server checks persisted replay before validating a new command against the new scope contract.
  const legacy = Object.hasOwn(input, "preauthorization_message_id") && input.preparation_scope_kind === undefined;
  const sent: ChatPolicyInput = Object.freeze({ enabled: input.enabled, allow_preparation: input.allow_preparation,
    ...(legacy ? { preauthorization_message_id: input.preauthorization_message_id }
      : input.preparation_scope_kind === undefined ? {} : { preparation_scope_kind: input.preparation_scope_kind }),
    action: input.action, max_admissions: input.max_admissions, expected_revision: input.expected_revision });
  if (typeof sent.enabled !== "boolean" || typeof sent.allow_preparation !== "boolean" || sent.action !== "text_submission"
    || !budget(sent.max_admissions) || !integer(sent.expected_revision) || sent.expected_revision >= Number.MAX_SAFE_INTEGER) throw invalid();
  if (legacy ? !text(sent.preauthorization_message_id) || (sent.preauthorization_message_id !== "" && !identifier(sent.preauthorization_message_id))
      || (sent.allow_preparation && !sent.preauthorization_message_id)
    : Boolean(input.preauthorization_message_id) || (sent.allow_preparation ? sent.preparation_scope_kind !== "topic_summary"
      : sent.preparation_scope_kind !== undefined)) throw invalid();
  return request(policyPath(groupId, topicId), { method: "PUT", headers: { "Content-Type": "application/json", "Idempotency-Key": key },
    body: JSON.stringify(sent) }, (value): value is ChatPolicy => isPolicy(value, groupId, topicId)
      && value.revision === sent.expected_revision + 1 && value.enabled === sent.enabled && value.allow_preparation === sent.allow_preparation
      && value.action === sent.action && value.max_admissions === sent.max_admissions
      && (legacy ? value.preauthorization_message_id === sent.preauthorization_message_id && !value.preparation_scope_kind && !value.preparation_supported
        : hasChatPolicyMetadata(value) && identifier(value.configuration_source_id)
          && (value.preparation_scope_kind ?? "") === (sent.preparation_scope_kind ?? "") && value.preparation_supported === sent.allow_preparation));
}
export async function getChatAdmission(groupId: string, taskId: string, signal?: AbortSignal): Promise<ChatAdmission | null> {
  if (!pathId(groupId) || !pathId(taskId)) throw invalid();
  try {
    return await request(`/api/v1/groups/${encodeURIComponent(groupId)}/tasks/${encodeURIComponent(taskId)}/chat-admission`, { signal },
      (value): value is ChatAdmission => isAdmission(value, groupId, taskId));
  } catch (error) {
    // Only a recognized absence is quiet; malformed or unexpected 404s remain unreadable.
    if (error instanceof LegacyPlainNotFound || (error instanceof ChatRejection && error.status === 404 && error.code === "not_found")) return null;
    throw error;
  }
}
