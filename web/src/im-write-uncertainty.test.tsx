import { useRef } from "react";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { Groups } from "./Groups";
import { Members } from "./Members";
import { Discussion } from "./Discussion";
import { RequestWork } from "./RequestWork";
import type { RequestWorkHandle } from "./RequestWork";
import { ReceptionDefaults } from "./ReceptionDefaults";
import type { GroupInput, MemberInput, MessageInput } from "./api";
import type { AdmissionReceipt, RequestWorkInput } from "./coordination-api";
import type { ReceptionConfiguration, ReceptionDefaultsInput } from "./reception-defaults-api";
import { deferred, group, json, other, page } from "./test/groups";
import { member, provider } from "./test/members";
import { message } from "./test/messages";
import { endpoint, settings } from "./test/reception-defaults";

// Current-task reads are independent of the admission write under test.
vi.mock("./task-api", () => ({ getTask: () => new Promise(() => {}) }));

type Kind = "Groups" | "Members" | "Discussion" | "RequestWork" | "ReceptionDefaults";
type Attempt = { method: string; url: string; body: string; key: string | null };
type Reply = "unknown" | "receipt" | { status: number; code: string } | Promise<Response>;
const kinds: Kind[] = ["Groups", "Members", "Discussion", "RequestWork", "ReceptionDefaults"];
const paths: Record<Kind, string> = {
  Groups: "/api/v1/groups", Members: `/api/v1/groups/${group.id}/members`,
  Discussion: `/api/v1/groups/${group.id}/messages`, RequestWork: `/api/v1/groups/${group.id}/coordination/requests`,
  ReceptionDefaults: endpoint,
};
function admission(sent: RequestWorkInput): AdmissionReceipt {
  const created_at = "2026-11-01T08:00:00.123456789Z";
  return {
    decision: { id: "decision-1", trigger_id: "trigger-1", group_id: group.id, topic_id: sent.topic_id, kind: "create_new",
      actor_id: "user", policy_revision: "local-user-explicit/v1", reason: sent.reason, task_id: "task-1",
      task_revision_at_admission: 1, mandate_id: "mandate-1", created_at },
    mandate: { id: "mandate-1", decision_id: "decision-1", origin_trigger_id: "trigger-1", group_id: group.id,
      topic_id: sent.topic_id, task_id: "task-1", task_revision_at_admission: 1, authority_kind: "local_user_explicit",
      authorized_by: "user", strength: "explicit", policy_revision: "local-user-explicit/v1", created_at,
      scope: { objective: sent.new_task.title, description: sent.new_task.description ?? "", acceptance: sent.new_task.acceptance,
        assignee_id: "", dependency_ids: [] } },
    task: { id: "task-1", group_id: group.id, revision_at_admission: 1 },
  };
}
function configuration(body: string): ReceptionConfiguration {
  const sent = JSON.parse(body) as ReceptionDefaultsInput;
  return { id: `defaults-${sent.expected_revision + 1}`, revision: sent.expected_revision + 1,
    provider_id: sent.provider_id, model: sent.model, role: sent.role, created_at: "2026-11-01T06:00:00Z" };
}
function receipt(kind: Kind, body: string): Response {
  switch (kind) {
    case "Groups": return json({ ...group, ...JSON.parse(body) as GroupInput }, 201);
    case "Members": return json({ ...member, ...JSON.parse(body) as MemberInput }, 201);
    case "Discussion": return json({ ...message, ...JSON.parse(body) as MessageInput, deliveries: [] }, 201);
    case "RequestWork": return json(admission(JSON.parse(body) as RequestWorkInput), 201);
    case "ReceptionDefaults": return json(configuration(body));
  }
}
function fixture(kind: Kind) {
  const attempts: Attempt[] = [], reads: string[] = [];
  const effects = new Map<string | null, Attempt>();
  let reply: Reply = "unknown", saved: ReceptionConfiguration | null = null;
  vi.stubGlobal("fetch", async (url: RequestInfo | URL, init?: RequestInit) => {
    const path = String(url), method = init?.method ?? "GET";
    if (method !== "GET") {
      if (path !== paths[kind] || method !== (kind === "ReceptionDefaults" ? "PUT" : "POST")) {
        throw new Error(`Unexpected fixture write: ${method} ${path}`);
      }
      const attempt = { method, url: path, body: String(init?.body), key: new Headers(init?.headers).get("Idempotency-Key") };
      attempts.push(attempt);
      if (reply instanceof Promise) return reply;
      if (typeof reply === "object") return json({ error: { code: reply.code } }, reply.status);
      // Simulate one committed effect per key, not one effect per HTTP attempt.
      if (!effects.has(attempt.key)) effects.set(attempt.key, attempt);
      if (reply === "unknown") throw new TypeError("fixture: committed write, receipt lost");
      return receipt(kind, effects.get(attempt.key)!.body);
    }
    reads.push(path);
    if (path === endpoint) return settings(saved);
    if (path === "/api/v1/providers?limit=50") return page([provider]);
    if (path === "/api/v1/groups?limit=50") return page([group, other]);
    if ([group.id, other.id].some(id => path === `/api/v1/groups/${id}/members?limit=50`
      || path === `/api/v1/groups/${id}/messages?limit=50`)) return page();
    throw new Error(`Unexpected fixture read: ${path}`);
  });
  return { attempts, reads, effects, reply(value: Reply) { reply = value; }, saved(value: ReceptionConfiguration) { saved = value; } };
}
const form = (name: string) => screen.getByRole("form", { name });
const field = (name: string) => screen.getByRole("textbox", { name });
const change = (name: string, value: string) => fireEvent.change(field(name), { target: { value } });
const submit = (name: string) => fireEvent.submit(form(name));
const retry = (formName: string, name: string) => within(form(formName)).getByRole("button", { name });
const unknown = async (name: string, status?: number) => {
  const alert = await within(form(name)).findByRole("alert");
  await waitFor(() => expect(alert).toHaveTextContent(status ? `HTTP ${status}` : "未确认"));
  expect(alert).toHaveTextContent("未确认");
};
function frozen(formName: string, retryLabel: string) {
  const controls = within(form(formName));
  for (const control of [...controls.getAllByRole("textbox"), ...controls.queryAllByRole("combobox"), ...controls.queryAllByRole("checkbox")]) {
    expect(control).toBeDisabled();
  }
  expect(retry(formName, retryLabel)).toBeEnabled();
}
function identical(attempts: Attempt[], count: number) {
  expect(attempts).toHaveLength(count);
  expect(attempts[0].key).toMatch(/^[\da-f-]{36}$/);
  for (const attempt of attempts) expect(attempt).toEqual(attempts[0]);
}
async function fillGroups() {
  await screen.findByRole("button", { name: group.name });
  fireEvent.click(screen.getByRole("button", { name: "展开创建群组" }));
  change("群组名称", "保留群组"); change("群组说明（可选）", "原始说明");
}
async function fillMembers() {
  fireEvent.click(await screen.findByRole("button", { name: "展开创建成员" }));
  change("成员名称", "保留成员"); change("成员职责", "原始职责");
  change("模型（可选）", "原始模型"); change("工作目录（可选）", "/original/workspace");
  await screen.findByRole("option", { name: /pi/ });
  fireEvent.change(screen.getByRole("combobox", { name: "运行器（可选）" }), { target: { value: provider.id } });
}
async function fillDiscussion() {
  await screen.findByText("尚无消息，可发送第一条讨论。");
  change("消息内容", "  保留消息\n原文  ");
  fireEvent.change(screen.getByRole("combobox", { name: "接收方式" }), { target: { value: "selected" } });
  fireEvent.click(screen.getByRole("checkbox", { name: `${member.name} · ${member.role} · ${member.id}` }));
}
function WorkHarness({ activeGroup = group.id }: { activeGroup?: string }) {
  const a = useRef<RequestWorkHandle>(null), b = useRef<RequestWorkHandle>(null);
  return <>{[group.id, other.id].map(id => {
    const handle = id === group.id ? a : b;
    const source = { ...message, group_id: id, id: id === group.id ? message.id : "message-b" };
    return <section key={id} aria-label={`群 ${id}`} hidden={id !== activeGroup}>
      <button type="button" onClick={event => handle.current?.open(source, event.currentTarget)}>选择来源 {id}</button>
      <button type="button" onClick={event => handle.current?.open({ ...source, id: "message-next" }, event.currentTarget)}>选择下一来源 {id}</button>
      <RequestWork groupId={id} active={id === activeGroup} ref={handle} />
    </section>;
  })}</>;
}
function fillWork() {
  fireEvent.click(screen.getByRole("button", { name: `选择来源 ${group.id}` }));
  change("任务标题", "保留任务"); change("任务描述（可选）", "原始任务描述");
  change("验收条件", "可验证"); change("授权理由", "明确授权");
}
async function fillReception() {
  await screen.findByText("尚未配置默认接待者。"); await screen.findByRole("option", { name: /pi/ });
  fireEvent.change(screen.getByRole("combobox", { name: "运行器" }), { target: { value: provider.id } });
  change("模型", "保留模型"); change("职责", "保留职责");
}
afterEach(() => vi.unstubAllGlobals());

