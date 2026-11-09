import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtemp, readdir, rm } from "node:fs/promises";
import { Socket } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { createClient } from "../bridge.mjs";
import { loadExtensions, loadSessionSDK } from "./pi-loader.mjs";
import { BASE, BUDGET, TOKEN, json, loopback, privateConfig, safeFailure } from "./helpers.mjs";

const entry = fileURLToPath(new URL("../index.ts", import.meta.url));
const ordinary = ["list_members", "read_channel", "send_message", "create_member", "search_history", "read_history"];
const roleNames = ["decide_dispatch", "submit_result"];
const catalog = [...ordinary, ...roleNames];
const profiles = {
  coordinator: ["list_members", "read_channel", "search_history", "read_history", "decide_dispatch"],
  worker: ["read_channel", "search_history", "read_history", "submit_result"],
};
const payloads = {
  send_message: { content: "fixture message", recipient_ids: [] },
  create_member: { name: "成员", role: "校对", reason: "检查" },
  decide_dispatch: { kind: "assign", reason: "  分派理由\n保留原文  ", worker_member_id: "worker" },
  submit_result: { summary: "  完成摘要\n保留原文  ", artifact_versions: [{ artifact_id: "artifact", version_id: "version" }] },
};
const decision = { decision_id: "decision", kind: "assign", assignment_id: "assignment", created_at: "2026-11-01T00:00:00.123456789Z" };
const submission = { submission_id: "submission", created_at: "2026-11-01T08:00:00+08:00", artifact_version_count: 1, summary_omitted: true };
const page = { items: [], next_cursor: "" };
const writeNames = Object.keys(payloads);
const readPayloads = { list_members: {}, read_channel: {}, search_history: { query: "q" }, read_history: { message_id: "m" } };

function nameOf(request) {
  return request.url.slice(BASE.length + 1);
}

function receipt(name, params) {
  if (name === "decide_dispatch") return { ...decision, kind: params.kind, assignment_id: params.kind === "assign" ? "assignment" : "" };
  if (name === "submit_result") return { ...submission, artifact_version_count: params.artifact_versions?.length || 0 };
  if (name === "send_message") return { message_id: "message", created_at: decision.created_at, delivery_count: 0, content_omitted: true };
  if (name === "create_member") return { member_id: "member", reused: false, join_message_id: "join" };
  return page;
}

function success(request, response, body) {
  const name = nameOf(request);
  json(response, receipt(name, JSON.parse(body)), writeNames.includes(name) ? 201 : 200);
}

async function clientFor(t, handler = success) {
  const server = await loopback(t, handler);
  return { ...server, invoke: createClient(await privateConfig(t, { endpoint: server.endpoint })) };
}

async function load(t, endpoint = `http://127.0.0.1:1${BASE}`) {
  const config = await privateConfig(t, { endpoint });
  const previous = process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG;
  try {
    process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG = config;
    const result = await loadExtensions([entry], process.cwd());
    assert.deepEqual(result.errors, []);
    assert.deepEqual(result.warnings, []);
    assert.equal(result.extensions.length, 1);
    return result;
  } finally {
    if (previous === undefined) delete process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG;
    else process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG = previous;
  }
}

function definitions(result) {
  return new Map([...result.extensions[0].tools].map(([name, tool]) => [name, tool.definition]));
}

async function toolsFor(t, handler = success) {
  const server = await loopback(t, handler);
  const tools = definitions(await load(t, server.endpoint));
  return { ...server, tools, invoke: (name, params, id, signal) => execute(tools, name, params, id, signal) };
}

function execute(tools, name, params, id = "native-role", signal) {
  const tool = tools.get(name);
  assert.ok(tool, `${name} must be registered by the real loader`);
  return tool.execute(id, params, signal, () => assert.fail("no streaming output expected"), {
    caller_id: "untrusted", group_id: "untrusted", task_id: "untrusted", attempt_id: "untrusted",
    revision: 999, grants: ["*"], token: TOKEN,
  });
}

async function unknown(pending) {
  let original;
  await assert.rejects(pending, error => {
    safeFailure(error, /结果未知.*对账/);
    assert.match(error.message, /不要自动重试/);
    original = error;
    return true;
  });
  return original;
}

