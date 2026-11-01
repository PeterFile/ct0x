import { createRef, StrictMode, useRef } from "react";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { RequestWork } from "./RequestWork";
import type { RequestWorkHandle } from "./RequestWork";
import type { Member, Message } from "./api";
import type { AdmissionReceipt, RequestWorkInput } from "./coordination-api";
import type { Task } from "./task-api";
import { deferred, group, json, other } from "./test/groups";
import { member } from "./test/members";
import { message } from "./test/messages";

const agent: Message = { ...message, id: "message-agent", sender_id: member.id, topic_id: "topic-agent", content: "建议不等于授权" };
const foreign: Message = { ...message, id: "message-b", group_id: other.id, content: "另一群的消息" };
const sources = [message, agent, foreign];
const successText = "授权已登记，任务已创建；尚未自动启动执行。";
const input = { title: "任务目标", description: "任务说明", acceptance: "可核验结果", reason: "我明确要求这项工作" };
const writes = vi.fn<typeof fetch>(), reads = vi.fn<typeof fetch>();
function admission(groupId: string, sent: RequestWorkInput): AdmissionReceipt {
  const taskId = `task-${sent.message_id}`, decisionId = `decision-${sent.message_id}`, mandateId = `mandate-${sent.message_id}`;
  const triggerId = `trigger-${sent.message_id}`, created_at = "2026-11-01T08:00:00.123456789Z";
  return {
    decision: { id: decisionId, trigger_id: triggerId, group_id: groupId, topic_id: sent.topic_id, kind: "create_new",
      actor_id: "user", policy_revision: "local-user-explicit/v1", reason: sent.reason, task_id: taskId,
      task_revision_at_admission: 1, mandate_id: mandateId, created_at },
    mandate: { id: mandateId, decision_id: decisionId, origin_trigger_id: triggerId, group_id: groupId, topic_id: sent.topic_id,
      task_id: taskId, task_revision_at_admission: 1, authority_kind: "local_user_explicit", authorized_by: "user",
      strength: "explicit", policy_revision: "local-user-explicit/v1", created_at,
      scope: { objective: sent.new_task.title, description: sent.new_task.description ?? "", acceptance: sent.new_task.acceptance,
        assignee_id: "", dependency_ids: [] } },
    task: { id: taskId, group_id: groupId, revision_at_admission: 1 },
  };
}
function currentTask(groupId = group.id, id = `task-${message.id}`, changes: Partial<Task> = {}): Task {
  return { id, group_id: groupId, title: "当前任务标题", description: "当前描述", acceptance: "当前验收", assignee_id: "",
    dependency_ids: [], status: "open", revision: 1, active_attempt_id: "", current_submission_id: "",
    created_at: message.created_at, ...changes };
}
function Harness({ activeGroup = group.id, items = sources, members = [member] }: {
  activeGroup?: string; items?: Message[]; members?: Member[];
}) {
  const a = useRef<RequestWorkHandle>(null), b = useRef<RequestWorkHandle>(null);
  return <>
    <button type="button">外部焦点</button>
    <button type="button" onClick={event => a.current?.open(foreign, event.currentTarget)}>错误群来源</button>
    {[group.id, other.id].map(id => <section key={id} aria-label={`群 ${id}`} hidden={activeGroup !== "both" && activeGroup !== id}>
      {items.filter(source => source.group_id === id).map(source => <button key={source.id} type="button"
        onClick={event => (id === group.id ? a : b).current?.open(source, event.currentTarget)}>选择 {source.id}</button>)}
      <RequestWork groupId={id} active={activeGroup === "both" || activeGroup === id} members={members} ref={id === group.id ? a : b} />
    </section>)}
  </>;
}
const scope = (id = group.id) => within(screen.getByRole("region", { name: `群 ${id}` }));
const panel = (id = group.id) => within(scope(id).getByRole("region", { name: "从消息创建任务" }));
const receipt = (id = group.id) => within(panel(id).getByRole("region", { name: "授权回执" }));
const current = (id = group.id) => within(panel(id).getByRole("region", { name: "当前任务" }));
const field = (name: string, id = group.id) => panel(id).getByRole("textbox", { name });
const change = (name: string, value: string, id = group.id) => fireEvent.change(field(name, id), { target: { value } });
const select = (id = message.id) => fireEvent.click(screen.getByRole("button", { name: `选择 ${id}` }));
const submit = (id = group.id) => fireEvent.submit(panel(id).getByRole("form", { name: "从消息创建任务" }));
const close = (id = group.id) => fireEvent.click(panel(id).getByRole("button", { name: "收起" }));
const restore = (id = group.id) => fireEvent.click(scope(id).getByRole("button", { name: /继续任务草稿|查看任务创建进度|恢复未确认的任务创建|查看授权回执/ }));
function fill(id = group.id) {
  change("任务标题", input.title, id); change("任务描述（可选）", input.description, id);
  change("验收条件", input.acceptance, id); change("授权理由", input.reason, id);
}
const attempt = (index: number) => {
  const [url, init] = writes.mock.calls[index];
  return { url, body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key") };
};
function expand(name: string, id = group.id) {
  const summary = panel(id).getByText(name, { selector: "summary" });
  const details = summary.closest("details")!;
  if (!details.open) fireEvent.click(summary);
  return details;
}
beforeEach(() => {
  writes.mockReset().mockImplementation(async (url, init) => json(admission(decodeURIComponent(String(url).split("/")[4]), JSON.parse(String(init?.body))), 201));
  reads.mockReset().mockImplementation(async url => {
    const parts = String(url).split("/");
    return json(currentTask(decodeURIComponent(parts[4]), decodeURIComponent(parts[6])));
  });
  vi.stubGlobal("fetch", vi.fn((url: RequestInfo | URL, init?: RequestInit) => (init?.method === "POST" ? writes : reads)(url, init)));
});
afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });

