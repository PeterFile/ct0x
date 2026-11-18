import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { createHash } from "node:crypto";
import { constants, closeSync, fstatSync, openSync, readSync } from "node:fs";
import { request, type ClientRequest, type IncomingMessage } from "node:http";
import { isAbsolute } from "node:path";
import { Type } from "typebox";
import { Value } from "typebox/value";
import { freshTypedToolResult, validTypedWriteCallID, validWorkInputCursor, validWorkInputID } from "./fresh-task-work.mjs";

const CONFIG_ERROR = "调查传输配置无效，未启用工具。";
const SESSION_ERROR = "调查会话绑定无效，未发送请求。";
const ARG_ERROR = "成员工具参数无效，未发送请求。";
const UNKNOWN = "执行结果未知；请保留原调用标识、幂等键和内容先对账，不要自动重试。";
const wireLimit = 64 * 1024;
const artifactLimit = 64 * 1024 * 1024;
const summaryLimit = 8192;
const questionLimit = 4096, versionLimit = 16;
const reportKinds = ["progress", "question", "blocked", "delivered"] as const;
const channel = "read_investigation_channel", artifact = "read_investigation_artifact";
const owner = ["read_work_inputs", "ack_work_input", "hand_back_work_input", "submit_result"] as const;
const delegate = "delegate_work", handback = "handback_work";
const roles = { initial_owner: ["submit_result"], followup: owner, delegation: ["report_work"], report: ["read_work_report", ...owner] } as const;
const closed = { additionalProperties: false } as const;
const id = Type.String({ minLength: 1, maxLength: 256 });
const text = (maxLength: number) => Type.String({ minLength: 1, maxLength, description: `非空文本，最多 ${maxLength} UTF-8 字节。` });
const count = (minimum: number, maximum: number) => Type.Optional(Type.Integer({ minimum, maximum }));
const cursor = (maxLength: number) => Type.Optional(Type.String({ minLength: 1, maxLength, description: "上一页的 next_cursor；第一页省略。" }));
const inputPage = { limit: count(1, 20), before: cursor(512) };
const inputSegment = { offset: count(0, Number.MAX_SAFE_INTEGER), max_bytes: count(4, 8192) };
const pageShape = Type.Object(inputPage, closed);
const segmentShape = Type.Object({ input_id: id, ...inputSegment }, closed);
const versionShape = Type.Object({ artifact_id: Type.String({ minLength: 1, maxLength: 128 }), version_id: Type.String({ minLength: 1, maxLength: 128 }) }, closed);
const schemas = {
  [channel]: Type.Object({ limit: count(1, 200), before: cursor(2048) }, closed),
  [artifact]: Type.Object({ ...versionShape.properties, offset: count(0, artifactLimit), max_bytes: count(1, 16384) }, closed),
  read_work_inputs: Type.Object({ ...inputPage, input_id: Type.Optional(id), ...inputSegment }, { ...closed, oneOf: [pageShape, segmentShape] }),
  ack_work_input: Type.Object({ input_id: id }, closed),
  hand_back_work_input: Type.Object({ input_id: id, reason: text(2048) }, closed),
  submit_result: Type.Object({ summary: text(summaryLimit) }, closed),
  report_work: Type.Object({ kind: Type.String({ enum: [...reportKinds] }), summary: text(summaryLimit) }, closed),
  read_work_report: Type.Object({}, closed),
  [delegate]: Type.Object({ member_id: id, question: text(questionLimit), allow_topic_messages: Type.Optional(Type.Boolean()),
    artifact_versions: Type.Optional(Type.Array(versionShape, { maxItems: versionLimit })) }, closed),
  [handback]: Type.Object({ summary: text(summaryLimit) }, closed),
};
type Operation = keyof typeof schemas;
const descriptions: Record<Operation, string> = {
  [channel]: "分页读取获准的绑定调查话题，不接受话题选择器；超限时请减小 limit。",
  [artifact]: "仅读取获准的确切成果版本（不是路径或当前版本）。ID 各最多 128 UTF-8 字节；offset 和 next_offset 是字节位置，以 content_complete 判断末尾。",
  read_work_inputs: "读取绑定输入目录（limit/before）或按 input_id 分段读取。offset 和 max_bytes 按 UTF-8 字节；处理状态变化后从头重读。读取不会确认。",
  ack_work_input: "实际处理后确认单条绑定输入，不提交成果。",
  hand_back_work_input: "将未处理的单条输入交回并说明原因，不交回整项任务。",
  submit_result: "提交绑定尝试的纯文本成果，不接受成果版本；提交不等于验收。",
  report_work: "报告本轮委派，不提交成果。progress 不唤醒责任方；其他种类由服务端绑定回送。",
  read_work_report: "只读取唤醒本轮的绑定正式报告。必须传空对象，不接受任何选择器。",
  [delegate]: "请本轮列出的同伴核对一个问题。可请求确切获准材料的子集；省略材料字段则不共享。平台检查当前权限与余额。",
  [handback]: "将整项调查的待决问题和原因正式交回绑定协调员查阅。不交回单条输入，不改派或触发执行。",
};
const status: Record<Operation, number> = {
  [channel]: 200, [artifact]: 200, read_work_report: 200, read_work_inputs: 200,
  ack_work_input: 200, hand_back_work_input: 200, submit_result: 201, report_work: 201, [delegate]: 201, [handback]: 201,
};
const writes = new Set<Operation>(["ack_work_input", "hand_back_work_input", "submit_result", "report_work", delegate, handback]);
const rejections = new Map([
  [400, ["invalid_input", "invalid_json"]], [403, ["forbidden", "invalid_host", "invalid_origin"]], [404, ["not_found"]],
  [405, ["method_not_allowed"]], [409, ["conflict"]], [413, ["body_too_large"]], [415, ["unsupported_media_type"]],
]);

