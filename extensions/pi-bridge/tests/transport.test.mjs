import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { once } from "node:events";
import { unlink, writeFile } from "node:fs/promises";
import { test } from "node:test";
import { createClient } from "../bridge.mjs";
import { BASE, BUDGET, TOKEN, json, loopback, privateConfig, safeFailure } from "./helpers.mjs";

const names = ["list_members", "read_channel", "send_message", "create_member"];
const isWrite = name => name === "send_message" || name === "create_member";
const payloads = [{ limit: 200, before: "opaque" }, {}, { content: "消息", recipient_ids: ["member-a"] },
  { name: "研究员", role: "调研", reason: "查证资料" }];
const receipts = [{ items: [{ id: "a" }], next_cursor: "opaque" }, { items: [], next_cursor: "" },
  { message_id: "m", created_at: "2026-11-01T00:00:00Z", delivery_count: 1, content_omitted: true },
  { member_id: "b", reused: false, join_message_id: "join" }];

async function clientFor(t, handler) {
  const server = await loopback(t, handler);
  return { ...server, invoke: createClient(await privateConfig(t, { endpoint: server.endpoint })) };
}

function sizedJSON(bytes, prefix = '{"items":["', suffix = '"],"next_cursor":""}') {
  const remaining = bytes - Buffer.byteLength(prefix + suffix);
  const value = "中".repeat(Math.floor(remaining / 3)) + "x".repeat(remaining % 3);
  return prefix + value + suffix;
}

test("all four dispatches use actual POST headers and exact derived write replay keys", async t => {
  const server = await clientFor(t, (request, response) => {
    const index = names.indexOf(request.url.slice(BASE.length + 1));
    json(response, receipts[index], isWrite(names[index]) ? 201 : 200);
  });
  for (const [index, name] of names.entries()) {
    const result = await server.invoke(name, payloads[index], "native-call-1");
    assert.equal(result.text, JSON.stringify(receipts[index]));
    assert.deepEqual(result.json, receipts[index]);
  }
  await server.invoke("send_message", payloads[2], "native-call-1");
  assert.equal(server.requests.length, 5);
  for (const [index, request] of server.requests.entries()) {
    const name = index === 4 ? "send_message" : names[index];
    assert.equal(request.method, "POST");
    assert.equal(request.url, `${BASE}/${name}`);
    assert.equal(request.headers.authorization, `Bearer ${TOKEN}`);
    assert.equal(request.headers["content-type"], "application/json");
    const headerNames = request.rawHeaders.filter((_, i) => i % 2 === 0).map(value => value.toLowerCase());
    assert.equal(headerNames.filter(name => name === "content-type").length, 1);
    assert.equal(headerNames.filter(name => name === "authorization").length, 1);
    assert.ok(headerNames.every(name => name !== "origin" && !name.startsWith("sec-fetch-")));
    assert.equal(request.headers["content-length"], String(request.body.length));
    assert.equal(request.body.includes(TOKEN), false);
    assert.deepEqual(JSON.parse(request.body), payloads[index === 4 ? 2 : index]);
    const key = isWrite(name) ? createHash("sha256").update(name + "native-call-1").digest("hex") : undefined;
    assert.equal(request.headers["idempotency-key"], key);
  }
  assert.equal(server.requests[2].headers["idempotency-key"], server.requests[4].headers["idempotency-key"]);
  assert.notEqual(server.requests[2].headers["idempotency-key"], server.requests[3].headers["idempotency-key"]);
});

test("create reuse and small write receipts are preserved without client projection", async t => {
  const receipt = { member_id: "existing", reused: true, join_message_id: "" };
  const server = await clientFor(t, (_, response) => json(response, receipt));
  assert.deepEqual((await server.invoke("create_member", payloads[3], "reuse")).json, receipt);
  assert.ok(Buffer.byteLength(JSON.stringify(receipts[2])) < 200);
  assert.ok(Buffer.byteLength(JSON.stringify(receipt)) < 100);
});

test("config is read only once and its lifetime remains with the launcher", async t => {
  const server = await loopback(t, (_, response) => json(response, receipts[0]));
  const path = await privateConfig(t, { endpoint: server.endpoint });
  const invoke = createClient(path);
  await writeFile(path, `not config: ${TOKEN}`);
  await unlink(path);
  assert.deepEqual((await invoke("list_members", {}, "one-read")).json, receipts[0]);
});

