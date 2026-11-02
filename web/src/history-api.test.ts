import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError } from "./api";
import { readHistory, searchHistory } from "./history-api";
import { group, json, other, page } from "./test/groups";
import { delivery, message } from "./test/messages";

const transport = vi.fn<typeof fetch>();
beforeEach(() => { transport.mockReset(); vi.stubGlobal("fetch", transport); });
afterEach(() => vi.unstubAllGlobals());

it("encodes the group, literal query and opaque cursor without changing input or adding credentials", async () => {
  const groupId = "群/组?+", q = " \u3000100%_\\ +?&中文 ", before = "opaque+/ =?&中文";
  const result = { ...message, group_id: groupId };
  const signal = new AbortController().signal;
  transport.mockResolvedValueOnce(page([result], "next-opaque"));
  expect(await searchHistory(groupId, { q, before }, signal)).toEqual({ items: [result], next_cursor: "next-opaque" });
  const [path, init] = transport.mock.calls[0];
  expect(String(path).split("?")[0]).toBe("/api/v1/groups/%E7%BE%A4%2F%E7%BB%84%3F%2B/history");
  expect(String(path)).toContain("100%25_%5C+%2B%3F%26");
  expect(String(path)).toContain("before=opaque%2B%2F+%3D%3F%26");
  expect([...new URL(String(path), "http://localhost").searchParams]).toEqual([["q", q], ["limit", "50"], ["before", before]]);
  expect(init).toEqual({ signal });
  transport.mockResolvedValueOnce(page());
  await searchHistory(group.id, { q: "中文", before: "" });
  expect(transport.mock.calls[1][0]).toBe(`/api/v1/groups/${group.id}/history?q=%E4%B8%AD%E6%96%87&limit=50`);
});

it.each([undefined, "", " 话题 /?+ ", " \t\u3000 "])("preserves exact topic %j in search and detail", async topic => {
  const result = { ...message, topic_id: topic ?? "another-topic" };
  transport.mockResolvedValueOnce(page([result]));
  await expect(searchHistory(group.id, { q: "词", topic_id: topic })).resolves.toEqual({ items: [result], next_cursor: "" });
  transport.mockResolvedValueOnce(json(result));
  await expect(readHistory(group.id, message.id, topic)).resolves.toEqual(result);
  for (const [path] of transport.mock.calls) {
    const params = new URL(String(path), "http://localhost").searchParams;
    expect(params.has("topic_id")).toBe(topic !== undefined);
    expect(params.get("topic_id")).toBe(topic ?? null);
  }
  const detailPath = String(transport.mock.calls[1][0]);
  expect(detailPath).toBe(`/api/v1/groups/${group.id}/history/${message.id}${topic === undefined ? "" : `?${new URLSearchParams({ topic_id: topic })}`}`);
});

it("encodes both detail IDs and only includes its exact topic filter and abort signal", async () => {
  const groupId = "群/组?+", id = "原文/一?+#", topic = " \t话题+ ";
  const result = { ...message, id, group_id: groupId, topic_id: topic, deliveries: [] };
  const signal = new AbortController().signal;
  transport.mockResolvedValueOnce(json(result));
  await expect(readHistory(groupId, id, topic, signal)).resolves.toEqual(result);
  expect(transport).toHaveBeenCalledExactlyOnceWith(
    "/api/v1/groups/%E7%BE%A4%2F%E7%BB%84%3F%2B/history/%E5%8E%9F%E6%96%87%2F%E4%B8%80%3F%2B%23?topic_id=+%09%E8%AF%9D%E9%A2%98%2B+", { signal },
  );
});

const invalidMessages: [string, unknown][] = [
  ["null message", null], ["array message", []], ["empty ID", { ...message, id: "" }],
  ["foreign group", { ...message, group_id: other.id }],
  ...["id", "group_id", "topic_id", "sender_id", "content", "created_at"].map(field =>
    [`missing ${field}`, { ...message, [field]: undefined }] as [string, unknown]),
  ["non-string content", { ...message, content: {} }],
  ["null deliveries", { ...message, deliveries: null }], ["missing deliveries", { ...message, deliveries: undefined }],
  ["non-array deliveries", { ...message, deliveries: {} }], ["null delivery", { ...message, deliveries: [null] }],
  ["empty delivery ID", { ...message, deliveries: [{ ...delivery, id: "" }] }],
  ["foreign delivery relationship", { ...message, deliveries: [{ ...delivery, message_id: "foreign-message" }] }],
  ...["id", "message_id", "conversation_id", "recipient_id", "state", "turn_id", "error", "created_at"].map(field =>
    [`invalid delivery ${field}`, { ...message, deliveries: [{ ...delivery, [field]: null }] }] as [string, unknown]),
];
it.each(invalidMessages)("rejects %s in both search items and single-record reads", async (_name, invalid) => {
  transport.mockResolvedValueOnce(page([invalid])).mockResolvedValueOnce(json(invalid));
  for (const run of [() => searchHistory(group.id, { q: "词" }), () => readHistory(group.id, message.id)]) {
    const error: unknown = await run().catch((failure: unknown) => failure);
    expect(error).toBeInstanceOf(ApiError);
    expect(error).toMatchObject({ status: 200, code: "invalid_response" });
    expect((error as ApiError).message).toContain("服务响应不符合约定");
  }
});

