import { useEffect, useId, useRef, useState } from "react";
import { ApiError } from "./api";
import { getDiscussionPolicy, isDiscussionInviteHops, isDiscussionPolicyDefinitiveRejection, setDiscussionPolicy } from "./discussion-policy-api";
import type { DiscussionInviteHops, DiscussionPolicy, DiscussionPolicyInput } from "./discussion-policy-api";

type Scope = { groupId: string; active: boolean };
type Attempt = Readonly<{ key: string; groupId: string; input: DiscussionPolicyInput }>;
type State = {
  policy: DiscussionPolicy | null; reading: boolean; readError: string; draft: DiscussionInviteHops | null;
  attempt: Attempt | null; saving: boolean; savedRevision: number | null; savedHops: DiscussionInviteHops | null; error: string;
};
const unavailable = "当前实例的讨论策略不可用。";
const errorText = (error: unknown) => error instanceof ApiError ? error.message : "请求未完成，请重试。";

export function ChatDiscussionPolicySettings({ groupId, active }: Scope) {
  const id = useId();
  const [expanded, setExpanded] = useState<boolean | null>(null);
  return <section aria-label="讨论策略">
    <button type="button" aria-expanded={expanded === true} aria-controls={id} onClick={() => setExpanded(value => !value)}>
      {expanded ? "收起讨论策略" : "展开讨论策略"}</button>
    <div id={id} hidden={!active || !expanded}>
      {expanded !== null ? <DiscussionPolicyEditor key={groupId} groupId={groupId} active={active && expanded} /> : null}
    </div>
  </section>;
}

function DiscussionPolicyEditor({ groupId, active }: Scope) {
  const [version, setVersion] = useState(0), mounted = useRef(false);
  const [state, setState] = useState<State>({ policy: null, reading: true, readError: "", draft: null, attempt: null,
    saving: false, savedRevision: null, savedHops: null, error: "" });
  const current = useRef(state);
  function update(change: (value: State) => State) {
    const next = change(current.current); current.current = next; setState(next);
  }
  function refresh() {
    update(value => ({ ...value, reading: true, readError: "" }));
    setVersion(value => value + 1);
  }
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  useEffect(() => {
    if (!active) return;
    const controller = new AbortController();
    update(value => ({ ...value, reading: true, readError: "" }));
    void getDiscussionPolicy(groupId, controller.signal).then(policy => {
      if (controller.signal.aborted) return;
      update(value => {
        const floor = Math.max(value.policy?.revision ?? 0, value.savedRevision ?? 0);
        if (policy.revision < floor || (policy.revision === value.policy?.revision && policy.max_invite_hops !== value.policy.max_invite_hops)
          || (policy.revision === value.savedRevision && policy.max_invite_hops !== value.savedHops)) {
          return { ...value, reading: false, readError: "读取的讨论策略过期或内容不一致，请重新读取。" };
        }
        return { ...value, policy, reading: false, readError: "" };
      });
    }, error => {
      if (!controller.signal.aborted) update(value => ({ ...value, reading: false,
        readError: error instanceof ApiError && error.status === 404 ? unavailable : errorText(error) }));
    });
    return () => controller.abort();
  }, [active, groupId, version]);

  async function submit() {
    const previous = current.current;
    if (!active || previous.saving) return;
    let attempt = previous.attempt;
    if (!attempt) {
      const policy = previous.policy;
      if (!policy || previous.reading || previous.readError || policy.revision >= Number.MAX_SAFE_INTEGER) return;
      try {
        const key = crypto.randomUUID();
        if (!key) throw new Error("Empty idempotency key");
        attempt = Object.freeze({ groupId, key, input: Object.freeze({ expected_revision: policy.revision,
          max_invite_hops: previous.draft ?? policy.max_invite_hops }) });
      } catch {
        update(value => ({ ...value, error: "无法生成保存标识，尚未提交。" })); return;
      }
    }
    update(value => ({ ...value, attempt, draft: attempt.input.max_invite_hops, saving: true, error: "" }));
    try {
      const saved = await setDiscussionPolicy(attempt.groupId, attempt.input, attempt.key);
      if (!mounted.current) return;
      update(value => ({ ...value, attempt: null, draft: null, saving: false, savedRevision: saved.revision, savedHops: saved.max_invite_hops }));
      refresh();
    } catch (error) {
      if (!mounted.current) return;
      const definitive = isDiscussionPolicyDefinitiveRejection(error);
      const unknown = previous.attempt !== null || !definitive;
      const conflict = definitive && error instanceof ApiError && error.status === 409;
      const removed = !unknown && error instanceof ApiError && error.status === 404;
      update(value => ({ ...value, attempt: unknown ? attempt : null, saving: false,
        readError: removed ? unavailable : value.readError,
        error: unknown ? "保存结果未确认。请保留原设置，重试同一次讨论策略保存。"
          : conflict ? "版本或幂等键冲突，已保留草稿。请核对重新读取的版本，再决定是否保存。" : errorText(error) }));
      if (conflict) refresh();
    }
  }

  const policy = !state.reading && !state.readError ? state.policy : null;
  return <>
    <p className="tasks-note">仅本群主话题；不会邀请其他群的成员，不扩大消息可见范围，也不授予工作权限。</p>
    <p className="tasks-note">每次邀请预留一次返回唤醒。0 次关闭新讨论的成员互邀，不影响公开消息、本机用户 @ 投递或已开始的讨论。</p>
    <section aria-label="当前讨论策略">
      {policy ? <p>版本 {policy.revision}{policy.revision === 0 ? "（默认）" : ""}：最多 {policy.max_invite_hops} 次邀请、{policy.max_targeted_wakes} 次定向唤醒。</p>
        : <p role="status">{state.readError || "正在读取讨论策略…"}</p>}
    </section>
    <button type="button" disabled={state.saving} onClick={refresh}>重新读取讨论策略</button>
    <form aria-label="保存讨论策略" onSubmit={event => { event.preventDefault(); void submit(); }}>
      <label>每条新讨论的邀请上限
        <select value={state.draft ?? policy?.max_invite_hops ?? ""} disabled={!policy || !!state.attempt} onChange={event => {
          const hops = Number(event.target.value);
          if (active && !current.current.attempt && isDiscussionInviteHops(hops)) update(value => ({ ...value, draft: hops, error: "" }));
        }}>
          <option value="" disabled>先读取当前策略</option>
          {[0, 1, 2].map(hops => <option key={hops} value={hops}>{hops} 次邀请 / {2 * hops} 次定向唤醒</option>)}
        </select>
      </label>
      {state.attempt ? <p className="tasks-note">本次保存的基准版本：{state.attempt.input.expected_revision}。重试将使用原设置和幂等键。</p> : null}
      {state.error ? <p role="alert">{state.error}</p> : null}
      <button type="submit" disabled={state.saving || (!state.attempt && (!policy || policy.revision >= Number.MAX_SAFE_INTEGER))}>
        {state.saving ? "正在保存讨论策略…" : state.attempt ? "重试同一次讨论策略保存" : "保存讨论策略"}</button>
    </form>
    {state.savedRevision !== null ? <p role="status">已确认保存（历史版本 {state.savedRevision}）；当前值以重新读取为准。</p> : null}
    <p className="tasks-note">草稿和未确认的保存仅保留在本页；重载不会取消已提交的请求。</p>
  </>;
}
