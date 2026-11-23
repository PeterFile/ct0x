import { ApiError } from "./api";

const terminals = ["succeeded", "failed", "aborted", "truncated"] as const;
const outcomes = [...terminals, "not_started", "unknown"] as const;
const runStates = [...outcomes, "starting", "running", "cancel_requested"] as const;
const operationStates = ["pending", "done", "unchanged", "unknown", "applied_sync_error"] as const;
const operationKinds = ["create", "replace"] as const;

export type AgentRunState = typeof runStates[number];
export type AgentRunSummary = {
  run_id: string; group_id: string; group_name: string; member_id: string; member_name: string;
  state: AgentRunState; created_at: string; updated_at: string;
};
export type AgentRunIdentity = Pick<AgentRunSummary, "group_id" | "run_id" | "member_id">;
export type AgentRunReceipt = {
  outcome: typeof outcomes[number]; observed_terminal: typeof terminals[number] | "";
  started: boolean; accepted: boolean; delivery_failed: boolean; direct_child_reaped: boolean; output_complete: boolean;
};
export type AgentRunMessage = { message_id: string; sender_id: string; preview: string; truncated: boolean };
export type AgentRunSubmission = {
  submission_id: string; task_id: string; task_revision: number; attempt_id: string; preview: string; truncated: boolean;
};
export type AgentRunDispatchDecision = {
  decision_id: string; request_id: string; task_id: string; reason_preview: string; truncated: boolean;
} & (
  | { kind: "assign"; worker_member_id: string; assignment_id: string }
  | { kind: "decline"; worker_member_id?: never; assignment_id?: never }
);
export type AgentRunWorkspaceOperation = {
  operation_id: string; task_id: string; attempt_id: string; kind: typeof operationKinds[number];
  relative_path: string; state: typeof operationStates[number];
  result: { observed_hash: string; observed_size: number; error_code: string; finished_at: string } | null;
};
export type AgentRunEntry = { id: string; created_at: string } & (
  | { kind: "trigger_message" | "published_reply"; message: AgentRunMessage }
  | { kind: "submission"; submission: AgentRunSubmission }
  | { kind: "dispatch_decision"; dispatch_decision: AgentRunDispatchDecision }
  | { kind: "workspace_operation"; workspace_operation: AgentRunWorkspaceOperation }
);
export type AgentRunPage<T> = { items: T[]; next_cursor: string };
export type AgentRunDetail = {
  run: AgentRunSummary; accepted_at: string | null; cancel_requested_at: string | null;
  receipt: AgentRunReceipt | null; timeline: AgentRunPage<AgentRunEntry>;
};

const record = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);
const text = (value: unknown): value is string => typeof value === "string";
const id = (value: unknown): value is string => text(value) && value.length > 0;
const selector = (value: unknown): value is string => id(value) && value.length <= 256 && !/[^!-~]|,/.test(value)
  && value !== "." && value !== "..";
const time = (value: unknown): value is string => text(value) && /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z$/.test(value)
  && Number.isFinite(Date.parse(value));
const maybeTime = (value: unknown) => value === null || time(value);
const oneOf = (value: unknown, choices: readonly string[]) => text(value) && choices.includes(value);
const preview = (value: Record<string, unknown>) => text(value.preview) && typeof value.truncated === "boolean";
const summary = (value: unknown): value is AgentRunSummary => record(value) && selector(value.run_id) && selector(value.group_id)
  && selector(value.member_id) && text(value.group_name) && text(value.member_name) && oneOf(value.state, runStates)
  && time(value.created_at) && time(value.updated_at);
const pageOf = <T>(value: unknown, item: (value: unknown) => value is T, before: string): value is AgentRunPage<T> => record(value)
  && Array.isArray(value.items) && value.items.every(item) && text(value.next_cursor)
  && (!value.next_cursor || (value.items.length > 0 && value.next_cursor !== before));

