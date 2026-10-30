import { useEffect, useRef, useState } from "react";
import type { FormEvent } from "react";
import { ApiError, createGroup, listGroups } from "./api";
import type { Group, GroupInput } from "./api";

function unique(groups: Group[]) {
  const seen = new Set<string>();
  return groups.filter(group => {
    if (seen.has(group.id)) return false;
    seen.add(group.id);
    return true;
  });
}
const errorText = (error: unknown) => error instanceof ApiError ? error.message : "请求未完成，请重试。";

export function Groups({ selectedId, onSelect }: { selectedId?: string; onSelect: (group: Group) => void }) {
  const [groups, setGroups] = useState<Group[]>([]);
  const [request, setRequest] = useState({ before: "", version: 0 });
  const [page, setPage] = useState({ pending: true, error: "", next: "" });
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [mutation, setMutation] = useState({ pending: false, error: "", uncertain: false });
  const operation = useRef<{ key: string; input: GroupInput } | null>(null);
  const confirmed = useRef<Group[]>([]);
  const posting = useRef<AbortController | null>(null);
  const mounted = useRef(false);
  const load = (before = "") => setRequest(previous => ({ before, version: previous.version + 1 }));

  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; posting.current?.abort(); };
  }, []);
  useEffect(() => {
    let ignore = false;
    const controller = new AbortController();
    setPage(previous => ({ ...previous, pending: true, error: "" }));
    listGroups(request.before, controller.signal).then(result => {
      if (ignore) return;
      // Confirmed creations outrank reads that may have started before the POST.
      setGroups(previous => unique([...confirmed.current, ...(request.before ? previous : []), ...result.items]));
      setPage({ pending: false, error: "", next: result.next_cursor });
    }, error => {
      if (!ignore) setPage(previous => ({ ...previous, pending: false, error: errorText(error) }));
    });
    return () => { ignore = true; controller.abort(); };
  }, [request]);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (posting.current) return;
    if (!operation.current && !name.trim()) {
      setMutation({ pending: false, error: "请输入群组名称。", uncertain: false });
      return;
    }
    operation.current ??= { key: crypto.randomUUID(), input: { name: name.trim(), ...(description ? { description } : {}) } };
    const current = operation.current;
    const controller = new AbortController();
    posting.current = controller;
    setMutation({ pending: true, error: "", uncertain: false });
    try {
      const group = await createGroup(current.input, current.key, controller.signal);
      if (!mounted.current) return;
      confirmed.current = unique([group, ...confirmed.current]);
      setGroups(previous => unique([group, ...previous]));
      onSelect(group);
      operation.current = null;
      setName("");
      setDescription("");
      setMutation({ pending: false, error: "", uncertain: false });
    } catch (error) {
      if (!mounted.current) return;
      const uncertain = !(error instanceof ApiError && [400, 404, 409].includes(error.status));
      if (!uncertain) operation.current = null;
      setMutation({ pending: false, uncertain, error: uncertain
        ? `创建结果未确认。${errorText(error)} 请重试同一次创建。` : errorText(error) });
    } finally { posting.current = null; }
  }

  return (
    <div className="group-panel">
      <button type="button" onClick={() => load()}>刷新群组</button>
      <div aria-busy={page.pending}>
        {page.pending ? <p role="status">正在载入群组…</p> : null}
        {page.error ? <div role="alert"><p>{page.error}</p>
          <button type="button" onClick={() => load(request.before)}>重试载入</button></div> : null}
        {!page.pending && !page.error && groups.length === 0 ? <p>尚无群组，可在下方创建。</p> : null}
        <ul className="group-list" aria-label="群组列表">
          {groups.map(group => <li key={group.id}>
            <button type="button" aria-pressed={selectedId === group.id} onClick={() => onSelect(group)}>{group.name}</button>
          </li>)}
        </ul>
        {page.next ? <button type="button" disabled={page.pending} onClick={() => load(page.next)}>加载更多群组</button> : null}
      </div>
      <form aria-label="创建群组" onSubmit={submit}>
        <fieldset disabled={mutation.pending || mutation.uncertain}>
          <legend>创建群组</legend>
          <label htmlFor="group-name">群组名称</label>
          <input id="group-name" required value={name} onChange={event => setName(event.target.value)} />
          <label htmlFor="group-description">群组说明（可选）</label>
          <textarea id="group-description" rows={3} value={description} onChange={event => setDescription(event.target.value)} />
        </fieldset>
        {mutation.error ? <p role="alert">{mutation.error}</p> : null}
        <button type="submit" disabled={mutation.pending}>
          {mutation.pending ? "正在创建…" : mutation.uncertain ? "重试同一次创建" : "创建群组"}
        </button>
      </form>
    </div>
  );
}
