import { createHash, randomUUID } from "node:crypto";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { prepareDecisionAnswer, readDecisionAnswer } from "./decision-api";
import type { PendingDecision } from "./decision-api";
import { journalDecisionAttempt, loadDecisionJournal } from "./decision-journal";
import { PendingReviews } from "./PendingReviews";
import { deferred, json, page } from "./test/groups";

const timestamp = "2026-11-19T12:00:00Z";
const taskQuestion = (source: string, version: number, topic_id: string) => ({
  id: `question-${source}`, version, kind: "authorization", group_id: `group/${source}`, topic_id,
  group_name: "告警", recipient_id: "user", relay: "task_owner",
  task: { task_id: `task-${source}`, task_title: "周末样本验证", task_revision: 3, assignment_id: `assignment-${source}`,
    mandate_id: `mandate-${source}`, authority_generation: 1, owner_member_id: `owner-${source}`, origin_trigger_id: `trigger-${source}` },
  source_message_id: `message-${source}`, source_sequence: 2, author_member_id: `owner-${source}`,
  author_turn_id: `turn-${source}`, author_attempt_id: `attempt-${source}`,
  facts: "工作日样本已核对。", question: "是否纳入周末样本？", recommendation: "先交工作日结果。",
  choices: ["仅工作日", "纳入周末"], authorization: { scope: "只读取现有周末样本。", impact: "需额外核对两批数据。" },
  created_at: timestamp, updated_at: timestamp,
} satisfies PendingDecision);
const receiptFor = (item: PendingDecision, text: string, option_index?: number) => ({
  id: `answer-${item.id}`, request_id: item.id, group_id: item.group_id, topic_id: item.topic_id, version: item.version,
  actor_id: "user", task_id: item.task?.task_id ?? "", task_revision: item.task?.task_revision ?? 0, text, option_index,
  reply_message_id: `reply-${item.id}`, wake_delivery_id: `delivery-${item.id}`,
  wake_message_id: item.relay === "origin_thread" ? `reply-${item.id}` : `wake-${item.id}`, created_at: timestamp,
});
const groupPath = (id: string) => `/api/v1/groups/${encodeURIComponent(id)}`;
const identityPath = (id: string) => `${groupPath(id)}/identity`;
const membership = (group_id: string, project_id: string) => ({ group_id, project_id, workspace_id: "workspace-1", channel_id: `channel:${group_id}` });
const pendingPath = "/api/v1/me/pending-decisions";
const answerPath = (item: PendingDecision, groupScoped = false) =>
  `${groupScoped ? groupPath(item.group_id) : "/api/v1/me"}/decisions/${encodeURIComponent(item.id)}/answer`;
const statusPath = (item: PendingDecision) => `${answerPath(item)}?${new URLSearchParams({
  group_id: item.group_id, topic_id: item.topic_id, version: String(item.version),
})}`;
const absent = () => json({ error: { code: "not_found" } }, 404);
const formFor = (name: string) => within(screen.getByRole("article", { name }));
const payments = "支付项目 · 告警 · 周末样本验证", settlement = "结算项目 · 告警 · 周末样本验证";
const fallback = "项目未确认 · 告警 · 周末样本验证";
const handling = { turn_id: "continuation-turn", attempt_id: "continuation-attempt", consumed_at: timestamp };
const storedValues = () => Object.values(sessionStorage).join("\n");
let PendingDecisions: typeof import("./PendingDecisions").PendingDecisions;
let setVisibility: (value: DocumentVisibilityState) => void;
async function reloadModule() {
  vi.resetModules();
  ({ PendingDecisions } = await import("./PendingDecisions"));
}

