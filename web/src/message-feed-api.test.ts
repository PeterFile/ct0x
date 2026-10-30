import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { getMessage, listMessages } from "./api";
import { group, json, other, page } from "./test/groups";
import { delivery, message } from "./test/messages";
const transport = vi.fn<typeof fetch>();
beforeEach(() => { transport.mockReset(); vi.stubGlobal("fetch", transport); });
afterEach(() => vi.unstubAllGlobals());
it("bounds and encodes the whole-group feed without filtering out topics or eagerly paging", async () => {
  const groupId = "群/组?+", signal = new AbortController().signal;
  const items = [{ ...message, group_id: groupId, topic_id: "topic-A" }];
  transport.mockResolvedValueOnce(page(items, "next"));
  expect(await listMessages(groupId, "before+/=&", signal)).toEqual({ items, next_cursor: "next" });
  expect(transport).toHaveBeenCalledExactlyOnceWith("/api/v1/groups/%E7%BE%A4%2F%E7%BB%84%3F%2B/messages?limit=50&before=before%2B%2F%3D%26", { signal });
});
it("encodes the detail identity and returns the latest durable deliveries", async () => {
  const id = "消息/?+", signal = new AbortController().signal;
  const receipt = { ...message, id, deliveries: [{ ...delivery, message_id: id, state: "accepted" }] };
  transport.mockResolvedValueOnce(json(receipt));
  expect(await getMessage(group.id, id, signal)).toEqual(receipt);
  expect(transport).toHaveBeenCalledExactlyOnceWith(`/api/v1/groups/${group.id}/messages/%E6%B6%88%E6%81%AF%2F%3F%2B`, { signal });
});
it.each(["list", "detail"])("rejects foreign-group %s records", async mode => {
  const foreign = { ...message, group_id: other.id };
  transport.mockResolvedValueOnce(mode === "list" ? page([foreign]) : json(foreign));
  await expect(mode === "list" ? listMessages(group.id) : getMessage(group.id, message.id)).rejects.toMatchObject({ code: "invalid_response" });
});
it("rejects a valid-shaped detail for a different message ID", async () => {
  transport.mockResolvedValueOnce(json({ ...message, id: "other-message", deliveries: [] }));
  await expect(getMessage(group.id, message.id)).rejects.toMatchObject({ status: 200, code: "invalid_response" });
});
