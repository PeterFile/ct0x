import { useEffect, useId, useRef, useState } from "react";
import type { FormEvent } from "react";
import { ApiError, isDefinitiveRejection } from "./api";
import type { Group } from "./api";
import { createTask, getTask, getTaskAttempt, getTaskSubmission, listTasks, registerTaskAttempt, reviewTask, submitTask } from "./task-api";
import type { Task, TaskAttempt, TaskAttemptInput, TaskInput, TaskReview, TaskReviewInput, TaskSubmission, TaskSubmissionInput } from "./task-api";
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
  const [openedTasks, setOpenedTasks] = useState<string[]>([]);
  const visible = useRef({ active, taskId: selection.id });
  visible.current = { active, taskId: selection.id };
  useEffect(() => {
    const id = selection.id;
    if (id) setOpenedTasks(previous => previous.includes(id) ? previous : [...previous, id]);
  }, [selection.id]);
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
    if (!active || !selection.id) { setDetail({ task: null, pending: false, error: "" }); return; }
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
    const wasUncertain = mutation.uncertain;
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
      const uncertain = wasUncertain || !isDefinitiveRejection(error);
      if (!uncertain) operation.current = null;
      setMutation({ pending: false, uncertain, error: uncertain ? `创建结果未确认。${errorText(error)} 请重试同一次创建。` : errorText(error) });
    } finally { posting.current = null; successfulReads.current.clear(); }
  }

  const refreshAfterReceipt = (taskId: string) => {
    if (visible.current.active && visible.current.taskId === taskId) { load(); select(taskId); }
  };
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
        {/* Retain each task's drafts and immutable operation without reading hidden tasks. */}
        {openedTasks.map(id => <div key={id} hidden={id !== selection.id}>
          <TaskLifecycle groupId={groupId} taskId={id} task={active && selected?.id === id ? selected : null} onReceipt={refreshAfterReceipt} />
        </div>)}
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

type LifecycleOperation = { key: string } & (
  { kind: "attempt"; input: TaskAttemptInput } | { kind: "submission"; input: TaskSubmissionInput }
  | { kind: "review"; input: TaskReviewInput; evidence: Readonly<TaskSubmission> }
);
type LifecycleReceipt = TaskAttempt | TaskSubmission | TaskReview;

