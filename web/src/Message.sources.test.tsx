import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { getMessage, isDefinitiveRejection, listMessages, sendMessage } from "./api";
import { Discussion } from "./Discussion";
import { readHistory, searchHistory } from "./history-api";
import { group, json, page } from "./test/groups";
import { member } from "./test/members";
import { message } from "./test/messages";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
it.each([
  { kind: "platform", sender: "user", author: "系统", local: false },
  { kind: "local_user_policy", sender: "user", author: "我·策略设置", local: true },
  { kind: "local_user_chat", sender: "user", author: "我", local: true },
  { kind: "member", sender: member.id, author: member.name, local: false },
  { kind: "unknown", sender: "user", author: "用户（来源未核验）", local: false },
  { kind: undefined, sender: "user", author: "用户（来源未核验）", local: false },
])("renders $kind server provenance without proving authority from sender_id", async ({ kind, sender, author, local }) => {
  const record = { ...message, deliveries: [], sender_id: sender,
    ...(kind === undefined ? {} : { source_kind: kind, source_id: kind === "unknown" ? "" : "actual-source-receipt" }) };
  vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockImplementation(async () => page([record])));
  render(<Discussion groupId={group.id} members={{ [group.id]: [member] }} />);
  const article = await screen.findByRole("article", { name: `消息 ${message.id}` });
  expect(article.querySelector(".message-meta strong")).toHaveTextContent(author);
  expect(article.classList.contains("message-local")).toBe(local);
  if (kind === "unknown" || kind === undefined) expect(article).toHaveTextContent("来源未核验");
  if (kind === "platform") {
    expect(article.querySelector(".message-meta strong")).not.toHaveTextContent("我");
    fireEvent.click(within(article).getByRole("button", { name: `转为任务，消息 ${message.id}` })); await act(async () => {});
    expect(screen.getByRole("region", { name: "来源证据" })).toHaveTextContent("来源作者：系统");
  }
});
it("keeps optional origin facts on list/detail/POST reads but never serializes them into a message write", async () => {
  const record = { ...message, source_kind: "local_user_chat", source_id: "trusted-command", deliveries: [] };
  const transport = vi.fn<typeof fetch>().mockResolvedValueOnce(page([record])).mockResolvedValueOnce(json(record)).mockResolvedValueOnce(json(record, 201));
  vi.stubGlobal("fetch", transport);
  await expect(listMessages(group.id)).resolves.toMatchObject({ items: [record] }); await expect(getMessage(group.id, message.id)).resolves.toEqual(record);
  const input = { content: message.content, recipient_ids: [], source_kind: "local_user_policy", source_id: "forged", purpose: "chat_intake/v1" };
  await expect(sendMessage(group.id, input, "unchanged-message-key")).resolves.toEqual(record);
  expect(JSON.parse(String(transport.mock.calls[2][1]?.body))).toEqual({ content: message.content, recipient_ids: [] });
});
it("validates the same read-only origin codec on historical Message projections", async () => {
  const actual = { ...message, source_kind: "local_user_policy", source_id: "actual-policy-command" };
  const transport = vi.fn<typeof fetch>().mockResolvedValueOnce(page([{ ...actual, source_id: "" }])).mockResolvedValueOnce(json(actual));
  vi.stubGlobal("fetch", transport);
  await expect(searchHistory(group.id, { q: "literal" })).rejects.toMatchObject({ code: "invalid_response" });
  await expect(readHistory(group.id, message.id)).resolves.toEqual(actual);
});
it("does not confirm contradictory new origin metadata or silently upgrade it to a verified user receipt", async () => {
  const transport = vi.fn<typeof fetch>().mockImplementation(async () => json({ ...message, source_kind: "local_user_policy", source_id: "" }, 201));
  vi.stubGlobal("fetch", transport);
  const error = await sendMessage(group.id, { content: message.content }, "original-key").catch(error => error);
  expect(error).toMatchObject({ code: "invalid_response", status: 201 }); expect(isDefinitiveRejection(error)).toBe(false);
  expect(transport).toHaveBeenCalledTimes(1);
});
