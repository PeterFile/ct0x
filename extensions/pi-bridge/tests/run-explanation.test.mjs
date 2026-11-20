import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFile, writeFile } from "node:fs/promises";
import { dirname } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { createClient, openBridge, PUBLISH_RUN_EXPLANATION as op, RUN_EXPLANATION_ACK } from "../bridge.mjs";
import { BASE, TOKEN, json, loopback, privateConfig, safeFailure } from "./helpers.mjs";
import { loadExtensions } from "./pi-loader.mjs";

const entry = fileURLToPath(new URL("../index.ts", import.meta.url));
const runID = "fixture-turn";
const id = "native-call|0";
const sha = bytes => createHash("sha256").update(bytes).digest("hex");
const expectedKey = sha(`${op}\0${runID}\0${id}`);
const ack = { explanation_id: `public_explanation:${runID}`, created_at: "2026-11-20T00:00:00.012345678Z", text_omitted: true };
const payload = { text: ` \n${"🙂".repeat(255)} \t` };

async function load(path, digest) {
  const keys = ["AGENT_WORKSPACE_MEMBER_TOOL_CONFIG", "AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256"];
  const old = keys.map(key => process.env[key]);
  const set = (key, value) => value === undefined ? delete process.env[key] : (process.env[key] = value);
  try {
    keys.forEach((key, i) => set(key, [path, digest][i]));
    return await loadExtensions([entry], dirname(path));
  } finally { keys.forEach((key, i) => set(key, old[i])); }
}

async function issued(t, endpoint, extra = {}) {
  const path = await privateConfig(t, { endpoint, run_explanation: runID, ...extra });
  return { path, digest: sha(await readFile(path)) };
}

test("public explanation registers in the real Pi loader only from an exact issued config", async t => {
  const server = await loopback(t, (_, response) => json(response, ack, 201));
  const normal = await privateConfig(t, { endpoint: server.endpoint });
  const legacy = await load(normal, sha(await readFile(normal)));
  assert.deepEqual(legacy.errors, []);
  assert.equal(legacy.extensions[0].tools.has(op), false);
  await assert.rejects(createClient(normal)(op, payload, id), error => safeFailure(error, /未发送/));
  const fx = await issued(t, server.endpoint);
  for (const bad of [undefined, "a".repeat(64)]) {
    const denied = await load(fx.path, bad);
    assert.equal(denied.extensions.length, 0);
    assert.equal(denied.errors.length, 1);
  }
  for (const extra of [{ run_explanation: true }, { continue_work: true }, { discussion_only_excerpt: true },
    { intake_assessment_action: "read_only_investigation" }, { investigation_tools: ["read_investigation_channel", "submit_result"] }]) {
    const { path, digest } = await issued(t, server.endpoint, extra);
    assert.throws(() => openBridge(path, digest), error => safeFailure(error, /配置/));
  }
  const loaded = await load(fx.path, fx.digest);
  assert.deepEqual(loaded.errors, []);
  const tool = loaded.extensions[0].tools.get(op).definition;
  assert.deepEqual(tool.parameters.required, ["text"]);
  assert.equal(tool.parameters.additionalProperties, false);
  for (const bad of [{ ...payload, member_id: "forged" }, { ...payload, idempotency_key: "forged" },
    { text: 42 }, { text: null }, { text: "" }, { text: "\u0085" }, { text: "\ud800" }, { text: "no\x00" }, { text: payload.text + "x" }]) {
    assert.throws(() => tool.prepareArguments(bad), error => safeFailure(error, /参数/));
    await assert.rejects(tool.execute(id, bad), error => safeFailure(error, /未发送/));
  }
  assert.equal(server.requests.length, 0);
  for (let i = 0; i < 2; i++) {
    const reply = await tool.execute(id, payload);
    assert.deepEqual(reply, { content: [{ type: "text", text: JSON.stringify(ack) }], details: { [RUN_EXPLANATION_ACK]: expectedKey } });
    const sent = server.requests.at(-1);
    assert.equal(sent.url, `${BASE}/${op}`);
    assert.equal(sent.headers.authorization, `Bearer ${TOKEN}`);
    assert.equal(sent.headers["idempotency-key"], expectedKey);
    assert.equal(sent.body.toString(), JSON.stringify(payload));
  }
  await writeFile(fx.path, "\n", { flag: "a" });
  assert.throws(() => openBridge(fx.path, fx.digest), error => safeFailure(error, /配置/));
});

for (const mode of ["unconfirmed storage", "abort after send", "unmatched acknowledgement"]) {
  test(`public explanation ${mode} retains the first call and latches writes`, async t => {
    const abort = new AbortController();
    let admitted;
    const pending = new Promise(resolve => { admitted = resolve; });
    const server = await loopback(t, (request, response) => {
      if (request.url.endsWith("/read_channel")) return json(response, { items: [], next_cursor: "" });
      admitted(() => {
        if (mode === "abort after send") abort.abort(new Error(TOKEN));
        else if (mode === "unconfirmed storage") json(response, { error: { code: "run_explanation_unknown", message: "未确认" } }, 503);
        else json(response, { ...ack, explanation_id: "public_explanation:wrong-turn" }, 201);
      });
    });
    const { path, digest } = await issued(t, server.endpoint);
    const { invoke } = openBridge(path, digest);
    const mutable = { ...payload };
    const first = invoke(op, mutable, id, abort.signal).catch(error => error);
    const resolve = await pending;
    mutable.text = "changed";
    await assert.rejects(invoke(op, payload, "parallel-id"), error => safeFailure(error, /尚未完成.*未发送/));
    await assert.rejects(invoke("send_message", { content: "second write" }, id), error => safeFailure(error, /尚未完成.*未发送/));
    resolve();
    const original = await first;
    safeFailure(original, /结果未知.*幂等键和原文.*不要自动重试/);
    // Even an already aborted exact replay keeps the original unknown.
    await assert.rejects(invoke(op, payload, id, AbortSignal.abort()), error => error === original);
    await assert.rejects(invoke(op, mutable, id), error => safeFailure(error, /阻止后续写入/));
    await assert.rejects(invoke(op, payload, "new-id"), error => safeFailure(error, /阻止后续写入/));
    await assert.rejects(invoke("send_message", { content: "second write" }, id), error => safeFailure(error, /阻止后续写入/));
    assert.equal(server.requests.length, 1);
    assert.equal(server.requests[0].body.toString(), JSON.stringify(payload));
    assert.equal(server.requests[0].headers["idempotency-key"], expectedKey);
    assert.deepEqual((await invoke("read_channel", {}, "read")).json, { items: [], next_cursor: "" });
  });
}