beforeEach(async () => {
  sessionStorage.clear();
  const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  setVisibility = value => { visibility.mockReturnValue(value); };
  vi.stubGlobal("crypto", { randomUUID, subtle: {
    async digest(_algorithm: string, bytes: Uint8Array) {
      return Uint8Array.from(createHash("sha256").update(bytes).digest()).buffer;
    },
  } });
  await reloadModule();
});
afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it("answers only the chosen same-name Task and replays the exact write after leaving the workbench page", async () => {
  const first = taskQuestion("payments", 7, "alerts/weekend"), second = taskQuestion("settlement", 2, "");
  const lost = deferred<Response>();
  const text = "  这次只看工作日。  ";
  const receipt = receiptFor(first, text);
  const post = vi.fn<typeof fetch>().mockReturnValueOnce(lost.promise)
    .mockResolvedValueOnce(json({ ...receipt, topic_id: second.topic_id })) // HTTP 200, wrong Thread.
    .mockResolvedValueOnce(json({ ...receipt, delivery_state: "accepted", run_state: "completed" }));
  let items = [first, second];
  const transport = vi.fn<typeof fetch>(async (input, init) => {
    if (init?.method === "POST") return post(input, init);
    switch (String(input)) {
    case `${pendingPath}?limit=50`: return page(items);
    case identityPath(first.group_id): return json(membership(first.group_id, "payments"));
    case identityPath(second.group_id): return json(membership(second.group_id, "settlement"));
    case "/api/v1/projects/payments": return json({ id: "payments", name: "支付项目", workspace_id: "workspace-1" });
    case "/api/v1/projects/settlement": return json({ id: "settlement", name: "结算项目", workspace_id: "workspace-1" });
    case statusPath(first): return absent();
    default: throw new Error(`Unexpected request: ${String(input)}`);
    }
  });
  vi.stubGlobal("fetch", transport);
  const view = render(<PendingDecisions active={false} />);
  expect(transport).not.toHaveBeenCalled();
  view.rerender(<PendingDecisions active />);
  expect(screen.getByText("正在载入待决事项…")).toBeVisible();
  const chosen = within(await screen.findByRole("article", { name: payments }));
  const unrelated = within(await screen.findByRole("article", { name: settlement }));
  const priorReceipts = screen.queryAllByText("答复已保存").length;
  for (const text of [first.facts, first.question, first.recommendation]) expect(chosen.getByText(text)).toBeVisible();
  expect(chosen.getByText(first.authorization.scope)).toBeVisible();
  expect(chosen.getByText(first.authorization.impact)).toBeVisible();
  fireEvent.change(chosen.getByRole("textbox", { name: "答复原文" }), { target: { value: text } });
  fireEvent.change(unrelated.getByRole("textbox", { name: "答复原文" }), { target: { value: "结算留待核对" } });
  const submit = chosen.getByRole("button", { name: "提交答复" });
  act(() => { fireEvent.click(submit); fireEvent.click(submit); });
  expect(submit).toBeDisabled();
  await waitFor(() => expect(post).toHaveBeenCalledTimes(1));
  const [path, init] = post.mock.calls[0];
  const key = new Headers(init?.headers).get("Idempotency-Key");
  expect(key).toBeTruthy();
  expect(path).toBe(`/api/v1/me/decisions/${first.id}/answer`);
  expect(init?.redirect).toBe("error");
  expect(JSON.parse(String(init?.body))).toEqual({ group_id: first.group_id, topic_id: first.topic_id, version: 7, text });

  // A lost response and an item missing from the list are not evidence of saving.
  view.unmount();
  items = [second];
  await act(async () => lost.resolve(json({}, 502)));
  render(<PendingDecisions active />);
  await screen.findByRole("article", { name: settlement });
  expect(formFor(settlement).getByRole("textbox", { name: "答复原文" })).toHaveValue("结算留待核对");
  const unresolved = () => within(screen.getByRole("region", { name: "尚未确认的答复" }));
  expect(await unresolved().findByRole("alert")).toHaveTextContent("答复结果未确认");
  expect(screen.queryAllByText("答复已保存")).toHaveLength(priorReceipts);
  const retry = () => fireEvent.click(unresolved().getByRole("button", { name: "重试同一次答复" }));
  retry();
  expect(await unresolved().findByRole("alert")).toHaveTextContent("答复结果未确认");
  expect(screen.queryAllByText("答复已保存")).toHaveLength(priorReceipts);
  retry();
  const saved = within((await screen.findByText(receipt.id)).closest("li")!);
  expect(saved.getByText("答复已保存")).toBeVisible();
  expect(saved.getByText(receipt.id)).toBeVisible();
  expect(saved.queryByText(/负责人已处理/)).not.toBeInTheDocument();
  expect(screen.queryByRole("article", { name: payments })).not.toBeInTheDocument();
  expect(formFor(settlement).getByRole("button", { name: "提交答复" })).toBeEnabled();
  expect(post).toHaveBeenCalledTimes(3);
  for (const [replayPath, replay] of post.mock.calls.slice(1)) {
    expect(replayPath).toBe(path);
    expect(replay?.body).toBe(init?.body);
    expect(new Headers(replay?.headers).get("Idempotency-Key")).toBe(key);
  }
});

it("filters by exact group, coalesces source reads, and requires a new selection after a stale version", async () => {
  const first = taskQuestion("scoped", 3, ""), foreign = taskQuestion("scoped-other", 1, "");
  const sameGroup = { ...taskQuestion("coalesced", 2, ""), group_id: first.group_id,
    task: { ...first.task, task_id: "task-coalesced", task_title: "复核通知" } };
  const newer = { ...first, version: 4, choices: ["纳入周末", "仅工作日"] };
  const post = vi.fn<typeof fetch>().mockResolvedValueOnce(json({ error: { code: "decision_conflict" } }, 409))
    .mockResolvedValueOnce(json(receiptFor(newer, "纳入周末", 1)));
  let items = [first, sameGroup, foreign];
  const transport = vi.fn<typeof fetch>(async input => {
    switch (String(input)) {
    case `${pendingPath}?limit=50`: return page(items);
    case identityPath(first.group_id): return json(membership(first.group_id, "looking-up"));
    case "/api/v1/projects/looking-up": return json({ id: "wrong-project", name: "不应出现的项目", workspace_id: "workspace-1" });
    default: throw new Error(`Unexpected request: ${String(input)}`);
    }
  });
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => init?.method === "POST" ? post(input, init) : transport(input, init));
  render(<PendingDecisions active groupId={first.group_id} />);
  const name = "项目未确认 · 告警 · 周末样本验证";
  await screen.findByRole("article", { name });
  expect(screen.getAllByRole("article")).toHaveLength(2);
  const sourceReads = () => transport.mock.calls.map(([url]) => String(url))
    .filter(url => url.endsWith("/identity") || url.startsWith("/api/v1/projects/"));
  const expectedProofReads = [identityPath(first.group_id), "/api/v1/projects/looking-up"];
  await waitFor(() => expect(sourceReads()).toEqual(expectedProofReads));
  expect(screen.queryByText("不应出现的项目", { exact: false })).not.toBeInTheDocument();
  expect(formFor(name).getByText(/项目未确认/)).toHaveTextContent(first.group_id);
  const choose = () => fireEvent.click(formFor(name).getByRole("radio", { name: "纳入周末" }));
  const submit = () => formFor(name).getByRole("button", { name: "提交答复" });
  choose(); fireEvent.click(submit());
  expect(await formFor(name).findByRole("alert")).toHaveTextContent("刷新待决事项");
  expect(submit()).toBeDisabled();
  items = [newer, sameGroup, foreign];
  fireEvent.click(screen.getByRole("button", { name: "刷新待决事项" }));
  expect(within(await screen.findByRole("article", { name })).getByText("决定版本 4")).toBeVisible();
  expect(submit()).toBeDisabled();
  choose(); fireEvent.click(submit());
  expect(await screen.findByText("答复已保存")).toBeVisible();
  expect(sourceReads()).toEqual(expectedProofReads); // The failed optional proof was cached in this group scope.
  expect(post).toHaveBeenCalledTimes(2);
  const keys = post.mock.calls.map(([path, init], index) => {
    expect(path).toBe(`${groupPath(first.group_id)}/decisions/${first.id}/answer`);
    expect(JSON.parse(String(init?.body))).toEqual({ group_id: first.group_id, topic_id: "", version: 3 + index, option_index: 2 - index });
    return new Headers(init?.headers).get("Idempotency-Key");
  });
  expect(keys[0]).toBeTruthy(); expect(keys[1]).toBeTruthy(); expect(keys[0]).not.toBe(keys[1]);
});

