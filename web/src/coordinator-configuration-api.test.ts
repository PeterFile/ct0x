// @vitest-environment node
import { once } from "node:events";
import { createServer } from "node:http";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError } from "./api";
import {
  getCoordinator, getDispatchPolicy, isCoordinatorConfigurationDefinitiveRejection,
  setCoordinator, setDispatchPolicy,
} from "./coordinator-configuration-api";
import type { CoordinatorInput, DispatchPolicyInput } from "./coordinator-configuration-api";
import { deferred, json } from "./test/groups";

const nativeFetch = globalThis.fetch;

const groupId = "群/one?part=1#fragment%+";
const key = "Exact-Save-Key!";
const binding = { group_id: groupId, member_id: "lead-1", revision: 3 };
const policy = { group_id: groupId, revision: 7, allowed_worker_ids: ["worker-a", "worker-b"] };
const coordinatorInput = { member_id: binding.member_id, expected_revision: 2 };
const policyInput = { allowed_worker_ids: ["worker-b", "worker-a", "worker-b"], expected_revision: 6 };
const transport = vi.fn<typeof fetch>();
const resources = [
  {
    resource: "coordinator", input: coordinatorInput, receipt: binding,
    read: async (group: string, signal?: AbortSignal) => getCoordinator(group, signal),
    save: async (group: string, input: unknown, saveKey: string, signal?: AbortSignal) =>
      setCoordinator(group, input as CoordinatorInput, saveKey, signal),
  },
  {
    resource: "dispatch-policy", input: policyInput, receipt: policy,
    read: async (group: string, signal?: AbortSignal) => getDispatchPolicy(group, signal),
    save: async (group: string, input: unknown, saveKey: string, signal?: AbortSignal) =>
      setDispatchPolicy(group, input as DispatchPolicyInput, saveKey, signal),
  },
];
const routes = resources.flatMap(resource => ["GET", "PUT"].map(method => ({ ...resource, method })));
type Route = (typeof routes)[number];
async function invoke(route: Route, signal?: AbortSignal): Promise<unknown> {
  return route.method === "GET" ? route.read(groupId, signal) : route.save(groupId, route.input, key, signal);
}
const failure = (operation: Promise<unknown>): Promise<unknown> => operation.then(() => undefined, error => error);
function expectUnknown(error: unknown, status: number, code?: string) {
  expect(error).toBeInstanceOf(ApiError);
  expect(error).toMatchObject({ status, message: expect.stringContaining("结果未知"), ...(code ? { code } : {}) });
  expect(error).toHaveProperty("message", expect.not.stringContaining("SECRET"));
  expect(isCoordinatorConfigurationDefinitiveRejection(error)).toBe(false);
}
function expectLocalRejection(error: unknown) {
  expect(error).toBeInstanceOf(ApiError);
  expect(error).toMatchObject({ status: 0, code: "invalid_input" });
  expect(isCoordinatorConfigurationDefinitiveRejection(error)).toBe(true);
}
beforeEach(() => { transport.mockReset(); vi.stubGlobal("fetch", transport); });
afterEach(() => vi.unstubAllGlobals());

it.each(routes)("$method $resource uses one encoded group segment and only its owned DTO", async route => {
  const signal = new AbortController().signal;
  const untrusted = { ...route.input, group_id: "foreign", actor: "other", role: "admin", grants: ["all"], runtime: "ready" };
  transport.mockResolvedValueOnce(json(route.receipt));
  const result: unknown = await (route.method === "GET" ? route.read(groupId, signal)
    : route.save(groupId, untrusted, key, signal)).catch(error => error);
  expect(result).toEqual(route.receipt);
  expect(transport).toHaveBeenCalledTimes(1);
  const [path, init] = transport.mock.calls[0];
  expect(path).toBe(`/api/v1/groups/${encodeURIComponent(groupId)}/coordination/${route.resource}`);
  expect(init?.signal).toBe(signal);
  expect(init?.redirect).toBe("error");
  expect(init?.method ?? "GET").toBe(route.method);
  if (route.method === "PUT") {
    expect(init?.body).toBe(JSON.stringify(route.input));
    expect(Object.fromEntries(new Headers(init?.headers))).toEqual({
      "content-type": "application/json", "idempotency-key": key,
    });
  } else {
    expect(init?.body).toBeUndefined();
    expect(new Headers(init?.headers).has("Idempotency-Key")).toBe(false);
  }
});

it.each(routes)("$method $resource preserves a valid 404 rather than synthesizing configuration", async route => {
  transport.mockResolvedValueOnce(json({ error: { code: "not_found", message: "记录不存在或不可访问" } }, 404));
  const error = await failure(invoke(route));
  expect(error).toBeInstanceOf(ApiError);
  expect(error).toMatchObject({ status: 404, code: "not_found" });
  expect(isCoordinatorConfigurationDefinitiveRejection(error)).toBe(true);
  expect(transport).toHaveBeenCalledTimes(1);
});

