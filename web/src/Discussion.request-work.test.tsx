import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { Message } from "./api";
import type { AdmissionReceipt, RequestWorkInput } from "./coordination-api";
import { App } from "./App";
import { Discussion } from "./Discussion";
import { deferred, group, json, other, page } from "./test/groups";
import { member } from "./test/members";
import { message } from "./test/messages";
import { task } from "./test/tasks";

const raw = "  <script>window.bad = true</script>\r\n<img src=x onerror=bad()>\n真实原文  ";
const agent: Message = { ...message, id: "persisted-agent /?+", sender_id: member.id,
  topic_id: " \t真实话题 /?+ \r\n\u0000", content: raw, deliveries: [] };
const local: Message = { ...message, content: raw, deliveries: [] };
const foreign: Message = { ...message, id: "persisted-b", group_id: other.id, content: "B 的真实消息", deliveries: [] };
const roster = { [group.id]: [member] };
const draft = { title: "  明确目标\n保留原值  ", description: "任务说明", acceptance: "可核验结果", reason: "我现在明确要求这项工作" };
const successText = "授权已登记，任务已创建；尚未自动启动执行。";
const currentTitle = "独立读取的当前任务";
const feedReads = vi.fn<typeof fetch>(), workWrites = vi.fn<typeof fetch>();
const taskReads = vi.fn<typeof fetch>(), messageWrites = vi.fn<typeof fetch>();
const requestURL = (id = group.id) => `/api/v1/groups/${encodeURIComponent(id)}/coordination/requests`;
const taskURL = (source: Message) => `/api/v1/groups/${encodeURIComponent(source.group_id)}/tasks/${encodeURIComponent(`task-${source.id}`)}`;
const main = () => screen.getByRole("main", { name: "讨论" });
const region = () => screen.getByRole("region", { name: "从消息创建任务" });
const work = () => within(region());
const field = (name = "任务标题") => work().getByRole("textbox", { name });
const action = (source = local) => screen.getByRole("button", { name: `转为任务，消息 ${source.id}` });
const openSource = (source = local) => fireEvent.click(action(source));
const change = (element: HTMLElement, value: string) => fireEvent.change(element, { target: { value } });
const submitWork = () => fireEvent.submit(work().getByRole("form", { name: "从消息创建任务" }));
const collapse = () => fireEvent.click(work().getByRole("button", { name: "收起" }));
const reset = () => fireEvent.click(screen.getByRole("button", { name: "刷新并重置历史窗口" }));
function fill() {
  change(field(), draft.title); change(field("任务描述（可选）"), draft.description);
  change(field("验收条件"), draft.acceptance); change(field("授权理由"), draft.reason);
}
function admission(groupId: string, sent: RequestWorkInput): AdmissionReceipt {
  const taskId = `task-${sent.message_id}`, decisionId = `decision-${sent.message_id}`;
  const mandateId = `mandate-${sent.message_id}`, triggerId = `trigger-${sent.message_id}`;
  const created_at = "2026-11-01T08:00:00.123456789Z";
  return {
    decision: { id: decisionId, trigger_id: triggerId, group_id: groupId, topic_id: sent.topic_id, kind: "create_new",
      actor_id: "user", policy_revision: "local-user-explicit/v1", reason: sent.reason, task_id: taskId,
      task_revision_at_admission: 1, mandate_id: mandateId, created_at },
    mandate: { id: mandateId, decision_id: decisionId, origin_trigger_id: triggerId, group_id: groupId, topic_id: sent.topic_id,
      task_id: taskId, task_revision_at_admission: 1, authority_kind: "local_user_explicit", authorized_by: "user",
      strength: "explicit", policy_revision: "local-user-explicit/v1", created_at,
      scope: { objective: sent.new_task.title, description: sent.new_task.description ?? "", acceptance: sent.new_task.acceptance,
        assignee_id: "", dependency_ids: [] } },
    task: { id: taskId, group_id: groupId, revision_at_admission: 1 },
  };
}
const attempt = (index: number) => {
  const [url, init] = workWrites.mock.calls[index];
  return { url, body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key") };
};
const expectedInput = (source: Message) => ({ topic_id: source.topic_id, message_id: source.id, mode: "create_new", reason: draft.reason,
  new_task: { title: draft.title, description: draft.description, acceptance: draft.acceptance } });
function onlyWorkPosts(count: number) {
  const posts = vi.mocked(fetch).mock.calls.filter(([, init]) => init?.method === "POST");
  expect(posts).toHaveLength(count);
  expect(posts.every(([url]) => String(url) === requestURL())).toBe(true);
  expect(messageWrites).not.toHaveBeenCalled();
}
function evidence() {
  const source = work().getByRole("region", { name: "来源证据" });
  const summary = within(source).getByText("完整来源与标识", { selector: "summary" });
  const details = summary.closest("details")!;
  if (!details.open) fireEvent.click(summary);
  return { source, details };
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
async function openApp(mobile = false) {
  resize(mobile); render(<App />);
  fireEvent.click(await screen.findByRole("button", { name: group.name }));
  await screen.findByRole("article", { name: `消息 ${local.id}` });
}
async function selectGroup(name: string) {
  const back = screen.queryByRole("button", { name: "返回聊天" });
  if (back) fireEvent.click(back);
  if (!screen.queryByRole("button", { name })) fireEvent.click(screen.getByRole("button", { name: "返回群组" }));
  fireEvent.click(await screen.findByRole("button", { name }));
  await screen.findByRole("article", { name: `消息 ${name === group.name ? local.id : foreign.id}` });
}
async function visitMobileDetails(originalRegion: HTMLElement, title: HTMLElement) {
  for (const name of ["成员", "任务", "搜索"]) {
    const trigger = within(main()).getByRole("button", { name });
    fireEvent.click(trigger);
    const context = screen.getByRole("complementary", { name });
    expect(document.getElementById(context.getAttribute("aria-labelledby")!)).toHaveFocus();
    expect(originalRegion).toBeInTheDocument(); expect(originalRegion).not.toBeVisible();
    expect(title).toBeInTheDocument(); expect(title).toBeDisabled();
    expect(within(originalRegion).queryByRole("textbox", { name: "任务标题" })).not.toBeInTheDocument();
    expect(taskReads).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "返回聊天" }));
    expect(trigger).toHaveFocus(); expect(region()).toBe(originalRegion); expect(field()).toBe(title);
    await act(async () => {});
  }
}
function keyboardActivate(button: HTMLElement, key: "Enter" | " ") {
  button.focus(); fireEvent.keyDown(button, { key });
  // JSDOM does not synthesize a native button's keyboard click.
  if (key === "Enter") fireEvent.click(button);
  fireEvent.keyUp(button, { key });
  if (key === " ") fireEvent.click(button);
}

