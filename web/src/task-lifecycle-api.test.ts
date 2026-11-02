// @vitest-environment node
import { createServer } from "node:http";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, isDefinitiveRejection } from "./api";
import * as taskApi from "./task-api";
import type { ArtifactVersionRef, TaskAttempt, TaskSubmission, TaskReview, TaskReviewInput } from "./task-api";
import { deferred, group, json } from "./test/groups";
import { task, taskInput } from "./test/tasks";

const nativeFetch = globalThis.fetch;
const transport = vi.fn<typeof fetch>();
beforeEach(() => { transport.mockReset(); vi.stubGlobal("fetch", transport); });
afterEach(() => vi.unstubAllGlobals());

type CapturedRequest = { url: string; method: string; body: string; key: string | undefined };
async function withRedirect(status: number, receipt: unknown, run: (requests: CapturedRequest[]) => Promise<void>) {
  const requests: CapturedRequest[] = [];
  const server = createServer(async (request, response) => {
    const chunks: Buffer[] = [];
    for await (const chunk of request) chunks.push(Buffer.from(chunk));
    const url = request.url ?? "";
    requests.push({ url, method: request.method ?? "", body: Buffer.concat(chunks).toString(),
      key: request.headers["idempotency-key"] as string | undefined });
    if (url === "/redirect-target") {
      response.writeHead(201, { "Content-Type": "application/json" });
      response.end(JSON.stringify(receipt));
    } else {
      response.writeHead(status, { Location: "/redirect-target" });
      response.end();
    }
  });
  try {
    await new Promise<void>((resolve, reject) => {
      server.once("error", reject);
      server.listen(0, "127.0.0.1", resolve);
    });
    const address = server.address();
    if (address === null || typeof address === "string") throw new Error("Missing fixture port");
    vi.stubGlobal("fetch", (path: RequestInfo | URL, init?: RequestInit) =>
      nativeFetch(new URL(String(path), `http://127.0.0.1:${address.port}`), init));
    await run(requests);
  } finally {
    await new Promise<void>((resolve, reject) => {
      server.close(error => error ? reject(error) : resolve());
      server.closeAllConnections();
    });
  }
}

it.each([303, 307])("keeps createTask HTTP %s redirects unknown without visiting or forwarding to the target", async status => {
  await withRedirect(status, task, async requests => {
    const outcomes = [];
    for (let index = 0; index < 2; index++) {
      const result: unknown = await taskApi.createTask(group.id, taskInput, "original-key").catch((error: unknown) => error);
      outcomes.push(result instanceof ApiError
        ? { status: result.status, code: result.code, definitive: isDefinitiveRejection(result) }
        : { confirmed: result });
    }
    expect({ outcomes, forwarded: requests.filter(request => request.url === "/redirect-target") }).toEqual({
      outcomes: Array.from({ length: 2 }, () => ({ status: 0, code: "disconnected", definitive: false })), forwarded: [],
    });
    const original = requests.filter(request => request.url !== "/redirect-target");
    expect(original).toHaveLength(2);
    expect(original[1]).toEqual(original[0]);
    expect(original[0]).toEqual({ url: `/api/v1/groups/${group.id}/tasks`, method: "POST",
      body: JSON.stringify(taskInput), key: "original-key" });
  });
});

const attempt: TaskAttempt = { id: "attempt-1", group_id: group.id, task_id: task.id, task_revision: 1,
  turn_id: "", created_at: task.created_at };
const refs: ArtifactVersionRef[] = [
  { artifact_id: "报告/一", version_id: "opaque-v2" },
  { artifact_id: "报告/一", version_id: "opaque-v1" },
  { artifact_id: "source", version_id: "snapshot" },
];
const submission: TaskSubmission = { id: "submission-1", group_id: group.id, task_id: task.id, task_revision: 1,
  attempt_id: attempt.id, summary: "  已核对来源。\n保留原文与空白。  ", artifact_versions: refs, submitted_by: "user", created_at: task.created_at };