it("distinguishes a missing policy from an explicit empty policy, without inventing a coordinator", async () => {
  const missing = { group_id: groupId, revision: 0, allowed_worker_ids: [] };
  transport.mockResolvedValueOnce(json(missing));
  await expect(getDispatchPolicy(groupId)).resolves.toEqual(missing);
  const empty = { ...missing, revision: 1 };
  transport.mockResolvedValueOnce(json(empty));
  await expect(setDispatchPolicy(groupId, { allowed_worker_ids: [], expected_revision: 0 }, key)).resolves.toEqual(empty);
  expect(transport.mock.calls[1][1]?.body).toBe('{"allowed_worker_ids":[],"expected_revision":0}');
  transport.mockResolvedValueOnce(json({ ...binding, revision: 0 }));
  expectUnknown(await failure(getCoordinator(groupId)), 200, "invalid_response");
  transport.mockResolvedValueOnce(json({ ...policy, revision: 0 }));
  expectUnknown(await failure(getDispatchPolicy(groupId)), 200, "invalid_response");
  expect(transport).toHaveBeenCalledTimes(4);
});

const malformedReceipts = resources.flatMap(resource => [
  ["null", null], ["array", []], ["primitive", "configuration"], ["missing fields", {}],
  ["wrong group", { ...resource.receipt, group_id: "another-group" }],
  ["missing group", { ...resource.receipt, group_id: undefined }],
  ["wrong group type", { ...resource.receipt, group_id: 42 }],
  ["extra runtime field", { ...resource.receipt, runtime_ready: true }],
  ["extra input field", { ...resource.receipt, expected_revision: resource.input.expected_revision }],
  ["missing revision", { ...resource.receipt, revision: undefined }],
  ["negative revision", { ...resource.receipt, revision: -1 }],
  ["fractional revision", { ...resource.receipt, revision: 1.5 }],
  ["unsafe revision", { ...resource.receipt, revision: Number.MAX_SAFE_INTEGER + 1 }],
  ["string revision", { ...resource.receipt, revision: "1" }],
  ["null revision", { ...resource.receipt, revision: null }],
  ["boolean revision", { ...resource.receipt, revision: true }],
].map(([name, value]) => ({ ...resource, name, value })));
it.each(malformedReceipts)("$resource rejects $name on both reads and receipts", async resource => {
  transport.mockResolvedValueOnce(json(resource.value));
  expectUnknown(await failure(resource.read(groupId)), 200, "invalid_response");
  transport.mockResolvedValueOnce(json(resource.value));
  expectUnknown(await failure(resource.save(groupId, resource.input, key)), 200, "invalid_response");
});

it.each([
  ["missing member", undefined], ["null member", null], ["wrong member type", 1], ["empty member", ""],
  ["Go whitespace", "\u0085lead"], ["broken Unicode", "\ud800"], ["oversized member", "😀".repeat(1025)],
])("rejects a binding with %s", async (_name, member_id) => {
  transport.mockResolvedValueOnce(json({ ...binding, member_id }));
  expectUnknown(await failure(getCoordinator(groupId)), 200, "invalid_response");
});
it.each([
  ["missing workers", undefined], ["null workers", null], ["non-array workers", "worker-a"],
  ["duplicate workers", ["worker-a", "worker-a"]], ["empty ID", [""]], ["null ID", [null]],
  ["wrong ID type", [1]], ["Go whitespace", ["worker\u0085"]], ["broken Unicode", ["\udc00"]],
])("rejects a dispatch policy with %s", async (_name, allowed_worker_ids) => {
  transport.mockResolvedValueOnce(json({ ...policy, allowed_worker_ids }));
  expectUnknown(await failure(getDispatchPolicy(groupId)), 200, "invalid_response");
});

it.each(resources)("$resource reads MAX_SAFE_INTEGER without allowing a further write", async resource => {
  const receipt = { ...resource.receipt, revision: Number.MAX_SAFE_INTEGER };
  transport.mockResolvedValueOnce(json(receipt));
  await expect(resource.read(groupId)).resolves.toEqual(receipt);
  expectLocalRejection(await failure(resource.save(groupId, { ...resource.input, expected_revision: receipt.revision }, key)));
  expect(transport).toHaveBeenCalledTimes(1);
});
it.each(resources.flatMap(resource => [0, Number.MAX_SAFE_INTEGER - 1].map(expected_revision => ({ ...resource, expected_revision })) ))(
  "$resource accepts exact expected revision $expected_revision and the next safe receipt", async resource => {
    const input = { ...resource.input, expected_revision: resource.expected_revision };
    const receipt = { ...resource.receipt, revision: resource.expected_revision + 1 };
    transport.mockResolvedValueOnce(json(receipt));
    await expect(resource.save(groupId, input, key)).resolves.toEqual(receipt);
    expect(transport.mock.calls[0][1]?.body).toBe(JSON.stringify(input));
  },
);
it.each([-1, 0.5, NaN, Infinity, Number.MAX_SAFE_INTEGER, Number.MAX_SAFE_INTEGER + 1, null, undefined, "0", true])(
  "rejects unsafe or nonnumeric expected revision %s locally", async expected_revision => {
    for (const resource of resources) {
      expectLocalRejection(await failure(resource.save(groupId, { ...resource.input, expected_revision }, key)));
    }
    expect(transport).not.toHaveBeenCalled();
  },
);

