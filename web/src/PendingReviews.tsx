import { useCallback, useEffect, useId, useRef, useState } from "react";
import type { ReactNode } from "react";
import { ApiError, isDefinitiveRejection } from "./api";
import { listPendingReviews, reviewTask } from "./task-api";
import type { PendingReview, TaskReview, TaskReviewInput } from "./task-api";
import "./pending-reviews.css";

type Operation = { key: string; item: PendingReview; input: TaskReviewInput };
type WriteState =
  | { kind: "posting"; operation: Operation }
  | { kind: "uncertain"; operation: Operation; error: string }
  | { kind: "rejected"; error: string; stale?: PendingReview }
  | { kind: "saved"; item: PendingReview; receipt: TaskReview };
type Page = { items: PendingReview[]; next: string; pending: boolean; error: string; before: string };
const errorText = (error: unknown) => error instanceof ApiError ? error.message : "请求未完成，请重试。";
const itemKey = (item: PendingReview) => item.submission.id;
const outcomeText = { accepted: "接纳", rejected: "拒收" };

export function PendingReviews({ active }: { active: boolean }) {
  const [page, setPage] = useState<Page>({ items: [], next: "", pending: true, error: "", before: "" });
  const [drafts, setDrafts] = useState(() => new Map<string, string>());
  const [writes, setWrites] = useState(() => new Map<string, WriteState>());
  const ledger = useRef(writes);
  const reading = useRef<AbortController | null>(null);
  const mounted = useRef(false);
  const focusReceipt = useRef(false);
  const receipts = useRef<HTMLElement>(null);
  const readOnly = page.before === "" && (page.pending || Boolean(page.error));
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);

  const load = useCallback(async (before = "") => {
    reading.current?.abort();
    const controller = new AbortController();
    reading.current = controller;
    setPage(previous => ({ ...previous, before, pending: true, error: "" }));
    try {
      const result = await listPendingReviews(before, controller.signal);
      if (controller.signal.aborted) return;
      setPage(previous => {
        const items = new Map((before ? previous.items : []).map(item => [itemKey(item), item]));
        for (const item of result.items) items.set(itemKey(item), item);
        return { items: [...items.values()], next: result.next_cursor, pending: false, error: "", before };
      });
    } catch (error) {
      if (!controller.signal.aborted) setPage(previous => ({ ...previous, pending: false, error: errorText(error) }));
    }
  }, []);
  useEffect(() => {
    if (!active) return;
    void load();
    return () => reading.current?.abort();
  }, [active, load]);

  function record(id: string, state: WriteState) {
    // Fence rapid clicks synchronously; rendering is not the write lock.
    ledger.current = new Map(ledger.current).set(id, state);
    setWrites(ledger.current);
  }
  async function decide(item: PendingReview, outcome?: TaskReview["outcome"], card?: HTMLElement) {
    const id = itemKey(item), state = ledger.current.get(id);
    if (!active || state?.kind === "saved" || state?.kind === "posting") return;
    const retry = state?.kind === "uncertain";
    let operation: Operation;
    if (retry) {
      if (outcome !== undefined) return;
      operation = state.operation;
    } else {
      if (!outcome || readOnly || !page.items.includes(item) || (state?.kind === "rejected" && state.stale === item)) return;
      operation = { item, key: crypto.randomUUID(), input: Object.freeze({ expected_revision: item.submission.task_revision,
        submission_id: id, outcome, reason: drafts.get(id) ?? "" }) };
    }
    record(id, { kind: "posting", operation });
    try {
      const source = operation.item;
      const receipt = await reviewTask(source.group.id, source.task.id, operation.input, source.submission, operation.key);
      if (!mounted.current) return;
      if (card?.contains(document.activeElement)) focusReceipt.current = true;
      record(id, { kind: "saved", item: source, receipt });
      setDrafts(previous => { const next = new Map(previous); next.delete(id); return next; });
    } catch (error) {
      if (!mounted.current) return;
      const uncertain = retry || !isDefinitiveRejection(error);
      if (uncertain) {
        record(id, { kind: "uncertain", operation, error: `验收结果未确认。${errorText(error)} 请重试同一次验收。` });
      } else {
        const stale = error instanceof ApiError && (error.status === 404 || error.status === 409);
        record(id, { kind: "rejected", stale: stale ? item : undefined,
          error: stale ? `${errorText(error)} 请刷新待验收事项。` : errorText(error) });
      }
    }
  }

  const items = page.items.filter(item => writes.get(itemKey(item))?.kind !== "saved");
  const listed = new Set(items.map(itemKey));
  const unconfirmed = [...writes].flatMap(([id, state]) => !listed.has(id) && (state.kind === "posting" || state.kind === "uncertain")
    ? [state] : []);
  const saved = [...writes.values()].filter(state => state.kind === "saved");
  useEffect(() => {
    if (focusReceipt.current) {
      if (active) receipts.current?.focus();
      focusReceipt.current = false;
    }
  }, [active, writes]);
  const mutationStatus = (state: WriteState | undefined) => {
    if (state?.kind === "posting") return <p role="status">正在保存验收决定…</p>;
    if (state?.kind === "uncertain") return <>
      <p role="alert">{state.error}</p>
      <button type="button" onClick={() => void decide(state.operation.item)}>重试同一次验收</button>
    </>;
    return state?.kind === "rejected" ? <p role="alert">{state.error}</p> : null;
  };
  return <div className="review-inbox">
    <header className="review-inbox-toolbar">
      <div><h2>待我决定</h2><p className="review-inbox-note">各群组当前待验收的交付</p></div>
      <button type="button" disabled={page.pending} onClick={() => void load()}>刷新待验收事项</button>
    </header>
    {page.pending ? <p role="status">正在载入待验收事项…</p> : null}
    {page.error ? <div role="alert"><p>{page.error}</p>
      <button type="button" onClick={() => void load(page.before)}>重试载入待验收事项</button></div> : null}
    {!page.pending && !page.error && !items.length ? <p role="status">没有待验收事项。</p> : null}
    {items.length > 0 ? <ul className="review-inbox-list" aria-label="待验收事项">
      {items.map(item => {
        const id = itemKey(item), state = writes.get(id);
        return <li key={id}><ReviewCard item={item} reason={drafts.get(id) ?? ""}
          blocked={readOnly || state?.kind === "posting" || state?.kind === "uncertain" || (state?.kind === "rejected" && state.stale === item)}
          onReason={reason => setDrafts(previous => new Map(previous).set(id, reason))}
          onDecision={(outcome, card) => void decide(item, outcome, card)}>
          {mutationStatus(state)}
        </ReviewCard></li>;
      })}
    </ul> : null}
    {page.next && !page.error ? <div><button type="button" disabled={page.pending}
      onClick={() => void load(page.next)}>加载更多待验收事项</button></div> : null}
    {unconfirmed.length > 0 ? <section className="review-inbox-records" aria-label="尚未确认的验收">
      <h3>尚未确认的验收</h3>
      {unconfirmed.map(state => <div key={itemKey(state.operation.item)}>
        <p className="review-inbox-note">提交标识：{state.operation.item.submission.id}</p>
        {mutationStatus(state)}
      </div>)}
    </section> : null}
    {saved.length > 0 ? <section className="review-inbox-records" aria-label="验收回执" ref={receipts} tabIndex={-1}>
      <h3>验收回执</h3>
      {saved.map(({ item, receipt }) => <div key={receipt.id} className="review-inbox-receipt">
        <p role="status">已保存：{outcomeText[receipt.outcome]} · {item.group.name} / {item.task.title} · 任务版本 {receipt.task_revision}</p>
        {receipt.reason ? <p className="review-inbox-copy">{receipt.reason}</p> : null}
        <p className="review-inbox-note">验收标识：<code>{receipt.id}</code></p>
      </div>)}
    </section> : null}
  </div>;
}

