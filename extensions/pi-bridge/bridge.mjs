import { constants, openSync, fstatSync, readSync, closeSync } from "node:fs";
import { isAbsolute } from "node:path";
import { createHash } from "node:crypto";
import { request } from "node:http";

export const INVESTIGATION_ASSESSMENT_ACTION = "read_only_investigation";
export const INVESTIGATION_CHANNEL = "read_investigation_channel";
export const INVESTIGATION_ARTIFACT = "read_investigation_artifact";
export const PUBLISH_RUN_EXPLANATION = "publish_run_explanation";
export const RUN_EXPLANATION_ACK = "agent_workspace_run_explanation_ack_v1";
export const RUN_EXPLANATION_UNKNOWN = "agent_workspace_run_explanation_unknown_v1";
export const RUN_EXPLANATION_BYTES = 1024;

// Only a sent explanation write with no definitive result gets this evidence.
// The cached error survives an exact local replay; neither it nor its details
// contains the request body, token, or an untrusted response.
export class RunExplanationUnknownError extends Error {
  /** @param {string} message @param {string} key @param {string} textSHA256 */
  constructor(message, key, textSHA256) {
    super(message);
    this.evidence = Object.freeze({ idempotency_key: key, text_sha256: textSHA256 });
  }
}

const CONFIG_LIMIT = 4096;
const BYTE_LIMIT = 64 * 1024;
const REQUEST_TIMEOUT_MS = 25_000;
export const TOOL_NAMES = /** @type {const} */ (["list_members", "read_channel", "send_message", "create_member", "search_history", "read_history", "decide_dispatch", "submit_result", "workspace_list", "workspace_read", "workspace_write", "publish_workspace_artifact", "read_work_inputs", "ack_work_input", "hand_back_work_input", INVESTIGATION_CHANNEL, INVESTIGATION_ARTIFACT, PUBLISH_RUN_EXPLANATION]);

/** @typedef {{ text: string, json: unknown, explanationKey?: string }} Reply */
/** @typedef {(toolName: string, params: unknown, toolCallId: string, signal?: AbortSignal) => Promise<Reply>} Client */
/** @typedef {"not_sent" | "rejected" | "unknown"} FailureOutcome */
/** @typedef {{ active?: boolean, unknown?: { toolName: string, toolCallId: string, fingerprint: string, error: Error, body?: Buffer } }} WriteState */
const REJECTION_CODES = new Map([
  [400, ["invalid_input", "invalid_json"]], [403, ["forbidden", "invalid_host", "invalid_origin"]],
  [405, ["method_not_allowed"]],
  [413, ["body_too_large"]], [415, ["unsupported_media_type"]],
]);
const CONFIG_ERROR = "成员工具配置无效，未启用任何成员工具。";
const SERVER_ERROR_MESSAGES = new Map([
  ["request_canceled", "成员工具请求已取消，执行结果未知；请先对账，保留原工具调用标识及幂等键，不要自动重试"],
  ["deadline_exceeded", "成员工具请求已超时，执行结果未知；请先对账，保留原工具调用标识及幂等键，不要自动重试"],
  ["member_capacity", "群组成员容量已满；请仅在有空余容量后重试"],
  ["tool_result_too_large", "工具结果超过 64 KiB 限制；请减小 limit 后重新读取，结果不会截断"],
]);