const sha = (bytes: string | Buffer) => createHash("sha256").update(bytes).digest("hex");
const decode = (bytes: Buffer) => new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(bytes);
const record = (value: unknown): value is Record<string, unknown> => value !== null && typeof value === "object" && !Array.isArray(value);
const keys = (value: Record<string, unknown>, ...fields: string[]) =>
  Object.keys(value).length === fields.length && fields.every(key => Object.hasOwn(value, key));
const unicode = (value: string) => !/[\ud800-\udfff]/u.test(value);
const validText = (value: unknown, maxBytes: number) => typeof value === "string" && Buffer.byteLength(value) <= maxBytes &&
  /\P{White_Space}/u.test(value) && unicode(value) && !value.includes("\0");
const versionHandle = (value: unknown) => validText(value, 128) && typeof value === "string" &&
  !/^\p{White_Space}|\p{White_Space}$|[\r\n]/u.test(value);
const channelCursor = (value: unknown) => typeof value === "string" && /^[A-Za-z0-9_-]{1,2048}$/.test(value);
const reportKind = (value: unknown) => reportKinds.some(kind => value === kind);

function validProfile(kind: string, operations: unknown): operations is Operation[] {
  if (!Object.hasOwn(roles, kind) || !Array.isArray(operations)) return false;
  let start = 0;
  for (const op of [channel, artifact]) if (operations[start] === op) start++;
  if (kind === "initial_owner" && start === 0) return false;
  const base = roles[kind as keyof typeof roles];
  if (!base.every((name, index) => operations[start + index] === name)) return false;
  const suffix = operations.slice(start + base.length);
  if (suffix.length === 0) return true;
  if (kind === "initial_owner") return suffix.length === 1 && suffix[0] === delegate;
  if (kind !== "followup" && (kind !== "report" || start !== 0)) return false;
  return (suffix.length === 1 && suffix[0] === handback) || (suffix.length === 2 && suffix[0] === delegate && suffix[1] === handback);
}