it("serves pending work before optional D reads and never uses a label from another workspace or scope", async () => {
  const first = taskQuestion("labels-late", 6, "alerts/late"), foreign = taskQuestion("labels-mismatch", 2, "");
  const broken = { ...taskQuestion("labels-broken", 1, ""), group_name: "告警故障演练" };
  const late = deferred<Response>(), failing = deferred<Response>();
  let identityReads = 0;
  const fetcher = vi.fn<typeof fetch>(async input => {
    switch (String(input)) {
    case `${pendingPath}?limit=50`: return page([first, foreign, broken], "older");
    case `${pendingPath}?limit=50&before=older`: return page([first]);
    case identityPath(first.group_id): return ++identityReads === 1 ? json(membership(first.group_id, "late")) : json({}, 503);
    case identityPath(foreign.group_id): return json(membership(foreign.group_id, "mismatch"));
    case identityPath(broken.group_id): return failing.promise;
    case "/api/v1/projects/late": return late.promise;
    case "/api/v1/projects/mismatch": return json({ id: "mismatch", workspace_id: "workspace-2", name: "不应出现的跨工作区项目" });
    default: throw new Error(`Unexpected request: ${String(input)}`);
    }
  });
  vi.stubGlobal("fetch", fetcher);
  const view = render(<PendingDecisions active />);
  const target = within((await screen.findAllByRole("article", { name: fallback }))[0]);
  fireEvent.change(target.getByRole("textbox"), { target: { value: "不用等待项目名称。" } });
  expect(target.getByRole("button", { name: "提交答复" })).toBeEnabled();
  expect(target.getByText(/项目未确认/)).toHaveTextContent(first.group_id);
  expect(screen.getByRole("button", { name: "加载更多待决事项" })).toBeEnabled();
  await act(async () => {
    late.resolve(json({ id: "late", workspace_id: "workspace-1", name: "稍后核实的项目" }));
    failing.resolve(json({}, 503));
  });
  const confirmed = "稍后核实的项目 · 告警 · 周末样本验证";
  expect(within(await screen.findByRole("article", { name: confirmed })).getByRole("textbox")).toHaveValue("不用等待项目名称。");
  expect(formFor(fallback).getByText(/项目未确认/)).toHaveTextContent(foreign.group_id);
  expect(formFor(fallback).queryByText("项目 ID")).not.toBeInTheDocument();
  expect(screen.queryByText(/不应出现的跨工作区项目/)).not.toBeInTheDocument();
  expect(formFor("项目未确认 · 告警故障演练 · 周末样本验证").getByText(/项目未确认/)).toHaveTextContent(broken.group_id);

  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "加载更多待决事项" })); });
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "刷新待决事项" })); });
  expect(screen.getByRole("article", { name: confirmed })).toBeVisible();
  expect(formFor(fallback).getByText(/项目未确认/)).toHaveTextContent(foreign.group_id);
  const readsFor = (path: string) => fetcher.mock.calls.filter(([url]) => url === path);
  const outsideScope = [identityPath(foreign.group_id), identityPath(broken.group_id), "/api/v1/projects/mismatch"];
  for (const path of [identityPath(first.group_id), "/api/v1/projects/late", ...outsideScope]) {
    expect(readsFor(path)).toHaveLength(1);
  }

  view.rerender(<PendingDecisions active groupId={first.group_id} />);
  expect(await screen.findByRole("article", { name: fallback })).toBeVisible();
  await waitFor(() => expect(identityReads).toBe(2));
  expect(screen.queryByRole("article", { name: confirmed })).not.toBeInTheDocument();
  expect(readsFor("/api/v1/projects/late")).toHaveLength(1);
  for (const path of outsideScope) expect(readsFor(path)).toHaveLength(1);
});