function TaskLifecycle({ groupId, taskId, task, onReceipt }: {
  groupId: string; taskId: string; task: Task | null; onReceipt: (taskId: string) => void;
}) {
  const formId = useId();
  const [summary, setSummary] = useState("");
  const [reason, setReason] = useState("");
  const [content, setContent] = useState<{
    task: Task | null; attempt: TaskAttempt | null; submission: TaskSubmission | null; pending: boolean; error: string;
  }>({ task: null, attempt: null, submission: null, pending: false, error: "" });
  const [mutation, setMutation] = useState<{ pending: boolean; uncertain: boolean; error: string; receipt: LifecycleReceipt | null }>(
    { pending: false, uncertain: false, error: "", receipt: null });
  const operation = useRef<LifecycleOperation | null>(null);
  const posting = useRef(false);
  const mounted = useRef(false);
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);
  useEffect(() => {
    let ignore = false;
    const controller = new AbortController();
    const empty = { task, attempt: null, submission: null, pending: false, error: "" };
    setContent(empty);
    if (!task) return;
    const load = async () => {
      if (task.status === "open" && task.active_attempt_id) {
        const attempt = await getTaskAttempt(groupId, taskId, task.active_attempt_id, controller.signal);
        if (ignore) return;
        if (attempt.task_revision !== task.revision) throw new ApiError("当前尝试与任务详情不匹配，请刷新详情。");
        setContent({ ...empty, attempt });
      } else if (task.current_submission_id) {
        const submission = await getTaskSubmission(groupId, taskId, task.current_submission_id, controller.signal);
        if (ignore) return;
        if (submission.task_revision !== task.revision || (task.status === "submitted" && submission.attempt_id !== task.active_attempt_id)) {
          throw new ApiError("当前交付与任务详情不匹配，请刷新详情。");
        }
        setContent({ ...empty, submission });
      }
    };
    if (task.active_attempt_id || task.current_submission_id) setContent({ ...empty, pending: true });
    void load().catch(error => { if (!ignore) setContent({ ...empty, error: errorText(error) }); });
    return () => { ignore = true; controller.abort(); };
  }, [groupId, taskId, task]);

  // Each authoritative Task object is a read epoch, also fencing the render before effects run.
  const loaded = content.task === task ? content : null;
  const attempt = loaded?.attempt, submission = loaded?.submission;
  const blocked = mutation.pending || mutation.uncertain;
  async function mutate(kind?: LifecycleOperation["kind"], outcome?: TaskReview["outcome"]) {
    if (posting.current || (operation.current && kind)) return;
    if (!operation.current) {
      if (!task) return;
      if (kind === "attempt" && task.status === "open" && !task.active_attempt_id) {
        operation.current = { key: crypto.randomUUID(), kind, input: Object.freeze({ expected_revision: task.revision, expected_active_attempt_id: "" }) };
      } else if (kind === "submission" && task.status === "open" && attempt?.turn_id === "") {
        if (!/\P{White_Space}/u.test(summary)) { setMutation({ pending: false, uncertain: false, error: "请输入交付摘要。", receipt: null }); return; }
        operation.current = { key: crypto.randomUUID(), kind, input: Object.freeze({ expected_revision: task.revision, attempt_id: attempt.id, summary }) };
      } else if (kind === "review" && task.status === "submitted" && submission && outcome) {
        const evidence = { ...submission, artifact_versions: submission.artifact_versions.map(ref => Object.freeze({ ...ref })) };
        Object.freeze(evidence.artifact_versions);
        operation.current = { key: crypto.randomUUID(), kind, evidence: Object.freeze(evidence),
          input: Object.freeze({ expected_revision: task.revision, submission_id: submission.id, outcome, reason }) };
      } else return;
      Object.freeze(operation.current);
    }
    const current = operation.current;
    // A retry rejection cannot disprove an earlier write with an unknown outcome.
    const wasUncertain = mutation.uncertain;
    posting.current = true;
    setMutation({ pending: true, uncertain: false, error: "", receipt: null });
    try {
      const receipt = current.kind === "attempt" ? await registerTaskAttempt(groupId, taskId, current.input, current.key)
        : current.kind === "submission" ? await submitTask(groupId, taskId, current.input, current.key)
          : await reviewTask(groupId, taskId, current.input, current.evidence, current.key);
      if (!mounted.current) return;
      operation.current = null;
      if (current.kind === "submission") setSummary("");
      if (current.kind === "review") setReason("");
      setMutation({ pending: false, uncertain: false, error: "", receipt });
      // A receipt is immutable evidence, never the current Task or a navigation instruction.
      onReceipt(taskId);
    } catch (error) {
      if (!mounted.current) return;
      const uncertain = wasUncertain || !isDefinitiveRejection(error);
      if (!uncertain) operation.current = null;
      setMutation({ pending: false, uncertain, receipt: null, error: uncertain
        ? `操作结果未确认。${errorText(error)} 请重试同一次操作。` : errorText(error) });
    } finally { posting.current = false; }
  }

  const review = mutation.receipt && "outcome" in mutation.receipt ? mutation.receipt : null;
  return <div className="tasks-lifecycle">
    {mutation.pending ? <p role="status">正在保存操作，请等待回执…</p> : null}
    {mutation.receipt ? <p role="status" className="tasks-meta">已确认操作回执：{mutation.receipt.id}。</p> : null}
    {review ? <section className="tasks-delivery" aria-label="审查回执">
      <h3>实际审查决定</h3>
      <dl><dt>决定</dt><dd>{review.outcome === "accepted" ? "接纳交付" : "退回修改"}</dd>
        <dt>说明</dt><dd className="tasks-copy">{review.reason || "未填写说明"}</dd>
        <dt>审查标识</dt><dd className="tasks-meta">{review.id}</dd></dl>
      <p className="tasks-note">此回执记录原始决定；当前任务状态以最新详情为准。</p>
    </section> : null}
    {mutation.error ? <p role="alert">{mutation.error}</p> : null}
    {mutation.uncertain ? <button type="button" onClick={() => void mutate()}>重试同一次操作</button> : null}
    {loaded?.error ? <p role={task?.status === "accepted" ? "status" : "alert"}>{loaded.error}</p> : null}
    {task?.status === "open" ? <>
      {!task.active_attempt_id ? <>
        <button type="button" disabled={blocked} onClick={() => void mutate("attempt")}>登记人工尝试</button>
        <p className="tasks-note">登记只记录人工尝试，不会启动成员执行。</p>
      </> : !loaded || loaded.pending ? <p role="status">正在载入当前尝试…</p>
        : attempt?.turn_id ? <>
          <p className="tasks-note">该尝试属于成员，不能人工替换或提交交付。</p><p className="tasks-meta">{attempt.turn_id}</p>
        </> : attempt ? <form aria-label="提交交付" onSubmit={event => { event.preventDefault(); void mutate("submission"); }}>
          <fieldset disabled={blocked}>
            <legend>提交交付</legend><p className="tasks-note">仅文本成果，不附加文件或成果版本</p>
            <label htmlFor={`${formId}-summary`}>交付摘要</label>
            <textarea id={`${formId}-summary`} required value={summary} onChange={event => setSummary(event.target.value)} />
            <button type="submit">提交交付</button>
          </fieldset>
        </form> : null}
    </> : null}
    {task && task.status !== "open" ? <>
      <section className="tasks-delivery" aria-label="当前交付" aria-busy={!loaded || loaded.pending}>
        <h3>当前交付</h3>
        {!loaded || loaded.pending ? <p role="status">正在载入当前交付…</p> : null}
        {submission ? <>
          <p className="tasks-copy">{submission.summary}</p><p className="tasks-meta">提交者：{submission.submitted_by}</p>
          {submission.artifact_versions.length ? <ul className="tasks-artifact-refs" aria-label="冻结成果版本">
            {submission.artifact_versions.map(ref => <li key={JSON.stringify([ref.artifact_id, ref.version_id])} className="tasks-meta">{ref.artifact_id} / {ref.version_id}</li>)}
          </ul> : <p className="tasks-note">无冻结成果版本，仅文本交付。</p>}
        </> : null}
      </section>
      {task.status === "submitted" ? <fieldset disabled={blocked || !submission}>
        <legend>人工验收</legend><label htmlFor={`${formId}-reason`}>验收说明（可选）</label>
        <textarea id={`${formId}-reason`} value={reason} onChange={event => setReason(event.target.value)} />
        <div className="tasks-actions">
          <button type="button" onClick={() => void mutate("review", "accepted")}>接纳交付</button>
          <button type="button" onClick={() => void mutate("review", "rejected")}>退回修改</button>
        </div>
      </fieldset> : null}
    </> : null}
  </div>;
}
