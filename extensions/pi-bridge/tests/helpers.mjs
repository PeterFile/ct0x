import assert from "node:assert/strict";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createServer } from "node:http";

export const TOKEN = "fixture-capability-NOT-A-REAL-CREDENTIAL-937519";
export const BASE = "/api/v1/member-tools";
export const BUDGET = 64 * 1024;

export async function privateConfig(t, config = {}, options = {}) {
  const directory = await mkdtemp(join(tmpdir(), "pi-bridge-test-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const path = join(directory, "capability.json");
  await writeFile(path, JSON.stringify({
    endpoint: `http://127.0.0.1:7310${BASE}`,
    token: TOKEN,
    ...config,
  }), { mode: 0o600, ...options });
  return path;
}

export async function loopback(t, handler, host = "127.0.0.1") {
  const requests = [];
  const server = createServer(async (request, response) => {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const body = Buffer.concat(chunks);
    requests.push({ headers: request.headers, rawHeaders: request.rawHeaders,
      method: request.method, url: request.url, body });
    handler(request, response, body);
  });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, host, resolve);
  });
  t.after(async () => {
    server.closeAllConnections();
    await new Promise(resolve => server.close(resolve));
  });
  const address = host.includes(":") ? `[${host}]` : host;
  return { server, requests, endpoint: `http://${address}:${server.address().port}${BASE}` };
}

export function json(response, value, status = 200) {
  response.writeHead(status, { "Content-Type": "application/json" });
  response.end(JSON.stringify(value));
}

export function safeFailure(error, expected, secrets = [TOKEN]) {
  assert.ok(error instanceof Error);
  assert.match(error.message, expected);
  assert.match(error.message, /[\u3400-\u9fff]/u);
  for (const secret of secrets) {
    assert.ok(!`${String(error)} ${JSON.stringify(error)}`.includes(secret), "secret escaped into an error");
  }
  assert.equal(error.cause, undefined);
  return true;
}
