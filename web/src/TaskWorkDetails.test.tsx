import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { Tasks } from "./Tasks";
import type { Task } from "./task-api";
import { deferred, group, json, other, page } from "./test/groups";
import { secondTask, task } from "./test/tasks";

const selectedGroup = { ...group, id: "group /+?雪" };
const first = { ...task, group_id: selectedGroup.id, id: "task /+?旧", revision: 4 };
const second = { ...secondTask, group_id: selectedGroup.id };
const third = { ...second, group_id: other.id };
const path = (value: Task) => `/api/v1/groups/${encodeURIComponent(value.group_id)}/tasks/${encodeURIComponent(value.id)}`;
const scope = (value: Task) => ({ group_id: value.group_id, task_id: value.id, topic_id: "", task_revision: 1, created_at: value.created_at });
const rawInput = { ...scope(first), id: "input-1", source_id: "source-1", reply_message_id: "user-message-1", notice_message_id: "notice-1",
  content: "补充原文：<b>逐项复查</b>", before_constraint_revision: 0, before_constraint: "",
  after_constraint_revision: 1, after_constraint: "必须记录复查结果。" };
const delivered = (value: Task, summary: string) => ({ ...scope(value), id: "report-1", kind: "delivered", summary,
  actor_id: "worker-1", owner_member_id: "owner-1" });
const flush = () => act(async () => {});
const tick = (ms = 1) => act(() => vi.advanceTimersByTimeAsync(ms));
const disclosure = () => within(screen.getByRole("region", { name: "任务详情" })).getByText("工作输入与报告", { selector: "summary" });
const inputs = () => within(screen.getByRole("region", { name: "工作输入" }));
const reports = () => within(screen.getByRole("region", { name: "工作报告" }));
async function select(value: Task) {
  fireEvent.click(screen.getByRole("button", { name: `${value.title}，待交付` }));
  await flush();
}
async function toggle() {
  fireEvent.click(disclosure());
  await tick();
}
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });

