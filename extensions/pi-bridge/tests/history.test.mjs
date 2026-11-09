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
import { BASE, TOKEN, json, loopback, privateConfig, safeFailure } from "./helpers.mjs";

const entry = fileURLToPath(new URL("../index.ts", import.meta.url));
const four = ["list_members", "read_channel", "send_message", "create_member"];
const history = ["search_history", "read_history"];
const six = [...four, ...history];
const payloads = [{ query: "关键词", limit: 50, before: "" }, { message_id: "message", offset: 0, length: 4096 }];
const receipts = [
  { items: [{ message_id: "message", sender_id: "member", created_at: "2026-11-01T00:00:00Z",
    excerpt: { offset: 0, next_offset: 2, end: false, total_runes: 5000, content: "前缀", content_complete: false } }], next_cursor: "opaque" },
  { message_id: "message", offset: 0, next_offset: 2, end: false, total_runes: 5000, content: "片段", content_complete: false },
];
const message = { message_id: "written", created_at: "2026-11-01T00:00:00Z", delivery_count: 0, content_omitted: true };

async function load(t) {
  // This config is read at registration only; these schema/session tests never invoke HTTP.
  const config = await privateConfig(t, { endpoint: "http://127.0.0.1:1/api/v1/member-tools" });
  const previous = process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG;
  try {
    process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG = config;
    const result = await loadExtensions([entry], process.cwd());
    assert.deepEqual(result.errors, []);
    assert.deepEqual(result.warnings, []);
    return result;
  } finally {
    if (previous === undefined) delete process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG;
    else process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG = previous;
  }
}

function definitions(result) {
  return new Map([...result.extensions[0].tools].map(([name, tool]) => [name, tool.definition]));
}

function execute(tool, params) {
  return tool.execute("native-history", params, undefined, () => assert.fail("no updates expected"), {});
}

test("history schemas use bounded search and fragment fields, not channel pagination", async t => {
  const tools = definitions(await load(t));
  assert.ok(tools.has("search_history"), "missing search_history schema");
  assert.ok(tools.has("read_history"), "missing read_history schema");
  for (const name of history) {
    const tool = tools.get(name);
    assert.equal(tool.parameters.additionalProperties, false);
    assert.match(tool.label, /[\u3400-\u9fff]/u);
    assert.match(tool.description, /服务端绑定/);
    assert.match(tool.description, /话题/);
    assert.match(tool.description, /前缀|片段/);
  }
  const search = tools.get("search_history").parameters;
  assert.deepEqual(search.required, ["query"]);
  assert.deepEqual(Object.keys(search.properties).sort(), ["before", "limit", "query"]);
  assert.equal(search.properties.query.type, "string");
  assert.equal(search.properties.before.type, "string");
  assert.equal(search.properties.limit.type, "integer");
  assert.equal(search.properties.limit.minimum, 1);
  assert.equal(search.properties.limit.maximum, 50);
  const read = tools.get("read_history").parameters;
  assert.deepEqual(read.required, ["message_id"]);
  assert.deepEqual(Object.keys(read.properties).sort(), ["length", "message_id", "offset"]);
  assert.equal(read.properties.message_id.type, "string");
  assert.equal(read.properties.offset.type, "integer");
  assert.equal(read.properties.offset.minimum, 0);
  assert.equal(read.properties.length.type, "integer");
  assert.equal(read.properties.length.minimum, 1);
  assert.equal(read.properties.length.maximum, 4096);
});

test("history runtime rejects selectors, invalid original values and unsafe JSON snapshots before HTTP", async t => {
  const tools = definitions(await load(t));
  for (const [index, name] of history.entries()) {
    assert.ok(tools.has(name), `missing ${name} schema`);
    const tool = tools.get(name);
    for (const field of ["caller", "caller_id", "group", "group_id", "member_id", "conversation_id", "topic", "topic_id",
      "grants", "model", "provider", "run_id", "TurnID", "claim", "token", "endpoint", "toolCallId", "idempotency_key", "profile"]) {
      await assert.rejects(execute(tool, { ...payloads[index], [field]: "untrusted" }), error => safeFailure(error, /参数.*未发送/));
    }
    for (const invalid of [null, [], "text", 1, {},
      ...(index === 0 ? [{ query: null }, { query: 1 }, { query: "q", before: null }, { query: "q", before: 1 },
        ...[0, 51, 1.5, "1", null, undefined, NaN, Infinity].map(limit => ({ query: "q", limit }))] :
        [{ message_id: null }, { message_id: 1 },
          ...[-1, 0.5, "0", null, undefined, NaN, Infinity].map(offset => ({ message_id: "m", offset })),
          ...[0, 4097, 1.5, "1", null, undefined, NaN, Infinity].map(length => ({ message_id: "m", length }))])]) {
      await assert.rejects(execute(tool, invalid), error => safeFailure(error, /参数.*未发送/));
    }
    const field = index === 0 ? "query" : "message_id";
    let accesses = 0;
    await assert.rejects(execute(tool, { get [field]() { return ++accesses === 1 ? "ok" : 1; } }),
      error => safeFailure(error, /参数.*未发送/));
    await assert.rejects(execute(tool, { get [field]() { throw new Error(TOKEN); } }),
      error => safeFailure(error, /参数.*未发送/));
  }
});

