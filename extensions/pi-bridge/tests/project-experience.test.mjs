import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { openBridge } from "../bridge.mjs";
import { BASE, TOKEN, json, loopback, privateConfig, safeFailure } from "./helpers.mjs";
import { loadExtensions } from "./pi-loader.mjs";

const entry = fileURLToPath(new URL("../index.ts", import.meta.url));
const lookup = "lookup_project_experiences", artifact = "read_artifact", investigation = "read_investigation_artifact";
const projection = ["search_history", "read_history", lookup];
const ids = { artifact_id: "export", version_id: "accepted-v2.9" };
const hash = value => createHash("sha256").update(value).digest("hex");

async function load(path, digest) {
  const env = { AGENT_WORKSPACE_MEMBER_TOOL_CONFIG: path, AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256: digest };
  const saved = Object.fromEntries(Object.keys(env).map(key => [key, process.env[key]]));
  const apply = values => Object.entries(values).forEach(([key, value]) => {
    if (value === undefined) delete process.env[key]; else process.env[key] = value;
  });
  try {
    apply(env);
    return await loadExtensions([entry], process.cwd());
  } finally {
    apply(saved);
  }
}

async function projected(t, endpoint, names) {
  const path = await privateConfig(t, { endpoint, project_experience_tools: names });
  const digest = hash(await readFile(path));
  const result = await load(path, digest);
  assert.deepEqual(result.errors, []);
  assert.equal(result.extensions.length, 1);
  return { path, digest, extension: result.extensions[0] };
}

const execute = (tool, args) => tool.execute("offline-call", args, undefined, undefined, { group_id: "must-not-select", grants: [artifact] });

test("qualified fresh Pi ABI registers only the issued history projection; metadata cannot add tools", async t => {
  const page = { items: [{ status: "active", accepted_outcome: ids }], next_cursor: "" };
  const server = await loopback(t, (_, response) => json(response, page));
  const { extension, path, digest } = await projected(t, server.endpoint, projection);
  assert.deepEqual([...extension.tools.keys()], projection);
  const lookupTool = extension.tools.get(lookup).definition;
  assert.deepEqual(Object.keys(lookupTool.parameters.properties), ["query", "component", "version", "limit", "before"]);
  assert.equal(lookupTool.parameters.additionalProperties, false);
  const query = { component: "CSV", version: "v2.9", query: "导出", before: "cursor-from-server", limit: 1 };
  assert.deepEqual(await execute(lookupTool, query), { content: [{ type: "text", text: JSON.stringify(page) }], details: undefined });
  assert.deepEqual(JSON.parse(server.requests[0].body), query);
  assert.equal(server.requests[0].url, `${BASE}/${lookup}`);
  assert.equal(server.requests[0].headers.authorization, `Bearer ${TOKEN}`);
  assert.equal(server.requests[0].headers["idempotency-key"], undefined);
  for (const input of [{}, { query: " " }, { query: "😀".repeat(129) }, { query: "\ud800" }, { query: "CSV", before: "bad=" },
    { component: " CSV" }, { limit: 21, query: "CSV" }, { limit: "2", query: "CSV" }, { limit: null, query: "CSV" },
    { query: "CSV", group_id: "other" }, { query: "CSV", grants: [artifact] }]) {
    assert.throws(() => lookupTool.prepareArguments(input), error => safeFailure(error, /参数/));
    await assert.rejects(execute(lookupTool, input), error => safeFailure(error, /参数/));
  }
  const direct = openBridge(path, digest);
  direct.projectExperienceTools.push(artifact); // Discovery results don't mutate the client.
  await assert.rejects(direct.invoke(artifact, ids, "direct-call"), error => safeFailure(error, /授权范围/));
  await assert.rejects(direct.invoke(investigation, ids, "direct-call"), error => safeFailure(error, /授权范围/));
  assert.deepEqual([...extension.tools.keys()], projection);
  assert.equal(server.requests.length, 1);
});

test("approved native profile forwards exact artifact bytes, not current-version or typed receipts", async t => {
  const bytes = Buffer.from([0, 0xff, 0x41]);
  const result = { ...ids, name: "export notes", current_version_id: "draft", source_task_id: "task", author_id: "user", sha256: hash(bytes),
    recorded_size: bytes.length, applied: true, offset: 0, next_offset: bytes.length, content_complete: true,
    encoding: "base64", content: bytes.toString("base64") };
  let reply = result;
  const server = await loopback(t, (_, response) => json(response, reply));
  const { extension } = await projected(t, server.endpoint, [...projection, artifact]);
  assert.deepEqual([...extension.tools.keys()], [...projection, artifact]);
  const read = extension.tools.get(artifact).definition;
  assert.deepEqual(read.parameters.required, ["artifact_id", "version_id"]);
  assert.equal(read.parameters.additionalProperties, false);
  const input = { ...ids, max_bytes: 3 };
  const response = await execute(read, input);
  assert.equal(response.content[0].text, JSON.stringify(result));
  assert.deepEqual(Buffer.from(JSON.parse(response.content[0].text).content, "base64"), bytes);
  assert.equal(server.requests[0].url, `${BASE}/${artifact}`);
  assert.equal(server.requests[0].headers["idempotency-key"], undefined);
  assert.deepEqual(JSON.parse(server.requests[0].body), input);
  for (const input of [{ artifact_id: "export" }, { ...ids, version_id: null }, { ...ids, path: "foo" }, { ...ids, version_id: "a/b" },
    { ...ids, version_id: "a\\b" }, { ...ids, offset: 67108865 }, { ...ids, max_bytes: 0 }]) {
    await assert.rejects(execute(read, input), error => safeFailure(error, /参数/));
  }
  assert.equal(server.requests.length, 1);
  reply = { ...result, version_id: "draft" };
  await assert.rejects(execute(read, input), error => safeFailure(error, /无效响应/));
  reply = { ...result, next_offset: 2 };
  await assert.rejects(execute(read, input), error => safeFailure(error, /无效响应/));
});

test("lookup and artifact projection require the exact digest and reject legacy or typed hybrids", async t => {
  const path = await privateConfig(t, { project_experience_tools: projection });
  const digest = hash(await readFile(path));
  for (const invalid of [undefined, "", (digest[0] === "0" ? "1" : "0") + digest.slice(1)]) {
    const loaded = await load(path, invalid);
    assert.equal(loaded.extensions.length, 0);
    assert.equal(loaded.errors.length, 1);
  }
  for (const invalid of [{ project_experience_tools: [artifact] }, { project_experience_tools: [...projection, artifact, artifact] },
    { project_experience_tools: ["read_history", "search_history", lookup] }, { project_experience_tools: [...projection, investigation] },
    { project_experience_tools: projection, investigation_tools: [investigation, "submit_result"] }]) {
    const path = await privateConfig(t, invalid);
    const loaded = await load(path, hash(await readFile(path)));
    assert.equal(loaded.extensions.length, 0);
    assert.equal(loaded.errors.length, 1);
  }
  const legacy = await load(await privateConfig(t));
  for (const name of [lookup, artifact, investigation]) assert.ok(!legacy.extensions[0].tools.has(name));
  const typedPath = await privateConfig(t, { investigation_tools: [investigation, "submit_result"] });
  const typed = await load(typedPath, hash(await readFile(typedPath)));
  assert.deepEqual([...typed.extensions[0].tools.keys()], [investigation, "submit_result"]);
});