it("starts without a source, form or reads and ignores a foreign-group source", () => {
  render(<Harness />);
  expect(screen.queryByRole("form")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "收起" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "错误群来源" }));
  expect(screen.queryByRole("form")).not.toBeInTheDocument();
  expect(fetch).not.toHaveBeenCalled();
});
it.each([
  { sender: "user", roster: [{ ...member, id: "user", name: "伪造本地名称" }], author: "我" },
  { sender: member.id, roster: [member], author: member.name },
  { sender: "unloaded", roster: [], author: "未载入成员" },
  { sender: member.id, roster: [{ ...member, group_id: other.id, name: "外群同名" }], author: "未载入成员" },
  { sender: member.id, roster: [{ ...member, status: "left", name: "历史成员" }], author: "历史成员" },
])("uses real scoped author identity for $author / $sender", ({ sender, roster, author }) => {
  render(<Harness items={[{ ...message, sender_id: sender }]} members={roster} />); select();
  expect(panel().getByText(author, { exact: true })).toBeVisible();
  expect(panel().queryByText(/伪造本地名称|外群同名/)).not.toBeInTheDocument();
  expect(panel().getByRole("textbox", { name: "验收条件" })).toHaveValue("");
  expect(panel().getByRole("textbox", { name: "授权理由" })).toHaveValue("");
  const details = expand("完整来源与标识");
  expect(within(details).getByText(sender, { exact: true })).toBeVisible();
  expect(details.querySelector("time")).toHaveAttribute("datetime", message.created_at);
  expect(fetch).not.toHaveBeenCalled();
});
it("uses supplied roster updates without a separate member fetch", () => {
  const view = render(<Harness items={[agent]} members={[]} />); select(agent.id);
  expect(panel().getByText("未载入成员")).toBeVisible();
  view.rerender(<Harness items={[agent]} members={[member]} />);
  expect(panel().getByText(member.name, { exact: true })).toBeVisible(); expect(fetch).not.toHaveBeenCalled();
});
it.each([["user", ""], [member.id, " topic\r\n\u0000原始话题 "]] as const)("freezes exact %s source with raw topic %j and inert full text", async (sender, topic) => {
  const text = "  <script>window.bad = true</script>\r\n<img src=x onerror=bad()>\n" + "原文".repeat(130);
  const source: Message = { ...message, sender_id: sender, topic_id: topic, content: text };
  render(<Harness items={[source]} />); select();
  expect(panel().getByText("来源预览")).toBeVisible();
  if (topic === "") expect(panel().getByText("主话题", { exact: true })).toBeVisible();
  const evidence = expand("完整来源与标识");
  expect(evidence.querySelector(".message-content")?.textContent).toBe(text);
  expect(evidence.querySelector("script, img")).toBeNull();
  expect(evidence.textContent).toContain(topic || "主话题");
  source.group_id = other.id; source.topic_id = "mutated"; source.id = "mutated";
  source.sender_id = "mutated"; source.content = "mutated"; source.created_at = "mutated";
  fill(); submit();
  await panel().findByText(successText);
  expect(JSON.parse(String(attempt(0).body))).toEqual({ topic_id: topic, message_id: message.id, mode: "create_new", reason: input.reason,
    new_task: { title: input.title, description: input.description, acceptance: input.acceptance } });
  expect(attempt(0).url).toBe(`/api/v1/groups/${group.id}/coordination/requests`);
  expect(evidence.querySelector(".message-content")?.textContent).toBe(text);
  expect(evidence.textContent).not.toContain("mutated");
});
it.each(["任务标题", "验收条件", "授权理由"])("rejects Go-whitespace-only %s before creating a key or POST", name => {
  const uuid = vi.spyOn(crypto, "randomUUID"); render(<Harness />); select();
  for (const label of ["任务标题", "任务描述（可选）", "验收条件", "授权理由"]) expect(field(label)).toHaveValue("");
  submit(); expect(writes).not.toHaveBeenCalled(); fill(); change(name, "\t \n\u0085\u3000"); submit();
  expect(panel().getByRole("alert")).toHaveTextContent("任务标题、验收条件和授权理由");
  expect(uuid).not.toHaveBeenCalled(); expect(writes).not.toHaveBeenCalled();
});
it.each(["\ufeff", "  多行\n标题  "])("sends raw multiline fields and Go-nonblank title %j without extra fields", async title => {
  render(<Harness />); select();
  const raw = { title, description: "  <b>描述</b>\n\u0000 ", acceptance: " \t验收\n\u0085", reason: "  明确授权\n理由  " };
  change("任务标题", raw.title); change("任务描述（可选）", raw.description); change("验收条件", raw.acceptance); change("授权理由", raw.reason);
  expect(field("任务标题").tagName).toBe("TEXTAREA"); submit();
  await panel().findByText(successText); await panel().findByText("当前任务标题");
  expect(JSON.parse(String(attempt(0).body))).toEqual({ topic_id: "", message_id: message.id, mode: "create_new", reason: raw.reason,
    new_task: { title: raw.title, description: raw.description, acceptance: raw.acceptance } });
  expect(receipt().getByText(raw.reason, { normalizer: value => value })).toBeVisible();
  expect(fetch).toHaveBeenCalledTimes(2);
});
it("posts once under synchronous double submit and renders a real receipt before an independent current-task GET", async () => {
  const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise);
  render(<Harness />); select(); fill();
  expect(panel().getByText("将记录这次明确授权及验收条件；不会自动启动执行。")).toBeVisible();
  const form = panel().getByRole("form", { name: "从消息创建任务" });
  act(() => { fireEvent.submit(form); fireEvent.submit(form); });
  expect(writes).toHaveBeenCalledTimes(1); expect(reads).not.toHaveBeenCalled();
  expect(attempt(0).key).toMatch(/^[\da-f-]{36}$/);
  for (const name of ["任务标题", "任务描述（可选）", "验收条件", "授权理由"]) expect(field(name)).toBeDisabled();
  expect(panel().queryByRole("button", { name: "放弃草稿" })).not.toBeInTheDocument();
  expect(panel().queryByText(successText)).not.toBeInTheDocument();
  const sent = JSON.parse(String(attempt(0).body));
  await act(async () => pending.resolve(json(admission(group.id, sent), 201)));
  expect(receipt().getByText(successText)).toBeVisible();
  await current().findByText("当前任务标题"); expect(current().getByText("待交付")).toBeVisible();
  expect(writes.mock.calls[0][1]?.signal).toBeUndefined();
  expect(reads.mock.calls[0][0]).toBe(`/api/v1/groups/${group.id}/tasks/task-${message.id}`);
  expect(receipt().getByText(input.title)).toBeVisible(); expect(receipt().getByText(input.acceptance)).toBeVisible();
  const details = receipt().getByText("授权技术详情").closest("details")!;
  expect(details).not.toHaveAttribute("open");
  fireEvent.click(within(details).getByText("授权技术详情"));
  for (const id of [`decision-${message.id}`, `mandate-${message.id}`, `trigger-${message.id}`, `task-${message.id}`]) {
    expect(within(details).getByText(id, { exact: true })).toBeVisible();
  }
  expect(details.textContent).toContain("2026-11-01T08:00:00.123456789Z");
  expect(details.textContent).toContain("授权时修订");
  fireEvent.submit(form); expect(writes).toHaveBeenCalledTimes(1);
  expect(panel().queryByRole("button", { name: /执行|派工|接纳成果|提交成果/ })).not.toBeInTheDocument();
});
it.each(["network", 503, 408, 504, "malformed", "mismatched"])("locks unknown %s and retries only the exact original operation", async failure => {
  if (failure === "network") writes.mockRejectedValueOnce(new TypeError("English offline diagnostic"));
  else if (typeof failure === "number") writes.mockResolvedValueOnce(json({}, failure));
  else writes.mockImplementationOnce(async (url, init) => {
    const result = admission(decodeURIComponent(String(url).split("/")[4]), JSON.parse(String(init?.body)));
    return json(failure === "malformed" ? {} : { ...result, task: { ...result.task, group_id: other.id } }, 201);
  });
  const view = render(<Harness />); select(); fill(); submit();
  expect(await panel().findByRole("alert")).toHaveTextContent("结果未确认");
  expect(panel().getByRole("alert")).not.toHaveTextContent("English");
  const original = attempt(0); expect(reads).not.toHaveBeenCalled();
  select(agent.id); expect(panel().getByRole("status")).toHaveTextContent("原请求");
  expect(field("任务标题")).toBeDisabled(); change("任务标题", "不能替换");
  expect(field("任务标题")).toHaveValue(input.title);
  expect(panel().queryByRole("button", { name: "放弃草稿" })).not.toBeInTheDocument();
  close(); expect(scope().getByText(/重载会丢失/)).toBeVisible();
  view.rerender(<Harness activeGroup={other.id} items={[]} />);
  const outside = screen.getByRole("button", { name: "外部焦点" }); outside.focus();
  view.rerender(<Harness items={[]} />); expect(outside).toHaveFocus(); restore();
  expect(field("验收条件")).toHaveValue(input.acceptance);
  fireEvent.click(panel().getByRole("button", { name: "重试同一次创建" }));
  await panel().findByText(successText); expect(attempt(1)).toEqual(original);
  expect(writes).toHaveBeenCalledTimes(2); expect(reads).toHaveBeenCalledTimes(1);
});
it("protects a pending source across second-source clicks, close/reopen and source removal", async () => {
  const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise);
  const view = render(<Harness />); select(); fill(); submit(); const original = attempt(0);
  select(agent.id); expect(panel().getByRole("status")).toHaveTextContent("原请求");
  expect(expand("完整来源与标识").textContent).toContain(message.id);
  close(); restore(); expect(field("任务标题")).toBeDisabled();
  expect(panel().queryByRole("button", { name: "放弃草稿" })).not.toBeInTheDocument();
  close(); view.rerender(<Harness activeGroup={other.id} items={[]} />);
  const outside = screen.getByRole("button", { name: "外部焦点" }); outside.focus();
  await act(async () => pending.resolve(json(admission(group.id, JSON.parse(String(original.body))), 201)));
  expect(outside).toHaveFocus(); expect(reads).not.toHaveBeenCalled(); expect(writes).toHaveBeenCalledTimes(1);
  expect(screen.queryByRole("region", { name: "授权回执" })).not.toBeInTheDocument();
  view.rerender(<Harness items={[]} />); expect(outside).toHaveFocus();
  expect(screen.queryByRole("region", { name: "授权回执" })).not.toBeInTheDocument();
  restore(); await current().findByText("当前任务标题");
  expect(receipt().getByText(input.title)).toBeVisible(); expect(attempt(0)).toEqual(original);
});
it("does not expand or focus when a late 201 arrives in a collapsed active scope", async () => {
  const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise);
  render(<Harness />); select(); fill(); submit(); close();
  const outside = screen.getByRole("button", { name: "外部焦点" }); outside.focus();
  await act(async () => pending.resolve(json(admission(group.id, JSON.parse(String(attempt(0).body))), 201)));
  expect(outside).toHaveFocus(); expect(reads).not.toHaveBeenCalled();
  expect(scope().getByRole("button", { name: "查看授权回执" })).toBeVisible();
  expect(screen.queryByRole("form")).not.toBeInTheDocument();
  restore(); await current().findByText("当前任务标题"); expect(writes).toHaveBeenCalledTimes(1);
});
it("cannot overwrite a nonempty draft, preserves same-source edits and requires explicit safe discard", () => {
  render(<Harness />); select(); change("任务描述（可选）", " ");
  select(agent.id); expect(panel().getByRole("status")).toHaveTextContent("放弃草稿");
  expect(field("任务描述（可选）")).toHaveValue(" ");
  expect(expand("完整来源与标识").textContent).toContain(message.id);
  select(); expect(field("任务描述（可选）")).toHaveValue(" ");
  close(); restore(); expect(field("任务描述（可选）")).toHaveValue(" ");
  fireEvent.click(panel().getByRole("button", { name: "放弃草稿" })); select(agent.id);
  expect(panel().getByText(member.name, { exact: true })).toBeVisible();
  for (const name of ["任务标题", "任务描述（可选）", "验收条件", "授权理由"]) expect(field(name)).toHaveValue("");
  expect(fetch).not.toHaveBeenCalled();
});
it("allows replacing a pristine source but keeps a same-source confirmed receipt without resubmission", async () => {
  render(<Harness />); select(); select(agent.id);
  expect(panel().getByText(member.name, { exact: true })).toBeVisible(); fill(); submit();
  await panel().findByText("当前任务标题"); close(); select(agent.id);
  expect(receipt().getByText(successText)).toBeVisible(); expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
  expect(writes).toHaveBeenCalledTimes(1); select();
  expect(screen.queryByRole("region", { name: "授权回执" })).not.toBeInTheDocument();
  for (const name of ["任务标题", "任务描述（可选）", "验收条件", "授权理由"]) expect(field(name)).toHaveValue("");
});
it.each([400, 409])("retains an editable draft after definitive %s without fake success or new-key advice", async status => {
  writes.mockResolvedValueOnce(json({ error: { message: "English backend text" } }, status));
  render(<Harness />); select(); fill(); submit();
  expect(await panel().findByRole("alert")).toHaveTextContent(String(status));
  expect(panel().getByRole("alert")).not.toHaveTextContent(/English|换.*键|新.*键|幂等键冲突/);
  expect(field("任务标题")).toBeEnabled(); expect(field("授权理由")).toHaveValue(input.reason);
  expect(panel().queryByText(successText)).not.toBeInTheDocument(); expect(reads).not.toHaveBeenCalled();
  select(agent.id); expect(field("任务标题")).toHaveValue(input.title);
  expect(panel().getByRole("button", { name: "放弃草稿" })).toBeEnabled();
  change("验收条件", "修订验收"); submit(); await panel().findByText(successText);
  expect(JSON.parse(String(attempt(1).body)).new_task.acceptance).toBe("修订验收");
  expect(attempt(1).key).not.toBe(attempt(0).key);
});
it("retries failed current-task reads with GET only and never mutates the historical admission", async () => {
  reads.mockResolvedValueOnce(json({}, 503)); render(<Harness />); select(); fill(); submit();
  await panel().findByText(successText);
  expect(await current().findByRole("alert")).toHaveTextContent(/授权已登记.*当前任务.*不可用/);
  expect(writes).toHaveBeenCalledTimes(1);
  reads.mockResolvedValueOnce(json(currentTask(group.id, `task-${message.id}`, { title: "交付后的新标题", status: "submitted",
    revision: 3, active_attempt_id: "attempt-1", current_submission_id: "submission-1" })));
  fireEvent.click(current().getByRole("button", { name: "重试载入当前任务" }));
  expect(await current().findByText("待验收")).toBeVisible(); expect(current().getByText("3")).toBeVisible();
  const historical = receipt().getByText("授权技术详情").closest("details")!;
  fireEvent.click(within(historical).getByText("授权技术详情"));
  const original = historical.textContent;
  reads.mockResolvedValueOnce(json(currentTask(group.id, `task-${message.id}`, { title: "接纳后的新标题", status: "accepted",
    revision: 4, current_submission_id: "submission-1" })));
  fireEvent.click(current().getByRole("button", { name: "刷新当前任务" }));
  expect(await current().findByText("已接纳")).toBeVisible(); expect(current().getByText("4")).toBeVisible();
  expect(historical.textContent).toBe(original); expect(within(historical).getByText("1", { exact: true })).toBeVisible();
  expect(receipt().getByText(input.title)).toBeVisible(); expect(receipt().getByText(input.acceptance)).toBeVisible();
  expect(receipt().getByText(input.reason)).toBeVisible(); expect(receipt().queryByText(/新标题|当前验收/)).not.toBeInTheDocument();
  expect(writes).toHaveBeenCalledTimes(1); expect(reads).toHaveBeenCalledTimes(3);
});
it.each(["close", "deactivate"])("aborts and resumes an interrupted GET on %s without stale overwrite or focus theft", async action => {
  const old = deferred<Response>(); reads.mockReturnValueOnce(old.promise);
  const view = render(<Harness />); select(); fill(); submit();
  await panel().findByText("正在载入当前任务…");
  if (action === "close") close(); else view.rerender(<Harness activeGroup={other.id} />);
  expect(reads.mock.calls[0][1]?.signal?.aborted).toBe(true);
  const outside = screen.getByRole("button", { name: "外部焦点" }); outside.focus();
  const fresh = deferred<Response>(); reads.mockReturnValueOnce(fresh.promise);
  if (action === "close") restore(); else view.rerender(<Harness />);
  if (action === "deactivate") expect(outside).toHaveFocus();
  expect(current().getByText("正在载入当前任务…")).toBeVisible();
  await act(async () => fresh.resolve(json(currentTask(group.id, `task-${message.id}`, { title: "较新投影", revision: 5 }))));
  await act(async () => old.resolve(json({}, 503)));
  expect(current().getByText("较新投影")).toBeVisible(); expect(current().getByText("5")).toBeVisible();
  expect(current().queryByRole("alert")).not.toBeInTheDocument(); expect(writes).toHaveBeenCalledTimes(1);
});
it("ignores an obsolete successful GET after an explicit refresh and after changing the source", async () => {
  const first = deferred<Response>(); reads.mockReturnValueOnce(first.promise);
  render(<Harness />); select(); fill(); submit(); await panel().findByText("正在载入当前任务…");
  const second = deferred<Response>(); reads.mockReturnValueOnce(second.promise);
  fireEvent.click(current().getByRole("button", { name: "刷新当前任务" }));
  expect(reads.mock.calls[0][1]?.signal?.aborted).toBe(true);
  await act(async () => second.resolve(json(currentTask(group.id, `task-${message.id}`, { title: "最新投影", revision: 2 }))));
  await act(async () => first.resolve(json(currentTask())));
  expect(current().getByText("最新投影")).toBeVisible();
  const obsolete = deferred<Response>(); reads.mockReturnValueOnce(obsolete.promise);
  fireEvent.click(current().getByRole("button", { name: "刷新当前任务" })); select(agent.id);
  expect(reads.mock.calls[2][1]?.signal?.aborted).toBe(true); fill(); submit();
  await panel().findByText("当前任务标题");
  await act(async () => obsolete.resolve(json(currentTask(group.id, `task-${message.id}`, { title: "旧来源投影" }))));
  expect(current().queryByText("旧来源投影")).not.toBeInTheDocument();
  expect(receipt().getByText(successText)).toBeVisible();
  expect(expand("完整来源与标识").textContent).toContain(agent.id);
});
it("isolates two simultaneously mounted active groups' keys, frozen sources, receipts and current tasks", async () => {
  const pendingA = deferred<Response>(), pendingB = deferred<Response>();
  writes.mockReturnValueOnce(pendingA.promise).mockReturnValueOnce(pendingB.promise);
  render(<Harness activeGroup="both" />); select(); fill(); submit();
  select(foreign.id); fill(other.id); change("授权理由", "B 群明确授权", other.id); submit(other.id);
  expect(attempt(0).key).not.toBe(attempt(1).key);
  expect(attempt(0).url).toContain(group.id); expect(attempt(1).url).toContain(other.id);
  expect(field("授权理由")).toHaveValue(input.reason); expect(field("授权理由", other.id)).toHaveValue("B 群明确授权");
  await act(async () => pendingB.resolve(json(admission(other.id, JSON.parse(String(attempt(1).body))), 201)));
  expect(receipt(other.id).getByText("B 群明确授权")).toBeVisible();
  expect(panel().queryByText(successText)).not.toBeInTheDocument(); expect(field("任务标题")).toBeDisabled();
  await act(async () => pendingA.resolve(json(admission(group.id, JSON.parse(String(attempt(0).body))), 201)));
  expect(receipt().getByText(input.reason)).toBeVisible(); expect(receipt().queryByText("B 群明确授权")).not.toBeInTheDocument();
  await current().findByText("当前任务标题"); await current(other.id).findByText("当前任务标题");
  expect(reads.mock.calls.map(([url]) => url)).toEqual([
    `/api/v1/groups/${other.id}/tasks/task-${foreign.id}`, `/api/v1/groups/${group.id}/tasks/task-${message.id}`,
  ]);
});
it("focuses editable drafts on explicit open, returns to the trigger and focuses locked reopening as a region", async () => {
  writes.mockRejectedValueOnce(new TypeError("offline")); render(<Harness />); select();
  expect(field("任务标题")).toHaveFocus(); fill(); close();
  expect(screen.getByRole("button", { name: `选择 ${message.id}` })).toHaveFocus();
  restore(); expect(field("任务标题")).toHaveFocus(); submit(); await panel().findByRole("alert");
  close(); restore(); expect(scope().getByRole("region", { name: "从消息创建任务" })).toHaveFocus();
  expect(field("任务标题")).toBeDisabled();
});
it.each(["removed", "hidden", "css-hidden", "transparent"])("returns focus to its visible restore control when the trigger is %s", mode => {
  const view = render(<Harness />); select(); change("任务标题", "保留草稿");
  const trigger = screen.getByRole("button", { name: `选择 ${message.id}` });
  if (mode === "removed") view.rerender(<Harness items={[agent, foreign]} />);
  else if (mode === "hidden") trigger.hidden = true;
  else if (mode === "transparent") trigger.style.opacity = "0";
  else trigger.style.display = "none";
  close(); expect(scope().getByRole("button", { name: "继续任务草稿" })).toHaveFocus();
  expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
  expect(scope().getByText(/重载会丢失/)).toBeVisible(); restore(); expect(field("任务标题")).toHaveValue("保留草稿");
});
it("uses the restore control when the connected trigger is inside closed native details", () => {
  const handle = createRef<RequestWorkHandle>();
  const frame = (expanded: boolean) => <><details open={expanded}><summary>来源操作</summary>
    <button type="button" onClick={event => handle.current?.open(message, event.currentTarget)}>折叠来源入口</button></details>
    <RequestWork groupId={group.id} active ref={handle} /></>;
  const view = render(frame(true)); fireEvent.click(screen.getByRole("button", { name: "折叠来源入口" }));
  view.rerender(frame(false)); fireEvent.click(screen.getByRole("button", { name: "收起" }));
  expect(screen.getByRole("button", { name: "继续任务草稿" })).toHaveFocus();
});
it("does not queue explicit-open focus across group deactivation or let asynchronous reads steal focus", async () => {
  const view = render(<Harness />); select(); fill();
  view.rerender(<Harness activeGroup={other.id} />);
  const outside = screen.getByRole("button", { name: "外部焦点" }); outside.focus();
  view.rerender(<Harness />); expect(outside).toHaveFocus(); submit();
  await panel().findByText("当前任务标题"); expect(outside).toHaveFocus();
});
it("survives StrictMode cleanup/setup while retaining an unknown operation through a hidden group", async () => {
  writes.mockRejectedValueOnce(new TypeError("offline"));
  const view = render(<StrictMode><Harness /></StrictMode>); select(); fill(); submit();
  await panel().findByRole("alert"); const original = attempt(0); close();
  view.rerender(<StrictMode><Harness activeGroup={other.id} /></StrictMode>);
  view.rerender(<StrictMode><Harness /></StrictMode>); restore();
  fireEvent.click(panel().getByRole("button", { name: "重试同一次创建" }));
  await panel().findByText("当前任务标题"); expect(attempt(1)).toEqual(original);
  expect(receipt().getByText(successText)).toBeVisible(); expect(writes).toHaveBeenCalledTimes(2);
});
it("keeps the same DOM mounted across collapse and group deactivation", () => {
  const view = render(<Harness />); select(); fill(); const originalTitle = field("任务标题");
  const originalRegion = scope().getByRole("region", { name: "从消息创建任务" });
  close(); expect(originalTitle).toBeInTheDocument(); expect(originalTitle).not.toBeVisible();
  view.rerender(<Harness activeGroup={other.id} />);
  expect(originalRegion).toBeInTheDocument(); expect(originalRegion).not.toBeVisible();
  view.rerender(<Harness />); restore(); expect(field("任务标题")).toBe(originalTitle);
  expect(scope().getByRole("region", { name: "从消息创建任务" })).toBe(originalRegion);
});
it("hides its own inactive pane and ignores inactive open calls without a parent hiding wrapper", () => {
  const handle = createRef<RequestWorkHandle>();
  const frame = (active: boolean) => <><button type="button" onClick={() => handle.current?.open(message)}>无返回入口</button>
    <RequestWork groupId={group.id} active={active} ref={handle} /></>;
  const view = render(frame(false)); fireEvent.click(screen.getByRole("button", { name: "无返回入口" }));
  expect(screen.queryByRole("form")).not.toBeInTheDocument();
  view.rerender(frame(true)); expect(screen.queryByRole("form")).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "无返回入口" }));
  fireEvent.change(screen.getByRole("textbox", { name: "任务标题" }), { target: { value: "本页草稿" } });
  view.rerender(frame(false)); expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "无返回入口" }));
  view.rerender(frame(true)); expect(screen.getByRole("textbox", { name: "任务标题" })).toHaveValue("本页草稿");
  fireEvent.click(screen.getByRole("button", { name: "收起" }));
  expect(screen.getByRole("button", { name: "继续任务草稿" })).toHaveFocus(); expect(fetch).not.toHaveBeenCalled();
});
it("reopens a confirmed receipt with region focus rather than a resubmittable form", async () => {
  render(<Harness />); select(); fill(); submit(); await panel().findByText("当前任务标题");
  close(); restore(); expect(scope().getByRole("region", { name: "从消息创建任务" })).toHaveFocus();
  expect(screen.queryByRole("form")).not.toBeInTheDocument(); expect(writes).toHaveBeenCalledTimes(1);
});
it("resumes a GET rejected by intentional abort without showing an abort error or staying pending", async () => {
  reads.mockImplementationOnce((_url, init) => new Promise((_resolve, reject) => {
    init?.signal?.addEventListener("abort", () => reject(new DOMException("Aborted", "AbortError")), { once: true });
  }));
  render(<Harness />); select(); fill(); submit(); await panel().findByText("正在载入当前任务…"); close();
  await act(async () => {}); restore(); await current().findByText("当前任务标题");
  expect(current().queryByRole("alert")).not.toBeInTheDocument();
  expect(current().queryByText("正在载入当前任务…")).not.toBeInTheDocument(); expect(writes).toHaveBeenCalledTimes(1);
});
it("aborts only reads on full unmount and ignores late POST/GET results", async () => {
  const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise);
  const view = render(<Harness />); select(); fill(); submit(); view.unmount();
  expect(writes.mock.calls[0][1]?.signal).toBeUndefined();
  await act(async () => pending.resolve(json(admission(group.id, JSON.parse(String(attempt(0).body))), 201)));
  expect(reads).not.toHaveBeenCalled();
  const pendingRead = deferred<Response>(); reads.mockReturnValueOnce(pendingRead.promise);
  const next = render(<Harness />); select(); fill(); submit(); await panel().findByText("正在载入当前任务…"); next.unmount();
  expect(reads.mock.calls[0][1]?.signal?.aborted).toBe(true);
  await act(async () => pendingRead.resolve(json(currentTask())));
  expect(screen.queryByRole("region", { name: "授权回执" })).not.toBeInTheDocument();
});
