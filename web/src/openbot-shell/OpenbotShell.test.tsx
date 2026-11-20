import { webcrypto } from "node:crypto";
import * as ReactDOMClient from "react-dom/client";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { PendingDecision } from "../decision-api";
import { deferred, group, json, other, page } from "../test/groups";
import { member } from "../test/members";
import { message } from "../test/messages";
import { task } from "../test/tasks";

const transport = vi.fn<typeof fetch>();
const decisionsPath = "/api/v1/me/pending-decisions?limit=50";
const inboxPath = "/api/v1/me/pending-reviews?limit=50";
const decisionProbe = "/api/v1/me/pending-decisions?limit=1";
const reviewProbe = "/api/v1/me/pending-reviews?limit=1";
const pendingReads = () => transport.mock.calls.filter(([url]) => /^\/api\/v1\/me\/pending-(decisions|reviews)\?/.test(String(url)));
const probes = () => pendingReads().filter(([url]) => url === decisionProbe || url === reviewProbe);
const writes = () => transport.mock.calls.filter(([, init]) => init?.method === "POST");
const identity = ([url, init]: Parameters<typeof fetch>) => ({ url, body: init?.body,
  key: new Headers(init?.headers).get("Idempotency-Key") });
vi.mock("react-dom/client", async importOriginal => {
  const actual = await importOriginal<typeof ReactDOMClient>();
  return { ...actual, createRoot: vi.fn((...args: Parameters<typeof actual.createRoot>) => {
    const root = actual.createRoot(...args);
    root.render = vi.fn(root.render.bind(root));
    return root;
  }) };
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
    if (url === decisionsPath || url === decisionProbe) return json({ error: { code: "not_found" } }, 404);
    if (url === reviewProbe) return page();
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
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.doUnmock("./OpenbotShell");
  vi.unstubAllGlobals();
});

async function loadEntry(search = "") {
  window.history.replaceState({}, "", `/${search}`);
  // A fresh entry module, but shared React/App modules, for each simulated page load.
  const entry = `../main.tsx?openbot-entry-test=${++entrySequence}`;
  await act(async () => { await import(/* @vite-ignore */ entry); });
}
async function start(search = "") {
  await loadEntry(search);
  await screen.findByRole("button", { name: group.name });
}
function expectSingleEntryRender() {
  expect(ReactDOMClient.createRoot).toHaveBeenCalledTimes(1);
  expect(vi.mocked(ReactDOMClient.createRoot).mock.results[0].value.render).toHaveBeenCalledTimes(1);
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
const inbox = () => within(screen.getByRole("main", { name: "待我决定" }));
const reviewItem = (source: typeof group, revision: number) => ({
  group: { id: source.id, name: source.name }, topic_id: "原话题",
  task: { id: `task-${source.id}`, title: task.title, acceptance: task.acceptance },
  submission: { id: `submission-${source.id}-${revision}`, group_id: source.id, task_id: `task-${source.id}`,
    task_revision: revision, attempt_id: `attempt-${source.id}-${revision}`, summary: `${source.name}交付摘要`,
    artifact_versions: [{ artifact_id: `artifact-${source.id}`, version_id: `version-${revision}` }],
    submitted_by: "worker", created_at: task.created_at },
});
const firstReview = reviewItem(group, 3), secondReview = reviewItem(other, 8);
const card = (item: typeof firstReview) => within(inbox().getByRole("article", { name: `${item.group.name} · ${item.task.title}` }));
const reviewPath = (item: typeof firstReview) => `/api/v1/groups/${item.group.id}/tasks/${item.task.id}/reviews`;
const reviewReceipt = (item: typeof firstReview, outcome: "accepted" | "rejected", reason: string) => ({
  id: "review-saved", group_id: item.group.id, task_id: item.task.id,
  task_revision: item.submission.task_revision, submission_id: item.submission.id, outcome, reason,
  artifact_versions: item.submission.artifact_versions, created_at: task.created_at, reviewed_by: "user",
});

describe("load-time shell entry", () => {
  it.each(["", "?shell=openbot", "?shell=other", "?shell=Openbot", "?shell=Classic", "?shell=", "?shell=openbot&shell=classic"])("loads Openbot by default for %s", async search => {
    await start(search);
    expect(navigation().getAllByRole("button")).toHaveLength(6);
    expect(screen.getByRole("banner")).toHaveTextContent("ct0x");
    expect(screen.getByRole("main", { name: "讨论" })).toBeVisible();
    expectSingleEntryRender();
  });

  it.each(["?shell=classic", "?other=1&shell=classic&shell=openbot"])("keeps the classic entry and its draft after history changes for %s", async search => {
    await start(search);
    expect(screen.queryByRole("navigation", { name: "工作区导航" })).not.toBeInTheDocument();
    expect(screen.getByRole("banner")).toHaveTextContent("ct0x");
    expect(screen.getByRole("main", { name: "讨论" })).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: group.name }));
    await screen.findByText("尚无消息，可发送第一条讨论。");
    const input = content(); change(input, "经典入口草稿");
    expectSingleEntryRender();
    window.history.replaceState({}, "", "/?shell=openbot");
    fireEvent(window, new PopStateEvent("popstate"));
    expect(screen.queryByRole("navigation", { name: "工作区导航" })).not.toBeInTheDocument();
    expect(content()).toBe(input); expect(input).toHaveValue("经典入口草稿");
    expectSingleEntryRender();
  });

  it("loads the default entry once and keeps its controller after history changes", async () => {
    await start();
    expect(navigation().getAllByRole("button").map(button => button.getAttribute("aria-label"))).toEqual([
      "待我决定", "群组列表", "聊天", "搜索", "成员", "任务",
    ]);
    const license = screen.getByRole("link", { name: "来源与许可" });
    expect(license).toHaveAttribute("href", "/licenses/openbot/index.html");
    fireEvent.click(rail("聊天"));
    expect(screen.getByRole("main", { name: "讨论" })).toHaveTextContent("请先选择群组");
    fireEvent.click(rail("任务"));
    expect(screen.getByRole("complementary", { name: "任务" })).toHaveTextContent("请先选择群组");
    expect(rail("任务")).toHaveAttribute("aria-expanded", "true");
    fireEvent.click(screen.getByRole("button", { name: "关闭详情" }));
    expect(rail("任务")).toHaveFocus();
    expect(writes()).toHaveLength(0);
    expect(transport.mock.calls.filter(([url]) => String(url).includes("/tasks"))).toHaveLength(0);
    selectGroup(group.name); await screen.findByText("尚无消息，可发送第一条讨论。");
    const input = content(); change(input, "默认入口草稿");
    expectSingleEntryRender();
    window.history.pushState({}, "", "/?shell=classic");
    fireEvent(window, new PopStateEvent("popstate"));
    expect(navigation().getAllByRole("button")).toHaveLength(6);
    expect(content()).toBe(input); expect(input).toHaveValue("默认入口草稿");
    expectSingleEntryRender();
  });

  it("reports a failed default chunk in Chinese with an explicit classic recovery link before mounting App", async () => {
    vi.doMock("./OpenbotShell", () => { throw new Error("fixture chunk unavailable"); });
    await loadEntry();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("界面未能载入");
    expect(screen.getByRole("link", { name: "打开经典界面" })).toHaveAttribute("href", "/?shell=classic");
    expect(transport).not.toHaveBeenCalled();
    expectSingleEntryRender();
    window.history.replaceState({}, "", "/?shell=classic");
    fireEvent(window, new PopStateEvent("popstate"));
    expect(screen.getByRole("alert")).toBe(alert);
    expect(transport).not.toHaveBeenCalled();
    expectSingleEntryRender();
  });

  it("starts the real classic App while the optional Openbot module is unavailable", async () => {
    vi.doMock("./OpenbotShell", () => { throw new Error("fixture chunk unavailable"); });
    await start("?shell=classic");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.queryByRole("navigation", { name: "工作区导航" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: group.name }));
    await screen.findByText("尚无消息，可发送第一条讨论。");
    change(content(), "兼容界面仍可撰写");
    expect(content()).toHaveValue("兼容界面仍可撰写");
    expect(pendingReads()).toHaveLength(0);
    expectSingleEntryRender();
  });
});

