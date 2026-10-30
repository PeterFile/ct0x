import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { isDefinitiveRejection, sendMessage } from "./api";
import type { Message } from "./api";
import { group, json, other } from "./test/groups";

const transport = vi.fn<typeof fetch>();
const content = "  <b>仅作为文本</b>\n@同名成员\r\n第二行\t  ";
const delivery = { id: "delivery-1", message_id: "message-1", conversation_id: "conversation-1",
  recipient_id: "member-1", state: "queued", turn_id: "", error: "", created_at: group.created_at };
const message: Message = { id: "message-1", group_id: group.id, topic_id: "", sender_id: "user",
  content, created_at: group.created_at, deliveries: [delivery] };
beforeEach(() => { transport.mockReset(); vi.stubGlobal("fetch", transport); });
afterEach(() => vi.unstubAllGlobals());

it("encodes the group, sends only the JSON DTO, and returns a direct 201 receipt", async () => {
  const groupId = "群/组?+";
  const input = { content, recipient_ids: ["member+/1"], topic_id: "话题 /?+", sender_id: "forged",
    actor: "forged", group_id: other.id, owner_id: "forged", creator_id: "forged" };
  const receipt = { ...message, group_id: groupId, topic_id: input.topic_id };
  transport.mockResolvedValueOnce(json(receipt, 201));
  const signal = new AbortController().signal;
  expect(await sendMessage(groupId, input, "opaque-key /+", signal)).toEqual(receipt);
  expect(transport).toHaveBeenCalledExactlyOnceWith("/api/v1/groups/%E7%BE%A4%2F%E7%BB%84%3F%2B/messages", {
    method: "POST", signal, headers: { "Content-Type": "application/json", "Idempotency-Key": "opaque-key /+" },
    body: JSON.stringify({ content, recipient_ids: input.recipient_ids, topic_id: input.topic_id }),
  });
});
it.each([
  { content },
  { content, recipient_ids: [] },
  { content, recipient_ids: ["stable-member-1", "stable-member-2"] },
])("preserves recipient routing payload $recipient_ids without parsing mentions", async input => {
  transport.mockResolvedValueOnce(json({ ...message, deliveries: [] }, 201));
  await sendMessage(group.id, input, "routing-key");
  expect(JSON.parse(String(transport.mock.calls[0][1]?.body))).toStrictEqual(input);
  expect(transport).toHaveBeenCalledTimes(1);
});
it.each([undefined, "", "opaque-topic /?+"])("passes topic %s through without inventing a default", async topic => {
  const input = topic === undefined ? { content } : { content, topic_id: topic };
  transport.mockResolvedValueOnce(json(message, 201));
  await sendMessage(group.id, input, "topic-key");
  expect(JSON.parse(String(transport.mock.calls[0][1]?.body))).toStrictEqual(input);
});
it("preserves raw content and the same key/body on explicit caller retry, never retrying automatically", async () => {
  const input = { content, recipient_ids: [], topic_id: "" };
  transport.mockRejectedValueOnce(new TypeError("offline"));
  const error: unknown = await sendMessage(group.id, input, "same-key").catch(error => error);
  expect(error).toMatchObject({ status: 0, code: "disconnected" });
  expect(isDefinitiveRejection(error)).toBe(false);
  expect(transport).toHaveBeenCalledTimes(1);
  transport.mockResolvedValueOnce(json({ ...message, deliveries: [] }, 201));
  await sendMessage(group.id, input, "same-key");
  expect(transport).toHaveBeenCalledTimes(2);
  const attempts = transport.mock.calls.map(([url, init]) => ({ url, body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key") }));
  expect(attempts[1]).toEqual(attempts[0]);
  expect(JSON.parse(String(attempts[0].body)).content).toBe(content);
});
it.each([200, 202])("does not confirm HTTP %s even with a valid receipt", async status => {
  transport.mockResolvedValueOnce(json(message, status));
  const error: unknown = await sendMessage(group.id, { content }, "key").catch(error => error);
  expect(error).toMatchObject({ status, code: "invalid_response" });
  expect(isDefinitiveRejection(error)).toBe(false);
});
it.each([
  ["foreign group", { ...message, group_id: other.id }],
  ["foreign message delivery", { ...message, deliveries: [{ ...delivery, message_id: "another-message" }] }],
  ["null deliveries", { ...message, deliveries: null }],
  ["missing deliveries", { ...message, deliveries: undefined }],
  ["non-array deliveries", { ...message, deliveries: {} }],
  ["null delivery", { ...message, deliveries: [null] }],
  ["missing delivery scalar", { ...message, deliveries: [{ ...delivery, recipient_id: undefined }] }],
  ["non-string delivery scalar", { ...message, deliveries: [{ ...delivery, turn_id: null }] }],
  ["missing message scalar", { ...message, sender_id: undefined }],
])("does not confirm a malformed or cross-scope 201: %s", async (_label, receipt) => {
  transport.mockResolvedValueOnce(json(receipt, 201));
  const error: unknown = await sendMessage(group.id, { content }, "key").catch(error => error);
  expect(error).toMatchObject({ status: 201, code: "invalid_response" });
  expect(isDefinitiveRejection(error)).toBe(false);
});
it("preserves known and future delivery states as data without promoting acknowledgements", async () => {
  const receipt = { ...message, deliveries: ["queued", "accepted", "failed", "future-state"].map((state, index) => ({ ...delivery, id: `delivery-${index}`, state })) };
  transport.mockResolvedValueOnce(json(receipt, 201));
  expect(await sendMessage(group.id, { content }, "key")).toEqual(receipt);
});
it.each([[503, "unavailable", false], [413, "body_too_large", true], [415, "unsupported_media_type", true]] as const)(
  "retains message rejection status/code HTTP %s and its uncertainty classification", async (status, code, definitive) => {
    transport.mockResolvedValueOnce(json({ error: { code, message: "请求未完成" } }, status));
    const error: unknown = await sendMessage(group.id, { content }, "key").catch(error => error);
    expect(error).toMatchObject({ status, code });
    expect(isDefinitiveRejection(error)).toBe(definitive);
    expect(transport).toHaveBeenCalledTimes(1);
  },
);
