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
