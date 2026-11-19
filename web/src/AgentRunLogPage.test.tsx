import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { AgentRunLogPage } from "./AgentRunLogPage";
import type { AgentRunDetail, AgentRunEntry, AgentRunSummary, AgentRunWorkspaceOperation } from "./agent-runs-api";
import { deferred, json, page } from "./test/groups";

const transport = vi.fn<typeof fetch>();
const runsPath = "/api/v1/me/agent-runs";
const urlAt = (index: number) => new URL(String(transport.mock.calls[index][0]), "http://localhost");
const stamp = (second: number) => `2026-11-19T10:00:${String(second).padStart(2, "0")}.123456789Z`;
const first: AgentRunSummary = {
  run_id: "run/1?+", group_id: "group/1?+", group_name: "原群", member_id: "agent-1", member_name: "同名 Agent",
  state: "unknown", created_at: stamp(2), updated_at: stamp(10),
};
const second: AgentRunSummary = { ...first, run_id: "run-2", group_id: "group-2", group_name: "另一群", member_id: "agent-2", state: "succeeded" };
const pick = (run: AgentRunSummary) => screen.getByRole("button", { name: `${run.group_name} · 运行 ${run.run_id}` });
const evidence = () => within(screen.getByRole("region", { name: "本轮证据" }));
const timeline = () => within(evidence().getByRole("list", { name: "事件时间线" }));
const timelineLabels = () => timeline().getAllByRole("article").map(node => node.getAttribute("aria-label"));
const chooseAgent = (id: string) => fireEvent.change(screen.getByRole("combobox", { name: "Agent 范围" }), { target: { value: id } });
const flush = () => act(async () => {});
const tick = (ms = 5000) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });

const trigger = (id: string, preview: string, truncated = false) => ({
  kind: "trigger_message" as const, id: `message:${id}`, created_at: stamp(1),
  message: { message_id: id, sender_id: "user", preview, truncated },
});
const reply = (id: string, preview: string, run: AgentRunSummary, time = stamp(3), truncated = false) => ({
  kind: "published_reply" as const, id: `message:${id}`, created_at: time,
  message: { message_id: id, sender_id: run.member_id, preview, truncated },
});
const assigned = { id: "dispatch_decision:decision-1", kind: "dispatch_decision", created_at: stamp(4), dispatch_decision: {
  decision_id: "decision-1", request_id: "request-1", task_id: "task-1", kind: "assign", reason_preview: "按现有分工处理", truncated: true,
  worker_member_id: "worker-1", assignment_id: "assignment-1",
} } satisfies AgentRunEntry;
const hash = "c".repeat(64);
function operation(id: string, state: AgentRunWorkspaceOperation["state"], second: number) {
  return {
    id: `workspace:${id}`, kind: "workspace_operation" as const, created_at: stamp(second),
    workspace_operation: {
      operation_id: id, task_id: "task-1", attempt_id: "attempt-1", kind: "replace" as const,
      relative_path: "private-relative/file.txt", state, result: state === "pending" ? null : {
        observed_hash: state === "unknown" ? "" : hash, observed_size: state === "unknown" ? 0 : 12,
        error_code: state === "unknown" ? "io_unknown" : state === "applied_sync_error" ? "sync_failed" : "", finished_at: stamp(9),
      },
    },
  };
}
const view = (run: AgentRunSummary, items: AgentRunEntry[], next_cursor = "", receipt: AgentRunDetail["receipt"] = null) => ({
  run, accepted_at: null, cancel_requested_at: null, receipt, timeline: { items, next_cursor },
});
const successReceipt = { outcome: "succeeded", observed_terminal: "succeeded", started: true,
  accepted: true, delivery_failed: false, direct_child_reaped: true, output_complete: true } satisfies AgentRunDetail["receipt"];
function expand(summary: HTMLElement) {
  const region = summary.closest("details")!;
  expect(region).not.toHaveAttribute("open");
  fireEvent.click(summary);
  expect(region).toHaveAttribute("open");
  return region;
}

beforeEach(() => { transport.mockReset(); vi.stubGlobal("fetch", transport); });
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });

it("renders a global empty run list without requiring a selected chat", async () => {
  transport.mockResolvedValueOnce(page());
  render(<AgentRunLogPage active />);
  expect(screen.getByRole("heading", { name: "Agent 运行日志" })).toBeVisible();
  expect(await screen.findByText("暂无可查看的运行记录。")).toBeVisible();
  expect(String(transport.mock.calls[0][0])).toMatch(/^\/api\/v1\/me\/agent-runs\?limit=\d+$/);
});

it("groups same-name Agents by member ID and paginates global and member scopes independently", async () => {
  const older = { ...first, run_id: "older-1", created_at: stamp(0) };
  const otherOlder = { ...second, run_id: "older-2", created_at: stamp(0) };
  transport.mockResolvedValueOnce(page([first, second], "all+cursor/&")).mockResolvedValueOnce(page([older]))
    .mockResolvedValueOnce(page([second], "member+cursor/?")).mockResolvedValueOnce(page([otherOlder]));
  render(<AgentRunLogPage active />);
  expect(await screen.findByRole("option", { name: "同名 Agent · 原群 · agent-1" })).toHaveValue(first.member_id);
  expect(screen.getByRole("option", { name: "同名 Agent · 另一群 · agent-2" })).toHaveValue(second.member_id);
  const firstList = within(screen.getByRole("list", { name: "同名 Agent agent-1 的轮次" }));
  const secondList = within(screen.getByRole("list", { name: "同名 Agent agent-2 的轮次" }));
  expect(firstList.getByText("原群")).toBeVisible();
  expect(secondList.getByText("另一群")).toBeVisible();
  expect(firstList.getByRole("button").querySelector("time")).toHaveAttribute("datetime", first.created_at);
  const olderButton = screen.getByRole("button", { name: "加载更早轮次" });
  olderButton.focus();
  fireEvent.click(olderButton);
  expect(olderButton).toHaveFocus();
  expect(await firstList.findByRole("button", { name: "原群 · 运行 older-1" })).toBeVisible();
  expect(olderButton).not.toBeInTheDocument();
  expect(screen.getByRole("heading", { name: "运行轮次" })).toHaveFocus();
  expect(secondList.getAllByRole("button")).toHaveLength(1);
  expect(urlAt(1).searchParams.get("before")).toBe("all+cursor/&");
  expect(urlAt(1).searchParams.has("member_id")).toBe(false);

  chooseAgent(second.member_id);
  expect(await screen.findByRole("button", { name: "另一群 · 运行 run-2" })).toBeVisible();
  expect(screen.queryByRole("list", { name: "同名 Agent agent-1 的轮次" })).not.toBeInTheDocument();
  expect(urlAt(2).pathname).toBe(runsPath);
  expect(urlAt(2).searchParams.get("member_id")).toBe(second.member_id);
  expect(urlAt(2).searchParams.has("before")).toBe(false);
  fireEvent.click(screen.getByRole("button", { name: "加载更早轮次" }));
  expect(await screen.findByRole("button", { name: "另一群 · 运行 older-2" })).toBeVisible();
  expect(urlAt(3).searchParams.get("before")).toBe("member+cursor/?");
  expect(urlAt(3).searchParams.get("member_id")).toBe(second.member_id);
});