test("IPv6 numeric loopback is used directly without DNS or browser metadata", async t => {
  const server = await loopback(t, (_, response) => json(response, receipts[1]), "::1");
  const invoke = createClient(await privateConfig(t, { endpoint: server.endpoint }));
  await invoke("read_channel", {}, "ipv6");
  assert.equal(server.requests.length, 1);
  assert.match(server.requests[0].headers.host, /^\[::1\]:/);
});

test("request budget counts UTF8 JSON bytes including escaping at 64 KiB", async t => {
  const server = await clientFor(t, (_, response) => json(response, receipts[2], 201));
  for (const escaping of [false, true]) {
    const prefix = escaping ? '\\"\n\u0000' : "";
    const baseBytes = Buffer.byteLength(JSON.stringify({ content: prefix }));
    const room = BUDGET - baseBytes;
    const content = prefix + "中".repeat(Math.floor(room / 3)) + "x".repeat(room % 3);
    assert.equal(Buffer.byteLength(JSON.stringify({ content })), BUDGET);
    await server.invoke("send_message", { content }, `exact-${escaping}`);
    await assert.rejects(server.invoke("send_message", { content: content + "x" }, "too-big"),
      error => safeFailure(error, /请求.*64 KiB/));
  }
  assert.equal(server.requests.length, 2);
  assert.ok(server.requests.every(request => request.body.length === BUDGET));
});

test("response preserves exact successful JSON whitespace, escaping, fields and 64 KiB UTF8 bytes", async t => {
  const exact = sizedJSON(BUDGET);
  const bodies = [exact, '{ "items": [{"text":"\\u4e2d\\n\\\"", "extra": true}], "next_cursor": "" }\n'];
  let index = 0;
  const server = await clientFor(t, (_, response) => {
    response.writeHead(200, { "Content-Type": "application/json; charset=utf-8" });
    const body = Buffer.from(bodies[index++]);
    // Split a multibyte character over chunks; count bytes, not decoded characters.
    response.write(body.subarray(0, 12));
    setImmediate(() => response.end(body.subarray(12)));
  });
  for (const body of bodies) {
    const result = await server.invoke("read_channel", {}, "exact-response");
    assert.equal(result.text, body);
    assert.deepEqual(result.json, JSON.parse(body));
  }
});

test("response overflow is explicit for both Content-Length and streamed bodies; closes the body", async t => {
  for (const declared of [true, false]) {
    let closed;
    const server = await clientFor(t, (_, response) => {
      closed = once(response, "close");
      response.writeHead(200, { "Content-Type": "application/json",
        ...(declared ? { "Content-Length": BUDGET + 1 } : {}) });
      response.write(sizedJSON(BUDGET + 1));
    });
    await assert.rejects(server.invoke("read_channel", {}, "oversized"),
      error => safeFailure(error, /tool_result_too_large/));
    await closed;
    assert.equal(server.requests.length, 1);
  }
});

test("HTTP 413 tool_result_too_large is explicit, never truncated or retried", async t => {
  const server = await clientFor(t, (_, response) => json(response, {
    error: { code: "tool_result_too_large", message: `untrusted: ${TOKEN}` }, extra: TOKEN,
  }, 413));
  await assert.rejects(server.invoke("list_members", {}, "oversized-server"), error => {
    safeFailure(error, /tool_result_too_large/);
    assert.match(error.message, /减小 limit/);
    assert.match(error.message, /不会截断/);
    return true;
  });
  assert.equal(server.requests.length, 1);
});

