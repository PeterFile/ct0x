import { useEffect, useId, useRef, useState } from "react";
import type { FormEvent } from "react";
import { ApiError, isDefinitiveRejection } from "./api";
import type { Group } from "./api";
import { createTask, getTask, listTasks } from "./task-api";
import type { Task, TaskInput } from "./task-api";
import "./tasks.css";

const statusText = { open: "待交付", submitted: "待验收", accepted: "已接纳" };
const emptyDraft = { title: "", description: "", acceptance: "" };
const errorText = (error: unknown) => error instanceof ApiError ? error.message : "请求未完成，请重试。";
const mergeTasks = (items: Task[]) => [...new Map(items.map(task => [task.id, task])).values()];

export function Tasks({ group }: { group: Group | null }) {
  const groupId = group?.id;
  const [visited, setVisited] = useState<string[]>([]);
  useEffect(() => {
    if (groupId) setVisited(previous => previous.includes(groupId) ? previous : [...previous, groupId]);
  }, [groupId]);
  return <section className="tasks-panel" aria-label="任务">
    {!groupId ? <p className="tasks-note">请先选择群组，再查看任务。</p> : null}
    {/* Keep unresolved operations and drafts scoped to their original group across switches. */}
    {visited.map(id => <div key={id} hidden={id !== groupId}>
      <TaskScope groupId={id} active={id === groupId} />
    </div>)}
  </section>;
}