const goWhitespace = ["\t", "\n", "\v", "\f", "\r", " ", "\u0085", "\u00a0", "\u1680",
  ...Array.from({ length: 11 }, (_, index) => String.fromCharCode(0x2000 + index)),
  "\u2028", "\u2029", "\u202f", "\u205f", "\u3000"];
const invalidIdentifiers: [string, unknown][] = [
  ["empty", ""], ["null", null], ["missing", undefined], ["number", 1], ["lone high surrogate", "\ud800"],
  ["lone low surrogate", "\udc00"], ["NUL inside", "a\0b"], ["CR inside", "a\rb"], ["LF inside", "a\nb"],
  ["ASCII over byte limit", "a".repeat(4097)], ["Unicode over byte limit", "😀".repeat(1025)],
  ...goWhitespace.flatMap(space => [[`leading ${JSON.stringify(space)}`, `${space}id`],
    [`trailing ${JSON.stringify(space)}`, `id${space}`]] as [string, unknown][]),
];
it.each(invalidIdentifiers)("rejects invalid group/member IDs before any fetch: %s", async (_name, id) => {
  for (const resource of resources) {
    expectLocalRejection(await failure(resource.read(id as string)));
    expectLocalRejection(await failure(resource.save(id as string, resource.input, key)));
  }
  expectLocalRejection(await failure(setCoordinator(groupId, { member_id: id as string, expected_revision: 0 }, key)));
  expectLocalRejection(await failure(setDispatchPolicy(groupId, { allowed_worker_ids: [id as string], expected_revision: 0 }, key)));
  expect(transport).not.toHaveBeenCalled();
});
it.each([
  ["one byte", "a"], ["4096 ASCII bytes", "a".repeat(4096)], ["4096 Unicode bytes", "😀".repeat(1024)],
  ["BOM boundaries", "\ufeffid\ufeff"], ["interior NEL", "a\u0085b"], ["interior tab", "a\tb"],
  ["interior space", "a b"], ["composed Unicode", "é"], ["decomposed Unicode", "e\u0301"],
])("accepts valid Go-compatible IDs unchanged: %s", async (_name, id) => {
  const coordinator = { group_id: id, member_id: id, revision: 1 };
  const dispatch = { group_id: id, allowed_worker_ids: [id], revision: 1 };
  transport.mockResolvedValueOnce(json(coordinator)).mockResolvedValueOnce(json(coordinator))
    .mockResolvedValueOnce(json(dispatch)).mockResolvedValueOnce(json(dispatch));
  await expect(getCoordinator(id)).resolves.toEqual(coordinator);
  await expect(setCoordinator(id, { member_id: id, expected_revision: 0 }, key)).resolves.toEqual(coordinator);
  await expect(getDispatchPolicy(id)).resolves.toEqual(dispatch);
  await expect(setDispatchPolicy(id, { allowed_worker_ids: [id], expected_revision: 0 }, key)).resolves.toEqual(dispatch);
  expect(transport.mock.calls[0][0]).toBe(`/api/v1/groups/${encodeURIComponent(id)}/coordination/coordinator`);
  expect(transport.mock.calls[1][1]?.body).toBe(JSON.stringify({ member_id: id, expected_revision: 0 }));
  expect(transport.mock.calls[3][1]?.body).toBe(JSON.stringify({ allowed_worker_ids: [id], expected_revision: 0 }));
});
it.each(["", " ", " key", "key ", "a b", "a,b", ",", "a\tb", "a\nb", "a\rb", "a\0b", "\x7f", "\u0080", "\u0085", "\ufeff", "中文", "x".repeat(129), null, undefined, 1])(
  "rejects a non-token idempotency key %# before sending", async saveKey => {
    for (const resource of resources) {
      expectLocalRejection(await failure(resource.save(groupId, resource.input, saveKey as string)));
    }
    expect(transport).not.toHaveBeenCalled();
  },
);
it.each(["!", "~", Array.from({ length: 94 }, (_, index) => String.fromCharCode(33 + index)).join("").replace(",", ""), "X".repeat(128)])(
  "preserves every permitted key byte %#", async saveKey => {
    for (const resource of resources) {
      transport.mockResolvedValueOnce(json(resource.receipt));
      await expect(resource.save(groupId, resource.input, saveKey)).resolves.toEqual(resource.receipt);
    }
    for (const [, init] of transport.mock.calls) expect(new Headers(init?.headers).get("Idempotency-Key")).toBe(saveKey);
  },
);
it.each([null, undefined, [], "input", 42, {}])("rejects malformed input containers %# locally", async input => {
  for (const resource of resources) expectLocalRejection(await failure(resource.save(groupId, input, key)));
  expect(transport).not.toHaveBeenCalled();
});
it.each([null, undefined, "worker", {}, [null], [42], Array(1)])("requires an actual, fully populated worker array %#", async allowed_worker_ids => {
  expectLocalRejection(await failure(setDispatchPolicy(groupId, { allowed_worker_ids, expected_revision: 0 } as DispatchPolicyInput, key)));
  expect(transport).not.toHaveBeenCalled();
});

