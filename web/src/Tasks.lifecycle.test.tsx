import { createServer } from "node:http";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Tasks } from "./Tasks";
import type { Group } from "./api";
import type { Task, TaskAttempt, TaskSubmission, TaskReview } from "./task-api";
import { deferred, group, json, other, page } from "./test/groups";
import { acceptedTask, secondTask, submittedTask, task, taskInput } from "./test/tasks";

const humanAttempt: TaskAttempt = { id: "attempt-1", group_id: group.id, task_id: task.id, task_revision: 1,
  turn_id: "", created_at: task.created_at };
const activeTask: Task = { ...task, active_attempt_id: humanAttempt.id };
const summary = "  已核对来源。\n保留原文与空白。  ";
const delivery: TaskSubmission = { id: "submission-1", group_id: group.id, task_id: task.id, task_revision: 1,
  attempt_id: humanAttempt.id, summary, submitted_by: "user", artifact_versions: [], created_at: task.created_at };
const decision: TaskReview = { id: "review-1", group_id: group.id, task_id: task.id, task_revision: 1,
  submission_id: delivery.id, outcome: "accepted", reason: "", reviewed_by: "user", artifact_versions: [], created_at: task.created_at };
const nativeFetch = globalThis.fetch;
const frozenRefs = [{ artifact_id: "report/一", version_id: "opaque-v2" },
  { artifact_id: "report/一", version_id: "opaque-v1" }, { artifact_id: "source", version_id: "snapshot" }];
const reads = vi.fn<typeof fetch>(), details = vi.fn<typeof fetch>(), evidence = vi.fn<typeof fetch>(), writes = vi.fn<typeof fetch>();
let current: Task;
const panel = () => within(screen.getByRole("region", { name: "群组任务" }));
const detail = () => within(panel().getByRole("region", { name: "任务详情" }));
const row = (title = task.title) => panel().getByRole("button", { name: new RegExp(`^${title}，`) });
const change = (name: string, value: string) => fireEvent.change(detail().getByRole("textbox", { name }), { target: { value } });
const captured = (index: number) => {
  const [url, init] = writes.mock.calls[index];
  return { url, body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key") };
};
const ui = (value: Group | null) => <><button type="button">聊天输入</button><Tasks group={value} /></>;
const refresh = () => fireEvent.click(detail().getByRole("button", { name: "刷新详情" }));
const noUsableReview = () => {
  for (const button of detail().queryAllByRole("button", { name: /接纳交付|退回修改/ })) expect(button).toBeDisabled();
};
const kinds = ["attempt", "submission", "review"] as const;
type Kind = typeof kinds[number];
const initialState = (kind: Kind) => kind === "attempt" ? task : kind === "submission" ? activeTask : submittedTask;
const receiptFor = (kind: Kind) => kind === "attempt" ? humanAttempt : kind === "submission" ? delivery : decision;
async function perform(kind: Kind) {
  if (kind === "attempt") fireEvent.click(await detail().findByRole("button", { name: "登记人工尝试" }));
  else if (kind === "submission") {
    await detail().findByRole("textbox", { name: "交付摘要" }); change("交付摘要", summary);
    fireEvent.submit(detail().getByRole("form", { name: "提交交付" }));
  } else {
    await detail().findByText(summary, { normalizer: text => text });
    fireEvent.click(detail().getByRole("button", { name: "接纳交付" }));
  }
}
async function open(state = task) {
  current = state;
  const view = render(ui(group));
  fireEvent.click(await panel().findByRole("button", { name: new RegExp(`^${state.title}，`) }));
  await detail().findByRole("heading", { name: state.title });
  return view;
}
beforeEach(() => {
  current = task;
  reads.mockReset().mockImplementation(async () => page([current]));
  details.mockReset().mockImplementation(async () => json(current));
  evidence.mockReset().mockImplementation(async url => json(String(url).includes("/attempts/") ? humanAttempt : delivery));
  writes.mockReset().mockResolvedValue(json({}, 503));
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) =>
    (init?.method === "POST" ? writes : /\/(attempts|submissions)\//.test(String(input)) ? evidence
      : String(input).includes("/tasks?") ? reads : details)(input, init));
});
afterEach(() => vi.unstubAllGlobals());