describe("global pending reviews in the production entry", () => {
  it.each([false, true])("pages and reviews an exact same-title item without selecting a group (mobile=%s)", async mobile => {
    const network = transport.getMockImplementation()!;
    const loading = deferred<void>(), saved = deferred<Response>();
    transport.mockImplementation((input, init) => {
      const url = String(input);
      if (url === inboxPath) return loading.promise.then(() => page([firstReview], "earlier+/"));
      if (url === `${inboxPath}&before=earlier%2B%2F`) return Promise.resolve(page([secondReview]));
      if (url === reviewPath(secondReview)) return saved.promise;
      return network(input, init);
    });
    resize(mobile); await start();
    expect(pendingReads().every(([url]) => url === decisionProbe || url === reviewProbe)).toBe(true);
    fireEvent.click(rail("待我决定"));
    expect(rail("待我决定")).toHaveAttribute("aria-current", "page");
    expect(rail("群组列表")).not.toHaveAttribute("aria-current");
    expect(screen.getByRole("main", { name: "待我决定" })).toHaveFocus();
    expect(screen.getByRole("link", { name: "跳至待我决定" })).toHaveAttribute("href", "#im-pending-reviews");
    expect(inbox().getByText("正在载入待验收事项…")).toHaveAttribute("role", "status");
    await act(async () => loading.resolve());
    expect(await inbox().findByText("待决答复暂不可用。")).toBeVisible();
    expect(inbox().queryByText("没有待决事项。")).not.toBeInTheDocument();
    expect(inbox().queryByRole("alert")).not.toBeInTheDocument();
    fireEvent.click(inbox().getByRole("button", { name: "加载更多待验收事项" }));
    await inbox().findByText(secondReview.submission.summary);
    expect(card(secondReview).getByText(`来源群组：${other.name}`)).toBeVisible();
    expect(card(secondReview).getByText(task.acceptance)).toBeVisible();
    expect(card(secondReview).getByText("任务版本 8")).toBeVisible();
    fireEvent.click(card(secondReview).getByText("来源与版本标识", { selector: "summary" }));
    const download = card(secondReview).getByRole("link", { name: "下载固定成果文件" });
    const { group_id, artifact_versions: [ref] } = secondReview.submission;
    expect(download).toHaveAttribute("href", `/api/v1/groups/${encodeURIComponent(group_id)}/artifacts/${encodeURIComponent(ref.artifact_id)}/versions/${encodeURIComponent(ref.version_id)}/content`);
    expect(download).toHaveAttribute("download");
    expect(inbox().queryByText(/项目/)).not.toBeInTheDocument();
    expect(screen.queryByRole("main", { name: "讨论" })).not.toBeInTheDocument();

    const raw = "  保留理由原文\n  ";
    change(card(secondReview).getByRole("textbox", { name: "验收理由（可选）" }), raw);
    fireEvent.click(card(secondReview).getByRole("button", { name: "拒收" }));
    fireEvent.click(card(secondReview).getByRole("button", { name: "接纳" }));
    expect(card(secondReview).getByRole("textbox")).toBeDisabled();
    expect(card(firstReview).getByRole("button", { name: "接纳" })).toBeEnabled();
    expect(writes()).toHaveLength(1);
    expect(identity(writes()[0]).url).toBe(reviewPath(secondReview));
    expect(identity(writes()[0]).key).toMatch(/^[\da-f-]{36}$/);
    expect(JSON.parse(String(writes()[0][1]?.body))).toEqual({ expected_revision: 8,
      submission_id: secondReview.submission.id, outcome: "rejected", reason: raw });
    await act(async () => saved.resolve(json(reviewReceipt(secondReview, "rejected", raw), 201)));
    expect(inbox().getByText(/已保存：拒收/)).toHaveAttribute("role", "status");
    expect(inbox().getByText("review-saved")).toBeInTheDocument();
    expect(card(firstReview).getByRole("button", { name: "接纳" })).toBeEnabled();
    expect(inbox().queryByText(secondReview.submission.summary)).not.toBeInTheDocument();
    expect(transport.mock.calls.some(([url]) => String(url).includes("/messages"))).toBe(false);
  });

  it("keeps pagination available after saving the only loaded review", async () => {
    const network = transport.getMockImplementation()!;
    transport.mockImplementation((input, init) => {
      const url = String(input);
      if (url === inboxPath) return Promise.resolve(page([firstReview], "earlier"));
      if (url === `${inboxPath}&before=earlier`) return Promise.resolve(page([secondReview]));
      if (url === reviewPath(firstReview)) return Promise.resolve(json(reviewReceipt(firstReview, "accepted", ""), 201));
      return network(input, init);
    });
    await start(); fireEvent.click(rail("待我决定"));
    await inbox().findByText(firstReview.submission.summary);
    fireEvent.click(card(firstReview).getByRole("button", { name: "接纳" }));
    await inbox().findByText(/已保存：接纳/);
    expect(inbox().queryByRole("article")).not.toBeInTheDocument();
    expect(inbox().queryByText("没有待验收事项。")).not.toBeInTheDocument();
    const more = inbox().getByRole("button", { name: "加载更多待验收事项" });
    expect(more).toBeEnabled();
    fireEvent.click(more);
    expect(await inbox().findByText(secondReview.submission.summary)).toBeVisible();
  });

  it("retains the loaded chat window, draft and focus, and retries an unconfirmed review with its original evidence", async () => {
    const network = transport.getMockImplementation()!;
    const initialWrite = deferred<Response>();
    let pendingItems = [firstReview, secondReview];
    const older = { ...message, id: "earlier-chat-message", content: "更早的聊天记录", deliveries: [] };
    transport.mockImplementation((input, init) => {
      const url = String(input);
      if (url === inboxPath) return Promise.resolve(page(pendingItems));
      if (url.includes("/messages")) return Promise.resolve(url.includes("before=") ? page([older]) : page([], "older"));
      if (url === reviewPath(firstReview)) {
        if (writes().length === 1) return initialWrite.promise;
        if (writes().length === 2) return Promise.resolve(json({}, 409));
        return Promise.resolve(json(reviewReceipt(firstReview, "accepted", "原决定"), 201));
      }
      return network(input, init);
    });
    await start(); selectGroup(group.name);
    fireEvent.click(await screen.findByRole("button", { name: "加载更早消息" }));
    await screen.findByText(older.content);
    const input = content() as HTMLTextAreaElement, history = screen.getByRole("region", { name: "消息记录" });
    history.scrollTop = 90; fireEvent.scroll(history);
    change(input, "跨页聊天草稿");
    act(() => { input.focus(); input.setSelectionRange(2, 2); rail("待我决定").focus(); });
    fireEvent.click(rail("待我决定"));
    await inbox().findByText(firstReview.submission.summary);
    change(card(secondReview).getByRole("textbox"), "未提交的验收理由");
    change(card(firstReview).getByRole("textbox"), "原决定");
    fireEvent.click(card(firstReview).getByRole("button", { name: "接纳" }));
    fireEvent.click(rail("聊天"));
    expect(content()).toBe(input); expect(input).toHaveValue("跨页聊天草稿");
    expect(input).toHaveFocus(); expect(input).toHaveProperty("selectionStart", 2);
    expect(screen.getByRole("region", { name: "消息记录" })).toBe(history);
    expect(history.scrollTop).toBe(90); expect(screen.getByText(older.content)).toBeVisible();
    expect(writes()[0][1]?.signal?.aborted).not.toBe(true);

    // A successful HTTP status with the wrong frozen refs is not a Review receipt.
    pendingItems = [secondReview];
    await act(async () => initialWrite.resolve(json({ ...reviewReceipt(firstReview, "accepted", "原决定"), artifact_versions: [] }, 201)));
    fireEvent.click(rail("待我决定"));
    await inbox().findByRole("button", { name: "重试同一次验收" });
    expect(card(secondReview).getByRole("textbox")).toHaveValue("未提交的验收理由");
    expect(inbox().getByRole("alert")).toHaveTextContent("验收结果未确认");
    expect(inbox().queryByText(/已保存/)).not.toBeInTheDocument();
    fireEvent.click(inbox().getByRole("button", { name: "重试同一次验收" }));
    expect(await inbox().findByText(/HTTP 409/)).toHaveTextContent("验收结果未确认");
    fireEvent.click(inbox().getByRole("button", { name: "重试同一次验收" }));
    expect(await inbox().findByText(/已保存：接纳/)).toHaveAttribute("role", "status");
    expect(writes().map(identity)).toEqual(Array(3).fill(identity(writes()[0])));
  });

  it("distinguishes read failure, a fresh conflict and a successful empty inbox", async () => {
    const network = transport.getMockImplementation()!;
    let reads = 0;
    transport.mockImplementation((input, init) => {
      const url = String(input);
      if (url === inboxPath) {
        // StrictMode cancels the first mount read. Count only live requests.
        return Promise.resolve().then(() => init?.signal?.aborted ? page()
          : ++reads === 1 ? json({}, 503) : reads === 2 ? page([firstReview]) : page());
      }
      if (url === reviewPath(firstReview)) return Promise.resolve(json({}, 409));
      return network(input, init);
    });
    await start(); fireEvent.click(rail("待我决定"));
    expect(await inbox().findByRole("alert")).toHaveTextContent("HTTP 503");
    expect(inbox().queryByText("没有待验收事项。")).not.toBeInTheDocument();
    fireEvent.click(inbox().getByRole("button", { name: "重试载入待验收事项" }));
    await inbox().findByText(firstReview.submission.summary);
    fireEvent.click(card(firstReview).getByRole("button", { name: "接纳" }));
    expect(await card(firstReview).findByRole("alert")).toHaveTextContent("HTTP 409");
    expect(card(firstReview).getByRole("button", { name: "接纳" })).toBeDisabled();
    expect(inbox().queryByRole("button", { name: "重试同一次验收" })).not.toBeInTheDocument();
    fireEvent.click(inbox().getByRole("button", { name: "刷新待验收事项" }));
    expect(await inbox().findByText("没有待验收事项。")).toBeVisible();
    expect(writes()).toHaveLength(1);
  });
});

