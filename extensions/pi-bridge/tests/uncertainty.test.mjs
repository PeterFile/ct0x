import assert from "node:assert/strict";
import { test } from "node:test";
import { createClient } from "../bridge.mjs";
import { BASE, BUDGET, TOKEN, json, loopback, privateConfig, safeFailure } from "./helpers.mjs";

const payloads = {
  send_message: { content: "fixture-only simulated write", recipient_ids: [] },
  create_member: { name: "成员", role: "校对", reason: "检查" },
};
const message = { message_id: "simulated-message", created_at: "2026-11-01T00:00:00.123456789Z",
  delivery_count: 0, content_omitted: true };
const member = { member_id: "simulated-member", reused: false, join_message_id: "simulated-join" };
const page = { items: [], next_cursor: "" };
const writeNames = Object.keys(payloads);

function success(request, response) {
  const tool = request.url.slice(BASE.length + 1);
  json(response, tool === "send_message" ? message : tool === "create_member" ? member : page,
    writeNames.includes(tool) ? 201 : 200);
}

async function clientFor(t, handler) {
  const server = await loopback(t, handler);
  assert.notEqual(new URL(server.endpoint).port, "7310");
  const config = await privateConfig(t, { endpoint: server.endpoint });
  return { ...server, config, invoke: createClient(config) };
}

async function unknown(pending) {
  let result;
  await assert.rejects(pending, error => {
    safeFailure(error, /结果未知/);
    assert.match(error.message, /对账/);
    assert.match(error.message, /不要自动重试/);
    result = error;
    return true;
  });
  return result;
}

async function latched(server, original, tool = "send_message", params = payloads[tool], id = "original") {
  const before = server.requests.length;
  await assert.rejects(server.invoke(tool, params, id), error => {
    assert.equal(error, original, "identical replay must return the locally cached original unknown");
    return safeFailure(error, /结果未知/);
  });
  const other = tool === "send_message" ? "create_member" : "send_message";
  for (const [name, payload, callId] of [
    [tool, params, "new-native-id"],
    [tool, { ...params, [tool === "send_message" ? "content" : "reason"]: "changed" }, id],
    [tool, Object.fromEntries(Object.entries(params).reverse()), id],
    [other, payloads[other], id],
  ]) {
    await assert.rejects(server.invoke(name, payload, callId), error => safeFailure(error, /结果未知.*对账/));
  }
  assert.equal(server.requests.length, before, "no replay or changed write may reach HTTP after uncertainty");
  for (const read of ["list_members", "read_channel"]) {
    assert.deepEqual((await server.invoke(read, {}, `read-${read}`)).json, page);
  }
  assert.equal(server.requests.length, before + 2);
  await assert.rejects(server.invoke(tool, params, "after-successful-reads"),
    error => safeFailure(error, /结果未知.*对账/));
  assert.equal(server.requests.length, before + 2, "successful reads cannot unlock writes");
}

test("lost receipt after simulated commit blocks the next native-new-ID write", async t => {
  let effects = 0;
  const server = await clientFor(t, (request, response) => {
    if (request.url.endsWith("/send_message") && ++effects === 1) request.socket.destroy();
    else success(request, response);
  });
  const original = await unknown(server.invoke("send_message", payloads.send_message, "original"));
  // The old implementation commits a second simulated effect here under a new key.
  const next = await server.invoke("send_message", payloads.send_message, "native-second").catch(error => error);
  assert.equal(server.requests.length, 1, "post-unknown native-new ID must not send a second HTTP write");
  assert.equal(effects, 1, "the fixture must have only one simulated committed effect");
  safeFailure(next, /结果未知.*对账/);
  await latched(server, original);
});

