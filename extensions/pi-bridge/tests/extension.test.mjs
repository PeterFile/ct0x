import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFile, unlink } from "node:fs/promises";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { loadExtensions } from "./pi-loader.mjs";
import { BASE, TOKEN, json, loopback, privateConfig, safeFailure } from "./helpers.mjs";

const entry = fileURLToPath(new URL("../index.ts", import.meta.url));
const names = ["list_members", "read_channel", "send_message", "create_member"];

async function load(config, digest) {
  const env = [
    ["AGENT_WORKSPACE_MEMBER_TOOL_CONFIG", config],
    ["AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256", digest],
  ];
  const previous = env.map(([key]) => [key, process.env[key]]);
  const setEnv = entries => {
    for (const [key, value] of entries) {
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
  };
  try {
    setEnv(env);
    return await loadExtensions([entry], process.cwd());
  } finally {
    setEnv(previous);
  }
}

async function toolsFor(t, handler = (_, response) => json(response, { items: [], next_cursor: "" }), config = {}) {
  const server = await loopback(t, handler);
  const result = await load(await privateConfig(t, { endpoint: server.endpoint, ...config }));
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

test("actual Pi 1.4.0 loader registers the native tools with closed schemas", async t => {
  const { tools } = await toolsFor(t);
  assert.deepEqual([...tools.keys()], [...names, "search_history", "read_history", "decide_dispatch", "submit_result", "workspace_list", "workspace_read", "workspace_write", "publish_workspace_artifact", "read_work_inputs", "ack_work_input", "hand_back_work_input"]);
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
  assert.deepEqual(Object.keys(send.properties).sort(), ["content", "intent_assessment", "recipient_ids"]);
  assert.equal(send.properties.intent_assessment.additionalProperties, false);
  assert.deepEqual(send.properties.intent_assessment.properties.disposition.anyOf.map(schema => schema.const),
    ["discuss", "clarify", "request_work", "prepare_work"]);
  assert.equal(send.properties.intent_assessment.properties.followup, undefined);
  assert.ok(!JSON.stringify(send).includes("continue_work"));
  assert.deepEqual(send.required, ["content"]);
  assert.equal(send.properties.content.type, "string");
  assert.equal(send.properties.recipient_ids.type, "array");
  assert.equal(send.properties.recipient_ids.items.type, "string");
  const create = tools.get("create_member").parameters;
  assert.deepEqual(Object.keys(create.properties).sort(), ["name", "reason", "role"]);
  assert.deepEqual([...create.required].sort(), ["name", "reason", "role"]);
  for (const schema of Object.values(create.properties)) assert.equal(schema.type, "string");
});

test("contextual chat assessment uses the existing send transport without caller or grants", async t => {
  const receipt = { message_id: "notice", created_at: "2026-11-17T00:00:00Z", delivery_count: 0, content_omitted: true,
    intent_assessment: { task_id: "task", dispatch_request_id: "dispatch" } };
  const { tools, requests } = await toolsFor(t, (_, response) => json(response, receipt, 201));
  const assessment = { disposition: "request_work", action: "text_submission", title: "摘要", acceptance: "三条结论", reason: "用户结合前文要求交付" };
  const payload = { content: "将按约定交付文本。", intent_assessment: assessment };
  assert.deepEqual((await execute(tools.get("send_message"), payload)).content, [{ type: "text", text: JSON.stringify(receipt) }]);
  assert.deepEqual(JSON.parse(requests[0].body), payload);
  for (const extra of [{ caller: "user" }, { authorized_by: "user" }, { grants: ["write"] }, { policy_revision: 999 }, { source_id: "forged" }, { source_kind: "local_user_chat" }, { purpose: "chat_intake/v1" }, { action: "write_files" }]) {
    await assert.rejects(execute(tools.get("send_message"), { ...payload, intent_assessment: { ...assessment, ...extra } }), error => safeFailure(error, /参数/));
  }
  assert.equal(requests.length, 1);
});

test("continuation schema and forwarding use the issued config, not environment or tool arguments", async t => {
  const previous = process.env.AGENT_WORKSPACE_CONTINUE_WORK;
  process.env.AGENT_WORKSPACE_CONTINUE_WORK = "true";
  t.after(() => {
    if (previous === undefined) delete process.env.AGENT_WORKSPACE_CONTINUE_WORK;
    else process.env.AGENT_WORKSPACE_CONTINUE_WORK = previous;
  });
  const assessment = { disposition: "continue_work", reason: "用户明确要求继续当前工作" };
  const send = intent_assessment => ({ content: "已记录续办评估。", intent_assessment });
  const off = await toolsFor(t);
  for (const input of [send(assessment), { ...send(assessment), continue_work: true }]) {
    await assert.rejects(execute(off.tools.get("send_message"), input), error => safeFailure(error, /未发送/));
  }
  assert.equal(off.requests.length, 0);
  const receipt = { message_id: "notice", created_at: "2026-11-18T00:00:00Z", delivery_count: 0, content_omitted: true };
  const { tools, requests } = await toolsFor(t, (_, response) => json(response, receipt, 201), { continue_work: true });
  const schema = tools.get("send_message").parameters.properties.intent_assessment;
  assert.equal(schema.properties.disposition.anyOf.at(-1).const, "continue_work");
  assert.deepEqual(Object.keys(schema.properties.followup.properties), ["task_id", "constraint"]);
  assert.equal(requests.length, 0);
  const tool = tools.get("send_message");
  const followup = { task_id: "referenced-task", constraint: " 仅校对文字🙂\n" };
  const maxReason = "🙂".repeat(1024);
  const maxContent = "🙂".repeat(4096);
  const maxConstraint = "🙂".repeat(512);
  const valid = [send(assessment), send({ ...assessment, followup }), send({ ...assessment, followup: { constraint: "" } }),
    { content: maxContent, intent_assessment: { ...assessment, reason: maxReason, followup: { constraint: maxConstraint } } },
    { content: "\ufeff", intent_assessment: { ...assessment, reason: "\ufeff" } }];
  for (const input of valid) {
    await execute(tool, input, undefined, `continue-${requests.length}`);
    assert.deepEqual(JSON.parse(requests.at(-1).body), input);
  }
  for (const input of [
    ...["", "\u0085", "\u0000", "\ud800", maxReason + "x"].map(reason => ({ ...assessment, reason })),
    ...["\u0085", "\u0000", "\udfff", maxConstraint + "x"].map(constraint => ({ ...assessment, followup: { constraint } })),
    { ...assessment, followup: { task_id: "" } },
    { ...assessment, task_id: "not-a-reference" }, { ...assessment, followup: null },
    { ...assessment, followup: { ...followup, member_id: "forged" } },
    { ...assessment, followup: { ...followup, constraint: null } },
    { ...assessment, action: "workspace_change", followup },
    { ...assessment, disposition: "discuss", followup },
  ]) {
    await assert.rejects(execute(tool, send(input)), error => safeFailure(error, /参数/));
  }
  for (const input of [{ intent_assessment: assessment },
    ...["", "\u0085", "\u0000", "\ud800", maxContent + "x"].map(content => ({ content, intent_assessment: assessment }))]) {
    await assert.rejects(execute(tool, input), error => safeFailure(error, /未发送/));
  }
  assert.equal(requests.length, valid.length);
});

test("invalid or absent initialization config registers no usable tools and exposes no private path", async t => {
  const path = await privateConfig(t, { token: "" });
  for (const config of [undefined, path, await privateConfig(t, { continue_work: "true" }), await privateConfig(t, { continue_work: false })]) {
    const result = await load(config);
    assert.equal(result.extensions.length, 0);
    assert.equal(result.errors.length, 1);
    assert.match(result.errors[0].error, /配置/);
    assert.ok(!result.errors[0].error.includes(TOKEN));
    assert.ok(!result.errors[0].error.includes(path));
  }
});

test("loader rejects malformed or mismatched runtime digests before registering tools", async t => {
  const path = await privateConfig(t);
  const digest = createHash("sha256").update(await readFile(path)).digest("hex");
  const mismatch = (digest[0] === "a" ? "b" : "a") + digest.slice(1);
  for (const invalid of ["", "A".repeat(64), digest.slice(1), `${digest}0`, ` ${digest}`, `${digest}\n`, `sha256:${digest}`, mismatch]) {
    const result = await load(path, invalid);
    assert.equal(result.extensions.length, 0, "invalid marker must not fall back to path-only mode");
    assert.equal(result.errors.length, 1);
    assert.match(result.errors[0].error, /配置/);
    for (const secret of [path, TOKEN, digest, mismatch]) assert.ok(!result.errors[0].error.includes(secret));
  }
});

test("matching runtime digest preserves strict caller scope and exact legacy output", async t => {
  const text = '{ "items": [], "next_cursor": "" }\n';
  const { endpoint, requests } = await loopback(t, (_, response) => {
    response.writeHead(200, { "Content-Type": "application/json" });
    response.end(text);
  });
  const path = await privateConfig(t, { endpoint });
  const digest = createHash("sha256").update(await readFile(path)).digest("hex");
  const loaded = [await load(path), await load(path, digest)];
  await unlink(path); // Both modes must keep using the initialized bytes.
  for (const result of loaded) {
    assert.deepEqual(result.errors, []);
    const tools = result.extensions[0].tools;
    assert.ok(!JSON.stringify(tools.get("send_message").definition.parameters).includes("continue_work"));
    const tool = tools.get("read_channel").definition;
    await assert.rejects(execute(tool, { group_id: "outside-scope" }), error => safeFailure(error, /未发送/));
    assert.deepEqual(await execute(tool, {}), { content: [{ type: "text", text }], details: undefined });
  }
  assert.equal(requests.length, 2);
  for (const request of requests) {
    assert.equal(request.url, `${BASE}/read_channel`);
    assert.equal(request.headers.authorization, `Bearer ${TOKEN}`);
    assert.equal(request.headers["idempotency-key"], undefined);
    assert.equal(request.body.toString("utf8"), "{}");
    assert.ok(!JSON.stringify(request.headers).includes(digest));
  }
});

test("every registered native execute dispatches via real HTTP and returns exact content without duplicate details", async t => {
  const receipts = [
    { items: [{ id: "m1" }], next_cursor: "" }, { items: [], next_cursor: "" },
    { message_id: "m2", created_at: "2026-11-01T00:00:00Z", delivery_count: 0, content_omitted: true },
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
    assert.deepEqual(result, { content: [{ type: "text", text: JSON.stringify(receipts[index]) }], details: undefined });
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

test("work input tools send only handles and preserve write identity; untrusted receipts keep writes unknown", async t => {
  const { tools, requests } = await toolsFor(t, (request, response, body) => {
    const name = request.url.slice(BASE.length + 1);
    const { input_id: id } = JSON.parse(body);
    if (name === "read_work_inputs") return json(response, { items: [{ input_id: "bound-input", content: "继续校对" }] });
    json(response, { input_id: name === "ack_work_input" ? id : "different-input" }, 201);
  });
  const read = tools.get("read_work_inputs");
  const ack = tools.get("ack_work_input");
  const back = tools.get("hand_back_work_input");
  const id = { input_id: "bound-input" };
  const reason = { ...id, reason: " " + "🙂".repeat(511) + "中" }; // Exactly 2048 bytes.
  assert.equal(read.defaultActive, false);
  assert.equal(ack.defaultActive, false);
  assert.equal(back.defaultActive, false);
  for (const [tool, invalid] of [[read, { task_id: "foreign" }], [ack, { ...id, reason: "unexpected" }],
    [ack, { input_id: null }], [ack, { input_id: "\ud800" }], [back, id], [back, { ...reason, reason: "  " }],
    [back, { ...reason, turn_id: "foreign" }],
    ...["", "\u0085", "\u0000", "\udfff", reason.reason + "x"].map(reason => [back, { ...id, reason }])]) {
    await assert.rejects(execute(tool, invalid), error => safeFailure(error, /未发送/));
  }
  for (const callId of ["call-\ud800", "call-\ud801", "call-\udfff"]) {
    await assert.rejects(execute(ack, id, undefined, callId), error => safeFailure(error, /调用标识无效.*未发送/));
  }
  assert.equal(requests.length, 0);
  await execute(read, {});
  assert.equal(requests[0].headers["idempotency-key"], undefined);
  for (const callId of [" call-\ufffd🙂 ", " call-\ufffd🙂 "]) {
    assert.deepEqual((await execute(ack, id, undefined, callId)).content, [{ type: "text", text: JSON.stringify(id) }]);
    assert.equal(requests.at(-1).headers["idempotency-key"], createHash("sha256").update("ack_work_input" + callId).digest("hex"));
  }
  await assert.rejects(execute(back, reason, undefined, "back-1"), error => safeFailure(error, /结果未知/));
  assert.deepEqual(JSON.parse(requests.at(-1).body), reason);
  assert.equal(requests.at(-1).headers["idempotency-key"], createHash("sha256").update("hand_back_work_inputback-1").digest("hex"));
  await assert.rejects(execute(back, reason, undefined, "back-1"), error => safeFailure(error, /结果未知/));
  await assert.rejects(execute(ack, id, undefined, "ack-after-unknown"), error => safeFailure(error, /阻止后续写入/));
  assert.equal(requests.length, 4);
  await execute(read, {});
  assert.equal(requests.length, 5);
});

test("opaque ID whitespace boundary", async t => {
  const receipt = { message_id: "notice", created_at: "2026-11-18T00:00:00Z", delivery_count: 0, content_omitted: true };
  const { tools, requests } = await toolsFor(t, (_, response, body) => {
    const { input_id } = JSON.parse(body);
    json(response, input_id === undefined ? receipt : { input_id }, 201);
  }, { continue_work: true });
  const maxID = "🙂".repeat(64);
  // Same cases as the Go HTTP/MCP fixtures. The first ID is deliberately not NFC.
  const cases = [
    ["\ufeffe\u0301\u3000🙂\ufeff", true], ["\ufeff", true], [maxID, true],
    ["\u0085handle", false], ["handle\u0085", false], ["\u3000handle", false], ["handle\u2003", false], [maxID + "x", false],
  ];
  for (const [name, payload] of [
    ["ack_work_input", input_id => ({ input_id })],
    ["hand_back_work_input", input_id => ({ input_id, reason: "fixture" })],
    ["send_message", task_id => ({ content: "fixture", intent_assessment: {
      disposition: "continue_work", reason: "fixture", followup: { task_id },
    } })],
  ]) {
    for (const [i, [id, valid]] of cases.entries()) {
      const args = payload(id);
      const before = requests.length;
      const result = execute(tools.get(name), args, undefined, `opaque-id-${i}`);
      if (valid) {
        await result;
        assert.equal(requests.length, before + 1);
        assert.deepEqual(requests[before].body, Buffer.from(JSON.stringify(args)));
      } else {
        await assert.rejects(result, error => safeFailure(error, /未发送/));
        assert.equal(requests.length, before);
      }
    }
  }
});

test("oversized work input reports an unavailable complete read, not pagination", async t => {
  const { tools, requests } = await toolsFor(t, (_, response) => json(response, { error: {
    code: "tool_result_too_large", message: "untrusted body",
  } }, 413));
  await assert.rejects(execute(tools.get("read_work_inputs"), {}), error => {
    assert.doesNotMatch(error.message, /limit|length|untrusted body|未知/);
    return safeFailure(error, /未返回部分内容.*不支持分段读取.*tool_result_too_large/);
  });
  assert.equal(requests.length, 1);
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
