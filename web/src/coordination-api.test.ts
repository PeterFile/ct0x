import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError } from "./api";
import { isRequestWorkDefinitiveRejection, requestWork } from "./coordination-api";
import type { AdmissionReceipt, RequestWorkInput } from "./coordination-api";
import { deferred, json } from "./test/groups";

const groupId = "群/组?+";
const path = "/api/v1/groups/%E7%BE%A4%2F%E7%BB%84%3F%2B/coordination/requests";
const key = "caller-stable-key";
const stamp = "2026-11-01T07:00:00.123456789Z";
const transport = vi.fn<typeof fetch>();
beforeEach(() => { transport.mockReset(); vi.stubGlobal("fetch", transport); });
afterEach(() => vi.unstubAllGlobals());

function input() {
  return {
    topic_id: " topic\r\n\x00 ", message_id: "source-message", mode: "create_new" as const, reason: " 授权理由。 \n",
    new_task: { title: " 任务目标。 ", description: "\n原始描述\t", acceptance: " 验收标准。 \n",
      assignee_id: "member-1", dependency_ids: ["dep-b", "dep-a", "dep-b"] },
  };
}
function receipt(request: RequestWorkInput = input(), group = groupId) {
  const task = request.new_task;
  return {
    decision: {
      id: "decision-1", trigger_id: "trigger-1", group_id: group, topic_id: request.topic_id, kind: "create_new",
      actor_id: "user", policy_revision: "local-user-explicit/v1", reason: request.reason, task_id: "task-1",
      task_revision_at_admission: 1, mandate_id: "mandate-1", created_at: stamp,
    },
    mandate: {
      id: "mandate-1", decision_id: "decision-1", origin_trigger_id: "trigger-1", group_id: group,
      topic_id: request.topic_id, task_id: "task-1", task_revision_at_admission: 1,
      authority_kind: "local_user_explicit", authorized_by: "user", strength: "explicit", policy_revision: "local-user-explicit/v1",
      scope: { objective: task.title, description: task.description ?? "", acceptance: task.acceptance,
        assignee_id: task.assignee_id ?? "", dependency_ids: [...new Set(task.dependency_ids ?? [])]
          .sort((a, b) => Buffer.compare(Buffer.from(a), Buffer.from(b))) },
      created_at: stamp,
    },
    task: { id: "task-1", group_id: group, revision_at_admission: 1 },
  } satisfies AdmissionReceipt;
}
function changeField(value: object, field: string, replacement: unknown) {
  const parts = field.split(".");
  let target = value as Record<string, unknown>;
  for (const part of parts.slice(0, -1)) target = target[part] as Record<string, unknown>;
  if (replacement === undefined) delete target[parts.at(-1)!];
  else target[parts.at(-1)!] = replacement;
}
async function invalidSuccess(value: unknown, status = 201) {
  transport.mockResolvedValueOnce(status === 204 ? new Response(null, { status }) : json(value, status));
  const error: unknown = await requestWork(groupId, input(), key).catch((failure: unknown) => failure);
  expect(error).toBeInstanceOf(ApiError);
  expect(error).toMatchObject({ status, code: "invalid_response" });
  expect(isRequestWorkDefinitiveRejection(error)).toBe(false);
  expect((error as ApiError).message).toMatch(/结果未知/);
  expect(transport).toHaveBeenCalledTimes(1);
}

it("posts one allowlisted nested command with the encoded group, exact source, raw text and caller key", async () => {
  const request = Object.freeze({ ...input(), actor_id: "forged", caller: "forged", sender_id: "forged",
    policy_revision: "forged", strength: "forged", sequence: 99, mandate: {}, turn: {}, grant: {}, liveTask: {},
    new_task: Object.freeze({ ...input().new_task, dependency_ids: Object.freeze(input().new_task.dependency_ids),
      id: "forged", group_id: "foreign", status: "accepted", revision: 99, active_attempt_id: "forged" }) });
  const expected = receipt(request);
  const signal = new AbortController().signal;
  transport.mockResolvedValueOnce(json(expected, 201));
  await expect(requestWork(groupId, request, key, signal)).resolves.toEqual(expected);
  const [url, init] = transport.mock.calls[0];
  expect(url).toBe(path);
  expect(init?.method).toBe("POST"); expect(init?.signal).toBe(signal);
  expect(new Headers(init?.headers).get("Content-Type")).toBe("application/json");
  expect(new Headers(init?.headers).get("Idempotency-Key")).toBe(key);
  expect(JSON.parse(String(init?.body))).toEqual(input());
  expect(transport).toHaveBeenCalledTimes(1); // No Task, Message, Trigger, execution or model request.
  expect(expected.decision.trigger_id).not.toBe(request.message_id);
  expect(expected).not.toHaveProperty("message_id");
});

