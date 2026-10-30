import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ApiError, getMessage, listMessages } from "./api";
import type { Message } from "./api";

type FeedRecord = { message: Message; source: "read" | "receipt"; dataVersion: number; presenceVersion: number };
type Feed = { entries: Map<string, FeedRecord>; floor: number };
const errorText = (error: unknown) => error instanceof ApiError ? error.message : "请求未完成，请重试。";
function merge(previous: Feed, incoming: Message[], version: number, reset = false): Feed {
  const resetting = reset && version >= previous.floor;
  const entries = new Map([...previous.entries].filter(([, record]) => !resetting || record.presenceVersion > version));
  for (const message of incoming) {
    const current = previous.entries.get(message.id);
    // A reset blocks old reads from resurrecting absent rows, not updating independently proven ones.
    if (!current && version < previous.floor) continue;
    entries.set(message.id, current && current.dataVersion > version ? current : {
      message, source: "read", dataVersion: version, presenceVersion: Math.max(version, current?.presenceVersion ?? 0),
    });
  }
  return { entries, floor: resetting ? version : previous.floor };
}
// Go UTC timestamps have variable fractional precision. Preserve all nine digits before comparing.
const timeKey = (value: string) => value.replace(/(?:\.(\d+))?Z$/, (_match, fraction?: string) => `.${(fraction ?? "").padEnd(9, "0")}Z`);
const compare = (a: string, b: string) => a < b ? -1 : a > b ? 1 : 0;

export function useMessageFeed(groupId: string, active: boolean) {
  const [feed, setFeed] = useState<Feed>({ entries: new Map(), floor: 0 });
  const [page, setPage] = useState({ pending: true, error: "", next: "" });
  const [details, setDetails] = useState<Record<string, { pending: boolean; error: string }>>({});
  const clock = useRef(0), epoch = useRef(0), running = useRef(false), pageBusy = useRef(false);
  const controllers = useRef(new Set<AbortController>()), detailBusy = useRef(new Set<string>());
  const lastRequest = useRef({ before: "", reset: true });
  const beginRead = useCallback(() => {
    const controller = new AbortController(), generation = epoch.current, version = ++clock.current;
    controllers.current.add(controller);
    return { version, signal: controller.signal,
      valid: () => running.current && generation === epoch.current && !controller.signal.aborted,
      close: () => controllers.current.delete(controller) };
  }, []);
  const loadPage = useCallback(async (before = "", reset = true) => {
    if (!running.current || pageBusy.current) return;
    pageBusy.current = true; lastRequest.current = { before, reset };
    const read = beginRead();
    setPage(previous => ({ ...previous, pending: true, error: "" }));
    try {
      const result = await listMessages(groupId, before, read.signal);
      if (!read.valid()) return;
      setFeed(previous => merge(previous, result.items, read.version, reset));
      setPage({ pending: false, error: "", next: result.next_cursor });
    } catch (error) {
      if (read.valid()) setPage(previous => ({ ...previous, pending: false, error: errorText(error) }));
    } finally { if (read.valid()) pageBusy.current = false; read.close(); }
  }, [groupId, beginRead]);
  const refreshDetail = useCallback(async (id: string) => {
    if (!running.current || detailBusy.current.has(id)) return;
    detailBusy.current.add(id);
    const read = beginRead();
    setDetails(previous => ({ ...previous, [id]: { pending: true, error: "" } }));
    try {
      const result = await getMessage(groupId, id, read.signal);
      if (read.valid()) setFeed(previous => merge(previous, [result], read.version));
      if (read.valid()) setDetails(previous => ({ ...previous, [id]: { pending: false, error: "" } }));
    } catch (error) {
      if (read.valid()) setDetails(previous => ({ ...previous, [id]: { pending: false, error: errorText(error) } }));
    } finally { if (read.valid()) detailBusy.current.delete(id); read.close(); }
  }, [groupId, beginRead]);
  useEffect(() => {
    running.current = active; ++epoch.current;
    pageBusy.current = false; detailBusy.current.clear(); setDetails({});
    if (active) void loadPage();
    return () => {
      running.current = false; ++epoch.current;
      for (const controller of controllers.current) controller.abort();
      controllers.current.clear();
    };
  }, [active, loadPage]);
  const beginSend = useCallback(() => ++clock.current, []);
  const confirm = useCallback((message: Message, attempt: number) => {
    const version = ++clock.current;
    setFeed(previous => {
      const current = previous.entries.get(message.id), entries = new Map(previous.entries);
      // A POST proves presence, not a new observation of delivery state.
      entries.set(message.id, current?.source === "read" ? { ...current, presenceVersion: version } : {
        message, source: "receipt", dataVersion: attempt, presenceVersion: version,
      });
      return { ...previous, entries };
    });
  }, []);
  const items = useMemo(() => [...feed.entries.values()].map(record => record.message)
    .sort((a, b) => compare(timeKey(a.created_at), timeKey(b.created_at)) || compare(a.id, b.id)), [feed]);
  return { items, page, details, loadPage, refreshDetail, beginSend, confirm,
    retry: () => loadPage(lastRequest.current.before, lastRequest.current.reset) };
}
