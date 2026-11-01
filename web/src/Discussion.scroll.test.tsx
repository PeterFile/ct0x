import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { Message } from "./api";
import { Discussion } from "./Discussion";
import { deferred, group, json, other, page } from "./test/groups";
import { delivery, message } from "./test/messages";

const reads = vi.fn<typeof fetch>(), details = vi.fn<typeof fetch>(), writes = vi.fn<typeof fetch>();
const row = (index: number, groupId = group.id): Message => ({ ...message, group_id: groupId,
  id: `${groupId}-${String(index).padStart(2, "0")}`, content: `真实消息 ${groupId} ${index}`,
  created_at: `2026-10-30T12:00:${String(index).padStart(2, "0")}Z`, deliveries: [] });
const initial = Array.from({ length: 8 }, (_, index) => row(index + 1));
const latestURL = (id = group.id) => `/api/v1/groups/${id}/messages?limit=50`;
const olderURL = `${latestURL()}&before=older%2B%2F`;
const raw = "  公开原文\n保留空白  ";
const resetNotice = "历史窗口已更新，可加载更早消息";
const flush = () => act(async () => { await vi.advanceTimersByTimeAsync(0); });
const poll = () => act(async () => { await vi.advanceTimersByTimeAsync(5000); });
const scope = (id = group.id) => {
  const record = screen.getByRole("article", { name: `消息 ${row(3, id).id}`, hidden: true });
  return record.closest<HTMLElement>(".discussion-scope")!;
};
const history = (id = group.id) => scope(id).querySelector<HTMLElement>(".message-history")!;
const item = (id: string) => screen.getByRole("article", { name: `消息 ${id}`, hidden: true }).closest<HTMLLIElement>("li")!;
const button = (name: string, id = group.id) => within(scope(id)).getByRole("button", { name });
const recordId = (element: Element) => element.querySelector("article")!.getAttribute("aria-label")!.slice(3);
const rect = (top: number, height: number) => ({ top, bottom: top + height, left: 0, right: 320,
  width: 320, height, x: 0, y: top, toJSON: () => ({}) }) as DOMRect;

// These are controlled coordinates, not jsdom layout or real-browser acceptance.
const tops = new WeakMap<HTMLElement, number>();
const heights = new Map<string, number>();
let header = 80, footer = 20, measurements = 0, scrollWrites = 0, gutterWidth = 16;
let nativeShift: { count: number; delta: number } | undefined;
const hidden = (element: Element) => Boolean(element.closest("[hidden]"));
const rows = (element: HTMLElement) => [...element.querySelectorAll(".message-list > li")];
const heightOf = (element: Element) => heights.get(recordId(element)) ?? 100;
const hasComposerNotice = (element: HTMLElement) => [...(element.closest(".discussion-scope")?.querySelectorAll(".message-composer .feed-note") ?? [])]
  .some(note => note.textContent === resetNotice);
const viewportHeight = (element: HTMLElement) => 200 - (hasComposerNotice(element) ? 40 : 0);
const extent = (element: HTMLElement) => header + rows(element).reduce((sum, child) => sum + heightOf(child), 0) + footer;
function readAt(element: HTMLElement, top: number) {
  fireEvent.wheel(element, { deltaY: top - element.scrollTop });
  element.scrollTop = top;
  fireEvent.scroll(element);
}
function offset(id: string) {
  const node = item(id), container = node.closest<HTMLElement>(".message-history")!;
  return node.getBoundingClientRect().top - container.getBoundingClientRect().top;
}
function expectVisible(id: string) {
  const node = item(id), container = node.closest<HTMLElement>(".message-history")!;
  const box = node.getBoundingClientRect(), viewport = container.getBoundingClientRect();
  expect(box.top).toBeGreaterThanOrEqual(viewport.top);
  expect(box.bottom).toBeLessThanOrEqual(viewport.bottom);
}
function expectTail(element = history()) {
  expect(element.scrollTop).toBe(element.scrollHeight - element.clientHeight);
}
function expectRequests(urls: string[]) {
  expect(vi.mocked(fetch).mock.calls.map(([url]) => String(url))).toEqual(urls);
}
async function open(items = initial, next = "older+/") {
  reads.mockResolvedValueOnce(page(items, next));
  const view = render(<Discussion groupId={group.id} members={{}} />);
  await flush();
  return view;
}
function startReading(input: string, container = history()) {
  if (input === "wheel") fireEvent.wheel(container, { deltaY: -470 });
  else if (input === "keyboard") fireEvent.keyDown(container, { key: "PageUp" });
  else if (input === "touch") fireEvent.touchMove(container);
  else {
    expect(container.offsetWidth).toBe(320); expect(container.clientLeft).toBe(0); expect(container.clientWidth).toBe(304);
    fireEvent.pointerDown(container, { pointerId: 1, clientX: 312, clientY: 180 });
  }
}
function endReading(input: string, container = history()) {
  if (input === "keyboard") fireEvent.keyUp(container, { key: "PageUp" });
  else if (input === "touch") fireEvent.touchEnd(container);
  else if (input === "scrollbar") fireEvent.pointerUp(container, { pointerId: 1, clientX: 312, clientY: 180 });
}
async function openFallback() {
  const current = initial.slice(2), view = await open(current);
  reads.mockResolvedValueOnce(page(initial.slice(0, 2), "older+/")); fireEvent.click(button("加载更早消息")); await flush();
  readAt(history(), 100);
  view.rerender(<Discussion groupId={undefined} members={{}} />);
  reads.mockResolvedValueOnce(page(current, "older+/")); view.rerender(<Discussion groupId={group.id} members={{}} />); await flush();
  return view;
}
function send() {
  const panel = within(scope());
  fireEvent.change(panel.getByRole("textbox", { name: "消息内容" }), { target: { value: raw } });
  fireEvent.change(panel.getByRole("combobox", { name: "接收方式" }), { target: { value: "public" } });
  fireEvent.submit(panel.getByRole("form", { name: "发送消息" }));
}

