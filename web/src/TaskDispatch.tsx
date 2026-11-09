import { useEffect, useId, useRef, useState } from "react";
import { ApiError } from "./api";
import type { Task } from "./task-api";
import { getDispatchRequest, getDispatchTask, getTaskCoordination, isDispatchRequestDefinitiveRejection, requestDispatch } from "./dispatch-request-api";
import type { DispatchRequest, DispatchRequestInput, DispatchState, DispatchView, TaskCoordinationView } from "./dispatch-request-api";

type Operation = Readonly<{
  groupId: string; taskId: string; url: string; key: string; body: string; input: DispatchRequestInput; topicId: string; mandateId: string;
}>;
type Mutation = {
  operation: Operation | null; pending: boolean; everUnknown: boolean; needsRefresh: boolean; error: string; receipt: DispatchRequest | null;
};
type Current = { task: Task | null; authority: TaskCoordinationView | null; status: "idle" | "pending" | "ready" | "unavailable"; error: string };
const errorText = (error: unknown) => error instanceof ApiError ? error.message : "未收到可信响应，请重试。";
const versions = (revision: number, generation: number) => `任务版本 ${revision} / 授权代次 ${generation}`;
const statusText: Record<DispatchState["status"], string> = {
  pending: "已登记，等待协调", enqueued: "已进入协调员投递队列", held: "协调请求暂停",
  assigned: "已记录分配，具体执行尚未由本页核实", declined: "协调员已拒绝本次分派，不能自动重派",
};
const heldText: Readonly<Record<string, string>> = {
  task_stale: "任务已变化", authority_stale: "授权已变化", binding_stale: "协调员绑定已变化", policy_stale: "分派策略已变化",
  coordinator_inactive: "协调员已不在群内", no_eligible_worker: "没有符合授权的执行成员",
  payload_too_large: "协调输入过大", payload_not_encodable: "协调输入无法编码",
};
const sameRequest = (left: DispatchRequest, right: DispatchRequest) =>
  (Object.keys(left) as (keyof DispatchRequest)[]).every(field => left[field] === right[field]);
function stateSignature(state: DispatchState) {
  const d = state.delivery;
  return JSON.stringify([state.request_id, state.generation, state.status, state.attempt_count, state.next_attempt_at, state.last_error_code,
    state.held_reason, d && [d.message_id, d.delivery_id, d.conversation_id, d.coordinator_member_id, d.materialized_from_generation, d.created_at]]);
}
function followsState(old: DispatchState, incoming: DispatchState) {
  if (incoming.generation < old.generation) return false;
  if (incoming.generation === old.generation) return stateSignature(old) === stateSignature(incoming);
  // Reads may skip retries, materialization and a decision; held/decided states cannot reopen.
  if (old.status === "pending") return incoming.attempt_count >= old.attempt_count
    && (incoming.attempt_count > old.attempt_count || incoming.next_attempt_at === old.next_attempt_at);
  // The only update after enqueued is one decision, preserving every receipt/retry field.
  return old.status === "enqueued" && (incoming.status === "assigned" || incoming.status === "declined")
    && incoming.generation - old.generation === 1
    && stateSignature(old) === stateSignature({ ...incoming, generation: old.generation, status: old.status });
}
function eligible(current: Current) {
  const t = current.task, m = current.authority?.admission.mandate;
  if (current.status !== "ready" || !t || !m) return false;
  const scope = m.scope;
  return t.status === "open" && !t.active_attempt_id && !t.current_submission_id && t.revision === m.task_revision_at_admission
    && t.title === scope.objective && t.description === scope.description && t.acceptance === scope.acceptance && t.assignee_id === scope.assignee_id
    && t.dependency_ids.length === scope.dependency_ids.length && new Set(t.dependency_ids).size === t.dependency_ids.length
    && t.dependency_ids.every(id => scope.dependency_ids.includes(id));
}

