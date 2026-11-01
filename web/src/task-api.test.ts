import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, isDefinitiveRejection } from "./api";
import { createTask, getTask, listTasks } from "./task-api";
import { group, json, other, page } from "./test/groups";
import { acceptedTask, submittedTask, task, taskInput } from "./test/tasks";

const transport = vi.fn<typeof fetch>();
beforeEach(() => { transport.mockReset(); vi.stubGlobal("fetch", transport); });
afterEach(() => vi.unstubAllGlobals());

it("bounds and encodes list queries, and passes the read cancellation signal", async () => {
  const groupId = "群/组?+";
  const signal = new AbortController().signal;
  transport.mockResolvedValueOnce(page([{ ...task, group_id: groupId }], "next"));
  expect(await listTasks(groupId, "a+/中文?=&", signal)).toEqual({ items: [{ ...task, group_id: groupId }], next_cursor: "next" });
  expect(transport).toHaveBeenCalledExactlyOnceWith(
    "/api/v1/groups/%E7%BE%A4%2F%E7%BB%84%3F%2B/tasks?limit=50&before=a%2B%2F%E4%B8%AD%E6%96%87%3F%3D%26", { signal },
  );
  transport.mockResolvedValueOnce(page());
  await listTasks(group.id);
  expect(transport.mock.calls[1][0]).toBe(`/api/v1/groups/${group.id}/tasks?limit=50`);
});

it("reads authoritative detail with both path IDs encoded", async () => {
  const groupId = "群/组?+", taskId = "任务/一?+#";
  const result = { ...task, id: taskId, group_id: groupId };
  const signal = new AbortController().signal;
  transport.mockResolvedValueOnce(json(result));
  expect(await getTask(groupId, taskId, signal)).toEqual(result);
  expect(transport).toHaveBeenCalledExactlyOnceWith(
    "/api/v1/groups/%E7%BE%A4%2F%E7%BB%84%3F%2B/tasks/%E4%BB%BB%E5%8A%A1%2F%E4%B8%80%3F%2B%23", { signal },
  );
});

it("posts only the frozen input allowlist with an idempotency key and HTTP 201", async () => {
  const groupId = "群/组?+";
  const input = Object.freeze({ ...taskInput, assignee_id: "member/1", dependency_ids: Object.freeze(["dependency-1"]),
    id: "forged", group_id: other.id, status: "accepted", revision: 99, active_attempt_id: "forged", current_submission_id: "forged", created_at: "forged" });
  const signal = new AbortController().signal;
  const result = { ...task, group_id: groupId };
  transport.mockResolvedValueOnce(json(result, 201));
  expect(await createTask(groupId, input, "stable-key", signal)).toEqual(result);
  const [url, init] = transport.mock.calls[0];
  expect(url).toBe("/api/v1/groups/%E7%BE%A4%2F%E7%BB%84%3F%2B/tasks");
  expect(init?.method).toBe("POST"); expect(init?.signal).toBe(signal);
  expect(new Headers(init?.headers).get("Content-Type")).toBe("application/json");
  expect(new Headers(init?.headers).get("Idempotency-Key")).toBe("stable-key");
  expect(JSON.parse(String(init?.body))).toEqual({ ...taskInput, assignee_id: "member/1", dependency_ids: ["dependency-1"] });
  transport.mockResolvedValueOnce(json(task, 201));
  await createTask(group.id, taskInput, "another-key");
  expect(JSON.parse(String(transport.mock.calls[1][1]?.body))).toEqual(taskInput);
});

it("omits an optional description from the allowlisted request", async () => {
  const input = { title: task.title, acceptance: task.acceptance };
  transport.mockResolvedValueOnce(json(task, 201));
  await createTask(group.id, input, "key");
  expect(JSON.parse(String(transport.mock.calls[0][1]?.body))).toEqual(input);
});

