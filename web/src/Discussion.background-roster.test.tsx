import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { App } from "./App";
import { Members } from "./Members";
import { refreshBackgroundRoster } from "./background-roster-refresh";
import { deferred, group, json, other, page } from "./test/groups";
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

it("refreshes the visible roster when a burst pushes a join out of the latest page", async () => {
  vi.useFakeTimers();
  const joined = { ...member, name: "跨页加入的成员", creator_id: "system" };
  const older = { ...message, id: "loaded-history", created_at: "2026-10-30T11:59:00Z", deliveries: [] };
  const notice = { ...message, id: "missed-join", sender_id: "platformnotice", source_kind: "platform" as const,
    source_id: `group_background_join:${joined.id}`, created_at: "2026-10-30T12:01:00Z", deliveries: [] };
  const batch = (minute: string) => Array.from({ length: 50 }, (_, i) => ({ ...message,
    id: `batch-${minute}-${49 - i}`, created_at: `2026-10-30T12:${minute}:${String(49 - i).padStart(2, "0")}Z`, deliveries: [] }));
  let history = [...batch("00"), older], latestCursor = "history-start";
  const roster = vi.fn(async () => page(history.includes(notice) ? [joined] : []));
  const messages = vi.fn(async (route: string) => route.endsWith("&before=history-start") ? page([older], "history-continuation")
    : route.endsWith("&before=history-continuation") ? page() : page(history.slice(0, 50), latestCursor));
  vi.stubGlobal("fetch", async (input: RequestInfo | URL) => {
    const route = String(input);
    if (route.startsWith("/api/v1/groups?")) return page([group]);
    if (route.startsWith("/api/v1/providers?")) return page();
    if (route.includes("/members?")) return roster();
    if (route.includes("/messages?")) return messages(route);
    throw new Error(`Unexpected test route ${route}`);
  });
  render(<App />);
  await flush();
  fireEvent.click(screen.getByRole("button", { name: group.name }));
  fireEvent.click(screen.getByRole("button", { name: "成员" }));
  await flush();
  fireEvent.click(screen.getByRole("button", { name: "加载更早消息" }));
  fireEvent.change(screen.getByRole("textbox", { name: "消息内容" }), { target: { value: "未发送草稿" } });
  await flush(5000);
  expect(roster).toHaveBeenCalledTimes(1);
  const olderArticle = screen.getByRole("article", { name: `消息 ${older.id}` });

  history = [...batch("02"), notice, ...history];
  latestCursor = "after-burst";
  await flush(5000);
  expect(screen.queryByRole("article", { name: `消息 ${notice.id}` })).not.toBeInTheDocument();
  expect(olderArticle).toBeVisible();
  expect(screen.getByRole("textbox", { name: "消息内容" })).toHaveValue("未发送草稿");
  const members = within(screen.getByRole("list", { name: "成员列表" }));
  expect(members.getByRole("button", { name: joined.name })).toBeVisible();
  await flush(5000);
  expect(roster).toHaveBeenCalledTimes(2);
  fireEvent.click(screen.getByRole("button", { name: "加载更早消息" }));
  await flush();
  expect(messages.mock.calls.at(-1)?.[0]).toBe(`/api/v1/groups/${group.id}/messages?limit=50&before=history-continuation`);
});

