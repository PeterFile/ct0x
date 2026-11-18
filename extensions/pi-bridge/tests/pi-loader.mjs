import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const root = process.env.PI_BRIDGE_PI_PACKAGE || resolve(dirname(fileURLToPath(import.meta.resolve("@earendil-works/pi-coding-agent"))), "..");
const manifest = JSON.parse(await readFile(resolve(root, "package.json"), "utf8"));
assert.equal(manifest.version, "1.4.0", "Loader smoke requires real Pi 1.4.0");
// Loader-only tests do not create sessions. The separate active-profile fixture
// explicitly loads the SDK with in-memory state and a deny-all model runtime.
export const { loadExtensions } = await import(pathToFileURL(resolve(root, "dist/core/extensions/loader.js")).href);

export async function loadSessionSDK() {
  const sdk = await import(pathToFileURL(resolve(root, "dist/core/sdk.js")).href);
  const { SessionManager } = await import(pathToFileURL(resolve(root, "dist/core/session-manager.js")).href);
  const { SettingsManager } = await import(pathToFileURL(resolve(root, "dist/core/settings-manager.js")).href);
  return { createAgentSession: sdk.createAgentSession, SessionManager, SettingsManager };
}

// Call Pi's real print-mode path (including the JSON event serializer), but
// use the supplied in-memory SDK session, not CLI discovery or user settings.
export async function runJSONPrint(t, session, initialMessage) {
  const { runPrintMode } = await import(pathToFileURL(resolve(root, "dist/modes/print-mode.js")).href);
  const chunks = [], errors = [];
  const out = t.mock.method(process.stdout, "write", (chunk, callback) => {
    chunks.push(chunk);
    callback?.();
    return true;
  });
  const err = t.mock.method(console, "error", message => errors.push(message));
  try {
    const exit = await runPrintMode({ session, setRebindSession() {}, async dispose() { session.dispose(); } },
      { mode: "json", initialMessage });
    const jsonl = chunks.join("");
    return { exit, errors, jsonl, frames: jsonl.trimEnd().split("\n").map(line => JSON.parse(line)) };
  } finally {
    out.mock.restore();
    err.mock.restore();
  }
}
