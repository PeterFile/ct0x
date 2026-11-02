import { useEffect, useId, useRef, useState } from "react";
import type { FormEvent } from "react";
import { ApiError, listMembers } from "./api";
import type { Member } from "./api";
import { getCoordinator, getDispatchPolicy, setCoordinator, setDispatchPolicy,
  isCoordinatorConfigurationDefinitiveRejection } from "./coordinator-configuration-api";
import type { CoordinatorBinding, DispatchPolicy, CoordinatorInput, DispatchPolicyInput } from "./coordinator-configuration-api";

const kinds = ["coordinator", "policy"] as const;
type Kind = typeof kinds[number];
const labels = { coordinator: "协调员", policy: "分派策略" };
type Configuration = { revision: number; ids: readonly string[] };
type Operation = Readonly<{
  groupId: string;
  key: string;
  everUnknown: boolean;
  pending: boolean;
  controller: AbortController;
} & ({ kind: "coordinator"; sent: CoordinatorInput } | { kind: "policy"; sent: DispatchPolicyInput })>;
type Entry = {
  currentRead: {
    value: Configuration | null;
    status: "idle" | "pending" | "ready" | "unavailable";
    round: number;
    error: string;
  };
  draft: readonly string[];
  dirty: boolean;
  operation: Operation | null;
  lastReceipt: Configuration | null;
  minRevision: number;
  minRead: number;
  needsRefresh: boolean;
  error: string;
};
const empty = (): Entry => ({
  currentRead: { value: null, status: "idle", round: 0, error: "" }, draft: [], dirty: false,
  operation: null, lastReceipt: null, minRevision: 0, minRead: 0, needsRefresh: false, error: "",
});
const configuration = (value: CoordinatorBinding | DispatchPolicy): Configuration => ({
  revision: value.revision, ids: "member_id" in value ? [value.member_id] : [...value.allowed_worker_ids],
});
const sameIds = (left: readonly string[], right: readonly string[]) => {
  const ids = new Set(right);
  return left.length === right.length && left.every(id => ids.has(id));
};
const errorText = (error: unknown) => error instanceof ApiError ? error.message : "读取未完成，请重试。";