beforeEach(() => {
  vi.useFakeTimers(); heights.clear(); header = 80; footer = 20; measurements = 0; scrollWrites = 0; gutterWidth = 16; nativeShift = undefined;
  reads.mockReset().mockImplementation(async url => page(String(url).includes(other.id)
    ? initial.map((_, index) => row(index + 1, other.id)) : initial, "older+/"));
  details.mockReset().mockResolvedValue(json({ ...initial[7], deliveries: [{ ...delivery, message_id: initial[7].id, state: "accepted" }] }));
  writes.mockReset().mockResolvedValue(json({ ...row(9), content: raw }, 201));
  vi.stubGlobal("fetch", vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    if (!String(input).startsWith("/api/v1/groups/") || !String(input).includes("/messages")) throw new Error("Unexpected endpoint");
    return (init?.method === "POST" ? writes : /\/messages\//.test(String(input)) ? details : reads)(input, init);
  }));
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockImplementation(function (this: HTMLElement) {
    return this.matches(".message-history") && !hidden(this) ? viewportHeight(this) : 0;
  });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockImplementation(function (this: HTMLElement) {
    return this.matches(".message-history") && !hidden(this) ? 320 - gutterWidth : 0;
  });
  vi.spyOn(HTMLElement.prototype, "clientLeft", "get").mockReturnValue(0);
  vi.spyOn(HTMLElement.prototype, "offsetWidth", "get").mockImplementation(function (this: HTMLElement) {
    return this.matches(".message-history") && !hidden(this) ? 320 : 0;
  });
  vi.spyOn(HTMLElement.prototype, "scrollHeight", "get").mockImplementation(function (this: HTMLElement) {
    return this.matches(".message-history") && !hidden(this) ? extent(this) : 0;
  });
  vi.spyOn(Element.prototype, "scrollTop", "get").mockImplementation(function (this: HTMLElement) {
    if (hidden(this)) return 0;
    if (nativeShift && rows(this).length === nativeShift.count) {
      tops.set(this, (tops.get(this) ?? 0) + nativeShift.delta); nativeShift = undefined;
    }
    return tops.get(this) ?? 0;
  });
  vi.spyOn(Element.prototype, "scrollTop", "set").mockImplementation(function (this: HTMLElement, value: number) {
    ++scrollWrites;
    tops.set(this, Math.max(0, Math.min(value, extent(this) - viewportHeight(this))));
  });
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
    if (hidden(this)) return rect(0, 0);
    if (this.matches(".message-history")) return rect(100, viewportHeight(this));
    const composer = this.closest<HTMLElement>(".message-composer");
    if (composer) {
      const container = composer.closest(".discussion-scope")!.querySelector<HTMLElement>(".message-history")!;
      const top = 100 + viewportHeight(container);
      return this === composer ? rect(top, hasComposerNotice(container) ? 140 : 100) : rect(top + 8, 32);
    }
    const node = this.matches(".message-list > li") ? this : this.closest<HTMLElement>(".message-list > li");
    if (!node) return rect(0, 0);
    ++measurements;
    const container = node.closest<HTMLElement>(".message-history")!;
    let top = 100 + header - container.scrollTop;
    for (const sibling of rows(container)) {
      if (sibling === node) break;
      top += heightOf(sibling);
    }
    return rect(top, heightOf(node));
  });
});
afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers(); vi.unstubAllGlobals(); });