async function cached(server, original, name, params = payloads[name], id = "original") {
  const before = server.requests.length;
  await assert.rejects(server.invoke(name, params, id), error => {
    assert.equal(error, original, "identical sent bytes must return the first cached unknown error");
    return true;
  });
  assert.equal(server.requests.length, before, "cached uncertainty must perform zero HTTP");
}

async function latched(server, original, name, params = payloads[name], id = "original") {
  await cached(server, original, name, params, id);
  const before = server.requests.length;
  const changed = name === "submit_result" ? { ...params, summary: "changed" } :
    name === "send_message" ? { ...params, content: "changed" } : { ...params, reason: "changed" };
  for (const [tool, payload, callId] of [
    [name, params, "new-native-id"], [name, changed, id],
    [name, Object.fromEntries(Object.entries(params).reverse()), id],
    ...writeNames.filter(tool => tool !== name).map(tool => [tool, payloads[tool], id]),
  ]) {
    await assert.rejects(server.invoke(tool, payload, callId), error => safeFailure(error, /结果未知.*对账/));
  }
  assert.equal(server.requests.length, before, "all four writes must share the same fuse");
  for (const [tool, payload] of Object.entries(readPayloads)) {
    const result = await server.invoke(tool, payload, `read-${tool}`);
    assert.deepEqual(result.json ?? JSON.parse(result.content[0].text), page);
    assert.equal(server.requests.at(-1).headers["idempotency-key"], undefined);
  }
  await cached(server, original, name, params, id);
  assert.equal(server.requests.length, before + 4, "only the four reads may reach HTTP");
}

test("schema/profile: real loader exposes object role schemas without identity or configuration selectors", async t => {
  const tools = definitions(await load(t));
  for (const name of roleNames) {
    const tool = tools.get(name);
    assert.ok(tool, `${name} must be registered`);
    assert.match(tool.label, /[\u3400-\u9fff]/u);
    assert.match(tool.description, /[\u3400-\u9fff]/u);
    assert.equal(tool.parameters.type, "object", "Pi requires a top-level object schema");
    assert.equal(tool.parameters.additionalProperties, false);
    assert.ok(!JSON.stringify(tool).includes(TOKEN));
  }
  const decide = tools.get("decide_dispatch").parameters;
  assert.deepEqual(Object.keys(decide.properties).sort(), ["kind", "reason", "worker_member_id"]);
  assert.deepEqual([...decide.required].sort(), ["kind", "reason"]);
  assert.equal(decide.properties.reason.type, "string");
  assert.equal(decide.properties.worker_member_id.type, "string");
  const submit = tools.get("submit_result").parameters;
  assert.deepEqual(Object.keys(submit.properties).sort(), ["artifact_versions", "summary"]);
  assert.deepEqual(submit.required, ["summary"]);
  assert.equal(submit.properties.summary.type, "string");
  assert.equal(submit.properties.artifact_versions.type, "array");
  const ref = submit.properties.artifact_versions.items;
  assert.equal(ref.type, "object");
  assert.equal(ref.additionalProperties, false);
  assert.deepEqual(Object.keys(ref.properties).sort(), ["artifact_id", "version_id"]);
  assert.deepEqual([...ref.required].sort(), ["artifact_id", "version_id"]);
  for (const field of Object.values(ref.properties)) assert.equal(field.type, "string");
});

