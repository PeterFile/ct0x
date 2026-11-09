import { useEffect, useId, useRef, useState } from "react";
import type { FormEvent } from "react";
import { ApiError, isDefinitiveRejection, listProviders } from "./api";
import type { Provider } from "./api";
import { getReceptionDefaults, receptionDefaultsInputError, setReceptionDefaults } from "./reception-defaults-api";
import type { ReceptionConfiguration, ReceptionDefaultsInput, ReceptionDefaultsState } from "./reception-defaults-api";
import "./ReceptionDefaults.css";

type Draft = Pick<ReceptionDefaultsInput, "provider_id" | "model" | "role">;
const emptyDraft: Draft = { provider_id: "", model: "", role: "参与当前群组的讨论，清楚区分建议、待办与已确认决定。" };
const draftOf = (configuration: ReceptionConfiguration | null): Draft => configuration
  ? { provider_id: configuration.provider_id, model: configuration.model, role: configuration.role } : emptyDraft;
const errorText = (error: unknown) => error instanceof ApiError ? error.message : "请求未完成，请重试。";
const providerLabel = (provider: Provider) => `${provider.name} · ${provider.version || "版本未知"} · ${provider.path || "路径未知"} · ${
  provider.account === "authenticated" ? "账户已登录（运行未验证）" : provider.account === "unauthenticated" ? "未登录" : "账户未知（可手动选择）"}`;

function useProviderDirectory() {
  const [request, setRequest] = useState({ before: "", version: 0 });
  const [state, setState] = useState({ items: [] as Provider[], pending: true, error: "", next: "" });
  useEffect(() => {
    let ignore = false;
    const controller = new AbortController();
    setState(previous => ({ ...previous, pending: true, error: "" }));
    listProviders(request.before, controller.signal).then(result => {
      if (ignore) return;
      setState(previous => ({ items: [...new Map([...previous.items, ...result.items].map(item => [item.id, item])).values()],
        pending: false, error: "", next: result.next_cursor }));
    }, () => {
      if (!ignore) setState(previous => ({ ...previous, pending: false, error: "运行器目录暂不可用，请重试载入。" }));
    });
    return () => { ignore = true; controller.abort(); };
  }, [request]);
  return { ...state, load: (before = request.before) => setRequest(previous => ({ before, version: previous.version + 1 })) };
}

