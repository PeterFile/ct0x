import { useCallback, useEffect, useId, useRef, useState, useSyncExternalStore } from "react";
import type { ReactNode } from "react";
import { ApiError, isDefinitiveRejection } from "./api";
import { decisionProjects, listPendingDecisions, prepareDecisionAnswer, validDecisionText } from "./decision-api";
import type { DecisionProject, DecisionReceipt, DecisionSelection, PendingDecision } from "./decision-api";
import "./pending-decisions.css";

type Row = { item: PendingDecision; project?: DecisionProject };
type Draft = { text: string; optionIndex: number };
const emptyDraft: Draft = { text: "", optionIndex: 0 };
type Send = ReturnType<typeof prepareDecisionAnswer>;
type Write = { kind: "posting"; send: Send } | { kind: "uncertain"; send: Send; message: string }
  | { kind: "rejected"; stale: boolean; message: string } | { kind: "saved"; receipt: DecisionReceipt };
type Interaction = { row: Row; draft?: Draft; write?: Write };
const binding = (item: PendingDecision) => [item.group_id, item.topic_id, item.id, item.task?.task_id ?? "", item.task?.task_revision ?? 0];
const requestKey = (item: PendingDecision) => JSON.stringify(binding(item));
const versionKey = (item: PendingDecision) => JSON.stringify([...binding(item), item.version]);

// One local-user workbench, shared by the global inbox and optional group mount.
// A navigation/unmount isn't a reason to abandon an unresolved write or fork its key.
let interactions = new Map<string, Interaction>();
const listeners = new Set<() => void>();
function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}
const snapshot = () => interactions;
function remember(row: Row, patch: Omit<Partial<Interaction>, "row">) {
  const key = versionKey(row.item);
  interactions = new Map(interactions).set(key, { ...interactions.get(key), ...patch, row });
  for (const listener of listeners) listener();
}

type Read = { groupId?: string; rows: Row[]; next: string; before: string; state: "loading" | "ready" | "error" | "unavailable"; message?: string };
const loading = (groupId?: string, before = "", rows: Row[] = []): Read => ({ groupId, rows, before, next: "", state: "loading" });
const errorText = (error: unknown) => error instanceof ApiError ? error.message : "请求未完成，请重试。";
const unavailable = (error: unknown) => error instanceof ApiError && error.status === 503;
const inGroup = (groupId: string | undefined, item: PendingDecision) => groupId === undefined || groupId === item.group_id;
const sourceLabel = ({ item, project }: Row) => `${project?.name ?? "项目未确认"} · ${item.group_name}`;
const originTitle = (item: PendingDecision) => item.task?.task_title ?? (item.topic_id === "" ? "主话题" : `话题 ${item.topic_id}`);

