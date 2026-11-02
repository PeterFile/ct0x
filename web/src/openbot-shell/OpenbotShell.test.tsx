import * as ReactDOMClient from "react-dom/client";
import { act, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { deferred, group, json, other, page } from "../test/groups";
import { member } from "../test/members";
import { message } from "../test/messages";
import { task } from "../test/tasks";

const transport = vi.fn<typeof fetch>();
const writes = () => transport.mock.calls.filter(([, init]) => init?.method === "POST");
const identity = ([url, init]: Parameters<typeof fetch>) => ({ url, body: init?.body,
  key: new Headers(init?.headers).get("Idempotency-Key") });
vi.mock("react-dom/client", async importOriginal => {
  const actual = await importOriginal<typeof ReactDOMClient>();
  return { ...actual, createRoot: vi.fn(actual.createRoot) };
});
let entrySequence = 0;
let resize: (mobile: boolean) => void;

beforeEach(() => {
  let mobile = false;
  const events = new EventTarget();
  const media = {
    get matches() { return mobile; }, media: "(max-width: 56rem)",
    addEventListener: events.addEventListener.bind(events), removeEventListener: events.removeEventListener.bind(events),
  };
  vi.stubGlobal("matchMedia", vi.fn(() => media));
  resize = value => act(() => { mobile = value; events.dispatchEvent(new Event("change")); });
  transport.mockReset().mockImplementation(async input => {
    const url = String(input);
    if (url.includes("/members")) return page(url.includes(other.id) ? [] : [member]);
    if (/\/tasks\//.test(url)) return json(task);
    if (url.includes("/messages") || url.includes("/tasks") || url.includes("/history") || url.startsWith("/api/v1/providers")) return page();
    if (url.startsWith("/api/v1/groups")) return page([group, other]);
    throw new Error(`Unexpected request: ${url}`);
  });
  vi.stubGlobal("fetch", transport);
  vi.mocked(ReactDOMClient.createRoot).mockClear();
  document.body.innerHTML = '<div id="root"></div>';
});
afterEach(() => {
  act(() => {
    for (const result of vi.mocked(ReactDOMClient.createRoot).mock.results) {
      if (result.type === "return") result.value.unmount();
    }
  });
  document.body.innerHTML = "";
  window.history.replaceState({}, "", "/");
  vi.restoreAllMocks();
  vi.doUnmock("./OpenbotShell");
  vi.unstubAllGlobals();
});

async function start(search = "?shell=openbot") {
  window.history.replaceState({}, "", `/${search}`);
  // A fresh entry module, but shared React/App modules, for each simulated page load.
  const entry = `../main.tsx?openbot-entry-test=${++entrySequence}`;
  await act(async () => { await import(/* @vite-ignore */ entry); });
  await screen.findByRole("button", { name: group.name });
}
const navigation = () => within(screen.getByRole("navigation", { name: "工作区导航" }));
const rail = (name: string) => navigation().getByRole("button", { name });
function selectGroup(name: string) {
  fireEvent.click(rail("群组列表"));
  fireEvent.click(screen.getByRole("button", { name }));
}
const content = () => screen.getByRole("textbox", { name: "消息内容" });
const change = (field: HTMLElement, value: string) => fireEvent.change(field, { target: { value } });
const taskPanel = () => within(screen.getByRole("region", { name: "群组任务" }));

describe("load-time shell entry", () => {
  it.each(["", "?shell=other", "?shell=Openbot"])("preserves the default entry for %s", async search => {
    await start(search);
    expect(screen.queryByRole("navigation", { name: "工作区导航" })).not.toBeInTheDocument();
    expect(screen.getByRole("banner")).toHaveTextContent("ct0x");
    expect(screen.getByRole("main", { name: "讨论" })).toBeVisible();
    window.history.replaceState({}, "", "/?shell=openbot");
    fireEvent.click(screen.getByRole("button", { name: group.name }));
    await screen.findByText("尚无消息，可发送第一条讨论。");
    expect(screen.queryByRole("navigation", { name: "工作区导航" })).not.toBeInTheDocument();
  });

  it("loads the opt-in entry once and keeps its controls after the URL changes", async () => {
    await start("?other=1&shell=openbot");
    expect(navigation().getAllByRole("button").map(button => button.getAttribute("aria-label"))).toEqual([
      "群组列表", "聊天", "搜索", "成员", "任务",
    ]);
    const license = screen.getByRole("link", { name: "来源与许可" });
    expect(license).toHaveAttribute("href", "/licenses/openbot/index.html");
    window.history.replaceState({}, "", "/");
    fireEvent.click(rail("聊天"));
    expect(screen.getByRole("main", { name: "讨论" })).toHaveTextContent("请先选择群组");
    fireEvent.click(rail("任务"));
    expect(screen.getByRole("complementary", { name: "任务" })).toHaveTextContent("请先选择群组");
    expect(rail("任务")).toHaveAttribute("aria-expanded", "true");
    fireEvent.click(screen.getByRole("button", { name: "关闭详情" }));
    expect(rail("任务")).toHaveFocus();
    expect(writes()).toHaveLength(0);
    expect(transport.mock.calls.filter(([url]) => String(url).includes("/tasks"))).toHaveLength(0);
  });

  it("reports an optional chunk failure in Chinese before mounting any live App state", async () => {
    vi.doMock("./OpenbotShell", () => { throw new Error("fixture chunk unavailable"); });
    window.history.replaceState({}, "", "/?shell=openbot");
    const entry = `../main.tsx?openbot-entry-test=${++entrySequence}`;
    await act(async () => { await import(/* @vite-ignore */ entry); });
    expect(await screen.findByRole("alert")).toHaveTextContent("界面未能载入");
    expect(screen.getByRole("link", { name: "返回默认界面" })).toHaveAttribute("href", "/");
    expect(transport).not.toHaveBeenCalled();
  });
});

describe("real Openbot rail interactions", () => {
  it.each([false, true])("keeps directed drafts and pending/unknown send identity through rail and group changes (mobile=%s)", async mobile => {
    resize(mobile); await start(); selectGroup(group.name);
    await screen.findByText("尚无消息，可发送第一条讨论。");
    const input = content(), composer = screen.getByRole("region", { name: "撰写消息" });
    const raw = "  保留原始消息\n切换不重发  "; change(input, raw);
    fireEvent.change(screen.getByRole("combobox", { name: "接收方式" }), { target: { value: "selected" } });
    const recipientName = `${member.name} · ${member.role} · ${member.id}`;
    fireEvent.click(await screen.findByRole("checkbox", { name: recipientName }));
    fireEvent.click(rail("成员")); fireEvent.click(rail("搜索"));
    expect(composer).toBeInTheDocument(); expect(composer.closest("[hidden]") !== null).toBe(mobile);
    fireEvent.click(screen.getByRole("button", { name: mobile ? "返回聊天" : "关闭详情" }));
    expect(rail("搜索")).toHaveFocus();
    selectGroup(other.name); await screen.findByText("尚无消息，可发送第一条讨论。");
    change(content(), "另一群草稿"); selectGroup(group.name);
    expect(content()).toBe(input); expect(input).toHaveValue(raw);
    expect(await screen.findByRole("checkbox", { name: recipientName })).toBeChecked();

    const pending = deferred<Response>();
    transport.mockImplementationOnce(() => pending.promise);
    fireEvent.submit(screen.getByRole("form", { name: "发送消息" }));
    expect(writes()).toHaveLength(1);
    fireEvent.click(rail("任务")); fireEvent.click(rail("搜索")); selectGroup(other.name);
    expect(content()).toHaveValue("另一群草稿");
    expect(writes()[0][1]?.signal?.aborted).toBe(false);
    await act(async () => pending.resolve(json({}, 503)));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    selectGroup(group.name);
    expect(content()).toBe(input); expect(input).toBeDisabled(); expect(input).toHaveValue(raw);
    expect(screen.getByRole("alert")).toHaveTextContent("发送结果未确认");
    fireEvent.click(rail("搜索")); fireEvent.click(rail("聊天"));
    expect(content()).toBe(input); expect(input).toBeDisabled(); expect(writes()).toHaveLength(1);

    transport.mockImplementationOnce(async () => json({ ...message, content: raw, deliveries: [] }, 201));
    fireEvent.click(screen.getByRole("button", { name: "重试同一次发送" }));
    await screen.findByText("消息已持久化；投递状态见记录。");
    expect(writes()).toHaveLength(2); expect(identity(writes()[1])).toEqual(identity(writes()[0]));
    expect(JSON.parse(String(writes()[0][1]?.body))).toEqual({ content: raw, recipient_ids: [member.id] });
    expect(identity(writes()[0]).key).toMatch(/^[\da-f-]{36}$/);
    expect(screen.getByRole("region", { name: "撰写消息" })).toBe(composer);
    expect(content()).toBe(input); expect(input).toHaveValue("");
    selectGroup(other.name); expect(content()).toHaveValue("另一群草稿");
    await screen.findByText("尚无消息，可发送第一条讨论。");
  });

  it.each([false, true])("keeps a task operation mounted and unknown until explicit same-operation retry (mobile=%s)", async mobile => {
    resize(mobile); await start(); selectGroup(group.name); fireEvent.click(rail("任务"));
    await taskPanel().findByText("尚无任务，可创建任务并约定验收条件。");
    const title = taskPanel().getByRole("textbox", { name: "任务标题" });
    const form = taskPanel().getByRole("form", { name: "创建任务" });
    change(title, task.title);
    change(taskPanel().getByRole("textbox", { name: "任务描述（可选）" }), task.description);
    change(taskPanel().getByRole("textbox", { name: "验收条件" }), task.acceptance);
    const pending = deferred<Response>(); transport.mockImplementationOnce(() => pending.promise);
    fireEvent.submit(form); fireEvent.click(rail("成员")); fireEvent.click(rail("搜索"));
    expect(form).toBeInTheDocument(); expect(form).not.toBeVisible();
    expect(writes()).toHaveLength(1); expect(writes()[0][1]?.signal?.aborted).toBe(false);
    selectGroup(other.name); fireEvent.click(rail("任务"));
    await taskPanel().findByText("尚无任务，可创建任务并约定验收条件。");
    change(taskPanel().getByRole("textbox", { name: "任务标题" }), "另一群任务草稿");
    await act(async () => pending.resolve(json({}, 503)));
    expect(taskPanel().queryByRole("alert")).not.toBeInTheDocument();
    selectGroup(group.name); fireEvent.click(rail("任务"));
    expect(taskPanel().getByRole("form", { name: "创建任务" })).toBe(form);
    expect(title).toHaveValue(task.title); expect(title).toBeDisabled();
    expect(taskPanel().getByRole("alert")).toHaveTextContent("创建结果未确认");
    fireEvent.click(rail("聊天")); fireEvent.click(rail("任务"));
    expect(title).toBeDisabled(); expect(writes()).toHaveLength(1);
    transport.mockImplementationOnce(async () => json(task, 201));
    fireEvent.click(taskPanel().getByRole("button", { name: "重试同一次任务创建" }));
    await taskPanel().findByRole("region", { name: "任务详情" });
    expect(writes()).toHaveLength(2); expect(identity(writes()[1])).toEqual(identity(writes()[0]));
    expect(title).toHaveValue(""); expect(title).toBeEnabled();
    selectGroup(other.name); fireEvent.click(rail("任务"));
    expect(taskPanel().getByRole("textbox", { name: "任务标题" })).toHaveValue("另一群任务草稿");
    await taskPanel().findByText("尚无任务，可创建任务并约定验收条件。");
  });

  it("opens real details from the phone list and recovers focus without exposing hidden panes", async () => {
    resize(true); await start();
    expect(screen.queryByRole("main")).not.toBeInTheDocument();
    fireEvent.click(rail("搜索"));
    const heading = screen.getByRole("heading", { name: "搜索" });
    expect(heading).toHaveFocus();
    expect(screen.getByRole("complementary", { name: "搜索" })).toHaveTextContent("请先选择群组");
    expect(screen.queryByRole("complementary", { name: "群组" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "返回聊天" })); expect(rail("搜索")).toHaveFocus();
    selectGroup(group.name); await screen.findByText("尚无消息，可发送第一条讨论。");
    const input = content(); change(input, "断点草稿");
    resize(false); const refresh = screen.getByRole("button", { name: "刷新群组" }); refresh.focus();
    resize(true); expect(screen.getByRole("heading", { name: group.name })).toHaveFocus();
    expect(refresh.closest("[hidden]")).not.toBeNull();
    fireEvent.click(rail("任务")); fireEvent.click(rail("群组列表"));
    expect(screen.getByRole("button", { name: group.name })).toHaveFocus();
    expect(screen.queryByRole("main")).not.toBeInTheDocument();
    fireEvent.click(rail("聊天")); expect(content()).toBe(input); expect(input).toHaveValue("断点草稿");
    expect(document.activeElement?.closest("[hidden], [inert]")).toBeNull();
    expect(screen.getByRole("link", { name: "跳至讨论区" })).toHaveAttribute("href", "#im-chat");
    await screen.findByText("尚无消息，可发送第一条讨论。");
  });
});