for (const [role, profile] of Object.entries(profiles)) {
  test(`schema/profile: real Pi 1.4.0 SDK clamps ${role} to exact ${profile.length} active and callable tools without a prompt`, async t => {
    const extensions = await load(t);
    const { createAgentSession, SessionManager, SettingsManager } = await loadSessionSDK();
    const directory = await mkdtemp(join(tmpdir(), "pi-role-session-"));
    t.after(() => rm(directory, { recursive: true, force: true }));
    const forbiddenCalls = [];
    const deny = name => () => { forbiddenCalls.push(name); assert.fail(`forbidden no-prompt fixture call: ${name}`); };
    t.mock.method(globalThis, "fetch", deny("network fetch"));
    t.mock.method(Socket.prototype, "connect", deny("network socket"));
    const modelRuntime = new Proxy({}, { get: (_target, name) => deny(`modelRuntime.${String(name)}`) });
    const model = { id: "synthetic-no-call", name: "Synthetic no-call model", provider: "synthetic",
      api: "synthetic-no-network", baseUrl: "http://127.0.0.1:1", reasoning: false, input: ["text"],
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 8192, maxTokens: 1 };
    const resourceLoader = {
      getExtensions: () => extensions,
      getSkills: () => ({ skills: [], diagnostics: [] }),
      getPrompts: () => ({ prompts: [], diagnostics: [] }),
      getThemes: () => ({ themes: [], diagnostics: [] }),
      getAgentsFiles: () => ({ agentsFiles: [] }),
      getSystemPrompt: () => "Synthetic no-prompt fixture.",
      getSystemPromptSource: () => undefined,
      getAppendSystemPrompt: () => [], getAppendSystemPromptSources: () => [],
      extendResources: deny("resource discovery"), reload: deny("resource reload"),
    };
    const { session, modelFallbackMessage } = await createAgentSession({
      cwd: directory, agentDir: directory, model, modelRuntime, resourceLoader, thinkingLevel: "off",
      sessionManager: SessionManager.inMemory(directory),
      settingsManager: SettingsManager.inMemory({ defaultTools: ["read", "bash", "edit", "write"],
        compaction: { enabled: false }, retry: { enabled: false }, cacheWarming: "off", enableInstallTelemetry: false }),
      noTools: "builtin", tools: [...profile],
      customTools: ["ungranted", "create_task"].map(name => ({ name, label: name, description: name,
        parameters: { type: "object", properties: {}, additionalProperties: false }, execute: deny(name) })),
    });
    try {
      session.agent.streamFn = deny("agent stream");
      session.agent.prompt = deny("agent prompt");
      session.prompt = deny("session prompt");
      await session.bindExtensions({ mode: "print" });
      assert.equal(modelFallbackMessage, undefined);
      assert.equal(session.model, model);
      assert.deepEqual(session.getActiveToolNames(), profile);
      assert.deepEqual(session.getCallableToolNames(), profile);
      const ungranted = ["read", "bash", "powershell", "edit", "write", "grep", "find", "ls", "codemode", "tool_search", "ungranted", "create_task"];
      session.setActiveToolsByName([...profile, ...catalog.filter(name => !profile.includes(name)), ...ungranted]);
      assert.deepEqual(session.getActiveToolNames(), profile, "model declarations must remain clamped");
      assert.deepEqual(session.getCallableToolNames(), profile, "nested callable tools must remain clamped");
      assert.deepEqual(session.messages, []);
      assert.equal(session.isStreaming, false);
    } finally {
      session.dispose();
    }
    assert.deepEqual(forbiddenCalls, [], "no model, auth, network, discovery or prompt may run");
    assert.deepEqual(await readdir(directory), [], "session/settings/auth/model files must not be created");
  });
}