const question = (source: typeof group, suffix: string, topic_id: string, version: number, title = task.title) => ({
  id: `decision/${suffix}`, group_id: source.id, group_name: source.name, topic_id, version,
  kind: "decision", relay: "task_owner", recipient_id: "user",
  task: { task_id: `task-${suffix}`, task_title: title, task_revision: 5, assignment_id: `assignment-${suffix}`,
    mandate_id: `mandate-${suffix}`, authority_generation: 2, owner_member_id: `owner-${suffix}`, origin_trigger_id: `trigger-${suffix}` },
  source_message_id: `message-${suffix}`, source_sequence: 3, author_member_id: `owner-${suffix}`, author_turn_id: `origin-${suffix}`,
  facts: "工作日数据已核对。", question: "是否纳入周末？", recommendation: "先交工作日结果。",
  choices: ["仅工作日", "纳入周末"], created_at: task.created_at, updated_at: task.created_at,
} satisfies PendingDecision);

const answerReceipt = (item: PendingDecision, text: string, option_index?: number) => ({
  id: `answer-${item.id}`, request_id: item.id, group_id: item.group_id, topic_id: item.topic_id, version: item.version,
  actor_id: "user", task_id: item.task?.task_id ?? "", task_revision: item.task?.task_revision ?? 0, text, option_index,
  reply_message_id: `reply-${item.id}`, wake_delivery_id: `delivery-${item.id}`, wake_message_id: `wake-${item.id}`, created_at: task.created_at,
});