it("initializes the first successful window at the latest message, but not on ordinary rerenders", async () => {
  const pending = deferred<Response>(); reads.mockReturnValueOnce(pending.promise);
  const view = render(<Discussion groupId={group.id} members={{}} />);
  await flush(); expect(screen.getByText("正在载入消息…")).toBeVisible();
  expect(scrollWrites).toBe(0);
  await act(async () => pending.resolve(page(initial, "older+/")));
  expectTail(); expectVisible(row(8).id);
  readAt(history(), 230); view.rerender(<Discussion groupId={group.id} members={{}} />);
  expect(history().scrollTop).toBe(230); expectRequests([latestURL()]);
});

it("does not consume initialization on a failed load; a successful retry reaches the latest", async () => {
  reads.mockResolvedValueOnce(json({}, 503));
  render(<Discussion groupId={group.id} members={{}} />); await flush();
  expect(screen.getByRole("alert")).toHaveTextContent("503"); expect(scrollWrites).toBe(0);
  reads.mockResolvedValueOnce(page(initial)); fireEvent.click(screen.getByRole("button", { name: "重试载入消息" }));
  await flush(); expectTail(); expectVisible(row(8).id); expectRequests([latestURL(), latestURL()]);
});

it("does not invent content or consume initialization for an empty successful window", async () => {
  await open([]); expect(screen.getByText("尚无消息，可发送第一条讨论。")).toBeVisible();
  expect(scrollWrites).toBe(0); reads.mockResolvedValueOnce(page(initial)); await poll();
  expectTail(); expectVisible(row(8).id); expectRequests([latestURL(), latestURL()]);
});

it("follows a new real ID when previously within a small tail tolerance", async () => {
  await open(); readAt(history(), history().scrollHeight - history().clientHeight - 8);
  const input = within(scope()).getByRole("textbox"); input.focus();
  reads.mockResolvedValueOnce(page([...initial, row(9)])); await poll();
  expectTail(); expectVisible(row(9).id); expect(input).toHaveFocus();
  expectRequests([latestURL(), latestURL()]);
});

it("keeps a reader's anchor and focus when a new ID arrives away from the tail", async () => {
  await open(); readAt(history(), 230); const before = offset(row(2).id);
  const input = within(scope()).getByRole("textbox"); input.focus();
  reads.mockResolvedValueOnce(page([...initial, row(9)])); await poll();
  expect(history().scrollTop).toBe(230); expect(offset(row(2).id)).toBe(before); expect(input).toHaveFocus();
  expectRequests([latestURL(), latestURL()]);
});

it("does not treat delivery-only polling or detail patches as arrivals or remeasure every row", async () => {
  await open(); const container = history(), top = container.scrollTop;
  footer += 40; measurements = 0; scrollWrites = 0;
  reads.mockResolvedValueOnce(page(initial.map(record => ({ ...record, content: `${record.content} 更新` })))); await poll();
  const disclosure = item(row(8).id).querySelector("details")!;
  expect(disclosure).not.toHaveAttribute("open"); fireEvent.click(disclosure.querySelector("summary")!);
  expect(disclosure).toHaveAttribute("open");
  const refresh = within(disclosure).getByRole("button", { name: `刷新消息 ${row(8).id} 投递` });
  expect(refresh).toBeVisible(); fireEvent.click(refresh); await flush();
  expect(container.scrollTop).toBe(top); expect(scrollWrites).toBe(0); expect(measurements).toBe(0);
  expectRequests([latestURL(), latestURL(), `/api/v1/groups/${group.id}/messages/${row(8).id}`]);
});

it("rechecks the actual tail after a size-changing patch before deciding whether a later arrival may follow", async () => {
  await open(); const top = history().scrollTop; footer += 40;
  reads.mockResolvedValueOnce(page(initial)); await poll();
  reads.mockResolvedValueOnce(page([...initial, row(9)])); await poll();
  expect(history().scrollTop).toBe(top); expectRequests([latestURL(), latestURL(), latestURL()]);
});

