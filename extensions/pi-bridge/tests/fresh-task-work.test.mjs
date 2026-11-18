import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { EventEmitter } from "node:events";
import fs from "node:fs";
import { readFile, rename, writeFile } from "node:fs/promises";
import http from "node:http";
import { syncBuiltinESMExports } from "node:module";
import { dirname } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { BASE, TOKEN, privateConfig, safeFailure } from "./helpers.mjs";
import { loadExtensions, loadSessionSDK } from "./pi-loader.mjs";

const typed = fileURLToPath(new URL("../fresh-task-work.ts", import.meta.url));
const ordinary = fileURLToPath(new URL("../index.ts", import.meta.url));
const profile = ["read_work_inputs", "ack_work_input", "hand_back_work_input", "submit_result"];
const sha = bytes => createHash("sha256").update(bytes).digest("hex");
const stamp = "2026-11-18T00:00:00.123456789Z";
const id = "fresh-tool-call";
const payloads = {
  read_work_inputs: { input_id: "fixture", offset: 5, max_bytes: 4 },
  ack_work_input: { input_id: "fixture" },
  hand_back_work_input: { input_id: "other", reason: "超出文本授权" },
  submit_result: { summary: "文本交付" },
};
const replies = {
  read_work_inputs: { input_id: "fixture", offset: 5, next_offset: 9, byte_count: 4, complete: false, text: "🧪" },
  ack_work_input: { input_id: "fixture", disposition: "confirmed" },
  hand_back_work_input: { input_id: "other", disposition: "returned" },
  submit_result: { submission_id: "submission", created_at: stamp, artifact_version_count: 0, summary_omitted: true },
};

async function load(entry, path, digest) {
  const keys = ["AGENT_WORKSPACE_MEMBER_TOOL_CONFIG", "AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256"];
  const old = keys.map(key => process.env[key]);
  const set = (key, value) => value === undefined ? delete process.env[key] : (process.env[key] = value);
  try {
    keys.forEach((key, i) => set(key, [path, digest][i]));
    return await loadExtensions([entry], dirname(path));
  } finally { keys.forEach((key, i) => set(key, old[i])); }
}

function definitions(loaded) {
  assert.deepEqual(loaded.errors, []);
  assert.deepEqual(loaded.warnings, []);
  assert.equal(loaded.extensions.length, 1);
  return new Map([...loaded.extensions[0].tools].map(([name, t]) => [name, t.definition]));
}

function reply(name) {
  return [name === "submit_result" ? 201 : 200, JSON.stringify(replies[name])];
}

// Replace http.request before the real SDK loads the extension. No bind(),
// sockets, subprocess, resolver or actual endpoint are used by these tests.
function fakeHTTP(t, respond = name => reply(name)) {
  const sent = [];
  const original = http.request;
  http.request = (options, ready) => {
    assert.equal(options.hostname, "127.0.0.1");
    assert.equal(String(options.port), "7310");
    assert.equal(options.method, "POST");
    assert.equal(options.agent, false);
    assert.equal(options.headers.Authorization, `Bearer ${TOKEN}`);
    assert.ok(options.path.startsWith(`${BASE}/`));
    const name = options.path.slice(BASE.length + 1);
    const req = new EventEmitter();
    req.destroy = () => {};
    req.end = bytes => {
      sent.push({ name, body: bytes.toString(), key: options.headers["Idempotency-Key"] });
      queueMicrotask(() => {
        const [statusCode, body, headers = {}] = respond(name);
        const res = Object.assign(new EventEmitter(), { statusCode, complete: false, destroy() {},
          headers: { "content-type": "application/json", ...headers } });
        ready(res);
        if (body === undefined) return res.emit("aborted");
        res.emit("data", Buffer.from(body));
        res.complete = true;
        res.emit("end");
      });
    };
    return req;
  };
  syncBuiltinESMExports();
  t.after(() => { http.request = original; syncBuiltinESMExports(); });
  return sent;
}

async function fixture(t) {
  const path = await privateConfig(t);
  const issued = await readFile(path);
  const loaded = await load(typed, path, sha(issued));
  const tools = definitions(loaded);
  assert.deepEqual([...tools.keys()], profile);
  const invoke = (name, args = payloads[name], toolID = id, signal) => tools.get(name).execute(toolID, args, signal);
  return { path, loaded, invoke };
}