test("HTTP cancellation and deadline errors retain unknown outcomes and the original replay identity", async t => {
  for (const [code, status, token] of [
    ["request_canceled", 408, TOKEN], ["deadline_exceeded", 504, TOKEN],
    ["request_canceled", 408, "request_canceled"],
  ]) {
    const server = await loopback(t, (_, response) => json(response, {
      error: { code, message: `untrusted operationError: ${token}` }, extra: token,
    }, status));
    const invoke = createClient(await privateConfig(t, { endpoint: server.endpoint, token }));
    const callId = "uncertain-write";
    await assert.rejects(invoke("send_message", payloads[2], callId), error => {
      safeFailure(error, /结果未知/, [token]);
      assert.match(error.message, /先对账/);
      assert.match(error.message, /不要自动重试/);
      assert.match(error.message, /保留.*调用标识.*幂等键/);
      if (token !== code) assert.ok(error.message.includes(code));
      assert.ok(!error.message.includes("untrusted operationError"));
      return true;
    });
    assert.equal(server.requests.length, 1);
    assert.equal(server.requests[0].headers["idempotency-key"],
      createHash("sha256").update("send_message" + callId).digest("hex"));
  }
});

test("HTTP member_capacity errors allow retry only after capacity becomes available", async t => {
  const server = await clientFor(t, (_, response) => json(response, {
    error: { code: "member_capacity", message: `untrusted capacity advice: ${TOKEN}` },
  }, 409));
  await assert.rejects(server.invoke("create_member", payloads[3], "capacity"), error => {
    safeFailure(error, /容量已满/);
    assert.match(error.message, /仅.*空余容量.*重试/);
    assert.match(error.message, /member_capacity/);
    assert.ok(!error.message.includes("untrusted capacity advice"));
    return true;
  });
  assert.equal(server.requests.length, 1);
});

test("valid error JSON retains only bounded stable code and a local Chinese safe message", async t => {
  let index = 0;
  const codes = ["conflict", TOKEN, `bad\n${TOKEN}`, "a".repeat(65)];
  const server = await clientFor(t, (_, response) => json(response, {
    error: { code: codes[index++], message: `secret detail ${TOKEN}` }, token: TOKEN,
  }, 409));
  for (const code of codes) {
    await assert.rejects(server.invoke("send_message", payloads[2], "error"), error => {
      safeFailure(error, code === "conflict" ? /conflict/ : /请求失败/);
      assert.ok(error.message.length < 200);
      return true;
    });
  }
  assert.equal(server.requests.length, codes.length);
});

test("even a valid stable server error code cannot echo a lowercase capability", async t => {
  const token = "private_capability_735197";
  const server = await loopback(t, (_, response) => json(response, {
    error: { code: `denied_${token}`, message: token },
  }, 403));
  const invoke = createClient(await privateConfig(t, { endpoint: server.endpoint, token }));
  await assert.rejects(invoke("send_message", payloads[2], "secret-code"),
    error => safeFailure(error, /请求失败/, [token]));
});

test("capabilities that collide with local error text remain secret on every failure path", async t => {
  const server = await loopback(t, (_, response) => json(response, { items: ["x".repeat(BUDGET)], next_cursor: "" }));
  const invoke = createClient(await privateConfig(t, { endpoint: server.endpoint, token: "tool_result_too_large" }));
  await assert.rejects(invoke("read_channel", {}, "oversized-secret"),
    error => safeFailure(error, /结果.*限制/, ["tool_result_too_large"]));
  const smallToken = createClient(await privateConfig(t, { endpoint: server.endpoint, token: "KiB" }));
  await assert.rejects(smallToken("send_message", { content: "x".repeat(BUDGET) }, "oversized-request"),
    error => safeFailure(error, /请求.*限制/, ["KiB"]));
});

test("malformed, HTML, unsafe JSON and unexpected-status failures never expose bodies or headers", async t => {
  const cases = [
    [500, "text/html", `<html>${TOKEN}</html>`],
    [502, "application/json", `broken ${TOKEN}`],
    [201, "text/html", `<html>${TOKEN}</html>`],
    [201, "application/json", Buffer.from([0xff])],
    [201, "application/json", Buffer.from('\ufeff{}')],
    [201, "application/json", '{"__proto__":{"polluted":true}}'],
    [201, "application/json", '{"constructor":{}}'],
    [201, "application/json", '{"value":1e400}'],
    [201, "application/json", `{"echo":"${TOKEN}"}`],
    [201, "application/json", JSON.stringify({ echo: TOKEN }).replace(/N/g, "\\u004e")],
    [204, "application/json", ""],
    [202, "application/json", JSON.stringify({ echo: TOKEN })],
    [400, "application/json", JSON.stringify({ error: TOKEN })],
    [201, "application/json", "{}", "gzip"],
  ];
  let index = 0;
  const server = await clientFor(t, (_, response) => {
    const [status, contentType, body, encoding] = cases[index++];
    response.writeHead(status, { "Content-Type": contentType, "X-Secret": TOKEN,
      ...(encoding ? { "Content-Encoding": encoding } : {}) });
    response.end(body);
  });
  for (const _ of cases) {
    await assert.rejects(server.invoke("send_message", payloads[2], "malformed"),
      error => safeFailure(error, /无效响应|敏感数据/));
  }
  assert.equal(server.requests.length, cases.length);
  assert.equal({}.polluted, undefined);
});