it("waits for a real successful send receipt, keeps exact public POST semantics, and shows its ID", async () => {
  await open(); readAt(history(), 230); const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise);
  send(); await flush(); expect(history().scrollTop).toBe(230); expect(writes).toHaveBeenCalledTimes(1);
  expect(screen.queryByRole("article", { name: `消息 ${row(9).id}` })).not.toBeInTheDocument();
  const [url, init] = writes.mock.calls[0];
  expect(url).toBe(`/api/v1/groups/${group.id}/messages`); expect(init?.method).toBe("POST");
  expect(JSON.parse(String(init?.body))).toEqual({ content: raw, recipient_ids: [] });
  expect(new Headers(init?.headers).get("Idempotency-Key")).toBeTruthy();
  await act(async () => pending.resolve(json({ ...row(9), content: raw }, 201)));
  expectVisible(row(9).id); expect(screen.getAllByRole("article")).toHaveLength(9);
  expectRequests([latestURL(), `/api/v1/groups/${group.id}/messages`]);
});

it("presents the confirmed ID even if polling already inserted it, without jumping to another newer ID", async () => {
  await open(); readAt(history(), 230); const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise);
  send(); reads.mockResolvedValueOnce(page([...initial, { ...row(9), content: raw }, row(10), row(11), row(12)])); await poll();
  expect(history().scrollTop).toBe(230);
  await act(async () => pending.resolve(json({ ...row(9), content: raw }, 201)));
  expectVisible(row(9).id); expect(history().scrollTop).toBeLessThan(history().scrollHeight - history().clientHeight);
  expect(screen.getAllByRole("article")).toHaveLength(12);
  expectRequests([latestURL(), `/api/v1/groups/${group.id}/messages`, latestURL()]);
});

it.each(["wheel", "keyboard", "touch", "scrollbar"])("lets newer %s reading intent outrank a late successful send receipt", async input => {
  await open(); const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise); send();
  startReading(input); history().scrollTop = 230; endReading(input);
  fireEvent.scroll(history()); const before = offset(row(2).id);
  await act(async () => pending.resolve(json({ ...row(9), content: raw }, 201)));
  expect(history().scrollTop).toBe(230); expect(offset(row(2).id)).toBe(before);
  expect(screen.getByText(/消息已持久化/)).toBeVisible(); expectRequests([latestURL(), `/api/v1/groups/${group.id}/messages`]);
});

it("does not suppress a confirmed send merely because the reader clicked a message without scrolling", async () => {
  await open(); readAt(history(), 230); const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise); send();
  fireEvent.pointerDown(item(row(3).id));
  await act(async () => pending.resolve(json({ ...row(9), content: raw }, 201)));
  expectVisible(row(9).id); expectRequests([latestURL(), `/api/v1/groups/${group.id}/messages`]);
});

it("does not mistake a native layout-compensation scroll for newer reading intent during a send", async () => {
  await open(); readAt(history(), 230); const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise); send();
  header += 30; tops.set(history(), 260); fireEvent.scroll(history());
  await act(async () => pending.resolve(json({ ...row(9), content: raw }, 201)));
  expectVisible(row(9).id); expectRequests([latestURL(), `/api/v1/groups/${group.id}/messages`]);
});

it("does not cancel an explicit reset because native anchoring compensated for a pending status", async () => {
  await open(); readAt(history(), 230); const pending = deferred<Response>(); reads.mockReturnValueOnce(pending.promise);
  fireEvent.click(button("刷新并重置历史窗口"));
  header += 30; tops.set(history(), 260); fireEvent.scroll(history());
  await act(async () => pending.resolve(page(initial)));
  expectTail(); expectRequests([latestURL(), latestURL()]);
});

it.each([503, 413])("does not jump or invent messages for an unknown/rejected HTTP %s send", async status => {
  await open(); readAt(history(), 230); writes.mockResolvedValueOnce(json({}, status)); send(); await flush();
  expect(screen.getByRole("alert")).toHaveTextContent(String(status)); expect(history().scrollTop).toBe(230);
  expect(screen.getAllByRole("article")).toHaveLength(8); expect(writes).toHaveBeenCalledTimes(1);
  expectRequests([latestURL(), `/api/v1/groups/${group.id}/messages`]);
});

it.each([false, true])("restores a real prepend anchor, not total height delta (native anchoring=%s)", async native => {
  await open(); const container = history(); readAt(container, 230); const before = offset(row(2).id);
  const pending = deferred<Response>(); reads.mockReturnValueOnce(pending.promise);
  fireEvent.click(button("加载更早消息")); expect(container.scrollTop).toBe(230);
  // Changes both before and after the visible anchor make total-height restoration incorrect.
  heights.set(row(1).id, 160); heights.set(row(8).id, 270);
  const older = [row(-2), row(-1)].map((record, index) => ({ ...record, id: `older-${index}`, created_at: `2026-10-30T11:59:0${index}Z` }));
  heights.set(older[0].id, 90); heights.set(older[1].id, 130);
  if (native) nativeShift = { count: 10, delta: 280 };
  await act(async () => pending.resolve(page(older)));
  expect(offset(row(2).id)).toBe(before); expect(container.scrollTop).toBe(510);
  expect(screen.getAllByRole("article")).toHaveLength(10); expectRequests([latestURL(), olderURL]);
});

