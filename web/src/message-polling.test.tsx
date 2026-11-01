import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { useMessageFeed } from "./useMessageFeed";
import { deferred, group, json, page } from "./test/groups";
import { delivery, message } from "./test/messages";
const latest = vi.fn<typeof fetch>(), details = vi.fn<typeof fetch>(), network = vi.fn<typeof fetch>();
const row = (id: string, state = "queued") => ({ ...message, id, deliveries: [{ ...delivery, id: `d-${id}`, message_id: id, state }] });
const rows = Array.from({ length: 12 }, (_, i) => row(`pending-${String(i).padStart(2, "0")}`, i % 2 ? "future-state" : "queued"));
const initial = [...rows, row("terminal-accepted", "accepted"), row("terminal-failed", "failed")];
const flush = () => act(async () => { await vi.advanceTimersByTimeAsync(0); });
const tick = (ms = 5000) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });
async function open(items = initial, next = "history-cursor") {
  latest.mockResolvedValueOnce(page(items, next));
  const view = renderHook(({ active }) => useMessageFeed(group.id, active), { initialProps: { active: true } });
  await flush(); return view;
}
beforeEach(() => {
  vi.useFakeTimers(); latest.mockReset().mockImplementation(async () => page(initial, "latest-cursor"));
  details.mockReset().mockImplementation(async url => json(row(decodeURIComponent(String(url).split("/messages/")[1]))));
  network.mockReset().mockImplementation((url, init) => {
    if (!String(url).startsWith(`/api/v1/groups/${group.id}/messages`)) throw new Error("Unexpected endpoint");
    return (/\/messages\//.test(String(url)) ? details : latest)(url, init);
  });
  vi.stubGlobal("fetch", network);
});
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });
it("polls only bounded read endpoints, skips terminal deliveries and fairly continues a five-detail budget", async () => {
  await open(); expect(details).not.toHaveBeenCalled();
  await tick(); expect(latest).toHaveBeenCalledTimes(2); expect(details).toHaveBeenCalledTimes(5);
  await tick(); expect(details).toHaveBeenCalledTimes(10);
  await tick(); expect(details).toHaveBeenCalledTimes(15);
  const ids = details.mock.calls.map(([url]) => String(url).split("/messages/")[1]);
  expect(new Set(ids)).toEqual(new Set(rows.map(item => item.id)));
  expect(ids.slice(10)).toEqual(["pending-10", "pending-11", "pending-00", "pending-01", "pending-02"]);
  expect(network.mock.calls.every(([, init]) => (init?.method ?? "GET") === "GET")).toBe(true);
  expect(latest.mock.calls.every(([url]) => String(url) === `/api/v1/groups/${group.id}/messages?limit=50`)).toBe(true);
});
it("does not spend detail requests on a delivery already settled by this cycle's latest read", async () => {
  const view = await open([row("settles")]);
  const accepted = row("settles", "accepted"); latest.mockResolvedValueOnce(page([accepted])); details.mockResolvedValueOnce(json(accepted));
  await tick(); expect(view.result.current.items[0].deliveries[0].state).toBe("accepted");
  expect(details).not.toHaveBeenCalled();
});
it("waits for latest and sequential detail completion, then waits another five seconds without overlapping cycles", async () => {
  await open(); const pendingPage = deferred<Response>(), pendingDetail = deferred<Response>();
  latest.mockReturnValueOnce(pendingPage.promise); details.mockReturnValueOnce(pendingDetail.promise);
  await tick(); await tick(20000); expect(latest).toHaveBeenCalledTimes(2); expect(details).not.toHaveBeenCalled();
  await act(async () => pendingPage.resolve(page(initial))); await flush();
  expect(details).toHaveBeenCalledTimes(1); await tick(20000); expect(details).toHaveBeenCalledTimes(1); expect(latest).toHaveBeenCalledTimes(2);
  await act(async () => pendingDetail.resolve(json(rows[0]))); await flush();
  expect(details).toHaveBeenCalledTimes(5); await tick(4999); expect(latest).toHaveBeenCalledTimes(2);
  await tick(1); expect(latest).toHaveBeenCalledTimes(3);
});
it("stays idle while inactive and ignores an aborted cycle across deactivate/reactivate", async () => {
  const view = renderHook(({ active }) => useMessageFeed(group.id, active), { initialProps: { active: false } });
  await tick(10000); expect(network).not.toHaveBeenCalled();
  view.rerender({ active: true }); await flush(); const old = deferred<Response>(); latest.mockReturnValueOnce(old.promise);
  await tick(); expect(latest).toHaveBeenCalledTimes(2);
  const signal = latest.mock.calls[1][1]?.signal; expect(signal?.aborted).toBe(false);
  view.rerender({ active: false }); expect(signal?.aborted).toBe(true);
  latest.mockResolvedValueOnce(page([row("fresh", "accepted")])); view.rerender({ active: true }); await flush();
  await act(async () => old.resolve(page([row("obsolete")])));
  expect(view.result.current.items.map(item => item.id)).toEqual(["fresh"]);
  view.unmount(); const count = network.mock.calls.length; await tick(30000); expect(network).toHaveBeenCalledTimes(count);
});
it("aborts a pending detail and cancels the timer on full unmount", async () => {
  const view = await open(); const pending = deferred<Response>(); details.mockReturnValueOnce(pending.promise);
  await tick(); expect(details).toHaveBeenCalledTimes(1); const signal = details.mock.calls[0][1]?.signal;
  view.unmount(); expect(signal?.aborted).toBe(true);
  await act(async () => pending.resolve(json(rows[0]))); await tick(30000);
  expect(details).toHaveBeenCalledTimes(1); expect(latest).toHaveBeenCalledTimes(2);
});
it("keeps manual before continuation across latest window gaps and permits reset to reach the missing range", async () => {
  const view = await open([row("old-1")], "first-cursor");
  latest.mockResolvedValueOnce(page([row("old-2")], "older-continuation"));
  await act(async () => view.result.current.loadPage("first-cursor", false));
  const newer = Array.from({ length: 50 }, (_, index) => row(`new-${index}`, "accepted"));
  latest.mockImplementation(async () => page(newer, "gap-cursor")); await tick();
  expect(view.result.current.page.next).toBe("older-continuation"); expect(view.result.current.items).toHaveLength(52);
  await act(async () => view.result.current.loadPage());
  expect(view.result.current.items).toHaveLength(50); expect(view.result.current.page.next).toBe("gap-cursor");
  latest.mockResolvedValueOnce(page([row("missing-middle", "accepted")], "continue-middle"));
  await act(async () => view.result.current.loadPage(view.result.current.page.next, false));
  expect(view.result.current.items.some(item => item.id === "missing-middle")).toBe(true);
  expect(latest.mock.calls.at(-1)?.[0]).toBe(`/api/v1/groups/${group.id}/messages?limit=50&before=gap-cursor`);
});
it("keeps known data on a polling error and retries on a later read-only cycle", async () => {
  const view = await open(); latest.mockRejectedValueOnce(new TypeError("offline")); await tick();
  expect(view.result.current.pollError).toContain("无法连接本地服务"); expect(view.result.current.items).toHaveLength(initial.length);
  await tick(); expect(view.result.current.pollError).toBe(""); expect(latest).toHaveBeenCalledTimes(3);
});
it("stops the remaining old detail budget when a manual reset replaces its loaded window", async () => {
  const view = await open(); const old = deferred<Response>(); details.mockReturnValueOnce(old.promise); await tick();
  expect(details).toHaveBeenCalledTimes(1);
  latest.mockResolvedValueOnce(page([row("new-window", "accepted")]));
  await act(async () => view.result.current.loadPage());
  await act(async () => old.resolve(json(rows[0]))); await flush();
  expect(view.result.current.items.map(item => item.id)).toEqual(["new-window"]);
  expect(details).toHaveBeenCalledTimes(1);
});