it.each([["", "topic"], ["topic", ""], [" \t ", ""], [" 话题 ", "话题"]])(
  "rejects response topic %j when the requested exact topic is %j", async (returned, requested) => {
    const invalid = { ...message, topic_id: returned };
    transport.mockResolvedValueOnce(page([invalid])).mockResolvedValueOnce(json(invalid));
    await expect(searchHistory(group.id, { q: "词", topic_id: requested })).rejects.toMatchObject({ code: "invalid_response" });
    await expect(readHistory(group.id, message.id, requested)).rejects.toMatchObject({ code: "invalid_response" });
  },
);
it("rejects another detail ID even inside the correct group and topic", async () => {
  transport.mockResolvedValueOnce(json({ ...message, id: "another-message", deliveries: [] }));
  await expect(readHistory(group.id, message.id, "")).rejects.toMatchObject({ code: "invalid_response" });
});
it("retains full message content and existing or future delivery states as data", async () => {
  const result = { ...message, content: "<b>原文</b>\n".repeat(2000), deliveries: ["queued", "accepted", "failed", "future-state"]
    .map((state, index) => ({ ...delivery, id: `delivery-${index}`, state })) };
  transport.mockResolvedValueOnce(page([result])).mockResolvedValueOnce(json(result));
  await expect(searchHistory(group.id, { q: "原文" })).resolves.toEqual({ items: [result], next_cursor: "" });
  await expect(readHistory(group.id, message.id)).resolves.toEqual(result);
});

it.each([null, [], {}, { items: null, next_cursor: "" }, { items: {}, next_cursor: "" }, { items: [] },
  { items: [], next_cursor: null }, { items: [], next_cursor: 7 }])("rejects malformed page %#", async invalid => {
  transport.mockResolvedValueOnce(json(invalid));
  await expect(searchHistory(group.id, { q: "词" })).rejects.toMatchObject({ code: "invalid_response" });
});
it.each([201, 202, 204, 206])("requires HTTP 200, not HTTP %s, for search and detail", async status => {
  transport.mockResolvedValueOnce(status === 204 ? new Response(null, { status }) : json({ items: [message], next_cursor: "" }, status))
    .mockResolvedValueOnce(status === 204 ? new Response(null, { status }) : json(message, status));
  await expect(searchHistory(group.id, { q: "词" })).rejects.toMatchObject({ status, code: "invalid_response" });
  await expect(readHistory(group.id, message.id)).rejects.toMatchObject({ status, code: "invalid_response" });
});
it.each(["search", "detail"])("rejects non-JSON HTTP 200 for %s", async operation => {
  transport.mockResolvedValueOnce(new Response("not JSON", { status: 200 }));
  await expect(operation === "search" ? searchHistory(group.id, { q: "词" }) : readHistory(group.id, message.id))
    .rejects.toMatchObject({ status: 200, code: "invalid_response" });
});
it.each([400, 404, 429, 503])("reports HTTP %s safely in Chinese through ApiError", async status => {
  transport.mockImplementation(async () => json({ error: { code: "server_code", message: "SECRET English backend diagnostic" } }, status));
  for (const run of [() => searchHistory(group.id, { q: "词" }), () => readHistory(group.id, message.id)]) {
    const error: unknown = await run().catch((failure: unknown) => failure);
    expect(error).toBeInstanceOf(ApiError); expect(error).toMatchObject({ status, code: "server_code" });
    expect((error as ApiError).message).toMatch(/输入|未找到|服务/);
    expect((error as ApiError).message).not.toMatch(/SECRET|English|backend/);
  }
});
it("handles non-JSON HTTP failure and network failure without exposing native diagnostics or auto-retrying", async () => {
  transport.mockResolvedValueOnce(new Response("SECRET diagnostic", { status: 503 }));
  await expect(searchHistory(group.id, { q: "词" })).rejects.toMatchObject({ status: 503, code: "http_error" });
  transport.mockRejectedValueOnce(new TypeError("SECRET network diagnostic"));
  await expect(readHistory(group.id, message.id)).rejects.toMatchObject({ status: 0, code: "disconnected", message: "无法连接本地服务，请确认服务已启动。" });
  expect(transport).toHaveBeenCalledTimes(2);
});
it("preserves an intentional abort", async () => {
  const controller = new AbortController(); controller.abort();
  const aborted = new DOMException("aborted", "AbortError"); transport.mockRejectedValueOnce(aborted);
  await expect(searchHistory(group.id, { q: "词" }, controller.signal)).rejects.toBe(aborted);
});