function readConfig(path: string | undefined, digest: string | undefined) {
  let fd: number | undefined;
  try {
    if (!path || !isAbsolute(path) || !digest || !/^[a-f0-9]{64}$/.test(digest)) throw new Error();
    fd = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
    const stat = fstatSync(fd), limit = 4096;
    if (!stat.isFile() || (stat.mode & 0o7777) !== 0o600 || stat.size > limit || stat.uid !== process.geteuid?.()) throw new Error();
    const buffer = Buffer.alloc(limit + 1);
    let size = 0;
    while (size < buffer.length) {
      const n = readSync(fd, buffer, size, buffer.length - size, null);
      if (n === 0) break;
      size += n;
    }
    const bytes = buffer.subarray(0, size);
    if (size > limit || sha(bytes) !== digest) throw new Error();
    const wire = decode(bytes);
    const value: unknown = JSON.parse(wire);
    if (!record(value)) throw new Error();
    const { endpoint, token, run_id, action, source_kind, operations } = value;
    if (typeof endpoint !== "string" || !/^http:\/\/(127\.0\.0\.1|\[::1\]):[0-9]{1,5}\/api\/v1\/member-tools$/.test(endpoint) ||
        typeof token !== "string" || token.length > 256 || !/^[A-Za-z0-9._~+/-]+=*$/.test(token) ||
        typeof run_id !== "string" || !/^[A-Za-z0-9](?:[A-Za-z0-9_-]{0,62}[A-Za-z0-9])?$/.test(run_id) ||
        action !== "read_only_investigation" || typeof source_kind !== "string" || !validProfile(source_kind, operations) ||
        wire !== JSON.stringify({ endpoint, token, run_id, action, source_kind, operations }) + "\n") throw new Error();
    const url = new URL(endpoint);
    if (url.port === "0") throw new Error();
    return { endpoint: url, token, runID: run_id, role: source_kind, operations: Object.freeze(operations) };
  } catch {
    throw new Error(CONFIG_ERROR);
  } finally {
    if (fd !== undefined) {
      try { closeSync(fd); } catch { throw new Error(CONFIG_ERROR); }
    }
  }
}

function ownFields(raw: unknown, properties: object) {
  if (!record(raw) || (Object.getPrototypeOf(raw) !== Object.prototype && Object.getPrototypeOf(raw) !== null)) throw new Error();
  return Reflect.ownKeys(raw).map(key => {
    const field = Object.getOwnPropertyDescriptor(raw, key);
    if (typeof key !== "string" || !Object.hasOwn(properties, key) || !field?.enumerable || !Object.hasOwn(field, "value")) throw new Error();
    return [key, field.value as unknown] as const;
  });
}

function checkedVersions(raw: unknown) {
  if (!Array.isArray(raw) || Object.getPrototypeOf(raw) !== Array.prototype || raw.length > versionLimit ||
      Reflect.ownKeys(raw).length !== raw.length + 1) throw new Error();
  const refs: Record<string, string>[] = [];
  for (let i = 0; i < raw.length; i++) {
    const field = Object.getOwnPropertyDescriptor(raw, String(i));
    if (!field?.enumerable || !Object.hasOwn(field, "value")) throw new Error();
    const ref = Object.fromEntries(ownFields(field.value, versionShape.properties));
    if (!Value.Check(versionShape, ref) || !versionHandle(ref.artifact_id) || !versionHandle(ref.version_id)) throw new Error();
    refs.push(ref);
  }
  return refs;
}

type Args = Record<string, string | number | boolean | Record<string, string>[]>;
function checkedArgs(op: Operation, raw: unknown): Args {
  try {
    // Snapshot only checked own data fields, including the exact nested version
    // tuples. Getters, toJSON, undefined, null and coercions cannot change the wire.
    const args: Args = Object.fromEntries(ownFields(raw, schemas[op].properties).map(([key, value]) => {
      if (op === delegate && key === "artifact_versions") return [key, checkedVersions(value)];
      if (op === delegate && key === "allow_topic_messages" && typeof value === "boolean") return [key, value];
      if (typeof value === "string" || typeof value === "number") return [key, value];
      throw new Error();
    }));
    if (!Value.Check(schemas[op], args as unknown)) throw new Error();
    const valid = (() => {
      switch (op) {
        case channel: return !Object.hasOwn(args, "before") || channelCursor(args.before);
        case artifact: return versionHandle(args.artifact_id) && versionHandle(args.version_id);
        case "read_work_inputs": return Object.hasOwn(args, "input_id") ? Value.Check(segmentShape, args) && validWorkInputID(args.input_id) :
          Value.Check(pageShape, args) && (!Object.hasOwn(args, "before") || validWorkInputCursor(args.before));
        case "ack_work_input": return validWorkInputID(args.input_id);
        case "hand_back_work_input": return validWorkInputID(args.input_id) && validText(args.reason, 2048);
        case "submit_result": return validText(args.summary, summaryLimit);
        case "report_work": return reportKind(args.kind) && validText(args.summary, summaryLimit);
        case "read_work_report": return true;
        case delegate: return validWorkInputID(args.member_id) && validText(args.question, questionLimit);
        case handback: return validText(args.summary, summaryLimit);
      }
    })();
    if (valid) return args;
  } catch { /* Never expose serialization or argument access errors. */ }
  throw new Error(ARG_ERROR);
}