it("restores a committed lost response and native handling by exact history reads after a fresh page load", async () => {
  const item = taskQuestion("reload-committed", 11, "alerts/lost +"), text = "  不在浏览器日志中保留原文。  ";
  const receipt = receiptFor(item, text), lost = deferred<Response>(), history = deferred<Response>();
  const post = vi.fn<typeof fetch>(() => lost.promise);
  const status = vi.fn<typeof fetch>().mockReturnValueOnce(history.promise).mockResolvedValueOnce(json({ receipt, handling }));
  let items = [item];
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
    if (String(input) === `${pendingPath}?limit=50`) return Promise.resolve(page(items));
    if (String(input) === answerPath(item, true) && init?.method === "POST") return post(input, init);
    if (String(input) === statusPath(item) && init?.method === "GET") return status(input, init);
    return Promise.resolve(absent());
  });
  let view = render(<PendingDecisions active groupId={item.group_id} />);
  const card = within(await screen.findByRole("article", { name: fallback }));
  fireEvent.change(card.getByRole("textbox"), { target: { value: text } });
  fireEvent.click(card.getByRole("button", { name: "提交答复" }));
  await waitFor(() => expect(post).toHaveBeenCalledTimes(1));
  const key = new Headers(post.mock.calls[0][1]?.headers).get("Idempotency-Key")!;
  expect(storedValues()).toContain(key);
  for (const secret of [text, item.facts, item.task.task_title, item.task.mandate_id]) expect(storedValues()).not.toContain(secret);

  view.unmount(); items = [];
  await reloadModule();
  view = render(<PendingDecisions active groupId="another-group" />);
  expect(await screen.findByText("没有待决事项。")).toBeVisible();
  expect(status).not.toHaveBeenCalled();
  view.rerender(<PendingDecisions active />);
  expect(await screen.findByText(/答复结果未确认/)).toBeVisible();
  expect(screen.queryByText("答复已保存")).not.toBeInTheDocument();
  expect(post).toHaveBeenCalledTimes(1);
  await act(async () => history.resolve(json({ receipt, handling })));
  expect(await screen.findByText(receipt.id)).toBeVisible();
  expect(screen.getByText("答复已保存")).toBeVisible();
  expect(screen.getByText("负责人已处理")).toBeVisible();
  expect(storedValues()).not.toContain(key);
  expect(storedValues()).not.toContain(text);

  view.unmount();
  await reloadModule();
  render(<PendingDecisions active />);
  expect(await screen.findByText("负责人已处理")).toBeVisible();
  expect(screen.getByText(receipt.id)).toBeVisible();
  expect(status).toHaveBeenCalledTimes(2);
  for (const [path, init] of status.mock.calls) {
    expect(path).toBe(statusPath(item));
    expect(init).toMatchObject({ method: "GET", redirect: "error", cache: "no-store" });
  }
  expect(post).toHaveBeenCalledTimes(1);
});

it("shows a saved Thread answer separately from its exact GET acknowledgement", async () => {
  vi.useFakeTimers();
  const item = {
    id: "question/thread", version: 4, kind: "clarification", group_id: "group/thread", topic_id: "alerts/weekend +",
    group_name: "Alerts", recipient_id: "user", relay: "origin_thread",
    chat: { source_id: "chat-source", candidate_id: "candidate", assessment_id: "assessment", notice_message_id: "notice" },
    source_message_id: "message", source_sequence: 2, author_member_id: "owner", author_turn_id: "question-turn",
    facts: "The weekend sample is available.", question: "Include the weekend sample?", recommendation: "Use weekdays first.",
    created_at: timestamp, updated_at: timestamp,
  } satisfies PendingDecision;
  const text = "Use weekday samples only.";
  const { attempt } = await prepareDecisionAnswer(item, { text }, randomUUID(), item.group_id);
  expect(journalDecisionAttempt(attempt)).not.toBeNull();
  const receipt = receiptFor(item, text), ack = { turn_id: "continued-thread-turn", consumed_at: timestamp };
  const status = deferred<Response>();
  const reads = vi.fn<typeof fetch>().mockReturnValueOnce(status.promise);
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) =>
    String(input) === `${pendingPath}?limit=50` ? Promise.resolve(page()) : reads(input, init));

  render(<PendingDecisions active groupId={item.group_id} />);
  expect(reads).toHaveBeenCalledTimes(1);
  expect(screen.getByText(/答复结果未确认/)).toBeVisible();
  expect(screen.queryByText("答复已保存")).not.toBeInTheDocument();
  expect(screen.queryByText("负责人已处理")).not.toBeInTheDocument();

  await act(async () => status.resolve(json({ receipt, handling: null, delivery_state: "accepted", run_state: "completed" })));
  const saved = within(screen.getByText(receipt.id).closest("li")!);
  function expectSavedWithoutHandling() {
    expect(saved.getByText("答复已保存")).toBeVisible();
    expect(saved.getByText("尚未确认负责人处理。")).toBeVisible();
    expect(saved.queryByText("负责人已处理")).not.toBeInTheDocument();
  }
  expectSavedWithoutHandling();

  async function readStatus(reply: unknown) {
    const before = reads.mock.calls.length;
    reads.mockResolvedValueOnce(json(reply));
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "刷新待决事项" })); });
    expect(reads).toHaveBeenCalledTimes(before + 1);
  }
  await readStatus({ receipt, handling: { ...ack, attempt_id: handling.attempt_id } });
  expectSavedWithoutHandling();

  const changedDelivery = { wake_delivery_id: "different-delivery" };
  for (const mismatch of [
    { request_id: "different-request" }, { group_id: "different-group" }, { topic_id: "different-thread" },
    { version: item.version + 1 }, { task_id: "unexpected-task" }, { task_revision: 1 },
    { id: "different-answer" }, changedDelivery, { text: "Use weekend samples too." },
  ]) {
    await readStatus({ receipt: { ...receipt, ...mismatch }, handling: ack });
    expectSavedWithoutHandling();
  }

  // On reload, the journal digest must bind the receipt without an in-memory copy.
  const [history] = loadDecisionJournal() ?? [];
  expect(history).toHaveProperty("receiptDigest");
  reads.mockResolvedValueOnce(json({ receipt: { ...receipt, ...changedDelivery }, handling: ack }));
  await expect(readDecisionAnswer(history, new AbortController().signal)).rejects.toMatchObject({ code: "invalid_response" });

  await readStatus({ receipt, handling: ack });
  for (const [path, init] of reads.mock.calls) {
    expect(path).toBe(statusPath(item));
    expect(init).toMatchObject({ method: "GET", redirect: "error", cache: "no-store" });
  }
  expect(saved.getByText("答复已保存")).toBeVisible();
  expect(saved.getByText("负责人已处理")).toBeVisible();
  expect(saved.queryByText("尚未确认负责人处理。")).not.toBeInTheDocument();
});

