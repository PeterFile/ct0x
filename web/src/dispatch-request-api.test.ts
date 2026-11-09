// @vitest-environment node
import { once } from "node:events";
import { createServer } from "node:http";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError } from "./api";
import { getDispatchRequest, getTaskCoordination, isDispatchRequestDefinitiveRejection, requestDispatch } from "./dispatch-request-api";
import type { DispatchRequestInput, DispatchState, TaskCoordinationView } from "./dispatch-request-api";
import { deferred, json } from "./test/groups";

const groupId = "群/one?part=1#fragment%+", taskId = "任务/two?x#%+", requestId = "请求/three?x#%+";
const stamp = "2026-11-09T12:30:00.123456789Z", key = "Exact-Dispatch-Key!";
type Mutable<T> = { -readonly [P in keyof T]: T[P] extends object ? Mutable<T[P]> : T[P] };
const input = { expected_task_revision: 7, expected_authority_generation: 3, reason: "  请按授权协调。\n保留原文。  " };
const receipt = { id: requestId, group_id: groupId, topic_id: "topic/一", task_id: taskId, task_revision: 7,
  authority_generation: 3, mandate_id: "mandate-1", coordinator_member_id: "lead-1", binding_revision: 8,
  policy_revision: 9, requested_by: "user", reason: input.reason, created_at: stamp };
const authority: TaskCoordinationView = {
  coordination: { task_id: taskId, group_id: groupId, topic_id: receipt.topic_id, current_mandate_id: receipt.mandate_id, authority_generation: 3 },
  admission: {
    task: { id: taskId, group_id: groupId, revision_at_admission: 7 },
    decision: { id: "decision-1", trigger_id: "trigger-1", group_id: groupId, topic_id: receipt.topic_id, kind: "link_existing",
      actor_id: "user", policy_revision: "local-user-explicit/v1", reason: "明确授权", task_id: taskId,
      task_revision_at_admission: 7, mandate_id: receipt.mandate_id, created_at: stamp },
    mandate: { id: receipt.mandate_id, decision_id: "decision-1", origin_trigger_id: "trigger-1", group_id: groupId,
      topic_id: receipt.topic_id, task_id: taskId, task_revision_at_admission: 7, authority_kind: "local_user_explicit",
      authorized_by: "user", strength: "explicit", policy_revision: "local-user-explicit/v1", created_at: stamp,
      scope: { objective: "目标", description: "描述", acceptance: "可核验条件", assignee_id: "", dependency_ids: [] } },
  },
};
const delivery = { message_id: "message-1", delivery_id: "delivery-1", conversation_id: "conversation-1",
  coordinator_member_id: receipt.coordinator_member_id, materialized_from_generation: 2, created_at: stamp };
function stateFor(status: DispatchState["status"] = "pending"): DispatchState {
  const generation = status === "pending" ? 1 : 4;
  const decision = status === "assigned" || status === "declined";
  return { request_id: requestId, generation, status, attempt_count: generation - (status === "pending" ? 1 : decision ? 3 : 2),
    next_attempt_at: stamp, last_error_code: "", held_reason: status === "held" ? "no_eligible_worker" : "",
    delivery: ["enqueued", "assigned", "declined"].includes(status)
      ? { ...delivery, materialized_from_generation: generation - (decision ? 2 : 1) } : null };
}
const nativeFetch = globalThis.fetch;
const transport = vi.fn<typeof fetch>();
const failure = (promise: Promise<unknown>) => promise.then(() => undefined, error => error as unknown);
const post = (value = input, saveKey = key) => requestDispatch(groupId, taskId, value, saveKey);
function expectUnknown(error: unknown, status = 201) {
  expect(error).toBeInstanceOf(ApiError);
  expect(error).toMatchObject({ status, message: expect.stringContaining("未确认") });
  expect(isDispatchRequestDefinitiveRejection(error)).toBe(false);
  expect(String(error)).not.toContain("SECRET");
}
beforeEach(() => { transport.mockReset(); vi.stubGlobal("fetch", transport); });
afterEach(() => vi.unstubAllGlobals());