const reminderText = "有待处理事项";
const reminderMarker = () => rail("待我决定").querySelector(".window-rail-marker");
function tabVisibility(initial: DocumentVisibilityState = "visible") {
  const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue(initial);
  return (state: DocumentVisibilityState) => {
    visibility.mockReturnValue(state);
    fireEvent(document, new Event("visibilitychange"));
  };
}

describe("global rail reminder", () => {
  it("keeps the marker for remaining work and clears after the final save without closing the inbox", async () => {
    tabVisibility();
    vi.stubGlobal("crypto", webcrypto);
    const network = transport.getMockImplementation()!;
    const blocker = { ...question(other, "rail", "", 1), kind: "unblock" } satisfies PendingDecision;
    const answerPath = "/api/v1/me/decisions/decision%2Frail/answer";
    const receipt = answerReceipt(blocker, "纳入周末", 2);
    let decisions = [blocker], reviews = [firstReview];
    transport.mockImplementation(async (input, init) => {
      switch (String(input)) {
        case decisionProbe: case decisionsPath: return page(decisions);
        case reviewProbe: case inboxPath: return page(reviews);
        case reviewPath(firstReview):
          reviews = [];
          return json(reviewReceipt(firstReview, "accepted", ""), 201);
        case answerPath:
          decisions = [];
          return json(receipt, 201);
        case `${answerPath}?group_id=${other.id}&topic_id=&version=1`:
          return json({ receipt, handling: null });
        default: return network(input, init);
      }
    });

    resize(true);
    await start();
    const button = rail("待我决定");
    expect(button).toHaveAccessibleDescription(reminderText);
    const status = navigation().getByRole("status");
    expect(status).toHaveTextContent(reminderText);
    const marker = reminderMarker();
    expect(marker).toBeVisible();
    expect(new Set(pendingReads().map(([url]) => url))).toEqual(new Set([decisionProbe, reviewProbe]));
    expect(pendingReads().every(([, init]) => (init?.method ?? "GET") === "GET")).toBe(true);
    expect(writes()).toHaveLength(0);

    fireEvent.click(button);
    expect(screen.getByRole("main", { name: "待我决定" })).toHaveFocus();
    await inbox().findByText(firstReview.submission.summary);
    const beforeReview = pendingReads().length;
    fireEvent.click(card(firstReview).getByRole("button", { name: "接纳" }));
    await inbox().findByText(/已保存：接纳/);
    const afterReview = pendingReads().length;
    expect(reminderMarker()).toBe(marker);
    expect(navigation().getByRole("status")).toBe(status);
    expect(button).toHaveAccessibleDescription(reminderText);

    const list = within(inbox().getByRole("list", { name: "待答复的问题" }));
    fireEvent.click(list.getByRole("radio", { name: "纳入周末" }));
    fireEvent.click(list.getByRole("button", { name: "提交答复" }));
    expect(await inbox().findByText(receipt.id)).toBeVisible();
    await waitFor(() => expect(reminderMarker()).not.toBeInTheDocument());
    expect(inbox().getByText("尚未确认负责人处理。")).toBeVisible();
    expect(rail("待我决定")).toBe(button);
    expect(button).toHaveAttribute("aria-current", "page");
    expect(button).not.toHaveAccessibleDescription(reminderText);
    expect(status).toBeEmptyDOMElement();
    expect(pendingReads().slice(beforeReview, afterReview).map(([url]) => url)).toEqual([decisionProbe, reviewProbe]);
    expect(pendingReads().slice(afterReview).map(([url]) => url)).toEqual([decisionProbe, reviewProbe]);
    expect(writes().map(([url]) => url)).toEqual([reviewPath(firstReview), answerPath]);

    const beforeLeaving = pendingReads().length;
    await act(async () => { fireEvent.click(rail("群组列表")); });
    expect(pendingReads().slice(beforeLeaving).map(([url]) => url)).toEqual([decisionProbe, reviewProbe]);
  });

  it("rechecks a review saved while hidden, even if the first visible probe is aborted", async () => {
    const showTab = tabVisibility();
    const network = transport.getMockImplementation()!;
    const saved = deferred<Response>(), interrupted = deferred<Response>();
    let reviewPending = true, holdProbe = true;
    transport.mockImplementation(async (input, init) => {
      if (input === reviewPath(firstReview)) return saved.promise;
      if (input === reviewProbe && !reviewPending && holdProbe) return interrupted.promise;
      if (input === inboxPath || input === reviewProbe) return page(reviewPending ? [firstReview] : []);
      return network(input, init);
    });

    await start();
    const marker = reminderMarker();
    expect(marker).toBeVisible();
    fireEvent.click(rail("待我决定"));
    await inbox().findByText(firstReview.submission.summary);
    const beforeSave = probes().length;
    fireEvent.click(card(firstReview).getByRole("button", { name: "接纳" }));
    showTab("hidden");
    reviewPending = false;
    await act(async () => saved.resolve(json(reviewReceipt(firstReview, "accepted", ""), 201)));
    expect(inbox().getByText(/已保存：接纳/)).toBeVisible();
    expect(probes()).toHaveLength(beforeSave);
    expect(reminderMarker()).toBe(marker);

    vi.useFakeTimers();
    showTab("visible");
    showTab("visible");
    expect(probes().slice(beforeSave).map(([url]) => url)).toEqual([decisionProbe, reviewProbe]);
    const signal = probes()[beforeSave + 1][1]?.signal;
    showTab("hidden");
    expect(signal?.aborted).toBe(true);
    await act(async () => interrupted.resolve(page()));
    expect(reminderMarker()).toBe(marker);

    holdProbe = false;
    await act(async () => { showTab("visible"); });
    expect(reminderMarker()).not.toBeInTheDocument();
    await act(async () => {
      showTab("hidden"); showTab("visible");
      await vi.advanceTimersByTimeAsync(30_000);
    });
    expect(probes().slice(beforeSave).map(([url]) => url)).toEqual([decisionProbe, reviewProbe, decisionProbe, reviewProbe]);
  });

  it.each(["transient", "timed-out"] as const)("retries a %s post-save probe once while the inbox stays visible", async failure => {
    const showTab = tabVisibility();
    const network = transport.getMockImplementation()!;
    const stalled = deferred<Response>();
    const timedOut = failure === "timed-out";
    let saved = false, readsAfterSave = 0;
    transport.mockImplementation(async (input, init) => {
      if (input === inboxPath || (input === reviewProbe && !saved)) return page([firstReview]);
      if (input === reviewProbe) {
        if (++readsAfterSave === 1 && timedOut) return stalled.promise;
        return readsAfterSave <= (timedOut ? 2 : 1) ? json({}, 503) : page();
      }
      if (input === reviewPath(firstReview)) {
        saved = true;
        return json(reviewReceipt(firstReview, "accepted", ""), 201);
      }
      return network(input, init);
    });

    await start();
    const marker = reminderMarker();
    expect(marker).toBeVisible();
    fireEvent.click(rail("待我决定"));
    await inbox().findByText(firstReview.submission.summary);
    vi.useFakeTimers();
    const advance = (ms: number) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });
    const beforeSave = probes().length;
    await act(async () => { fireEvent.click(card(firstReview).getByRole("button", { name: "接纳" })); });
    expect(inbox().getByText(/已保存：接纳/)).toBeVisible();
    expect(probes()).toHaveLength(beforeSave + 2);

    if (timedOut) {
      const signal = probes()[beforeSave + 1][1]?.signal;
      await advance(5_000);
      expect(signal?.aborted).toBe(true);
    }
    showTab("visible");
    await advance(1_999);
    expect(probes()).toHaveLength(beforeSave + 2);
    expect(reminderMarker()).toBe(marker);
    await advance(1);
    expect(probes().slice(beforeSave).map(([url]) => url)).toEqual([decisionProbe, reviewProbe, decisionProbe, reviewProbe]);
    if (timedOut) expect(reminderMarker()).toBe(marker);
    else expect(reminderMarker()).not.toBeInTheDocument();
    expect(rail("待我决定")).toHaveAttribute("aria-current", "page");
    await advance(30_000);
    showTab("visible");
    expect(probes()).toHaveLength(beforeSave + 4);

    if (timedOut) {
      // A failed retry spends the budget, but a real visibility return still reconciles the marker.
      await act(async () => { showTab("hidden"); showTab("visible"); });
      expect(probes()).toHaveLength(beforeSave + 6);
      expect(reminderMarker()).not.toBeInTheDocument();
      await act(async () => stalled.resolve(page([firstReview])));
      expect(reminderMarker()).not.toBeInTheDocument();
    }
    expect(writes()).toHaveLength(1);
  });

  it("ignores a superseded probe after the final review clears the marker", async () => {
    tabVisibility();
    const network = transport.getMockImplementation()!;
    const stale = deferred<Response>();
    const lastReceipt = { ...reviewReceipt(secondReview, "accepted", ""), id: "last-review" };
    let reviews = [firstReview, secondReview];
    transport.mockImplementation(async (input, init) => {
      switch (String(input)) {
        case inboxPath: return page(reviews);
        case reviewProbe: return reviews[0] === secondReview ? stale.promise : page(reviews.slice(0, 1));
        case reviewPath(firstReview):
          reviews = [secondReview];
          return json(reviewReceipt(firstReview, "accepted", ""), 201);
        case reviewPath(secondReview):
          reviews = [];
          return json(lastReceipt, 201);
        default: return network(input, init);
      }
    });

    await start();
    const marker = reminderMarker();
    expect(marker).toBeVisible();
    fireEvent.click(rail("待我决定"));
    await inbox().findByText(firstReview.submission.summary);
    const beforeSave = probes().length;
    fireEvent.click(card(firstReview).getByRole("button", { name: "接纳" }));
    await inbox().findByText(/已保存：接纳/);
    const signal = probes()[beforeSave + 1]?.[1]?.signal;
    expect(signal?.aborted).toBe(false);
    expect(reminderMarker()).toBe(marker);

    fireEvent.click(card(secondReview).getByRole("button", { name: "接纳" }));
    expect(await inbox().findByText(lastReceipt.id)).toBeVisible();
    expect(signal?.aborted).toBe(true);
    await waitFor(() => expect(reminderMarker()).not.toBeInTheDocument());
    await act(async () => { stale.resolve(page([secondReview])); });
    expect(reminderMarker()).not.toBeInTheDocument();
    expect(probes().slice(beforeSave).map(([url]) => url)).toEqual([decisionProbe, reviewProbe, decisionProbe, reviewProbe]);
  });

  it("bounds interval probes and aborts both interval and save probes on teardown", async () => {
    const showTab = tabVisibility("hidden");
    const network = transport.getMockImplementation()!;
    const hanging = deferred<void>(), afterSave = deferred<void>();
    let saved = false;
    transport.mockImplementation((input, init) => {
      if (input === decisionProbe || input === reviewProbe) {
        return (saved ? afterSave : hanging).promise.then(() => input === reviewProbe ? page([firstReview]) : page());
      }
      if (input === inboxPath) return Promise.resolve(page([firstReview]));
      if (input === reviewPath(firstReview)) {
        saved = true;
        return Promise.resolve(json(reviewReceipt(firstReview, "accepted", ""), 201));
      }
      return network(input, init);
    });
    await start();
    expect(pendingReads()).toHaveLength(0);
    vi.useFakeTimers();
    const advance = (ms: number) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });
    const signal = (index: number) => probes()[index][1]?.signal;
    showTab("visible");
    expect(probes()).toHaveLength(2);
    expect(signal(0)).toBe(signal(1));
    expect(signal(0)?.aborted).toBe(false);
    showTab("visible");
    expect(probes()).toHaveLength(2);
    await advance(5000);
    expect(signal(0)?.aborted).toBe(true);
    expect(probes()).toHaveLength(2);
    await advance(20000);
    expect(probes()).toHaveLength(4);
    expect(signal(2)?.aborted).toBe(false);

    showTab("hidden");
    expect(signal(2)?.aborted).toBe(true);
    await advance(30000);
    expect(probes()).toHaveLength(4);
    showTab("visible");
    expect(probes()).toHaveLength(6);
    await act(async () => { fireEvent.click(rail("待我决定")); });
    expect(signal(4)?.aborted).toBe(true);
    await advance(30000);
    await act(async () => { hanging.resolve(); });
    expect(probes()).toHaveLength(6);
    expect(reminderMarker()).not.toBeInTheDocument();
    expect(writes()).toHaveLength(0);

    await act(async () => { fireEvent.click(card(firstReview).getByRole("button", { name: "接纳" })); });
    expect(inbox().getByText(/已保存：接纳/)).toBeVisible();
    expect(probes()).toHaveLength(8);
    expect(signal(6)).toBe(signal(7));
    expect(signal(6)?.aborted).toBe(false);
    act(() => { vi.mocked(ReactDOMClient.createRoot).mock.results[0].value.unmount(); });
    expect(signal(6)?.aborted).toBe(true);
    await act(async () => { afterSave.resolve(); });
    await advance(30000);
    expect(probes()).toHaveLength(8);
  });

  it.each([[404, "not_found"], [503, "decision_answers_unavailable"]] as const)(
    "handles unavailable decisions (HTTP %s, %s) without treating a transient outage as empty", async (status, code) => {
      const showTab = tabVisibility();
      const network = transport.getMockImplementation()!;
      let reviewPending = true, transient = false;
      transport.mockImplementation(async (input, init) => {
        if (input === decisionProbe) return transient ? json({}, 503) : json({ error: { code } }, status);
        if (input === reviewProbe) return page(reviewPending ? [firstReview] : []);
        return network(input, init);
      });
      await start();
      const button = rail("待我决定");
      expect(button).toHaveAccessibleDescription(reminderText);
      const recheck = () => act(async () => { showTab("hidden"); showTab("visible"); });
      reviewPending = false;
      transient = true;
      await recheck();
      expect(button).toHaveAccessibleDescription(reminderText);
      transient = false;
      await recheck();
      expect(button).not.toHaveAccessibleDescription(reminderText);
      expect(reminderMarker()).not.toBeInTheDocument();
      expect(writes()).toHaveLength(0);
    });
});

