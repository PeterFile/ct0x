import { useEffect, useId, useRef, useState } from "react";
import type { FormEvent } from "react";
import { ApiError, isDefinitiveRejection, sendMessage } from "./api";
import type { Delivery, Member, MessageInput } from "./api";
import { RequestWork } from "./RequestWork";
import type { RequestWorkHandle } from "./RequestWork";
import { useMessageFeed } from "./useMessageFeed";
import { useMessageScroll } from "./useMessageScroll";
import "./discussion-presentation.css";
const emptyMembers: Member[] = [];
const deliveryLabel = (state: string) => state === "queued" ? "已入队" : state === "accepted" ? "CLI已接收" : state === "failed" ? "投递失败" : "状态未知";
function deliverySummary(deliveries: Delivery[]) {
  if (!deliveries.length) return "暂无投递记录";
  const counts = new Map<string, number>();
  let errors = 0;
  for (const delivery of deliveries) {
    const label = deliveryLabel(delivery.state);
    counts.set(label, (counts.get(label) ?? 0) + 1);
    if (delivery.error) ++errors;
  }
  const labels = [...counts].map(([label, count]) => `${label} ${count}`);
  if (errors) labels.push(`投递错误 ${errors}`);
  return labels.join(" · ");
}
function shortTime(value: string) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString("zh-CN", {
    month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false,
  });
}

