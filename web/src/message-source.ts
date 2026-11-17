export type MessageSourceKind = "local_user_chat" | "local_user_policy" | "member" | "platform" | "unknown";
export type MessageSource = { source_kind?: MessageSourceKind; source_id?: string };
const kinds: readonly string[] = ["local_user_chat", "local_user_policy", "member", "platform", "unknown"];
export const isMessageSourceKind = (value: unknown): value is MessageSourceKind => typeof value === "string" && kinds.includes(value);
const sourceId = (value: unknown): value is string => typeof value === "string" && value !== ""
  && !/^\p{White_Space}|\p{White_Space}$|[\0\r\n\ud800-\udfff]/u.test(value) && new TextEncoder().encode(value).length <= 4096;
// Missing/unknown provenance does not infer an identity from the sender.
export const sourceMatchesSender = (kind: MessageSourceKind | undefined, sender: unknown) =>
  kind === "local_user_chat" || kind === "local_user_policy" ? sender === "user" : kind !== "member" || sender !== "user";
export const hasMessageSourceIdentityConflict = (value: Record<string, unknown>) =>
  sourceId(value.source_id) && sourceId(value.sender_id) && isMessageSourceKind(value.source_kind)
    && !sourceMatchesSender(value.source_kind, value.sender_id);
export function hasValidMessageSource(value: Record<string, unknown>): boolean {
  if (value.source_kind === undefined) return value.source_id === undefined || value.source_id === "";
  if (!isMessageSourceKind(value.source_kind)) return false;
  if (value.source_kind === "unknown") return value.source_id === undefined || value.source_id === "";
  return sourceId(value.source_id) && sourceMatchesSender(value.source_kind, value.sender_id);
}
export const messageSourceLabel = (kind?: MessageSourceKind) => kind === "local_user_chat" ? "本机用户聊天命令"
  : kind === "local_user_policy" ? "用户策略配置命令" : kind === "member" ? "成员消息" : kind === "platform" ? "系统消息" : "来源未核验";
export function messageAuthor(source: MessageSource & { sender_id: string }, memberName = "未载入成员"): string {
  if (source.source_kind === "platform") return "系统";
  if (source.source_kind === "local_user_policy") return "我·策略设置";
  if (source.source_kind === "local_user_chat") return "我";
  return source.sender_id === "user" ? "用户（来源未核验）" : memberName;
}
