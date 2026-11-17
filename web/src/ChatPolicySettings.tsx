import { useEffect, useId, useRef, useState } from "react";
import { ApiError } from "./api";
import { getChatPolicy, hasChatPolicyMetadata, isChatPolicyDefinitiveRejection, setChatPolicy } from "./chat-api";
import type { ChatPolicy, ChatPolicyInput } from "./chat-api";

type Draft = { enabled: boolean; allowPreparation: boolean; allowWorkspaceChanges: boolean; scopeKind: string; budget: string };
type Operation = Readonly<{ key: string; input: ChatPolicyInput; source?: Readonly<{ id: string; content: string; created_at: string }> | null }>;
type State = {
  policy: ChatPolicy | null; read: "idle" | "pending" | "ready" | "unavailable"; readError: string;
  draft: Draft; dirty: boolean; operation: Operation | null; phase: "idle" | "pending" | "unknown";
  receipt: ChatPolicy | null; error: string; conflict: boolean;
};
const draftOf = (policy?: ChatPolicy): Draft => ({ enabled: policy?.enabled ?? false, allowPreparation: policy?.allow_preparation ?? false,
  allowWorkspaceChanges: policy?.workspace_changes_supported === true && policy.allow_workspace_changes === true,
  scopeKind: policy?.preparation_supported ? policy.preparation_scope_kind ?? "" : "", budget: policy?.revision ? String(policy.max_admissions) : "1" });
