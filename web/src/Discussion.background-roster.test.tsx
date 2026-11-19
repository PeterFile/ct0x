import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { App } from "./App";
import { group, json, page } from "./test/groups";
import { member } from "./test/members";
import { message } from "./test/messages";

const flush = (ms = 0) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });

it("refreshes the actual roster from the existing chat feed after a platform join event", async () => {
  vi.useFakeTimers();
  let joined = false;
  const joinedMember = { ...member, name: "讨论助手", creator_id: "system", workspace: "" };
  const original = { ...message, source_kind: "local_user_chat", source_id: "source-1", deliveries: [] };
  const notice = { ...message, id: "join-1", sender_id: "platformnotice", source_kind: "platform",
    source_id: `group_background_join:${joinedMember.id}`, content: "讨论助手 已加入群组，参与当前话题的讨论。",
    created_at: "2026-10-30T12:00:01Z", deliveries: [] };
  const roster = vi.fn(async () => page(joined ? [joinedMember] : []));
  const send = vi.fn(async (_init?: RequestInit) => json(original, 201));
  vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
    const route = String(input);
    if (route.startsWith("/api/v1/groups?")) return page([group]);
    if (route.startsWith("/api/v1/providers?")) return page();
    if (route.includes("/members?")) return roster();
    if (route.endsWith("/messages") && init?.method === "POST") return send(init);
    if (route.includes("/messages?")) return page(joined ? [notice, original] : []);
    throw new Error(`Unexpected test route ${route}`);
  });
  render(<App />);
  await flush();
  fireEvent.click(screen.getByRole("button", { name: group.name }));
  fireEvent.click(screen.getByRole("button", { name: "成员" }));
  await flush();
  const members = screen.getByRole("list", { name: "成员列表" });
  expect(within(members).queryAllByRole("listitem")).toHaveLength(0);
  expect(roster).toHaveBeenCalledTimes(1);
  fireEvent.change(screen.getByRole("textbox", { name: "消息内容" }), { target: { value: message.content } });
  fireEvent.submit(screen.getByRole("form", { name: "发送消息" }));
  await flush();
  expect(JSON.parse(String(send.mock.calls[0][0]?.body))).toEqual({ content: message.content });
  joined = true;
  await flush(5000);
  const event = screen.getByRole("article", { name: `消息 ${notice.id}` });
  expect(within(event).getByText("系统消息")).toBeVisible();
  expect(within(event).queryByRole("button", { name: /转为任务/ })).not.toBeInTheDocument();
  expect(within(members).getByRole("button", { name: joinedMember.name })).toBeVisible();
  expect(within(members).getAllByRole("listitem")).toHaveLength(1);
  await flush(10000);
  expect(roster).toHaveBeenCalledTimes(2);
});
