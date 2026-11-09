import { useEffect, useId, useImperativeHandle, useLayoutEffect, useRef, useState } from "react";
import type { FormEvent, Ref } from "react";
import { ApiError } from "./api";
import type { Member, Message } from "./api";
import { isRequestWorkDefinitiveRejection, requestWork } from "./coordination-api";
import type { AdmissionReceipt, RequestWorkInput } from "./coordination-api";
import { getTask } from "./task-api";
import type { Task } from "./task-api";

export type RequestWorkHandle = { open(source: Message, returnFocus?: HTMLElement): void };
type Source = Readonly<Pick<Message, "id" | "group_id" | "topic_id" | "sender_id" | "created_at" | "content">>;
type Draft = { title: string; description: string; acceptance: string; reason: string };
type Operation = Readonly<{ groupId: string; key: string; input: RequestWorkInput }>;
type State = {
  source: Source | null; draft: Draft; expanded: boolean; phase: "idle" | "pending" | "unknown" | "confirmed";
  operation: Operation | null; receipt: AdmissionReceipt | null; error: string; notice: string;
};
type TaskRead = { receipt: AdmissionReceipt; task: Task | null; pending: boolean; error: string };
const emptyDraft = (): Draft => ({ title: "", description: "", acceptance: "", reason: "" });
const initialState = (): State => ({ source: null, draft: emptyDraft(), expanded: false, phase: "idle",
  operation: null, receipt: null, error: "", notice: "" });
// Match Go strings.TrimSpace: NEL is whitespace, while BOM is not. Never normalize submitted values.
const goBlank = /^[\u0009-\u000d \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]*$/u;
const hasDraft = (draft: Draft) => Object.values(draft).some(value => value.length > 0);
const errorText = (error: unknown) => error instanceof ApiError ? error.message : "请求未完成，请重试。";
const statusText = { open: "待交付", submitted: "待验收", accepted: "已接纳" };
function visible(element: HTMLElement | null): element is HTMLElement {
  if (!element?.isConnected || element.closest("[hidden], [inert], [aria-hidden='true']") || element.matches(":disabled")) return false;
  for (let node: HTMLElement | null = element; node; node = node.parentElement) {
    const style = getComputedStyle(node);
    if (style.display === "none" || style.visibility === "hidden" || style.visibility === "collapse" || style.opacity === "0") return false;
    if (node instanceof HTMLDetailsElement && !node.open && !node.querySelector(":scope > summary")?.contains(element)) return false;
  }
  return true;
}
function preview(content: string) {
  let text = "", length = 0;
  for (const character of content) {
    if (length++ === 140) return `${text}…`;
    text += character;
  }
  return text;
}
function shortTime(value: string) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString("zh-CN", {
    month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false,
  });
}

