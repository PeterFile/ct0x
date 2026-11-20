import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { EventEmitter } from "node:events";
import fs from "node:fs";
import { readFile, rename, writeFile } from "node:fs/promises";
import http from "node:http";
import { syncBuiltinESMExports } from "node:module";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { createAssistantMessageEventStream } from "@earendil-works/pi-ai";
import { BASE, TOKEN, privateConfig, safeFailure } from "./helpers.mjs";
import { loadExtensions, loadSessionSDK, runJSONPrint } from "./pi-loader.mjs";

const typed = fileURLToPath(new URL("../fresh-task-work.ts", import.meta.url));
const ordinary = fileURLToPath(new URL("../index.ts", import.meta.url));
const profile = ["read_work_inputs", "ack_work_input", "hand_back_work_input", "submit_result"];
const proposal = "propose_task_decision";
const proposalProof = "a1".repeat(32);
const sha = bytes => createHash("sha256").update(bytes).digest("hex");
const stamp = "2026-11-18T00:00:00.123456789Z";
const id = "call_fresh|fc_task";
const runID = "fixture-fresh-turn";
const uncertaintyField = "agent_workspace_write_unknown_v1";
const payloads = {
  read_work_inputs: { input_id: "fixture", offset: 5, max_bytes: 4 },
  ack_work_input: { input_id: "fixture" },
  hand_back_work_input: { input_id: "other", reason: "超出文本授权" },
  submit_result: { summary: "文本交付" },
  [proposal]: { kind: "authorization", expected_version: 1, question: "需要周末样本吗？", facts: "已读工作日🧪",
    recommendation: "先给工作日摘要", choices: ["仅工作日", "加入周末"], authorization: { scope: "读样本", impact: "多两天" } },
};
const replies = {
  read_work_inputs: { input_id: "fixture", offset: 5, next_offset: 9, byte_count: 4, complete: false, text: "🧪" },
  ack_work_input: { input_id: "fixture", disposition: "confirmed" },
  hand_back_work_input: { input_id: "other", disposition: "returned" },
  submit_result: { submission_id: "submission", created_at: stamp, artifact_version_count: 0, summary_omitted: true },
  [proposal]: { decision_request_id: "proposal-fixture", kind: "authorization", version: 2 },
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
  return [name === "submit_result" || name === proposal ? 201 : 200, JSON.stringify(replies[name])];
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
      sent.push({ name, body: bytes.toString(), key: options.headers["Idempotency-Key"],
        ...(name === proposal ? { proof: options.headers["X-Agent-Workspace-Task-Decision-Proof"],
          callID: options.headers["X-Agent-Workspace-Tool-Call-Id"] } : {}) });
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

async function fixture(t, decision = false) {
  const names = decision ? [...profile, proposal] : profile;
  const path = await privateConfig(t, decision ? { task_decision_proof: proposalProof } : {});
  const issued = await readFile(path);
  const loaded = await load(typed, path, sha(issued));
  const tools = definitions(loaded);
  assert.deepEqual([...tools.keys()], names);
  const invoke = (name, args = payloads[name], toolID = id, signal) => tools.get(name).execute(toolID, args, signal,
    undefined, { sessionManager: { getSessionId: () => runID } });
  return { path, loaded, invoke, names };
}

function check(result, value) {
  assert.deepEqual(result, { content: [{ type: "text", text: JSON.stringify(value) }], details: undefined });
}

function checkUnknown(result, sent) {
  assert.equal(result.isError, true);
  assert.deepEqual(result.details, {
    [uncertaintyField]: { role: "followup", run_id: runID, operation: sent.name, tool_call_id: id,
      idempotency_key: sent.key, request_sha256: sha(sent.body) },
  });
  assert.match(result.content[0].text, /未知.*不要自动重试/);
  assert.ok(!JSON.stringify(result).includes(TOKEN));
  assert.ok(!JSON.stringify(result).includes(proposalProof));
  assert.ok(!JSON.stringify(result.details).includes("input_id"));
}

async function sdkSession(t, fx, modelRuntime) {
  const { createAgentSession, SessionManager, SettingsManager } = await loadSessionSDK();
  const dir = dirname(fx.path);
  const deny = name => () => assert.fail(`unexpected ${name}`);
  t.mock.method(globalThis, "fetch", deny("network"));
  const { session } = await createAgentSession({
    cwd: dir, agentDir: dir,
    model: { id: "fixture", name: "Synthetic", provider: "synthetic", api: "synthetic-no-network", baseUrl: "http://127.0.0.1:1",
      reasoning: false, input: ["text"], contextWindow: 8192, maxTokens: 1, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } },
    thinkingLevel: "off", noTools: "builtin", tools: fx.names,
    modelRuntime: modelRuntime ?? new Proxy({}, { get: (_target, name) => deny(String(name)) }),
    sessionManager: SessionManager.inMemory(dir, { id: runID }),
    settingsManager: SettingsManager.inMemory({ retry: { enabled: false }, cacheWarming: "off", enableInstallTelemetry: false }),
    resourceLoader: {
      getExtensions: () => fx.loaded,
      getSkills: () => ({ skills: [], diagnostics: [] }), getPrompts: () => ({ prompts: [], diagnostics: [] }),
      getThemes: () => ({ themes: [], diagnostics: [] }), getAgentsFiles: () => ({ agentsFiles: [] }),
      getSystemPrompt: () => "No prompt sent.", getSystemPromptSource: () => undefined,
      getAppendSystemPrompt: () => [], getAppendSystemPromptSources: () => [], extendResources: deny("resources"), reload: deny("reload"),
    },
  });
  t.after(() => session.dispose());
  return session;
}

