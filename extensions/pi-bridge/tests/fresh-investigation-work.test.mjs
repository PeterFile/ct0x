import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { EventEmitter } from "node:events";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import http from "node:http";
import { syncBuiltinESMExports } from "node:module";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { createAssistantMessageEventStream } from "@earendil-works/pi-ai";
import { BASE, TOKEN, privateConfig, safeFailure } from "./helpers.mjs";
import { loadExtensions, loadSessionSDK, runJSONPrint } from "./pi-loader.mjs";

const entry = fileURLToPath(new URL("../fresh-investigation-work.ts", import.meta.url));
const readyKind = "agent_workspace_investigation_ready_v1";
const turn = "turn-investigation";
const stamp = "2026-11-18T00:00:00.123456789Z";
const channel = "read_investigation_channel", artifact = "read_investigation_artifact";
const readReport = "read_work_report", report = "report_work";
const delegate = "delegate_work", handback = "handback_work";
const owner = ["read_work_inputs", "ack_work_input", "hand_back_work_input", "submit_result"];
const reportOwner = [channel, artifact, readReport, ...owner];
const initialOwner = [channel, artifact, "submit_result", delegate];
const managedReport = [readReport, ...owner, delegate, handback];
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

async function openSession(t, fx, operations, { id = turn, existing = false, persistent = false, excludeTools, modelRuntime } = {}) {
  const { createAgentSession, SessionManager, SettingsManager } = await loadSessionSDK();
  const dir = dirname(fx.path);
  const loaded = fx.loaded ?? await load(fx.path, fx.digest);
  assert.deepEqual(loaded.errors, [], "typed startup must leave the input guard installed even on config failure");
  assert.equal(loaded.extensions[0].tools.size, 0, "registered before verifying the live session");
  const deny = name => () => assert.fail(`unexpected ${name}`);
  t.mock.method(globalThis, "fetch", deny("network"));
  const model = { id: "fixture", name: "Synthetic", provider: "synthetic", api: "synthetic-no-network", baseUrl: "http://127.0.0.1:1",
    reasoning: false, input: ["text"], contextWindow: 8192, maxTokens: 1, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } };
  const sessionManager = persistent ? SessionManager.create(dir, dir, { id }) : SessionManager.inMemory(dir, { id });
  if (existing) sessionManager.appendMessage({ role: "user", content: "already used", timestamp: 1 });
  const { session } = await createAgentSession({
    cwd: dir, agentDir: dir, model, noTools: "builtin", tools: operations, excludeTools,
    modelRuntime: modelRuntime ?? new Proxy({}, { get: (_target, name) => deny(String(name)) }),
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
  return session;
}

async function bind(t, fx, operations, options) {
  const session = await openSession(t, fx, operations, options);
  const errors = [];
  await session.bindExtensions({ mode: "json", onError: err => errors.push(err.error) });
  const active = session.getActiveToolNames();
  // Attempt to activate names outside the signed registration.
  session.setActiveToolsByName([...operations, ...reportOwner, report, delegate, handback, "hand_back_work", "bash", "read", "send_message", "decide_dispatch"]);
  const tools = new Map(session.agent.state.tools.map(tool => [tool.name, tool]));
  return { tools, session, active, errors, invoke: (op, args, id = "fixture-call", signal) => tools.get(op).execute(id, args, signal) };
}

function checkReady(frames, digest, source) {
  assert.deepEqual(frames.slice(0, 2).map(frame => frame.type), ["session", "entry_appended"]);
  assert.equal(frames[0].id, turn);
  const { id, parentId, timestamp } = frames[1].entry;
  assert.match(id, /^[-a-z0-9]+$/);
  assert.ok(parentId === null || typeof parentId === "string");
  assert.ok(Number.isFinite(Date.parse(timestamp)));
  assert.deepEqual(frames[1], { type: "entry_appended", entry: {
    type: "custom", customType: readyKind, data: { run_id: turn, source_kind: source, config_sha256: digest }, id, parentId, timestamp,
  } });
  assert.equal(frames.filter(f => f.type === "entry_appended" && f.entry.customType === readyKind).length, 1);
  assert.ok(!JSON.stringify(frames[1]).includes(TOKEN));
}

function result(value) {
  return { content: [{ type: "text", text: JSON.stringify(value) }], details: undefined };
}

function checkUnknown(answer, role, call) {
  assert.equal(answer.isError, true);
  assert.equal(call.key, sha(call.op + "fixture-call"));
  assert.deepEqual(answer.details, {
    agent_workspace_write_unknown_v1: { role, run_id: turn, operation: call.op, tool_call_id: "fixture-call",
      idempotency_key: call.key, request_sha256: sha(call.body) },
  });
  assert.match(answer.content[0].text, /未知.*不要自动重试/);
  assert.ok(!JSON.stringify(answer).includes(TOKEN));
}

const fragmentArgs = { artifact_id: "artifact-1", version_id: "approved-v2", offset: 2, max_bytes: 4 };
const fragment = { artifact_id: "artifact-1", version_id: "approved-v2", sha256: "a".repeat(64), offset: 2, next_offset: 6,
  content_complete: false, encoding: "base64", content: "/wECBA==" };
const page = { items: [], next_cursor: "" };
const receipt = { submission_id: "submit-fixture", created_at: stamp, artifact_version_count: 0, summary_omitted: true };
const delegateArgs = { member_id: "approved-peer", question: "  核对这一项🧪。\n", allow_topic_messages: false,
  artifact_versions: [{ artifact_id: "artifact-1", version_id: "approved-v2" }] };
const handbackArgs = { summary: "  请协调者核对范围。\n" };
const delegateReceipt = { delegation_id: "delegation-fixture", created_at: stamp, wake_queued: true };
const handbackReceipt = { handback_id: "handback-fixture", notice_message_id: "notice-fixture", created_at: stamp };

const replies = {
  [channel]: [200, page], [artifact]: [200, fragment], [readReport]: [200, boundReport], [report]: [201, reportReceipt],
  read_work_inputs: [200, page], ack_work_input: [200, { input_id: "one", disposition: "confirmed" }],
  hand_back_work_input: [200, { input_id: "two", disposition: "returned" }], submit_result: [201, receipt],
  [delegate]: [201, delegateReceipt], [handback]: [201, handbackReceipt],
};
const args = {
  [artifact]: fragmentArgs, [report]: reportArgs, [delegate]: delegateArgs, [handback]: handbackArgs,
  ack_work_input: { input_id: "one" }, hand_back_work_input: { input_id: "two", reason: "未获授权" }, submit_result: submit,
};

for (const [kind, operations] of [
  ["followup", [channel, artifact, ...owner]], ["delegation", [artifact, report]], ["delegation", [report]], ["report", reportOwner],
  ["initial_owner", initialOwner], ["initial_owner", [channel, "submit_result"]],
  ["followup", [channel, ...owner, delegate, handback]], ["report", managedReport], ["report", [readReport, ...owner, handback]],
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
      assert.deepEqual(await invoke(op, tool.prepareArguments(payload), `fixture-${op}`), result(expected));
    }
    assert.deepEqual(sent, operations.map(op => ({ op, body: JSON.stringify(payloads[op] ?? {}),
      key: [report, delegate, handback, ...owner.slice(1)].includes(op) ? sha(op + `fixture-${op}`) : undefined })));
    assert.deepEqual(session.messages, []);
    await session.extensionRunner.emit({ type: "session_shutdown", reason: "quit" });
    await assert.rejects(invoke(operations[0], payloads[operations[0]] ?? {}), /会话.*未发送/);
    assert.equal(sent.length, operations.length);
  });
}

