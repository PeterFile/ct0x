import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Tasks } from "./Tasks";
import type { Group } from "./api";
import type { DispatchRequest, DispatchState, DispatchView, TaskCoordinationView } from "./dispatch-request-api";
import type { Task } from "./task-api";
import { deferred, group, json, other, page } from "./test/groups";
import { secondTask, submittedTask, task } from "./test/tasks";

// Real Tasks/TaskLifecycle/TaskDispatch/settings/clients; only HTTP transport is replaced.
const reason = "  按明确授权协调。\n保留理由原文。  ";
const currentTask = { ...task, revision: 7 };
const bTask = { ...task, group_id: other.id, title: "B 群任务" };
const scopeKey = (groupId: string, taskId: string) => JSON.stringify([groupId, taskId]);
function authorityFor(value = currentTask, generation = 3, revision = value.revision): TaskCoordinationView {
  const suffix = `${value.group_id}-${value.id}-${generation}`, mandateId = `mandate-${suffix}`, decisionId = `decision-${suffix}`, triggerId = `trigger-${suffix}`;
  return {
    coordination: { group_id: value.group_id, task_id: value.id, topic_id: "", current_mandate_id: mandateId, authority_generation: generation },
    admission: { task: { id: value.id, group_id: value.group_id, revision_at_admission: revision },
      decision: { id: decisionId, trigger_id: triggerId, group_id: value.group_id, topic_id: "", kind: "link_existing", actor_id: "user",
        policy_revision: "local-user-explicit/v1", reason: "用户明确授权", task_id: value.id, task_revision_at_admission: revision, mandate_id: mandateId, created_at: value.created_at },
      mandate: { id: mandateId, decision_id: decisionId, origin_trigger_id: triggerId, group_id: value.group_id, topic_id: "", task_id: value.id,
        task_revision_at_admission: revision, authority_kind: "local_user_explicit", authorized_by: "user", strength: "explicit",
        policy_revision: "local-user-explicit/v1", created_at: value.created_at,
        scope: { objective: value.title, description: value.description, acceptance: value.acceptance, assignee_id: value.assignee_id, dependency_ids: value.dependency_ids } } },
  };
}
function receiptFor(value = currentTask, authority = authorityFor(value), text = reason, id = `request-${value.group_id}-${value.id}`): DispatchRequest {
  return { id, group_id: value.group_id, task_id: value.id, topic_id: authority.coordination.topic_id, task_revision: value.revision,
    authority_generation: authority.coordination.authority_generation, mandate_id: authority.coordination.current_mandate_id,
    coordinator_member_id: "lead-1", binding_revision: 8, policy_revision: 9, requested_by: "user", reason: text, created_at: value.created_at };
}
function viewFor(receipt = receiptFor(), status: DispatchState["status"] = "pending", generation = status === "pending" ? 1 : 4): DispatchView {
  const decision = status === "assigned" || status === "declined";
  const count = generation - (status === "pending" ? 1 : decision ? 3 : 2);
  return { request: receipt, state: { request_id: receipt.id, generation, status, attempt_count: count, next_attempt_at: receipt.created_at,
    last_error_code: status === "pending" && count > 0 ? "sqlite_busy" : "", held_reason: status === "held" ? "no_eligible_worker" : "",
    delivery: ["enqueued", "assigned", "declined"].includes(status)
      ? { message_id: "message-1", delivery_id: "delivery-1", conversation_id: "conversation-1", coordinator_member_id: receipt.coordinator_member_id,
        materialized_from_generation: generation - (decision ? 2 : 1), created_at: receipt.created_at } : null } };
}
const tasks = new Map<string, Task>(), authorities = new Map<string, TaskCoordinationView>(), views = new Map<string, DispatchView>();
const details = vi.fn<typeof fetch>(), authorityReads = vi.fn<typeof fetch>(), stateReads = vi.fn<typeof fetch>();
const dispatchWrites = vi.fn<typeof fetch>(), taskWrites = vi.fn<typeof fetch>(), configWrites = vi.fn<typeof fetch>();
const requests = vi.fn<typeof fetch>();
let unexpected: string[];
const urlOf = (value: RequestInfo | URL) => new URL(String(value), "http://fixture.invalid");
const idsOf = (value: RequestInfo | URL) => urlOf(value).pathname.split("/").map(decodeURIComponent);
const panel = () => within(screen.getByRole("region", { name: "群组任务" }));
const detail = () => within(panel().getByRole("region", { name: "任务详情" }));
const dispatch = () => within(detail().getByRole("region", { name: "请求分派" }));
const history = () => within(dispatch().getByRole("region", { name: "分派登记历史回执" }));
const state = () => within(dispatch().getByRole("region", { name: "请求当前状态" }));
const change = (field: HTMLElement, value: string) => fireEvent.change(field, { target: { value } });
const submit = () => fireEvent.submit(dispatch().getByRole("form", { name: "发起分派" }));
const refresh = () => fireEvent.click(dispatch().getByRole("button", { name: "刷新任务与授权" }));
const refreshState = () => fireEvent.click(dispatch().getByRole("button", { name: "刷新请求状态" }));
const retry = () => fireEvent.click(dispatch().getByRole("button", { name: "重试原分派登记" }));
const captured = (index: number) => {
  const [url, init] = dispatchWrites.mock.calls[index];
  return { url, body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key") };
};
const ui = (value: Group | null) => <><button type="button">聊天输入</button><Tasks group={value} /></>;
const flush = async () => { await act(async () => {}); };
async function select(value = currentTask) {
  fireEvent.click(await panel().findByRole("button", { name: new RegExp(`^${value.title}，`) }));
  await detail().findByRole("heading", { name: value.title });
}
async function expand() {
  fireEvent.click(dispatch().getByRole("button", { name: "展开请求分派" }));
  await flush();
}
async function open(value = currentTask) {
  const result = render(ui(value.group_id === other.id ? other : group));
  await select(value); await expand(); return result;
}
function editReason(text = reason) { change(dispatch().getByRole("textbox", { name: "分派理由" }), text); }
const refusal = (status: number) => json({ error: { code: status === 404 ? "not_found" : "conflict", message: "拒绝登记" } }, status);
const storeTask = (value: Task, generation = 3, revision = value.revision) => {
  tasks.set(scopeKey(value.group_id, value.id), value); authorities.set(scopeKey(value.group_id, value.id), authorityFor(value, generation, revision));
};
beforeEach(() => {
  unexpected = []; tasks.clear(); authorities.clear(); views.clear();
  for (const value of [currentTask, secondTask, bTask]) storeTask(value);
  views.set(receiptFor().id, viewFor());
  details.mockReset().mockImplementation(async input => { const ids = idsOf(input); return json(tasks.get(scopeKey(ids[4], ids[6]))); });
  authorityReads.mockReset().mockImplementation(async input => { const ids = idsOf(input); return json(authorities.get(scopeKey(ids[4], ids[6]))); });
  stateReads.mockReset().mockImplementation(async input => json(views.get(idsOf(input)[7])));
  dispatchWrites.mockReset().mockResolvedValue(json(receiptFor(), 201));
  taskWrites.mockReset().mockRejectedValue(new TypeError("Lost lifecycle receipt"));
  configWrites.mockReset().mockRejectedValue(new TypeError("Lost settings receipt"));
  requests.mockReset().mockImplementation((input, init) => {
    const ids = idsOf(input), url = urlOf(input), method = init?.method ?? "GET", groupId = ids[4];
    if (method === "POST" && /\/tasks\/[^/]+\/dispatch-requests$/.test(url.pathname)) return dispatchWrites(input, init);
    if (method === "POST" && /\/tasks\/[^/]+\/(attempts|submissions|reviews)$/.test(url.pathname)) return taskWrites(input, init);
    if (method === "PUT" && /\/coordination\/(coordinator|dispatch-policy)$/.test(url.pathname)) return configWrites(input, init);
    if (method === "GET") {
      if (url.pathname.endsWith("/tasks")) return Promise.resolve(page([...tasks.values()].filter(value => value.group_id === groupId)));
      if (/\/tasks\/[^/]+\/coordination$/.test(url.pathname)) return authorityReads(input, init);
      if (/\/coordination\/dispatch-requests\/[^/]+$/.test(url.pathname)) return stateReads(input, init);
      if (/\/tasks\/[^/]+$/.test(url.pathname)) return details(input, init);
      if (/\/tasks\/[^/]+\/attempts\/[^/]+$/.test(url.pathname)) return Promise.resolve(json({ id: "attempt-1", group_id: groupId, task_id: ids[6],
        task_revision: 7, turn_id: "", created_at: task.created_at }));
      if (/\/tasks\/[^/]+\/submissions\/[^/]+$/.test(url.pathname)) return Promise.resolve(json({ id: "submission-1", group_id: groupId, task_id: ids[6],
        task_revision: 7, attempt_id: "attempt-1", summary: "已有交付", submitted_by: "user", artifact_versions: [], created_at: task.created_at }));
      if (url.pathname.endsWith("/members")) return Promise.resolve(page(["lead", "worker"].map(id => ({ id: `${groupId}-${id}`, group_id: groupId,
        name: id, role: "普通成员", status: "active", provider_id: "", model: "", creator_id: "user", owner_id: "user", workspace: "", created_at: task.created_at }))));
      if (url.pathname.endsWith("/coordination/coordinator")) return Promise.resolve(json({ group_id: groupId, member_id: `${groupId}-lead`, revision: 1 }));
      if (url.pathname.endsWith("/coordination/dispatch-policy")) return Promise.resolve(json({ group_id: groupId, allowed_worker_ids: [`${groupId}-worker`], revision: 1 }));
    }
    unexpected.push(`${method} ${input}`); return Promise.reject(new Error(`Unexpected HTTP: ${method} ${input}`));
  });
  vi.stubGlobal("fetch", requests);
});
afterEach(() => { vi.unstubAllGlobals(); expect(unexpected).toEqual([]); });

async function lookupKnown(id = receiptFor().id) {
  change(dispatch().getByRole("textbox", { name: "已知分派请求标识" }), id);
  fireEvent.submit(dispatch().getByRole("form", { name: "读取已知请求" })); await flush();
}
it.each(["7.0000000000000001", "9007199254740991.4"])("[repair 1] rejects the raw Task revision %s before any POST", async token => {
  const value = { ...currentTask, revision: Number(token) }; storeTask(value);
  const raw = JSON.stringify(value).replace(`"revision":${value.revision}`, `"revision":${token}`);
  details.mockImplementation(async () => new Response(raw));
  await open(value); editReason();
  expect.soft(dispatch().getByRole("button", { name: "确认发起分派" })).toBeDisabled();
  submit(); await flush();
  expect(dispatchWrites).not.toHaveBeenCalled();
});
it("[repair 1] rejects raw invalid UTF-8 that would otherwise match the authority replacement character", async () => {
  const value = { ...currentTask, title: "标题�" }; storeTask(value);
  const encoded = new TextEncoder().encode(JSON.stringify(value));
  const at = encoded.findIndex((byte, index) => byte === 239 && encoded[index + 1] === 191 && encoded[index + 2] === 189);
  const bytes = new Uint8Array([...encoded.slice(0, at), 255, ...encoded.slice(at + 3)]);
  details.mockImplementation(async () => new Response(bytes));
  await open(value); editReason();
  expect.soft(dispatch().getByRole("button", { name: "确认发起分派" })).toBeDisabled();
  submit(); await flush();
  expect(dispatchWrites).not.toHaveBeenCalled();
});
it.each([7, Number.MAX_SAFE_INTEGER])("[repair 1] permits an exact safe Task revision %s and valid replacement-character text", async revision => {
  const value = { ...currentTask, revision, title: "合法标题�" }; storeTask(value);
  const receipt = receiptFor(value); views.set(receipt.id, viewFor(receipt)); dispatchWrites.mockResolvedValueOnce(json(receipt, 201));
  await open(value); editReason();
  expect(dispatch().getByRole("button", { name: "确认发起分派" })).toBeEnabled();
  submit(); await flush();
  expect(history().getByText(receipt.id)).toBeVisible();
  expect(JSON.parse(String(captured(0).body))).toEqual({ expected_task_revision: revision, expected_authority_generation: 3, reason });
});
it.each([
  ["pending", 1, "已登记，等待协调"], ["held", 2, "协调请求暂停"], ["enqueued", 2, "已进入协调员投递队列"],
  ["assigned", 3, "已记录分配，具体执行尚未由本页核实"], ["declined", 3, "协调员已拒绝本次分派，不能自动重派"],
] as const)("[initial time] rejects zero-retry %s before observations and recovers at the same generation", async (status, generation, label) => {
  const valid = viewFor(receiptFor(), status, generation);
  stateReads.mockResolvedValueOnce(json({ ...valid, state: { ...valid.state, next_attempt_at: "2026-10-31T12:00:00Z" } }));
  await open(); editReason(); submit(); await flush();
  expect.soft(state().queryByText(label)).not.toBeInTheDocument();
  expect.soft(state().queryByRole("alert")).toHaveTextContent("当前状态未确认");
  expect(history().getByText(valid.request.id)).toBeVisible();
  views.set(valid.request.id, valid); refreshState(); await flush();
  expect(state().getByText(label)).toBeVisible(); expect(state().queryByRole("alert")).not.toBeInTheDocument();
  expect(dispatchWrites).toHaveBeenCalledTimes(1); expect(taskWrites).not.toHaveBeenCalled();
});
it("[initial time] rejects malformed initial next-at without poisoning the legal zero-retry enqueued successor", async () => {
  const initial = viewFor();
  stateReads.mockResolvedValueOnce(json({ ...initial, state: { ...initial.state, next_attempt_at: "2026-10-31T12:00:00Z" } }));
  await open(); editReason(); submit(); await flush();
  expect.soft(state().queryByText("已登记，等待协调")).not.toBeInTheDocument();
  expect.soft(state().queryByRole("alert")).toHaveTextContent("当前状态未确认");
  views.set(initial.request.id, viewFor(initial.request, "enqueued", 2)); refreshState(); await flush();
  expect(state().getByText("已进入协调员投递队列")).toBeVisible(); expect(state().queryByRole("alert")).not.toBeInTheDocument();
  expect(history().getByText(initial.request.id)).toBeVisible(); expect(dispatchWrites).toHaveBeenCalledTimes(1);
});
it("[initial time] permits retry wall-clock reversal before creation and preserves its materialization and decision", async () => {
  await open(); editReason(); submit(); await flush();
  const timeline = [
    { view: viewFor(receiptFor(), "pending", 2), next: "2026-10-31T12:00:00Z", label: "已登记，等待协调" },
    { view: viewFor(receiptFor(), "pending", 3), next: "2026-10-29T12:00:00Z", label: "已登记，等待协调" },
    { view: viewFor(receiptFor(), "enqueued", 4), next: "2026-10-29T12:00:00Z", label: "已进入协调员投递队列" },
    { view: viewFor(receiptFor(), "assigned", 5), next: "2026-10-29T12:00:00Z", label: "已记录分配，具体执行尚未由本页核实" },
  ];
  for (const { view, next, label } of timeline) {
    views.set(view.request.id, { ...view, state: { ...view.state, next_attempt_at: next } }); refreshState(); await flush();
    expect(state().getByText(label)).toBeVisible(); expect(state().queryByRole("alert")).not.toBeInTheDocument();
    expect(state().getByText(next)).toBeVisible();
  }
  expect(history().getByText(receiptFor().id)).toBeVisible(); expect(dispatchWrites).toHaveBeenCalledTimes(1);
  expect(taskWrites).not.toHaveBeenCalled(); expect(detail().getByText("待交付")).toBeVisible();
});

it("[repair 3] rejects malformed pending before it can poison the generation high-water mark", async () => {
  await open(); editReason(); submit(); await flush();
  const malformed = viewFor(receiptFor(), "pending", 9);
  stateReads.mockResolvedValueOnce(json({ ...malformed, state: { ...malformed.state, attempt_count: 0 } }));
  refreshState(); await flush();
  expect.soft(state().queryByRole("alert")).toHaveTextContent("当前状态未确认");
  views.set(receiptFor().id, viewFor(receiptFor(), "enqueued", 2)); refreshState(); await flush();
  expect(state().getByText("已进入协调员投递队列")).toBeVisible();
  expect(state().queryByRole("alert")).not.toBeInTheDocument();
});
it.each([
  ["enqueued", 2, "pending", 3, "已进入协调员投递队列"],
  ["held", 2, "pending", 3, "协调请求暂停"],
  ["assigned", 3, "declined", 4, "已记录分配，具体执行尚未由本页核实"],
  ["declined", 3, "enqueued", 4, "协调员已拒绝本次分派，不能自动重派"],
] as const)("[repair 3] rejects an impossible observed transition %s gen%s -> %s gen%s", async (before, generation, after, nextGeneration, label) => {
  const original = viewFor(receiptFor(), before, generation); views.set(receiptFor().id, original);
  await open(); editReason(); submit(); await flush();
  expect(state().getByText(label)).toBeVisible();
  stateReads.mockResolvedValueOnce(json(viewFor(receiptFor(), after, nextGeneration))); refreshState(); await flush();
  expect.soft(state().queryByRole("alert")).toHaveTextContent("当前状态未确认");
  stateReads.mockResolvedValueOnce(json(original)); refreshState(); await flush();
  expect(state().getByText(label)).toBeVisible();
  expect(history().getByText(receiptFor().id)).toBeVisible();
});
it.each([
  ["held", 5, "协调请求暂停"], ["enqueued", 5, "已进入协调员投递队列"],
  ["assigned", 6, "已记录分配，具体执行尚未由本页核实"], ["declined", 6, "协调员已拒绝本次分派，不能自动重派"],
] as const)("[repair 3] allows skipped retry generations followed by %s", async (status, generation, label) => {
  await open(); editReason(); submit(); await flush();
  views.set(receiptFor().id, viewFor(receiptFor(), "pending", 4)); refreshState(); await flush();
  expect(state().getByText("协调检查错误：sqlite_busy")).toBeVisible();
  views.set(receiptFor().id, viewFor(receiptFor(), status, generation)); refreshState(); await flush();
  expect(state().getByText(label)).toBeVisible(); expect(state().queryByRole("alert")).not.toBeInTheDocument();
  expect(dispatchWrites).toHaveBeenCalledTimes(1);
});
it.each(["assigned", "declined"] as const)("[repair 3] accepts the actual enqueued -> %s decision projection", async status => {
  views.set(receiptFor().id, viewFor(receiptFor(), "enqueued", 2));
  await open(); editReason(); submit(); await flush();
  views.set(receiptFor().id, viewFor(receiptFor(), status, 3)); refreshState(); await flush();
  expect(state().queryByRole("alert")).not.toBeInTheDocument();
  expect(state().getByText(status === "assigned" ? "已记录分配，具体执行尚未由本页核实" : "协调员已拒绝本次分派，不能自动重派")).toBeVisible();
});
it("[repair 4] keeps the original unknown operation when its 201 contradicts observed immutable history for that ID", async () => {
  dispatchWrites.mockRejectedValueOnce(new TypeError("Lost receipt"));
  await open(); editReason(); submit(); await flush(); const original = captured(0);
  const observed = { ...receiptFor(), reason: "另一条不可变理由" }; views.set(observed.id, viewFor(observed));
  await lookupKnown(observed.id);
  expect(state().getByText("已登记，等待协调")).toBeVisible();
  retry(); await flush();
  expect.soft(dispatch().queryByRole("button", { name: "重试原分派登记" })).toBeEnabled();
  expect.soft(dispatch().queryByRole("region", { name: "分派登记历史回执" })).not.toBeInTheDocument();
  expect.soft(dispatch().queryByRole("alert")).toHaveTextContent(/不可变历史冲突/);
  expect(captured(1)).toEqual(original);
});
it("[repair 4] confirms a matching historical 201 without clearing the newer trusted GET projection", async () => {
  dispatchWrites.mockRejectedValueOnce(new TypeError("Lost receipt"));
  await open(); editReason(); submit(); await flush(); const original = captured(0);
  views.set(receiptFor().id, viewFor(receiptFor(), "assigned", 3)); await lookupKnown();
  const waiting = deferred<Response>(); dispatchWrites.mockReturnValueOnce(waiting.promise); retry();
  storeTask({ ...currentTask, revision: 8 }, 5); refresh(); await flush();
  stateReads.mockResolvedValueOnce(refusal(404));
  await act(async () => waiting.resolve(json(receiptFor(), 201)));
  expect(history().getByText("任务版本 7 / 授权代次 3")).toBeVisible();
  expect(dispatch().getByText("任务版本 8 / 授权代次 5")).toBeVisible();
  expect.soft(stateReads).toHaveBeenCalledTimes(1);
  expect(state().getByText("已记录分配，具体执行尚未由本页核实")).toBeVisible();
  expect(dispatch().queryByRole("button", { name: "重试原分派登记" })).not.toBeInTheDocument();
  expect(captured(1)).toEqual(original);
});

it("reads lazily and requires an explicit reason and confirmation, freezing real versions and preventing double clicks", async () => {
  render(ui(group)); await select();
  expect(authorityReads).not.toHaveBeenCalled(); expect(dispatchWrites).not.toHaveBeenCalled();
  fireEvent.click(detail().getByRole("button", { name: "刷新详情" })); await flush();
  expect(authorityReads).not.toHaveBeenCalled();
  await expand();
  expect(authorityReads).toHaveBeenCalledTimes(1);
  expect(dispatch().getByText("任务版本 7 / 授权代次 3")).toBeVisible();
  expect(dispatch().getByText(task.acceptance)).toBeVisible();
  submit(); expect(dispatch().getByRole("alert")).toHaveTextContent("请输入分派理由");
  editReason("\u0085"); submit(); expect(dispatchWrites).not.toHaveBeenCalled();
  editReason();
  const waiting = deferred<Response>(); dispatchWrites.mockReturnValueOnce(waiting.promise);
  submit(); submit();
  expect(dispatchWrites).toHaveBeenCalledTimes(1);
  expect(captured(0).url).toBe(`/api/v1/groups/${group.id}/tasks/${task.id}/dispatch-requests`);
  expect(JSON.parse(String(captured(0).body))).toEqual({ expected_task_revision: 7, expected_authority_generation: 3, reason });
  expect(captured(0).key).toBeTruthy(); expect(dispatchWrites.mock.calls[0][1]?.redirect).toBe("error");
  expect(dispatch().getByRole("textbox", { name: "分派理由" })).toBeDisabled();
  await act(async () => waiting.resolve(json(receiptFor(), 201)));
  expect(history().getByText(receiptFor().id)).toBeVisible();
  expect(await state().findByText("已登记，等待协调")).toBeVisible();
  expect(taskWrites).not.toHaveBeenCalled(); expect(configWrites).not.toHaveBeenCalled();
  expect(detail().getByText("待交付")).toBeVisible();
});
it.each([
  ["pending", "已登记，等待协调"], ["enqueued", "已进入协调员投递队列"], ["held", "协调请求暂停"],
  ["assigned", "已记录分配，具体执行尚未由本页核实"], ["declined", "协调员已拒绝本次分派，不能自动重派"],
] as const)("separates immutable registration from %s without inventing lifecycle progress", async (status, label) => {
  views.set(receiptFor().id, viewFor(receiptFor(), status));
  await open(); editReason(); submit();
  await dispatch().findByRole("region", { name: "分派登记历史回执" });
  expect(history().getByText(receiptFor().id)).toBeVisible();
  expect(await state().findByText(label)).toBeVisible();
  if (status === "held") expect(state().getByText(/no_eligible_worker/)).toBeVisible();
  if (["enqueued", "assigned", "declined"].includes(status)) expect(state().getByText("delivery-1")).toBeVisible();
  expect(detail().getByText("待交付")).toBeVisible(); expect(taskWrites).not.toHaveBeenCalled();
  expect(dispatch().getByRole("button", { name: "确认发起分派" })).toBeDisabled();
  stateReads.mockResolvedValueOnce(refusal(404)); refreshState(); await flush();
  expect(state().getByRole("alert")).toHaveTextContent("当前状态未确认");
  expect(history().getByText(receiptFor().id)).toBeVisible();
  expect(dispatchWrites).toHaveBeenCalledTimes(1);
});
it.each(["missing", "broken", "stale", "active", "submitted"])("does not synthesize usable authority for %s reads", async kind => {
  if (kind === "missing") authorityReads.mockResolvedValue(refusal(404));
  if (kind === "broken") authorityReads.mockResolvedValue(json({ ...authorityFor(), coordination: { ...authorityFor().coordination, authority_generation: 0 } }));
  if (kind === "stale") storeTask(currentTask, 3, 6);
  if (kind === "active") storeTask({ ...currentTask, active_attempt_id: "attempt-1" });
  if (kind === "submitted") storeTask({ ...submittedTask, revision: 7 });
  await open();
  expect(dispatch().getByRole("button", { name: "确认发起分派" })).toBeDisabled();
  expect(dispatch().getByText(/不能发起分派/)).toBeVisible();
  expect(dispatchWrites).not.toHaveBeenCalled();
});
it.each([404, 409])("keeps old unknown scope/key/body after new Task/authority reads, exact GET and retry HTTP %s until its matching 201", async status => {
  dispatchWrites.mockRejectedValueOnce(new TypeError("Lost registration receipt"));
  await open(); editReason(); submit(); await dispatch().findByText(/分派登记结果未确认/);
  const original = captured(0), oldReceipt = receiptFor();
  const newer = { ...currentTask, revision: 8 }; storeTask(newer, 5); refresh(); await flush();
  expect(dispatch().getByText("任务版本 8 / 授权代次 5")).toBeVisible();
  expect(dispatch().getByText("原操作：任务版本 7 / 授权代次 3")).toBeVisible();
  change(dispatch().getByRole("textbox", { name: "已知分派请求标识" }), oldReceipt.id);
  fireEvent.submit(dispatch().getByRole("form", { name: "读取已知请求" })); await flush();
  expect(state().getByText("已登记，等待协调")).toBeVisible();
  expect(dispatch().getByRole("button", { name: "重试原分派登记" })).toBeEnabled();
  stateReads.mockResolvedValueOnce(refusal(404)); refreshState(); await flush();
  dispatchWrites.mockResolvedValueOnce(refusal(status)); retry(); await flush();
  expect(captured(1)).toEqual(original);
  expect(dispatch().getByRole("textbox", { name: "分派理由" })).toBeDisabled();
  expect(dispatch().queryByRole("region", { name: "分派登记历史回执" })).not.toBeInTheDocument();
  dispatchWrites.mockResolvedValueOnce(json({ ...oldReceipt, task_revision: 8, authority_generation: 5 }, 201)); retry(); await flush();
  expect(captured(2)).toEqual(original); expect(dispatch().getByRole("button", { name: "重试原分派登记" })).toBeEnabled();
  dispatchWrites.mockResolvedValueOnce(json(oldReceipt, 201)); retry(); await flush();
  expect(captured(3)).toEqual(original);
  expect(history().getByText("任务版本 7 / 授权代次 3")).toBeVisible();
  expect(dispatch().getByText("任务版本 8 / 授权代次 5")).toBeVisible();
  expect(dispatch().queryByRole("button", { name: "重试原分派登记" })).not.toBeInTheDocument();
  expect(dispatch().getByRole("textbox", { name: "分派理由" })).toHaveValue(reason);
});
it("requires explicit refresh after a first trusted 409, then distinguishes a new operation from replay", async () => {
  dispatchWrites.mockResolvedValueOnce(refusal(409));
  await open(); editReason(); submit(); await dispatch().findByText(/分派请求与当前任务、授权或协调配置冲突/);
  const original = captured(0); expect(dispatch().getByRole("button", { name: "确认发起分派" })).toBeDisabled();
  expect(dispatch().queryByRole("button", { name: "重试原分派登记" })).not.toBeInTheDocument();
  fireEvent.click(dispatch().getByRole("button", { name: "收起请求分派" })); await expand();
  expect(dispatch().getByRole("button", { name: "确认发起分派" })).toBeDisabled();
  const newer = { ...currentTask, revision: 8 }; storeTask(newer, 5);
  editReason("新的明确理由"); refresh(); await flush();
  const waiting = deferred<Response>(); dispatchWrites.mockReturnValueOnce(waiting.promise);
  submit(); expect(dispatchWrites).toHaveBeenCalledTimes(2);
  expect(captured(1).key).not.toBe(original.key);
  expect(JSON.parse(String(captured(1).body))).toEqual({ expected_task_revision: 8, expected_authority_generation: 5, reason: "新的明确理由" });
  storeTask({ ...newer, revision: 9 }, 6); refresh(); await flush();
  await act(async () => waiting.resolve(json(receiptFor(newer, authorityFor(newer, 5), "新的明确理由"), 201)));
  expect(history().getByText("任务版本 8 / 授权代次 5")).toBeVisible();
  expect(dispatch().getByText("任务版本 9 / 授权代次 6")).toBeVisible();
});
it("retains the operation through detail failures, hidden Task/group switches and late receipts without moving focus or clearing other drafts", async () => {
  dispatchWrites.mockRejectedValueOnce(new TypeError("Lost receipt"));
  const result = await open(); editReason(); submit(); await dispatch().findByText(/分派登记结果未确认/);
  const original = captured(0), aPanel = dispatch().getByRole("textbox", { name: "分派理由" });
  details.mockResolvedValueOnce(json({}, 503)); fireEvent.click(detail().getByRole("button", { name: "刷新详情" })); await flush();
  expect(dispatch().getByRole("button", { name: "重试原分派登记" })).toBeEnabled();
  const waiting = deferred<Response>(); dispatchWrites.mockReturnValueOnce(waiting.promise); retry(); retry();
  expect(dispatchWrites).toHaveBeenCalledTimes(2);
  await select(secondTask); await expand(); editReason("另一个任务的独立理由");
  const secondField = dispatch().getByRole("textbox", { name: "分派理由" });
  result.rerender(ui(other)); await select(bTask); await expand(); editReason("B 群独立理由");
  const bField = dispatch().getByRole("textbox", { name: "分派理由" });
  result.rerender(ui(null));
  const count = authorityReads.mock.calls.length, stateCount = stateReads.mock.calls.length;
  expect(dispatchWrites.mock.calls[1][1]?.signal).toBeUndefined();
  const focus = screen.getByRole("button", { name: "聊天输入" }); focus.focus();
  await act(async () => waiting.resolve(json(receiptFor(), 201)));
  expect(captured(1)).toEqual(original); expect(document.activeElement).toBe(focus);
  expect(authorityReads).toHaveBeenCalledTimes(count); expect(stateReads).toHaveBeenCalledTimes(stateCount);
  expect(secondField).toHaveValue("另一个任务的独立理由"); expect(bField).toHaveValue("B 群独立理由");
  result.rerender(ui(group)); await select(); await flush();
  expect(dispatch().getByRole("textbox", { name: "分派理由" })).toBe(aPanel);
  expect(history().getByText(receiptFor().id)).toBeVisible();
  await select(secondTask); expect(dispatch().getByRole("textbox", { name: "分派理由" })).toBe(secondField);
});
it("does not let another group's matching-looking 201 confirm an unknown operation", async () => {
  dispatchWrites.mockRejectedValueOnce(new TypeError("Lost receipt"));
  const result = await open(); editReason(); submit(); await flush();
  const first = captured(0);
  result.rerender(ui(other)); await select(bTask); await expand(); editReason("B 群理由");
  const bReceipt = receiptFor(bTask, authorityFor(bTask), "B 群理由"); views.set(bReceipt.id, viewFor(bReceipt));
  dispatchWrites.mockResolvedValueOnce(json(bReceipt, 201)); submit(); await flush();
  expect(history().getByText(bReceipt.id)).toBeVisible();
  result.rerender(ui(group)); await flush();
  expect(dispatch().getByRole("button", { name: "重试原分派登记" })).toBeEnabled();
  dispatchWrites.mockResolvedValueOnce(json(bReceipt, 201)); retry(); await flush();
  expect(captured(2)).toEqual(first); expect(dispatch().queryByRole("region", { name: "分派登记历史回执" })).not.toBeInTheDocument();
});
it("rejects generation downgrades and same-generation contradictions while retaining the immutable receipt", async () => {
  views.set(receiptFor().id, viewFor(receiptFor(), "enqueued", 4));
  await open(); editReason(); submit(); await flush();
  expect(state().getByText("已进入协调员投递队列")).toBeVisible();
  for (const incoming of [viewFor(receiptFor(), "pending", 3), viewFor(receiptFor(), "held", 4)]) {
    stateReads.mockResolvedValueOnce(json(incoming)); refreshState(); await flush();
    expect(state().getByRole("alert")).toHaveTextContent("当前状态未确认");
    expect(history().getByText(receiptFor().id)).toBeVisible();
  }
  const late = deferred<Response>(); stateReads.mockReturnValueOnce(late.promise); refreshState();
  stateReads.mockResolvedValueOnce(json(viewFor(receiptFor(), "assigned", 5))); refreshState(); await flush();
  expect(state().getByText("已记录分配，具体执行尚未由本页核实")).toBeVisible();
  await act(async () => late.resolve(json(viewFor(receiptFor(), "enqueued", 4))));
  expect(state().getByText("已记录分配，具体执行尚未由本页核实")).toBeVisible();
  expect(dispatchWrites).toHaveBeenCalledTimes(1);
});
it("keeps TaskLifecycle unknown, creation drafts and C2 settings independent from dispatch", async () => {
  await open();
  const creation = within(panel().getByRole("form", { name: "创建任务" }));
  change(creation.getByRole("textbox", { name: "任务标题" }), "未提交的创建草稿");
  fireEvent.click(detail().getByRole("button", { name: "登记人工尝试" })); await flush();
  expect(detail().getByRole("button", { name: "重试同一次操作" })).toBeEnabled();
  const settings = within(panel().getByRole("region", { name: "协调配置" }));
  fireEvent.click(settings.getByRole("button", { name: "展开协调配置" })); await flush();
  const configuration = within(settings.getByRole("form", { name: "保存协调员" }));
  const coordinator = configuration.getByRole("combobox");
  fireEvent.submit(settings.getByRole("form", { name: "保存协调员" })); await flush();
  dispatchWrites.mockRejectedValueOnce(new TypeError("Lost dispatch receipt")); editReason(); submit(); await flush();
  expect(dispatch().getByRole("button", { name: "重试原分派登记" })).toBeEnabled();
  expect(detail().getByRole("button", { name: "重试同一次操作" })).toBeEnabled();
  expect(creation.getByRole("textbox", { name: "任务标题" })).toHaveValue("未提交的创建草稿");
  expect(configuration.getByRole("combobox")).toBe(coordinator);
  expect(taskWrites).toHaveBeenCalledTimes(1); expect(configWrites).toHaveBeenCalledTimes(1);
});
it("does not accept a duplicate dependency subset as the authorized Task scope", async () => {
  const value = { ...currentTask, dependency_ids: ["dependency-a", "dependency-a"] };
  storeTask(value);
  const authority = authorityFor({ ...value, dependency_ids: ["dependency-a", "dependency-b"] });
  authorities.set(scopeKey(value.group_id, value.id), authority);
  await open();
  expect(dispatch().getByRole("button", { name: "确认发起分派" })).toBeDisabled();
  expect(dispatchWrites).not.toHaveBeenCalled();
});
it("does not fabricate current Task data when the independent dispatch Task GET fails", async () => {
  details.mockResolvedValueOnce(json(currentTask)).mockResolvedValueOnce(json({}, 503));
  await open();
  expect(dispatch().getByRole("button", { name: "确认发起分派" })).toBeDisabled();
  expect(dispatch().getByText(/不能发起分派/)).toBeVisible();
  expect(dispatchWrites).not.toHaveBeenCalled();
});
it.each(["mandate_id", "topic_id"])("retains unknown for a 201 with the wrong %s, and accepts only the late original receipt after revision changes", async field => {
  dispatchWrites.mockResolvedValueOnce(json({ ...receiptFor(), [field]: "unrelated" }, 201));
  await open(); editReason(); submit(); await flush();
  expect(dispatch().getByRole("button", { name: "重试原分派登记" })).toBeEnabled();
  const original = captured(0), waiting = deferred<Response>(); dispatchWrites.mockReturnValueOnce(waiting.promise);
  retry();
  storeTask({ ...currentTask, revision: 8 }, 5); refresh(); await flush();
  expect(dispatch().getByText("任务版本 8 / 授权代次 5")).toBeVisible();
  expect(dispatch().getByText("原操作：任务版本 7 / 授权代次 3")).toBeVisible();
  await act(async () => waiting.resolve(json(receiptFor(), 201)));
  expect(captured(1)).toEqual(original);
  expect(history().getByText("任务版本 7 / 授权代次 3")).toBeVisible();
  expect(dispatch().getByText("任务版本 8 / 授权代次 5")).toBeVisible();
});
it("reports an unknown future status as unreadable, not completion, and preserves the original 201", async () => {
  await open(); editReason(); submit(); await flush();
  stateReads.mockResolvedValueOnce(json({ ...viewFor(), state: { ...viewFor().state, status: "completed" } }));
  refreshState(); await flush();
  expect(state().getByRole("alert")).toHaveTextContent("当前状态未确认");
  expect(history().getByText(receiptFor().id)).toBeVisible();
  expect(detail().getByText("待交付")).toBeVisible();
  expect(dispatchWrites).toHaveBeenCalledTimes(1); expect(taskWrites).not.toHaveBeenCalled();
});
it("states the page-only recovery boundary and can read an explicitly known ID after full unmount, never discovering by Task", async () => {
  dispatchWrites.mockRejectedValueOnce(new TypeError("Lost receipt"));
  const result = await open(); editReason(); submit(); await flush(); result.unmount();
  await open();
  expect(dispatch().getByText(/刷新浏览器或完全离开本页会丢失本页幂等键和已知请求标识/)).toBeVisible();
  expect(dispatch().queryByRole("button", { name: "重试原分派登记" })).not.toBeInTheDocument();
  change(dispatch().getByRole("textbox", { name: "已知分派请求标识" }), receiptFor().id);
  fireEvent.submit(dispatch().getByRole("form", { name: "读取已知请求" })); await flush();
  expect(state().getByText("已登记，等待协调")).toBeVisible();
  expect(dispatch().queryByRole("region", { name: "分派登记历史回执" })).not.toBeInTheDocument();
  expect(dispatchWrites).toHaveBeenCalledTimes(1);
  expect(requests.mock.calls.every(([url]) => !String(url).includes("by-task") && !String(url).includes("/coordination/decisions"))).toBe(true);
});