it("does not jump on older-page failure and preserves the anchor on the existing retry path", async () => {
  await open(); readAt(history(), 230); const before = offset(row(2).id);
  reads.mockResolvedValueOnce(json({}, 503)); fireEvent.click(button("加载更早消息")); await flush();
  expect(history().scrollTop).toBe(230); expect(screen.getByRole("alert")).toHaveTextContent("503");
  reads.mockResolvedValueOnce(page([{ ...row(1), id: "older-real", created_at: "2026-10-30T11:59:00Z" }]));
  fireEvent.click(button("重试载入消息")); await flush();
  expect(offset(row(2).id)).toBe(before); expectRequests([latestURL(), olderURL, olderURL]);
});

it("preserves a mounted scope through mobile-style hiding without saving hidden zero geometry", async () => {
  const view = await open(); const container = history(); readAt(container, 230); const before = offset(row(2).id);
  view.rerender(<Discussion groupId={undefined} members={{}} />);
  scrollWrites = 0; fireEvent.scroll(container); tops.set(container, 0); await poll();
  expect(container.clientHeight).toBe(0); expect(scrollWrites).toBe(0); expectRequests([latestURL()]);
  const pending = deferred<Response>(); reads.mockReturnValueOnce(pending.promise);
  view.rerender(<Discussion groupId={group.id} members={{}} />); await flush();
  expect(offset(row(2).id)).toBe(before);
  heights.set(row(1).id, 140);
  await act(async () => pending.resolve(page(initial, "older+/")));
  expect(offset(row(2).id)).toBe(before); expectRequests([latestURL(), latestURL()]);
});

it("keeps independent A/B anchors and stable drafts across scope changes", async () => {
  const view = await open(); readAt(history(), 230); const aOffset = offset(row(2).id);
  fireEvent.change(within(scope()).getByRole("textbox"), { target: { value: "A 草稿" } });
  view.rerender(<Discussion groupId={other.id} members={{}} />); await flush();
  expectTail(history(other.id)); readAt(history(other.id), 340); const bOffset = offset(row(3, other.id).id);
  fireEvent.change(within(scope(other.id)).getByRole("textbox"), { target: { value: "B 草稿" } });
  view.rerender(<Discussion groupId={group.id} members={{}} />); await flush();
  expect(offset(row(2).id)).toBe(aOffset); expect(within(scope()).getByRole("textbox")).toHaveValue("A 草稿");
  view.rerender(<Discussion groupId={other.id} members={{}} />); await flush();
  expect(offset(row(3, other.id).id)).toBe(bOffset); expect(within(scope(other.id)).getByRole("textbox")).toHaveValue("B 草稿");
  expectRequests([latestURL(), latestURL(other.id), latestURL(), latestURL(other.id)]);
});

it("defers inactive A's confirmed presentation without moving or focusing B", async () => {
  const view = await open(); readAt(history(), 230); const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise); send();
  const aHistory = history(); view.rerender(<Discussion groupId={other.id} members={{}} />); await flush();
  const bHistory = history(other.id); readAt(bHistory, 340); const input = within(scope(other.id)).getByRole("textbox"); input.focus();
  scrollWrites = 0; await act(async () => pending.resolve(json({ ...row(9), content: raw }, 201)));
  expect(bHistory.scrollTop).toBe(340); expect(input).toHaveFocus(); expect(scrollWrites).toBe(0); expect(aHistory.clientHeight).toBe(0);
  reads.mockResolvedValueOnce(page([...initial, { ...row(9), content: raw }]));
  view.rerender(<Discussion groupId={group.id} members={{}} />); await flush();
  expectVisible(row(9).id); expect(tops.get(bHistory)).toBe(340);
  expectRequests([latestURL(), `/api/v1/groups/${group.id}/messages`, latestURL(other.id), latestURL()]);
});