it("lazily reads separate scoped work pages and distinguishes explicit handling without leaking a previous selection", async () => {
  vi.useFakeTimers();
  const initial = deferred<Response>(), oldInputs = deferred<Response>(), oldReports = deferred<Response>();
  const recoveredInput = { ...scope(third), id: "input-retried", content: "重试读取的输入。" };
  const readInputs = vi.fn<typeof fetch>()
    .mockReturnValueOnce(initial.promise).mockReturnValueOnce(oldInputs.promise)
    .mockResolvedValueOnce(page([rawInput])).mockResolvedValueOnce(new Response("404 page not found\n", { status: 404 }))
    .mockResolvedValueOnce(json({ error: { code: "not_found", message: "接口不存在" } }, 404))
    .mockResolvedValueOnce(page([recoveredInput]));
  const readReports = vi.fn<typeof fetch>()
    .mockResolvedValueOnce(page([delivered(first, "成员报告交付，尚未正式提交。")]))
    .mockResolvedValueOnce(page([{ ...delivered(second, "当前任务的报告。"), kind: "progress" }], "more-reports"))
    .mockReturnValueOnce(oldReports.promise).mockResolvedValueOnce(page([delivered(third, "另一群的报告。"),
      { ...delivered(third, "旧服务报告原文。"), id: "unrecognized-report", kind: "accepted" }]));
  const transport = vi.fn<typeof fetch>((input, init) => {
    const url = new URL(String(input), "http://fixture.invalid");
    if (url.pathname.endsWith("/work-inputs")) return readInputs(input, init);
    if (url.pathname.endsWith("/work-reports")) return readReports(input, init);
    if (url.pathname.endsWith("/tasks")) return Promise.resolve(page(url.pathname.startsWith(`/api/v1/groups/${other.id}/`) ? [third] : [first, second]));
    const value = [first, second, third].find(item => url.pathname === path(item));
    return Promise.resolve(value ? json(value) : json({ error: { code: "not_found", message: "记录不存在" } }, 404));
  });
  vi.stubGlobal("fetch", transport);
  const view = render(<Tasks group={selectedGroup} />); await flush(); await select(first);
  expect(disclosure().closest("details")).not.toHaveAttribute("open");
  await tick(10000);
  expect(readInputs).not.toHaveBeenCalled(); expect(readReports).not.toHaveBeenCalled();

  await toggle();
  expect(String(readInputs.mock.calls[0][0])).toBe(`${path(first)}/work-inputs?limit=50`);
  expect(String(readReports.mock.calls[0][0])).toBe(`${path(first)}/work-reports?limit=50`);
  expect(inputs().getByRole("status")).toHaveTextContent("正在读取工作输入");
  expect(reports().getByText("交付报告")).toBeVisible();
  expect(reports().getByText("成员报告不等于正式提交或用户验收。")).toBeVisible();
  expect(reports().getByText("成员报告交付，尚未正式提交。")).toBeVisible();
  expect(reports().getByText("报告成员标识").nextElementSibling?.textContent).toBe("worker-1");
  expect(screen.getByRole("button", { name: `${first.title}，待交付` })).toHaveAttribute("aria-pressed", "true");
  expect(screen.queryByRole("button", { name: "接纳交付" })).not.toBeInTheDocument();

  const returnedInput = { ...rawInput, id: "input-returned", return: {
    member_id: "bound-owner-1", turn_id: "return-turn-1", attempt_id: "return-attempt-1", notice_message_id: "return-notice-1",
    reason: "该输入超出原授权范围。", question: "是否只复查已有内容？", returned_at: "2026-10-30T12:00:02.123456789Z",
  } };
  await act(async () => initial.resolve(page([rawInput, { ...scope(first), id: "input-confirmed", content: "经执行成员确认的输入。",
    consumption: { turn_id: "worker-turn-1", attempt_id: "worker-attempt-1", consumed_at: "2026-10-30T12:00:01.123456789Z" } },
  { ...scope(first), id: "legacy-input", consumption: null, return: null }, returnedInput], "older/+&")));
  const [pending, confirmed, legacy, returned] = inputs().getAllByRole("listitem");
  expect(within(pending).getByText(rawInput.content)).toBeVisible();
  expect(pending.querySelector("b")).toBeNull();
  expect(within(pending).getByText("输入前约束 · 版本 0")).toBeVisible();
  expect(within(pending).getByText("输入后约束 · 版本 1")).toBeVisible();
  expect(within(pending).getByText(rawInput.after_constraint)).toBeVisible();
  expect(within(pending).getByText("待确认处理")).toBeVisible();
  expect(within(legacy).getByText("待确认处理")).toBeVisible();
  expect(within(confirmed).getByText("已明确确认处理")).toBeVisible();
  const turnId = within(confirmed).getByText("worker-turn-1");
  const attemptId = within(confirmed).getByText("确认尝试标识").nextElementSibling;
  expect(attemptId).toHaveTextContent("worker-attempt-1");
  expect(turnId).not.toBeVisible(); expect(attemptId).not.toBeVisible();
  fireEvent.click(within(confirmed).getByText("标识详情", { selector: "summary" })); await tick();
  expect(turnId).toBeVisible(); expect(attemptId).toBeVisible();

  const returnedDetails = within(returned);
  const evidence = returnedInput.return;
  expect(returnedDetails.getByText("已交回确认")).toBeVisible();
  expect(returnedDetails.queryByText(/待确认处理|已明确确认处理/)).not.toBeInTheDocument();
  expect(returnedDetails.getByText(evidence.returned_at, { selector: "time" })).toHaveAttribute("datetime", evidence.returned_at);
  expect(returnedDetails.getByText(returnedInput.content)).toBeVisible();
  expect(returnedDetails.getByText("输入后约束 · 版本 1")).toBeVisible();
  expect(returnedDetails.getByText(returnedInput.after_constraint)).toBeVisible();
  expect(returnedDetails.getByText("交回原因").nextElementSibling).toHaveTextContent(evidence.reason);
  expect(returnedDetails.getByText("确认问题").nextElementSibling).toHaveTextContent(evidence.question);
  fireEvent.click(returnedDetails.getByText("标识详情", { selector: "summary" })); await tick();
  for (const id of [evidence.member_id, evidence.turn_id, evidence.attempt_id, evidence.notice_message_id, returnedInput.notice_message_id]) {
    expect(returnedDetails.getByText(id)).toBeVisible();
  }

  fireEvent.click(inputs().getByRole("button", { name: "加载更多工作输入" }));
  const [olderURL, olderInit] = readInputs.mock.calls[1];
  expect(new URL(String(olderURL), "http://fixture.invalid").searchParams.get("before")).toBe("older/+&");
  await select(second);
  expect(olderInit?.signal?.aborted).toBe(true);
  expect(disclosure().closest("details")).not.toHaveAttribute("open");
  expect(screen.queryByText(rawInput.content)).not.toBeInTheDocument();
  expect(readInputs).toHaveBeenCalledTimes(2); expect(readReports).toHaveBeenCalledTimes(1);
  await toggle();
  await act(async () => oldInputs.resolve(page([{ ...rawInput, content: "迟到的原任务输入" }])));
  expect(screen.queryByText("迟到的原任务输入")).not.toBeInTheDocument();
  expect(inputs().getByRole("alert")).toHaveTextContent("服务响应不符合约定");
  expect(reports().getByText("进度报告")).toBeVisible();
  expect(reports().getByText("当前任务的报告。")).toBeVisible();

  fireEvent.click(reports().getByRole("button", { name: "加载更多工作报告" }));
  view.rerender(<Tasks group={other} />);
  expect(readReports.mock.calls[2][1]?.signal?.aborted).toBe(true);
  expect(screen.queryByText("当前任务的报告。")).not.toBeInTheDocument();
  await flush(); await select(third);
  await tick(10000);
  expect(readInputs).toHaveBeenCalledTimes(3); expect(readReports).toHaveBeenCalledTimes(3);
  await toggle();
  await act(async () => oldReports.resolve(page([delivered(second, "迟到的原群报告")])));
  expect(screen.queryByText("迟到的原群报告")).not.toBeInTheDocument();
  expect(reports().getByText("另一群的报告。")).toBeVisible();
  expect(reports().getByText("未知报告类型")).toBeVisible();
  expect(reports().queryByText("已接纳")).not.toBeInTheDocument();
  for (const index of [4, 5]) {
    expect(inputs().getByRole("alert")).toHaveTextContent("工作输入读取未确认。未找到请求的服务或记录（HTTP 404）。");
    expect(inputs().queryByRole("list")).not.toBeInTheDocument();
    fireEvent.click(inputs().getByRole("button", { name: "重试读取工作输入" })); await flush();
    expect(readInputs.mock.calls[index][0]).toBe(`${path(third)}/work-inputs?limit=50`);
  }
  expect(inputs().queryByRole("alert")).not.toBeInTheDocument();
  expect(inputs().getByText(recoveredInput.content)).toBeVisible();
  await toggle(); await tick(10000);
  expect(screen.queryByText(recoveredInput.content)).not.toBeInTheDocument();
  expect(screen.queryByText("另一群的报告。")).not.toBeInTheDocument();
  expect(readInputs).toHaveBeenCalledTimes(6); expect(readReports).toHaveBeenCalledTimes(4);
  expect(transport.mock.calls.every(([, init]) => (init?.method ?? "GET") === "GET")).toBe(true);
});

