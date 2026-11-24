import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { chmod, mkdir, readFile, readdir, stat, symlink, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
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
const claim = "synthetic-claim-for-private-evidence-only";
const journalName = "run-explanation-bridge-attempt";

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
  const path = await privateConfig(t);
  const runDir = join(dirname(path), runID);
  await mkdir(runDir, { mode: 0o700 });
  const journal = join(runDir, journalName);
  const config = { endpoint, token: TOKEN, run_explanation: runID, run_explanation_journal: journal,
    run_explanation_claim_fingerprint: `${Buffer.byteLength(claim)}:${sha(claim)}`, ...extra };
  await writeFile(path, JSON.stringify(config));
  return { path, digest: sha(await readFile(path)), journal, runDir };
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
    { intake_assessment_action: "read_only_investigation" }, { investigation_tools: ["read_investigation_channel", "submit_result"] },
    { run_explanation_journal: undefined }, { run_explanation_journal: "/other-run/run-explanation-bridge-attempt" },
    { run_explanation_claim_fingerprint: undefined }, { run_explanation_claim_fingerprint: claim }]) {
    const { path, digest } = await issued(t, server.endpoint, extra);
    assert.throws(() => openBridge(path, digest), error => safeFailure(error, /配置/));
  }
  const loaded = await load(fx.path, fx.digest);
  assert.deepEqual(loaded.errors, []);
  const tool = loaded.extensions[0].tools.get(op).definition;
  assert.deepEqual(tool.parameters.required, ["text"]);
  assert.equal(tool.parameters.additionalProperties, false);
  for (const bad of [{ ...payload, member_id: "forged" }, { ...payload, idempotency_key: "forged" }, { ...payload, path: fx.journal },
    { text: 42 }, { text: null }, { text: "" }, { text: "\u0085" }, { text: "\ud800" }, { text: "no\x00" }, { text: payload.text + "x" }]) {
    assert.throws(() => tool.prepareArguments(bad), error => safeFailure(error, /参数/));
    await assert.rejects(tool.execute(id, bad), error => safeFailure(error, /未发送/));
  }
  for (const sensitive of [TOKEN, claim]) {
    await assert.rejects(tool.execute(id, { text: ` \n${sensitive}\t` }), /未发送/);
  }
  assert.deepEqual(await readdir(fx.runDir), []);
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
  assert.equal(await readFile(fx.journal, "utf8"), `${expectedKey}\n${payload.text}`);
  await assert.rejects(tool.execute("different-call", payload), /原运行说明.*未发送/);
  assert.equal(server.requests.length, 2);
  await writeFile(fx.path, "\n", { flag: "a" });
  assert.throws(() => openBridge(fx.path, fx.digest), error => safeFailure(error, /配置/));
});