const review: TaskReview = { id: "review-1", group_id: group.id, task_id: task.id, task_revision: 1,
  submission_id: submission.id, outcome: "accepted", reason: "  核对完毕。  ", reviewed_by: "user", artifact_versions: refs, created_at: task.created_at };
const attemptInput = { expected_revision: 1, expected_active_attempt_id: "" };
const submissionInput = { expected_revision: 1, attempt_id: attempt.id, summary: submission.summary, artifact_versions: refs };
const reviewInput: TaskReviewInput = { expected_revision: 1, submission_id: submission.id, outcome: "accepted", reason: review.reason };
const kinds = ["attempt", "submission", "review"] as const;
type Kind = typeof kinds[number];
const receiptFor = (kind: Kind) => kind === "attempt" ? attempt : kind === "submission" ? submission : review;
const write = (kind: Kind, key = "original-key", signal?: AbortSignal) => kind === "attempt"
  ? taskApi.registerTaskAttempt(group.id, task.id, attemptInput, key, signal)
  : kind === "submission" ? taskApi.submitTask(group.id, task.id, submissionInput, key, signal)
    : taskApi.reviewTask(group.id, task.id, reviewInput, submission, key, signal);
async function expectUnknown(run: Promise<unknown>, status = 201, code = "invalid_response") {
  const result: unknown = await run.catch((error: unknown) => error);
  expect(result).toBeInstanceOf(ApiError);
  expect(result).toMatchObject({ status, code });
  expect(isDefinitiveRejection(result)).toBe(false);
}

it("encodes lifecycle paths and sends only allowlisted bodies without mutating callers", async () => {
  const groupId = "群/组?+", taskId = "任务/一#", recordId = "记录/一?";
  const signal = new AbortController().signal;
  const injected = { actor_id: "forged", actor: "forged", group_id: "foreign", task_id: "forged", turn_id: "forged",
    grants: ["forged"], submitted_by: "forged", reviewed_by: "forged" };
  const inputRefs = Object.freeze(refs.map(ref => Object.freeze({ ...ref, head: "forged" })));
  const inputs = {
    attempt: Object.freeze({ ...attemptInput, ...injected }),
    submission: Object.freeze({ ...submissionInput, artifact_versions: inputRefs, ...injected }),
    review: Object.freeze({ ...reviewInput, artifact_versions: [{ artifact_id: "forged", version_id: "head" }], ...injected }),
  };
  const before = structuredClone(inputs);
  const actual = { ...submission, group_id: groupId, task_id: taskId };
  const results = [{ ...attempt, group_id: groupId, task_id: taskId, id: recordId },
    { ...actual, id: recordId }, { ...review, group_id: groupId, task_id: taskId, id: recordId }];
  for (const result of results) transport.mockResolvedValueOnce(json(result, 201));
  expect(await taskApi.registerTaskAttempt(groupId, taskId, inputs.attempt, "attempt-key", signal)).toEqual(results[0]);
  expect(await taskApi.submitTask(groupId, taskId, inputs.submission, "submission-key", signal)).toEqual(results[1]);
  expect(await taskApi.reviewTask(groupId, taskId, inputs.review, actual, "review-key", signal)).toEqual(results[2]);
  const base = `/api/v1/groups/${encodeURIComponent(groupId)}/tasks/${encodeURIComponent(taskId)}`;
  for (const [index, collection] of ["attempts", "submissions", "reviews"].entries()) {
    const [url, init] = transport.mock.calls[index];
    expect(url).toBe(`${base}/${collection}`);
    expect(init).toMatchObject({ method: "POST", signal, redirect: "error" });
    expect(new Headers(init?.headers).get("Content-Type")).toBe("application/json");
    expect(new Headers(init?.headers).get("Idempotency-Key")).toBe(`${kinds[index]}-key`);
  }
  expect(JSON.parse(String(transport.mock.calls[0][1]?.body))).toEqual(attemptInput);
  expect(JSON.parse(String(transport.mock.calls[1][1]?.body))).toEqual(submissionInput);
  expect(JSON.parse(String(transport.mock.calls[2][1]?.body))).toEqual(reviewInput);
  expect(inputs).toEqual(before);
});