it.each([404, 400, 409])("retains unknown creation identity after retry HTTP %s across task/group/hidden switches", async status => {
  let aItems = [secondTask];
  const bTask = { ...secondTask, group_id: other.id, title: "B 的任务" };
  reads.mockImplementation(async url => page(String(url).includes(other.id) ? [bTask] : aItems));
  details.mockImplementation(async url => json(String(url).includes(other.id) ? bTask : String(url).endsWith(secondTask.id) ? secondTask : task));
  writes.mockRejectedValueOnce(new TypeError("Lost creation receipt"));
  const view = render(ui(group));
  await panel().findByRole("button", { name: `${secondTask.title}，待交付` });
  const form = panel().getByRole("form", { name: "创建任务" });
  const title = within(form).getByRole("textbox", { name: "任务标题" });
  const description = within(form).getByRole("textbox", { name: "任务描述（可选）" });
  const acceptance = within(form).getByRole("textbox", { name: "验收条件" });
  fireEvent.change(title, { target: { value: ` ${task.title} ` } });
  fireEvent.change(description, { target: { value: task.description } });
  fireEvent.change(acceptance, { target: { value: ` ${task.acceptance} ` } });
  fireEvent.submit(form);
  expect(await panel().findByRole("alert")).toHaveTextContent("创建结果未确认");
  const original = captured(0);
  expect(original.url).toBe(`/api/v1/groups/${group.id}/tasks`);
  expect(JSON.parse(String(original.body))).toEqual(taskInput);
  const rejection = deferred<Response>(); writes.mockReturnValueOnce(rejection.promise);
  fireEvent.submit(form); fireEvent.submit(form);
  expect(writes).toHaveBeenCalledTimes(2); expect(captured(1)).toEqual(original);
  fireEvent.click(row(secondTask.title)); await detail().findByRole("heading", { name: secondTask.title });
  view.rerender(ui(other));
  await panel().findByRole("button", { name: `${bTask.title}，待交付` });
  const bTitle = panel().getByRole("textbox", { name: "任务标题" });
  fireEvent.change(bTitle, { target: { value: "B 的独立草稿" } });
  view.rerender(ui(null));
  expect(writes.mock.calls[1][1]?.signal?.aborted).toBe(false);
  await act(async () => rejection.resolve(json({ error: { code: "unavailable" } }, status)));
  // Continue through UI checks so RED also proves the old implementation allocates a replacement key.
  for (const field of [title, description, acceptance]) expect.soft(field).toBeDisabled();
  expect.soft(form.querySelector('[role="alert"]')).toHaveTextContent("创建结果未确认");
  expect(writes).toHaveBeenCalledTimes(2);
  aItems = [task, secondTask]; view.rerender(ui(group));
  await panel().findByRole("button", { name: `${task.title}，待交付` });
  // Even a list GET containing the created Task cannot confirm the original POST.
  expect.soft(title).toBeDisabled(); expect(title).toHaveValue(` ${task.title} `);
  expect.soft(panel().queryByRole("button", { name: "重试同一次任务创建" })).toBeEnabled();
  const receipt = deferred<Response>(); writes.mockReturnValueOnce(receipt.promise);
  fireEvent.submit(form); fireEvent.submit(form);
  await act(async () => receipt.resolve(json(task, 201)));
  expect(writes).toHaveBeenCalledTimes(3); expect(captured(2)).toEqual(original);
  expect(title).toBeEnabled(); expect(title).toHaveValue(""); expect(acceptance).toHaveValue("");
  await detail().findByRole("heading", { name: task.title });
  view.rerender(ui(other));
  expect(panel().getByRole("textbox", { name: "任务标题" })).toHaveValue("B 的独立草稿");
});

it.each(["accepted", "rejected"] as const)("registers a human attempt, submits raw text and explicitly reviews %s using authoritative GETs", async outcome => {
  await open();
  expect(writes).not.toHaveBeenCalled();
  const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise);
  fireEvent.click(detail().getByRole("button", { name: "登记人工尝试" }));
  fireEvent.click(detail().getByRole("button", { name: "登记人工尝试" }));
  expect(writes).toHaveBeenCalledTimes(1);
  expect(JSON.parse(String(captured(0).body))).toEqual({ expected_revision: 1, expected_active_attempt_id: "" });
  expect(captured(0).url).toBe(`/api/v1/groups/${group.id}/tasks/${task.id}/attempts`);
  expect(detail().queryByRole("textbox", { name: "交付摘要" })).not.toBeInTheDocument();
  const refreshed = deferred<Response>(); details.mockReturnValueOnce(refreshed.promise);
  current = activeTask;
  await act(async () => pending.resolve(json(humanAttempt, 201)));
  expect(detail().getByText("正在载入任务详情…")).toBeVisible();
  expect(detail().queryByRole("textbox", { name: "交付摘要" })).not.toBeInTheDocument();
  await act(async () => refreshed.resolve(json(current)));
  await detail().findByRole("textbox", { name: "交付摘要" });
  expect(detail().getByText("仅文本成果，不附加文件或成果版本")).toBeVisible();
  fireEvent.submit(detail().getByRole("form", { name: "提交交付" }));
  expect(detail().getByRole("alert")).toHaveTextContent("请输入交付摘要");
  change("交付摘要", " \n "); fireEvent.submit(detail().getByRole("form", { name: "提交交付" }));
  expect(writes).toHaveBeenCalledTimes(1);
  change("交付摘要", summary);
  const submissionRead = deferred<Response>(); evidence.mockReturnValueOnce(submissionRead.promise);
  current = submittedTask; writes.mockResolvedValueOnce(json(delivery, 201));
  fireEvent.submit(detail().getByRole("form", { name: "提交交付" }));
  await detail().findByText("正在载入当前交付…");
  expect(detail().getByRole("button", { name: "接纳交付" })).toBeDisabled();
  expect(writes).toHaveBeenCalledTimes(2);
  expect(JSON.parse(String(captured(1).body))).toEqual({ expected_revision: 1, attempt_id: humanAttempt.id, summary });
  expect(captured(1).url).toBe(`/api/v1/groups/${group.id}/tasks/${task.id}/submissions`);
  await act(async () => submissionRead.resolve(json(delivery)));
  expect(detail().getByText(summary, { normalizer: text => text })).toBeVisible();
  expect(detail().getByText("提交者：user")).toBeVisible();
  expect(writes).toHaveBeenCalledTimes(2);
  change("验收说明（可选）", "  人工核对后的结论  ");
  const reviewing = deferred<Response>(); writes.mockReturnValueOnce(reviewing.promise);
  const label = outcome === "accepted" ? "接纳交付" : "退回修改";
  fireEvent.click(detail().getByRole("button", { name: label }));
  fireEvent.click(detail().getByRole("button", { name: label }));
  expect(writes).toHaveBeenCalledTimes(3);
  expect(detail().getByRole("textbox", { name: "验收说明（可选）" })).toBeDisabled();
  expect(JSON.parse(String(captured(2).body))).toEqual({ expected_revision: 1, submission_id: delivery.id, outcome, reason: "  人工核对后的结论  " });
  const finalRead = deferred<Response>(); details.mockReturnValueOnce(finalRead.promise);
  current = outcome === "accepted" ? acceptedTask : { ...task, revision: 2 };
  await act(async () => reviewing.resolve(json({ ...decision, outcome, reason: "  人工核对后的结论  " }, 201)));
  const receipt = within(detail().getByRole("region", { name: "审查回执" }));
  expect(receipt.getByText(decision.id)).toBeVisible();
  expect(receipt.getByText(label)).toBeVisible();
  expect(receipt.getByText("  人工核对后的结论  ", { normalizer: text => text })).toBeVisible();
  expect(detail().getByText("正在载入任务详情…")).toBeVisible();
  await act(async () => finalRead.resolve(json(current)));
  expect(detail().getByText(outcome === "accepted" ? "已接纳" : "待交付")).toBeVisible();
  if (outcome === "accepted") {
    await detail().findByText(summary, { normalizer: text => text });
    expect(detail().queryByRole("button", { name: /登记人工尝试|提交交付|接纳交付|退回修改/ })).not.toBeInTheDocument();
  } else expect(detail().getByRole("button", { name: "登记人工尝试" })).toBeEnabled();
  expect(reads).toHaveBeenCalledTimes(4); expect(details).toHaveBeenCalledTimes(4);
  expect(new Set(writes.mock.calls.map((_, index) => captured(index).key)).size).toBe(3);
  expect(writes.mock.calls.every(([, init]) => init?.redirect === "error")).toBe(true);
});