it("Groups keeps historical uncertainty through 404/409 and hidden retries until an exact background receipt", async () => {
  const f = fixture("Groups"), onSelect = vi.fn();
  const frame = (active: boolean) => <><button type="button">其他群焦点</button><div hidden={!active}>
    <Groups active={active} selectedId={other.id} onSelect={onSelect} />
  </div></>;
  const view = render(frame(true)); await fillGroups();
  const original = form("创建群组"); submit("创建群组"); await unknown("创建群组");
  const roundTrip = () => {
    fireEvent.click(screen.getByRole("button", { name: "收起创建群组" }));
    expect(screen.getByRole("alert", { name: "创建群组状态" })).toHaveTextContent("未确认");
    view.rerender(frame(false)); view.rerender(frame(true));
    fireEvent.click(screen.getByRole("button", { name: "展开创建群组" }));
    expect(form("创建群组")).toBe(original);
  };
  frozen("创建群组", "重试同一次创建"); roundTrip();
  for (const status of [404, 409]) {
    f.reply({ status, code: status === 404 ? "not_found" : "conflict" });
    fireEvent.click(retry("创建群组", "重试同一次创建")); await unknown("创建群组", status);
    frozen("创建群组", "重试同一次创建"); expect(field("群组名称")).toHaveValue("保留群组");
    expect(field("群组说明（可选）")).toHaveValue("原始说明"); roundTrip();
  }
  const pending = deferred<Response>(); f.reply(pending.promise);
  fireEvent.click(retry("创建群组", "重试同一次创建")); view.rerender(frame(false));
  const focused = screen.getByRole("button", { name: "其他群焦点" }); focused.focus();
  await act(async () => pending.resolve(receipt("Groups", f.attempts[0].body)));
  expect(focused).toHaveFocus(); expect(onSelect).not.toHaveBeenCalled(); expect(original).not.toBeVisible();
  view.rerender(frame(true)); expect(focused).toHaveFocus(); expect(form("创建群组")).toBe(original);
  expect(field("群组名称")).toBeEnabled(); expect(field("群组名称")).toHaveValue("");
  expect(field("群组说明（可选）")).toHaveValue("");
  expect(retry("创建群组", "创建群组")).toBeEnabled();
  identical(f.attempts, 4); expect(f.effects.size).toBe(1);
});