test("schema/profile: role native execute preserves exact request bytes and reconstructed same-key fixture replay", async t => {
  // This is protocol-fixture replay, not Store or role-token integration.
  const saved = new Map();
  let effects = 0;
  const server = await toolsFor(t, (request, response, body) => {
    const key = request.headers["idempotency-key"];
    if (!saved.has(key)) {
      effects++;
      saved.set(key, { body: Buffer.from(body), text: ` ${JSON.stringify(receipt(nameOf(request), JSON.parse(body)))}\n` });
    }
    assert.deepEqual(body, saved.get(key).body, "same key must carry the original payload bytes");
    response.writeHead(201, { "Content-Type": "application/json" });
    response.end(saved.get(key).text);
  });
  for (const name of roleNames) {
    for (let i = 0; i < 2; i++) {
      const result = await execute(server.tools, name, payloads[name], "shared-native-id");
      const request = server.requests.at(-1);
      const key = createHash("sha256").update(name + "shared-native-id").digest("hex");
      assert.equal(request.method, "POST");
      assert.equal(request.url, `${BASE}/${name}`);
      assert.equal(request.body.toString(), JSON.stringify(payloads[name]), "do not trim or rewrite payload bytes");
      assert.equal(request.headers.authorization, `Bearer ${TOKEN}`);
      assert.equal(request.headers["content-type"], "application/json");
      assert.equal(request.headers["content-length"], String(request.body.length));
      assert.equal(request.headers["idempotency-key"], key);
      const headers = request.rawHeaders.filter((_, index) => index % 2 === 0).map(header => header.toLowerCase());
      for (const header of ["idempotency-key", "authorization", "content-type"]) assert.equal(headers.filter(value => value === header).length, 1);
      assert.ok(headers.every(header => header !== "origin" && !header.startsWith("sec-fetch-")));
      assert.deepEqual(result, { content: [{ type: "text", text: JSON.stringify(JSON.parse(saved.get(key).text)) }], details: undefined });
    }
  }
  for (const [name, params] of [
    ["decide_dispatch", { kind: "decline", reason: "" }],
    ["submit_result", { summary: "" }], ["submit_result", { summary: "ok", artifact_versions: [] }],
  ]) {
    await execute(server.tools, name, params, `optional-${effects}`);
    assert.equal(server.requests.at(-1).body.toString(), JSON.stringify(params));
  }
  assert.equal(server.requests.length, 7, "confirmed successes may be explicitly invoked again");
  assert.equal(effects, 5, "same native IDs and payloads replay the fixture receipts");
});

test("schema/profile: invalid role inputs and unsafe snapshots never reach HTTP or poison later writes", async t => {
  const server = await toolsFor(t);
  const reserved = ["caller", "caller_id", "caller_member_id", "member_id", "group", "group_id", "task", "task_id",
    "attempt", "attempt_id", "revision", "observed_task_revision", "grants", "key", "idempotency_key", "profile", "endpoint", "token", "toolCallId"];
  const invalid = {
    decide_dispatch: [null, [], "text", 1, {}, { kind: "assign", worker_member_id: "worker" },
      { kind: "assign", reason: "ok" }, { kind: "assign", reason: "ok", worker_member_id: "" },
      { kind: "assign", reason: null, worker_member_id: "worker" }, { kind: "assign", reason: "ok", worker_member_id: null },
      { kind: "assign", reason: "ok", worker_member_id: undefined }, { kind: "review", reason: "ok" },
      { kind: "decline", reason: "ok", worker_member_id: "" }, { kind: "decline", reason: "ok", worker_member_id: "worker" },
      { kind: "decline", reason: 1 }, { kind: "decline", reason: undefined }],
    submit_result: [null, [], "text", 1, {}, { summary: null }, { summary: 1 }, { summary: undefined },
      ...[null, undefined, "refs", {}, [null], [undefined], [{}], [{ artifact_id: "a" }],
        [{ artifact_id: "a", version_id: null }], [{ artifact_id: 1, version_id: "v" }],
        [{ artifact_id: "a", version_id: "v", task_id: "untrusted" }]].map(artifact_versions => ({ summary: "ok", artifact_versions }))],
  };
  for (const name of roleNames) {
    for (const params of [...invalid[name], ...reserved.map(field => ({ ...payloads[name], [field]: "untrusted" }))]) {
      await assert.rejects(execute(server.tools, name, params), error => safeFailure(error, /参数.*未发送/));
    }
  }
  const refs = [{ artifact_id: "a", version_id: "v" }];
  refs.toJSON = () => [{ artifact_id: "a", version_id: 1 }];
  await assert.rejects(execute(server.tools, "submit_result", { summary: "ok", artifact_versions: refs }), error => safeFailure(error, /参数.*未发送/));
  let accesses = 0;
  await assert.rejects(execute(server.tools, "submit_result", { get summary() { return ++accesses === 1 ? "ok" : 1; } }),
    error => safeFailure(error, /参数.*未发送/));
  await assert.rejects(execute(server.tools, "decide_dispatch", { kind: "decline", get reason() { throw new Error(TOKEN); } }),
    error => safeFailure(error, /参数.*未发送/));
  const transformed = Object.defineProperty({ ...payloads.decide_dispatch }, "toJSON", {
    value: () => ({ kind: "decline", reason: "ok", worker_member_id: "" }),
  });
  await assert.rejects(execute(server.tools, "decide_dispatch", transformed), error => safeFailure(error, /参数.*未发送/));
  assert.equal(server.requests.length, 0);
  for (const name of roleNames) await execute(server.tools, name, payloads[name]);
  assert.equal(server.requests.length, 2, "local input rejection must not latch the shared write fuse");
});

