import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { WorkspaceGrantSettings } from "./WorkspaceGrantSettings";
import { deferred, group, json, other } from "./test/groups";

const memberId = "worker-1", root = "/existing/project ";
const grant = { id: "grant-1", group_id: group.id, member_id: memberId, root, resolved_root: "/physical/project",
  resource_key: "workspace:device:inode", scope: "read_write", node_id: "local", authorized_by: "user", created_at: "2026-11-17T12:00:00Z" };
const reads = vi.fn<typeof fetch>(), writes = vi.fn<typeof fetch>(), transport = vi.fn<typeof fetch>();
const saved = new Map<string, unknown>();
const path = (groupId = group.id, member = memberId) => `/api/v1/groups/${groupId}/members/${member}/workspace-grant`;
const absent = () => json({ error: { code: "not_found", message: "没有目录授权" } }, 404);
const flush = () => act(async () => {});
const form = () => within(screen.getByRole("form", { name: "保存目录授权" }));
const draft = (value = root, scope = "read_write") => {
  fireEvent.change(form().getByRole("textbox", { name: "已有目录的绝对路径" }), { target: { value } });
  fireEvent.change(form().getByRole("combobox", { name: "目录授权范围" }), { target: { value: scope } });
};
const submit = () => fireEvent.submit(screen.getByRole("form", { name: "保存目录授权" }));
const attempt = (index: number) => {
  const [url, init] = writes.mock.calls[index]; return { url, body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key") };
};
async function open() {
  const view = render(<WorkspaceGrantSettings groupId={group.id} memberId={memberId} active />);
  fireEvent.click(screen.getByRole("button", { name: "展开目录授权" })); await flush(); return view;
}
beforeEach(() => {
  saved.clear();
  reads.mockReset().mockImplementation(async url => saved.has(String(url)) ? json(saved.get(String(url))) : absent());
  writes.mockReset().mockImplementation(async (url, init) => {
    const input = JSON.parse(String(init?.body)), parts = String(url).split("/");
    const actual = { ...grant, ...input, group_id: decodeURIComponent(parts[4]), member_id: decodeURIComponent(parts[6]) };
    saved.set(String(url), actual); return json(actual, 201);
  });
  transport.mockReset().mockImplementation((url, init) => (init?.method === "POST" ? writes : reads)(url, init)); vi.stubGlobal("fetch", transport);
});
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });

it("reads only the selected stable member when expanded and active, never another catalogue or a model", async () => {
  const view = render(<WorkspaceGrantSettings groupId={group.id} memberId={memberId} active={false} />); await flush(); expect(reads).not.toHaveBeenCalled();
  view.rerender(<WorkspaceGrantSettings groupId={group.id} memberId={memberId} active />);
  fireEvent.click(await screen.findByRole("button", { name: "展开目录授权" })); await flush();
  expect(reads.mock.calls[0][0]).toBe(path()); expect(transport).toHaveBeenCalledTimes(1);
  expect(form().getByRole("textbox", { name: "已有目录的绝对路径" })).toHaveValue("");
  expect(form().getByRole("combobox", { name: "目录授权范围" })).toHaveValue("");
  expect(screen.getByText(/成员 ID/)).toHaveTextContent(memberId);
});
it("explains bounded permissions before one explicit directory authorization, without extra task or danger forms", async () => {
  await open();
  expect(screen.getByText(/保存读写授权后/)).toHaveTextContent(/有界文本文件.*不会授予.*shell.*个人配置.*全局权限/);
  expect(screen.getByText(/一次目录授权.*不是每个任务/)).toBeVisible();
  draft(); submit(); await flush();
  expect(JSON.parse(String(attempt(0).body))).toEqual({ root, scope: "read_write" });
  const receipt = screen.getByRole("region", { name: "已确认目录授权回执" });
  for (const value of [root, grant.resolved_root, grant.resource_key, grant.id, memberId, group.id]) expect(receipt).toHaveTextContent(value);
  expect(receipt).toHaveTextContent("读写范围"); expect(receipt).toHaveTextContent("不代表当前可执行");
  expect(screen.queryByRole("form", { name: "保存目录授权" })).not.toBeInTheDocument();
  expect(writes).toHaveBeenCalledTimes(1);
});
it("shows an immutable read-only receipt and offers no silent upgrade, reroot, revoke or member creation", async () => {
  saved.set(path(), { ...grant, scope: "read_only" }); await open();
  const receipt = screen.getByRole("region", { name: "已确认目录授权回执" }); expect(receipt).toHaveTextContent("只读范围");
  expect(screen.getByText(/不能升级.*不能改配.*另一目录/)).toBeVisible();
  expect(screen.getByText(/已有.*未授权.*工作成员.*添加成员入口/)).toBeVisible();
  expect(screen.queryByRole("form", { name: "保存目录授权" })).not.toBeInTheDocument(); expect(writes).not.toHaveBeenCalled();
});
it("freezes an unknown operation across collapse, group/member switches and a conflict retry", async () => {
  const view = await open(); draft(); const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise); submit(); const original = attempt(0);
  fireEvent.click(screen.getByRole("button", { name: "收起目录授权" }));
  view.rerender(<WorkspaceGrantSettings groupId={other.id} memberId="worker-2" active />);
  fireEvent.click(await screen.findByRole("button", { name: "展开目录授权" })); await flush(); draft("/other/draft", "read_only");
  expect(writes.mock.calls[0][1]?.signal?.aborted ?? false).toBe(false); await act(async () => pending.resolve(json({}, 503)));
  view.rerender(<WorkspaceGrantSettings groupId={group.id} memberId={memberId} active />);
  fireEvent.click(screen.getByRole("button", { name: "展开目录授权" })); await flush();
  expect(form().getByRole("textbox")).toHaveValue(root); expect(form().getByRole("textbox")).toBeDisabled();
  writes.mockResolvedValueOnce(json({ error: { code: "conflict", message: "冲突" } }, 409)); submit(); await flush();
  expect(attempt(1)).toEqual(original); expect(form().getByRole("button", { name: "重试同一次目录授权" })).toBeEnabled();
  submit(); await flush(); expect(attempt(2)).toEqual(original);
  view.rerender(<WorkspaceGrantSettings groupId={other.id} memberId="worker-2" active />);
  expect(form().getByRole("textbox")).toHaveValue("/other/draft");
});
it("keeps a completed POST with its original member while a different member is visible", async () => {
  const view = await open(); draft(); const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise); submit();
  view.rerender(<WorkspaceGrantSettings groupId={group.id} memberId="worker-2" active />);
  fireEvent.click(await screen.findByRole("button", { name: "展开目录授权" })); await flush();
  await act(async () => pending.resolve(json(grant, 201)));
  expect(screen.queryByRole("region", { name: "已确认目录授权回执" })).not.toBeInTheDocument();
  saved.set(path(), grant); view.rerender(<WorkspaceGrantSettings groupId={group.id} memberId={memberId} active />); await flush();
  expect(screen.getByRole("region", { name: "已确认目录授权回执" })).toHaveTextContent(grant.id);
});
it("never replaces an unknown command using a read-back receipt", async () => {
  await open(); draft(); writes.mockRejectedValueOnce(new TypeError("Lost receipt")); submit(); await flush(); const original = attempt(0);
  saved.set(path(), grant); fireEvent.click(screen.getByRole("button", { name: "读取目录授权并保留原操作" })); await flush();
  expect(screen.getByRole("region", { name: "已确认目录授权回执" })).toHaveTextContent(grant.id);
  expect(form().getByRole("button", { name: "重试同一次目录授权" })).toBeEnabled(); submit(); await flush(); expect(attempt(1)).toEqual(original);
});
it.each([null, { ...grant, id: "contradictory-grant" }, { ...grant, root: "/different/root" }])("does not roll back immutable grant evidence on a contradictory read %j", async actual => {
  saved.set(path(), grant); await open(); if (actual) saved.set(path(), actual); else saved.delete(path());
  fireEvent.click(screen.getByRole("button", { name: "读取目录授权并保留原操作" })); await flush();
  expect(screen.getByRole("alert")).toHaveTextContent(/不一致|消失/);
  expect(screen.getByRole("region", { name: "已确认目录授权回执" })).toHaveTextContent(grant.id);
  expect(screen.queryByRole("form", { name: "保存目录授权" })).not.toBeInTheDocument(); expect(writes).not.toHaveBeenCalled();
});
it("rereads an initial conflict to show the actual immutable read-only authorization", async () => {
  await open(); draft(); saved.set(path(), { ...grant, scope: "read_only" });
  writes.mockResolvedValueOnce(json({ error: { code: "conflict", message: "已有授权" } }, 409)); submit(); await flush();
  expect(screen.getByRole("region", { name: "已确认目录授权回执" })).toHaveTextContent("只读范围");
  expect(screen.queryByRole("form", { name: "保存目录授权" })).not.toBeInTheDocument(); expect(writes).toHaveBeenCalledTimes(1);
});
it("does not treat malformed HTTP success as permission or a saved authorization", async () => {
  await open(); draft(); writes.mockResolvedValueOnce(json({ ...grant, member_id: "other-member" }, 201)); submit(); await flush();
  expect(screen.getByRole("alert")).toHaveTextContent("未确认");
  expect(screen.queryByRole("region", { name: "已确认目录授权回执" })).not.toBeInTheDocument();
  expect(form().getByRole("button", { name: "重试同一次目录授权" })).toBeEnabled();
});
it("aborts hidden GETs, ignores their late receipts, and does not poll a low-frequency grant", async () => {
  vi.useFakeTimers(); const pending = deferred<Response>(); reads.mockReturnValueOnce(pending.promise);
  await open(); const signal = reads.mock.calls[0][1]?.signal; fireEvent.click(screen.getByRole("button", { name: "收起目录授权" }));
  expect(signal?.aborted).toBe(true); await act(async () => pending.resolve(json(grant)));
  fireEvent.click(screen.getByRole("button", { name: "展开目录授权" })); await flush();
  expect(screen.queryByRole("region", { name: "已确认目录授权回执" })).not.toBeInTheDocument();
  await act(async () => { await vi.advanceTimersByTimeAsync(20000); }); expect(reads).toHaveBeenCalledTimes(2);
});
it("does not grant when a missing route is indistinguishable from unavailable server support", async () => {
  reads.mockResolvedValueOnce(new Response("404 page not found\n", { status: 404 })); await open();
  expect(screen.getByRole("alert")).toHaveTextContent("未确认"); expect(form().getByRole("button", { name: "保存目录授权" })).toBeDisabled();
  submit(); expect(writes).not.toHaveBeenCalled();
});