it("Members retains the original URL/body/key across rejected retries and preserves B's draft and focus", async () => {
  const f = fixture("Members"), frame = (id: string) => <Members groupId={id} />;
  const view = render(frame(group.id)); await fillMembers();
  const original = form("创建成员"); submit("创建成员"); await unknown("创建成员");
  frozen("创建成员", "重试同一次成员创建");
  fireEvent.click(screen.getByRole("button", { name: "收起创建成员" }));
  view.rerender(frame(other.id)); fireEvent.click(await screen.findByRole("button", { name: "展开创建成员" }));
  change("成员名称", "B 成员草稿"); change("成员职责", "B 职责");
  const returnToA = () => {
    view.rerender(frame(group.id));
    fireEvent.click(screen.getByRole("button", { name: "展开创建成员" }));
    expect(form("创建成员")).toBe(original);
  };
  returnToA();
  for (const status of [404, 409]) {
    f.reply({ status, code: status === 404 ? "not_found" : "conflict" });
    fireEvent.click(retry("创建成员", "重试同一次成员创建")); await unknown("创建成员", status);
    frozen("创建成员", "重试同一次成员创建");
    for (const [name, value] of [["成员名称", "保留成员"], ["成员职责", "原始职责"], ["模型（可选）", "原始模型"], ["工作目录（可选）", "/original/workspace"]]) {
      expect(field(name)).toHaveValue(value);
    }
    expect(screen.getByRole("combobox", { name: "运行器（可选）" })).toHaveValue(provider.id);
    fireEvent.click(screen.getByRole("button", { name: "收起创建成员" }));
    view.rerender(frame(other.id)); expect(field("成员名称")).toHaveValue("B 成员草稿"); returnToA();
  }
  const pending = deferred<Response>(); f.reply(pending.promise);
  fireEvent.click(retry("创建成员", "重试同一次成员创建")); view.rerender(frame(other.id));
  const focused = field("成员名称"); focused.focus();
  await act(async () => pending.resolve(receipt("Members", f.attempts[0].body)));
  expect(focused).toHaveFocus(); expect(focused).toHaveValue("B 成员草稿"); expect(field("成员职责")).toHaveValue("B 职责");
  expect(screen.queryByRole("button", { name: "保留成员" })).not.toBeInTheDocument();
  view.rerender(frame(group.id)); expect(form("创建成员")).toBe(original);
  expect(field("成员名称")).toBeEnabled(); expect(field("成员名称")).toHaveValue("");
  expect(retry("创建成员", "创建成员")).toBeEnabled();
  expect(screen.getByRole("button", { name: "保留成员" })).toHaveAttribute("aria-pressed", "true");
  identical(f.attempts, 4); expect(f.effects.size).toBe(1);
});