it("supports text-only submissions and reviews with an omitted reason", async () => {
  const text = { ...submission, artifact_versions: [] };
  const input = { expected_revision: 1, attempt_id: attempt.id, summary: submission.summary };
  transport.mockResolvedValueOnce(json(text, 201)).mockResolvedValueOnce(json({ ...review, reason: "", artifact_versions: [] }, 201));
  expect(await taskApi.submitTask(group.id, task.id, input, "text-key")).toEqual(text);
  expect(JSON.parse(String(transport.mock.calls[0][1]?.body))).toEqual(input);
  const decision = { expected_revision: 1, submission_id: text.id, outcome: "accepted" as const };
  await expect(taskApi.reviewTask(group.id, task.id, decision, text, "review-key")).resolves.toMatchObject({ reason: "" });
  expect(JSON.parse(String(transport.mock.calls[1][1]?.body))).toEqual(decision);
});

it.each(["attempt", "submission"] as const)("reads exact encoded %s records, including member evidence, with unchanged GET options", async kind => {
  const groupId = "群/组?+", taskId = "任务/一#", recordId = "记录/一?";
  const original = kind === "attempt" ? { ...attempt, turn_id: "member-turn" } : { ...submission, submitted_by: "member-1" };
  const result = { ...original, id: recordId, group_id: groupId, task_id: taskId };
  const signal = new AbortController().signal;
  transport.mockResolvedValueOnce(json(result));
  const run = kind === "attempt" ? taskApi.getTaskAttempt(groupId, taskId, recordId, signal)
    : taskApi.getTaskSubmission(groupId, taskId, recordId, signal);
  expect(await run).toEqual(result);
  expect(transport).toHaveBeenCalledExactlyOnceWith(
    `/api/v1/groups/${encodeURIComponent(groupId)}/tasks/${encodeURIComponent(taskId)}/${kind === "attempt" ? "attempts" : "submissions"}/${encodeURIComponent(recordId)}`,
    { signal },
  );
});

it.each(kinds)("keeps malformed or foreign %s receipts unknown", async kind => {
  const original = receiptFor(kind);
  const invalid: unknown[] = [null, [], "record", {}, { ...original, id: " \t " }, { ...original, group_id: "foreign" },
    { ...original, task_id: "other-task" }, { ...original, created_at: null }, { ...original, created_at: "" },
    ...[undefined, 0, -1, 1.5, Number.MAX_SAFE_INTEGER + 1, "1", 2].map(task_revision => ({ ...original, task_revision }))];
  for (const value of invalid) {
    transport.mockResolvedValueOnce(json(value, 201));
    await expectUnknown(write(kind));
  }
});

it.each([null, 1, " \t ", "member-turn"])("does not confirm a human Attempt with turn_id=%j", async turn_id => {
  transport.mockResolvedValueOnce(json({ ...attempt, turn_id }, 201));
  await expectUnknown(write("attempt"));
});

it.each([
  ["wrong attempt", { attempt_id: "other-attempt" }], ["blank attempt", { attempt_id: " " }],
  ["changed raw summary", { summary: submission.summary.trim() }], ["nonstring summary", { summary: [] }],
  ["wrong submitter", { submitted_by: "member-1" }], ["missing refs", { artifact_versions: undefined }],
  ["null refs", { artifact_versions: null }], ["missing pair", { artifact_versions: refs.slice(1) }],
  ["wrong version", { artifact_versions: [{ ...refs[0], version_id: "current-head" }, ...refs.slice(1)] }],
  ["extra pair", { artifact_versions: [...refs, { artifact_id: "other", version_id: "v1" }] }],
  ["malformed pair", { artifact_versions: [{ artifact_id: refs[0].artifact_id, version_id: 1 }] }],
] as const)("keeps a Submission with %s unknown", async (_name, change) => {
  transport.mockResolvedValueOnce(json({ ...submission, ...change }, 201));
  await expectUnknown(write("submission"));
});

