import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { TaskExecution } from "./TaskExecution";
import type { DispatchExecution, ExecutionTurnState } from "./dispatch-execution-api";
import { deferred, json } from "./test/groups";

const stamp = "2026-11-16T08:00:00Z";
function execution(state: ExecutionTurnState = "unknown", requestId = "request", groupId = "group", taskId = "task"): DispatchExecution {
  const canceled = state === "cancel_requested" || state === "unknown";
  const worker = { id: "worker-turn", group_id: groupId, delivery_id: "worker-delivery", conversation_id: "worker-conversation", member_id: "worker",
    state, native_session_id: "native-worker", accepted_at: stamp, cancel_requested: canceled, cancel_requested_at: canceled ? stamp : null,
    created_at: stamp, updated_at: stamp, receipt: null };
  return { group_id: groupId, task_id: taskId, request_id: requestId, dispatch_status: "assigned",
    coordinator_turn: { ...worker, id: "lead-turn", delivery_id: "lead-delivery", member_id: "lead", state: "succeeded", native_session_id: "native-lead", cancel_requested: false, cancel_requested_at: null },
    decision: { id: "decision", kind: "assign", coordinator_turn_id: "lead-turn", created_at: stamp },
    assignment: { id: "assignment", request_id: requestId, decision_id: "decision", task_id: taskId, group_id: groupId, worker_member_id: "worker", created_at: stamp },
    assignment_dispatch_status: "enqueued", binding: { assignment_id: "assignment", group_id: groupId, task_id: taskId, worker_member_id: "worker", turn_id: worker.id,
      attempt_id: "attempt", task_revision: 1, created_at: stamp }, worker_turn: worker };
}
const transport = vi.fn<typeof fetch>();
const props = { groupId: "group", taskId: "task", requestId: "request", active: true };
const panel = () => within(screen.getByRole("region", { name: "执行回执" }));
const refresh = () => fireEvent.click(panel().getByRole("button", { name: "刷新执行回执" }));
const flush = async () => { await act(async () => {}); };
beforeEach(() => { transport.mockReset(); vi.stubGlobal("fetch", transport); });
afterEach(() => vi.unstubAllGlobals());