it("Discussion keeps content and routing locked through 404/409 without rerouting or stealing B's focus", async () => {
  const f = fixture("Discussion"), frame = (id: string) => <Discussion groupId={id} members={{ [group.id]: [member] }} />;
  const view = render(frame(group.id)); await fillDiscussion();
  const original = form("发送消息"); submit("发送消息"); await unknown("发送消息");
  frozen("发送消息", "重试同一次发送");
  view.rerender(frame(other.id)); change("消息内容", "B 消息草稿");
  fireEvent.change(screen.getByRole("combobox", { name: "接收方式" }), { target: { value: "public" } });
  view.rerender(frame(group.id));
  for (const status of [404, 409]) {
    f.reply({ status, code: status === 404 ? "not_found" : "conflict" });
    fireEvent.click(retry("发送消息", "重试同一次发送")); await unknown("发送消息", status);
    frozen("发送消息", "重试同一次发送"); expect(form("发送消息")).toBe(original);
    expect(field("消息内容")).toHaveValue("  保留消息\n原文  ");
    expect(screen.getByRole("combobox", { name: "接收方式" })).toHaveValue("selected");
    expect(screen.getByRole("checkbox")).toBeChecked();
    view.rerender(frame(other.id)); expect(field("消息内容")).toHaveValue("B 消息草稿"); view.rerender(frame(group.id));
  }
  const pending = deferred<Response>(); f.reply(pending.promise);
  fireEvent.click(retry("发送消息", "重试同一次发送")); view.rerender(frame(other.id));
  const focused = field("消息内容"); focused.focus();
  await act(async () => pending.resolve(receipt("Discussion", f.attempts[0].body)));
  expect(focused).toHaveFocus(); expect(focused).toHaveValue("B 消息草稿");
  expect(screen.getByRole("combobox", { name: "接收方式" })).toHaveValue("public");
  expect(screen.queryByRole("article")).not.toBeInTheDocument(); view.rerender(frame(group.id));
  expect(form("发送消息")).toBe(original); expect(field("消息内容")).toBeEnabled(); expect(field("消息内容")).toHaveValue("");
  expect(screen.getByText(/消息已持久化/)).toBeVisible(); expect(retry("发送消息", "发送消息")).toBeEnabled();
  identical(f.attempts, 4); expect(f.effects.size).toBe(1);
});