export function PendingDecisions({ active, groupId }: { active: boolean; groupId?: string }) {
  const sectionId = useId();
  const [read, setRead] = useState(() => loading(groupId));
  const page = read.groupId === groupId ? read : loading(groupId);
  const local = useSyncExternalStore(subscribe, snapshot, snapshot);
  const reading = useRef<AbortController | null>(null);
  const receiptList = useRef<HTMLElement>(null);
  const focusOnSave = useRef(false);

  const load = useCallback(async (before = "") => {
    reading.current?.abort();
    const controller = new AbortController();
    reading.current = controller;
    const { signal } = controller;
    setRead(previous => loading(groupId, before, before && previous.groupId === groupId ? previous.rows : []));
    try {
      const result = await listPendingDecisions(before, signal);
      if (signal.aborted) return;
      const projectFor = decisionProjects(signal);
      const rows = await Promise.all(result.items.filter(item => inGroup(groupId, item))
        .map(async item => ({ item, project: await projectFor(item.group_id) })));
      if (signal.aborted) return;
      setRead(previous => {
        const existing = before && previous.groupId === groupId ? previous.rows : [];
        const combined = new Map(existing.map(row => [row.item.id, row]));
        for (const row of rows) combined.set(row.item.id, row);
        return { groupId, rows: [...combined.values()], next: result.next_cursor, before, state: "ready" };
      });
    } catch (error) {
      if (!signal.aborted) setRead(previous => unavailable(error)
        ? { ...loading(groupId), state: "unavailable" }
        : { ...previous, state: "error", message: errorText(error) });
    }
  }, [groupId]);
  useEffect(() => {
    if (!active) return;
    void load();
    return () => reading.current?.abort();
  }, [active, load]);
  useEffect(() => {
    if (active && focusOnSave.current) receiptList.current?.focus();
    focusOnSave.current = false;
  }, [active, local]);

  async function submit(row: Row, selection?: DecisionSelection, origin?: HTMLElement | null) {
    if (!active || !inGroup(groupId, row.item)) return;
    const request = requestKey(row.item);
    // Consult the shared ledger before any await; a render is not the write lock.
    for (const entry of interactions.values()) {
      if ((entry.write?.kind === "posting" || entry.write?.kind === "saved") && requestKey(entry.row.item) === request) return;
    }
    const entry = interactions.get(versionKey(row.item));
    const previous = entry?.write;
    const replay = previous?.kind === "uncertain";
    if (replay ? selection !== undefined : (!selection || page.state !== "ready" || reading.current?.signal.aborted
      || !page.rows.includes(row) || (previous?.kind === "rejected" && previous.stale))) return;
    const source = replay && entry ? entry.row : row;
    let send: Send | undefined;
    try {
      send = previous?.kind === "uncertain" ? previous.send : selection
        ? prepareDecisionAnswer(row.item, selection, crypto.randomUUID(), groupId) : undefined;
      if (!send) return;
      remember(source, { write: { kind: "posting", send } });
      const receipt = await send();
      if (origin?.contains(document.activeElement)) focusOnSave.current = true;
      remember(source, { draft: undefined, write: { kind: "saved", receipt } });
    } catch (error) {
      const stale = error instanceof ApiError && (error.status === 404 || error.status === 409);
      const reason = unavailable(error) ? "待决答复暂不可用。"
        : error instanceof ApiError && error.code === "decision_relay_unsupported" ? "当前服务不支持回送此话题的答复。" : errorText(error);
      const message = reason + (stale ? "请刷新待决事项。" : "");
      if (send && (replay || !isDefinitiveRejection(error))) {
        remember(source, { write: { kind: "uncertain", send, message: `答复结果未确认。${message}可重试同一次答复。` } });
      } else {
        remember(source, { write: { kind: "rejected", stale, message } });
      }
    }
  }

  if (!active) return null;
  const scoped = [...local.values()].filter(({ row }) => inGroup(groupId, row.item));
  const saved = scoped.flatMap(({ row, write }) => write?.kind === "saved" ? [{ row, receipt: write.receipt }] : []);
  const answered = new Set(saved.map(({ row }) => requestKey(row.item)));
  const posting = new Set(scoped.flatMap(({ row, write }) => write?.kind === "posting" ? [requestKey(row.item)] : []));
  const rows = page.rows.filter(row => !answered.has(requestKey(row.item)));
  const listed = new Set(rows.map(row => versionKey(row.item)));
  const unresolved = scoped.filter(({ row, write }) => !answered.has(requestKey(row.item)) && !listed.has(versionKey(row.item))
    && (write?.kind === "posting" || write?.kind === "uncertain"));
  function writeStatus(row: Row, write?: Write): ReactNode {
    if (write?.kind === "posting") return <p role="status">正在保存答复…</p>;
    if (write?.kind === "rejected") return <p role="alert">{write.message}</p>;
    return write?.kind === "uncertain" ? <>
      <p role="alert">{write.message}</p>
      <button type="button" disabled={posting.has(requestKey(row.item))}
        onClick={event => void submit(row, undefined, event.currentTarget.parentElement)}>重试同一次答复</button>
    </> : null;
  }
  return <section className="pending-decisions" aria-labelledby={sectionId}>
    <header className="pending-decisions-toolbar">
      <h2 id={sectionId}>待决事项</h2>
      <button type="button" disabled={page.state === "loading"} onClick={() => void load()}>刷新待决事项</button>
    </header>
    {page.state === "loading" ? <p role="status">正在载入待决事项…</p> : null}
    {page.state === "error" || page.state === "unavailable" ? <div>
      <p role={page.state === "unavailable" ? "status" : "alert"}>{page.state === "unavailable" ? "待决答复暂不可用。" : page.message}</p>
      <button type="button" onClick={() => void load(page.before)}>重试载入待决事项</button>
    </div> : null}
    {page.state === "ready" && !rows.length ? <p role="status">{page.next ? "本页没有可显示的待决事项。" : "没有待决事项。"}</p> : null}
    {rows.length ? <ul className="pending-decisions-list" aria-label="待答复的问题">{rows.map(row => {
      const key = versionKey(row.item);
      const { draft = emptyDraft, write } = local.get(key) ?? {};
      const blocked = page.state !== "ready" || posting.has(requestKey(row.item)) || write?.kind === "uncertain"
        || (write?.kind === "rejected" && write.stale);
      return <li key={key}><DecisionCard row={row} draft={draft} blocked={blocked}
        onDraft={draft => { if (!blocked) remember(row, { draft }); }} onSubmit={(selection, card) => void submit(row, selection, card)}>
        {writeStatus(row, write)}
      </DecisionCard></li>;
    })}</ul> : null}
    {page.next ? <div><button type="button" onClick={() => void load(page.next)}>加载更多待决事项</button></div> : null}
    {unresolved.length ? <section className="pending-decisions-card" aria-label="尚未确认的答复">
      <h3>尚未确认的答复</h3>
      <ul className="pending-decisions-list">{unresolved.map(({ row, write }) => <li key={versionKey(row.item)}>
        <p>事项 <code>{row.item.id}</code> · 决定版本 {row.item.version}</p>
        {writeStatus(row, write)}
      </li>)}</ul>
    </section> : null}
    {saved.length ? <section className="pending-decisions-card" aria-label="答复回执" tabIndex={-1} ref={receiptList}>
      <h3>答复回执</h3>
      <ul className="pending-decisions-list">{saved.map(({ row, receipt }) => <li key={versionKey(row.item)}>
        <p role="status">答复已保存</p>
        <p className="pending-decisions-note">{sourceLabel(row)} · {originTitle(row.item)} · 决定版本 {receipt.version}</p>
        <p className="pending-decisions-copy">{receipt.text}</p>
        <p className="pending-decisions-note">回执 ID：<code>{receipt.id}</code></p>
        <SourceDetails row={row} />
      </li>)}</ul>
    </section> : null}
  </section>;
}

