import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFile, readdir, unlink } from "node:fs/promises";
import { dirname } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { loadExtensions, loadSessionSDK } from "./pi-loader.mjs";
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
  assert.deepEqual(Object.keys(send.properties).sort(), ["content", "intent_assessment", "recipient_ids", "reply_to_message_id"]);
  assert.equal(send.properties.reply_to_message_id.type, "string");
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

test("plain reply_to_message_id and optional recipients reach HTTP unchanged", async t => {
  const receipt = { message_id: "reply", created_at: "2026-11-20T00:00:00Z", delivery_count: 0, content_omitted: true };
  const { tools, requests } = await toolsFor(t, (_, response) => json(response, receipt, 201));
  const send = tools.get("send_message");
  const plain = { content: "直接接话。" };
  const quote = { content: "引用旧原文。", reply_to_message_id: "\ufeffe\u0301🙂" };
  for (const [index, payload] of [plain, { ...plain, recipient_ids: [] }, quote,
    { ...quote, recipient_ids: [] }, { ...quote, reply_to_message_id: "🙂".repeat(64), recipient_ids: ["peer"] }].entries()) {
    const args = send.prepareArguments(payload);
    assert.deepEqual(args, payload);
    const id = `reply-${index}`;
    await execute(send, args, undefined, id);
    assert.equal(requests.length, index + 1);
    assert.equal(requests[index].url, `${BASE}/send_message`);
    assert.deepEqual(requests[index].body, Buffer.from(JSON.stringify(payload)));
    assert.equal(requests[index].headers["idempotency-key"], createHash("sha256").update("send_message" + id).digest("hex"));
  }
});