it("renders and expands independent evidence without upgrading unknown runs or actions to success", async () => {
  const publicText = "  已完成工具动作。\n请看报告。  ";
  const submission = { id: "submission:submit-1", kind: "submission" as const, created_at: stamp(7), submission: {
    submission_id: "submit-1", task_id: "task-1", task_revision: 3, attempt_id: "attempt-1", preview: "交付记录", truncated: true,
  } };
  const declined: AgentRunEntry = { id: "dispatch_decision:decision-2", kind: "dispatch_decision", created_at: stamp(3), dispatch_decision: {
    decision_id: "decision-2", request_id: "request-2", task_id: "task-2", kind: "decline", reason_preview: "暂不分派", truncated: false,
  } };
  const receipt: AgentRunDetail["receipt"] = { outcome: "unknown", observed_terminal: "succeeded", started: true,
    accepted: true, delivery_failed: false, direct_child_reaped: true, output_complete: false };
  const rows = [operation("done-1", "done", 8), submission, operation("sync-1", "applied_sync_error", 6), operation("unknown-1", "unknown", 5),
    operation("pending-1", "pending", 4), assigned, reply("reply-1", publicText, first), trigger("trigger-1", "核对统计范围")];
  transport.mockResolvedValueOnce(page([first, second]))
    .mockResolvedValueOnce(json({ ...view(first, rows, "", receipt), accepted_at: stamp(2), native_session_id: "private-native",
      stdout_path: "/private/trace", unpublished_text: "未公开分析" }))
    .mockResolvedValueOnce(json(view(second, [declined, trigger("trigger-2", "另一群的触发")], "", { ...receipt, outcome: "succeeded", output_complete: true })));
  render(<AgentRunLogPage active />);
  fireEvent.click(await screen.findByRole("button", { name: `原群 · 运行 ${first.run_id}` }));
  expect(screen.getByRole("heading", { name: "本轮证据" })).toHaveFocus();
  expect(await evidence().findByText("运行结果未知", { selector: ".run-log-state" })).toBeVisible();
  expect(urlAt(1).pathname).toBe(`${runsPath}/${encodeURIComponent(first.run_id)}`);
  expect(urlAt(1).searchParams.get("group_id")).toBe(first.group_id);
  expect(urlAt(1).searchParams.has("member_id")).toBe(false);
  expect(timeline().getAllByRole("article").map(node => node.querySelector("h4")?.textContent)).toEqual([
    "触发消息", "群内发言", "分派决定", "工作区动作", "工作区动作", "工作区动作", "正式提交", "工作区动作",
  ]);
  const publication = within(timeline().getByRole("article", { name: "群内发言 message:reply-1" }));
  expect(publication.getByText("已完成工具动作。 请看报告。").textContent).toBe(publicText);
  expect(publication.getByText("群内原话不作为动作成功的回执。")).toBeVisible();
  expect(publication.queryByText(/已确认写入/)).not.toBeInTheDocument();
  expect(publication.getByText("reply-1")).not.toBeVisible();
  expect(within(expand(publication.getByText("查看消息标识"))).getByText("reply-1")).toBeVisible();
  const runReceipt = within(expand(evidence().getByText("查看运行回执")));
  expect(runReceipt.getByText("报告成功")).toBeVisible();
  expect(runReceipt.getByText("运行结果未知")).toBeVisible();
  expect(runReceipt.getByText("输出完整").nextElementSibling).toHaveTextContent("否");
  expect(runReceipt.getByText("CLI 接收输入不表示消息已读或已处理。")).toBeVisible();
  expect(evidence().queryByText("运行成功")).not.toBeInTheDocument();

  expect(timeline().getByText(/已登记，尚无操作回执/)).toBeVisible();
  expect(timeline().getByText(/已应用，持久同步未确认/)).toBeVisible();
  const unresolved = within(timeline().getByRole("article", { name: "工作区动作 workspace:unknown-1" }));
  expect(unresolved.getByText(/操作结果未知，待核对/)).toBeVisible();
  const result = within(expand(unresolved.getByText("查看动作结果")));
  expect(result.getByText("io_unknown")).toBeVisible();
  expect(result.getByText("workspace:unknown-1")).toBeVisible();
  const done = within(timeline().getByRole("article", { name: "工作区动作 workspace:done-1" }));
  expect(done.getByText(/已确认写入并同步/)).toBeVisible();
  expect(within(expand(done.getByText("查看动作结果"))).getByText(hash)).toBeVisible();
  expect(timeline().getByText("已记录正式提交，非验收")).toBeVisible();
  const submitted = within(expand(timeline().getByText("查看提交与版本")));
  expect(submitted.getByText("submit-1")).toBeVisible();
  expect(submitted.getByText("任务版本").nextElementSibling).toHaveTextContent("3");
  expect(timeline().getByText("已记录分派，不代表已执行")).toBeVisible();
  const decision = within(expand(timeline().getByText("查看分派决定")));
  for (const id of ["decision-1", "request-1", "task-1", "worker-1", "assignment-1"]) expect(decision.getByText(id)).toBeVisible();
  expect(decision.getByText("理由摘录")).toBeVisible();
  expect(decision.getByText("按现有分工处理")).toBeVisible();
  expect(decision.getByText("这条决定不代表本轮运行结果。")).toBeVisible();
  expect(evidence().queryByText(/private|未公开分析/)).not.toBeInTheDocument();

  fireEvent.click(pick(second));
  expect(await evidence().findByText("运行成功", { selector: ".run-log-state" })).toBeVisible();
  expect(timeline().getByText("已记录拒绝分派")).toBeVisible();
  expect(within(expand(timeline().getByText("查看分派决定"))).queryByText("分派 ID")).not.toBeInTheDocument();
  expect(evidence().queryByText("运行结果未知")).not.toBeInTheDocument();
  expect(evidence().queryByText("核对统计范围")).not.toBeInTheDocument();
  expect(urlAt(2).searchParams.get("group_id")).toBe(second.group_id);
  fireEvent.click(evidence().getByRole("button", { name: "返回轮次列表" }));
  expect(pick(second)).toHaveFocus();
});

