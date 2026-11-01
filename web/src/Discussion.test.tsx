import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { App } from "./App";
import { deferred, group, json, other, page } from "./test/groups";
import { member } from "./test/members";
import { delivery, message } from "./test/messages";
const reads = vi.fn<typeof fetch>(), details = vi.fn<typeof fetch>(), writes = vi.fn<typeof fetch>();
const main = () => screen.getByRole("main", { name: "讨论" });
const panel = () => within(within(main()).queryByRole("form", { name: "发送消息" })?.parentElement ?? main());
const content = () => panel().getByRole("textbox", { name: "消息内容" });
const change = (value: string) => fireEvent.change(content(), { target: { value } });
const submit = () => fireEvent.submit(panel().getByRole("form", { name: "发送消息" }));
const select = (name: string) => fireEvent.click(screen.getByRole("button", { name }));
const article = (id = message.id) => panel().getByRole("article", { name: `消息 ${id}` });
const route = (value: string) => fireEvent.change(panel().getByRole("combobox", { name: "接收方式" }), { target: { value } });
// Native textarea values use LF; received records may still contain CRLF.
const raw = "  <script>window.bad = true</script>\n@研究员\n原文尾部  ";
async function open() { render(<App />); select((await screen.findByRole("button", { name: group.name })).textContent!); }
beforeEach(() => {
  reads.mockReset().mockImplementation(async () => page());
  details.mockReset().mockImplementation(async () => json(message));
  writes.mockReset().mockImplementation(async (_url, init) => {
    const input = JSON.parse(String(init?.body));
    return json({ ...message, content: input.content, deliveries: input.recipient_ids?.length === 0 ? [] : [delivery] }, 201);
  });
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url.includes("/messages")) return (init?.method === "POST" ? writes : /\/messages\//.test(url) ? details : reads)(input, init);
    if (url.includes("/members")) return Promise.resolve(page(url.includes(other.id) ? [] : [member, { ...member, id: "member-2", role: "复核" }]));
    return Promise.resolve(url.startsWith("/api/v1/providers") ? page() : page([group, other]));
  });
});
afterEach(() => vi.unstubAllGlobals());
it("keeps discussion inactive before selection, then distinguishes loading, failure and real emptiness", async () => {
  render(<App />); await screen.findByRole("button", { name: group.name });
  expect(panel().getByText("请先选择群组，再查看讨论和发送消息。")).toBeVisible();
  expect(panel().queryByRole("form")).not.toBeInTheDocument(); expect(reads).not.toHaveBeenCalled();
  const pending = deferred<Response>(); reads.mockReturnValueOnce(pending.promise); select(group.name);
  expect(panel().getByText("正在载入消息…")).toBeVisible();
  await act(async () => pending.resolve(json({}, 503)));
  expect(panel().getByRole("alert")).toHaveTextContent("503");
  fireEvent.click(panel().getByRole("button", { name: "重试载入消息" }));
  expect(await panel().findByText("尚无消息，可发送第一条讨论。")).toBeVisible();
});
it("renders raw text, topic, sender and honest delivery labels without executing markup", async () => {
  const received = raw.replace("@研究员\n", "@研究员\r\n");
  reads.mockResolvedValueOnce(page([{ ...message, content: received, topic_id: "topic-A", deliveries: ["queued", "accepted", "failed", "future"].map((state, index) => ({ ...delivery, id: `d-${index}`, state })) }]));
  await open(); await panel().findByRole("article");
  expect(article().querySelector(".message-content")?.textContent).toBe(received);
  expect(article().querySelector("script")).toBeNull(); expect(article()).toHaveTextContent("话题消息 · topic-A");
  for (const label of ["发送者：user", "已入队", "CLI已接收", "投递失败", "状态未知"]) expect(article()).toHaveTextContent(label);
  expect(panel().getByText(/CLI接收不代表轮次完成、任务提交或结果接受/)).toBeVisible();
  expect(panel().getByText(/已加载片段可能存在间隔/)).toBeVisible();
});
it("pages only explicitly, deduplicates and updates server values, sorts nanoseconds, and resets the history window", async () => {
  const earlier = { ...message, id: "z-earlier", content: "纳秒较早", created_at: "2026-10-30T12:00:00.000000001Z", deliveries: [] };
  const later = { ...message, id: "a-later", content: "纳秒较晚", created_at: "2026-10-30T12:00:00.000000002Z", deliveries: [] };
  reads.mockResolvedValueOnce(page([later, earlier], "opaque+/")); await open(); await panel().findByRole("article", { name: `消息 ${later.id}` });
  expect(panel().getAllByRole("article").map(node => node.getAttribute("aria-label"))).toEqual([`消息 ${earlier.id}`, `消息 ${later.id}`]);
  expect(reads).toHaveBeenCalledTimes(1);
  reads.mockResolvedValueOnce(page([earlier, message])); fireEvent.click(panel().getByRole("button", { name: "加载更早消息" }));
  await panel().findByRole("article", { name: `消息 ${message.id}` }); expect(panel().getAllByRole("article")).toHaveLength(3);
  expect(reads.mock.calls[1][0]).toBe(`/api/v1/groups/${group.id}/messages?limit=50&before=opaque%2B%2F`);
  reads.mockResolvedValueOnce(page([{ ...message, deliveries: [{ ...delivery, state: "accepted" }] }]));
  fireEvent.click(panel().getByRole("button", { name: "刷新并重置历史窗口" }));
  await panel().findByText(/CLI已接收/); expect(panel().getAllByRole("article")).toHaveLength(1);
});
it.each(["default", "public", "selected"])("sends raw content with exact %s routing from actual scoped members", async mode => {
  await open(); await panel().findByText("尚无消息，可发送第一条讨论。"); change(raw); route(mode);
  if (mode === "selected") fireEvent.click(await panel().findByRole("checkbox", { name: `${member.name} · ${member.role} · ${member.id}` }));
  submit(); expect(await panel().findByText(/消息已持久化/)).toBeVisible();
  expect(JSON.parse(String(writes.mock.calls[0][1]?.body))).toEqual({ content: raw, ...(mode === "default" ? {} : { recipient_ids: mode === "public" ? [] : [member.id] }) });
  expect(content()).toHaveValue(""); expect(article().querySelector(".message-content")?.textContent).toBe(raw);
});
it("requires nonblank content and explicit valid selected recipients, and never posts optimistically or twice", async () => {
  await open(); change(" \n "); submit(); expect(panel().getByRole("alert")).toHaveTextContent("请输入消息内容");
  change(raw); route("selected"); submit(); expect(panel().getByRole("alert")).toHaveTextContent("请选择本群已载入的在群成员");
  fireEvent.click(await panel().findByRole("checkbox", { name: `${member.name} · ${member.role} · ${member.id}` }));
  const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise); submit(); submit();
  expect(writes).toHaveBeenCalledTimes(1); expect(content()).toBeDisabled(); expect(panel().queryByRole("article")).not.toBeInTheDocument();
  await act(async () => pending.resolve(json({ ...message, content: raw }, 201)));
  expect(article()).toHaveTextContent("已入队"); expect(content()).toHaveValue("");
});
it.each(["network", 503, 200, 202, "malformed", "foreign", "topic"])("freezes unknown %s operations across A to B to A with identical retry", async failure => {
  await open(); change(raw); route("selected");
  fireEvent.click(await panel().findByRole("checkbox", { name: `${member.name} · ${member.role} · ${member.id}` }));
  if (failure === "network") writes.mockRejectedValueOnce(new TypeError("offline"));
  else writes.mockResolvedValueOnce(json(failure === "malformed" ? {} : failure === "foreign" ? { ...message, group_id: other.id } : failure === "topic" ? { ...message, topic_id: "wrong" } : message, typeof failure === "number" ? failure : 201));
  submit(); expect(await panel().findByRole("alert")).toHaveTextContent("发送结果未确认"); expect(content()).toBeDisabled();
  select(other.name); await panel().findByText("尚无消息，可发送第一条讨论。"); change("B 的草稿"); route("public");
  expect(panel().queryByRole("alert")).not.toBeInTheDocument(); select(group.name);
  expect(content()).toHaveValue(raw); fireEvent.click(panel().getByRole("button", { name: "重试同一次发送" }));
  await panel().findByText(/消息已持久化/);
  const attempts = writes.mock.calls.map(([url, init]) => [url, init?.body, new Headers(init?.headers).get("Idempotency-Key")]);
  expect(attempts[1]).toEqual(attempts[0]); expect(attempts[0][0]).toBe(`/api/v1/groups/${group.id}/messages`);
  select(other.name); expect(content()).toHaveValue("B 的草稿");
  expect(panel().getByRole("combobox", { name: "接收方式" })).toHaveValue("public");
});
it.each([413, 415])("retains an editable draft after HTTP %s and uses a new key for corrected content", async status => {
  await open(); change(raw.repeat(50)); writes.mockResolvedValueOnce(json({}, status)); submit();
  expect(await panel().findByRole("alert")).toHaveTextContent(String(status)); expect(content()).toBeEnabled(); expect(content()).toHaveValue(raw.repeat(50));
  change("缩短的内容"); submit(); await panel().findByText(/消息已持久化/);
  expect(writes.mock.calls[1][1]?.body).toBe(JSON.stringify({ content: "缩短的内容" }));
  expect(new Headers(writes.mock.calls[1][1]?.headers).get("Idempotency-Key")).not.toBe(new Headers(writes.mock.calls[0][1]?.headers).get("Idempotency-Key"));
});
it.each([false, true])("protects a POST receipt from an earlier active GET (empty=%s), then accepts fresh detail data", async empty => {
  const old = deferred<Response>(); reads.mockReturnValueOnce(old.promise); await open(); change(raw); submit();
  await panel().findByText(/消息已持久化/);
  await act(async () => old.resolve(page(empty ? [] : [{ ...message, content: "旧快照" }])));
  expect(article().querySelector(".message-content")?.textContent).toBe(raw);
  details.mockResolvedValueOnce(json({ ...message, content: raw, deliveries: [{ ...delivery, state: "accepted" }] }));
  fireEvent.click(panel().getByRole("button", { name: `刷新消息 ${message.id} 投递` }));
  await panel().findByText(/CLI已接收/);
});
it("keeps late A receipts out of B and protects creation from an earlier empty GET", async () => {
  const old = deferred<Response>(); reads.mockReturnValueOnce(old.promise); await open(); change(raw);
  const receipt = deferred<Response>(); writes.mockReturnValueOnce(receipt.promise); submit(); select(other.name);
  await panel().findByText("尚无消息，可发送第一条讨论。"); change("B draft");
  await act(async () => receipt.resolve(json({ ...message, content: raw }, 201)));
  expect(content()).toHaveValue("B draft"); expect(panel().queryByRole("article")).not.toBeInTheDocument();
  reads.mockResolvedValueOnce(page([{ ...message, content: raw }])); select(group.name); await panel().findByRole("article");
  await act(async () => old.resolve(page())); expect(article()).toHaveTextContent(raw.trim().split("\n")[0]);
});
it("does not let an older page downgrade a newer detail, nor an older detail downgrade a new latest read", async () => {
  reads.mockResolvedValueOnce(page([message], "older")); await open(); await panel().findByRole("article");
  const oldPage = deferred<Response>(); reads.mockReturnValueOnce(oldPage.promise);
  fireEvent.click(panel().getByRole("button", { name: "加载更早消息" }));
  details.mockResolvedValueOnce(json({ ...message, deliveries: [{ ...delivery, state: "accepted" }] }));
  fireEvent.click(panel().getByRole("button", { name: `刷新消息 ${message.id} 投递` })); await panel().findByText(/CLI已接收/);
  await act(async () => oldPage.resolve(page([message]))); expect(article()).toHaveTextContent("CLI已接收");
  const oldDetail = deferred<Response>(); details.mockReturnValueOnce(oldDetail.promise);
  fireEvent.click(panel().getByRole("button", { name: `刷新消息 ${message.id} 投递` }));
  reads.mockResolvedValueOnce(page([{ ...message, deliveries: [{ ...delivery, state: "failed" }] }]));
  fireEvent.click(panel().getByRole("button", { name: "刷新并重置历史窗口" })); await panel().findByText(/投递失败/);
  await act(async () => oldDetail.resolve(json(message))); expect(article()).toHaveTextContent("投递失败");
});
it("confirms a late POST without downgrading data read after that attempt began", async () => {
  await open(); await panel().findByText("尚无消息，可发送第一条讨论。"); change(raw);
  const receipt = deferred<Response>(); writes.mockReturnValueOnce(receipt.promise); submit();
  reads.mockResolvedValueOnce(page([{ ...message, content: raw }]));
  fireEvent.click(panel().getByRole("button", { name: "刷新并重置历史窗口" })); await panel().findByRole("article");
  details.mockResolvedValueOnce(json({ ...message, content: raw, deliveries: [{ ...delivery, state: "accepted" }] }));
  fireEvent.click(panel().getByRole("button", { name: `刷新消息 ${message.id} 投递` })); await panel().findByText(/CLI已接收/);
  await act(async () => receipt.resolve(json({ ...message, content: raw }, 201)));
  expect(article()).toHaveTextContent("CLI已接收"); expect(article()).not.toHaveTextContent("已入队");
  expect(content()).toHaveValue(""); expect(panel().getByText(/消息已持久化/)).toBeVisible();
});
it("inserts a late 201 receipt even after an empty reset begun during its attempt", async () => {
  await open(); await panel().findByText("尚无消息，可发送第一条讨论。"); change(raw);
  const receipt = deferred<Response>(); writes.mockReturnValueOnce(receipt.promise); submit();
  fireEvent.click(panel().getByRole("button", { name: "刷新并重置历史窗口" }));
  await panel().findByText("尚无消息，可发送第一条讨论。");
  const olderRead = deferred<Response>(); reads.mockReturnValueOnce(olderRead.promise);
  fireEvent.click(panel().getByRole("button", { name: "刷新并重置历史窗口" }));
  await act(async () => receipt.resolve(json({ ...message, content: raw }, 201)));
  await act(async () => olderRead.resolve(page()));
  expect(article()).toHaveTextContent("已入队"); expect(content()).toHaveValue("");
});
it.each([["history", "receipt-first"], ["history", "read-first"], ["detail", "receipt-first"], ["detail", "read-first"]])(
  "arrival matrix: post-attempt %s accepted data wins with %s", async (kind, arrival) => {
    await open(); await panel().findByText("尚无消息，可发送第一条讨论。"); change(raw);
    const receipt = deferred<Response>(), read = deferred<Response>();
    if (kind === "detail") {
      writes.mockRejectedValueOnce(new TypeError("offline")); submit(); await panel().findByRole("alert");
      reads.mockResolvedValueOnce(page([{ ...message, content: raw }]));
      fireEvent.click(panel().getByRole("button", { name: "刷新并重置历史窗口" })); await panel().findByRole("article");
      writes.mockReturnValueOnce(receipt.promise); fireEvent.click(panel().getByRole("button", { name: "重试同一次发送" }));
      details.mockReturnValueOnce(read.promise);
      fireEvent.click(panel().getByRole("button", { name: `刷新消息 ${message.id} 投递` }));
    } else {
      writes.mockReturnValueOnce(receipt.promise); submit(); reads.mockReturnValueOnce(read.promise);
      fireEvent.click(panel().getByRole("button", { name: "刷新并重置历史窗口" }));
    }
    const accepted = { ...message, content: raw, deliveries: [{ ...delivery, state: "accepted" }] };
    const resolveRead = () => act(async () => read.resolve(kind === "history" ? page([accepted]) : json(accepted)));
    const resolveReceipt = () => act(async () => receipt.resolve(json({ ...message, content: raw }, 201)));
    if (arrival === "read-first") { await resolveRead(); await resolveReceipt(); }
    else { await resolveReceipt(); await resolveRead(); }
    expect(article()).toHaveTextContent("CLI已接收"); expect(article()).not.toHaveTextContent("已入队");
    expect(content()).toHaveValue(""); expect(panel().getByText(/消息已持久化/)).toBeVisible();
  },
);
it("preserves an already-read accepted row when an idempotent retry returns its stale queued receipt", async () => {
  await open(); await panel().findByText("尚无消息，可发送第一条讨论。"); change(raw);
  writes.mockRejectedValueOnce(new TypeError("offline")); submit(); await panel().findByRole("alert");
  reads.mockResolvedValueOnce(page([{ ...message, content: raw, deliveries: [{ ...delivery, state: "accepted" }] }]));
  fireEvent.click(panel().getByRole("button", { name: "刷新并重置历史窗口" })); await panel().findByText(/CLI已接收/);
  fireEvent.click(panel().getByRole("button", { name: "重试同一次发送" })); await panel().findByText(/消息已持久化/);
  expect(article()).toHaveTextContent("CLI已接收");
  const attempts = writes.mock.calls.map(([, init]) => [init?.body, new Headers(init?.headers).get("Idempotency-Key")]);
  expect(attempts[1]).toEqual(attempts[0]);
});
it("does not resurrect a row outside a reset window from an earlier detail read", async () => {
  reads.mockResolvedValueOnce(page([message])); await open(); await panel().findByRole("article");
  const old = deferred<Response>(); details.mockReturnValueOnce(old.promise);
  fireEvent.click(panel().getByRole("button", { name: `刷新消息 ${message.id} 投递` }));
  reads.mockResolvedValueOnce(page([{ ...message, id: "new-window", content: "新窗口", deliveries: [] }]));
  fireEvent.click(panel().getByRole("button", { name: "刷新并重置历史窗口" }));
  await panel().findByRole("article", { name: "消息 new-window" }); await act(async () => old.resolve(json(message)));
  expect(panel().getAllByRole("article")).toHaveLength(1);
});
it("ignores an obsolete A read after A to B to A and aborts reads and POST on full unmount", async () => {
  const old = deferred<Response>(); reads.mockReturnValueOnce(old.promise); const view = render(<App />);
  select((await screen.findByRole("button", { name: group.name })).textContent!); select(other.name);
  await panel().findByText("尚无消息，可发送第一条讨论。"); reads.mockResolvedValueOnce(page([{ ...message, content: "新的 A" }]));
  select(group.name); await panel().findByRole("article"); await act(async () => old.resolve(page([{ ...message, content: "旧的 A" }])));
  expect(article()).toHaveTextContent("新的 A"); expect(reads.mock.calls[0][1]?.signal?.aborted).toBe(true);
  const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise); change(raw); submit();
  const detail = deferred<Response>(); details.mockReturnValueOnce(detail.promise); fireEvent.click(panel().getByRole("button", { name: `刷新消息 ${message.id} 投递` }));
  view.unmount(); expect(writes.mock.calls[0][1]?.signal?.aborted).toBe(true); expect(details.mock.calls[0][1]?.signal?.aborted).toBe(true);
  await act(async () => { pending.resolve(json(message, 201)); detail.resolve(json(message)); });
});
