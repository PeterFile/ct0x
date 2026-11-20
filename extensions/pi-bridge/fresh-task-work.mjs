import { createHash } from "node:crypto";
import { constants, openSync, readSync, fstatSync, closeSync } from "node:fs";
import { request } from "node:http";
import { isAbsolute } from "node:path";

// Shared only by the pinned fresh typed entries. Keep the published ordinary
// index.ts/bridge.mjs pair independent (including its pins).
const CONFIG_ERROR = "任务续办配置无效，未启用任何工具。";
const UNKNOWN = "执行结果未知；请保留原调用标识、幂等键和内容先对账，不要自动重试。";
const BYTE_LIMIT = 64 * 1024;
const decisionOperation = "propose_task_decision";
const statuses = new Map([["read_work_inputs", 200], ["ack_work_input", 200], ["hand_back_work_input", 200], ["submit_result", 201], [decisionOperation, 201]]);
const definiteRejections = new Map([
  [400, ["invalid_input", "invalid_json"]], [403, ["forbidden", "invalid_host", "invalid_origin"]],
  [404, ["not_found"]], [405, ["method_not_allowed"]], [409, ["conflict"]],
  [413, ["body_too_large"]], [415, ["unsupported_media_type"]],
]);

/** @param {string | Buffer} bytes */
const sha256 = bytes => createHash("sha256").update(bytes).digest("hex");
/** @param {Buffer} bytes */
const decode = bytes => new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(bytes);
/** @param {unknown} value @returns {value is Record<string, unknown>} */
const record = value => value !== null && typeof value === "object" && !Array.isArray(value);
/** @param {Record<string, unknown>} obj @param {string} keys */
const hasKeys = (obj, keys) => Object.keys(obj).sort().join(",") === keys;
/** @param {string} value */
const unicode = value => !/[\ud800-\udfff]/u.test(value);

/** @param {string} id @param {string} token */
export function validTypedWriteCallID(id, token) {
  // Require a retainable native ID before sending a write. Keep its exact bytes.
  return id.length > 0 && id.length <= 256 && !/[^!-~]/.test(id) && !id.includes(token);
}

/** @typedef {{operation: string, tool_call_id: string, idempotency_key: string, request_sha256: string}} UnknownWrite */

/** @param {{text: string, writeUnknown?: UnknownWrite}} reply @param {string} role @param {string} runID */
export function freshTypedToolResult(reply, role, runID) {
  const content = [{ type: /** @type {const} */ ("text"), text: reply.text }];
  if (!reply.writeUnknown) return { content, details: undefined };
  // Pi serializes details in native tool_execution_end. The model-facing text
  // is still the same fixed, sanitized error and goes through normal recovery.
  return { content, isError: true, details: { agent_workspace_write_unknown_v1: { role, run_id: runID, ...reply.writeUnknown } } };
}

/** @param {unknown} value */
export function validWorkInputID(value) {
  return typeof value === "string" && value !== "" && Buffer.byteLength(value) <= 256 &&
    !/[\p{Cc}\ud800-\udfff]/u.test(value) && !/^\p{White_Space}|\p{White_Space}$/u.test(value);
}

/** @param {unknown} value */
export function validWorkInputCursor(value) {
  return typeof value === "string" && /^[A-Za-z0-9_-]{1,512}$/.test(value) && value === value.trim();
}

/** @param {string | undefined} path @param {string | undefined} expectedSHA256 */
function readConfig(path, expectedSHA256) {
  let fd;
  try {
    if (!path || !isAbsolute(path) || !expectedSHA256 || !/^[a-f0-9]{64}$/.test(expectedSHA256)) throw new Error();
    fd = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
    const stat = fstatSync(fd);
    const limit = 4096;
    if (!stat.isFile() || (stat.mode & 0o7777) !== 0o600 || stat.size > limit ||
        (process.getuid && stat.uid !== process.getuid())) throw new Error();
    const buffer = Buffer.alloc(limit + 1);
    let size = 0;
    while (size < buffer.length) {
      const count = readSync(fd, buffer, size, buffer.length - size, null);
      if (!count) break;
      size += count;
    }
    const bytes = buffer.subarray(0, size);
    if (size > limit || sha256(bytes) !== expectedSHA256) throw new Error();
    const config = JSON.parse(decode(bytes));
    if (!record(config)) throw new Error();
    const proof = config.task_decision_proof, runID = config.run_id;
    if ((proof === undefined ? !hasKeys(config, "endpoint,token") :
        !hasKeys(config, "endpoint,run_id,task_decision_proof,token") || typeof proof !== "string" || !/^[a-f0-9]{64}$/.test(proof) || proof === config.token ||
        typeof runID !== "string" || !/^[A-Za-z0-9](?:[A-Za-z0-9_-]{0,62}[A-Za-z0-9])?$/.test(runID)) ||
        typeof config.token !== "string" || config.token !== config.token.trim() || config.token.length > 256 || !/^[A-Za-z0-9._~+/-]+=*$/.test(config.token) ||
        typeof config.endpoint !== "string" || config.endpoint !== config.endpoint.trim() ||
        !/^http:\/\/(127\.0\.0\.1|\[::1\]):[0-9]{1,5}\/api\/v1\/member-tools$/.test(config.endpoint)) throw new Error();
    const endpoint = new URL(config.endpoint);
    if (endpoint.port === "0") throw new Error();
    return { endpoint, token: config.token, proof: /** @type {string | undefined} */ (proof), decisionRunID: /** @type {string | undefined} */ (runID) };
  } catch {
    throw new Error(CONFIG_ERROR);
  } finally {
    if (fd !== undefined) {
      try { closeSync(fd); } catch { throw new Error(CONFIG_ERROR); }
    }
  }
}

