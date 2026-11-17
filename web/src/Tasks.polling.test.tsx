import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Tasks } from "./Tasks";
import type { Task, TaskAttempt, TaskSubmission } from "./task-api";
import { deferred, group, json, page } from "./test/groups";
import { acceptedTask, secondTask, submittedTask, task } from "./test/tasks";

const transport = vi.fn<typeof fetch>();
let current: Task, items: Task[];
let evidenceResponse: Promise<Response> | undefined;
const humanAttempt: TaskAttempt = { id: "attempt-1", task_id: task.id, group_id: group.id, task_revision: 1,
  turn_id: "", created_at: task.created_at };
const submission: TaskSubmission = { id: "submission-1", task_id: task.id, group_id: group.id, task_revision: 1,
  attempt_id: "attempt-1", summary: "服务端真实文本交付", submitted_by: "member-1", artifact_versions: [], created_at: task.created_at };
const flush = () => act(async () => {});
const tick = (ms = 5000) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });
const panel = () => within(screen.getByRole("region", { name: "群组任务" }));
beforeEach(() => {
  vi.useFakeTimers(); current = task; items = []; evidenceResponse = undefined;
  transport.mockReset().mockImplementation(async url => String(url).includes("/submissions/") ? evidenceResponse ?? json(submission)
    : String(url).includes("/attempts/") ? evidenceResponse ?? json(humanAttempt)
      : String(url).includes("/tasks?") ? page(String(url).includes("before=") ? [secondTask] : items, "older") : json(current));
  vi.stubGlobal("fetch", transport);
});
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });

it("UI-3: continues independent list reads during slow Submission evidence without replacing its detail epoch", async () => {
  render(<Tasks group={group} />); await flush();
  expect(panel().getByText("尚无任务，可创建任务并约定验收条件。")).toBeVisible();
  items = [task]; await tick();
  fireEvent.click(panel().getByRole("button", { name: `${task.title}，待交付` })); await flush();
  current = submittedTask; items = [current]; await tick();
  expect(panel().getByText(submission.summary)).toBeVisible();
  expect(panel().getByRole("button", { name: "接纳交付" })).toBeEnabled();
  const reason = panel().getByRole("textbox", { name: "验收说明（可选）" });
  fireEvent.change(reason, { target: { value: "保留人工验收草稿" } }); reason.focus();
  const evidence = deferred<Response>(); evidenceResponse = evidence.promise; await tick();
  expect(panel().getByRole("textbox", { name: "验收说明（可选）" })).toBe(reason);
  expect(reason).toBeEnabled(); expect(reason).toHaveFocus(); expect(reason).toHaveValue("保留人工验收草稿");
  expect(panel().getByRole("button", { name: "接纳交付" })).toBeDisabled();
  const count = transport.mock.calls.length;
  const nonListReads = () => transport.mock.calls.filter(([url]) => !String(url).includes("/tasks?"));
  const originalReads = nonListReads(), signal = originalReads.at(-1)?.[1]?.signal;
  items = [current, secondTask];
  for (let cycle = 0; cycle < 4; ++cycle) await tick();
  expect(panel().getByRole("button", { name: `${secondTask.title}，待交付` })).toBeVisible();
  expect(transport).toHaveBeenCalledTimes(count + 4);
  expect(nonListReads()).toEqual(originalReads); expect(signal?.aborted).toBe(false);
  await act(async () => evidence.resolve(json(submission))); evidenceResponse = undefined;
  expect(panel().getByRole("button", { name: "接纳交付" })).toBeEnabled();
  current = acceptedTask; items = [current]; await tick();
  expect(panel().getByRole("button", { name: `${task.title}，已接纳` })).toBeVisible();
  expect(panel().queryByRole("button", { name: "接纳交付" })).not.toBeInTheDocument();
  expect(transport.mock.calls.every(([, init]) => (init?.method ?? "GET") === "GET")).toBe(true);
});
it("UI-2: keeps the human summary DOM and editing focus during background Attempt reads, but fences submission", async () => {
  current = { ...task, active_attempt_id: humanAttempt.id }; items = [current];
  render(<Tasks group={group} />); await flush();
  fireEvent.click(panel().getByRole("button", { name: `${task.title}，待交付` })); await flush();
  const summary = panel().getByRole("textbox", { name: "交付摘要" });
  fireEvent.change(summary, { target: { value: "人工交付草稿" } }); summary.focus(); fireEvent.compositionStart(summary);
  const evidence = deferred<Response>(); evidenceResponse = evidence.promise; await tick();
  expect(summary).toBeInTheDocument(); expect(panel().getByRole("textbox", { name: "交付摘要" })).toBe(summary);
  expect(summary).toBeEnabled(); expect(summary).toHaveFocus();
  fireEvent.change(summary, { target: { value: "后台期间继续编辑" } });
  expect(panel().getByRole("button", { name: "提交交付" })).toBeDisabled();
  fireEvent.submit(panel().getByRole("form", { name: "提交交付" })); await flush();
  expect(transport.mock.calls.every(([, init]) => (init?.method ?? "GET") === "GET")).toBe(true);
  await act(async () => evidence.resolve(json(humanAttempt)));
  fireEvent.compositionEnd(summary);
  expect(panel().getByRole("textbox", { name: "交付摘要" })).toBe(summary); expect(summary).toHaveFocus();
  expect(summary).toHaveValue("后台期间继续编辑"); expect(panel().getByRole("button", { name: "提交交付" })).toBeEnabled();
});
it("keeps loaded older rows and creation drafts during latest-page polling, and stops while the panel is inactive", async () => {
  items = [task]; const view = render(<Tasks group={group} />); await flush();
  fireEvent.click(panel().getByRole("button", { name: "加载更多任务" })); await flush();
  const title = panel().getByRole("textbox", { name: "任务标题" }); fireEvent.change(title, { target: { value: "独立草稿" } });
  items = [submittedTask]; await tick();
  expect(panel().getByRole("button", { name: `${task.title}，待验收` })).toBeVisible();
  expect(panel().getByRole("button", { name: `${secondTask.title}，待交付` })).toBeVisible();
  expect(title).toHaveValue("独立草稿");
  view.rerender(<Tasks group={null} />); const count = transport.mock.calls.length; await tick(20000);
  expect(transport).toHaveBeenCalledTimes(count);
  view.rerender(<Tasks group={group} />); await flush(); expect(panel().getByRole("textbox", { name: "任务标题" })).toBe(title);
  expect(title).toHaveValue("独立草稿");
});
it("does not overlap a slow read and ignores its late result after hiding and reopening", async () => {
  const initial = deferred<Response>(); transport.mockReturnValueOnce(initial.promise);
  const view = render(<Tasks group={group} />); await tick(20000); expect(transport).toHaveBeenCalledTimes(1);
  await act(async () => initial.resolve(page([task])));
  const obsolete = deferred<Response>(); transport.mockReturnValueOnce(obsolete.promise); await tick(); await tick(20000);
  expect(transport).toHaveBeenCalledTimes(2); const signal = transport.mock.calls[1][1]?.signal;
  view.rerender(<Tasks group={null} />); expect(signal?.aborted).toBe(true);
  items = [acceptedTask]; view.rerender(<Tasks group={group} />); await flush();
  await act(async () => obsolete.resolve(page([secondTask])));
  expect(panel().getByRole("button", { name: `${task.title}，已接纳` })).toBeVisible();
  expect(panel().queryByRole("button", { name: `${secondTask.title}，待交付` })).not.toBeInTheDocument();
});
