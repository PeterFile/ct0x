import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { EventEmitter } from "node:events";
import { readFile, writeFile } from "node:fs/promises";
import http from "node:http";
import { syncBuiltinESMExports } from "node:module";
import { dirname } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { BASE, TOKEN, privateConfig, safeFailure } from "./helpers.mjs";
import { loadExtensions, loadSessionSDK } from "./pi-loader.mjs";

const entry = fileURLToPath(new URL("../fresh-investigation-work.ts", import.meta.url));
const turn = "turn-investigation";
const stamp = "2026-11-18T00:00:00.123456789Z";
const channel = "read_investigation_channel", artifact = "read_investigation_artifact";
const readReport = "read_work_report", report = "report_work";
const owner = ["read_work_inputs", "ack_work_input", "hand_back_work_input", "submit_result"];
const reportOwner = [channel, artifact, readReport, ...owner];
const sha = bytes => createHash("sha256").update(bytes).digest("hex");
const issue = (kind, operations) => ({ run_id: turn, action: "read_only_investigation", source_kind: kind, operations });
const submit = { summary: "纯文本成果" };
const reportArgs = { kind: "delivered", summary: "正式报告" };
const boundReport = { report_id: "report-fixture", kind: "delivered", summary: "绑定原报告", created_at: stamp };
const reportReceipt = { report_id: "report-fixture", kind: "delivered", created_at: stamp, wake_queued: true };

async function load(path, digest, extension = entry) {
  const keys = ["AGENT_WORKSPACE_MEMBER_TOOL_CONFIG", "AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256"];
  const old = keys.map(key => process.env[key]);
  const set = (key, value) => value === undefined ? delete process.env[key] : (process.env[key] = value);
  try {
    keys.forEach((key, i) => set(key, [path, digest][i]));
    return await loadExtensions([extension], dirname(path));
  } finally { keys.forEach((key, i) => set(key, old[i])); }
}

async function issueFile(t, profile) {
  // Same canonical JSON plus newline as the Go issuer.
  const path = await privateConfig(t, profile);
  const wire = (await readFile(path, "utf8")) + "\n";
  await writeFile(path, wire);
  return { path, wire, digest: sha(wire) };
}

// Drive the actual HTTP client without sockets, DNS, an HTTP server, or a model.
function intercept(t, respond) {
  const sent = [];
  const original = http.request;
  http.request = (options, ready) => {
    assert.equal(options.hostname, "127.0.0.1");
    assert.equal(String(options.port), "7310");
    assert.equal(options.method, "POST");
    assert.equal(options.agent, false);
    assert.equal(options.headers.Authorization, `Bearer ${TOKEN}`);
    assert.ok(options.path.startsWith(`${BASE}/`));
    const req = new EventEmitter();
    req.destroy = () => {};
    req.end = bytes => {
      assert.equal(options.headers["Content-Length"], bytes.length);
      const body = bytes.toString();
      const op = options.path.slice(BASE.length + 1);
      sent.push({ op, body, key: options.headers["Idempotency-Key"] });
      queueMicrotask(() => {
        const [statusCode, reply] = respond(op, JSON.parse(body));
        const response = Object.assign(new EventEmitter(), { statusCode, complete: false, destroy() {},
          headers: { "content-type": "application/json" } });
        ready(response);
        if (reply === undefined) return response.emit("aborted");
        response.emit("data", Buffer.from(typeof reply === "string" ? reply : JSON.stringify(reply)));
        response.complete = true;
        response.emit("end");
      });
    };
    return req;
  };
  syncBuiltinESMExports();
  t.after(() => { http.request = original; syncBuiltinESMExports(); });
  return sent;
}