function SourceDetails({ row: { item, project } }: { row: Row }) {
  return <details>
    <summary>来源与版本标识</summary>
    <dl>
      {project ? <><dt>项目 ID</dt><dd><code>{project.id}</code></dd></> : null}
      <dt>群组 ID</dt><dd><code>{item.group_id}</code></dd>
      {item.task ? <><dt>任务 ID</dt><dd><code>{item.task.task_id}</code> · 任务版本 {item.task.task_revision}</dd></> : null}
      <dt>话题 ID</dt><dd>{item.topic_id === "" ? "主话题（空标识）" : <code>{item.topic_id}</code>}</dd>
      <dt>来源消息 ID</dt><dd><code>{item.source_message_id}</code></dd>
      <dt>事项 ID</dt><dd><code>{item.id}</code></dd>
    </dl>
  </details>;
}

function DecisionCard({ row, draft, blocked, onDraft, onSubmit, children }: {
  row: Row; draft: Draft; blocked: boolean; onDraft: (draft: Draft) => void;
  onSubmit: (selection: DecisionSelection, card: HTMLElement | null) => void; children: ReactNode;
}) {
  const field = useId(), { item } = row;
  const choosing = draft.optionIndex !== 0;
  const valid = choosing ? Boolean(item.choices?.[draft.optionIndex - 1]) : validDecisionText(draft.text);
  const title = originTitle(item);
  return <article className="pending-decisions-card" aria-label={`${sourceLabel(row)} · ${title}`}>
    <header>
      <p className="pending-decisions-note">{sourceLabel(row)}{!row.project?.name ? <> · <code>{item.group_id}</code></> : null}</p>
      <h3>{title}</h3>
      <p className="pending-decisions-note">决定版本 {item.version}</p>
    </header>
    <dl>
      <dt>已知事实</dt><dd className="pending-decisions-copy">{item.facts}</dd>
      <dt>问题</dt><dd id={`${field}-question`}>{item.question}</dd>
      <dt>建议</dt><dd className="pending-decisions-copy">{item.recommendation}</dd>
      {item.authorization ? <>
        <dt>拟授权范围</dt><dd className="pending-decisions-copy">{item.authorization.scope}</dd>
        <dt>影响</dt><dd className="pending-decisions-copy">{item.authorization.impact}</dd>
      </> : null}
    </dl>
    <SourceDetails row={row} />
    <form onSubmit={event => {
      event.preventDefault();
      if (!blocked && valid) onSubmit(choosing ? { option_index: draft.optionIndex } : { text: draft.text }, event.currentTarget.closest("article"));
    }}>
      <fieldset disabled={blocked} aria-describedby={`${field}-question`}>
        <legend>作答</legend>
        {item.choices?.map((choice, index) => <label className="pending-decisions-choice" key={index}>
          <input type="radio" name={`${field}-choice`} checked={draft.optionIndex === index + 1}
            onChange={() => onDraft({ ...draft, optionIndex: index + 1 })} />{choice}
        </label>)}
        {item.choices?.length ? <label className="pending-decisions-choice">
          <input type="radio" name={`${field}-choice`} checked={!choosing} onChange={() => onDraft({ ...draft, optionIndex: 0 })} />自拟答复
        </label> : null}
        <label htmlFor={field}>答复原文</label>
        <textarea id={field} rows={2} maxLength={4096} value={draft.text} disabled={choosing}
          onChange={event => onDraft({ ...draft, text: event.currentTarget.value })} />
        {!choosing && draft.text !== "" && !valid ? <p className="pending-decisions-note">答复不能只含空白，长度不能超过 4096 字节。</p> : null}
        <button type="submit" disabled={blocked || !valid}>提交答复</button>
      </fieldset>
    </form>
    {children}
  </article>;
}
