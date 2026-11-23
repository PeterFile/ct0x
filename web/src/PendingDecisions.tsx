import { useCallback, useEffect, useId, useRef, useState, useSyncExternalStore } from "react";
import type { ReactNode } from "react";
import { ApiError, isDefinitiveRejection } from "./api";
import { decisionHistory, decisionKey, decisionProjects, decisionReference, decisionWriteSlot, isDecisionAnswersUnavailable,
  isPendingDecisionsUnavailable, listPendingDecisions, prepareDecisionAnswer, readDecisionAnswer, requestKey, validDecisionText } from "./decision-api";
import type { DecisionAnswerView, DecisionAttempt, DecisionJournalEntry, DecisionProject, DecisionReference, DecisionSelection, PendingDecision } from "./decision-api";
import { journalDecisionAttempt, journalLimit, loadDecisionJournal, settleDecisionAttempt } from "./decision-journal";
import "./pending-decisions.css";

type Row = { item: PendingDecision; project?: DecisionProject };
type Draft = { text: string; optionIndex: number };
const emptyDraft: Draft = { text: "", optionIndex: 0 };
type Post = Awaited<ReturnType<typeof prepareDecisionAnswer>>["post"];
type Write = { kind: "posting" } | { kind: "uncertain"; record: DecisionJournalEntry; post?: Post; message: string }
  | { kind: "rejected"; stale: boolean; message: string }
  | ({ kind: "saved"; record: DecisionJournalEntry } & DecisionAnswerView);
type Interaction = { reference: DecisionReference; item?: PendingDecision; draft?: Draft; write?: Write };
const inGroup = (groupId: string | undefined, source: { group_id: string }) => groupId === undefined || groupId === source.group_id;
const unconfirmed = "答复结果未确认。";
const visible = () => document.visibilityState !== "hidden";

// Keep older unknowns for reconciliation without fencing a newer pending
// version. The Engine still checks the version and enforces uniqueness.
function blocksAnswer({ reference, write }: Interaction, version: number, replay?: Write) {
  return write !== replay && (write?.kind === "posting" || write?.kind === "saved"
    || (write?.kind === "uncertain" && reference.version >= version));
}

// One local-user workbench, shared by the global inbox and optional group mount.
// Admitted writes outlive a view; optional labels and status reads do not.
let interactions = new Map<string, Interaction>();
let restored = false;
const listeners = new Set<() => void>();
const checkOnWrite = new Set<(reference: DecisionReference) => void>();
function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}
const snapshot = () => interactions;
function remember(reference: DecisionReference, patch: Omit<Partial<Interaction>, "reference">, evicted?: DecisionReference) {
  const key = decisionKey(reference);
  interactions = new Map(interactions).set(key, { ...interactions.get(key), ...patch, reference });
  if (evicted) interactions.delete(decisionKey(evicted));
  for (const listener of listeners) listener();
}
function restoreJournal() {
  if (restored) return;
  const entries = loadDecisionJournal();
  if (!entries) return;
  restored = true;
  for (const record of entries) {
    if (interactions.get(decisionKey(record.ref))?.write) continue;
    remember(record.ref, { write: { kind: "uncertain", record, message: `${unconfirmed}请核对保存状态。` } });
  }
}

async function confirmAnswer(expected: Write, record: DecisionJournalEntry, view: DecisionAnswerView,
  { signal, beforeSave, onSaved }: { signal?: AbortSignal; beforeSave?: () => void; onSaved?: () => void }) {
  const history = "key" in record ? await decisionHistory(record.ref, view.receipt).catch(() => undefined) : undefined;
  if ((signal && (signal.aborted || !visible())) || interactions.get(decisionKey(record.ref))?.write !== expected) return false;
  const pointer = history && "key" in record && settleDecisionAttempt(record, history) ? history : record;
  beforeSave?.();
  remember(record.ref, { draft: undefined, write: { kind: "saved", record: pointer, ...view } });
  // Restoring a previously saved receipt does not confirm a new write.
  if ("key" in record) onSaved?.();
  return true;
}

const lastStatusCheck = new WeakMap<DecisionReference, number>();
let statusSequence = 0;