it.each(["", " \n\t ", "line\ntopic", "nul\0topic", " padded topic ", "x".repeat(4097), "💡".repeat(1100)])(
  "[repair 2] preserves opaque topic text across authority, 201 and exact GET %#", async topic => {
    const nextAuthority = { ...authority, coordination: { ...authority.coordination, topic_id: topic }, admission: { ...authority.admission,
      decision: { ...authority.admission.decision, topic_id: topic }, mandate: { ...authority.admission.mandate, topic_id: topic } } };
    const nextReceipt = { ...receipt, topic_id: topic }, view = { request: nextReceipt, state: stateFor() };
    transport.mockResolvedValueOnce(json(nextAuthority)).mockResolvedValueOnce(json(nextReceipt, 201)).mockResolvedValueOnce(json(view));
    await expect(getTaskCoordination(groupId, taskId)).resolves.toEqual(nextAuthority);
    await expect(post()).resolves.toEqual(nextReceipt);
    await expect(getDispatchRequest(groupId, taskId, requestId)).resolves.toEqual(view);
  },
);
it.each([null, 1, undefined, "\ud800"])("[repair 2] rejects non-text or unrepresentable topic %#", async topic => {
  const nextAuthority = { ...authority, coordination: { ...authority.coordination, topic_id: topic }, admission: { ...authority.admission,
    decision: { ...authority.admission.decision, topic_id: topic }, mandate: { ...authority.admission.mandate, topic_id: topic } } };
  transport.mockResolvedValueOnce(json(nextAuthority)).mockResolvedValueOnce(json({ ...receipt, topic_id: topic }, 201))
    .mockResolvedValueOnce(json({ request: { ...receipt, topic_id: topic }, state: stateFor() }));
  expect(await failure(getTaskCoordination(groupId, taskId))).toMatchObject({ code: "invalid_response" });
  expectUnknown(await failure(post()));
  expect(await failure(getDispatchRequest(groupId, taskId, requestId))).toMatchObject({ code: "invalid_response" });
});
it.each([
  { generation: 1, attempt_count: 0, last_error_code: "sqlite_busy" },
  { generation: 2, attempt_count: 1, last_error_code: "" },
  { generation: 1, attempt_count: 1, last_error_code: "sqlite_busy" },
  { generation: 4, attempt_count: 1, last_error_code: "sqlite_locked" },
  { status: "held", generation: 1, attempt_count: 0, held_reason: "task_stale" },
  { status: "held", generation: 4, attempt_count: 1, held_reason: "task_stale" },
  { status: "enqueued", generation: 4, attempt_count: 1, delivery: { ...delivery, materialized_from_generation: 3 } },
  { status: "enqueued", generation: 3, attempt_count: 1, delivery: { ...delivery, materialized_from_generation: 1 } },
  { status: "assigned", generation: 2, attempt_count: 0, delivery: { ...delivery, materialized_from_generation: 1 } },
  { status: "declined", generation: 5, attempt_count: 1, delivery: { ...delivery, materialized_from_generation: 3 } },
])("[repair 3] rejects impossible generation/count/error tuples %#", async change => {
  transport.mockResolvedValueOnce(json({ request: receipt, state: { ...stateFor(), ...change } }));
  expect(await failure(getDispatchRequest(groupId, taskId, requestId))).toMatchObject({ code: "invalid_response" });
});
it.each([
  { status: "pending", generation: 1, attempt_count: 0, last_error_code: "", held_reason: "", delivery: null },
  { status: "pending", generation: 6, attempt_count: 5, last_error_code: "sqlite_locked", held_reason: "", delivery: null },
  { status: "held", generation: 7, attempt_count: 5, last_error_code: "", held_reason: "no_eligible_worker", delivery: null },
  { status: "enqueued", generation: 7, attempt_count: 5, last_error_code: "", held_reason: "", delivery: { ...delivery, materialized_from_generation: 6 } },
  { status: "assigned", generation: 8, attempt_count: 5, last_error_code: "", held_reason: "", delivery: { ...delivery, materialized_from_generation: 6 } },
  { status: "declined", generation: 8, attempt_count: 5, last_error_code: "", held_reason: "", delivery: { ...delivery, materialized_from_generation: 6 } },
])("[repair 3] accepts reachable retry/held/decision tuple $status generation $generation", async change => {
  const view = { request: receipt, state: { ...stateFor(), ...change } };
  transport.mockResolvedValueOnce(json(view));
  await expect(getDispatchRequest(groupId, taskId, requestId)).resolves.toEqual(view);
});

