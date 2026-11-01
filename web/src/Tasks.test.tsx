import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Tasks } from "./Tasks";
import { deferred, group, json, other, page } from "./test/groups";
import { acceptedTask, secondTask, submittedTask, task, taskInput } from "./test/tasks";

const reads = vi.fn<typeof fetch>();
const details = vi.fn<typeof fetch>();
const writes = vi.fn<typeof fetch>();
const panel = () => within(screen.getByRole("region", { name: "群组任务" }));
const detail = () => within(panel().getByRole("region", { name: "任务详情" }));
const field = (name: string) => panel().getByRole("textbox", { name });
const change = (name: string, value: string) => fireEvent.change(field(name), { target: { value } });
const submit = () => fireEvent.submit(panel().getByRole("form", { name: "创建任务" }));
const row = (title = task.title) => panel().getByRole("button", { name: new RegExp(`^${title}，`) });
const open = () => render(<Tasks group={group} />);
async function draft() {
  await panel().findByText("尚无任务，可创建任务并约定验收条件。");
  change("任务标题", task.title); change("验收条件", task.acceptance); change("任务描述（可选）", task.description);
}
const attempt = (index: number) => {
  const [url, init] = writes.mock.calls[index];
  return { url, body: init?.body, key: new Headers(init?.headers).get("Idempotency-Key") };
};
beforeEach(() => {
  reads.mockReset().mockImplementation(async () => page());
  details.mockReset().mockImplementation(async () => json(task));
  writes.mockReset().mockImplementation(async () => json(task, 201));
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) =>
    (init?.method === "POST" ? writes : String(input).includes("/tasks?") ? reads : details)(input, init));
});
afterEach(() => vi.unstubAllGlobals());

it("does not read or allow creation without a group, including after a previous group", async () => {
  const view = render(<Tasks group={null} />);
  expect(screen.getByText("请先选择群组，再查看任务。")).toBeVisible();
  expect(screen.queryByRole("form")).not.toBeInTheDocument();
  expect(reads).not.toHaveBeenCalled(); expect(details).not.toHaveBeenCalled();
  view.rerender(<Tasks group={group} />); await draft();
  view.rerender(<Tasks group={null} />);
  expect(screen.getByText("请先选择群组，再查看任务。")).toBeVisible();
  expect(screen.queryByRole("form")).not.toBeInTheDocument();
  view.rerender(<Tasks group={group} />);
  expect(field("任务标题")).toHaveValue(task.title);
  await act(async () => {});
});

it("distinguishes a pending list from a real empty result", async () => {
  const pending = deferred<Response>(); reads.mockReturnValueOnce(pending.promise); open();
  expect(panel().getByText("正在载入任务…")).toBeVisible();
  expect(panel().queryByText("尚无任务，可创建任务并约定验收条件。")).not.toBeInTheDocument();
  expect(details).not.toHaveBeenCalled();
  await act(async () => pending.resolve(page()));
  expect(panel().getByText("尚无任务，可创建任务并约定验收条件。")).toBeVisible();
  expect(panel().getByText("选择任务以读取最新详情。")).toBeVisible();
});
it.each(["network", "server", "malformed"])("offers explicit list retry after %s failure", async failure => {
  if (failure === "network") reads.mockRejectedValueOnce(new TypeError("English network diagnostic"));
  else reads.mockResolvedValueOnce(failure === "server" ? json({ error: { message: "English backend diagnostic" } }, 503) : json({ items: null }));
  open();
  const alert = await panel().findByRole("alert"); expect(alert).not.toHaveTextContent("English");
  expect(panel().queryByText("尚无任务，可创建任务并约定验收条件。")).not.toBeInTheDocument();
  fireEvent.click(panel().getByRole("button", { name: "重试载入任务" }));
  await panel().findByText("尚无任务，可创建任务并约定验收条件。"); expect(reads).toHaveBeenCalledTimes(2);
});