it.each(resources)("$resource matches the original synchronous sent snapshot after caller mutation", async resource => {
  const waiting = deferred<Response>();
  const input = { member_id: binding.member_id, allowed_worker_ids: [...policyInput.allowed_worker_ids], expected_revision: resource.input.expected_revision };
  const originalWorkers = input.allowed_worker_ids;
  transport.mockReturnValueOnce(waiting.promise);
  const pending = resource.save(groupId, input, key);
  expect(transport).toHaveBeenCalledTimes(1);
  expect(transport.mock.calls[0][1]?.body).toBe(JSON.stringify(resource.input));
  input.member_id = "mutated-lead";
  input.expected_revision = 50;
  originalWorkers.splice(0, originalWorkers.length, "mutated-worker");
  input.allowed_worker_ids = ["replacement-worker"];
  waiting.resolve(json(resource.receipt));
  await expect(pending).resolves.toEqual(resource.receipt);
});
it.each(resources)("$resource cannot accept a receipt for the caller's later mutation", async resource => {
  const waiting = deferred<Response>();
  const input = { member_id: binding.member_id, allowed_worker_ids: [...policyInput.allowed_worker_ids], expected_revision: resource.input.expected_revision };
  transport.mockReturnValueOnce(waiting.promise);
  const pending = resource.save(groupId, input, key);
  input.member_id = "mutated-lead";
  input.allowed_worker_ids.splice(0, input.allowed_worker_ids.length, "mutated-worker");
  input.expected_revision = 50;
  waiting.resolve(json(resource.resource === "coordinator"
    ? { group_id: groupId, member_id: input.member_id, revision: 51 }
    : { group_id: groupId, allowed_worker_ids: [...input.allowed_worker_ids], revision: 51 }));
  expectUnknown(await failure(pending), 200, "invalid_response");
});
it("matches Go's canonical worker set without sorting or deduplicating the sent array", async () => {
  const workers = Object.freeze(["😀", "\ue000", "😀", "worker"]);
  const input = Object.freeze({ allowed_worker_ids: workers, expected_revision: 0 });
  const receipt = { group_id: groupId, revision: 1, allowed_worker_ids: ["worker", "\ue000", "😀"] };
  transport.mockResolvedValueOnce(json(receipt));
  await expect(setDispatchPolicy(groupId, input, key)).resolves.toEqual(receipt);
  expect(transport.mock.calls[0][1]?.body).toBe(JSON.stringify(input));
  expect(workers).toEqual(["😀", "\ue000", "😀", "worker"]);
  transport.mockResolvedValueOnce(json({ ...receipt, allowed_worker_ids: ["😀", "worker", "\ue000"] }));
  await expect(getDispatchPolicy(groupId)).resolves.toEqual({ ...receipt, allowed_worker_ids: ["😀", "worker", "\ue000"] });
});
it.each([
  ["wrong member", { ...binding, member_id: "other-lead" }], ["old revision", { ...binding, revision: 2 }],
  ["skipped revision", { ...binding, revision: 4 }], ["zero revision", { ...binding, revision: 0 }],
])("keeps a 200 coordinator receipt with %s unknown", async (_name, receipt) => {
  transport.mockResolvedValueOnce(json(receipt));
  expectUnknown(await failure(setCoordinator(groupId, coordinatorInput, key)), 200, "invalid_response");
});
it.each([
  ["missing worker", ["worker-a"]], ["extra worker", ["worker-a", "worker-b", "worker-c"]],
  ["substituted worker", ["worker-a", "worker-c"]], ["duplicate worker", ["worker-a", "worker-b", "worker-b"]],
  ["empty set", []],
])("keeps a 200 policy receipt with %s unknown", async (_name, allowed_worker_ids) => {
  transport.mockResolvedValueOnce(json({ ...policy, allowed_worker_ids }));
  expectUnknown(await failure(setDispatchPolicy(groupId, policyInput, key)), 200, "invalid_response");
});
it.each([0, 6, 8])("requires the policy receipt revision to be exactly expected + 1, not %s", async revision => {
  transport.mockResolvedValueOnce(json({ ...policy, revision }));
  expectUnknown(await failure(setDispatchPolicy(groupId, policyInput, key)), 200, "invalid_response");
});
it.each(resources)("$resource returns historical PUT receipts separately from newer GET values, without implicit requests", async resource => {
  const current = resource.resource === "coordinator" ? { ...binding, member_id: "new-lead", revision: 90 }
    : { ...policy, allowed_worker_ids: ["new-worker"], revision: 90 };
  transport.mockResolvedValueOnce(json(current)).mockResolvedValueOnce(json(resource.receipt)).mockResolvedValueOnce(json(current));
  await expect(resource.read(groupId)).resolves.toEqual(current);
  await expect(resource.save(groupId, resource.input, key)).resolves.toEqual(resource.receipt);
  await expect(resource.read(groupId)).resolves.toEqual(current);
  expect(transport.mock.calls.map(([, init]) => init?.method ?? "GET")).toEqual(["GET", "PUT", "GET"]);
});