/** @param {unknown} value @returns {value is Record<string, unknown>} */
function isRecord(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

/** @param {string} value */
export function validUnicode(value) {
  // The Unicode-mode pattern sees valid pairs as a single non-surrogate scalar.
  return !/[\ud800-\udfff]/u.test(value);
}

/** @param {unknown} value */
function endpointURL(value) {
  if (typeof value !== "string" ||
      !/^http:\/\/(127\.0\.0\.1|\[::1\]):[0-9]{1,5}\/api\/v1\/member-tools$/.test(value)) {
    throw new Error(CONFIG_ERROR);
  }
  const url = new URL(value);
  const port = Number(url.port || 80);
  if (port < 1 || port > 65535) throw new Error(CONFIG_ERROR);
  return url;
}

/**
 * Read once; the launcher owns the config file's lifetime. An expected digest
 * selects the bound profile; it covers exactly the bytes this bridge consumes.
 * @param {string | undefined} path
 * @param {string} [expectedSHA256]
 * @returns {{ invoke: Client, continueWork: boolean, discussionOnlyExcerpt: boolean, investigationIntake: boolean, investigationTools?: string[], runExplanation?: string }}
 */
export function openBridge(path, expectedSHA256) {
  let fd;
  try {
    if (!path || !isAbsolute(path)) throw new Error(CONFIG_ERROR);
    fd = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
    const stat = fstatSync(fd);
    if (!stat.isFile() || (stat.mode & 0o7777) !== 0o600 ||
        (process.getuid && stat.uid !== process.getuid()) || stat.size > CONFIG_LIMIT) {
      throw new Error(CONFIG_ERROR);
    }
    const buffer = Buffer.alloc(CONFIG_LIMIT + 1);
    let size = 0;
    while (size < buffer.length) {
      const count = readSync(fd, buffer, size, buffer.length - size, null);
      if (count === 0) break;
      size += count;
    }
    if (size > CONFIG_LIMIT) throw new Error(CONFIG_ERROR);
    const bytes = buffer.subarray(0, size);
    if (expectedSHA256 !== undefined &&
        (!/^[0-9a-f]{64}$/.test(expectedSHA256) || createHash("sha256").update(bytes).digest("hex") !== expectedSHA256)) {
      throw new Error(CONFIG_ERROR);
    }
    const wire = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(bytes);
    const config = JSON.parse(wire);
    if (!isRecord(config)) throw new Error(CONFIG_ERROR);
    const continueWork = Object.hasOwn(config, "continue_work");
    const discussionOnlyExcerpt = Object.hasOwn(config, "discussion_only_excerpt");
    const investigationIntake = Object.hasOwn(config, "intake_assessment_action");
    const strict = Object.hasOwn(config, "investigation_tools");
    const explanation = Object.hasOwn(config, "run_explanation");
    if (!Object.hasOwn(config, "endpoint") || !Object.hasOwn(config, "token") ||
        Object.keys(config).length !== 2 + Number(continueWork) + Number(discussionOnlyExcerpt) + Number(investigationIntake) + Number(strict) + Number(explanation) ||
        (continueWork && config.continue_work !== true) || (discussionOnlyExcerpt && config.discussion_only_excerpt !== true) ||
        (investigationIntake && config.intake_assessment_action !== INVESTIGATION_ASSESSMENT_ACTION) ||
        typeof config.token !== "string" || !/^[A-Za-z0-9._~+/-]+=*$/.test(config.token)) {
      throw new Error(CONFIG_ERROR);
    }
    /** @type {string | undefined} */
    let runExplanation;
    if (explanation) {
      if (!expectedSHA256 || strict || continueWork || discussionOnlyExcerpt || investigationIntake ||
          !validRoleReceiptWire(wire, 3) || typeof config.run_explanation !== "string" ||
          !/^[A-Za-z0-9](?:[A-Za-z0-9_-]{0,62}[A-Za-z0-9])?$/.test(config.run_explanation)) throw new Error(CONFIG_ERROR);
      runExplanation = config.run_explanation;
    }
    /** @type {string[] | undefined} */
    let investigationTools;
    if (strict) {
      const names = config.investigation_tools;
      if (expectedSHA256 === undefined || continueWork || discussionOnlyExcerpt || investigationIntake || !Array.isArray(names)) throw new Error(CONFIG_ERROR);
      const reads = [INVESTIGATION_CHANNEL, INVESTIGATION_ARTIFACT].filter(name => names.includes(name));
      if (!reads.length || names.length !== reads.length + 1 || !reads.every((name, i) => names[i] === name) ||
          !["decide_dispatch", "submit_result"].includes(names.at(-1))) throw new Error(CONFIG_ERROR);
      investigationTools = [...names];
    }
    const endpoint = endpointURL(config.endpoint);
    const token = config.token;
    /** @type {WriteState} */
    const writes = {};
    return {
      invoke: async (toolName, params, toolCallId, signal) => {
        if (investigationTools ? !investigationTools.includes(toolName) : toolName === INVESTIGATION_CHANNEL || toolName === INVESTIGATION_ARTIFACT ||
            toolName === PUBLISH_RUN_EXPLANATION && !runExplanation) {
          throw new Error("工具不在本轮授权范围内，未发送请求。");
        }
        return requestTool(endpoint, token, writes, toolName, params, toolCallId, signal, runExplanation);
      },
      continueWork, discussionOnlyExcerpt, investigationIntake, investigationTools, ...(runExplanation ? { runExplanation } : {}),
    };
  } catch {
    throw new Error(CONFIG_ERROR);
  } finally {
    if (fd !== undefined) {
      try { closeSync(fd); } catch { throw new Error(CONFIG_ERROR); }
    }
  }
}

/** @param {string | undefined} path @returns {Client} */
export function createClient(path) {
  return openBridge(path).invoke;
}

/** @param {Buffer} buffer @returns {{ text: string, json: unknown }} */
function parseJSON(buffer) {
  const text = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(buffer);
  const json = JSON.parse(text, (key, value) => {
    if (["__proto__", "prototype", "constructor"].includes(key) ||
        (typeof value === "number" && !Number.isFinite(value))) throw new Error();
    return value;
  });
  return { text, json };
}

/** @param {string} text @param {number} [count] */
function validRoleReceiptWire(text, count = 4) {
  // JSON syntax and decoded field types are already checked. The fixed receipts
  // allow only primitive fields, unique decoded keys and unsigned decimal integers.
  const wire = text.trim();
  if (wire[0] !== "{") return false;
  const field = /\s*("(?:[^"\\]|\\.)*")\s*:\s*("(?:[^"\\]|\\.)*"|true|false|0|[1-9]\d*)\s*([,}])/y;
  field.lastIndex = 1;
  const keys = new Set();
  for (let index = 0; index < count; index++) {
    const match = field.exec(wire);
    if (!match || match[3] !== (index === count - 1 ? "}" : ",")) return false;
    const key = JSON.parse(match[1]);
    if (keys.has(key)) return false;
    keys.add(key);
  }
  return field.lastIndex === wire.length;
}