it("rejects Task handling without a valid Attempt ID", async () => {
  const item = taskQuestion("missing-ack-attempt", 3, "alerts"), text = "Use weekday samples only.";
  const { attempt } = await prepareDecisionAnswer(item, { text }, randomUUID());
  const receipt = receiptFor(item, text), fetcher = vi.fn<typeof fetch>();
  vi.stubGlobal("fetch", fetcher);
  for (const attempt_id of [undefined, "", " "]) {
    fetcher.mockResolvedValueOnce(json({ receipt, handling: { ...handling, attempt_id } }));
    await expect(readDecisionAnswer(attempt, new AbortController().signal)).rejects.toMatchObject({ code: "invalid_response" });
  }
});

it("retries a recovered attempt only on explicit matching input after checking exact history", async () => {
  const item = taskQuestion("reload-retry", 5, ""), text = "  原样重输；保留空格。  ";
  const receipt = receiptFor(item, text);
  const post = vi.fn<typeof fetch>().mockResolvedValueOnce(json({}, 502)).mockResolvedValueOnce(json(receipt));
  let ambiguous = true, current = item;
  const reads = vi.fn<typeof fetch>(async () => ambiguous ? json({ receipt: { ...receipt, text: "别的答复" }, handling }) : absent());
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
    if (String(input) === `${pendingPath}?limit=50`) return Promise.resolve(page([current]));
    if (init?.method === "POST") return post(input, init);
    if (String(input) === statusPath(item)) return reads(input, init);
    return Promise.resolve(absent());
  });
  const first = render(<PendingDecisions active groupId={item.group_id} />);
  const enter = (value: string) => fireEvent.change(formFor(fallback).getByRole("textbox"), { target: { value } });
  await screen.findByRole("article", { name: fallback });
  enter(text); fireEvent.click(formFor(fallback).getByRole("button", { name: "提交答复" }));
  await screen.findByText(/答复结果未确认/);
  first.unmount();
  await reloadModule();
  render(<PendingDecisions active />);
  await screen.findByRole("article", { name: fallback });
  expect(formFor(fallback).getByRole("textbox")).toHaveValue("");
  expect(post).toHaveBeenCalledTimes(1);
  expect(screen.queryByText("答复已保存")).not.toBeInTheDocument();
  const retry = () => fireEvent.click(formFor(fallback).getByRole("button", { name: "重试同一次答复" }));
  enter(text.trim()); await act(async () => retry());
  expect(screen.getByRole("alert")).toHaveTextContent(/不一致/);
  expect(post).toHaveBeenCalledTimes(1);
  enter(text); await act(async () => retry());
  expect(screen.getByRole("alert")).toHaveTextContent(/答复结果未确认/);
  expect(screen.queryByText("负责人已处理")).not.toBeInTheDocument();
  expect(post).toHaveBeenCalledTimes(1);

  const refresh = () => fireEvent.click(screen.getByRole("button", { name: "刷新待决事项" }));
  current = { ...item, version: item.version + 1 };
  refresh();
  const currentCard = within(await screen.findByRole("article", { name: fallback }));
  expect(currentCard.getByText(`决定版本 ${current.version}`)).toBeVisible();
  expect(currentCard.getByRole("textbox")).toHaveValue("");
  const history = within(screen.getByRole("region", { name: "尚未确认的答复" }));
  expect(history.getByRole("alert")).toHaveTextContent("答复结果未确认");
  expect(post).toHaveBeenCalledTimes(1);

  ambiguous = false; current = item;
  const before = reads.mock.calls.length;
  refresh();
  await waitFor(() => expect(reads.mock.calls.length).toBeGreaterThan(before));
  await screen.findByRole("article", { name: fallback });
  expect(post).toHaveBeenCalledTimes(1);
  const priorReads = reads.mock.calls.length;
  retry();
  expect(await screen.findByText("答复已保存")).toBeVisible();
  expect(post).toHaveBeenCalledTimes(2);
  const identity = ([path, init]: Parameters<typeof fetch>) => ({ path, body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key") });
  expect(identity(post.mock.calls[0])).toMatchObject({ path: answerPath(item, true), key: expect.any(String) });
  expect(identity(post.mock.calls[1])).toEqual(identity(post.mock.calls[0]));
  expect(reads.mock.invocationCallOrder.slice(priorReads).some(call => call < post.mock.invocationCallOrder[1])).toBe(true);
});