/** @param {unknown} value */
function validTime(value) {
  if (typeof value !== "string" || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$/.test(value) || !Number.isFinite(Date.parse(value))) return false;
  return new Date(value).toISOString().slice(0, 19) === value.slice(0, 19);
}

/** @param {string} text @param {number} count */
function fixedReceipt(text, count) {
  // A write requires unique decoded keys and primitive receipt fields. JSON.parse
  // alone would accept duplicates and noncanonical equivalents such as -0.
  const wire = text.trim();
  if (!wire.startsWith("{")) return false;
  const field = /\s*("(?:[^"\\]|\\.)*")\s*:\s*("(?:[^"\\]|\\.)*"|true|false|0|[1-9]\d*)\s*([,}])/y;
  field.lastIndex = 1;
  const keys = new Set();
  for (let i = 0; i < count; i++) {
    const match = field.exec(wire);
    if (!match || match[3] !== (i === count - 1 ? "}" : ",")) return false;
    keys.add(JSON.parse(match[1]));
  }
  return keys.size === count && field.lastIndex === wire.length;
}

/** @param {string} name @param {unknown} value @param {Record<string, unknown>} sent @param {string} wire */
function validReply(name, value, sent, wire) {
  if (!record(value)) return false;
  if (name === decisionOperation) {
    const v = value.version, expected = Number(sent.expected_version);
    return hasKeys(value, "decision_request_id,kind,version") && fixedReceipt(wire, 3) &&
      validWorkInputID(value.decision_request_id) && value.kind === sent.kind && typeof v === "number" &&
      Number.isSafeInteger(v) && v > 0 && (expected === 0 || v === expected || v === expected + 1);
  }
  if (name === "submit_result") {
    return hasKeys(value, "artifact_version_count,created_at,submission_id,summary_omitted") && fixedReceipt(wire, 4) &&
      validWorkInputID(value.submission_id) && validTime(value.created_at) && value.artifact_version_count === 0 && value.summary_omitted === true;
  }
  if (name !== "read_work_inputs") {
    return hasKeys(value, "disposition,input_id") && fixedReceipt(wire, 2) && value.input_id === sent.input_id &&
      value.disposition === (name === "ack_work_input" ? "confirmed" : "returned");
  }
  if (!Object.hasOwn(sent, "input_id")) {
    return hasKeys(value, "items,next_cursor") && Array.isArray(value.items) && value.items.length <= Number(sent.limit ?? 10) &&
      (value.next_cursor === "" || (value.items.length > 0 && value.next_cursor !== sent.before && validWorkInputCursor(value.next_cursor))) &&
      value.items.every(i => record(i) && hasKeys(i, "created_at,input_id") && validWorkInputID(i.input_id) && validTime(i.created_at));
  }
  const count = value.byte_count;
  return hasKeys(value, "byte_count,complete,input_id,next_offset,offset,text") && value.input_id === sent.input_id &&
    value.offset === (sent.offset ?? 0) && Number.isSafeInteger(value.next_offset) &&
    typeof count === "number" && Number.isSafeInteger(count) && count >= 0 && count <= Number(sent.max_bytes ?? 4096) &&
    value.next_offset === Number(value.offset) + count && typeof value.text === "string" && unicode(value.text) &&
    Buffer.byteLength(value.text) === count && typeof value.complete === "boolean" && (value.complete || count > 0);
}