beforeEach(() => {
  resize = stubViewport();
  feedReads.mockReset().mockImplementation(async url => page(String(url).includes(`/${other.id}/`) ? [foreign] : [local, agent]));
  workWrites.mockReset().mockImplementation(async (url, init) =>
    json(admission(decodeURIComponent(String(url).split("/")[4]), JSON.parse(String(init?.body))), 201));
  taskReads.mockReset().mockImplementation(async url => {
    const parts = String(url).split("/");
    return json({ ...task, group_id: decodeURIComponent(parts[4]), id: decodeURIComponent(parts[6]), title: currentTitle });
  });
  messageWrites.mockReset().mockRejectedValue(new Error("Unexpected message POST"));
  vi.stubGlobal("fetch", vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input), method = init?.method ?? "GET";
    if (method === "POST" && url.endsWith("/coordination/requests")) return workWrites(input, init);
    if (method === "POST" && url.endsWith("/messages")) return messageWrites(input, init);
    if (method !== "GET") return Promise.reject(new Error(`Unexpected mutation: ${url}`));
    if (/\/messages\?/.test(url)) return feedReads(input, init);
    if (/\/tasks\//.test(url)) return taskReads(input, init);
    if (/\/tasks\?/.test(url) || url.includes("/history") || url.startsWith("/api/v1/providers")) return Promise.resolve(page());
    if (url.includes("/members")) return Promise.resolve(page(url.includes(`/${other.id}/`) ? [] : [member]));
    if (url.startsWith("/api/v1/groups?")) return Promise.resolve(page([group, other]));
    return Promise.reject(new Error(`Unexpected request: ${url}`));
  }));
});
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it.each([local, agent])("opens the exact persisted source $id in one stable history sibling and posts only its explicit admission", async source => {
  render(<Discussion groupId={group.id} members={roster} />);
  await screen.findByRole("article", { name: `消息 ${source.id}` });
  const composer = screen.getByRole("textbox", { name: "消息内容" });
  change(composer, "  未发送的消息草稿\n不能当作来源  ");
  fireEvent.change(screen.getByRole("combobox", { name: "接收方式" }), { target: { value: "selected" } });
  const recipient = screen.getByRole("checkbox", { name: `${member.name} · ${member.role} · ${member.id}` });
  fireEvent.click(recipient);
  const button = action(source);
  expect(button).toHaveAttribute("type", "button"); expect(button).toHaveTextContent("转为任务");
  expect(screen.getAllByRole("button", { name: /^转为任务，消息 / })).toHaveLength(2);
  openSource(source);
  const originalRegion = region(), originalTitle = field(), { details, source: sourceRegion } = evidence();
  expect(originalTitle).toHaveFocus();
  expect(field("验收条件")).toHaveValue(""); expect(field("授权理由")).toHaveValue("");
  expect(sourceRegion).toHaveTextContent(source.sender_id === "user" ? "我" : member.name);
  expect(details.querySelector("p")?.textContent).toBe(raw);
  expect(within(details).getByText(source.id, { exact: true })).toBeVisible();
  expect(within(details).getByText(source.group_id, { exact: true })).toBeVisible();
  if (source.topic_id) expect(details.querySelector("dd.message-content")?.textContent).toBe(source.topic_id);
  else expect(within(sourceRegion).getByText("主话题", { exact: true })).toBeVisible();
  expect(originalRegion.querySelector("script, img")).toBeNull();
  const history = screen.getByRole("region", { name: "消息记录" }), list = screen.getByRole("list", { name: "群组消息" });
  const mount = originalRegion.parentElement!.parentElement!;
  // Protect the history/composer scroll boundary, not a CSS or geometry snapshot.
  expect(mount.parentElement).toBe(history); expect(mount.nextElementSibling).toBe(list);
  expect(history.querySelector(".discussion-tools")!.compareDocumentPosition(mount) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  expect(list).not.toContainElement(originalRegion);
  expect([...history.parentElement!.children]).toEqual([history, screen.getByRole("region", { name: "撰写消息" })]);
  expect(screen.getByRole("textbox", { name: "消息内容" })).toBe(composer); expect(composer).toBeEnabled();
  const skip = screen.getByRole("link", { name: "跳至消息输入" });
  expect(skip).toHaveAttribute("href", `#${composer.id}`); composer.focus(); expect(composer).toHaveFocus();
  fill(); collapse(); expect(originalRegion).not.toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "继续任务草稿" }));
  expect(region()).toBe(originalRegion); expect(field()).toBe(originalTitle);
  expect(composer).toHaveValue("  未发送的消息草稿\n不能当作来源  "); expect(recipient).toBeChecked();
  submitWork(); await work().findByText(successText); await work().findByText(currentTitle);
  expect(attempt(0).url).toBe(requestURL(source.group_id));
  expect(JSON.parse(String(attempt(0).body))).toEqual(expectedInput(source));
  expect(attempt(0).key).toMatch(/^[\da-f-]{36}$/);
  expect(taskReads.mock.calls.map(([url]) => url)).toEqual([taskURL(source)]);
  expect(screen.getAllByRole("article")).toHaveLength(2); expect(screen.getByRole("list", { name: "群组消息" })).not.toContainElement(work().getByRole("region", { name: "授权回执" }));
  onlyWorkPosts(1);
});

