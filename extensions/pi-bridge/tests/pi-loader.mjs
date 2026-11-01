import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const root = process.env.PI_BRIDGE_PI_PACKAGE || resolve(dirname(fileURLToPath(import.meta.resolve("@earendil-works/pi-coding-agent"))), "..");
const manifest = JSON.parse(await readFile(resolve(root, "package.json"), "utf8"));
assert.equal(manifest.version, "1.4.0", "Loader smoke requires real Pi 1.4.0");
// Import only the loader: no AgentSession, authentication, model discovery or prompt.
export const { loadExtensions } = await import(pathToFileURL(resolve(root, "dist/core/extensions/loader.js")).href);