function check(result, value) {
  assert.deepEqual(result, { content: [{ type: "text", text: JSON.stringify(value) }], details: undefined });
}

test("real Pi 1.4.0 loader and SDK activate only the four typed tools", async t => {
  const sent = fakeHTTP(t);
  const fx = await fixture(t);
  const { createAgentSession, SessionManager, SettingsManager } = await loadSessionSDK();
  const dir = dirname(fx.path);
  const deny = name => () => assert.fail(`unexpected ${name}`);
  t.mock.method(globalThis, "fetch", deny("network"));
  const { session } = await createAgentSession({
    cwd: dir, agentDir: dir,
    model: { id: "fixture", name: "Synthetic", provider: "synthetic", api: "synthetic-no-network", baseUrl: "http://127.0.0.1:1",
      reasoning: false, input: ["text"], contextWindow: 8192, maxTokens: 1, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } },
    thinkingLevel: "off", noTools: "builtin", tools: profile,
    modelRuntime: new Proxy({}, { get: (_target, name) => deny(String(name)) }),
    sessionManager: SessionManager.inMemory(dir),
    settingsManager: SettingsManager.inMemory({ retry: { enabled: false }, cacheWarming: "off", enableInstallTelemetry: false }),
    resourceLoader: {
      getExtensions: () => fx.loaded,
      getSkills: () => ({ skills: [], diagnostics: [] }), getPrompts: () => ({ prompts: [], diagnostics: [] }),
      getThemes: () => ({ themes: [], diagnostics: [] }), getAgentsFiles: () => ({ agentsFiles: [] }),
      getSystemPrompt: () => "No prompt sent.", getSystemPromptSource: () => undefined,
      getAppendSystemPrompt: () => [], getAppendSystemPromptSources: () => [], extendResources: deny("resources"), reload: deny("reload"),
    },
  });
  try {
    session.prompt = session.agent.prompt = session.agent.streamFn = deny("prompt or stream");
    await session.bindExtensions({ mode: "print" });
    session.setActiveToolsByName([...profile, "read", "bash", "list_members", "send_message", "report_work", "hand_back_work"]);
    assert.deepEqual(session.getActiveToolNames(), profile);
    assert.deepEqual(session.getCallableToolNames(), profile);
    for (const tool of session.agent.state.tools) {
      const name = tool.name;
      assert.throws(() => tool.prepareArguments({ ...payloads[name], claim: "untrusted" }), /参数.*未发送/);
      check(await tool.execute(id, tool.prepareArguments(payloads[name])), replies[name]);
    }
    assert.deepEqual(session.messages, []);
  } finally { session.dispose(); }
  assert.deepEqual(sent, profile.map(name => ({ name, body: JSON.stringify(payloads[name]),
    key: name === profile[0] ? undefined : sha(name + id) })));
});

test("typed config hashes the buffer it consumes and has no digest-less fallback", async t => {
  const sent = fakeHTTP(t);
  const path = await privateConfig(t);
  const bytes = await readFile(path);
  const digest = sha(bytes);
  for (const bad of [undefined, "0".repeat(64), digest.toUpperCase()]) {
    const loaded = await load(typed, path, bad);
    assert.equal(loaded.extensions.length, 0);
    assert.equal(loaded.errors.length, 1);
    assert.match(loaded.errors[0].error, /配置/);
    assert.ok(!loaded.errors[0].error.includes(TOKEN));
  }
  const alternate = `${path}.other`;
  await writeFile(alternate, JSON.stringify({ endpoint: `http://127.0.0.1:7311${BASE}`, token: "alternate" }), { mode: 0o600 });
  const original = fs.openSync;
  let reads = 0;
  const swap = `${path}.rename`;
  await writeFile(swap, await readFile(alternate), { mode: 0o600 });
  fs.openSync = (name, ...args) => {
    const fd = original(name, ...args);
    if (name === path) {
      reads++;
      fs.renameSync(swap, path); // Replacement after open cannot substitute bytes.
    }
    return fd;
  };
  syncBuiltinESMExports();
  let tools;
  try { tools = definitions(await load(typed, path, digest)); }
  finally { fs.openSync = original; syncBuiltinESMExports(); }
  assert.equal(reads, 1);
  check(await tools.get(profile[0]).execute(id, payloads[profile[0]]), replies[profile[0]]);
  assert.equal(sent.length, 1);
  assert.equal((await load(typed, path, digest)).extensions.length, 0);
  await rename(alternate, path);
});

