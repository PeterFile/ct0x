import { ApiError } from "./api";

export type Task = {
  id: string; group_id: string; title: string; description: string; acceptance: string;
  assignee_id: string; dependency_ids: string[]; status: "open" | "submitted" | "accepted";
  revision: number; active_attempt_id: string; current_submission_id: string; created_at: string;
};
export type TaskInput = Readonly<{
  title: string; description?: string; acceptance: string; assignee_id?: string; dependency_ids?: readonly string[];
}>;
export type TaskAttempt = {
  id: string; group_id: string; task_id: string; task_revision: number; turn_id: string; created_at: string;
};
export type ArtifactVersionRef = { artifact_id: string; version_id: string };
export type TaskSubmission = {
  id: string; group_id: string; task_id: string; task_revision: number; attempt_id: string;
  summary: string; artifact_versions: ArtifactVersionRef[]; submitted_by: string; created_at: string;
};
export type TaskReview = {
  id: string; group_id: string; task_id: string; task_revision: number; submission_id: string;
  outcome: "accepted" | "rejected"; reason: string; reviewed_by: string; artifact_versions: ArtifactVersionRef[]; created_at: string;
};
export type TaskAttemptInput = Readonly<{ expected_revision: number; expected_active_attempt_id: string }>;
export type TaskSubmissionInput = Readonly<{
  expected_revision: number; attempt_id: string; summary: string; artifact_versions?: readonly Readonly<ArtifactVersionRef>[];
}>;
export type TaskReviewInput = Readonly<{
  expected_revision: number; submission_id: string; outcome: TaskReview["outcome"]; reason?: string;
}>;
type WorkRecord = {
  id: string; group_id: string; topic_id: string; task_id: string; task_revision: number; created_at: string;
};
type TaskWorkInputReturn = {
  member_id: string; turn_id: string; attempt_id: string; reason: string;
  question: string; notice_message_id: string; returned_at: string;
};
export type TaskWorkInput = WorkRecord & {
  content?: string; source_id?: string; reply_message_id?: string; notice_message_id?: string;
  trigger_id?: string; mandate_id?: string; assignment_id?: string; worker_member_id?: string;
  before_constraint_revision?: number; before_constraint?: string;
  after_constraint_revision?: number; after_constraint?: string;
  consumption?: { turn_id: string; consumed_at: string } | null;
  return?: TaskWorkInputReturn | null;
};
export type TaskWorkReport = WorkRecord & {
  kind: string; summary: string; actor_id: string; owner_member_id: string;
};
type TaskPage<T = Task> = { items: T[]; next_cursor: string };
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