/** @param {URL} endpoint @param {string} name @param {Buffer} body @param {Record<string, string | number>} headers @param {AbortSignal | undefined} signal @returns {Promise<{ status: number, text: string, receiptKey: string | string[] | undefined }>} */
function exchange(endpoint, name, body, headers, signal) {
  // No pooled socket, proxy, redirects, retry, or unbounded buffers.
  return new Promise((resolve, reject) => {
    /** @type {import('node:http').ClientRequest | undefined} */
    let req;
    /** @type {import('node:http').IncomingMessage | undefined} */
    let res;
    const cleanup = () => { clearTimeout(timer); signal?.removeEventListener("abort", stop); };
    const stop = () => { cleanup(); reject(new Error()); res?.destroy(); req?.destroy(); };
    const timer = setTimeout(stop, 25_000);
    try {
      req = request({ hostname: endpoint.hostname.replace(/^\[|\]$/g, ""), port: endpoint.port || 80,
        path: `${endpoint.pathname}/${name}`, method: "POST", headers, agent: false, maxHeaderSize: 8192 }, response => {
        res = response;
        res.on("error", stop).on("aborted", stop).on("close", () => { if (!response.complete) stop(); });
        const status = res.statusCode || 0;
        if (status < 200 || (status >= 300 && status < 400) || status > 599 ||
            !/^application\/json(?:\s*;\s*charset=utf-8)?$/i.test(res.headers["content-type"] || "") ||
            (res.headers["content-encoding"] && res.headers["content-encoding"] !== "identity") ||
            Number(res.headers["content-length"]) > BYTE_LIMIT) return stop();
        /** @type {Buffer[]} */
        const chunks = [];
        let size = 0;
        res.on("data", chunk => {
          size += chunk.length;
          if (size > BYTE_LIMIT) return stop();
          chunks.push(chunk);
        });
        res.on("end", () => {
          try {
            if (!response.complete) return stop();
            const text = decode(Buffer.concat(chunks, size));
            cleanup();
            resolve({ status, text, receiptKey: response.headers["idempotency-key"] });
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

/** @param {string | undefined} path @param {string | undefined} digest */
export function createFreshTaskWorkClient(path, digest) {
  const { endpoint, token, proof, decisionRunID } = readConfig(path, digest);
  /** @type {{key: string, fingerprint: string, error: Error} | undefined} */
  let uncertain;
  let writing = false;
  /** @param {string} value */
  const hasSecret = value => value.includes(token) || (proof !== undefined && value.includes(proof));
  /** @param {string} message */
  const safe = message => new Error(proof === undefined ? message.replaceAll(token, "〔已隐藏〕") :
    message.replaceAll(proof, "〔已隐藏〕").replaceAll(token, "〔已隐藏〕"));
  /** @param {string} name @param {Record<string, unknown>} params @param {string} id @param {AbortSignal} [signal] @param {string} [runID] */
  const invoke = async (name, params, id, signal, runID) => {
    const expected = statuses.get(name);
    const write = name !== "read_work_inputs";
    const decision = name === decisionOperation;
    if (!expected || decision && (proof === undefined || runID !== decisionRunID) || typeof id !== "string" || !id.trim() || !unicode(id) ||
        (write && (!validTypedWriteCallID(id, token) || hasSecret(id)))) {
      throw safe("成员工具调用无效，未发送请求。");
    }
    // The entry has already snapshotted and checked the argument shape.
    const json = JSON.stringify(params);
    const body = Buffer.from(json, "utf8");
    if (body.length > BYTE_LIMIT || hasSecret(json)) throw safe("成员工具参数无效，未发送请求。");
    // Only the proposal uses the issued and SDK-verified Run identity.
    const key = decision ? sha256(`${name}\0${runID}\0${id}`) : write ? sha256(name + id) : "";
    const fingerprint = write ? sha256(body) : "";
    if (write && uncertain) {
      if (uncertain.key === key && uncertain.fingerprint === fingerprint) throw uncertain.error;
      throw safe(`当前实例已阻止后续写入。${UNKNOWN}`);
    }
    if (write && writing) throw safe("前一写入尚未完成，未发送请求。");
    if (signal?.aborted) throw safe("工具调用已取消，未发送请求。");
    /** @type {Record<string, string | number>} */
    const headers = { Authorization: `Bearer ${token}`, "Content-Type": "application/json", "Content-Length": body.length };
    if (write) headers["Idempotency-Key"] = key;
    if (decision && proof !== undefined) {
      headers["X-Agent-Workspace-Tool-Call-Id"] = id;
      headers["X-Agent-Workspace-Task-Decision-Proof"] = proof;
    }
    if (write) writing = true;
    let failure = UNKNOWN;
    try {
      const { status, text, receiptKey } = await exchange(endpoint, name, body, headers, signal);
      const result = JSON.parse(text, (key, value) => {
        if (["__proto__", "constructor", "prototype"].includes(key)) throw new Error();
        return value;
      });
      if (hasSecret(text) || hasSecret(JSON.stringify(result))) throw new Error();
      if (status === expected && (!decision || receiptKey === key) && validReply(name, result, params, text)) return { text: JSON.stringify(result) };
      const envelope = /^\s*\{\s*"error"\s*:\s*(\{[\s\S]*\})\s*\}\s*$/.exec(text);
      if (envelope && record(result.error) && typeof result.error.code === "string" && typeof result.error.message === "string" &&
          fixedReceipt(envelope[1], 2) && (definiteRejections.get(status)?.includes(result.error.code) ||
            (!write && status === 413 && result.error.code === "tool_result_too_large"))) {
        failure = `成员工具请求被拒绝（${result.error.code}）。`;
      }
    } catch { /* Missing or invalid evidence must remain unknown. */ }
    finally { if (write) writing = false; }
    const error = safe(failure);
    if (write && failure === UNKNOWN) {
      uncertain = { key, fingerprint, error };
      return { text: error.message, writeUnknown: { operation: name, tool_call_id: id, idempotency_key: key, request_sha256: fingerprint } };
    }
    throw error;
  };
  return Object.assign(invoke, { taskDecision: proof !== undefined });
}