/** @param {unknown} value @param {string} token @param {boolean} roleWrite @param {string} toolName */
function serverError(value, token, roleWrite, toolName) {
  if (!isRecord(value) || !isRecord(value.error) || typeof value.error.code !== "string") return undefined;
  const raw = value.error.code;
  let code = /^[a-z][a-z0-9_]{0,63}$/.test(raw) && !raw.includes(token) ? raw : "request_failed";
  if (code.includes(token)) code = "";
  const message = toolName === "read_work_inputs" && raw === "tool_result_too_large" ?
    "完整工作输入超过响应上限，未返回部分内容；当前工具不支持分段读取" :
    ((!roleWrite || raw === "request_canceled" || raw === "deadline_exceeded" ?
      SERVER_ERROR_MESSAGES.get(raw) : undefined) || "成员工具请求失败");
  return code ? `${message}（${code}）。` : `${message}。`;
}

/** @param {unknown} value */
function validReceiptTime(value) {
  if (typeof value !== "string" ||
      !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/.test(value) ||
      !Number.isFinite(Date.parse(value))) return false;
  // Check wall-clock fields without shifting offsets or rounding fractional seconds.
  const wall = value.slice(0, 19);
  const date = new Date(`${wall}Z`);
  return Number.isFinite(date.getTime()) && date.toISOString().slice(0, 19) === wall;
}

/** @param {string} toolName */
function isWorkInputWrite(toolName) {
  return toolName === "ack_work_input" || toolName === "hand_back_work_input";
}