it("keeps the history statement and a bounded composer notice visible at fallback, following tail through its height changes", async () => {
  await openFallback();
  const container = history(), notice = screen.getByText(resetNotice);
  expect(within(container).getByText("已载入 6 条（含话题，片段可能有间隔）。")).toBeInTheDocument();
  const composer = within(scope()).getByRole("region", { name: "撰写消息" });
  expect(notice.closest(".message-composer")).toBe(composer); expect(notice.closest(".message-history")).toBeNull();
  expect(notice).toHaveClass("feed-note"); expect(notice).not.toHaveAttribute("role");
  const noticeBox = notice.getBoundingClientRect(), composerBox = composer.getBoundingClientRect();
  expect(noticeBox.top).toBeGreaterThanOrEqual(composerBox.top); expect(noticeBox.bottom).toBeLessThanOrEqual(composerBox.bottom);
  expect(container.getBoundingClientRect().bottom).toBe(composerBox.top);
  expect(container.clientHeight).toBe(160); expectTail(); expectVisible(row(8).id);
  expect(screen.queryByRole("article", { name: `消息 ${row(1).id}` })).not.toBeInTheDocument();
  const pending = deferred<Response>(); reads.mockReturnValueOnce(pending.promise);
  fireEvent.click(button("加载更早消息")); await flush();
  expect(screen.queryByText(resetNotice)).not.toBeInTheDocument();
  expect(container.clientHeight).toBe(200); expectTail(); expectVisible(row(8).id);
  await act(async () => pending.resolve(page(initial.slice(0, 2))));
  expectTail(); expectRequests([latestURL(), olderURL, latestURL(), olderURL]);
});

it("keeps a real reading anchor when dismissing the composer notice expands history away from tail", async () => {
  await openFallback(); const container = history(); expect(container.clientHeight).toBe(160);
  startReading("wheel"); container.scrollTop = 230; const before = offset(row(5).id);
  fireEvent.scroll(container); await flush();
  expect(screen.queryByText(resetNotice)).not.toBeInTheDocument(); expect(container.clientHeight).toBe(200);
  expect(container.scrollTop).toBe(230); expect(offset(row(5).id)).toBe(before);
  expect(within(container).getByText("已载入 6 条（含话题，片段可能有间隔）。")).toBeInTheDocument();
  expectRequests([latestURL(), olderURL, latestURL()]);
});

it("does not apply a saved reactivation anchor after the user chooses a newer reading position", async () => {
  const view = await open(); readAt(history(), 230); view.rerender(<Discussion groupId={undefined} members={{}} />);
  const pending = deferred<Response>(); reads.mockReturnValueOnce(pending.promise);
  view.rerender(<Discussion groupId={group.id} members={{}} />); await flush(); readAt(history(), 440);
  await act(async () => pending.resolve(page(initial)));
  expect(history().scrollTop).toBe(440); expectRequests([latestURL(), latestURL()]);
});

it("ignores an obsolete older request across A/B/A instead of restoring its old scroll intent", async () => {
  const view = await open(); readAt(history(), 230); const pending = deferred<Response>(); reads.mockReturnValueOnce(pending.promise);
  fireEvent.click(button("加载更早消息")); view.rerender(<Discussion groupId={other.id} members={{}} />); await flush();
  view.rerender(<Discussion groupId={group.id} members={{}} />); await flush(); readAt(history(), 440);
  await act(async () => pending.resolve(page([{ ...row(1), id: "obsolete-older", created_at: "2026-10-30T11:59:00Z" }])));
  expect(history().scrollTop).toBe(440); expect(screen.queryByRole("article", { name: "消息 obsolete-older" })).not.toBeInTheDocument();
  expect(reads.mock.calls[1][1]?.signal?.aborted).toBe(true);
  expectRequests([latestURL(), olderURL, latestURL(other.id), latestURL()]);
});

it.each([false, true])("resets explicitly only after success, keeping old content/position on failure (failure=%s)", async failure => {
  await open(); readAt(history(), 230); const pending = deferred<Response>(); reads.mockReturnValueOnce(pending.promise);
  fireEvent.click(button("刷新并重置历史窗口")); expect(history().scrollTop).toBe(230);
  await act(async () => pending.resolve(failure ? json({}, 503) : page(initial.slice(2))));
  if (failure) {
    expect(history().scrollTop).toBe(230); expect(screen.getAllByRole("article")).toHaveLength(8);
    reads.mockResolvedValueOnce(page(initial.slice(2)));
    const retry = button("重试载入消息"); fireEvent.pointerDown(retry); fireEvent.keyDown(retry, { key: " " });
    fireEvent.click(retry); await flush();
  }
  expectTail(); expectVisible(row(8).id); expect(screen.getAllByRole("article")).toHaveLength(6);
  expect(screen.queryByText(/历史窗口已更新，可加载更早消息/)).not.toBeInTheDocument();
  expectRequests(failure ? [latestURL(), latestURL(), latestURL()] : [latestURL(), latestURL()]);
});

