import { useEffect, useState } from "react";
import type { ReactNode } from "react";
import { ApiError } from "./api";
import { listTaskWorkInputs, listTaskWorkReports } from "./task-api";
import type { TaskWorkInput, TaskWorkReport } from "./task-api";

type Scope = { groupId: string; taskId: string };
const reportKindText = new Map([["progress", "进度报告"], ["question", "提问报告"], ["blocked", "受阻报告"], ["delivered", "交付报告"]]);

export function TaskWorkDetails({ groupId, taskId, active }: Scope & { active: boolean }) {
  return active ? <WorkDisclosure key={JSON.stringify([groupId, taskId])} groupId={groupId} taskId={taskId} /> : null;
}
function WorkDisclosure({ groupId, taskId }: Scope) {
  const [open, setOpen] = useState(false);
  return <details onToggle={event => { if (event.target === event.currentTarget) setOpen(event.currentTarget.open); }}>
    <summary>工作输入与报告</summary>
    {open ? <div className="tasks-lifecycle">
      <WorkSurface groupId={groupId} taskId={taskId} title="工作输入" load={listTaskWorkInputs} renderItem={item => <WorkInput item={item} />} />
      <WorkSurface groupId={groupId} taskId={taskId} title="工作报告" load={listTaskWorkReports} renderItem={item => <WorkReport item={item} />}
        note="成员报告不等于正式提交或用户验收。" />
    </div> : null}
  </details>;
}

function WorkSurface<T extends { id: string }>({ groupId, taskId, title, load, renderItem, note }: Scope & {
  title: string; load: (groupId: string, taskId: string, before: string, signal: AbortSignal) => Promise<{ items: T[]; next_cursor: string }>;
  renderItem: (item: T) => ReactNode; note?: string;
}) {
  const [request, setRequest] = useState({ before: "" });
  const [page, setPage] = useState<{ request: typeof request | null; items: T[]; next: string; error: string; unavailable: boolean }>(
    { request: null, items: [], next: "", error: "", unavailable: false });
  const pending = page.request !== request;
  useEffect(() => {
    const controller = new AbortController();
    // A closed or replaced panel cannot publish even when the transport ignores abort.
    void load(groupId, taskId, request.before, controller.signal).then(result => {
      if (controller.signal.aborted) return;
      setPage(previous => ({ request, next: result.next_cursor, error: "", unavailable: false,
        items: [...new Map([...(request.before ? previous.items : []), ...result.items].map(item => [item.id, item])).values()] }));
    }, error => {
      if (controller.signal.aborted) return;
      const unavailable = error instanceof ApiError && error.status === 404;
      setPage(previous => ({ request, items: unavailable ? [] : previous.items, next: "", unavailable,
        error: unavailable ? "" : error instanceof ApiError ? error.message : "请求未完成，请重试。" }));
    });
    return () => controller.abort();
  }, [groupId, taskId, request, load]);
  return <section className="tasks-delivery" aria-label={title} aria-busy={pending}>
    <h3>{title}</h3>
    {pending ? <p role="status">正在读取{title}…</p> : page.unavailable ? <p className="tasks-note">当前服务不提供{title}。</p>
      : !page.error && !page.items.length ? <p className="tasks-note">未返回{title}记录。</p> : null}
    {page.items.length ? <>
      {note ? <p className="tasks-note">{note}</p> : null}
      <ul className="tasks-list">{page.items.map(item => <li key={item.id} className="tasks-delivery">{renderItem(item)}</li>)}</ul>
    </> : null}
    {page.error ? <div className="tasks-lifecycle">
      {!pending ? <p role="alert">{title}读取未确认。{page.error}</p> : null}
      <div className="tasks-actions"><button type="button" disabled={pending} onClick={() => setRequest({ before: request.before })}>重试读取{title}</button></div>
    </div> : null}
    {page.next ? <div className="tasks-actions">
      <button type="button" disabled={pending} onClick={() => setRequest({ before: page.next })}>加载更多{title}</button>
    </div> : null}
  </section>;
}

function RecordMeta({ item }: { item: Pick<TaskWorkInput, "task_revision" | "created_at"> }) {
  return <p className="tasks-meta">任务版本 {item.task_revision} · <time dateTime={item.created_at}>{item.created_at}</time></p>;
}
function Identifiers({ entries }: { entries: [string, string | undefined][] }) {
  return <details><summary className="tasks-note">标识详情</summary>
    <dl>{entries.map(([label, id]) => id ? <div key={label}><dt>{label}</dt><dd className="tasks-meta">{id}</dd></div> : null)}</dl>
  </details>;
}
function Constraint({ label, revision, text }: { label: string; revision?: number; text?: string }) {
  if (revision === undefined && text === undefined) return null;
  return <><dt>{label}{revision === undefined ? "" : ` · 版本 ${revision}`}</dt>
    <dd className="tasks-copy">{text || (revision === 0 && text === "" ? "无补充约束" : "未提供")}</dd></>;
}
function WorkInput({ item }: { item: TaskWorkInput }) {
  const confirmation = item.consumption;
  const hasConstraint = item.before_constraint_revision !== undefined || item.before_constraint !== undefined
    || item.after_constraint_revision !== undefined || item.after_constraint !== undefined;
  return <>
    <p><strong>{confirmation ? "已明确确认处理" : "待确认处理"}</strong>{confirmation ? <>
      {" · "}<time className="tasks-meta" dateTime={confirmation.consumed_at}>{confirmation.consumed_at}</time>
    </> : null}</p>
    <RecordMeta item={item} />
    {item.content || hasConstraint ? <dl>
      {item.content ? <><dt>用户原文</dt><dd className="tasks-copy">{item.content}</dd></> : null}
      <Constraint label="输入前约束" revision={item.before_constraint_revision} text={item.before_constraint} />
      <Constraint label="输入后约束" revision={item.after_constraint_revision} text={item.after_constraint} />
    </dl> : null}
    <Identifiers entries={[["输入标识", item.id], ["话题标识", item.topic_id], ["来源标识", item.source_id],
      ["用户消息标识", item.reply_message_id], ["通知消息标识", item.notice_message_id], ["触发标识", item.trigger_id],
      ["授权标识", item.mandate_id], ["分配标识", item.assignment_id], ["执行成员标识", item.worker_member_id],
      ["确认轮次标识", confirmation?.turn_id]]} />
  </>;
}
function WorkReport({ item }: { item: TaskWorkReport }) {
  return <>
    <p><strong>{reportKindText.get(item.kind) ?? "未知报告类型"}</strong></p>
    <RecordMeta item={item} />
    <p className="tasks-copy">{item.summary}</p>
    <Identifiers entries={[["报告标识", item.id], ["话题标识", item.topic_id],
      ["报告成员标识", item.actor_id], ["责任成员标识", item.owner_member_id]]} />
  </>;
}