it("submits a FEFF-only human summary without trimming the valid original text", async () => {
  const actual = { ...delivery, summary: "\uFEFF" };
  evidence.mockImplementation(async url => json(String(url).includes("/attempts/") ? humanAttempt : actual));
  writes.mockResolvedValueOnce(json(actual, 201));
  await open(activeTask); await detail().findByRole("textbox", { name: "交付摘要" });
  change("交付摘要", actual.summary); current = submittedTask;
  fireEvent.submit(detail().getByRole("form", { name: "提交交付" }));
  expect(writes).toHaveBeenCalledTimes(1);
  expect(JSON.parse(String(captured(0).body))).toEqual({ expected_revision: 1, attempt_id: humanAttempt.id, summary: "\uFEFF" });
  await detail().findByText(`已确认操作回执：${actual.id}。`);
  expect(await detail().findByText(actual.summary, { normalizer: text => text })).toBeVisible();
  expect(detail().queryByRole("alert")).not.toBeInTheDocument();
});

it("rejects a U+0085-only human summary locally without changing the original draft", async () => {
  await open(activeTask); await detail().findByRole("textbox", { name: "交付摘要" });
  change("交付摘要", "\u0085");
  fireEvent.submit(detail().getByRole("form", { name: "提交交付" }));
  expect(writes).not.toHaveBeenCalled();
  expect(detail().getByRole("alert")).toHaveTextContent("请输入交付摘要");
  expect(detail().getByRole("textbox", { name: "交付摘要" })).toHaveValue("\u0085");
  expect(detail().getByRole("textbox", { name: "交付摘要" })).toBeEnabled();
});

it.each([
  ["read failure", null], ["wrong group", { ...humanAttempt, group_id: other.id }],
  ["wrong task", { ...humanAttempt, task_id: secondTask.id }], ["wrong pointer", { ...humanAttempt, id: "old-attempt" }],
  ["wrong revision", { ...humanAttempt, task_revision: 2 }],
] as const)("fails closed on %s Attempt evidence until explicit matching recovery", async (_name, invalid) => {
  evidence.mockResolvedValueOnce(invalid === null ? json({}, 503) : json(invalid));
  await open(activeTask);
  await detail().findByRole("alert");
  expect(detail().queryByRole("form", { name: "提交交付" })).not.toBeInTheDocument();
  expect(detail().queryByRole("button", { name: "登记人工尝试" })).not.toBeInTheDocument();
  expect(writes).not.toHaveBeenCalled();
  const recovery = deferred<Response>(); evidence.mockReturnValueOnce(recovery.promise);
  refresh(); await detail().findByText("正在载入当前尝试…");
  expect(detail().queryByRole("form", { name: "提交交付" })).not.toBeInTheDocument();
  await act(async () => recovery.resolve(json(humanAttempt)));
  expect(detail().getByRole("textbox", { name: "交付摘要" })).toBeEnabled();
  expect(details).toHaveBeenCalledTimes(2); expect(evidence).toHaveBeenCalledTimes(2);
  expect(evidence.mock.calls.every(([url]) => String(url).endsWith(`/attempts/${humanAttempt.id}`))).toBe(true);
  expect(writes).not.toHaveBeenCalled();
});

it("reads the exact active member Attempt without allowing replacement or human submission", async () => {
  const pending = deferred<Response>(); evidence.mockReturnValueOnce(pending.promise);
  await open(activeTask);
  expect(detail().getByText("正在载入当前尝试…")).toBeVisible();
  expect(detail().queryByRole("button", { name: "登记人工尝试" })).not.toBeInTheDocument();
  expect(detail().queryByRole("form", { name: "提交交付" })).not.toBeInTheDocument();
  await act(async () => pending.resolve(json({ ...humanAttempt, turn_id: "member-turn" })));
  expect(detail().getByText("该尝试属于成员，不能人工替换或提交交付。")).toBeVisible();
  expect(detail().getByText("member-turn")).toBeVisible();
  expect(detail().queryByRole("form", { name: "提交交付" })).not.toBeInTheDocument();
  expect(evidence.mock.calls[0][0]).toBe(`/api/v1/groups/${group.id}/tasks/${task.id}/attempts/${humanAttempt.id}`);
  expect(writes).not.toHaveBeenCalled();
});