it("observes a join in the mobile roster without reopening the chat", async () => {
  vi.useFakeTimers();
  vi.stubGlobal("matchMedia", (media: string) => ({ media, matches: true, addEventListener: vi.fn(), removeEventListener: vi.fn() }));
  let joined = false;
  const joinedMember = { ...member, name: "新成员", creator_id: "system" };
  const notice = { ...message, id: "join-mobile", sender_id: "platformnotice", source_kind: "platform",
    source_id: `group_background_join:${joinedMember.id}`, deliveries: [] };
  const roster = vi.fn(async () => page(joined ? [joinedMember] : []));
  const messages = vi.fn(async () => page(joined ? [notice] : []));
  vi.stubGlobal("fetch", async (input: RequestInfo | URL) => {
    const route = String(input);
    if (route.startsWith("/api/v1/groups?")) return page([group]);
    if (route.startsWith("/api/v1/providers?")) return page();
    if (route.includes("/members?")) return roster();
    if (route.includes("/messages?")) return messages();
    throw new Error(`Unexpected test route ${route}`);
  });
  render(<App />);
  await flush();
  fireEvent.click(screen.getByRole("button", { name: group.name }));
  await flush();
  fireEvent.change(screen.getByRole("textbox", { name: "消息内容" }), { target: { value: "未发送草稿" } });
  fireEvent.click(screen.getByRole("button", { name: "成员" }));
  await flush();
  const members = screen.getByRole("list", { name: "成员列表" });
  expect(roster).toHaveBeenCalledTimes(1);
  expect(screen.queryByRole("main", { name: "讨论" })).toBeNull();
  joined = true;
  await flush(5000);
  expect(within(members).getByRole("button", { name: joinedMember.name })).toBeVisible();
  await flush(5000);
  expect(roster).toHaveBeenCalledTimes(2);
  fireEvent.click(screen.getByRole("button", { name: "返回聊天" }));
  expect(screen.getByRole("textbox", { name: "消息内容" })).toHaveValue("未发送草稿");
  fireEvent.click(screen.getByRole("button", { name: "返回群组" }));
  const reads = messages.mock.calls.length;
  await flush(10000);
  expect(messages).toHaveBeenCalledTimes(reads);
});

it("defers and retries a join refresh without disturbing older roster pages", async () => {
  vi.useFakeTimers();
  const older = { ...member, id: "old-member", name: "早期成员" };
  const joined = { ...member, id: "new-member", name: "刚加入的成员" };
  const pending = deferred<Response>();
  const roster = vi.fn<typeof fetch>()
    .mockResolvedValueOnce(page([member], "older-page"))
    .mockReturnValueOnce(pending.promise)
    .mockResolvedValueOnce(json({}, 503))
    .mockResolvedValueOnce(page([joined, member], "from-refreshed-page"))
    .mockImplementation(async () => page());
  vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
    const route = String(input);
    if (route.includes("/members?")) return roster(route, init);
    throw new Error(`Unexpected test route ${route}`);
  });
  const onMembers = vi.fn();
  render(<Members groupId={group.id} onMembers={onMembers} catalogueActive={false} />);
  await flush();
  fireEvent.click(screen.getByRole("button", { name: "加载更多成员" }));
  act(() => refreshBackgroundRoster(other.id));
  expect(roster).toHaveBeenCalledTimes(2);
  act(() => { refreshBackgroundRoster(group.id); refreshBackgroundRoster(group.id); });
  expect(roster).toHaveBeenCalledTimes(2);
  expect(roster.mock.calls[1][1]?.signal?.aborted).toBe(false);
  await act(async () => pending.resolve(page([older], "even-older-page")));
  await flush();
  expect(roster).toHaveBeenCalledTimes(3);
  fireEvent.click(screen.getByRole("button", { name: older.name }));
  fireEvent.click(screen.getByRole("button", { name: "重试载入成员" }));
  await flush();
  expect(screen.getByRole("button", { name: joined.name })).toBeVisible();
  expect(screen.getByRole("button", { name: older.name, pressed: true })).toBeVisible();
  expect(within(screen.getByRole("region", { name: "成员详情" })).getByRole("heading", { name: older.name })).toBeVisible();
  expect(onMembers).toHaveBeenLastCalledWith(group.id, [joined, member, older]);
  expect(roster.mock.calls[3][0]).toBe(`/api/v1/groups/${group.id}/members?limit=50`);
  fireEvent.click(screen.getByRole("button", { name: "加载更多成员" }));
  await flush();
  expect(roster.mock.calls[4][0]).toBe(`/api/v1/groups/${group.id}/members?limit=50&before=even-older-page`);
});