function entry(value: unknown, memberId: string): value is AgentRunEntry {
  if (!record(value) || !time(value.created_at)) return false;
  switch (value.kind) {
    case "trigger_message":
    case "published_reply": {
      const message = value.message;
      return record(message) && id(message.message_id) && value.id === `message:${message.message_id}`
        && id(message.sender_id) && (value.kind === "trigger_message" || message.sender_id === memberId) && preview(message);
    }
    case "submission": {
      const submission = value.submission;
      return record(submission) && id(submission.submission_id) && value.id === `submission:${submission.submission_id}`
        && id(submission.task_id) && id(submission.attempt_id) && typeof submission.task_revision === "number"
        && Number.isSafeInteger(submission.task_revision) && submission.task_revision > 0 && preview(submission);
    }
    case "dispatch_decision": {
      const decision = value.dispatch_decision;
      if (!record(decision) || !id(decision.decision_id) || value.id !== `dispatch_decision:${decision.decision_id}`
        || !id(decision.request_id) || !id(decision.task_id) || !text(decision.reason_preview) || typeof decision.truncated !== "boolean") return false;
      return (decision.kind === "assign" && id(decision.worker_member_id) && id(decision.assignment_id))
        || (decision.kind === "decline" && decision.worker_member_id === undefined && decision.assignment_id === undefined);
    }
    case "workspace_operation": {
      const op = value.workspace_operation;
      if (!record(op) || !id(op.operation_id) || value.id !== `workspace:${op.operation_id}` || !id(op.task_id) || !id(op.attempt_id)
        || !oneOf(op.kind, operationKinds) || !text(op.relative_path) || !oneOf(op.state, operationStates)) return false;
      // Registration is not an execution result. Only the persisted join supplies a receipt.
      const result = op.result;
      return op.state === "pending" ? result === null : record(result)
        && text(result.observed_hash) && (result.observed_hash === "" || /^[a-f0-9]{64}$/.test(result.observed_hash))
        && typeof result.observed_size === "number" && Number.isSafeInteger(result.observed_size) && result.observed_size >= 0
        && text(result.error_code) && /^[a-z0-9_]{0,64}$/.test(result.error_code) && time(result.finished_at);
    }
    default: return false;
  }
}

function receipt(value: unknown, state: AgentRunState): value is AgentRunReceipt | null {
  return value === null || (record(value) && value.outcome === state && oneOf(value.outcome, outcomes)
    && (value.observed_terminal === "" || oneOf(value.observed_terminal, terminals))
    && [value.started, value.accepted, value.delivery_failed, value.direct_child_reaped, value.output_complete]
      .every(flag => typeof flag === "boolean"));
}

async function read<T>(path: string, signal: AbortSignal | undefined, isValid: (value: unknown) => value is T): Promise<T> {
  let response: Response;
  try { response = await fetch(path, { method: "GET", signal, cache: "no-store", redirect: "error" }); }
  catch (error) {
    if (signal?.aborted) throw error;
    throw new ApiError("无法连接本地服务，请确认服务已启动。");
  }
  if (!response.ok) {
    const reason = response.status === 404 ? "未找到可查看的运行记录" : "无法读取运行记录";
    throw new ApiError(`${reason}（HTTP ${response.status}）。`, response.status, "http_error");
  }
  const value: unknown = await response.json().catch(() => null);
  if (response.status === 200 && isValid(value)) return value;
  throw new ApiError(`运行记录响应不符合约定（HTTP ${response.status}），请重试。`, response.status, "invalid_response");
}

const runsPath = "/api/v1/me/agent-runs";
function params(before: string) {
  const query = new URLSearchParams({ limit: "50" });
  if (before) query.set("before", before);
  return query;
}
export function listAgentRuns(memberId = "", before = "", signal?: AbortSignal) {
  const query = params(before);
  if (memberId) query.set("member_id", memberId);
  return read(`${runsPath}?${query}`, signal, (value): value is AgentRunPage<AgentRunSummary> => pageOf(value,
    (run): run is AgentRunSummary => summary(run) && (!memberId || run.member_id === memberId), before));
}
export async function getAgentRun({ group_id, run_id, member_id }: AgentRunIdentity, before = "", signal?: AbortSignal) {
  if (!selector(group_id) || !selector(run_id) || !selector(member_id)) throw new ApiError("无法读取运行记录：标识无效。", 0, "invalid_input");
  const query = params(before);
  query.set("group_id", group_id);
  // Keep the server's newest-first order and opaque cursor intact across pages.
  return read(`${runsPath}/${encodeURIComponent(run_id)}?${query}`, signal, (value): value is AgentRunDetail => record(value)
    && summary(value.run) && value.run.run_id === run_id && value.run.group_id === group_id && value.run.member_id === member_id
    && maybeTime(value.accepted_at) && maybeTime(value.cancel_requested_at) && receipt(value.receipt, value.run.state)
    && pageOf(value.timeline, (item): item is AgentRunEntry => entry(item, member_id), before));
}