it("reads full authoritative detail separately from the list and explains unset fields", async () => {
  reads.mockResolvedValueOnce(page([task]));
  const pending = deferred<Response>(); details.mockReturnValueOnce(pending.promise); open();
  await panel().findByRole("button", { name: `${task.title}，待交付` });
  expect(details).not.toHaveBeenCalled(); row().focus(); expect(row()).toHaveFocus(); fireEvent.click(row());
  expect(detail().getByText("正在载入任务详情…")).toBeVisible();
  expect(detail().queryByRole("heading", { name: task.title })).not.toBeInTheDocument();
  const authoritative = { ...task, title: "服务器中的完整标题", description: "第一行\n第二行", acceptance: "完整验收条件\n核对证据", revision: 2 };
  await act(async () => pending.resolve(json(authoritative)));
  expect(detail().getByRole("heading", { name: authoritative.title })).toBeVisible();
  expect(detail().getByText(authoritative.description, { normalizer: text => text })).toBeVisible();
  expect(detail().getByText(authoritative.acceptance, { normalizer: text => text })).toBeVisible();
  expect(detail().getByText("未指定负责人")).toBeVisible(); expect(detail().getByText("无依赖任务")).toBeVisible();
  expect(detail().getByText("2")).toBeVisible(); expect(detail().getByText("待交付")).toBeVisible();
  expect(details.mock.calls[0][0]).toBe(`/api/v1/groups/${group.id}/tasks/${task.id}`);
  expect(panel().queryByRole("button", { name: /提交成果|接纳成果|开始执行/ })).not.toBeInTheDocument();
});
it.each([["待交付", task], ["待验收", submittedTask], ["已接纳", acceptedTask]] as const)("uses real %s task language, not execution language", async (label, state) => {
  const result = { ...state, assignee_id: "member-1", dependency_ids: ["dep-1", "dep-2"], description: "" };
  reads.mockResolvedValueOnce(page([result])); details.mockResolvedValueOnce(json(result)); open();
  fireEvent.click(await panel().findByRole("button", { name: `${task.title}，${label}` }));
  expect(await detail().findByText(label)).toBeVisible(); expect(detail().getByText("member-1")).toBeVisible();
  expect(detail().getByText("dep-1")).toBeVisible(); expect(detail().getByText("dep-2")).toBeVisible();
  expect(detail().getByText("未填写描述")).toBeVisible();
  expect(panel().getByText(/待交付.*不代表已开始执行/)).toBeVisible();
  expect(panel().getByText(/已接纳.*不等同于 CLI 运行成功/)).toBeVisible();
});
it("retries a failed authoritative detail without substituting the list snapshot", async () => {
  reads.mockResolvedValueOnce(page([task])); details.mockResolvedValueOnce(json({}, 503)); open();
  fireEvent.click(await panel().findByRole("button", { name: `${task.title}，待交付` }));
  expect(await detail().findByRole("alert")).toHaveTextContent("503");
  expect(detail().queryByRole("heading", { name: task.title })).not.toBeInTheDocument();
  fireEvent.click(detail().getByRole("button", { name: "重试载入详情" }));
  expect(await detail().findByRole("heading", { name: task.title })).toBeVisible(); expect(details).toHaveBeenCalledTimes(2);
});
it("reselects and refreshes detail with fresh GETs, and refreshes list and detail in parallel", async () => {
  reads.mockResolvedValueOnce(page([task])); open();
  fireEvent.click(await panel().findByRole("button", { name: `${task.title}，待交付` }));
  await detail().findByRole("heading", { name: task.title });
  details.mockResolvedValueOnce(json({ ...task, title: "重新选择后的标题", revision: 2 })); fireEvent.click(row());
  await detail().findByRole("heading", { name: "重新选择后的标题" });
  details.mockResolvedValueOnce(json({ ...task, title: "刷新详情后的标题", revision: 3 }));
  fireEvent.click(detail().getByRole("button", { name: "刷新详情" }));
  await detail().findByRole("heading", { name: "刷新详情后的标题" }); expect(reads).toHaveBeenCalledTimes(1);
  const listRead = deferred<Response>(), detailRead = deferred<Response>();
  reads.mockReturnValueOnce(listRead.promise); details.mockReturnValueOnce(detailRead.promise);
  fireEvent.click(panel().getByRole("button", { name: "刷新任务" }));
  expect(reads).toHaveBeenCalledTimes(2); expect(details).toHaveBeenCalledTimes(4);
  await act(async () => detailRead.resolve(json({ ...acceptedTask, revision: 4 })));
  expect(detail().getByText("已接纳")).toBeVisible(); expect(panel().getByText("正在载入任务…")).toBeVisible();
  await act(async () => listRead.resolve(page([task])));
  expect(detail().getByText("已接纳")).toBeVisible();
});
it("updates duplicate page rows, preserves selection, and resets pagination on refresh", async () => {
  reads.mockResolvedValueOnce(page([task], "a+/")); open();
  fireEvent.click(await panel().findByRole("button", { name: `${task.title}，待交付` }));
  await detail().findByRole("heading", { name: task.title }); expect(reads).toHaveBeenCalledTimes(1);
  const updated = { ...submittedTask, title: "更新后的任务", revision: 2 };
  reads.mockResolvedValueOnce(page([updated, secondTask, { ...secondTask, title: "页内最新标题" }]));
  fireEvent.click(panel().getByRole("button", { name: "加载更多任务" }));
  await panel().findByRole("button", { name: "页内最新标题，待交付" });
  expect(row(updated.title)).toHaveAttribute("aria-pressed", "true");
  expect(panel().queryByRole("button", { name: `${task.title}，待交付` })).not.toBeInTheDocument();
  expect(panel().getAllByRole("button", { name: "更新后的任务，待验收" })).toHaveLength(1);
  expect(reads.mock.calls[1][0]).toBe(`/api/v1/groups/${group.id}/tasks?limit=50&before=a%2B%2F`);
  expect(details).toHaveBeenCalledTimes(1); expect(detail().getByText("待交付")).toBeVisible();
  reads.mockResolvedValueOnce(page([secondTask])); fireEvent.click(panel().getByRole("button", { name: "刷新任务" }));
  await panel().findByRole("button", { name: `${secondTask.title}，待交付` });
  expect(reads.mock.calls[2][0]).toBe(`/api/v1/groups/${group.id}/tasks?limit=50`);
  expect(panel().queryByRole("button", { name: "更新后的任务，待验收" })).not.toBeInTheDocument();
});
it("retries a failed page with the same cursor without discarding existing tasks", async () => {
  reads.mockResolvedValueOnce(page([task], "next+/")); open();
  await panel().findByRole("button", { name: `${task.title}，待交付` }); reads.mockResolvedValueOnce(json({}, 503));
  fireEvent.click(panel().getByRole("button", { name: "加载更多任务" })); await panel().findByRole("alert");
  expect(row()).toBeVisible(); reads.mockResolvedValueOnce(page([secondTask]));
  fireEvent.click(panel().getByRole("button", { name: "重试载入任务" }));
  await panel().findByRole("button", { name: `${secondTask.title}，待交付` });
  expect(reads.mock.calls[2][0]).toBe(reads.mock.calls[1][0]); expect(row()).toBeVisible();
});

