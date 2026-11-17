import { useEffect, useId, useRef, useState } from "react";
import { ApiError, getMessage, listMessages } from "./api";
import type { Message } from "./api";
import { getChatPolicy, isChatPolicyDefinitiveRejection, setChatPolicy } from "./chat-api";
import type { ChatPolicy, ChatPolicyInput } from "./chat-api";

type Draft = { enabled: boolean; allowPreparation: boolean; sourceId: string; budget: string };
type Operation = Readonly<{ key: string; input: ChatPolicyInput; source: Message | null }>;
type State = {
  policy: ChatPolicy | null; read: "idle" | "pending" | "ready" | "unavailable"; readError: string;
  draft: Draft; dirty: boolean; operation: Operation | null; phase: "idle" | "pending" | "unknown";
  receipt: ChatPolicy | null; error: string; conflict: boolean;
};
const draftOf = (policy?: ChatPolicy): Draft => ({ enabled: policy?.enabled ?? false, allowPreparation: policy?.allow_preparation ?? false,
  sourceId: policy?.preauthorization_message_id ?? "", budget: policy?.revision ? String(policy.max_admissions) : "1" });
const errorText = (error: unknown) => error instanceof ApiError ? error.message : "请求未完成，请保留草稿后重试。";
const samePolicy = (left: ChatPolicy, right: ChatPolicy) => (["revision", "enabled", "allow_preparation", "preauthorization_message_id", "action", "max_admissions", "authorized_by"] as const)
  .every(field => left[field] === right[field]) && (left.preauthorization_text ?? "") === (right.preauthorization_text ?? "");
const sourceValid = (message: Message | null, groupId: string) => message?.group_id === groupId && message.topic_id === "" && message.sender_id === "user"
  && /\P{White_Space}/u.test(message.content) && !/[\0\ud800-\udfff]/u.test(message.content) && new TextEncoder().encode(message.content).length <= 8192;
const preview = (message: Message) => `${message.content.slice(0, 100)}${message.content.length > 100 ? "…" : ""} · ${message.created_at}`;