it.each([
  ["read failure", null], ["wrong group", { ...delivery, group_id: other.id }],
  ["wrong task", { ...delivery, task_id: secondTask.id }], ["wrong submission pointer", { ...delivery, id: "old-submission" }],
  ["wrong revision", { ...delivery, task_revision: 2 }], ["wrong attempt pointer", { ...delivery, attempt_id: "old-attempt" }],
  ["malformed refs", { ...delivery, artifact_versions: [{ artifact_id: "report", version_id: "" }] }],
] as const)("keeps review unusable on %s Submission evidence and recovers only after matching GET evidence", async (_name, invalid) => {
  evidence.mockResolvedValueOnce(invalid === null ? json({}, 503) : json(invalid));
  await open(submittedTask);
  expect(await detail().findByRole("alert")).not.toHaveTextContent("English");
  noUsableReview();
  expect(detail().getByRole("textbox", { name: "验收说明（可选）" })).toBeDisabled();
  expect(detail().queryByText(summary, { normalizer: text => text })).not.toBeInTheDocument();
  const recovery = deferred<Response>(); evidence.mockReturnValueOnce(recovery.promise);
  refresh(); await detail().findByText("正在载入当前交付…");
  noUsableReview(); expect(writes).not.toHaveBeenCalled();
  await act(async () => recovery.resolve(json(delivery)));
  expect(detail().getByText(summary, { normalizer: text => text })).toBeVisible();
  expect(detail().getByRole("button", { name: "接纳交付" })).toBeEnabled();
  expect(detail().getByRole("button", { name: "退回修改" })).toBeEnabled();
  expect(details.mock.calls.every(([url]) => String(url).endsWith(`/tasks/${task.id}`))).toBe(true);
  expect(evidence.mock.calls.every(([url]) => String(url).endsWith(`/submissions/${delivery.id}`))).toBe(true);
  expect(writes).not.toHaveBeenCalled();
});

it("renders actual member Submission pairs read-only, without inventing filenames or sending refs in a Review", async () => {
  const actual = { ...delivery, summary: "成员提交的真实成果", submitted_by: "member-1", artifact_versions: frozenRefs };
  evidence.mockResolvedValueOnce(json(actual));
  await open(submittedTask);
  await detail().findByText(actual.summary);
  expect(detail().getByText("提交者：member-1")).toBeVisible();
  const refs = within(detail().getByRole("list", { name: "冻结成果版本" }));
  expect(refs.getAllByRole("listitem")).toHaveLength(3);
  for (const ref of frozenRefs) expect(refs.getByText(`${ref.artifact_id} / ${ref.version_id}`)).toBeVisible();
  expect(detail().queryByRole("link")).not.toBeInTheDocument();
  expect(detail().queryByRole("combobox")).not.toBeInTheDocument();
  expect(detail().queryByRole("textbox", { name: /文件|成果版本|JSON/ })).not.toBeInTheDocument();
  expect(writes).not.toHaveBeenCalled();
  writes.mockResolvedValueOnce(json({ ...decision, artifact_versions: frozenRefs }, 201));
  current = acceptedTask;
  fireEvent.click(detail().getByRole("button", { name: "接纳交付" }));
  await detail().findByRole("region", { name: "审查回执" });
  expect(JSON.parse(String(captured(0).body))).toEqual({ expected_revision: 1, submission_id: delivery.id, outcome: "accepted", reason: "" });
  expect([...reads.mock.calls, ...details.mock.calls, ...evidence.mock.calls, ...writes.mock.calls]
    .some(([url]) => String(url).includes("/artifacts"))).toBe(false);
});

it("loads accepted Task and Submission read-only without fabricating a historical Review", async () => {
  evidence.mockResolvedValueOnce(json({ ...delivery, artifact_versions: frozenRefs }));
  await open(acceptedTask);
  await detail().findByText(summary, { normalizer: text => text });
  expect(detail().getByText("已接纳")).toBeVisible();
  expect(detail().getByRole("list", { name: "冻结成果版本" }).children).toHaveLength(3);
  expect(detail().queryByRole("textbox")).not.toBeInTheDocument();
  expect(detail().queryByRole("button", { name: /登记人工尝试|提交交付|接纳交付|退回修改/ })).not.toBeInTheDocument();
  expect(detail().queryByRole("region", { name: "审查回执" })).not.toBeInTheDocument();
  expect(detail().queryByText(/审查者|验收说明|已审读/)).not.toBeInTheDocument();
  expect(writes).not.toHaveBeenCalled();
});

it("reports unavailable accepted Submission evidence read-only and recovers explicitly without inventing Review history", async () => {
  evidence.mockResolvedValueOnce(json({}, 503));
  await open(acceptedTask);
  expect(await detail().findByText(/HTTP 503/)).toHaveAttribute("role", "status");
  expect(detail().queryByRole("button", { name: /登记人工尝试|提交交付|接纳交付|退回修改/ })).not.toBeInTheDocument();
  expect(detail().queryByRole("region", { name: "审查回执" })).not.toBeInTheDocument();
  refresh(); await detail().findByText(summary, { normalizer: text => text });
  expect(writes).not.toHaveBeenCalled();
});