it("reverses combined newest-first pages, deduplicates exact IDs and preserves the full publication through refresh", async () => {
  const excerpt = `  \n ${"核".repeat(340)}`, original = `${excerpt}\n完整内容 <b>不解析 HTML</b>  `;
  const input = trigger("z-msg", "核对原文");
  const published = reply("a-reply/1?+", excerpt, first, "2026-11-19T10:00:01.12345679Z", true);
  const otherReply = reply(assigned.dispatch_decision.decision_id, "另一条群内发言", first, published.created_at);
  const full = { id: published.message.message_id, group_id: first.group_id, sender_id: first.member_id, topic_id: "原话题", content: original,
    created_at: published.created_at, deliveries: [] };
  const live = { ...first, state: "running" as const }, completed = { ...first, state: "succeeded" as const, updated_at: stamp(11) };
  const more = reply("reply-2", "新增群发言", live, stamp(5));
  const olderOp = operation("older-op", "pending", 1), fullRead = deferred<Response>();
  transport.mockResolvedValueOnce(page([live])).mockResolvedValueOnce(json(view(live, [assigned, otherReply], "older+/&")))
    // An overlapping boundary row must not hide either the other same-time reply or the decision with the same source ID.
    .mockResolvedValueOnce(json(view(live, [otherReply, published, olderOp, input]))).mockResolvedValueOnce(json({ ...full, id: "unrelated" }))
    .mockReturnValueOnce(fullRead.promise).mockResolvedValueOnce(json(view(live, [more, assigned], "newer-anchor")))
    .mockResolvedValueOnce(json(view(live, [otherReply, published], "oldest-anchor")))
    .mockResolvedValueOnce(json(view(completed, [operation("older-op", "unknown", 1), input], "", successReceipt)));
  render(<AgentRunLogPage active />);
  fireEvent.click(await screen.findByRole("button", { name: `原群 · 运行 ${first.run_id}` }));
  const olderButton = await evidence().findByRole("button", { name: "加载更早证据" });
  const headLabels = [`群内发言 ${otherReply.id}`, `分派决定 ${assigned.id}`];
  expect(timelineLabels()).toEqual(headLabels);
  olderButton.focus();
  fireEvent.click(olderButton);
  const publication = within(await timeline().findByRole("article", { name: `群内发言 ${published.id}` }));
  expect(olderButton).not.toBeInTheDocument();
  expect(evidence().getByRole("heading", { name: "事件时间线" })).toHaveFocus();
  expect(urlAt(2).searchParams.get("before")).toBe("older+/&");
  expect(urlAt(2).searchParams.get("group_id")).toBe(first.group_id);
  const labels = [`触发消息 ${input.id}`, `工作区动作 ${olderOp.id}`, `群内发言 ${published.id}`, ...headLabels];
  expect(timelineLabels()).toEqual(labels);
  expect(timeline().getByText(/已登记，尚无操作回执/)).toBeVisible();
  const fullButton = publication.getByRole("button", { name: `读取完整消息 ${full.id}` });
  fullButton.focus();
  fireEvent.click(fullButton);
  expect(await publication.findByRole("alert")).toHaveTextContent("服务响应不符合约定");
  expect(publication.queryByText(/完整内容/)).not.toBeInTheDocument();
  expect(urlAt(3).pathname).toBe(`/api/v1/groups/${encodeURIComponent(first.group_id)}/history/${encodeURIComponent(full.id)}`);
  expect(urlAt(3).search).toBe("");
  const retry = publication.getByRole("button", { name: "重试读取完整消息" });
  expect(retry).toHaveFocus();
  fireEvent.click(retry);
  expect(retry).toHaveAttribute("aria-disabled", "true");
  expect(retry).toHaveFocus();
  await act(async () => fullRead.resolve(json(full)));
  const text = publication.getByText(/完整内容/);
  expect(text.textContent).toBe(original);
  expect(text.querySelector("b")).toBeNull();
  expect(text).toHaveFocus();
  fireEvent.click(evidence().getByRole("button", { name: "刷新本轮证据" }));
  expect(await evidence().findByText("新增群发言")).toBeVisible();
  expect(evidence().getByText(/完整内容/)).toBe(text);
  expect(evidence().getByText("运行中")).toBeVisible();
  expect(evidence().getByText("运行回执（未提供）")).toBeVisible();
  expect(timeline().getByText(/操作结果未知，待核对/)).toBeVisible();
  expect(timeline().queryByText(/已登记，尚无操作回执/)).not.toBeInTheDocument();
  expect(timelineLabels()).toEqual([...labels, `群内发言 ${more.id}`]);
  expect(urlAt(5).searchParams.has("before")).toBe(false);
  expect(urlAt(6).searchParams.get("before")).toBe("newer-anchor");
  expect(urlAt(7).searchParams.get("before")).toBe("oldest-anchor");
  expect(transport.mock.calls.every(([, init]) => (init?.method ?? "GET") === "GET")).toBe(true);
});

