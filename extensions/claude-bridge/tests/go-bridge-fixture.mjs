// Independent Go transport fixture. No production entry import or SDK query.
import { spawn } from "node:child_process";
import { createInterface } from "node:readline";
import { writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { once } from "node:events";

const reader = createInterface({ input: process.stdin });
const initial = await once(reader, "line");
const start = JSON.parse(initial[0]);
const mode = start.body.prompt;
const token = process.env[start.body.token_env_name];
if (!token || process.argv.some(arg => arg.includes(token))) throw new Error("fixture environment invariant");
writeFileSync(join(start.body.cwd, "bridge-argv.json"), JSON.stringify(process.argv), { mode: 0o600 });
let sequence = 0;
const send = (kind, body = {}, overrides = {}) => process.stdout.write(JSON.stringify({ v: 1, request_id: start.request_id, seq: ++sequence, kind, session_id: start.session_id, input_uuid: start.input_uuid, body, ...overrides }) + "\n");
const permissionID = "44444444-4444-4444-8444-444444444444";
const tool = `mcp__${start.body.mcp_server}__${start.body.allowed_platform_tools[0]}`;
const approval = () => send("permission_request", { permission_id: permissionID, tool_use_id: "toolu_fixture", tool_name: mode === "fixture-native-deny" ? "Bash" : tool, input_sha256: "a".repeat(64), mcp_server: mode === "fixture-native-deny" ? "" : start.body.mcp_server, mcp_source: mode === "fixture-native-deny" ? "" : "dynamic", agent_id: mode === "fixture-subagent-deny" ? "agent_fixture" : "", phase: "pre_tool_use" });
let approvalResolve;
const approved = new Promise(resolve => { approvalResolve = resolve; });
let cancelledResolve;
const cancelled = new Promise(resolve => { cancelledResolve = resolve; });
reader.on("line", raw => {
  const frame = JSON.parse(raw);
  if (frame.kind === "permission_result") {
    writeFileSync(join(start.body.cwd, "permission-decision.json"), JSON.stringify(frame.body), { mode: 0o600 });
    approvalResolve(frame.body);
  }
  if (frame.kind === "cancel") { cancelledResolve(); if (mode === "fixture-cancel-pending") approval(); }
});
const script = join(dirname(fileURLToPath(import.meta.url)), "go-native-fixture.cjs");
const early = mode === "fixture-node-exit-unclosed";
const child = spawn(process.execPath, early ? [script, join(start.body.cwd, "descendant-completed")] : [script], { stdio: early ? "ignore" : ["pipe", "pipe", "pipe"] });
const native = { exited: false, stdout: false, stderr: false, close: false };
child.on("exit", () => { native.exited = true; });
child.stdout?.on("end", () => { native.stdout = true; });
child.stderr?.on("end", () => { native.stderr = true; });
child.stdout?.resume(); child.stderr?.resume();
const closed = once(child, "close").then(() => { native.close = true; });
send("submitted");
send("initialized", { sdk_version: "0.3.326", cli_version: "2.1.326", cwd: start.body.cwd, model: start.body.model, permission_mode: "default", tools: [tool], mcp_server: start.body.mcp_server, guards_applied: true, native_pid: child.pid, cli_path: start.body.cli_path });
process.stderr.write(token.slice(0, 4)); process.stderr.write(token.slice(4));
if (early) process.exit(0); // Deliberately no native close receipt: MUST stay unknown.
if (mode === "fixture-cancel-pending") { await cancelled; await approved; }
if (mode === "fixture-tool" || mode === "fixture-native-deny" || mode === "fixture-subagent-deny") { approval(); await approved; }
const result = { result_uuid: "33333333-3333-4333-8333-333333333333", subtype: "success", is_error: false, terminal_reason: "completed", user_message_uuid: start.input_uuid, queued_turn_count: 0, pending_tools: 0, pending_background: 0, pending_permissions: 0, text: "受控 result" };
if (mode === "fixture-error-result") Object.assign(result, { is_error: true, terminal_reason: "api_error", text: "" });
if (mode === "fixture-error-success") result.is_error = true;
if (mode === "fixture-wrong-input") result.user_message_uuid = "55555555-5555-4555-8555-555555555555";
if (mode === "fixture-pending-tool") result.pending_tools = 1;
if (mode === "fixture-unsupported-reason") result.terminal_reason = "background_requested";
if (mode === "fixture-duplicate-key") process.stdout.write('{"v":1,"v":1}\n');
if (mode === "fixture-oversized") process.stdout.write("x".repeat((256 << 10) + 1) + "\n");
if (mode === "fixture-unknown-kind") send("accepted", {});
if (mode === "fixture-bad-seq") sequence += 2;
if (mode !== "fixture-submitted-only" && mode !== "fixture-cancel-pending") send("terminal", result);
if (mode === "fixture-duplicate-terminal") send("terminal", result);
await closed;
writeFileSync(join(start.body.cwd, "native-wait.json"), JSON.stringify({ ...native, pid: child.pid, exit_code: child.exitCode }), { mode: 0o600 });
if (mode !== "fixture-no-native-closed") send("native_closed", { pid: child.pid, exit_code: child.exitCode, signal: child.signalCode, stdout_drained: mode !== "fixture-undrained" && native.stdout, stderr_drained: native.stderr, pending_tools: 0, pending_background: 0, pending_permissions: 0 });
await new Promise(resolve => setTimeout(resolve, 50));
writeFileSync(join(start.body.cwd, "bridge-finished"), "controlled bridge waited", { mode: 0o600 });
reader.close(); process.stdin.destroy(); process.stdout.end(); process.stderr.end();