it("refreshes both expanded work pages on demand without coupling their pagination to task polling", async () => {
  vi.useFakeTimers();
  const lateInputs = deferred<Response>(), lateReports = deferred<Response>();
  const newInput = { ...rawInput, id: "input-new", content: "刷新后的输入。" };
  const newReport = { ...delivered(first, "刷新后的报告。"), id: "report-new" };
  const readInputs = vi.fn<typeof fetch>()
    .mockResolvedValueOnce(page([rawInput], "older-inputs"))
    .mockResolvedValueOnce(page([{ ...rawInput, id: "input-older", content: "已加载的旧输入。" }], "older-inputs-2"))
    .mockReturnValueOnce(lateInputs.promise).mockResolvedValueOnce(page([newInput]));
  const readReports = vi.fn<typeof fetch>()
    .mockResolvedValueOnce(page([delivered(first, "初始报告。")], "older-reports"))
    .mockResolvedValueOnce(json({ error: { code: "not_found", message: "任务不存在" } }, 404))
    .mockResolvedValueOnce(page([{ ...delivered(first, "已加载的旧报告。"), id: "report-older" }], "older-reports-2"))
    .mockReturnValueOnce(lateReports.promise).mockResolvedValueOnce(page([newReport]));
  const readDetail = vi.fn<typeof fetch>(async () => json(first));
  const transport: typeof fetch = (input, init) => {
    const url = new URL(String(input), "http://fixture.invalid");
    if (url.pathname === path(first)) return readDetail(input, init);
    if (url.pathname === `${path(first)}/work-inputs`) return readInputs(input, init);
    if (url.pathname === `${path(first)}/work-reports`) return readReports(input, init);
    return Promise.resolve(url.pathname.endsWith("/tasks") ? page([first]) : json({}, 404));
  };
  vi.stubGlobal("fetch", transport);
  const refreshDetails = () => fireEvent.click(screen.getByRole("button", { name: "刷新详情" }));
  const loadMore = async () => {
    fireEvent.click(inputs().getByRole("button", { name: "加载更多工作输入" }));
    fireEvent.click(reports().getByRole("button", { name: "加载更多工作报告" }));
    await flush();
  };
  render(<Tasks group={selectedGroup} />); await flush(); await select(first);
  refreshDetails(); await flush();
  expect(readDetail).toHaveBeenCalledTimes(2);
  expect(readInputs).not.toHaveBeenCalled(); expect(readReports).not.toHaveBeenCalled();

  await toggle(); await loadMore();
  expect(reports().getByRole("alert")).toHaveTextContent("工作报告读取未确认。未找到请求的服务或记录（HTTP 404）。");
  expect(reports().getByText("初始报告。")).toBeVisible();
  fireEvent.click(reports().getByRole("button", { name: "重试读取工作报告" })); await flush();
  expect(readReports.mock.calls[1][0]).toBe(`${path(first)}/work-reports?limit=50&before=older-reports`);
  expect(readReports.mock.calls[2][0]).toBe(readReports.mock.calls[1][0]);
  expect(reports().queryByRole("alert")).not.toBeInTheDocument();
  expect(reports().getByText("初始报告。")).toBeVisible();

  const openDisclosure = disclosure().closest("details");
  await tick(5000);
  expect(readDetail).toHaveBeenCalledTimes(3);
  expect(readInputs).toHaveBeenCalledTimes(2); expect(readReports).toHaveBeenCalledTimes(3);
  expect(inputs().getByText("已加载的旧输入。")).toBeVisible();
  expect(reports().getByText("已加载的旧报告。")).toBeVisible();

  await loadMore();
  const [inputURL, inputInit] = readInputs.mock.calls[2];
  const [reportURL, reportInit] = readReports.mock.calls[3];
  expect(inputURL).toBe(`${path(first)}/work-inputs?limit=50&before=older-inputs-2`);
  expect(reportURL).toBe(`${path(first)}/work-reports?limit=50&before=older-reports-2`);
  refreshDetails(); await flush();
  expect(readDetail).toHaveBeenCalledTimes(4);
  expect(disclosure().closest("details")).toBe(openDisclosure);
  expect(openDisclosure).toHaveAttribute("open");
  expect(readInputs).toHaveBeenCalledTimes(4); expect(readReports).toHaveBeenCalledTimes(5);
  expect(readInputs.mock.calls[3][0]).toBe(`${path(first)}/work-inputs?limit=50`);
  expect(readReports.mock.calls[4][0]).toBe(`${path(first)}/work-reports?limit=50`);
  expect(inputInit?.signal?.aborted).toBe(true); expect(reportInit?.signal?.aborted).toBe(true);

  await act(async () => {
    lateInputs.resolve(page([{ ...rawInput, content: "迟到的输入。" }]));
    lateReports.resolve(page([delivered(first, "迟到的报告。")]));
  });
  expect(inputs().getByText(newInput.content)).toBeVisible();
  expect(reports().getByText(newReport.summary)).toBeVisible();
  expect(screen.queryByText("迟到的输入。")).not.toBeInTheDocument();
  expect(screen.queryByText("迟到的报告。")).not.toBeInTheDocument();
});