it.each([false, true])("moves focus to the full message only if its control still has focus (moved away: %s)", async (movedAway) => {
  const published = reply("long", "消息摘录", first, stamp(3), true), fullRead = deferred<Response>();
  transport.mockResolvedValueOnce(page([first])).mockResolvedValueOnce(json(view(first, [published])))
    .mockReturnValueOnce(fullRead.promise);
  render(<AgentRunLogPage active />);
  fireEvent.click(await screen.findByRole("button", { name: `${first.group_name} · 运行 ${first.run_id}` }));
  const button = await evidence().findByRole("button", { name: "读取完整消息 long" });
  button.focus();
  fireEvent.click(button);
  expect(button).toHaveFocus();
  const refresh = evidence().getByRole("button", { name: "刷新本轮证据" });
  if (movedAway) refresh.focus();
  await act(async () => fullRead.resolve(json({ id: published.message.message_id, group_id: first.group_id, sender_id: first.member_id,
    topic_id: "", content: "消息摘录和完整正文", created_at: published.created_at, deliveries: [] })));
  const text = evidence().getByText("消息摘录和完整正文");
  expect(text).toBeVisible();
  expect(button).not.toBeInTheDocument();
  expect(movedAway ? refresh : text).toHaveFocus();
});

it("discards late reads from a previous Agent or exact run selection even if transport ignores abort", async () => {
  const oldAgent = deferred<Response>(), oldRun = deferred<Response>();
  transport.mockResolvedValueOnce(page([first, second])).mockReturnValueOnce(oldAgent.promise)
    .mockResolvedValueOnce(page([second])).mockResolvedValueOnce(page([first, second])).mockReturnValueOnce(oldRun.promise)
    .mockResolvedValueOnce(json(view(second, [trigger("current-trigger", "当前触发消息")])))
    .mockResolvedValueOnce(json(view(first, [trigger("reopened", "重新打开的轮次")])));
  render(<AgentRunLogPage active />);
  await screen.findByRole("option", { name: "同名 Agent · 另一群 · agent-2" });
  chooseAgent(first.member_id);
  expect(screen.getByRole("status")).toHaveTextContent("正在读取运行轮次");
  chooseAgent(second.member_id);
  expect(transport.mock.calls[1][1]?.signal?.aborted).toBe(true);
  expect(await screen.findByRole("button", { name: "另一群 · 运行 run-2" })).toBeVisible();
  await act(async () => oldAgent.resolve(page([first])));
  expect(screen.queryByRole("button", { name: `原群 · 运行 ${first.run_id}` })).not.toBeInTheDocument();

  chooseAgent("");
  fireEvent.click(await screen.findByRole("button", { name: `原群 · 运行 ${first.run_id}` }));
  expect(evidence().getByRole("status")).toHaveTextContent("正在读取本轮证据");
  fireEvent.click(pick(second));
  expect(transport.mock.calls[4][1]?.signal?.aborted).toBe(true);
  expect(await evidence().findByText("当前触发消息")).toBeVisible();
  fireEvent.click(pick(first));
  expect(await evidence().findByText("重新打开的轮次")).toBeVisible();
  await act(async () => oldRun.resolve(json(view(first, [trigger("stale", "过期触发消息")]))));
  expect(evidence().getByText("重新打开的轮次")).toBeVisible();
  expect(evidence().queryByText("过期触发消息")).not.toBeInTheDocument();
});