it("RequestWork retains its source and unknown phase through rejected retries and a collapsed background receipt", async () => {
  const f = fixture("RequestWork"), view = render(<WorkHarness />); fillWork();
  const original = form("从消息创建任务"); submit("从消息创建任务"); await unknown("从消息创建任务");
  frozen("从消息创建任务", "重试同一次创建");
  fireEvent.click(screen.getByRole("button", { name: `选择下一来源 ${group.id}` }));
  expect(screen.getByRole("status")).toHaveTextContent("原请求尚未确认");
  fireEvent.click(screen.getByRole("button", { name: "收起" }));
  view.rerender(<WorkHarness activeGroup={other.id} />);
  fireEvent.click(screen.getByRole("button", { name: `选择来源 ${other.id}` })); change("任务标题", "B 任务草稿");
  const returnToA = () => {
    view.rerender(<WorkHarness />);
    fireEvent.click(screen.getByRole("button", { name: "恢复未确认的任务创建" }));
    expect(form("从消息创建任务")).toBe(original);
  };
  returnToA();
  for (const status of [404, 409]) {
    f.reply({ status, code: status === 404 ? "not_found" : "conflict" });
    fireEvent.click(retry("从消息创建任务", "重试同一次创建")); await unknown("从消息创建任务", status);
    frozen("从消息创建任务", "重试同一次创建");
    for (const [name, value] of [["任务标题", "保留任务"], ["任务描述（可选）", "原始任务描述"], ["验收条件", "可验证"], ["授权理由", "明确授权"]]) {
      expect(field(name)).toHaveValue(value);
    }
    expect(screen.queryByRole("button", { name: "放弃草稿" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "收起" }));
    view.rerender(<WorkHarness activeGroup={other.id} />); expect(field("任务标题")).toHaveValue("B 任务草稿"); returnToA();
  }
  const pending = deferred<Response>(); f.reply(pending.promise);
  fireEvent.click(retry("从消息创建任务", "重试同一次创建")); fireEvent.click(screen.getByRole("button", { name: "收起" }));
  view.rerender(<WorkHarness activeGroup={other.id} />); const focused = field("任务标题"); focused.focus();
  await act(async () => pending.resolve(receipt("RequestWork", f.attempts[0].body)));
  expect(focused).toHaveFocus(); expect(focused).toHaveValue("B 任务草稿");
  expect(screen.queryByRole("region", { name: "授权回执" })).not.toBeInTheDocument();
  view.rerender(<WorkHarness />); expect(screen.queryByRole("region", { name: "授权回执" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "查看授权回执" }));
  expect(screen.getByRole("region", { name: "授权回执" })).toHaveTextContent("保留任务");
  fireEvent.click(screen.getByRole("button", { name: `选择下一来源 ${group.id}` }));
  expect(field("任务标题")).toBeEnabled(); expect(field("任务标题")).toHaveValue("");
  identical(f.attempts, 4); expect(f.effects.size).toBe(1);
});

