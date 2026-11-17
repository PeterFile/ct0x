import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Tasks } from "./Tasks";
import { deferred, group, json, other, page } from "./test/groups";

const blank = (groupId = group.id) => ({ group_id: groupId, topic_id: "", enabled: false, allow_preparation: false,
  preauthorization_message_id: "", action: "", max_admissions: 0, revision: 0, authorized_by: "",
  preparation_supported: false, configuration_source_id: "" });
const legacy = { group_id: group.id, topic_id: "", enabled: true, allow_preparation: true,
  preauthorization_message_id: "old-user-message", preauthorization_text: "旧自由文字只允许某个更窄目标，不是话题摘要许可",
  action: "text_submission", max_admissions: 3, revision: 16, authorized_by: "user" };
const policyReads = vi.fn<typeof fetch>(), messages = vi.fn<typeof fetch>(), writes = vi.fn<typeof fetch>();
const saved = new Map<string, unknown>();
const policyURL = (id = group.id) => `/api/v1/groups/${id}/coordination/chat-policy`;
const flush = () => act(async () => {});
const form = () => within(screen.getByRole("form", { name: "保存聊天策略" }));
const check = (name: string) => fireEvent.click(form().getByRole("checkbox", { name }));
const budget = (value: string) => fireEvent.change(form().getByRole("spinbutton", { name: "本版本累计任务上限" }), { target: { value } });
const chooseScope = () => fireEvent.change(form().getByRole("combobox", { name: "准备工作范围" }), { target: { value: "topic_summary" } });
const submit = () => fireEvent.submit(screen.getByRole("form", { name: "保存聊天策略" }));
const attempt = (index: number) => {
  const [url, init] = writes.mock.calls[index];
  return { url, body: String(init?.body), key: new Headers(init?.headers).get("Idempotency-Key") };
};
const refusal = (status: number) => json({ error: { code: status === 409 ? "conflict" : "not_found", message: "请求被拒绝" } }, status);
function receipt(body: string, groupId = group.id) {
  const input = JSON.parse(body);
  return { group_id: groupId, topic_id: "", enabled: input.enabled, allow_preparation: input.allow_preparation,
    action: input.action, max_admissions: input.max_admissions, revision: input.expected_revision + 1, authorized_by: "user",
    preparation_scope_kind: input.preparation_scope_kind ?? "", preparation_supported: input.allow_preparation,
    configuration_source_id: "actual-policy-command", preauthorization_message_id: input.allow_preparation ? "generated-rule-message" : "",
    ...(input.allow_preparation ? { preauthorization_text: "本群主话题已有消息的纯文本摘要；目标与验收由服务端固定" } : {}) };
}
async function open() {
  const view = render(<Tasks group={group} />);
  fireEvent.click(await screen.findByRole("button", { name: "展开自然聊天工作配置" })); await flush(); return view;
}
beforeEach(() => {
  saved.clear(); policyReads.mockReset().mockImplementation(async url => json(saved.get(String(url)) ?? blank(decodeURIComponent(String(url).split("/")[4]))));
  messages.mockReset().mockImplementation(async () => page());
  writes.mockReset().mockImplementation(async (url, init) => {
    const value = receipt(String(init?.body), decodeURIComponent(String(url).split("/")[4])); saved.set(String(url), value); return json(value);
  });
  vi.stubGlobal("fetch", (url: RequestInfo | URL, init?: RequestInit) => String(url).includes("/chat-policy")
    ? (init?.method === "PUT" ? writes : policyReads)(url, init)
    : String(url).includes("/messages") ? messages(url, init) : Promise.resolve(page()));
});
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });

