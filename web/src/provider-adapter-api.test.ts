import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { listProviders } from "./api";
import { page } from "./test/groups";
import { provider } from "./test/members";

const transport = vi.fn<typeof fetch>();
const capabilityKeys = ["platform_tools", "native_resume", "permission_interaction", "cancel", "terminal_evidence", "steer"] as const;
const currentProvider = { ...provider, version: "1.4.0" };
function adapter() {
  return {
    id: "pi_fixture", protocol: "pi_extension", enabled: false,
    capabilities: {
      platform_tools: "supported", native_resume: "unknown", permission_interaction: "unsupported",
      cancel: "unknown", terminal_evidence: "supported", steer: "unsupported",
      evidence: { source: "builtin_contract", provider_id: currentProvider.id, name: currentProvider.name,
        path: currentProvider.path, version: currentProvider.version, protocol: "pi_extension" },
    },
  };
}
beforeEach(() => { transport.mockReset(); vi.stubGlobal("fetch", transport); });
afterEach(() => vi.unstubAllGlobals());

it("accepts legacy providers without creating adapter information", async () => {
  transport.mockResolvedValueOnce(page([provider]));
  const result = await listProviders();
  expect(result.items).toEqual([provider]);
  expect(result.items[0]).not.toHaveProperty("adapter");
});
it.each(["one_shot", "pi_extension", "codex_app_server"])("accepts an unknown %s report without upgrading legacy tools/resume", async protocol => {
  const value = { ...currentProvider, capabilities: { one_shot: true, tools: true, resume: true },
    adapter: { id: "fixture", protocol, enabled: false } };
  transport.mockResolvedValueOnce(page([value]));
  expect((await listProviders()).items[0]).toEqual(value);
});
it("preserves explicit unknown states independently of account and enablement", async () => {
  const value = { ...currentProvider, account: "authenticated", adapter: adapter() };
  transport.mockResolvedValueOnce(page([value]));
  expect((await listProviders()).items[0]).toEqual(value);
});
it.each([
  null, [], "pi_extension", {},
  { id: "", protocol: "pi_extension", enabled: false },
  { id: " fixture ", protocol: "pi_extension", enabled: false },
  { id: "fixture", protocol: "future", enabled: false },
  { id: "fixture", protocol: "pi_extension", enabled: "false" },
  { id: "fixture", protocol: "pi_extension" },
  { ...adapter(), capabilities: null },
  { ...adapter(), capabilities: [] },
  { ...adapter(), capabilities: {} },
])("rejects malformed adapter metadata %#", async value => {
  transport.mockResolvedValueOnce(page([{ ...currentProvider, adapter: value }]));
  await expect(listProviders()).rejects.toMatchObject({ code: "invalid_response" });
});
it.each(capabilityKeys)("validates the %s tri-state instead of accepting booleans or invented states", async key => {
  for (const invalid of [true, false, "ready", "", null, 1, undefined]) {
    const value = adapter();
    transport.mockResolvedValueOnce(page([{ ...currentProvider, adapter: {
      ...value, capabilities: { ...value.capabilities, [key]: invalid },
    } }]));
    await expect(listProviders()).rejects.toMatchObject({ code: "invalid_response" });
  }
});
it.each([
  ["source", "account"], ["source", "handshake"], ["provider_id", "another-provider"],
  ["name", "codex"], ["path", "/other/pi"], ["version", "1.4.1"], ["protocol", "one_shot"],
  ["source", null], ["path", 1],
])("rejects unbound capability evidence %s=%s", async (key, invalid) => {
  const value = adapter();
  transport.mockResolvedValueOnce(page([{ ...currentProvider, adapter: { ...value,
    capabilities: { ...value.capabilities, evidence: { ...value.capabilities.evidence, [key]: invalid } },
  } }]));
  await expect(listProviders()).rejects.toMatchObject({ code: "invalid_response" });
});
it.each([undefined, null, [], {}])("rejects missing or malformed capability evidence %#", async evidence => {
  const value = adapter();
  transport.mockResolvedValueOnce(page([{ ...currentProvider, adapter: { ...value,
    capabilities: { ...value.capabilities, evidence },
  } }]));
  await expect(listProviders()).rejects.toMatchObject({ code: "invalid_response" });
});
