import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { App } from "./App";
import { Tasks } from "./Tasks";
import type { Group, Member } from "./api";
import type { CoordinatorBinding, DispatchPolicy } from "./coordinator-configuration-api";
import { OpenbotShell } from "./openbot-shell/OpenbotShell";
import type { Task, TaskAttempt, TaskSubmission } from "./task-api";
import { deferred, group, json, other, page } from "./test/groups";
import { secondTask, submittedTask, task } from "./test/tasks";

// Real App/Tasks/TaskLifecycle/settings/clients; only the fetch transport is replaced.
type Kind = "coordinator" | "policy";
const kinds: Kind[] = ["coordinator", "policy"];
const labels = { coordinator: "协调员", policy: "分派策略" };
const paths = { coordinator: "coordinator", policy: "dispatch-policy" };
const memberId = (groupId: string, name: string) => `${groupId}-${name}`;
const binding = (groupId = group.id, name = "lead", revision = 1): CoordinatorBinding =>
  ({ group_id: groupId, member_id: memberId(groupId, name), revision });
const policy = (groupId = group.id, names = ["worker"], revision = 1): DispatchPolicy =>
  ({ group_id: groupId, allowed_worker_ids: names.map(name => memberId(groupId, name)), revision });
const roster = (groupId: string, names = ["lead", "worker", "candidate", "extra"]): Member[] => names.map(name => ({
  id: memberId(groupId, name), group_id: groupId, name: `${groupId} ${name}`, role: "普通成员", status: "active",
  provider_id: "", model: "", creator_id: "user", owner_id: "user", workspace: "", created_at: task.created_at,
}));
const humanAttempt: TaskAttempt = { id: "attempt-1", group_id: group.id, task_id: task.id,
  task_revision: 1, turn_id: "", created_at: task.created_at };
const activeTask: Task = { ...task, active_attempt_id: humanAttempt.id };
const reviewableTask: Task = { ...submittedTask, id: secondTask.id, title: secondTask.title,
  active_attempt_id: "attempt-2", current_submission_id: "submission-2" };
const delivery: TaskSubmission = { id: "submission-1", group_id: group.id, task_id: task.id, task_revision: 1,
  attempt_id: humanAttempt.id, summary: "  人工交付原文\n保留空白。  ", artifact_versions: [], submitted_by: "user", created_at: task.created_at };
const reviewDelivery: TaskSubmission = { ...delivery, id: "submission-2", task_id: reviewableTask.id,
  attempt_id: reviewableTask.active_attempt_id, summary: "第二个任务的已有交付" };
const bTask: Task = { ...task, id: "b-task", group_id: other.id, title: "B 群任务" };
const configurations = { coordinator: new Map<string, CoordinatorBinding>(), policy: new Map<string, DispatchPolicy>() };
const reads = { coordinator: vi.fn<typeof fetch>(), policy: vi.fn<typeof fetch>() };
const puts = { coordinator: vi.fn<typeof fetch>(), policy: vi.fn<typeof fetch>() };
const members = vi.fn<typeof fetch>(), taskReads = vi.fn<typeof fetch>(), details = vi.fn<typeof fetch>();
const evidence = vi.fn<typeof fetch>(), taskWrites = vi.fn<typeof fetch>(), requests = vi.fn<typeof fetch>();
let unexpected: string[];
const urlOf = (input: RequestInfo | URL) => new URL(String(input), "http://fixture.invalid");
const groupOf = (input: RequestInfo | URL) => decodeURIComponent(urlOf(input).pathname.split("/")[4]);
const identity = ([input, init]: Parameters<typeof fetch>) => ({
  url: String(input), body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key"),
});
const readCount = () => requests.mock.calls.filter(([, init]) => (init?.method ?? "GET") === "GET").length;
const mutations = () => requests.mock.calls.filter(([, init]) => (init?.method ?? "GET") !== "GET");
const panel = () => within(screen.getByRole("region", { name: "群组任务" }));
const detail = () => within(panel().getByRole("region", { name: "任务详情" }));
const settings = () => panel().getByRole("region", { name: "协调配置" });
const form = (kind: Kind, scope = settings()) => within(scope).getByRole("form", { name: `保存${labels[kind]}` });
const current = (kind: Kind, scope = settings()) => within(within(scope).getByRole("region", { name: `当前${labels[kind]}` }));
const history = (kind: Kind, scope = settings()) => within(within(scope).getByRole("region", { name: `${labels[kind]}历史保存回执` }));
const choose = (kind: Kind, name: string, groupId = group.id) =>
  fireEvent.change(within(form(kind)).getByRole("combobox"), { target: { value: memberId(groupId, name) } });