function TaskScope({ groupId, active }: { groupId: string; active: boolean }) {
  const formId = useId();
  const [items, setItems] = useState<Task[]>([]);
  const [request, setRequest] = useState({ before: "", version: 0 });
  const [page, setPage] = useState({ pending: true, error: "", next: "" });
  const [selection, setSelection] = useState<{ id: string | null; version: number }>({ id: null, version: 0 });
  const [detail, setDetail] = useState<{ task: Task | null; pending: boolean; error: string }>({ task: null, pending: false, error: "" });
  const [draft, setDraft] = useState(emptyDraft);
  const [mutation, setMutation] = useState({ pending: false, uncertain: false, error: "" });
  const confirmed = useRef<{ task: Task; startedAfterRead: number; confirmedAfterRead: number }[]>([]);
  const readSequence = useRef(0);
  const successfulReads = useRef(new Map<string, { task: Task; read: number }>());
  const operation = useRef<{ key: string; input: TaskInput } | null>(null);
  const posting = useRef<AbortController | null>(null);
  const mounted = useRef(false);
  const load = (before = "") => setRequest(previous => ({ before, version: previous.version + 1 }));
  const select = (id: string | null = selection.id) => setSelection(previous => ({ id, version: previous.version + 1 }));
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; posting.current?.abort(); };
  }, []);
  useEffect(() => {
    if (!active) { if (request.before) load(); return; }
    let ignore = false;
    const controller = new AbortController();
    const read = ++readSequence.current;
    setPage(previous => ({ ...previous, pending: true, error: "" }));
    listTasks(groupId, request.before, controller.signal).then(result => {
      if (ignore) return;
      const updates = new Map(result.items.map(task => [task.id, task]));
      if (posting.current) {
        for (const task of updates.values()) successfulReads.current.set(task.id, { task, read });
      }
      // Concurrent reads can update same-ID content, but cannot disprove a new receipt's existence.
      confirmed.current = confirmed.current.filter(receipt => receipt.confirmedAfterRead >= read).map(receipt => {
        const update = updates.get(receipt.task.id);
        return update && read > receipt.startedAfterRead ? { ...receipt, task: update } : receipt;
      });
      const protectedTasks = confirmed.current.map(receipt => receipt.task);
      const protectedIds = new Set(protectedTasks.map(task => task.id));
      setItems(previous => [...protectedTasks, ...mergeTasks([...(request.before ? previous : []), ...result.items])
        .filter(task => !protectedIds.has(task.id))]);
      setPage({ pending: false, error: "", next: result.next_cursor });
    }, error => {
      if (!ignore) setPage(previous => ({ ...previous, pending: false, error: errorText(error) }));
    });
    return () => { ignore = true; controller.abort(); };
  }, [active, groupId, request]);
  useEffect(() => {
    if (!active || !selection.id) return;
    let ignore = false;
    const controller = new AbortController();
    setDetail({ task: null, pending: true, error: "" });
    getTask(groupId, selection.id, controller.signal).then(task => {
      if (!ignore) setDetail({ task, pending: false, error: "" });
    }, error => {
      if (!ignore) setDetail({ task: null, pending: false, error: errorText(error) });
    });
    return () => { ignore = true; controller.abort(); };
  }, [active, groupId, selection]);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (posting.current) return;
    if (!operation.current && (!draft.title.trim() || !draft.acceptance.trim())) {
      setMutation({ pending: false, uncertain: false, error: "请输入任务标题和验收条件。" }); return;
    }
    operation.current ??= { key: crypto.randomUUID(), input: Object.freeze({ title: draft.title.trim(),
      description: draft.description, acceptance: draft.acceptance.trim() }) };
    const current = operation.current;
    const controller = new AbortController(); posting.current = controller;
    const startedAfterRead = readSequence.current;
    setMutation({ pending: true, uncertain: false, error: "" });
    try {
      const task = await createTask(groupId, current.input, current.key, controller.signal);
      if (!mounted.current) return;
      // A same-ID read started after this POST outranks its receipt, regardless of response order.
      const latest = successfulReads.current.get(task.id);
      const freshTask = latest && latest.read > startedAfterRead ? latest.task : task;
      confirmed.current = [{ task: freshTask, startedAfterRead, confirmedAfterRead: readSequence.current },
        ...confirmed.current.filter(receipt => receipt.task.id !== task.id)];
      setItems(previous => [freshTask, ...previous.filter(item => item.id !== task.id)]); select(task.id);
      operation.current = null; setDraft(emptyDraft);
      setMutation({ pending: false, uncertain: false, error: "" });
    } catch (error) {
      if (!mounted.current) return;
      const uncertain = !isDefinitiveRejection(error);
      if (!uncertain) operation.current = null;
      setMutation({ pending: false, uncertain, error: uncertain ? `创建结果未确认。${errorText(error)} 请重试同一次创建。` : errorText(error) });
    } finally { posting.current = null; successfulReads.current.clear(); }
  }

  const selected = detail.task?.id === selection.id ? detail.task : null;
  return <section className="tasks-scope" aria-label="群组任务">
    <header className="tasks-toolbar"><h2>任务</h2><button type="button" onClick={() => { load(); select(); }}>刷新任务</button></header>
    <p className="tasks-note">“待交付”不代表已开始执行；“已接纳”表示成果被接纳，不等同于 CLI 运行成功。</p>
    <div className="tasks-layout">
      <div aria-busy={page.pending}>
        {page.pending ? <p role="status">正在载入任务…</p> : null}
        {page.error ? <div role="alert"><p>{page.error}</p><button type="button" onClick={() => load(request.before)}>重试载入任务</button></div> : null}
        {!page.pending && !page.error && !items.length ? <p className="tasks-note">尚无任务，可创建任务并约定验收条件。</p> : null}
        <ul className="tasks-list" aria-label="任务列表">{items.map(task => <li key={task.id}>
          <button type="button" aria-label={`${task.title}，${statusText[task.status]}`} aria-pressed={selection.id === task.id} onClick={() => select(task.id)}>
            <span>{task.title}</span><span className="tasks-status">{statusText[task.status]}</span>
          </button>
        </li>)}</ul>
        {page.next ? <button type="button" disabled={page.pending} onClick={() => load(page.next)}>加载更多任务</button> : null}
      </div>
      {selection.id ? <section className="tasks-detail" aria-label="任务详情" aria-busy={detail.pending}>
        <header className="tasks-toolbar"><h3>任务详情</h3><button type="button" onClick={() => select()}>刷新详情</button></header>
        {detail.pending ? <p role="status">正在载入任务详情…</p> : null}
        {detail.error ? <div role="alert"><p>{detail.error}</p><button type="button" onClick={() => select()}>重试载入详情</button></div> : null}
        {selected ? <>
          <h3 className="tasks-copy">{selected.title}</h3>
          <dl><dt>任务描述</dt><dd className="tasks-copy">{selected.description || "未填写描述"}</dd>
            <dt>验收条件</dt><dd className="tasks-copy">{selected.acceptance}</dd>
            <dt>任务状态</dt><dd>{statusText[selected.status]}</dd><dt>修订版本</dt><dd>{selected.revision}</dd>
            <dt>负责人</dt><dd>{selected.assignee_id || "未指定负责人"}</dd>
            <dt>依赖任务</dt><dd>{selected.dependency_ids.length ? <ul>{selected.dependency_ids.map((id, index) => <li key={`${index}-${id}`}>{id}</li>)}</ul> : "无依赖任务"}</dd>
            <dt>任务标识</dt><dd className="tasks-meta">{selected.id}</dd>
            <dt>关联尝试</dt><dd className="tasks-meta">{selected.active_attempt_id || "未关联尝试"}</dd>
            <dt>当前提交</dt><dd className="tasks-meta">{selected.current_submission_id || "尚无提交"}</dd>
            <dt>创建时间</dt><dd className="tasks-meta"><time dateTime={selected.created_at}>{selected.created_at}</time></dd>
          </dl>
        </> : null}
      </section> : <p className="tasks-note">选择任务以读取最新详情。</p>}
    </div>
    <form aria-label="创建任务" onSubmit={submit}>
      <fieldset disabled={mutation.pending || mutation.uncertain}>
        <legend>创建任务</legend>
        <label htmlFor={`${formId}-title`}>任务标题</label>
        <input id={`${formId}-title`} required value={draft.title} onChange={event => setDraft(previous => ({ ...previous, title: event.target.value }))} />
        <label htmlFor={`${formId}-description`}>任务描述（可选）</label>
        <textarea id={`${formId}-description`} value={draft.description} onChange={event => setDraft(previous => ({ ...previous, description: event.target.value }))} />
        <label htmlFor={`${formId}-acceptance`}>验收条件</label>
        <textarea id={`${formId}-acceptance`} required value={draft.acceptance} onChange={event => setDraft(previous => ({ ...previous, acceptance: event.target.value }))} />
      </fieldset>
      <p className="tasks-note">创建只记录任务与验收条件，不会启动执行。</p>
      {mutation.error ? <p role="alert">{mutation.error}</p> : null}
      <button type="submit" disabled={mutation.pending}>{mutation.pending ? "正在创建任务…" : mutation.uncertain ? "重试同一次任务创建" : "创建任务"}</button>
    </form>
  </section>;
}
