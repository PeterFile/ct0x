import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Tasks } from "./Tasks";
import { deferred, group, json, other, page } from "./test/groups";
import { task } from "./test/tasks";
import { autoDispatch, autoExecution, chatAdmission } from "./test/chat";

const admissions = vi.fn<typeof fetch>(), requests = vi.fn<typeof fetch>(), executions = vi.fn<typeof fetch>(), transport = vi.fn<typeof fetch>();
const flush = () => act(async () => {});
async function open() {
  const view = render(<Tasks group={group} />); await flush();
  fireEvent.click(screen.getByRole("button", { name: `${task.title}，待交付` })); await flush(); return view;
}
beforeEach(() => {
  admissions.mockReset().mockImplementation(async () => json(chatAdmission)); requests.mockReset().mockImplementation(async () => json(autoDispatch));
  executions.mockReset().mockImplementation(async () => json(autoExecution));
  transport.mockReset().mockImplementation((input, init) => {
    const url = String(input), groupId = decodeURIComponent(url.split("/")[4]);
    if (url.endsWith("/chat-admission")) return admissions(input, init);
    if (/\/coordination\/dispatch-requests\//.test(url)) return requests(input, init);
    if (url.endsWith("/execution")) return executions(input, init);
    if (url.includes("/tasks?")) return Promise.resolve(page([{ ...task, group_id: groupId }]));
    if (url.endsWith(`/tasks/${task.id}`)) return Promise.resolve(json({ ...task, group_id: groupId }));
    return Promise.resolve(json({ error: { code: "not_found", message: "旧实例没有该记录" } }, 404));
  }); vi.stubGlobal("fetch", transport);
});
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });

it("discovers the exact automatic request on task selection and reads real receipts without opening manual dispatch", async () => {
  await open();
  expect(admissions.mock.calls[0][0]).toBe(`/api/v1/groups/${group.id}/tasks/${task.id}/chat-admission`);
  expect(requests.mock.calls[0][0]).toBe(`/api/v1/groups/${group.id}/coordination/dispatch-requests/${chatAdmission.dispatch_request_id}`);
  expect(executions.mock.calls[0][0]).toBe(`/api/v1/groups/${group.id}/tasks/${task.id}/dispatch-requests/${chatAdmission.dispatch_request_id}/execution`);
  const evidence = screen.getByRole("region", { name: "聊天自动准入依据" });
  for (const value of [chatAdmission.message_id, chatAdmission.authorization_message_id, chatAdmission.mandate_id, chatAdmission.assessed_by]) expect(evidence).toHaveTextContent(value);
  expect(evidence).toHaveTextContent("明确请求"); expect(evidence).toHaveTextContent("聊天策略版本 3");
  expect(evidence).toHaveTextContent("分派策略版本 4");
  expect(screen.getByRole("button", { name: "展开请求分派" })).toHaveAttribute("aria-expanded", "false");
  expect(within(evidence).getAllByText(/尚未观测到轮次/)).toHaveLength(2);
  expect(transport.mock.calls.every(([, init]) => (init?.method ?? "GET") === "GET")).toBe(true);
});
it("shows a preparation's distinct written authorization and actual member source from the immutable receipt", async () => {
  admissions.mockResolvedValueOnce(json({ ...chatAdmission, disposition: "prepare_work", authority: "written_preauthorization",
    source_sender_id: "member-context", authorization_message_id: "user-written-preauthorization" })); await open();
  const evidence = screen.getByRole("region", { name: "聊天自动准入依据" });
  expect(evidence).toHaveTextContent("用户书面预授权"); expect(evidence).toHaveTextContent("member-context");
  expect(evidence).toHaveTextContent("user-written-preauthorization"); expect(evidence).toHaveTextContent(chatAdmission.message_id);
  expect(requests).toHaveBeenCalledTimes(1);
});
it("quietly keeps the legacy manual entry on chat-admission 404 without guessing a request", async () => {
  admissions.mockResolvedValueOnce(json({ error: { code: "not_found", message: "旧任务" } }, 404)); await open();
  expect(screen.queryByRole("region", { name: "聊天自动准入依据" })).not.toBeInTheDocument();
  expect(screen.queryByRole("alert")).not.toBeInTheDocument(); expect(requests).not.toHaveBeenCalled(); expect(executions).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "展开请求分派" })); await flush();
  expect(screen.getByRole("form", { name: "发起分派" })).toBeVisible();
});
it.each([
  { group_id: other.id }, { task_id: "foreign-task" }, { source_sender_id: "agent-1" },
])("rejects a receipt with an invalid binding %j instead of loading its request", async change => {
  admissions.mockResolvedValueOnce(json({ ...chatAdmission, ...change })); await open();
  expect(screen.getByRole("alert")).toHaveTextContent("未确认"); expect(requests).not.toHaveBeenCalled(); expect(executions).not.toHaveBeenCalled();
});
it("binds the request to the admission's exact mandate and topic before loading execution evidence", async () => {
  requests.mockResolvedValueOnce(json({ ...autoDispatch, request: { ...autoDispatch.request, mandate_id: "wrong-mandate" } })); await open();
  expect(screen.getByRole("alert")).toHaveTextContent("未确认"); expect(executions).not.toHaveBeenCalled();
});
it("aborts an obsolete group discovery and never presents its late receipt in the current group", async () => {
  const late = deferred<Response>(); admissions.mockReturnValueOnce(late.promise);
  const view = await open(); const signal = admissions.mock.calls[0][1]?.signal;
  admissions.mockResolvedValueOnce(json({}, 404)); view.rerender(<Tasks group={other} />); await flush();
  fireEvent.click(screen.getByRole("button", { name: `${task.title}，待交付` })); await flush();
  expect(signal?.aborted).toBe(true); await act(async () => late.resolve(json(chatAdmission)));
  expect(screen.queryByRole("region", { name: "聊天自动准入依据" })).not.toBeInTheDocument(); expect(requests).not.toHaveBeenCalled();
});
it("does not reopen a terminal held receipt when a later read contradicts its immutable state", async () => {
  vi.useFakeTimers(); requests.mockResolvedValueOnce(json({ ...autoDispatch, state: { ...autoDispatch.state,
    generation: 2, status: "held", held_reason: "no_eligible_worker" } })); await open();
  requests.mockResolvedValueOnce(json({ ...autoDispatch, state: { ...autoDispatch.state, generation: 3,
    attempt_count: 2, last_error_code: "sqlite_busy" } }));
  await act(async () => { await vi.advanceTimersByTimeAsync(5000); });
  expect(within(screen.getByRole("region", { name: "聊天自动准入依据" })).getByText(/自动分派读取.*未确认/)).toBeVisible();
});
it("automatically updates the discovered request without creating another Dispatch or Review", async () => {
  vi.useFakeTimers(); await open();
  requests.mockResolvedValueOnce(json({ ...autoDispatch, state: { ...autoDispatch.state, generation: 2, status: "held", held_reason: "no_eligible_worker" } }));
  await act(async () => { await vi.advanceTimersByTimeAsync(5000); });
  expect(screen.getByRole("region", { name: "聊天自动准入依据" })).toHaveTextContent("协调请求暂停");
  expect(requests).toHaveBeenCalledTimes(2);
  expect(transport.mock.calls.every(([, init]) => (init?.method ?? "GET") === "GET")).toBe(true);
});
