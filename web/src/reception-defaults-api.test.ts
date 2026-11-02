import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, isDefinitiveRejection } from "./api";
import { getReceptionDefaults, setReceptionDefaults } from "./reception-defaults-api";
import type { ReceptionDefaultsInput } from "./reception-defaults-api";
import { json } from "./test/groups";
import { endpoint, saved, settings } from "./test/reception-defaults";

const transport = vi.fn<typeof fetch>();
const input: ReceptionDefaultsInput = { provider_id: saved.provider_id, model: saved.model, role: saved.role, expected_revision: 0 };
beforeEach(() => { transport.mockReset(); vi.stubGlobal("fetch", transport); });
afterEach(() => vi.unstubAllGlobals());

it("reads the live settings endpoint with a signal and preserves the not-configured state", async () => {
  const signal = new AbortController().signal;
  transport.mockResolvedValueOnce(settings());
  expect(await getReceptionDefaults(signal)).toEqual({ configuration: null,
    availability: { code: "not_configured", message: "服务提供的状态说明。" }, execution_enabled: false });
  expect(transport).toHaveBeenCalledExactlyOnceWith(endpoint, { signal, redirect: "error" });
});
it.each(["configured", "provider_unavailable"])("accepts a saved snapshot with %s availability, independently of execution", async code => {
  transport.mockResolvedValueOnce(json({ configuration: saved, availability: { code, message: "状态说明" }, execution_enabled: false }));
  expect((await getReceptionDefaults()).configuration).toEqual(saved);
});
it("sends only the four input fields with an exact, untrimmed body, key, JSON content type and signal", async () => {
  const signal = new AbortController().signal;
  const exact = { provider_id: saved.provider_id, model: " 自选模型 ", role: " 保留正文\n ", expected_revision: 3 };
  transport.mockResolvedValueOnce(json({ ...saved, ...exact, revision: 4 }));
  const untrusted = { ...exact, actor: "other", scope: "other", name: "other", workspace: "/private" };
  await setReceptionDefaults(untrusted, "same-save-key", signal);
  const [url, init] = transport.mock.calls[0];
  expect(url).toBe(endpoint); expect(init?.method).toBe("PUT"); expect(init?.signal).toBe(signal);
  expect(init?.redirect).toBe("error");
  expect(init?.body).toBe(JSON.stringify(exact));
  expect(Object.fromEntries(new Headers(init?.headers))).toEqual({ "content-type": "application/json", "idempotency-key": "same-save-key" });
});