export function ReceptionDefaults() {
  const id = useId(), directory = useProviderDirectory();
  const [draft, setDraft] = useState<Draft>(emptyDraft);
  const dirty = useRef(false);
  const [saved, setSaved] = useState<ReceptionConfiguration | null>(null);
  const known = useRef<ReceptionConfiguration | null>(null);
  const [environment, setEnvironment] = useState<ReceptionDefaultsState | null>(null);
  const [readRequest, setReadRequest] = useState({ version: 0, review: false });
  const [read, setRead] = useState({ pending: true, ready: false, error: "" });
  const hasRead = useRef(false);
  const [mutation, setMutation] = useState({ pending: false, uncertain: false, error: "" });
  const [revisionConflict, setRevisionConflict] = useState(false);
  const [notice, setNotice] = useState("");
  const operation = useRef<Readonly<{ key: string; input: ReceptionDefaultsInput }> | null>(null);
  const posting = useRef<AbortController | null>(null);
  const mounted = useRef(false);
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; posting.current?.abort(); };
  }, []);
  useEffect(() => {
    let ignore = false;
    const controller = new AbortController();
    setRead(previous => ({ ...previous, pending: true, error: "" }));
    getReceptionDefaults(controller.signal).then(result => {
      if (ignore) return;
      hasRead.current = true;
      setRead({ pending: false, ready: true, error: "" });
      // Immutable revisions, not arrival time, determine which snapshot is current.
      if ((result.configuration?.revision ?? 0) < (known.current?.revision ?? 0)) {
        setNotice("已忽略较旧的设置版本，保留当前已知设置；可再次刷新确认。"); return;
      }
      known.current = result.configuration;
      setSaved(result.configuration); setEnvironment(result);
      if (!dirty.current && !operation.current) setDraft(draftOf(result.configuration));
      // A read started before the conflict cannot implicitly rebase a rejected save.
      if (readRequest.review) {
        setRevisionConflict(false); setMutation(previous => ({ ...previous, error: "" }));
        setNotice("已载入最新设置，草稿未变。请对照已保存内容，确认后再保存。");
      } else if (dirty.current && !operation.current) {
        setNotice("已刷新已保存设置，草稿未变。请对照已保存内容，确认后再保存。");
      }
    }, error => {
      if (!ignore) setRead(previous => ({ ...previous, pending: false, error: errorText(error) }));
    });
    return () => { ignore = true; controller.abort(); };
  }, [readRequest]);
  function refresh() {
    setReadRequest(previous => ({ version: previous.version + 1, review: revisionConflict }));
  }
  function change(field: keyof Draft, value: string) {
    if (operation.current) return;
    dirty.current = true; setDraft(previous => ({ ...previous, [field]: value }));
  }
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (posting.current) return;
    if (!operation.current) {
      if (revisionConflict || !hasRead.current) return;
      const input = { ...draft, expected_revision: known.current?.revision ?? 0 };
      const error = receptionDefaultsInputError(input) || (!directory.items.some(item => item.id === input.provider_id)
        ? "请从已载入目录中选择运行器；已保存但未载入的运行器不能直接再次保存。" : "");
      if (error) { setMutation({ pending: false, uncertain: false, error }); return; }
      dirty.current = true;
      operation.current = Object.freeze({ key: crypto.randomUUID(), input: Object.freeze(input) });
    }
    const current = operation.current;
    const controller = new AbortController(); posting.current = controller;
    setMutation({ pending: true, uncertain: false, error: "" }); setNotice("");
    try {
      const receipt = await setReceptionDefaults(current.input, current.key, controller.signal);
      if (!mounted.current) return;
      const superseded = receipt.revision < (known.current?.revision ?? 0);
      if (!superseded) {
        known.current = receipt; setSaved(receipt); setDraft(draftOf(receipt)); dirty.current = false;
      }
      operation.current = null; setRevisionConflict(false);
      setMutation({ pending: false, uncertain: false, error: "" });
      setNotice(superseded ? `本次保存已确认（版本 ${receipt.revision}），但已有更新的设置；已保留草稿，请对照当前设置后再保存。`
        : "已保存默认接待设置；不会修改已有群组成员。");
    } catch (error) {
      if (!mounted.current) return;
      const uncertain = mutation.uncertain || !isDefinitiveRejection(error);
      if (!uncertain) operation.current = null;
      if (!uncertain && error instanceof ApiError && error.status === 409 && error.code === "reception_revision_conflict") setRevisionConflict(true);
      setMutation({ pending: false, uncertain, error: uncertain
        ? `保存结果未确认。${errorText(error)} 请重试同一次保存。` : errorText(error) });
    } finally { if (posting.current === controller) posting.current = null; }
  }

  const loadedSelection = directory.items.some(item => item.id === draft.provider_id);
  const savedMissing = saved && !directory.items.some(item => item.id === saved.provider_id);
  const availability = environment && (environment.configuration?.revision ?? 0) === (saved?.revision ?? 0) ? environment.availability.code : null;
  return <section className="reception-defaults" aria-label="默认接待设置">
    <h2>默认接待设置</h2>
    <p>设置仅用于未来需要回应、且从未有成员的空群；不会替换已有成员，也不会唤醒所有成员。</p>
    <p>接待助手由本地用户创建，使用平台管理的工作目录；保存不会授予外部目录的读写权限。</p>
    <p role="status">{environment ? environment.execution_enabled
      ? "执行已启用；能否运行仍以实际执行结果为准。"
      : "执行未启用：设置仍可保存，消息可以排队，但不会开始执行。" : "执行状态尚未载入。"}</p>
    <button type="button" onClick={refresh} disabled={read.pending}>
      {revisionConflict ? "载入最新设置并保留草稿" : "刷新已保存设置"}
    </button>
    {read.pending ? <p role="status">正在载入默认接待设置…</p> : null}
    {read.error ? <div role="alert"><p>{read.error}</p><button type="button" onClick={refresh}>重试载入设置</button></div> : null}
    <section className="reception-defaults-saved" aria-label="已保存设置" aria-busy={read.pending}>
      <h3>已保存设置</h3>
      {saved ? <><p>版本 {saved.revision}</p><dl>
        <dt>运行器标识</dt><dd>{saved.provider_id}</dd><dt>模型</dt><dd>{saved.model}</dd><dt>职责</dt><dd>{saved.role}</dd>
      </dl></> : <p>{read.ready ? "尚未配置默认接待者。" : "尚未载入已保存设置。"}</p>}
      {availability === "configured" ? <p>已保存默认设置；运行器支持尝试，账户与模型可用性以实际执行为准。</p>
        : availability === "provider_unavailable" ? <p role="status">已保存的运行器当前不受支持，请重新选择运行器；不会自动回退。</p>
        : saved && !availability ? <p>已保存快照；运行器支持情况尚未刷新，账户与模型可用性仍需实际执行确认。</p> : null}
      {savedMissing ? <p>已保存的运行器不在已载入目录中：{saved.provider_id}。不会自动更换；可继续载入目录或手动重新选择。</p> : null}
    </section>
    <form aria-label="默认接待设置" onSubmit={submit}>
      <fieldset disabled={mutation.pending || mutation.uncertain}>
        <legend>编辑默认接待设置</legend>
        <div><label htmlFor={`${id}-provider`}>运行器</label>
          <select id={`${id}-provider`} value={draft.provider_id} required aria-describedby={`${id}-runtime-help`}
            onChange={event => change("provider_id", event.target.value)}>
            <option value="">请选择运行器</option>
            {draft.provider_id && !loadedSelection ? <option value={draft.provider_id} disabled>当前选择（目录未载入）：{draft.provider_id}</option> : null}
            {directory.items.map(item => <option key={item.id} value={item.id}>{providerLabel(item)}</option>)}
          </select></div>
        <div><label htmlFor={`${id}-model`}>模型</label>
          <textarea id={`${id}-model`} required rows={1} value={draft.model} aria-describedby={`${id}-model-help`}
            onChange={event => change("model", event.target.value)} />
          <p id={`${id}-model-help`}>请显式填写模型，最多 256 个 UTF-8 字节，不含 NUL、回车或换行；不会自动猜测。</p></div>
        <div><label htmlFor={`${id}-role`}>职责</label>
          <textarea id={`${id}-role`} required rows={5} value={draft.role} aria-describedby={`${id}-role-help`}
            onChange={event => change("role", event.target.value)} />
          <p id={`${id}-role-help`}>最多 4096 个 UTF-8 字节；预填职责可编辑，点击保存后才生效。</p></div>
      </fieldset>
      <div aria-label="运行器目录" aria-busy={directory.pending}>
        {directory.pending ? <p role="status">正在载入运行器目录…</p> : null}
        {directory.error ? <div role="alert"><p>{directory.error}</p>
          <button type="button" onClick={() => directory.load()}>重试运行器目录</button></div> : null}
        {!directory.pending && !directory.error && !directory.items.length ? <p>未检测到运行器条目。</p> : null}
        {directory.next ? <button type="button" disabled={directory.pending || !!directory.error}
          onClick={() => directory.load(directory.next)}>加载更多运行器</button> : null}
      </div>
      <p id={`${id}-runtime-help`}>安装、账户状态和模型填写都不保证可运行。账户未知仍可手动选择；支持范围由后台决定，以实际执行结果为准。</p>
      {mutation.error ? <p role="alert">{mutation.error}</p> : null}
      {notice ? <p role="status">{notice}</p> : null}
      <button type="submit" disabled={mutation.pending || !read.ready || revisionConflict}>
        {mutation.pending ? "正在保存…" : mutation.uncertain ? "重试同一次保存" : "保存默认接待设置"}
      </button>
      <p className="reception-defaults-note">未确认的保存仅在本页面存续期间保留；离开或刷新页面前，请先重试确认。</p>
    </form>
  </section>;
}