const stringFields = ["id", "group_id", "title", "description", "acceptance", "assignee_id", "active_attempt_id", "current_submission_id", "created_at"];
const invalidTasks: [string, unknown][] = [
  ...stringFields.flatMap(field => [undefined, null, 7, [], {}].map(value => [`${field}: ${JSON.stringify(value)}`, { ...task, [field]: value }] as [string, unknown])),
  ["empty ID", { ...task, id: "" }], ["blank ID", { ...task, id: "  " }],
  ["foreign group", { ...task, group_id: other.id }],
  ...[undefined, null, "future", "open ", 1, {}].map(status => [`status: ${JSON.stringify(status)}`, { ...task, status }] as [string, unknown]),
  ...[undefined, null, 0, -1, 1.5, Number.MAX_SAFE_INTEGER + 1, "1", true].map(revision => [`revision: ${JSON.stringify(revision)}`, { ...task, revision }] as [string, unknown]),
  ...[undefined, null, {}, "dependency", [1], ["valid", null], [["nested"]]].map(dependency_ids => [`dependencies: ${JSON.stringify(dependency_ids)}`, { ...task, dependency_ids }] as [string, unknown]),
  ["null task", null], ["array task", []], ["primitive task", "task"],
];
it.each(invalidTasks)("rejects %s in list, detail and creation without confirming a mutation", async (_name, invalid) => {
  const requests = [
    { response: page([invalid]), run: () => listTasks(group.id), status: 200 },
    { response: json(invalid), run: () => getTask(group.id, task.id), status: 200 },
    { response: json(invalid, 201), run: () => createTask(group.id, taskInput, "key"), status: 201 },
  ];
  for (const request of requests) {
    transport.mockResolvedValueOnce(request.response);
    const error: unknown = await request.run().catch((failure: unknown) => failure);
    expect(error).toBeInstanceOf(ApiError);
    expect(error).toMatchObject({ status: request.status, code: "invalid_response" });
    expect(isDefinitiveRejection(error)).toBe(false);
  }
});

it.each([task, { ...task, active_attempt_id: "attempt-1" }, submittedTask, acceptedTask])("accepts the real $status status and positive safe revision", async state => {
  const result = { ...state, revision: Number.MAX_SAFE_INTEGER, dependency_ids: ["dependency-1"], assignee_id: "member-1" };
  transport.mockResolvedValueOnce(json(result));
  expect(await getTask(group.id, task.id)).toEqual(result);
});

