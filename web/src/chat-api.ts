import { ApiError } from "./api";
import { isMessageSourceKind, sourceMatchesSender } from "./message-source";
import type { MessageSourceKind } from "./message-source";

export type ChatInvestigationPolicy = Readonly<{
  objective: string; allow_topic_messages: boolean;
  artifact_versions: readonly Readonly<{ artifact_id: string; version_id: string }>[] | null;
  max_delegations: number;
}>;
export const chatInvestigationLimits = { objectiveBytes: 256, artifactVersions: 16, idBytes: 128, maxDelegations: 4 } as const;

// Experimental local chat-authority contract; absent legacy metadata is never promoted into authority.
export type ChatPolicy = Readonly<{
  group_id: string; topic_id: string; enabled: boolean; allow_preparation: boolean;
  preauthorization_message_id: string; action: "" | "text_submission"; max_admissions: number;
  revision: number; authorized_by: "" | "user"; preauthorization_text?: string;
  preparation_scope_kind?: string; preparation_supported?: boolean; configuration_source_id?: string;
  allow_workspace_changes?: boolean; workspace_changes_supported?: boolean;
  investigation?: ChatInvestigationPolicy | null;
  investigation_supported?: boolean; investigation_execution_enabled?: boolean;
  automatic_text_dispatch?: boolean;
}>;
export type ChatPolicyInput = Readonly<{
  enabled: boolean; allow_preparation: boolean; preparation_scope_kind?: "topic_summary"; allow_workspace_changes?: boolean;
  investigation?: ChatInvestigationPolicy | null; automatic_text_dispatch?: boolean;
  // Retained only for an already-submitted v16 operation; new UI commands never populate this field.
  preauthorization_message_id?: string;
  action: "text_submission"; max_admissions: number; expected_revision: number;
}>;
export type TaskWorkspaceGrant = Readonly<{
  grant_id: string; member_id: string; root: string; resolved_root: string; grant_schema_version: 19;
}>;
export type TaskWorkspaceScope = Readonly<{
  group_id: string; topic_id: string; task_id: string; task_revision: number; mandate_id: string;
  candidate_id: string; assessment_id: string; dispatch_request_id: string; policy_revision: number;
  resource_key: string; node_id: "local"; grants: readonly TaskWorkspaceGrant[];
}>;
export type ChatRepairLink = Readonly<{
  id: string; group_id: string; topic_id: string; repair_task_id: string; parent_task_id: string;
  parent_submission_id: string; parent_review_id: string; reviewed_task_revision: number; parent_mandate_id: string;
  candidate_id: string; assessment_id: string; mandate_id: string; dispatch_request_id: string; policy_revision: number;
  authorization: "review_request" | "original_agreement" | "chat_request";
  // This evidence label does not extend the global message source kinds.
  authorization_source_kind: "local_user_review" | "local_user_chat" | "local_user_policy";
  authorization_source_id: string; authorization_message_id?: string; authorization_policy_revision: number;
  artifact_versions: readonly Readonly<{ artifact_id: string; version_id: string }>[]; created_at: string;
}>;
export type ChatAdmission = Readonly<{
  id: string; candidate_id: string; group_id: string; topic_id: string; message_id: string;
  source_sender_id: string; assessed_by: string; turn_id: string; policy_revision: number; candidate_generation: number;
  disposition: "request_work" | "prepare_work"; authority: "explicit_request" | "written_preauthorization";
  authorization_message_id: string; action: "text_submission" | "workspace_change" | "read_only_investigation"; task_id: string; mandate_id: string;
  dispatch_request_id: string; notice_message_id: string; created_at: string;
  source_id?: string; source_kind?: MessageSourceKind; purpose?: string; authority_evidence_verified?: boolean;
  outcome?: "" | "admitted"; reason_code?: string; question?: string; workspace_scope?: TaskWorkspaceScope; repair_link?: ChatRepairLink;
}>;
const record = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);
const text = (value: unknown): value is string => typeof value === "string" && !/[\ud800-\udfff]/u.test(value);
const nonblank = (value: unknown): value is string => text(value) && /\P{White_Space}/u.test(value);
const identifier = (value: unknown, maxBytes = 4096): value is string => text(value) && value !== "" && !/^\p{White_Space}|\p{White_Space}$/u.test(value)
  && !/[\0\r\n]/u.test(value) && new TextEncoder().encode(value).length <= maxBytes;