function timestamp(value: unknown) {
  return typeof value === "string" && /^(?!0000)\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$/.test(value) &&
    !/^0001-01-01T00:00:00(?:\.0+)?Z$/.test(value) && Number.isFinite(Date.parse(value)) && new Date(value).toISOString().slice(0, 19) === value.slice(0, 19);
}

function flatWire(text: string, count: number) {
  // Fixed success/error receipts may not collapse duplicate decoded keys or
  // accept numerically equivalent non-integer spellings (e.g. 0e0, -0).
  const wire = text.trim();
  if (!wire.startsWith("{")) return false;
  const field = /\s*"(?:[^"\\]|\\.)*"\s*:\s*(?:"(?:[^"\\]|\\.)*"|true|false|0|[1-9]\d*)\s*([,}])/y;
  field.lastIndex = 1;
  for (let i = 0; i < count; i++) if (field.exec(wire)?.[1] !== (i === count - 1 ? "}" : ",")) return false;
  return field.lastIndex === wire.length;
}

function validPage(value: Record<string, unknown>, sent: Args, maxItems: number, validCursor: (value: unknown) => boolean) {
  return keys(value, "items", "next_cursor") && Array.isArray(value.items) && value.items.length <= maxItems &&
    (value.next_cursor === "" || (value.items.length !== 0 && value.next_cursor !== sent.before && validCursor(value.next_cursor)));
}

function validReply(op: Operation, value: unknown, sent: Args, wire: string): boolean {
  if (!record(value)) return false;
  switch (op) {
    case channel:
      // C projects the messages; the bridge checks only the page envelope.
      return validPage(value, sent, Number(sent.limit ?? 50), channelCursor) && (value.items as unknown[]).every(record);
    case artifact: {
      if (!keys(value, "artifact_id", "version_id", "sha256", "offset", "next_offset", "content_complete", "encoding", "content") ||
          !flatWire(wire, 8) || value.artifact_id !== sent.artifact_id || value.version_id !== sent.version_id ||
          typeof value.sha256 !== "string" || !/^[a-f0-9]{64}$/.test(value.sha256) || typeof value.content !== "string" ||
          value.offset !== (sent.offset ?? 0) || typeof value.content_complete !== "boolean") return false;
      const encoding = value.encoding;
      if (encoding !== "utf8" && encoding !== "base64") return false;
      const bytes = Buffer.from(value.content, encoding);
      return (encoding === "utf8" || bytes.toString("base64") === value.content) && bytes.length <= Number(sent.max_bytes ?? 8192) &&
        value.next_offset === Number(value.offset) + bytes.length && value.next_offset <= artifactLimit && (bytes.length > 0 || value.content_complete);
    }
    case "read_work_report":
      return keys(value, "report_id", "kind", "summary", "created_at") && flatWire(wire, 4) && validWorkInputID(value.report_id) &&
        reportKind(value.kind) && value.kind !== "progress" && validText(value.summary, summaryLimit) && timestamp(value.created_at);
    case "report_work":
      return keys(value, "report_id", "kind", "created_at", "wake_queued") && flatWire(wire, 4) && validWorkInputID(value.report_id) &&
        value.kind === sent.kind && value.wake_queued === (sent.kind !== "progress") && timestamp(value.created_at);
    case "submit_result":
      return keys(value, "submission_id", "created_at", "artifact_version_count", "summary_omitted") && flatWire(wire, 4) &&
        validWorkInputID(value.submission_id) && timestamp(value.created_at) && value.artifact_version_count === 0 && value.summary_omitted === true;
    case delegate:
      return keys(value, "delegation_id", "created_at", "wake_queued") && flatWire(wire, 3) && validWorkInputID(value.delegation_id) &&
        timestamp(value.created_at) && value.wake_queued === true;
    case handback:
      return keys(value, "handback_id", "notice_message_id", "created_at") && flatWire(wire, 3) && validWorkInputID(value.handback_id) &&
        validWorkInputID(value.notice_message_id) && timestamp(value.created_at);
    case "ack_work_input":
    case "hand_back_work_input":
      return keys(value, "input_id", "disposition") && flatWire(wire, 2) && value.input_id === sent.input_id &&
        value.disposition === (op === "ack_work_input" ? "confirmed" : "returned");
    case "read_work_inputs":
      if (!Object.hasOwn(sent, "input_id")) {
        return validPage(value, sent, Number(sent.limit ?? 10), validWorkInputCursor) && (value.items as unknown[]).every(item =>
          record(item) && keys(item, "input_id", "created_at") && validWorkInputID(item.input_id) && timestamp(item.created_at));
      }
      return keys(value, "input_id", "offset", "next_offset", "byte_count", "complete", "text") && flatWire(wire, 6) &&
        value.input_id === sent.input_id && value.offset === (sent.offset ?? 0) && typeof value.text === "string" &&
        value.byte_count === Buffer.byteLength(value.text) && value.byte_count <= Number(sent.max_bytes ?? 4096) &&
        value.next_offset === Number(value.offset) + value.byte_count && Number.isSafeInteger(value.next_offset) &&
        typeof value.complete === "boolean" && (value.byte_count > 0 || value.complete);
  }
}