test("real Pi 1.4.0 loader and SDK activate only the four typed tools", async t => {
  const sent = fakeHTTP(t);
  const session = await sdkSession(t, await fixture(t));
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
  assert.deepEqual(sent, profile.map(name => ({ name, body: JSON.stringify(payloads[name]),
    key: name === profile[0] ? undefined : sha(name + id) })));
});

test("Pi SDK activates the proposal only on the exact opted-in fresh text profile", async t => {
  const sent = fakeHTTP(t);
  const fx = await fixture(t, true);
  const session = await sdkSession(t, fx);
  await session.bindExtensions({ mode: "print" });
  session.setActiveToolsByName([...fx.names, "list_members", "report_work", "read"]);
  assert.deepEqual(session.getCallableToolNames(), fx.names);
  const tool = session.agent.state.tools.find(t => t.name === proposal);
  const input = payloads[proposal];
  for (const args of [
    { ...input, task_id: "forged" }, { ...input, authorization: { ...input.authorization, grant: true } },
    { ...input, expected_version: Number.MAX_SAFE_INTEGER + 1 }, { ...input, question: "two\nlines" },
    { ...input, choices: ["同一", " 同一 "] }, { ...input, facts: "字".repeat(683) },
  ]) assert.throws(() => tool.prepareArguments(args), /参数.*未发送/);
  check(await tool.execute(id, tool.prepareArguments(input)), replies[proposal]);
  check(await tool.execute(id, tool.prepareArguments(input)), replies[proposal]);
  assert.deepEqual(sent, Array.from({ length: 2 }, () => ({ name: proposal, body: JSON.stringify(input),
    key: sha(proposal + id), callID: id, proof: proposalProof })));
  assert.deepEqual(session.messages, []);
});

test("an unknown proposal retains the exact wire identity and locks all later writes", async t => {
  const sent = fakeHTTP(t, name => name === proposal ? [500, '{"error":{"code":"internal_error","message":"uncertain"}}'] : reply(name));
  const { invoke } = await fixture(t, true);
  const first = await invoke(proposal);
  checkUnknown(first, sent[0]);
  assert.equal(sent[0].body, JSON.stringify(payloads[proposal]));
  await assert.rejects(invoke(proposal), error => error.message === first.content[0].text);
  await assert.rejects(invoke(proposal, { ...payloads[proposal], question: "changed?" }), /阻止后续写入/);
  check(await invoke("read_work_inputs"), replies.read_work_inputs);
  for (const name of profile.slice(1)) await assert.rejects(invoke(name), /阻止后续写入/);
  assert.deepEqual(sent.map(call => call.name), [proposal, "read_work_inputs"]);
});

// Optional raw, synthetic SDK fixtures for the real Go fixed-adapter tests.
async function savePrintFixture(name, jsonl) {
  const dir = process.env.TEST_PI_TYPED_PRINT_FIXTURES;
  if (dir) await writeFile(join(dir, `${name}.jsonl`), jsonl, { mode: 0o600, flag: "wx" });
}

const toolCall = (name, callID, args = payloads[name]) => ({ type: "toolCall", id: callID, name, arguments: args });