test("receipt: fixed role receipts accept real calendar timestamps and safe boundary counts", async t => {
  const cases = [
    ["decide_dispatch", payloads.decide_dispatch, { ...decision, created_at: "2000-02-29T00:30:00.123456789+05:45" }],
    ["decide_dispatch", { kind: "decline", reason: "ok" }, { ...decision, kind: "decline", assignment_id: "", created_at: "0096-02-29T23:30:00.1-01:00" }],
    ["submit_result", payloads.submit_result, { ...submission, artifact_version_count: 0 }],
    ["submit_result", payloads.submit_result, { ...submission, artifact_version_count: Number.MAX_SAFE_INTEGER, created_at: "0000-02-29T23:30:00.123456789-01:00" }],
  ];
  let index = 0;
  const server = await clientFor(t, (_, response) => json(response, cases[index++][2], 201));
  for (const [name, params, expected] of cases) assert.deepEqual((await server.invoke(name, params, `valid-${index}`)).json, expected);
  assert.equal(server.requests.length, cases.length);
});

test("receipt: role wire reconstruction accepts unique escaped keys and punctuation in strings", async t => {
  for (const [name, key, wireKey, value] of [
    ["decide_dispatch", "decision_id", "decision_\\u0069d", { ...decision, decision_id: 'decision\\",}:true' }],
    ["submit_result", "submission_id", "\\u0073ubmission_id", { ...submission, submission_id: 'submission\\",}:false' }],
  ]) {
    await t.test(name, async t => {
      const text = ` \t${JSON.stringify(value).replace(`"${key}"`, `"${wireKey}"`)}\r\n`;
      const server = await toolsFor(t, (_, response) => {
        response.writeHead(201, { "Content-Type": "application/json" });
        response.end(text);
      });
      const result = await server.invoke(name, payloads[name], "escaped-receipt");
      assert.deepEqual(result, { content: [{ type: "text", text: JSON.stringify(value) }], details: undefined });
      assert.equal(server.requests.length, 1);
    });
  }
});

test("receipt: role raw-wire duplicates and non-integer count tokens are unknown and share the fuse", async t => {
  const marker = "SYNTHETIC_PRIVATE_ROLE_VALUE";
  const submitText = JSON.stringify(submission);
  const cases = [
    ["submit_result", "overwritten private id and omission flag", `{"submission_id":"${marker}",${submitText.slice(1).replace('"summary_omitted":true', '"summary_omitted":false,"summary_omitted":true')}`],
    ["submit_result", "escaped duplicate omission flag", submitText.replace('"summary_omitted":true', '"summary_omitted":true,"summary_omit\\u0074ed":true')],
    ["decide_dispatch", "escaped duplicate decision id", `{"decision_\\u0069d":"${marker}",${JSON.stringify(decision).slice(1)}`],
    ...["-1e-324", "1e-324", "-0", "1.0000000000000001", "9007199254740991.1", "1.0", "1e0"].map(token =>
      ["submit_result", `count token ${token}`, submitText.replace('"artifact_version_count":1', `"artifact_version_count":${token}`)]),
  ];
  for (const [name, label, text] of cases) {
    await t.test(label, async t => {
      const server = await toolsFor(t, (request, response, body) => {
        if (server.requests.length !== 1) return success(request, response, body);
        response.writeHead(201, { "Content-Type": "application/json" });
        response.end(text);
      });
      const original = await unknown(server.invoke(name, payloads[name], "original"));
      safeFailure(original, /结果未知.*对账/, [TOKEN, marker]);
      await latched(server, original, name);
    });
  }
});