const invalidSnapshots: [string, unknown][] = [
  ["array", []], ["null", null], ["missing id", { ...saved, id: undefined }], ["blank id", { ...saved, id: " " }],
  ["empty provider", { ...saved, provider_id: "" }], ["non-string provider", { ...saved, provider_id: 1 }],
  ["blank model", { ...saved, model: " \t" }], ["blank role", { ...saved, role: "\n " }],
  ["wrong role type", { ...saved, role: {} }], ["missing timestamp", { ...saved, created_at: undefined }],
  ["invalid timestamp", { ...saved, created_at: "not-a-date" }],
  ...[0, -1, 1.5, Number.MAX_SAFE_INTEGER + 1, "1", null].map(revision => [`revision ${revision}`, { ...saved, revision }] as [string, unknown]),
  ...["\0", "\r", "\n", "中".repeat(86), "\ud800"].map(model => ["invalid model", { ...saved, model }] as [string, unknown]),
  ...["😀".repeat(1025), "\udc00"].map(role => ["invalid role", { ...saved, role }] as [string, unknown]),
];
it.each(invalidSnapshots)("rejects a malformed snapshot: %s", async (_name, configuration) => {
  transport.mockResolvedValueOnce(json({ configuration, availability: { code: "configured", message: "说明" }, execution_enabled: true }));
  await expect(getReceptionDefaults()).rejects.toMatchObject({ status: 200, code: "invalid_response" });
  transport.mockResolvedValueOnce(json(configuration));
  await expect(setReceptionDefaults(input, "key")).rejects.toMatchObject({ status: 200, code: "invalid_response" });
});
it.each([
  { configuration: null, availability: { code: "configured", message: "说明" }, execution_enabled: true },
  { configuration: saved, availability: { code: "not_configured", message: "说明" }, execution_enabled: true },
  { configuration: null, availability: { code: "provider_unavailable", message: "说明" }, execution_enabled: true },
  { configuration: saved, availability: { code: "future", message: "说明" }, execution_enabled: true },
  { configuration: saved, availability: { code: "configured", message: 7 }, execution_enabled: true },
  { configuration: saved, availability: null, execution_enabled: true },
  { configuration: saved, availability: { code: "configured", message: "说明" } },
  { configuration: saved, availability: { code: "configured", message: "说明" }, execution_enabled: "false" },
  { availability: { code: "not_configured", message: "说明" }, execution_enabled: false },
])("rejects incoherent availability or missing/incorrect execution state %#", async value => {
  transport.mockResolvedValueOnce(json(value));
  await expect(getReceptionDefaults()).rejects.toMatchObject({ code: "invalid_response" });
});
it.each([201, 202, 206, 302])("never treats HTTP %s as a successful settings read or save", async status => {
  transport.mockResolvedValueOnce(json(saved, status));
  const error = await setReceptionDefaults(input, "key").catch(error => error);
  expect(error).toBeInstanceOf(ApiError); expect(error).toMatchObject({ status, code: "invalid_response" });
  expect(isDefinitiveRejection(error)).toBe(false);
  transport.mockResolvedValueOnce(json({ configuration: saved, availability: { code: "configured", message: "说明" }, execution_enabled: true }, status));
  await expect(getReceptionDefaults()).rejects.toMatchObject({ status, code: "invalid_response" });
});
it("keeps malformed JSON and a mismatched 200 receipt unknown", async () => {
  transport.mockResolvedValueOnce(new Response("not-json", { status: 200 }));
  await expect(setReceptionDefaults(input, "key")).rejects.toMatchObject({ code: "invalid_response" });
  for (const receipt of [{ ...saved, model: "another-model" }, { ...saved, role: "another-role" },
    { ...saved, provider_id: "another-provider" }, { ...saved, revision: 2 }]) {
    transport.mockResolvedValueOnce(json(receipt));
    const error = await setReceptionDefaults(input, "key").catch(error => error);
    expect(error).toMatchObject({ code: "invalid_response" }); expect(isDefinitiveRejection(error)).toBe(false);
  }
});
it("accepts exact UTF-8 byte boundaries and a positive safe revision without rewriting text", async () => {
  const exact = { ...input, model: "😀".repeat(64), role: "😀".repeat(1024), expected_revision: Number.MAX_SAFE_INTEGER - 1 };
  const receipt = { ...saved, ...exact, revision: Number.MAX_SAFE_INTEGER };
  transport.mockResolvedValueOnce(json(receipt));
  expect(await setReceptionDefaults(exact, "key")).toEqual(receipt);
  expect(transport.mock.calls[0][1]?.body).toBe(JSON.stringify(exact));
});
it.each([
  { ...input, provider_id: " " }, { ...input, model: " " }, { ...input, model: "中".repeat(86) },
  { ...input, model: "a\0b" }, { ...input, model: "a\rb" }, { ...input, model: "a\nb" }, { ...input, model: "\ud800" },
  { ...input, role: " \n" }, { ...input, role: "😀".repeat(1025) }, { ...input, role: "\udc00" },
  { ...input, expected_revision: -1 }, { ...input, expected_revision: 0.5 }, { ...input, expected_revision: Number.MAX_SAFE_INTEGER },
])("rejects invalid input before sending %#", async invalid => {
  await expect(setReceptionDefaults(invalid, "key")).rejects.toMatchObject({ status: 400, code: "invalid_input" });
  expect(transport).not.toHaveBeenCalled();
});
it.each([
  ["reception_defaults_required", "默认接待设置"], ["reception_provider_unavailable", "重新选择运行器"],
  ["reception_member_required", "添加或恢复成员"], ["reception_revision_conflict", "重新载入"],
  ["conflict", "同一保存标识"], ["future_conflict", "发生冲突"],
])("maps 409 %s to safe Chinese product text", async (code, text) => {
  transport.mockResolvedValueOnce(json({ error: { code, message: "SECRET arbitrary server text" } }, 409));
  const error = await setReceptionDefaults(input, "key").catch(error => error);
  expect(error).toMatchObject({ status: 409, code }); expect(error.message).toContain(text);
  expect(error.message).not.toContain("SECRET"); expect(isDefinitiveRejection(error)).toBe(true);
  if (code !== "conflict") expect(error.message).not.toContain("幂等");
});
it.each([400, 404, 413, 415])("preserves definitive HTTP %s without exposing arbitrary messages", async status => {
  transport.mockResolvedValueOnce(json({ error: { code: "rejected", message: "SECRET" } }, status));
  const error = await setReceptionDefaults(input, "key").catch(error => error);
  expect(error).toMatchObject({ status, code: "rejected" }); expect(error.message).not.toContain("SECRET");
  expect(isDefinitiveRejection(error)).toBe(true);
});
it("leaves transport failures and server timeouts unknown", async () => {
  transport.mockRejectedValueOnce(new TypeError("SECRET offline"));
  const disconnected = await setReceptionDefaults(input, "key").catch(error => error);
  expect(disconnected).toMatchObject({ status: 0, code: "disconnected" }); expect(isDefinitiveRejection(disconnected)).toBe(false);
  for (const status of [408, 500, 503, 504]) {
    transport.mockResolvedValueOnce(json({ error: { code: "deadline_exceeded", message: "SECRET" } }, status));
    const error = await setReceptionDefaults(input, "key").catch(error => error);
    expect(isDefinitiveRejection(error)).toBe(false); expect(error.message).not.toContain("SECRET");
  }
});