for (const mode of ["pre-attempt loss", "same-call replay loss", "abort after sending"]) {
  test(`public explanation ${mode} produces only the original typed unknown result`, async t => {
    const abort = new AbortController();
    let calls = 0;
    const server = await loopback(t, (_, response) => {
      if (++calls === 1 && mode === "same-call replay loss") return json(response, ack, 201);
      if (mode === "abort after sending") abort.abort(new Error(TOKEN));
      response.destroy(); // No trustworthy receipt reaches Pi.
    });
    const { path, digest } = await issued(t, server.endpoint);
    const loaded = await load(path, digest);
    assert.deepEqual(loaded.errors, []);
    const tools = loaded.extensions[0].tools;
    const tool = tools.get(op).definition;
    await assert.rejects(tool.execute(id, payload, AbortSignal.abort()), error => safeFailure(error, /未发送/));
    if (mode === "same-call replay loss") {
      assert.deepEqual((await tool.execute(id, payload)).details, { [RUN_EXPLANATION_ACK]: expectedKey });
    }
    const result = await tool.execute(id, payload, abort.signal);
    assert.equal(result.isError, true);
    assert.match(result.content[0].text, /结果未知.*不要自动重试/);
    assert.deepEqual(result.details, { agent_workspace_run_explanation_unknown_v1: {
      idempotency_key: expectedKey, text_sha256: sha(payload.text),
    } });
    assert.ok(!JSON.stringify(result).includes(TOKEN) && !JSON.stringify(result).includes("🙂"));
    assert.deepEqual(await tool.execute(id, payload, AbortSignal.abort()), result);
    await assert.rejects(tool.execute(id, { text: payload.text.trim() }), /阻止后续写入/);
    await assert.rejects(tool.execute("new-id", payload), /阻止后续写入/);
    await assert.rejects(tools.get("send_message").definition.execute("later", { content: "later" }), /阻止后续写入/);
    assert.equal(server.requests.length, mode === "same-call replay loss" ? 2 : 1);
    for (const sent of server.requests) {
      assert.equal(sent.url, `${BASE}/${op}`);
      assert.equal(sent.headers["idempotency-key"], expectedKey);
      assert.equal(sent.body.toString(), JSON.stringify(payload));
    }
  });
}

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
    const { path, digest, runDir, journal } = await issued(t, server.endpoint);
    const { invoke } = openBridge(path, digest);
    const mutable = { ...payload };
    const first = invoke(op, mutable, id, abort.signal).catch(error => error);
    const resolve = await pending;
    mutable.text = "changed";
    // Exceed the native frame count limit while one request is on the wire.
    for (let i = 0; i < 600; i++) {
      await assert.rejects(invoke(op, { text: `rejected ${i}` }, `parallel-${i}`), error => safeFailure(error, /尚未完成.*未发送/));
    }
    await assert.rejects(invoke("send_message", { content: "second write" }, id), error => safeFailure(error, /尚未完成.*未发送/));
    assert.equal(await readFile(journal, "utf8"), `${expectedKey}\n${payload.text}`);
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
    assert.deepEqual(await readdir(runDir), [journalName]);
    assert.equal((await stat(journal)).mode & 0o777, 0o600);
    assert.equal(await readFile(journal, "utf8"), `${expectedKey}\n${payload.text}`);
    assert.deepEqual((await invoke("read_channel", {}, "read")).json, { items: [], next_cursor: "" });
  });
}

test("explanation cannot send without a private exclusive journal; ordinary tools do not use it", async t => {
  const server = await loopback(t, (_, response) => json(response, { items: [] }));
  const { path, digest, runDir, journal } = await issued(t, server.endpoint);
  const { invoke } = openBridge(path, digest);
  await chmod(runDir, 0o755);
  await assert.rejects(invoke(op, payload, id), /无法安全保留.*未发送/);
  await chmod(runDir, 0o700);
  const target = join(dirname(runDir), "must-not-touch");
  await writeFile(target, "original", { mode: 0o600 });
  await symlink(target, journal);
  await assert.rejects(invoke(op, payload, id), /无法安全保留.*未发送/);
  assert.equal(await readFile(target, "utf8"), "original");
  assert.equal(server.requests.length, 0);
  assert.deepEqual((await invoke("read_channel", {}, "read")).json, { items: [] });
  assert.equal(server.requests.length, 1);
});

test("definite rejection clears only a first reservation, not a previously sent attempt", async t => {
  const server = await loopback(t, (_, response) => {
    if (server.requests.length === 2) json(response, ack, 201);
    else json(response, { error: { code: "conflict", message: "definite test rejection" } }, 409);
  });
  const { path, digest, runDir, journal } = await issued(t, server.endpoint);
  const { invoke } = openBridge(path, digest);
  await assert.rejects(invoke(op, payload, "not-sent", AbortSignal.abort()), /尚未发送/);
  assert.deepEqual(await readdir(runDir), []);
  await assert.rejects(invoke(op, { text: "first rejected" }, "rejected-id"), /conflict/);
  assert.deepEqual(await readdir(runDir), []);
  assert.equal((await invoke(op, payload, id)).explanationKey, expectedKey);
  await assert.rejects(invoke(op, payload, id), /conflict/);
  assert.equal(await readFile(journal, "utf8"), `${expectedKey}\n${payload.text}`);
  assert.equal(server.requests.length, 3);
});