const save = (kind: Kind) => fireEvent.submit(form(kind));
const refresh = () => fireEvent.click(within(settings()).getByRole("button", { name: "刷新配置并保留草稿" }));
const row = (value: Task) => panel().getByRole("button", { name: `${value.title}，${value.status === "submitted" ? "待验收" : "待交付"}` });
const change = (field: HTMLElement, value: string) => fireEvent.change(field, { target: { value } });
const flush = async () => { await act(async () => {}); };
const ui = (value: Group | null) => <><button type="button">聊天输入</button><Tasks group={value} /></>;
async function expand() {
  fireEvent.click(within(settings()).getByRole("button", { name: "展开协调配置" }));
  await flush();
  expect(current("coordinator").getByText(/版本 1/)).toBeVisible();
  expect(current("policy").getByText(/版本 1/)).toBeVisible();
  expect(within(form("coordinator")).getByRole("combobox")).toBeEnabled();
}
async function select(value: Task) {
  fireEvent.click(await panel().findByRole("button", { name: `${value.title}，${value.status === "submitted" ? "待验收" : "待交付"}` }));
  await detail().findByRole("heading", { name: value.title });
  await flush();
}
const rejection = (status: number) => json({ error: { code: status === 404 ? "not_found" : "conflict", message: "请求被拒绝" } }, status);

beforeEach(() => {
  unexpected = [];
  for (const value of [group, other]) {
    configurations.coordinator.set(value.id, binding(value.id));
    configurations.policy.set(value.id, policy(value.id));
  }
  for (const kind of kinds) {
    reads[kind].mockReset().mockImplementation(async input => json(configurations[kind].get(groupOf(input))));
    puts[kind].mockReset().mockRejectedValue(new TypeError("Lost configuration receipt"));
  }
  members.mockReset().mockImplementation(async input => page(roster(groupOf(input))));
  taskReads.mockReset().mockImplementation(async input => page(groupOf(input) === other.id ? [bTask] : [activeTask, reviewableTask]));
  details.mockReset().mockImplementation(async input => json(groupOf(input) === other.id ? bTask
    : urlOf(input).pathname.endsWith(`/${reviewableTask.id}`) ? reviewableTask : activeTask));
  evidence.mockReset().mockImplementation(async input => json(urlOf(input).pathname.includes("/attempts/") ? humanAttempt : reviewDelivery));
  taskWrites.mockReset().mockRejectedValue(new TypeError("Lost task receipt"));
  requests.mockReset().mockImplementation((input, init) => {
    const url = urlOf(input), method = init?.method ?? "GET";
    const kind = kinds.find(kind => url.pathname.endsWith(`/coordination/${paths[kind]}`));
    if (kind && (method === "GET" || method === "PUT")) return (method === "PUT" ? puts[kind] : reads[kind])(input, init);
    if (method === "GET") {
      if (url.pathname === "/api/v1/groups") return Promise.resolve(page([group, other]));
      if (url.pathname.endsWith("/members")) return members(input, init);
      if (url.pathname.endsWith("/messages")) return Promise.resolve(page());
      if (/\/tasks\/[^/]+\/(attempts|submissions)\/[^/]+$/.test(url.pathname)) return evidence(input, init);
      if (url.pathname.endsWith("/tasks")) return taskReads(input, init);
      if (/\/tasks\/[^/]+$/.test(url.pathname)) return details(input, init);
    }
    if (method === "POST" && /\/tasks\/[^/]+\/submissions$/.test(url.pathname)) return taskWrites(input, init);
    unexpected.push(`${method} ${input}`);
    return Promise.reject(new Error(`Unexpected request: ${method} ${input}`));
  });
  vi.stubGlobal("fetch", requests);
});
afterEach(() => {
  vi.unstubAllGlobals();
  expect(unexpected).toEqual([]);
});