it.each([
  ["pending", 1], ["held", 2], ["enqueued", 2], ["assigned", 3], ["declined", 3],
] as const)("[initial time] requires exact creation time for zero-retry %s", async (status, generation) => {
  const request = { ...receipt, created_at: "2026-11-09T12:30:00Z" }, base = stateFor(status);
  const state = { ...base, generation, attempt_count: 0, next_attempt_at: request.created_at,
    delivery: base.delivery && { ...base.delivery, materialized_from_generation: 1 } };
  for (const next of ["2026-11-10T12:30:00Z", "2026-11-09T12:30:00.000Z"]) {
    transport.mockResolvedValueOnce(json({ request, state: { ...state, next_attempt_at: next } }));
    expect.soft(await failure(getDispatchRequest(groupId, taskId, requestId))).toMatchObject({ code: "invalid_response" });
  }
  const view = { request, state }; transport.mockResolvedValueOnce(json(view));
  await expect(getDispatchRequest(groupId, taskId, requestId)).resolves.toEqual(view);
});
it.each([
  ["pending", 2], ["held", 3], ["enqueued", 3], ["assigned", 4], ["declined", 4],
] as const)("[initial time] permits different positive-retry times, including before creation, for %s", async (status, generation) => {
  const base = stateFor(status);
  for (const next of ["2026-11-08T12:30:00.123456789Z", "2026-11-10T12:30:00.123456789Z"]) {
    const view = { request: receipt, state: { ...base, generation, attempt_count: 1, next_attempt_at: next,
      last_error_code: status === "pending" ? "sqlite_busy" : "",
      delivery: base.delivery && { ...base.delivery, materialized_from_generation: 2 } } };
    transport.mockResolvedValueOnce(json(view));
    await expect(getDispatchRequest(groupId, taskId, requestId)).resolves.toEqual(view);
  }
});

it("sends only the explicit three-field snapshot to opaque encoded path segments", async () => {
  transport.mockResolvedValueOnce(json(receipt, 201));
  const untrusted = { ...input, group_id: "foreign", task_id: "foreign", requested_by: "other", mandate_id: "fake",
    coordinator_member_id: "fake", binding_revision: 99, policy_revision: 99, actor: "admin", grants: ["all"], state: "ready" };
  await expect(post(untrusted)).resolves.toEqual(receipt);
  const [url, init] = transport.mock.calls[0];
  expect(url).toBe(`/api/v1/groups/${encodeURIComponent(groupId)}/tasks/${encodeURIComponent(taskId)}/dispatch-requests`);
  expect(init?.method).toBe("POST"); expect(init?.redirect).toBe("error");
  expect(init?.body).toBe(JSON.stringify(input));
  expect(Object.fromEntries(new Headers(init?.headers))).toEqual({ "content-type": "application/json", "idempotency-key": key });
  expect(transport).toHaveBeenCalledTimes(1);
});
it("matches the original sent DTO even if its caller mutates while awaiting the historical 201", async () => {
  const waiting = deferred<Response>(), mutable = { ...input };
  transport.mockReturnValueOnce(waiting.promise);
  const pending = post(mutable);
  mutable.expected_task_revision = 99; mutable.expected_authority_generation = 99; mutable.reason = "changed";
  waiting.resolve(json(receipt, 201));
  await expect(pending).resolves.toEqual(receipt);
  transport.mockResolvedValueOnce(json(receipt, 201));
  await expect(post()).resolves.toEqual(receipt);
  expect(transport.mock.calls[1]).toEqual(transport.mock.calls[0]);
});
it("reads the current authority and its exact immutable admission, including link_existing", async () => {
  const signal = new AbortController().signal;
  transport.mockResolvedValueOnce(json(authority));
  await expect(getTaskCoordination(groupId, taskId, signal)).resolves.toEqual(authority);
  expect(transport.mock.calls[0][0]).toBe(`/api/v1/groups/${encodeURIComponent(groupId)}/tasks/${encodeURIComponent(taskId)}/coordination`);
  expect(transport.mock.calls[0][1]).toMatchObject({ signal, redirect: "error" });
  expect(transport.mock.calls[0][1]?.body).toBeUndefined();
});
it.each(["pending", "enqueued", "held", "assigned", "declined"] as const)("reads exact known Request ID with %s state", async status => {
  const view = { request: receipt, state: stateFor(status) }, signal = new AbortController().signal;
  transport.mockResolvedValueOnce(json(view));
  await expect(getDispatchRequest(groupId, taskId, requestId, signal)).resolves.toEqual(view);
  expect(transport.mock.calls[0][0]).toBe(`/api/v1/groups/${encodeURIComponent(groupId)}/coordination/dispatch-requests/${encodeURIComponent(requestId)}`);
  expect(transport.mock.calls[0][1]).toMatchObject({ signal, redirect: "error" });
});