it.each(routes.flatMap(route => [201, 202, 204, 206, 207, 301, 302, 303, 307, 308].map(status => ({ ...route, status }))))(
  "$method $resource never accepts unexpected HTTP $status as success", async route => {
    transport.mockResolvedValueOnce(route.status === 204 ? new Response(null, { status: 204 }) : json(route.receipt, route.status));
    expectUnknown(await failure(invoke(route)), route.status, "invalid_response");
    expect(transport).toHaveBeenCalledTimes(1);
  },
);
it.each(routes.flatMap(route => [400, 404, 409, 413, 415].map(status => ({ ...route, status }))))(
  "$method $resource classifies only the current valid HTTP $status rejection", async route => {
    transport.mockResolvedValueOnce(json({ error: { code: "future_rejection", message: "SECRET arbitrary server message" } }, route.status));
    const error = await failure(invoke(route));
    expect(error).toBeInstanceOf(ApiError);
    expect(error).toMatchObject({ status: route.status, code: "future_rejection", message: expect.stringMatching(/[\u4e00-\u9fff]/) });
    expect(error).toHaveProperty("message", expect.not.stringContaining("SECRET"));
    expect(isCoordinatorConfigurationDefinitiveRejection(error)).toBe(true);
    if (route.status === 409) {
      expect(error).toHaveProperty("message", expect.stringContaining("协调配置与当前版本或成员配置冲突"));
      expect(error).toHaveProperty("message", expect.not.stringMatching(/换.*键|更换|幂等键.*冲突/));
    }
    expect(transport).toHaveBeenCalledTimes(1);
  },
);
it.each(routes.flatMap(route => [401, 403, 405, 408, 410, 422, 429, 500, 502, 503, 504].map(status => ({ ...route, status }))))(
  "$method $resource conservatively leaves HTTP $status unknown", async route => {
    transport.mockResolvedValueOnce(json({ error: { code: "request_failed", message: "SECRET" } }, route.status));
    expectUnknown(await failure(invoke(route)), route.status, "request_failed");
    expect(transport).toHaveBeenCalledTimes(1);
  },
);
const invalidEnvelopes: [string, unknown][] = [
  ["null", null], ["array", []], ["missing error", {}], ["null error", { error: null }], ["array error", { error: [] }],
  ["string error", { error: "rejected" }], ["missing code", { error: { message: "SECRET" } }],
  ["empty code", { error: { code: "", message: "SECRET" } }], ["blank code", { error: { code: " ", message: "SECRET" } }],
  ["non-string code", { error: { code: 1, message: "SECRET" } }], ["missing message", { error: { code: "invalid_input" } }],
  ["null message", { error: { code: "invalid_input", message: null } }],
  ["blank message", { error: { code: "invalid_input", message: " " } }],
  ["broken Unicode", { error: { code: "invalid_input", message: "\ud800" } }],
  ["extra envelope field", { error: { code: "invalid_input", message: "SECRET" }, committed: true }],
  ["extra error field", { error: { code: "invalid_input", message: "SECRET", accepted: true } }],
];
it.each(invalidEnvelopes)("never trusts an invalid rejection envelope: %s", async (_name, envelope) => {
  for (const resource of resources) {
    for (const status of [400, 404, 409, 413, 415]) {
      transport.mockResolvedValueOnce(json(envelope, status));
      expectUnknown(await failure(resource.save(groupId, resource.input, key)), status, "invalid_response");
    }
  }
});
it.each(routes)("$method $resource keeps malformed JSON and body-read failures unknown", async route => {
  for (const status of [200, 400, 404, 409, 413, 415]) {
    transport.mockResolvedValueOnce(new Response("SECRET malformed JSON", { status }));
    expectUnknown(await failure(invoke(route)), status, "invalid_response");
  }
  transport.mockResolvedValueOnce(new Response(new ReadableStream({
    start(controller) { controller.error(new DOMException("SECRET interrupted body", "AbortError")); },
  }), { status: 200 }));
  expectUnknown(await failure(invoke(route)), 200, "invalid_response");
});
it.each(routes)("$method $resource converts disconnects and aborts to unknown ApiError without retry", async route => {
  transport.mockRejectedValueOnce(new TypeError("SECRET disconnected"));
  expectUnknown(await failure(invoke(route)), 0, "disconnected");
  transport.mockRejectedValueOnce(new DOMException("SECRET interrupted", "AbortError"));
  expectUnknown(await failure(invoke(route)), 0, "request_canceled");
  const controller = new AbortController();
  controller.abort();
  transport.mockRejectedValueOnce(new TypeError("SECRET aborted"));
  expectUnknown(await failure(invoke(route, controller.signal)), 0, "request_canceled");
  expect(transport).toHaveBeenCalledTimes(3);
});
it.each(resources)("$resource keeps its helper response-local across unknown, rejection and explicit same-key retry", async resource => {
  transport.mockRejectedValueOnce(new TypeError("offline"))
    .mockResolvedValueOnce(json({ error: { code: "conflict", message: "配置冲突" } }, 409))
    .mockResolvedValueOnce(json(resource.receipt));
  const unknown = await failure(resource.save(groupId, resource.input, key));
  expectUnknown(unknown, 0, "disconnected");
  const rejection = await failure(resource.save(groupId, resource.input, key));
  expect(isCoordinatorConfigurationDefinitiveRejection(rejection)).toBe(true);
  expect(isCoordinatorConfigurationDefinitiveRejection(unknown)).toBe(false);
  await expect(resource.save(groupId, resource.input, key)).resolves.toEqual(resource.receipt);
  expect(transport).toHaveBeenCalledTimes(3);
  for (const [, init] of transport.mock.calls) {
    expect(init?.body).toBe(JSON.stringify(resource.input));
    expect(new Headers(init?.headers).get("Idempotency-Key")).toBe(key);
  }
});
it("does not infer a trusted rejection from arbitrary errors or a reserved-looking server code", async () => {
  for (const error of [null, undefined, new Error("failed"), { status: 400, code: "invalid_input" }, new ApiError("未知", 0, "disconnected")]) {
    expect(isCoordinatorConfigurationDefinitiveRejection(error)).toBe(false);
  }
  transport.mockResolvedValueOnce(json({ error: { code: "invalid_response", message: "请求被拒绝" } }, 400));
  const error = await failure(setCoordinator(groupId, coordinatorInput, key));
  expect(error).toMatchObject({ status: 400, code: "invalid_response" });
  expect(isCoordinatorConfigurationDefinitiveRejection(error)).toBe(true);
});