for (const [label, outcome] of [
  ["uncertain server error", [500, '{"error":{"code":"internal_error","message":"do not retry"}}']],
  ["aborted response", [200, undefined]],
  ["redirect", [307, "{}"]],
  ["oversize response", [200, " ".repeat(65537)]],
  ["wrong input receipt", [200, '{"input_id":"other","disposition":"confirmed"}']],
  ["ambiguous rejection", [409, '{"error":{"code":"conflict","code":"conflict","message":"duplicate"}}']],
]) {
  test(`${label} latches all writes without resending the unknown command`, async t => {
    const sent = fakeHTTP(t, name => name === "ack_work_input" ? outcome : reply(name));
    const { invoke } = await fixture(t);
    let first;
    await assert.rejects(invoke("ack_work_input"), error => { first = error; return safeFailure(error, /未知.*不要自动重试/); });
    await assert.rejects(invoke("ack_work_input"), error => error === first);
    await assert.rejects(invoke("ack_work_input", { input_id: "other" }), /阻止后续写入/);
    check(await invoke("read_work_inputs"), replies.read_work_inputs);
    for (const name of profile.slice(2)) await assert.rejects(invoke(name), /阻止后续写入/);
    assert.deepEqual(sent.map(r => r.name), ["ack_work_input", "read_work_inputs"]);
  });
}

test("definite B rejection and local validation do not close a new write", async t => {
  const sent = fakeHTTP(t, name => name === "ack_work_input" ? [409, '{"error":{"code":"conflict","message":"rejected"}}'] : reply(name));
  const { invoke } = await fixture(t);
  await assert.rejects(invoke("submit_result", { summary: "text", artifact_versions: [] }), /参数.*未发送/);
  await assert.rejects(invoke("ack_work_input", payloads.ack_work_input, "\ud800"), /调用无效.*未发送/);
  await assert.rejects(invoke("ack_work_input", payloads.ack_work_input, id, AbortSignal.abort()), /取消.*未发送/);
  await assert.rejects(invoke("ack_work_input"), /被拒绝.*conflict/);
  check(await invoke("submit_result"), replies.submit_result);
  assert.deepEqual(sent.map(r => r.name), ["ack_work_input", "submit_result"]);
});

test("ordinary path-only loader still consumes the #181 old read and mutation receipts", async t => {
  const sent = fakeHTTP(t, name => name === "read_work_inputs" ? [200, '{"items":[{"input_id":"legacy","content":"legacy input"}]}'] :
    [201, '{"input_id":"legacy"}']);
  const path = await privateConfig(t);
  const tools = definitions(await load(ordinary, path, undefined));
  assert.ok(tools.has("list_members") && tools.has("search_history"));
  for (const [name, args, pattern] of [["read_work_inputs", {}, /legacy input/], ["ack_work_input", { input_id: "legacy" }, /"legacy"/]]) {
    const result = await tools.get(name).execute(id, args);
    assert.match(result.content[0].text, pattern);
  }
  assert.deepEqual(sent.map(r => r.body), ["{}", '{"input_id":"legacy"}']);
});

test("published ordinary entry verifies issued bytes independently of typed pair", { skip: process.env.TEST_PI_PUBLISHED_DIGEST !== "1" }, async t => {
  const sent = fakeHTTP(t);
  const path = await privateConfig(t);
  const digest = sha(await readFile(path));
  assert.ok(definitions(await load(ordinary, path, digest)).has("read_channel"));
  const wrong = await load(ordinary, path, "0".repeat(64));
  assert.equal(wrong.extensions.length, 0);
  assert.equal(wrong.errors.length, 1);
  assert.ok(definitions(await load(typed, path, digest)).has("read_work_inputs"));
  assert.equal(sent.length, 0);
});