it("keeps the same form, source and operation through actual draft/pending/unknown history-window replacements", async () => {
  render(<Discussion groupId={group.id} members={roster} />);
  await screen.findByRole("article", { name: `消息 ${local.id}` }); openSource(); fill();
  const originalRegion = region(), title = field(), form = work().getByRole("form", { name: "从消息创建任务" });
  const loading = deferred<Response>(); feedReads.mockReturnValueOnce(loading.promise); reset();
  expect(screen.getByText("正在载入消息…")).toBeVisible(); expect(region()).toBe(originalRegion); expect(field()).toBe(title);
  await act(async () => loading.resolve(page([agent])));
  expect(screen.queryByRole("article", { name: `消息 ${local.id}` })).not.toBeInTheDocument();
  expect(field()).toHaveValue(draft.title);
  const pending = deferred<Response>(); workWrites.mockReturnValueOnce(pending.promise); submitWork();
  const original = attempt(0); expect(JSON.parse(String(original.body))).toEqual(expectedInput(local));
  openSource(agent); expect(work().getByRole("status")).toHaveTextContent("原请求尚未确认");
  const empty = deferred<Response>(); feedReads.mockReturnValueOnce(empty.promise); reset();
  expect(region()).toBe(originalRegion); expect(field()).toBeDisabled();
  await act(async () => empty.resolve(page()));
  expect(screen.getByText("尚无消息，可发送第一条讨论。")).toBeVisible();
  expect(work().getByRole("form", { name: "从消息创建任务" })).toBe(form); expect(field()).toBe(title);
  await act(async () => pending.resolve(json({}, 503)));
  expect(work().getByRole("alert")).toHaveTextContent("创建结果未确认"); expect(taskReads).not.toHaveBeenCalled();
  feedReads.mockResolvedValueOnce(json({}, 503)); reset();
  await screen.findByText("服务暂时无法完成请求（HTTP 503）。");
  expect(region()).toBe(originalRegion); expect(field()).toBe(title);
  feedReads.mockResolvedValueOnce(page([agent])); reset(); await screen.findByRole("article", { name: `消息 ${agent.id}` });
  openSource(agent); expect(region()).toBe(originalRegion); expect(field()).toBe(title); expect(title).toHaveValue(draft.title);
  expect(within(evidence().details).getByText(local.id, { exact: true })).toBeVisible();
  fireEvent.click(work().getByRole("button", { name: "重试同一次创建" }));
  await work().findByText(currentTitle); expect(attempt(1)).toEqual(original);
  expect(taskReads.mock.calls.map(([url]) => url)).toEqual([taskURL(local)]); onlyWorkPosts(2);
});