// Mount one stable instance per visited group; hide via active, never recycle it for another groupId.
export function RequestWork({ groupId, active, members = [], ref }: {
  groupId: string; active: boolean; members?: readonly Member[]; ref?: Ref<RequestWorkHandle>;
}) {
  const formId = useId(), [state, setState] = useState(initialState);
  // Event handlers share the synchronous snapshot, including before React commits a second click.
  const current = useRef(state), mounted = useRef(false);
  const trigger = useRef<HTMLElement | null>(null), restoreButton = useRef<HTMLButtonElement>(null);
  const region = useRef<HTMLElement>(null), title = useRef<HTMLTextAreaElement>(null);
  const focusIntent = useRef<"edit" | "region" | "return" | null>(null);
  const [read, setRead] = useState<TaskRead | null>(null), [readVersion, setReadVersion] = useState(0);
  function update(patch: Partial<State>) {
    const next = { ...current.current, ...patch };
    current.current = next; setState(next);
  }
  function reveal() {
    if (!active || !current.current.source) return;
    focusIntent.current = current.current.phase === "idle" ? "edit" : "region";
    update({ expanded: true });
  }
  function open(source: Message, returnFocus?: HTMLElement) {
    if (!active || source.group_id !== groupId) return;
    const previous = current.current;
    const different = previous.source && (previous.source.id !== source.id || previous.source.topic_id !== source.topic_id);
    trigger.current = returnFocus ?? null;
    if (different && (previous.phase === "pending" || previous.phase === "unknown")) {
      update({ notice: "本群原请求尚未确认；请先确认原请求的结果，不能替换来源。" });
    } else if (different && !previous.receipt && hasDraft(previous.draft)) {
      update({ notice: "已有未提交草稿；请先明确放弃草稿，再选择另一条消息。" });
    } else if (!previous.source || different) {
      const snapshot = Object.freeze({ id: source.id, group_id: source.group_id, topic_id: source.topic_id,
        sender_id: source.sender_id, created_at: source.created_at, content: source.content });
      update({ ...initialState(), source: snapshot });
    }
    reveal();
  }
  useImperativeHandle(ref, () => ({ open }), [open]);
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);
  // Consume focus only for explicit actions, even if that render deactivates the group.
  useLayoutEffect(() => {
    const intent = focusIntent.current; focusIntent.current = null;
    if (!intent || !active) return;
    const target = intent === "return" ? (visible(trigger.current) ? trigger.current : restoreButton.current)
      : state.expanded ? (intent === "edit" ? title.current : region.current) : null;
    if (visible(target)) target.focus();
  });
  useEffect(() => {
    if (!active || !state.expanded || !state.receipt) return;
    const receipt = state.receipt, controller = new AbortController();
    let ignore = false;
    setRead({ receipt, task: null, pending: true, error: "" });
    getTask(groupId, receipt.task.id, controller.signal).then(task => {
      if (!ignore) setRead({ receipt, task, pending: false, error: "" });
    }, error => {
      if (!ignore) setRead({ receipt, task: null, pending: false, error: errorText(error) });
    });
    return () => { ignore = true; controller.abort(); };
  }, [active, groupId, state.expanded, state.receipt, readVersion]);

  function change(field: keyof Draft, value: string) {
    if (!active || !current.current.expanded || current.current.phase !== "idle") return;
    update({ draft: { ...current.current.draft, [field]: value } });
  }
  function discard() {
    if (!active || current.current.phase !== "idle") return;
    focusIntent.current = "edit";
    update({ draft: emptyDraft(), error: "", notice: "草稿已放弃，可选择另一条消息。" });
  }
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const previous = current.current;
    if (!active || !previous.expanded || !previous.source || previous.phase === "pending" || previous.receipt) return;
    let operation = previous.operation;
    if (!operation) {
      const draft = previous.draft;
      if ([draft.title, draft.acceptance, draft.reason].some(value => goBlank.test(value))) {
        update({ error: "请输入非空的任务标题、验收条件和授权理由。" }); return;
      }
      try {
        const key = crypto.randomUUID();
        if (!key) throw new Error("Empty idempotency key");
        operation = Object.freeze({ groupId: previous.source.group_id, key,
          input: Object.freeze({ topic_id: previous.source.topic_id, message_id: previous.source.id, mode: "create_new",
            reason: draft.reason, new_task: Object.freeze({ title: draft.title, description: draft.description, acceptance: draft.acceptance }) }) });
      } catch {
        update({ error: "无法生成请求标识，尚未提交。请保留草稿后重试。" }); return;
      }
    }
    update({ operation, phase: "pending", error: "", notice: "" });
    try {
      // Hiding or deactivating this scope must never cancel a possibly committed write.
      const receipt = await requestWork(operation.groupId, operation.input, operation.key);
      if (mounted.current) update({ receipt, operation: null, phase: "confirmed", error: "", notice: "" });
    } catch (error) {
      if (!mounted.current) return;
      const unknown = previous.phase === "unknown" || !isRequestWorkDefinitiveRejection(error);
      update({ phase: unknown ? "unknown" : "idle", operation: unknown ? operation : null,
        error: unknown ? `创建结果未确认。${errorText(error)} 请重试同一次创建。` : errorText(error) });
    }
  }

  const locked = state.phase === "pending" || state.phase === "unknown";
  const warning = !state.receipt && (hasDraft(state.draft) || locked);
  const detail = read?.receipt === state.receipt ? read : null;
  const restoreLabel = state.receipt ? "查看授权回执" : state.phase === "pending" ? "查看任务创建进度"
    : state.phase === "unknown" ? "恢复未确认的任务创建" : "继续任务草稿";
  return <div hidden={!active}>
    {state.source ? <>
      <button ref={restoreButton} type="button" hidden={state.expanded} aria-expanded={false} aria-controls={formId} onClick={reveal}>{restoreLabel}</button>
      {warning ? <p className="feed-note">草稿与未确认请求（含幂等键）仅保留在本页，重载会丢失。</p> : null}
      <div hidden={!state.expanded}>
        <section id={formId} ref={region} className="creation-disclosure" aria-label="从消息创建任务" tabIndex={-1}>
          <div className="discussion-tools"><strong>从消息创建任务</strong>
            <button type="button" onClick={() => { focusIntent.current = "return"; update({ expanded: false }); }}>收起</button></div>
          <SourceEvidence source={state.source} members={members} />
          {state.notice ? <p role="status">{state.notice}</p> : null}
          {!state.receipt ? <form aria-label="从消息创建任务" noValidate onSubmit={submit}>
            <fieldset disabled={locked}>
              <label htmlFor={`${formId}-title`}>任务标题</label>
              <textarea ref={title} id={`${formId}-title`} required rows={1} value={state.draft.title} onChange={event => change("title", event.target.value)} />
              <label htmlFor={`${formId}-description`}>任务描述（可选）</label>
              <textarea id={`${formId}-description`} rows={2} value={state.draft.description} onChange={event => change("description", event.target.value)} />
              <label htmlFor={`${formId}-acceptance`}>验收条件</label>
              <textarea id={`${formId}-acceptance`} required rows={2} value={state.draft.acceptance} onChange={event => change("acceptance", event.target.value)} />
              <label htmlFor={`${formId}-reason`}>授权理由</label>
              <textarea id={`${formId}-reason`} required rows={2} value={state.draft.reason} onChange={event => change("reason", event.target.value)} />
            </fieldset>
            <p className="feed-note">将记录这次明确授权及验收条件；不会自动启动执行。</p>
            <div className="discussion-tools">
              <button className="primary-send" type="submit" disabled={state.phase === "pending"}>
                {state.phase === "pending" ? "正在创建任务…" : state.phase === "unknown" ? "重试同一次创建" : "确认并创建任务"}</button>
              {!locked && hasDraft(state.draft) ? <button type="button" onClick={discard}>放弃草稿</button> : null}
            </div>
            {state.error ? <p role="alert">{state.error}</p> : null}
          </form> : <>
            <AuthorizationReceipt receipt={state.receipt} />
            <section aria-label="当前任务" aria-busy={detail?.pending ?? true}>
              <div className="discussion-tools"><strong>当前任务</strong>
                <button type="button" onClick={() => setReadVersion(version => version + 1)}>刷新当前任务</button></div>
              <p className="feed-note">当前状态独立于原授权回执；待交付不代表已开始执行。</p>
              {!detail || detail.pending ? <p role="status">正在载入当前任务…</p> : null}
              {detail?.error ? <><p role="alert">授权已登记，当前任务暂不可用。{detail.error}</p>
                <button type="button" onClick={() => setReadVersion(version => version + 1)}>重试载入当前任务</button></> : null}
              {detail?.task ? <dl><dt>当前标题</dt><dd className="message-content">{detail.task.title}</dd>
                <dt>当前状态</dt><dd>{statusText[detail.task.status]}</dd><dt>当前修订</dt><dd>{detail.task.revision}</dd></dl> : null}
            </section>
          </>}
        </section>
      </div>
    </> : null}
  </div>;
}