const unknownCases = [
  ["partial receipt disconnect", (request, response) => {
    response.writeHead(201, { "Content-Type": "application/json", "Content-Length": 100 });
    response.write('{"message_id":"');
    setImmediate(() => request.socket.destroy());
  }],
  ...[
    [500, "internal_error"], [503, "service_unavailable"], [502, "invalid_input"],
    [408, "request_canceled"], [504, "deadline_exceeded"],
    [400, "request_canceled"], [403, "invalid_input"], [400, "conflict"],
    [499, "forbidden"], [413, "tool_result_too_large"], [409, "new_refusal"],
  ].map(([status, code]) => [`status ${status} code ${code}`, (_, response) =>
    json(response, { error: { code, message: "simulated refusal" } }, status)]),
  ["refusal missing message", (_, response) => json(response, { error: { code: "invalid_input" } }, 400)],
  ["refusal wrong message type", (_, response) => json(response, { error: { code: "conflict", message: 1 } }, 409)],
  ["refusal missing error envelope", (_, response) => json(response, { code: "invalid_input" }, 400)],
  ["sensitive authoritative-looking refusal", (_, response) =>
    json(response, { error: { code: "conflict", message: TOKEN } }, 409)],
  ...[301, 302, 303, 307, 308].map(status => [`redirect ${status}`, (_, response) => {
    response.writeHead(status, { Location: `/not-followed/${TOKEN}` });
    response.end();
  }]),
  ...[
    [201, "text/html", `<html>${TOKEN}</html>`],
    [500, "application/json", `malformed ${TOKEN}`],
    [400, "application/json", "{}"],
    [201, "application/json", Buffer.from([0xff])],
    [201, "application/json", Buffer.from("\ufeff{}")],
    [201, "application/json", '{"__proto__":{"polluted":true}}'],
    [201, "application/json", '{"constructor":{}}'],
    [201, "application/json", '{"value":1e400}'],
    [201, "application/json", JSON.stringify({ ...message, echo: TOKEN })],
    [201, "application/json", JSON.stringify({ ...message, echo: TOKEN }).replace(/N/g, "\\u004e")],
    [202, "application/json", JSON.stringify(message)],
    [200, "application/json", JSON.stringify(message)],
    [204, "application/json", ""],
    [201, "application/json", JSON.stringify(message), "gzip"],
  ].map(([status, type, body, encoding], index) => [`untrusted response ${index}`, (_, response) => {
    response.writeHead(status, { "Content-Type": type, "X-Secret": TOKEN,
      ...(encoding ? { "Content-Encoding": encoding } : {}) });
    response.end(body);
  }]),
  ...[true, false].map(declared => [`oversized ${declared ? "declared" : "streamed"} response`, (_, response) => {
    response.writeHead(201, { "Content-Type": "application/json",
      ...(declared ? { "Content-Length": BUDGET + 1 } : {}) });
    response.end(" ".repeat(BUDGET + 1));
  }]),
  ["oversized headers", (_, response) => {
    response.writeHead(201, { "Content-Type": "application/json", "X-Large": "x".repeat(9000) });
    response.end(JSON.stringify(message));
  }],
];

for (const [name, respond] of unknownCases) {
  test(`write uncertainty latches: ${name}`, async t => {
    let first = true;
    const server = await clientFor(t, (request, response) => {
      if (first) { first = false; respond(request, response); }
      else success(request, response);
    });
    const original = await unknown(server.invoke("send_message", payloads.send_message, "original"));
    assert.equal(server.requests.length, 1);
    await latched(server, original);
  });
}

const invalidReceipts = [
  ["send_message", 201, {}],
  ...[
    { message_id: "" }, { message_id: null }, { created_at: "" }, { created_at: "not-a-time" },
    { created_at: 1 }, { created_at: "1" }, { created_at: "11/01/2026" },
    { created_at: "2026-11-01 00:00:00Z" }, { delivery_count: -1 }, { delivery_count: 0.5 },
    { delivery_count: Number.MAX_SAFE_INTEGER + 1 }, { content_omitted: false }, { content_omitted: "true" },
  ].map(change => ["send_message", 201, { ...message, ...change }]),
  ...Object.keys(message).map(key => ["send_message", 201,
    Object.fromEntries(Object.entries(message).filter(([name]) => name !== key))]),
  ["create_member", 201, {}],
  ["create_member", 201, { ...member, member_id: "" }],
  ["create_member", 201, { ...member, reused: true }],
  ["create_member", 201, { ...member, reused: "false" }],
  ["create_member", 201, { ...member, join_message_id: "" }],
  ["create_member", 201, { member_id: "m", reused: false }],
  ["create_member", 200, member],
  ["create_member", 200, { ...member, reused: true }],
  ["create_member", 200, { reused: true, join_message_id: "" }],
  ["create_member", 200, { ...member, reused: true, join_message_id: null }],
];