it.each(["same", "newer"] as const)("leaves a hanging POST uncertain and lets the %s version proceed", async version => {
  vi.useFakeTimers();
  const tick = (ms: number) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });
  const older = taskQuestion("unknown-version", 2, "alerts"), newer = { ...older, version: 3 };
  const target = version === "newer" ? newer : older;
  const hanging = deferred<Response>(); // Never settles, even when the client aborts.
  const post = vi.fn<typeof fetch>().mockReturnValueOnce(hanging.promise)
    .mockResolvedValueOnce(json(receiptFor(target, target.choices[1], 2)));
  const reads = vi.fn<typeof fetch>(async () => absent());
  let current = older;
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
    if (String(input) === `${pendingPath}?limit=50`) return Promise.resolve(page([current]));
    if (init?.method === "POST") return post(input, init);
    if (String(input) === statusPath(older)) return reads(input, init);
    return Promise.resolve(absent());
  });
  render(<PendingDecisions active />);
  await tick(0);
  const choice = () => formFor(fallback).getByRole("radio", { name: "纳入周末" });
  const submit = () => formFor(fallback).getByRole("button", { name: "提交答复" });
  fireEvent.click(choice()); fireEvent.click(submit());
  await tick(0);
  expect(post).toHaveBeenCalledTimes(1);
  const [firstPath, first] = post.mock.calls[0];
  const key = new Headers(first?.headers).get("Idempotency-Key")!;
  expect(firstPath).toBe(answerPath(older));
  expect(key).toBeTruthy();
  if (version === "newer") {
    current = newer;
    fireEvent.click(screen.getByRole("button", { name: "刷新待决事项" }));
    await tick(0);
    expect(formFor(fallback).getByText(`决定版本 ${newer.version}`)).toBeVisible();
  }

  await tick(9999);
  expect(first?.signal?.aborted).toBe(false);
  expect(choice()).toBeDisabled();
  expect(reads).not.toHaveBeenCalled();
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  await tick(1);
  expect(first?.signal?.aborted).toBe(true);
  expect(screen.getByRole("alert")).toHaveTextContent("答复结果未确认");
  expect(screen.queryByText("答复已保存")).not.toBeInTheDocument();
  expect(storedValues()).toContain(key);
  expect(post).toHaveBeenCalledTimes(1);
  expect(reads).toHaveBeenCalledTimes(1);
  expect(reads).toHaveBeenCalledWith(statusPath(older), expect.objectContaining({ method: "GET", cache: "no-store", redirect: "error" }));

  if (version === "newer") {
    const history = within(screen.getByRole("region", { name: "尚未确认的答复" }));
    expect(history.getByText(`决定版本 ${older.version}`, { exact: false }))
      .toHaveTextContent(`事项 ${older.id} · 决定版本 ${older.version}`);
    expect(history.queryByRole("button", { name: "重试同一次答复" })).not.toBeInTheDocument();
    expect(choice()).toBeEnabled();
    expect(submit()).toBeDisabled(); // A new version requires a fresh selection.
    fireEvent.click(choice());
    expect(submit()).toBeEnabled();
    fireEvent.click(submit());
  } else {
    expect(submit()).toBeDisabled();
    fireEvent.click(formFor(fallback).getByRole("button", { name: "重试同一次答复" }));
  }
  await tick(0);
  expect(screen.getByText("答复已保存")).toBeVisible();
  expect(post).toHaveBeenCalledTimes(2);
  const [path, next] = post.mock.calls[1];
  const nextKey = new Headers(next?.headers).get("Idempotency-Key");
  expect(path).toBe(answerPath(target));
  expect(next?.signal?.aborted).toBe(false);
  if (version === "newer") {
    expect(screen.getByRole("alert")).toHaveTextContent("答复结果未确认");
    expect(JSON.parse(String(next?.body))).toEqual({ group_id: newer.group_id, topic_id: newer.topic_id, version: newer.version, option_index: 2 });
    expect(nextKey).not.toBe(key);
  } else {
    expect(next?.body).toBe(first?.body);
    expect(nextKey).toBe(key);
    expect(reads.mock.invocationCallOrder[1]).toBeLessThan(post.mock.invocationCallOrder[1]);
  }
});

it.each(["disabled", "at quota"])("sends and explicitly replays the same answer when sessionStorage is %s", async storage => {
  const item = taskQuestion("journal-unavailable", 1, ""), text = "  保留答复原文。  ";
  const receipt = receiptFor(item, text);
  const post = vi.fn<typeof fetch>().mockResolvedValueOnce(json({}, 502)).mockResolvedValueOnce(json(receipt));
  const unavailable = () => { throw new DOMException("Storage unavailable", storage === "disabled" ? "SecurityError" : "QuotaExceededError"); };
  if (storage === "disabled") vi.spyOn(Storage.prototype, "getItem").mockImplementation(unavailable);
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(unavailable);
  const reads = vi.fn<typeof fetch>(async () => absent());
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
    if (String(input) === `${pendingPath}?limit=50`) return Promise.resolve(page([item]));
    if (String(input) === answerPath(item) && init?.method === "POST") return post(input, init);
    if (String(input) === statusPath(item) && init?.method === "GET") return reads(input, init);
    return Promise.resolve(absent());
  });
  const view = render(<PendingDecisions active />);
  const card = within(await screen.findByRole("article", { name: fallback }));
  fireEvent.change(card.getByRole("textbox"), { target: { value: text } });
  fireEvent.click(card.getByRole("button", { name: "提交答复" }));
  expect(await card.findByRole("alert")).toHaveTextContent("答复结果未确认");
  expect(post).toHaveBeenCalledTimes(1);

  view.rerender(<PendingDecisions active={false} />);
  view.rerender(<PendingDecisions active />);
  const reopened = within(await screen.findByRole("article", { name: fallback }));
  expect(screen.queryByText("答复已保存")).not.toBeInTheDocument();
  expect(post).toHaveBeenCalledTimes(1);
  const priorReads = reads.mock.calls.length;
  fireEvent.click(reopened.getByRole("button", { name: "重试同一次答复" }));
  expect(await screen.findByText(receipt.id)).toBeVisible();
  expect(screen.getByText("答复已保存")).toBeVisible();
  expect(post).toHaveBeenCalledTimes(2);
  expect(storedValues()).not.toContain(text);
  const [[path, init], [replayPath, replay]] = post.mock.calls;
  const key = new Headers(init?.headers).get("Idempotency-Key");
  expect(key).toBeTruthy();
  expect(path).toBe(answerPath(item));
  expect(JSON.parse(String(init?.body))).toEqual({ group_id: item.group_id, topic_id: "", version: 1, text });
  expect(replayPath).toBe(path);
  expect(replay?.body).toBe(init?.body);
  expect(new Headers(replay?.headers).get("Idempotency-Key")).toBe(key);
  expect(reads.mock.invocationCallOrder.slice(priorReads).some(call => call < post.mock.invocationCallOrder[1])).toBe(true);
});