async function checkAnswer(key: string, signal: AbortSignal, onSaved?: () => void) {
  const entry = interactions.get(key), write = entry?.write;
  if (!entry || !write || (write.kind !== "uncertain" && write.kind !== "saved") || (write.kind === "saved" && write.handling)) return false;
  lastStatusCheck.set(entry.reference, ++statusSequence);
  try {
    const answer = await readDecisionAnswer(write.record, signal, write.kind === "saved" ? write.receipt : undefined);
    if (signal.aborted || !visible() || interactions.get(key)?.write !== write) return false;
    if (!answer) return write.kind === "uncertain" && "key" in write.record;
    if (write.kind === "saved") {
      if (answer.handling) remember(entry.reference, { write: { ...write, handling: answer.handling } });
    } else if (!await confirmAnswer(write, write.record, answer, { signal, onSaved })) return false;
    return answer.handling === null;
  } catch {
    // Stop automatic reads for this reference on an error. Neither an error nor
    // a missing receipt can erase proven state or permit another answer.
    return false;
  }
}

const statusBackoff = [2000, 4000, 8000, 12000, 20000, 20000];
const statusBatch = 4;
function useAnswerChecks(groupId: string | undefined, refreshVersion: number, onSaved?: () => void) {
  useEffect(() => {
    let wave: AbortController | undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;
    function stop() { wave?.abort(); clearTimeout(timer); }
    function eligible() {
      // Memory-only history can exceed the journal limit. Check the least
      // recently visited first so interrupted waves still make progress.
      return [...interactions].reverse().filter(([, { reference, write }]) => inGroup(groupId, reference)
        && (write?.kind === "uncertain" || (write?.kind === "saved" && !write.handling)))
        .sort(([, a], [, b]) => (lastStatusCheck.get(a.reference) ?? 0) - (lastStatusCheck.get(b.reference) ?? 0))
        .map(([key]) => key);
    }
    function start(keys = eligible(), immediate = journalLimit) {
      stop();
      if (!keys.length || !visible()) return;
      const controller = new AbortController();
      wave = controller;
      const { signal } = controller;
      const watching = () => !signal.aborted && visible();
      const firstPass = keys.slice(0, immediate);
      const queue = keys.slice(immediate);
      async function checkBatch(batch: string[]) {
        const repeat = await Promise.all(batch.map(key => checkAnswer(key, signal, onSaved)));
        queue.push(...batch.filter((_, index) => repeat[index]));
      }
      function poll(round: number, limit: number) {
        if (!watching() || !queue.length || round >= limit) return;
        const delay = statusBackoff[Math.min(round, statusBackoff.length - 1)];
        timer = setTimeout(async () => {
          if (!watching()) return;
          await checkBatch(queue.splice(0, statusBatch));
          poll(round + 1, limit);
        }, delay);
      }
      async function reconcile() {
        for (let i = 0; i < firstPass.length; i += statusBatch) {
          if (!watching()) return;
          await checkBatch(firstPass.slice(i, i + statusBatch));
        }
        // Size the round limit once from the initial backoff queue. Repeats
        // go behind deferred records and cannot extend this wave.
        poll(0, Math.max(statusBackoff.length, Math.ceil(queue.length / statusBatch)));
      }
      void reconcile();
    }
    // A write checks its own status promptly without dropping the rest
    // from the backoff queue.
    function afterWrite(reference: DecisionReference) {
      if (!inGroup(groupId, reference)) return;
      const key = decisionKey(reference);
      start([key, ...eligible().filter(other => other !== key)], statusBatch);
    }
    const visibility = () => start();
    checkOnWrite.add(afterWrite);
    document.addEventListener("visibilitychange", visibility);
    start();
    return () => { stop(); checkOnWrite.delete(afterWrite); document.removeEventListener("visibilitychange", visibility); };
  }, [groupId, refreshVersion, onSaved]);
}

type Read = { rows: PendingDecision[]; next: string; before: string; state: "loading" | "ready" | "error" | "unavailable"; message?: string };
const loading = (before = "", rows: PendingDecision[] = []): Read => ({ rows, before, next: "", state: "loading" });
const errorText = (error: unknown) => error instanceof ApiError ? error.message : "请求未完成，请重试。";
const sourceLabel = ({ item, project }: Row) => `${project?.name ?? "项目未确认"} · ${item.group_name}`;
const originTitle = (item: PendingDecision) => item.task?.task_title ?? (item.topic_id === "" ? "主话题" : `话题 ${item.topic_id}`);

type DecisionInboxProps = { groupId?: string; onSaved?: () => void };

export function PendingDecisions({ active, groupId, onSaved }: DecisionInboxProps & { active: boolean }) {
  return active ? <DecisionInbox key={JSON.stringify(groupId ?? null)} groupId={groupId} onSaved={onSaved} /> : null;
}

