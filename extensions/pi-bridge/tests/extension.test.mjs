import assert from "node:assert/strict";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { loadExtensions } from "./pi-loader.mjs";
import { BASE, TOKEN, json, loopback, privateConfig, safeFailure } from "./helpers.mjs";

const entry = fileURLToPath(new URL("../index.ts", import.meta.url));
const names = ["list_members", "read_channel", "send_message", "create_member"];

async function load(config) {
  const previous = process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG;
  try {
    if (config === undefined) delete process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG;
    else process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG = config;
    return await loadExtensions([entry], process.cwd());
  } finally {
    if (previous === undefined) delete process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG;
    else process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG = previous;
  }
}

async function toolsFor(t, handler = (_, response) => json(response, { items: [], next_cursor: "" })) {
  const server = await loopback(t, handler);
  const result = await load(await privateConfig(t, { endpoint: server.endpoint }));
  assert.deepEqual(result.errors, []);
  assert.deepEqual(result.warnings, []);
  assert.equal(result.extensions.length, 1);
  const extension = result.extensions[0];
  assert.equal(extension.handlers.size, 0);
  assert.equal(extension.commands.size, 0);
  assert.equal(extension.flags.size, 0);
  return { ...server, tools: new Map([...extension.tools].map(([name, tool]) => [name, tool.definition])) };
}

function execute(tool, params, signal, callId = "native-call") {
  // Deliberately hostile native context must not choose platform identity or grants.
  return tool.execute(callId, params, signal, () => assert.fail("no streaming output expected"), {
    cwd: "/not-authority", group_id: "evil", token: TOKEN,
    sessionManager: { getSessionId: () => "not-authority" },
  });
}

test("actual Pi 1.4.0 loader registers exactly four native tools with the frozen schemas", async t => {
  const { tools } = await toolsFor(t);
  assert.deepEqual([...tools.keys()], names);
  for (const tool of tools.values()) {
    assert.match(tool.label, /[\u3400-\u9fff]/u);
    assert.match(tool.description, /[\u3400-\u9fff]/u);
    assert.equal(tool.parameters.type, "object");
    assert.equal(tool.parameters.additionalProperties, false);
    assert.equal(tool.execute.length, 5);
    assert.ok(!JSON.stringify(tool).includes(TOKEN));
  }
  for (const name of names.slice(0, 2)) {
    const schema = tools.get(name).parameters;
    assert.deepEqual(Object.keys(schema.properties).sort(), ["before", "limit"]);
    assert.deepEqual(schema.required || [], []);
    assert.equal(schema.properties.limit.type, "integer");
    assert.equal(schema.properties.limit.minimum, 1);
    assert.equal(schema.properties.limit.maximum, 200);
    assert.equal(schema.properties.before.type, "string");
  }
  const send = tools.get("send_message").parameters;
  assert.deepEqual(Object.keys(send.properties).sort(), ["content", "recipient_ids"]);
  assert.deepEqual(send.required, ["content"]);
  assert.equal(send.properties.content.type, "string");
  assert.equal(send.properties.recipient_ids.type, "array");
  assert.equal(send.properties.recipient_ids.items.type, "string");
  const create = tools.get("create_member").parameters;
  assert.deepEqual(Object.keys(create.properties).sort(), ["name", "reason", "role"]);
  assert.deepEqual([...create.required].sort(), ["name", "reason", "role"]);
  for (const schema of Object.values(create.properties)) assert.equal(schema.type, "string");
});

test("invalid or absent initialization config registers no usable tools and exposes no private path", async t => {
  const path = await privateConfig(t, { token: "" });
  for (const config of [undefined, path]) {
    const result = await load(config);
    assert.equal(result.extensions.length, 0);
    assert.equal(result.errors.length, 1);
    assert.match(result.errors[0].error, /配置/);
    assert.ok(!result.errors[0].error.includes(TOKEN));
    assert.ok(!result.errors[0].error.includes(path));
  }
});