it.each([
  ["wrong submission", { submission_id: "other-submission" }], ["blank submission", { submission_id: " " }],
  ["wrong outcome", { outcome: "rejected" }], ["unknown outcome", { outcome: "approved" }],
  ["wrong raw reason", { reason: review.reason.trim() }], ["nonstring reason", { reason: null }],
  ["wrong reviewer", { reviewed_by: "member-1" }], ["missing refs", { artifact_versions: undefined }],
  ["null refs", { artifact_versions: null }], ["malformed refs", { artifact_versions: [{ artifact_id: "report", version_id: " " }] }],
] as const)("keeps a Review with %s unknown", async (_name, change) => {
  transport.mockResolvedValueOnce(json({ ...review, ...change }, 201));
  await expectUnknown(write("review"));
});

it.each(["attempt", "submission"] as const)("rejects wrong %s GET identities and non-200 reads", async kind => {
  const original = kind === "attempt" ? attempt : submission;
  const run = () => kind === "attempt" ? taskApi.getTaskAttempt(group.id, task.id, original.id)
    : taskApi.getTaskSubmission(group.id, task.id, original.id);
  for (const change of [{ id: "other-record" }, { group_id: "foreign" }, { task_id: "other-task" }, { task_revision: 0 }]) {
    transport.mockResolvedValueOnce(json({ ...original, ...change }));
    await expectUnknown(run(), 200);
  }
  transport.mockResolvedValueOnce(json(original, 201));
  await expectUnknown(run());
});

it.each(kinds)("requires contract HTTP 201 JSON for a %s receipt", async kind => {
  for (const status of [200, 202, 204, 206]) {
    transport.mockResolvedValueOnce(status === 204 ? new Response(null, { status }) : json(receiptFor(kind), status));
    await expectUnknown(write(kind), status);
  }
  transport.mockResolvedValueOnce(new Response("not JSON", { status: 201 }));
  await expectUnknown(write(kind));
});

it.each(kinds)("preserves sanitized server rejection versus unknown outcomes for %s", async kind => {
  for (const status of [400, 404, 409, 413, 415, 408, 401, 429, 500, 503]) {
    transport.mockResolvedValueOnce(json({ error: { code: "server_code", message: "SECRET English diagnostic" } }, status));
    const result: unknown = await write(kind).catch((error: unknown) => error);
    expect(result).toBeInstanceOf(ApiError);
    expect(result).toMatchObject({ status, code: "server_code" });
    expect((result as ApiError).message).not.toMatch(/SECRET|English|diagnostic/);
    expect((result as ApiError).message).toMatch(/输入|未找到|冲突|过大|JSON|服务/);
    expect(isDefinitiveRejection(result)).toBe([400, 404, 409, 413, 415].includes(status));
    if (status === 409) expect((result as ApiError).message).toMatch(/任务状态、版本、依赖或请求内容冲突/);
  }
  transport.mockRejectedValueOnce(new TypeError("SECRET network diagnostic"));
  const result: unknown = await write(kind).catch((error: unknown) => error);
  expect(result).toMatchObject({ status: 0, code: "disconnected", message: "无法连接本地服务，请确认服务已启动。" });
  expect(isDefinitiveRejection(result)).toBe(false);
});