test("history execute preserves optional defaults, empty cursor and exact prefix/fragment responses without write keys", async t => {
  const server = await loopback(t, (request, response) => json(response, receipts[history.indexOf(request.url.slice(BASE.length + 1))]));
  const previous = process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG;
  let result;
  try {
    process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG = await privateConfig(t, { endpoint: server.endpoint });
    result = await loadExtensions([entry], process.cwd());
  } finally {
    if (previous === undefined) delete process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG;
    else process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG = previous;
  }
  assert.deepEqual(result.errors, []);
  const tools = definitions(result);
  for (const [index, name] of history.entries()) {
    assert.ok(tools.has(name), `missing ${name} schema`);
    for (const params of [payloads[index], index === 0 ? { query: "q" } : { message_id: "m" },
      index === 0 ? { query: "q", limit: 1, before: "opaque" } : { message_id: "m", offset: 5, length: 1 }]) {
      assert.deepEqual(await execute(tools.get(name), params), {
        content: [{ type: "text", text: JSON.stringify(receipts[index]) }], details: undefined,
      });
      const request = server.requests.at(-1);
      assert.deepEqual(JSON.parse(request.body), params);
      assert.equal(request.headers["idempotency-key"], undefined);
    }
  }
  assert.equal(server.requests.length, 6);
});

test("both history reads remain usable after write unknown without unlocking the frozen write fuse", async t => {
  const server = await loopback(t, (request, response) => {
    if (request.url.endsWith("/send_message")) request.socket.destroy();
    else json(response, receipts[history.indexOf(request.url.slice(BASE.length + 1))]);
  });
  const invoke = createClient(await privateConfig(t, { endpoint: server.endpoint }));
  await assert.rejects(invoke("send_message", { content: "write" }, "original"), error => safeFailure(error, /结果未知/));
  for (const [index, name] of history.entries()) {
    assert.deepEqual((await invoke(name, payloads[index], "history-native")).json, receipts[index]);
    assert.equal(server.requests.at(-1).headers["idempotency-key"], undefined);
    await assert.rejects(invoke("send_message", { content: "write" }, "new-id"), error => safeFailure(error, /结果未知.*对账/));
    await assert.rejects(invoke("create_member", { name: "n", role: "r", reason: "r" }, "other-write"),
      error => safeFailure(error, /结果未知.*对账/));
  }
  assert.equal(server.requests.length, 3, "only original write and two reads may reach HTTP");
});

for (const [index, name] of history.entries()) {
  test(`${name} failure does not latch subsequent legitimate writes or change their stable keys`, async t => {
    const server = await loopback(t, (request, response) => {
      if (request.url.endsWith(`/${name}`)) request.socket.destroy();
      else json(response, message, 201);
    });
    const invoke = createClient(await privateConfig(t, { endpoint: server.endpoint }));
    await assert.rejects(invoke(name, payloads[index], "failed-read"), error => safeFailure(error, /结果未知/));
    assert.equal(server.requests[0].headers["idempotency-key"], undefined);
    for (let i = 0; i < 2; i++) assert.deepEqual((await invoke("send_message", { content: "ok" }, "write-id")).json, message);
    assert.equal(server.requests.length, 3);
    const key = createHash("sha256").update("send_messagewrite-id").digest("hex");
    assert.equal(server.requests[1].headers["idempotency-key"], key);
    assert.equal(server.requests[2].headers["idempotency-key"], key);
  });
}

for (const profile of [four, six]) {
  test(`real Pi 1.4.0 SDK activates exactly ${profile.length} granted tools and cannot widen them without a prompt`, async t => {
    const extensions = await load(t);
    assert.deepEqual([...definitions(extensions).keys()], [...six, "decide_dispatch", "submit_result"], "the actual loader must register all eight schemas");
    const { createAgentSession, SessionManager, SettingsManager } = await loadSessionSDK();
    const directory = await mkdtemp(join(tmpdir(), "pi-history-session-"));
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
      getAppendSystemPrompt: () => [],
      getAppendSystemPromptSources: () => [],
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
      const inactive = ["read", "bash", "powershell", "edit", "write", "grep", "find", "ls", "codemode", "tool_search", "ungranted", "create_task", "decide_dispatch", "submit_result"];
      session.setActiveToolsByName([...profile, ...history, ...inactive]);
      assert.deepEqual(session.getActiveToolNames(), profile, "session API must not widen the allowlist");
      assert.deepEqual(session.getCallableToolNames(), profile);
      assert.deepEqual(session.messages, []);
      assert.equal(session.isStreaming, false);
    } finally {
      session.dispose();
    }
    assert.deepEqual(forbiddenCalls, [], "no model, auth, resource discovery, stream or prompt may be invoked");
    assert.deepEqual(await readdir(directory), [], "session/settings/auth/models files must not be created");
  });
}
