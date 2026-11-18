import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { test } from "node:test";
import { chmod, mkdir, readFile, symlink, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { privateConfig, safeFailure, TOKEN, BASE } from "./helpers.mjs";

import { createClient, openBridge } from "../bridge.mjs";

function sha256(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

function rejectConfig(path, digest) {
  for (const open of [() => createClient(path), () => openBridge(path, digest)]) {
    assert.throws(open, error => safeFailure(error, /配置/, [TOKEN, path, digest]));
  }
}

test("valid private config initializes a closure without exposing the capability", async t => {
  assert.equal(typeof createClient, "function");
  const path = await privateConfig(t);
  const client = createClient(path);
  assert.equal(typeof client, "function");
  assert.equal(JSON.stringify(client), undefined);
  assert.deepEqual(Object.keys(client), []);
});

test("private config may carry the continuation marker only as true", async t => {
  assert.equal(typeof createClient(await privateConfig(t, { continue_work: true })), "function");
  for (const marker of [false, null, "true", 1]) {
    const path = await privateConfig(t, { continue_work: marker });
    assert.throws(() => createClient(path), error => safeFailure(error, /配置/));
  }
});

test("bound config hashes exact JSON bytes and preserves the issued continuation hint", async t => {
  const path = await privateConfig(t, { continue_work: true });
  const original = await readFile(path, "utf8");
  const issued = `${original}\r\n`;
  const digest = sha256(issued);
  await writeFile(path, issued);
  assert.equal(openBridge(path, digest).continueWork, true);

  // Same JSON, length, mode and file; only the raw bytes changed.
  await writeFile(path, `${original} \n`);
  assert.equal(openBridge(path).continueWork, true);
  assert.throws(() => openBridge(path, digest), error => safeFailure(error, /配置/, [TOKEN, path, digest]));
});

test("missing, relative, or inline config fails closed without exposing input", () => {
  for (const input of [undefined, "", "relative/private.json", `{"token":"${TOKEN}"}`, `/missing/${TOKEN}`]) {
    assert.throws(() => createClient(input), error => safeFailure(error, /配置/));
  }
});

test("config must be a regular current-user 0600 file, not a symlink", async t => {
  const path = await privateConfig(t);
  const digest = sha256(await readFile(path));
  for (const mode of [0o644, 0o400, 0o660, 0o700, 0o1600]) {
    await chmod(path, mode);
    rejectConfig(path, digest);
  }
  await chmod(path, 0o600);
  const link = join(dirname(path), "link.json");
  await symlink(path, link);
  rejectConfig(link, digest);
  const directory = join(dirname(path), "directory");
  await mkdir(directory, { mode: 0o600 });
  rejectConfig(directory, digest);
});

test("malformed, excessive, non-UTF8 or extra-field config fails with a generic error", async t => {
  const path = await privateConfig(t);
  const invalid = [
    TOKEN, "null", "[]", "{}", JSON.stringify({ endpoint: `http://127.0.0.1:7310${BASE}` }),
    JSON.stringify({ endpoint: `http://127.0.0.1:7310${BASE}`, token: TOKEN, group_id: TOKEN }),
    JSON.stringify({ endpoint: `http://127.0.0.1:7310${BASE}`, token: "bad\r\nheader" }),
    JSON.stringify({ endpoint: `http://127.0.0.1:7310${BASE}`, token: "" }),
    JSON.stringify({ endpoint: `http://127.0.0.1:7310${BASE}`, token: 42 }),
    Buffer.from([0xff]), " ".repeat(4097),
    `\ufeff${JSON.stringify({ endpoint: `http://127.0.0.1:7310${BASE}`, token: TOKEN })}`,
  ];
  for (const contents of invalid) {
    await writeFile(path, contents);
    rejectConfig(path, sha256(contents));
  }
});

test("endpoint permits only exact numeric loopback HTTP bases with explicit ports", async t => {
  for (const endpoint of ["http://127.0.0.1:7310", "http://[::1]:7310"].map(base => base + BASE)) {
    assert.equal(typeof createClient(await privateConfig(t, { endpoint })), "function");
  }
  for (const endpoint of [
    `https://127.0.0.1:7310${BASE}`, `http://localhost:7310${BASE}`,
    `http://127.0.0.2:7310${BASE}`, `http://0.0.0.0:7310${BASE}`,
    `http://192.168.1.1:7310${BASE}`, `http://[::2]:7310${BASE}`,
    `http://127.1:7310${BASE}`, `http://2130706433:7310${BASE}`,
    `http://0x7f000001:7310${BASE}`, `http://127.0.0.1${BASE}`,
    `http://127.0.0.1:0${BASE}`, `http://127.0.0.1:65536${BASE}`,
    `http://user:pass@127.0.0.1:7310${BASE}`, `http://127.0.0.1:7310${BASE}?`,
    `http://127.0.0.1:7310${BASE}#`, `http://127.0.0.1:7310${BASE}/`,
    `http://127.0.0.1:7310/api/v1/else/../member-tools`,
    ` http://127.0.0.1:7310${BASE}`, `http://127.0.0.1:7310${BASE}\n`,
    `http://127.0.0.1:7310${BASE.replace("member", "%6dember")}`,
  ]) {
    const path = await privateConfig(t, { endpoint });
    rejectConfig(path, sha256(await readFile(path)));
  }
});