test("redirects are rejected without following or leaking authorization", async t => {
  const target = await loopback(t, (_, response) => json(response, receipts[2], 201));
  for (const status of [301, 302, 303, 307, 308]) {
    const server = await clientFor(t, (_, response) => {
      response.writeHead(status, { Location: `${target.endpoint}/${TOKEN}`, "Content-Type": "text/html" });
      response.end(TOKEN);
    });
    await assert.rejects(server.invoke("send_message", payloads[2], "redirect"),
      error => safeFailure(error, /重定向/));
    assert.equal(server.requests.length, 1);
  }
  assert.equal(target.requests.length, 0);
});

test("empty IDs and invalid tool names are rejected before any network request", async t => {
  const server = await clientFor(t, (_, response) => json(response, receipts[2], 201));
  for (const id of ["", " ", null, 1]) {
    await assert.rejects(server.invoke("send_message", payloads[2], id), error => safeFailure(error, /调用标识/));
  }
  await assert.rejects(server.invoke("../other", {}, "valid"), error => safeFailure(error, /工具/));
  assert.equal(server.requests.length, 0);
});

test("pre-abort sends nothing; abort after send closes the body and reports unknown without retry", async t => {
  const controller = new AbortController();
  controller.abort(new Error(TOKEN));
  let responseReceived;
  const received = new Promise(resolve => { responseReceived = resolve; });
  const server = await clientFor(t, (_, response) => {
    response.writeHead(200, { "Content-Type": "application/json" });
    responseReceived(response);
  });
  await assert.rejects(server.invoke("send_message", payloads[2], "pre-abort", controller.signal),
    error => safeFailure(error, /取消.*未发送/));
  assert.equal(server.requests.length, 0);

  const live = new AbortController();
  const pending = server.invoke("send_message", payloads[2], "in-flight", live.signal);
  const response = await received;
  const closed = once(response, "close");
  live.abort(new Error(TOKEN));
  await assert.rejects(pending, error => safeFailure(error, /取消.*结果未知/));
  await closed;
  assert.equal(server.requests.length, 1);
});

test("disconnect before receipt or mid-body reports unknown and makes only one attempt", async t => {
  for (const partial of [false, true]) {
    const server = await clientFor(t, (request, response) => {
      if (partial) {
        response.writeHead(201, { "Content-Type": "application/json", "Content-Length": "100" });
        response.write('{"message_id":"');
        setImmediate(() => response.destroy(new Error(TOKEN)));
      } else request.socket.destroy();
    });
    await assert.rejects(server.invoke("send_message", payloads[2], "disconnect"),
      error => safeFailure(error, /结果未知/));
    assert.equal(server.requests.length, 1);
  }
});

test("real 25-second total timeout bounds even a dripping response, closes it and never retries", { timeout: 35000 }, async t => {
  let closed;
  const server = await clientFor(t, (_, response) => {
    closed = once(response, "close");
    response.writeHead(201, { "Content-Type": "application/json" });
    response.write("{");
    const timer = setInterval(() => response.write(" "), 1000);
    response.once("close", () => clearInterval(timer));
  });
  const started = Date.now();
  await assert.rejects(server.invoke("send_message", payloads[2], "timeout"),
    error => safeFailure(error, /超时.*结果未知/));
  assert.ok(Date.now() - started >= 24000);
  assert.ok(Date.now() - started < 34000);
  await closed;
  assert.equal(server.requests.length, 1);
});