it.each([200, 503])("ignores delayed group A list HTTP %s after switching to B", async status => {
  const old = deferred<Response>(); reads.mockReturnValueOnce(old.promise); const view = open();
  expect(panel().getByText("正在载入任务…")).toBeVisible(); view.rerender(<Tasks group={other} />);
  await panel().findByText("尚无任务，可创建任务并约定验收条件。");
  await act(async () => old.resolve(status === 200 ? page([task]) : json({}, status)));
  expect(panel().queryByRole("button", { name: `${task.title}，待交付` })).not.toBeInTheDocument();
  expect(panel().queryByRole("alert")).not.toBeInTheDocument(); expect(reads.mock.calls[0][1]?.signal?.aborted).toBe(true);
  expect(reads.mock.calls[1][0]).toBe(`/api/v1/groups/${other.id}/tasks?limit=50`);
});
it("clears group-bound cursors and re-reads selected detail after returning to a group", async () => {
  reads.mockResolvedValueOnce(page([task], "cursor-a")); const view = open();
  fireEvent.click(await panel().findByRole("button", { name: `${task.title}，待交付` })); await detail().findByText(task.acceptance);
  reads.mockResolvedValueOnce(page([secondTask], "cursor-b")); fireEvent.click(panel().getByRole("button", { name: "加载更多任务" }));
  await panel().findByRole("button", { name: `${secondTask.title}，待交付` });
  view.rerender(<Tasks group={other} />); await panel().findByText("尚无任务，可创建任务并约定验收条件。");
  reads.mockResolvedValueOnce(page([task])); details.mockResolvedValueOnce(json({ ...task, acceptance: "返回后最新验收条件" }));
  view.rerender(<Tasks group={group} />); await detail().findByText("返回后最新验收条件");
  expect(reads.mock.calls[3][0]).toBe(`/api/v1/groups/${group.id}/tasks?limit=50`); expect(details).toHaveBeenCalledTimes(2);
});
it.each([200, 503])("ignores stale selected detail HTTP %s after selecting another task", async status => {
  reads.mockResolvedValueOnce(page([task, secondTask])); const old = deferred<Response>(); details.mockReturnValueOnce(old.promise); open();
  fireEvent.click(await panel().findByRole("button", { name: `${task.title}，待交付` }));
  details.mockResolvedValueOnce(json(secondTask)); fireEvent.click(row(secondTask.title));
  await detail().findByRole("heading", { name: secondTask.title });
  await act(async () => old.resolve(status === 200 ? json(task) : json({}, status)));
  expect(detail().getByRole("heading", { name: secondTask.title })).toBeVisible(); expect(detail().queryByRole("alert")).not.toBeInTheDocument();
  expect(details.mock.calls[0][1]?.signal?.aborted).toBe(true);
});
it("ignores both stale group detail and stale same-task detail refreshes", async () => {
  reads.mockResolvedValueOnce(page([task])); const old = deferred<Response>(); details.mockReturnValueOnce(old.promise); const view = open();
  fireEvent.click(await panel().findByRole("button", { name: `${task.title}，待交付` })); view.rerender(<Tasks group={other} />);
  await panel().findByText("尚无任务，可创建任务并约定验收条件。");
  await act(async () => old.resolve(json(task)));
  expect(panel().queryByRole("region", { name: "任务详情" })).not.toBeInTheDocument(); expect(details.mock.calls[0][1]?.signal?.aborted).toBe(true);
  const stale = deferred<Response>(); details.mockReturnValueOnce(stale.promise); reads.mockResolvedValueOnce(page([task]));
  view.rerender(<Tasks group={group} />); expect(detail().getByText("正在载入任务详情…")).toBeVisible();
  details.mockResolvedValueOnce(json({ ...task, title: "最新详情" })); fireEvent.click(detail().getByRole("button", { name: "刷新详情" }));
  await detail().findByRole("heading", { name: "最新详情" }); await act(async () => stale.resolve(json(task)));
  expect(detail().getByRole("heading", { name: "最新详情" })).toBeVisible(); expect(details.mock.calls[1][1]?.signal?.aborted).toBe(true);
});
it("ignores an older list after a newer same-group refresh", async () => {
  const old = deferred<Response>(); reads.mockReturnValueOnce(old.promise); open();
  reads.mockResolvedValueOnce(page([secondTask])); fireEvent.click(panel().getByRole("button", { name: "刷新任务" }));
  await panel().findByRole("button", { name: `${secondTask.title}，待交付` }); await act(async () => old.resolve(page([task])));
  expect(row(secondTask.title)).toBeVisible(); expect(panel().queryByRole("button", { name: `${task.title}，待交付` })).not.toBeInTheDocument();
});