function exchange(endpoint: URL, op: Operation, body: Buffer, headers: Record<string, string | number>, signal?: AbortSignal) {
  return new Promise<{ status: number; text: string }>((resolve, reject) => {
    let req: ClientRequest | undefined;
    let res: IncomingMessage | undefined;
    const cleanup = () => { clearTimeout(timer); signal?.removeEventListener("abort", stop); };
    const stop = () => { cleanup(); reject(new Error()); res?.destroy(); req?.destroy(); };
    const timer = setTimeout(stop, 25_000);
    try {
      // Numeric loopback only. No fetch headers, proxy, pooling, redirects or retries.
      req = request({ hostname: endpoint.hostname.replace(/^\[|\]$/g, ""), port: endpoint.port || 80,
        path: `${endpoint.pathname}/${op}`, method: "POST", headers, agent: false, maxHeaderSize: 8192 }, response => {
        res = response;
        res.on("error", stop).on("aborted", stop).on("close", () => { if (!response.complete) stop(); });
        const status = res.statusCode || 0;
        if (status < 200 || (status >= 300 && status < 400) || status > 599 ||
            !/^application\/json(?:\s*;\s*charset=utf-8)?$/i.test(res.headers["content-type"] || "") ||
            (res.headers["content-encoding"] && res.headers["content-encoding"] !== "identity") || Number(res.headers["content-length"]) > wireLimit) return stop();
        const chunks: Buffer[] = [];
        let size = 0;
        res.on("data", (chunk: Buffer) => {
          size += chunk.length;
          if (size > wireLimit) return stop();
          chunks.push(chunk);
        });
        res.on("end", () => {
          try {
            if (!response.complete) return stop();
            const text = decode(Buffer.concat(chunks, size));
            cleanup();
            resolve({ status, text });
          } catch { stop(); }
        });
      });
      req.on("error", stop);
      signal?.addEventListener("abort", stop, { once: true });
      if (signal?.aborted) return stop();
      req.end(body);
    } catch { stop(); }
  });
}

