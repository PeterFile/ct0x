// @vitest-environment node
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, createGroup, createMember, isDefinitiveRejection, sendMessage } from "./api";
import { deferred, group, json } from "./test/groups";
import { member } from "./test/members";
import { message } from "./test/messages";

const transport = vi.fn<typeof fetch>();
const groupInput = { name: "  原始群组\t ", description: " 原始说明\r\n第二行  " };
const memberInput = { name: "  原始成员\t ", role: " 原始职责\r\n第二行  ", provider_id: " exact-provider ",
  model: " exact-model ", workspace: " /exact/workspace " };
const content = "  原始消息\r\n<b>纯文本</b>\t  ";
const groupReceipt = { ...group, ...groupInput };
const memberReceipt = { ...member, ...memberInput };
const messageReceipt = { ...message, content, deliveries: [] };
beforeEach(() => { transport.mockReset(); vi.stubGlobal("fetch", transport); });
afterEach(() => vi.unstubAllGlobals());
async function invalid(result: Promise<unknown>) {
  const error: unknown = await result.then(() => null, failure => failure);
  expect(error).toBeInstanceOf(ApiError);
  expect(error).toMatchObject({ status: 201, code: "invalid_response" });
  expect(isDefinitiveRejection(error)).toBe(false);
}

it.each(["name", "description"] as const)("rejects a valid-shaped group receipt with normalized %s", async field => {
  transport.mockResolvedValueOnce(json({ ...groupReceipt, [field]: groupInput[field].trim() }, 201));
  await invalid(createGroup(groupInput, "group-key"));
  expect(transport.mock.calls[0][1]?.body).toBe(JSON.stringify(groupInput));
});
it.each(["name", "role", "provider_id", "model", "workspace"] as const)("rejects a valid-shaped member receipt with normalized %s", async field => {
  transport.mockResolvedValueOnce(json({ ...memberReceipt, [field]: memberInput[field].trim() }, 201));
  await invalid(createMember(group.id, memberInput, "member-key"));
  expect(transport.mock.calls[0][1]?.body).toBe(JSON.stringify(memberInput));
});
it("rejects a valid-shaped message receipt whose content does not match the raw write", async () => {
  transport.mockResolvedValueOnce(json({ ...messageReceipt, content: content.trim() }, 201));
  await invalid(sendMessage(group.id, { content }, "message-key"));
});
it("does not treat a nonempty group description as the default for an omitted field", async () => {
  transport.mockResolvedValueOnce(json({ ...group, name: groupInput.name }, 201));
  await invalid(createGroup({ name: groupInput.name }, "group-key"));
});
it.each(["provider_id", "model", "workspace"] as const)("does not treat nonempty member %s as an omitted-field default", async field => {
  transport.mockResolvedValueOnce(json({ ...member, [field]: "unrelated-value" }, 201));
  await invalid(createMember(group.id, { name: member.name, role: member.role }, "member-key"));
});