it.each([undefined, null, "", " ", " padded", "padded ", "a b", "a\tb", "a\nb", "key\n", "key\r", "a,b", ["a", "b"],
  "é", "\u007f", "\x00", "x".repeat(129)])("rejects invalid or absent key %# before any network call", async invalid => {
  const error: unknown = await requestWork(groupId, input(), invalid as string).catch((failure: unknown) => failure);
  expect(error).toBeInstanceOf(ApiError); expect(error).toMatchObject({ code: "invalid_input" });
  expect(isRequestWorkDefinitiveRejection(error)).toBe(true);
  expect(transport).not.toHaveBeenCalled();
});
it.each(["!", "x".repeat(128), "key/with:?[brackets]\"~+"])("preserves valid visible ASCII key %# without replacement", async valid => {
  transport.mockResolvedValueOnce(json(receipt(), 201));
  await requestWork(groupId, input(), valid);
  expect(new Headers(transport.mock.calls[0][1]?.headers).get("Idempotency-Key")).toBe(valid);
  expect(transport).toHaveBeenCalledTimes(1);
});

const invalidInputs: [string, string, unknown][] = [
  ...[undefined, null, "link_existing", "attach", "followup", "dispatch", "execute"].map(mode => ["unsupported mode", "mode", mode] as [string, string, unknown]),
  ["missing new variant", "new_task", undefined], ["null new variant", "new_task", null],
  ["other variant", "existing_task", { task_id: "task-2", expected_revision: 1 }], ["null other variant", "existing_task", null],
  ...["reason", "new_task.title", "new_task.acceptance"].flatMap(field => ["", " \t\n", "\u0085", "\u2003\u3000"].map(blank => ["Go-blank required text", field, blank] as [string, string, unknown])),
  ["blank assignee", "new_task.assignee_id", " \u0085"], ["non-string description", "new_task.description", 7],
  ["invalid dependencies", "new_task.dependency_ids", [1]], ["blank dependency", "new_task.dependency_ids", ["\u0085"]],
  ["non-string topic", "topic_id", null], ["non-string reason", "reason", {}],
];
it.each(invalidInputs)("rejects %s (%s, case %#) rather than reinterpreting the command", async (_name, field, value) => {
  const request = input(); changeField(request, field, value);
  const error: unknown = await requestWork(groupId, request, key).catch((failure: unknown) => failure);
  expect(error).toMatchObject({ code: "invalid_input" }); expect(isRequestWorkDefinitiveRejection(error)).toBe(true);
  expect(transport).not.toHaveBeenCalled();
});

const invalidIDs = ["", " ", " padded", "padded ", "\u0085id", "id\u0085", "a\x00b", "a\rb", "a\nb", "\ud800", "\udfff", "界".repeat(1366)];
it.each(invalidIDs)("rejects invalid group/message identifier %# locally", async invalid => {
  for (const field of ["group", "message"]) {
    const request = input(); if (field === "message") request.message_id = invalid;
    const error: unknown = await requestWork(field === "group" ? invalid : groupId, request, key).catch((failure: unknown) => failure);
    expect(error).toMatchObject({ code: "invalid_input" }); expect(isRequestWorkDefinitiveRejection(error)).toBe(true);
  }
  expect(transport).not.toHaveBeenCalled();
});
it.each(["id\tinside", "id\x01inside", "\ufeffid\ufeff", "界".repeat(1365) + "x", "id𐀀"])("accepts source identifiers allowed by Go, including internal controls and BOM (case %#)", async id => {
  const request = input(); request.message_id = id;
  transport.mockResolvedValueOnce(json(receipt(request, id), 201));
  await expect(requestWork(id, request, key)).resolves.toEqual(receipt(request, id));
  expect(transport.mock.calls[0][0]).toBe(`/api/v1/groups/${encodeURIComponent(id)}/coordination/requests`);
});