it("reads only while active and expanded, and enables an explicit-request revision without a preauthorization message", async () => {
  const view = render(<Tasks group={group} />); await flush(); expect(policyReads).not.toHaveBeenCalled();
  fireEvent.click(await screen.findByRole("button", { name: "展开自然聊天工作配置" })); await flush();
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent(/版本 0.*未启用/);
  check("启用自然聊天工作"); budget("3"); submit(); await flush();
  expect(JSON.parse(attempt(0).body)).toEqual({ enabled: true, allow_preparation: false,
    action: "text_submission", max_admissions: 3, expected_revision: 0 });
  expect(attempt(0).url).toBe(policyURL()); expect(messages).not.toHaveBeenCalled();
  expect(screen.getByText(/仅本群主话题.*文本提交/)).toBeVisible(); expect(screen.getByText(/按策略版本累计/)).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "收起自然聊天工作配置" }));
  const count = policyReads.mock.calls.length; view.rerender(<Tasks group={null} />); await flush(); expect(policyReads).toHaveBeenCalledTimes(count);
});
it("requires an explicit topic-summary scope and displays the server-generated rule and command IDs read-only", async () => {
  await open(); check("启用自然聊天工作"); check("允许受限准备工作");
  expect(form().queryByRole("combobox", { name: "书面授权来源" })).not.toBeInTheDocument();
  submit(); expect(writes).not.toHaveBeenCalled(); chooseScope(); submit(); await flush();
  expect(JSON.parse(attempt(0).body)).toEqual({ enabled: true, allow_preparation: true, preparation_scope_kind: "topic_summary",
    action: "text_submission", max_admissions: 1, expected_revision: 0 });
  const current = screen.getByRole("region", { name: "当前聊天策略" });
  expect(current).toHaveTextContent("generated-rule-message"); expect(current).toHaveTextContent("actual-policy-command");
  expect(current).toHaveTextContent("topic_summary"); expect(current).toHaveTextContent("目标与验收由服务端固定");
  expect(screen.getByText(/8192.*UTF-8/)).toHaveTextContent(/不代表.*语义质量.*用户接纳/);
  expect(messages).not.toHaveBeenCalled(); expect(form().queryByRole("textbox", { name: /来源|规则|目标|验收/ })).not.toBeInTheDocument();
});
it("keeps legacy free-text rules inspectable without silently upgrading them to topic_summary", async () => {
  saved.set(policyURL(), { ...legacy, preparation_supported: false, configuration_source_id: "" }); await open();
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent(legacy.preauthorization_text);
  expect(screen.getByText(/历史准备规则.*不支持.*不会自动升级/)).toBeVisible();
  expect(form().getByRole("combobox", { name: "准备工作范围" })).toHaveValue(""); budget("4"); submit(); expect(writes).not.toHaveBeenCalled();
  chooseScope(); submit(); await flush();
  expect(JSON.parse(attempt(0).body)).toEqual({ enabled: true, allow_preparation: true, preparation_scope_kind: "topic_summary",
    action: "text_submission", max_admissions: 4, expected_revision: 16 }); expect(attempt(0).body).not.toContain(legacy.preauthorization_message_id);
});
it("distinguishes an old server lacking scope metadata from a supported zero configuration", async () => {
  saved.set(policyURL(), legacy); await open();
  expect(screen.getByText(/旧实例.*范围.*来源.*未确认/)).toBeVisible();
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent("历史启用标记");
  expect(form().getByRole("button", { name: "保存聊天策略" })).toBeDisabled(); submit(); expect(writes).not.toHaveBeenCalled();
});
it.each([404, 409])("freezes unknown PUT identity across collapse, group switches, newer reads and a rejected %s retry", async status => {
  const view = await open(); check("启用自然聊天工作"); budget("2");
  const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise); submit(); const original = attempt(0);
  fireEvent.click(screen.getByRole("button", { name: "收起自然聊天工作配置" }));
  view.rerender(<Tasks group={other} />); fireEvent.click(await screen.findByRole("button", { name: "展开自然聊天工作配置" })); await flush(); budget("7");
  expect(writes.mock.calls[0][1]?.signal?.aborted ?? false).toBe(false); await act(async () => pending.resolve(json({}, 503)));
  saved.set(policyURL(), { ...receipt(original.body), enabled: false, max_admissions: 9, revision: 8 });
  view.rerender(<Tasks group={group} />); fireEvent.click(screen.getByRole("button", { name: "展开自然聊天工作配置" })); await flush();
  expect(form().getByRole("spinbutton")).toHaveValue(2); expect(form().getByRole("spinbutton")).toBeDisabled();
  const readsBeforeRetry = policyReads.mock.calls.length; writes.mockResolvedValueOnce(refusal(status)); submit(); await flush();
  if (status === 409) expect(policyReads).toHaveBeenCalledTimes(readsBeforeRetry + 1);
  expect(form().getByRole("button", { name: "重试同一次聊天策略保存" })).toBeEnabled();
  writes.mockResolvedValueOnce(json(receipt(original.body))); submit(); await flush(); expect(attempt(1)).toEqual(original); expect(attempt(2)).toEqual(original);
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent("版本 8");
  view.rerender(<Tasks group={other} />); expect(form().getByRole("spinbutton")).toHaveValue(7);
});
it("keeps an unknown preparation scope and its original revision frozen while reading a newer non-preparation policy", async () => {
  await open(); check("启用自然聊天工作"); check("允许受限准备工作"); chooseScope();
  writes.mockRejectedValueOnce(new TypeError("Lost receipt")); submit(); await flush(); const original = attempt(0);
  saved.set(policyURL(), { ...receipt(original.body), allow_preparation: false, preparation_scope_kind: "", preparation_supported: false,
    preauthorization_message_id: "", preauthorization_text: "", revision: 8 });
  fireEvent.click(screen.getByRole("button", { name: "读取当前聊天策略并保留草稿" })); await flush();
  expect(form().getByRole("combobox", { name: "准备工作范围" })).toHaveValue("topic_summary"); expect(form().getByRole("combobox")).toBeDisabled();
  writes.mockResolvedValueOnce(json(receipt(original.body))); submit(); await flush(); expect(attempt(1)).toEqual(original);
});
it("reads the real current revision after conflict, preserves the draft and only writes again after a user action", async () => {
  await open(); check("启用自然聊天工作"); budget("4");
  saved.set(policyURL(), { ...receipt(JSON.stringify({ enabled: false, allow_preparation: false, action: "text_submission", max_admissions: 8, expected_revision: 4 })) });
  writes.mockResolvedValueOnce(refusal(409)); submit(); await flush(); expect(policyReads).toHaveBeenCalledTimes(2); expect(writes).toHaveBeenCalledTimes(1);
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent("版本 5"); expect(form().getByRole("spinbutton")).toHaveValue(4);
  submit(); await flush(); expect(JSON.parse(attempt(1).body).expected_revision).toBe(5); expect(attempt(1).key).not.toBe(attempt(0).key);
});
it("treats GET 404 as unavailable rather than enabled or a writable zero revision", async () => {
  policyReads.mockResolvedValueOnce(refusal(404)); await open(); expect(screen.getByText(/当前实例.*不可用/)).toBeVisible();
  expect(form().getByRole("button", { name: "保存聊天策略" })).toBeDisabled(); submit(); expect(writes).not.toHaveBeenCalled();
});
it("aborts a collapsed policy GET, ignores its late result, and never polls this low-frequency setting", async () => {
  vi.useFakeTimers(); const pending = deferred<Response>(); policyReads.mockReturnValueOnce(pending.promise);
  const view = render(<Tasks group={group} />); await flush(); fireEvent.click(screen.getByRole("button", { name: "展开自然聊天工作配置" })); await flush();
  const signal = policyReads.mock.calls[0][1]?.signal; fireEvent.click(screen.getByRole("button", { name: "收起自然聊天工作配置" })); expect(signal?.aborted).toBe(true);
  await act(async () => pending.resolve(json(receipt(JSON.stringify({ enabled: true, allow_preparation: false, action: "text_submission", max_admissions: 1, expected_revision: 0 })))));
  await act(async () => { await vi.advanceTimersByTimeAsync(20000); }); expect(policyReads).toHaveBeenCalledTimes(1);
  fireEvent.click(screen.getByRole("button", { name: "展开自然聊天工作配置" })); await flush(); expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent(/版本 0.*未启用/);
  await act(async () => { await vi.advanceTimersByTimeAsync(20000); }); expect(policyReads).toHaveBeenCalledTimes(2);
  view.rerender(<Tasks group={null} />); await act(async () => { await vi.advanceTimersByTimeAsync(20000); }); expect(policyReads).toHaveBeenCalledTimes(2);
});
it.each(["revision", "source"])("keeps a mismatched %s PUT receipt unknown", async field => {
  await open(); check("启用自然聊天工作"); writes.mockImplementationOnce(async (_url, init) => json({ ...receipt(String(init?.body)),
    ...(field === "revision" ? { revision: 2 } : { preauthorization_message_id: "wrong-read-only-rule", preauthorization_text: "不属于本次无准备配置" }) }));
  submit(); await flush(); expect(form().getByRole("button", { name: "重试同一次聊天策略保存" })).toBeEnabled();
  expect(form().getByRole("checkbox", { name: "启用自然聊天工作" })).toBeDisabled();
});
