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
    ...(input.allow_preparation ? { preauthorization_text: "本群主话题已有消息的纯文本摘要；目标与验收由服务端固定" } : {}),
    ...(input.investigation ? { investigation: input.investigation } : {}),
    ...(input.automatic_text_dispatch === true ? { automatic_text_dispatch: true } : {}) };
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
it("requires verified workspace capability for the once-per-policy opt-in without blocking legacy text settings", async () => {
  await open();
  expect(form().getByRole("checkbox", { name: "允许明确请求使用已授权写目录" })).toBeDisabled();
  expect(screen.getByText(/目录工作协议能力未确认/)).toBeVisible();
  check("启用自然聊天工作"); submit(); await flush();
  expect(attempt(0).body).not.toContain("allow_workspace_changes"); expect(attempt(0).body).not.toContain("workspace_changes_supported");
});
it("saves an explicit workspace opt-in only with supported metadata and leaves preparation text-only", async () => {
  saved.set(policyURL(), { ...blank(), workspace_changes_supported: true }); await open();
  check("启用自然聊天工作"); check("允许明确请求使用已授权写目录"); check("允许受限准备工作"); chooseScope();
  writes.mockImplementationOnce(async (url, init) => {
    const actual = { ...receipt(String(init?.body)), workspace_changes_supported: true, allow_workspace_changes: true };
    saved.set(String(url), actual); return json(actual);
  });
  submit(); await flush();
  expect(JSON.parse(attempt(0).body)).toEqual({ enabled: true, allow_preparation: true, preparation_scope_kind: "topic_summary",
    action: "text_submission", max_admissions: 1, expected_revision: 0, allow_workspace_changes: true });
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent("允许明确请求使用已授权写目录");
  expect(screen.getByText(/已有同群.*读写授权.*当前已验证.*适配器/)).toBeVisible();
  expect(screen.getByText(/topic_summary 结构限制/)).toHaveTextContent("不允许文件或外部动作");
});
it("keeps an unknown workspace opt-in body while newer reads remove capability", async () => {
  saved.set(policyURL(), { ...blank(), workspace_changes_supported: true }); await open();
  check("启用自然聊天工作"); check("允许明确请求使用已授权写目录"); writes.mockRejectedValueOnce(new TypeError("Receipt lost"));
  submit(); await flush(); const original = attempt(0);
  saved.set(policyURL(), { ...receipt(original.body), revision: 8, workspace_changes_supported: false });
  fireEvent.click(screen.getByRole("button", { name: "读取当前聊天策略并保留草稿" })); await flush();
  expect(form().getByRole("checkbox", { name: "允许明确请求使用已授权写目录" })).toBeChecked();
  writes.mockResolvedValueOnce(refusal(409)); submit(); await flush(); expect(attempt(1)).toEqual(original);
  expect(form().getByRole("button", { name: "重试同一次聊天策略保存" })).toBeEnabled();
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

const workspacePolicy = (revision = 1, allow = true, maxAdmissions = 3) => ({ ...blank(), enabled: true,
  action: "text_submission", max_admissions: maxAdmissions, revision, authorized_by: "user", configuration_source_id: `policy-command-${revision}`,
  workspace_changes_supported: true, ...(allow ? { allow_workspace_changes: true } : {}) });

it("refreshes a confirmed workspace draft after revocation before a budget-only save", async () => {
  saved.set(policyURL(), workspacePolicy()); await open();
  writes.mockImplementation(async (url, init) => {
    const input = JSON.parse(String(init?.body));
    const actual = workspacePolicy(input.expected_revision + 1, input.allow_workspace_changes === true, input.max_admissions);
    saved.set(String(url), actual); return json(actual);
  });
  budget("4"); submit(); await flush();
  expect(screen.getByText(/保存已确认.*历史版本 2/)).toBeVisible();
  saved.set(policyURL(), workspacePolicy(3, false, 4));
  fireEvent.click(screen.getByRole("button", { name: "读取当前聊天策略并保留草稿" })); await flush();
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent("目录改动许可：未开启");
  expect(form().getByRole("checkbox", { name: "允许明确请求使用已授权写目录" })).not.toBeChecked();
  budget("5"); submit(); await flush();
  expect(JSON.parse(attempt(1).body)).toEqual({ enabled: true, allow_preparation: false, action: "text_submission", max_admissions: 5, expected_revision: 3 });
  expect(attempt(1).key).not.toBe(attempt(0).key);
});
it("bases edits during the post-save read on the confirmed receipt, not the prior GET", async () => {
  saved.set(policyURL(), workspacePolicy()); await open();
  const pending = deferred<Response>(); policyReads.mockReturnValueOnce(pending.promise);
  writes.mockResolvedValueOnce(json(workspacePolicy(2, false, 4))); budget("4"); check("允许明确请求使用已授权写目录"); submit(); await flush();
  expect(screen.getByText(/保存已确认.*历史版本 2/)).toBeVisible();
  budget("6"); check("允许明确请求使用已授权写目录");
  await act(async () => pending.resolve(json(workspacePolicy(3, false, 9))));
  expect(currentPolicy()).toHaveTextContent("版本 3");
  expect(form().getByRole("spinbutton")).toHaveValue(6);
  expect(form().getByRole("checkbox", { name: "允许明确请求使用已授权写目录" })).toBeChecked();
  expect(saveButton()).toBeEnabled();
});
it("cleans only the confirmed group's draft when its save finishes while another group is edited", async () => {
  saved.set(policyURL(), workspacePolicy()); const view = await open(); budget("4");
  const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise); submit();
  view.rerender(<Tasks group={other} />);
  fireEvent.click(await screen.findByRole("button", { name: "展开自然聊天工作配置" })); await flush();
  budget("7"); check("启用自然聊天工作");
  await act(async () => pending.resolve(json(workspacePolicy(2, true, 4))));
  expect(form().getByRole("spinbutton")).toHaveValue(7); expect(form().getByRole("checkbox", { name: "启用自然聊天工作" })).toBeChecked();
  saved.set(policyURL(), workspacePolicy(3, false, 4)); view.rerender(<Tasks group={group} />); await flush();
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent("版本 3");
  expect(form().getByRole("checkbox", { name: "允许明确请求使用已授权写目录" })).not.toBeChecked();
  view.rerender(<Tasks group={other} />); await flush(); expect(form().getByRole("spinbutton")).toHaveValue(7);
  expect(writes).toHaveBeenCalledTimes(1); expect(attempt(0).url).toBe(policyURL());
});
it("retains an unknown workspace key and body through revocation and rejection until its matching replay confirms", async () => {
  saved.set(policyURL(), workspacePolicy()); await open(); budget("4");
  writes.mockRejectedValueOnce(new TypeError("Receipt lost")); submit(); await flush(); const original = attempt(0);
  saved.set(policyURL(), workspacePolicy(3, false, 8));
  fireEvent.click(screen.getByRole("button", { name: "读取当前聊天策略并保留草稿" })); await flush();
  const workspace = form().getByRole("checkbox", { name: "允许明确请求使用已授权写目录" });
  expect(workspace).toBeChecked(); expect(workspace).toBeDisabled(); expect(form().getByRole("spinbutton")).toHaveValue(4);
  writes.mockResolvedValueOnce(refusal(409)); submit(); await flush(); expect(attempt(1)).toEqual(original);
  expect(form().getByRole("button", { name: "重试同一次聊天策略保存" })).toBeEnabled(); expect(workspace).toBeChecked();
  writes.mockResolvedValueOnce(json(workspacePolicy(2, true, 4))); submit(); await flush(); expect(attempt(2)).toEqual(original);
  expect(screen.getByText(/保存已确认.*历史版本 2/)).toBeVisible();
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent("版本 3");
  expect(workspace).not.toBeChecked(); expect(workspace).toBeEnabled(); expect(form().getByRole("spinbutton")).toHaveValue(8);
});
it.each([false, undefined])("updates same-revision workspace capability %s -> true without granting permission or discarding edits", async capability => {
  saved.set(policyURL(), { ...blank(), ...(capability === undefined ? {} : { workspace_changes_supported: capability }) }); await open();
  check("启用自然聊天工作"); budget("6");
  saved.set(policyURL(), { ...blank(), workspace_changes_supported: true });
  fireEvent.click(screen.getByRole("button", { name: "读取当前聊天策略并保留草稿" })); await flush();
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent("版本 0");
  expect(form().getByRole("checkbox", { name: "启用自然聊天工作" })).toBeChecked(); expect(form().getByRole("spinbutton")).toHaveValue(6);
  const workspace = form().getByRole("checkbox", { name: "允许明确请求使用已授权写目录" });
  expect(workspace).toBeEnabled(); expect(workspace).not.toBeChecked();
  submit(); await flush(); expect(JSON.parse(attempt(0).body)).toEqual({ enabled: true, allow_preparation: false,
    action: "text_submission", max_admissions: 6, expected_revision: 0 });
});
it("keeps receipt policy facts strict while accepting same-revision capability changes after confirmation", async () => {
  saved.set(policyURL(), { ...blank(), workspace_changes_supported: false }); await open(); check("启用自然聊天工作");
  writes.mockImplementationOnce(async (url, init) => {
    const actual = { ...receipt(String(init?.body)), workspace_changes_supported: false };
    saved.set(String(url), { ...actual, workspace_changes_supported: true }); return json(actual);
  });
  submit(); await flush();
  const workspace = form().getByRole("checkbox", { name: "允许明确请求使用已授权写目录" });
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent("版本 1"); expect(workspace).toBeEnabled(); expect(workspace).not.toBeChecked();
  const actual = saved.get(policyURL()) as Record<string, unknown>; const withoutCapability = { ...actual }; delete withoutCapability.workspace_changes_supported;
  saved.set(policyURL(), withoutCapability);
  fireEvent.click(screen.getByRole("button", { name: "读取当前聊天策略并保留草稿" })); await flush();
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent("版本 1"); expect(workspace).toBeDisabled();
  saved.set(policyURL(), actual);
  fireEvent.click(screen.getByRole("button", { name: "读取当前聊天策略并保留草稿" })); await flush();
  expect(workspace).toBeEnabled(); expect(workspace).not.toBeChecked(); expect(writes).toHaveBeenCalledTimes(1);
});
it.each([
  { allow_workspace_changes: true }, { max_admissions: 9 }, { configuration_source_id: "another-command" }, { enabled: false },
])("still rejects same-revision policy contradictions %j even when capability changes", async change => {
  const initial = { ...workspacePolicy(1, false), workspace_changes_supported: false }; saved.set(policyURL(), initial); await open(); budget("5");
  saved.set(policyURL(), { ...initial, ...change, workspace_changes_supported: true });
  fireEvent.click(screen.getByRole("button", { name: "读取当前聊天策略并保留草稿" })); await flush();
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent("当前策略读取过期或内容不一致");
  expect(form().getByRole("spinbutton")).toHaveValue(5); submit(); expect(writes).not.toHaveBeenCalled();
  saved.set(policyURL(), { ...initial, workspace_changes_supported: true });
  fireEvent.click(screen.getByRole("button", { name: "读取当前聊天策略并保留草稿" })); await flush();
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent("版本 1");
  expect(form().getByRole("spinbutton")).toHaveValue(5); expect(form().getByRole("checkbox", { name: "允许明确请求使用已授权写目录" })).not.toBeChecked();
});
it("still rejects policy revision rollback even when workspace capability becomes available", async () => {
  saved.set(policyURL(), { ...workspacePolicy(2, false), workspace_changes_supported: false }); await open();
  saved.set(policyURL(), workspacePolicy(1, false));
  fireEvent.click(screen.getByRole("button", { name: "读取当前聊天策略并保留草稿" })); await flush();
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent("当前策略读取过期或内容不一致");
  submit(); expect(writes).not.toHaveBeenCalled();
});
it("preserves historical workspace permission in a budget-only draft when same-revision capability becomes known", async () => {
  saved.set(policyURL(), { ...workspacePolicy(), workspace_changes_supported: undefined }); await open();
  const workspace = form().getByRole("checkbox", { name: "允许明确请求使用已授权写目录" });
  expect(workspace).toBeDisabled(); budget("4");
  saved.set(policyURL(), workspacePolicy());
  fireEvent.click(screen.getByRole("button", { name: "读取当前聊天策略并保留草稿" })); await flush();
  expect(workspace).toBeEnabled(); expect(form().getByRole("spinbutton")).toHaveValue(4);
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent("目录改动许可：允许明确请求使用已授权写目录");
  writes.mockImplementationOnce(async (url, init) => {
    const input = JSON.parse(String(init?.body));
    const actual = workspacePolicy(input.expected_revision + 1, input.allow_workspace_changes === true, input.max_admissions);
    saved.set(String(url), actual); return json(actual);
  });
  submit(); await flush();
  expect(JSON.parse(attempt(0).body)).toEqual({ enabled: true, allow_preparation: false, action: "text_submission",
    max_admissions: 4, expected_revision: 1, allow_workspace_changes: true });
  expect(workspace).toBeChecked(); expect(screen.getByText(/保存已确认.*历史版本 2/)).toBeVisible();
});
it("blocks a budget-only save of historical workspace permission while capability remains unconfirmed", async () => {
  saved.set(policyURL(), { ...workspacePolicy(), workspace_changes_supported: undefined }); await open();
  budget("4"); submit(); await flush();
  expect(writes).not.toHaveBeenCalled(); expect(screen.getByRole("alert")).toHaveTextContent("目录工作协议能力未确认，不能开启目录改动许可");
  const workspace = form().getByRole("checkbox", { name: "允许明确请求使用已授权写目录" });
  expect(workspace).toBeChecked(); expect(workspace).toBeDisabled(); expect(form().getByRole("spinbutton")).toHaveValue(4);
  expect(screen.getByRole("region", { name: "当前聊天策略" })).toHaveTextContent("历史许可标记开启");
});
const investigationChoice = "配置只读调查";
const topicEvidence = "允许使用本群主话题已保存消息";
const currentPolicy = () => screen.getByRole("region", { name: "当前聊天策略" });
const readPolicy = async () => {
  fireEvent.click(screen.getByRole("button", { name: "读取当前聊天策略并保留草稿" })); await flush();
};
const goal = () => form().getByRole("textbox", { name: "调查目标" });
const fill = (input: HTMLElement, value: string) => fireEvent.change(input, { target: { value } });
const artifactId = (row: number) => form().getByRole("textbox", { name: `成果 ID（第 ${row} 项）` });
const versionId = (row: number) => form().getByRole("textbox", { name: `版本 ID（第 ${row} 项）` });
const addVersion = () => form().getByRole("button", { name: "添加确切成果版本" });
const topicScope = { objective: "核对本话题仍未解决的问题", allow_topic_messages: true, artifact_versions: null, max_delegations: 0 };
const opts = { investigation_supported: true, investigation_execution_enabled: false };
const summaryScope = { allow_preparation: true, preparation_scope_kind: "topic_summary", preparation_supported: true,
  preauthorization_message_id: "summary-rule", preauthorization_text: "仅本话题摘要" };
const adoptLabel = "采用当前授权，保留任务上限草稿";
const adopt = () => fireEvent.click(form().getByRole("button", { name: adoptLabel }));
const saveButton = () => form().getByRole("button", { name: "保存聊天策略" });

it.each([true, false])("preserves recorded automatic text dispatch in a budget save with chat policy enabled=%s", async enabled => {
  saved.set(policyURL(), { ...workspacePolicy(1, false), enabled, automatic_text_dispatch: true }); await open();
  budget("4"); submit(); await flush();
  expect(writes).toHaveBeenCalledTimes(1);
  expect(attempt(0).body).toBe(JSON.stringify({ enabled, allow_preparation: false, action: "text_submission",
    max_admissions: 4, expected_revision: 1, automatic_text_dispatch: true }));
  expect(screen.getByText(/保存已确认.*历史版本 2/)).toBeVisible();
  expect(currentPolicy()).toHaveTextContent("自动文本分派：已记录");
  expect(form().queryByRole("checkbox", { name: /自动文本分派/ })).not.toBeInTheDocument();
});
it.each([true, false])("requires adoption when automatic text dispatch changes to %s while a draft is dirty", async automatic_text_dispatch => {
  saved.set(policyURL(), { ...workspacePolicy(1, false), automatic_text_dispatch: !automatic_text_dispatch }); await open();
  budget("5");
  saved.set(policyURL(), { ...workspacePolicy(2, false), automatic_text_dispatch }); await readPolicy();
  expect(currentPolicy()).toHaveTextContent("版本 2");
  if (!automatic_text_dispatch) expect(currentPolicy()).not.toHaveTextContent("自动文本分派");
  expect(saveButton()).toBeDisabled(); submit(); await flush(); expect(writes).not.toHaveBeenCalled();
  adopt();
  expect(form().getByRole("spinbutton")).toHaveValue(5);
  submit(); await flush();
  expect(JSON.parse(attempt(0).body)).toEqual({ enabled: true, allow_preparation: false, action: "text_submission",
    max_admissions: 5, expected_revision: 2, ...(automatic_text_dispatch ? { automatic_text_dispatch } : {}) });
});

it.each([undefined, false])("keeps old policy writes unchanged when investigation capability is %s", async investigation_supported => {
  saved.set(policyURL(), { ...blank(), investigation_supported }); await open();
  const choice = form().getByRole("checkbox", { name: investigationChoice });
  expect(choice).toBeDisabled(); expect(choice).not.toBeChecked();
  expect(currentPolicy()).toHaveTextContent("只读调查记录：未确认");
  expect(currentPolicy()).toHaveTextContent(investigation_supported === false ? "调查配置能力：当前实例不支持" : "调查配置能力：未确认");
  expect(currentPolicy()).toHaveTextContent("调查运行时：未确认");
  check("启用自然聊天工作"); submit(); await flush();
  expect(JSON.parse(attempt(0).body)).toEqual({ enabled: true, allow_preparation: false,
    action: "text_submission", max_admissions: 1, expected_revision: 0 });
});
it("explicitly saves a same-group exact version scope before runtime readiness without dropping other permissions", async () => {
  saved.set(policyURL(), { ...workspacePolicy(), ...opts, allow_preparation: true, preparation_scope_kind: "topic_summary",
    preparation_supported: true, preauthorization_message_id: "existing-rule", preauthorization_text: "仅本话题摘要" });
  await open();
  expect(form().getByRole("checkbox", { name: investigationChoice })).not.toBeChecked();
  expect(currentPolicy()).toHaveTextContent("只读调查记录：未配置");
  expect(currentPolicy()).toHaveTextContent("调查运行时：未启用");
  check(investigationChoice); fill(goal(), "  核对未决问题  ");
  expect(form().getByRole("checkbox", { name: topicEvidence })).not.toBeChecked();
  fireEvent.click(addVersion()); fireEvent.click(addVersion());
  fill(artifactId(2), "成果 /+?雪"); fill(versionId(2), "精确版本 /+?旧");
  fireEvent.click(form().getByRole("button", { name: "移除第 1 项" }));
  expect(artifactId(2)).toHaveValue("成果 /+?雪"); expect(versionId(2)).toHaveValue("精确版本 /+?旧");
  fill(form().getByRole("combobox", { name: "每项调查最多委派次数" }), "4");
  writes.mockImplementationOnce(async (url, init) => {
    const actual = { ...receipt(String(init?.body)), ...opts, workspace_changes_supported: true, allow_workspace_changes: true };
    saved.set(String(url), actual); return json(actual);
  });
  submit(); await flush();
  expect(JSON.parse(attempt(0).body)).toEqual({ enabled: true, allow_preparation: true, preparation_scope_kind: "topic_summary",
    allow_workspace_changes: true, action: "text_submission", max_admissions: 3, expected_revision: 1,
    investigation: { objective: "  核对未决问题  ", allow_topic_messages: false,
      artifact_versions: [{ artifact_id: "成果 /+?雪", version_id: "精确版本 /+?旧" }], max_delegations: 4 } });
  expect(attempt(0).key).toBeTruthy(); expect(attempt(0).url).toBe(policyURL()); expect(messages).not.toHaveBeenCalled();
  expect(currentPolicy()).toHaveTextContent("只读调查记录：已配置"); expect(currentPolicy()).toHaveTextContent("精确版本 /+?旧");
  expect(currentPolicy()).toHaveTextContent("调查运行时：未启用");
  expect(currentPolicy()).not.toHaveTextContent(/调查已执行|调查已提交|调查已接纳/);
});
it("requires explicit valid materials and prevents overlong objectives or more than 16 exact pairs", async () => {
  saved.set(policyURL(), { ...blank(), ...opts }); await open(); check(investigationChoice);
  fill(goal(), topicScope.objective); submit(); expect(writes).not.toHaveBeenCalled(); expect(screen.getByRole("alert")).toHaveTextContent("资料");
  check(topicEvidence); fill(goal(), "字".repeat(86)); submit(); expect(writes).not.toHaveBeenCalled(); expect(screen.getByRole("alert")).toHaveTextContent("256");
  const add = addVersion();
  for (let n = 0; n < 16; n++) fireEvent.click(add);
  expect(add).toBeDisabled(); expect(artifactId(16)).toBeVisible();
  fireEvent.click(form().getByRole("button", { name: "移除第 1 项" }));
  expect(add).toBeEnabled(); expect(artifactId(16)).toBeVisible(); expect(form().queryByRole("textbox", { name: "成果 ID（第 1 项）" })).not.toBeInTheDocument();
});
it("blocks permission-carrying saves without configure support but allows explicit removal", async () => {
  const configured = { ...workspacePolicy(1, false), ...opts, investigation: topicScope };
  saved.set(policyURL(), configured); await open(); budget("4");
  saved.set(policyURL(), { ...configured, investigation_supported: false, investigation_execution_enabled: true,
    investigation: { ...topicScope, artifact_versions: [] } }); await readPolicy();
  const choice = form().getByRole("checkbox", { name: investigationChoice });
  expect(choice).toBeChecked(); expect(goal()).toHaveValue(topicScope.objective); expect(goal()).toBeDisabled();
  expect(currentPolicy()).toHaveTextContent("只读调查记录：已配置"); expect(currentPolicy()).toHaveTextContent("调查运行时：已报告就绪");
  check("启用自然聊天工作"); submit(); expect(writes).not.toHaveBeenCalled();
  expect(screen.getByRole("alert")).toHaveTextContent("不能保存带有只读调查权限的策略");
  expect(choice).toBeEnabled(); check(investigationChoice); expect(choice).not.toBeChecked(); expect(choice).toBeDisabled();
  submit(); await flush(); expect(JSON.parse(attempt(0).body)).toEqual({ enabled: false, allow_preparation: false,
    action: "text_submission", max_admissions: 4, expected_revision: 1 });
});
it("requires adoption before a budget draft can save against newly configured investigation on a disabled policy", async () => {
  saved.set(policyURL(), { ...workspacePolicy(), ...opts }); await open(); budget("7");
  saved.set(policyURL(), { ...workspacePolicy(2, false, 8), ...opts, ...summaryScope, enabled: false, investigation: topicScope });
  await readPolicy();
  submit(); await flush(); expect(writes).not.toHaveBeenCalled();
  expect(currentPolicy()).toHaveTextContent(/版本 2.*未启用/);
  expect(currentPolicy()).toHaveTextContent("只读调查记录：已配置");
  expect(saveButton()).toBeDisabled();
  expect(form().getByRole("checkbox", { name: "启用自然聊天工作" })).toBeChecked();
  expect(form().getByRole("checkbox", { name: "允许明确请求使用已授权写目录" })).toBeChecked();
  expect(form().getByRole("checkbox", { name: "允许受限准备工作" })).not.toBeChecked();
  expect(form().getByRole("checkbox", { name: investigationChoice })).not.toBeChecked();

  adopt();
  expect(form().getByRole("spinbutton")).toHaveValue(7);
  expect(form().getByRole("checkbox", { name: "启用自然聊天工作" })).not.toBeChecked();
  expect(form().getByRole("checkbox", { name: "允许明确请求使用已授权写目录" })).not.toBeChecked();
  expect(form().getByRole("combobox", { name: "准备工作范围" })).toHaveValue("topic_summary");
  expect(goal()).toHaveValue(topicScope.objective);
  submit(); await flush();
  expect(JSON.parse(attempt(0).body)).toEqual({ enabled: false, allow_preparation: true, preparation_scope_kind: "topic_summary",
    max_admissions: 7, action: "text_submission", expected_revision: 2, investigation: { ...topicScope, artifact_versions: [] } });
});
it("requires adoption before an objective edit can save after the exact artifact version changes", async () => {
  const scope = { ...topicScope, allow_topic_messages: false, artifact_versions: [{ artifact_id: "成果", version_id: "v1" }] };
  saved.set(policyURL(), { ...workspacePolicy(1, false), ...opts, investigation: scope }); await open();
  fill(goal(), "目标草稿");
  const replacement = { ...scope, artifact_versions: [{ artifact_id: "成果", version_id: "v2" }] };
  saved.set(policyURL(), { ...workspacePolicy(2, false), ...opts, investigation: replacement });
  writes.mockResolvedValueOnce(refusal(409)); submit(); await flush();
  expect(JSON.parse(attempt(0).body)).toMatchObject({ expected_revision: 1, investigation: { ...scope, objective: "目标草稿" } });
  expect(currentPolicy()).toHaveTextContent("版本 ID：v2");
  submit(); await flush(); expect(writes).toHaveBeenCalledTimes(1);
  expect(saveButton()).toBeDisabled(); expect(goal()).toHaveValue("目标草稿"); expect(versionId(1)).toHaveValue("v1");

  adopt();
  expect(goal()).toHaveValue(replacement.objective); expect(versionId(1)).toHaveValue("v2");
  fill(goal(), "在当前范围内调整目标"); submit(); await flush();
  expect(JSON.parse(attempt(1).body)).toMatchObject({ expected_revision: 2, investigation: { ...replacement, objective: "在当前范围内调整目标" } });
  expect(attempt(1).key).not.toBe(attempt(0).key);
});
it.each([{ enabled: false }, { allow_workspace_changes: true }, summaryScope])("gates standalone authority changes %j even without an investigation scope", async change => {
  saved.set(policyURL(), workspacePolicy(1, false)); await open(); budget("5");
  saved.set(policyURL(), { ...workspacePolicy(2, false), ...change }); await readPolicy();
  expect(saveButton()).toBeDisabled(); submit(); await flush(); expect(writes).not.toHaveBeenCalled();
  expect(form().getByRole("button", { name: adoptLabel })).toBeEnabled();
});
it("keeps the draft when a newer revision changes only budget, generated rule evidence and capability observations", async () => {
  const configured = { ...workspacePolicy(1, false), ...opts, ...summaryScope, investigation: topicScope };
  saved.set(policyURL(), configured); await open(); budget("5"); fill(goal(), "目标草稿");
  saved.set(policyURL(), { ...configured, ...workspacePolicy(2, false, 8), ...summaryScope,
    preauthorization_message_id: "new-summary-rule", preauthorization_text: "本版总准入额度=8；仅本话题摘要",
    workspace_changes_supported: false, investigation_execution_enabled: true, investigation: { ...topicScope, artifact_versions: [] } });
  await readPolicy();
  expect(form().queryByRole("button", { name: adoptLabel })).not.toBeInTheDocument();
  expect(saveButton()).toBeEnabled(); expect(goal()).toHaveValue("目标草稿");
  submit(); await flush(); expect(JSON.parse(attempt(0).body)).toMatchObject({ max_admissions: 5, expected_revision: 2,
    investigation: { ...topicScope, objective: "目标草稿", artifact_versions: [] } });
});
it("requires adoption for changed authority read during a definitively rejected save", async () => {
  await open(); budget("4");
  const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise); submit();
  saved.set(policyURL(), { ...workspacePolicy(2, false), investigation: topicScope }); await readPolicy();
  const choice = form().getByRole("checkbox", { name: investigationChoice });
  expect(choice).not.toBeChecked(); expect(choice).toBeDisabled();
  expect(form().queryByRole("button", { name: adoptLabel })).not.toBeInTheDocument();
  expect(currentPolicy()).toHaveTextContent("只读调查记录：已配置");
  await act(async () => pending.resolve(json({ error: { code: "invalid_input", message: "请求被拒绝" } }, 400)));
  expect(choice).not.toBeChecked(); expect(saveButton()).toBeDisabled();
  submit(); await flush(); expect(writes).toHaveBeenCalledTimes(1);
  adopt(); expect(choice).toBeChecked();
  submit(); expect(writes).toHaveBeenCalledTimes(1);
  expect(screen.getByRole("alert")).toHaveTextContent("不能保存带有只读调查权限的策略");
});
it("checks current investigation facts against the receipt but ignores same-revision capability changes", async () => {
  saved.set(policyURL(), { ...blank(), ...opts }); await open(); check(investigationChoice);
  fill(goal(), topicScope.objective); check(topicEvidence);
  writes.mockImplementationOnce(async (url, init) => {
    const actual = { ...receipt(String(init?.body)), ...opts, investigation: topicScope };
    saved.set(String(url), { ...actual, investigation_execution_enabled: true, investigation: { ...topicScope, artifact_versions: [] } });
    return json(actual);
  });
  submit(); await flush(); expect(currentPolicy()).toHaveTextContent("版本 1");
  expect(currentPolicy()).toHaveTextContent("调查运行时：已报告就绪");
  saved.set(policyURL(), { ...receipt(attempt(0).body), ...opts, investigation: { ...topicScope, max_delegations: 1 } });
  await readPolicy(); expect(currentPolicy()).toHaveTextContent("当前策略读取过期或内容不一致");
  expect(currentPolicy()).toHaveTextContent("上次确认的只读调查记录：已配置");
  expect(form().getByRole("combobox", { name: "每项调查最多委派次数" })).toHaveValue("0");
  submit(); expect(writes).toHaveBeenCalledTimes(1);
});
it("freezes an investigation PUT across groups, a mismatched receipt and stale and newer observations", async () => {
  const configured = { ...workspacePolicy(4, false), ...opts, investigation: topicScope };
  saved.set(policyURL(), configured); const view = await open();
  fill(goal(), "新目标"); fireEvent.click(addVersion()); fill(artifactId(1), "成果"); fill(versionId(1), "原选中的版本");
  const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise); submit(); const original = attempt(0);
  view.rerender(<Tasks group={other} />);
  fireEvent.click(await screen.findByRole("button", { name: "展开自然聊天工作配置" })); await flush(); budget("8");
  expect(writes.mock.calls[0][1]?.signal?.aborted ?? false).toBe(false);
  await act(async () => pending.resolve(json({ ...receipt(original.body), investigation: topicScope })));
  saved.set(policyURL(), { ...configured, investigation_supported: false, investigation: null });
  view.rerender(<Tasks group={group} />); await flush();
  expect(currentPolicy()).toHaveTextContent("当前策略读取过期或内容不一致");
  expect(goal()).toHaveValue("新目标"); expect(versionId(1)).toHaveValue("原选中的版本"); expect(versionId(1)).toBeDisabled();
  expect(currentPolicy()).toHaveTextContent("上次确认的只读调查记录：已配置");
  saved.set(policyURL(), { ...workspacePolicy(7, false), investigation: null });
  writes.mockResolvedValueOnce(refusal(409)); submit(); await flush(); expect(attempt(1)).toEqual(original);
  expect(currentPolicy()).toHaveTextContent("版本 7"); expect(currentPolicy()).toHaveTextContent("只读调查记录：未确认");
  expect(form().queryByRole("button", { name: adoptLabel })).not.toBeInTheDocument();
  expect(form().getByRole("button", { name: "重试同一次聊天策略保存" })).toBeEnabled();
  writes.mockResolvedValueOnce(json(receipt(original.body))); submit(); await flush(); expect(attempt(2)).toEqual(original);
  expect(screen.getByText(/保存已确认.*历史版本 5/)).toBeVisible(); expect(currentPolicy()).toHaveTextContent("版本 7");
  expect(form().getByRole("checkbox", { name: investigationChoice })).not.toBeChecked();
  view.rerender(<Tasks group={other} />); await flush(); expect(form().getByRole("spinbutton")).toHaveValue(8);
});

it("preserves an explicit workspace opt-out across temporarily unconfirmed same-revision capability", async () => {
  saved.set(policyURL(), workspacePolicy()); await open();
  check("允许明确请求使用已授权写目录"); budget("4");
  saved.set(policyURL(), { ...workspacePolicy(), workspace_changes_supported: undefined });
  fireEvent.click(screen.getByRole("button", { name: "读取当前聊天策略并保留草稿" })); await flush();
  const workspace = form().getByRole("checkbox", { name: "允许明确请求使用已授权写目录" });
  expect(workspace).toBeDisabled(); expect(workspace).not.toBeChecked();
  saved.set(policyURL(), workspacePolicy());
  fireEvent.click(screen.getByRole("button", { name: "读取当前聊天策略并保留草稿" })); await flush();
  expect(workspace).toBeEnabled(); expect(workspace).not.toBeChecked(); expect(form().getByRole("spinbutton")).toHaveValue(4);
  submit(); await flush();
  expect(JSON.parse(attempt(0).body)).toEqual({ enabled: true, allow_preparation: false, action: "text_submission", max_admissions: 4, expected_revision: 1 });
});
