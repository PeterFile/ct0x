import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { createClient, TOOL_NAMES } from "../bridge.mjs";
import { loadExtensions } from "./pi-loader.mjs";
import { BASE, TOKEN, json, loopback, privateConfig, safeFailure } from "./helpers.mjs";

const entry = fileURLToPath(new URL("../index.ts", import.meta.url));
const fileNames = ["workspace_list", "workspace_read", "workspace_write", "publish_workspace_artifact"];
const digest = createHash("sha256").update("actual bytes").digest("hex");
const payloads = {
  workspace_list: { path: "." },
  workspace_read: { path: "docs/report.txt" },
  workspace_write: { path: "docs/report.txt", create: false, expected_sha256: digest, text: "  新正文\n" },
  publish_workspace_artifact: { operation_id: "server-operation", name: "报告" },
};
const receipts = {
  workspace_list: { entries: [{ name: "report.txt", kind: "file", size: 12 }] },
  workspace_read: { path: "docs/report.txt", text: "actual bytes", sha256: digest },
  workspace_write: { operation_id: "server-operation", outcome: "done", before_sha256: digest, after_sha256: digest, text_omitted: true },
  publish_workspace_artifact: { artifact_id: "artifact", version_id: "exact-version", sha256: digest, size: 12, applied: true },
};

async function setup(t, handler) {
  const server = await loopback(t, handler || ((request, response) => {
    const name = request.url.slice(BASE.length + 1);
    json(response, receipts[name], fileNames.indexOf(name) >= 2 ? 201 : 200);
  }));
  const config = await privateConfig(t, { endpoint: server.endpoint });
  const previous = process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG;
  let result;
  try {
    process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG = config;
    result = await loadExtensions([entry], process.cwd());
  } finally {
    if (previous === undefined) delete process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG;
    else process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG = previous;
  }
  assert.deepEqual(result.errors, []);
  assert.deepEqual(result.warnings, []);
  const tools = new Map([...result.extensions[0].tools].map(([name, tool]) => [name, tool.definition]));
  return { ...server, tools, client: createClient(config),
    invoke(name, payload = payloads[name], id = "native-workspace", signal) {
      const tool = tools.get(name);
      assert.ok(tool, `${name} must be registered by the real Pi loader`);
      return tool.execute(id, payload, signal, () => assert.fail("no streaming results"), {
        cwd: "/untrusted", caller: "user", group_id: "outside", member_id: "outside", grants: ["*"],
      });
    } };
}

test("repair assessment forwards only review identity and semantic authorization", async t => {
  const server = await setup(t, (request, response, body) => {
    assert.deepEqual(JSON.parse(body).intent_assessment.repair, { review_id: "actual-review", authorization: "original_agreement" });
    json(response, { message_id: "notice", created_at: "2026-11-17T00:00:00Z", delivery_count: 0, content_omitted: true }, 201);
  });
  const payload = { content: "依据真实评审和原约定评估。", intent_assessment: { disposition: "request_work", action: "workspace_change", reason: "结合完整评审与冻结验收", repair: { review_id: "actual-review", authorization: "original_agreement" } } };
  await server.invoke("send_message", payload, "repair-call");
  for (const disposition of ["discuss", "clarify", "prepare_work"]) {
    await assert.rejects(server.invoke("send_message", { ...payload, intent_assessment: { ...payload.intent_assessment, disposition } }), error => safeFailure(error, /参数/));
  }
  for (const repair of [{ review_id: "r", authorization: "agent_consensus" }, { review_id: "r", authorization: "review_request", root: "/outside" }, { review_id: "r" }, { authorization: "review_request" }]) {
    await assert.rejects(server.invoke("send_message", { ...payload, intent_assessment: { ...payload.intent_assessment, repair } }), error => safeFailure(error, /参数/));
  }
});

test("repair assessment accepts the exact final-plan chat_request label without caller selectors", async t => {
  const server = await setup(t, (_, response) => json(response,
    { message_id: "notice", created_at: "2026-11-17T00:00:00Z", delivery_count: 0, content_omitted: true }, 201));
  const payload = { content: "按真实用户修复请求评估。", intent_assessment: { disposition: "request_work", action: "workspace_change", reason: "由服务器核对当前实际聊天命令与Review引用", repair: { review_id: "actual-review", authorization: "chat_request" } } };
  await server.invoke("send_message", payload, "chat-repair");
  assert.deepEqual(JSON.parse(server.requests[0].body).intent_assessment.repair, payload.intent_assessment.repair);
});