it.each(kinds)("accepts the original historical %s receipt after a later Task revision without rebasing replay", async kind => {
  const pending = deferred<Response>();
  transport.mockReturnValueOnce(pending.promise);
  const original = write(kind);
  const latest = { ...task, revision: 7 };
  transport.mockResolvedValueOnce(json(latest));
  expect(await taskApi.getTask(group.id, task.id)).toEqual(latest);
  pending.resolve(json(receiptFor(kind), 201));
  expect(await original).toEqual(receiptFor(kind));
  transport.mockResolvedValueOnce(json(receiptFor(kind), 201));
  expect(await write(kind)).toEqual(receiptFor(kind));
  const posts = transport.mock.calls.filter(([, init]) => init?.method === "POST")
    .map(([url, init]) => ({ url, body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key") }));
  expect(posts).toHaveLength(2);
  expect(posts[1]).toEqual(posts[0]);
  expect(JSON.parse(String(posts[0].body)).expected_revision).toBe(1);
  expect(latest.revision).toBe(7);
});

it("snapshots a pending Submission's revision, raw summary and each artifact/version pair", async () => {
  const input = structuredClone(submissionInput);
  const originalInput = structuredClone(input);
  const pending = deferred<Response>();
  transport.mockReturnValueOnce(pending.promise);
  const result = taskApi.submitTask(group.id, task.id, input, "original-key");
  input.expected_revision = 9; input.attempt_id = "other-attempt"; input.summary = "later draft";
  input.artifact_versions[0].version_id = "current-head";
  input.artifact_versions.splice(1);
  pending.resolve(json({ ...submission, artifact_versions: [...refs].reverse() }, 201));
  await expect(result).resolves.toMatchObject({ task_revision: 1, summary: submission.summary });
  transport.mockResolvedValueOnce(json(submission, 201));
  await taskApi.submitTask(group.id, task.id, originalInput, "original-key");
  expect(transport.mock.calls[1]).toEqual(transport.mock.calls[0]);
  expect(JSON.parse(String(transport.mock.calls[0][1]?.body))).toEqual(originalInput);
  expect(input.artifact_versions).toEqual([{ ...refs[0], version_id: "current-head" }]);
});

it("keeps a pending Attempt bound to the original revision and pointer", async () => {
  const input = { ...attemptInput };
  const pending = deferred<Response>();
  transport.mockReturnValueOnce(pending.promise);
  const result = taskApi.registerTaskAttempt(group.id, task.id, input, "original-key");
  input.expected_revision = 9; input.expected_active_attempt_id = "later-attempt";
  pending.resolve(json(attempt, 201));
  await expect(result).resolves.toEqual(attempt);
  expect(JSON.parse(String(transport.mock.calls[0][1]?.body))).toEqual(attemptInput);
});

it.each(kinds.flatMap(kind => [303, 307].map(status => ({ kind, status }))))(
  "keeps $kind HTTP $status redirects unknown and replays the identical original request", async ({ kind, status }) => {
    await withRedirect(status, receiptFor(kind), async requests => {
      await expectUnknown(write(kind), 0, "disconnected");
      await expectUnknown(write(kind), 0, "disconnected");
      expect(requests).toHaveLength(2);
      expect(requests[1]).toEqual(requests[0]);
      expect(requests[0].url).toBe(`/api/v1/groups/${group.id}/tasks/${task.id}/${kind === "attempt" ? "attempts" : kind === "submission" ? "submissions" : "reviews"}`);
      expect(requests[0].method).toBe("POST");
      expect(requests[0].key).toBe("original-key");
      expect(requests.some(request => request.url === "/redirect-target")).toBe(false);
    });
  },
);

it.each([
  ["empty", []], ["missing version of the same artifact", [refs[0], refs[2]]],
  ["extra pair", [...refs, { artifact_id: "extra", version_id: "v1" }]],
  ["wrong artifact", [{ ...refs[0], artifact_id: "other-artifact" }, ...refs.slice(1)]],
  ["later head", [{ ...refs[0], version_id: "current-head" }, ...refs.slice(1)]],
] as const)("keeps a Review with %s frozen refs unknown even when every other receipt field matches", async (_name, artifact_versions) => {
  transport.mockResolvedValueOnce(json({ ...submission, submitted_by: "member-1" }));
  const actual = await taskApi.getTaskSubmission(group.id, task.id, submission.id);
  transport.mockResolvedValueOnce(json({ ...review, artifact_versions }, 201));
  await expectUnknown(taskApi.reviewTask(group.id, task.id, reviewInput, actual, "original-key"));
});

it.each(["accepted", "rejected"] as const)("confirms %s only against the actual loaded Submission's exact frozen pair set", async outcome => {
  transport.mockResolvedValueOnce(json({ ...submission, submitted_by: "member-1" }));
  const actual = await taskApi.getTaskSubmission(group.id, task.id, submission.id);
  const before = structuredClone(actual);
  actual.artifact_versions.forEach(ref => Object.freeze(ref));
  Object.freeze(actual.artifact_versions); Object.freeze(actual);
  const result = { ...review, outcome, artifact_versions: [...refs].reverse() };
  transport.mockResolvedValueOnce(json(result, 201));
  expect(await taskApi.reviewTask(group.id, task.id, { ...reviewInput, outcome }, actual, "original-key")).toEqual(result);
  expect(actual).toEqual(before);
  expect(result.artifact_versions.filter(ref => ref.artifact_id === refs[0].artifact_id)).toHaveLength(2);
  expect(JSON.parse(String(transport.mock.calls[1][1]?.body))).toEqual({ ...reviewInput, outcome });
});

it.each(["original", "mutated"] as const)("validates a pending Review against original evidence, not caller mutations (%s receipt)", async receipt => {
  transport.mockResolvedValueOnce(json(submission));
  const actual = await taskApi.getTaskSubmission(group.id, task.id, submission.id);
  const original = structuredClone(actual);
  const input = { ...reviewInput };
  const pending = deferred<Response>();
  transport.mockReturnValueOnce(pending.promise);
  const result = taskApi.reviewTask(group.id, task.id, input, actual, "original-key");
  const artifactHead = actual.artifact_versions[0];
  artifactHead.version_id = "later-head";
  actual.artifact_versions.splice(1, 1);
  actual.artifact_versions.push({ artifact_id: "later-artifact", version_id: "later-version" });
  input.expected_revision = 7; input.submission_id = "later-submission"; input.outcome = "rejected"; input.reason = "later draft";
  if (receipt === "original") {
    actual.group_id = "later-group"; actual.task_id = "later-task"; actual.id = "later-submission"; actual.task_revision = 7;
  }
  const changed = structuredClone(actual);
  pending.resolve(json({ ...review, artifact_versions: receipt === "original" ? original.artifact_versions : changed.artifact_versions }, 201));
  if (receipt === "original") await expect(result).resolves.toEqual(review);
  else await expectUnknown(result);
  expect(actual).toEqual(changed);
  expect(JSON.parse(String(transport.mock.calls[1][1]?.body))).toEqual(reviewInput);
  transport.mockResolvedValueOnce(json(review, 201));
  await expect(taskApi.reviewTask(group.id, task.id, reviewInput, original, "original-key")).resolves.toEqual(review);
  expect(transport.mock.calls[2]).toEqual(transport.mock.calls[1]);
});

it.each([{ group_id: "foreign" }, { task_id: "other-task" }, { id: "other-submission" }, { task_revision: 2 }])(
  "never validates a Review using mismatched Submission context %#", async change => {
    transport.mockResolvedValueOnce(json(review, 201));
    await expectUnknown(taskApi.reviewTask(group.id, task.id, reviewInput, { ...submission, ...change }, "original-key"));
  },
);

it("uses canonical pair equivalence without collapsing different versions of one artifact", async () => {
  const input = { ...submissionInput, artifact_versions: [...refs, { ...refs[0] }] };
  transport.mockResolvedValueOnce(json({ ...submission, artifact_versions: [...refs].reverse() }, 201));
  await expect(taskApi.submitTask(group.id, task.id, input, "original-key")).resolves.toMatchObject({ artifact_versions: [...refs].reverse() });
  expect(JSON.parse(String(transport.mock.calls[0][1]?.body)).artifact_versions).toEqual(input.artifact_versions);
});