test("receipt: invalid, mismatched, additive or sensitive role success is unknown and cached", async t => {
  const cases = [
    ...roleNames.flatMap(name => [200, 202].map(status => [name, status, name === "decide_dispatch" ? decision : submission])),
    ["decide_dispatch", 204, decision], ["submit_result", 201, null],
    ...Object.keys(decision).map(field => ["decide_dispatch", 201, Object.fromEntries(Object.entries(decision).filter(([key]) => key !== field))]),
    ...[{ decision_id: "" }, { decision_id: 1 }, { kind: "review" }, { assignment_id: "" }, { assignment_id: null },
      { kind: "decline", assignment_id: "" }, { extra: true }, { reason: "private reason" }, { caller_id: "private caller" },
      { created_at: "2026-02-30T00:00:00Z" }, { created_at: "1900-02-29T00:00:00Z" },
      { created_at: "2026-11-01T24:00:00Z" }, { created_at: "2026-11-01T00:00:00+24:00" },
      { echo: TOKEN }].map(change => ["decide_dispatch", 201, { ...decision, ...change }]),
    ["decide_dispatch", 201, decision, { kind: "decline", reason: "ok" }],
    ["decide_dispatch", 201, { ...decision, kind: "decline", assignment_id: "not-empty" }, { kind: "decline", reason: "ok" }],
    ...Object.keys(submission).map(field => ["submit_result", 201, Object.fromEntries(Object.entries(submission).filter(([key]) => key !== field))]),
    ...[{ submission_id: "" }, { submission_id: null }, { artifact_version_count: -1 }, { artifact_version_count: 0.5 },
      { artifact_version_count: Number.MAX_SAFE_INTEGER + 1 }, { artifact_version_count: null },
      { summary_omitted: false }, { summary_omitted: "true" }, { created_at: "2025-02-29T00:00:00Z" },
      { extra: null }, { summary: "private summary" }, { artifact_versions: [] }, { token: TOKEN }]
      .map(change => ["submit_result", 201, { ...submission, ...change }]),
    ["submit_result", 201, { review_id: "not-a-submission", created_at: submission.created_at, artifact_version_count: 1, summary_omitted: true }],
  ];
  for (const [index, [name, status, value, params = payloads[name]]] of cases.entries()) {
    await t.test(`${name} invalid receipt ${index}`, async t => {
      const server = await clientFor(t, (request, response, body) => {
        if (server.requests.length === 1) json(response, value, status);
        else success(request, response, body);
      });
      const original = await unknown(server.invoke(name, params, "original"));
      await cached(server, original, name, params);
      const other = name === "decide_dispatch" ? "submit_result" : "decide_dispatch";
      await assert.rejects(server.invoke(other, payloads[other], "other-write"), error => safeFailure(error, /结果未知.*对账/));
      assert.equal(server.requests.length, 1);
    });
  }
});

for (const name of writeNames) {
  test(`uncertainty: shared latch from ${name} blocks every old/new write but not reads`, async t => {
    const server = await clientFor(t, (request, response, body) => {
      if (server.requests.length === 1) request.socket.destroy();
      else success(request, response, body);
    });
    const original = await unknown(server.invoke(name, payloads[name], "original"));
    await latched(server, original, name);
  });
}

test("uncertainty: Decide Submit-only status/code pairs latch at the native HTTP boundary", async t => {
  for (const [status, code, message] of [
    [409, "conflict", "任意文案"],
    [404, "not_found", "已确认拒绝，可更换幂等键"],
  ]) {
    await t.test(`${status}/${code}`, async t => {
      const server = await toolsFor(t, (request, response, body) => {
        if (server.requests.length === 1) json(response, { error: { code, message } }, status);
        else success(request, response, body);
      });
      const original = await server.invoke("decide_dispatch", payloads.decide_dispatch, "original").catch(error => error);
      const next = await server.invoke("send_message", payloads.send_message, "must-not-send").catch(error => error);
      assert.equal(server.requests.length, 1, "an unguaranteed Decide rejection must block the next HTTP write");
      safeFailure(original, /结果未知.*对账/);
      safeFailure(next, /结果未知.*对账/);
      await latched(server, original, "decide_dispatch");
    });
  }
});