it.each(["classic", "openbot"] as const)("lazily reads settings through the real %s App, separately from its existing roster", async shell => {
  members.mockImplementation(async input => page(roster(groupOf(input), urlOf(input).searchParams.has("before")
    ? ["candidate", "extra"] : ["lead", "worker"]), urlOf(input).searchParams.has("before") ? "" : "members+/"));
  render(<App shell={shell === "openbot" ? OpenbotShell : undefined} />);
  fireEvent.click(await screen.findByRole("button", { name: group.name }));
  await screen.findByText("尚无消息，可发送第一条讨论。");
  expect(members).toHaveBeenCalledTimes(1);
  const appRoster = members.mock.calls[0];
  expect(reads.coordinator).not.toHaveBeenCalled(); expect(reads.policy).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "任务" }));
  await panel().findByRole("button", { name: `${activeTask.title}，待交付` });
  const configurationPanel = settings();
  expect(within(configurationPanel).getByRole("button", { name: "展开协调配置" })).toHaveAttribute("aria-expanded", "false");
  expect(within(configurationPanel).queryByRole("form")).not.toBeInTheDocument();
  await select(activeTask);
  fireEvent.click(detail().getByRole("button", { name: "刷新详情" }));
  await detail().findByRole("textbox", { name: "交付摘要" });
  fireEvent.click(panel().getByRole("button", { name: "刷新任务" }));
  await detail().findByRole("textbox", { name: "交付摘要" });
  expect(reads.coordinator).not.toHaveBeenCalled(); expect(reads.policy).not.toHaveBeenCalled();
  expect(members).toHaveBeenCalledTimes(1);
  await expand();
  expect(settings()).toBe(configurationPanel);
  expect(reads.coordinator).toHaveBeenCalledTimes(1); expect(reads.policy).toHaveBeenCalledTimes(1);
  expect(members).toHaveBeenCalledTimes(2);
  expect(members.mock.calls[1][1]?.signal).not.toBe(appRoster[1]?.signal);
  expect(within(form("coordinator")).queryByRole("option", { name: /candidate/ })).not.toBeInTheDocument();
  fireEvent.click(within(configurationPanel).getByRole("button", { name: "加载更多成员" }));
  await within(form("coordinator")).findByRole("option", { name: /candidate/ });
  expect(members).toHaveBeenCalledTimes(3);
  expect(String(members.mock.calls[2][0])).toBe(`/api/v1/groups/${group.id}/members?limit=50&before=members%2B%2F`);
  const coordinatorForm = form("coordinator");
  fireEvent.click(within(configurationPanel).getByRole("button", { name: "收起协调配置" }));
  expect(coordinatorForm).toBeInTheDocument(); expect(coordinatorForm).not.toBeVisible();
  expect(reads.coordinator.mock.calls[0][1]?.signal?.aborted).toBe(true);
  expect(appRoster[1]?.signal?.aborted).toBe(false);
  fireEvent.click(screen.getByRole("button", { name: "关闭详情" }));
  fireEvent.click(screen.getByRole("button", { name: "任务" }));
  await detail().findByRole("textbox", { name: "交付摘要" });
  expect(settings()).toBe(configurationPanel);
  expect(within(configurationPanel).getByRole("button", { name: "展开协调配置" })).toHaveAttribute("aria-expanded", "false");
  expect(reads.coordinator).toHaveBeenCalledTimes(1); expect(reads.policy).toHaveBeenCalledTimes(1);
  expect(members).toHaveBeenCalledTimes(3); expect(mutations()).toHaveLength(0);
});

