import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { PendingDecisions } from "./PendingDecisions";
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
});
const receiptFor = (item: ReturnType<typeof taskQuestion>, text: string, option_index?: number) => ({
  id: `answer-${item.id}`, request_id: item.id, group_id: item.group_id, topic_id: item.topic_id, version: item.version,
  actor_id: "user", task_id: item.task.task_id, task_revision: item.task.task_revision, text, option_index,
  reply_message_id: `reply-${item.id}`, wake_delivery_id: `delivery-${item.id}`, wake_message_id: `wake-${item.id}`, created_at: timestamp,
});
const groupPath = (id: string) => `/api/v1/groups/${encodeURIComponent(id)}`;
const identityPath = (id: string) => `${groupPath(id)}/identity`;
const membership = (group_id: string, project_id: string) => ({ group_id, project_id, workspace_id: "workspace-1", channel_id: `channel:${group_id}` });
const pendingPath = "/api/v1/me/pending-decisions";
const formFor = (name: string) => within(screen.getByRole("article", { name }));
const payments = "支付项目 · 告警 · 周末样本验证", settlement = "结算项目 · 告警 · 周末样本验证";

afterEach(() => vi.unstubAllGlobals());

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
    case "/api/v1/projects/payments": return json({ id: "payments", name: "支付项目" });
    case "/api/v1/projects/settlement": return json({ id: "settlement", name: "结算项目" });
    default: throw new Error(`Unexpected request: ${String(input)}`);
    }
  });
  vi.stubGlobal("fetch", transport);
  const view = render(<PendingDecisions active={false} />);
  expect(transport).not.toHaveBeenCalled();
  view.rerender(<PendingDecisions active />);
  expect(screen.getByText("正在载入待决事项…")).toBeVisible();
  await screen.findByRole("article", { name: payments });
  const chosen = formFor(payments), unrelated = formFor(settlement);
  const priorReceipts = screen.queryAllByText("答复已保存").length;
  for (const text of [first.facts, first.question, first.recommendation]) expect(chosen.getByText(text)).toBeVisible();
  expect(chosen.getByText(first.authorization.scope)).toBeVisible();
  expect(chosen.getByText(first.authorization.impact)).toBeVisible();
  fireEvent.change(chosen.getByRole("textbox", { name: "答复原文" }), { target: { value: text } });
  fireEvent.change(unrelated.getByRole("textbox", { name: "答复原文" }), { target: { value: "结算留待核对" } });
  const submit = chosen.getByRole("button", { name: "提交答复" });
  act(() => { fireEvent.click(submit); fireEvent.click(submit); });
  expect(submit).toBeDisabled();
  expect(post).toHaveBeenCalledTimes(1);
  const [path, init] = post.mock.calls[0];
  const key = new Headers(init?.headers).get("Idempotency-Key");
  expect(key).toBeTruthy();
  expect(path).toBe(`/api/v1/me/decisions/${first.id}/answer`);
  expect(init?.redirect).toBe("error");
  expect(JSON.parse(String(init?.body))).toEqual({ group_id: first.group_id, topic_id: first.topic_id, version: 7, text });

  // The server committed but the browser lost the response. Reloading the
  // list does not return this item and is not evidence of the answer itself.
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
    case "/api/v1/projects/looking-up": return json({ id: "wrong-project", name: "不应出现的项目" });
    default: throw new Error(`Unexpected request: ${String(input)}`);
    }
  });
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => init?.method === "POST" ? post(input, init) : transport(input, init));
  render(<PendingDecisions active groupId={first.group_id} />);
  const name = "项目未确认 · 告警 · 周末样本验证";
  await screen.findByRole("article", { name });
  expect(screen.getAllByRole("article")).toHaveLength(2);
  expect(transport.mock.calls.map(([url]) => url)).toEqual([
    `${pendingPath}?limit=50`, identityPath(first.group_id), "/api/v1/projects/looking-up",
  ]);
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
  expect(post).toHaveBeenCalledTimes(2);
  const keys = post.mock.calls.map(([path, init], index) => {
    expect(path).toBe(`${groupPath(first.group_id)}/decisions/${first.id}/answer`);
    expect(JSON.parse(String(init?.body))).toEqual({ group_id: first.group_id, topic_id: "", version: 3 + index, option_index: 2 - index });
    return new Headers(init?.headers).get("Idempotency-Key");
  });
  expect(keys[0]).toBeTruthy(); expect(keys[1]).toBeTruthy(); expect(keys[0]).not.toBe(keys[1]);
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