test("uncertainty: frozen role status/code table permits only guaranteed rejections and retains the key", async t => {
  const rejected = [
    ["decide_dispatch", 400, "invalid_json"], ["decide_dispatch", 400, "invalid_input"],
    ["submit_result", 403, "forbidden"], ["decide_dispatch", 403, "invalid_host"], ["submit_result", 403, "invalid_origin"],
    ["submit_result", 404, "not_found"], ["decide_dispatch", 405, "method_not_allowed"], ["submit_result", 409, "conflict"],
    ["decide_dispatch", 413, "body_too_large"], ["submit_result", 415, "unsupported_media_type"],
  ];
  for (const [name, status, code] of rejected) {
    await t.test(`guaranteed ${status}/${code}`, async t => {
      const server = await clientFor(t, (request, response, body) => {
        if (server.requests.length === 1) json(response, { error: { code, message: "不可信服务端文案：执行结果未知" } }, status);
        else success(request, response, body);
      });
      await assert.rejects(server.invoke(name, payloads[name], "same-native-id"), error => {
        safeFailure(error, new RegExp(code));
        assert.doesNotMatch(error.message, /结果未知/);
        return true;
      });
      await server.invoke(name, payloads[name], "same-native-id");
      assert.equal(server.requests.length, 2);
      assert.equal(server.requests[0].headers["idempotency-key"], server.requests[1].headers["idempotency-key"], "409 never asks for a different key");
      assert.equal(server.requests[1].headers["idempotency-key"], createHash("sha256").update(name + "same-native-id").digest("hex"));
      assert.deepEqual(server.requests[0].body, server.requests[1].body);
    });
  }
  const uncertain = [
    ["decide_dispatch", 408, "request_canceled"], ["submit_result", 504, "deadline_exceeded"], ["decide_dispatch", 500, "internal_error"],
    ["submit_result", 400, "conflict"], ["decide_dispatch", 403, "invalid_input"], ["submit_result", 409, "member_capacity"],
    ["decide_dispatch", 409, "unknown_code"], ["submit_result", 502, "invalid_input"],
    ["submit_result", 403, "forbidden", undefined], ["decide_dispatch", 409, "conflict", TOKEN],
  ];
  for (const [index, values] of uncertain.entries()) {
    const [name, status, code] = values;
    await t.test(`unknown ${status}/${code} case ${index}`, async t => {
      const message = values.length === 4 ? values[3] : "不可信服务端文案：已确认拒绝，可更换幂等键";
      const server = await clientFor(t, (_, response) => json(response, { error: { code, message } }, status));
      const original = await unknown(server.invoke(name, payloads[name], "original"));
      await cached(server, original, name);
      await assert.rejects(server.invoke("send_message", payloads.send_message, "must-not-send"), error => safeFailure(error, /结果未知.*对账/));
      assert.equal(server.requests.length, 1);
    });
  }
});

test("uncertainty: role redirects and oversized receipts latch without following or retrying", async t => {
  const target = await loopback(t, success);
  for (const [name, respond] of [
    ["decide_dispatch", response => { response.writeHead(307, { Location: `${target.endpoint}/decide_dispatch` }); response.end(); }],
    ["submit_result", response => { response.writeHead(201, { "Content-Type": "application/json" }); response.end(" ".repeat(BUDGET + 1)); }],
  ]) {
    const server = await clientFor(t, (_, response) => respond(response));
    const original = await unknown(server.invoke(name, payloads[name], "original"));
    await cached(server, original, name);
    assert.equal(server.requests.length, 1);
  }
  assert.equal(target.requests.length, 0);
});