it("reads only on explicit refresh and distinguishes coordinator, worker, submission and user Review", async () => {
  transport.mockResolvedValueOnce(json(execution()));
  render(<TaskExecution {...props} />);
  expect(transport).not.toHaveBeenCalled();
  expect(panel().getByText(/执行不等于任务提交或用户验收/)).toBeVisible();
  refresh(); await flush();
  const coordinator = within(panel().getByRole("region", { name: "协调员执行" }));
  const worker = within(panel().getByRole("region", { name: "执行成员执行" }));
  expect(coordinator.getByText("执行成功")).toBeVisible();
  expect(worker.getByText("执行结果未知，需对账")).toBeVisible();
  expect(worker.getByText(/尚未观测到持久执行回执/)).toBeVisible();
  expect(panel().getByText("attempt")).toBeVisible();
  expect(panel().queryByRole("button", { name: /验收|提交|分派|取消/ })).not.toBeInTheDocument();
  expect(transport.mock.calls.every(([, init]) => init?.method === "GET" && !init.body && !init.headers)).toBe(true);
});
it.each([
  ["starting", "正在启动（已认领轮次）"], ["running", "运行中"], ["cancel_requested", "已请求取消，尚未确认停止"],
  ["unknown", "执行结果未知，需对账"], ["succeeded", "执行成功"], ["failed", "执行失败"], ["aborted", "执行已中止"],
  ["truncated", "执行输出被截断"], ["not_started", "执行未启动"],
] as const)("renders the actual %s state without deriving it from assignment or receipt absence", async (state, label) => {
  transport.mockResolvedValueOnce(json(execution(state)));
  render(<TaskExecution {...props} />); refresh(); await flush();
  expect(within(panel().getByRole("region", { name: "执行成员执行" })).getByText(label)).toBeVisible();
});
it("shows native terminal separately from unknown outcome and reports output, direct child, logs and delivery observations", async () => {
  const value = execution();
  const receipt = { started: true, accepted: false, outcome: "unknown", terminal: "succeeded", delivery_failed: true, native_session_id: "native-worker",
    direct_child_reaped: false, output_complete: false, exit_code: -1, logs_truncated: true };
  transport.mockResolvedValueOnce(json({ ...value, worker_turn: { ...value.worker_turn, receipt } }));
  render(<TaskExecution {...props} />); refresh(); await flush();
  const worker = within(panel().getByRole("region", { name: "执行成员执行" }));
  expect(worker.getByText("执行结果未知，需对账")).toBeVisible();
  expect(worker.getByText(/原生终态证据：执行成功/)).toBeVisible();
  expect(worker.getByText("输出完整性：未确认完整")).toBeVisible();
  expect(worker.getByText(/未确认直接子进程已回收/)).toBeVisible();
  expect(worker.getByText(/日志截断：是/)).toBeVisible();
  expect(worker.getByText(/投递失败观测：是/)).toBeVisible();
  expect(worker.getByText(/CLI 接收观测：本次未观测/)).toBeVisible();
  expect(worker.getByText(/退出码：-1/)).toBeVisible();
});
it("does not turn a queued assignment or absent Turn into not_started or running", async () => {
  const value = { ...execution(), assignment_dispatch_status: "pending", binding: null, worker_turn: null };
  transport.mockResolvedValueOnce(json(value));
  render(<TaskExecution {...props} />); refresh(); await flush();
  const worker = within(panel().getByRole("region", { name: "执行成员执行" }));
  expect(worker.getByText(/尚未观测到轮次/)).toBeVisible();
  expect(worker.queryByText("运行中")).not.toBeInTheDocument(); expect(worker.queryByText("执行未启动")).not.toBeInTheDocument();
});
it.each(["requestId", "groupId", "taskId"] as const)("clears evidence synchronously and aborts/ignores old work when %s changes", async field => {
  const result = render(<TaskExecution {...props} />);
  transport.mockResolvedValueOnce(json(execution("running"))); refresh(); await flush();
  expect(panel().getByText("native-worker")).toBeVisible();
  const late = deferred<Response>(); transport.mockReturnValueOnce(late.promise); refresh();
  const oldSignal = transport.mock.calls[1][1]?.signal;
  result.rerender(<TaskExecution {...props} {...{ [field]: "other" }} />);
  expect(oldSignal?.aborted).toBe(true);
  expect(panel().queryByText("native-worker")).not.toBeInTheDocument();
  await act(async () => late.resolve(json(execution("succeeded"))));
  expect(panel().queryByText("native-worker")).not.toBeInTheDocument();
  const current = { ...props, [field]: "other" };
  transport.mockResolvedValueOnce(json(execution("failed", current.requestId, current.groupId, current.taskId))); refresh(); await flush();
  expect(panel().getByText("执行失败")).toBeVisible();
});
it("drops displayed and in-flight evidence when hidden and does not restore it on reopening", async () => {
  const result = render(<TaskExecution {...props} />);
  transport.mockResolvedValueOnce(json(execution("running"))); refresh(); await flush();
  const late = deferred<Response>(); transport.mockReturnValueOnce(late.promise); refresh();
  const signal = transport.mock.calls[1][1]?.signal;
  result.rerender(<TaskExecution {...props} active={false} />);
  expect(signal?.aborted).toBe(true); expect(screen.queryByRole("region", { name: "执行回执" })).not.toBeInTheDocument();
  await act(async () => late.resolve(json(execution("succeeded"))));
  result.rerender(<TaskExecution {...props} />);
  expect(panel().queryByText("native-worker")).not.toBeInTheDocument(); expect(transport).toHaveBeenCalledTimes(2);
});
it.each(["failure", "malformed", "wrong-group", "wrong-task", "wrong-request"])("removes stale evidence after %s without writes", async fault => {
  render(<TaskExecution {...props} />);
  transport.mockResolvedValueOnce(json(execution("running"))); refresh(); await flush();
  if (fault === "failure") transport.mockRejectedValueOnce(new Error("PRIVATE"));
  else transport.mockResolvedValueOnce(json(fault === "malformed" ? {} : { ...execution(),
    [fault === "wrong-group" ? "group_id" : fault === "wrong-task" ? "task_id" : "request_id"]: "foreign" }));
  refresh(); expect(panel().queryByText("native-worker")).not.toBeInTheDocument(); await flush();
  expect(panel().getByRole("alert")).toHaveTextContent("未确认");
  expect(panel().queryByText("native-worker")).not.toBeInTheDocument();
  expect(panel().getByRole("alert")).not.toHaveTextContent("PRIVATE");
});
it("ignores a superseded refresh even if transport resolves after abort", async () => {
  render(<TaskExecution {...props} />);
  const late = deferred<Response>(); transport.mockReturnValueOnce(late.promise); refresh();
  const signal = transport.mock.calls[0][1]?.signal;
  transport.mockResolvedValueOnce(json(execution("failed"))); refresh(); await flush();
  expect(signal?.aborted).toBe(true);
  await act(async () => late.resolve(json(execution("running"))));
  expect(panel().getByText("执行失败")).toBeVisible(); expect(panel().queryByText("运行中")).not.toBeInTheDocument();
});
it("aborts and ignores an unmounted read", async () => {
  const late = deferred<Response>(); transport.mockReturnValueOnce(late.promise);
  const result = render(<TaskExecution {...props} />); refresh(); const signal = transport.mock.calls[0][1]?.signal;
  result.unmount(); expect(signal?.aborted).toBe(true);
  await act(async () => late.resolve(json(execution())));
  expect(screen.queryByRole("region", { name: "执行回执" })).not.toBeInTheDocument();
});