it.each(["handled", "uncertain", "hidden", "budget"] as const)("checks for exact handling with %s", async ending => {
  vi.useFakeTimers();
  const item = taskQuestion("poll-ack", 3, ""), receipt = receiptFor(item, item.choices[1], 2);
  const uncertain = ending === "uncertain";
  let ack: typeof handling | null = null;
  const reads = vi.fn<typeof fetch>(async () => uncertain && !ack ? absent()
    : json({ receipt, handling: ack, delivery_state: "accepted", run_state: "completed" }));
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
    if (String(input) === `${pendingPath}?limit=50`) return Promise.resolve(page([item]));
    if (String(input) === statusPath(item)) return reads(input, init);
    if (init?.method === "POST") return Promise.resolve(uncertain ? json({}, 502)
      : json({ ...receipt, delivery_state: "accepted", run_state: "completed" }));
    return Promise.resolve(absent());
  });
  render(<PendingDecisions active />);
  await act(async () => { await vi.advanceTimersByTimeAsync(0); });
  fireEvent.click(formFor(fallback).getByRole("radio", { name: "纳入周末" }));
  await act(async () => {
    fireEvent.click(formFor(fallback).getByRole("button", { name: "提交答复" }));
    await vi.advanceTimersByTimeAsync(0);
  });
  if (uncertain) {
    expect(screen.getByText(/答复结果未确认/)).toBeVisible();
    expect(screen.queryByText("答复已保存")).not.toBeInTheDocument();
  } else {
    expect(screen.getByText("答复已保存")).toBeVisible();
    expect(screen.getByText("尚未确认负责人处理。")).toBeVisible();
  }
  expect(screen.queryByText("负责人已处理")).not.toBeInTheDocument();
  expect(reads).toHaveBeenCalledTimes(1);

  if (ending === "handled" || uncertain) {
    ack = handling;
    await act(async () => { await vi.advanceTimersByTimeAsync(4000); });
    expect(screen.getByText("答复已保存")).toBeVisible();
    expect(screen.getByText("负责人已处理")).toBeVisible();
    expect(reads).toHaveBeenCalledTimes(2);
  } else if (ending === "hidden") {
    setVisibility("hidden"); // No visibilitychange event is required to suspend the next read.
  } else {
    await act(async () => { await vi.advanceTimersByTimeAsync(120000); });
    expect(reads.mock.calls.length).toBeGreaterThan(1);
    expect(reads.mock.calls.length).toBeLessThanOrEqual(7);
  }
  const stopped = reads.mock.calls.length;
  await act(async () => { await vi.advanceTimersByTimeAsync(120000); });
  expect(reads).toHaveBeenCalledTimes(stopped);
  if (ending === "hidden") expect(reads).toHaveBeenCalledTimes(1);
  if (ending === "budget") {
    ack = handling;
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "刷新待决事项" })); });
    expect(screen.getByText("负责人已处理")).toBeVisible();
    expect(reads).toHaveBeenCalledTimes(stopped + 1);
  }
});

it.each(["visibility", "write"] as const)("rotates bounded status reads across five records after %s", async trigger => {
  vi.useFakeTimers();
  setVisibility("hidden");
  const tick = (ms: number) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });
  const items = Array.from({ length: 5 }, (_, i) => ({ ...taskQuestion(`poll-${i}`, 1, ""), group_name: `告警 ${i}` }));
  const receipts = items.map(item => receiptFor(item, item.choices[1], 2));
  const posts = vi.fn<typeof fetch>(async input => {
    const index = items.findIndex(item => answerPath(item) === String(input));
    if (index < 0) throw new Error(`Unexpected write: ${String(input)}`);
    return index === 0 ? json({}, 502) : json(receipts[index]);
  });
  const visits = new Map<string, number>();
  let inFlight = 0, peak = 0, acknowledged = false;
  vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = String(input);
    if (path === `${pendingPath}?limit=50`) return page(items);
    if (init?.method === "POST") return posts(input, init);
    const index = items.findIndex(item => statusPath(item) === path);
    if (index < 0 || init?.method !== "GET") return absent();
    visits.set(path, (visits.get(path) ?? 0) + 1);
    peak = Math.max(peak, ++inFlight);
    await new Promise<void>(resolve => setTimeout(resolve, 100));
    inFlight--;
    if (index === 0 && !acknowledged) return absent();
    return json({ receipt: receipts[index], handling: index === 0 ? handling : null });
  });
  render(<PendingDecisions active />);
  await tick(0);
  for (const item of items) {
    if (trigger === "write" && item === items.at(-1)) setVisibility("visible");
    const card = formFor(`项目未确认 · ${item.group_name} · 周末样本验证`);
    fireEvent.click(card.getByRole("radio", { name: "纳入周末" }));
    fireEvent.click(card.getByRole("button", { name: "提交答复" }));
    await tick(0);
  }
  if (trigger === "visibility") {
    expect(visits.size).toBe(0);
    setVisibility("visible");
    act(() => { document.dispatchEvent(new Event("visibilitychange")); });
  }
  await tick(1000);
  const initial = trigger === "visibility" ? items.length : 4;
  expect(visits.size).toBe(initial);
  expect(screen.getByRole("alert")).toHaveTextContent("答复结果未确认");
  expect(screen.queryByText("负责人已处理")).not.toBeInTheDocument();

  acknowledged = true; // The other four remain unhandled for the whole wave.
  await tick(120000);
  expect(visits.get(statusPath(items[0]))).toBe(trigger === "visibility" ? 2 : 1);
  for (const item of items.slice(1)) expect(visits.get(statusPath(item))).toBeGreaterThan(1);
  expect(screen.getByText(receipts[0].id)).toBeVisible();
  expect(screen.getByText("负责人已处理")).toBeVisible();
  expect(posts).toHaveBeenCalledTimes(items.length);
  expect(peak).toBe(4);
  const totalReads = () => [...visits.values()].reduce((sum, count) => sum + count, 0);
  const stopped = totalReads();
  expect(stopped).toBeLessThanOrEqual(initial + 6 * 4);
  await tick(120000);
  expect(totalReads()).toBe(stopped);
});