type Observation = { path: string | undefined; method: string | undefined; body: string; key: string | string[] | undefined };
async function withRawResponse(status: number, body: string | Uint8Array, check: (requests: Observation[]) => Promise<void>) {
  const requests: Observation[] = [];
  const server = createServer((req, res) => {
    let sent = "";
    req.setEncoding("utf8");
    req.on("data", chunk => { sent += chunk; });
    req.on("end", () => {
      requests.push({ path: req.url, method: req.method, body: sent, key: req.headers["idempotency-key"] });
      res.writeHead(status, { "Content-Type": "application/json; charset=utf-8" }).end(body);
    });
  });
  try {
    server.listen(0, "127.0.0.1");
    await once(server, "listening");
    const address = server.address();
    if (!address || typeof address === "string" || [55594, 7310].includes(address.port)) throw new Error("Unexpected fixture address");
    const base = `http://127.0.0.1:${address.port}`;
    vi.stubGlobal("fetch", (url: RequestInfo | URL, init?: RequestInit) => nativeFetch(new URL(String(url), base), init));
    await check(requests);
  } finally {
    if (server.listening) await new Promise<void>((resolve, reject) => {
      server.close(error => error ? reject(error) : resolve());
      server.closeAllConnections();
    });
  }
}
it.each(routes.flatMap(route => [303, 307].map(status => ({ ...route, status })) ))(
  "$method $resource blocks native HTTP $status redirects and never forwards the key or body", async route => {
    const source: Observation[] = [], target: Observation[] = [];
    const effects: unknown[] = [];
    const path = `/api/v1/groups/${encodeURIComponent(groupId)}/coordination/${route.resource}`;
    const server = createServer((req, res) => {
      let body = "";
      req.setEncoding("utf8");
      req.on("data", chunk => { body += chunk; });
      req.on("end", () => {
        const observation = { path: req.url, method: req.method, body, key: req.headers["idempotency-key"] };
        if (req.url === path) {
          source.push(observation);
          if (source.length === 1) {
            if (route.method === "PUT") effects.push(route.receipt);
            res.writeHead(route.status, { Location: "/redirect-target" }).end();
            return;
          }
        } else {
          target.push(observation);
        }
        res.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify(route.receipt));
      });
    });
    try {
      server.listen(0, "127.0.0.1");
      await once(server, "listening");
      const address = server.address();
      if (!address || typeof address === "string" || [55594, 7310].includes(address.port)) throw new Error("Unexpected fixture address");
      const base = `http://127.0.0.1:${address.port}`;
      vi.stubGlobal("fetch", (url: RequestInfo | URL, init?: RequestInit) => nativeFetch(new URL(String(url), base), init));
      const error = await failure(invoke(route));
      const sent = { path, method: route.method, body: route.method === "PUT" ? JSON.stringify(route.input) : "",
        key: route.method === "PUT" ? key : undefined };
      expect(source).toEqual([sent]);
      expect.soft(target, "Redirect targets must receive neither requests nor save identities").toEqual([]);
      expect.soft(error).toBeInstanceOf(ApiError);
      expect.soft(error).toMatchObject({ status: 0, code: "disconnected", message: expect.stringContaining("结果未知") });
      expect(isCoordinatorConfigurationDefinitiveRejection(error)).toBe(false);
      await expect(invoke(route)).resolves.toEqual(route.receipt);
      expect(source).toEqual([sent, sent]);
      expect(effects).toEqual(route.method === "PUT" ? [route.receipt] : []);
      expect.soft(target).toEqual([]);
    } finally {
      if (server.listening) await new Promise<void>((resolve, reject) => {
        server.close(error => error ? reject(error) : resolve());
        server.closeAllConnections();
      });
    }
  },
);