it.each([false, true])("isolates a pending admission and late 201 across actual App group navigation (mobile=%s)", async mobile => {
  const pending = deferred<Response>(); workWrites.mockReturnValueOnce(pending.promise);
  await openApp(mobile); openSource(); fill();
  const originalRegion = region(), title = field(); submitWork(); const original = attempt(0);
  if (mobile) { await visitMobileDetails(originalRegion, title); collapse(); }
  await selectGroup(other.name);
  expect(title).toBeInTheDocument(); expect(title).not.toBeVisible(); expect(title).toBeDisabled();
  openSource(foreign); change(field(), "B 的任务草稿"); const bTitle = field(); bTitle.focus();
  if (mobile) fireEvent.click(within(main()).getByRole("button", { name: "搜索" }));
  const focused = document.activeElement;
  expect(workWrites.mock.calls[0][1]?.signal).toBeUndefined();
  await act(async () => pending.resolve(json(admission(group.id, JSON.parse(String(original.body))), 201)));
  expect(document.activeElement).toBe(focused); expect(taskReads).not.toHaveBeenCalled();
  expect(within(originalRegion).getByText(successText)).not.toBeVisible(); expect(bTitle).toHaveValue("B 的任务草稿");
  await selectGroup(group.name);
  if (mobile) {
    expect(screen.queryByRole("region", { name: "从消息创建任务" })).not.toBeInTheDocument();
    expect(taskReads).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "查看授权回执" })); expect(originalRegion).toHaveFocus();
  } else expect(within(main()).getByRole("heading", { name: group.name })).toHaveFocus();
  expect(region()).toBe(originalRegion); await work().findByText(currentTitle);
  expect(taskReads.mock.calls.map(([url]) => url)).toEqual([taskURL(local)]);
  await selectGroup(other.name); expect(field()).toBe(bTitle); expect(bTitle).toHaveValue("B 的任务草稿");
  expect(work().queryByRole("region", { name: "授权回执" })).not.toBeInTheDocument(); onlyWorkPosts(1);
});

