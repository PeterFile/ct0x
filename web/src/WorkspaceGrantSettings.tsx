import { useEffect, useId, useRef, useState } from "react";
import { ApiError } from "./api";
import { authorizeWorkspaceGrant, getWorkspaceGrant, isWorkspaceGrantDefinitiveRejection } from "./workspace-grant-api";
import type { WorkspaceGrantInput, WorkspaceGrantReceipt } from "./workspace-grant-api";
import "./workspace-grants.css";

type Identity = { groupId: string; memberId: string };
type Operation = Readonly<{ key: string; input: WorkspaceGrantInput }>;
type State = {
  grant: WorkspaceGrantReceipt | null; receipt: WorkspaceGrantReceipt | null;
  read: "idle" | "pending" | "ready" | "unavailable"; readError: string;
  draft: { root: string; scope: "" | WorkspaceGrantInput["scope"] }; operation: Operation | null;
  phase: "idle" | "pending" | "unknown"; error: string;
};
const errorText = (error: unknown) => error instanceof ApiError ? error.message : "请求未完成，请保留原操作后重试。";
const sameGrant = (left: WorkspaceGrantReceipt, right: WorkspaceGrantReceipt) =>
  (Object.keys(left) as (keyof WorkspaceGrantReceipt)[]).every(field => left[field] === right[field]);

