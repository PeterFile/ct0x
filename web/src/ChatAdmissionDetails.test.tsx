import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Tasks } from "./Tasks";
import { deferred, group, json, other, page } from "./test/groups";
import { submittedTask, task } from "./test/tasks";
import type { Task } from "./task-api";
import { message } from "./test/messages";
import { autoDispatch, autoExecution, chatAdmission } from "./test/chat";

const admissions = vi.fn<typeof fetch>(), requests = vi.fn<typeof fetch>(), executions = vi.fn<typeof fetch>(), transport = vi.fn<typeof fetch>();
const verifiedAdmission = { ...chatAdmission, source_id: "actual-chat-command", source_kind: "local_user_chat", purpose: "chat_intake/v1",
  authority_evidence_verified: true, outcome: "admitted" };
let selected: Task;
const flush = () => act(async () => {});
async function open() {
  const view = render(<Tasks group={group} />); await flush();
  fireEvent.click(screen.getByRole("button", { name: `${task.title}，${selected.status === "submitted" ? "待验收" : "待交付"}` })); await flush(); return view;
}
beforeEach(() => {
  selected = task;
  admissions.mockReset().mockImplementation(async () => json(verifiedAdmission)); requests.mockReset().mockImplementation(async () => json(autoDispatch));
  executions.mockReset().mockImplementation(async () => json(autoExecution));
  transport.mockReset().mockImplementation((input, init) => {
    const url = String(input), groupId = decodeURIComponent(url.split("/")[4]);
    if (url.endsWith("/chat-admission")) return admissions(input, init);
    if (/\/coordination\/dispatch-requests\//.test(url)) return requests(input, init);
    if (url.endsWith("/execution")) return executions(input, init);
    if (init?.method === "POST" && url.endsWith("/reviews")) {
      selected = { ...task, revision: 2 };
      return Promise.resolve(json({ id: "user-review", group_id: groupId, task_id: task.id, task_revision: 1,
        submission_id: "submission-1", outcome: "rejected", reason: "", reviewed_by: "user", artifact_versions: [], created_at: task.created_at }, 201));
    }
    if (url.endsWith("/submissions/submission-1")) return Promise.resolve(json({ id: "submission-1", group_id: groupId, task_id: task.id,
      task_revision: 1, attempt_id: "attempt-1", summary: "已有真实交付", submitted_by: "user", artifact_versions: [], created_at: task.created_at }));
    if (url.endsWith(`/messages/${message.id}`)) return Promise.resolve(json({ ...message, source_kind: "local_user_chat", source_id: verifiedAdmission.source_id }));
    if (url.includes("/tasks?")) return Promise.resolve(page([{ ...selected, group_id: groupId }]));
    if (url.endsWith(`/tasks/${task.id}`)) return Promise.resolve(json({ ...selected, group_id: groupId }));
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
  expect(evidence).toHaveTextContent("受信意图判定为明确请求"); expect(evidence).toHaveTextContent("来源与评估职责已核验");
  expect(evidence).not.toHaveTextContent(/人类批准已验证|已接纳成果/); expect(evidence).toHaveTextContent(verifiedAdmission.source_id); expect(evidence).toHaveTextContent("chat_intake/v1");
  expect(evidence).toHaveTextContent("聊天策略版本 3");
  expect(evidence).toHaveTextContent("分派策略版本 4");
  expect(screen.getByRole("button", { name: "展开请求分派" })).toHaveAttribute("aria-expanded", "false");
  expect(within(evidence).getAllByText(/尚未观测到轮次/)).toHaveLength(2);
  expect(transport.mock.calls.every(([, init]) => (init?.method ?? "GET") === "GET")).toBe(true);
});
it("shows a preparation's distinct written authorization and actual member source from the immutable receipt", async () => {
  admissions.mockResolvedValueOnce(json({ ...verifiedAdmission, disposition: "prepare_work", authority: "written_preauthorization",
    source_kind: "member", source_id: "member-command", source_sender_id: "member-context", authorization_message_id: "user-written-preauthorization" })); await open();
  const evidence = screen.getByRole("region", { name: "聊天自动准入依据" });
  expect(evidence).toHaveTextContent("策略授权的准备工作"); expect(evidence).toHaveTextContent("member-context"); expect(evidence).toHaveTextContent("成员消息");
  expect(evidence).toHaveTextContent("user-written-preauthorization"); expect(evidence).toHaveTextContent(chatAdmission.message_id);
  expect(requests).toHaveBeenCalledTimes(1);
});
it.each([
  ["local_user_policy", "用户策略配置命令"], ["platform", "系统消息"], ["unknown", "来源未核验"],
])("shows actual %s source facts without converting a sender=user into verified approval", async (kind, label) => {
  admissions.mockResolvedValueOnce(json({ ...verifiedAdmission, disposition: "prepare_work", authority: "written_preauthorization",
    authorization_message_id: "policy-rule", source_kind: kind, source_id: kind === "unknown" ? "" : `${kind}-receipt`,
    purpose: "", authority_evidence_verified: false })); await open();
  const evidence = screen.getByRole("region", { name: "聊天自动准入依据" }); expect(evidence).toHaveTextContent(label);
  expect(evidence).toHaveTextContent("历史来源未核验"); expect(evidence).not.toHaveTextContent("来源与评估职责已核验"); expect(requests).toHaveBeenCalledTimes(1);
});
it("lets a user inspect the exact source message rather than treating model text as source evidence", async () => {
  await open(); fireEvent.click(screen.getByRole("button", { name: "检查来源消息" })); await flush();
  const source = screen.getByRole("region", { name: "来源消息原文" }); expect(source).toHaveTextContent(message.content);
  expect(source.querySelector("time")).toHaveAttribute("datetime", message.created_at);
  expect(transport.mock.calls.some(([url]) => String(url) === `/api/v1/groups/${group.id}/messages/${message.id}`)).toBe(true);
});
it("binds source reads to the receipt topic and cancels hidden reads without accepting a late result", async () => {
  await open(); transport.mockResolvedValueOnce(json({ ...message, topic_id: "foreign-topic", content: "wrong source text" }));
  fireEvent.click(screen.getByRole("button", { name: "检查来源消息" })); await flush();
  expect(screen.getByRole("region", { name: "来源消息原文" })).toHaveTextContent("不一致"); expect(screen.queryByText("wrong source text")).not.toBeInTheDocument();
  const pending = deferred<Response>(); transport.mockReturnValueOnce(pending.promise);
  fireEvent.click(screen.getByRole("button", { name: "重新读取来源消息" })); await flush();
  const signal = transport.mock.calls.at(-1)![1]?.signal; fireEvent.click(screen.getByRole("button", { name: "收起来源消息" })); expect(signal?.aborted).toBe(true);
  await act(async () => pending.resolve(json({ ...message, content: "late source text" })));
  expect(screen.queryByRole("region", { name: "来源消息原文" })).not.toBeInTheDocument(); expect(screen.queryByText("late source text")).not.toBeInTheDocument();
});
it("keeps legacy authority unverified without blocking an existing Submission or an explicit user rejection", async () => {
  selected = submittedTask;
  admissions.mockResolvedValueOnce(json({ ...chatAdmission, source_id: "", source_kind: "unknown", purpose: "", authority_evidence_verified: false })); await open();
  expect(screen.getByRole("region", { name: "聊天自动准入依据" })).toHaveTextContent("历史来源未核验");
  expect(screen.getByText("已有真实交付")).toBeVisible(); expect(screen.getByRole("button", { name: "接纳交付" })).toBeEnabled();
  expect(screen.getByRole("button", { name: "退回修改" })).toBeEnabled();
  expect(transport.mock.calls.every(([, init]) => (init?.method ?? "GET") === "GET")).toBe(true);
  fireEvent.click(screen.getByRole("button", { name: "退回修改" })); await flush();
  expect(screen.getByRole("region", { name: "审查回执" })).toHaveTextContent("退回修改");
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
  { purpose: "generic_member" }, { source_kind: "platform" }, { source_id: "" },
  { disposition: "prepare_work", authority: "written_preauthorization", authorization_message_id: "policy-rule", source_kind: "platform" },
  { disposition: "prepare_work", authority: "written_preauthorization", authorization_message_id: "policy-rule", source_kind: "local_user_policy" },
  { disposition: "prepare_work", authority: "written_preauthorization", authorization_message_id: "policy-rule", source_sender_id: "member-forged-user" },
])("rejects a receipt with an invalid binding %j instead of loading its request", async change => {
  admissions.mockResolvedValueOnce(json({ ...verifiedAdmission, ...change })); await open();
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