test("every registered native execute dispatches via real HTTP and returns exact content and safe details", async t => {
  const receipts = [
    { items: [{ id: "m1" }], next_cursor: "" }, { items: [], next_cursor: "" },
    { message_id: "m2", topic_id: "topic", created_at: "2026-11-01T00:00:00Z", delivery_count: 0, content_omitted: true },
    { member_id: "m3", reused: true, join_message_id: "" },
  ];
  const { tools, requests } = await toolsFor(t, (request, response) => {
    const index = names.indexOf(request.url.slice(BASE.length + 1));
    json(response, receipts[index], index === 2 ? 201 : 200);
  });
  const payloads = [{ limit: 1, before: "cursor" }, {}, { content: "你好", recipient_ids: [] },
    { name: "成员", role: "校对", reason: "检查" }];
  for (const [index, name] of names.entries()) {
    const result = await execute(tools.get(name), payloads[index]);
    assert.deepEqual(result, { content: [{ type: "text", text: JSON.stringify(receipts[index]) }], details: receipts[index] });
    assert.deepEqual(JSON.parse(requests[index].body), payloads[index]);
  }
  assert.equal(requests.length, 4);
});

test("native schema is not the authority boundary: runtime extra fields and wrong types never reach HTTP", async t => {
  const { tools, requests } = await toolsFor(t);
  const reserved = ["token", "endpoint", "group_id", "member_id", "identity", "topic_id", "provider", "grants", "TurnID", "claim", "idempotency_key"];
  for (const name of names) {
    const base = name === "send_message" ? { content: "ok" } : name === "create_member" ? { name: "n", role: "r", reason: "why" } : {};
    for (const field of reserved) {
      await assert.rejects(execute(tools.get(name), { ...base, [field]: TOKEN }), error => safeFailure(error, /参数/));
    }
    for (const invalid of [null, [], "text", 1]) {
      await assert.rejects(execute(tools.get(name), invalid), error => safeFailure(error, /参数/));
    }
  }
  for (const name of names.slice(0, 2)) {
    for (const invalid of [{ limit: 0 }, { limit: 201 }, { limit: 1.5 }, { limit: "1" }, { limit: NaN },
      { limit: Infinity }, { before: 1 }, { before: null }, { limit: undefined }]) {
      await assert.rejects(execute(tools.get(name), invalid), error => safeFailure(error, /参数/));
    }
  }
  for (const invalid of [{}, { content: 1 }, { content: null }, { content: "ok", recipient_ids: "x" },
    { content: "ok", recipient_ids: [1] }, { content: "ok", recipient_ids: [undefined] }]) {
    await assert.rejects(execute(tools.get("send_message"), invalid), error => safeFailure(error, /参数/));
  }
  for (const invalid of [{}, { name: "n", role: "r" }, { name: "n", role: 1, reason: "r" }]) {
    await assert.rejects(execute(tools.get("create_member"), invalid), error => safeFailure(error, /参数/));
  }
  assert.equal(requests.length, 0);
});

test("runtime serialization cannot change validated types or introduce fields", async t => {
  const { tools, requests } = await toolsFor(t);
  const recipients = ["member"];
  recipients.toJSON = () => ({ group_id: "outside-scope" });
  await assert.rejects(execute(tools.get("send_message"), { content: "ok", recipient_ids: recipients }),
    error => safeFailure(error, /参数/));
  await assert.rejects(execute(tools.get("read_channel"), new Date()), error => safeFailure(error, /参数/));
  assert.equal(requests.length, 0);
});

test("exceptions from runtime argument access cannot leak their raw messages", async t => {
  const { tools, requests } = await toolsFor(t);
  const params = { get content() { throw new Error(TOKEN); } };
  await assert.rejects(execute(tools.get("send_message"), params), error => safeFailure(error, /参数/));
  assert.equal(requests.length, 0);
});

test("native execution forwards cancellation and never returns partial results or a false success", async t => {
  const { tools, requests } = await toolsFor(t);
  const controller = new AbortController();
  controller.abort(new Error(TOKEN));
  await assert.rejects(execute(tools.get("send_message"), { content: "ok" }, controller.signal),
    error => safeFailure(error, /取消.*未发送/));
  assert.equal(requests.length, 0);
});