async function bind(t, fx, operations, { id = turn, existing = false, persistent = false } = {}) {
  const { createAgentSession, SessionManager, SettingsManager } = await loadSessionSDK();
  const dir = dirname(fx.path);
  const loaded = fx.loaded ?? await load(fx.path, fx.digest);
  assert.deepEqual(loaded.errors, []);
  assert.equal(loaded.extensions[0].tools.size, 0, "registered before verifying the live session");
  const deny = name => () => assert.fail(`unexpected ${name}`);
  t.mock.method(globalThis, "fetch", deny("network"));
  const model = { id: "fixture", name: "Synthetic", provider: "synthetic", api: "synthetic-no-network", baseUrl: "http://127.0.0.1:1",
    reasoning: false, input: ["text"], contextWindow: 8192, maxTokens: 1, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } };
  const sessionManager = persistent ? SessionManager.create(dir, dir, { id }) : SessionManager.inMemory(dir, { id });
  if (existing) sessionManager.appendMessage({ role: "user", content: "already used", timestamp: 1 });
  const { session } = await createAgentSession({
    cwd: dir, agentDir: dir, model, noTools: "builtin", tools: operations,
    modelRuntime: new Proxy({}, { get: (_target, name) => deny(String(name)) }),
    sessionManager,
    settingsManager: SettingsManager.inMemory({ retry: { enabled: false }, cacheWarming: "off", enableInstallTelemetry: false }),
    resourceLoader: {
      getExtensions: () => loaded, getSkills: () => ({ skills: [], diagnostics: [] }), getPrompts: () => ({ prompts: [], diagnostics: [] }),
      getThemes: () => ({ themes: [], diagnostics: [] }), getAgentsFiles: () => ({ agentsFiles: [] }),
      getSystemPrompt: () => "No prompt sent.", getSystemPromptSource: () => undefined,
      getAppendSystemPrompt: () => [], getAppendSystemPromptSources: () => [], extendResources: deny("resources"), reload: deny("reload"),
    },
  });
  t.after(() => session.dispose());
  session.prompt = session.agent.prompt = session.agent.streamFn = deny("prompt or stream");
  const errors = [];
  await session.bindExtensions({ mode: "json", onError: err => errors.push(err.error) });
  const active = session.getActiveToolNames();
  // Attempt to activate names outside the signed registration.
  session.setActiveToolsByName([...operations, ...reportOwner, report, "bash", "read", "send_message", "decide_dispatch"]);
  const tools = new Map(session.agent.state.tools.map(tool => [tool.name, tool]));
  return { tools, session, active, errors, invoke: (op, args, id = "fixture-call", signal) => tools.get(op).execute(id, args, signal) };
}

function result(value) {
  return { content: [{ type: "text", text: JSON.stringify(value) }], details: undefined };
}

const fragmentArgs = { artifact_id: "artifact-1", version_id: "approved-v2", offset: 2, max_bytes: 4 };
const fragment = { artifact_id: "artifact-1", version_id: "approved-v2", sha256: "a".repeat(64), offset: 2, next_offset: 6,
  content_complete: false, encoding: "base64", content: "/wECBA==" };
const page = { items: [], next_cursor: "" };
const receipt = { submission_id: "submit-fixture", created_at: stamp, artifact_version_count: 0, summary_omitted: true };

const replies = {
  [channel]: [200, page], [artifact]: [200, fragment], [readReport]: [200, boundReport], [report]: [201, reportReceipt],
  read_work_inputs: [200, page], ack_work_input: [200, { input_id: "one", disposition: "confirmed" }],
  hand_back_work_input: [200, { input_id: "two", disposition: "returned" }], submit_result: [201, receipt],
};
const args = {
  [artifact]: fragmentArgs, [report]: reportArgs,
  ack_work_input: { input_id: "one" }, hand_back_work_input: { input_id: "two", reason: "未获授权" }, submit_result: submit,
};

