import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Tasks } from "./Tasks";
import { deferred, group, json, other, page } from "./test/groups";

const url = (groupId = group.id) => `/api/v1/groups/${groupId}/coordination/discussion-policy`;
const policy = (revision = 0, hops = 2, groupId = group.id) => ({ group_id: groupId, topic_id: "", revision,
  max_invite_hops: hops, max_targeted_wakes: 2 * hops });
const rejection = (status: number, code: string) => json({ error: { code, message: "请求被拒绝" } }, status);
const reads = vi.fn<typeof fetch>(), writes = vi.fn<typeof fetch>(), messages = vi.fn<typeof fetch>();
const flush = () => act(async () => {});
const current = () => screen.getByRole("region", { name: "当前讨论策略" });
const form = () => screen.getByRole("form", { name: "保存讨论策略" });
const limit = () => within(form()).getByRole("combobox", { name: "新结构化讨论链的邀请上限" });
const choose = (hops: number) => fireEvent.change(limit(), { target: { value: String(hops) } });
const submit = () => fireEvent.submit(form());
const attempt = (index: number) => {
  const [path, init] = writes.mock.calls[index];
  return { path, body: String(init?.body), key: new Headers(init?.headers).get("Idempotency-Key") };
};
async function expandWork() {
  fireEvent.click(await screen.findByRole("button", { name: "展开自然聊天工作配置" }));
  await flush();
}
async function expandDiscussion() {
  fireEvent.click(screen.getByRole("button", { name: "展开讨论策略" }));
  await flush();
}

beforeEach(() => {
  reads.mockReset().mockImplementation(async path => json(policy(0, 2, decodeURIComponent(String(path).split("/")[4]))));
  writes.mockReset(); messages.mockReset();
  vi.stubGlobal("fetch", (path: RequestInfo | URL, init?: RequestInit) => {
    if (String(path).endsWith("/discussion-policy")) return (init?.method === "PUT" ? writes : reads)(path, init);
    if (String(path).endsWith("/chat-policy")) return Promise.resolve(rejection(404, "not_found"));
    if (String(path).includes("/messages")) return messages(path, init);
    return Promise.resolve(page());
  });
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it("lazily edits the main-topic discussion budget without touching work policy or history", async () => {
  const view = render(<Tasks group={group} />);
  await expandWork(); expect(reads).not.toHaveBeenCalled();
  await expandDiscussion(); expect(reads).toHaveBeenCalledTimes(1); expect(reads.mock.calls[0][0]).toBe(url());
  expect(current()).toHaveTextContent(/版本 0（默认）.*新结构化讨论链最多 2 次邀请.*4 次定向唤醒/);
  expect(screen.getByText(/仅本群主话题，不影响独立话题/)).toBeVisible();
  expect(screen.getByText(/执行入口提供结构化讨论交接工具时.*0、1 或 2 次邀请上限.*每次邀请预留一次返回唤醒/)).toBeVisible();
  expect(screen.getByText(/普通成员工具仍按现有规则运行.*设为 0 时.*新的定向发送.*会唤醒新成员的自动增员/)).toBeVisible();
  choose(0);
  writes.mockResolvedValueOnce(json(policy(1, 0))); reads.mockResolvedValueOnce(json(policy(1, 0)));
  submit(); await flush();
  expect(JSON.parse(attempt(0).body)).toEqual({ expected_revision: 0, max_invite_hops: 0 });
  expect(attempt(0).path).toBe(url()); expect(attempt(0).key).toBeTruthy();
  expect(current()).toHaveTextContent(/版本 1.*0 次邀请.*0 次定向唤醒/);
  expect(messages).not.toHaveBeenCalled();
  const pending = deferred<Response>(); reads.mockReturnValueOnce(pending.promise);
  fireEvent.click(screen.getByRole("button", { name: "重新读取讨论策略" }));
  view.rerender(<Tasks group={other} />);
  expect(reads.mock.calls.at(-1)?.[1]?.signal?.aborted).toBe(true);
  await act(async () => pending.resolve(json(policy(42, 1))));
  expect(reads).toHaveBeenCalledTimes(3);
});

it("pins the original attempt across a foreign receipt, a group switch, and a newer read", async () => {
  const view = render(<Tasks group={group} />); await expandWork(); await expandDiscussion();
  choose(1);
  const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise); submit();
  const original = attempt(0);
  view.rerender(<Tasks group={other} />); await expandWork(); await expandDiscussion(); choose(0);
  await act(async () => pending.resolve(json(policy(1, 1, other.id))));
  expect(limit()).toHaveValue("0"); expect(limit()).toBeEnabled();
  reads.mockResolvedValueOnce(json(policy(4, 0)));
  view.rerender(<Tasks group={group} />); await flush();
  expect(current()).toHaveTextContent("版本 4"); expect(limit()).toHaveValue("1"); expect(limit()).toBeDisabled();
  expect(screen.getByRole("button", { name: "重试同一次讨论策略保存" })).toBeEnabled();
  reads.mockResolvedValueOnce(json(policy(4, 0))); writes.mockResolvedValueOnce(rejection(409, "conflict")); submit(); await flush();
  expect(attempt(1)).toEqual(original); expect(limit()).toBeDisabled();
  reads.mockResolvedValueOnce(json(policy(4, 0))); writes.mockResolvedValueOnce(json(policy(1, 1))); submit(); await flush();
  expect(attempt(2)).toEqual(original);
  expect(current()).toHaveTextContent("版本 4"); expect(limit()).toHaveValue("0"); expect(limit()).toBeEnabled();
});

it("requires a scoped read before saving; after a definitive conflict it rereads and preserves the draft", async () => {
  render(<Tasks group={group} />); await expandWork();
  reads.mockResolvedValueOnce(new Response("Not Found", { status: 404 })); await expandDiscussion();
  expect(current()).toHaveTextContent("不可用"); expect(limit()).toBeDisabled(); submit(); expect(writes).not.toHaveBeenCalled();
  const reload = () => fireEvent.click(screen.getByRole("button", { name: "重新读取讨论策略" }));
  reads.mockResolvedValueOnce(json(policy(6, 0, other.id))); reload(); await flush();
  expect(limit()).toBeDisabled(); submit(); expect(writes).not.toHaveBeenCalled();
  reload(); await flush(); choose(0);
  writes.mockResolvedValueOnce(rejection(409, "conflict")); reads.mockResolvedValueOnce(json(policy(3, 1)));
  submit(); await flush();
  expect(current()).toHaveTextContent("版本 3"); expect(limit()).toHaveValue("0"); expect(writes).toHaveBeenCalledTimes(1);
  writes.mockResolvedValueOnce(json(policy(4, 0))); reads.mockResolvedValueOnce(json(policy(4, 0)));
  submit(); await flush();
  expect(JSON.parse(attempt(1).body)).toEqual({ expected_revision: 3, max_invite_hops: 0 });
  expect(attempt(1).key).not.toBe(attempt(0).key);
});