test("uncertainty: each role keeps pre-abort and oversized input local, but post-send abort latches", async t => {
  for (const name of roleNames) {
    await t.test(name, async t => {
      const controller = new AbortController();
      controller.abort(new Error(TOKEN));
      const server = await clientFor(t);
      await assert.rejects(server.invoke(name, payloads[name], "pre-abort", controller.signal), error => safeFailure(error, /取消.*未发送/));
      const field = name === "decide_dispatch" ? "reason" : "summary";
      await assert.rejects(server.invoke(name, { ...payloads[name], [field]: "中".repeat(BUDGET) }, "oversized"), error => safeFailure(error, /请求.*64 KiB.*未发送/));
      assert.equal(server.requests.length, 0);
      await server.invoke(name, payloads[name], "valid-after-local-failures");
      assert.equal(server.requests.length, 1);
      const live = new AbortController();
      const aborting = await clientFor(t, (request, response, body) => {
        if (aborting.requests.length === 1) live.abort(new Error(TOKEN));
        else success(request, response, body);
      });
      const original = await unknown(aborting.invoke(name, payloads[name], "original", live.signal));
      await latched(aborting, original, name);
    });
  }
});

test("receipt: decision kind and the unknown fingerprint use sent snapshots, not later mutable arguments", async t => {
  await t.test("decision success validates the sent kind", async t => {
    let received;
    const arrived = new Promise(resolve => { received = resolve; });
    const server = await clientFor(t, (request, response, body) => received({ request, response, body }));
    const params = { ...payloads.decide_dispatch };
    const pending = server.invoke("decide_dispatch", params, "sent-kind");
    const held = await arrived;
    params.kind = "decline";
    delete params.worker_member_id;
    success(held.request, held.response, held.body);
    assert.deepEqual((await pending).json, decision);
    assert.equal(server.requests[0].body.toString(), JSON.stringify(payloads.decide_dispatch));
  });
  await t.test("submission uncertainty caches the original nested bytes", async t => {
    let received;
    const arrived = new Promise(resolve => { received = resolve; });
    const server = await clientFor(t, (request, response, body) => {
      if (server.requests.length === 1) received(request);
      else success(request, response, body);
    });
    const params = structuredClone(payloads.submit_result);
    const snapshot = structuredClone(params);
    const pending = unknown(server.invoke("submit_result", params, "original"));
    const held = await arrived;
    params.summary = "changed";
    params.artifact_versions[0].version_id = "changed-version";
    held.socket.destroy();
    const original = await pending;
    assert.equal(server.requests[0].body.toString(), JSON.stringify(snapshot));
    await assert.rejects(server.invoke("submit_result", params, "original"), error => safeFailure(error, /结果未知.*对账/));
    await latched(server, original, "submit_result", snapshot);
  });
});

test("uncertainty: concurrent old/new completions never clear or replace the first unknown latch", { timeout: 5000 }, async t => {
  for (const names of [
    ["decide_dispatch", "send_message", "create_member", "submit_result"],
    ["create_member", "submit_result", "decide_dispatch", "send_message"],
  ]) {
    await t.test(`first unknown ${names[0]}`, async t => {
      const held = new Map();
      let received;
      const arrived = new Promise(resolve => { received = resolve; });
      const server = await clientFor(t, (request, response, body) => {
        if (!writeNames.includes(nameOf(request))) return success(request, response, body);
        held.set(nameOf(request), { request, response, body });
        if (held.size === 4) received();
      });
      const pending = names.map((name, index) => server.invoke(name, payloads[name], index === 0 ? "original" : `concurrent-${index}`)
        .then(value => ({ value }), error => ({ error })));
      await arrived;
      assert.equal(server.requests.length, 4, "already-sent writes must not be serialized");
      held.get(names[0]).request.socket.destroy();
      const original = (await pending[0]).error;
      safeFailure(original, /结果未知.*对账/);
      await cached(server, original, names[0]);
      const completed = held.get(names[1]);
      success(completed.request, completed.response, completed.body);
      assert.deepEqual((await pending[1]).value.json, receipt(names[1], payloads[names[1]]));
      await cached(server, original, names[0]);
      json(held.get(names[2]).response, { error: { code: "invalid_input", message: "夹具拒绝" } }, 400);
      safeFailure((await pending[2]).error, /invalid_input/);
      await cached(server, original, names[0]);
      held.get(names[3]).request.socket.destroy();
      safeFailure((await pending[3]).error, /结果未知/);
      await latched(server, original, names[0]);
      await cached(server, original, names[0]);
    });
  }
});