function ReviewCard({ item, reason, blocked, onReason, onDecision, children }: {
  item: PendingReview; reason: string; blocked: boolean; onReason: (reason: string) => void;
  onDecision: (outcome: TaskReview["outcome"], card: HTMLElement | undefined) => void; children: ReactNode;
}) {
  const field = useId();
  const submission = item.submission;
  return <article className="review-inbox-card" aria-label={`${item.group.name} · ${item.task.title}`}>
    <header>
      <p className="review-inbox-note">来源群组：{item.group.name}</p>
      <h3>{item.task.title}</h3>
      <p className="review-inbox-note">任务版本 {submission.task_revision}</p>
    </header>
    <p>是否接纳这份交付？</p>
    <dl>
      <dt>验收条件</dt><dd className="review-inbox-copy">{item.task.acceptance}</dd>
      <dt>提交摘要</dt><dd className="review-inbox-copy">{submission.summary}</dd>
    </dl>
    <details>
      <summary>来源与版本标识</summary>
      <dl>
        <dt>群组 ID</dt><dd>{item.group.id}</dd>
        <dt>任务 ID</dt><dd>{item.task.id}</dd>
        {item.topic_id ? <><dt>话题 ID</dt><dd>{item.topic_id}</dd></> : null}
        <dt>提交 ID</dt><dd>{submission.id}</dd>
        {submission.artifact_versions.length > 0 ? <><dt>冻结成果版本</dt><dd><ul>
          {submission.artifact_versions.map(ref => <li key={JSON.stringify([ref.artifact_id, ref.version_id])}>
            {ref.artifact_id} / {ref.version_id}
          </li>)}
        </ul></dd></> : null}
      </dl>
    </details>
    <fieldset disabled={blocked}>
      <label htmlFor={field}>验收理由（可选）</label>
      <textarea id={field} rows={2} value={reason} onChange={event => onReason(event.currentTarget.value)} />
      <div className="review-inbox-actions">
        {(["accepted", "rejected"] as const).map(outcome => <button key={outcome} type="button"
          onClick={event => onDecision(outcome, event.currentTarget.closest("article") ?? undefined)}>{outcomeText[outcome]}</button>)}
      </div>
    </fieldset>
    {children}
  </article>;
}