it.each(kinds)("retains pending and unknown %s across tasks, groups and null-hidden panels despite newer or failed Task GETs", async kind => {
  const sameIdOther = { ...task, group_id: other.id, title: "另一群的同标识任务" };
  reads.mockImplementation(async url => page(String(url).includes(other.id) ? [sameIdOther] : [current, secondTask]));
  details.mockImplementation(async url => json(String(url).includes(other.id) ? sameIdOther : String(url).endsWith(secondTask.id) ? secondTask : current));
  const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise);
  const view = await open(initialState(kind));
  await perform(kind);
  const original = captured(0);
  const field = kind === "attempt" ? null : detail().getByRole("textbox", { name: kind === "submission" ? "交付摘要" : "验收说明（可选）" });
  if (field) expect(field).toBeDisabled();
  fireEvent.click(row(secondTask.title)); await detail().findByRole("heading", { name: secondTask.title });
  expect(detail().getByRole("button", { name: "登记人工尝试" })).toBeEnabled();
  view.rerender(ui(other));
  fireEvent.click(await panel().findByRole("button", { name: `${sameIdOther.title}，待交付` }));
  await detail().findByRole("heading", { name: sameIdOther.title });
  expect(detail().getByRole("button", { name: "登记人工尝试" })).toBeEnabled();
  view.rerender(ui(null));
  expect(screen.queryByRole("region", { name: "群组任务" })).not.toBeInTheDocument();
  const readCount = reads.mock.calls.length + details.mock.calls.length + evidence.mock.calls.length;
  expect(writes.mock.calls[0][1]?.signal?.aborted).not.toBe(true);
  await act(async () => pending.resolve(json({}, 503)));
  expect(reads.mock.calls.length + details.mock.calls.length + evidence.mock.calls.length).toBe(readCount);
  expect(writes).toHaveBeenCalledTimes(1);
  current = { ...task, revision: 7 };
  view.rerender(ui(group)); await detail().findByRole("heading", { name: secondTask.title });
  fireEvent.click(row()); await detail().findByText("7");
  expect(detail().getByRole("alert")).toHaveTextContent("操作结果未确认");
  expect(detail().getByRole("button", { name: "登记人工尝试" })).toBeDisabled();
  noUsableReview();
  details.mockResolvedValueOnce(json({}, 503)); refresh(); await detail().findByRole("button", { name: "重试载入详情" });
  expect(detail().getByRole("button", { name: "重试同一次操作" })).toBeEnabled();
  fireEvent.click(detail().getByRole("button", { name: "重试载入详情" }));
  await detail().findByText("7");
  expect(detail().getByRole("button", { name: "登记人工尝试" })).toBeDisabled();
  writes.mockResolvedValueOnce(json(receiptFor(kind), 201));
  fireEvent.click(detail().getByRole("button", { name: "重试同一次操作" }));
  await detail().findByText(`已确认操作回执：${receiptFor(kind).id}。`);
  expect(captured(1)).toEqual(original);
  expect(JSON.parse(String(captured(1).body)).expected_revision).toBe(1);
  await detail().findByText("7");
  expect(detail().getByRole("button", { name: "登记人工尝试" })).toBeEnabled();
  expect(writes).toHaveBeenCalledTimes(2);
});

it.each(kinds.flatMap(kind => ["network", "408", "503", "200", "202", "malformed", "wrong receipt"].map(failure => ({ kind, failure }))))(
  "keeps $kind $failure unknown, with only explicit same-operation retry", async ({ kind, failure }) => {
    const invalid = kind === "attempt" ? { ...humanAttempt, task_revision: 2 }
      : kind === "submission" ? { ...delivery, attempt_id: "wrong-attempt" } : { ...decision, artifact_versions: frozenRefs };
    if (failure === "network") writes.mockRejectedValueOnce(new TypeError("English network diagnostic"));
    else writes.mockResolvedValueOnce(failure === "malformed" ? new Response("not JSON", { status: 201 })
      : failure === "wrong receipt" ? json(invalid, 201) : json(receiptFor(kind), Number(failure)));
    await open(initialState(kind)); await perform(kind);
    expect(await detail().findByRole("alert")).toHaveTextContent("操作结果未确认");
    expect(detail().getByRole("alert")).not.toHaveTextContent("English");
    noUsableReview();
    expect(detail().queryByText(/已确认操作回执/)).not.toBeInTheDocument();
    expect(details).toHaveBeenCalledTimes(1); expect(writes).toHaveBeenCalledTimes(1);
    writes.mockResolvedValueOnce(json(receiptFor(kind), 201));
    fireEvent.click(detail().getByRole("button", { name: "重试同一次操作" }));
    await detail().findByText(`已确认操作回执：${receiptFor(kind).id}。`);
    expect(captured(1)).toEqual(captured(0));
  },
);