it.each(["", " ", " \r\n\t\x00 ", "topic".repeat(1100), "主题𐀀"])("preserves opaque topic %# without manual-search restrictions", async topic => {
  const request = input(); request.topic_id = topic;
  transport.mockResolvedValueOnce(json(receipt(request), 201));
  await expect(requestWork(groupId, request, key)).resolves.toEqual(receipt(request));
  expect(JSON.parse(String(transport.mock.calls[0][1]?.body)).topic_id).toBe(topic);
});
it("uses Go nonblank semantics without trimming BOM, NEL or any raw text", async () => {
  const request = input();
  request.reason = "\ufeff"; request.new_task.title = "\ufeff"; request.new_task.acceptance = "\ufeff";
  request.new_task.description = "\u0085 \ufeff\n"; request.new_task.assignee_id = " \ufeff ";
  transport.mockResolvedValueOnce(json(receipt(request), 201));
  await expect(requestWork(groupId, request, key)).resolves.toEqual(receipt(request));
  expect(JSON.parse(String(transport.mock.calls[0][1]?.body))).toEqual(request);
});
it.each([undefined, null, []] as const)("accepts optional defaults and omitted/nil/empty dependencies (case %#)", async dependencies => {
  const request: RequestWorkInput = { topic_id: "", message_id: "message-1", mode: "create_new", reason: "理由",
    new_task: { title: "标题", acceptance: "验收", dependency_ids: dependencies } };
  const expected = receipt(request);
  transport.mockResolvedValueOnce(json(expected, 201));
  await expect(requestWork(groupId, request, key)).resolves.toEqual(expected);
  expect(expected.mandate.scope).toEqual({ objective: "标题", description: "", acceptance: "验收", assignee_id: "", dependency_ids: [] });
  expect(JSON.parse(String(transport.mock.calls[0][1]?.body))).toEqual(JSON.parse(JSON.stringify(request)));
});
it("compares dependency sets without UTF-16 ordering assumptions or caller mutation", async () => {
  const request = input(); request.new_task.dependency_ids = ["𐀀", "\ue000", "𐀀", "e\u0301", "é"];
  const before = structuredClone(request);
  const expected = receipt(request);
  expect(expected.mandate.scope.dependency_ids).toEqual(["e\u0301", "é", "\ue000", "𐀀"]);
  transport.mockResolvedValueOnce(json(expected, 201));
  await expect(requestWork(groupId, request, key)).resolves.toEqual(expected);
  expect(request).toEqual(before);
  expect(JSON.parse(String(transport.mock.calls[0][1]?.body))).toEqual(before);
});
it.each([false, true])("binds receipt association to the actual sent snapshot after caller edits (foreign response=%s)", async foreign => {
  const request = input(); const sent = structuredClone(request); const pending = deferred<Response>();
  const dependencies = request.new_task.dependency_ids;
  transport.mockReturnValueOnce(pending.promise);
  const result = requestWork(groupId, request, key);
  request.topic_id = "changed"; request.message_id = "changed-message"; request.reason = "changed reason";
  request.new_task.title = "changed title"; request.new_task.description = "changed description";
  request.new_task.acceptance = "changed acceptance"; request.new_task.assignee_id = "changed assignee";
  dependencies.splice(0, dependencies.length, "changed-dependency");
  const edited = structuredClone(request);
  request.new_task = { ...request.new_task, dependency_ids: ["replacement-dependency"] };
  pending.resolve(json(receipt(foreign ? edited : sent), 201));
  if (foreign) {
    const error: unknown = await result.catch((failure: unknown) => failure);
    expect(error).toMatchObject({ code: "invalid_response" }); expect(isRequestWorkDefinitiveRejection(error)).toBe(false);
  } else await expect(result).resolves.toEqual(receipt(sent));
  expect(JSON.parse(String(transport.mock.calls[0][1]?.body))).toEqual(sent);
  expect(dependencies).toEqual(["changed-dependency"]);
  expect(request.new_task.dependency_ids).toEqual(["replacement-dependency"]);
  expect(transport).toHaveBeenCalledTimes(1);
});