export function CoordinationSettings({ groupId, active }: { groupId: string; active: boolean }) {
  const bodyId = useId();
  const [expanded, setExpanded] = useState(false);
  const [refresh, setRefresh] = useState(0);
  const [before, setBefore] = useState("");
  const [roster, setRoster] = useState({ items: [] as Member[], pending: false, error: "", next: "" });
  const [entries, setEntries] = useState(() => ({ coordinator: empty(), policy: empty() }));
  const state = useRef(entries);
  const readRound = useRef(0);
  const mounted = useRef(false);
  const enabled = active && expanded;
  // Synchronous ownership also closes the double-submit window before React commits.
  function update(kind: Kind, change: (previous: Entry) => Entry) {
    const next = change(state.current[kind]);
    state.current = { ...state.current, [kind]: next };
    setEntries(previous => ({ ...previous, [kind]: next }));
  }
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      for (const kind of kinds) state.current[kind].operation?.controller.abort();
    };
  }, []);
  useEffect(() => {
    if (!enabled) return;
    let ignore = false;
    const controller = new AbortController();
    const round = ++readRound.current;
    for (const kind of kinds) update(kind, previous => ({ ...previous,
      currentRead: { ...previous.currentRead, status: "pending", round, error: "" } }));
    function receive(kind: Kind, incoming: Configuration | null, failure?: unknown, allowUnbound = false) {
      if (ignore) return;
      update(kind, previous => {
        let value = incoming;
        if (allowUnbound && !previous.minRevision && !previous.currentRead.value?.revision) value = { revision: 0, ids: [] };
        let error = value ? "" : errorText(failure);
        const old = previous.currentRead.value;
        const receipt = previous.lastReceipt;
        if (value && (round < previous.minRead || value.revision < Math.max(previous.minRevision, old?.revision ?? 0)
          || (old?.revision === value.revision && !sameIds(old.ids, value.ids))
          || (receipt?.revision === value.revision && !sameIds(receipt.ids, value.ids)))) {
          error = "读取已过期或读取内容不一致，请刷新配置。";
          value = null;
        }
        return { ...previous, currentRead: { value: value ?? old, status: value ? "ready" : "unavailable", round, error },
          draft: value && !previous.dirty && !previous.operation ? [...value.ids] : previous.draft };
      });
    }
    const coordinator = getCoordinator(groupId, controller.signal);
    const policy = getDispatchPolicy(groupId, controller.signal);
    coordinator.then(value => receive("coordinator", configuration(value)), async error => {
      const missing = error instanceof ApiError && error.status === 404 && error.code === "not_found"
        && isCoordinatorConfigurationDefinitiveRejection(error);
      // Only the unbound inference waits for this round's policy; real GETs resolve independently.
      const allowUnbound = missing && await policy.then(() => true, () => false);
      receive("coordinator", null, error, allowUnbound);
    });
    policy.then(value => receive("policy", configuration(value)), error => receive("policy", null, error));
    return () => { ignore = true; controller.abort(); };
  }, [enabled, groupId, refresh]);
  useEffect(() => {
    if (!enabled) { setBefore(""); return; }
    let ignore = false;
    const controller = new AbortController();
    setRoster(previous => ({ ...previous, pending: true, error: "" }));
    listMembers(groupId, before, controller.signal).then(result => {
      if (!ignore) setRoster(previous => ({
        items: [...new Map([...(before ? previous.items : []), ...result.items].map(member => [member.id, member])).values()],
        pending: false, error: "", next: result.next_cursor,
      }));
    }, error => {
      if (!ignore) setRoster(previous => ({ ...previous, pending: false, error: errorText(error) }));
    });
    return () => { ignore = true; controller.abort(); };
  }, [enabled, groupId, before, refresh]);

  const directory = new Map(roster.items.map(member => [member.id, member]));
  function memberLabel(id: string) {
    const member = directory.get(id);
    return member ? `${member.name} · ${id} · ${member.role}（${member.status === "active" ? "在群" : member.status === "left" ? "已离群" : "状态未知"}）`
      : `${id}（成员未载入）`;
  }
  function blocked(kind: Kind) {
    const entry = state.current[kind];
    const other = state.current[kind === "coordinator" ? "policy" : "coordinator"];
    if (!enabled) return "请在当前群组展开协调配置。";
    if (entry.operation) return entry.operation.pending ? "正在等待原操作结果。" : "";
    if (other.operation) return "另一项保存尚未确认，不能开始新的保存。";
    if (kinds.some(key => state.current[key].needsRefresh)) return "保存被拒绝后，请显式刷新配置并保留草稿，核对群组、成员和版本后再保存。";
    if (kinds.some(key => state.current[key].currentRead.status !== "ready")) return "当前配置尚未重新确认，请刷新配置。";
    if (entry.currentRead.value!.revision >= Number.MAX_SAFE_INTEGER) return "版本已达到安全整数上限，不能继续保存。";
    if (kind === "coordinator" && !entry.draft.length) return "请选择协调员；不支持清空绑定。";
    if (entry.draft.length && (roster.pending || roster.error)) return "成员目录未确认，请刷新后核对。";
    if (entry.draft.some(id => directory.get(id)?.status !== "active")) return "所选成员未载入或已离群，请继续载入或显式移除、更换。";
    if (entry.draft.some(id => other.currentRead.value!.ids.includes(id))) return kind === "coordinator"
      ? "成员仍在已保存的 worker 集合中；请先显式移除、保存策略并确认当前值。"
      : "不能包含已保存的协调员；请先显式保存并确认协调员变更。";
    return "";
  }
  function editDraft(kind: Kind, ids: readonly string[]) {
    update(kind, previous => previous.operation ? previous : { ...previous, draft: [...ids], dirty: true, error: "" });
  }
  function refreshExplicitly() {
    for (const kind of kinds) update(kind, previous => ({ ...previous, needsRefresh: false,
      currentRead: { ...previous.currentRead, status: "pending", error: "" } }));
    setBefore("");
    setRefresh(previous => previous + 1);
  }
  async function submit(event: FormEvent<HTMLFormElement>, kind: Kind) {
    event.preventDefault();
    if (blocked(kind)) return;
    const entry = state.current[kind];
    const controller = new AbortController();
    const identity = entry.operation ?? {
      groupId, key: crypto.randomUUID(), everUnknown: false,
      ...(kind === "coordinator" ? { kind, sent: Object.freeze({
        member_id: entry.draft[0], expected_revision: entry.currentRead.value!.revision,
      }) } : { kind, sent: Object.freeze({
        allowed_worker_ids: Object.freeze([...entry.draft]), expected_revision: entry.currentRead.value!.revision,
      }) }),
    };
    const operation: Operation = { ...identity, pending: true, controller };
    update(kind, previous => ({ ...previous, operation, dirty: true, error: "" }));
    try {
      const receipt = operation.kind === "coordinator"
        ? await setCoordinator(operation.groupId, operation.sent, operation.key, controller.signal)
        : await setDispatchPolicy(operation.groupId, operation.sent, operation.key, controller.signal);
      if (!mounted.current) return;
      update(kind, previous => ({ ...previous, operation: null, lastReceipt: configuration(receipt),
        minRevision: Math.max(previous.minRevision, receipt.revision), minRead: readRound.current + 1,
        currentRead: { ...previous.currentRead, status: "unavailable" }, error: "" }));
      setRefresh(previous => previous + 1);
    } catch (error) {
      if (!mounted.current) return;
      const unknown = operation.everUnknown || !isCoordinatorConfigurationDefinitiveRejection(error);
      const refreshRequired = !unknown && error instanceof ApiError && [404, 409].includes(error.status);
      update(kind, previous => ({ ...previous,
        operation: unknown ? { ...operation, pending: false, everUnknown: true } : null,
        needsRefresh: previous.needsRefresh || refreshRequired,
        currentRead: refreshRequired ? { ...previous.currentRead, status: "unavailable" } : previous.currentRead,
        error: unknown ? `保存结果未确认。${errorText(error)} 请重试同一次保存。` : errorText(error),
      }));
    }
  }

  return <section className="group-panel" aria-label="协调配置" style={{ minWidth: 0, overflowWrap: "anywhere" }}>
    <button type="button" aria-expanded={expanded} aria-controls={bodyId} onClick={() => setExpanded(previous => !previous)}>
      {expanded ? "收起协调配置" : "展开协调配置"}
    </button>
    {kinds.map(kind => <p key={kind} role="status">{labels[kind]}：{entries[kind].operation?.everUnknown
      ? entries[kind].operation?.pending ? "结果未知，正在重试原操作。" : "结果未知，请保留原操作。"
      : entries[kind].operation?.pending ? "正在保存，结果待确认。" : "无待确认保存。"}</p>)}
    <div id={bodyId} hidden={!expanded || !active}>
      <p className="tasks-note">配置用于指定协调员和允许分派的 worker。保存仅登记配置，不代表运行就绪，不会创建任务授权或自动开始分派；两项读取不是原子快照，最终以服务端校验为准。</p>
      <p className="tasks-note">重载页面会丢失本地待确认身份，不代表取消或保存失败；本页不释放执行资源。</p>
      <button type="button" onClick={refreshExplicitly}>刷新配置并保留草稿</button>
      <p role="status">{roster.pending ? "正在载入成员目录…" : roster.error
        ? `成员目录未确认，已有目录已过期。${roster.error}` : "成员目录仅含已载入页面，职责不决定资格。"}</p>
      {roster.next ? <button type="button" disabled={roster.pending} onClick={() => setBefore(roster.next)}>加载更多成员</button> : null}
      {kinds.map(kind => {
        const entry = entries[kind];
        const value = entry.currentRead.value;
        const reason = blocked(kind);
        const candidates = roster.items.filter(member => member.status === "active"
          && (kind === "coordinator" || !entry.draft.includes(member.id)));
        const selected = kind === "coordinator" ? entry.draft[0] ?? "" : "";
        return <form key={kind} aria-label={`保存${labels[kind]}`} onSubmit={event => void submit(event, kind)}>
          <section aria-label={`当前${labels[kind]}`}>
            <h3>当前{labels[kind]}</h3>
            {value && !(kind === "coordinator" && !value.revision && entry.minRevision) ? <p>
              {kind === "coordinator" && !value.revision ? "尚未绑定协调员 · 版本 0"
                : `版本 ${value.revision} · ${value.ids.map(memberLabel).join("、") || "禁止向任何 worker 分派"}`}
            </p> : <p>暂无可信当前值。</p>}
            {entry.currentRead.status !== "ready" ? <p role="status">当前配置尚未重新确认。{entry.currentRead.error}</p> : null}
          </section>
          <fieldset disabled={Boolean(entry.operation)}>
            <legend>{labels[kind]}</legend>
            <label htmlFor={`${bodyId}-${kind}`}>{kind === "coordinator" ? "协调员成员" : "新增分派成员"}</label>
            <select id={`${bodyId}-${kind}`} value={selected} disabled={roster.pending || Boolean(roster.error)}
              onChange={event => {
                const id = event.target.value;
                editDraft(kind, kind === "coordinator" ? id ? [id] : [] : id ? [...entry.draft, id] : entry.draft);
              }}>
              <option value="">{kind === "coordinator" ? "请选择协调员（不能清空绑定）" : "逐个选择允许分派的成员"}</option>
              {selected && !candidates.some(member => member.id === selected)
                ? <option value={selected} disabled>{memberLabel(selected)}</option> : null}
              {candidates.map(member => <option key={member.id} value={member.id}>{memberLabel(member.id)}</option>)}
            </select>
            {kind === "policy" ? <ul aria-label="已选分派成员">{entry.draft.map(id => <li key={id}>
              {memberLabel(id)} <button type="button" aria-label={`移除 ${id}`}
                onClick={() => editDraft(kind, entry.draft.filter(value => value !== id))}>移除 {id}</button>
            </li>)}</ul> : null}
          </fieldset>
          {reason ? <p className="tasks-note">{reason}</p> : null}
          {entry.error ? <p role="alert">{entry.error}</p> : null}
          <button type="submit" disabled={Boolean(reason)}>{entry.operation?.pending ? "正在保存…" : entry.operation
            ? `重试同一次${labels[kind]}保存` : kind === "policy" && !entry.draft.length
              ? "保存空策略，禁止向任何 worker 分派" : `保存${labels[kind]}`}</button>
          <section aria-label={`${labels[kind]}历史保存回执`}>
            <h4>{labels[kind]}历史保存回执（不等同当前配置）</h4>
            {entry.lastReceipt ? <p>本次保存已确认（版本 {entry.lastReceipt.revision}） ·
              {entry.lastReceipt.ids.map(memberLabel).join("、") || "禁止向任何 worker 分派"}</p>
              : <p>本页尚无已确认保存回执。</p>}
          </section>
        </form>;
      })}
    </div>
  </section>;
}
