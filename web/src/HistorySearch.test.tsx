import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { HistorySearch } from "./HistorySearch";
import { deferred, group, json, other, page } from "./test/groups";
import { message } from "./test/messages";

const reads = vi.fn<typeof fetch>(), details = vi.fn<typeof fetch>(), transport = vi.fn<typeof fetch>();
const panel = () => within(screen.getByRole("region", { name: "历史消息搜索" }));
const results = () => within(panel().getByRole("region", { name: "历史搜索结果" }));
const detail = () => within(panel().getByRole("region", { name: "完整消息" }));
const change = (name: string, value: string) => fireEvent.change(panel().getByRole("textbox", { name }), { target: { value } });
const scope = (value: string) => fireEvent.change(panel().getByRole("combobox", { name: "检索范围" }), { target: { value } });
const submit = () => fireEvent.submit(panel().getByRole("form", { name: "搜索历史消息" }));
const search = (q = "讨论") => { change("关键词", q); submit(); };
const read = (id = message.id) => panel().getByRole("button", { name: `读取完整消息 ${id}` });
const open = () => render(<HistorySearch group={group} />);
const row = (id: string, content: string, topic = "") => ({ ...message, id, content, topic_id: topic, deliveries: [] });
const params = (index: number) => new URL(String(reads.mock.calls[index][0]), "http://localhost").searchParams;