const stringFields = {
  decision: ["id", "trigger_id", "group_id", "topic_id", "kind", "actor_id", "policy_revision", "reason", "task_id", "mandate_id", "created_at"],
  mandate: ["id", "decision_id", "origin_trigger_id", "group_id", "topic_id", "task_id", "authority_kind", "authorized_by", "strength", "policy_revision", "created_at"],
  "mandate.scope": ["objective", "description", "acceptance", "assignee_id"],
  task: ["id", "group_id"],
};
const malformedFields: [string, unknown][] = Object.entries(stringFields).flatMap(([prefix, fields]) => fields
  .flatMap(field => [undefined, null, 7, [], {}].map(value => [`${prefix}.${field}`, value] as [string, unknown])));
it.each(malformedFields)("keeps malformed %s uncertain (case %#)", async (field, value) => {
  const invalid = receipt(); changeField(invalid, field, value); await invalidSuccess(invalid);
});
it.each(["decision.task_revision_at_admission", "mandate.task_revision_at_admission", "task.revision_at_admission"]
  .flatMap(field => [undefined, null, 0, -1, 2, 1.5, "1", true, Number.MAX_SAFE_INTEGER + 1].map(value => [field, value] as const)))(
  "requires create_new admission revision 1 at %s (case %#)", async (field, value) => {
    const invalid = receipt(); changeField(invalid, field, value); await invalidSuccess(invalid);
  },
);
const associations: [string, unknown][] = [
  ...["decision.group_id", "mandate.group_id", "task.group_id"].map(field => [field, "foreign-group"] as [string, unknown]),
  ...["decision.topic_id", "mandate.topic_id"].map(field => [field, input().topic_id.trim()] as [string, unknown]),
  ...["decision.id", "decision.trigger_id", "decision.task_id", "decision.mandate_id", "mandate.id", "mandate.decision_id", "mandate.origin_trigger_id", "mandate.task_id", "task.id"]
    .map(field => [field, "foreign-record"] as [string, unknown]),
  ["decision.kind", "link_existing"], ["decision.actor_id", "agent"], ["mandate.authorized_by", "agent"],
  ["decision.policy_revision", "future-policy"], ["mandate.policy_revision", "future-policy"],
  ["mandate.authority_kind", "inferred"], ["mandate.strength", "implicit"],
  ["decision.reason", input().reason.trim()], ["mandate.scope.objective", input().new_task.title.trim()],
  ["mandate.scope.description", input().new_task.description.trim()], ["mandate.scope.acceptance", input().new_task.acceptance.trim()],
  ["mandate.scope.assignee_id", "foreign-assignee"], ["mandate.created_at", "2026-11-01T07:00:00.123456788Z"],
];
it.each(associations)("rejects mismatched admission evidence at %s (case %#)", async (field, value) => {
  const invalid = receipt(); changeField(invalid, field, value); await invalidSuccess(invalid);
});
it.each([undefined, null, {}, "dep-a", [1], [null], ["dep-a"], ["dep-a", "foreign"], ["dep-a", "dep-a", "dep-b"], ["dep-a", "dep-b", "extra"]])(
  "rejects malformed or different dependency snapshot %#", async value => {
    const invalid = receipt(); changeField(invalid, "mandate.scope.dependency_ids", value); await invalidSuccess(invalid);
  },
);
it("does not Unicode-normalize dependency identities", async () => {
  const request = input(); request.new_task.dependency_ids = ["e\u0301"];
  const invalid = receipt(request); invalid.mandate.scope.dependency_ids = ["é"];
  transport.mockResolvedValueOnce(json(invalid, 201));
  await expect(requestWork(groupId, request, key)).rejects.toMatchObject({ code: "invalid_response" });
});
const identityLinks = [
  ["decision.id", "mandate.decision_id"], ["decision.mandate_id", "mandate.id"],
  ["decision.trigger_id", "mandate.origin_trigger_id"], ["decision.task_id", "mandate.task_id", "task.id"],
];
it.each(identityLinks.flatMap(fields => invalidIDs.map(id => [fields, id] as const)))(
  "rejects coherent but invalid server record identity %#", async (fields, id) => {
    const invalid = receipt(); for (const field of fields) changeField(invalid, field, id);
    await invalidSuccess(invalid);
  },
);
it("accepts opaque server identities with internal TAB/control characters and BOM", async () => {
  const expected = receipt();
  for (const [index, fields] of identityLinks.entries()) {
    for (const field of fields) changeField(expected, field, `\ufeffrecord-${index}\t\x01𐀀\ufeff`);
  }
  transport.mockResolvedValueOnce(json(expected, 201));
  await expect(requestWork(groupId, input(), key)).resolves.toEqual(expected);
});
it.each(["", "not a time", "2026-02-30T07:00:00Z", "2025-02-29T07:00:00Z", "2026-11-01T24:00:00Z",
  "2026-11-01T07:60:00Z", "2026-11-01T07:00:60Z", "2026-11-01T07:00:00+00:00", "2026-11-01T07:00:00.1234567890Z",
  "0001-01-01T00:00:00Z", "0000-11-01T07:00:00Z", stamp + "\n"])("rejects equal but invalid/non-writer timestamps %#", async createdAt => {
  const invalid = receipt(); invalid.decision.created_at = createdAt; invalid.mandate.created_at = createdAt;
  await invalidSuccess(invalid);
});
it.each(["2024-02-29T07:00:00Z", "2026-11-01T07:00:00.1Z", stamp])("accepts UTC time.Time JSON precision %#", async createdAt => {
  const expected = receipt(); expected.decision.created_at = createdAt; expected.mandate.created_at = createdAt;
  transport.mockResolvedValueOnce(json(expected, 201));
  await expect(requestWork(groupId, input(), key)).resolves.toEqual(expected);
});
it.each([null, [], {}, "receipt", { decision: receipt().decision }, { ...receipt(), task: null },
  { ...receipt(), mandate: [] }, { ...receipt(), mandate: { ...receipt().mandate, scope: null } }])("rejects malformed receipt nesting %#", async value => {
  await invalidSuccess(value);
});
it.each(["liveTask", "task.status", "task.revision", "task.active_attempt_id", "task.current_submission_id", "task.submission"])(
  "rejects live lifecycle fields instead of conflating them with TaskRef: %s", async field => {
    const invalid = receipt(); changeField(invalid, field, "unrelated live state"); await invalidSuccess(invalid);
  },
);