for (const [index, [tool, status, receipt]] of invalidReceipts.entries()) {
  test(`invalid authoritative write receipt latches: ${tool} case ${index}`, async t => {
    let first = true;
    const server = await clientFor(t, (request, response) => {
      if (first) { first = false; json(response, receipt, status); }
      else success(request, response);
    });
    const original = await unknown(server.invoke(tool, payloads[tool], "original"));
    await latched(server, original, tool);
  });
}

test("valid write receipts allow safe additive fields without text projection or an unnecessary date policy", async t => {
  const replies = [
    ["send_message", 201, { ...message, created_at: "2026-11-01T08:00:00+08:00", extra: { version: 2 } }],
    ["send_message", 201, { ...message, delivery_count: Number.MAX_SAFE_INTEGER }],
    ["create_member", 201, { ...member, extra: [true] }],
    ["create_member", 200, { member_id: "existing", reused: true, join_message_id: "", extra: null }],
  ];
  let index = 0;
  const server = await clientFor(t, (_, response) => {
    const [, status, receipt] = replies[index++];
    response.writeHead(status, { "Content-Type": "application/json" });
    response.end(` ${JSON.stringify(receipt)}\n`);
  });
  for (const [tool, , receipt] of replies) {
    const result = await server.invoke(tool, payloads[tool], `valid-${index}`);
    assert.equal(result.text, ` ${JSON.stringify(receipt)}\n`);
    assert.deepEqual(result.json, receipt);
  }
  assert.equal(server.requests.length, replies.length);
});

const refusals = [
  [400, "invalid_input"], [400, "invalid_json"],
  [403, "forbidden"], [403, "invalid_host"], [403, "invalid_origin"],
  [404, "not_found"], [405, "method_not_allowed"], [409, "conflict"], [409, "member_capacity"],
  [413, "body_too_large"], [415, "unsupported_media_type"],
];
for (const [status, code] of refusals) {
  test(`authoritative rejection does not latch: ${status} ${code}`, async t => {
    let first = true;
    const server = await clientFor(t, (request, response) => {
      if (first) { first = false; json(response, { error: { code, message: "simulated refusal" } }, status); }
      else success(request, response);
    });
    await assert.rejects(server.invoke("create_member", payloads.create_member, "rejected"), error => {
      safeFailure(error, new RegExp(code));
      assert.doesNotMatch(error.message, /结果未知/);
      return true;
    });
    assert.deepEqual((await server.invoke("send_message", payloads.send_message, "next-write")).json, message);
    assert.equal(server.requests.length, 2);
  });
}

test("local validation and pre-abort do not poison later writes", async t => {
  const server = await clientFor(t, success);
  const controller = new AbortController();
  controller.abort(new Error(TOKEN));
  const circular = {};
  circular.self = circular;
  const invalid = [undefined, 1n, circular, { content: TOKEN }, { content: "x".repeat(BUDGET) },
    { get content() { throw new Error(TOKEN); } }];
  for (const params of invalid) {
    await assert.rejects(server.invoke("send_message", params, "invalid"), error => safeFailure(error, /未发送/));
  }
  await assert.rejects(server.invoke("not_a_tool", {}, "invalid"), error => safeFailure(error, /未发送/));
  await assert.rejects(server.invoke("send_message", payloads.send_message, ""), error => safeFailure(error, /未发送/));
  await assert.rejects(server.invoke("create_member", payloads.create_member, "pre-abort", controller.signal),
    error => safeFailure(error, /未发送/));
  assert.equal(server.requests.length, 0);
  assert.deepEqual((await server.invoke("send_message", payloads.send_message, "valid")).json, message);
  assert.equal(server.requests.length, 1);
});

for (const tool of ["list_members", "read_channel"]) {
  test(`unknown read does not poison writes: ${tool}`, async t => {
    let first = true;
    const server = await clientFor(t, (request, response) => {
      if (first) { first = false; request.socket.destroy(); }
      else success(request, response);
    });
    await unknown(server.invoke(tool, {}, "read-unknown"));
    assert.deepEqual((await server.invoke("send_message", payloads.send_message, "write-after-read")).json, message);
    assert.equal(server.requests.length, 2);
  });
}