// The host supplies its existing selection; no name inference or second member catalogue.
export function WorkspaceGrantSettings({ groupId, memberId, active }: { groupId: string; memberId?: string; active: boolean }) {
  const [visited, setVisited] = useState<Identity[]>(() => memberId ? [{ groupId, memberId }] : []);
  useEffect(() => {
    if (memberId) setVisited(previous => previous.some(scope => scope.groupId === groupId && scope.memberId === memberId)
      ? previous : [...previous, { groupId, memberId }]);
  }, [groupId, memberId]);
  return <>
    {!memberId && active ? <p className="tasks-note">请先选择群内工作成员，再配置一次目录授权。</p> : null}
    {visited.map(scope => {
      const visible = active && scope.groupId === groupId && scope.memberId === memberId;
      return <div key={JSON.stringify([scope.groupId, scope.memberId])} hidden={!visible}>
        <GrantScope {...scope} active={visible} />
      </div>;
    })}
  </>;
}
function GrantScope({ groupId, memberId, active }: Identity & { active: boolean }) {
  const id = useId(), mounted = useRef(false);
  const [expanded, setExpanded] = useState(false), [version, setVersion] = useState(0);
  const enabled = active && expanded;
  const [state, setState] = useState<State>({ grant: null, receipt: null, read: "idle", readError: "",
    draft: { root: "", scope: "" }, operation: null, phase: "idle", error: "" });
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
    void getWorkspaceGrant(groupId, memberId, controller.signal).then(grant => {
      if (ignore) return;
      update(previous => {
        const floor = previous.grant ?? previous.receipt;
        if (floor && (!grant || !sameGrant(floor, grant))) return { ...previous, read: "unavailable",
          readError: "不可变目录授权读取消失或内容不一致；保留历史回执，当前读取未确认。" };
        return { ...previous, grant, read: "ready", readError: "" };
      });
    }, error => { if (!ignore) update(previous => ({ ...previous, read: "unavailable", readError: errorText(error) })); });
    return () => { ignore = true; controller.abort(); };
  }, [enabled, groupId, memberId, version]);

  function change(patch: Partial<State["draft"]>) {
    if (!enabled || current.current.operation) return;
    update(previous => ({ ...previous, draft: { ...previous.draft, ...patch }, error: "" }));
  }
  async function submit() {
    const previous = current.current;
    if (!enabled || previous.phase === "pending") return;
    let operation = previous.operation;
    if (!operation) {
      if (previous.read !== "ready" || previous.grant || previous.receipt) return;
      const { root, scope } = previous.draft;
      if (!root.startsWith("/") || root.includes("\0") || /[\ud800-\udfff]/u.test(root) || !scope) {
        update(value => ({ ...value, error: "请输入已有目录的绝对路径，并明确选择只读或读写范围。" })); return;
      }
      let key: string;
      try { key = crypto.randomUUID(); if (!key) throw new Error("Empty idempotency key"); }
      catch { update(value => ({ ...value, error: "无法生成授权标识，尚未提交；请保留草稿后重试。" })); return; }
      operation = Object.freeze({ key, input: Object.freeze({ root, scope }) });
    }
    update(value => ({ ...value, operation, phase: "pending", error: "" }));
    try {
      const receipt = await authorizeWorkspaceGrant(groupId, memberId, operation.input, operation.key);
      if (!mounted.current) return;
      const floor = current.current.grant ?? current.current.receipt;
      if (floor && !sameGrant(floor, receipt)) throw new ApiError("保存回执与已读取的不可变目录授权不一致，结果未确认。", 201, "invalid_response");
      update(value => ({ ...value, receipt, operation: null, phase: "idle", error: "", read: "pending" }));
      setVersion(value => value + 1);
    } catch (error) {
      if (!mounted.current) return;
      const definitive = isWorkspaceGrantDefinitiveRejection(error), unknown = previous.phase === "unknown" || !definitive;
      const conflict = definitive && error instanceof ApiError && error.status === 409;
      update(value => ({ ...value, operation: unknown ? operation : null, phase: unknown ? "unknown" : "idle",
        error: unknown ? `目录授权保存结果未确认。${errorText(error)} 请重试同一次目录授权。` : errorText(error) }));
      if (conflict) setVersion(value => value + 1);
    }
  }
  const known = state.grant ?? state.receipt, locked = Boolean(state.operation);
  return <section className="group-panel workspace-grant-settings" aria-label="成员目录授权">
    <button type="button" aria-expanded={expanded} aria-controls={id} onClick={() => setExpanded(value => !value)}>
      {expanded ? "收起目录授权" : "展开目录授权"}</button>
    {state.phase !== "idle" ? <p role="status">{state.phase === "pending" ? "正在等待目录授权保存回执。" : "目录授权保存结果未知，原操作已保留。"}</p> : null}
    <div id={id} hidden={!enabled}>
      <p className="tasks-meta">成员 ID：{memberId} · 群组 ID：{groupId}</p>
      <p className="tasks-note">这是一次目录授权，不是每个任务的授权或分派表；不会创建任务或推动执行。成员工作目录偏好不代表授权。</p>
      {state.read === "pending" ? <p role="status">正在读取目录授权…</p> : null}
      {state.readError ? <p role="alert">{state.readError}</p> : null}
      {known ? <section aria-label="已确认目录授权回执">
        <h4>已确认目录授权回执</h4>
        {state.read !== "ready" ? <p role="status">当前读取未确认；以下为已核验的历史授权回执，不代表当前可执行。</p> : null}
        <dl><dt>实际授权范围</dt><dd>{known.scope === "read_write" ? "读写范围（read_write）" : "只读范围（read_only）"}</dd>
          <dt>授权目录</dt><dd>{known.root}</dd><dt>授权时解析目录</dt><dd>{known.resolved_root}</dd>
          <dt>物理资源标识</dt><dd>{known.resource_key}</dd><dt>授权 ID</dt><dd>{known.id}</dd>
          <dt>所属群组 ID</dt><dd>{known.group_id}</dd><dt>授权成员 ID</dt><dd>{known.member_id}</dd>
          <dt>节点</dt><dd>本机（{known.node_id}）</dd><dt>授权者</dt><dd>本机用户（{known.authorized_by}）</dd>
          <dt>授权时间</dt><dd><time dateTime={known.created_at}>{known.created_at}</time></dd></dl>
        <p className="tasks-note">回执仅证明目录权限，不代表当前可执行、实际文件改动、Artifact 交付或用户接纳；执行仍需服务端核对当前任务与已验证适配器能力。</p>
        <p className="tasks-note">此授权不可变，不能升级范围，也不能改配到另一目录；本入口没有撤销操作。</p>
        {known.scope === "read_only" ? <p className="tasks-note">只读授权不允许目录写入。请在成员列表选择已有的未授权工作成员，或使用现有添加成员入口；不会自动创建替代成员。</p> : null}
      </section> : state.read === "ready" ? <p role="status">该成员尚无已确认目录授权。</p> : null}
      <button type="button" onClick={() => setVersion(value => value + 1)}>读取目录授权并保留原操作</button>
      {!known || state.operation ? <form aria-label="保存目录授权" onSubmit={event => { event.preventDefault(); void submit(); }}>
        <fieldset disabled={locked || state.read !== "ready" || Boolean(known)}>
          <legend>一次目录授权</legend>
          <label htmlFor={`${id}-root`}>已有目录的绝对路径</label>
          <input id={`${id}-root`} value={state.draft.root} required autoComplete="off" spellCheck={false}
            onChange={event => change({ root: event.target.value })} aria-describedby={`${id}-permission`} />
          <label htmlFor={`${id}-scope`}>目录授权范围</label>
          <select id={`${id}-scope`} value={state.draft.scope} required onChange={event => change({ scope: event.target.value as State["draft"]["scope"] })}>
            <option value="">请明确选择授权范围</option><option value="read_only">只读</option><option value="read_write">读写（有界文本文件）</option>
          </select>
        </fieldset>
        <p id={`${id}-permission`} className="tasks-note">保存读写授权后，仅允许在该已有目录内使用受限工具处理有界文本文件；不会授予 shell、个人配置或目录外的全局权限，也不是操作系统沙箱。目录是否存在及是否受保护由服务端校验。</p>
        {state.operation ? <details><summary>原未确认目录授权（仅用于对账）</summary><pre>{JSON.stringify(state.operation.input)}</pre></details> : null}
        {state.error ? <p role="alert">{state.error}</p> : null}
        <button type="submit" disabled={state.phase === "pending" || (!state.operation && (state.read !== "ready" || Boolean(known)))}>
          {state.phase === "pending" ? "正在保存目录授权…" : state.phase === "unknown" ? "重试同一次目录授权" : "保存目录授权"}</button>
      </form> : state.error ? <p role="alert">{state.error}</p> : null}
      <p className="tasks-note">草稿与未确认操作仅保留在本页；重载丢失原幂等身份，不表示撤销或授权失败。</p>
    </div>
  </section>;
}
