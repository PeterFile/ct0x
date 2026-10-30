import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { App } from "./App";
import { deferred, group, json, other, page } from "./test/groups";
import { member, provider } from "./test/members";
const reads = vi.fn<typeof fetch>();
const writes = vi.fn<typeof fetch>();
const catalogue = vi.fn<typeof fetch>();
const panel = () => {
  const landmark = screen.getByRole("complementary", { name: "成员" });
  const form = within(landmark).queryByRole("form", { name: "创建成员" });
  return within(form?.parentElement ?? landmark);
};
const field = (name: string) => panel().getByRole("textbox", { name });
const change = (name: string, value: string) => fireEvent.change(field(name), { target: { value } });
const submit = () => fireEvent.submit(panel().getByRole("form", { name: "创建成员" }));
const selectGroup = (name: string) => fireEvent.click(screen.getByRole("button", { name }));
async function open() { render(<App />); fireEvent.click(await screen.findByRole("button", { name: group.name })); }
async function draft() {
  await panel().findByText("尚无成员，可创建群内成员。");
  change("成员名称", member.name); change("成员职责", member.role);
}
beforeEach(() => {
  reads.mockReset().mockImplementation(async () => page());
  writes.mockReset().mockImplementation(async () => json(member, 201));
  catalogue.mockReset().mockImplementation(async () => page([provider]));
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => String(input).includes("/members")
    ? (init?.method === "POST" ? writes : reads)(input, init)
    : String(input).startsWith("/api/v1/providers") ? catalogue(input, init) : Promise.resolve(page([group, other])));
});
afterEach(() => vi.unstubAllGlobals());