function DecisionInbox({ groupId, onSaved }: DecisionInboxProps) {
  const sectionId = useId();
  const [page, setRead] = useState(() => loading());
  const [projects, setProjects] = useState(() => new Map<string, DecisionProject>());
  const [statusVersion, setStatusVersion] = useState(0);
  const local = useSyncExternalStore(subscribe, snapshot, snapshot);
  const projectLookup = useRef<ReturnType<typeof decisionProjects>>(null);
  const reading = useRef<AbortController | null>(null);
  const receiptList = useRef<HTMLElement>(null);
  const focusOnSave = useRef(false);

  const load = useCallback(async (before = "") => {
    reading.current?.abort();
    const controller = new AbortController();
    reading.current = controller;
    const { signal } = controller;
    setRead(previous => loading(before, before ? previous.rows : []));
    try {
      const result = await listPendingDecisions(before, signal);
      if (signal.aborted) return;
      const items = result.items.filter(item => inGroup(groupId, item));
      setRead(previous => {
        const combined = new Map((before ? previous.rows : []).map(item => [item.id, item]));
        for (const item of items) combined.set(item.id, item);
        return { rows: [...combined.values()], next: result.next_cursor, before, state: "ready" };
      });
      projectLookup.current?.(items.map(item => item.group_id));
    } catch (error) {
      if (!signal.aborted) setRead(previous => isPendingDecisionsUnavailable(error)
        ? { ...loading(), state: "unavailable" }
        : { ...previous, state: "error", message: errorText(error) });
    }
  }, [groupId]);
  useEffect(() => {
    const scope = new AbortController();
    projectLookup.current = decisionProjects(scope.signal, (groupId, project) => {
      setProjects(previous => new Map(previous).set(groupId, project));
    });
    restoreJournal();
    void load();
    return () => { scope.abort(); reading.current?.abort(); };
  }, [load]);
  useAnswerChecks(groupId, statusVersion, onSaved);
  useEffect(() => {
    if (focusOnSave.current) receiptList.current?.focus();
    focusOnSave.current = false;
  }, [local]);

  function refresh(before = "") {
    restoreJournal();
    setStatusVersion(previous => previous + 1);
    void load(before);
  }

  const itemsById = new Map(page.rows.map(item => [item.id, item]));
  function hasNewerRow(reference: DecisionReference) {
    const item = itemsById.get(reference.request_id);
    return item !== undefined && item.group_id === reference.group_id && item.version > reference.version;
  }

  async function submit(reference: DecisionReference, item?: PendingDecision, selection?: DecisionSelection, origin?: HTMLElement | null) {
    if (!inGroup(groupId, reference)) return;
    restoreJournal();
    const key = decisionKey(reference), slot = decisionWriteSlot(reference);
    const previous = interactions.get(key)?.write;
    const replay = previous?.kind === "uncertain" ? previous : undefined;
    const current = item && page.state === "ready" && !reading.current?.signal.aborted && itemsById.get(item.id) === item;
    if (replay ? hasNewerRow(reference) || !("key" in replay.record) || (replay.post ? selection !== undefined : !current || !selection)
      : !current || !selection || (previous?.kind === "rejected" && previous.stale)) return;
    // Take the same-document lock before hashing or reading. The server, not
    // browser storage, enforces a single effect across independent tabs.
    for (const entry of interactions.values()) {
      if (decisionWriteSlot(entry.reference) === slot && blocksAnswer(entry, reference.version, replay)) return;
    }
    const posting: Write = { kind: "posting" };
    remember(reference, { ...(item ? { item } : {}), write: posting });
    const focus = () => { if (origin?.contains(document.activeElement)) focusOnSave.current = true; };
    const wake = () => { for (const listener of checkOnWrite) listener(reference); };
    let attempt: DecisionAttempt | undefined, post: Post | undefined, sent = false;
    try {
      if (replay && "key" in replay.record) {
        attempt = replay.record;
        post = replay.post;
        if (!post) {
          const prepared = await prepareDecisionAnswer(item!, selection!, attempt.key, attempt.scope === "group" ? reference.group_id : undefined);
          if (prepared.attempt.digest !== attempt.digest) throw new ApiError("输入与原答复不一致。请原样重选或重输。");
          post = prepared.post;
        }
        const found = await readDecisionAnswer(attempt, new AbortController().signal);
        if (found) {
          if (await confirmAnswer(posting, attempt, found, { beforeSave: focus, onSaved })) wake();
          return;
        }
      } else {
        const prepared = await prepareDecisionAnswer(item!, selection!, crypto.randomUUID(), groupId);
        attempt = prepared.attempt;
        post = prepared.post;
        const recovery = journalDecisionAttempt(attempt);
        if (recovery?.evicted) remember(reference, {}, recovery.evicted.ref);
      }
      sent = true;
      const receipt = await post();
      if (await confirmAnswer(posting, attempt, { receipt, handling: null }, { beforeSave: focus, onSaved })) wake();
    } catch (error) {
      const stale = error instanceof ApiError && (error.status === 404 || error.status === 409);
      const reason = isDecisionAnswersUnavailable(error) ? "待决答复暂不可用。"
        : error instanceof ApiError && error.code === "decision_relay_unsupported" ? "当前服务不支持回送此话题的答复。"
        : errorText(error);
      if (attempt && (replay || (sent && !isDefinitiveRejection(error)))) {
        remember(reference, { write: { kind: "uncertain", record: attempt, post: sent ? post : replay?.post, message: unconfirmed + reason } });
        if (sent) wake();
      } else {
        if (attempt && sent) settleDecisionAttempt(attempt);
        remember(reference, { write: { kind: "rejected", stale, message: reason + (stale ? "请刷新待决事项。" : "") } });
      }
    }
  }

  const scoped = [...local.values()].filter(({ reference }) => inGroup(groupId, reference));
  const saved = scoped.flatMap(({ reference, item, write }) => write?.kind === "saved" ? [{ reference, item, ...write }] : []);
  const answered = new Set(saved.map(({ reference }) => requestKey(reference)));
  const writesByIssue = new Map<string, Interaction[]>();
  for (const entry of scoped) {
    if (!entry.write || entry.write.kind === "rejected") continue;
    const slot = decisionWriteSlot(entry.reference);
    const writes = writesByIssue.get(slot);
    if (writes) writes.push(entry);
    else writesByIssue.set(slot, [entry]);
  }
  const writeLocked = (reference: DecisionReference, replay?: Write) =>
    writesByIssue.get(decisionWriteSlot(reference))?.some(entry => blocksAnswer(entry, reference.version, replay)) ?? false;
  const rows = page.rows.map(item => ({ item, reference: decisionReference(item), project: projects.get(item.group_id) }))
    .filter(({ reference }) => !answered.has(requestKey(reference)));
  const listed = new Set(rows.map(({ reference }) => decisionKey(reference)));
  const unresolved = scoped.filter(({ reference, write }) => !listed.has(decisionKey(reference))
    && (write?.kind === "posting" || write?.kind === "uncertain"));
  function writeStatus(reference: DecisionReference, write?: Write): ReactNode {
    if (write?.kind === "posting") return <p role="status">正在核对或保存答复…</p>;
    if (write?.kind === "rejected") return <p role="alert">{write.message}</p>;
    return write?.kind === "uncertain" ? <>
      <p role="alert">{write.message}</p>
      {write.post && !hasNewerRow(reference) && !writeLocked(reference, write)
        ? <button type="button" onClick={event => void submit(reference, undefined, undefined, event.currentTarget.parentElement)}>
          重试同一次答复
        </button> : null}
    </> : null;
  }
  return <section className="pending-decisions" aria-labelledby={sectionId}>
    <header className="pending-decisions-toolbar">
      <h3 id={sectionId}>待决事项</h3>
      <button type="button" disabled={page.state === "loading"} onClick={() => refresh()}>刷新待决事项</button>
    </header>
    {page.state === "loading" ? <p role="status">正在载入待决事项…</p> : null}
    {page.state === "error" || page.state === "unavailable" ? <div>
      <p role={page.state === "unavailable" ? "status" : "alert"}>{page.state === "unavailable" ? "待决答复暂不可用。" : page.message}</p>
      <button type="button" onClick={() => refresh(page.before)}>重试载入待决事项</button>
    </div> : null}
    {page.state === "ready" && !rows.length ? <p role="status">{page.next ? "本页没有可显示的待决事项。" : "没有待决事项。"}</p> : null}
    {rows.length ? <ul className="pending-decisions-list" aria-label="待答复的问题">{rows.map(row => {
      const { item, reference } = row, key = decisionKey(reference);
      const { draft = emptyDraft, write } = local.get(key) ?? {};
      const reenter = write?.kind === "uncertain" && "key" in write.record && !write.post;
      const blocked = page.state !== "ready" || writeLocked(reference, reenter ? write : undefined) || (write?.kind === "rejected" && write.stale);
      return <li key={key}><DecisionCard row={row} draft={draft} blocked={blocked} reenter={reenter}
        onDraft={draft => { if (!blocked) remember(reference, { item, draft }); }}
        onSubmit={(selection, card) => void submit(reference, item, selection, card)}>
        {writeStatus(reference, write)}
      </DecisionCard></li>;
    })}</ul> : null}
    {page.next ? <div><button type="button" onClick={() => void load(page.next)}>加载更多待决事项</button></div> : null}
    {unresolved.length ? <section className="pending-decisions-card" aria-label="尚未确认的答复">
      <h4>尚未确认的答复</h4>
      <ul className="pending-decisions-list">{unresolved.map(({ reference, item, write }) => <li key={decisionKey(reference)}>
        <p>事项 <code>{reference.request_id}</code> · 决定版本 {reference.version}</p>
        <SourceDetails reference={reference} item={item} project={projects.get(reference.group_id)} />
        {writeStatus(reference, write)}
      </li>)}</ul>
    </section> : null}
    {saved.length ? <section className="pending-decisions-card" aria-label="答复回执" tabIndex={-1} ref={receiptList}>
      <h4>答复回执</h4>
      <ul className="pending-decisions-list">{saved.map(({ reference, item, receipt, handling }) => {
        const project = projects.get(reference.group_id);
        return <li key={decisionKey(reference)}>
          <p role="status">答复已保存</p>
          {handling ? <p role="status">负责人已处理</p> : <p className="pending-decisions-note">尚未确认负责人处理。</p>}
          <p className="pending-decisions-note">{item ? `${sourceLabel({ item, project })} · ${originTitle(item)}` : `${project?.name ?? "项目未确认"} · ${reference.group_id}`} · 决定版本 {receipt.version}</p>
          <p className="pending-decisions-copy">{receipt.text}</p>
          <p className="pending-decisions-note">回执 ID：<code>{receipt.id}</code></p>
          <SourceDetails reference={reference} item={item} project={project} />
        </li>;
      })}</ul>
    </section> : null}
  </section>;
}

