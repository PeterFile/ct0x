import { constants, openSync, fstatSync, readSync, closeSync } from "node:fs";
import { isAbsolute } from "node:path";
import { createHash } from "node:crypto";
import { request } from "node:http";

const CONFIG_LIMIT = 4096;
const BYTE_LIMIT = 64 * 1024;
const REQUEST_TIMEOUT_MS = 25_000;
export const TOOL_NAMES = /** @type {const} */ (["list_members", "read_channel", "send_message", "create_member"]);

/** @typedef {{ text: string, json: unknown }} Reply */
/** @typedef {(toolName: string, params: unknown, toolCallId: string, signal?: AbortSignal) => Promise<Reply>} Client */
/** @typedef {"not_sent" | "rejected" | "unknown"} FailureOutcome */
/** @typedef {{ unknown?: { toolName: string, toolCallId: string, fingerprint: string, error: Error } }} WriteState */
const REJECTION_CODES = new Map([
  [400, ["invalid_input", "invalid_json"]], [403, ["forbidden", "invalid_host", "invalid_origin"]],
  [404, ["not_found"]], [405, ["method_not_allowed"]], [409, ["conflict", "member_capacity"]],
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
 * Read once; the launcher owns the config file's lifetime.
 * @param {string | undefined} path
 * @returns {Client}
 */
export function createClient(path) {
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
    const config = JSON.parse(new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(buffer.subarray(0, size)));
    if (!isRecord(config) || Object.keys(config).sort().join(",") !== "endpoint,token" ||
        typeof config.token !== "string" || !/^[A-Za-z0-9._~+/-]+=*$/.test(config.token)) {
      throw new Error(CONFIG_ERROR);
    }
    const endpoint = endpointURL(config.endpoint);
    const token = config.token;
    /** @type {WriteState} */
    const writes = {};
    return (toolName, params, toolCallId, signal) => requestTool(endpoint, token, writes, toolName, params, toolCallId, signal);
  } catch {
    throw new Error(CONFIG_ERROR);
  } finally {
    if (fd !== undefined) {
      try { closeSync(fd); } catch { throw new Error(CONFIG_ERROR); }
    }
  }
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

/** @param {unknown} value @param {string} token */
function serverError(value, token) {
  if (!isRecord(value) || !isRecord(value.error) || typeof value.error.code !== "string") return undefined;
  const raw = value.error.code;
  let code = /^[a-z][a-z0-9_]{0,63}$/.test(raw) && !raw.includes(token) ? raw : "request_failed";
  if (code.includes(token)) code = "";
  const message = SERVER_ERROR_MESSAGES.get(raw) || "成员工具请求失败";
  return code ? `${message}（${code}）。` : `${message}。`;
}

/** @param {string} toolName @param {number} status @param {unknown} value */
function validWriteReceipt(toolName, status, value) {
  if (toolName !== "send_message" && toolName !== "create_member") return true;
  if (!isRecord(value)) return false;
  if (toolName === "send_message") {
    return status === 201 && typeof value.message_id === "string" && value.message_id.length > 0 &&
      typeof value.created_at === "string" &&
      /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/.test(value.created_at) &&
      Number.isFinite(Date.parse(value.created_at)) &&
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
 * @returns {Promise<Reply>}
 */
async function requestTool(endpoint, token, writes, toolName, params, toolCallId, signal) {
  /** @param {string} message */
  const safeError = message => new Error(message.replaceAll(token, "〔已隐藏〕"));
  if (!TOOL_NAMES.some(name => name === toolName)) throw safeError("成员工具名称无效，未发送请求。");
  if (typeof toolCallId !== "string" || !toolCallId.trim()) throw safeError("工具调用标识无效，未发送请求。");
  let body;
  try {
    const text = JSON.stringify(params);
    if (typeof text !== "string" || text.includes(token)) throw new Error();
    body = Buffer.from(text, "utf8");
  } catch {
    throw safeError("成员工具参数无效，未发送请求。");
  }
  if (body.length > BYTE_LIMIT) throw safeError("工具请求超过 64 KiB 限制，未发送请求。");
  const write = toolName === "send_message" || toolName === "create_member";
  const fingerprint = write ? createHash("sha256").update(body).digest("hex") : "";
  if (write && writes.unknown) {
    const previous = writes.unknown;
    if (previous.toolName === toolName && previous.toolCallId === toolCallId && previous.fingerprint === fingerprint) {
      throw previous.error;
    }
    throw safeError("成员工具先前写入结果未知，当前实例已阻止后续写入；请先对账，不要自动重试。");
  }
  // Local validation and pre-abort are not_sent; only transport outcomes can latch.
  if (signal?.aborted) throw safeError("工具调用已取消，请求尚未发送。");

  return new Promise((resolve, reject) => {
    /** @type {import('node:http').ClientRequest | undefined} */
    let req;
    /** @type {import('node:http').IncomingMessage | undefined} */
    let response;
    /** @type {ReturnType<typeof setTimeout> | undefined} */
    let timer;
    let sent = false;
    let settled = false;
    const unknown = "执行结果未知；请先对账，不要自动重试。";
    const invalidResponse = `成员工具返回无效响应，${unknown}`;
    const overflow = `工具结果超过 64 KiB 限制（tool_result_too_large），${unknown}`;
    const interrupted = () => sent ? `成员工具请求中断，${unknown}` : "成员工具连接失败，请求尚未发送。";
    const cleanup = () => {
      clearTimeout(timer);
      signal?.removeEventListener("abort", onAbort);
    };
    /** @param {string} message @param {FailureOutcome} [outcome] */
    const fail = (message, outcome = sent ? "unknown" : "not_sent") => {
      if (settled) return;
      settled = true;
      const error = safeError(message);
      if (write && outcome === "unknown" && !writes.unknown) {
        writes.unknown = { toolName, toolCallId, fingerprint, error };
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
      if (write) {
        headers["Idempotency-Key"] = createHash("sha256").update(toolName + toolCallId).digest("hex");
      }
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
          const success = toolName === "send_message" ? status === 201 :
            toolName === "create_member" ? status === 200 || status === 201 : status === 200;
          const sensitive = result.text.includes(token) || JSON.stringify(result.json)?.includes(token);
          if (!success) {
            const message = status >= 400 && status <= 599 ? serverError(result.json, token) : undefined;
            const error = isRecord(result.json) && isRecord(result.json.error) ? result.json.error : undefined;
            const code = error?.code;
            const rejected = !sensitive && typeof code === "string" && typeof error?.message === "string" &&
              REJECTION_CODES.get(status)?.includes(code);
            const suffix = rejected || code === "request_canceled" || code === "deadline_exceeded" ? "" : ` ${unknown}`;
            return fail(message ? message + suffix : invalidResponse, rejected ? "rejected" : "unknown");
          }
          if (sensitive) return fail(`成员工具返回了敏感数据，已拒绝显示；${unknown}`);
          if (!validWriteReceipt(toolName, status, result.json)) return fail(invalidResponse);
          settled = true;
          cleanup();
          resolve(result);
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