// The parent retains one instance per visited group and opened task, never per Task revision.
export function TaskDispatch({ groupId, taskId, active }: { groupId: string; taskId: string; active: boolean }) {
  const formId = useId();
  const [expanded, setExpanded] = useState(false);
  const [refresh, setRefresh] = useState(0);
  const [reason, setReason] = useState("");
  const [current, setCurrent] = useState<Current>({ task: null, authority: null, status: "idle", error: "" });
  const [mutation, setMutation] = useState<Mutation>({ operation: null, pending: false, everUnknown: false, needsRefresh: false, error: "", receipt: null });
  const mutationRef = useRef(mutation);
  const currentRef = useRef(current);
  const mounted = useRef(false);
  const readRound = useRef(0);
  const recoveryRound = useRef(0);
  const [knownId, setKnownId] = useState("");
  const [lookup, setLookup] = useState({ id: "", version: 0 });
  const [reading, setReading] = useState<{ pending: boolean; error: string; view: DispatchView | null }>({ pending: false, error: "", view: null });
  const observed = useRef(new Map<string, DispatchView>());
  const enabled = active && expanded;
  const visible = useRef(enabled);
  visible.current = enabled;
  function updateMutation(change: (previous: Mutation) => Mutation) {
    const next = change(mutationRef.current); mutationRef.current = next; setMutation(next);
  }
  function updateCurrent(value: Current) { currentRef.current = value; setCurrent(value); }
  useEffect(() => {
    mounted.current = true;
    // A view disappearing cannot undo or cancel a possibly committed POST.
    return () => { mounted.current = false; };
  }, []);
  useEffect(() => {
    if (!enabled) return;
    let ignore = false;
    const controller = new AbortController(), round = ++readRound.current;
    updateCurrent({ task: null, authority: null, status: "pending", error: "" });
    // These reads are not atomic; Store performs the final same-transaction check.
    void Promise.all([getDispatchTask(groupId, taskId, controller.signal), getTaskCoordination(groupId, taskId, controller.signal)]).then(([task, authority]) => {
      if (ignore) return;
      updateCurrent({ task, authority, status: "ready", error: "" });
      if (recoveryRound.current === round) {
        recoveryRound.current = 0;
        updateMutation(previous => ({ ...previous, needsRefresh: false }));
      }
    }, error => {
      if (!ignore) updateCurrent({ task: null, authority: null, status: "unavailable", error: errorText(error) });
    });
    return () => { ignore = true; controller.abort(); };
  }, [enabled, groupId, taskId, refresh]);
  useEffect(() => {
    if (!enabled || !lookup.id) return;
    let ignore = false;
    const controller = new AbortController(), id = lookup.id;
    setReading({ pending: true, error: "", view: null });
    void getDispatchRequest(groupId, taskId, id, controller.signal).then(incoming => {
      if (ignore) return;
      const old = observed.current.get(id), receipt = mutationRef.current.receipt;
      if ((receipt?.id === id && !sameRequest(receipt, incoming.request)) || (old && (!sameRequest(old.request, incoming.request)
        || !followsState(old.state, incoming.state)))) {
        setReading({ pending: false, error: "当前状态未确认：读取已过期、状态迁移无效或不可变历史冲突，请刷新请求状态。", view: null });
        return;
      }
      observed.current.set(id, incoming);
      setReading({ pending: false, error: "", view: incoming });
    }, error => {
      if (!ignore) setReading({ pending: false, error: `当前状态未确认。${errorText(error)}`, view: null });
    });
    return () => { ignore = true; controller.abort(); };
  }, [enabled, groupId, taskId, lookup]);

  function refreshExplicitly() {
    if (mutationRef.current.needsRefresh) recoveryRound.current = readRound.current + 1;
    updateCurrent({ task: null, authority: null, status: "pending", error: "" });
    setRefresh(previous => previous + 1);
  }
  async function mutate(replay = false) {
    const previous = mutationRef.current;
    if (!visible.current || previous.pending) return;
    let operation = previous.operation;
    if (replay) { if (!operation || !previous.everUnknown) return; }
    else {
      if (operation || previous.receipt || previous.needsRefresh || observed.current.size || !eligible(currentRef.current)) return;
      if (!/\P{White_Space}/u.test(reason)) {
        updateMutation(value => ({ ...value, error: "请输入分派理由。" })); return;
      }
      const task = currentRef.current.task!, coordination = currentRef.current.authority!.coordination;
      const input = Object.freeze({ expected_task_revision: task.revision, expected_authority_generation: coordination.authority_generation, reason });
      operation = Object.freeze({ groupId, taskId, key: crypto.randomUUID(), input, body: JSON.stringify(input),
        url: `/api/v1/groups/${encodeURIComponent(groupId)}/tasks/${encodeURIComponent(taskId)}/dispatch-requests`,
        topicId: coordination.topic_id, mandateId: coordination.current_mandate_id });
    }
    const original = operation!;
    // Synchronous ownership fences double clicks before React commits the disabled controls.
    updateMutation(value => ({ ...value, operation: original, pending: true, error: "" }));
    try {
      const receipt = await requestDispatch(original.groupId, original.taskId, original.input, original.key);
      if (receipt.topic_id !== original.topicId || receipt.mandate_id !== original.mandateId) {
        throw new ApiError("分派回执与原授权不匹配，登记结果未确认。", 201, "invalid_response");
      }
      if (!mounted.current) return;
      const known = observed.current.get(receipt.id);
      if (known && !sameRequest(known.request, receipt)) {
        throw new ApiError("分派回执与已读取的不可变历史冲突，登记结果未确认。", 201, "invalid_response");
      }
      updateMutation(value => ({ ...value, operation: null, pending: false, everUnknown: false, needsRefresh: false, error: "", receipt }));
      setKnownId(value => value || receipt.id);
      setLookup(value => value.id ? value : { id: receipt.id, version: value.version + 1 });
    } catch (error) {
      if (!mounted.current) return;
      const uncertain = previous.everUnknown || !isDispatchRequestDefinitiveRejection(error);
      updateMutation(value => ({ ...value, operation: uncertain ? original : null, pending: false, everUnknown: uncertain,
        needsRefresh: !uncertain, error: uncertain ? `分派登记结果未确认。${errorText(error)} 请仅重试原分派登记。`
          : `${errorText(error)} 请显式刷新任务与授权，重新核对后再决定是否发起新登记。` }));
    }
  }
  const ready = enabled && eligible(current);
  const blocked = !ready || mutation.pending || mutation.everUnknown || mutation.needsRefresh || !!mutation.receipt || observed.current.size > 0;
  const original = mutation.operation;
  const receipt = mutation.receipt;
  const view = reading.view;
  return <section className="tasks-lifecycle" aria-label="请求分派">
    <header className="tasks-toolbar"><h3>请求分派</h3>
      <button type="button" aria-expanded={expanded} aria-controls={`${formId}-body`} onClick={() => setExpanded(value => !value)}>
        {expanded ? "收起请求分派" : "展开请求分派"}</button>
    </header>
    <div id={`${formId}-body`} hidden={!expanded}>
      <div className="tasks-lifecycle">
        <p className="tasks-note">仅登记分派意图，不表示成员已开始执行，也不改变任务交付或验收状态。</p>
        <p className="tasks-note">刷新浏览器或完全离开本页会丢失本页幂等键和已知请求标识，不等于未提交或已取消。本页不能按任务发现旧请求；未知结果不要换键试探。</p>
        <section className="tasks-delivery" aria-label="当前任务与授权" aria-busy={current.status === "pending"}>
          <h3>当前任务与授权</h3>
          {current.status === "pending" ? <p role="status">正在读取当前任务与授权…</p> : null}
          {current.error ? <p role="alert">{current.error}</p> : null}
          {current.status === "ready" && current.task && current.authority ? <>
            <p>{versions(current.task.revision, current.authority.coordination.authority_generation)}</p>
            <dl><dt>授权目标</dt><dd className="tasks-copy">{current.authority.admission.mandate.scope.objective}</dd>
              <dt>授权验收条件</dt><dd className="tasks-copy">{current.authority.admission.mandate.scope.acceptance}</dd>
              <dt>授权标识</dt><dd className="tasks-meta">{current.authority.coordination.current_mandate_id}</dd></dl>
          </> : null}
          {!ready ? <p className="tasks-note">当前任务授权尚未确认，或任务状态、授权版本不匹配，不能发起分派。</p> : null}
          <button type="button" disabled={!enabled} onClick={refreshExplicitly}>刷新任务与授权</button>
        </section>
        <form aria-label="发起分派" onSubmit={event => { event.preventDefault(); void mutate(); }}>
          <fieldset disabled={!enabled || mutation.pending || mutation.everUnknown || !!receipt}>
            <legend>显式发起分派</legend><label htmlFor={`${formId}-reason`}>分派理由</label>
            <textarea id={`${formId}-reason`} required value={reason} onChange={event => setReason(event.target.value)} />
          </fieldset>
          <p className="tasks-note">任务与授权为分开读取；最终由服务端核对当前版本。配置保存、消息或授权登记都不会自动发起分派。</p>
          <button type="submit" disabled={blocked}>确认发起分派</button>
        </form>
        {mutation.pending ? <p role="status">正在等待原分派登记回执…</p> : null}
        {mutation.error ? <p role="alert">{mutation.error}</p> : null}
        {original ? <section className="tasks-delivery" aria-label="原分派操作">
          <h3>原分派操作</h3><p>原操作：{versions(original.input.expected_task_revision, original.input.expected_authority_generation)}</p>
          <dl><dt>原群组 / 任务</dt><dd className="tasks-meta">{original.groupId} / {original.taskId}</dd>
            <dt>原幂等键</dt><dd className="tasks-meta">{original.key}</dd><dt>原理由</dt><dd className="tasks-copy">{original.input.reason}</dd></dl>
          {mutation.everUnknown ? <button type="button" disabled={!enabled || mutation.pending} onClick={() => void mutate(true)}>重试原分派登记</button> : null}
        </section> : null}
        {receipt ? <section className="tasks-delivery" aria-label="分派登记历史回执">
          <h3>请求已登记（历史回执）</h3><p>{versions(receipt.task_revision, receipt.authority_generation)}</p>
          <dl><dt>请求标识</dt><dd className="tasks-meta">{receipt.id}</dd><dt>原登记理由</dt><dd className="tasks-copy">{receipt.reason}</dd>
            <dt>捕获的授权</dt><dd className="tasks-meta">{receipt.mandate_id}</dd>
            <dt>捕获的协调员</dt><dd className="tasks-meta">{receipt.coordinator_member_id}</dd>
            <dt>绑定 / 策略版本</dt><dd>{receipt.binding_revision} / {receipt.policy_revision}</dd>
            <dt>登记时间</dt><dd className="tasks-meta"><time dateTime={receipt.created_at}>{receipt.created_at}</time></dd></dl>
          <p className="tasks-note">此回执属于原操作，不是当前任务版本、当前配置或具体执行的证明。</p>
        </section> : null}
        <form aria-label="读取已知请求" onSubmit={event => { event.preventDefault(); setLookup(value => ({ id: knownId, version: value.version + 1 })); }}>
          <label htmlFor={`${formId}-known`}>已知分派请求标识</label>
          <input id={`${formId}-known`} value={knownId} onChange={event => setKnownId(event.target.value)} />
          <button type="submit" disabled={!enabled || !knownId}>读取已知请求状态</button>
          <p className="tasks-note">只读取当前群组、当前任务下的精确已知标识；读取成功也不能确认结果未知的原操作。</p>
        </form>
        <section className="tasks-delivery" aria-label="请求当前状态" aria-busy={reading.pending}>
          <h3>请求当前状态</h3>
          {lookup.id ? <p className="tasks-meta">查询标识：{lookup.id}</p> : <p className="tasks-note">尚未读取请求状态；这不表示任务没有已登记的分派。</p>}
          {reading.pending ? <p role="status">正在读取请求状态…</p> : null}
          {reading.error ? <p role="alert">{reading.error}</p> : null}
          {view ? <>
            <p role="status">{statusText[view.state.status]}</p>
            <dl><dt>状态代次</dt><dd>{view.state.generation}</dd><dt>协调投递尝试次数</dt><dd>{view.state.attempt_count}</dd>
              <dt>下次协调检查时间</dt><dd className="tasks-meta"><time dateTime={view.state.next_attempt_at}>{view.state.next_attempt_at}</time></dd></dl>
            {view.state.held_reason ? <p>暂停原因：{heldText[view.state.held_reason] ?? "未识别的结构化原因"}（{view.state.held_reason}）</p> : null}
            {view.state.last_error_code ? <p>协调检查错误：{view.state.last_error_code}</p> : null}
            {view.state.delivery ? <dl><dt>协调员投递回执</dt><dd className="tasks-meta">{view.state.delivery.delivery_id}</dd>
              <dt>协调消息</dt><dd className="tasks-meta">{view.state.delivery.message_id}</dd></dl> : null}
            <p className="tasks-note">投递回执不表示 CLI 已接收、执行成员已运行或成果已接纳；本页不提供决定或分配详情。</p>
          </> : null}
          <button type="button" disabled={!enabled || !lookup.id} onClick={() => setLookup(value => ({ ...value, version: value.version + 1 }))}>刷新请求状态</button>
        </section>
      </div>
    </div>
  </section>;
}