it("gives all 33 unresolved attempts a turn without expanding the eager pass", async () => {
  vi.useFakeTimers();
  setVisibility("hidden");
  const tick = (ms: number) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });
  const items = Array.from({ length: 33 }, (_, i) => taskQuestion(`poll-overflow-${i}`, 1, ""));
  const journaled = items.slice(0, -1), overflow = items.at(-1)!;
  // Restore the first 32 attempts from the journal; only the overflow needs a form.
  for (const item of journaled) {
    const { attempt } = await prepareDecisionAnswer(item, { option_index: 2 }, randomUUID());
    expect(journalDecisionAttempt(attempt)).not.toBeNull();
  }
  const paths = items.map(statusPath);
  const reads: string[] = [], posts: [string, RequestInit][] = [];
  let inFlight = 0, peak = 0;
  vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = String(input);
    if (init?.method === "POST") {
      posts.push([path, init]);
      return json({}, 502);
    }
    if (path === `${pendingPath}?limit=50`) return page([overflow]);
    if ((init?.method ?? "GET") !== "GET" || !path.split("?")[0].endsWith("/answer")) return absent();
    reads.push(path);
    peak = Math.max(peak, ++inFlight);
    await new Promise<void>(resolve => setTimeout(resolve, 100));
    inFlight--;
    return absent();
  });
  render(<PendingDecisions active />);
  await tick(0);
  await tick(180000);
  expect(reads).toHaveLength(0);
  expect(posts).toHaveLength(0);

  // The last write is memory-only and wakes checks without a visibility event.
  setVisibility("visible");
  const card = formFor(fallback);
  fireEvent.click(card.getByRole("radio", { name: "纳入周末" }));
  fireEvent.click(card.getByRole("button", { name: "提交答复" }));
  await tick(1000);
  expect(screen.getAllByRole("alert")).toHaveLength(33);
  expect(loadDecisionJournal()?.map(entry => entry.ref.request_id)).toEqual(journaled.map(item => item.id));
  expect(reads).toHaveLength(4);
  expect(reads[0]).toBe(paths.at(-1));
  await tick(180000);
  expect(new Set(reads)).toEqual(new Set(paths));
  const stopped = reads.length;
  await tick(180000);
  expect(reads).toHaveLength(stopped);

  act(() => { document.dispatchEvent(new Event("visibilitychange")); });
  await tick(1000);
  const eager = reads.slice(stopped);
  expect(eager).toHaveLength(32);
  const skipped = paths.filter(path => !eager.includes(path));
  expect(skipped).toHaveLength(1);

  // Interrupt before backoff: the skipped record is now least recently checked.
  const restarted = reads.length;
  fireEvent.click(screen.getByRole("button", { name: "刷新待决事项" }));
  await tick(1000);
  expect(reads[restarted]).toBe(skipped[0]);
  await tick(2000);
  expect(new Set(reads.slice(restarted))).toEqual(new Set(paths));
  expect(peak).toBe(4);
  expect(posts).toHaveLength(1);
  const [[path, init]] = posts;
  expect(path).toBe(answerPath(overflow));
  expect(JSON.parse(String(init.body))).toEqual({
    group_id: overflow.group_id, topic_id: overflow.topic_id, version: overflow.version, option_index: 2,
  });
  expect(new Headers(init.headers).get("Idempotency-Key")).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
});

it("keeps a migration-absent decision inbox optional alongside the real review inbox", async () => {
  const reads = vi.fn<typeof fetch>().mockResolvedValueOnce(json({ error: { code: "decision_answers_unavailable" } }, 503))
    .mockResolvedValueOnce(page());
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => String(input) === `${pendingPath}?limit=50`
    ? reads(input, init) : Promise.resolve(String(input) === "/api/v1/me/pending-reviews?limit=50" ? page() : json({}, 404)));
  render(<><PendingDecisions active /><PendingReviews active /></>);
  expect(await screen.findByText("待决答复暂不可用。")).toBeVisible();
  expect(await screen.findByText("没有待验收事项。")).toBeVisible();
  expect(screen.queryByText("没有待决事项。")).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "重试载入待决事项" }));
  expect(await screen.findByText("没有待决事项。")).toBeVisible();
});