test("workspace schemas expose only task-local file data and semantic workspace_change intake", async t => {
  const server = await setup(t);
  assert.deepEqual(TOOL_NAMES.slice(-4), fileNames);
  for (const name of fileNames) {
    const tool = server.tools.get(name);
    assert.ok(tool, name);
    assert.equal(tool.parameters.type, "object");
    assert.equal(tool.parameters.additionalProperties, false);
    assert.match(tool.description, /[\u3400-\u9fff]/u);
  }
  assert.deepEqual(Object.keys(server.tools.get("workspace_list").parameters.properties), ["path"]);
  assert.deepEqual(Object.keys(server.tools.get("workspace_read").parameters.properties), ["path"]);
  assert.deepEqual(Object.keys(server.tools.get("workspace_write").parameters.properties).sort(), ["create", "expected_sha256", "path", "text"]);
  assert.deepEqual(Object.keys(server.tools.get("publish_workspace_artifact").parameters.properties).sort(), ["artifact_id", "expected_version_id", "name", "operation_id"]);
  const action = server.tools.get("send_message").parameters.properties.intent_assessment.properties.action;
  assert.ok(action.anyOf.some(item => item.const === "workspace_change"));
});

test("workspace native execution keeps exact bytes, native IDs and HTTP write key namespace", async t => {
  const server = await setup(t);
  for (const name of fileNames) {
    const result = await server.invoke(name);
    assert.deepEqual(result, { content: [{ type: "text", text: JSON.stringify(receipts[name]) }], details: undefined });
    const request = server.requests.at(-1);
    assert.equal(request.url, `${BASE}/${name}`);
    assert.equal(request.body.toString(), JSON.stringify(payloads[name]));
    assert.equal(request.headers.authorization, `Bearer ${TOKEN}`);
    assert.equal(request.headers["content-length"], String(request.body.length));
    assert.equal(request.headers["idempotency-key"], fileNames.indexOf(name) < 2 ? undefined :
      createHash("sha256").update(name + "native-workspace").digest("hex"));
  }
});

test("workspace strict parameters reject selectors, unsafe paths, conditionals and UTF-8 byte overflow before HTTP", async t => {
  const server = await setup(t);
  const reserved = ["caller", "group_id", "member_id", "task_id", "attempt_id", "root", "resolved_root", "grant_id", "resource_key", "blob_hash", "after_hash", "hash", "operation_id", "idempotency_key", "profile"];
  for (const name of fileNames) {
    for (const field of reserved.filter(field => !(name === "publish_workspace_artifact" && field === "operation_id"))) {
      await assert.rejects(server.invoke(name, { ...payloads[name], [field]: "untrusted" }), error => safeFailure(error, /参数.*未发送/));
    }
    for (const value of [null, [], 1, "payload"]) {
      await assert.rejects(server.invoke(name, value), error => safeFailure(error, /参数.*未发送/));
    }
  }
  for (const name of fileNames.slice(0, 3)) {
    for (const path of ["", "/absolute", "..", "../secret", "a/../b", "./a", "a//b", "a/", "a\\b", "a\u0000b", "a\nb", "a\u0085b", "\ud800", "x".repeat(4097), ...(name === "workspace_list" ? [] : ["."])]) {
      await assert.rejects(server.invoke(name, { ...payloads[name], path }), error => safeFailure(error, /参数.*未发送/));
    }
  }
  const write = payloads.workspace_write;
  for (const input of [
    { ...write, create: true }, { ...write, expected_sha256: undefined }, { ...write, expected_sha256: "A".repeat(64) },
    { ...write, expected_sha256: "0".repeat(63) }, { ...write, create: "false" }, { ...write, text: "\ud800" },
    { ...write, text: "中".repeat(2731) }, { ...write, text: "a".repeat(8193) },
  ]) await assert.rejects(server.invoke("workspace_write", input), error => safeFailure(error, /参数.*未发送/));
  const publish = payloads.publish_workspace_artifact;
  for (const input of [
    { ...publish, artifact_id: "artifact" }, { ...publish, expected_version_id: "version" },
    { operation_id: "op" }, { ...publish, operation_id: "" }, { ...publish, operation_id: "o".repeat(257) },
    { ...publish, artifact_id: "a", expected_version_id: "v" },
    { operation_id: "op", artifact_id: "a", expected_version_id: "" },
    { ...publish, operation_id: "a\u0085b" }, { ...publish, name: "中".repeat(86) },
  ]) await assert.rejects(server.invoke("publish_workspace_artifact", input), error => safeFailure(error, /参数.*未发送/));
  assert.equal(server.requests.length, 0);
});