it.each(kinds.flatMap(kind => [404, 400, 409].map(status => ({ kind, status }))))(
  "retains an unknown $kind after retry HTTP $status until its original 201 despite newer GETs", async ({ kind, status }) => {
    const originalReason = "  原始审查说明  ";
    if (kind === "review") evidence.mockResolvedValueOnce(json({ ...delivery, artifact_versions: frozenRefs }));
    writes.mockRejectedValueOnce(new TypeError("Lost original response"));
    await open(initialState(kind));
    if (kind === "review") {
      await detail().findByText(summary, { normalizer: text => text });
      change("验收说明（可选）", originalReason);
      for (const ref of frozenRefs) expect(detail().getByText(`${ref.artifact_id} / ${ref.version_id}`)).toBeVisible();
    }
    await perform(kind);
    expect(await detail().findByRole("alert")).toHaveTextContent("操作结果未确认");
    const original = captured(0);
    writes.mockResolvedValueOnce(json({ error: { code: "unavailable", message: "English diagnostic" } }, status));
    fireEvent.click(detail().getByRole("button", { name: "重试同一次操作" }));
    const alert = await detail().findByRole("alert");
    expect(alert).toHaveTextContent(String(status));
    expect(alert).toHaveTextContent("操作结果未确认");
    expect(alert).not.toHaveTextContent("English");
    expect(detail().getByRole("button", { name: "重试同一次操作" })).toBeEnabled();
    if (kind === "attempt") expect(detail().getByRole("button", { name: "登记人工尝试" })).toBeDisabled();
    else {
      expect(detail().getByRole("textbox", { name: kind === "submission" ? "交付摘要" : "验收说明（可选）" })).toBeDisabled();
      expect(detail().getByRole("button", { name: kind === "submission" ? "提交交付" : "退回修改" })).toBeDisabled();
    }
    expect(writes).toHaveBeenCalledTimes(2); expect(captured(1)).toEqual(original);

    const latestRefs = frozenRefs.map(ref => ({ ...ref, version_id: `new-${ref.version_id}` }));
    current = { ...initialState(kind), revision: 7, active_attempt_id: kind === "attempt" ? "" : "next-attempt",
      current_submission_id: kind === "review" ? "next-submission" : "" };
    evidence.mockImplementation(async url => json(String(url).includes("/attempts/")
      ? { ...humanAttempt, id: "next-attempt", task_revision: 7 }
      : { ...delivery, id: "next-submission", attempt_id: "next-attempt", task_revision: 7,
        summary: "新的真实交付", artifact_versions: latestRefs }));
    refresh(); await detail().findByText("7");
    expect(detail().getByRole("button", { name: "重试同一次操作" })).toBeEnabled();
    if (kind === "attempt") expect(detail().getByRole("button", { name: "登记人工尝试" })).toBeDisabled();
    else if (kind === "submission") expect(await detail().findByRole("textbox", { name: "交付摘要" })).toBeDisabled();
    else {
      await detail().findByText("新的真实交付"); noUsableReview();
      expect(detail().getByRole("textbox", { name: "验收说明（可选）" })).toHaveValue(originalReason);
      for (const ref of latestRefs) expect(detail().getByText(`${ref.artifact_id} / ${ref.version_id}`)).toBeVisible();
      // Only the first Submission's refs may validate the retried Review, not the newer evidence above.
      writes.mockResolvedValueOnce(json({ ...decision, reason: originalReason, artifact_versions: latestRefs }, 201));
      fireEvent.click(detail().getByRole("button", { name: "重试同一次操作" }));
      expect(await detail().findByRole("alert")).toHaveTextContent("操作结果未确认");
      expect(detail().queryByRole("region", { name: "审查回执" })).not.toBeInTheDocument();
      expect(captured(2)).toEqual(original); noUsableReview();
    }
    const receipt = kind === "review" ? { ...decision, reason: originalReason, artifact_versions: frozenRefs } : receiptFor(kind);
    writes.mockResolvedValueOnce(json(receipt, 201));
    const finalIndex = writes.mock.calls.length;
    fireEvent.click(detail().getByRole("button", { name: "重试同一次操作" }));
    await detail().findByText(`已确认操作回执：${receipt.id}。`);
    expect(captured(finalIndex)).toEqual(original);
    expect(JSON.parse(String(captured(finalIndex).body)).expected_revision).toBe(1);
    expect(detail().queryByRole("button", { name: "重试同一次操作" })).not.toBeInTheDocument();
    await detail().findByText("7");
    if (kind === "review") expect(within(detail().getByRole("region", { name: "审查回执" }))
      .getByText(originalReason, { normalizer: text => text })).toBeVisible();
  },
);

it.each(kinds.flatMap(kind => [400, 404, 409].map(status => ({ kind, status }))))(
  "allows a new $kind key only after definitive HTTP $status and an explicit corrected action", async ({ kind, status }) => {
    writes.mockResolvedValueOnce(json({ error: { code: "conflict", message: "English diagnostic" } }, status));
    await open(initialState(kind)); await perform(kind);
    const alert = await detail().findByRole("alert");
    expect(alert).toHaveTextContent(String(status)); expect(alert).not.toHaveTextContent("English");
    if (status === 409) expect(alert).toHaveTextContent("任务状态、版本、依赖或请求内容冲突");
    expect(detail().queryByRole("button", { name: "重试同一次操作" })).not.toBeInTheDocument();
    expect(writes).toHaveBeenCalledTimes(1);
    current = { ...initialState(kind), revision: 2 };
    evidence.mockImplementation(async url => json(String(url).includes("/attempts/")
      ? { ...humanAttempt, task_revision: 2 } : { ...delivery, task_revision: 2 }));
    refresh(); await detail().findByText("2");
    writes.mockResolvedValueOnce(json({ ...receiptFor(kind), task_revision: 2 }, 201));
    await perform(kind); await detail().findByText(`已确认操作回执：${receiptFor(kind).id}。`);
    expect(captured(1).key).not.toBe(captured(0).key);
    expect(JSON.parse(String(captured(1).body)).expected_revision).toBe(2);
  },
);

