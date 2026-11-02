import { StrictMode } from "react";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { deferred, group, json, other, page } from "./test/groups";
import { member, provider } from "./test/members";
import { message } from "./test/messages";
import { acceptedTask, secondTask, task, taskInput } from "./test/tasks";

const transport = vi.fn<typeof fetch>();
const posts = () => transport.mock.calls.filter(([, init]) => init?.method === "POST");
const nameInput = () => screen.getByRole("textbox", { name: "群组名称" });
function openCreateGroup() {
  const toggle = screen.queryByRole("button", { name: "展开创建群组" });
  if (toggle) fireEvent.click(toggle);
}
function submit(name = "调研") {
  openCreateGroup();
  fireEvent.change(nameInput(), { target: { value: name } });
  fireEvent.submit(screen.getByRole("form", { name: "创建群组" }));
}
function stubViewport() {
  let mobile = false;
  const events = new EventTarget();
  const media = {
    get matches() { return mobile; }, media: "(max-width: 56rem)", onchange: null,
    addEventListener: events.addEventListener.bind(events), removeEventListener: events.removeEventListener.bind(events),
    dispatchEvent: events.dispatchEvent.bind(events),
    addListener: (listener: EventListener) => events.addEventListener("change", listener),
    removeListener: (listener: EventListener) => events.removeEventListener("change", listener),
  };
  vi.stubGlobal("matchMedia", vi.fn(() => media));
  return (value: boolean) => act(() => {
    if (value === mobile) return;
    mobile = value;
    events.dispatchEvent(Object.assign(new Event("change"), { matches: mobile, media: media.media }));
  });
}
let resize: ReturnType<typeof stubViewport>;
beforeEach(() => {
  resize = stubViewport();
  transport.mockReset();
  transport.mockImplementation(async () => page());
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) =>
    String(input).includes("/members") || String(input).includes("/messages") || String(input).startsWith("/api/v1/providers")
      ? Promise.resolve(page()) : transport(input, init));
});
afterEach(() => vi.unstubAllGlobals());