it("keeps one settings DOM and independent creation, delivery and review drafts across task reads and disclosure changes", async () => {
  render(ui(group)); await panel().findByRole("button", { name: `${activeTask.title}，待交付` });
  await expand();
  const configurationPanel = settings(), coordinatorForm = form("coordinator"), policyForm = form("policy");
  const coordinatorField = within(coordinatorForm).getByRole("combobox");
  const creation = panel().getByRole("form", { name: "创建任务" });
  for (const [name, value] of [["任务标题", "未提交的新任务"], ["任务描述（可选）", "创建描述草稿"], ["验收条件", "创建验收草稿"]]) {
    change(within(creation).getByRole("textbox", { name }), value);
  }
  await select(activeTask); change(detail().getByRole("textbox", { name: "交付摘要" }), delivery.summary);
  await select(reviewableTask); change(detail().getByRole("textbox", { name: "验收说明（可选）" }), "  原始验收说明  ");
  choose("coordinator", "candidate"); choose("policy", "extra");
  refresh(); await flush();
  fireEvent.click(detail().getByRole("button", { name: "刷新详情" }));
  await detail().findByRole("textbox", { name: "验收说明（可选）" });
  expect(detail().getByRole("textbox", { name: "验收说明（可选）" })).toHaveValue("  原始验收说明  ");
  await select(activeTask);
  expect(detail().getByRole("textbox", { name: "交付摘要" })).toHaveValue(delivery.summary);
  expect(settings()).toBe(configurationPanel); expect(form("coordinator")).toBe(coordinatorForm); expect(form("policy")).toBe(policyForm);
  expect(coordinatorField).toHaveValue(memberId(group.id, "candidate"));
  expect(within(policyForm).getByRole("button", { name: `移除 ${memberId(group.id, "extra")}` })).toBeVisible();
  expect(reads.coordinator).toHaveBeenCalledTimes(2); expect(reads.policy).toHaveBeenCalledTimes(2);
  fireEvent.click(within(configurationPanel).getByRole("button", { name: "收起协调配置" }));
  fireEvent.click(within(configurationPanel).getByRole("button", { name: "展开协调配置" }));
  await flush();
  expect(within(form("coordinator")).getByRole("combobox")).toBe(coordinatorField);
  expect(coordinatorField).toHaveValue(memberId(group.id, "candidate"));
  expect(panel().getByRole("form", { name: "创建任务" })).toBe(creation);
  expect(within(creation).getByRole("textbox", { name: "任务标题" })).toHaveValue("未提交的新任务");
  expect(within(creation).getByRole("textbox", { name: "任务描述（可选）" })).toHaveValue("创建描述草稿");
  expect(within(creation).getByRole("textbox", { name: "验收条件" })).toHaveValue("创建验收草稿");
  expect(panel().getByText(/待交付.*不代表已开始执行/)).toBeVisible();
  expect(panel().getByText(/已接纳.*不等同于 CLI 运行成功/)).toBeVisible();
  expect(mutations()).toHaveLength(0);
});