it("requires nonblank title and acceptance, posts the three-field DTO once and waits for a receipt", async () => {
  open(); await draft(); change("任务标题", "  "); submit();
  expect(panel().getByRole("alert")).toHaveTextContent("请输入任务标题和验收条件"); expect(writes).not.toHaveBeenCalled();
  change("任务标题", task.title); change("验收条件", " \n "); submit(); expect(writes).not.toHaveBeenCalled();
  change("任务标题", ` ${task.title} `); change("验收条件", ` ${task.acceptance} `); change("任务描述（可选）", "");
  const receipt = deferred<Response>(); writes.mockReturnValueOnce(receipt.promise); submit(); submit();
  expect(field("任务标题")).toBeDisabled(); expect(field("验收条件")).toBeDisabled(); expect(writes).toHaveBeenCalledTimes(1);
  expect(JSON.parse(String(attempt(0).body))).toEqual({ ...taskInput, description: "" }); expect(attempt(0).key).toMatch(/^[\da-f-]{36}$/);
  expect(panel().queryByRole("button", { name: `${task.title}，待交付` })).not.toBeInTheDocument();
  await act(async () => receipt.resolve(json(task, 201)));
  expect(row()).toHaveAttribute("aria-pressed", "true"); expect(field("任务标题")).toHaveValue(""); expect(field("任务标题")).toBeEnabled();
  expect(await detail().findByRole("heading", { name: task.title })).toBeVisible(); expect(details).toHaveBeenCalledTimes(1);
});
it.each(["network", "server", "malformed", "wrong-group", "unsafe-revision", "200", "202"])("preserves exact key and original input after unknown %s creation", async failure => {
  open(); await draft();
  if (failure === "network") writes.mockRejectedValueOnce(new TypeError("English offline diagnostic"));
  else writes.mockResolvedValueOnce(failure === "server" ? json({}, 503) : failure === "malformed" ? json({}, 201)
    : failure === "wrong-group" ? json({ ...task, group_id: other.id }, 201)
    : failure === "unsafe-revision" ? json({ ...task, revision: Number.MAX_SAFE_INTEGER + 1 }, 201) : json(task, Number(failure)));
  submit(); expect(await panel().findByRole("alert")).toHaveTextContent("创建结果未确认");
  expect(panel().getByRole("alert")).not.toHaveTextContent("English"); expect(field("任务标题")).toBeDisabled();
  expect(field("任务标题")).toHaveValue(task.title); expect(field("验收条件")).toBeDisabled(); expect(field("任务描述（可选）")).toBeDisabled();
  expect(panel().queryByRole("button", { name: `${task.title}，待交付` })).not.toBeInTheDocument();
  fireEvent.click(panel().getByRole("button", { name: "重试同一次任务创建" })); await panel().findByRole("button", { name: `${task.title}，待交付` });
  expect(attempt(1)).toEqual(attempt(0)); expect(JSON.parse(String(attempt(0).body))).toEqual(taskInput);
});
it("keeps an invalid pointer receipt unknown and accepts an advanced 201 retry with the same key and input", async () => {
  open(); await draft();
  writes.mockResolvedValueOnce(json({ ...submittedTask, current_submission_id: "" }, 201)).mockResolvedValueOnce(json(acceptedTask, 201));
  details.mockResolvedValueOnce(json(acceptedTask)); submit();
  expect(await panel().findByRole("alert")).toHaveTextContent("创建结果未确认");
  expect(field("任务标题")).toBeDisabled(); expect(details).not.toHaveBeenCalled();
  expect(panel().queryByRole("button", { name: `${task.title}，待验收` })).not.toBeInTheDocument();
  fireEvent.click(panel().getByRole("button", { name: "重试同一次任务创建" }));
  expect(await panel().findByRole("button", { name: `${task.title}，已接纳` })).toHaveAttribute("aria-pressed", "true");
  expect(await detail().findByText("已接纳")).toBeVisible(); expect(detail().getByText(acceptedTask.current_submission_id)).toBeVisible();
  expect(detail().getByText("未关联尝试")).toBeVisible(); expect(panel().queryByRole("alert")).not.toBeInTheDocument();
  expect(field("任务标题")).toBeEnabled(); expect(field("任务标题")).toHaveValue("");
  expect(writes).toHaveBeenCalledTimes(2); expect(attempt(1)).toEqual(attempt(0));
  expect(JSON.parse(String(attempt(1).body))).toEqual(taskInput);
});
it.each([400, 404, 409, 413, 415])("allows corrected input and a new key after definitive HTTP %s rejection", async status => {
  open(); await draft(); writes.mockResolvedValueOnce(json({ error: { message: "English backend diagnostic" } }, status)); submit();
  const alert = await panel().findByRole("alert"); expect(alert).toHaveTextContent(String(status)); expect(alert).not.toHaveTextContent("English");
  expect(field("任务标题")).toBeEnabled(); expect(field("任务标题")).toHaveValue(task.title);
  change("验收条件", "修正后的验收条件"); submit(); await panel().findByRole("button", { name: `${task.title}，待交付` });
  expect(attempt(1).key).not.toBe(attempt(0).key); expect(JSON.parse(String(attempt(1).body))).toEqual({ ...taskInput, acceptance: "修正后的验收条件" });
});
it("keeps pending and then uncertain A creation across switches, with independent B draft and immutable retry", async () => {
  const view = open(); await draft(); const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise); submit();
  const original = attempt(0); view.rerender(<Tasks group={other} />); await panel().findByText("尚无任务，可创建任务并约定验收条件。");
  expect(field("任务标题")).toHaveValue(""); expect(panel().queryByRole("alert")).not.toBeInTheDocument(); change("任务标题", "B 的独立草稿");
  view.rerender(<Tasks group={group} />); expect(field("任务标题")).toHaveValue(task.title); expect(field("任务标题")).toBeDisabled();
  expect(panel().getByRole("button", { name: "正在创建任务…" })).toBeDisabled(); expect(writes).toHaveBeenCalledTimes(1);
  expect(writes.mock.calls[0][1]?.signal?.aborted).toBe(false); await act(async () => pending.resolve(json({}, 503)));
  await panel().findByRole("button", { name: "重试同一次任务创建" }); view.rerender(<Tasks group={null} />);
  expect(screen.queryByRole("form")).not.toBeInTheDocument(); view.rerender(<Tasks group={other} />);
  expect(field("任务标题")).toHaveValue("B 的独立草稿"); expect(field("任务标题")).toBeEnabled();
  view.rerender(<Tasks group={group} />); expect(field("任务描述（可选）")).toHaveValue(task.description);
  fireEvent.click(panel().getByRole("button", { name: "重试同一次任务创建" })); await panel().findByRole("button", { name: `${task.title}，待交付` });
  expect(attempt(1)).toEqual(original); expect(original.url).toBe(`/api/v1/groups/${group.id}/tasks`);
  view.rerender(<Tasks group={other} />); expect(field("任务标题")).toHaveValue("B 的独立草稿"); await act(async () => {});
});
it("keeps a late A creation receipt and selection out of visible B", async () => {
  const view = open(); await draft(); const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise); submit();
  const bTask = { ...secondTask, group_id: other.id, title: "B 的任务" };
  reads.mockResolvedValueOnce(page([bTask])); details.mockResolvedValueOnce(json(bTask)); view.rerender(<Tasks group={other} />);
  fireEvent.click(await panel().findByRole("button", { name: `${bTask.title}，待交付` })); await detail().findByRole("heading", { name: bTask.title });
  await act(async () => pending.resolve(json(task, 201)));
  expect(row(bTask.title)).toHaveAttribute("aria-pressed", "true"); expect(detail().getByRole("heading", { name: bTask.title })).toBeVisible();
  expect(panel().queryByRole("button", { name: `${task.title}，待交付` })).not.toBeInTheDocument(); expect(details).toHaveBeenCalledTimes(1);
  reads.mockResolvedValueOnce(page([task])); view.rerender(<Tasks group={group} />);
  expect(row()).toHaveAttribute("aria-pressed", "true"); await detail().findByRole("heading", { name: task.title });
});
it.each(["missing", "stale"])("protects a new confirmed receipt from an older %s list, but allows newer authoritative updates", async snapshot => {
  const old = deferred<Response>(); reads.mockReturnValueOnce(old.promise); open();
  change("任务标题", task.title); change("验收条件", task.acceptance); submit();
  await panel().findByRole("button", { name: `${task.title}，待交付` });
  await act(async () => old.resolve(page(snapshot === "missing" ? [] : [{ ...acceptedTask, title: "旧快照标题" }])));
  expect(row()).toBeVisible(); expect(panel().queryByRole("button", { name: "旧快照标题，已接纳" })).not.toBeInTheDocument();
  const updated = { ...submittedTask, title: "新的权威标题", revision: 2 };
  reads.mockResolvedValueOnce(page([updated])); details.mockResolvedValueOnce(json(updated)); fireEvent.click(panel().getByRole("button", { name: "刷新任务" }));
  await panel().findByRole("button", { name: "新的权威标题，待验收" }); await detail().findByRole("heading", { name: updated.title });
  expect(detail().getByText("待验收")).toBeVisible(); expect(panel().queryByRole("button", { name: `${task.title}，待交付` })).not.toBeInTheDocument();
});
it("aborts list, detail and pending POST on full unmount and ignores late results", async () => {
  reads.mockResolvedValueOnce(page([task])); const detailRead = deferred<Response>(); details.mockReturnValueOnce(detailRead.promise); const view = open();
  fireEvent.click(await panel().findByRole("button", { name: `${task.title}，待交付` }));
  const listRead = deferred<Response>(); reads.mockReturnValueOnce(listRead.promise); fireEvent.click(panel().getByRole("button", { name: "刷新任务" }));
  const latestDetail = deferred<Response>(); details.mockReturnValueOnce(latestDetail.promise);
  fireEvent.click(detail().getByRole("button", { name: "刷新详情" }));
  change("任务标题", secondTask.title); change("验收条件", task.acceptance);
  const receipt = deferred<Response>(); writes.mockReturnValueOnce(receipt.promise); submit(); view.unmount();
  expect(reads.mock.calls[1][1]?.signal?.aborted).toBe(true); expect(details.mock.calls.at(-1)?.[1]?.signal?.aborted).toBe(true);
  expect(writes.mock.calls[0][1]?.signal?.aborted).toBe(true);
  await act(async () => { listRead.resolve(page([task])); detailRead.resolve(json(task)); latestDetail.resolve(json(task)); receipt.resolve(json(secondTask, 201)); });
  expect(screen.queryByRole("region", { name: "任务" })).not.toBeInTheDocument();
});
