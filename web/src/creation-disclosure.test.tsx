import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { App } from "./App";
import { deferred, group, json, other, page } from "./test/groups";
import { member } from "./test/members";
import { message } from "./test/messages";
const writes = vi.fn<typeof fetch>();
const sidebar = (kind: "group" | "member") => within(screen.getByRole("complementary", { name: kind === "group" ? "群组" : "成员" }));
const label = (kind: "group" | "member") => kind === "group" ? "创建群组" : "创建成员";
const inputName = (kind: "group" | "member") => kind === "group" ? "群组名称" : "成员名称";
const select = (name: string) => fireEvent.click(screen.getByRole("button", { name }));
async function open(kind: "group" | "member") {
  render(<App />); await screen.findByRole("button", { name: group.name });
  if (kind === "member") { select(group.name); fireEvent.click(screen.getByRole("button", { name: "成员" })); }
}
function fill(kind: "group" | "member", value: string) {
  fireEvent.change(sidebar(kind).getByRole("textbox", { name: inputName(kind) }), { target: { value } });
  if (kind === "member") fireEvent.change(sidebar(kind).getByRole("textbox", { name: "成员职责" }), { target: { value: member.role } });
}
beforeEach(() => {
  writes.mockReset().mockImplementation(async url => json(String(url).includes("/members") ? member : { ...group, description: "" }, 201));
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === "POST") return writes(input, init);
    const url = String(input);
    return Promise.resolve(url.includes("/messages") ? page([message], "older")
      : url.includes("/members") || url.includes("/providers") ? page() : page([group, other]));
  });
});
afterEach(() => vi.unstubAllGlobals());
it.each(["group", "member"] as const)("initially collapses the %s form behind a focusable button and preserves its mounted draft", async kind => {
  await open(kind); const panel = sidebar(kind), name = label(kind);
  const toggle = panel.getByRole("button", { name: `展开${name}` });
  expect(toggle).toHaveAttribute("aria-expanded", "false"); expect(toggle.tagName).toBe("BUTTON");
  expect(toggle.tabIndex).toBe(0); toggle.focus(); expect(toggle).toHaveFocus();
  expect(panel.queryByRole("form", { name })).not.toBeInTheDocument();
  fireEvent.click(toggle); expect(toggle).toHaveAttribute("aria-expanded", "true");
  const form = panel.getByRole("form", { name }); fill(kind, "保留的草稿");
  expect(document.getElementById(toggle.getAttribute("aria-controls")!)).toContainElement(form);
  fireEvent.click(toggle); expect(form).toBeInTheDocument(); expect(form).not.toBeVisible(); expect(toggle).toHaveFocus();
  fireEvent.click(toggle); expect(panel.getByRole("form", { name })).toBe(form);
  expect(panel.getByRole("textbox", { name: inputName(kind) })).toHaveValue("保留的草稿");
  expect(writes).not.toHaveBeenCalled();
});
it.each(["group", "member"] as const)("keeps collapsed %s pending/unknown status visible without cancelling or changing a retry", async kind => {
  await open(kind); const panel = sidebar(kind), name = label(kind);
  fireEvent.click(panel.getByRole("button", { name: `展开${name}` })); fill(kind, kind === "group" ? group.name : member.name);
  const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise);
  fireEvent.submit(panel.getByRole("form", { name }));
  fireEvent.click(panel.getByRole("button", { name: `收起${name}` }));
  expect(panel.getByRole("status", { name: `${name}状态` })).toHaveTextContent("正在创建");
  expect(writes.mock.calls[0][1]?.signal?.aborted).toBe(false);
  await act(async () => pending.resolve(json({}, 503)));
  expect(panel.getByRole("alert", { name: `${name}状态` })).toHaveTextContent("创建结果未确认");
  expect(panel.queryByRole("form", { name })).not.toBeInTheDocument(); expect(writes).toHaveBeenCalledTimes(1);
  fireEvent.click(panel.getByRole("button", { name: `展开${name}` }));
  expect(panel.getByRole("textbox", { name: inputName(kind) })).toBeDisabled();
  fireEvent.click(panel.getByRole("button", { name: kind === "group" ? "重试同一次创建" : "重试同一次成员创建" }));
  await act(async () => {});
  const attempts = writes.mock.calls.map(([url, init]) => [url, init?.body, new Headers(init?.headers).get("Idempotency-Key")]);
  expect(attempts[1]).toEqual(attempts[0]); expect(writes).toHaveBeenCalledTimes(2);
  expect(panel.getByRole("textbox", { name: inputName(kind) })).toHaveValue("");
});
it("preserves per-group disclosure state and B's open draft while retrying collapsed unknown A", async () => {
  await open("member"); let panel = sidebar("member");
  fireEvent.click(panel.getByRole("button", { name: "展开创建成员" })); fill("member", member.name);
  writes.mockRejectedValueOnce(new TypeError("offline")); fireEvent.submit(panel.getByRole("form", { name: "创建成员" }));
  await panel.findByRole("alert"); fireEvent.click(panel.getByRole("button", { name: "收起创建成员" }));
  select(other.name); panel = sidebar("member");
  expect(panel.getByRole("button", { name: "展开创建成员" })).toHaveAttribute("aria-expanded", "false");
  fireEvent.click(panel.getByRole("button", { name: "展开创建成员" })); fill("member", "B 的草稿");
  expect(panel.queryByRole("alert")).not.toBeInTheDocument(); select(group.name); panel = sidebar("member");
  expect(panel.getByRole("button", { name: "展开创建成员" })).toHaveAttribute("aria-expanded", "false");
  expect(panel.getByRole("alert", { name: "创建成员状态" })).toHaveTextContent("创建结果未确认");
  fireEvent.click(panel.getByRole("button", { name: "展开创建成员" }));
  fireEvent.click(panel.getByRole("button", { name: "重试同一次成员创建" })); await act(async () => {});
  const attempts = writes.mock.calls.map(([url, init]) => [url, init?.body, new Headers(init?.headers).get("Idempotency-Key")]);
  expect(attempts[1]).toEqual(attempts[0]); select(other.name); panel = sidebar("member");
  expect(panel.getByRole("button", { name: "收起创建成员" })).toHaveAttribute("aria-expanded", "true");
  expect(panel.getByRole("textbox", { name: "成员名称" })).toHaveValue("B 的草稿");
});
it("keeps history controls and composer in named focusable regions with a direct input link and no selected manifesto", async () => {
  await open("member"); fireEvent.click(screen.getByRole("button", { name: "关闭详情" }));
  const main = within(screen.getByRole("main", { name: "讨论" }));
  const intro = main.getByRole("region", { name: "群组简介" });
  expect(within(intro).getByRole("heading", { name: group.name })).toBeVisible();
  expect(within(intro).getByText(group.description)).toBeVisible();
  expect(intro.tabIndex).toBe(0); intro.focus(); expect(intro).toHaveFocus();
  const history = main.getByRole("region", { name: "消息记录" }), composer = main.getByRole("region", { name: "撰写消息" });
  expect(history.tabIndex).toBe(0); expect(composer.tabIndex).toBe(0);
  expect(await within(history).findByRole("button", { name: "加载更早消息" })).toBeVisible();
  expect(within(history).getByRole("button", { name: "刷新并重置历史窗口" })).toBeVisible();
  const field = within(composer).getByRole("textbox", { name: "消息内容" });
  expect(within(history).getByRole("link", { name: "跳至消息输入" })).toHaveAttribute("href", `#${field.id}`);
  field.focus(); expect(field).toHaveFocus(); expect(main.queryByText(/先有讨论/)).not.toBeInTheDocument();
});