const pathId = (value: unknown): value is string => identifier(value) && value !== "." && value !== "..";
const integer = (value: unknown): value is number => typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
const positive = (value: unknown): value is number => integer(value) && value > 0;
const budget = (value: unknown): value is number => integer(value) && value >= 1 && value <= 100;
const keyValid = (value: string) => value.length > 0 && value.length <= 128 && !/[^\x21-\x7e]|,/.test(value);
const policyFields = ["group_id", "topic_id", "enabled", "allow_preparation", "preauthorization_message_id", "action", "max_admissions", "revision", "authorized_by"];
export const hasChatPolicyMetadata = (policy: ChatPolicy) => typeof policy.preparation_supported === "boolean"
  && typeof policy.configuration_source_id === "string";
const policyMetadata = ["preauthorization_text", "preparation_scope_kind", "preparation_supported", "configuration_source_id",
  "allow_workspace_changes", "workspace_changes_supported", "investigation", "investigation_supported", "investigation_execution_enabled",
  "automatic_text_dispatch"];
const investigationFields = ["objective", "allow_topic_messages", "artifact_versions", "max_delegations"];
export function isChatInvestigationPolicy(value: unknown): value is ChatInvestigationPolicy {
  const limits = chatInvestigationLimits;
  if (!record(value) || Object.keys(value).length !== investigationFields.length || !investigationFields.every(field => Object.hasOwn(value, field))
    || !nonblank(value.objective) || value.objective.includes("\0") || new TextEncoder().encode(value.objective).length > limits.objectiveBytes
    || typeof value.allow_topic_messages !== "boolean" || !integer(value.max_delegations) || value.max_delegations > limits.maxDelegations) return false;
  if (value.artifact_versions === null) return value.allow_topic_messages;
  if (!Array.isArray(value.artifact_versions) || value.artifact_versions.length > limits.artifactVersions
    || (!value.allow_topic_messages && !value.artifact_versions.length)) return false;
  const seen = new Set<string>();
  for (const ref of value.artifact_versions) {
    if (!record(ref) || Object.keys(ref).length !== 2 || !Object.hasOwn(ref, "artifact_id") || !Object.hasOwn(ref, "version_id")
      || !identifier(ref.artifact_id, limits.idBytes) || !identifier(ref.version_id, limits.idBytes)) return false;
    const key = JSON.stringify([ref.artifact_id, ref.version_id]);
    if (seen.has(key)) return false;
    seen.add(key);
  }
  return true;
}
export function sameChatInvestigation(left: ChatPolicy["investigation"], right: ChatPolicy["investigation"]): boolean {
  if (!left || !right) return !left && !right;
  // A Go nil slice denotes no Artifact versions. Preserve all strings and pair order.
  const leftRefs = left.artifact_versions ?? [], rightRefs = right.artifact_versions ?? [];
  return left.objective === right.objective && left.allow_topic_messages === right.allow_topic_messages && left.max_delegations === right.max_delegations
    && leftRefs.length === rightRefs.length && leftRefs.every((ref, index) =>
      ref.artifact_id === rightRefs[index].artifact_id && ref.version_id === rightRefs[index].version_id);
}
function isPolicy(value: unknown, groupId: string, topicId: string): value is ChatPolicy {
  if (!record(value) || !policyFields.every(field => Object.hasOwn(value, field))
    || Object.keys(value).some(field => !policyFields.includes(field) && !policyMetadata.includes(field))
    || value.group_id !== groupId || value.topic_id !== topicId || !integer(value.revision)
    || typeof value.enabled !== "boolean" || typeof value.allow_preparation !== "boolean"
    || !text(value.preauthorization_message_id) || (value.preauthorization_text !== undefined && !text(value.preauthorization_text))
    || (value.allow_workspace_changes !== undefined && typeof value.allow_workspace_changes !== "boolean")
    || (value.workspace_changes_supported !== undefined && typeof value.workspace_changes_supported !== "boolean")
    || (value.allow_workspace_changes === true && value.workspace_changes_supported === false)
    || (value.automatic_text_dispatch !== undefined && typeof value.automatic_text_dispatch !== "boolean")
    || (value.automatic_text_dispatch === true && (!positive(value.revision) || !identifier(value.configuration_source_id)))
    || (value.investigation_supported !== undefined && typeof value.investigation_supported !== "boolean")
    || (value.investigation_execution_enabled !== undefined && typeof value.investigation_execution_enabled !== "boolean")
    || (value.investigation != null && (!positive(value.revision) || !identifier(value.configuration_source_id)
      || !isChatInvestigationPolicy(value.investigation)))) return false;
  const metadata = ["preparation_scope_kind", "preparation_supported", "configuration_source_id"].some(field => Object.hasOwn(value, field));
  if (metadata && (typeof value.preparation_supported !== "boolean" || !text(value.configuration_source_id)
    || (value.preparation_scope_kind !== undefined && !text(value.preparation_scope_kind)))) return false;
  const scope = value.preparation_scope_kind ?? "";
  // B fbe95f8 reflects Go-escaped investigation JSON in the topic_summary rule. Even with 6×
  // expansion of the objective and all 32 IDs, the decoded text with its fixed prefix fits in 32 KiB.
  const ruleBytes = value.allow_preparation && value.preparation_supported === true && scope === "topic_summary"
    && value.investigation != null ? 32 * 1024 : 8192;
  if (value.revision === 0) return !value.enabled && !value.allow_preparation && value.preauthorization_message_id === ""
    && value.action === "" && value.max_admissions === 0 && value.authorized_by === "" && !value.preauthorization_text && !value.allow_workspace_changes
    && (!metadata || (!value.preparation_supported && value.configuration_source_id === "" && scope === ""));
  if (value.action !== "text_submission" || value.authorized_by !== "user" || !budget(value.max_admissions)
    || (value.allow_preparation && !identifier(value.preauthorization_message_id))
    || (value.preauthorization_message_id === "" ? Boolean(value.preauthorization_text)
      : !identifier(value.preauthorization_message_id) || !nonblank(value.preauthorization_text) || value.preauthorization_text.includes("\0")
        || new TextEncoder().encode(value.preauthorization_text).length > ruleBytes)) return false;
  if (!metadata) return true;
  if (value.configuration_source_id === "") return !value.preparation_supported && scope === "";
  if (!identifier(value.configuration_source_id)) return false;
  if (!value.allow_preparation) return !value.preparation_supported && scope === "" && value.preauthorization_message_id === "";
  return value.preparation_supported === (scope === "topic_summary");
}
const admissionFields = ["id", "candidate_id", "group_id", "topic_id", "message_id", "source_sender_id", "assessed_by", "turn_id",
  "policy_revision", "candidate_generation", "disposition", "authority", "authorization_message_id", "action", "task_id", "mandate_id",
  "dispatch_request_id", "notice_message_id", "created_at"];