// One instance per visited group; visibility only suspends reads, never an owned PUT.
export function ChatPolicySettings({ groupId, active }: { groupId: string; active: boolean }) {
  const id = useId(), mounted = useRef(false);
  const [expanded, setExpanded] = useState(false), [version, setVersion] = useState(0);
  const enabled = active && expanded;
  const [state, setState] = useState<State>({ policy: null, read: "idle", readError: "", draft: draftOf(), dirty: false,
    operation: null, phase: "idle", receipt: null, error: "", conflict: false });
  const current = useRef(state);
  function update(change: (previous: State) => State) {
    const next = change(current.current); current.current = next; setState(next);
  }
  const [before, setBefore] = useState("");
  const [messages, setMessages] = useState({ items: [] as Message[], pending: false, error: "", next: "" });
  const [source, setSource] = useState<{ id: string; value: Message | null; pending: boolean; error: string }>({ id: "", value: null, pending: false, error: "" });
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  useEffect(() => {
    if (!enabled) return;
    let ignore = false;
    const controller = new AbortController();
    update(previous => ({ ...previous, read: "pending", readError: "" }));
    void getChatPolicy(groupId, controller.signal).then(policy => {
      if (ignore) return;
      update(previous => {
        const floor = Math.max(previous.policy?.revision ?? 0, previous.receipt?.revision ?? 0);
        if (policy.revision < floor || (previous.policy?.revision === policy.revision && !samePolicy(previous.policy, policy))
          || (previous.receipt?.revision === policy.revision && !samePolicy(previous.receipt, policy))) {
          return { ...previous, read: "unavailable", readError: "当前策略读取过期或内容不一致，请重新读取。" };
        }
        return { ...previous, policy, read: "ready", readError: "", draft: !previous.dirty && !previous.operation ? draftOf(policy) : previous.draft };
      });
    }, error => {
      if (!ignore) update(previous => ({ ...previous, read: "unavailable", readError: error instanceof ApiError && error.status === 404
        ? "当前实例的自然聊天工作配置不可用，不能确认已启用。" : errorText(error) }));
    });
    return () => { ignore = true; controller.abort(); };
  }, [enabled, groupId, version]);
  useEffect(() => {
    if (!enabled || !state.draft.allowPreparation) { setBefore(""); return; }
    let ignore = false;
    const controller = new AbortController();
    setMessages(previous => ({ ...previous, pending: true, error: "" }));
    void listMessages(groupId, before, controller.signal).then(page => {
      if (!ignore) setMessages(previous => ({ items: [...new Map([...(before ? previous.items : []), ...page.items]
        .filter(message => sourceValid(message, groupId)).map(message => [message.id, message])).values()], pending: false, error: "", next: page.next_cursor }));
    }, error => { if (!ignore) setMessages(previous => ({ ...previous, pending: false, error: errorText(error) })); });
    return () => { ignore = true; controller.abort(); };
  }, [enabled, groupId, state.draft.allowPreparation, before, version]);
  useEffect(() => {
    const sourceId = state.draft.sourceId;
    if (!enabled || !state.draft.allowPreparation || !sourceId) return;
    let ignore = false;
    const controller = new AbortController();
    setSource({ id: sourceId, value: null, pending: true, error: "" });
    void getMessage(groupId, sourceId, controller.signal).then(message => {
      if (!ignore) setSource({ id: sourceId, value: sourceValid(message, groupId) ? message : null, pending: false,
        error: sourceValid(message, groupId) ? "" : "授权来源必须是本群主话题的真实用户书面消息（非空、无 NUL，最多 8192 UTF-8 字节）。" });
    }, error => { if (!ignore) setSource({ id: sourceId, value: null, pending: false, error: errorText(error) }); });
    return () => { ignore = true; controller.abort(); };
  }, [enabled, groupId, state.draft.allowPreparation, state.draft.sourceId, version]);

  function change(patch: Partial<Draft>) {
    if (!enabled || current.current.operation) return;
    update(previous => ({ ...previous, draft: { ...previous.draft, ...patch }, dirty: true, error: "" }));
  }
  async function submit() {
    const previous = current.current;
    if (!enabled || previous.phase === "pending") return;
    let operation = previous.operation;
    if (!operation) {
      if (previous.read !== "ready" || !previous.policy || previous.policy.revision >= Number.MAX_SAFE_INTEGER) return;
      const maxAdmissions = Number(previous.draft.budget);
      const chosen = source.id === previous.draft.sourceId && !source.pending ? source.value : null;
      if (!Number.isSafeInteger(maxAdmissions) || maxAdmissions < 1 || maxAdmissions > 100) {
        update(value => ({ ...value, error: "本版本累计任务上限必须是 1 至 100 的整数。" })); return;
      }
      if (previous.draft.allowPreparation && !sourceValid(chosen, groupId)) {
        update(value => ({ ...value, error: "请从本群主话题的真实用户消息中选择并确认书面授权来源。" })); return;
      }
      let key: string;
      try {
        key = crypto.randomUUID();
        if (!key) throw new Error("Empty idempotency key");
      } catch {
        update(value => ({ ...value, error: "无法生成保存标识，尚未提交；草稿已保留，请重试。" })); return;
      }
      operation = Object.freeze({ key, source: chosen ? Object.freeze({ ...chosen }) : null,
        input: Object.freeze({ enabled: previous.draft.enabled, allow_preparation: previous.draft.allowPreparation,
          preauthorization_message_id: previous.draft.allowPreparation ? previous.draft.sourceId : "", action: "text_submission",
          max_admissions: maxAdmissions, expected_revision: previous.policy.revision }) });
    }
    update(value => ({ ...value, operation, phase: "pending", dirty: true, error: "" }));
    try {
      const receipt = await setChatPolicy(groupId, operation.input, operation.key);
      if (!mounted.current) return;
      update(value => ({ ...value, receipt, operation: null, phase: "idle", error: "", conflict: false, read: "pending" }));
      setVersion(value => value + 1);
    } catch (error) {
      if (!mounted.current) return;
      const definitive = isChatPolicyDefinitiveRejection(error);
      const unknown = previous.phase === "unknown" || !definitive;
      const conflict = definitive && error instanceof ApiError && error.status === 409;
      const unavailable = !unknown && error instanceof ApiError && error.status === 404;
      update(value => ({ ...value, operation: unknown ? operation : null, phase: unknown ? "unknown" : "idle",
        conflict: value.conflict || conflict, read: conflict ? "pending" : unavailable ? "unavailable" : value.read,
        readError: unavailable ? "当前实例的自然聊天工作配置不可用，不能确认已启用。" : value.readError,
        error: unknown ? `保存结果未确认。${errorText(error)} 请重试同一次聊天策略保存。` : errorText(error) }));
      if (conflict) setVersion(value => value + 1);
    }
  }
  const locked = Boolean(state.operation), draft = state.draft;
  const chosen = source.id === draft.sourceId ? source.value : null;
  const shownSource = state.operation?.source ?? chosen;
  const choices = [...messages.items];
  if (chosen && !choices.some(message => message.id === chosen.id)) choices.unshift(chosen);
  return <section className="group-panel chat-policy-settings" aria-label="自然聊天工作配置">
    <button type="button" aria-expanded={expanded} aria-controls={id} onClick={() => setExpanded(value => !value)}>
      {expanded ? "收起自然聊天工作配置" : "展开自然聊天工作配置"}</button>
    {state.phase !== "idle" ? <p role="status">{state.phase === "pending" ? "正在等待聊天策略保存回执。" : "聊天策略保存结果未知，请保留原操作。"}</p> : null}
    <div id={id} hidden={!enabled} className="tasks-lifecycle">
      <p className="tasks-note">范围：仅本群主话题 · 文本提交。不授予文件写入、命令执行或文件成果发布权限；执行仍受已有成员与资源授权约束。</p>
      <p className="tasks-note">任务预算按策略版本累计，不按消息或评估轮次重置。保存会产生新版本和新的累计预算；停用只阻止后续准入，不取消在途工作。</p>
      <p className="tasks-note">普通明确请求不需要选择书面预授权消息。不会固定模型或强制独立审查，最终接纳仍由用户完成。</p>
      <section aria-label="当前聊天策略">
        {state.read === "ready" && state.policy ? <p>版本 {state.policy.revision} · {state.policy.enabled ? "已启用" : "未启用"}
          {state.policy.revision ? ` · 本版本累计最多 ${state.policy.max_admissions} 项任务` : "（尚未配置）"}</p> : <p role="status">当前策略未确认。{state.readError}</p>}
        {state.policy?.preauthorization_text && state.read === "ready" ? <p className="tasks-copy">当前书面授权：{state.policy.preauthorization_text}</p> : null}
        <p className="tasks-note">接口未提供已用数量，不推算剩余预算。以下为配置草稿，保存后才生效。</p>
      </section>
      <button type="button" onClick={() => setVersion(value => value + 1)}>读取当前聊天策略并保留草稿</button>
      <form aria-label="保存聊天策略" onSubmit={event => { event.preventDefault(); void submit(); }}>
        <fieldset disabled={locked || state.read === "unavailable"}>
          <legend>自然聊天工作</legend>
          <label className="chat-policy-toggle"><input type="checkbox" checked={draft.enabled} onChange={event => change({ enabled: event.target.checked })} />启用自然聊天工作</label>
          <label className="chat-policy-toggle"><input type="checkbox" checked={draft.allowPreparation} onChange={event => change({ allowPreparation: event.target.checked })} />允许按书面预授权开展准备工作</label>
          <label htmlFor={`${id}-budget`}>本版本累计任务上限</label>
          <input id={`${id}-budget`} type="number" min={1} max={100} step={1} required value={draft.budget} onChange={event => change({ budget: event.target.value })} />
          {draft.allowPreparation ? <>
            <label htmlFor={`${id}-source`}>书面授权来源</label>
            <select id={`${id}-source`} value={draft.sourceId} disabled={messages.pending || Boolean(messages.error)} onChange={event => change({ sourceId: event.target.value })}>
              <option value="">请选择本群主话题的用户书面消息</option>
              {draft.sourceId && !choices.some(message => message.id === draft.sourceId) ? <option value={draft.sourceId} disabled>正在确认原授权来源</option> : null}
              {choices.map(message => <option key={message.id} value={message.id}>{preview(message)} · {message.id}</option>)}
            </select>
            {messages.error ? <p role="alert">授权消息未载入。{messages.error}</p> : null}
            {messages.next ? <button type="button" disabled={messages.pending} onClick={() => setBefore(messages.next)}>加载更多授权消息</button> : null}
          </> : null}
        </fieldset>
        {draft.allowPreparation ? <section aria-label="授权来源预览">
          {source.pending ? <p role="status">正在确认真实授权消息…</p> : null}
          {shownSource ? <><p className="tasks-copy">{shownSource.content}</p><time dateTime={shownSource.created_at}>{shownSource.created_at}</time>
            <details><summary>来源标识</summary><p className="tasks-meta">{shownSource.id}</p></details></> : null}
          {source.error ? <p role="alert">{source.error}</p> : null}
        </section> : null}
        {state.conflict ? <p role="status">{state.operation ? "当前版本仅供核对；原保存仍未确认，不能换键或替换草稿。"
          : "版本冲突后已保留草稿；请核对真实当前版本，再由你决定是否保存。"}</p> : null}
        {state.error ? <p role="alert">{state.error}</p> : null}
        <button type="submit" disabled={state.phase === "pending" || (!state.operation && state.read !== "ready")}>{state.phase === "pending" ? "正在保存聊天策略…"
          : state.phase === "unknown" ? "重试同一次聊天策略保存" : "保存聊天策略"}</button>
      </form>
      {state.receipt ? <p role="status">聊天策略保存已确认（历史版本 {state.receipt.revision}）；当前值以重新读取为准。</p> : null}
      <p className="tasks-note">草稿和未确认保存仅保留在本页；重载会丢失原幂等身份，不表示取消或保存失败。</p>
    </div>
  </section>;
}
