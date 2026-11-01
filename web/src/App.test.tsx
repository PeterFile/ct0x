import { StrictMode } from "react";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { deferred, group, json, other, page } from "./test/groups";
import { member } from "./test/members";
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
beforeEach(() => {
  transport.mockReset();
  transport.mockImplementation(async () => page());
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) =>
    String(input).includes("/members") || String(input).includes("/messages") || String(input).startsWith("/api/v1/providers")
      ? Promise.resolve(page()) : transport(input, init));
});
afterEach(() => vi.unstubAllGlobals());

describe("workspace groups", () => {
  it("names the workspace and its three semantic columns", async () => {
    render(<App />);
    expect(screen.getByRole("banner")).toContainElement(
      screen.getByRole("heading", { level: 1, name: "ct0x" }),
    );
    expect(screen.getByRole("complementary", { name: "群组" })).toBeVisible();
    expect(screen.getByRole("main", { name: "讨论" })).toBeVisible();
    expect(screen.getByRole("complementary", { name: "成员" })).toBeVisible();
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
    expect(within(screen.getByRole("complementary", { name: "成员" })).queryByRole("button")).not.toBeInTheDocument();
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

  it("submits JSON once while pending and selects only the returned server group", async () => {
    render(<App />);
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
    expect(within(screen.getByRole("main")).queryByText(group.name)).not.toBeInTheDocument();
    await act(async () => response.resolve(json(group, 201)));
    expect(within(screen.getByRole("main")).getByRole("heading", { name: group.name })).toBeVisible();
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
    transport.mockResolvedValueOnce(json(group, 201));
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
    transport.mockResolvedValueOnce(json(other, 201));
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
    transport.mockReturnValueOnce(old.promise).mockResolvedValueOnce(json(group, 201));
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

describe("workspace navigation", () => {
  const taskReads = vi.fn<typeof fetch>(), taskDetails = vi.fn<typeof fetch>(), taskWrites = vi.fn<typeof fetch>();
  const messageReads = vi.fn<typeof fetch>(), messageWrites = vi.fn<typeof fetch>(), memberReads = vi.fn<typeof fetch>();
  const main = () => screen.getByRole("main");
  const navigation = () => within(screen.getByRole("navigation", { name: "工作区页面" }));
  const navigate = (name: "讨论" | "任务") => fireEvent.click(navigation().getByRole("button", { name }));
  const selectGroup = (name: string) => fireEvent.click(screen.getByRole("button", { name }));
  const tasks = () => within(screen.getByRole("region", { name: "群组任务" }));
  const detail = () => within(tasks().getByRole("region", { name: "任务详情" }));
  const taskField = (name: string) => tasks().getByRole("textbox", { name });
  const content = () => screen.getByRole("textbox", { name: "消息内容" });
  const change = (field: HTMLElement, value: string) => fireEvent.change(field, { target: { value } });
  const identity = ([url, init]: Parameters<typeof fetch>) => ({ url, body: init?.body,
    key: new Headers(init?.headers).get("Idempotency-Key") });
  function draftTask() {
    change(taskField("任务标题"), task.title);
    change(taskField("任务描述（可选）"), task.description);
    change(taskField("验收条件"), task.acceptance);
  }

  beforeEach(() => {
    transport.mockImplementation(async () => page([group, other]));
    taskReads.mockReset().mockImplementation(async () => page());
    taskDetails.mockReset().mockImplementation(async () => json(task));
    taskWrites.mockReset().mockImplementation(async () => json(task, 201));
    messageReads.mockReset().mockImplementation(async () => page());
    messageWrites.mockReset().mockImplementation(async (_url, init) =>
      json({ ...message, content: JSON.parse(String(init?.body)).content, deliveries: [] }, 201));
    memberReads.mockReset().mockImplementation(async url => page(String(url).includes(other.id) ? [] : [member]));
    vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.includes("/tasks")) return (init?.method === "POST" ? taskWrites : /\/tasks\//.test(url) ? taskDetails : taskReads)(input, init);
      if (url.includes("/messages")) return (init?.method === "POST" ? messageWrites : messageReads)(input, init);
      if (url.includes("/members")) return memberReads(input, init);
      if (url.startsWith("/api/v1/providers")) return Promise.resolve(page());
      if (url.startsWith("/api/v1/groups?")) return transport(input, init);
      return Promise.reject(new Error(`Unexpected request: ${url}`));
    });
  });

  it("defaults to an accessible discussion page without reading tasks, even after group selection", async () => {
    render(<App />);
    expect(navigation().getByRole("button", { name: "讨论" })).toHaveAttribute("aria-pressed", "true");
    expect(navigation().getByRole("button", { name: "任务" })).toHaveAttribute("aria-pressed", "false");
    const heading = screen.getByRole("heading", { level: 2, name: "讨论" });
    expect(screen.getByRole("main", { name: "讨论" })).toHaveAttribute("aria-labelledby", heading.id);
    expect(screen.getByRole("link", { name: "跳至讨论区" })).toHaveAttribute("href", `#${main().id}`);
    expect(main()).toHaveAttribute("tabindex", "-1");
    main().focus(); expect(main()).toHaveFocus();
    expect(screen.getByRole("banner")).toHaveTextContent("群组工作台 · 群内讨论");
    expect(taskReads).not.toHaveBeenCalled(); expect(taskDetails).not.toHaveBeenCalled();
    selectGroup((await screen.findByRole("button", { name: group.name })).textContent!);
    await screen.findByText("尚无消息，可发送第一条讨论。");
    expect(screen.getByRole("complementary", { name: "群组" })).toBeVisible();
    expect(screen.getByRole("complementary", { name: "成员" })).toContainElement(await screen.findByRole("button", { name: member.name }));
    expect(content()).toHaveAttribute("rows", "2");
    expect(taskReads).not.toHaveBeenCalled(); expect(taskDetails).not.toHaveBeenCalled(); expect(taskWrites).not.toHaveBeenCalled();
  });

  it("shows the real no-group tasks state, then lists, creates and reads server-backed task detail", async () => {
    render(<App />); navigate("任务");
    expect(screen.getByRole("main", { name: "任务" })).toBeVisible();
    const heading = screen.getByRole("heading", { level: 2, name: "任务" });
    expect(main()).toHaveAttribute("aria-labelledby", heading.id);
    expect(screen.getByRole("link", { name: "跳至任务区" })).toHaveAttribute("href", `#${main().id}`);
    expect(main()).toHaveAttribute("tabindex", "-1"); main().focus(); expect(main()).toHaveFocus();
    expect(screen.getByRole("banner")).toHaveTextContent("群组工作台 · 群组任务");
    expect(navigation().getByRole("button", { name: "任务" })).toHaveAttribute("aria-pressed", "true");
    expect(navigation().getByRole("button", { name: "讨论" })).toHaveAttribute("aria-pressed", "false");
    expect(within(main()).getByText("请先选择群组，再查看任务。")).toBeVisible();
    expect(screen.queryByRole("heading", { name: "讨论" })).not.toBeInTheDocument();
    expect(within(main()).queryByRole("form")).not.toBeInTheDocument();
    expect(taskReads).not.toHaveBeenCalled(); expect(taskDetails).not.toHaveBeenCalled(); expect(taskWrites).not.toHaveBeenCalled();
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
    expect(tasks().getByRole("button", { name: `${task.title}，待交付` })).toHaveAttribute("aria-pressed", "true");
    expect(taskWrites).toHaveBeenCalledTimes(1);
    expect(identity(taskWrites.mock.calls[0]).url).toBe(`/api/v1/groups/${group.id}/tasks`);
    expect(JSON.parse(String(identity(taskWrites.mock.calls[0]).body))).toEqual(taskInput);
    expect(new Headers(taskWrites.mock.calls[0][1]?.headers).get("Content-Type")).toBe("application/json");
    expect(taskDetails.mock.calls[0][0]).toBe(`/api/v1/groups/${group.id}/tasks/${task.id}`);
    expect(messageReads).not.toHaveBeenCalled();
  });

  it("aborts inactive reads, hides pages accessibly, keeps side columns usable and re-reads tasks on return", async () => {
    const oldMessages = deferred<Response>(); messageReads.mockReturnValueOnce(oldMessages.promise);
    taskReads.mockResolvedValueOnce(page([task]));
    render(<App />); selectGroup((await screen.findByRole("button", { name: group.name })).textContent!);
    navigate("任务"); expect(messageReads.mock.calls[0][1]?.signal?.aborted).toBe(true);
    fireEvent.click(await tasks().findByRole("button", { name: `${task.title}，待交付` }));
    await detail().findByRole("heading", { name: task.title });
    const form = tasks().getByRole("form", { name: "创建任务" });
    expect(screen.queryByRole("region", { name: "消息记录" })).not.toBeInTheDocument();
    selectGroup((await screen.findByRole("button", { name: member.name })).textContent!);
    expect(screen.getByRole("region", { name: "成员详情" })).toHaveTextContent(member.role);
    fireEvent.click(screen.getByRole("button", { name: "刷新成员" }));
    await screen.findByRole("button", { name: member.name }); expect(memberReads).toHaveBeenCalledTimes(2);
    fireEvent.click(screen.getByRole("button", { name: "刷新群组" }));
    await screen.findByRole("button", { name: group.name }); expect(transport).toHaveBeenCalledTimes(2);
    expect(screen.getByRole("button", { name: group.name })).toHaveAttribute("aria-pressed", "true");
    const oldList = deferred<Response>(), oldDetail = deferred<Response>();
    taskReads.mockReturnValueOnce(oldList.promise); taskDetails.mockReturnValueOnce(oldDetail.promise);
    fireEvent.click(tasks().getByRole("button", { name: "刷新任务" }));
    expect(taskReads.mock.calls[1][1]?.signal?.aborted).toBe(false);
    expect(taskDetails.mock.calls[1][1]?.signal?.aborted).toBe(false);
    navigate("讨论");
    expect(taskReads.mock.calls[1][1]?.signal?.aborted).toBe(true);
    expect(taskDetails.mock.calls[1][1]?.signal?.aborted).toBe(true);
    expect(form).toBeInTheDocument(); expect(form).not.toBeVisible();
    expect(screen.queryByRole("region", { name: "群组任务" })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "任务" })).not.toBeInTheDocument();
    await screen.findByText("尚无消息，可发送第一条讨论。");
    expect(taskReads).toHaveBeenCalledTimes(2); expect(taskDetails).toHaveBeenCalledTimes(2);
    taskReads.mockResolvedValueOnce(page([acceptedTask]));
    taskDetails.mockResolvedValueOnce(json({ ...acceptedTask, acceptance: "返回后重新读取的详情", revision: 2 }));
    navigate("任务");
    expect(await tasks().findByRole("button", { name: `${task.title}，已接纳` })).toBeVisible();
    expect(await detail().findByText("返回后重新读取的详情")).toBeVisible();
    expect(tasks().getByRole("form", { name: "创建任务" })).toBe(form);
    expect(taskReads).toHaveBeenCalledTimes(3); expect(taskDetails).toHaveBeenCalledTimes(3);
    await act(async () => {
      oldMessages.resolve(page([message])); oldList.resolve(page([{ ...task, title: "废弃的列表快照" }])); oldDetail.resolve(json(task));
    });
    expect(detail().getByText("返回后重新读取的详情")).toBeVisible();
    expect(tasks().queryByRole("button", { name: "废弃的列表快照，待交付" })).not.toBeInTheDocument();
  });

  it("keeps the compact discussion composer, per-group drafts and an unknown send's original retry identity", async () => {
    render(<App />); selectGroup((await screen.findByRole("button", { name: group.name })).textContent!);
    await screen.findByText("尚无消息，可发送第一条讨论。");
    const originalInput = content(), composer = screen.getByRole("region", { name: "撰写消息" });
    const raw = "  导航期间保留\n原始消息  "; change(originalInput, raw);
    fireEvent.change(screen.getByRole("combobox", { name: "接收方式" }), { target: { value: "selected" } });
    const recipientName = `${member.name} · ${member.role} · ${member.id}`;
    fireEvent.click(await screen.findByRole("checkbox", { name: recipientName }));
    navigate("任务");
    expect(composer).toBeInTheDocument(); expect(composer).not.toBeVisible();
    expect(screen.queryByRole("textbox", { name: "消息内容" })).not.toBeInTheDocument();
    selectGroup(other.name); await tasks().findByText("尚无任务，可创建任务并约定验收条件。");
    expect(messageReads).toHaveBeenCalledTimes(1);
    navigate("讨论"); await screen.findByText("尚无消息，可发送第一条讨论。");
    expect(content()).toHaveValue(""); change(content(), "写作群草稿");
    selectGroup(group.name);
    expect(content()).toBe(originalInput); expect(content()).toHaveValue(raw); expect(content()).toHaveAttribute("rows", "2");
    expect(await screen.findByRole("checkbox", { name: recipientName })).toBeChecked();
    const pending = deferred<Response>(); messageWrites.mockReturnValueOnce(pending.promise);
    fireEvent.submit(screen.getByRole("form", { name: "发送消息" }));
    expect(content()).toBeDisabled(); navigate("任务"); selectGroup(other.name); navigate("讨论");
    expect(content()).toHaveValue("写作群草稿"); expect(messageWrites).toHaveBeenCalledTimes(1);
    expect(messageWrites.mock.calls[0][1]?.signal?.aborted).toBe(false);
    await act(async () => pending.resolve(json({}, 503)));
    expect(within(main()).queryByRole("alert")).not.toBeInTheDocument(); expect(content()).toBeEnabled();
    navigate("任务"); selectGroup(group.name); navigate("讨论");
    expect(content()).toBe(originalInput); expect(content()).toHaveValue(raw); expect(content()).toBeDisabled();
    expect(screen.getByRole("region", { name: "撰写消息" })).toBe(composer);
    expect(screen.getByRole("alert")).toHaveTextContent("发送结果未确认");
    fireEvent.click(screen.getByRole("button", { name: "重试同一次发送" }));
    await screen.findByText("消息已持久化；投递状态见记录。");
    expect(messageWrites).toHaveBeenCalledTimes(2);
    expect(identity(messageWrites.mock.calls[1])).toEqual(identity(messageWrites.mock.calls[0]));
    expect(identity(messageWrites.mock.calls[0]).url).toBe(`/api/v1/groups/${group.id}/messages`);
    expect(JSON.parse(String(identity(messageWrites.mock.calls[0]).body))).toEqual({ content: raw, recipient_ids: [member.id] });
    expect(identity(messageWrites.mock.calls[0]).key).toMatch(/^[\da-f-]{36}$/);
    expect(content()).toBe(originalInput); expect(content()).toHaveValue("");
    selectGroup(other.name); expect(content()).toHaveValue("写作群草稿");
    await screen.findByText("尚无消息，可发送第一条讨论。");
  });

  it("keeps pending then unknown task creation across pages and groups without aborting or changing its operation", async () => {
    render(<App />); selectGroup((await screen.findByRole("button", { name: group.name })).textContent!);
    navigate("任务"); await tasks().findByText("尚无任务，可创建任务并约定验收条件。");
    draftTask(); const originalTitle = taskField("任务标题"), form = tasks().getByRole("form", { name: "创建任务" });
    const pending = deferred<Response>(); taskWrites.mockReturnValueOnce(pending.promise); fireEvent.submit(form);
    expect(tasks().getByRole("button", { name: "正在创建任务…" })).toBeDisabled();
    navigate("讨论"); expect(form).toBeInTheDocument(); expect(form).not.toBeVisible();
    navigate("任务");
    expect(taskField("任务标题")).toBe(originalTitle); expect(taskField("任务标题")).toBeDisabled();
    expect(taskWrites).toHaveBeenCalledTimes(1); expect(taskWrites.mock.calls[0][1]?.signal?.aborted).toBe(false);
    selectGroup(other.name); await tasks().findByText("尚无任务，可创建任务并约定验收条件。");
    expect(taskField("任务标题")).toHaveValue(""); change(taskField("任务标题"), "写作群的任务草稿");
    navigate("讨论"); await screen.findByText("尚无消息，可发送第一条讨论。");
    await act(async () => pending.resolve(json({}, 503)));
    expect(within(main()).queryByRole("alert")).not.toBeInTheDocument();
    expect(taskWrites.mock.calls[0][1]?.signal?.aborted).toBe(false);
    navigate("任务");
    expect(taskField("任务标题")).toHaveValue("写作群的任务草稿"); expect(taskField("任务标题")).toBeEnabled();
    expect(tasks().queryByRole("alert")).not.toBeInTheDocument(); selectGroup(group.name);
    expect(taskField("任务标题")).toBe(originalTitle); expect(taskField("任务标题")).toHaveValue(task.title);
    expect(taskField("任务描述（可选）")).toHaveValue(task.description); expect(taskField("验收条件")).toHaveValue(task.acceptance);
    expect(taskField("任务标题")).toBeDisabled(); expect(tasks().getByRole("alert")).toHaveTextContent("创建结果未确认");
    navigate("讨论"); navigate("任务");
    expect(taskField("任务标题")).toBe(originalTitle); expect(taskField("验收条件")).toBeDisabled();
    fireEvent.click(tasks().getByRole("button", { name: "重试同一次任务创建" }));
    await tasks().findByRole("region", { name: "任务详情" });
    expect(await detail().findByRole("heading", { name: task.title })).toBeVisible();
    expect(taskWrites).toHaveBeenCalledTimes(2); expect(identity(taskWrites.mock.calls[1])).toEqual(identity(taskWrites.mock.calls[0]));
    expect(identity(taskWrites.mock.calls[0]).url).toBe(`/api/v1/groups/${group.id}/tasks`);
    expect(JSON.parse(String(identity(taskWrites.mock.calls[0]).body))).toEqual(taskInput);
    expect(identity(taskWrites.mock.calls[0]).key).toMatch(/^[\da-f-]{36}$/);
    expect(taskField("任务标题")).toBeEnabled(); expect(taskField("任务标题")).toHaveValue("");
    selectGroup(other.name); expect(taskField("任务标题")).toHaveValue("写作群的任务草稿");
    expect(tasks().queryByRole("button", { name: `${task.title}，待交付` })).not.toBeInTheDocument();
    await tasks().findByText("尚无任务，可创建任务并约定验收条件。");
  });
});