const absoluteRoot = (value: unknown): value is string => text(value) && value.startsWith("/") && !value.includes("\0");
function isUTCTimestamp(value: unknown, strictCalendar: boolean): boolean {
  if (!text(value) || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$/.test(value)) return false;
  const stamp = Date.parse(value);
  // Round-trip the calendar fields without restricting fractional-second precision or legacy admissions.
  return Number.isFinite(stamp) && (!strictCalendar || new Date(stamp).toISOString().slice(0, 19) === value.slice(0, 19));
}
function isWorkspaceScope(value: unknown, admission: Record<string, unknown>): value is TaskWorkspaceScope {
  const fields = ["group_id", "topic_id", "task_id", "task_revision", "mandate_id", "candidate_id", "assessment_id",
    "dispatch_request_id", "policy_revision", "resource_key", "node_id", "grants"];
  if (!record(value) || Object.keys(value).length !== fields.length || !fields.every(field => Object.hasOwn(value, field))
    || !["group_id", "topic_id", "task_id", "mandate_id", "candidate_id", "dispatch_request_id", "policy_revision"].every(field => value[field] === admission[field])
    || value.assessment_id !== admission.id || !positive(value.task_revision) || value.node_id !== "local"
    || !identifier(value.resource_key) || !value.resource_key.startsWith("workspace:") || value.resource_key === "workspace:"
    || !Array.isArray(value.grants) || !value.grants.length) return false;
  const members = new Set<string>(), grants = new Set<string>();
  return value.grants.every(grant => {
    if (!record(grant) || Object.keys(grant).length !== 5 || !pathId(grant.grant_id) || !pathId(grant.member_id)
      || !absoluteRoot(grant.root) || !absoluteRoot(grant.resolved_root) || grant.grant_schema_version !== 19
      || members.has(grant.member_id) || grants.has(grant.grant_id)) return false;
    members.add(grant.member_id); grants.add(grant.grant_id); return true;
  });
}
function isRepairLink(value: unknown, admission: Record<string, unknown>): value is ChatRepairLink {
  const fields = ["id", "group_id", "topic_id", "repair_task_id", "parent_task_id", "parent_submission_id", "parent_review_id",
    "reviewed_task_revision", "parent_mandate_id", "candidate_id", "assessment_id", "mandate_id", "dispatch_request_id", "policy_revision",
    "authorization", "authorization_source_kind", "authorization_source_id", "authorization_policy_revision", "artifact_versions", "created_at"];
  if (!record(value) || !fields.every(field => Object.hasOwn(value, field))
    || Object.keys(value).some(field => !fields.includes(field) && field !== "authorization_message_id")
    || !["group_id", "topic_id", "candidate_id", "mandate_id", "dispatch_request_id", "policy_revision"].every(field => value[field] === admission[field])
    || value.repair_task_id !== admission.task_id || value.assessment_id !== admission.id || value.parent_task_id === value.repair_task_id
    || !["id", "parent_task_id", "parent_submission_id", "parent_review_id", "parent_mandate_id", "authorization_source_id"].every(field => identifier(value[field]))
    || !positive(value.reviewed_task_revision) || !positive(value.authorization_policy_revision)
    || admission.disposition !== "request_work" || admission.outcome !== "admitted" || admission.purpose !== "chat_intake/v1"
    || admission.authority_evidence_verified !== true || !isUTCTimestamp(value.created_at, true)
    || !Array.isArray(value.artifact_versions) || (!value.artifact_versions.length && admission.action !== "text_submission")) return false;
  const versions = new Set<string>();
  if (!value.artifact_versions.every(ref => {
    if (!record(ref) || Object.keys(ref).length !== 2 || !pathId(ref.artifact_id) || !pathId(ref.version_id)) return false;
    const key = JSON.stringify([ref.artifact_id, ref.version_id]);
    if (versions.has(key)) return false;
    versions.add(key); return true;
  })) return false;
  if (value.authorization_source_kind === "local_user_review") {
    if (Object.hasOwn(value, "authorization_message_id") || admission.authorization_message_id !== "") return false;
  } else if ((value.authorization_source_kind !== "local_user_chat" && value.authorization_source_kind !== "local_user_policy")
    || !identifier(value.authorization_message_id) || admission.authorization_message_id !== value.authorization_message_id) return false;
  if (admission.authority !== (value.authorization_source_kind === "local_user_policy" ? "written_preauthorization" : "explicit_request")) return false;
  if (value.authorization === "chat_request") return admission.source_kind === "local_user_chat"
    && value.authorization_source_kind === "local_user_chat" && value.authorization_source_id === admission.source_id
    && value.authorization_message_id === admission.message_id && value.authorization_policy_revision === admission.policy_revision;
  if (admission.source_kind !== "platform" || admission.source_id !== `review:${value.parent_review_id}`) return false;
  return value.authorization === "review_request" ? value.authorization_source_kind === "local_user_review"
    && value.authorization_source_id === value.parent_review_id && value.authorization_policy_revision === admission.policy_revision
    : value.authorization === "original_agreement" && positive(admission.policy_revision) && value.authorization_policy_revision <= admission.policy_revision;
}
function isAdmission(value: unknown, groupId: string, taskId: string): value is ChatAdmission {
  const metadata = ["source_id", "source_kind", "purpose", "authority_evidence_verified", "outcome", "reason_code", "question", "workspace_scope", "repair_link"];
  if (!record(value) || Object.keys(value).some(field => !admissionFields.includes(field) && !metadata.includes(field))
    || !admissionFields.every(field => Object.hasOwn(value, field))
    || value.group_id !== groupId || value.task_id !== taskId || !text(value.topic_id)
    || (value.action !== "text_submission" && value.action !== "workspace_change" && value.action !== "read_only_investigation")
    || !positive(value.policy_revision) || !positive(value.candidate_generation)
    || !["id", "candidate_id", "message_id", "source_sender_id", "assessed_by", "turn_id", "mandate_id", "notice_message_id"].every(field => identifier(value[field]))
    || !text(value.authorization_message_id) || !pathId(value.dispatch_request_id)
    || !isUTCTimestamp(value.created_at, value.action === "read_only_investigation")) return false;
  if (value.action === "workspace_change" ? value.disposition !== "request_work" || !isWorkspaceScope(value.workspace_scope, value)
    : Object.hasOwn(value, "workspace_scope")) return false;
  if ((value.source_kind !== undefined && !isMessageSourceKind(value.source_kind))
    || (value.source_id !== undefined && value.source_id !== "" && !identifier(value.source_id))
    || (value.purpose !== undefined && value.purpose !== "" && value.purpose !== "chat_intake/v1")
    || (value.authority_evidence_verified !== undefined && typeof value.authority_evidence_verified !== "boolean")
    || (value.outcome !== undefined && value.outcome !== "" && value.outcome !== "admitted")
    || (value.reason_code !== undefined && value.reason_code !== "") || (value.question !== undefined && value.question !== "")) return false;
  const knownSource = value.source_kind !== undefined && value.source_kind !== "unknown";
  if (knownSource ? !identifier(value.source_id) : value.source_id !== undefined && value.source_id !== "") return false;
  if (!sourceMatchesSender(value.source_kind, value.source_sender_id)) return false;
  if (Object.hasOwn(value, "repair_link")) return value.action !== "read_only_investigation" && isRepairLink(value.repair_link, value);
  if (!identifier(value.authorization_message_id)) return false;
  if (value.authority_evidence_verified === true && (value.purpose !== "chat_intake/v1"
    || (value.source_kind !== "local_user_chat" && value.source_kind !== "member")
    || (value.disposition === "request_work" && value.source_kind !== "local_user_chat"))) return false;
  if (value.action === "read_only_investigation") return value.disposition === "prepare_work" && value.authority === "written_preauthorization"
    && value.authority_evidence_verified === true && value.outcome === "admitted" && value.authorization_message_id !== value.message_id;
  return value.disposition === "request_work" ? value.authority === "explicit_request" && value.source_sender_id === "user"
    && value.authorization_message_id === value.message_id
    : value.disposition === "prepare_work" && value.authority === "written_preauthorization";
}
function decode(buffer: ArrayBuffer): unknown {
  const source = new TextDecoder("utf-8", { fatal: true }).decode(buffer), maximum = String(Number.MAX_SAFE_INTEGER);
  // Every numeric wire field in these contracts is a nonnegative integer. Reject rounding before parse.
  const tokens = /"(?:[^"\\]|\\.)*"|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?/uy;
  const stack: { keys: Set<string> | null; expectingKey: boolean }[] = [];
  for (let index = 0; index < source.length; index++) {
    const part = source[index], top = stack.at(-1);
    if (part === "{" || part === "[") stack.push({ keys: part === "{" ? new Set() : null, expectingKey: part === "{" });
    else if (part === "}" || part === "]") stack.pop();
    else if (part === "," && top?.keys) top.expectingKey = true;
    else if (part === ":" && top) top.expectingKey = false;
    if (!/["\d-]/u.test(part)) continue;
    tokens.lastIndex = index;
    const token = tokens.exec(source)?.[0];
    if (token === undefined) return null;
    index = tokens.lastIndex - 1;
    if (token.startsWith('"')) {
      if (top?.keys && top.expectingKey) {
        const key: string = JSON.parse(token);
        if (top.keys.has(key)) return null;
        top.keys.add(key); top.expectingKey = false;
      }
      continue;
    }
    if (!/^(?:0|[1-9]\d*)$/.test(token) || token.length > maximum.length || (token.length === maximum.length && token > maximum)) return null;
  }
  return JSON.parse(source);
}
class ChatRejection extends ApiError {}
class CanonicalNotFound extends ChatRejection {}
class LegacyPlainNotFound extends ApiError {}
function isCanonicalNotFound(body: ArrayBuffer | null, message: string): boolean {
  if (body === null) return false;
  // Match WriteJSON's raw shape, not a parsed envelope that may have lost duplicate keys.
  const canonical = JSON.stringify({ error: { code: "not_found", message } })
    .replace(/[<>&\u2028\u2029]/g, char => `\\u${char.charCodeAt(0).toString(16).padStart(4, "0")}`);
  return new TextDecoder("utf-8", { fatal: true }).decode(body).trim() === canonical;
}
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
    const quietNotFound = response.status === 404 && value.error.code === "not_found" && isCanonicalNotFound(body, value.error.message);
    const ErrorType = quietNotFound ? CanonicalNotFound : definitive ? ChatRejection : ApiError;
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
  const investigation = input.investigation;
  if ((input.automatic_text_dispatch !== undefined && typeof input.automatic_text_dispatch !== "boolean")
    || (legacy && input.automatic_text_dispatch === true)
    || (investigation != null && (legacy || !isChatInvestigationPolicy(investigation)))) throw invalid();
  const sent: ChatPolicyInput = Object.freeze({ enabled: input.enabled, allow_preparation: input.allow_preparation,
    ...(legacy ? { preauthorization_message_id: input.preauthorization_message_id }
      : input.preparation_scope_kind === undefined ? {} : { preparation_scope_kind: input.preparation_scope_kind }),
    action: input.action, max_admissions: input.max_admissions, expected_revision: input.expected_revision,
    ...(input.allow_workspace_changes === true ? { allow_workspace_changes: true } : {}),
    ...(investigation == null ? {} : { investigation: { objective: investigation.objective, allow_topic_messages: investigation.allow_topic_messages,
      artifact_versions: investigation.artifact_versions?.map(({ artifact_id, version_id }) => ({ artifact_id, version_id })) ?? null,
      max_delegations: investigation.max_delegations } }),
    ...(input.automatic_text_dispatch === true ? { automatic_text_dispatch: true } : {}) });
  if (typeof sent.enabled !== "boolean" || typeof sent.allow_preparation !== "boolean" || sent.action !== "text_submission"
    || !budget(sent.max_admissions) || !integer(sent.expected_revision) || sent.expected_revision >= Number.MAX_SAFE_INTEGER
    || (input.allow_workspace_changes !== undefined && typeof input.allow_workspace_changes !== "boolean")) throw invalid();
  if (legacy ? !text(sent.preauthorization_message_id) || (sent.preauthorization_message_id !== "" && !identifier(sent.preauthorization_message_id))
      || (sent.allow_preparation && !sent.preauthorization_message_id)
    : Boolean(input.preauthorization_message_id) || (sent.allow_preparation ? sent.preparation_scope_kind !== "topic_summary"
      : sent.preparation_scope_kind !== undefined)) throw invalid();
  return request(policyPath(groupId, topicId), { method: "PUT", headers: { "Content-Type": "application/json", "Idempotency-Key": key },
    body: JSON.stringify(sent) }, (value): value is ChatPolicy => isPolicy(value, groupId, topicId)
      && value.revision === sent.expected_revision + 1 && value.enabled === sent.enabled && value.allow_preparation === sent.allow_preparation
      && value.action === sent.action && value.max_admissions === sent.max_admissions
      && (value.allow_workspace_changes ?? false) === (sent.allow_workspace_changes ?? false)
      && (!sent.allow_workspace_changes || value.workspace_changes_supported === true)
      && sameChatInvestigation(value.investigation, sent.investigation)
      && (value.automatic_text_dispatch ?? false) === (sent.automatic_text_dispatch ?? false)
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
    if (error instanceof LegacyPlainNotFound || error instanceof CanonicalNotFound) return null;
    throw error;
  }
}