for (const [kind, operations] of [
  ["followup", [channel, artifact, ...owner]], ["delegation", [artifact, report]], ["delegation", [report]], ["report", reportOwner],
]) {
  test(`Pi 1.4.0 registers only the exact ${kind} profile (${operations.length} tools)`, async t => {
    const reportOnly = operations.length === 1;
    const payloads = reportOnly ? { [report]: { ...reportArgs, kind: "progress" } } : args;
    const sent = intercept(t, op => reportOnly ? [201, { ...reportReceipt, kind: "progress", wake_queued: false }] : replies[op]);
    const fx = await issueFile(t, issue(kind, operations));
    // The loader uses its checked bytes, not a second read during session binding.
    fx.loaded = await load(fx.path, fx.digest);
    await writeFile(fx.path, fx.wire.replace(TOKEN, "different"));
    const { session, active, errors, tools, invoke } = await bind(t, fx, operations);
    assert.deepEqual(errors, []);
    assert.deepEqual(active, operations);
    assert.deepEqual([...tools.keys()], operations);
    assert.deepEqual(session.getCallableToolNames(), operations);
    for (const op of operations) {
      const tool = tools.get(op);
      const payload = payloads[op] ?? {};
      assert.throws(() => tool.prepareArguments({ ...payload, task_id: "forged" }), /参数.*未发送/);
      const expected = reportOnly ? { ...reportReceipt, kind: "progress", wake_queued: false } : replies[op][1];
      assert.deepEqual(await invoke(op, tool.prepareArguments(payload)), result(expected));
    }
    assert.deepEqual(sent, operations.map(op => ({ op, body: JSON.stringify(payloads[op] ?? {}),
      key: op === report || owner.slice(1).includes(op) ? sha(op + "fixture-call") : undefined })));
    assert.deepEqual(session.messages, []);
    await session.extensionRunner.emit({ type: "session_shutdown", reason: "quit" });
    await assert.rejects(invoke(operations[0], payloads[operations[0]] ?? {}), /会话.*未发送/);
    assert.equal(sent.length, operations.length);
  });
}

test("config digest and exact source profile are checked before session binding", async t => {
  const good = issue("delegation", [report]);
  const fx = await issueFile(t, good);
  const denied = loaded => {
    assert.equal(loaded.extensions.length, 0);
    assert.equal(loaded.errors.length, 1);
    assert.match(loaded.errors[0].error, /配置/);
    assert.ok(!loaded.errors[0].error.includes(TOKEN));
  };
  for (const digest of [undefined, "0".repeat(64), fx.digest.toUpperCase()]) denied(await load(fx.path, digest));
  for (const bad of [
    { ...good, task_id: "unbound" }, { ...good, run_id: "../run" }, { ...good, action: "text_submission" },
    issue("handback", [report]), issue("followup", [channel, "submit_result"]), issue("delegation", [report, readReport]),
    issue("report", [readReport]), issue("delegation", [artifact, channel, report]), issue("delegation", [report, report]),
  ]) {
    const changed = await issueFile(t, bad);
    denied(await load(changed.path, changed.digest));
  }
  for (const wire of [fx.wire + " ", fx.wire.replace('"run_id":', '"run_id":"other","run_id":')]) {
    await writeFile(fx.path, wire);
    denied(await load(fx.path, sha(wire)));
  }
  await writeFile(fx.path, fx.wire);
  for (const name of ["index.ts", "fresh-task-work.ts"]) {
    denied(await load(fx.path, fx.digest, fileURLToPath(new URL(`../${name}`, import.meta.url))));
  }
});

test("wrong, persistent or previously used native session registers no tools", async t => {
  const sent = intercept(t, () => assert.fail("unbound session called HTTP"));
  const fx = await issueFile(t, issue("delegation", [report]));
  for (const options of [{ id: "other-turn" }, { existing: true }, { persistent: true }]) {
    const { tools, errors } = await bind(t, fx, [report], options);
    assert.equal(tools.size, 0);
    assert.equal(errors.length, 1);
    assert.match(errors[0], /会话/);
  }
  assert.equal(sent.length, 0);
});