it.each([
  ["id", ""], ["group_id", "foreign"], ["task_id", "foreign"], ["task_revision", 8], ["authority_generation", 4],
  ["reason", input.reason.trim()], ["requested_by", "worker"], ["topic_id", null], ["mandate_id", "\u0085bad"],
  ["coordinator_member_id", ""], ["binding_revision", 0], ["policy_revision", -1], ["created_at", "2026-02-30T00:00:00Z"],
  ["created_at", "0001-01-01T00:00:00Z"], ["created_at", "2026-11-09T12:30:00+00:00"], ["runtime_ready", true],
])("keeps a mismatching or malformed 201 unknown: %s %#", async (field, value) => {
  transport.mockResolvedValueOnce(json({ ...receipt, [field]: value }, 201));
  expectUnknown(await failure(post()));
});
it.each([200, 202, 204])("never treats HTTP %s as the registration receipt", async status => {
  transport.mockResolvedValueOnce(status === 204 ? new Response(null, { status }) : json(receipt, status));
  expectUnknown(await failure(post()), status);
});
it.each([
  ["pointer", (v: Mutable<TaskCoordinationView>) => { v.coordination.current_mandate_id = "wrong"; }],
  ["generation", (v: Mutable<TaskCoordinationView>) => { v.coordination.authority_generation = 0; }],
  ["task", (v: Mutable<TaskCoordinationView>) => { v.admission.task.id = "foreign"; }],
  ["topic", (v: Mutable<TaskCoordinationView>) => { v.admission.mandate.topic_id = "foreign"; }],
  ["decision", (v: Mutable<TaskCoordinationView>) => { v.admission.mandate.decision_id = "foreign"; }],
  ["trigger", (v: Mutable<TaskCoordinationView>) => { v.admission.mandate.origin_trigger_id = "foreign"; }],
  ["revision", (v: Mutable<TaskCoordinationView>) => { v.admission.mandate.task_revision_at_admission = 8; }],
  ["time", (v: Mutable<TaskCoordinationView>) => { v.admission.mandate.created_at = "2026-11-09T00:00:00Z"; }],
  ["scope", (v: Mutable<TaskCoordinationView>) => { v.admission.mandate.scope.dependency_ids = ["a", "a"]; }],
] as const)("rejects a broken authority association: %s", async (_name, change) => {
  const value = structuredClone(authority) as Mutable<TaskCoordinationView>; change(value);
  transport.mockResolvedValueOnce(json(value));
  expect(await failure(getTaskCoordination(groupId, taskId))).toMatchObject({ code: "invalid_response" });
});
it.each([
  { status: "ready" }, { status: { toString: null } }, { request_id: "foreign" }, { generation: 0 }, { attempt_count: -1 }, { attempt_count: 1.5 },
  { next_attempt_at: null }, { last_error_code: null }, { held_reason: "task_stale" }, { delivery: undefined },
  { delivery }, { status: "held", held_reason: "" }, { status: "held", held_reason: "made_up" }, { status: "enqueued", delivery: null },
  { status: "assigned", delivery: { ...delivery, coordinator_member_id: "foreign" } },
  { status: "declined", delivery: { ...delivery, materialized_from_generation: 4 } },
  { status: "enqueued", delivery: { ...delivery, created_at: "bad" } },
])("rejects malformed state or delivery %# without inventing execution", async change => {
  transport.mockResolvedValueOnce(json({ request: receipt, state: { ...stateFor(), ...change } }));
  expect(await failure(getDispatchRequest(groupId, taskId, requestId))).toMatchObject({ code: "invalid_response" });
});
it.each(["id", "group_id", "task_id"])("checks GET request scope: %s", async field => {
  transport.mockResolvedValueOnce(json({ request: { ...receipt, [field]: "foreign" }, state: stateFor() }));
  expect(await failure(getDispatchRequest(groupId, taskId, requestId))).toMatchObject({ code: "invalid_response" });
});