it("preserves an actual 503 unknown through mobile details/groups and retries only the frozen original source/key/payload", async () => {
  workWrites.mockResolvedValueOnce(json({}, 503)); await openApp(true);
  const composer = screen.getByRole("textbox", { name: "消息内容" }); change(composer, "原来的消息草稿");
  fireEvent.change(screen.getByRole("combobox", { name: "接收方式" }), { target: { value: "selected" } });
  const recipient = await screen.findByRole("checkbox", { name: `${member.name} · ${member.role} · ${member.id}` }); fireEvent.click(recipient);
  openSource(agent); fill(); const originalRegion = region(), title = field(); submitWork();
  expect(await work().findByRole("alert")).toHaveTextContent("创建结果未确认");
  const original = attempt(0); expect(JSON.parse(String(original.body))).toEqual(expectedInput(agent));
  await visitMobileDetails(originalRegion, title); collapse();
  await selectGroup(other.name); openSource(foreign); change(field(), "B 的独立任务草稿");
  await selectGroup(group.name); fireEvent.click(screen.getByRole("button", { name: "恢复未确认的任务创建" }));
  expect(region()).toBe(originalRegion); expect(field()).toBe(title); expect(title).toHaveValue(draft.title);
  openSource(local); expect(work().getByRole("status")).toHaveTextContent("不能替换来源");
  expect(within(evidence().details).getByText(agent.id, { exact: true })).toBeVisible();
  change(title, "不能替换输入"); expect(title).toHaveValue(draft.title); expect(title).toBeDisabled();
  const retry = deferred<Response>(); workWrites.mockReturnValueOnce(retry.promise);
  fireEvent.click(work().getByRole("button", { name: "重试同一次创建" })); expect(attempt(1)).toEqual(original);
  collapse(); fireEvent.click(within(main()).getByRole("button", { name: "搜索" }));
  const focused = document.activeElement;
  await act(async () => retry.resolve(json(admission(group.id, JSON.parse(String(original.body))), 201)));
  expect(document.activeElement).toBe(focused); expect(taskReads).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "返回聊天" }));
  expect(screen.queryByRole("region", { name: "从消息创建任务" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "查看授权回执" })); await work().findByText(currentTitle);
  expect(taskReads.mock.calls.map(([url]) => url)).toEqual([taskURL(agent)]);
  expect(screen.getByRole("textbox", { name: "消息内容" })).toBe(composer); expect(composer).toHaveValue("原来的消息草稿");
  expect(composer).toBeEnabled(); expect(recipient).toBeChecked(); composer.focus(); expect(composer).toHaveFocus(); onlyWorkPosts(2);
});

it.each(["Enter", " "] as const)("connects native message-button %j activation, collapse focus and missing-source recovery", async key => {
  render(<Discussion groupId={group.id} members={roster} />);
  await screen.findByRole("article", { name: `消息 ${local.id}` });
  const trigger = action(); expect(trigger).toHaveAttribute("type", "button"); keyboardActivate(trigger, key);
  const title = field(); expect(title).toHaveFocus(); change(title, "保留的任务草稿");
  keyboardActivate(work().getByRole("button", { name: "收起" }), key); expect(trigger).toHaveFocus();
  keyboardActivate(trigger, key); expect(field()).toBe(title); expect(title).toHaveFocus();
  feedReads.mockResolvedValueOnce(page([agent])); reset(); await screen.findByRole("article", { name: `消息 ${agent.id}` });
  expect(trigger).not.toBeInTheDocument();
  keyboardActivate(work().getByRole("button", { name: "收起" }), key);
  const restore = screen.getByRole("button", { name: "继续任务草稿" }); expect(restore).toHaveFocus();
  keyboardActivate(restore, key); expect(field()).toBe(title); expect(title).toHaveValue("保留的任务草稿"); expect(title).toHaveFocus();
  expect(screen.getByRole("textbox", { name: "消息内容" })).toBeEnabled(); onlyWorkPosts(0);
});