it.each(kinds.flatMap(kind => ["task", "group", "null"].map(boundary => ({ kind, boundary }))))(
  "keeps late $kind receipts hidden across $boundary navigation without changing visible selection, focus or B GETs", async ({ kind, boundary }) => {
    const a = initialState(kind), b = { ...secondTask, group_id: boundary === "group" ? other.id : group.id };
    reads.mockImplementation(async url => page(String(url).includes(other.id) ? [b] : [a, secondTask]));
    details.mockImplementation(async url => json(String(url).endsWith(secondTask.id)
      ? String(url).includes(other.id) ? b : secondTask : a));
    const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise);
    const view = await open(a); await perform(kind);
    const aDetail = panel().getByRole("region", { name: "任务详情" });
    if (boundary === "group") view.rerender(ui(other));
    if (boundary === "null") view.rerender(ui(null));
    else {
      fireEvent.click(await panel().findByRole("button", { name: `${b.title}，待交付` }));
      await detail().findByRole("heading", { name: b.title }); row(b.title).focus();
    }
    const focused = boundary === "null" ? screen.getByRole("button", { name: "聊天输入" }) : row(b.title);
    focused.focus();
    const readCount = reads.mock.calls.length + details.mock.calls.length + evidence.mock.calls.length;
    expect(writes.mock.calls[0][1]?.signal?.aborted).not.toBe(true);
    await act(async () => pending.resolve(json(receiptFor(kind), 201)));
    const hiddenReceipt = within(aDetail).getByText(`已确认操作回执：${receiptFor(kind).id}。`);
    expect(hiddenReceipt).toBeInTheDocument(); expect(hiddenReceipt).not.toBeVisible();
    expect(focused).toHaveFocus();
    if (boundary === "null") expect(screen.queryByRole("region", { name: "群组任务" })).not.toBeInTheDocument();
    else {
      expect(detail().getByRole("heading", { name: b.title })).toBeVisible();
      expect(row(b.title)).toHaveAttribute("aria-pressed", "true");
    }
    expect(reads.mock.calls.length + details.mock.calls.length + evidence.mock.calls.length).toBe(readCount);
    if (boundary !== "task") view.rerender(ui(group));
    fireEvent.click(row(a.title)); await detail().findByRole("heading", { name: a.title });
    expect(detail().getByText(`已确认操作回执：${receiptFor(kind).id}。`)).toBeVisible();
    expect(writes).toHaveBeenCalledTimes(1);
  },
);

it.each((["attempt", "submission"] as const).flatMap(kind => ["task", "group", "revision", "pointer", "null"].map(boundary => ({ kind, boundary }))))(
  "ignores late $kind GET evidence across $boundary even when transport ignores abort", async ({ kind, boundary }) => {
    const first = kind === "attempt" ? activeTask : submittedTask;
    const next: Task = { ...first, id: boundary === "task" ? secondTask.id : task.id,
      group_id: boundary === "group" ? other.id : group.id, title: "新的权威任务",
      revision: boundary === "revision" ? 2 : 1, active_attempt_id: "next-attempt",
      current_submission_id: kind === "submission" ? "next-submission" : "" };
    const old = deferred<Response>(); evidence.mockReturnValueOnce(old.promise);
    const view = await open(first);
    await detail().findByText(kind === "attempt" ? "正在载入当前尝试…" : "正在载入当前交付…");
    const fresh = kind === "attempt" ? { ...humanAttempt, id: next.active_attempt_id, group_id: next.group_id,
      task_id: next.id, task_revision: next.revision, turn_id: "member-turn" }
      : { ...delivery, id: next.current_submission_id, group_id: next.group_id, task_id: next.id,
        task_revision: next.revision, attempt_id: next.active_attempt_id, summary: "新的真实交付", artifact_versions: frozenRefs };
    evidence.mockResolvedValueOnce(json(fresh)); current = next;
    if (boundary === "task") {
      reads.mockResolvedValueOnce(page([first, next])); fireEvent.click(panel().getByRole("button", { name: "刷新任务" }));
      fireEvent.click(await panel().findByRole("button", { name: `${next.title}，${kind === "attempt" ? "待交付" : "待验收"}` }));
    } else if (boundary === "group") {
      view.rerender(ui(other));
      fireEvent.click(await panel().findByRole("button", { name: `${next.title}，${kind === "attempt" ? "待交付" : "待验收"}` }));
    } else if (boundary === "null") { view.rerender(ui(null)); view.rerender(ui(group)); }
    else refresh();
    await detail().findByText(kind === "attempt" ? "该尝试属于成员，不能人工替换或提交交付。" : "新的真实交付");
    await act(async () => old.resolve(json(kind === "attempt" ? humanAttempt : delivery)));
    expect(evidence.mock.calls[0][1]?.signal?.aborted).toBe(true);
    expect(detail().getByRole("heading", { name: next.title })).toBeVisible();
    if (kind === "attempt") expect(detail().queryByRole("form", { name: "提交交付" })).not.toBeInTheDocument();
    else {
      expect(detail().getByText("新的真实交付")).toBeVisible();
      const staleSummary = detail().queryByText(summary, { normalizer: text => text });
      if (staleSummary) expect(staleSummary).not.toBeVisible();
      expect(detail().getByRole("button", { name: "接纳交付" })).toBeEnabled();
    }
    expect(detail().queryByRole("alert")).toBeNull(); expect(writes).not.toHaveBeenCalled();
  },
);