it.each([200, 202, 204, 206])("requires exact HTTP 201, not successful HTTP %s", async status => { await invalidSuccess(receipt(), status); });
it("treats malformed JSON in a 201 as unknown and never fabricates a receipt", async () => {
  transport.mockResolvedValueOnce(new Response("SECRET not JSON", { status: 201 }));
  const error: unknown = await requestWork(groupId, input(), key).catch((failure: unknown) => failure);
  expect(error).toMatchObject({ status: 201, code: "invalid_response" }); expect(isRequestWorkDefinitiveRejection(error)).toBe(false);
  expect((error as ApiError).message).not.toMatch(/SECRET/); expect(transport).toHaveBeenCalledTimes(1);
});
it("accepts historical replay without fetching or folding in the current Task", async () => {
  const historical = receipt();
  let liveTask = { id: historical.task.id, group_id: groupId, revision: 1, status: "open", title: input().new_task.title };
  transport.mockImplementation(async (url, init) => {
    if (url === path && init?.method === "POST") return json(historical, 201);
    if (String(url).includes("/tasks/")) return json(liveTask);
    throw new Error("Unexpected route");
  });
  await expect(requestWork(groupId, input(), key)).resolves.toEqual(historical);
  liveTask = { ...liveTask, revision: 7, status: "accepted", title: "最新任务标题" };
  await expect(requestWork(groupId, input(), key)).resolves.toEqual(historical);
  expect(transport.mock.calls.map(([url]) => url)).toEqual([path, path]);
  expect(historical.task).toEqual({ id: "task-1", group_id: groupId, revision_at_admission: 1 });
});
it("never retries or replaces the caller key after a network failure; manual same-key replay is one more POST", async () => {
  transport.mockRejectedValueOnce(new TypeError("SECRET network diagnostic")).mockResolvedValueOnce(json(receipt(), 201));
  const error: unknown = await requestWork(groupId, input(), key).catch((failure: unknown) => failure);
  expect(error).toBeInstanceOf(ApiError); expect(error).toMatchObject({ code: "disconnected" });
  expect(isRequestWorkDefinitiveRejection(error)).toBe(false); expect((error as ApiError).message).not.toMatch(/SECRET|diagnostic/);
  expect(transport).toHaveBeenCalledTimes(1);
  await expect(requestWork(groupId, input(), key)).resolves.toEqual(receipt());
  expect(transport).toHaveBeenCalledTimes(2);
  expect(transport.mock.calls[1]).toEqual(transport.mock.calls[0]);
});
it.each([false, true])("keeps cancellation unknown and sanitizes native errors (signal aborted=%s)", async abortSignal => {
  const controller = new AbortController(); if (abortSignal) controller.abort();
  transport.mockRejectedValueOnce(new DOMException("SECRET cancellation diagnostic", "AbortError"));
  const error: unknown = await requestWork(groupId, input(), key, controller.signal).catch((failure: unknown) => failure);
  expect(error).toBeInstanceOf(ApiError); expect(error).toMatchObject({ code: "request_canceled" });
  expect(isRequestWorkDefinitiveRejection(error)).toBe(false); expect((error as ApiError).message).toMatch(/结果未知/);
  expect((error as ApiError).message).not.toMatch(/SECRET|diagnostic/); expect(transport).toHaveBeenCalledTimes(1);
});
const httpErrors: [number, string, boolean][] = [
  [400, "invalid_input", true], [400, "invalid_json", true], [403, "invalid_host", true], [403, "invalid_origin", true],
  [404, "not_found", true], [405, "method_not_allowed", true], [409, "conflict", true], [413, "body_too_large", true],
  [415, "unsupported_media_type", true], [408, "request_canceled", false], [504, "deadline_exceeded", false],
  [500, "internal_error", false], [502, "http_error", false], [503, "internal_error", false], [401, "http_error", false], [429, "http_error", false],
];
it.each(httpErrors)("classifies HTTP %s/%s safely (definitive=%s)", async (status, code, definitive) => {
  transport.mockResolvedValueOnce(json({ error: { code, message: "SECRET backend diagnostic" } }, status));
  const error: unknown = await requestWork(groupId, input(), key).catch((failure: unknown) => failure);
  expect(error).toBeInstanceOf(ApiError); expect(error).toMatchObject({ status, code });
  expect(isRequestWorkDefinitiveRejection(error)).toBe(definitive);
  expect((error as ApiError).message).not.toMatch(/SECRET|backend|diagnostic/);
  expect((error as ApiError).message).toMatch(/[\u4e00-\u9fff]/);
  if (!definitive) expect((error as ApiError).message).toMatch(/结果未知/);
  if (status === 409) {
    expect((error as ApiError).message).toContain("工作请求与已有记录或任务状态冲突");
    expect((error as ApiError).message).not.toMatch(/幂等键.*冲突|更换|换.*键|新.*键/);
  }
  expect(transport).toHaveBeenCalledTimes(1);
});
it.each([409, 503])("uses safe fallbacks for non-JSON HTTP %s errors", async status => {
  transport.mockResolvedValueOnce(new Response("SECRET backend detail", { status }));
  const error: unknown = await requestWork(groupId, input(), key).catch((failure: unknown) => failure);
  expect(error).toMatchObject({ status, code: "http_error" });
  expect(isRequestWorkDefinitiveRejection(error)).toBe(status === 409); expect((error as ApiError).message).not.toMatch(/SECRET|backend/);
});
it("does not classify arbitrary errors as definitive rejection", () => {
  for (const error of [null, {}, new Error("invalid_input"), new DOMException("aborted", "AbortError"), new ApiError("unknown")]) {
    expect(isRequestWorkDefinitiveRejection(error)).toBe(false);
  }
});