it("bounds visible-anchor measurements on scroll and does no row geometry work for ordinary polling", async () => {
  const many = Array.from({ length: 50 }, (_, index) => row(index + 1)); await open(many);
  measurements = 0; readAt(history(), 2430); expect(measurements).toBeLessThanOrEqual(16);
  measurements = 0; reads.mockResolvedValueOnce(page(many)); await poll();
  expect(measurements).toBe(0); expect(history().scrollTop).toBe(2430); expectRequests([latestURL(), latestURL()]);
});

it("review: a completed message click cannot turn later native compensation into send-canceling reading", async () => {
  await open(); readAt(history(), 230); const pending = deferred<Response>(); writes.mockReturnValueOnce(pending.promise); send();
  const clicked = item(row(3).id);
  fireEvent.pointerDown(clicked); fireEvent.pointerUp(clicked); fireEvent.click(clicked);
  header += 30; tops.set(history(), 260); fireEvent.scroll(history());
  await act(async () => pending.resolve(json({ ...row(9), content: raw }, 201)));
  expectVisible(row(9).id);
  expectRequests([latestURL(), `/api/v1/groups/${group.id}/messages`]);
});

it("review: a completed message click cannot make native compensation cancel an explicit reset", async () => {
  await open(); readAt(history(), 230); const pending = deferred<Response>(); reads.mockReturnValueOnce(pending.promise);
  fireEvent.click(button("刷新并重置历史窗口"));
  const clicked = item(row(3).id);
  fireEvent.pointerDown(clicked); fireEvent.pointerUp(clicked); fireEvent.click(clicked);
  header += 30; tops.set(history(), 260); fireEvent.scroll(history());
  await act(async () => pending.resolve(page(initial)));
  expectTail(); expectRequests([latestURL(), latestURL()]);
});

it.each(["wheel", "keyboard", "touch", "scrollbar"])("keeps newer %s reading through an explicit reset and input release", async input => {
  await open(); const pending = deferred<Response>(); reads.mockReturnValueOnce(pending.promise);
  fireEvent.click(button("刷新并重置历史窗口"));
  startReading(input); history().scrollTop = 230; endReading(input); fireEvent.scroll(history());
  const before = offset(row(2).id); await act(async () => pending.resolve(page(initial)));
  expect(history().scrollTop).toBe(230); expect(offset(row(2).id)).toBe(before);
  expectRequests([latestURL(), latestURL()]);
});

it.each(["send", "reset"])("reconciles scrollbar movement at release before a late scroll event and %s receipt", async operation => {
  await open(); const pending = deferred<Response>();
  if (operation === "send") { writes.mockReturnValueOnce(pending.promise); send(); }
  else { reads.mockReturnValueOnce(pending.promise); fireEvent.click(button("刷新并重置历史窗口")); }
  startReading("scrollbar"); history().scrollTop = 230; endReading("scrollbar");
  const before = offset(row(2).id);
  await act(async () => pending.resolve(operation === "send" ? json({ ...row(9), content: raw }, 201) : page(initial)));
  fireEvent.scroll(history());
  expect(history().scrollTop).toBe(230); expect(offset(row(2).id)).toBe(before);
  expectRequests(operation === "send" ? [latestURL(), `/api/v1/groups/${group.id}/messages`] : [latestURL(), latestURL()]);
});

it.each(["send", "reset"])("review: a completed container-padding click cannot cancel %s on later native compensation", async operation => {
  await open(); readAt(history(), 230); const pending = deferred<Response>();
  if (operation === "send") { writes.mockReturnValueOnce(pending.promise); send(); }
  else { reads.mockReturnValueOnce(pending.promise); fireEvent.click(button("刷新并重置历史窗口")); }
  const container = history();
  fireEvent.pointerDown(container, { pointerId: 1, clientX: 4, clientY: 105 });
  fireEvent.pointerUp(container, { pointerId: 1, clientX: 4, clientY: 105 });
  fireEvent.click(container, { clientX: 4, clientY: 105 });
  await act(async () => { await vi.advanceTimersByTimeAsync(100); });
  header += 30; tops.set(container, 260); fireEvent.scroll(container);
  await act(async () => pending.resolve(operation === "send" ? json({ ...row(9), content: raw }, 201) : page(initial)));
  if (operation === "send") expectVisible(row(9).id);
  else expectTail();
  expectRequests(operation === "send" ? [latestURL(), `/api/v1/groups/${group.id}/messages`] : [latestURL(), latestURL()]);
});