it("accepts exact raw group receipts and explicit same-key replay without changing the payload", async () => {
  transport.mockImplementation(async () => json(groupReceipt, 201));
  await expect(createGroup(groupInput, "same-group-key")).resolves.toEqual(groupReceipt);
  await expect(createGroup(groupInput, "same-group-key")).resolves.toEqual(groupReceipt);
  const attempts = transport.mock.calls.map(([url, init]) => ({ url, body: init?.body,
    key: new Headers(init?.headers).get("Idempotency-Key"), redirect: init?.redirect }));
  expect(attempts).toEqual(Array(2).fill({ url: "/api/v1/groups", body: JSON.stringify(groupInput),
    key: "same-group-key", redirect: "error" }));
});
it("accepts exact raw member preferences and explicit same-key replay", async () => {
  transport.mockImplementation(async () => json(memberReceipt, 201));
  await expect(createMember(group.id, memberInput, "same-member-key")).resolves.toEqual(memberReceipt);
  await expect(createMember(group.id, memberInput, "same-member-key")).resolves.toEqual(memberReceipt);
  const attempts = transport.mock.calls.map(([url, init]) => ({ url, body: init?.body,
    key: new Headers(init?.headers).get("Idempotency-Key") }));
  expect(attempts).toEqual(Array(2).fill({ url: `/api/v1/groups/${group.id}/members`, body: JSON.stringify(memberInput), key: "same-member-key" }));
});
it.each([undefined, "", " 原始话题 /?+ "])("accepts raw message replay with topic %j and unchanged routing payload", async topic_id => {
  const input = { content, recipient_ids: [member.id], topic_id };
  const receipt = { ...messageReceipt, topic_id: topic_id ?? "" };
  transport.mockImplementation(async () => json(receipt, 201));
  await expect(sendMessage(group.id, input, "same-message-key")).resolves.toEqual(receipt);
  await expect(sendMessage(group.id, input, "same-message-key")).resolves.toEqual(receipt);
  expect(transport.mock.calls.map(([, init]) => init?.body)).toEqual(Array(2).fill(JSON.stringify(input)));
});
it.each([{ name: groupInput.name }, { name: groupInput.name, description: undefined }, { name: groupInput.name, description: "" }])(
  "accepts the Go empty-string description default without adding optional wire fields %#", async input => {
    transport.mockResolvedValueOnce(json({ ...group, name: input.name, description: "" }, 201));
    await expect(createGroup(input, "group-default-key")).resolves.toMatchObject({ name: input.name, description: "" });
    expect(transport.mock.calls[0][1]?.body).toBe(JSON.stringify(input));
  },
);
it.each([
  { name: memberInput.name, role: memberInput.role },
  { name: memberInput.name, role: memberInput.role, provider_id: undefined, model: undefined, workspace: undefined },
  { name: memberInput.name, role: memberInput.role, provider_id: "", model: "", workspace: "" },
])("accepts Go empty-string preference defaults without adding optional wire fields %#", async input => {
  const receipt = { ...member, name: input.name, role: input.role };
  transport.mockResolvedValueOnce(json(receipt, 201));
  await expect(createMember(group.id, input, "member-default-key")).resolves.toEqual(receipt);
  expect(transport.mock.calls[0][1]?.body).toBe(JSON.stringify(input));
});

it.each(["sent", "mutated"] as const)("correlates group %s content against a snapshot, not a later caller mutation", async which => {
  const input = { ...groupInput }, pending = deferred<Response>();
  transport.mockReturnValueOnce(pending.promise);
  const result = createGroup(input, "group-snapshot-key");
  input.name = "后来修改的群名"; input.description = "后来修改的说明";
  const receipt = { ...group, ...(which === "sent" ? groupInput : input) };
  pending.resolve(json(receipt, 201));
  if (which === "sent") await expect(result).resolves.toEqual(receipt);
  else await invalid(result);
  expect(transport.mock.calls[0][1]?.body).toBe(JSON.stringify(groupInput));
});
it.each(["sent", "mutated"] as const)("correlates member %s content against a snapshot, not later caller mutations", async which => {
  const input = { ...memberInput }, pending = deferred<Response>();
  transport.mockReturnValueOnce(pending.promise);
  const result = createMember(group.id, input, "member-snapshot-key");
  for (const key of ["name", "role", "provider_id", "model", "workspace"] as const) input[key] = `later-${key}`;
  const receipt = { ...member, ...(which === "sent" ? memberInput : input) };
  pending.resolve(json(receipt, 201));
  if (which === "sent") await expect(result).resolves.toEqual(receipt);
  else await invalid(result);
  expect(transport.mock.calls[0][1]?.body).toBe(JSON.stringify(memberInput));
});
it.each(["sent", "mutated"] as const)("correlates message %s content to the sent DTO despite caller and recipient-array mutations", async which => {
  const input = { content, recipient_ids: [member.id], topic_id: "original-topic" }, pending = deferred<Response>();
  const body = JSON.stringify(input);
  transport.mockReturnValueOnce(pending.promise);
  const result = sendMessage(group.id, input, "message-snapshot-key");
  input.content = "后来修改的消息"; input.topic_id = "later-topic"; input.recipient_ids.push("later-recipient");
  const receipt = { ...messageReceipt, topic_id: "original-topic", content: which === "sent" ? content : input.content };
  pending.resolve(json(receipt, 201));
  if (which === "sent") await expect(result).resolves.toEqual(receipt);
  else await invalid(result);
  expect(transport.mock.calls[0][1]?.body).toBe(body);
  expect(input.recipient_ids).toEqual([member.id, "later-recipient"]);
});