/** @param {string} toolName @param {number} status @param {unknown} value @param {unknown} requestValue */
function validWriteReceipt(toolName, status, value, requestValue) {
  // Role receipts are fixed projections; ordinary receipts remain additive.
  if (isWorkInputWrite(toolName)) {
    return status === 201 && isRecord(value) && Object.keys(value).join(",") === "input_id" &&
      typeof value.input_id === "string" && value.input_id !== "" && value.input_id === requestValue;
  }
  if (toolName === "decide_dispatch" || toolName === "submit_result") {
    if (status !== 201 || !isRecord(value) || !validReceiptTime(value.created_at)) return false;
    if (toolName === "decide_dispatch") {
      return Object.keys(value).sort().join(",") === "assignment_id,created_at,decision_id,kind" &&
        typeof value.decision_id === "string" && value.decision_id.length > 0 && value.kind === requestValue &&
        (value.kind === "assign" ? typeof value.assignment_id === "string" && value.assignment_id.length > 0 :
          value.kind === "decline" && value.assignment_id === "");
    }
    return Object.keys(value).sort().join(",") === "artifact_version_count,created_at,submission_id,summary_omitted" &&
      typeof value.submission_id === "string" && value.submission_id.length > 0 &&
      typeof value.artifact_version_count === "number" && Number.isSafeInteger(value.artifact_version_count) &&
      value.artifact_version_count >= 0 && value.summary_omitted === true;
  }
  if (toolName === "workspace_write" || toolName === "publish_workspace_artifact") {
    if (status !== 201 || !isRecord(value)) return false;
    const hash = (/** @type {unknown} */ digest) => typeof digest === "string" && /^[a-f0-9]{64}$/.test(digest);
    const id = (/** @type {unknown} */ identity) => typeof identity === "string" && identity.length > 0 && Buffer.byteLength(identity) <= 256;
    if (toolName === "workspace_write") {
      return Object.keys(value).sort().join(",") === "after_sha256,before_sha256,operation_id,outcome,text_omitted" &&
        id(value.operation_id) && (value.outcome === "done" || value.outcome === "unchanged") &&
        (value.before_sha256 === "" || hash(value.before_sha256)) && hash(value.after_sha256) && value.text_omitted === true;
    }
    return Object.keys(value).sort().join(",") === "applied,artifact_id,sha256,size,version_id" &&
      id(value.artifact_id) && id(value.version_id) && hash(value.sha256) &&
      Number.isSafeInteger(value.size) && typeof value.size === "number" && value.size >= 0 && value.size <= 65536 &&
      typeof value.applied === "boolean";
  }
  if (toolName !== "send_message" && toolName !== "create_member") return true;
  if (!isRecord(value)) return false;
  if (toolName === "send_message") {
    return status === 201 && typeof value.message_id === "string" && value.message_id.length > 0 &&
      validReceiptTime(value.created_at) &&
      typeof value.delivery_count === "number" && Number.isSafeInteger(value.delivery_count) &&
      value.delivery_count >= 0 && value.content_omitted === true;
  }
  return typeof value.member_id === "string" && value.member_id.length > 0 &&
    typeof value.join_message_id === "string" && (status === 200 ?
      value.reused === true && value.join_message_id === "" :
      status === 201 && value.reused === false && value.join_message_id.length > 0);
}

/**
 * No retry or redirect: once sent, a missing receipt is an unknown outcome.
 * @param {URL} endpoint
 * @param {string} token
 * @param {WriteState} writes
 * @param {string} toolName
 * @param {unknown} params
 * @param {string} toolCallId
 * @param {AbortSignal | undefined} signal
 * @param {string | undefined} runExplanation
 * @returns {Promise<Reply>}
 */