it.each(kinds)("retains a pending %s through task/null/A-B-A boundaries, aborting only reads and isolating late receipts", async kind => {
  const view = render(ui(group)); await panel().findByRole("button", { name: `${activeTask.title}，待交付` });
  await expand(); choose(kind, "candidate");
  const aSettings = settings(), aForm = form(kind), aField = within(aForm).getByRole("combobox");
  const receiptSection = within(aForm).getByRole("region", { name: `${labels[kind]}历史保存回执` });
  const pending = deferred<Response>(); puts[kind].mockReturnValueOnce(pending.promise);
  save(kind); save(kind);
  expect(puts[kind]).toHaveBeenCalledTimes(1);
  const original = identity(puts[kind].mock.calls[0]);
  expect(original.url).toBe(`/api/v1/groups/${group.id}/coordination/${paths[kind]}`);
  expect(original.key).toMatch(/^[\da-f-]{36}$/);
  expect(JSON.parse(String(original.body))).toEqual(kind === "coordinator"
    ? { member_id: memberId(group.id, "candidate"), expected_revision: 1 }
    : { allowed_worker_ids: [memberId(group.id, "worker"), memberId(group.id, "candidate")], expected_revision: 1 });
  const oldBinding = deferred<Response>(), oldPolicy = deferred<Response>(), oldRoster = deferred<Response>();
  reads.coordinator.mockReturnValueOnce(oldBinding.promise); reads.policy.mockReturnValueOnce(oldPolicy.promise);
  members.mockReturnValueOnce(oldRoster.promise); refresh();
  await select(activeTask); await select(reviewableTask);
  expect(settings()).toBe(aSettings); expect(form(kind)).toBe(aForm); expect(aField).toBeDisabled();
  expect(reads.coordinator).toHaveBeenCalledTimes(2); expect(reads.policy).toHaveBeenCalledTimes(2);
  view.rerender(ui(null));
  expect(aSettings).toBeInTheDocument(); expect(aSettings).not.toBeVisible();
  const suspendedReads = readCount(); await flush(); expect(readCount()).toBe(suspendedReads);
  expect(reads.coordinator.mock.calls[1][1]?.signal?.aborted).toBe(true);
  expect(reads.policy.mock.calls[1][1]?.signal?.aborted).toBe(true);
  expect(members.mock.calls[1][1]?.signal?.aborted).toBe(true);
  expect(puts[kind].mock.calls[0][1]?.signal?.aborted).toBe(false);
  await act(async () => { oldBinding.resolve(json(binding(group.id, "extra", 9))); oldPolicy.resolve(json(policy(group.id, [], 9))); oldRoster.resolve(page()); });
  view.rerender(ui(other)); await panel().findByRole("button", { name: `${bTask.title}，待交付` });
  expect(within(settings()).getByRole("button", { name: "展开协调配置" })).toHaveAttribute("aria-expanded", "false");
  expect(reads[kind].mock.calls.some(([input]) => groupOf(input) === other.id)).toBe(false);
  await expand(); choose("coordinator", "extra", other.id);
  const bSettings = settings(), bForm = form("coordinator");
  change(panel().getByRole("textbox", { name: "任务标题" }), "B 的独立创建草稿");
  view.rerender(ui(group)); await detail().findByRole("heading", { name: reviewableTask.title }); await flush();
  expect(settings()).toBe(aSettings); expect(form(kind)).toBe(aForm);
  expect(within(aSettings).getByText(`${labels[kind]}：正在保存，结果待确认。`)).toBeVisible();
  expect(aField).toBeDisabled(); expect(identity(puts[kind].mock.calls[0])).toEqual(original);
  view.rerender(ui(other)); await panel().findByRole("button", { name: `${bTask.title}，待交付` }); await flush();
  row(bTask).focus();
  const beforeReceiptReads = readCount();
  const receipt = kind === "coordinator" ? binding(group.id, "candidate", 2) : policy(group.id, ["worker", "candidate"], 2);
  if (kind === "coordinator") configurations.coordinator.set(group.id, binding(group.id, "candidate", 2));
  else configurations.policy.set(group.id, policy(group.id, ["worker", "candidate"], 2));
  await act(async () => pending.resolve(json(receipt)));
  expect(within(receiptSection).getByText(/本次保存已确认（版本 2）/)).not.toBeVisible();
  expect(row(bTask)).toHaveFocus(); expect(readCount()).toBe(beforeReceiptReads);
  expect(settings()).toBe(bSettings); expect(form("coordinator")).toBe(bForm);
  expect(within(bForm).getByRole("combobox")).toHaveValue(memberId(other.id, "extra"));
  expect(panel().getByRole("textbox", { name: "任务标题" })).toHaveValue("B 的独立创建草稿");
  view.rerender(ui(group)); await detail().findByRole("heading", { name: reviewableTask.title }); await flush();
  expect(settings()).toBe(aSettings); expect(form(kind)).toBe(aForm);
  expect(history(kind).getByText(/本次保存已确认（版本 2）/)).toBeVisible();
  expect(current(kind).getByText(/版本 2/)).toBeVisible();
  expect(current(kind).queryByText(/版本 9/)).not.toBeInTheDocument();
  expect(puts[kind]).toHaveBeenCalledTimes(1); expect(puts[kind === "coordinator" ? "policy" : "coordinator"]).not.toHaveBeenCalled();
  expect(taskWrites).not.toHaveBeenCalled(); expect(mutations()).toHaveLength(1);
});