function rawReceipt(resource: (typeof resources)[number], revision: string): string {
  const fields = resource.resource === "coordinator" ? `"member_id":${JSON.stringify(binding.member_id)}`
    : `"allowed_worker_ids":${JSON.stringify(policy.allowed_worker_ids)}`;
  // Insert the original numeric token, never a JSON.stringify-rounded number.
  return `{"group_id":${JSON.stringify(groupId)},${fields},"revision":${revision}}`;
}
const rawRevisionCases = [
  { revision: "1.0000000000000001", expected: 0 },
  { revision: "0.99999999999999999", expected: 0 },
  { revision: "9007199254740991.1", expected: Number.MAX_SAFE_INTEGER - 1 },
  { revision: "9007199254740990.9", expected: Number.MAX_SAFE_INTEGER - 1 },
  { revision: "1.0", expected: 0 },
  { revision: "1e0", expected: 0 },
  { revision: "10000000000000001e-16", expected: 0 },
  { revision: "9007199254740992", expected: Number.MAX_SAFE_INTEGER - 1 },
  { revision: "9223372036854775807", expected: Number.MAX_SAFE_INTEGER - 1 },
];
it.each(resources.flatMap(resource => rawRevisionCases.map(value => ({ ...resource, ...value })) ))(
  "$resource rejects raw revision $revision before lossy number conversion", async resource => {
    const raw = rawReceipt(resource, resource.revision);
    transport.mockResolvedValueOnce(new Response(raw, { status: 200 }));
    expectUnknown(await failure(resource.read(groupId)), 200, "invalid_response");
    transport.mockResolvedValueOnce(new Response(raw, { status: 200 }));
    expectUnknown(await failure(resource.save(groupId, { ...resource.input, expected_revision: resource.expected }, key)), 200, "invalid_response");
    expect(transport).toHaveBeenCalledTimes(2);
  },
);
it.each(["0.00000000000000000001", "1e-400", "-0", "0.0"])("rejects non-Go-integer raw default policy revision %s", async revision => {
  transport.mockResolvedValueOnce(new Response(`{"group_id":${JSON.stringify(groupId)},"revision":${revision},"allowed_worker_ids":[]}`));
  expectUnknown(await failure(getDispatchPolicy(groupId)), 200, "invalid_response");
});
it.each(resources.flatMap(resource => [rawRevisionCases[0], rawRevisionCases[2]].map(value => ({ ...resource, ...value })) ))(
  "$resource rejects raw HTTP revision $revision without confirming the write", async resource => {
    await withRawResponse(200, rawReceipt(resource, resource.revision), async requests => {
      const input = { ...resource.input, expected_revision: resource.expected };
      const error = await failure(resource.save(groupId, input, key));
      expect(requests).toEqual([{
        path: `/api/v1/groups/${encodeURIComponent(groupId)}/coordination/${resource.resource}`,
        method: "PUT", body: JSON.stringify(input), key,
      }]);
      expectUnknown(error, 200, "invalid_response");
    });
  },
);
it.each(resources.flatMap(resource => ["1", "9007199254740991"].map(revision => ({ ...resource, revision })) ))(
  "$resource accepts exact integer wire revision $revision even with an escaped property name", async resource => {
    const raw = rawReceipt(resource, resource.revision).replace('"revision"', '"revi\\u0073ion"');
    const receipt = { ...resource.receipt, revision: Number(resource.revision) };
    transport.mockResolvedValueOnce(new Response(raw));
    await expect(resource.read(groupId)).resolves.toEqual(receipt);
    transport.mockResolvedValueOnce(new Response(raw));
    await expect(resource.save(groupId, { ...resource.input, expected_revision: receipt.revision - 1 }, key)).resolves.toEqual(receipt);
  },
);
it("keeps numeric-looking string contents opaque, including escaped quotes, backslashes and valid U+FFFD", async () => {
  const id = 'id/1.0000000000000001/9007199254740991.1/1e400/"revision":1.0/\\123/\\u0031/�';
  const coordinator = { group_id: id, member_id: id, revision: 1 };
  const dispatch = { group_id: id, revision: 1, allowed_worker_ids: [id] };
  const rawBinding = `{"group_id":${JSON.stringify(id)},"member_id":${JSON.stringify(id)},"revision":1}`;
  const rawPolicy = `{"group_id":${JSON.stringify(id)},"revision":1,"allowed_worker_ids":[${JSON.stringify(id)}]}`;
  transport.mockResolvedValueOnce(new Response(rawBinding)).mockResolvedValueOnce(new Response(rawBinding))
    .mockResolvedValueOnce(new Response(rawPolicy)).mockResolvedValueOnce(new Response(rawPolicy));
  await expect(getCoordinator(id)).resolves.toEqual(coordinator);
  await expect(setCoordinator(id, { member_id: id, expected_revision: 0 }, key)).resolves.toEqual(coordinator);
  await expect(getDispatchPolicy(id)).resolves.toEqual(dispatch);
  await expect(setDispatchPolicy(id, { allowed_worker_ids: [id], expected_revision: 0 }, key)).resolves.toEqual(dispatch);
  const code = 'code/1.0000000000000001/9007199254740991.1/1e400/"revision":1/\\123';
  const rawError = `{"error":{"code":${JSON.stringify(code)},"message":${JSON.stringify(id)}}}`;
  transport.mockResolvedValueOnce(new Response(rawError, { status: 409 }));
  const error = await failure(setCoordinator(id, { member_id: id, expected_revision: 0 }, key));
  expect(error).toMatchObject({ status: 409, code });
  expect(isCoordinatorConfigurationDefinitiveRejection(error)).toBe(true);
});

