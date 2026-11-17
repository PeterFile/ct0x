import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { App } from "./App";
import { Discussion } from "./Discussion";
import type { Member } from "./api";
import { deferred, group, json, other, page } from "./test/groups";
import { member } from "./test/members";
import { message } from "./test/messages";

const twin = { ...member, id: "member-twin", role: member.role };
const departed = { ...member, id: "member-left", name: "已离群", status: "left" };
const foreign = { ...member, id: "member-foreign", name: "外群成员", group_id: other.id };
const roster = [member, twin, departed, foreign];
const writes = vi.fn<typeof fetch>();
const content = () => screen.getByRole("textbox", { name: "消息内容" });
const change = (value: string) => fireEvent.change(content(), { target: { value, selectionStart: value.length } });
const submit = () => fireEvent.submit(screen.getByRole("form", { name: "发送消息" }));
const chosen = () => within(screen.getByRole("list", { name: "已选接收者" }));
const choices = () => within(screen.getByRole("listbox", { name: "选择消息接收者" }));
const choice = (id: string) => choices().getByRole("option", { name: new RegExp(id) });
const attempt = (index = 0) => {
  const [url, init] = writes.mock.calls[index];
  return { url, body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key") };
};
function scope(groupId = group.id, members: Member[] = roster) {
  return <Discussion groupId={groupId} members={{ [group.id]: members }} />;
}
beforeEach(() => {
  writes.mockReset().mockImplementation(async (_url, init) => json({ ...message, content: JSON.parse(String(init?.body)).content }, 201));
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === "POST") return writes(input, init);
    const url = String(input);
    return Promise.resolve(url.includes("/members") ? page([member, twin, departed])
      : url.includes("/messages") || url.startsWith("/api/v1/providers") ? page() : page([group, other]));
  });
});
afterEach(() => vi.unstubAllGlobals());