test("read response compatibility does not acquire a complete receipt schema", async t => {
  const arbitrary = { future_read_shape: ["safe", { extra: true }] };
  const server = await clientFor(t, (_, response) => json(response, arbitrary));
  assert.deepEqual((await server.invoke("read_channel", {}, "future-read")).json, arbitrary);
});

test("post-send abort latches before the next model-visible write", async t => {
  const controller = new AbortController();
  let first = true;
  const server = await clientFor(t, (request, response) => {
    if (first) { first = false; controller.abort(new Error(TOKEN)); }
    else success(request, response);
  });
  const original = await unknown(server.invoke("send_message", payloads.send_message, "original", controller.signal));
  await latched(server, original);
});

test("real total timeout latches while the server drips bytes", { timeout: 35000 }, async t => {
  let first = true;
  const server = await clientFor(t, (request, response) => {
    if (!first) return success(request, response);
    first = false;
    response.writeHead(201, { "Content-Type": "application/json" });
    response.write("{");
    const timer = setInterval(() => response.write(" "), 1000);
    response.once("close", () => clearInterval(timer));
  });
  const original = await unknown(server.invoke("send_message", payloads.send_message, "original"));
  await latched(server, original);
});

test("the uncertainty fingerprint uses the sent bytes, not later mutable params", async t => {
  let release;
  const received = new Promise(resolve => { release = resolve; });
  let first = true;
  const server = await clientFor(t, (request, response) => {
    if (first) { first = false; release(request); }
    else success(request, response);
  });
  const params = { ...payloads.send_message, recipient_ids: ["member-a"] };
  const snapshot = JSON.parse(JSON.stringify(params));
  const pending = unknown(server.invoke("send_message", params, "original"));
  const request = await received;
  params.content = "mutated";
  params.recipient_ids.push("member-b");
  request.socket.destroy();
  const original = await pending;
  assert.deepEqual(JSON.parse(server.requests[0].body), snapshot);
  await assert.rejects(server.invoke("send_message", params, "original"), error => safeFailure(error, /结果未知.*对账/));
  await latched(server, original, "send_message", snapshot);
});

test("already-sent concurrent writes finish but success, rejection and later unknown never clear or replace the latch", { timeout: 5000 }, async t => {
  const held = [];
  let ready;
  const received = new Promise(resolve => { ready = resolve; });
  const server = await clientFor(t, (request, response) => {
    if (!writeNames.includes(request.url.slice(BASE.length + 1))) return success(request, response);
    held.push({ request, response });
    if (held.length === 4) ready();
  });
  // All four must arrive before any receipt: no blanket serialization is allowed.
  const pending = ["original", "later-success", "later-rejected", "later-unknown"].map(id =>
    server.invoke("send_message", payloads.send_message, id).then(value => ({ value }), error => ({ error })));
  await received;
  assert.equal(server.requests.length, 4);
  held[0].request.socket.destroy();
  const original = (await pending[0]).error;
  safeFailure(original, /结果未知/);
  await latched(server, original);
  json(held[1].response, message, 201);
  assert.deepEqual((await pending[1]).value.json, message);
  await latched(server, original);
  json(held[2].response, { error: { code: "conflict", message: "simulated refusal" } }, 409);
  safeFailure((await pending[2]).error, /conflict/);
  await latched(server, original);
  held[3].request.socket.destroy();
  safeFailure((await pending[3]).error, /结果未知/);
  await latched(server, original);
});

test("uncertainty is client-instance scoped, not a cross-client durability claim", async t => {
  let first = true;
  const server = await clientFor(t, (request, response) => {
    if (first) { first = false; request.socket.destroy(); }
    else success(request, response);
  });
  const original = await unknown(server.invoke("send_message", payloads.send_message, "original"));
  const another = createClient(server.config);
  assert.deepEqual((await another("send_message", payloads.send_message, "fresh-instance")).json, message);
  await latched(server, original);
});
