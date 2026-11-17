import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Tasks } from "./Tasks";
import { deferred, group, json, other, page } from "./test/groups";
import { message } from "./test/messages";

const blank = (groupId = group.id) => ({ group_id: groupId, topic_id: "", enabled: false, allow_preparation: false,
  preauthorization_message_id: "", action: "", max_admissions: 0, revision: 0, authorized_by: "" });
const source = { ...message, id: "written-user-a", content: "允许在本群预算内准备文本成果", deliveries: [] };
const second = { ...source, id: "written-user-b", created_at: "2026-11-17T08:01:00Z" };
const policyReads = vi.fn<typeof fetch>(), messages = vi.fn<typeof fetch>(), writes = vi.fn<typeof fetch>();
const saved = new Map<string, ReturnType<typeof blank>>();
const policyURL = (id = group.id) => `/api/v1/groups/${id}/coordination/chat-policy`;
const flush = () => act(async () => {});
const form = () => within(screen.getByRole("form", { name: "保存聊天策略" }));
const check = (name: string) => fireEvent.click(form().getByRole("checkbox", { name }));
const budget = (value: string) => fireEvent.change(form().getByRole("spinbutton", { name: "本版本累计任务上限" }), { target: { value } });
const submit = () => fireEvent.submit(screen.getByRole("form", { name: "保存聊天策略" }));
const attempt = (index: number) => {
  const [url, init] = writes.mock.calls[index];
  return { url, body: String(init?.body), key: new Headers(init?.headers).get("Idempotency-Key") };
};
const refusal = (status: number) => json({ error: { code: status === 409 ? "conflict" : "not_found", message: "请求被拒绝" } }, status);
function receipt(body: string, groupId = group.id) {
  const { expected_revision, ...input } = JSON.parse(body);
  return { group_id: groupId, topic_id: "", ...input, revision: expected_revision + 1, authorized_by: "user",
    ...(input.preauthorization_message_id ? { preauthorization_text: source.content } : {}) };
}
async function open() {
  const view = render(<Tasks group={group} />);
  fireEvent.click(await screen.findByRole("button", { name: "展开自然聊天工作配置" })); await flush(); return view;
}
beforeEach(() => {
  saved.clear(); policyReads.mockReset().mockImplementation(async url => json(saved.get(String(url)) ?? blank(decodeURIComponent(String(url).split("/")[4]))));
  messages.mockReset().mockImplementation(async url => /\/messages\//.test(String(url)) ? json(second) : page([
    source, second, { ...source, id: "agent", sender_id: "agent-1", content: "Agent 不能代为授权" },
    { ...source, id: "topic-user", topic_id: "another-topic", content: "其他话题授权" },
  ]));
  writes.mockReset().mockImplementation(async (url, init) => {
    const value = receipt(String(init?.body), decodeURIComponent(String(url).split("/")[4])); saved.set(String(url), value); return json(value);
  });
  vi.stubGlobal("fetch", (url: RequestInfo | URL, init?: RequestInit) => String(url).includes("/chat-policy")
    ? (init?.method === "PUT" ? writes : policyReads)(url, init)
    : String(url).includes("/messages") ? messages(url, init) : Promise.resolve(page()));
});
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });

it("reads only while active and expanded, and enables the exact text-only revision without preauthorization for explicit requests", async () => {
  const view = render(<Tasks group={group} />); await flush(); expect(policyReads).not.toHaveBeenCalled();
  fireEvent.click(await screen.findByRole("button", { name: "展开自然聊天工作配置" })); await flush();
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent(/版本 0.*未启用/);
  check("启用自然聊天工作"); budget("3"); submit(); await flush();
  expect(JSON.parse(attempt(0).body)).toEqual({ enabled: true, allow_preparation: false, preauthorization_message_id: "",
    action: "text_submission", max_admissions: 3, expected_revision: 0 });
  expect(attempt(0).url).toBe(policyURL()); expect(messages).not.toHaveBeenCalled();
  expect(screen.getByText(/仅本群主话题.*文本提交/)).toBeVisible();
  expect(screen.getByText(/按策略版本累计/)).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "收起自然聊天工作配置" }));
  const count = policyReads.mock.calls.length; view.rerender(<Tasks group={null} />); await flush(); expect(policyReads).toHaveBeenCalledTimes(count);
});
it("selects real main-topic user messages by stable ID with content and time, never Agent or another topic", async () => {
  await open(); check("启用自然聊天工作"); check("允许按书面预授权开展准备工作"); await flush();
  const picker = form().getByRole("combobox", { name: "书面授权来源" });
  expect(within(picker).getAllByRole("option")).toHaveLength(3);
  expect(picker).not.toHaveTextContent(/Agent 不能代为授权|其他话题授权/);
  submit(); expect(writes).not.toHaveBeenCalled();
  fireEvent.change(picker, { target: { value: second.id } }); await flush();
  const preview = screen.getByRole("region", { name: "授权来源预览" }); expect(preview).toHaveTextContent(source.content);
  expect(preview.querySelector("time")).toHaveAttribute("datetime", second.created_at);
  submit(); await flush(); expect(JSON.parse(attempt(0).body).preauthorization_message_id).toBe(second.id);
});
it.each([404, 409])("freezes unknown PUT identity across collapse, group switches, newer reads and a rejected %s retry", async status => {
  const view = await open(); check("启用自然聊天工作"); budget("2");
  const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise); submit(); const original = attempt(0);
  fireEvent.click(screen.getByRole("button", { name: "收起自然聊天工作配置" }));
  view.rerender(<Tasks group={other} />); fireEvent.click(await screen.findByRole("button", { name: "展开自然聊天工作配置" })); await flush(); budget("7");
  expect(writes.mock.calls[0][1]?.signal?.aborted ?? false).toBe(false);
  await act(async () => pending.resolve(json({}, 503)));
  saved.set(policyURL(), { ...blank(), action: "text_submission", max_admissions: 9, revision: 8, authorized_by: "user" });
  view.rerender(<Tasks group={group} />); fireEvent.click(screen.getByRole("button", { name: "展开自然聊天工作配置" })); await flush();
  expect(form().getByRole("spinbutton")).toHaveValue(2); expect(form().getByRole("spinbutton")).toBeDisabled();
  const readsBeforeRetry = policyReads.mock.calls.length;
  writes.mockResolvedValueOnce(refusal(status)); submit(); await flush();
  if (status === 409) expect(policyReads).toHaveBeenCalledTimes(readsBeforeRetry + 1);
  expect(form().getByRole("button", { name: "重试同一次聊天策略保存" })).toBeEnabled();
  writes.mockResolvedValueOnce(json(receipt(original.body))); submit(); await flush();
  expect(attempt(1)).toEqual(original); expect(attempt(2)).toEqual(original);
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent("版本 8");
  view.rerender(<Tasks group={other} />); expect(form().getByRole("spinbutton")).toHaveValue(7);
});
it("reads the real current revision after conflict, preserves the draft and only writes again after a user action", async () => {
  await open(); check("启用自然聊天工作"); budget("4");
  saved.set(policyURL(), { ...blank(), action: "text_submission", max_admissions: 8, revision: 5, authorized_by: "user" });
  writes.mockResolvedValueOnce(refusal(409)); submit(); await flush();
  expect(policyReads).toHaveBeenCalledTimes(2); expect(writes).toHaveBeenCalledTimes(1);
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent("版本 5"); expect(form().getByRole("spinbutton")).toHaveValue(4);
  submit(); await flush(); expect(JSON.parse(attempt(1).body).expected_revision).toBe(5); expect(attempt(1).key).not.toBe(attempt(0).key);
});
it("treats GET 404 as unavailable rather than enabled or a writable zero revision", async () => {
  policyReads.mockResolvedValueOnce(refusal(404)); await open();
  expect(screen.getByText(/当前实例.*不可用/)).toBeVisible();
  expect(form().getByRole("button", { name: "保存聊天策略" })).toBeDisabled(); submit(); expect(writes).not.toHaveBeenCalled();
});
it("aborts a collapsed policy GET, ignores its late result, and never polls this low-frequency setting", async () => {
  vi.useFakeTimers(); const pending = deferred<Response>(); policyReads.mockReturnValueOnce(pending.promise);
  const view = render(<Tasks group={group} />); await flush();
  fireEvent.click(screen.getByRole("button", { name: "展开自然聊天工作配置" })); await flush();
  const signal = policyReads.mock.calls[0][1]?.signal;
  fireEvent.click(screen.getByRole("button", { name: "收起自然聊天工作配置" })); expect(signal?.aborted).toBe(true);
  await act(async () => pending.resolve(json({ ...blank(), enabled: true, action: "text_submission", max_admissions: 1, revision: 1, authorized_by: "user" })));
  await act(async () => { await vi.advanceTimersByTimeAsync(20000); }); expect(policyReads).toHaveBeenCalledTimes(1);
  fireEvent.click(screen.getByRole("button", { name: "展开自然聊天工作配置" })); await flush();
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent(/版本 0.*未启用/);
  await act(async () => { await vi.advanceTimersByTimeAsync(20000); }); expect(policyReads).toHaveBeenCalledTimes(2);
  view.rerender(<Tasks group={null} />); await act(async () => { await vi.advanceTimersByTimeAsync(20000); }); expect(policyReads).toHaveBeenCalledTimes(2);
});
it.each(["revision", "source"])("keeps a mismatched %s PUT receipt unknown", async field => {
  await open(); check("启用自然聊天工作");
  writes.mockImplementationOnce(async (_url, init) => json({ ...receipt(String(init?.body)),
    ...(field === "revision" ? { revision: 2 } : { preauthorization_message_id: source.id, preauthorization_text: source.content }) }));
  submit(); await flush(); expect(form().getByRole("button", { name: "重试同一次聊天策略保存" })).toBeEnabled();
  expect(form().getByRole("checkbox", { name: "启用自然聊天工作" })).toBeDisabled();
});