const errorText = (error: unknown) => error instanceof ApiError ? error.message : "请求未完成，请保留草稿后重试。";
// Workspace support reflects current schema capability, not an immutable policy fact.
const samePolicy = (left: ChatPolicy, right: ChatPolicy) => (["revision", "enabled", "allow_preparation", "preauthorization_message_id", "action", "max_admissions", "authorized_by"] as const)
  .every(field => left[field] === right[field]) && (left.preauthorization_text ?? "") === (right.preauthorization_text ?? "")
  && (left.preparation_scope_kind ?? "") === (right.preparation_scope_kind ?? "")
  && (left.preparation_supported ?? false) === (right.preparation_supported ?? false)
  && (left.configuration_source_id ?? "") === (right.configuration_source_id ?? "")
  && (left.allow_workspace_changes ?? false) === (right.allow_workspace_changes ?? false);

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

  function change(patch: Partial<Draft>) {
    if (!enabled || current.current.operation) return;
    update(previous => ({ ...previous, draft: { ...previous.draft, ...patch }, dirty: true, error: "" }));
  }
  async function submit() {
    const previous = current.current;
    if (!enabled || previous.phase === "pending") return;
    let operation = previous.operation;
    if (!operation) {
      if (previous.read !== "ready" || !previous.policy || !hasChatPolicyMetadata(previous.policy)
        || previous.policy.revision >= Number.MAX_SAFE_INTEGER) return;
      const maxAdmissions = Number(previous.draft.budget);
      if (!Number.isSafeInteger(maxAdmissions) || maxAdmissions < 1 || maxAdmissions > 100) {
        update(value => ({ ...value, error: "本版本累计任务上限必须是 1 至 100 的整数。" })); return;
      }
      if (previous.draft.allowPreparation && previous.draft.scopeKind !== "topic_summary") {
        update(value => ({ ...value, error: "请明确选择本群主话题已有消息的纯文本摘要范围；历史规则不能自动升级。" })); return;
      }
      if (previous.draft.allowWorkspaceChanges && previous.policy.workspace_changes_supported !== true) {
        update(value => ({ ...value, error: "目录工作协议能力未确认，不能开启目录改动许可。" })); return;
      }
      let key: string;
      try {
        key = crypto.randomUUID();
        if (!key) throw new Error("Empty idempotency key");
      } catch {
        update(value => ({ ...value, error: "无法生成保存标识，尚未提交；草稿已保留，请重试。" })); return;
      }
      operation = Object.freeze({ key, input: Object.freeze({ enabled: previous.draft.enabled, allow_preparation: previous.draft.allowPreparation,
        ...(previous.draft.allowPreparation ? { preparation_scope_kind: "topic_summary" as const } : {}), action: "text_submission",
        max_admissions: maxAdmissions, expected_revision: previous.policy.revision,
        ...(previous.draft.allowWorkspaceChanges ? { allow_workspace_changes: true } : {}) }) });
    }
    update(value => ({ ...value, operation, phase: "pending", dirty: true, error: "" }));
    try {
      const receipt = await setChatPolicy(groupId, operation.input, operation.key);
      if (!mounted.current) return;
      // Edits stay locked until this operation receives a matching receipt.
      update(value => ({ ...value, receipt, operation: null, phase: "idle", dirty: false, error: "", conflict: false, read: "pending" }));
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
  const locked = Boolean(state.operation), draft = state.draft, policy = state.policy;
  const metadata = Boolean(policy && hasChatPolicyMetadata(policy));
  const workspaceSupported = policy?.workspace_changes_supported === true;
  return <section className="group-panel chat-policy-settings" aria-label="自然聊天工作配置">
    <button type="button" aria-expanded={expanded} aria-controls={id} onClick={() => setExpanded(value => !value)}>
      {expanded ? "收起自然聊天工作配置" : "展开自然聊天工作配置"}</button>
    {state.phase !== "idle" ? <p role="status">{state.phase === "pending" ? "正在等待聊天策略保存回执。" : "聊天策略保存结果未知，请保留原操作。"}</p> : null}
    <div id={id} hidden={!enabled} className="tasks-lifecycle">
      <p className="tasks-note">范围：仅本群主话题 · 默认文本提交。目录工作须单独开启下方许可；策略本身不授予目录、命令执行或全局权限。</p>
      <p className="tasks-note">目录工作仅限明确聊天请求，必须同时具备已有同群成员读写授权与当前已验证的适配器工具能力；由服务端核对目录、任务与权限，不以配置路径或模型建议作为授权。</p>
      <p className="tasks-note">任务预算按策略版本累计，不按消息或评估轮次重置。保存会产生新版本和新的累计预算；停用只阻止后续准入，不取消在途工作。</p>
      <p className="tasks-note">普通明确请求不需要选择书面预授权消息。不会固定模型或强制独立审查，最终接纳仍由用户完成。</p>
      <section aria-label="当前聊天策略">
        {state.read === "ready" && policy ? <>
          <p>版本 {policy.revision} · {policy.revision && !policy.configuration_source_id ? `历史启用标记：${policy.enabled ? "开启" : "关闭"}` : policy.enabled ? "已启用" : "未启用"}
            {policy.revision ? ` · 本版本累计最多 ${policy.max_admissions} 项任务` : "（尚未配置）"}</p>
          {!metadata ? <p role="status">旧实例未提供范围与来源字段，新配置能力未确认；保留历史事实，不能据此保存新授权。</p> : null}
          {!workspaceSupported ? <p role="status">目录工作协议能力未确认或当前实例不支持，不能开启目录改动许可；原有文本配置不受影响。</p> : null}
          <p>目录改动许可：{policy.allow_workspace_changes ? workspaceSupported ? "允许明确请求使用已授权写目录" : "历史许可标记开启（协议能力未确认）" : "未开启"}。许可不代表已执行、已交付或用户接纳。</p>
          {policy.allow_preparation && !policy.preparation_supported ? <p role="status">历史准备规则当前不支持，不会自动升级为 topic_summary；请明确选择新范围或关闭准备工作后保存。</p> : null}
          <dl><dt>当前受支持的准备范围</dt><dd>{policy.preparation_supported ? "本群主话题已有消息的纯文本摘要（topic_summary）" : "无已确认的受支持准备范围"}</dd>
            {policy.preparation_scope_kind ? <><dt>服务端范围标识</dt><dd>{policy.preparation_scope_kind}</dd></> : null}
            <dt>策略配置命令 Source ID</dt><dd className="tasks-meta">{policy.configuration_source_id || "未提供（未核验历史配置来源）"}</dd>
            {policy.preauthorization_message_id ? <><dt>规范规则 / 历史书面消息 ID（只读）</dt><dd className="tasks-meta">{policy.preauthorization_message_id}</dd></> : null}</dl>
          {policy.preauthorization_text ? <p className="tasks-copy">{policy.preparation_supported ? "服务端规范书面规则" : "历史书面规则（不作为当前支持的准备授权）"}：{policy.preauthorization_text}</p> : null}
        </> : <p role="status">当前策略未确认。{state.readError}</p>}
        <p className="tasks-note">接口未提供已用数量，不推算剩余预算。以下为配置草稿，保存后才生效。</p>
      </section>
      <button type="button" onClick={() => setVersion(value => value + 1)}>读取当前聊天策略并保留草稿</button>
      <form aria-label="保存聊天策略" onSubmit={event => { event.preventDefault(); void submit(); }}>
        <fieldset disabled={locked || state.read === "unavailable" || !metadata}>
          <legend>自然聊天工作</legend>
          <label className="chat-policy-toggle"><input type="checkbox" checked={draft.enabled} onChange={event => change({ enabled: event.target.checked })} />启用自然聊天工作</label>
          <label className="chat-policy-toggle"><input type="checkbox" disabled={!workspaceSupported} checked={draft.allowWorkspaceChanges}
            onChange={event => change({ allowWorkspaceChanges: event.target.checked })} />允许明确请求使用已授权写目录</label>
          <label className="chat-policy-toggle"><input type="checkbox" checked={draft.allowPreparation} onChange={event => change({ allowPreparation: event.target.checked })} />允许受限准备工作</label>
          <label htmlFor={`${id}-budget`}>本版本累计任务上限</label>
          <input id={`${id}-budget`} type="number" min={1} max={100} step={1} required value={draft.budget} onChange={event => change({ budget: event.target.value })} />
          {draft.allowPreparation ? <>
            <label htmlFor={`${id}-scope`}>准备工作范围</label>
            <select id={`${id}-scope`} value={draft.scopeKind ?? ""} onChange={event => change({ scopeKind: event.target.value })}>
              <option value="">请明确选择新的受支持范围</option>
              <option value="topic_summary">本群主话题已有消息的纯文本摘要（topic_summary）</option>
            </select>
          </> : null}
        </fieldset>
        <p className="tasks-note">topic_summary 由服务端固定任务标题“本话题纯文本摘要”、绑定本群主话题与实际来源及策略版本的描述；验收要求区分事实、建议、未决问题并引用消息依据。不能自行填写更窄自由文字授权、目标或验收。</p>
        <p className="tasks-note">topic_summary 结构限制：摘要须非空有效 UTF-8、最多 8192 UTF-8 字节且无 Artifact 引用，不允许文件或外部动作；这些检查不代表摘要语义质量合格或用户接纳。</p>
        {state.operation ? <details><summary>原未确认保存（仅对账，不是新授权）</summary>
          <pre className="tasks-copy">{JSON.stringify(state.operation.input)}</pre>
          {state.operation.source ? <><p className="tasks-copy">原引用消息快照：{state.operation.source.content}</p>
            <p className="tasks-meta">{state.operation.source.id}</p><time dateTime={state.operation.source.created_at}>{state.operation.source.created_at}</time></> : null}
        </details> : null}
        {state.conflict ? <p role="status">{state.operation ? "当前版本仅供核对；原保存仍未确认，不能换键或替换草稿。"
          : "版本冲突后已保留草稿；请核对真实当前版本，再由你决定是否保存。"}</p> : null}
        {state.error ? <p role="alert">{state.error}</p> : null}
        <button type="submit" disabled={state.phase === "pending" || (!state.operation && (state.read !== "ready" || !metadata))}>{state.phase === "pending" ? "正在保存聊天策略…"
          : state.phase === "unknown" ? "重试同一次聊天策略保存" : "保存聊天策略"}</button>
      </form>
      {state.receipt ? <p role="status">聊天策略保存已确认（历史版本 {state.receipt.revision}）；当前值以重新读取为准。</p> : null}
      <p className="tasks-note">草稿和未确认保存仅保留在本页；重载会丢失原幂等身份，不表示取消或保存失败。</p>
    </div>
  </section>;
}