it("does not finalize a live head on an older terminal observation, including after a hidden read resumes", async () => {
  vi.useFakeTimers();
  const live = { ...first, state: "running" as const };
  const completed = { ...live, state: "succeeded" as const, updated_at: stamp(11) };
  const prior = reply("prior", "先前发言", live), input = trigger("input", "更早的触发消息");
  const abandoned = deferred<Response>(), resumed = deferred<Response>(), newest = deferred<Response>();
  transport.mockResolvedValueOnce(page([live])).mockResolvedValueOnce(json(view(live, [prior], "before+/&")))
    .mockReturnValueOnce(abandoned.promise).mockReturnValueOnce(resumed.promise).mockReturnValueOnce(newest.promise);
  const pageView = render(<AgentRunLogPage active />);
  await flush();
  fireEvent.click(pick(live));
  await flush();
  fireEvent.click(evidence().getByRole("button", { name: "加载更早证据" }));
  pageView.rerender(<AgentRunLogPage active={false} />);
  expect(transport.mock.calls[2][1]?.signal?.aborted).toBe(true);
  await tick(10000);
  expect(transport).toHaveBeenCalledTimes(3);
  pageView.rerender(<AgentRunLogPage active />);
  await flush();
  expect(urlAt(3).href).toBe(urlAt(2).href);
  await act(async () => abandoned.resolve(json(view(completed, [trigger("stale", "过期的更早响应")], "", successReceipt))));
  expect(evidence().queryByText("过期的更早响应")).not.toBeInTheDocument();
  expect(evidence().getByText("正在读取本轮证据…")).toBeVisible();
  await act(async () => resumed.resolve(json(view(completed, [input], "", successReceipt))));
  expect(evidence().getByText("更早的触发消息")).toBeVisible();
  expect(evidence().getByText("运行中")).toBeVisible();
  expect(evidence().getByText("运行回执（未提供）")).toBeVisible();
  await tick();
  expect(transport).toHaveBeenCalledTimes(5);
  expect(urlAt(4).searchParams.has("before")).toBe(false);
  expect(evidence().getByText("运行中")).toBeVisible();
  const final = reply("final", "实际最终发言", completed, stamp(10));
  await act(async () => newest.resolve(json(view(completed, [final, prior], "after-prior", successReceipt))));
  expect(evidence().getByText("实际最终发言")).toBeVisible();
  expect(evidence().getByText("运行成功", { selector: ".run-log-state" })).toBeVisible();
  expect(within(expand(evidence().getByText("查看运行回执"))).getByText("报告成功")).toBeVisible();
  expect(timelineLabels()).toEqual([`触发消息 ${input.id}`, `群内发言 ${prior.id}`, `群内发言 ${final.id}`]);
  await tick(10000);
  expect(transport).toHaveBeenCalledTimes(5);
});