test("workspace create and artifact CAS conditionals accept only the canonical variants", async t => {
  const server = await setup(t);
  for (const params of [
    { path: "new.txt", create: true, text: "" },
    { path: "new.txt", create: true, text: "\u0000".repeat(8192) },
    { path: "new.txt", create: true, text: "中".repeat(2730) + "ab" },
  ]) {
    await server.invoke("workspace_write", params);
    assert.equal(server.requests.at(-1).body.toString(), JSON.stringify(params), "no silent text truncation");
    assert.ok(server.requests.at(-1).body.length < 65536, "escaping and fixed overhead fit the real HTTP budget");
  }
  await server.invoke("publish_workspace_artifact", { operation_id: "op", artifact_id: "artifact", expected_version_id: "version" });
  assert.equal(server.requests.length, 4);
});

test("workspace_change is a semantic proposal with no root or caller selectors", async t => {
  const server = await setup(t, (_, response) => json(response,
    { message_id: "notice", created_at: "2026-11-17T00:00:00Z", delivery_count: 0, content_omitted: true }, 201));
  const payload = { content: "按已授权范围准备。", intent_assessment: { disposition: "request_work", action: "workspace_change", title: "修改文档", description: "仅任务范围", acceptance: "实际文件及版本", reason: "结合上下文评估" } };
  await server.invoke("send_message", payload);
  assert.equal(server.requests[0].body.toString(), JSON.stringify(payload));
  for (const extra of [{ root: "/outside" }, { caller: "user" }, { grant_id: "x" }]) {
    await assert.rejects(server.invoke("send_message", { ...payload, intent_assessment: { ...payload.intent_assessment, ...extra } }), error => safeFailure(error, /参数/));
  }
  assert.equal(server.requests.length, 1);
});

for (const name of fileNames.slice(2)) {
  test(`${name} unknown latches every mutation but leaves file reads available without new IO retry`, async t => {
    const server = await setup(t, (request, response) => {
      const tool = request.url.slice(BASE.length + 1);
      if (fileNames.indexOf(tool) < 2) return json(response, receipts[tool]);
      response.destroy();
    });
    const original = await server.client(name, payloads[name], "uncertain").catch(error => error);
    safeFailure(original, /结果未知.*对账/);
    assert.equal(server.requests.length, 1);
    await assert.rejects(server.client(name, payloads[name], "uncertain"), error => error === original);
    for (const tool of ["workspace_write", "publish_workspace_artifact", "submit_result", "decide_dispatch", "send_message", "create_member"]) {
      await assert.rejects(server.client(tool, payloads[tool] || {}, "new-id"), error => safeFailure(error, /结果未知.*对账/));
    }
    assert.equal(server.requests.length, 1, "all subsequent mutations rejected locally");
    for (const tool of fileNames.slice(0, 2)) await server.client(tool, payloads[tool], "read-only");
    assert.equal(server.requests.length, 3);
  });
}

for (const [name, invalid] of [
  ["workspace_write", { ...receipts.workspace_write, outcome: "unknown" }],
  ["workspace_write", { ...receipts.workspace_write, outcome: "pending" }],
  ["workspace_write", { ...receipts.workspace_write, after_sha256: "not-a-hash" }],
  ["workspace_write", { ...receipts.workspace_write, root: "/untrusted" }],
  ["publish_workspace_artifact", { ...receipts.publish_workspace_artifact, version_id: "" }],
  ["publish_workspace_artifact", { ...receipts.publish_workspace_artifact, size: -1 }],
  ["publish_workspace_artifact", { ...receipts.publish_workspace_artifact, source_task_id: "injected" }],
]) {
  test(`${name} does not treat an incomplete or unresolved receipt as success: ${JSON.stringify(invalid)}`, async t => {
    const server = await setup(t, (_, response) => json(response, invalid, 201));
    await assert.rejects(server.client(name, payloads[name], "receipt"), error => safeFailure(error, /结果未知.*对账/));
    await assert.rejects(server.client("send_message", { content: "must not send" }, "next"), error => safeFailure(error, /结果未知.*对账/));
    assert.equal(server.requests.length, 1);
  });
}