it("answers the exact project Task from the real inbox and the original chat, keeping saved and handled separate", async () => {
  vi.stubGlobal("crypto", webcrypto);
  const network = transport.getMockImplementation()!;
  const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  const foreign = question(group, "foreign", "", 3), chosen = question(other, "chosen", "topic/alerts +", 7);
  const local = question(other, "local", "", 9, "复核说明");
  const text = "  保留答复原文。  ";
  const receipt = answerReceipt(chosen, "纳入周末", 2), localReceipt = answerReceipt(local, text);
  const handling = { turn_id: "continuation-turn", attempt_id: "continuation-attempt", consumed_at: "2026-10-30T12:01:00Z" };
  let view: { receipt: typeof receipt; handling: typeof handling | null } = {
    receipt: { ...receipt, wake_delivery_id: "different-wake" }, handling,
  };
  const globalPost = "/api/v1/me/decisions/decision%2Fchosen/answer";
  const groupPost = `/api/v1/groups/${other.id}/decisions/decision%2Flocal/answer`;
  const globalStatus = `${globalPost}?group_id=${other.id}&topic_id=topic%2Falerts+%2B&version=7`;
  const localStatus = `/api/v1/me/decisions/decision%2Flocal/answer?group_id=${other.id}&topic_id=&version=9`;
  const wrongStatus = deferred<void>(), unknownWrite = deferred<void>();
  const announcement = { ...message, group_id: other.id, topic_id: chosen.topic_id, content: "已收到相关答复。", deliveries: [] };
  let pending = [foreign, chosen, local], localSaved = false;
  transport.mockImplementation((input, init) => {
    const url = String(input);
    if (url === decisionsPath) return Promise.resolve(page(pending));
    if (url === inboxPath) return Promise.resolve(page([firstReview]));
    if (url === globalPost && init?.method === "POST") return Promise.resolve(json(receipt));
    if (url === groupPost && init?.method === "POST") {
      return unknownWrite.promise.then(() => json({ error: { code: "decision_answer_unknown" } }, 503));
    }
    if (url === globalStatus && init?.method === "GET") {
      const snapshot = view;
      return wrongStatus.promise.then(() => json(snapshot));
    }
    if (url === localStatus && init?.method === "GET") return Promise.resolve(localSaved
      ? json({ receipt: localReceipt, handling: null }) : json({ error: { code: "not_found" } }, 404));
    for (const [source, project_id, name] of [[group, "payments", "支付项目"], [other, "settlement", "结算项目"]] as const) {
      if (url === `/api/v1/groups/${source.id}/identity`) return Promise.resolve(json({
        group_id: source.id, project_id, workspace_id: "workspace", channel_id: `channel-${source.id}`,
      }));
      if (url === `/api/v1/projects/${project_id}`) return Promise.resolve(json({ id: project_id, name, workspace_id: "workspace" }));
    }
    if (url.startsWith(`/api/v1/groups/${other.id}/messages?`)) return Promise.resolve(page([announcement]));
    return network(input, init);
  });
  const chat = () => within(screen.getByRole("main", { name: "讨论" }));
  const listReads = () => transport.mock.calls.filter(([url]) => url === decisionsPath).length;
  const chosenName = `结算项目 · ${other.name} · ${task.title}`, foreignName = `支付项目 · ${group.name} · ${task.title}`;
  const localName = `结算项目 · ${other.name} · ${local.task.task_title}`;
  const receiptRow = (area: ReturnType<typeof within>, id: string) => within(area.getByText(id).closest("li")!);
  function returnToTab() {
    visibility.mockReturnValue("hidden"); fireEvent(document, new Event("visibilitychange"));
    visibility.mockReturnValue("visible"); fireEvent(document, new Event("visibilitychange"));
  }

  await start(); expect(listReads()).toBe(0);
  fireEvent.click(rail("待我决定"));
  expect(inbox().getAllByRole("heading", { name: "待我决定" })).toHaveLength(1);
  expect(await inbox().findByText(firstReview.submission.summary)).toBeVisible();
  const decisionList = within(await inbox().findByRole("list", { name: "待答复的问题" }));
  const target = within(await decisionList.findByRole("article", { name: chosenName }));
  expect(await decisionList.findByRole("article", { name: foreignName })).toBeVisible();
  fireEvent.click(target.getByText("来源与版本标识", { selector: "summary" }));
  for (const id of ["settlement", other.id, chosen.task.task_id]) expect(target.getByText(id)).toBeVisible();
  change(within(decisionList.getByRole("article", { name: localName })).getByRole("textbox"), text);
  fireEvent.click(target.getByRole("radio", { name: "纳入周末" }));
  fireEvent.click(target.getByRole("button", { name: "提交答复" }));
  await waitFor(() => expect(writes()).toHaveLength(1));
  expect(identity(writes()[0])).toMatchObject({ url: globalPost, key: expect.any(String) });
  expect(JSON.parse(String(writes()[0][1]?.body))).toEqual({ group_id: other.id, topic_id: chosen.topic_id, version: 7, option_index: 2 });
  expect(transport.mock.calls.some(([url]) => /\/(messages|tasks)(?:\/|\?)/.test(String(url)))).toBe(false);
  await inbox().findByText(receipt.id);
  expect(receiptRow(inbox(), receipt.id).getByText("答复已保存")).toBeVisible();
  await act(async () => wrongStatus.resolve());
  expect(transport.mock.calls.find(([url]) => url === globalStatus)?.[1]).toMatchObject({ method: "GET", cache: "no-store", redirect: "error" });
  expect(inbox().queryByText("负责人已处理")).not.toBeInTheDocument();
  view = { receipt, handling: null };
  pending = [foreign, local];
  fireEvent.click(inbox().getByRole("button", { name: "刷新待决事项" }));
  await receiptRow(inbox(), receipt.id).findByText("尚未确认负责人处理。");
  expect(writes()).toHaveLength(1);

  const readCount = listReads();
  selectGroup(other.name);
  await chat().findByText(announcement.content);
  const summary = chat().getByText("本群待决事项", { selector: "summary" });
  expect(summary.closest("details")).not.toHaveAttribute("open");
  expect(listReads()).toBe(readCount);
  fireEvent.click(summary);
  const grouped = within(await chat().findByRole("region", { name: "待决事项" }));
  const localArticle = await grouped.findByRole("article", { name: localName });
  expect(grouped.getAllByRole("article")).toEqual([localArticle]);
  const localCard = within(localArticle);
  expect(localCard.getByRole("textbox")).toHaveValue(text);
  expect(receiptRow(grouped, receipt.id).getByText("答复已保存")).toBeVisible();
  expect(grouped.queryByText("负责人已处理")).not.toBeInTheDocument();
  expect(writes()).toHaveLength(1);
  view = { receipt, handling }; returnToTab();
  const beforeAck = probes().length;
  expect(await receiptRow(grouped, receipt.id).findByText("负责人已处理")).toBeVisible();
  expect(probes()).toHaveLength(beforeAck);

  fireEvent.click(localCard.getByRole("button", { name: "提交答复" }));
  await waitFor(() => expect(writes()).toHaveLength(2));
  expect(identity(writes()[1])).toMatchObject({ url: groupPost, key: expect.any(String) });
  expect(JSON.parse(String(writes()[1][1]?.body))).toEqual({ group_id: other.id, topic_id: "", version: 9, text });
  pending = [foreign];
  fireEvent.click(rail("待我决定"));
  const beforeConfirmation = probes().length;
  await act(async () => unknownWrite.resolve());
  expect(await inbox().findByText(/答复结果未确认/)).toBeVisible();
  expect(inbox().queryByText(localReceipt.id)).not.toBeInTheDocument();
  expect(probes()).toHaveLength(beforeConfirmation);
  localSaved = true; returnToTab();
  await inbox().findByText(localReceipt.id);
  expect(probes().slice(beforeConfirmation).map(([url]) => url)).toEqual([decisionProbe, reviewProbe]);
  expect(receiptRow(inbox(), localReceipt.id).getByText("答复已保存")).toBeVisible();
  expect(receiptRow(inbox(), localReceipt.id).queryByText("负责人已处理")).not.toBeInTheDocument();
  expect(receiptRow(inbox(), receipt.id).getByText("负责人已处理")).toBeVisible();
  expect(writes()).toHaveLength(2);
});

describe("real Openbot rail interactions", () => {
  it.each([false, true])("keeps directed drafts and pending/unknown send identity through rail and group changes (mobile=%s)", async mobile => {
    resize(mobile); await start(); selectGroup(group.name);
    await screen.findByText("尚无消息，可发送第一条讨论。");
    const input = content(), composer = screen.getByRole("region", { name: "撰写消息" });
    const raw = "  保留原始消息\n切换不重发  "; change(input, raw);
    fireEvent.click(screen.getByText("发送选项", { selector: "summary" }));
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