it.each(["send", "reset"])("does not mistake side whitespace inside the client viewport for a scrollbar during %s", async operation => {
  await open(); readAt(history(), 230); const pending = deferred<Response>();
  if (operation === "send") { writes.mockReturnValueOnce(pending.promise); send(); }
  else { reads.mockReturnValueOnce(pending.promise); fireEvent.click(button("刷新并重置历史窗口")); }
  const container = history(), point = { pointerId: 1, clientX: 290, clientY: 180 };
  expect(point.clientX).toBeLessThan(container.clientLeft + container.clientWidth);
  fireEvent.pointerDown(container, point); fireEvent.pointerUp(container, point); fireEvent.click(container, point);
  await act(async () => { await vi.advanceTimersByTimeAsync(100); });
  header += 30; tops.set(container, 260); fireEvent.scroll(container);
  await act(async () => pending.resolve(operation === "send" ? json({ ...row(9), content: raw }, 201) : page(initial)));
  if (operation === "send") expectVisible(row(9).id);
  else expectTail();
  expectRequests(operation === "send" ? [latestURL(), `/api/v1/groups/${group.id}/messages`] : [latestURL(), latestURL()]);
});

it.each(["send", "reset"].flatMap(operation => ["track", "drag", "overlay-movement", "overlay-drag"].map(kind => ({ operation, kind }))))(
  "preserves real $kind geometry/movement priority over a late $operation receipt", async ({ operation, kind }) => {
    if (kind.startsWith("overlay")) gutterWidth = 0;
    await open(); const pending = deferred<Response>();
    if (operation === "send") { writes.mockReturnValueOnce(pending.promise); send(); }
    else { reads.mockReturnValueOnce(pending.promise); fireEvent.click(button("刷新并重置历史窗口")); }
    const container = history(), point = { pointerId: 1, clientX: 312, clientY: 180 };
    expect(container.offsetWidth).toBe(320); expect(container.clientLeft).toBe(0);
    expect(container.clientWidth).toBe(kind.startsWith("overlay") ? 320 : 304);
    if (kind.startsWith("overlay")) expect(point.clientX).toBeLessThan(container.clientWidth);
    else expect(point.clientX).toBeGreaterThanOrEqual(container.clientLeft + container.clientWidth);
    fireEvent.pointerDown(container, point);
    const release = kind.endsWith("drag") ? { ...point, clientY: 150 } : point;
    if (kind.endsWith("drag")) fireEvent.pointerMove(container, { ...release, buttons: 1 });
    if (kind === "drag" || kind === "overlay-movement") container.scrollTop = 230;
    if (kind === "overlay-movement") fireEvent.scroll(container);
    fireEvent.pointerUp(container, release);
    await act(async () => { await vi.advanceTimersByTimeAsync(100); });
    if (kind === "track" || kind === "overlay-drag") container.scrollTop = 230;
    fireEvent.scroll(container); const before = offset(row(2).id);
    await act(async () => pending.resolve(operation === "send" ? json({ ...row(9), content: raw }, 201) : page(initial)));
    expect(container.scrollTop).toBe(230); expect(offset(row(2).id)).toBe(before);
    expectRequests(operation === "send" ? [latestURL(), `/api/v1/groups/${group.id}/messages`] : [latestURL(), latestURL()]);
  },
);

it.each(["send", "reset"])("keeps moved pointer input for scrolling delivered just after release during %s", async operation => {
  await open(); const pending = deferred<Response>();
  if (operation === "send") { writes.mockReturnValueOnce(pending.promise); send(); }
  else { reads.mockReturnValueOnce(pending.promise); fireEvent.click(button("刷新并重置历史窗口")); }
  const target = item(row(7).id);
  fireEvent.pointerDown(target, { pointerId: 1, clientX: 100, clientY: 200 });
  fireEvent.pointerMove(target, { pointerId: 1, clientX: 100, clientY: 160, buttons: 1 });
  fireEvent.pointerUp(target, { pointerId: 1, clientX: 100, clientY: 160 });
  history().scrollTop = 230; fireEvent.scroll(history()); const before = offset(row(2).id);
  await act(async () => pending.resolve(operation === "send" ? json({ ...row(9), content: raw }, 201) : page(initial)));
  expect(history().scrollTop).toBe(230); expect(offset(row(2).id)).toBe(before);
  expectRequests(operation === "send" ? [latestURL(), `/api/v1/groups/${group.id}/messages`] : [latestURL(), latestURL()]);
});