it("only polls the newest page after loading deep history and reuses the verified older frontier", async () => {
  vi.useFakeTimers();
  const live = { ...first, state: "running" as const };
  const cancelling = { ...live, state: "cancel_requested" as const, updated_at: stamp(30) };
  const completed = { ...live, state: "succeeded" as const, updated_at: stamp(31) };
  const newestOp = operation("recent", "pending", 22), unknown = operation("recent", "unknown", 22);
  const older = Array.from({ length: 8 }, (_, index) => operation(`deep-${index}`, index === 7 ? "pending" : "done", 20 - index));
  const cursor = (index: number) => `deep+${index}/?`;
  const progress = reply("progress", "新进度", live, stamp(23)), final = reply("complete", "最后输出", live, stamp(24));
  transport.mockResolvedValueOnce(page([live])).mockResolvedValueOnce(json(view(live, [newestOp], cursor(0))));
  older.forEach((entry, index) => transport.mockResolvedValueOnce(json(view(live, [entry], cursor(index + 1)))));
  transport.mockResolvedValueOnce(json(view(cancelling, [progress, unknown], "recent-progress")))
    .mockResolvedValueOnce(json(view(completed, [final, progress, unknown], "recent-complete", successReceipt)))
    .mockResolvedValueOnce(json(view(completed, [trigger("first-event", "最早的触发消息")], "", successReceipt)));
  render(<AgentRunLogPage active />);
  await flush();
  fireEvent.click(pick(live));
  await flush();
  const olderButton = evidence().getByRole("button", { name: "加载更早证据" });
  olderButton.focus();
  for (const [index] of older.entries()) {
    fireEvent.click(olderButton);
    await flush();
    expect(urlAt(index + 2).searchParams.get("before")).toBe(cursor(index));
    expect(olderButton).toHaveFocus();
  }
  const loaded = older.length + 2;
  expect(transport).toHaveBeenCalledTimes(loaded);
  await tick();
  expect(transport).toHaveBeenCalledTimes(loaded + 1);
  expect(urlAt(loaded).searchParams.has("before")).toBe(false);
  expect(evidence().getByText("已请求取消，尚未确认停止")).toBeVisible();
  expect(timeline().getByText(/操作结果未知，待核对/)).toBeVisible();
  expect(timeline().getByText(/已登记，尚无操作回执/)).toBeVisible();
  await tick();
  expect(transport).toHaveBeenCalledTimes(loaded + 2);
  expect(urlAt(loaded + 1).searchParams.has("before")).toBe(false);
  expect(evidence().getByText("运行成功", { selector: ".run-log-state" })).toBeVisible();
  expect(timelineLabels()).toEqual([
    ...[...older].reverse().map(entry => `工作区动作 ${entry.id}`),
    `工作区动作 ${newestOp.id}`, `群内发言 ${progress.id}`, `群内发言 ${final.id}`,
  ]);
  await tick(10000);
  expect(transport).toHaveBeenCalledTimes(loaded + 2);
  expect(olderButton).toHaveFocus();
  fireEvent.click(olderButton);
  await flush();
  expect(urlAt(loaded + 2).searchParams.get("before")).toBe(cursor(older.length));
  expect(evidence().getByText("最早的触发消息")).toBeVisible();
});

