import { useEffect, useId, useState } from "react";
import type { FormEvent } from "react";
import { ApiError } from "./api";
import type { Group, Message } from "./api";
import { readHistory, searchHistory } from "./history-api";
import type { HistoryQuery } from "./history-api";
import "./history-search.css";

const errorText = (error: unknown) => error instanceof ApiError ? error.message : "请求未完成，请重试。";
function excerpt(content: string) {
  let text = "", length = 0;
  for (const character of content) {
    if (length === 240) return `${text}…`;
    text += character; length += 1;
  }
  return text;
}
function mergeMessages(previous: Message[], incoming: Message[]) {
  const seen = new Set<string>();
  return [...previous, ...incoming].filter(message => {
    if (seen.has(message.id)) return false;
    seen.add(message.id); return true;
  });
}

export function HistorySearch({ group }: { group: Group | null }) {
  return <section className="history-search" aria-label="历史消息搜索">
    <header className="history-search-heading"><h2>历史消息</h2><span>只读检索</span></header>
    <p className="history-search-note">搜索和读取原始记录，不会发送消息或启动执行。</p>
    {group ? <SearchScope key={group.id} groupId={group.id} /> : <p className="history-search-note">请先选择群组，再搜索历史消息。</p>}
  </section>;
}

function SearchScope({ groupId }: { groupId: string }) {
  const formId = useId();
  const [keyword, setKeyword] = useState("");
  const [scope, setScope] = useState("all");
  const [topicId, setTopicId] = useState("");
  const [error, setError] = useState("");
  const [submitted, setSubmitted] = useState<{ query: HistoryQuery; version: number } | null>(null);
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!keyword.trim()) { setError("请输入关键词。"); return; }
    if (scope === "topic" && topicId === "") { setError("请输入话题 ID；主频道请从检索范围中选择。"); return; }
    setError("");
    setSubmitted(previous => ({ query: { q: keyword, topic_id: scope === "all" ? undefined : scope === "main" ? "" : topicId },
      version: (previous?.version ?? 0) + 1 }));
  }
  return <>
    <form className="history-search-form" aria-label="搜索历史消息" noValidate onSubmit={submit}>
      <div className="history-search-fields">
        <label htmlFor={`${formId}-keyword`}>关键词
          <input id={`${formId}-keyword`} required value={keyword} onChange={event => setKeyword(event.target.value)} />
        </label>
        <label htmlFor={`${formId}-scope`}>检索范围
          <select id={`${formId}-scope`} value={scope} onChange={event => setScope(event.target.value)}>
            <option value="all">整个群组</option><option value="main">主频道</option><option value="topic">指定话题 ID</option>
          </select>
        </label>
        {scope === "topic" ? <label htmlFor={`${formId}-topic`}>话题 ID
          <input id={`${formId}-topic`} required aria-describedby={`${formId}-topic-note`} value={topicId} onChange={event => setTopicId(event.target.value)} />
        </label> : null}
      </div>
      {scope === "topic" ? <p id={`${formId}-topic-note`} className="history-search-note">按原始 ID 精确匹配，保留空白。</p> : null}
      {error ? <p role="alert">{error}</p> : null}
      <button className="history-search-submit" type="submit">搜索历史消息</button>
    </form>
    {/* Each submission owns its cursors, results and detail, independently of the draft. */}
    {submitted ? <SearchResults key={submitted.version} groupId={groupId} query={submitted.query} />
      : <p className="history-search-note">输入关键词并提交搜索。</p>}
  </>;
}