it("sends ordinary text and unselected @ names through default routing without a mode picker", async () => {
  render(scope()); await screen.findByText("尚无消息，可发送第一条讨论。");
  expect(screen.getByRole("combobox", { name: "接收方式" })).not.toBeVisible();
  change(`请${member.name}看看，也只是正文 @${member.name}`); submit();
  await screen.findByText(/消息已持久化/);
  expect(JSON.parse(String(attempt().body))).toEqual({ content: `请${member.name}看看，也只是正文 @${member.name}` });
});
it("binds the clicked same-name suggestion to its ID, not a subsequent matching name in text", async () => {
  render(scope()); await screen.findByText("尚无消息，可发送第一条讨论。");
  change("请看看@");
  expect(choices().getAllByRole("option")).toHaveLength(2);
  expect(choices().queryByRole("option", { name: /member-left|member-foreign/ })).not.toBeInTheDocument();
  fireEvent.click(choice(twin.id));
  expect(chosen().getByText(new RegExp(twin.id))).toBeVisible();
  expect(chosen().queryByText(new RegExp(`${member.id}$`))).not.toBeInTheDocument();
  change(`${member.name} 和 @${member.name} 都只是正文`); submit();
  await screen.findByText(/消息已持久化/);
  expect(JSON.parse(String(attempt().body))).toEqual({ content: `${member.name} 和 @${member.name} 都只是正文`, recipient_ids: [twin.id] });
});
it.each([
  { mode: "public", entry: "mention" },
  { mode: "default", entry: "avatar" },
])("UI-1: selects only the new $entry recipient after switching to $mode", async ({ mode, entry }) => {
  const view = render(scope()); await screen.findByText("尚无消息，可发送第一条讨论。");
  change("@"); fireEvent.click(choice(member.id));
  fireEvent.click(screen.getByText("发送选项", { selector: "summary" }));
  fireEvent.change(screen.getByRole("combobox", { name: "接收方式" }), { target: { value: mode } });
  expect(screen.queryByRole("list", { name: "已选接收者" })).not.toBeInTheDocument();
  if (entry === "mention") {
    change("@"); expect.soft(choices().queryByRole("option", { name: new RegExp(member.id) })).toBeVisible();
    fireEvent.click(choice(twin.id));
  } else {
    view.rerender(<Discussion groupId={group.id} members={{ [group.id]: roster }}
      recipientSelection={{ groupId: group.id, memberId: twin.id, version: 1 }} />);
  }
  expect.soft(chosen().getAllByRole("listitem")).toHaveLength(1);
  change("本次只发给 B"); submit(); await screen.findByText(/消息已持久化/);
  expect(JSON.parse(String(attempt().body))).toEqual({ content: "本次只发给 B", recipient_ids: [twin.id] });
});
it("supports keyboard selection, IME-safe Enter and explicit recipient removal", async () => {
  render(scope()); await screen.findByText("尚无消息，可发送第一条讨论。");
  change("@");
  fireEvent.keyDown(content(), { key: "ArrowDown" });
  fireEvent.keyDown(content(), { key: "Enter", isComposing: true });
  expect(screen.queryByRole("list", { name: "已选接收者" })).not.toBeInTheDocument();
  fireEvent.keyDown(content(), { key: "Enter" });
  expect(chosen().getByText(new RegExp(twin.id))).toBeVisible();
  expect(content()).toHaveFocus();
  fireEvent.click(chosen().getByRole("button", { name: `移除接收者 ${twin.id}` }));
  expect(screen.queryByRole("list", { name: "已选接收者" })).not.toBeInTheDocument();
  submit(); await screen.findByText(/消息已持久化/);
  expect(JSON.parse(String(attempt().body))).not.toHaveProperty("recipient_ids");
});
it("blocks a newly invalid recipient rather than silently changing the draft to another ID", async () => {
  const view = render(scope()); await screen.findByText("尚无消息，可发送第一条讨论。");
  change("@"); fireEvent.click(choice(twin.id));
  view.rerender(scope(group.id, [member, { ...twin, status: "left" }])); submit();
  expect(screen.getByRole("alert")).toHaveTextContent("请选择本群已载入的在群成员");
  expect(chosen().getByText(/已失效/)).toBeVisible(); expect(writes).not.toHaveBeenCalled();
  view.rerender(scope(group.id, [member, { ...twin, group_id: other.id }])); submit();
  expect(writes).not.toHaveBeenCalled();
  fireEvent.click(chosen().getByRole("button", { name: `移除接收者 ${twin.id}` }));
  submit(); await screen.findByText(/消息已持久化/);
  expect(JSON.parse(String(attempt().body))).not.toHaveProperty("recipient_ids");
});
it("retains the frozen selected-ID operation through hidden pending work, roster changes and uncertain retries", async () => {
  const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise);
  const view = render(scope()); await screen.findByText("尚无消息，可发送第一条讨论。");
  change("@"); fireEvent.click(choice(twin.id)); change("A 原始内容"); submit();
  const original = attempt(); const signal = writes.mock.calls[0][1]?.signal;
  view.rerender(scope(other.id)); await screen.findByText("尚无消息，可发送第一条讨论。"); change("B 的草稿");
  expect(signal?.aborted).toBe(false);
  await act(async () => pending.resolve(json({}, 503)));
  view.rerender(scope(group.id, []));
  expect(content()).toHaveValue("A 原始内容"); expect(content()).toBeDisabled();
  expect(chosen().getByRole("button", { name: `移除接收者 ${twin.id}` })).toBeDisabled();
  fireEvent.click(screen.getByRole("button", { name: "重试同一次发送" })); await screen.findByText(/消息已持久化/);
  expect(attempt(1)).toEqual(original);
  expect(JSON.parse(String(original.body))).toEqual({ content: "A 原始内容", recipient_ids: [twin.id] });
  view.rerender(scope(other.id)); expect(content()).toHaveValue("B 的草稿");
});
it.each(["right click", "keyboard"])("selects a real member from the avatar menu by %s without changing message text", async method => {
  render(<App />); fireEvent.click(await screen.findByRole("button", { name: group.name }));
  await screen.findByText("尚无消息，可发送第一条讨论。"); change("保留正文");
  fireEvent.click(screen.getByRole("button", { name: "成员" }));
  const avatar = await screen.findByRole("button", { name: `成员头像 ${twin.name} · ${twin.role} · ${twin.id}` });
  if (method === "right click") fireEvent.contextMenu(avatar);
  else { avatar.focus(); fireEvent.keyDown(avatar, { key: "F10", shiftKey: true }); }
  expect(screen.getByRole("menu")).toHaveTextContent(twin.id);
  const action = screen.getByRole("menuitem", { name: "设为消息接收者" }); expect(action).toHaveFocus(); fireEvent.click(action);
  expect(content()).toHaveValue("保留正文"); expect(content()).toHaveFocus();
  expect(chosen().getByText(new RegExp(twin.id))).toBeVisible();
  submit(); await screen.findByText(/消息已持久化/);
  expect(JSON.parse(String(attempt().body))).toEqual({ content: "保留正文", recipient_ids: [twin.id] });
});