it("uses the new cursor rather than joining disjoint timeline windows", async () => {
  vi.useFakeTimers();
  const live = { ...first, state: "running" as const };
  const prior = reply("prior", "之前的发言", live), input = trigger("input", "更早的消息");
  const newest = reply("newest", "最新发言", live, stamp(5)), middle = reply("middle", "中间的发言", live, stamp(4));
  transport.mockResolvedValueOnce(page([live])).mockResolvedValueOnce(json(view(live, [prior], "old-cursor")))
    .mockResolvedValueOnce(json(view(live, [input]))).mockResolvedValueOnce(json(view(live, [newest], "new-cursor")))
    .mockResolvedValueOnce(json(view(live, [middle, prior, input])));
  render(<AgentRunLogPage active />);
  await flush();
  fireEvent.click(pick(live));
  await flush();
  fireEvent.click(evidence().getByRole("button", { name: "加载更早证据" }));
  await flush();
  expect(timelineLabels()).toEqual([`触发消息 ${input.id}`, `群内发言 ${prior.id}`]);
  await tick();
  expect(transport).toHaveBeenCalledTimes(4);
  expect(timelineLabels()).toEqual([`群内发言 ${newest.id}`]);
  fireEvent.click(evidence().getByRole("button", { name: "加载更早证据" }));
  await flush();
  expect(urlAt(4).searchParams.get("before")).toBe("new-cursor");
  expect(timelineLabels()).toEqual([`触发消息 ${input.id}`, `群内发言 ${prior.id}`, `群内发言 ${middle.id}`, `群内发言 ${newest.id}`]);
});

it("suspends pending-result refresh while hidden, then updates from a real action receipt and keeps the disclosure", async () => {
  vi.useFakeTimers();
  transport.mockResolvedValueOnce(page([first])).mockResolvedValueOnce(json(view(first, [operation("op-1", "pending", 4)])))
    .mockResolvedValueOnce(json(view(first, [operation("op-1", "done", 4)])));
  const pageView = render(<AgentRunLogPage active={false} />);
  await tick(10000);
  expect(transport).not.toHaveBeenCalled();
  pageView.rerender(<AgentRunLogPage active />);
  await flush();
  fireEvent.click(pick(first));
  await flush();
  const disclosure = expand(evidence().getByText("查看动作结果"));
  expect(within(disclosure).getByText("只有操作登记，尚未记录执行结果。")).toBeVisible();
  pageView.rerender(<AgentRunLogPage active={false} />);
  expect(disclosure).not.toBeVisible();
  await tick(15000);
  expect(transport).toHaveBeenCalledTimes(2);
  pageView.rerender(<AgentRunLogPage active />);
  expect(disclosure).toHaveAttribute("open");
  await tick();
  expect(within(disclosure).getByText(hash)).toBeVisible();
  expect(evidence().getByText(/已确认写入并同步/)).toBeVisible();
  expect(evidence().getByText("运行结果未知")).toBeVisible();
  await tick(10000);
  expect(transport).toHaveBeenCalledTimes(3);
});

it.each([
  { label: "a mismatched run", invalid: view({ ...first, group_id: second.group_id }, [trigger("foreign", "错误范围的内容")]) },
  { label: "an assign decision missing its assignment ID", invalid: { ...view(first, []), timeline: { next_cursor: "",
    items: [{ ...assigned, dispatch_decision: { ...assigned.dispatch_decision, assignment_id: undefined } }],
  } } },
])("rejects $label and retries the exact selection", async ({ invalid }) => {
  const retryRead = deferred<Response>();
  transport.mockResolvedValueOnce(page([first])).mockResolvedValueOnce(json(invalid)).mockReturnValueOnce(retryRead.promise);
  render(<AgentRunLogPage active />);
  fireEvent.click(await screen.findByRole("button", { name: `原群 · 运行 ${first.run_id}` }));
  expect(await evidence().findByRole("alert")).toHaveTextContent("运行记录响应不符合约定");
  expect(evidence().queryByRole("list", { name: "事件时间线" })).not.toBeInTheDocument();
  const retry = evidence().getByRole("button", { name: "重试读取本轮证据" });
  retry.focus();
  fireEvent.click(retry);
  expect(retry).toHaveAttribute("aria-disabled", "true");
  expect(retry).toHaveFocus();
  await act(async () => retryRead.resolve(json(view(first, [trigger("verified", "真实触发消息")]))));
  expect(evidence().getByText("真实触发消息")).toBeVisible();
  expect(retry).not.toBeInTheDocument();
  expect(evidence().getByRole("heading", { name: "本轮证据" })).toHaveFocus();
  expect(urlAt(2).href).toBe(urlAt(1).href);
});