const refusalCodes = [[400, "invalid_input"], [400, "invalid_json"], [403, "invalid_host"], [403, "invalid_origin"],
  [404, "not_found"], [405, "method_not_allowed"], [409, "conflict"], [413, "body_too_large"], [415, "unsupported_media_type"]] as const;
it.each(refusalCodes)("recognizes only the actual HTTP %s / %s refusal contract", async (status, code) => {
  transport.mockResolvedValueOnce(json({ error: { code, message: "SECRET" } }, status));
  const error = await failure(post());
  expect(error).toMatchObject({ status, code }); expect(String(error)).not.toContain("SECRET");
  expect(isDispatchRequestDefinitiveRejection(error)).toBe(true);
  if (status === 409) expect(String(error)).toContain("分派请求与当前任务、授权或协调配置冲突");
  expect(isDispatchRequestDefinitiveRejection(new ApiError("fake", status, code))).toBe(false);
});
it.each([400, 403, 404, 405, 409, 413, 415])("keeps malformed or wrong-code HTTP %s unknown", async status => {
  for (const value of [null, {}, { error: { code: "conflict" } }, { error: { code: "unrelated", message: "SECRET" } },
    { error: { code: "conflict", message: "" } }, { error: { code: "conflict", message: "SECRET", extra: true } }]) {
    transport.mockResolvedValueOnce(json(value, status));
    expectUnknown(await failure(post()), status);
  }
});
it.each([408, 504, 500, 502, 503])("keeps HTTP %s unknown", async status => {
  transport.mockResolvedValueOnce(json({ error: { code: "internal_error", message: "SECRET" } }, status));
  expectUnknown(await failure(post()), status);
});
it("keeps disconnected and failed-body responses unknown", async () => {
  transport.mockRejectedValueOnce(new TypeError("SECRET"));
  expectUnknown(await failure(post()), 0);
  transport.mockResolvedValueOnce(new Response(new ReadableStream({ start(controller) { controller.error(new Error("SECRET")); } }), { status: 201 }));
  expectUnknown(await failure(post()));
});
it.each(["7.0000000000000001", "7e0", "9007199254740992", "9007199254740990.5", "9223372036854775807", "-0"])(
  "checks wire integers before JSON parsing can round: %s", async token => {
    transport.mockResolvedValueOnce(new Response(JSON.stringify(receipt).replace('"binding_revision":8', `"binding_revision":${token}`), { status: 201 }));
    expectUnknown(await failure(post()));
    transport.mockResolvedValueOnce(new Response(JSON.stringify(authority).replace('"authority_generation":3', `"authority_generation":${token}`)));
    expect(await failure(getTaskCoordination(groupId, taskId))).toMatchObject({ code: "invalid_response" });
    transport.mockResolvedValueOnce(new Response(JSON.stringify({ request: receipt, state: stateFor() }).replace('"attempt_count":0', `"attempt_count":${token}`)));
    expect(await failure(getDispatchRequest(groupId, taskId, requestId))).toMatchObject({ code: "invalid_response" });
  },
);
it("rejects invalid raw UTF-8 instead of accepting replacement characters", async () => {
  const encoded = new TextEncoder().encode(JSON.stringify({ ...receipt, coordinator_member_id: "MARK" }));
  const at = new TextDecoder().decode(encoded).indexOf("MARK");
  const bytes = new Uint8Array([...encoded.slice(0, at), 0xff, ...encoded.slice(at + 4)]);
  transport.mockResolvedValueOnce(new Response(bytes, { status: 201 }));
  expectUnknown(await failure(post()));
});
it.each([0, -1, 0.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1, null, "1"])("rejects local invalid expected versions %# before fetch", async value => {
  for (const field of ["expected_task_revision", "expected_authority_generation"]) {
    const error = await failure(post({ ...input, [field]: value } as DispatchRequestInput));
    expect(error).toMatchObject({ status: 0, code: "invalid_input" });
    expect(isDispatchRequestDefinitiveRejection(error)).toBe(true);
  }
  expect(transport).not.toHaveBeenCalled();
});
it.each(["", " ", "\u0085", "\ud800"])("rejects invalid reasons %# locally without trimming", async reason => {
  expect(isDispatchRequestDefinitiveRejection(await failure(post({ ...input, reason })))).toBe(true);
  expect(transport).not.toHaveBeenCalled();
});
it.each(["\ufeff", "💡 7.5e20"])("preserves valid reason bytes %#", async reason => {
  transport.mockResolvedValueOnce(json({ ...receipt, reason }, 201));
  await expect(post({ ...input, reason })).resolves.toMatchObject({ reason });
});
it.each(["", ".", "..", " id", "id\u0085", "a\nb", "\ud800", "x".repeat(4097)])("rejects unaddressable IDs %# before fetch", async id => {
  for (const promise of [requestDispatch(id, taskId, input, key), requestDispatch(groupId, id, input, key),
    getTaskCoordination(id, taskId), getTaskCoordination(groupId, id), getDispatchRequest(groupId, taskId, id)]) {
    expect(await failure(promise)).toMatchObject({ status: 0, code: "invalid_input" });
  }
  expect(transport).not.toHaveBeenCalled();
});
it.each(["", "bad key", "a,b", "\n", "中文", "x".repeat(129)])("rejects invalid key %# before fetch", async saveKey => {
  expect(isDispatchRequestDefinitiveRejection(await failure(post(input, saveKey)))).toBe(true);
  expect(transport).not.toHaveBeenCalled();
});
it.each([303, 307, "drop"] as const)("uses real HTTP and the exact original operation after response %s, never forwarding writes", async outcome => {
  const path = `/api/v1/groups/${encodeURIComponent(groupId)}/tasks/${encodeURIComponent(taskId)}/dispatch-requests`;
  const source: { path: string | undefined; key: string | string[] | undefined; body: string }[] = [], targets: string[] = [];
  const saved = new Set<string>();
  const server = createServer((req, res) => {
    let body = "";
    req.setEncoding("utf8"); req.on("data", chunk => { body += chunk; });
    req.on("end", () => {
      if (req.url !== path) { targets.push(req.url ?? ""); res.writeHead(500).end(); return; }
      source.push({ path: req.url, key: req.headers["idempotency-key"], body });
      saved.add(`${req.headers["idempotency-key"]}:${body}`);
      if (source.length === 1) {
        if (outcome === "drop") req.socket.destroy();
        else res.writeHead(outcome, { Location: "/must-not-receive-the-write" }).end();
        return;
      }
      res.writeHead(201, { "Content-Type": "application/json" }).end(JSON.stringify(receipt));
    });
  });
  try {
    server.listen(0, "127.0.0.1"); await once(server, "listening");
    const address = server.address();
    if (!address || typeof address === "string" || [7310, 55594, 4173].includes(address.port)) throw new Error("Unexpected fixture address");
    const base = `http://127.0.0.1:${address.port}`;
    vi.stubGlobal("fetch", (url: RequestInfo | URL, init?: RequestInit) => nativeFetch(new URL(String(url), base), init));
    expectUnknown(await failure(post()), 0);
    await expect(post()).resolves.toEqual(receipt);
    expect(source).toEqual(Array(2).fill({ path, key, body: JSON.stringify(input) }));
    expect(targets).toEqual([]); expect(saved.size).toBe(1);
  } finally {
    if (server.listening) await new Promise<void>((resolve, reject) => {
      server.close(error => error ? reject(error) : resolve()); server.closeAllConnections();
    });
  }
});
it("accepts safe maximum snapshots without inventing a version increment", async () => {
  const value = { ...input, expected_task_revision: Number.MAX_SAFE_INTEGER, expected_authority_generation: Number.MAX_SAFE_INTEGER };
  transport.mockResolvedValueOnce(json({ ...receipt, task_revision: value.expected_task_revision, authority_generation: value.expected_authority_generation }, 201));
  await expect(post(value)).resolves.toMatchObject({ task_revision: Number.MAX_SAFE_INTEGER, authority_generation: Number.MAX_SAFE_INTEGER });
});