function SourceEvidence({ source, members }: { source: Source; members: readonly Member[] }) {
  const author = source.sender_id === "user" ? "我"
    : members.find(member => member.group_id === source.group_id && member.id === source.sender_id)?.name ?? "未载入成员";
  return <section aria-label="来源证据">
    <p className="feed-note">来源作者：<strong>{author}</strong> · <time dateTime={source.created_at}>{shortTime(source.created_at)}</time></p>
    <p className="feed-note">{source.topic_id === "" ? "主话题" : <>话题：<span className="message-content">{source.topic_id}</span></>}</p>
    <p className="feed-note">来源预览</p><p className="message-content">{preview(source.content)}</p>
    <details className="message-details"><summary>完整来源与标识</summary>
      <p className="message-content">{source.content}</p>
      <dl className="message-technical"><div><dt>来源消息 ID</dt><dd>{source.id}</dd></div>
        <div><dt>来源群组 ID</dt><dd>{source.group_id}</dd></div>
        <div><dt>来源话题 ID</dt><dd className="message-content">{source.topic_id === "" ? "空串（主话题）" : source.topic_id}</dd></div>
        <div><dt>发送者 ID</dt><dd>{source.sender_id}</dd></div>
        <div><dt>完整时间</dt><dd><time dateTime={source.created_at}>{source.created_at}</time></dd></div></dl>
    </details>
  </section>;
}
function AuthorizationReceipt({ receipt: { decision, mandate, task } }: { receipt: AdmissionReceipt }) {
  return <section aria-label="授权回执">
    <p role="status">授权已登记，任务已创建；尚未自动启动执行。</p>
    <dl><dt>授权目标</dt><dd className="message-content">{mandate.scope.objective}</dd>
      <dt>授权描述</dt><dd className="message-content">{mandate.scope.description || "未填写描述"}</dd>
      <dt>授权验收条件</dt><dd className="message-content">{mandate.scope.acceptance}</dd>
      <dt>授权理由</dt><dd className="message-content">{decision.reason}</dd></dl>
    <p className="feed-note">来源证据为上方保存的真实消息快照；以下为原始授权记录，不代表当前任务状态。</p>
    <details className="message-details"><summary>授权技术详情</summary>
      <dl className="message-technical"><div><dt>Decision ID</dt><dd>{decision.id}</dd></div>
        <div><dt>Mandate ID</dt><dd>{mandate.id}</dd></div>
        <div><dt>来源 Trigger ID</dt><dd>{mandate.origin_trigger_id}</dd></div>
        <div><dt>Task ID</dt><dd>{task.id}</dd></div>
        <div><dt>授权时修订</dt><dd>{task.revision_at_admission}</dd></div>
        <div><dt>授权登记时间</dt><dd><time dateTime={decision.created_at}>{decision.created_at}</time></dd></div>
        <div><dt>授权群组 ID</dt><dd>{decision.group_id}</dd></div>
        <div><dt>授权话题 ID</dt><dd className="message-content">{decision.topic_id === "" ? "空串（主话题）" : decision.topic_id}</dd></div>
        <div><dt>授权者</dt><dd>{mandate.authorized_by}</dd></div>
        <div><dt>授权策略</dt><dd>{decision.policy_revision}</dd></div></dl>
    </details>
  </section>;
}
