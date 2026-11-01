import { useLayoutEffect, useRef, useState } from "react";
import type { KeyboardEvent, PointerEvent } from "react";
import type { Message } from "./api";

type Anchor = { id: string; offset: number };
type PageIntent = { kind: "older" | "reset" | "restore"; anchor: Anchor | null; revision: number; started: boolean };
type PointerInput = { id: number; x: number; y: number; top: number; moved: boolean; scrollbar: boolean };
const tailTolerance = 16;
const inputLifetime = 250;

export function useMessageScroll(active: boolean, items: Message[], page: { pending: boolean; error: string }) {
  const historyRef = useRef<HTMLElement>(null), listRef = useRef<HTMLOListElement>(null);
  const [windowReset, setWindowReset] = useState(false);
  const saved = useRef<Anchor | null>(null), pageIntent = useRef<PageIntent | null>(null);
  const attempt = useRef<{ id: number; revision: number } | null>(null);
  const confirmed = useRef<{ id: string; revision: number } | null>(null);
  const revision = useRef(0), initialized = useRef(false), wasActive = useRef(false), atTail = useRef(true);
  const previousIDs = useRef(new Set<string>()), lastTop = useRef(0), expectedTop = useRef<number | null>(null);
  const previousNote = useRef(windowReset), inputUntil = useRef(0);
  const pointer = useRef<PointerInput | null>(null);
  const isTail = (history: HTMLElement) => history.scrollHeight - history.clientHeight - history.scrollTop <= tailTolerance;

  function clearInput() {
    inputUntil.current = 0;
    pointer.current = null;
  }
  function capture() {
    const history = historyRef.current, children = listRef.current?.children;
    if (!history || !history.clientHeight || !children?.length) return null;
    const top = history.getBoundingClientRect().top;
    // Message order is already the feed's order. Binary search bounds layout reads on scroll.
    let low = 0, high = children.length;
    while (low < high) {
      const middle = (low + high) >>> 1;
      if (children[middle].getBoundingClientRect().bottom <= top) low = middle + 1;
      else high = middle;
    }
    const node = children[low] as HTMLElement | undefined;
    if (!node) return null;
    const box = node.getBoundingClientRect();
    return box.top < top + history.clientHeight && node.dataset.messageId
      ? { id: node.dataset.messageId, offset: box.top - top } : null;
  }
  function remember() {
    const history = historyRef.current;
    if (!history?.clientHeight) return;
    const anchor = capture();
    saved.current = anchor ?? saved.current;
    lastTop.current = history.scrollTop; atTail.current = isTail(history);
    return anchor;
  }
  function move(top: number) {
    const history = historyRef.current!;
    if (Math.abs(history.scrollTop - top) > 0.5) history.scrollTop = Math.max(0, top);
    expectedTop.current = history.scrollTop;
    remember();
  }
  const latest = () => move(historyRef.current!.scrollHeight - historyRef.current!.clientHeight);
  const nodeFor = (id: string) => listRef.current?.children[items.findIndex(message => message.id === id)];
  function restore(anchor: Anchor | null) {
    const node = anchor && nodeFor(anchor.id), history = historyRef.current!;
    if (!node || !anchor) return false;
    // Apply only the residual offset: native scroll anchoring may already have compensated.
    move(history.scrollTop + node.getBoundingClientRect().top - history.getBoundingClientRect().top - anchor.offset);
    return true;
  }
  function show(id: string) {
    const node = nodeFor(id), history = historyRef.current!;
    if (!node) return false;
    const box = node.getBoundingClientRect(), top = history.getBoundingClientRect().top;
    const above = box.top - top, below = box.bottom - top - history.clientHeight;
    move(history.scrollTop + (above < 0 ? above : below > 0 ? Math.min(above, below) : 0));
    return true;
  }
  function beforePage(kind: "older" | "reset") {
    const anchor = remember() ?? null;
    clearInput();
    pageIntent.current = { kind, anchor, revision: ++revision.current, started: false };
    confirmed.current = null; setWindowReset(false);
  }
  function onScroll() {
    const history = historyRef.current;
    if (!active || !history?.clientHeight) return;
    const top = history.scrollTop;
    if (top === expectedTop.current || top === lastTop.current) return;
    expectedTop.current = null;
    remember();
    if (performance.now() < inputUntil.current) recordReading();
    const intent = pageIntent.current;
    if (intent && intent.kind !== "reset") intent.anchor = saved.current;
  }
  function recordReading() {
    ++revision.current;
    confirmed.current = null;
    setWindowReset(false);
    const intent = pageIntent.current;
    if (intent) {
      intent.kind = intent.kind === "older" ? "older" : "restore";
      intent.revision = revision.current;
      intent.anchor = saved.current;
    }
  }
  function onReadingIntent() {
    if (!active || !historyRef.current?.clientHeight) return;
    inputUntil.current = performance.now() + inputLifetime;
    if (pointer.current) pointer.current.moved = true;
  }
  function onPointerDown(event: PointerEvent<HTMLElement>) {
    const history = historyRef.current, target = event.target;
    if (!active || !history?.clientHeight) return;
    const containerTarget = target === event.currentTarget;
    if (!containerTarget && !(target instanceof Element && target.closest(".message-list"))) return;
    let scrollbar = false;
    if (containerTarget) {
      const box = history.getBoundingClientRect(), style = getComputedStyle(history);
      const x = event.clientX - box.left;
      const borderLeft = Number.parseFloat(style.borderLeftWidth) || 0;
      const borderRight = Number.parseFloat(style.borderRightWidth) || 0;
      const inGutter = (x >= borderLeft && x < history.clientLeft)
        || (x >= history.clientLeft + history.clientWidth && x < history.offsetWidth - borderRight);
      scrollbar = inGutter && history.scrollHeight > history.clientHeight
        && event.clientY >= box.top + history.clientTop
        && event.clientY < box.top + history.clientTop + history.clientHeight;
    }
    pointer.current = { id: event.pointerId, x: event.clientX, y: event.clientY,
      top: history.scrollTop, moved: false, scrollbar };
    inputUntil.current = scrollbar ? performance.now() + inputLifetime : 0;
  }
  function onPointerMove(event: PointerEvent<HTMLElement>) {
    const current = pointer.current;
    if (!current || current.id !== event.pointerId) return;
    if (!event.buttons) { onPointerEnd(event); return; }
    if (Math.abs(event.clientX - current.x) + Math.abs(event.clientY - current.y) > 2) current.moved = true;
    if (current.moved) inputUntil.current = performance.now() + inputLifetime;
  }
  function onPointerEnd(event: PointerEvent<HTMLElement>) {
    const current = pointer.current, history = historyRef.current;
    if (!current || current.id !== event.pointerId) return;
    pointer.current = null;
    // Overlay scrollbars may expose no gutter; observed viewport movement is still evidence.
    const shifted = Boolean(active && history?.clientHeight && history.scrollTop !== current.top);
    if (!current.moved && !current.scrollbar && !shifted) { inputUntil.current = 0; return; }
    inputUntil.current = performance.now() + inputLifetime;
    if (shifted) {
      // Reconcile even if a scroll event already updated lastTop without a measurable gutter.
      remember();
      recordReading();
    }
  }
  function onKeyDown(event: KeyboardEvent<HTMLElement>) {
    if (event.key === " " && event.target instanceof Element && event.target.closest("button, a, input, select, textarea, summary")) return;
    if (["ArrowUp", "ArrowDown", "PageUp", "PageDown", "Home", "End", " "].includes(event.key)) onReadingIntent();
  }
  function beginSend(id: number) {
    clearInput();
    attempt.current = { id, revision: revision.current }; confirmed.current = null;
  }
  function confirmSend(id: string, sendAttempt: number) {
    if (attempt.current?.id === sendAttempt && attempt.current.revision === revision.current) {
      confirmed.current = { id, revision: revision.current };
    }
  }

  useLayoutEffect(() => {
    const history = historyRef.current, entering = active && !wasActive.current;
    wasActive.current = active;
    const arrived = items.some(message => !previousIDs.current.has(message.id));
    previousIDs.current = new Set(items.map(message => message.id));
    const noteChanged = previousNote.current !== windowReset;
    previousNote.current = windowReset;
    if (!active) { pageIntent.current = null; clearInput(); return; }
    if (!history?.clientHeight) return;
    if (entering) clearInput();
    if (entering && initialized.current) {
      pageIntent.current = { kind: "restore", anchor: saved.current, revision: revision.current, started: page.pending };
      restore(saved.current);
    }
    const intent = pageIntent.current;
    if (intent && page.pending) intent.started = true;
    let positioned = false;
    const target = confirmed.current;
    if (target && target.revision === revision.current && show(target.id)) {
      confirmed.current = null; setWindowReset(false); positioned = true;
      // An automatic reset following reactivation must not undo the confirmed presentation.
      if (intent) { intent.kind = "restore"; intent.anchor = saved.current; }
    }
    if (intent?.started && !page.pending) {
      intent.started = false;
      if (!page.error) {
        if (!positioned && intent.revision === revision.current) {
          if (intent.kind === "reset") { latest(); setWindowReset(false); }
          else if (!restore(intent.anchor) && intent.kind === "restore" && intent.anchor) {
            latest(); setWindowReset(true);
          } else setWindowReset(false);
          positioned = true;
        }
        pageIntent.current = null;
      }
      // Keep a failed operation's anchor for the feed's existing retry callback.
    }
    if (!initialized.current && !page.pending && !page.error && items.length) {
      if (!positioned) latest();
      initialized.current = true; positioned = true;
    }
    if (!positioned && arrived && initialized.current && atTail.current && !page.pending && !pageIntent.current) latest();
    if (noteChanged && initialized.current) {
      if (atTail.current) latest();
      else restore(saved.current);
    }
    if (!arrived && !page.pending) atTail.current = isTail(history);
  }, [active, items, page.pending, page.error, windowReset]);

  return { historyRef, listRef, onScroll, onReadingIntent, onPointerDown, onPointerMove, onPointerEnd, onKeyDown,
    beforeOlder: () => beforePage("older"), beforeReset: () => beforePage("reset"),
    beginSend, confirmSend, windowReset };
}
