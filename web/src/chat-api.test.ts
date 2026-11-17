import { afterEach, expect, it, vi } from "vitest";
import { getChatAdmission, getChatPolicy, isChatPolicyDefinitiveRejection, setChatPolicy } from "./chat-api";
import { group, json } from "./test/groups";
import { task } from "./test/tasks";

const input = { enabled: true, allow_preparation: false, action: "text_submission" as const, max_admissions: 2, expected_revision: 0 };
const policy = { group_id: group.id, topic_id: "", enabled: true, allow_preparation: false, preauthorization_message_id: "",
  action: "text_submission", max_admissions: 2, revision: 1, authorized_by: "user", preparation_supported: false,
  configuration_source_id: "policy-command-1" };
afterEach(() => vi.unstubAllGlobals());

it("uses an exact optional topic, the standard idempotency header and only the owned PUT DTO", async () => {
  const topic = " topic /+? ", transport = vi.fn<typeof fetch>().mockImplementation(async () => json({ ...policy, topic_id: topic })); vi.stubGlobal("fetch", transport);
  await getChatPolicy(group.id, undefined, topic);
  await setChatPolicy(group.id, { ...input, authorized_by: "agent", grants: ["write"] } as typeof input, "exact-policy-key", topic);
  const path = `/api/v1/groups/${group.id}/coordination/chat-policy?${new URLSearchParams({ topic_id: topic })}`;
  expect(transport.mock.calls[0][0]).toBe(path); expect(transport.mock.calls[1][0]).toBe(path);
  const init = transport.mock.calls[1][1]; expect(init?.method).toBe("PUT"); expect(init?.redirect).toBe("error");
  expect(new Headers(init?.headers).get("Idempotency-Key")).toBe("exact-policy-key"); expect(JSON.parse(String(init?.body))).toEqual(input);
});
it("accepts a server-allocated canonical source instead of comparing it with an empty client ID", async () => {
  const request = { ...input, allow_preparation: true, preparation_scope_kind: "topic_summary" as const };
  const actual = { ...policy, allow_preparation: true, preparation_scope_kind: "topic_summary", preparation_supported: true,
    preauthorization_message_id: "server-rule-id", preauthorization_text: "只允许本话题已有消息的纯文本摘要" };
  const transport = vi.fn<typeof fetch>().mockResolvedValueOnce(json(actual)); vi.stubGlobal("fetch", transport);
  await expect(setChatPolicy(group.id, request, "canonical-source-key")).resolves.toEqual(actual);
  expect(JSON.parse(String(transport.mock.calls[0][1]?.body))).toEqual(request);
  expect(String(transport.mock.calls[0][1]?.body)).not.toContain("preauthorization_message_id");
});
it("replays a previously submitted v16 input byte-for-byte with its original key after new-scope reads", async () => {
  const old = Object.freeze({ enabled: true, allow_preparation: true, preauthorization_message_id: "legacy-message-id",
    action: "text_submission" as const, max_admissions: 2, expected_revision: 16 });
  const historical = { ...policy, allow_preparation: true, revision: 17, preauthorization_message_id: old.preauthorization_message_id,
    preauthorization_text: "历史的更窄自由文字规则", preparation_supported: false, configuration_source_id: "" };
  const transport = vi.fn<typeof fetch>().mockResolvedValueOnce(json({}, 503))
    .mockResolvedValueOnce(json({ ...policy, revision: 20 })) .mockResolvedValueOnce(json(historical)); vi.stubGlobal("fetch", transport);
  await expect(setChatPolicy(group.id, old, "original-v16-key")).rejects.toMatchObject({ status: 503 });
  await getChatPolicy(group.id); await expect(setChatPolicy(group.id, old, "original-v16-key")).resolves.toEqual(historical);
  const capture = (index: number) => { const [url, init] = transport.mock.calls[index]; return { url, body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key") }; };
  expect(capture(2)).toEqual(capture(0)); expect(capture(0).body).toBe(JSON.stringify(old)); expect(capture(2).body).not.toContain("preparation_scope_kind");
});
it("does not accept claimed preparation support without the actual rule and configuration source", async () => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValueOnce(json({ ...policy, allow_preparation: true,
    preparation_scope_kind: "topic_summary", preparation_supported: true, configuration_source_id: "" })));
  await expect(getChatPolicy(group.id)).rejects.toMatchObject({ code: "invalid_response" });
});
it.each(["revision", "max_admissions"])("rejects a raw rounded %s before treating policy reads or PUT receipts as facts", async field => {
  const raw = JSON.stringify(policy).replace(`\"${field}\":${policy[field as "revision" | "max_admissions"]}`, `\"${field}\":${policy[field as "revision" | "max_admissions"]}.0000000000000001`);
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockImplementation(async () => new Response(raw, { status: 200 })));
  await expect(getChatPolicy(group.id)).rejects.toMatchObject({ code: "invalid_response" });
  const error = await setChatPolicy(group.id, input, "exact-policy-key").catch(error => error);
  expect(error).toMatchObject({ code: "invalid_response" }); expect(isChatPolicyDefinitiveRejection(error)).toBe(false);
});
it("does not treat an unrecognized negative envelope as proof that a policy PUT failed", async () => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockImplementation(async () => json({ error: { code: "conflict", message: "版本冲突" }, extra: "unrecognized" }, 409)));
  const error = await setChatPolicy(group.id, input, "exact-policy-key").catch(error => error);
  expect(isChatPolicyDefinitiveRejection(error)).toBe(false); expect(error).toMatchObject({ status: 409, code: "invalid_response" });
});
it("quietly supports legacy plain 404 admission reads but never treats a malformed PUT rejection as proof of failure", async () => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockImplementation(async () => new Response("404 page not found", { status: 404 })));
  await expect(getChatAdmission(group.id, task.id)).resolves.toBeNull();
  await expect(getChatPolicy(group.id)).rejects.toMatchObject({ status: 404 });
  const error = await setChatPolicy(group.id, input, "exact-policy-key").catch(error => error);
  expect(error).toMatchObject({ status: 404, code: "invalid_response" }); expect(isChatPolicyDefinitiveRejection(error)).toBe(false);
});