// Match Go strings.TrimSpace (Unicode White_Space), not JavaScript trim.
const nonblank = (value: unknown): value is string => typeof value === "string" && /\P{White_Space}/u.test(value);
function validEvidenceTime(value: unknown): value is string {
  if (typeof value !== "string") return false;
  // Evidence is normalized to UTC; Go JSON preserves up to nine fractional digits.
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?Z$/.exec(value);
  if (!match || match[0] !== value) return false;
  const [year, month, day, hour, minute, second] = match.slice(1, 7).map(Number);
  const leapYear = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const daysInMonth = [31, leapYear ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  return year >= 1 && month >= 1 && month <= 12 && day >= 1 && day <= daysInMonth[month - 1]
    && hour < 24 && minute < 60 && second < 60
    && !(year === 1 && month === 1 && day === 1 && hour === 0 && minute === 0 && second === 0 && Number(match[7] ?? 0) === 0);
}
const evidenceIn = (groupId: string, taskId: string) => (value: unknown): value is Record<string, unknown> => isRecord(value)
  && nonblank(value.id) && value.group_id === groupId && value.task_id === taskId && validEvidenceTime(value.created_at)
  && typeof value.task_revision === "number" && Number.isSafeInteger(value.task_revision) && value.task_revision > 0;
const validRefs = (value: unknown): value is ArtifactVersionRef[] => Array.isArray(value)
  && value.every(ref => isRecord(ref) && nonblank(ref.artifact_id) && nonblank(ref.version_id));
const attemptIn = (groupId: string, taskId: string) => (value: unknown): value is TaskAttempt => evidenceIn(groupId, taskId)(value)
  && typeof value.turn_id === "string" && (value.turn_id === "" || nonblank(value.turn_id));
const submissionIn = (groupId: string, taskId: string) => (value: unknown): value is TaskSubmission => evidenceIn(groupId, taskId)(value)
  && nonblank(value.attempt_id) && nonblank(value.summary) && nonblank(value.submitted_by) && validRefs(value.artifact_versions);
const reviewIn = (groupId: string, taskId: string) => (value: unknown): value is TaskReview => evidenceIn(groupId, taskId)(value)
  && nonblank(value.submission_id) && nonblank(value.reviewed_by) && typeof value.reason === "string"
  && (value.outcome === "accepted" || value.outcome === "rejected") && validRefs(value.artifact_versions);
const sameRefs = (left: readonly ArtifactVersionRef[], right: readonly ArtifactVersionRef[]) => {
  const pairs = (refs: readonly ArtifactVersionRef[]) => new Set(refs.map(ref => JSON.stringify([ref.artifact_id, ref.version_id])));
  const a = pairs(left), b = pairs(right);
  return a.size === b.size && [...a].every(pair => b.has(pair));
};

async function request<T>(path: string, init: RequestInit, valid: (value: unknown) => value is T, expectedStatus = 200): Promise<T> {
  let response: Response;
  try { response = await fetch(path, init.method === "POST" ? { ...init, redirect: "error" } : init); }
  catch (error) {
    if (init.signal?.aborted) throw error;
    throw new ApiError("无法连接本地服务，请确认服务已启动。");
  }
  const data: unknown = await response.json().catch(() => null);
  if (!response.ok) {
    const error = isRecord(data) && isRecord(data.error) ? data.error : {};
    const reason = response.status === 400 ? "输入未通过验证，请检查输入信息"
      : response.status === 404 ? "未找到请求的服务或记录"
      : response.status === 409 ? "任务状态、版本、依赖或请求内容冲突"
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

function post<T>(path: string, body: object, key: string, valid: (value: unknown) => value is T, signal?: AbortSignal) {
  return request(path, {
    method: "POST", signal,
    headers: { "Content-Type": "application/json", "Idempotency-Key": key },
    body: JSON.stringify(body),
  }, valid, 201);
}
const tasksPath = (groupId: string) => `/api/v1/groups/${encodeURIComponent(groupId)}/tasks`;
const taskPath = (groupId: string, taskId: string) => `${tasksPath(groupId)}/${encodeURIComponent(taskId)}`;
export function registerTaskAttempt(groupId: string, taskId: string, input: TaskAttemptInput, key: string, signal?: AbortSignal) {
  const body = { expected_revision: input.expected_revision, expected_active_attempt_id: input.expected_active_attempt_id };
  return post(`${taskPath(groupId, taskId)}/attempts`, body, key,
    (value): value is TaskAttempt => attemptIn(groupId, taskId)(value)
      && value.task_revision === body.expected_revision && value.turn_id === "", signal);
}
export function getTaskAttempt(groupId: string, taskId: string, attemptId: string, signal?: AbortSignal) {
  return request(`${taskPath(groupId, taskId)}/attempts/${encodeURIComponent(attemptId)}`, { signal },
    (value): value is TaskAttempt => attemptIn(groupId, taskId)(value) && value.id === attemptId);
}
export function submitTask(groupId: string, taskId: string, input: TaskSubmissionInput, key: string, signal?: AbortSignal) {
  const body = { expected_revision: input.expected_revision, attempt_id: input.attempt_id, summary: input.summary,
    artifact_versions: input.artifact_versions?.map(ref => ({ artifact_id: ref.artifact_id, version_id: ref.version_id })) };
  return post(`${taskPath(groupId, taskId)}/submissions`, body, key,
    (value): value is TaskSubmission => submissionIn(groupId, taskId)(value)
      && value.task_revision === body.expected_revision && value.attempt_id === body.attempt_id
      && value.summary === body.summary && value.submitted_by === "user" && sameRefs(value.artifact_versions, body.artifact_versions ?? []), signal);
}
export function getTaskSubmission(groupId: string, taskId: string, submissionId: string, signal?: AbortSignal) {
  return request(`${taskPath(groupId, taskId)}/submissions/${encodeURIComponent(submissionId)}`, { signal },
    (value): value is TaskSubmission => submissionIn(groupId, taskId)(value) && value.id === submissionId);
}
export function reviewTask(groupId: string, taskId: string, input: TaskReviewInput, submission: Readonly<TaskSubmission>, key: string, signal?: AbortSignal) {
  const body = { expected_revision: input.expected_revision, submission_id: input.submission_id, outcome: input.outcome, reason: input.reason };
  // Snapshot the loaded evidence before fetch; caller state can change while the receipt is pending.
  const source = { id: submission.id, group_id: submission.group_id, task_id: submission.task_id, task_revision: submission.task_revision,
    artifact_versions: submission.artifact_versions.map(ref => ({ artifact_id: ref.artifact_id, version_id: ref.version_id })) };
  return post(`${taskPath(groupId, taskId)}/reviews`, body, key,
    (value): value is TaskReview => reviewIn(groupId, taskId)(value) && value.task_revision === body.expected_revision
      && value.submission_id === body.submission_id && value.outcome === body.outcome && value.reason === (body.reason ?? "") && value.reviewed_by === "user"
      && source.group_id === groupId && source.task_id === taskId && source.id === value.submission_id && source.task_revision === value.task_revision
      && sameRefs(value.artifact_versions, source.artifact_versions), signal);
}
const workRecordIn = (groupId: string, taskId: string) => {
  const scoped = evidenceIn(groupId, taskId);
  return (value: unknown): value is WorkRecord & Record<string, unknown> => scoped(value) && typeof value.topic_id === "string";
};
const optionalConstraintRevision = (value: unknown) => value === undefined
  || (typeof value === "number" && Number.isSafeInteger(value) && value >= 0);
const validWorkInputReturn = (value: unknown): value is TaskWorkInputReturn => isRecord(value)
  && ["member_id", "turn_id", "attempt_id", "reason", "question", "notice_message_id"].every(key => nonblank(value[key]))
  && validEvidenceTime(value.returned_at);

async function listTaskWorkPage<T>(groupId: string, taskId: string, surface: "work-inputs" | "work-reports", before: string,
  signal: AbortSignal | undefined, valid: (value: unknown) => value is T) {
  if ([groupId, taskId].some(id => !nonblank(id) || id === "." || id === ".." || /[\ud800-\udfff]/u.test(id))) {
    throw new ApiError("群组或任务标识无效。");
  }
  const query = new URLSearchParams({ limit: "50" });
  if (before) query.set("before", before);
  return request(`${taskPath(groupId, taskId)}/${surface}?${query}`, { signal }, (value): value is TaskPage<T> => isRecord(value)
    && Array.isArray(value.items) && value.items.every(valid) && typeof value.next_cursor === "string");
}
export function listTaskWorkInputs(groupId: string, taskId: string, before = "", signal?: AbortSignal) {
  const scoped = workRecordIn(groupId, taskId);
  return listTaskWorkPage(groupId, taskId, "work-inputs", before, signal, (value): value is TaskWorkInput => scoped(value)
    && ["content", "source_id", "reply_message_id", "notice_message_id", "trigger_id", "mandate_id", "assignment_id", "worker_member_id",
      "before_constraint", "after_constraint"].every(key => value[key] === undefined || typeof value[key] === "string")
    && optionalConstraintRevision(value.before_constraint_revision) && optionalConstraintRevision(value.after_constraint_revision)
    && (value.consumption === undefined || value.consumption === null || (isRecord(value.consumption)
      && nonblank(value.consumption.turn_id) && validEvidenceTime(value.consumption.consumed_at)))
    && (value.return === undefined || value.return === null
      || ((value.consumption === undefined || value.consumption === null) && validWorkInputReturn(value.return))));
}
export function listTaskWorkReports(groupId: string, taskId: string, before = "", signal?: AbortSignal) {
  const scoped = workRecordIn(groupId, taskId);
  return listTaskWorkPage(groupId, taskId, "work-reports", before, signal, (value): value is TaskWorkReport => scoped(value)
    && ["kind", "summary", "actor_id", "owner_member_id"].every(key => nonblank(value[key])));
}
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