it.each(["conflict", "reception_revision_conflict"])("ReceptionDefaults keeps historical uncertainty after %s and a matching GET", async code => {
  const f = fixture("ReceptionDefaults");
  const frame = (active: boolean) => <><button type="button">其他面板焦点</button><div hidden={!active}><ReceptionDefaults /></div></>;
  const view = render(frame(true)); await fillReception();
  const original = form("默认接待设置"); submit("默认接待设置"); await unknown("默认接待设置");
  frozen("默认接待设置", "重试同一次保存");
  const rejections = code === "conflict" ? [404, 409] : [409, 404];
  for (const status of rejections) {
    f.reply({ status, code: status === 409 ? code : "not_found" });
    fireEvent.click(retry("默认接待设置", "重试同一次保存")); await unknown("默认接待设置", status);
    frozen("默认接待设置", "重试同一次保存");
    expect(field("模型")).toHaveValue("保留模型"); expect(field("职责")).toHaveValue("保留职责");
    expect(screen.getByRole("combobox", { name: "运行器" })).toHaveValue(provider.id);
    const readCount = f.reads.filter(path => path === endpoint).length;
    f.saved(configuration(f.attempts[0].body));
    fireEvent.click(screen.getByRole("button", { name: /刷新已保存设置|载入最新设置并保留草稿/ }));
    await waitFor(() => expect(f.reads.filter(path => path === endpoint)).toHaveLength(readCount + 1));
    await waitFor(() => expect(screen.getByRole("button", { name: /刷新已保存设置|载入最新设置并保留草稿/ })).toBeEnabled());
    expect(screen.getByText("版本 1")).toBeVisible(); frozen("默认接待设置", "重试同一次保存");
    expect(field("模型")).toHaveValue("保留模型"); expect(field("职责")).toHaveValue("保留职责");
    view.rerender(frame(false)); view.rerender(frame(true)); expect(form("默认接待设置")).toBe(original);
  }
  const pending = deferred<Response>(); f.reply(pending.promise);
  fireEvent.click(retry("默认接待设置", "重试同一次保存")); view.rerender(frame(false));
  const focused = screen.getByRole("button", { name: "其他面板焦点" }); focused.focus();
  await act(async () => pending.resolve(receipt("ReceptionDefaults", f.attempts[0].body)));
  expect(focused).toHaveFocus(); expect(original).not.toBeVisible(); view.rerender(frame(true)); expect(focused).toHaveFocus();
  expect(field("模型")).toBeEnabled(); expect(field("职责")).toBeEnabled();
  expect(screen.getByRole("combobox", { name: "运行器" })).toBeEnabled();
  expect(screen.getByText(/已保存默认接待设置；不会修改/)).toBeVisible();
  expect(retry("默认接待设置", "保存默认接待设置")).toBeEnabled();
  identical(f.attempts, 4); expect(f.effects.size).toBe(1);
  expect(JSON.parse(f.attempts[3].body).expected_revision).toBe(0);
});

it.each(kinds)("%s still permits a corrected new-key operation after its first definitive rejection", async kind => {
  const f = fixture(kind); f.reply({ status: 400, code: "invalid" });
  let formName: string, primary: string;
  switch (kind) {
    case "Groups": render(<Groups onSelect={() => {}} />); await fillGroups(); formName = "创建群组"; primary = "群组名称"; break;
    case "Members": render(<Members groupId={group.id} />); await fillMembers(); formName = "创建成员"; primary = "成员名称"; break;
    case "Discussion": render(<Discussion groupId={group.id} members={{ [group.id]: [member] }} />); await fillDiscussion(); formName = "发送消息"; primary = "消息内容"; break;
    case "RequestWork": render(<WorkHarness />); fillWork(); formName = "从消息创建任务"; primary = "任务标题"; break;
    case "ReceptionDefaults": render(<ReceptionDefaults />); await fillReception(); formName = "默认接待设置"; primary = "模型"; break;
  }
  submit(formName); expect(await within(form(formName)).findByRole("alert")).toHaveTextContent("HTTP 400");
  expect(field(primary)).toBeEnabled(); expect(f.effects.size).toBe(0);
  change(primary, "已修正输入"); f.reply("receipt"); submit(formName);
  if (kind === "RequestWork") {
    expect(await screen.findByRole("region", { name: "授权回执" })).toHaveTextContent("已修正输入");
  } else {
    await waitFor(() => expect(field(primary)).toBeEnabled());
    expect(field(primary)).toHaveValue(kind === "ReceptionDefaults" ? "已修正输入" : "");
    expect(within(form(formName)).queryByRole("alert")).not.toBeInTheDocument();
  }
  expect(f.attempts).toHaveLength(2); expect(f.attempts[1].url).toBe(f.attempts[0].url);
  expect(f.attempts[1].key).not.toBe(f.attempts[0].key); expect(f.attempts[1].body).not.toBe(f.attempts[0].body);
  expect(f.effects.size).toBe(1);
});