async function requestTool(endpoint, token, writes, toolName, params, toolCallId, signal, runExplanation) {
  /** @param {string} message */
  const safeError = message => new Error(message.replaceAll(token, "〔已隐藏〕"));
  if (!TOOL_NAMES.some(name => name === toolName)) throw safeError("成员工具名称无效，未发送请求。");
  const workInputWrite = isWorkInputWrite(toolName);
  const roleWrite = toolName === "decide_dispatch" || toolName === "submit_result" || workInputWrite;
  const workspaceWrite = toolName === "workspace_write" || toolName === "publish_workspace_artifact";
  const explanationWrite = toolName === PUBLISH_RUN_EXPLANATION;
  const write = roleWrite || workspaceWrite || explanationWrite || toolName === "send_message" || toolName === "create_member";
  // Hashing malformed UTF-16 as UTF-8 would alias valid replacement characters.
  if (typeof toolCallId !== "string" || !toolCallId.trim() || (write && !validUnicode(toolCallId)) ||
      (explanationWrite && (!/^[!-~]{1,256}$/.test(toolCallId) || toolCallId.includes(token)))) {
    throw safeError("工具调用标识无效，未发送请求。");
  }
  let body;
  /** @type {unknown} */
  let requestValue;
  let explanationTextSHA256 = "";
  try {
    const text = JSON.stringify(params);
    if (typeof text !== "string" || text.includes(token)) throw new Error();
    body = Buffer.from(text, "utf8");
    if (toolName === "decide_dispatch" || workInputWrite) {
      const snapshot = JSON.parse(text);
      if (isRecord(snapshot)) requestValue = workInputWrite ? snapshot.input_id : snapshot.kind;
    }
    if (explanationWrite) {
      const snapshot = JSON.parse(text);
      if (!isRecord(snapshot) || Object.keys(snapshot).join(",") !== "text" || typeof snapshot.text !== "string" ||
          !validUnicode(snapshot.text) || Buffer.byteLength(snapshot.text, "utf8") > RUN_EXPLANATION_BYTES ||
          !/\P{White_Space}/u.test(snapshot.text) || snapshot.text.includes("\x00")) throw new Error();
      explanationTextSHA256 = createHash("sha256").update(snapshot.text, "utf8").digest("hex");
    }
  } catch {
    throw safeError("成员工具参数无效，未发送请求。");
  }
  if (body.length > BYTE_LIMIT) throw safeError("工具请求超过 64 KiB 限制，未发送请求。");
  const fingerprint = write ? createHash("sha256").update(body).digest("hex") : "";
  if (write && writes.unknown) {
    const previous = writes.unknown;
    if (previous.toolName === toolName && previous.toolCallId === toolCallId &&
        (previous.body ? previous.body.equals(body) : previous.fingerprint === fingerprint)) {
      throw previous.error;
    }
    throw safeError("成员工具先前写入结果未知，当前实例已阻止后续写入；请先对账，不要自动重试。");
  }
  // Local validation and pre-abort are not_sent; only transport outcomes can latch.
  // Only the opted-in profile serializes writes, so no concurrent call can
  // pass the uncertainty gate while the explanation is on the wire.
  if (runExplanation && write && writes.active) throw safeError("前一写入尚未完成，未发送请求。");
  if (signal?.aborted) throw safeError("工具调用已取消，请求尚未发送。");
  const key = write ? createHash("sha256").update(explanationWrite ? `${toolName}\0${runExplanation}\0${toolCallId}` : toolName + toolCallId).digest("hex") : "";
  if (runExplanation && write) writes.active = true;

  return new Promise((resolve, reject) => {
    /** @type {import('node:http').ClientRequest | undefined} */
    let req;
    /** @type {import('node:http').IncomingMessage | undefined} */
    let response;
    /** @type {ReturnType<typeof setTimeout> | undefined} */
    let timer;
    let sent = false;
    let settled = false;
    const unknown = explanationWrite ? "运行说明保存结果未知；请保留原工具调用标识、幂等键和原文，先对账，不要自动重试。" :
      "执行结果未知；请先对账，不要自动重试。";
    const invalidResponse = `成员工具返回无效响应，${unknown}`;
    const overflow = `工具结果超过 64 KiB 限制（tool_result_too_large），${unknown}`;
    const interrupted = () => sent ? `成员工具请求中断，${unknown}` : "成员工具连接失败，请求尚未发送。";
    const cleanup = () => {
      if (runExplanation && write) writes.active = false;
      clearTimeout(timer);
      signal?.removeEventListener("abort", onAbort);
    };
    /** @param {string} message @param {FailureOutcome} [outcome] */
    const fail = (message, outcome = sent ? "unknown" : "not_sent") => {
      if (settled) return;
      settled = true;
      const error = sent && outcome === "unknown" && explanationTextSHA256 ?
        new RunExplanationUnknownError(unknown.replaceAll(token, "〔已隐藏〕"), key, explanationTextSHA256) : safeError(message);
      if (write && outcome === "unknown" && !writes.unknown) {
        writes.unknown = { toolName, toolCallId, fingerprint, error, ...(explanationWrite ? { body } : {}) };
      }
      cleanup();
      response?.destroy();
      req?.destroy();
      reject(error);
    };
    const onAbort = () => fail(sent ? `工具调用已取消，${unknown}` : "工具调用已取消，请求尚未发送。");
    try {
      /** @type {Record<string, string | number>} */
      const headers = { Authorization: `Bearer ${token}`, "Content-Type": "application/json", "Content-Length": body.length };
      if (write) headers["Idempotency-Key"] = key;
      req = request({
        hostname: endpoint.hostname.replace(/^\[|\]$/g, ""), port: endpoint.port || 80,
        path: `${endpoint.pathname}/${toolName}`, method: "POST", headers, agent: false, maxHeaderSize: 8192,
      }, res => {
        response = res;
        res.on("error", () => fail(interrupted()));
        res.on("aborted", () => fail(interrupted()));
        res.on("close", () => { if (!res.complete) fail(interrupted()); });
        const status = res.statusCode || 0;
        if (status >= 300 && status < 400) return fail(`成员工具服务返回重定向，已拒绝继续；${unknown}`);
        if (!/^application\/json(?:\s*;\s*charset=utf-8)?$/i.test(res.headers["content-type"] || "") ||
            (res.headers["content-encoding"] && res.headers["content-encoding"] !== "identity")) return fail(invalidResponse);
        if (Number(res.headers["content-length"]) > BYTE_LIMIT) return fail(overflow);
        /** @type {Buffer[]} */
        const chunks = [];
        let size = 0;
        res.on("data", chunk => {
          size += chunk.length;
          if (size > BYTE_LIMIT) return fail(overflow);
          chunks.push(chunk);
        });
        res.on("end", () => {
          if (settled) return;
          let result;
          try { result = parseJSON(Buffer.concat(chunks, size)); } catch { return fail(invalidResponse); }
          const success = roleWrite || workspaceWrite || explanationWrite || toolName === "send_message" ? status === 201 :
            toolName === "create_member" ? status === 200 || status === 201 : status === 200;
          const sensitive = result.text.includes(token) || JSON.stringify(result.json)?.includes(token);
          if (!success) {
            const message = status >= 400 && status <= 599 ? serverError(result.json, token, roleWrite, toolName) : undefined;
            const error = isRecord(result.json) && isRecord(result.json.error) ? result.json.error : undefined;
            const code = error?.code;
            // A malformed response cannot prove definite rejection of the
            // optional publication. Keep the legacy receipt rules untouched.
            const envelope = explanationWrite && /^\s*\{\s*"error"\s*:\s*(\{[\s\S]*\})\s*\}\s*$/.exec(result.text);
            const rejected = !sensitive && typeof code === "string" && typeof error?.message === "string" &&
              (!explanationWrite || envelope && validRoleReceiptWire(envelope[1], 2)) &&
              (REJECTION_CODES.get(status)?.includes(code) ||
                (toolName === "read_work_inputs" && status === 413 && code === "tool_result_too_large") ||
                (toolName !== "decide_dispatch" && ((status === 404 && code === "not_found") || (status === 409 && code === "conflict"))) ||
                (!roleWrite && status === 409 && code === "member_capacity") ||
                (explanationWrite && status === 503 && code === "run_explanation_unavailable"));
            const suffix = rejected || !explanationWrite && (code === "request_canceled" || code === "deadline_exceeded") ? "" : ` ${unknown}`;
            return fail(message ? message + suffix : invalidResponse, rejected ? "rejected" : "unknown");
          }
          if (sensitive) return fail(`成员工具返回了敏感数据，已拒绝显示；${unknown}`);
          if (!validWriteReceipt(toolName, status, result.json, requestValue)) return fail(invalidResponse);
          if (explanationWrite) {
            const { json: ack, text } = result;
            if (!isRecord(ack) || Object.keys(ack).sort().join(",") !== "created_at,explanation_id,text_omitted" ||
                ack.explanation_id !== `public_explanation:${runExplanation}` || ack.text_omitted !== true ||
                !validReceiptTime(ack.created_at) || !validRoleReceiptWire(text, 3)) return fail(invalidResponse);
            result = { text: JSON.stringify(ack), json: ack };
          }
          if (roleWrite || workspaceWrite) {
            if (!validRoleReceiptWire(result.text, workInputWrite ? 1 : workspaceWrite ? 5 : 4)) return fail(invalidResponse);
            result = { text: JSON.stringify(result.json), json: result.json };
          }
          settled = true;
          cleanup();
          resolve(explanationWrite ? { ...result, explanationKey: key } : result);
        });
      });
      req.on("error", () => fail(interrupted()));
      signal?.addEventListener("abort", onAbort, { once: true });
      if (signal?.aborted) return onAbort();
      timer = setTimeout(() => fail(`成员工具请求超时，${unknown}`), REQUEST_TIMEOUT_MS);
      sent = true;
      req.end(body);
    } catch {
      fail(interrupted());
    }
  });
}