test("report owner denies selectors, mixed work input reads, versions on submit, and malformed evidence", async t => {
  let answer = boundReport;
  const segment = { input_id: "one", offset: 5, next_offset: 9, byte_count: 4, text: "🧪", complete: false };
  const sent = intercept(t, (op, body) => op === readReport ? [200, answer] :
    op === "read_work_inputs" && body.input_id ? [200, segment] : replies[op]);
  const fx = await issueFile(t, issue("report", reportOwner));
  const { tools, invoke } = await bind(t, fx, reportOwner);
  for (const [op, bad] of [
    [readReport, { report_id: boundReport.report_id }], [readReport, null], [readReport, { before: undefined }],
    [channel, { topic_id: "other" }], [channel, { limit: "1" }], [channel, { before: "a".repeat(2049) }],
    [artifact, { ...fragmentArgs, offset: 67108865 }], [artifact, { ...fragmentArgs, max_bytes: 0 }],
    [artifact, { ...fragmentArgs, version_id: "界".repeat(43) }], [artifact, { ...fragmentArgs, version_id: "\ud800" }],
    ["read_work_inputs", { input_id: "one", limit: 1 }], ["read_work_inputs", { max_bytes: 4 }],
    ["hand_back_work_input", { input_id: "two", reason: "界".repeat(683) }],
    ["submit_result", { ...submit, artifact_versions: [] }],
  ]) {
    assert.throws(() => tools.get(op).prepareArguments(bad), /参数.*未发送/);
    await assert.rejects(invoke(op, bad), /参数.*未发送/);
  }
  assert.equal(sent.length, 0);
  for (const bad of [{ ...boundReport, kind: "progress" }, { ...boundReport, summary: "🧪".repeat(2048) + "." },
    { ...boundReport, source_id: "private" }, { ...boundReport, summary: "\u0000" },
    { ...boundReport, created_at: "0001-01-01T00:00:00.0Z" }]) {
    answer = bad;
    await assert.rejects(invoke(readReport, {}), error => safeFailure(error, /未知/));
  }
  answer = { ...boundReport, summary: "🧪".repeat(2048) };
  assert.deepEqual(await invoke(readReport, {}), result(answer));
  assert.deepEqual(await invoke("read_work_inputs", { input_id: "one", offset: 5, max_bytes: 4 }), result(segment));
  assert.deepEqual(await invoke("submit_result", submit), result(receipt));
});

test("a malformed or missing write receipt never retries or permits a different write", async t => {
  let reportReply = [409, { error: { code: "conflict", message: "rejected" } }];
  const sent = intercept(t, op => op === report ? reportReply : op === "ack_work_input" ?
    [200, { input_id: "wrong-input", disposition: "confirmed" }] : replies[op]);
  const child = await bind(t, await issueFile(t, issue("delegation", [report])), [report]);
  await assert.rejects(child.invoke(report, reportArgs), /被拒绝.*conflict/);
  reportReply = [201, reportReceipt];
  assert.deepEqual(await child.invoke(report, reportArgs, "next-id"), result(reportReceipt));
  for (const reply of [
    [201, undefined], [200, reportReceipt], [201, { ...reportReceipt, wake_queued: false }],
    [201, JSON.stringify(reportReceipt).replace('"kind":', '"kind":"progress","kind":')],
  ]) {
    reportReply = reply;
    const fx = await issueFile(t, issue("delegation", [report]));
    const { tools, invoke } = await bind(t, fx, [report]);
    const n = sent.length;
    for (const bad of [{ ...reportArgs, answer_to: "ungranted-input" }, { ...reportArgs, kind: "submitted" },
      { ...reportArgs, summary: "界".repeat(2731) }, { ...reportArgs, summary: "\u3000" }]) {
      assert.throws(() => tools.get(report).prepareArguments(bad), /参数.*未发送/);
    }
    const first = await invoke(report, reportArgs).catch(error => error);
    safeFailure(first, /未知/);
    await assert.rejects(invoke(report, reportArgs), error => error === first);
    await assert.rejects(invoke(report, reportArgs, "new-id"), /阻止后续写入/);
    assert.equal(sent.length, n + 1);
  }
  const fx = await issueFile(t, issue("report", reportOwner));
  const { invoke } = await bind(t, fx, reportOwner);
  const n = sent.length;
  await assert.rejects(invoke("ack_work_input", args.ack_work_input), /未知/);
  assert.deepEqual(await invoke(readReport, {}), result(boundReport));
  await assert.rejects(invoke("submit_result", submit), /阻止后续写入/);
  assert.deepEqual(sent.slice(n).map(call => call.op), ["ack_work_input", readReport]);
});