it("keeps unknown settings identity across task, hidden and group retries until the original matching receipt", async () => {
  const view = render(ui(group)); await panel().findByRole("button", { name: `${activeTask.title}，待交付` });
  await expand(); choose("coordinator", "candidate");
  const aSettings = settings(), aForm = form("coordinator"), aField = within(aForm).getByRole("combobox");
  save("coordinator");
  await within(aForm).findByRole("button", { name: "重试同一次协调员保存" });
  const original = identity(puts.coordinator.mock.calls[0]);
  await select(activeTask); change(detail().getByRole("textbox", { name: "交付摘要" }), delivery.summary);
  await select(reviewableTask); change(detail().getByRole("textbox", { name: "验收说明（可选）" }), "未知配置期间的验收草稿");
  // Even a matching current GET cannot acknowledge the unknown PUT.
  configurations.coordinator.set(group.id, binding(group.id, "candidate", 2)); refresh(); await flush();
  expect(current("coordinator").getByText(/版本 2/)).toBeVisible();
  expect(aField).toBeDisabled(); expect(history("coordinator").queryByText(/本次保存已确认/)).not.toBeInTheDocument();
  for (const status of [404, 409]) {
    const retry = deferred<Response>(); puts.coordinator.mockReturnValueOnce(retry.promise);
    fireEvent.click(within(aForm).getByRole("button", { name: "重试同一次协调员保存" })); save("coordinator");
    expect(within(aSettings).getByText("协调员：结果未知，正在重试原操作。")).toBeVisible();
    expect(puts.coordinator).toHaveBeenCalledTimes(status === 404 ? 2 : 3);
    view.rerender(ui(null));
    expect(puts.coordinator.mock.calls.at(-1)?.[1]?.signal?.aborted).toBe(false);
    if (status === 409) { view.rerender(ui(other)); await select(bTask); }
    const focused = status === 404 ? screen.getByRole("button", { name: "聊天输入" }) : row(bTask);
    focused.focus(); const hiddenReads = readCount();
    await act(async () => retry.resolve(rejection(status)));
    expect(focused).toHaveFocus(); expect(readCount()).toBe(hiddenReads);
    expect(within(aSettings).getByText("协调员：结果未知，请保留原操作。")).not.toBeVisible();
    view.rerender(ui(group)); await detail().findByRole("textbox", { name: "验收说明（可选）" }); await flush();
    expect(settings()).toBe(aSettings); expect(form("coordinator")).toBe(aForm); expect(aField).toBeDisabled();
    expect(within(aForm).getByRole("alert")).toHaveTextContent(`HTTP ${status}`);
    expect(within(aForm).getByRole("button", { name: "重试同一次协调员保存" })).toBeEnabled();
    expect(detail().getByRole("textbox", { name: "验收说明（可选）" })).toHaveValue("未知配置期间的验收草稿");
    expect(identity(puts.coordinator.mock.calls.at(-1)!)).toEqual(original);
  }
  configurations.coordinator.set(group.id, binding(group.id, "extra", 7)); refresh(); await flush();
  puts.coordinator.mockResolvedValueOnce(json(binding(group.id, "candidate", 3)));
  fireEvent.click(within(aForm).getByRole("button", { name: "重试同一次协调员保存" }));
  await within(aForm).findByRole("button", { name: "重试同一次协调员保存" });
  expect(aField).toBeDisabled(); expect(history("coordinator").queryByText(/本次保存已确认/)).not.toBeInTheDocument();
  puts.coordinator.mockResolvedValueOnce(json(binding(group.id, "candidate", 2)));
  fireEvent.click(within(aForm).getByRole("button", { name: "重试同一次协调员保存" })); await flush();
  expect(history("coordinator").getByText(/本次保存已确认（版本 2）/)).toBeVisible();
  expect(current("coordinator").getByText(/版本 7/)).toBeVisible();
  expect(row(reviewableTask)).toHaveAttribute("aria-pressed", "true");
  expect(puts.coordinator).toHaveBeenCalledTimes(5);
  expect(puts.coordinator.mock.calls.map(identity)).toEqual(Array.from({ length: 5 }, () => original));
  await select(activeTask); expect(detail().getByRole("textbox", { name: "交付摘要" })).toHaveValue(delivery.summary);
  expect(puts.policy).not.toHaveBeenCalled(); expect(taskWrites).not.toHaveBeenCalled();
});

