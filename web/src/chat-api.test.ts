import { afterEach, expect, it, vi } from "vitest";
import { getChatAdmission, getChatPolicy, isChatPolicyDefinitiveRejection, setChatPolicy } from "./chat-api";
import { group, json } from "./test/groups";
import { task } from "./test/tasks";

const input = { enabled: true, allow_preparation: false, preauthorization_message_id: "", action: "text_submission" as const,
  max_admissions: 2, expected_revision: 0 };
const policy = { group_id: group.id, topic_id: "", enabled: true, allow_preparation: false, preauthorization_message_id: "",
  action: "text_submission", max_admissions: 2, revision: 1, authorized_by: "user" };
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