function SourceDetails({ reference, item, project }: { reference: DecisionReference; item?: PendingDecision; project?: DecisionProject }) {
  return <details>
    <summary>来源与版本标识</summary>
    <dl>
      {project ? <><dt>项目 ID</dt><dd><code>{project.id}</code></dd></> : null}
      <dt>群组 ID</dt><dd><code>{reference.group_id}</code></dd>
      {reference.task_id ? <><dt>任务 ID</dt><dd><code>{reference.task_id}</code> · 任务版本 {reference.task_revision}</dd></> : null}
      <dt>话题 ID</dt><dd>{reference.topic_id === "" ? "主话题（空标识）" : <code>{reference.topic_id}</code>}</dd>
      {item ? <><dt>来源消息 ID</dt><dd><code>{item.source_message_id}</code></dd></> : null}
      <dt>事项 ID</dt><dd><code>{reference.request_id}</code></dd>
    </dl>
  </details>;
}

function DecisionCard({ row, draft, blocked, reenter, onDraft, onSubmit, children }: {
  row: Row; draft: Draft; blocked: boolean; reenter: boolean; onDraft: (draft: Draft) => void;
  onSubmit: (selection: DecisionSelection, card: HTMLElement | null) => void; children: ReactNode;
}) {
  const field = useId(), { item } = row;
  const choosing = draft.optionIndex !== 0;
  const valid = choosing ? Boolean(item.choices?.[draft.optionIndex - 1]) : validDecisionText(draft.text);
  const title = originTitle(item);
  return <article className="pending-decisions-card" aria-label={`${sourceLabel(row)} · ${title}`}>
    <header>
      <p className="pending-decisions-note">{sourceLabel(row)}{!row.project ? <> · <code>{item.group_id}</code></> : null}</p>
      <h4>{title}</h4>
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
    <SourceDetails reference={decisionReference(item)} item={item} project={row.project} />
    {reenter ? <p className="pending-decisions-note">如需重试，请原样重选或重输答复；重试前会核对保存状态。</p> : null}
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
        <button type="submit" disabled={blocked || !valid}>{reenter ? "重试同一次答复" : "提交答复"}</button>
      </fieldset>
    </form>
    {children}
  </article>;
}