it("does not load members or enable creation before a group is selected", async () => {
  render(<App />);
  await screen.findByRole("button", { name: group.name });
  expect(panel().getByText("请先选择群组，再查看成员。")).toBeVisible();
  expect(panel().queryByRole("form")).not.toBeInTheDocument();
  expect(reads).not.toHaveBeenCalled(); expect(catalogue).not.toHaveBeenCalled();
});
it.each([["active", "在群"], ["left", "已离开"], ["future", "状态未知"]])("shows %s membership without claiming runtime readiness", async (status, label) => {
  reads.mockResolvedValueOnce(page([{ ...member, status, provider_id: provider.id, model: "custom-model", workspace: "/work/research" }]));
  await open();
  fireEvent.click(await panel().findByRole("button", { name: member.name }));
  expect(panel().getByText(label)).toBeVisible();
  expect(panel().getByText("custom-model")).toBeVisible(); expect(panel().getByText("/work/research")).toBeVisible();
  expect(panel().getByText("工作目录配置不代表已授予读写权限。")).toBeVisible();
  expect(panel().getByText(/未检测连接状态/)).toBeVisible();
  expect(panel().queryByRole("textbox", { name: /运行器/ })).not.toBeInTheDocument();
  expect(await panel().findByRole("option", { name: /pi.*版本未知.*\/opt\/bin\/pi.*账户已登录（运行未验证）/ })).toHaveValue(provider.id);
});
it("reports member loading, error and real empty results with explicit retry", async () => {
  reads.mockResolvedValueOnce(json({ error: { code: "missing", message: "未找到" } }, 404));
  await open();
  expect(await panel().findByRole("alert")).toHaveTextContent("404");
  fireEvent.click(panel().getByRole("button", { name: "重试载入成员" }));
  await panel().findByText("尚无成员，可创建群内成员。");
  expect(reads).toHaveBeenCalledTimes(2);
});
it("paginates explicitly, deduplicates members and clears group-bound cursors", async () => {
  reads.mockResolvedValueOnce(page([member], "a+/"));
  await open();
  fireEvent.click(await panel().findByRole("button", { name: member.name }));
  expect(reads).toHaveBeenCalledTimes(1);
  reads.mockResolvedValueOnce(page([member, { ...member, id: "member-2", name: "复核员" }]));
  fireEvent.click(panel().getByRole("button", { name: "加载更多成员" }));
  await panel().findByRole("button", { name: "复核员" });
  expect(panel().getAllByRole("button", { name: member.name })).toHaveLength(1);
  expect(panel().getByRole("button", { name: member.name })).toHaveAttribute("aria-pressed", "true");
  expect(reads.mock.calls[1][0]).toBe(`/api/v1/groups/${group.id}/members?limit=50&before=a%2B%2F`);
  selectGroup(other.name); await panel().findByText("尚无成员，可创建群内成员。");
  expect(reads.mock.calls[2][0]).toBe(`/api/v1/groups/${other.id}/members?limit=50`);
  expect(panel().queryByRole("button", { name: member.name })).not.toBeInTheDocument();
  selectGroup(group.name); await act(async () => {});
  expect(reads.mock.calls[3][0]).toBe(`/api/v1/groups/${group.id}/members?limit=50`);
  expect(catalogue).toHaveBeenCalledTimes(1);
});
it("requires a nonblank role and posts only the exact configuration DTO once pending", async () => {
  await open(); await draft();
  change("成员职责", "  "); submit();
  expect(panel().getByRole("alert")).toHaveTextContent("请输入成员名称和职责"); expect(writes).not.toHaveBeenCalled();
  change("成员名称", ` ${member.name} `); change("成员职责", ` ${member.role} `);
  await panel().findByRole("option", { name: /pi.*账户已登录/ });
  fireEvent.change(panel().getByRole("combobox", { name: "运行器（可选）" }), { target: { value: provider.id } });
  change("模型（可选）", "my-model"); change("工作目录（可选）", "/work/research");
  const receipt = deferred<Response>(); writes.mockReturnValueOnce(receipt.promise);
  submit(); submit();
  expect(field("成员名称")).toBeDisabled(); expect(writes).toHaveBeenCalledTimes(1);
  expect(JSON.parse(String(writes.mock.calls[0][1]?.body))).toEqual({ name: member.name, role: member.role, provider_id: provider.id, model: "my-model", workspace: "/work/research" });
  expect(writes.mock.calls[0][0]).toBe(`/api/v1/groups/${group.id}/members`);
  expect(new Headers(writes.mock.calls[0][1]?.headers).get("Content-Type")).toBe("application/json");
  expect(panel().queryByRole("button", { name: member.name })).not.toBeInTheDocument();
  await act(async () => receipt.resolve(json(member, 201)));
  expect(panel().getByRole("button", { name: member.name })).toHaveAttribute("aria-pressed", "true");
  expect(field("成员名称")).toHaveValue("");
});
it.each(["network", "server", "malformed", "wrong-group", "200", "202"])("retains the operation after an unknown %s result", async failure => {
  await open(); await draft();
  if (failure === "network") writes.mockRejectedValueOnce(new TypeError("offline"));
  else writes.mockResolvedValueOnce(failure === "server" ? json({}, 503) : failure === "malformed" ? json({}, 201)
    : failure === "wrong-group" ? json({ ...member, group_id: other.id }, 201) : json(member, Number(failure)));
  submit(); expect(await panel().findByRole("alert")).toHaveTextContent("创建结果未确认");
  expect(field("成员名称")).toBeDisabled(); expect(field("成员名称")).toHaveValue(member.name);
  expect(panel().queryByRole("button", { name: member.name })).not.toBeInTheDocument();
  fireEvent.click(panel().getByRole("button", { name: "重试同一次成员创建" }));
  await panel().findByRole("button", { name: member.name });
  const [first, retry] = writes.mock.calls.map(([url, init]) => ({ url, body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key") }));
  expect(first.key).toMatch(/^[\da-f-]{36}$/); expect(retry).toEqual(first);
});
it.each([400, 413, 415])("allows correction after a definitive validation error HTTP %s with a new key", async status => {
  await open(); await draft();
  const rejectedRole = status === 413 ? member.role.repeat(100) : member.role;
  change("成员职责", rejectedRole);
  const code = status === 413 ? "body_too_large" : status === 415 ? "unsupported_media_type" : "invalid";
  writes.mockResolvedValueOnce(json({ error: { code, message: "请求被拒绝" } }, status));
  submit(); const alert = await panel().findByRole("alert"); expect(alert).toHaveTextContent(String(status));
  if (status === 413) expect(alert).toHaveTextContent("请求内容过大");
  if (status === 415) expect(alert).toHaveTextContent("仅接受 JSON");
  expect(field("成员职责")).toBeEnabled(); expect(field("成员职责")).toHaveValue(rejectedRole);
  change("成员职责", "补充复核"); submit();
  await panel().findByRole("button", { name: member.name });
  expect(writes.mock.calls[1][1]?.body).toBe(JSON.stringify({ name: member.name, role: "补充复核" }));
  expect(new Headers(writes.mock.calls[1][1]?.headers).get("Idempotency-Key")).not.toBe(new Headers(writes.mock.calls[0][1]?.headers).get("Idempotency-Key"));
});
it.each([200, 503])("ignores delayed group A GET %s after switching to B", async status => {
  const delayed = deferred<Response>(); reads.mockReturnValueOnce(delayed.promise);
  await open(); expect(panel().getByText("正在载入成员…")).toBeVisible();
  selectGroup(other.name); await panel().findByText("尚无成员，可创建群内成员。");
  await act(async () => delayed.resolve(status === 200 ? page([member]) : json({}, status)));
  expect(panel().queryByRole("button", { name: member.name })).not.toBeInTheDocument();
  expect(panel().queryByRole("alert")).not.toBeInTheDocument(); expect(field("成员名称")).toHaveValue("");
  expect(reads.mock.calls[0][1]?.signal?.aborted).toBe(true);
});
it("preserves unknown A creation across A to B to A without rerouting its payload", async () => {
  await open(); await draft(); writes.mockRejectedValueOnce(new TypeError("offline")); submit();
  await panel().findByRole("alert"); selectGroup(other.name); await panel().findByText("尚无成员，可创建群内成员。");
  expect(panel().queryByRole("alert")).not.toBeInTheDocument(); expect(field("成员名称")).toHaveValue("");
  change("成员名称", "B 的草稿"); selectGroup(group.name);
  fireEvent.click(await panel().findByRole("button", { name: "重试同一次成员创建" }));
  await panel().findByRole("button", { name: member.name });
  const attempts = writes.mock.calls.map(([url, init]) => [url, init?.body, new Headers(init?.headers).get("Idempotency-Key")]);
  expect(attempts[1]).toEqual(attempts[0]); expect(attempts[0][0]).toBe(`/api/v1/groups/${group.id}/members`);
  selectGroup(other.name); expect(field("成员名称")).toHaveValue("B 的草稿");
});
it("keeps a late A POST receipt and selection out of visible B", async () => {
  await open(); await draft(); const delayed = deferred<Response>(); writes.mockReturnValueOnce(delayed.promise); submit();
  reads.mockResolvedValueOnce(page([{ ...member, id: "b-member", group_id: other.id, name: "B 成员" }]));
  selectGroup(other.name); fireEvent.click(await panel().findByRole("button", { name: "B 成员" }));
  expect(writes.mock.calls[0][1]?.signal?.aborted).toBe(false);
  await act(async () => delayed.resolve(json(member, 201)));
  expect(panel().getByRole("button", { name: "B 成员" })).toHaveAttribute("aria-pressed", "true");
  expect(panel().queryByRole("button", { name: member.name })).not.toBeInTheDocument();
  selectGroup(group.name); expect(panel().getByRole("button", { name: member.name })).toHaveAttribute("aria-pressed", "true");
  await act(async () => {});
});
it.each(["existing", "created"])("refreshes selected %s member details from a newer GET", async origin => {
  if (origin === "existing") reads.mockResolvedValueOnce(page([member]));
  await open();
  if (origin === "created") { await draft(); submit(); }
  fireEvent.click(await panel().findByRole("button", { name: member.name }));
  expect(panel().getByText("在群")).toBeVisible();
  const updated = { ...member, role: "更新后的职责", status: "left" };
  reads.mockResolvedValueOnce(page([updated]));
  fireEvent.click(panel().getByRole("button", { name: "刷新成员" }));
  expect(await panel().findByText(updated.role)).toBeVisible();
  expect(panel().getByText("已离开")).toBeVisible();
  expect(panel().queryByText("在群")).not.toBeInTheDocument();
  expect(panel().getByRole("button", { name: member.name })).toHaveAttribute("aria-pressed", "true");
});
it("does not restore a pre-creation snapshot after returning to A and refreshing it", async () => {
  const old = deferred<Response>(); reads.mockReturnValueOnce(old.promise); await open();
  change("成员名称", member.name); change("成员职责", member.role); submit();
  await panel().findByRole("button", { name: member.name });
  selectGroup(other.name); await panel().findByText("尚无成员，可创建群内成员。");
  reads.mockResolvedValueOnce(page([{ ...member, role: "返回后的职责", status: "left" }]));
  selectGroup(group.name); await panel().findByText("返回后的职责");
  await act(async () => old.resolve(page([{ ...member, role: "旧职责", status: "active" }])));
  expect(panel().getByText("返回后的职责")).toBeVisible(); expect(panel().getByText("已离开")).toBeVisible();
  expect(panel().queryByText("旧职责")).not.toBeInTheDocument();
});
it("protects a confirmed creation from an older member GET", async () => {
  const delayed = deferred<Response>(); reads.mockReturnValueOnce(delayed.promise); await open();
  change("成员名称", member.name); change("成员职责", member.role); submit();
  await panel().findByRole("button", { name: member.name });
  await act(async () => delayed.resolve(page([{ ...member, name: "旧快照" }])));
  expect(panel().getByRole("button", { name: member.name })).toBeVisible(); expect(panel().queryByText("旧快照")).not.toBeInTheDocument();
});
it.each([404, 503])("allows unbound creation when the real catalogue returns %s", async status => {
  catalogue.mockResolvedValueOnce(json({}, status)); await open(); await draft();
  expect(await panel().findByText(/运行器目录暂不可用/)).toBeVisible(); submit();
  await panel().findByRole("button", { name: member.name });
  expect(JSON.parse(String(writes.mock.calls[0][1]?.body))).toEqual({ name: member.name, role: member.role });
});
it("pages the catalogue explicitly and renders unknown future account strings honestly", async () => {
  catalogue.mockResolvedValueOnce(page([{ ...provider, account: "future" }], "next+/")); await open();
  await panel().findByRole("option", { name: /账户未知/ }); expect(catalogue).toHaveBeenCalledTimes(1);
  catalogue.mockResolvedValueOnce(page([{ ...provider, id: "actual-second-id", name: "codex", account: "unauthenticated", version: "1.2.3" }]));
  fireEvent.click(panel().getByRole("button", { name: "加载更多运行器" }));
  expect(await panel().findByRole("option", { name: /codex.*1.2.3.*未登录/ })).toHaveValue("actual-second-id");
  expect(catalogue.mock.calls[1][0]).toBe("/api/v1/providers?limit=50&before=next%2B%2F");
});
it("shows a real empty catalogue, retries a network failure, and aborts on full unmount", async () => {
  catalogue.mockRejectedValueOnce(new TypeError("offline")); await open();
  await panel().findByText(/运行器目录暂不可用/); catalogue.mockResolvedValueOnce(page());
  fireEvent.click(panel().getByRole("button", { name: "重试运行器目录" })); await panel().findByText("未检测到运行器条目。");
  await draft(); const delayed = deferred<Response>(); writes.mockReturnValueOnce(delayed.promise); submit();
  const signal = writes.mock.calls[0][1]?.signal;
  // Full app unmount differs from an in-page group switch.
  cleanup();
  expect(signal?.aborted).toBe(true); await act(async () => delayed.resolve(json(member, 201)));
});