it.each(["submission", "review"] as const)("retains independent %s drafts per task and group when hidden", async kind => {
  const first = initialState(kind), b = { ...first, id: secondTask.id, title: secondTask.title };
  const otherTask = { ...first, group_id: other.id, title: "另一群的任务" };
  reads.mockImplementation(async url => page(String(url).includes(other.id) ? [otherTask] : [first, b]));
  details.mockImplementation(async url => json(String(url).includes(other.id) ? otherTask : String(url).endsWith(b.id) ? b : first));
  evidence.mockImplementation(async url => {
    const groupId = String(url).includes(other.id) ? other.id : group.id;
    const taskId = String(url).includes(`/tasks/${b.id}/`) ? b.id : task.id;
    return json(kind === "submission" ? { ...humanAttempt, group_id: groupId, task_id: taskId }
      : { ...delivery, group_id: groupId, task_id: taskId });
  });
  const view = await open(first), label = kind === "submission" ? "交付摘要" : "验收说明（可选）";
  const aField = await detail().findByRole("textbox", { name: label }); change(label, "A 的原始草稿");
  fireEvent.click(row(b.title)); await detail().findByRole("textbox", { name: label }); change(label, "B 的独立草稿");
  expect(aField).not.toBeVisible();
  view.rerender(ui(other)); fireEvent.click(await panel().findByRole("button", { name: new RegExp(`^${otherTask.title}，`) }));
  await detail().findByRole("textbox", { name: label }); change(label, "另一群的草稿");
  view.rerender(ui(null)); expect(screen.queryByRole("textbox")).toBeNull();
  view.rerender(ui(group)); await detail().findByRole("textbox", { name: label });
  expect(detail().getByRole("textbox", { name: label })).toHaveValue("B 的独立草稿");
  fireEvent.click(row()); await detail().findByRole("textbox", { name: label });
  expect(detail().getByRole("textbox", { name: label })).toHaveValue("A 的原始草稿");
  view.rerender(ui(other)); await detail().findByRole("textbox", { name: label });
  expect(detail().getByRole("textbox", { name: label })).toHaveValue("另一群的草稿");
  expect(writes).not.toHaveBeenCalled();
});

it("keeps a real native-Fetch 307 Review unknown without forwarding, and freezes first evidence across retries and newer GETs", async () => {
  const actual = { ...delivery, artifact_versions: frozenRefs.map(ref => ({ ...ref })) };
  const sourceResponse = json(actual); vi.spyOn(sourceResponse, "json").mockResolvedValue(actual);
  evidence.mockResolvedValueOnce(sourceResponse);
  const requests: { url: string; method: string; body: string; key: string | undefined }[] = [];
  const posts: Promise<Response>[] = [];
  let receiptRefs = frozenRefs;
  const server = createServer(async (request, response) => {
    const chunks: Buffer[] = [];
    for await (const chunk of request) chunks.push(Buffer.from(chunk));
    requests.push({ url: request.url ?? "", method: request.method ?? "", body: Buffer.concat(chunks).toString(),
      key: request.headers["idempotency-key"] as string | undefined });
    if (request.url === "/forwarded") { response.writeHead(201); response.end(JSON.stringify(decision)); }
    else if (requests.length === 1) { response.writeHead(307, { Location: "/forwarded" }); response.end(); }
    else { response.writeHead(201, { "Content-Type": "application/json" }); response.end(JSON.stringify({ ...decision, artifact_versions: receiptRefs })); }
  });
  try {
    await new Promise<void>((resolve, reject) => { server.once("error", reject); server.listen(0, "127.0.0.1", resolve); });
    const address = server.address();
    if (address === null || typeof address === "string") throw new Error("Missing fixture port");
    vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === "POST") {
        const post = nativeFetch(new URL(String(input), `http://127.0.0.1:${address.port}`), init);
        posts.push(post); return post;
      }
      return (/\/(attempts|submissions)\//.test(String(input)) ? evidence : String(input).includes("/tasks?") ? reads : details)(input, init);
    });
    await open(submittedTask); await perform("review");
    // Event dispatch does not await the native transport or its React state updates.
    await act(async () => {
      expect(posts).toHaveLength(1);
      await expect(posts[0]).rejects.toThrow();
    });
    expect(detail().getByRole("alert")).toHaveTextContent("操作结果未确认");
    expect(requests).toHaveLength(1); expect(requests[0].method).toBe("POST");
    expect(requests[0].url).toBe(`/api/v1/groups/${group.id}/tasks/${task.id}/reviews`);
    expect(JSON.parse(requests[0].body)).toEqual({ expected_revision: 1, submission_id: delivery.id, outcome: "accepted", reason: "" });
    // A shared caller object and later GET can change without rebasing the unknown operation's validation evidence.
    actual.artifact_versions[0].version_id = "new-head";
    actual.artifact_versions.push({ artifact_id: "new-artifact", version_id: "v9" });
    evidence.mockResolvedValueOnce(json(actual)); refresh();
    await detail().findByText("report/一 / new-head"); noUsableReview();
    receiptRefs = actual.artifact_versions;
    fireEvent.click(detail().getByRole("button", { name: "重试同一次操作" }));
    expect(await detail().findByRole("alert")).toHaveTextContent("操作结果未确认");
    expect(detail().queryByRole("region", { name: "审查回执" })).not.toBeInTheDocument();
    expect(requests).toHaveLength(2); expect(requests[1]).toEqual(requests[0]);
    current = { ...task, title: "后来重新打开的任务", revision: 7 };
    refresh(); await detail().findByRole("heading", { name: current.title });
    expect(detail().getByRole("button", { name: "登记人工尝试" })).toBeDisabled();
    receiptRefs = frozenRefs;
    fireEvent.click(detail().getByRole("button", { name: "重试同一次操作" }));
    await detail().findByRole("region", { name: "审查回执" });
    await detail().findByRole("heading", { name: current.title });
    expect(detail().getByText("待交付")).toBeVisible(); expect(detail().getByText("7")).toBeVisible();
    expect(within(detail().getByRole("region", { name: "审查回执" })).getByText("接纳交付")).toBeVisible();
    expect(requests).toHaveLength(3); expect(requests[2]).toEqual(requests[0]);
    expect(requests.some(request => request.url === "/forwarded")).toBe(false);
  } finally {
    await new Promise<void>((resolve, reject) => {
      server.close(error => error ? reject(error) : resolve()); server.closeAllConnections();
    });
  }
});