test("invalid reply_to_message_id and conflicting structured input fail before Pi coercion or HTTP", async t => {
  const { tools, requests } = await toolsFor(t);
  const send = tools.get("send_message");
  const quote = { content: "引用旧原文。", reply_to_message_id: "old-message" };
  for (const input of [
    ...[undefined, null, 42, false, {}, [], "", ".", "..", " id", "id\u3000", "a\u0085b", "a\u0000b", "\ud800", "🙂".repeat(64) + "x"]
      .map(reply_to_message_id => ({ ...quote, reply_to_message_id })),
    { ...quote, recipient_ids: null }, { ...quote, recipient_ids: [1] },
    ...[undefined, null, { disposition: "discuss", reason: "只讨论" }].map(intent_assessment => ({ ...quote, intent_assessment })),
    { ...quote, discussion: null },
  ]) {
    assert.throws(() => send.prepareArguments(input), /未发送/);
    await assert.rejects(execute(send, input), /未发送/);
  }
  assert.equal(requests.length, 0);
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

test("issued discussion excerpt is verbatim and cannot be coerced or normalized", async t => {
  const { tools, requests } = await toolsFor(t, (_, response) => json(response, {
    message_id: "notice", created_at: "2026-11-18T00:00:00Z", delivery_count: 0, content_omitted: true,
  }, 201), { discussion_only_excerpt: true });
  const send = tools.get("send_message");
  const exact = " \n只讨论🙂e\u0301\n";
  const payload = { content: "先讨论。", intent_assessment: { disposition: "discuss", reason: "明确范围", discussion_only_excerpt: exact } };
  assert.deepEqual(send.prepareArguments(payload), payload);
  for (const bad of [null, 42, false]) {
    assert.throws(() => send.prepareArguments({ ...payload, intent_assessment: { ...payload.intent_assessment, discussion_only_excerpt: bad } }), /未发送/);
  }
  assert.throws(() => send.prepareArguments({ ...payload, recipient_ids: null }), /未发送/);
  for (const bad of ["\u0085", "\ud800", "\0", "🙂".repeat(1024) + "x"]) {
    await assert.rejects(execute(send, { ...payload, intent_assessment: { ...payload.intent_assessment, discussion_only_excerpt: bad } }), /未发送/);
  }
  await execute(send, payload);
  assert.equal(requests.length, 1);
  assert.deepEqual(JSON.parse(requests[0].body), payload);
});

test("issued intake guidance separates continuation, new investigation, and literal discussion", async t => {
  const config = await privateConfig(t, {
    continue_work: true, discussion_only_excerpt: true, intake_assessment_action: "read_only_investigation",
  });
  const digest = createHash("sha256").update(await readFile(config)).digest("hex");
  const loaded = await load(config, digest);
  assert.deepEqual(loaded.errors, []);
  const send = loaded.extensions[0].tools.get("send_message").definition;
  const fields = send.parameters.properties.intent_assessment.properties;
  assert.deepEqual(fields.disposition.anyOf.map(v => v.const), ["discuss", "clarify", "request_work", "prepare_work", "continue_work"]);
  assert.deepEqual(fields.action.anyOf.map(v => v.const), ["text_submission", "workspace_change", "read_only_investigation"]);
  assert.match(fields.disposition.description, /仅讨论时优先选discuss.*逐字保留discussion_only_excerpt/u);
  assert.match(fields.disposition.description, /continue_work仅用于.*续办.*followup\.task_id/u);
  assert.match(fields.disposition.description, /text_submission仅用于受支持的topic_summary；read_only_investigation仅用于获准范围内明确的新调查/u);
  assert.match(fields.action.description, /chat_candidate\.policy.*单收件人本机用户原文明确要求另开/u);
  assert.doesNotMatch(fields.disposition.description, /prepare_work仅限受支持的topic_summary/u);
  for (const intent_assessment of [
    { disposition: "continue_work", action: "read_only_investigation", reason: "不能混用动作" },
    { disposition: "prepare_work", action: "read_only_investigation", reason: "不能续办", followup: { task_id: "old" } },
    { disposition: "prepare_work", action: "read_only_investigation", reason: "仅讨论", discussion_only_excerpt: " 只讨论。\n" },
  ]) {
    await assert.rejects(execute(send, { content: "fixture", intent_assessment }), error => safeFailure(error, /未发送/));
  }
});

test("only issued relay intake advertises discussion; raw ambiguous fields cannot fall through", async t => {
  const payload = { content: "请核对来源。", discussion: { kind: "invite", recipient_id: "peer", expected_reply: "原始来源。" } };
  const off = await toolsFor(t);
  const ordinary = off.tools.get("send_message");
  assert.equal(ordinary.parameters.properties.discussion, undefined);
  assert.throws(() => ordinary.prepareArguments({ ...payload, discussion: null }), /未发送/);
  await assert.rejects(execute(ordinary, payload), /未发送/);
  assert.equal(off.requests.length, 0);

  const receipt = { message_id: "invite", created_at: "2026-11-18T00:00:00Z", delivery_count: 1, content_omitted: true };
  const server = await loopback(t, (_, response) => json(response, receipt, 201));
  const path = await privateConfig(t, { endpoint: server.endpoint, discussion_relay: true, discussion_only_excerpt: true,
    continue_work: true, intake_assessment_action: "read_only_investigation" });
  const digest = createHash("sha256").update(await readFile(path)).digest("hex");
  const { extensions, errors } = await load(path, digest);
  assert.deepEqual(errors, []);
  const send = extensions[0].tools.get("send_message").definition;
  const fields = send.parameters.properties;
  assert.deepEqual(Object.keys(fields.discussion.properties).sort(), ["expected_reply", "kind", "recipient_id"]);
  assert.ok(fields.intent_assessment.properties.discussion_only_excerpt);
  assert.ok(fields.intent_assessment.properties.followup);
  assert.ok(fields.intent_assessment.properties.action.anyOf.some(action => action.const === "read_only_investigation"));
  assert.match(extensions[0].tools.get("read_channel").definition.description, /discussion_context/);
  assert.deepEqual(send.prepareArguments(payload), payload);
  for (const input of [
    { ...payload, discussion: null }, { ...payload, recipient_ids: [] }, { ...payload, recipient_ids: null },
    { ...payload, intent_assessment: { disposition: "discuss", reason: "讨论" } },
    { ...payload, reply_to_message_id: "old-message" }, { ...payload, reply_to_message_id: null },
    { ...payload, discussion: { ...payload.discussion, recipient_id: null } },
    { ...payload, discussion: { ...payload.discussion, expected_reply: null } },
    { ...payload, discussion: { kind: "invite" } },
    { ...payload, discussion: { kind: "reply", recipient_id: "" } },
    { ...payload, discussion: { kind: "resolve", expected_reply: "" } },
    { ...payload, discussion: { kind: "reply", candidate_id: "forged" } },
  ]) {
    assert.throws(() => send.prepareArguments(input), /未发送/);
    await assert.rejects(execute(send, input), /未发送/);
  }
  assert.equal(server.requests.length, 0);
  assert.equal((await execute(send, payload)).content[0].text, JSON.stringify(receipt));
  assert.deepEqual(JSON.parse(server.requests[0].body), payload);

  // Each string is exactly 1024 bytes after Go's compact JSON encoding.
  assert.equal(fields.discussion.properties.expected_reply.maxLength, 1022);
  for (const atLimit of ["x".repeat(1022), "中".repeat(340) + "ab",
    '"\\\n\b\f\r\t'.repeat(73), "<>&\u2028\u2029".repeat(34) + "ab"]) {
    const withExpected = expected_reply => ({ ...payload, discussion: { ...payload.discussion, expected_reply } });
    const exact = withExpected(atLimit), oversized = withExpected(atLimit + "x");
    assert.deepEqual(send.prepareArguments(exact), exact);
    const sent = server.requests.length;
    assert.throws(() => send.prepareArguments(oversized), /未发送/);
    await assert.rejects(execute(send, oversized), /未发送/);
    assert.equal(server.requests.length, sent);
    await execute(send, exact, undefined, `limit-${sent}`);
    assert.deepEqual(JSON.parse(server.requests.at(-1).body), exact);
  }
});

test("real Pi 1.4.0 SDK invokes native relay tools and stops writes on an unknown receipt", async t => {
  const page = { items: [], next_cursor: "" };
  let writes = 0;
  const server = await loopback(t, (request, response) => {
    if (request.url.endsWith("/read_channel")) return json(response, page);
    const rejected = ++writes === 1;
    json(response, { error: { code: rejected ? "invalid_input" : "internal_error", message: "offline fixture" } }, rejected ? 400 : 500);
  });
  const path = await privateConfig(t, { endpoint: server.endpoint, discussion_relay: true });
  const sha = bytes => createHash("sha256").update(bytes).digest("hex");
  const loaded = await load(path, sha(await readFile(path)));
  assert.deepEqual(loaded.errors, []);
  const { createAgentSession, SessionManager, SettingsManager } = await loadSessionSDK();
  const directory = dirname(path);
  const deny = name => () => assert.fail(`unexpected ${name}`);
  t.mock.method(globalThis, "fetch", deny("network fetch"));
  const modelRuntime = new Proxy({}, { get: (_target, name) => deny(String(name)) });
  const model = { id: "relay-fixture", name: "Synthetic", provider: "synthetic", api: "synthetic-no-network",
    baseUrl: "http://127.0.0.1:1", reasoning: false, input: ["text"], contextWindow: 8192, maxTokens: 1,
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } };
  const profile = ["list_members", "read_channel", "send_message", "search_history", "read_history"];
  const { session } = await createAgentSession({
    cwd: directory, agentDir: directory, model, modelRuntime, thinkingLevel: "off", noTools: "builtin", tools: [...profile],
    sessionManager: SessionManager.inMemory(directory),
    settingsManager: SettingsManager.inMemory({ retry: { enabled: false }, cacheWarming: "off", enableInstallTelemetry: false }),
    resourceLoader: {
      getExtensions: () => loaded, getSkills: () => ({ skills: [], diagnostics: [] }), getPrompts: () => ({ prompts: [], diagnostics: [] }),
      getThemes: () => ({ themes: [], diagnostics: [] }), getAgentsFiles: () => ({ agentsFiles: [] }),
      getSystemPrompt: () => "Offline relay fixture.", getSystemPromptSource: () => undefined,
      getAppendSystemPrompt: () => [], getAppendSystemPromptSources: () => [], extendResources: deny("resources"), reload: deny("reload"),
    },
  });
  try {
    session.agent.streamFn = deny("agent stream");
    session.agent.prompt = deny("agent prompt");
    session.prompt = deny("session prompt");
    await session.bindExtensions({ mode: "json" });
    assert.deepEqual(session.getActiveToolNames(), profile);
    assert.deepEqual(session.getCallableToolNames(), profile);
    const native = new Map(session.agent.state.tools.map(tool => [tool.name, tool]));
    const send = (id, params) => native.get("send_message").execute(id, params);
    const invite = { content: "请核对来源。", discussion: { kind: "invite", recipient_id: "peer" } };
    await assert.rejects(send("definite", invite), error => safeFailure(error, /invalid_input/) && !error.message.includes("未知"));
    let original;
    await assert.rejects(send("uncertain", invite), error => {
      original = error;
      return safeFailure(error, /结果未知.*幂等键.*对账/);
    });
    assert.equal(writes, 2, "a definite rejection must not stop the next valid write");
    assert.deepEqual(JSON.parse(server.requests[1].body), invite);
    assert.equal(server.requests[1].headers["idempotency-key"], sha("send_messageuncertain"));
    await assert.rejects(send("uncertain", invite), error => error === original);
    await assert.rejects(send("uncertain", { ...invite, content: "不同请求。" }), /阻止后续写入/);
    const channel = await native.get("read_channel").execute("reconcile", {});
    assert.deepEqual(channel.content, [{ type: "text", text: JSON.stringify(page) }]);
    await assert.rejects(send("new-key", invite), /阻止后续写入/);
    assert.equal(writes, 2);
    assert.equal(server.requests.length, 3, "only an additional read may reach HTTP");
  } finally {
    session.dispose();
  }
  assert.deepEqual(await readdir(directory), ["capability.json"]);
});

test("strict investigation registers only the digested tool set and blocks host calls", async t => {
  const names = ["read_investigation_artifact", "submit_result"];
  const config = await privateConfig(t, { investigation_tools: names });
  const digest = createHash("sha256").update(await readFile(config)).digest("hex");
  for (const bad of [undefined, "0".repeat(64)]) {
    assert.equal((await load(config, bad)).extensions.length, 0);
  }
  const loaded = await load(config, digest);
  assert.deepEqual(loaded.errors, []);
  const ext = loaded.extensions[0];
  assert.deepEqual([...ext.tools.keys()], names);
  const call = ext.handlers.get("tool_call")[0];
  for (const toolName of ["read", "bash", "write", "mcp__foreign", "read_channel", "workspace_write"]) {
    assert.equal((await call({ type: "tool_call", toolName }, {})).block, true);
  }
  assert.equal(await call({ type: "tool_call", toolName: names[0] }, {}), undefined);
  assert.equal(ext.tools.get("submit_result").definition.parameters.properties.artifact_versions, undefined);
});
