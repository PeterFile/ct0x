import { useEffect, useId, useRef, useState } from "react";
import type { FormEvent } from "react";
import { ApiError, createMember, isDefinitiveRejection, listMembers, listProviders } from "./api";
import type { Member, MemberInput, Provider } from "./api";

function unique<T extends { id: string }>(items: T[]) {
  const seen = new Set<string>();
  return items.filter(item => { if (seen.has(item.id)) return false; seen.add(item.id); return true; });
}
const errorText = (error: unknown) => error instanceof ApiError ? error.message : "请求未完成，请重试。";
const providerLabel = (provider: Provider) => `${provider.name} · ${provider.version || "版本未知"} · ${provider.path || "路径未知"} · ${
  provider.account === "authenticated" ? "账户已登录（运行未验证）" : provider.account === "unauthenticated" ? "未登录" : "账户未知"}`;
const emptyDraft = { name: "", role: "", provider_id: "", model: "", workspace: "" };
const fields = [["name", "成员名称"], ["role", "成员职责"], ["model", "模型（可选）"], ["workspace", "工作目录（可选）"]] as const;

function useCatalogue(enabled: boolean) {
  const [request, setRequest] = useState({ before: "", version: 0 });
  const [state, setState] = useState({ items: [] as Provider[], pending: true, error: "", next: "" });
  useEffect(() => {
    if (!enabled) return;
    let ignore = false;
    const controller = new AbortController();
    setState(previous => ({ ...previous, pending: true, error: "" }));
    listProviders(request.before, controller.signal).then(result => {
      if (!ignore) setState(previous => ({ items: unique([...(request.before ? previous.items : []), ...result.items]),
        pending: false, error: "", next: result.next_cursor }));
    }, error => {
      if (!ignore) setState(previous => ({ ...previous, pending: false, error: errorText(error) }));
    });
    return () => { ignore = true; controller.abort(); };
  }, [enabled, request]);
  return { ...state, load: (before = request.before) => setRequest(previous => ({ before, version: previous.version + 1 })) };
}

type MemberListener = (groupId: string, members: Member[]) => void;
export function Members({ groupId, onMembers }: { groupId?: string; onMembers?: MemberListener }) {
  const [visited, setVisited] = useState<string[]>([]);
  useEffect(() => {
    if (groupId) setVisited(previous => previous.includes(groupId) ? previous : [...previous, groupId]);
  }, [groupId]);
  const catalogue = useCatalogue(visited.length > 0);
  return <>
    {!groupId ? <p className="empty-panel">请先选择群组，再查看成员。</p> : null}
    {/* Keep per-group operations alive across switches; hidden scopes cannot leak into the active UI. */}
    {visited.map(id => <div key={id} hidden={id !== groupId}>
      <MemberScope groupId={id} active={id === groupId} catalogue={catalogue} onMembers={onMembers} />
    </div>)}
  </>;
}