function investigationClient(endpoint: URL, token: string) {
  let uncertain: { key: string; fingerprint: string; error: Error } | undefined;
  let writing = false;
  // A native owner management call is sent at most once, even if its receipt
  // was definitive. A new call ID is a new operation, still subject to B.
  const sentOwnerCalls = new Set<string>();
  const safe = (message: string) => new Error(message.replaceAll(token, "〔已隐藏〕"));
  return async (op: Operation, args: Args, id: string, signal?: AbortSignal) => {
    const write = writes.has(op);
    if (typeof id !== "string" || !id.trim() || !unicode(id) || (write && !validTypedWriteCallID(id, token))) {
      throw safe("工具调用标识无效，未发送请求。");
    }
    const json = JSON.stringify(args), body = Buffer.from(json, "utf8");
    if (body.length > wireLimit || json.includes(token)) throw safe(ARG_ERROR);
    const key = write ? sha(op + id) : "", fingerprint = write ? sha(body) : "";
    if (write && uncertain) {
      if (uncertain.key === key && uncertain.fingerprint === fingerprint) throw uncertain.error;
      throw safe(`当前实例已阻止后续写入。${UNKNOWN}`);
    }
    if (write && writing) throw safe("前一写入尚未完成，未发送请求。");
    if (signal?.aborted) throw safe("工具调用已取消，未发送请求。");
    if (op === delegate || op === handback) {
      if (sentOwnerCalls.has(id) || sentOwnerCalls.size >= 256) throw safe("工具调用标识已使用或已达本轮上限，未发送请求。");
      sentOwnerCalls.add(id);
    }
    const headers: Record<string, string | number> = { Authorization: `Bearer ${token}`, "Content-Type": "application/json", "Content-Length": body.length };
    if (write) { headers["Idempotency-Key"] = key; writing = true; }
    let failure = UNKNOWN;
    try {
      const reply = await exchange(endpoint, op, body, headers, signal);
      const value: unknown = JSON.parse(reply.text, (key, value: unknown) => {
        if (["__proto__", "constructor", "prototype"].includes(key) || !unicode(key) ||
            (typeof value === "string" && !unicode(value)) || (typeof value === "number" && !Number.isFinite(value))) throw new Error();
        return value;
      });
      const text = JSON.stringify(value);
      if (reply.text.includes(token) || text.includes(token)) throw new Error();
      if (reply.status === status[op] && validReply(op, value, args, reply.text)) return { text };
      const envelope = /^\s*\{\s*"error"\s*:\s*(\{[\s\S]*\})\s*\}\s*$/.exec(reply.text);
      if (envelope && record(value) && record(value.error) && keys(value.error, "code", "message") && typeof value.error.code === "string" &&
          typeof value.error.message === "string" && flatWire(envelope[1], 2)) {
        const code = value.error.code;
        if (rejections.get(reply.status)?.includes(code)) failure = `成员工具请求被拒绝（${code}）。`;
        if (!write && reply.status === 413 && code === "tool_result_too_large") {
          failure = `工具结果超过 64 KiB 限制，未截断。${op === artifact ? "请减小 max_bytes 后重读。" : op === channel ? "请减小 limit 后重读。" : ""}`;
        }
      }
    } catch { /* Missing or malformed evidence must remain unknown. */ }
    finally { if (write) writing = false; }
    const error = safe(failure);
    if (write && failure === UNKNOWN) {
      uncertain = { key, fingerprint, error };
      return { text: error.message, writeUnknown: { operation: op, tool_call_id: id, idempotency_key: key, request_sha256: fingerprint } };
    }
    throw error;
  };
}

export default function freshInvestigationWork(pi: ExtensionAPI) {
  const { endpoint, token, runID, role, operations } = readConfig(process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG,
    process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256);
  const invoke = investigationClient(endpoint, token);
  let started = false, active = false;
  // Pi 1.4.0 reports session_start errors and still calls prompt() in print
  // mode. Consume that one-shot JSON input before prompt's model/auth path.
  // Never exit the process or intercept interactive input.
  pi.on("input", (_event, ctx) => {
    if (ctx.mode === "json" && !active) return { action: "handled" };
  });
  pi.on("session_start", (event, ctx) => {
    if (started) { active = false; throw new Error(SESSION_ERROR); }
    started = true;
    if (event.reason !== "startup" || ctx.mode !== "json" || ctx.sessionManager.getSessionId() !== runID ||
        ctx.sessionManager.getSessionFile() !== undefined ||
        ctx.sessionManager.getEntries().some(entry => entry.type !== "model_change" && entry.type !== "thinking_level_change")) throw new Error(SESSION_ERROR);
    for (const op of operations) {
      const parameters = schemas[op];
      pi.registerTool<typeof parameters>({
        name: op, label: op, description: descriptions[op] + (writes.has(op) ? ` ${UNKNOWN}` : ""), parameters,
        prepareArguments(raw) { return checkedArgs(op, raw); },
        async execute(id, params, signal, _onUpdate, toolContext) {
          if (!active || toolContext.sessionManager.getSessionId() !== runID) throw new Error(SESSION_ERROR);
          const reply = await invoke(op, checkedArgs(op, params), id, signal);
          return freshTypedToolResult(reply, role, runID);
        },
      });
    }
    pi.setActiveTools([...operations]);
    const selected = pi.getActiveTools();
    if (selected.length !== operations.length || selected.some((name, index) => name !== operations[index])) throw new Error(SESSION_ERROR);
    active = true;
  });
  pi.on("session_shutdown", () => { active = false; });
}