function invalidUtf8Error(bytes: readonly number[]): Buffer<ArrayBuffer> {
  return Buffer.concat([Buffer.from('{"error":{"code":"conflict","message":"'), Buffer.from(bytes), Buffer.from('"}}')]);
}
const malformedUtf8 = [
  [0xff], [0x80], [0xc0, 0xaf], [0xed, 0xa0, 0x80], [0xe2, 0x82], [0xe2, 0x28, 0xa1], [0xf4, 0x90, 0x80, 0x80],
];
it.each(routes.flatMap(route => [400, 404, 409, 413, 415, 500].map(status => ({ ...route, status })) ))(
  "$method $resource rejects malformed UTF-8 at HTTP $status before trusting any envelope", async route => {
    for (const bytes of malformedUtf8) {
      transport.mockResolvedValueOnce(new Response(invalidUtf8Error(bytes), { status: route.status }));
      expectUnknown(await failure(invoke(route)), route.status, "invalid_response");
    }
    expect(transport).toHaveBeenCalledTimes(malformedUtf8.length);
  },
);
it.each(resources.flatMap(resource => [400, 404, 409].map(status => ({ ...resource, status })) ))(
  "$resource keeps raw HTTP $status with byte ff unknown", async resource => {
    await withRawResponse(resource.status, invalidUtf8Error([0xff]), async requests => {
      const error = await failure(resource.save(groupId, resource.input, key));
      expect(requests).toHaveLength(1);
      expect(requests[0]).toMatchObject({ method: "PUT", body: JSON.stringify(resource.input), key });
      expectUnknown(error, resource.status, "invalid_response");
    });
  },
);
it.each(routes)("$method $resource rejects a corrupted 200 identity rather than accepting UTF-8 replacement", async route => {
  const raw = rawReceipt(route, String(route.receipt.revision));
  const expectedGroup = `group-${String.fromCharCode(0xfffd)}`;
  const bytes = Buffer.from(raw.replace(JSON.stringify(groupId), '"group-!"'));
  bytes[bytes.indexOf(Buffer.from("group-!")) + 6] = 0xff;
  transport.mockResolvedValueOnce(new Response(bytes));
  const operation = route.method === "GET" ? route.read(expectedGroup) : route.save(expectedGroup, route.input, key);
  expectUnknown(await failure(operation), 200, "invalid_response");
});

it.each(routes.flatMap(route => [".", ".."].map(group => ({ ...route, group })) ))(
  "$method $resource rejects exact dot group $group before native fetch can normalize its path", async route => {
    await withRawResponse(404, '{"error":{"code":"not_found","message":"记录不存在或不可访问"}}', async requests => {
      const operation = route.method === "GET" ? route.read(route.group) : route.save(route.group, route.input, key);
      const error = await failure(operation);
      expect.soft(requests, "Local dot-group rejection must send no HTTP request").toEqual([]);
      expectLocalRejection(error);
    });
  },
);
it.each(resources.flatMap(resource => ["%2E", "%2e%2e", "a.b", "...", "a/../b"].map(group => ({ ...resource, group })) ))(
  "$resource preserves addressable group $group on the actual native HTTP path", async resource => {
    const receipt = { ...resource.receipt, group_id: resource.group };
    await withRawResponse(200, JSON.stringify(receipt), async requests => {
      await expect(resource.read(resource.group)).resolves.toEqual(receipt);
      await expect(resource.save(resource.group, resource.input, key)).resolves.toEqual(receipt);
      expect(requests.map(request => request.path)).toEqual(Array(2).fill(
        `/api/v1/groups/${encodeURIComponent(resource.group)}/coordination/${resource.resource}`,
      ));
      expect(requests.map(request => request.method)).toEqual(["GET", "PUT"]);
    });
  },
);
it.each([".", ".."])('keeps non-path member ID "%s" under the existing domain validator', async member_id => {
  const coordinator = { ...binding, member_id, revision: 1 };
  const dispatch = { ...policy, allowed_worker_ids: [member_id], revision: 1 };
  transport.mockResolvedValueOnce(json(coordinator)).mockResolvedValueOnce(json(coordinator))
    .mockResolvedValueOnce(json(dispatch)).mockResolvedValueOnce(json(dispatch));
  await expect(getCoordinator(groupId)).resolves.toEqual(coordinator);
  await expect(setCoordinator(groupId, { member_id, expected_revision: 0 }, key)).resolves.toEqual(coordinator);
  await expect(getDispatchPolicy(groupId)).resolves.toEqual(dispatch);
  await expect(setDispatchPolicy(groupId, { allowed_worker_ids: [member_id], expected_revision: 0 }, key)).resolves.toEqual(dispatch);
});