function MemberScope({ groupId, active, catalogue, onMembers }: { groupId: string; active: boolean; catalogue: ReturnType<typeof useCatalogue>; onMembers?: MemberListener }) {
  const formId = useId();
  const [items, setItems] = useState<Member[]>([]);
  useEffect(() => { onMembers?.(groupId, items); }, [groupId, items, onMembers]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const selected = items.find(member => member.id === selectedId);
  const [draft, setDraft] = useState(emptyDraft);
  const [request, setRequest] = useState({ before: "", version: 0 });
  const [page, setPage] = useState({ pending: true, error: "", next: "" });
  const [mutation, setMutation] = useState({ pending: false, error: "", uncertain: false });
  const confirmed = useRef<{ member: Member; afterRead: number }[]>([]);
  const readSequence = useRef(0);
  const operation = useRef<{ key: string; input: MemberInput } | null>(null);
  const posting = useRef<AbortController | null>(null);
  const mounted = useRef(false);
  const load = (before = "") => setRequest(previous => ({ before, version: previous.version + 1 }));
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
    listMembers(groupId, request.before, controller.signal).then(result => {
      if (ignore) return;
      // A receipt only outranks reads already started when it arrived.
      confirmed.current = confirmed.current.filter(receipt => receipt.afterRead >= read);
      const protectedMembers = confirmed.current.map(receipt => receipt.member);
      setItems(previous => {
        const updates = new Map(result.items.map(member => [member.id, member]));
        const retained = request.before ? previous.map(member => updates.get(member.id) ?? member) : [];
        return unique([...protectedMembers, ...retained, ...result.items]);
      });
      setPage({ pending: false, error: "", next: result.next_cursor });
    }, error => {
      if (!ignore) setPage(previous => ({ ...previous, pending: false, error: errorText(error) }));
    });
    return () => { ignore = true; controller.abort(); };
  }, [active, groupId, request]);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (posting.current) return;
    if (!operation.current && (!draft.name.trim() || !draft.role.trim())) {
      setMutation({ pending: false, uncertain: false, error: "请输入成员名称和职责。" }); return;
    }
    operation.current ??= { key: crypto.randomUUID(), input: { name: draft.name.trim(), role: draft.role.trim(),
      ...(draft.provider_id ? { provider_id: draft.provider_id } : {}), ...(draft.model ? { model: draft.model } : {}),
      ...(draft.workspace ? { workspace: draft.workspace } : {}) } };
    const current = operation.current;
    const controller = new AbortController(); posting.current = controller;
    setMutation({ pending: true, error: "", uncertain: false });
    try {
      const member = await createMember(groupId, current.input, current.key, controller.signal);
      if (!mounted.current) return;
      confirmed.current = [{ member, afterRead: readSequence.current }, ...confirmed.current.filter(receipt => receipt.member.id !== member.id)];
      setItems(previous => unique([member, ...previous])); setSelectedId(member.id);
      operation.current = null; setDraft(emptyDraft);
      setMutation({ pending: false, error: "", uncertain: false });
    } catch (error) {
      if (!mounted.current) return;
      const uncertain = !isDefinitiveRejection(error);
      if (!uncertain) operation.current = null;
      setMutation({ pending: false, uncertain, error: uncertain ? `创建结果未确认。${errorText(error)} 请重试同一次创建。` : errorText(error) });
    } finally { posting.current = null; }
  }

  return <div className="group-panel">
    <button type="button" onClick={() => load()}>刷新成员</button>
    <div aria-busy={page.pending}>
      {page.pending ? <p role="status">正在载入成员…</p> : null}
      {page.error ? <div role="alert"><p>{page.error}</p><button type="button" onClick={() => load(request.before)}>重试载入成员</button></div> : null}
      {!page.pending && !page.error && !items.length ? <p>尚无成员，可创建群内成员。</p> : null}
      <ul className="group-list" aria-label="成员列表">{items.map(member => <li key={member.id}>
        <button type="button" aria-pressed={selectedId === member.id} onClick={() => setSelectedId(member.id)}>{member.name}</button>
      </li>)}</ul>
      {page.next ? <button type="button" disabled={page.pending} onClick={() => load(page.next)}>加载更多成员</button> : null}
    </div>
    {selected ? <section className="empty-panel member-detail" aria-label="成员详情">
      <h3>{selected.name}</h3><p>{selected.role}</p>
      <dl><dt>群内状态</dt><dd>{selected.status === "active" ? "在群" : selected.status === "left" ? "已离开" : "状态未知"}</dd>
        <dt>运行器配置</dt><dd>{selected.provider_id ? catalogue.items.find(provider => provider.id === selected.provider_id)?.name || "已有配置，目录未验证" : "未绑定"}</dd>
        <dt>模型配置</dt><dd>{selected.model || "未指定"}</dd><dt>工作目录配置</dt><dd>{selected.workspace || "未配置"}</dd></dl>
    </section> : null}
    <form aria-label="创建成员" onSubmit={submit}>
      <fieldset disabled={mutation.pending || mutation.uncertain}>
        <legend>创建成员</legend>
        {fields.map(([key, label]) => <div key={key}><label htmlFor={`${formId}-${key}`}>{label}</label>
          <input id={`${formId}-${key}`} required={key === "name" || key === "role"} value={draft[key]}
            onChange={event => setDraft(previous => ({ ...previous, [key]: event.target.value }))} /></div>)}
        <label htmlFor={`${formId}-provider`}>运行器（可选）</label>
        <select id={`${formId}-provider`} value={draft.provider_id} aria-describedby={`${formId}-runtime-help`}
          onChange={event => setDraft(previous => ({ ...previous, provider_id: event.target.value }))}>
          <option value="">暂不绑定运行器</option>
          {catalogue.items.map(provider => <option key={provider.id} value={provider.id}>{providerLabel(provider)}</option>)}
        </select>
        {catalogue.pending ? <p role="status">正在载入运行器目录…</p> : catalogue.error
          ? <div role="status">运行器目录暂不可用，未检测。{catalogue.error}<button type="button" onClick={() => catalogue.load()}>重试运行器目录</button></div>
          : !catalogue.items.length ? <p>未检测到运行器条目。</p> : null}
        {catalogue.next ? <button type="button" disabled={catalogue.pending} onClick={() => catalogue.load(catalogue.next)}>加载更多运行器</button> : null}
      </fieldset>
      <p id={`${formId}-runtime-help`}>未检测连接状态；目录账户状态与运行器、模型配置不代表运行就绪。</p>
      <p>工作目录配置不代表已授予读写权限。</p>
      {mutation.error ? <p role="alert">{mutation.error}</p> : null}
      <button type="submit" disabled={mutation.pending}>{mutation.pending ? "正在创建成员…" : mutation.uncertain ? "重试同一次成员创建" : "创建成员"}</button>
    </form>
  </div>;
}