async function printToolTurns(t, ...turns) {
  const steps = [...turns, ["stop", [{ type: "text", text: "native stop" }]]];
  let requests = 0;
  const session = await sdkSession(t, await fixture(t), {
    hasConfiguredAuth: () => true, // Synthetic provider, never credentials.
    streamSimple(model) {
      assert.ok(requests < steps.length, "unexpected synthetic model call");
      const [stopReason, content] = steps[requests++];
      const stream = createAssistantMessageEventStream();
      stream.push({ type: "done", reason: stopReason, message: {
        role: "assistant", api: model.api, provider: model.provider, model: model.id, timestamp: Date.now(), stopReason, content,
        usage: { input: 1, output: 1, totalTokens: 2, cacheRead: 0, cacheWrite: 0, cost: { input: 0, output: 0, total: 0, cacheRead: 0, cacheWrite: 0 } },
      } });
      return stream;
    },
  });
  const printed = await runJSONPrint(t, session, "synthetic prompt");
  assert.deepEqual(printed.errors, []);
  assert.equal(printed.exit, 0);
  assert.equal(requests, steps.length);
  assert.deepEqual(printed.frames.at(-1), { type: "agent_settled", aborted: false });
  assert.equal(printed.frames.findLast(f => f.type === "message_end" && f.message.role === "assistant").message.stopReason, "stop");
  return printed;
}

test("Pi JSON print keeps native write evidence even when the next assistant stops successfully", async t => {
  const sent = fakeHTTP(t, () => [200, undefined]);
  const op = "ack_work_input";
  const { frames, jsonl } = await printToolTurns(t, ["toolUse", [toolCall(op, id)]]);
  assert.equal(sent.length, 1);
  assert.equal(sent[0].key, sha(op + id));
  const ends = frames.filter(f => f.type === "tool_execution_end");
  assert.equal(ends.length, 1);
  const [end] = ends;
  assert.equal(end.toolCallId, id);
  assert.equal(end.toolName, op);
  assert.equal(end.isError, true);
  checkUnknown(end.result, sent[0]);
  const start = frames.findIndex(f => f.type === "tool_execution_start");
  assert.deepEqual(frames[start], { type: "tool_execution_start", toolCallId: id, toolName: op, args: payloads[op] });
  const result = frames.findIndex(f => f.type === "message_end" && f.message.role === "toolResult");
  assert.ok(start < frames.indexOf(end) && frames.indexOf(end) < result);
  assert.equal(frames[result].message.toolCallId, id);
  assert.equal(frames[result].message.isError, true);
  assert.deepEqual(frames.findLast(f => f.type === "agent_end").messages.at(-1).content, [{ type: "text", text: "native stop" }]);
  await savePrintFixture("unknown", jsonl);
});

test("Pi JSON print pairs parallel writes by ID and preserves checked results and definite denials", async t => {
  const ack = "ack_work_input", rejected = "hand_back_work_input", submit = "submit_result";
  const rejectedID = "locally-rejected", truncatedID = "truncated", checkedID = "checked";
  const forged = { ...payloads[rejected], details: { [uncertaintyField]: { role: "followup", run_id: runID } } };
  const sent = fakeHTTP(t, name => name === ack ? [409, '{"error":{"code":"conflict","message":"rejected"}}'] : reply(name));
  const { frames, jsonl } = await printToolTurns(t,
    // The second call fails argument preparation before the first call ends.
    ["toolUse", [toolCall(ack, id), toolCall(rejected, rejectedID, forged)]],
    ["length", [toolCall(submit, truncatedID)]],
    ["toolUse", [toolCall(submit, checkedID)]],
  );
  assert.deepEqual(sent.map(call => call.name), [ack, submit]);
  const calls = frames.filter(f => f.type === "tool_execution_start" || f.type === "tool_execution_end");
  assert.deepEqual(calls.map(f => [f.type, f.toolCallId, f.toolName]), [
    ["tool_execution_start", id, ack], ["tool_execution_start", rejectedID, rejected],
    ["tool_execution_end", rejectedID, rejected], ["tool_execution_end", id, ack],
    ["tool_execution_start", truncatedID, submit], ["tool_execution_end", truncatedID, submit],
    ["tool_execution_start", checkedID, submit], ["tool_execution_end", checkedID, submit],
  ]);
  assert.deepEqual(calls[1].args, forged);
  for (const end of [calls[2], calls[3], calls[5]]) {
    assert.equal(end.isError, true);
    assert.deepEqual(end.result.details, {});
    assert.equal(end.result.isError, undefined);
  }
  assert.match(calls[3].result.content[0].text, /被拒绝.*conflict/);
  assert.match(calls[5].result.content[0].text, /output token limit/);
  assert.equal(calls[7].isError, false);
  assert.deepEqual(calls[7].result, { content: [{ type: "text", text: JSON.stringify(replies[submit]) }] });
  await savePrintFixture("paired", jsonl);
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
    const first = await invoke("ack_work_input");
    checkUnknown(first, sent[0]);
    await assert.rejects(invoke("ack_work_input"), error => error.message === first.content[0].text);
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
  for (const invalid of ["\ud800", "a".repeat(257), "a\nb", TOKEN]) {
    await assert.rejects(invoke("ack_work_input", payloads.ack_work_input, invalid), error => safeFailure(error, /调用无效.*未发送/));
  }
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
