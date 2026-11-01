import { useEffect, useId, useRef, useState } from "react";
import type { FormEvent } from "react";
import { ApiError, isDefinitiveRejection, sendMessage } from "./api";
import type { Member, MessageInput } from "./api";
import { useMessageFeed } from "./useMessageFeed";
const emptyMembers: Member[] = [];
const deliveryLabel = (state: string) => state === "queued" ? "已入队" : state === "accepted" ? "CLI已接收" : state === "failed" ? "投递失败" : "状态未知";

export function Discussion({ groupId, members }: { groupId?: string; members: Record<string, Member[]> }) {
  const [visited, setVisited] = useState<string[]>([]);
  useEffect(() => {
    if (groupId) setVisited(previous => previous.includes(groupId) ? previous : [...previous, groupId]);
  }, [groupId]);
  return <>
    {!groupId ? <p className="empty-panel">请先选择群组，再查看讨论和发送消息。</p> : null}
    {visited.map(id => <div key={id} hidden={id !== groupId}>
      <DiscussionScope groupId={id} active={id === groupId} members={members[id] ?? emptyMembers} />
    </div>)}
  </>;
}
function DiscussionScope({ groupId, active, members }: { groupId: string; active: boolean; members: Member[] }) {
  const formId = useId(), feed = useMessageFeed(groupId, active);
  const [content, setContent] = useState("");
  const [route, setRoute] = useState("default"), [recipients, setRecipients] = useState<string[]>([]);
  const [mutation, setMutation] = useState({ pending: false, uncertain: false, error: "", notice: "" });
  const operation = useRef<{ key: string; input: MessageInput } | null>(null);
  const posting = useRef<AbortController | null>(null), mounted = useRef(false);
  const eligible = members.filter(member => member.group_id === groupId && member.status === "active");
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
    try {
      const receipt = await sendMessage(groupId, current.input, current.key, controller.signal);
      if (!mounted.current) return;
      feed.confirm(receipt, attempt); operation.current = null; setContent("");
      setMutation({ pending: false, uncertain: false, error: "", notice: "消息已持久化；投递状态见消息记录，不代表模型已回复。" });
    } catch (error) {
      if (!mounted.current) return;
      const uncertain = !isDefinitiveRejection(error), reason = error instanceof ApiError ? error.message : "请求未完成，请重试。";
      if (!uncertain) operation.current = null;
      setMutation({ pending: false, uncertain, notice: "", error: uncertain ? `发送结果未确认。${reason} 请重试同一次发送。` : reason });
    } finally { posting.current = null; }
  }
  return <div className="group-panel discussion-feed">
    <button type="button" disabled={feed.page.pending} onClick={() => feed.loadPage()}>刷新并重置历史窗口</button>
    <p className="feed-note">已载入 {feed.items.length} 条群组消息（含话题）。已加载片段可能存在间隔；刷新重置窗口后可重新向前加载，不代表已读完历史。</p>
    {feed.page.pending ? <p role="status">正在载入消息…</p> : null}
    {feed.page.error ? <div role="alert"><p>{feed.page.error}</p><button type="button" onClick={feed.retry}>重试载入消息</button></div> : null}
    {!feed.page.pending && !feed.page.error && !feed.items.length ? <p>尚无消息，可发送第一条讨论。</p> : null}
    {feed.page.next ? <button type="button" disabled={feed.page.pending} onClick={() => feed.loadPage(feed.page.next, false)}>加载更早消息</button> : null}
    <ol className="message-list" aria-label="群组消息">{feed.items.map(message => <li key={message.id}>
      <article aria-label={`消息 ${message.id}`}>
        <header className="message-meta"><span>发送者：{message.sender_id}</span><time dateTime={message.created_at}>{message.created_at}</time></header>
        {message.topic_id ? <p className="eyebrow accent">话题消息 · {message.topic_id}</p> : <p className="eyebrow">主频道消息</p>}
        <p className="message-content">{message.content}</p>
        {message.deliveries.length ? <ul className="delivery-list">{message.deliveries.map(delivery => <li key={delivery.id}>
          {delivery.recipient_id} · {deliveryLabel(delivery.state)}{delivery.error ? ` · 投递错误：${delivery.error}` : ""}
        </li>)}</ul> : <p className="feed-note">暂无投递记录。</p>}
        <button type="button" aria-label={`刷新消息 ${message.id} 投递`} disabled={feed.details[message.id]?.pending}
          onClick={() => feed.refreshDetail(message.id)}>刷新投递</button>
        {feed.details[message.id]?.error ? <p role="alert">{feed.details[message.id].error}</p> : null}
      </article>
    </li>)}</ol>
    <p className="feed-note">CLI接收不代表轮次完成、任务提交或结果接受。</p>
    <form aria-label="发送消息" onSubmit={submit}>
      <fieldset disabled={mutation.pending || mutation.uncertain}>
        <legend>发送主频道消息</legend>
        <label htmlFor={`${formId}-content`}>消息内容</label>
        <textarea id={`${formId}-content`} required rows={4} value={content} onChange={event => setContent(event.target.value)} />
        <label htmlFor={`${formId}-route`}>接收方式</label>
        <select id={`${formId}-route`} value={route} onChange={event => setRoute(event.target.value)}>
          <option value="default">默认路由（由服务决定）</option><option value="public">仅公开可见（不唤醒）</option><option value="selected">指定本群成员</option>
        </select>
        {route === "selected" ? <div className="recipient-list">
          <p className="feed-note">仅列出右侧已载入的本群在群成员；可在成员栏刷新或加载更多。</p>
          {eligible.map(member => <label key={member.id}><input type="checkbox" checked={recipients.includes(member.id)}
            onChange={event => setRecipients(previous => event.target.checked ? [...previous, member.id] : previous.filter(id => id !== member.id))} />
            {member.name} · {member.role} · {member.id}</label>)}
          <button type="button" onClick={() => setRecipients([])}>清除所选成员</button>
        </div> : null}
      </fieldset>
      <p className="feed-note">群内可见性与模型唤醒分开；@文字不参与路由。草稿与未确认发送仅保留在当前页面，重载会丢失。</p>
      {mutation.error ? <p role="alert">{mutation.error}</p> : null}
      {mutation.notice ? <p role="status">{mutation.notice}</p> : null}
      <button type="submit" disabled={mutation.pending}>{mutation.pending ? "正在发送…" : mutation.uncertain ? "重试同一次发送" : "发送消息"}</button>
    </form>
  </div>;
}