it("does not reset a pending or unknown TaskLifecycle operation when saving settings, including an explicit empty policy", async () => {
  render(ui(group)); await panel().findByRole("button", { name: `${activeTask.title}，待交付` }); await expand();
  const creationTitle = panel().getByRole("textbox", { name: "任务标题" }); change(creationTitle, "创建任务草稿仍保留");
  await select(reviewableTask); change(detail().getByRole("textbox", { name: "验收说明（可选）" }), "另一个任务的验收草稿");
  await select(activeTask); change(detail().getByRole("textbox", { name: "交付摘要" }), delivery.summary);
  const submission = deferred<Response>(); taskWrites.mockReturnValueOnce(submission.promise);
  fireEvent.submit(detail().getByRole("form", { name: "提交交付" }));
  const original = identity(taskWrites.mock.calls[0]);
  expect(detail().getByText("正在保存操作，请等待回执…")).toBeVisible();
  choose("coordinator", "candidate"); configurations.coordinator.set(group.id, binding(group.id, "candidate", 2));
  puts.coordinator.mockResolvedValueOnce(json(binding(group.id, "candidate", 2))); save("coordinator"); await flush();
  expect(history("coordinator").getByText(/本次保存已确认（版本 2）/)).toBeVisible();
  expect(detail().getByText("正在保存操作，请等待回执…")).toBeVisible();
  expect(taskWrites).toHaveBeenCalledTimes(1); expect(creationTitle).toHaveValue("创建任务草稿仍保留");
  await select(reviewableTask);
  expect(detail().getByRole("textbox", { name: "验收说明（可选）" })).toHaveValue("另一个任务的验收草稿");
  await select(activeTask);
  expect(detail().getByRole("textbox", { name: "交付摘要" })).toBeDisabled();
  expect(detail().getByRole("textbox", { name: "交付摘要" })).toHaveValue(delivery.summary);
  await act(async () => submission.resolve(json({}, 503)));
  expect(detail().getByRole("alert")).toHaveTextContent("操作结果未确认");
  fireEvent.click(within(form("policy")).getByRole("button", { name: `移除 ${memberId(group.id, "worker")}` }));
  expect(within(form("policy")).getByRole("button", { name: "保存空策略，禁止向任何 worker 分派" })).toBeEnabled();
  configurations.policy.set(group.id, policy(group.id, [], 2)); puts.policy.mockResolvedValueOnce(json(policy(group.id, [], 2)));
  save("policy"); refresh(); await flush();
  expect(history("policy").getByText(/本次保存已确认（版本 2）/)).toBeVisible();
  expect(current("policy").getByText(/禁止向任何 worker 分派/)).toBeVisible();
  expect(JSON.parse(String(identity(puts.policy.mock.calls[0]).body))).toEqual({ allowed_worker_ids: [], expected_revision: 1 });
  expect(detail().getByRole("button", { name: "重试同一次操作" })).toBeEnabled();
  expect(detail().getByRole("textbox", { name: "交付摘要" })).toBeDisabled();
  expect(detail().getByRole("textbox", { name: "交付摘要" })).toHaveValue(delivery.summary);
  expect(taskWrites).toHaveBeenCalledTimes(1); expect(creationTitle).toHaveValue("创建任务草稿仍保留");
  taskWrites.mockResolvedValueOnce(json(delivery, 201));
  fireEvent.click(detail().getByRole("button", { name: "重试同一次操作" }));
  await detail().findByText(`已确认操作回执：${delivery.id}。`); await flush();
  expect(identity(taskWrites.mock.calls[1])).toEqual(original);
  expect(JSON.parse(String(original.body))).toEqual({ expected_revision: 1, attempt_id: humanAttempt.id, summary: delivery.summary });
  expect(detail().getByRole("textbox", { name: "交付摘要" })).toHaveValue("");
  expect(creationTitle).toHaveValue("创建任务草稿仍保留");
  expect(mutations().map(([input, init]) => `${init?.method} ${input}`)).toEqual([
    `POST /api/v1/groups/${group.id}/tasks/${task.id}/submissions`,
    `PUT /api/v1/groups/${group.id}/coordination/coordinator`,
    `PUT /api/v1/groups/${group.id}/coordination/dispatch-policy`,
    `POST /api/v1/groups/${group.id}/tasks/${task.id}/submissions`,
  ]);
  expect(panel().getByText(/保存仅登记配置.*不会创建任务授权或自动开始分派/)).toBeVisible();
  expect(panel().getByText(/本页不释放执行资源/)).toBeVisible();
  expect(panel().getByText(/待交付.*不代表已开始执行/)).toBeVisible();
  expect(panel().getByText(/已接纳.*不等同于 CLI 运行成功/)).toBeVisible();
});