export function Discussion({ groupId, members }: { groupId?: string; members: Record<string, Member[]> }) {
  const [visited, setVisited] = useState<string[]>([]);
  useEffect(() => {
    if (groupId) setVisited(previous => previous.includes(groupId) ? previous : [...previous, groupId]);
  }, [groupId]);
  return <>
    {!groupId ? <p className="empty-panel">请先选择群组，再查看讨论和发送消息。</p> : null}
    {visited.map(id => <div key={id} className="discussion-scope" hidden={id !== groupId}>
      <DiscussionScope groupId={id} active={id === groupId} members={members[id] ?? emptyMembers} />
    </div>)}
  </>;
}
function DiscussionScope({ groupId, active, members }: { groupId: string; active: boolean; members: Member[] }) {
  const formId = useId(), feed = useMessageFeed(groupId, active);
  const scroll = useMessageScroll(active, feed.items, feed.page);
  const [content, setContent] = useState("");
  const [route, setRoute] = useState("default"), [recipients, setRecipients] = useState<string[]>([]);
  const [mutation, setMutation] = useState({ pending: false, uncertain: false, error: "", notice: "" });
  const requestWork = useRef<RequestWorkHandle>(null);
  const operation = useRef<{ key: string; input: MessageInput } | null>(null);
  const posting = useRef<AbortController | null>(null), mounted = useRef(false);
  const eligible = members.filter(member => member.group_id === groupId && member.status === "active");
  const memberNames = new Map(members.filter(member => member.group_id === groupId).map(member => [member.id, member.name]));
  const authorName = (id: string) => id === "user" ? "我" : memberNames.get(id) ?? "未载入成员";
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; posting.current?.abort(); };
  }, []);
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (posting.current) return;
    const invalidRecipients = route === "selected" && (!recipients.length || recipients.some(id => !eligible.some(member => member.id === id)));
    if (!operation.current && (!content.trim() || invalidRecipients)) {
      setMutation({ pending: false, uncertain: false, notice: "", error: !content.trim() ? "请输入消息内容。" : "请选择本群已载入的在群成员，或清除选择后重选。" }); return;
    }
    operation.current ??= { key: crypto.randomUUID(), input: { content,
      ...(route === "default" ? {} : { recipient_ids: route === "public" ? [] : [...recipients] }) } };
    const current = operation.current, controller = new AbortController(); posting.current = controller;
    setMutation({ pending: true, uncertain: false, error: "", notice: "" });
    const attempt = feed.beginSend();
    scroll.beginSend(attempt);
    try {
      const receipt = await sendMessage(groupId, current.input, current.key, controller.signal);
      if (!mounted.current) return;
      feed.confirm(receipt, attempt); operation.current = null; setContent("");
      scroll.confirmSend(receipt.id, attempt);
      setMutation({ pending: false, uncertain: false, error: "", notice: "消息已持久化；投递状态见记录。" });
    } catch (error) {
      if (!mounted.current) return;
      const uncertain = !isDefinitiveRejection(error), reason = error instanceof ApiError ? error.message : "请求未完成，请重试。";
      if (!uncertain) operation.current = null;
      setMutation({ pending: false, uncertain, notice: "", error: uncertain ? `发送结果未确认，消息可能已保存。${reason} 请重试同一次发送。` : reason });
    } finally { posting.current = null; }
  }
  return <div className="group-panel discussion-feed">
    <section className="message-history" aria-label="消息记录" tabIndex={0} ref={scroll.historyRef} onScroll={scroll.onScroll}
      onWheel={scroll.onReadingIntent} onTouchMove={scroll.onReadingIntent} onKeyDown={event => {
        const target = event.target;
        if (target instanceof HTMLElement && (target.closest("input, textarea, select") || target.isContentEditable)) return;
        scroll.onKeyDown(event);
      }}
      onPointerDown={scroll.onPointerDown} onPointerMove={scroll.onPointerMove} onPointerUp={scroll.onPointerEnd}
      onPointerCancel={scroll.onPointerEnd} onPointerLeave={scroll.onPointerEnd}>
    <div className="discussion-tools">
      <button type="button" disabled={feed.page.pending} onClick={() => { scroll.beforeReset(); void feed.loadPage(); }}>刷新并重置历史窗口</button>
      <a href={`#${formId}-content`}>跳至消息输入</a>
      <span className="feed-note">自动更新</span>
    </div>
    <p className="feed-note">已载入 {feed.items.length} 条（含话题，片段可能有间隔）。</p>
    {feed.pollError ? <p role="status">自动刷新暂不可用：{feed.pollError} 将稍后重试。</p> : null}
    {feed.page.pending ? <p role="status">正在载入消息…</p> : null}
    {feed.page.error ? <div role="alert"><p>{feed.page.error}</p><button type="button" onClick={feed.retry}>重试载入消息</button></div> : null}
    {!feed.page.pending && !feed.page.error && !feed.items.length ? <p>尚无消息，可发送第一条讨论。</p> : null}
    {feed.page.next ? <button type="button" disabled={feed.page.pending} onClick={() => { scroll.beforeOlder(); void feed.loadPage(feed.page.next, false); }}>加载更早消息</button> : null}
    <RequestWork groupId={groupId} active={active} members={members} ref={requestWork} />
    <ol className="message-list" aria-label="群组消息" ref={scroll.listRef}>{feed.items.map(message => <li key={message.id} data-message-id={message.id}>
      <article className={message.sender_id === "user" ? "message-local" : undefined} aria-label={`消息 ${message.id}`}>
        <header className="message-meta"><strong>{authorName(message.sender_id)}</strong><time dateTime={message.created_at}>{shortTime(message.created_at)}</time></header>
        {message.topic_id ? <p className="eyebrow accent">话题消息</p> : null}
        <p className="message-content">{message.content}</p>
        <div className="message-actions">
        <button className="message-task-action" type="button" aria-label={`转为任务，消息 ${message.id}`}
          onClick={event => requestWork.current?.open(message, event.currentTarget)}>转为任务</button>
        <details className="message-details">
          <summary className={message.deliveries.some(delivery => !["queued", "accepted"].includes(delivery.state) || Boolean(delivery.error)) ? "delivery-warning" : undefined}>
            投递详情 · {deliverySummary(message.deliveries)}
          </summary>
          <dl className="message-technical">
            <div><dt>消息 ID</dt><dd>{message.id}</dd></div>
            <div><dt>发送者 ID</dt><dd>{message.sender_id}</dd></div>
            {message.topic_id ? <div><dt>话题 ID</dt><dd>{message.topic_id}</dd></div> : null}
            <div><dt>完整时间</dt><dd><time dateTime={message.created_at}>{message.created_at}</time></dd></div>
          </dl>
          {message.deliveries.length ? <ul className="delivery-list">{message.deliveries.map(delivery => <li key={delivery.id}>
            <p><strong>{authorName(delivery.recipient_id)}</strong> · <span>{delivery.recipient_id}</span></p>
            <p>{deliveryLabel(delivery.state)}（{delivery.state}）</p>
            {delivery.error ? <p className="delivery-warning">投递错误：{delivery.error}</p> : null}
            <p>投递 ID：{delivery.id}</p>
          </li>)}</ul> : null}
          <button type="button" aria-label={`刷新消息 ${message.id} 投递`} disabled={feed.details[message.id]?.pending}
            onClick={() => feed.refreshDetail(message.id)}>刷新投递</button>
        </details>
        </div>
        {feed.details[message.id]?.error ? <p role="alert">{feed.details[message.id].error}</p> : null}
      </article>
    </li>)}</ol>
    </section>
    <section className="message-composer" aria-label="撰写消息" tabIndex={0}>
    <form aria-label="发送消息" onSubmit={submit}>
      {scroll.windowReset ? <p className="feed-note">历史窗口已更新，可加载更早消息</p> : null}
      <fieldset disabled={mutation.pending || mutation.uncertain}>
        <label htmlFor={`${formId}-content`}>消息内容</label>
        <textarea id={`${formId}-content`} required rows={2} value={content} onChange={event => setContent(event.target.value)} />
      </fieldset>
      <div className="composer-actions">
        <fieldset disabled={mutation.pending || mutation.uncertain}>
          <label htmlFor={`${formId}-route`}>接收方式</label>
          <select id={`${formId}-route`} value={route} onChange={event => setRoute(event.target.value)}>
            <option value="default">默认路由（由服务决定）</option><option value="public">仅公开可见（不唤醒）</option><option value="selected">指定本群成员</option>
          </select>
        </fieldset>
        <button className="primary-send" type="submit" disabled={mutation.pending}>{mutation.pending ? "正在发送…" : mutation.uncertain ? "重试同一次发送" : "发送消息"}</button>
      </div>
      {route === "selected" ? <fieldset disabled={mutation.pending || mutation.uncertain}>
        <div className="recipient-list">
          <p className="feed-note">仅列出已载入的在群成员；更多成员请打开成员详情加载。</p>
          {eligible.map(member => <label key={member.id}><input type="checkbox" checked={recipients.includes(member.id)}
            onChange={event => setRecipients(previous => event.target.checked ? [...previous, member.id] : previous.filter(id => id !== member.id))} />
            <span>{member.name} · {member.role} · {member.id}</span></label>)}
          <button type="button" onClick={() => setRecipients([])}>清除所选成员</button>
        </div>
      </fieldset> : null}
      {content.length > 0 || mutation.uncertain ? <p className="feed-note">草稿与未确认发送仅保留在本页，重载会丢失。</p> : null}
      {mutation.error ? <p role="alert">{mutation.error}</p> : null}
      {mutation.notice ? <p role="status">{mutation.notice}</p> : null}
    </form>
    </section>
  </div>;
}