describe("workspace groups", () => {
  it("names the conversation workspace without opening a context pane by default", async () => {
    render(<App />);
    expect(screen.getByRole("banner")).toContainElement(
      screen.getByRole("heading", { level: 1, name: "ct0x" }),
    );
    expect(screen.getByRole("complementary", { name: "群组" })).toBeVisible();
    expect(screen.getByRole("main", { name: "讨论" })).toBeVisible();
    expect(screen.queryByRole("complementary", { name: "成员" })).not.toBeInTheDocument();
    await screen.findByText("尚无群组，可在下方创建。");
  });

  it("provides a skip link to a focusable discussion landmark", async () => {
    render(<App />);
    const link = screen.getByRole("link", { name: "跳至讨论区" });
    const main = screen.getByRole("main", { name: "讨论" });
    expect(link).toHaveAttribute("href", `#${main.id}`);
    expect(main.id).not.toBe("");
    expect(main).toHaveAttribute("tabindex", "-1");
    main.focus();
    expect(main).toHaveFocus();
    await screen.findByText("尚无群组，可在下方创建。");
  });

  it("reports loading before a real empty page without enabling future operations", async () => {
    render(<App />);
    expect(screen.getByText("正在载入群组…")).toBeVisible();
    await screen.findByText("尚无群组，可在下方创建。");
    openCreateGroup();
    expect(nameInput()).toBeRequired();
    expect(within(screen.getByRole("main")).getByText("请先选择群组，再查看讨论和发送消息。")).toBeVisible();
    expect(within(screen.getByRole("main")).queryByRole("textbox")).not.toBeInTheDocument();
    expect(screen.queryByRole("complementary", { name: "成员" })).not.toBeInTheDocument();
  });

  it("distinguishes disconnection and HTTP rejection, then retries explicitly", async () => {
    transport.mockRejectedValueOnce(new TypeError("offline"));
    render(<App />);
    expect(await screen.findByRole("alert")).toHaveTextContent("无法连接本地服务");
    expect(screen.queryByText("尚无群组，可在下方创建。")).not.toBeInTheDocument();
    transport.mockResolvedValueOnce(json({ error: { code: "not_found", message: "未找到" } }, 404));
    fireEvent.click(screen.getByRole("button", { name: "重试载入" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("404");
    fireEvent.click(screen.getByRole("button", { name: "重试载入" }));
    await screen.findByText("尚无群组，可在下方创建。");
    expect(transport).toHaveBeenCalledTimes(3);
  });

  it("loads another page only on demand, deduplicates IDs and preserves selection", async () => {
    transport.mockResolvedValueOnce(page([group], "opaque+/"));
    render(<App />);
    fireEvent.click(await screen.findByRole("button", { name: group.name }));
    expect(within(screen.getByRole("main")).getByText(group.description)).toBeVisible();
    expect(transport).toHaveBeenCalledTimes(1);
    transport.mockResolvedValueOnce(page([group, other]));
    fireEvent.click(screen.getByRole("button", { name: "加载更多群组" }));
    await screen.findByRole("button", { name: other.name });
    expect(screen.getAllByRole("button", { name: group.name })).toHaveLength(1);
    expect(screen.getByRole("button", { name: group.name })).toHaveAttribute("aria-pressed", "true");
    expect(screen.queryByRole("button", { name: "加载更多群组" })).not.toBeInTheDocument();
    expect(transport.mock.calls[1][0]).toBe("/api/v1/groups?limit=50&before=opaque%2B%2F");
    expect(transport).toHaveBeenCalledTimes(2);
  });

  it.each([false, true])("submits JSON once while pending and opens the returned group when still waiting (mobile=%s)", async mobile => {
    resize(mobile); render(<App />);
    await screen.findByText("尚无群组，可在下方创建。");
    const response = deferred<Response>();
    transport.mockReturnValueOnce(response.promise);
    openCreateGroup();
    fireEvent.change(screen.getByRole("textbox", { name: "群组说明（可选）" }), { target: { value: group.description } });
    submit();
    fireEvent.submit(screen.getByRole("form", { name: "创建群组" }));
    expect(screen.getByRole("button", { name: "正在创建…" })).toBeDisabled();
    expect(nameInput()).toBeDisabled();
    expect(posts()).toHaveLength(1);
    expect(posts()[0][0]).toBe("/api/v1/groups");
    expect(JSON.parse(String(posts()[0][1]?.body))).toEqual({ name: group.name, description: group.description });
    expect(new Headers(posts()[0][1]?.headers).get("Content-Type")).toBe("application/json");
    expect(new Headers(posts()[0][1]?.headers).get("Idempotency-Key")).toMatch(/^[\da-f-]{36}$/);
    if (mobile) expect(screen.queryByRole("main")).not.toBeInTheDocument();
    else expect(within(screen.getByRole("main")).queryByText(group.name)).not.toBeInTheDocument();
    await act(async () => response.resolve(json(group, 201)));
    expect(within(screen.getByRole("main")).getByRole("heading", { name: group.name })).toHaveFocus();
    if (mobile) fireEvent.click(screen.getByRole("button", { name: "返回群组" }));
    expect(nameInput()).toHaveValue("");
    expect(screen.getByRole("button", { name: group.name })).toHaveAttribute("aria-pressed", "true");
  });

  it.each(["network", "server", "malformed", "accepted-202", "unexpected-200"])("retains the exact operation after an uncertain %s outcome", async failure => {
    render(<App />);
    await screen.findByText("尚无群组，可在下方创建。");
    if (failure === "network") transport.mockRejectedValueOnce(new TypeError("offline"));
    else if (failure === "accepted-202" || failure === "unexpected-200") transport.mockResolvedValueOnce(json(group, failure === "accepted-202" ? 202 : 200));
    else transport.mockResolvedValueOnce(failure === "server" ? json({ error: { code: "internal", message: "失败" } }, 503) : json({}, 201));
    submit();
    expect(await screen.findByRole("alert")).toHaveTextContent("创建结果未确认");
    expect(nameInput()).toBeDisabled();
    expect(nameInput()).toHaveValue(group.name);
    expect(screen.queryByRole("button", { name: group.name })).not.toBeInTheDocument();
    expect(within(screen.getByRole("main")).queryByText(group.name)).not.toBeInTheDocument();
    transport.mockResolvedValueOnce(json({ ...group, description: "" }, 201));
    fireEvent.click(screen.getByRole("button", { name: "重试同一次创建" }));
    await screen.findByRole("button", { name: group.name });
    const [first, retry] = posts().map(([, init]) => ({ body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key") }));
    expect(retry).toEqual(first);
    expect(posts()).toHaveLength(2);
  });

  it.each([400, 413, 415])("allows correction after definitive validation rejection HTTP %s with a new key", async status => {
    render(<App />);
    await screen.findByText("尚无群组，可在下方创建。");
    const rejectedName = status === 413 ? group.name.repeat(100) : group.name;
    const code = status === 413 ? "body_too_large" : status === 415 ? "unsupported_media_type" : "invalid";
    transport.mockResolvedValueOnce(json({ error: { code, message: "请求被拒绝" } }, status));
    submit(rejectedName);
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(String(status));
    if (status === 413) expect(alert).toHaveTextContent("请求内容过大");
    if (status === 415) expect(alert).toHaveTextContent("仅接受 JSON");
    expect(nameInput()).toBeEnabled();
    expect(nameInput()).toHaveValue(rejectedName);
    transport.mockResolvedValueOnce(json({ ...other, description: "" }, 201));
    submit(other.name);
    await screen.findByRole("button", { name: other.name });
    expect(posts()[1][1]?.body).toBe(JSON.stringify({ name: other.name }));
    expect(new Headers(posts()[1][1]?.headers).get("Idempotency-Key")).not.toBe(new Headers(posts()[0][1]?.headers).get("Idempotency-Key"));
  });

  it("ignores older reads even when the transport ignores abort", async () => {
    const old = deferred<Response>();
    transport.mockReturnValueOnce(old.promise).mockResolvedValueOnce(page([other]));
    render(<App />);
    fireEvent.click(screen.getByRole("button", { name: "刷新群组" }));
    fireEvent.click(await screen.findByRole("button", { name: other.name }));
    expect(transport.mock.calls[0][1]?.signal?.aborted).toBe(true);
    await act(async () => old.resolve(page([group])));
    expect(screen.queryByRole("button", { name: group.name })).not.toBeInTheDocument();
    expect(within(screen.getByRole("main")).getByRole("heading", { name: other.name })).toBeVisible();
  });

  it("does not remove or overwrite a confirmed creation with an older GET", async () => {
    const old = deferred<Response>();
    transport.mockReturnValueOnce(old.promise).mockResolvedValueOnce(json({ ...group, description: "" }, 201));
    render(<App />);
    submit();
    await screen.findByRole("button", { name: group.name });
    await act(async () => old.resolve(page([{ ...group, name: "旧名称" }, other])));
    expect(screen.getByRole("button", { name: group.name })).toBeVisible();
    expect(screen.queryByRole("button", { name: "旧名称" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "刷新群组" }));
    await act(async () => {});
    expect(screen.getByRole("button", { name: group.name })).toHaveAttribute("aria-pressed", "true");
  });

  it("cleans up StrictMode reads and an in-flight mutation on unmount", async () => {
    const stale = deferred<Response>();
    transport.mockReturnValueOnce(stale.promise);
    const view = render(<StrictMode><App /></StrictMode>);
    await screen.findByText("尚无群组，可在下方创建。");
    expect(transport).toHaveBeenCalledTimes(2);
    expect(transport.mock.calls[0][1]?.signal?.aborted).toBe(true);
    await act(async () => stale.resolve(page([group])));
    expect(screen.queryByRole("button", { name: group.name })).not.toBeInTheDocument();
    const pending = deferred<Response>();
    transport.mockReturnValueOnce(pending.promise);
    submit();
    view.unmount();
    expect(transport.mock.calls[1][1]?.signal?.aborted).toBe(true);
    expect(posts()[0][1]?.signal?.aborted).toBe(true);
    await act(async () => pending.resolve(json(group, 201)));
  });
});

describe("IM shell", () => {
  const taskReads = vi.fn<typeof fetch>(), taskDetails = vi.fn<typeof fetch>(), taskWrites = vi.fn<typeof fetch>();
  const messageReads = vi.fn<typeof fetch>(), messageWrites = vi.fn<typeof fetch>();
  const historyReads = vi.fn<typeof fetch>(), historyDetails = vi.fn<typeof fetch>();
  const memberReads = vi.fn<typeof fetch>(), memberWrites = vi.fn<typeof fetch>(), catalogue = vi.fn<typeof fetch>();
  const main = () => screen.getByRole("main", { name: "讨论" });
  const trigger = (name: "成员" | "任务" | "搜索") => within(main()).getByRole("button", { name });
  const context = (name: "成员" | "任务" | "搜索") => screen.getByRole("complementary", { name });
  const closeContext = () => fireEvent.click(screen.getByRole("button", { name: /^(关闭详情|返回聊天)$/ }));
  function showContext(name: "成员" | "任务" | "搜索") {
    if (screen.queryByRole("button", { name: "返回聊天" })) closeContext();
    if (trigger(name).getAttribute("aria-expanded") !== "true") fireEvent.click(trigger(name));
  }
  function selectGroup(name: string) {
    if (!screen.queryByRole("button", { name })) {
      if (screen.queryByRole("button", { name: "返回聊天" })) closeContext();
      fireEvent.click(screen.getByRole("button", { name: "返回群组" }));
    }
    fireEvent.click(screen.getByRole("button", { name }));
  }
  const tasks = () => within(screen.getByRole("region", { name: "群组任务" }));
  const detail = () => within(tasks().getByRole("region", { name: "任务详情" }));
  const taskField = (name: string) => tasks().getByRole("textbox", { name });
  const history = () => within(screen.getByRole("region", { name: "历史消息搜索" }));
  function search(q: string) {
    change(history().getByRole("textbox", { name: "关键词" }), q);
    fireEvent.submit(history().getByRole("form", { name: "搜索历史消息" }));
  }
  const content = () => screen.getByRole("textbox", { name: "消息内容" });
  const change = (field: HTMLElement, value: string) => fireEvent.change(field, { target: { value } });
  const identity = ([url, init]: Parameters<typeof fetch>) => ({ url, body: init?.body,
    key: new Headers(init?.headers).get("Idempotency-Key") });
  const recipientName = `${member.name} · ${member.role} · ${member.id}`;
  function draftTask() {
    change(taskField("任务标题"), task.title);
    change(taskField("任务描述（可选）"), task.description);
    change(taskField("验收条件"), task.acceptance);
  }
  function openMemberForm() {
    showContext("成员");
    const toggle = screen.queryByRole("button", { name: "展开创建成员" });
    if (toggle) fireEvent.click(toggle);
  }

  beforeEach(() => {
    transport.mockImplementation(async (_url, init) => init?.method === "POST" ? json(group, 201) : page([group, other]));
    taskReads.mockReset().mockImplementation(async () => page());
    taskDetails.mockReset().mockImplementation(async () => json(task));
    taskWrites.mockReset().mockImplementation(async () => json(task, 201));
    messageReads.mockReset().mockImplementation(async () => page());
    messageWrites.mockReset().mockImplementation(async (_url, init) =>
      json({ ...message, content: JSON.parse(String(init?.body)).content, deliveries: [] }, 201));
    historyReads.mockReset().mockImplementation(async () => page());
    historyDetails.mockReset().mockImplementation(async () => json(message));
    memberReads.mockReset().mockImplementation(async url => page(String(url).includes(other.id) ? [] : [member]));
    memberWrites.mockReset().mockImplementation(async () => json(member, 201));
    catalogue.mockReset().mockImplementation(async () => page([provider]));
    vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.includes("/history")) {
        if ((init?.method ?? "GET") !== "GET") throw new Error("Unexpected history mutation");
        return (/\/history\//.test(url) ? historyDetails : historyReads)(input, init);
      }
      if (url.includes("/tasks")) return (init?.method === "POST" ? taskWrites : /\/tasks\//.test(url) ? taskDetails : taskReads)(input, init);
      if (url.includes("/messages")) return (init?.method === "POST" ? messageWrites : messageReads)(input, init);
      if (url.includes("/members")) return (init?.method === "POST" ? memberWrites : memberReads)(input, init);
      if (url.startsWith("/api/v1/providers")) return catalogue(input, init);
      if (url.startsWith("/api/v1/groups")) return transport(input, init);
      return Promise.reject(new Error(`Unexpected request: ${url}`));
    });
  });

  it("opens the real search context without a group, mounts only on first visit and returns focus to its trigger", async () => {
    render(<App />); const button = trigger("搜索");
    expect(button).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByText("只读检索")).not.toBeInTheDocument();
    expect(document.getElementById(button.getAttribute("aria-controls")!)).toBeEmptyDOMElement();
    showContext("搜索");
    expect(within(context("搜索")).getByRole("heading", { name: "搜索" })).toHaveFocus();
    expect(history().getByText("请先选择群组，再搜索历史消息。")).toBeVisible();
    expect(button).toHaveAttribute("aria-expanded", "true");
    expect(history().queryByRole("form")).not.toBeInTheDocument();
    closeContext(); expect(button).toHaveFocus(); expect(button).toHaveAttribute("aria-expanded", "false");
    expect(screen.getByText("只读检索")).not.toBeVisible();
    expect(historyReads).not.toHaveBeenCalled(); expect(historyDetails).not.toHaveBeenCalled();
    await screen.findByRole("button", { name: group.name });
  });

  it.each([false, true])("uses the same search context for real scoped search/page/full reads and preserves its DOM when reopened (mobile=%s)", async mobile => {
    const topic = " \t话题 /?+ ", q = " \u3000原词 %_\\ ", cursor = "opaque+/=?&";
    const source = { ...message, id: "history/source", topic_id: topic, content: "历史摘录", deliveries: [] };
    const older = { ...source, id: "history-older", content: "更早结果" }, full = { ...source, content: "完整历史原文 <b>纯文本</b>" };
    historyReads.mockResolvedValueOnce(page([source], cursor)).mockResolvedValueOnce(page([older]));
    historyDetails.mockResolvedValueOnce(json(full));
    resize(mobile); render(<App />); selectGroup((await screen.findByRole("button", { name: group.name })).textContent!);
    await screen.findByText("尚无消息，可发送第一条讨论。");
    const oldMessages = deferred<Response>(); messageReads.mockReturnValueOnce(oldMessages.promise);
    fireEvent.click(screen.getByRole("button", { name: "刷新并重置历史窗口" }));
    const messageRead = messageReads.mock.calls.at(-1)!;
    const button = trigger("搜索"), chat = main(); showContext("搜索");
    expect(document.getElementById(button.getAttribute("aria-controls")!)).toContainElement(history().getByRole("form"));
    expect(within(context("搜索")).getByRole("heading", { name: "搜索" })).toHaveFocus();
    expect(button).toHaveAttribute("aria-expanded", "true"); expect(chat.closest("[hidden]") !== null).toBe(mobile);
    expect(screen.getByRole("link", { name: mobile ? "跳至搜索详情" : "跳至讨论区" })).toHaveAttribute("href", mobile ? "#im-context" : "#im-chat");
    expect(messageRead[1]?.signal?.aborted).toBe(mobile); expect(memberReads.mock.calls[0][1]?.signal?.aborted).toBe(false);
    await act(async () => oldMessages.resolve(page()));
    expect(historyReads).not.toHaveBeenCalled(); expect(historyDetails).not.toHaveBeenCalled();
    expect(taskReads).not.toHaveBeenCalled(); expect(catalogue).not.toHaveBeenCalled();
    const form = history().getByRole("form"), keyword = history().getByRole("textbox", { name: "关键词" });
    fireEvent.change(history().getByRole("combobox", { name: "检索范围" }), { target: { value: "topic" } });
    change(history().getByRole("textbox", { name: "话题 ID" }), topic); search(q);
    fireEvent.click(await history().findByRole("button", { name: `读取完整消息 ${source.id}` }));
    await history().findByText(full.content); change(keyword, "未提交草稿");
    fireEvent.change(history().getByRole("combobox", { name: "检索范围" }), { target: { value: "main" } });
    fireEvent.click(history().getByRole("button", { name: "加载更早结果" }));
    await history().findByRole("button", { name: `读取完整消息 ${older.id}` });
    const query = new URLSearchParams({ q, limit: "50", topic_id: topic });
    expect(historyReads.mock.calls[0][0]).toBe(`/api/v1/groups/${group.id}/history?${query}`);
    query.set("before", cursor); expect(historyReads.mock.calls[1][0]).toBe(`/api/v1/groups/${group.id}/history?${query}`);
    expect(historyDetails.mock.calls[0][0]).toBe(`/api/v1/groups/${group.id}/history/history%2Fsource?${new URLSearchParams({ topic_id: topic })}`);
    const fullRegion = history().getByRole("region", { name: "完整消息" });
    closeContext(); expect(button).toHaveFocus(); expect(button).toHaveAttribute("aria-expanded", "false");
    expect(form).toBeInTheDocument(); expect(form.closest("[hidden]")).not.toBeNull(); expect(form).not.toBeVisible();
    expect(screen.queryByRole("textbox", { name: "关键词" })).not.toBeInTheDocument();
    showContext("任务"); await tasks().findByText("尚无任务，可创建任务并约定验收条件。"); showContext("搜索");
    expect(history().getByRole("form")).toBe(form); expect(keyword).toHaveValue("未提交草稿");
    expect(history().getByRole("region", { name: "完整消息" })).toBe(fullRegion); expect(within(fullRegion).getByText(full.content)).toBeVisible();
    expect(history().getByRole("list", { name: "已提交的检索条件" })).toHaveTextContent("原词");
    expect(historyReads).toHaveBeenCalledTimes(2); expect(historyDetails).toHaveBeenCalledTimes(1);
    expect(messageWrites).not.toHaveBeenCalled(); expect(taskWrites).not.toHaveBeenCalled(); expect(memberWrites).not.toHaveBeenCalled(); expect(posts()).toHaveLength(0);
  });

  it.each([false, true])("aborts hidden search page/full reads and requires explicit original-operation retries after returning (mobile=%s)", async mobile => {
    historyReads.mockResolvedValueOnce(page([message], "original-cursor"));
    resize(mobile); render(<App />); selectGroup((await screen.findByRole("button", { name: group.name })).textContent!); showContext("搜索"); search("已提交词");
    const oldPage = deferred<Response>(), oldDetail = deferred<Response>();
    historyDetails.mockReturnValueOnce(oldDetail.promise);
    fireEvent.click(await history().findByRole("button", { name: `读取完整消息 ${message.id}` }));
    historyReads.mockReturnValueOnce(oldPage.promise); fireEvent.click(history().getByRole("button", { name: "加载更早结果" }));
    const keyword = history().getByRole("textbox", { name: "关键词" }); change(keyword, "未提交词");
    if (mobile) { closeContext(); fireEvent.click(screen.getByRole("button", { name: "返回群组" })); }
    else { showContext("任务"); showContext("成员"); }
    expect(historyReads.mock.calls[1][1]?.signal?.aborted).toBe(true); expect(historyDetails.mock.calls[0][1]?.signal?.aborted).toBe(true);
    expect(keyword).toBeInTheDocument(); expect(keyword).not.toBeVisible();
    await act(async () => { oldPage.resolve(page([{ ...message, id: "stale", content: "过期结果", deliveries: [] }])); oldDetail.resolve(json({}, 503)); });
    if (mobile) selectGroup(group.name); showContext("搜索");
    expect(historyReads).toHaveBeenCalledTimes(2); expect(historyDetails).toHaveBeenCalledTimes(1);
    expect(history().getAllByRole("alert")).toHaveLength(2); expect(history().getAllByText(/已暂停/)).toHaveLength(2);
    expect(history().queryByRole("status")).not.toBeInTheDocument(); expect(history().queryByText("过期结果")).not.toBeInTheDocument();
    expect(history().getByRole("textbox", { name: "关键词" })).toBe(keyword); expect(keyword).toHaveValue("未提交词");
    historyDetails.mockResolvedValueOnce(json({ ...message, content: "重试全文" }));
    fireEvent.click(history().getByRole("button", { name: "重试搜索" }));
    fireEvent.click(history().getByRole("button", { name: "重试读取完整消息" }));
    await history().findByText("重试全文"); expect(history().queryByRole("alert")).not.toBeInTheDocument();
    expect(historyReads.mock.calls[2][0]).toBe(historyReads.mock.calls[1][0]); expect(historyDetails.mock.calls[1][0]).toBe(historyDetails.mock.calls[0][0]);
  });

  it.each([false, true])("resets search to the real selected group even while hidden without issuing a GET (mobile=%s)", async mobile => {
    historyReads.mockResolvedValueOnce(page([message])); resize(mobile); render(<App />);
    selectGroup((await screen.findByRole("button", { name: group.name })).textContent!); showContext("搜索"); search("A 的词");
    await history().findByRole("button", { name: `读取完整消息 ${message.id}` });
    const form = history().getByRole("form"); closeContext(); selectGroup(other.name);
    expect(form).not.toBeInTheDocument(); expect(historyReads).toHaveBeenCalledTimes(1);
    showContext("搜索"); expect(context("搜索")).toHaveTextContent(other.name);
    expect(history().getByRole("textbox", { name: "关键词" })).toHaveValue("");
    expect(history().queryByRole("region", { name: "历史搜索结果" })).not.toBeInTheDocument(); expect(historyReads).toHaveBeenCalledTimes(1);
    search("B 的词"); await history().findByText("未找到匹配的历史消息。");
    expect(historyReads.mock.calls[1][0]).toBe(`/api/v1/groups/${other.id}/history?${new URLSearchParams({ q: "B 的词", limit: "50" })}`);
    closeContext(); selectGroup(group.name); showContext("搜索");
    expect(history().getByRole("textbox", { name: "关键词" })).toHaveValue(""); expect(historyReads).toHaveBeenCalledTimes(2);
    expect(history().queryByRole("region", { name: "历史搜索结果" })).not.toBeInTheDocument();
  });

  it("keeps chat primary on desktop and supplies real recipients without opening members or reading hidden catalogues/tasks", async () => {
    render(<App />);
    expect(screen.queryByRole("navigation", { name: "工作区页面" })).not.toBeInTheDocument();
    expect(screen.queryByText(/先有讨论/)).not.toBeInTheDocument();
    selectGroup((await screen.findByRole("button", { name: group.name })).textContent!);
    await screen.findByText("尚无消息，可发送第一条讨论。");
    expect(within(main()).getByRole("heading", { name: group.name })).toHaveFocus();
    expect(content()).toHaveAttribute("rows", "2");
    fireEvent.change(screen.getByRole("combobox", { name: "接收方式" }), { target: { value: "selected" } });
    fireEvent.click(await screen.findByRole("checkbox", { name: recipientName }));
    expect(catalogue).not.toHaveBeenCalled(); expect(taskReads).not.toHaveBeenCalled(); expect(taskDetails).not.toHaveBeenCalled();
    showContext("成员");
    expect(within(context("成员")).getByRole("heading", { name: "成员" })).toHaveFocus();
    expect(context("成员")).toHaveTextContent(group.name);
    expect(await screen.findByRole("button", { name: member.name })).toBeVisible();
    expect(main()).toBeVisible(); expect(content()).toBeVisible();
    expect(messageReads.mock.calls[0][1]?.signal?.aborted).toBe(false);
    closeContext();
    expect(trigger("成员")).toHaveFocus(); expect(trigger("成员")).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("complementary", { name: "成员" })).not.toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: recipientName })).toBeChecked();
    expect(memberReads).toHaveBeenCalledTimes(1); expect(memberReads.mock.calls[0][1]?.signal?.aborted).toBe(false);
    expect(catalogue.mock.calls[0][1]?.signal?.aborted).toBe(true);
    showContext("任务"); selectGroup(other.name);
    expect(context("任务")).toHaveTextContent(other.name);
    expect(within(main()).getByRole("heading", { name: other.name })).toBeVisible();
    expect(taskReads.mock.calls.at(-1)?.[0]).toBe(`/api/v1/groups/${other.id}/tasks?limit=50`);
    expect(memberReads.mock.calls.at(-1)?.[0]).toBe(`/api/v1/groups/${other.id}/members?limit=50`);
    closeContext(); expect(trigger("任务")).toHaveFocus();
    await screen.findByText("尚无消息，可发送第一条讨论。");
  });

  it("keeps genuine no-group context states and creates tasks only from the current group's context", async () => {
    render(<App />); showContext("任务");
    expect(within(context("任务")).getByText("请先选择群组，再查看任务。")).toBeVisible();
    expect(main()).toBeVisible(); expect(taskReads).not.toHaveBeenCalled(); expect(taskWrites).not.toHaveBeenCalled();
    taskReads.mockResolvedValueOnce(page([secondTask]));
    taskDetails.mockResolvedValueOnce(json({ ...task, acceptance: "服务端确认的验收条件", revision: 2 }));
    selectGroup((await screen.findByRole("button", { name: group.name })).textContent!);
    expect(await tasks().findByRole("button", { name: `${secondTask.title}，待交付` })).toBeVisible();
    expect(taskReads.mock.calls[0][0]).toBe(`/api/v1/groups/${group.id}/tasks?limit=50`);
    expect(taskDetails).not.toHaveBeenCalled();
    draftTask(); fireEvent.submit(tasks().getByRole("form", { name: "创建任务" }));
    await tasks().findByRole("region", { name: "任务详情" });
    expect(await detail().findByText("服务端确认的验收条件")).toBeVisible();
    expect(detail().getByRole("heading", { name: task.title })).toBeVisible();
    expect(taskWrites).toHaveBeenCalledTimes(1);
    expect(identity(taskWrites.mock.calls[0]).url).toBe(`/api/v1/groups/${group.id}/tasks`);
    expect(JSON.parse(String(identity(taskWrites.mock.calls[0]).body))).toEqual(taskInput);
    expect(new Headers(taskWrites.mock.calls[0][1]?.headers).get("Content-Type")).toBe("application/json");
    expect(taskDetails.mock.calls[0][0]).toBe(`/api/v1/groups/${group.id}/tasks/${task.id}`);
    expect(messageReads.mock.calls[0][1]?.signal?.aborted).toBe(false);
  });

  it("aborts hidden task list/detail reads, ignores late results and reuses the mounted form on return", async () => {
    taskReads.mockResolvedValueOnce(page([task]));
    render(<App />); selectGroup((await screen.findByRole("button", { name: group.name })).textContent!); showContext("任务");
    fireEvent.click(await tasks().findByRole("button", { name: `${task.title}，待交付` }));
    await detail().findByRole("heading", { name: task.title });
    const form = tasks().getByRole("form", { name: "创建任务" });
    const oldList = deferred<Response>(), oldDetail = deferred<Response>();
    taskReads.mockReturnValueOnce(oldList.promise); taskDetails.mockReturnValueOnce(oldDetail.promise);
    fireEvent.click(tasks().getByRole("button", { name: "刷新任务" })); closeContext();
    expect(taskReads.mock.calls[1][1]?.signal?.aborted).toBe(true);
    expect(taskDetails.mock.calls[1][1]?.signal?.aborted).toBe(true);
    expect(form).toBeInTheDocument(); expect(form).not.toBeVisible(); expect(form.closest("[hidden]")).not.toBeNull();
    expect(screen.queryByRole("region", { name: "群组任务" })).not.toBeInTheDocument();
    showContext("成员"); closeContext();
    expect(taskReads).toHaveBeenCalledTimes(2); expect(taskDetails).toHaveBeenCalledTimes(2);
    taskReads.mockResolvedValueOnce(page([acceptedTask]));
    taskDetails.mockResolvedValueOnce(json({ ...acceptedTask, acceptance: "返回后重新读取的详情", revision: 2 }));
    showContext("任务");
    expect(await tasks().findByRole("button", { name: `${task.title}，已接纳` })).toBeVisible();
    expect(await detail().findByText("返回后重新读取的详情")).toBeVisible();
    expect(tasks().getByRole("form", { name: "创建任务" })).toBe(form);
    await act(async () => { oldList.resolve(page([{ ...task, title: "废弃的列表快照" }])); oldDetail.resolve(json(task)); });
    expect(detail().getByText("返回后重新读取的详情")).toBeVisible();
    expect(tasks().queryByRole("button", { name: "废弃的列表快照，待交付" })).not.toBeInTheDocument();
  });

  it("stops only the hidden catalogue, ignores its old result and continues the current roster into directed drafts", async () => {
    const roster = deferred<Response>(), oldCatalogue = deferred<Response>();
    memberReads.mockReturnValueOnce(roster.promise); catalogue.mockReturnValueOnce(oldCatalogue.promise);
    render(<App />); selectGroup((await screen.findByRole("button", { name: group.name })).textContent!);
    openMemberForm(); const form = screen.getByRole("form", { name: "创建成员" }); closeContext();
    expect(catalogue.mock.calls[0][1]?.signal?.aborted).toBe(true);
    expect(memberReads.mock.calls[0][1]?.signal?.aborted).toBe(false);
    await act(async () => roster.resolve(page([member])));
    fireEvent.change(screen.getByRole("combobox", { name: "接收方式" }), { target: { value: "selected" } });
    fireEvent.click(await screen.findByRole("checkbox", { name: recipientName })); change(content(), "给指定成员的草稿");
    openMemberForm(); await screen.findByRole("option", { name: /pi.*账户已登录/ });
    await act(async () => oldCatalogue.resolve(page([{ ...provider, id: "stale", name: "过期运行器" }])));
    expect(screen.queryByRole("option", { name: /过期运行器/ })).not.toBeInTheDocument();
    expect(screen.getByRole("form", { name: "创建成员" })).toBe(form); closeContext();
    expect(content()).toHaveValue("给指定成员的草稿"); expect(screen.getByRole("checkbox", { name: recipientName })).toBeChecked();
    expect(memberReads).toHaveBeenCalledTimes(1); expect(catalogue).toHaveBeenCalledTimes(2);
  });

  it("starts mobile at the list, exposes only the active pane and restores focus through chat/details/list", async () => {
    resize(true); render(<App />);
    const selectedButton = await screen.findByRole("button", { name: group.name });
    expect(screen.getByRole("complementary", { name: "群组" })).toBeVisible();
    expect(screen.queryByRole("main")).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "跳至群组列表" })).toHaveAttribute("href", "#im-groups");
    openCreateGroup(); const groupDraft = nameInput(); change(groupDraft, "列表草稿");
    const oldGroups = deferred<Response>(), oldMessages = deferred<Response>();
    transport.mockReturnValueOnce(oldGroups.promise); messageReads.mockReturnValueOnce(oldMessages.promise);
    fireEvent.click(screen.getByRole("button", { name: "刷新群组" })); fireEvent.click(selectedButton);
    const chat = main(), heading = within(chat).getByRole("heading", { name: group.name });
    expect(heading).toHaveFocus(); expect(groupDraft).toBeInTheDocument(); expect(groupDraft).not.toBeVisible();
    expect(screen.queryByRole("complementary", { name: "群组" })).not.toBeInTheDocument();
    expect(transport.mock.calls[1][1]?.signal?.aborted).toBe(true);
    const taskTrigger = trigger("任务"); showContext("任务");
    expect(chat).not.toBeVisible(); expect(screen.queryByRole("main")).not.toBeInTheDocument();
    expect(screen.queryByRole("textbox", { name: "消息内容" })).not.toBeInTheDocument();
    expect(document.getElementById(context("任务").getAttribute("aria-labelledby")!)).toHaveFocus();
    expect(screen.getByRole("link", { name: "跳至任务详情" })).toHaveAttribute("href", `#${context("任务").id}`);
    expect(messageReads.mock.calls[0][1]?.signal?.aborted).toBe(true);
    expect(memberReads.mock.calls[0][1]?.signal?.aborted).toBe(false);
    closeContext(); expect(taskTrigger).toHaveFocus();
    await screen.findByText("尚无消息，可发送第一条讨论。");
    await act(async () => { oldGroups.resolve(page([{ ...other, id: "obsolete", name: "废弃群组" }])); oldMessages.resolve(page([message])); });
    expect(screen.queryByRole("article")).not.toBeInTheDocument();
    expect(transport).toHaveBeenCalledTimes(2);
    fireEvent.click(screen.getByRole("button", { name: "返回群组" }));
    expect(selectedButton).toHaveFocus(); expect(chat).not.toBeVisible(); expect(screen.queryByRole("main")).not.toBeInTheDocument();
    expect(nameInput()).toBe(groupDraft); expect(nameInput()).toHaveValue("列表草稿");
    expect(screen.queryByRole("button", { name: "废弃群组" })).not.toBeInTheDocument();
    expect(transport).toHaveBeenCalledTimes(3); expect(taskReads).toHaveBeenCalledTimes(1);
    fireEvent.click(selectedButton); expect(heading).toHaveFocus(); expect(main()).toBe(chat);
    await screen.findByText("尚无消息，可发送第一条讨论。");
  });

  it("realigns read activity and focus across breakpoint changes without stranding focus in hidden panes", async () => {
    render(<App />); selectGroup((await screen.findByRole("button", { name: group.name })).textContent!);
    const refresh = screen.getByRole("button", { name: "刷新群组" }); refresh.focus();
    resize(true);
    expect(within(main()).getByRole("heading", { name: group.name })).toHaveFocus();
    expect(refresh.closest("[hidden]")).not.toBeNull(); expect(transport.mock.calls[0][1]?.signal?.aborted).toBe(true);
    showContext("成员"); const heading = within(context("成员")).getByRole("heading", { name: "成员" });
    expect(heading).toHaveFocus(); expect(messageReads.mock.calls[0][1]?.signal?.aborted).toBe(true);
    resize(false); expect(heading).toHaveFocus(); expect(main()).toBeVisible(); expect(refresh).toBeVisible();
    expect(transport).toHaveBeenCalledTimes(2); expect(messageReads).toHaveBeenCalledTimes(2);
    closeContext(); expect(trigger("成员")).toHaveFocus(); resize(true);
    fireEvent.click(screen.getByRole("button", { name: "返回群组" }));
    resize(false); content().focus(); resize(true);
    expect(screen.getByRole("button", { name: group.name })).toHaveFocus();
    expect(screen.queryByRole("main")).not.toBeInTheDocument();
    await screen.findByRole("button", { name: other.name });
  });

  it.each([false, true])("preserves composer DOM, directed drafts and a pending/unknown send identity (mobile=%s)", async mobile => {
    resize(mobile); render(<App />); selectGroup((await screen.findByRole("button", { name: group.name })).textContent!);
    await screen.findByText("尚无消息，可发送第一条讨论。");
    const originalInput = content(), composer = screen.getByRole("region", { name: "撰写消息" });
    const raw = "  切换期间保留\n原始消息  "; change(originalInput, raw);
    fireEvent.change(screen.getByRole("combobox", { name: "接收方式" }), { target: { value: "selected" } });
    fireEvent.click(await screen.findByRole("checkbox", { name: recipientName }));
    showContext("任务"); showContext("搜索"); expect(composer).toBeInTheDocument(); expect(composer.closest("[hidden]") !== null).toBe(mobile);
    closeContext(); selectGroup(other.name); await screen.findByText("尚无消息，可发送第一条讨论。");
    change(content(), "写作群草稿"); selectGroup(group.name);
    expect(content()).toBe(originalInput); expect(content()).toHaveValue(raw); expect(content()).toHaveAttribute("rows", "2");
    expect(await screen.findByRole("checkbox", { name: recipientName })).toBeChecked();
    const pending = deferred<Response>(); messageWrites.mockReturnValueOnce(pending.promise);
    fireEvent.submit(screen.getByRole("form", { name: "发送消息" })); showContext("任务"); showContext("搜索");
    selectGroup(other.name); if (!mobile) closeContext();
    expect(content()).toHaveValue("写作群草稿"); expect(messageWrites.mock.calls[0][1]?.signal?.aborted).toBe(false);
    await act(async () => pending.resolve(json({}, 503)));
    expect(within(main()).queryByRole("alert")).not.toBeInTheDocument(); selectGroup(group.name);
    expect(content()).toBe(originalInput); expect(content()).toHaveValue(raw); expect(content()).toBeDisabled();
    expect(screen.getByRole("region", { name: "撰写消息" })).toBe(composer);
    expect(screen.getByRole("alert")).toHaveTextContent("发送结果未确认");
    showContext("搜索"); closeContext(); expect(content()).toBe(originalInput); expect(content()).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "重试同一次发送" }));
    await screen.findByText("消息已持久化；投递状态见记录。");
    expect(messageWrites).toHaveBeenCalledTimes(2); expect(identity(messageWrites.mock.calls[1])).toEqual(identity(messageWrites.mock.calls[0]));
    expect(identity(messageWrites.mock.calls[0]).url).toBe(`/api/v1/groups/${group.id}/messages`);
    expect(JSON.parse(String(identity(messageWrites.mock.calls[0]).body))).toEqual({ content: raw, recipient_ids: [member.id] });
    expect(identity(messageWrites.mock.calls[0]).key).toMatch(/^[\da-f-]{36}$/);
    expect(content()).toBe(originalInput); expect(content()).toHaveValue("");
    selectGroup(other.name); expect(content()).toHaveValue("写作群草稿");
    await screen.findByText("尚无消息，可发送第一条讨论。");
  });

  it.each([false, true])("preserves task DOM, per-group drafts and pending/unknown POST while hidden (mobile=%s)", async mobile => {
    resize(mobile); render(<App />); selectGroup((await screen.findByRole("button", { name: group.name })).textContent!);
    showContext("任务"); await tasks().findByText("尚无任务，可创建任务并约定验收条件。"); draftTask();
    const originalTitle = taskField("任务标题"), form = tasks().getByRole("form", { name: "创建任务" });
    const pending = deferred<Response>(); taskWrites.mockReturnValueOnce(pending.promise); fireEvent.submit(form);
    showContext("搜索"); expect(form).toBeInTheDocument(); expect(form).not.toBeVisible();
    showContext("任务"); expect(taskField("任务标题")).toBe(originalTitle); expect(originalTitle).toBeDisabled();
    selectGroup(other.name); showContext("任务"); await tasks().findByText("尚无任务，可创建任务并约定验收条件。");
    expect(taskField("任务标题")).toHaveValue(""); change(taskField("任务标题"), "写作群的任务草稿"); showContext("搜索"); closeContext();
    await act(async () => pending.resolve(json({}, 503)));
    expect(taskWrites).toHaveBeenCalledTimes(1); expect(taskWrites.mock.calls[0][1]?.signal?.aborted).toBe(false);
    expect(within(main()).queryByRole("alert")).not.toBeInTheDocument();
    selectGroup(group.name); showContext("任务");
    expect(taskField("任务标题")).toBe(originalTitle); expect(originalTitle).toHaveValue(task.title); expect(originalTitle).toBeDisabled();
    expect(taskField("任务描述（可选）")).toHaveValue(task.description); expect(taskField("验收条件")).toHaveValue(task.acceptance);
    expect(tasks().getByRole("alert")).toHaveTextContent("创建结果未确认");
    showContext("搜索"); showContext("任务"); expect(taskField("任务标题")).toBe(originalTitle); expect(originalTitle).toBeDisabled();
    fireEvent.click(tasks().getByRole("button", { name: "重试同一次任务创建" }));
    await tasks().findByRole("region", { name: "任务详情" }); await detail().findByRole("heading", { name: task.title });
    expect(taskWrites).toHaveBeenCalledTimes(2); expect(identity(taskWrites.mock.calls[1])).toEqual(identity(taskWrites.mock.calls[0]));
    expect(JSON.parse(String(identity(taskWrites.mock.calls[0]).body))).toEqual(taskInput);
    expect(identity(taskWrites.mock.calls[0]).key).toMatch(/^[\da-f-]{36}$/);
    expect(originalTitle).toBeEnabled(); expect(originalTitle).toHaveValue("");
    selectGroup(other.name); showContext("任务"); expect(taskField("任务标题")).toHaveValue("写作群的任务草稿");
    expect(tasks().queryByRole("button", { name: `${task.title}，待交付` })).not.toBeInTheDocument();
    await tasks().findByText("尚无任务，可创建任务并约定验收条件。");
  });

  it.each([false, true])("preserves member creation and catalogue configuration through hidden pending/unknown POST (mobile=%s)", async mobile => {
    memberReads.mockImplementation(async () => page());
    resize(mobile); render(<App />); selectGroup((await screen.findByRole("button", { name: group.name })).textContent!); openMemberForm();
    await screen.findByRole("option", { name: /pi.*账户已登录/ });
    const field = screen.getByRole("textbox", { name: "成员名称" }), form = screen.getByRole("form", { name: "创建成员" });
    change(field, member.name); change(screen.getByRole("textbox", { name: "成员职责" }), member.role);
    fireEvent.change(screen.getByRole("combobox", { name: "运行器（可选）" }), { target: { value: provider.id } });
    const pending = deferred<Response>(); memberWrites.mockReturnValueOnce(pending.promise); fireEvent.submit(form); showContext("搜索");
    expect(form).toBeInTheDocument(); expect(form).not.toBeVisible(); expect(memberWrites.mock.calls[0][1]?.signal?.aborted).toBe(false);
    selectGroup(other.name); openMemberForm(); change(screen.getByRole("textbox", { name: "成员名称" }), "B 的成员草稿"); showContext("搜索"); closeContext();
    await act(async () => pending.resolve(json({}, 503)));
    selectGroup(group.name); openMemberForm();
    expect(screen.getByRole("form", { name: "创建成员" })).toBe(form); expect(field).toHaveValue(member.name); expect(field).toBeDisabled();
    expect(screen.getByRole("combobox", { name: "运行器（可选）" })).toHaveValue(provider.id);
    expect(screen.getByRole("alert")).toHaveTextContent("创建结果未确认");
    showContext("搜索"); openMemberForm(); expect(screen.getByRole("form", { name: "创建成员" })).toBe(form); expect(field).toBeDisabled();
    memberWrites.mockResolvedValueOnce(json({ ...member, provider_id: provider.id }, 201));
    fireEvent.click(screen.getByRole("button", { name: "重试同一次成员创建" })); await screen.findByRole("button", { name: member.name });
    expect(memberWrites).toHaveBeenCalledTimes(2); expect(identity(memberWrites.mock.calls[1])).toEqual(identity(memberWrites.mock.calls[0]));
    expect(JSON.parse(String(identity(memberWrites.mock.calls[0]).body))).toEqual({ name: member.name, role: member.role, provider_id: provider.id });
    expect(identity(memberWrites.mock.calls[0]).key).toMatch(/^[\da-f-]{36}$/);
    expect(field).toHaveValue(""); closeContext();
    fireEvent.change(screen.getByRole("combobox", { name: "接收方式" }), { target: { value: "selected" } });
    expect(await screen.findByRole("checkbox", { name: recipientName })).toBeVisible();
    selectGroup(other.name); openMemberForm(); expect(screen.getByRole("textbox", { name: "成员名称" })).toHaveValue("B 的成员草稿");
    await screen.findByText("尚无成员，可创建群内成员。");
  });

  it.each(["selection", "context", "search", "mobile"])("inserts a late created group without hijacking a deliberate %s change or focus", async mode => {
    resize(mode === "mobile"); render(<App />); await screen.findByRole("button", { name: group.name });
    if (mode !== "mobile") selectGroup(group.name);
    const created = { ...group, id: "created", name: "新群", description: "" }, receipt = deferred<Response>();
    transport.mockReturnValueOnce(receipt.promise); submit(created.name);
    const originalInput = nameInput();
    if (mode === "context" || mode === "search") showContext(mode === "search" ? "搜索" : "任务");
    else { selectGroup(other.name); if (mode === "mobile") { showContext("成员"); showContext("搜索"); } }
    const focused = document.activeElement;
    expect(posts()[0][1]?.signal?.aborted).toBe(false);
    await act(async () => receipt.resolve(json(created, 201)));
    expect(document.activeElement).toBe(focused);
    if (mode === "mobile") {
      expect(context("搜索")).toHaveTextContent(other.name); closeContext();
      fireEvent.click(screen.getByRole("button", { name: "返回群组" }));
    } else expect(within(main()).getByRole("heading", { name: mode === "context" || mode === "search" ? group.name : other.name })).toBeVisible();
    expect(screen.getByRole("button", { name: created.name })).toHaveAttribute("aria-pressed", "false");
    expect(nameInput()).toBe(originalInput); expect(originalInput).toHaveValue(""); expect(originalInput).toBeEnabled();
    fireEvent.click(screen.getByRole("button", { name: created.name }));
    expect(within(main()).getByRole("heading", { name: created.name })).toHaveFocus();
    if (mode !== "mobile") { selectGroup(created.name); expect(within(main()).getByRole("heading", { name: created.name })).toHaveFocus(); }
    await screen.findByText("尚无消息，可发送第一条讨论。");
  });

  it("does not auto-open a late created group after a breakpoint moves the user to the mobile list", async () => {
    render(<App />); await screen.findByRole("button", { name: group.name }); showContext("任务");
    const receipt = deferred<Response>(), created = { ...group, id: "created", name: "断点前创建", description: "" };
    transport.mockReturnValueOnce(receipt.promise); submit(created.name);
    resize(true); screen.getByRole("button", { name: "刷新群组" }).focus();
    const focused = document.activeElement;
    await act(async () => receipt.resolve(json(created, 201)));
    expect(document.activeElement).toBe(focused);
    expect(screen.queryByRole("main")).not.toBeInTheDocument();
    expect(screen.queryByRole("complementary", { name: "任务" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: created.name })).toHaveAttribute("aria-pressed", "false");
    expect(taskReads).not.toHaveBeenCalled(); expect(nameInput()).toHaveValue("");
  });

  it("keeps the original unknown group creation across mobile panes and retries without reopening chat or stealing focus", async () => {
    resize(true); render(<App />); await screen.findByRole("button", { name: group.name });
    const pending = deferred<Response>(); transport.mockReturnValueOnce(pending.promise);
    submit("保留的群组操作"); const field = nameInput();
    selectGroup(other.name); showContext("搜索");
    expect(posts()[0][1]?.signal?.aborted).toBe(false);
    await act(async () => pending.resolve(json({}, 503)));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument(); closeContext();
    fireEvent.click(screen.getByRole("button", { name: "返回群组" })); await screen.findByRole("alert");
    expect(nameInput()).toBe(field); expect(field).toBeDisabled(); expect(field).toHaveValue("保留的群组操作");
    const created = { ...group, id: "created", name: "保留的群组操作", description: "" };
    transport.mockResolvedValueOnce(json(created, 201));
    const retry = screen.getByRole("button", { name: "重试同一次创建" }); retry.focus(); fireEvent.click(retry);
    await screen.findByRole("button", { name: created.name });
    expect(retry).toHaveFocus(); expect(screen.queryByRole("main")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: other.name })).toHaveAttribute("aria-pressed", "true");
    expect(identity(posts()[1])).toEqual(identity(posts()[0])); expect(posts()).toHaveLength(2);
    expect(field).toBeEnabled(); expect(field).toHaveValue("");
    expect(screen.getByRole("button", { name: created.name })).toHaveAttribute("aria-pressed", "false");
  });
});