// Controlled coordinates exercise the real scroll hook; they are not browser geometry evidence.
function installHistoryGeometry() {
  const tops = new WeakMap<HTMLElement, number>(); let compensation = 0;
  const rows = (history: HTMLElement) => [...history.querySelectorAll<HTMLElement>(".message-list > li")];
  const header = (history: HTMLElement) => {
    const disclosure = history.querySelector(".creation-disclosure");
    return 80 + (disclosure && !disclosure.closest("[hidden]") ? 360 : 0) + compensation;
  };
  const extent = (history: HTMLElement) => header(history) + rows(history).length * 100 + 20;
  const rect = (top: number, height: number) => ({ top, bottom: top + height, left: 0, right: 320,
    width: 320, height, x: 0, y: top, toJSON: () => ({}) }) as DOMRect;
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockImplementation(function (this: HTMLElement) {
    return this.matches(".message-history") && !this.closest("[hidden]") ? 200 : 0;
  });
  vi.spyOn(HTMLElement.prototype, "scrollHeight", "get").mockImplementation(function (this: HTMLElement) {
    return this.matches(".message-history") ? extent(this) : 0;
  });
  vi.spyOn(Element.prototype, "scrollTop", "get").mockImplementation(function (this: HTMLElement) { return tops.get(this) ?? 0; });
  vi.spyOn(Element.prototype, "scrollTop", "set").mockImplementation(function (this: HTMLElement, value: number) {
    tops.set(this, Math.max(0, Math.min(value, extent(this) - 200)));
  });
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
    if (this.matches(".message-history")) return rect(100, 200);
    const row = this.closest<HTMLElement>(".message-list > li"), history = row?.closest<HTMLElement>(".message-history");
    if (!row || !history) return rect(0, 0);
    return rect(100 + header(history) + rows(history).indexOf(row) * 100 - history.scrollTop, 100);
  });
  return { compensate: () => { compensation += 30; }, readingTop: 80 + 360 + 150 };
}
it.each(["task textarea", "history"])("distinguishes %s Home input from reading before a real pending-message confirmation", async target => {
  const geometry = installHistoryGeometry();
  vi.spyOn(performance, "now").mockReturnValue(1000);
  const records = Array.from({ length: 8 }, (_, index): Message => ({ ...local, id: `real-${index + 1}`,
    created_at: `2026-10-30T12:00:0${index}Z`, content: `真实消息 ${index + 1}` }));
  feedReads.mockResolvedValueOnce(page(records));
  render(<Discussion groupId={group.id} members={roster} />);
  await screen.findByRole("article", { name: "消息 real-1" }); openSource(records[0]);
  const title = field(), history = screen.getByRole("region", { name: "消息记录" });
  fireEvent.wheel(history, { deltaY: -500 }); history.scrollTop = geometry.readingTop; fireEvent.scroll(history);
  const anchor = screen.getByRole("article", { name: "消息 real-2" }).closest("li")!;
  const before = anchor.getBoundingClientRect().top;
  const sent: Message = { ...local, id: "confirmed-message", created_at: "2026-10-30T12:00:09Z", content: "真正发送的消息" };
  const pending = deferred<Response>(); messageWrites.mockReturnValueOnce(pending.promise);
  change(screen.getByRole("textbox", { name: "消息内容" }), sent.content);
  fireEvent.submit(screen.getByRole("form", { name: "发送消息" }));
  const input = target === "task textarea" ? title : history; input.focus();
  // Model a cursor/layout-compensation scroll after Home, without any wheel/pointer reading.
  expect(fireEvent.keyDown(input, { key: "Home" })).toBe(true);
  if (target === "task textarea") change(title, "正在编辑任务标题");
  geometry.compensate(); history.scrollTop += 30; fireEvent.scroll(history);
  expect(anchor.getBoundingClientRect().top).toBe(before);
  await act(async () => pending.resolve(json(sent, 201)));
  const confirmed = screen.getByRole("article", { name: `消息 ${sent.id}` }).closest("li")!;
  if (target === "task textarea") {
    expect(confirmed.getBoundingClientRect().top).toBeGreaterThanOrEqual(history.getBoundingClientRect().top);
    expect(confirmed.getBoundingClientRect().bottom).toBeLessThanOrEqual(history.getBoundingClientRect().bottom);
    expect(title).toHaveValue("正在编辑任务标题");
  } else {
    expect(history.scrollTop).toBe(geometry.readingTop + 30);
    expect(confirmed.getBoundingClientRect().top).toBeGreaterThan(history.getBoundingClientRect().bottom);
  }
  expect(input).toHaveFocus(); expect(workWrites).not.toHaveBeenCalled(); expect(messageWrites).toHaveBeenCalledTimes(1);
  expect(messageWrites.mock.calls[0][0]).toBe(`/api/v1/groups/${group.id}/messages`);
});
