import { useCallback, useEffect, useRef, useState } from "react";
import { isPendingDecisionsUnavailable, listPendingDecisions } from "./decision-api";
import { listPendingReviews } from "./task-api";

const intervalMs = 25_000;
const timeoutMs = 5_000;
const retryDelayMs = 2_000;
const visible = () => document.visibilityState === "visible";

export function usePendingReminder(enabled: boolean, poll: boolean) {
  const [pending, setPending] = useState(false);
  const recheck = useRef<(() => void) | null>(null);
  // Keep a confirmed save dirty until presence is known. Each save grants one delayed retry.
  const needsRecheck = useRef<false | "retryable" | "dirty">(false);
  const onSaved = useCallback(() => {
    needsRecheck.current = "retryable";
    recheck.current?.();
  }, []);
  useEffect(() => {
    if (!enabled) return;
    let interval: ReturnType<typeof setInterval> | undefined;
    // The same timer bounds a running batch or delays its one retry.
    let timer: ReturnType<typeof setTimeout> | undefined;
    let batch: AbortController | undefined;
    let wasVisible = false;

    function cancelCheck() {
      clearTimeout(timer);
      timer = undefined;
      batch?.abort();
      batch = undefined;
    }
    function scheduleRetry() {
      if (poll || !visible() || needsRecheck.current !== "retryable") return;
      needsRecheck.current = "dirty";
      timer = setTimeout(() => void check(), retryDelayMs);
    }
    async function check(afterSave = false) {
      if (batch && !afterSave) return;
      // A read started before the save may still report the saved item.
      cancelCheck();
      if (!visible()) return;
      const controller = new AbortController();
      batch = controller;
      const { signal } = controller;
      timer = setTimeout(() => {
        cancelCheck();
        scheduleRetry();
      }, timeoutMs);

      // Either list can prove presence, even if the other times out.
      function definitelyEmpty({ items, next_cursor }: { items: readonly unknown[]; next_cursor: string }) {
        if (items.length && !signal.aborted && visible()) {
          needsRecheck.current = false;
          setPending(true);
        }
        return items.length === 0 && !next_cursor;
      }
      // An unavailable decision route does not contribute. Other failures cannot prove emptiness.
      const [decisionsEmpty, reviewsEmpty] = await Promise.all([
        listPendingDecisions("", signal, 1).then(definitelyEmpty, isPendingDecisionsUnavailable),
        listPendingReviews("", signal, 1).then(definitelyEmpty, () => false),
      ]);
      if (signal.aborted) return;
      clearTimeout(timer);
      timer = undefined;
      batch = undefined;
      if (visible() && decisionsEmpty && reviewsEmpty) {
        needsRecheck.current = false;
        setPending(false);
      } else {
        scheduleRetry();
      }
    }
    function stop() {
      wasVisible = false;
      clearInterval(interval);
      interval = undefined;
      cancelCheck();
    }
    function onVisibility() {
      if (!visible()) return stop();
      if (wasVisible) return;
      wasVisible = true;
      if (needsRecheck.current || poll) void check();
      if (poll) interval = setInterval(() => void check(), intervalMs);
    }
    recheck.current = () => { void check(true); };
    document.addEventListener("visibilitychange", onVisibility);
    onVisibility();
    return () => {
      recheck.current = null;
      document.removeEventListener("visibilitychange", onVisibility);
      stop();
    };
  }, [enabled, poll]);
  return { pending, onSaved };
}