test("config digest and exact source profile are checked before session binding", async t => {
  const good = issue("delegation", [report]);
  const fx = await issueFile(t, good);
  const denied = async (path, digest) => {
    const { tools, errors } = await bind(t, { path, digest }, [report]);
    assert.equal(tools.size, 0);
    assert.equal(errors.length, 1);
    assert.match(errors[0], /配置/);
    assert.ok(!errors[0].includes(TOKEN));
  };
  for (const digest of [undefined, "0".repeat(64), fx.digest.toUpperCase()]) await denied(fx.path, digest);
  for (const bad of [
    { ...good, task_id: "unbound" }, { ...good, run_id: "../run" }, { ...good, action: "text_submission" },
    issue("handback", [report]), issue("followup", [channel, "submit_result"]), issue("delegation", [report, readReport]),
    issue("report", [readReport]), issue("delegation", [artifact, channel, report]), issue("delegation", [report, report]),
    issue("initial_owner", ["submit_result", delegate]), issue("initial_owner", [...initialOwner, handback]),
    issue("initial_owner", [channel, ...owner, delegate]), issue("delegation", [report, delegate]),
    issue("report", [...reportOwner, delegate, handback]), issue("report", [readReport, ...owner, delegate]),
    issue("followup", [...owner, handback, delegate]), issue("followup", [...owner, "hand_back_work"]),
  ]) {
    const changed = await issueFile(t, bad);
    await denied(changed.path, changed.digest);
  }
  for (const wire of [fx.wire + " ", fx.wire.replace('"run_id":', '"run_id":"other","run_id":')]) {
    await writeFile(fx.path, wire);
    await denied(fx.path, sha(wire));
  }
  await writeFile(fx.path, fx.wire);
  for (const name of ["index.ts", "fresh-task-work.ts"]) {
    const { extensions, errors } = await load(fx.path, fx.digest, fileURLToPath(new URL(`../${name}`, import.meta.url)));
    assert.equal(extensions.length, 0);
    assert.equal(errors.length, 1);
    assert.match(errors[0].error, /配置/);
    assert.ok(!errors[0].error.includes(TOKEN));
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

test("Pi JSON print refuses input before model admission when investigation startup didn't bind", async t => {
  const sent = intercept(t, () => assert.fail("unbound session called HTTP"));
  const fx = await issueFile(t, issue("delegation", [report]));
  for (const [options, digest, denial] of [
    [{}, "0".repeat(64), /配置.*未启用/], [{ id: "other-turn" }, fx.digest, /会话.*未发送/],
    [{ existing: true }, fx.digest, /会话.*未发送/], [{ persistent: true }, fx.digest, /会话.*未发送/],
    [{ excludeTools: [report] }, fx.digest, /会话.*未发送/],
  ]) {
    let admissions = 0;
    const modelRuntime = { hasConfiguredAuth() { admissions++; throw new Error("synthetic model admission"); } };
    const session = await openSession(t, { ...fx, digest }, [report], { ...options, modelRuntime });
    const { frames, errors, exit } = await runJSONPrint(t, session, "should not enter the agent");
    assert.equal(admissions, 0, "Pi reached model admission despite failing startup");
    assert.equal(exit, 0); // Pi's documented handled-input path, not an auth error or process kill.
    assert.equal(errors.length, 1);
    assert.match(errors[0], denial);
    assert.deepEqual(frames.map(frame => frame.type), ["session"]);
  }
  assert.equal(sent.length, 0);
});

test("investigation input gate neither swallows valid JSON nor intercepts TUI input", async t => {
  const sentinel = "synthetic model admission";
  const modelRuntime = { hasConfiguredAuth() { throw new Error(sentinel); } };
  const fx = await issueFile(t, issue("delegation", [report]));
  const session = await openSession(t, fx, [report], { modelRuntime });
  const { frames, errors, exit } = await runJSONPrint(t, session, "valid startup");
  assert.equal(exit, 1);
  assert.deepEqual(errors, [sentinel]);
  checkReady(frames, fx.digest, "delegation");
  assert.equal(frames.length, 2);
  const tui = await openSession(t, fx, [report], { id: "other-turn", modelRuntime });
  await tui.bindExtensions({ mode: "tui", onError: () => {} });
  assert.deepEqual(tui.getActiveToolNames(), []);
  await assert.rejects(tui.prompt("interactive input"), /synthetic model admission/);
});

for (const [role, ops, op, payload] of [
  ["delegation", [report], report, reportArgs],
  ["initial_owner", initialOwner, delegate, delegateArgs],
  ["report", managedReport, handback, handbackArgs],
]) test(`Pi JSON print pairs ${op} and retains the ${role} uncertainty marker`, async t => {
  const sent = intercept(t, () => [201, undefined]);
  const fx = await issueFile(t, issue(role, ops));
  let requests = 0;
  const session = await openSession(t, fx, ops, { modelRuntime: {
    hasConfiguredAuth: () => true, // Synthetic provider, never credentials.
    streamSimple(model) {
      const first = requests++ === 0;
      assert.ok(requests <= 2);
      const stream = createAssistantMessageEventStream();
      const stopReason = first ? "toolUse" : "stop";
      const content = first ? [{ type: "toolCall", id: "fixture-call", name: op, arguments: payload }] : [{ type: "text", text: "native stop" }];
      stream.push({ type: "done", reason: stopReason, message: {
        role: "assistant", api: model.api, provider: model.provider, model: model.id, timestamp: Date.now(), stopReason, content,
        usage: { input: 1, output: 1, totalTokens: 2, cacheRead: 0, cacheWrite: 0, cost: { input: 0, output: 0, total: 0, cacheRead: 0, cacheWrite: 0 } },
      } });
      return stream;
    },
  } });
  const { frames, jsonl, errors, exit } = await runJSONPrint(t, session, "synthetic typed write");
  assert.deepEqual(errors, []);
  assert.equal(exit, 0);
  assert.equal(requests, 2);
  assert.equal(sent.length, 1);
  const [start, end, ...extra] = frames.filter(f => /^tool_execution_(start|end)$/.test(f.type));
  assert.deepEqual(extra, []);
  assert.deepEqual(start, { type: "tool_execution_start", toolCallId: "fixture-call", toolName: op, args: payload });
  assert.equal(end.type, "tool_execution_end");
  assert.equal(end.toolName, op);
  assert.equal(end.toolCallId, start.toolCallId);
  assert.equal(end.isError, true);
  assert.deepEqual(sent[0], { op, body: JSON.stringify(payload), key: sha(op + "fixture-call") });
  checkUnknown(end.result, role, sent[0]);
  assert.deepEqual(frames.at(-1), { type: "agent_settled", aborted: false });
  const dir = process.env.TEST_PI_TYPED_PRINT_FIXTURES;
  if (dir) await writeFile(join(dir, `${role}-unknown.jsonl`), jsonl, { mode: 0o600, flag: "wx" });
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

test("owner commands reject any extra selector or unchecked nested material before send", async t => {
  const sent = intercept(t, op => replies[op]);
  const { tools, invoke } = await bind(t, await issueFile(t, issue("report", managedReport)), managedReport);
  const getter = { artifact_id: "artifact-1", get version_id() { assert.fail("evaluated a getter"); } };
  const version = delegateArgs.artifact_versions[0];
  for (const [op, payload] of [
    [delegate, { ...delegateArgs, caller: "forged" }], [delegate, { ...delegateArgs, idempotency_key: "forged" }],
    [delegate, { ...delegateArgs, question: "界".repeat(1366) }], [delegate, { ...delegateArgs, member_id: " owner" }],
    [delegate, { ...delegateArgs, allow_topic_messages: null }], [delegate, { ...delegateArgs, allow_topic_messages: 0 }],
    ...[null, [null], [getter], [new String("invalid")], Array(1), Array(17).fill(version),
      Object.assign([version], { extra: true }), [{ ...version, task_id: "forged" }], [{ ...version, version_id: "界".repeat(43) }],
      [{ ...version, version_id: "\ud800" }]].map(refs => [delegate, { ...delegateArgs, artifact_versions: refs }]),
    [handback, { ...handbackArgs, reason: "not the ABI" }], [handback, { ...handbackArgs, input_id: "not an input return" }],
    [handback, { ...handbackArgs, coordinator_id: "forged" }], [handback, { summary: "🧪".repeat(2049) }],
  ]) {
    assert.throws(() => tools.get(op).prepareArguments(payload), /参数.*未发送/);
    await assert.rejects(invoke(op, payload), /参数.*未发送/);
  }
  assert.equal(sent.length, 0);
  // Omitted selectors stay omitted; explicitly false and nested tuples are not rewritten.
  const bare = { member_id: delegateArgs.member_id, question: delegateArgs.question };
  assert.deepEqual(await invoke(delegate, bare, "without-selectors"), result(delegateReceipt));
  assert.deepEqual(await invoke(delegate, delegateArgs, "with-selectors"), result(delegateReceipt));
  await assert.rejects(invoke(delegate, delegateArgs, "with-selectors"), /已使用.*未发送/);
  assert.deepEqual(await invoke(handback, handbackArgs, "handback"), result(handbackReceipt));
  await assert.rejects(invoke(handback, handbackArgs, "handback"), /已使用.*未发送/);
  assert.deepEqual(sent.map(c => c.body), [bare, delegateArgs, handbackArgs].map(p => JSON.stringify(p)));
});

for (const [role, ops, op] of [["initial_owner", initialOwner, delegate], ["report", managedReport, handback]]) {
  test(`Pi ${op} treats only a definite HTTP denial as known and never resends an unknown write`, async t => {
    let answer = [409, { error: { code: "conflict", message: "declined" } }];
    let abort;
    const sent = intercept(t, () => { abort?.(); return answer; });
    const session = () => issueFile(t, issue(role, ops)).then(fx => bind(t, fx, ops));
    const payload = args[op];
    const { invoke } = await session();
    const before = new AbortController();
    before.abort();
    await assert.rejects(invoke(op, payload, "fixture-call", before.signal), /取消.*未发送/);
    assert.equal(sent.length, 0);
    await assert.rejects(invoke(op, payload), /被拒绝.*conflict/);
    await assert.rejects(invoke(op, payload), /已使用.*未发送/);
    assert.equal(sent.length, 1);
    const good = replies[op][1];
    for (const reply of [
      [500, { error: { code: "internal_error", message: "Current failure" } }], [201, undefined], [200, good],
      [201, { ...good, wake_queued: false }], [201, { ...good, created_at: "wrong" }],
      [201, JSON.stringify(good).replace('"created_at":', '"created_at":"wrong","created_at":')],
      [201, { ...good, task_id: "foreign" }], [302, good],
    ]) {
      const { invoke } = await session();
      const i = sent.length;
      answer = reply;
      checkUnknown(await invoke(op, payload), role, sent[i]);
      await assert.rejects(invoke(op, payload), /结果未知/);
      await assert.rejects(invoke("submit_result", submit, "another-command"), /阻止后续写入/);
      assert.equal(sent.length, i + 1);
    }
    const running = new AbortController();
    abort = () => running.abort();
    answer = [201, good];
    const cancelled = await session();
    const i = sent.length;
    checkUnknown(await cancelled.invoke(op, payload, "fixture-call", running.signal), role, sent[i]);
    await assert.rejects(cancelled.invoke(op, payload, "another-call"), /阻止后续写入/);
    assert.equal(sent.length, i + 1);
  });
}

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
    const first = await invoke(report, reportArgs);
    checkUnknown(first, "delegation", sent[n]);
    await assert.rejects(invoke(report, reportArgs), error => safeFailure(error, /未知/) && error.message === first.content[0].text);
    await assert.rejects(invoke(report, reportArgs, "new-id"), /阻止后续写入/);
    assert.equal(sent.length, n + 1);
  }
  const fx = await issueFile(t, issue("report", reportOwner));
  const { invoke } = await bind(t, fx, reportOwner);
  const n = sent.length;
  checkUnknown(await invoke("ack_work_input", args.ack_work_input), "report", sent[n]);
  assert.deepEqual(await invoke(readReport, {}), result(boundReport));
  await assert.rejects(invoke("submit_result", submit), /阻止后续写入/);
  assert.deepEqual(sent.slice(n).map(call => call.op), ["ack_work_input", readReport]);
});

test("Pi 1.4.0 loader suppresses ambient SYSTEM.md and APPEND_SYSTEM.md only with explicit prompts", async t => {
  t.mock.method(globalThis, "fetch", () => assert.fail("unexpected network"));
  const { DefaultResourceLoader, SettingsManager } = await import("@earendil-works/pi-coding-agent");
  const cwd = dirname(await privateConfig(t)), agentDir = join(cwd, "agent");
  await mkdir(agentDir, { mode: 0o700 });
  await writeFile(join(agentDir, "SYSTEM.md"), "ambient-system-fixture");
  await writeFile(join(agentDir, "APPEND_SYSTEM.md"), "ambient-append-fixture");
  const options = { cwd, agentDir, settingsManager: SettingsManager.inMemory({}), noContextFiles: true,
    noExtensions: true, noThemes: true, noSkills: true, noPromptTemplates: true };
  const baseline = new DefaultResourceLoader(options);
  await baseline.reload();
  assert.equal(baseline.getSystemPrompt(), "ambient-system-fixture");
  assert.deepEqual(baseline.getAppendSystemPrompt(), ["ambient-append-fixture"]);
  // The values are the exact argv pinned by TestPiFreshInvestigationExactBinding.
  const systemPrompt = "你只可使用本轮注册的调查工具，在绑定范围内读取材料或执行工具定义的平台操作。";
  const appendSystemPrompt = ["不得向宿主机请求文件、命令或网络工具。"];
  const strict = new DefaultResourceLoader({ ...options, systemPrompt, appendSystemPrompt });
  await strict.reload();
  assert.equal(strict.getSystemPrompt(), systemPrompt);
  assert.deepEqual(strict.getAppendSystemPrompt(), appendSystemPrompt);
  assert.deepEqual(strict.getAgentsFiles().agentsFiles, []);
});

test("Pi JSON print proves typed startup on a synthetic successful turn", async t => {
  const fx = await issueFile(t, issue("delegation", [report]));
  const session = await openSession(t, fx, [report], { modelRuntime: {
    hasConfiguredAuth: () => true,
    streamSimple(model, context) {
      assert.ok(!JSON.stringify(context).includes(fx.digest), "readiness leaked into the model context");
      const stream = createAssistantMessageEventStream();
      stream.push({ type: "done", reason: "stop", message: {
        role: "assistant", api: model.api, provider: model.provider, model: model.id, timestamp: Date.now(), stopReason: "stop",
        content: [{ type: "text", text: "native stop" }],
        usage: { input: 1, output: 1, totalTokens: 2, cacheRead: 0, cacheWrite: 0, cost: { input: 0, output: 0, total: 0, cacheRead: 0, cacheWrite: 0 } },
      } });
      return stream;
    },
  } });
  const { frames, jsonl, errors, exit } = await runJSONPrint(t, session, "synthetic typed investigation");
  assert.deepEqual(errors, []);
  assert.equal(exit, 0);
  checkReady(frames, fx.digest, "delegation");
  assert.deepEqual(frames[2], { type: "agent_start" });
  assert.ok(frames.some(f => f.type === "message_end" && f.message.role === "assistant" && f.message.stopReason === "stop"));
  assert.deepEqual(frames.at(-1), { type: "agent_settled", aborted: false });
  const dir = process.env.TEST_PI_TYPED_PRINT_FIXTURES;
  if (dir) await writeFile(join(dir, "delegation-checked.jsonl"), jsonl, { mode: 0o600, flag: "wx" });
});
