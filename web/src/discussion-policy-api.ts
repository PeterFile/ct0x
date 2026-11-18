import { ApiError } from "./api";

export type DiscussionInviteHops = 0 | 1 | 2;
export type DiscussionPolicyInput = Readonly<{ expected_revision: number; max_invite_hops: DiscussionInviteHops }>;
export type DiscussionPolicy = Readonly<{
  group_id: string; topic_id: ""; revision: number; max_invite_hops: DiscussionInviteHops; max_targeted_wakes: number;
}>;

export const isDiscussionInviteHops = (value: unknown): value is DiscussionInviteHops => value === 0 || value === 1 || value === 2;
const record = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);

const rejections: Readonly<Record<number, readonly string[]>> = {
  400: ["invalid_input", "invalid_json"], 403: ["invalid_host", "invalid_origin"], 404: ["not_found"],
  405: ["method_not_allowed"], 409: ["conflict"], 413: ["body_too_large"], 415: ["unsupported_media_type"],
};
export const isDiscussionPolicyDefinitiveRejection = (error: unknown) => error instanceof ApiError
  && (rejections[error.status]?.includes(error.code) ?? false);

// The task panel has no thread selector: this client edits only the main topic.
async function request(groupId: string, init: RequestInit, input?: DiscussionPolicyInput): Promise<DiscussionPolicy> {
  let response: Response;
  try {
    response = await fetch(`/api/v1/groups/${encodeURIComponent(groupId)}/coordination/discussion-policy`,
      { ...init, cache: "no-store", redirect: "error" });
  } catch {
    throw new ApiError("无法连接本地服务。");
  }
  const value: unknown = await response.json().catch(() => null);
  if (response.status === 200 && record(value) && value.group_id === groupId && value.topic_id === ""
    && typeof value.revision === "number" && Number.isSafeInteger(value.revision) && value.revision >= 0
    && isDiscussionInviteHops(value.max_invite_hops) && value.max_targeted_wakes === 2 * value.max_invite_hops
    && (value.revision !== 0 || value.max_invite_hops === 2)
    && (!input || (value.revision === input.expected_revision + 1 && value.max_invite_hops === input.max_invite_hops))) {
    return value as DiscussionPolicy;
  }
  const detail = !response.ok && record(value) && record(value.error) ? value.error : {};
  const code = typeof detail.code === "string" && typeof detail.message === "string" && detail.message.trim() ? detail.code : "invalid_response";
  throw new ApiError(`讨论策略请求未完成或响应不符合约定（HTTP ${response.status}）。`, response.status, code);
}

export const getDiscussionPolicy = (groupId: string, signal: AbortSignal) => request(groupId, { signal });
export const setDiscussionPolicy = (groupId: string, input: DiscussionPolicyInput, key: string) => request(groupId, {
  method: "PUT", headers: { "Content-Type": "application/json", "Idempotency-Key": key }, body: JSON.stringify(input),
}, input);