function SearchResults({ groupId, query: { q, topic_id: topicId } }: { groupId: string; query: HistoryQuery }) {
  const detailId = useId();
  const [items, setItems] = useState<Message[]>([]);
  const [request, setRequest] = useState({ before: "", version: 0 });
  const [page, setPage] = useState({ pending: true, error: "", next: "" });
  const [selection, setSelection] = useState<{ source: Message; version: number } | null>(null);
  const load = (before: string) => setRequest(previous => ({ before, version: previous.version + 1 }));
  useEffect(() => {
    let ignore = false;
    const controller = new AbortController();
    setPage(previous => ({ ...previous, pending: true, error: "" }));
    searchHistory(groupId, { q, topic_id: topicId, before: request.before }, controller.signal).then(result => {
      if (ignore) return;
      setItems(previous => mergeMessages(request.before ? previous : [], result.items));
      setPage({ pending: false, error: "", next: result.next_cursor });
    }, error => {
      if (!ignore) setPage(previous => ({ ...previous, pending: false, error: errorText(error) }));
    });
    return () => { ignore = true; controller.abort(); };
  }, [groupId, q, topicId, request]);
  return <section className="history-search-results" aria-label="历史搜索结果">
    <h3>搜索结果</h3>
    <ul className="history-search-summary" aria-label="已提交的检索条件">
      <li>关键词：<span className="history-search-copy">{q}</span></li>
      <li>检索范围：{topicId === undefined ? "整个群组" : topicId === "" ? "主频道" : <>话题 ID「<code className="history-search-copy">{topicId}</code>」</>}</li>
    </ul>
    <div className="history-search-pages" aria-busy={page.pending}>
      <p className="history-search-note">已载入 {items.length} 条 · 从新到旧</p>
      {page.pending ? <p role="status">正在搜索历史消息…</p> : null}
      {page.error ? <div role="alert"><p>{page.error}</p><button type="button" onClick={() => load(request.before)}>重试搜索</button></div> : null}
      {!page.pending && !page.error && !items.length ? <p className="history-search-note">未找到匹配的历史消息。</p> : null}
      <ol className="history-search-list" aria-label="历史消息结果">{items.map(message => <li key={message.id}>
        <article aria-label={`搜索结果 ${message.id}`}>
          <MessageSource message={message} />
          <p className="history-search-note">消息摘录</p>
          <p className="history-search-copy">{excerpt(message.content)}</p>
          <button type="button" aria-label={`读取完整消息 ${message.id}`} aria-pressed={selection?.source.id === message.id}
            aria-controls={detailId} onClick={() => setSelection(previous => ({ source: message, version: (previous?.version ?? 0) + 1 }))}>读取完整消息</button>
        </article>
      </li>)}</ol>
      {page.next && !page.error ? <button type="button" disabled={page.pending} onClick={() => load(page.next)}>加载更早结果</button> : null}
    </div>
    {selection ? <HistoryDetail key={selection.version} source={selection.source} id={detailId} />
      : <p className="history-search-note">选择一条结果，读取完整消息。</p>}
  </section>;
}

function MessageSource({ message }: { message: Message }) {
  return <>
    <dl className="history-search-source">
      <dt>原始消息 ID</dt><dd><code>{message.id}</code></dd>
      <dt>群组 ID</dt><dd><code>{message.group_id}</code></dd>
      <dt>来源</dt><dd>{message.topic_id === "" ? "主频道" : <>话题 ID「<code className="history-search-copy">{message.topic_id}</code>」</>}</dd>
    </dl>
    <p className="history-search-meta"><span>发送者：{message.sender_id}</span><time dateTime={message.created_at}>{message.created_at}</time></p>
  </>;
}

function HistoryDetail({ source, id }: { source: Message; id: string }) {
  const { group_id: groupId, topic_id: topicId, id: messageId } = source;
  const [version, setVersion] = useState(0);
  const [detail, setDetail] = useState<{ message: Message | null; pending: boolean; error: string }>({ message: null, pending: true, error: "" });
  useEffect(() => {
    let ignore = false;
    const controller = new AbortController();
    setDetail({ message: null, pending: true, error: "" });
    readHistory(groupId, messageId, topicId, controller.signal).then(message => {
      if (!ignore) setDetail({ message, pending: false, error: "" });
    }, error => {
      if (!ignore) setDetail({ message: null, pending: false, error: errorText(error) });
    });
    return () => { ignore = true; controller.abort(); };
  }, [groupId, topicId, messageId, version]);
  return <section className="history-search-detail" id={id} aria-label="完整消息" aria-busy={detail.pending}>
    <h3>完整消息</h3>
    <MessageSource message={detail.message ?? source} />
    {detail.pending ? <p role="status">正在读取完整消息…</p> : null}
    {detail.error ? <div role="alert"><p>{detail.error}</p><button type="button" onClick={() => setVersion(previous => previous + 1)}>重试读取完整消息</button></div> : null}
    {detail.message ? <p className="history-search-copy">{detail.message.content}</p> : null}
  </section>;
}