it.each([
  { ...task, current_submission_id: "submission-1" },
  { ...task, active_attempt_id: " \t " },
  { ...submittedTask, active_attempt_id: "" },
  { ...submittedTask, active_attempt_id: " \t " },
  { ...submittedTask, current_submission_id: "" },
  { ...submittedTask, current_submission_id: " \t " },
  { ...acceptedTask, active_attempt_id: "attempt-1" },
  { ...acceptedTask, current_submission_id: "" },
  { ...acceptedTask, current_submission_id: " \t " },
])("keeps invalid $status pointers uncertain for a 201 receipt (case %#)", async invalid => {
  transport.mockResolvedValueOnce(json(invalid, 201));
  const error: unknown = await createTask(group.id, taskInput, "key").catch((failure: unknown) => failure);
  expect(error).toBeInstanceOf(ApiError); expect(error).toMatchObject({ status: 201, code: "invalid_response" });
  expect(isDefinitiveRejection(error)).toBe(false);
});
it.each([submittedTask, acceptedTask])("accepts a valid $status same-key replay receipt after an unknown create", async advanced => {
  transport.mockRejectedValueOnce(new TypeError("lost receipt")).mockResolvedValueOnce(json(advanced, 201));
  await expect(createTask(group.id, taskInput, "same-key")).rejects.toMatchObject({ code: "disconnected" });
  await expect(createTask(group.id, taskInput, "same-key")).resolves.toEqual(advanced);
  const attempts = transport.mock.calls.map(([url, init]) => ({ url, body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key") }));
  expect(attempts[1]).toEqual(attempts[0]); expect(attempts[1].key).toBe("same-key");
});

it.each([null, [], {}, { items: null, next_cursor: "" }, { items: {}, next_cursor: "" }, { items: [] },
  { items: [], next_cursor: null }, { items: [], next_cursor: 1 }, { items: [task, { ...task, group_id: other.id }], next_cursor: "" }])(
  "rejects malformed page %#", async value => {
    transport.mockResolvedValueOnce(json(value));
    await expect(listTasks(group.id)).rejects.toMatchObject({ code: "invalid_response" });
  },
);

it("rejects detail from a different task even in the expected group", async () => {
  transport.mockResolvedValueOnce(json({ ...task, id: "other-task" }));
  await expect(getTask(group.id, task.id)).rejects.toMatchObject({ code: "invalid_response" });
});
it.each([200, 202, 204, 206])("does not confirm a create receipt with HTTP %s", async status => {
  transport.mockResolvedValueOnce(status === 204 ? new Response(null, { status }) : json(task, status));
  const error: unknown = await createTask(group.id, taskInput, "key").catch((failure: unknown) => failure);
  expect(error).toMatchObject({ status, code: "invalid_response" }); expect(isDefinitiveRejection(error)).toBe(false);
});
it("requires HTTP 200 for reads", async () => {
  transport.mockResolvedValueOnce(json(task, 201));
  await expect(getTask(group.id, task.id)).rejects.toMatchObject({ status: 201, code: "invalid_response" });
});
it.each(["list", "detail", "create"])("treats unparseable %s success as invalid, not success", async operation => {
  transport.mockResolvedValueOnce(new Response("not JSON", { status: operation === "create" ? 201 : 200 }));
  await expect(operation === "list" ? listTasks(group.id) : operation === "detail" ? getTask(group.id, task.id) : createTask(group.id, taskInput, "key"))
    .rejects.toMatchObject({ code: "invalid_response" });
});

it.each([400, 404, 409, 413, 415, 401, 429, 500, 503])("keeps HTTP %s errors Chinese and preserves rejection certainty", async status => {
  transport.mockResolvedValueOnce(json({ error: { code: "server_code", message: "SECRET backend English diagnostic" } }, status));
  const error: unknown = await createTask(group.id, taskInput, "key").catch((failure: unknown) => failure);
  expect(error).toBeInstanceOf(ApiError); expect(error).toMatchObject({ status, code: "server_code" });
  expect((error as ApiError).message).not.toMatch(/SECRET|backend|English/);
  expect((error as ApiError).message).toMatch(/输入|未找到|冲突|过大|JSON|服务/);
  expect(isDefinitiveRejection(error)).toBe([400, 404, 409, 413, 415].includes(status));
});
it("handles a non-JSON HTTP failure with a safe fallback code", async () => {
  transport.mockResolvedValueOnce(new Response("backend detail", { status: 503 }));
  await expect(listTasks(group.id)).rejects.toMatchObject({ status: 503, code: "http_error" });
});
it("keeps disconnected creation uncertain without leaking native errors", async () => {
  transport.mockRejectedValueOnce(new TypeError("secret network diagnostic"));
  const error: unknown = await createTask(group.id, taskInput, "key").catch((failure: unknown) => failure);
  expect(error).toMatchObject({ status: 0, code: "disconnected" }); expect(isDefinitiveRejection(error)).toBe(false);
  expect((error as ApiError).message).toBe("无法连接本地服务，请确认服务已启动。");
});
it("preserves an intentional abort instead of reporting a disconnected service", async () => {
  const controller = new AbortController(); controller.abort();
  const aborted = new DOMException("aborted", "AbortError"); transport.mockRejectedValueOnce(aborted);
  await expect(getTask(group.id, task.id, controller.signal)).rejects.toBe(aborted);
});