beforeEach(() => {
  reads.mockReset().mockImplementation(async () => page());
  details.mockReset().mockImplementation(async () => json(message));
  transport.mockReset().mockImplementation((input, init) => {
    if ((init?.method ?? "GET") !== "GET") throw new Error("Unexpected mutation");
    const path = String(input);
    if (/^\/api\/v1\/groups\/[^/]+\/history\?/.test(path)) return reads(input, init);
    if (/^\/api\/v1\/groups\/[^/]+\/history\//.test(path)) return details(input, init);
    throw new Error("Unexpected endpoint");
  });
  vi.stubGlobal("fetch", transport);
});
afterEach(() => vi.unstubAllGlobals());

it("does not request history without a selected group and clears previous scoped content immediately", async () => {
  const view = render(<HistorySearch group={null} />);
  expect(panel().getByText("请先选择群组，再搜索历史消息。")).toBeVisible();
  expect(panel().queryByRole("form")).not.toBeInTheDocument(); expect(transport).not.toHaveBeenCalled();
  view.rerender(<HistorySearch group={group} />); reads.mockResolvedValueOnce(page([message])); search();
  await panel().findByRole("button", { name: `读取完整消息 ${message.id}` });
  view.rerender(<HistorySearch group={null} />);
  expect(panel().queryByRole("form")).not.toBeInTheDocument();
  expect(panel().queryByRole("region", { name: "历史搜索结果" })).not.toBeInTheDocument();
  view.rerender(<HistorySearch group={group} />);
  expect(panel().getByRole("textbox", { name: "关键词" })).toHaveValue(""); expect(transport).toHaveBeenCalledTimes(1);
});

it.each([
  { label: "entire group", scope: "all", topic: undefined },
  { label: "main channel", scope: "main", topic: "" },
  { label: "specified topic", scope: "topic", topic: " 话题 /?+ " },
  { label: "whitespace topic", scope: "topic", topic: " \t\u3000 " },
])("only searches on explicit submit with the $label snapshot", async spec => {
  open(); expect(transport).not.toHaveBeenCalled();
  change("关键词", "中"); change("关键词", "中文"); change("关键词", " \u3000100%_\\ 中文 ");
  scope(spec.scope);
  if (spec.scope === "topic") change("话题 ID", spec.topic!);
  expect(transport).not.toHaveBeenCalled(); expect(details).not.toHaveBeenCalled();
  submit(); await panel().findByText("未找到匹配的历史消息。");
  expect(reads).toHaveBeenCalledTimes(1); expect(params(0).get("q")).toBe(" \u3000100%_\\ 中文 ");
  expect(params(0).has("topic_id")).toBe(spec.topic !== undefined); expect(params(0).get("topic_id")).toBe(spec.topic ?? null);
  expect(params(0).get("limit")).toBe("50"); expect(params(0).has("before")).toBe(false);
  expect(transport.mock.calls.every(([, init]) => (init?.method ?? "GET") === "GET")).toBe(true);
});

it("rejects blank keywords and missing specified IDs locally, but leaves nonblank length validation to the backend", async () => {
  open(); search(" \n\u3000 "); expect(panel().getByRole("alert")).toHaveTextContent("请输入关键词");
  expect(transport).not.toHaveBeenCalled(); change("关键词", "有效词"); scope("topic"); submit();
  expect(panel().getByRole("alert")).toHaveTextContent("请输入话题 ID"); expect(transport).not.toHaveBeenCalled();
  scope("all"); const raw = ` ${"词".repeat(300)} `;
  reads.mockResolvedValueOnce(json({ error: { code: "invalid_input", message: "English diagnostic" } }, 400)); search(raw);
  expect(await results().findByRole("alert")).toHaveTextContent("400"); expect(params(0).get("q")).toBe(raw);
  expect(panel().getByRole("textbox", { name: "关键词" })).toHaveValue(raw);
});
it("distinguishes the initial prompt, pending search and genuinely empty results", async () => {
  const pending = deferred<Response>(); reads.mockReturnValueOnce(pending.promise); open();
  expect(panel().getByText("输入关键词并提交搜索。")).toBeVisible(); search();
  expect(results().getByText("正在搜索历史消息…")).toBeVisible();
  expect(results().queryByText("未找到匹配的历史消息。")).not.toBeInTheDocument();
  await act(async () => pending.resolve(page()));
  expect(results().getByText("未找到匹配的历史消息。")).toBeVisible(); expect(details).not.toHaveBeenCalled();
});
it.each(["network", "server", "malformed"])("offers explicit search retry after %s failure using the submitted query", async failure => {
  if (failure === "network") reads.mockRejectedValueOnce(new TypeError("English network diagnostic"));
  else reads.mockResolvedValueOnce(failure === "server" ? json({ error: { message: "English backend diagnostic" } }, 503) : json({ items: null }));
  open(); search("已提交词");
  expect(await results().findByRole("alert")).not.toHaveTextContent("English");
  expect(results().queryByText("未找到匹配的历史消息。")).not.toBeInTheDocument();
  change("关键词", "尚未提交词"); scope("main");
  fireEvent.click(results().getByRole("button", { name: "重试搜索" }));
  await results().findByText("未找到匹配的历史消息。");
  expect(reads.mock.calls[1][0]).toBe(reads.mock.calls[0][0]); expect(reads).toHaveBeenCalledTimes(2);
});

it("labels bounded previews as excerpts, retains source IDs and reads complete authoritative content as plain text", async () => {
  const content = "<img src=x onerror=alert(1)>\n" + "逐行原文🙂".repeat(100) + "\n搜索记录末尾";
  const source = row("原始/消息?+#", content, "话题/原始");
  const full = { ...source, content: `${content}\n单条读取返回的最终一行 <b>不是 HTML</b>` };
  reads.mockResolvedValueOnce(page([source])); const pending = deferred<Response>(); details.mockReturnValueOnce(pending.promise);
  open(); search("原文");
  const button = await panel().findByRole("button", { name: `读取完整消息 ${source.id}` });
  const article = within(panel().getByRole("article", { name: `搜索结果 ${source.id}` }));
  expect(article.getByText("消息摘录")).toBeVisible(); expect(article.getByText(source.id)).toBeVisible();
  expect(article.getByText(group.id)).toBeVisible(); expect(article.getByText(source.topic_id)).toBeVisible();
  const excerpt = article.getByText(/^<img/); expect(excerpt.textContent!.length).toBeLessThan(content.length);
  expect(excerpt.textContent).toMatch(/…$/); expect(excerpt.textContent).not.toContain("\uFFFD");
  expect(panel().queryByText(/搜索记录末尾/)).not.toBeInTheDocument(); expect(details).not.toHaveBeenCalled();
  change("关键词", "未提交的新词"); scope("main"); button.focus(); expect(button).toHaveFocus(); fireEvent.click(button);
  expect(detail().getByRole("heading", { name: "完整消息" })).toBeVisible();
  expect(detail().getByText("正在读取完整消息…")).toBeVisible(); expect(detail().getByText(source.id)).toBeVisible();
  expect(detail().queryByText(full.content, { normalizer: text => text })).not.toBeInTheDocument();
  await act(async () => pending.resolve(json(full)));
  expect(detail().getByText(full.content, { normalizer: text => text })).toBeVisible();
  expect(screen.getByRole("region", { name: "历史消息搜索" }).querySelector("img, b")).toBeNull();
  expect(details.mock.calls[0][0]).toBe(`/api/v1/groups/${group.id}/history/%E5%8E%9F%E5%A7%8B%2F%E6%B6%88%E6%81%AF%3F%2B%23?topic_id=%E8%AF%9D%E9%A2%98%2F%E5%8E%9F%E5%A7%8B`);
});

it("keeps results and detail while paging, retries the same submitted scope and appends unique older results in server order", async () => {
  const topic = " \t话题 /?+ ", first = row("newest", "最新消息", topic), second = row("second", "第二条消息", topic), older = row("older", "更早消息", topic);
  reads.mockResolvedValueOnce(page([first, second], "opaque+/=?&")); details.mockResolvedValueOnce(json(first));
  open(); change("关键词", "旧词 %_\\ "); scope("topic"); change("话题 ID", topic); submit();
  fireEvent.click(await panel().findByRole("button", { name: `读取完整消息 ${first.id}` }));
  await detail().findByText(first.content);
  change("关键词", "未提交的新词"); scope("main");
  const summary = within(results().getByRole("list", { name: "已提交的检索条件" }));
  expect(summary.getByText("旧词 %_\\ ", { normalizer: text => text })).toBeVisible();
  expect(summary.getByText(topic, { normalizer: text => text })).toBeVisible();
  const pending = deferred<Response>(); reads.mockReturnValueOnce(pending.promise);
  fireEvent.click(results().getByRole("button", { name: "加载更早结果" }));
  expect(read(first.id)).toBeVisible(); expect(read(second.id)).toBeVisible(); expect(detail().getByText(first.content)).toBeVisible();
  expect(results().getByRole("button", { name: "加载更早结果" })).toBeDisabled();
  expect(params(1).get("q")).toBe("旧词 %_\\ "); expect(params(1).get("topic_id")).toBe(topic); expect(params(1).get("before")).toBe("opaque+/=?&");
  await act(async () => pending.resolve(json({}, 503)));
  expect(await results().findByRole("alert")).toHaveTextContent("503"); expect(read(first.id)).toBeVisible();
  change("关键词", "再次编辑"); scope("topic"); change("话题 ID", "另一个未提交话题");
  reads.mockResolvedValueOnce(page([second, older, older])); fireEvent.click(results().getByRole("button", { name: "重试搜索" }));
  await panel().findByRole("button", { name: `读取完整消息 ${older.id}` });
  expect(reads.mock.calls[2][0]).toBe(reads.mock.calls[1][0]);
  expect(within(results().getByRole("list", { name: "历史消息结果" })).getAllByRole("article").map(article => article.getAttribute("aria-label")))
    .toEqual(["搜索结果 newest", "搜索结果 second", "搜索结果 older"]);
  expect(results().queryByRole("button", { name: "加载更早结果" })).not.toBeInTheDocument(); expect(details).toHaveBeenCalledTimes(1);
  expect(detail().getByText(first.content)).toBeVisible();
});

it.each([200, 503])("a new submission resets results, cursor and detail and ignores canceled page/detail HTTP %s", async status => {
  const old = row("old", "旧检索结果", "old-topic"), fresh = row("fresh", "新的检索结果");
  reads.mockResolvedValueOnce(page([old], "old-cursor")); open(); search("旧词");
  const staleDetail = deferred<Response>(); details.mockReturnValueOnce(staleDetail.promise);
  fireEvent.click(await panel().findByRole("button", { name: `读取完整消息 ${old.id}` }));
  const stalePage = deferred<Response>(); reads.mockReturnValueOnce(stalePage.promise);
  fireEvent.click(results().getByRole("button", { name: "加载更早结果" }));
  const current = deferred<Response>(); reads.mockReturnValueOnce(current.promise); scope("main"); search("新词");
  expect(reads.mock.calls[1][1]?.signal?.aborted).toBe(true); expect(details.mock.calls[0][1]?.signal?.aborted).toBe(true);
  expect(panel().queryByRole("button", { name: `读取完整消息 ${old.id}` })).not.toBeInTheDocument();
  expect(panel().queryByRole("region", { name: "完整消息" })).not.toBeInTheDocument();
  expect(params(2).get("q")).toBe("新词"); expect(params(2).get("topic_id")).toBe(""); expect(params(2).has("before")).toBe(false);
  await act(async () => current.resolve(page([fresh])));
  await act(async () => {
    stalePage.resolve(status === 200 ? page([old], "stale-cursor") : json({}, status));
    staleDetail.resolve(status === 200 ? json({ ...old, content: "过期的完整消息" }) : json({}, status));
  });
  expect(read(fresh.id)).toBeVisible(); expect(results().queryByRole("alert")).not.toBeInTheDocument();
  expect(panel().queryByText("过期的完整消息")).not.toBeInTheDocument();
  expect(results().queryByRole("button", { name: "加载更早结果" })).not.toBeInTheDocument();
});
it.each([200, 503])("ignores an older same-group search HTTP %s even when the same query is submitted again", async status => {
  const stale = deferred<Response>(); reads.mockReturnValueOnce(stale.promise); open(); search("重复词");
  const current = deferred<Response>(); reads.mockReturnValueOnce(current.promise); submit();
  expect(reads.mock.calls[0][1]?.signal?.aborted).toBe(true);
  const fresh = row("fresh", "本次搜索结果"); await act(async () => current.resolve(page([fresh])));
  await act(async () => stale.resolve(status === 200 ? page([message], "old-cursor") : json({}, status)));
  expect(read(fresh.id)).toBeVisible(); expect(results().queryByRole("alert")).not.toBeInTheDocument();
  expect(panel().queryByRole("button", { name: `读取完整消息 ${message.id}` })).not.toBeInTheDocument();
});

it.each([200, 503])("resets A→B→A immediately and ignores old group page/detail and intervening search HTTP %s", async status => {
  const a = row("a-original", "A 的旧结果"), fresh = row("a-fresh", "A 返回后的结果");
  reads.mockResolvedValueOnce(page([a], "a-cursor")); const view = open(); search("同一个词");
  const staleDetail = deferred<Response>(); details.mockReturnValueOnce(staleDetail.promise);
  fireEvent.click(await panel().findByRole("button", { name: `读取完整消息 ${a.id}` }));
  const stalePage = deferred<Response>(); reads.mockReturnValueOnce(stalePage.promise);
  fireEvent.click(results().getByRole("button", { name: "加载更早结果" }));
  view.rerender(<HistorySearch group={other} />);
  expect(panel().queryByRole("region", { name: "历史搜索结果" })).not.toBeInTheDocument();
  expect(panel().queryByRole("region", { name: "完整消息" })).not.toBeInTheDocument();
  expect(panel().getByRole("textbox", { name: "关键词" })).toHaveValue(""); expect(reads).toHaveBeenCalledTimes(2);
  const staleB = deferred<Response>(); reads.mockReturnValueOnce(staleB.promise); search("B 的词");
  view.rerender(<HistorySearch group={group} />);
  expect(panel().queryByRole("region", { name: "历史搜索结果" })).not.toBeInTheDocument();
  expect(reads).toHaveBeenCalledTimes(3);
  reads.mockResolvedValueOnce(page([fresh])); search("同一个词"); await panel().findByRole("button", { name: `读取完整消息 ${fresh.id}` });
  expect(params(3).has("before")).toBe(false);
  for (const index of [1, 2]) expect(reads.mock.calls[index][1]?.signal?.aborted).toBe(true);
  expect(details.mock.calls[0][1]?.signal?.aborted).toBe(true);
  await act(async () => {
    stalePage.resolve(status === 200 ? page([a], "a-stale") : json({}, status));
    staleDetail.resolve(status === 200 ? json(a) : json({}, status));
    staleB.resolve(status === 200 ? page([{ ...message, group_id: other.id }]) : json({}, status));
  });
  expect(read(fresh.id)).toBeVisible(); expect(panel().queryByRole("alert")).not.toBeInTheDocument();
  expect(panel().queryByRole("region", { name: "完整消息" })).not.toBeInTheDocument();
  expect(results().getAllByRole("article")).toHaveLength(1);
});

it.each([200, 503])("keeps detail bound to the selected result when a previous selection returns late HTTP %s", async status => {
  const a = row("selection-a", "A 的摘录"), b = row("selection-b", "B 的摘录", " \t ");
  reads.mockResolvedValueOnce(page([a, b])); const stale = deferred<Response>(); details.mockReturnValueOnce(stale.promise); open(); search();
  fireEvent.click(await panel().findByRole("button", { name: `读取完整消息 ${a.id}` }));
  const current = deferred<Response>(); details.mockReturnValueOnce(current.promise); fireEvent.click(read(b.id));
  expect(details.mock.calls[0][1]?.signal?.aborted).toBe(true); expect(detail().getByText(b.id)).toBeVisible();
  expect(detail().queryByText(a.id)).not.toBeInTheDocument();
  expect(details.mock.calls[0][0]).toBe(`/api/v1/groups/${group.id}/history/${a.id}?topic_id=`);
  expect(new URL(String(details.mock.calls[1][0]), "http://localhost").searchParams.get("topic_id")).toBe(b.topic_id);
  await act(async () => current.resolve(json({ ...b, content: "B 的完整消息" })));
  await act(async () => stale.resolve(status === 200 ? json({ ...a, content: "过期 A 全文" }) : json({}, status)));
  expect(detail().getByText("B 的完整消息")).toBeVisible(); expect(detail().queryByText("过期 A 全文")).not.toBeInTheDocument();
  expect(detail().queryByRole("alert")).not.toBeInTheDocument();
});
it("retries a failed complete read for its original result without using the search content as full detail", async () => {
  reads.mockResolvedValueOnce(page([message])); details.mockResolvedValueOnce(json({}, 404)); open(); search();
  fireEvent.click(await panel().findByRole("button", { name: `读取完整消息 ${message.id}` }));
  expect(await detail().findByRole("alert")).toHaveTextContent("404");
  expect(detail().queryByText(message.content)).not.toBeInTheDocument(); expect(detail().getByText(message.id)).toBeVisible();
  scope("topic"); change("话题 ID", "未提交话题"); change("关键词", "未提交关键词");
  details.mockResolvedValueOnce(json({ ...message, content: "重试读取的完整原文" }));
  fireEvent.click(detail().getByRole("button", { name: "重试读取完整消息" }));
  await detail().findByText("重试读取的完整原文");
  expect(details.mock.calls[1][0]).toBe(details.mock.calls[0][0]); expect(reads).toHaveBeenCalledTimes(1);
});
it("aborts page and detail on unmount and ignores late responses when transport does not honor abort", async () => {
  reads.mockResolvedValueOnce(page([message], "next")); const view = open(); search();
  const pendingDetail = deferred<Response>(); details.mockReturnValueOnce(pendingDetail.promise);
  fireEvent.click(await panel().findByRole("button", { name: `读取完整消息 ${message.id}` }));
  const pendingPage = deferred<Response>(); reads.mockReturnValueOnce(pendingPage.promise);
  fireEvent.click(results().getByRole("button", { name: "加载更早结果" })); view.unmount();
  expect(reads.mock.calls[1][1]?.signal?.aborted).toBe(true); expect(details.mock.calls[0][1]?.signal?.aborted).toBe(true);
  await act(async () => { pendingPage.resolve(page([message])); pendingDetail.resolve(json(message)); });
  expect(screen.queryByRole("region", { name: "历史消息搜索" })).not.toBeInTheDocument(); expect(transport).toHaveBeenCalledTimes(3);
});
