import { useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { FormEvent } from "react";
import { ApiError, getMember, isDefinitiveRejection, matchesMessageQuote, sendMessage } from "./api";
import type { Delivery, Member, Message, MessageDiscussionWork, MessageInput, MessageQuoteTarget } from "./api";
import { messageAuthor, messageSourceLabel } from "./message-source";
import { refreshBackgroundRoster } from "./background-roster-refresh";
import { RequestWork } from "./RequestWork";
import type { RequestWorkHandle } from "./RequestWork";
import { useMessageFeed } from "./useMessageFeed";
import { useMessageScroll } from "./useMessageScroll";
import "./discussion-presentation.css";
const emptyMembers: Member[] = [];
const workspaceBeginPrefix = "workspace_begin:";
function isWorkspaceIntentTrace({ source_kind, source_id }: Message) {
  // workspace_finish: also covers receipts that require reconciliation; the ID does not distinguish those outcomes.
  return source_kind === "platform" && source_id !== undefined
    && source_id.startsWith(workspaceBeginPrefix) && source_id.length > workspaceBeginPrefix.length;
}
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

export type RecipientSelection = { groupId: string; memberId: string; version: number };

export function Discussion({ groupId, active = true, rosterGroupId, members, recipientSelection }: {
  groupId?: string; active?: boolean; rosterGroupId?: string; members: Record<string, Member[]>; recipientSelection?: RecipientSelection;
}) {
  const [visited, setVisited] = useState<string[]>([]);
  const feedGroupId = groupId ?? rosterGroupId;
  useEffect(() => {
    if (feedGroupId) setVisited(previous => previous.includes(feedGroupId) ? previous : [...previous, feedGroupId]);
  }, [feedGroupId]);
  return <>
    {!groupId ? <p className="empty-panel">请先选择群组，再查看讨论和发送消息。</p> : null}
    {visited.map(id => <div key={id} className="discussion-scope" hidden={id !== groupId}>
      <DiscussionScope groupId={id} active={id === groupId && active}
        paused={id === groupId && !active && id !== rosterGroupId}
        observeRoster={id === rosterGroupId}
        members={members[id] ?? emptyMembers} recipientSelection={recipientSelection} />
    </div>)}
  </>;
}
function DiscussionScope({ groupId, active, paused, observeRoster, members, recipientSelection }: {
  groupId: string; active: boolean; paused: boolean; observeRoster: boolean; members: Member[]; recipientSelection?: RecipientSelection;
}) {
  const observeFeed = active || observeRoster;
  const formId = useId(), feed = useMessageFeed(groupId, observeFeed, paused, active);
  const visibleItems = useMemo(() => feed.items.filter(message => !isWorkspaceIntentTrace(message)), [feed.items]);
  const scroll = useMessageScroll(active, visibleItems, feed.page);
  // Validate discussion relations against the full loaded window, not just rendered rows.
  const loaded = useMemo(() => new Map(feed.items.map(message => [message.id, message])), [feed.items]);
  const seenJoins = useRef(new Set<string>()), refreshedGap = useRef(0);
  useEffect(() => {
    if (!observeFeed) return;
    let joined = false;
    for (const message of feed.items) {
      if (message.group_id !== groupId || !backgroundJoin(message) || seenJoins.current.has(message.id)) continue;
      seenJoins.current.add(message.id);
      joined = true;
    }
    if (joined || (observeRoster && feed.latestGap !== refreshedGap.current)) {
      refreshedGap.current = feed.latestGap;
      refreshBackgroundRoster(groupId);
    }
  }, [observeFeed, observeRoster, feed.items, feed.latestGap, groupId]);
  const [content, setContent] = useState("");
  const [quote, setQuote] = useState<(Pick<Message, "id" | "sender_id" | "source_kind"> & { excerpt: string }) | null>(null);
  const [quoteAuthor, setQuoteAuthor] = useState<Pick<Member, "id" | "name"> | null>(null);
  const [quoteError, setQuoteError] = useState("");
  const [route, setRoute] = useState("default"), [recipients, setRecipients] = useState<string[]>([]);
  const [mention, setMention] = useState<{ start: number; end: number; query: string } | null>(null);
  const [highlight, setHighlight] = useState(0);
  const textarea = useRef<HTMLTextAreaElement>(null), caret = useRef<number | null>(null);
  const composing = useRef(false), receivedSelection = useRef<RecipientSelection | undefined>(undefined);
  const [mutation, setMutation] = useState({ pending: false, uncertain: false, error: "", notice: "" });
  const requestWork = useRef<RequestWorkHandle>(null);
  const operation = useRef<{ key: string; input: MessageInput } | null>(null);
  const posting = useRef<AbortController | null>(null), mounted = useRef(false);
  const scopedMembers = members.filter(member => member.group_id === groupId);
  const eligible = scopedMembers.filter(member => member.status === "active");
  const membersById = new Map(scopedMembers.map(member => [member.id, member]));
  const authorName = (id: string) => id === "user" ? "我" : membersById.get(id)?.name ?? "未载入成员";
  const quoteMemberID = quote && quote.sender_id !== "user" && quote.source_kind !== "platform" ? quote.sender_id : "";
  const quoteMemberName = membersById.get(quoteMemberID)?.name ?? (quoteAuthor?.id === quoteMemberID ? quoteAuthor.name : undefined);
  useEffect(() => {
    // Resolve an unpaged author's name only on demand, not for each timeline row.
    if (!active || !quoteMemberID || quoteMemberName) return;
    const controller = new AbortController();
    void getMember(groupId, quoteMemberID, controller.signal).then(member => {
      if (!controller.signal.aborted) setQuoteAuthor({ id: member.id, name: member.name });
    }).catch(() => { /* Keep the stable author ID if the historical name is unavailable. */ });
    return () => controller.abort();
  }, [active, groupId, quoteMemberID, quoteMemberName]);
  const blocked = mutation.pending || mutation.uncertain;
  const suggestions = mention && active && !blocked ? eligible.filter(member => (route !== "selected" || !recipients.includes(member.id))
    && `${member.name} ${member.role} ${member.id}`.toLocaleLowerCase().includes(mention.query.toLocaleLowerCase())) : [];
  const highlighted = Math.min(highlight, suggestions.length - 1);
  useLayoutEffect(() => {
    if (caret.current === null || !active) return;
    textarea.current?.focus(); textarea.current?.setSelectionRange(caret.current, caret.current); caret.current = null;
  }, [content, active]);
  useEffect(() => {
    if (!recipientSelection || recipientSelection.groupId !== groupId || receivedSelection.current === recipientSelection) return;
    receivedSelection.current = recipientSelection;
    if (posting.current || operation.current) {
      setMutation(previous => ({ ...previous, notice: "原发送尚未确认，不能替换接收者。" })); return;
    }
    if (!eligible.some(member => member.id === recipientSelection.memberId)) return;
    setRecipients(previous => route !== "selected" ? [recipientSelection.memberId]
      : previous.includes(recipientSelection.memberId) ? previous : [...previous, recipientSelection.memberId]);
    setRoute("selected"); setMention(null); textarea.current?.focus();
  }, [recipientSelection, groupId, members, route]);
  function updateMention(input: HTMLTextAreaElement) {
    const end = input.selectionStart, match = /@([^\s@]*)$/u.exec(input.value.slice(0, end));
    setMention(!composing.current && input.selectionStart === input.selectionEnd && match
      ? { start: end - match[1].length - 1, end, query: match[1] } : null);
    setHighlight(0);
  }
  function selectMention(id: string) {
    const member = eligible.find(member => member.id === id);
    if (blocked || posting.current || operation.current || !member || !mention) return;
    setRecipients(previous => route !== "selected" ? [id] : previous.includes(id) ? previous : [...previous, id]); setRoute("selected");
    const insertion = `@${member.name} `;
    caret.current = mention.start + insertion.length;
    setContent(previous => previous.slice(0, mention.start) + insertion + previous.slice(mention.end)); setMention(null);
  }
  function selectQuote(message: Message) {
    if (!active || blocked || operation.current || message.group_id !== groupId) return;
    setQuote({ id: message.id, sender_id: message.sender_id, source_kind: message.source_kind,
      excerpt: discussionPreview(message.content.replace(/[\p{White_Space}\p{Cc}]+/gu, " ").trim()) });
    textarea.current?.focus({ preventScroll: true });
  }
  async function navigateQuote(reference: MessageQuoteTarget) {
    if (!active) return;
    const navigation = scroll.beginQuoteNavigation(reference);
    setQuoteError("");
    try {
      const original = loaded.get(reference.message_id);
      if (original) {
        if (!matchesMessageQuote(reference, original)) throw new ApiError("被引原消息与引用不符。");
        feed.preserveReference(original.id);
      } else if (!await feed.loadQuote(reference, navigation.isCurrent)) return;
      navigation.focus();
    } catch (error) {
      if (navigation.isCurrent()) setQuoteError(error instanceof ApiError ? error.message : "未能载入被引原消息，请重试。");
    }
  }
  function removeRecipient(id: string) {
    if (blocked) return;
    setRecipients(previous => previous.filter(value => value !== id));
    if (recipients.length === 1) setRoute("default");
  }
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
    // Selecting a quote supplies a reference, not a topic selector.
    operation.current ??= { key: crypto.randomUUID(), input: Object.freeze({ content,
      ...(route === "default" ? {} : { recipient_ids: Object.freeze(route === "public" ? [] : [...recipients]) }),
      ...(quote ? { reply_to_message_id: quote.id } : {}) }) };
    setMention(null);
    const current = operation.current, controller = new AbortController(); posting.current = controller;
    setMutation({ pending: true, uncertain: false, error: "", notice: "" });
    const attempt = feed.beginSend();
    scroll.beginSend(attempt);
    try {
      const receipt = await sendMessage(groupId, current.input, current.key, controller.signal);
      if (!mounted.current) return;
      feed.confirm(receipt, attempt); operation.current = null; setContent(""); setQuote(null);
      scroll.confirmSend(receipt.id, attempt);
      const notice = receipt.thread_correction?.question ? "需澄清话题；归属未变更。"
        : receipt.thread_correction ? "话题纠正已记录；未向成员投递。" : "消息已持久化；投递状态见记录。";
      setMutation({ pending: false, uncertain: false, error: "", notice });
    } catch (error) {
      if (!mounted.current) return;
      const uncertain = mutation.uncertain || !isDefinitiveRejection(error), reason = error instanceof ApiError ? error.message : "请求未完成，请重试。";
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
      <span className="feed-note">最新页自动更新</span>
    </div>
    <p className="feed-note">已载入 {visibleItems.length} 条（含话题，片段可能有间隔）。</p>
    {feed.pollError ? <p role="status">自动刷新暂不可用：{feed.pollError} 将稍后重试。</p> : null}
    {quoteError ? <p role="alert">无法跳转到被引消息：{quoteError}</p> : null}
    {feed.page.pending ? <p role="status">正在载入消息…</p> : null}
    {feed.page.error ? <div role="alert"><p>{feed.page.error}</p><button type="button" onClick={feed.retry}>重试载入消息</button></div> : null}
    {!feed.page.pending && !feed.page.error && !visibleItems.length
      ? <p>{feed.items.length ? "当前窗口没有讨论消息。" : "尚无消息，可发送第一条讨论。"}</p> : null}
    {feed.page.next ? <button type="button" disabled={feed.page.pending} onClick={() => { scroll.beforeOlder(); void feed.loadPage(feed.page.next, false); }}>加载更早消息</button> : null}
    <RequestWork groupId={groupId} active={active} members={members} ref={requestWork} />
    <ol className="message-list" aria-label="群组消息" ref={scroll.listRef}>{visibleItems.map(message => {
      const discussion = discussionContextFor(message, loaded);
      const projection = message.reply_to;
      const reference = projection ?? (message.reply_to_message_id
        ? { message_id: message.reply_to_message_id, group_id: message.group_id } : undefined);
      const original = reference ? loaded.get(reference.message_id) : undefined;
      const refreshDiscussion = discussion !== undefined && message.deliveries.length === 1;
      const refreshLabel = refreshDiscussion ? "讨论状态" : "投递";
      const refresh = <button className={refreshDiscussion ? "message-discussion-refresh" : undefined} type="button"
        aria-label={`刷新消息 ${message.id} ${refreshLabel}`} disabled={feed.details[message.id]?.pending}
        onClick={() => feed.refreshDetail(message.id)}>刷新{refreshLabel}</button>;
      return <li key={message.id} data-message-id={message.id}>
        <article id={messageAnchor(formId, message.id)} tabIndex={-1}
          className={message.source_kind === "local_user_chat" || message.source_kind === "local_user_policy" ? "message-local" : undefined} aria-label={`消息 ${message.id}`}>
          <header className="message-meta"><strong>{messageAuthor(message, membersById.get(message.sender_id)?.name)}</strong><time dateTime={message.created_at}>{shortTime(message.created_at)}</time>
            <span className="feed-note">{messageSourceLabel(message.source_kind)}</span></header>
          {message.topic_id ? <p className="eyebrow accent">话题消息</p> : null}
          {reference ? original && !matchesMessageQuote(reference, original) ? <p className="message-quote">引用 · 无法确认原消息</p>
            : <a className="message-quote" href={`#${messageAnchor(formId, reference.message_id)}`} onClick={event => {
              if (event.defaultPrevented || event.button !== 0 || event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
              event.preventDefault(); void navigateQuote(reference);
            }}>{projection ? <><strong>引用 · {messageAuthor(projection, projection.sender_name || `成员 ${projection.sender_id}`)}</strong>
              <span>{projection.excerpt || "（无文字短摘）"}</span></>
              : <><strong>引用 · 查看原消息</strong><span>消息 ID：{reference.message_id}</span></>}</a> : null}
          <p className="message-content">{message.content}</p>
          {message.thread_correction?.question ? <p className="message-content" role="status">
            <strong>话题归属待澄清：</strong><span>{message.thread_correction.question}</span>
          </p> : null}
          {discussion ? <DiscussionContext relation={discussion} closureReason={message.discussion_closure_reason}
            work={message.discussion_work} members={membersById} scopeId={formId}
            onNavigateReference={id => { scroll.onNavigateReference(id); feed.preserveReference(id); }} /> : null}
          <div className="message-actions">
            <button className="message-reply-action" type="button" aria-label={`回复，消息 ${message.id}`} disabled={blocked}
              onClick={() => selectQuote(message)}>回复</button>
            {!backgroundJoin(message) && !message.thread_correction ? <button className="message-task-action" type="button" aria-label={`转为任务，消息 ${message.id}`}
              onClick={event => requestWork.current?.open(message, event.currentTarget)}>转为任务</button> : null}
            {refreshDiscussion ? refresh : null}
            <details className="message-details">
              <summary className={message.deliveries.some(delivery => !["queued", "accepted"].includes(delivery.state) || Boolean(delivery.error)) ? "delivery-warning" : undefined}>
                投递详情 · {deliverySummary(message.deliveries)}
              </summary>
              <dl className="message-technical">
                <div><dt>消息 ID</dt><dd>{message.id}</dd></div>
                <div><dt>发送者 ID</dt><dd>{message.sender_id}</dd></div>
                <div><dt>来源类型</dt><dd>{message.source_kind ?? "未提供（历史未核验）"}</dd></div>
                {message.source_id ? <div><dt>来源命令 / 回执 ID</dt><dd>{message.source_id}</dd></div> : null}
                {message.topic_id ? <div><dt>话题 ID</dt><dd>{message.topic_id}</dd></div> : null}
                <div><dt>完整时间</dt><dd><time dateTime={message.created_at}>{message.created_at}</time></dd></div>
              </dl>
              {message.deliveries.length ? <ul className="delivery-list">{message.deliveries.map(delivery => <li key={delivery.id}>
                <p><strong>{authorName(delivery.recipient_id)}</strong> · <span>{delivery.recipient_id}</span></p>
                <p>{deliveryLabel(delivery.state)}（{delivery.state}）</p>
                {delivery.error ? <p className="delivery-warning">投递错误：{delivery.error}</p> : null}
                <p>投递 ID：{delivery.id}</p>
              </li>)}</ul> : null}
              {!refreshDiscussion ? refresh : null}
            </details>
          </div>
          {feed.details[message.id]?.error ? <p role="alert">{feed.details[message.id].error}</p> : null}
        </article>
      </li>;
    })}</ol>
    </section>
    <section className="message-composer" aria-label="撰写消息" tabIndex={0}>
    <form aria-label="发送消息" onSubmit={submit}>
      {scroll.windowReset ? <p className="feed-note">历史窗口已更新，可加载更早消息</p> : null}
      {quote ? <div className="message-quote composer-quote">
        <span><span>引用 · </span><strong>{messageAuthor(quote, quoteMemberName ?? `成员 ${quote.sender_id}`)}</strong></span>
        <button type="button" disabled={blocked} onClick={() => { if (!operation.current) setQuote(null); }}>取消引用</button>
        <span>{quote.excerpt || "（无文字短摘）"}</span>
      </div> : null}
      <fieldset disabled={blocked}>
        <label htmlFor={`${formId}-content`}>消息内容</label>
        <textarea ref={textarea} id={`${formId}-content`} required rows={2} value={content} placeholder="直接发消息，输入 @ 选择成员"
          aria-autocomplete="list" aria-controls={suggestions.length ? `${formId}-mentions` : undefined}
          aria-activedescendant={suggestions.length ? `${formId}-mention-${highlighted}` : undefined}
          aria-describedby={`${formId}-help`}
          onChange={event => { if (!blocked) { setContent(event.target.value); updateMention(event.currentTarget); } }}
          onSelect={event => { if (!blocked) updateMention(event.currentTarget); }}
          onCompositionStart={() => { composing.current = true; setMention(null); }}
          onCompositionEnd={event => { composing.current = false; updateMention(event.currentTarget); }}
          onKeyDown={event => {
            if (event.nativeEvent.isComposing || composing.current || !mention) return;
            if (event.key === "Escape") { event.preventDefault(); setMention(null); }
            else if (suggestions.length && ["ArrowDown", "ArrowUp", "Enter"].includes(event.key)) {
              event.preventDefault();
              if (event.key === "Enter") selectMention(suggestions[highlighted].id);
              else setHighlight((highlighted + (event.key === "ArrowDown" ? 1 : -1) + suggestions.length) % suggestions.length);
            }
          }} />
      </fieldset>
      {mention && active && !blocked ? suggestions.length ? <ul className="mention-suggestions" id={`${formId}-mentions`} role="listbox" aria-label="选择消息接收者">
        {suggestions.map((member, index) => <li key={member.id} role="none"><button type="button" role="option" tabIndex={-1}
          id={`${formId}-mention-${index}`} aria-selected={index === highlighted}
          onMouseDown={event => event.preventDefault()} onClick={() => selectMention(member.id)}>
          <strong>{member.name}</strong><span>{member.role} · {member.id}</span>
        </button></li>)}
      </ul> : <p className="feed-note" role="status">没有匹配的已载入在群成员；可打开成员详情加载。</p> : null}
      {route === "selected" && recipients.length ? <fieldset disabled={blocked}>
        <ul className="selected-recipients" aria-label="已选接收者">{recipients.map(id => <li key={id}>
          <span>{membersById.get(id)?.name ?? "未载入成员"} · {id}{!eligible.some(member => member.id === id) ? "（已失效，请移除或重新载入）" : ""}</span>
          <button type="button" aria-label={`移除接收者 ${id}`} onClick={() => removeRecipient(id)}>移除</button>
        </li>)}</ul>
      </fieldset> : null}
      <div className="composer-actions">
        <p className="feed-note" id={`${formId}-help`}>@ 选择接收者 · ↑↓ 选择，Enter 确认</p>
        <button className="primary-send" type="submit" disabled={mutation.pending}>{mutation.pending ? "正在发送…" : mutation.uncertain ? "重试同一次发送" : "发送消息"}</button>
      </div>
      {route === "public" ? <p className="feed-note">仅公开可见，不唤醒成员。</p> : null}
      <details className="composer-options">
        <summary>发送选项</summary>
        <fieldset disabled={blocked}>
          <label htmlFor={`${formId}-route`}>接收方式</label>
          <select id={`${formId}-route`} value={route} onChange={event => { if (!blocked) setRoute(event.target.value); }}>
            <option value="default">默认路由（由服务决定）</option><option value="public">仅公开可见（不唤醒）</option><option value="selected">指定本群成员</option>
          </select>
          {route === "selected" ? <div className="recipient-list">
            <p className="feed-note">仅列出已载入的在群成员；更多成员请打开成员详情加载。</p>
            {eligible.map(member => <label key={member.id}><input type="checkbox" checked={recipients.includes(member.id)}
              onChange={event => { if (!blocked) setRecipients(previous => event.target.checked ? [...previous, member.id] : previous.filter(id => id !== member.id)); }} />
              <span>{member.name} · {member.role} · {member.id}</span></label>)}
            <button type="button" onClick={() => { if (!blocked) setRecipients([]); }}>清除所选成员</button>
          </div> : null}
        </fieldset>
      </details>
      {content.length > 0 || mutation.uncertain ? <p className="feed-note">草稿与未确认发送仅保留在本页，重载会丢失。</p> : null}
      {mutation.error ? <p role="alert">{mutation.error}</p> : null}
      {mutation.notice ? <p role="status">{mutation.notice}</p> : null}
    </form>
    </section>
  </div>;
}

function backgroundJoin(message: Message) {
  return message.source_kind === "platform" && message.source_id?.startsWith("group_background_join:");
}

const discussionLabels = { invite: "邀请协作", reply: "回送答复", resolve: "讨论总结" };
// Escape opaque IDs into unambiguous fragments, namespaced to this mounted group scope.
const messageAnchor = (scopeId: string, id: string) =>
  `${scopeId}-message-${id.replace(/[^a-zA-Z0-9-]/gu, character => `_${character.codePointAt(0)!.toString(16)}_`)}`;
function discussionPreview(content: string) {
  let text = "", length = 0;
  for (const character of content) {
    if (length++ === 120) return `${text.trimEnd()}…`;
    text += character;
  }
  return text;
}

function discussionContextFor(message: Message, loaded: ReadonlyMap<string, Message>) {
  const facts = message.discussion;
  if (!facts) return;
  const { kind, root_message_id, invitation_message_id, parent_invitation_message_id, inviter_id, assignee_id, expected_reply } = facts;
  const sameScope = (target: Message) => target.group_id === message.group_id && target.topic_id === message.topic_id;
  const root = loaded.get(root_message_id);
  // A missing row is not a contradiction. A loaded row must agree before lending its text or identity to this relation.
  if (root && (!sameScope(root) || root.sender_id !== "user"
    || (root.source_kind !== undefined && root.source_kind !== "unknown" && root.source_kind !== "local_user_chat"))) return;
  const invitationFacts = (target: Message) => sameScope(target) && target.discussion?.kind === "invite"
    && target.discussion.root_message_id === root_message_id ? target.discussion : undefined;
  const invitation = kind === "invite" ? undefined : loaded.get(invitation_message_id);
  if (invitation) {
    const evidence = invitationFacts(invitation);
    if (!evidence || evidence.inviter_id !== inviter_id || evidence.assignee_id !== assignee_id
      || evidence.parent_invitation_message_id !== parent_invitation_message_id || evidence.expected_reply !== expected_reply) return;
  }
  const parent = parent_invitation_message_id ? loaded.get(parent_invitation_message_id) : undefined;
  if (parent && invitationFacts(parent)?.assignee_id !== inviter_id) return;
  return { facts, root, invitation, parent };
}

function DiscussionContext({ relation, closureReason, work, members, scopeId, onNavigateReference }: {
  relation: NonNullable<ReturnType<typeof discussionContextFor>>; closureReason?: string; work?: MessageDiscussionWork;
  members: ReadonlyMap<string, Member>; scopeId: string; onNavigateReference: (id: string) => void;
}) {
  const { facts, root, invitation, parent } = relation;
  const { kind, root_message_id, invitation_message_id, parent_invitation_message_id, inviter_id, assignee_id, expected_reply } = facts;
  const actor = (id: string) => {
    const member = members.get(id);
    return member ? `${member.name || "未命名成员"}${member.status === "left" ? "（已离开）" : member.status === "active" ? "" : "（状态未知）"}` : "未载入成员";
  };
  const summary = kind === "invite" ? `${actor(inviter_id)} 邀请 ${actor(assignee_id)}`
    : kind === "reply" ? `${actor(assignee_id)} 回送给 ${actor(inviter_id)}` : `${actor(inviter_id)} 向群组总结`;
  const workerName = work?.worker_member_id === undefined ? undefined : members.get(work.worker_member_id)?.name;
  return <>
    <details className="message-details message-discussion">
      <summary><strong>{discussionLabels[kind]}</strong> · {summary}</summary>
      <dl className="message-technical">
        <DiscussionReference label="原始问题" id={root_message_id} target={root} scopeId={scopeId} onNavigate={onNavigateReference} />
        <DiscussionReference label="对应邀请" id={invitation_message_id} target={invitation} scopeId={scopeId} self={kind === "invite"}
          onNavigate={onNavigateReference} />
        {parent_invitation_message_id ? <DiscussionReference label="上级邀请" id={parent_invitation_message_id} target={parent} scopeId={scopeId}
          onNavigate={onNavigateReference} /> : null}
        <div><dt>邀请者 ID</dt><dd><code>{inviter_id}</code></dd></div>
        <div><dt>受邀者 ID</dt><dd><code>{assignee_id}</code></dd></div>
        {expected_reply.trim() ? <div><dt>预期答复</dt><dd>{expected_reply}</dd></div> : null}
      </dl>
    </details>
    {closureReason ? <p className="feed-note discussion-closure">{closureReason}</p> : null}
    {kind === "invite" && work ? <p className="discussion-work">
      <strong>已转为正式工作</strong> · 任务 ID：<code>{work.task_id}</code>
      {work.assignment_id ? <> · 分配 ID：<code>{work.assignment_id}</code>
        {workerName?.trim() ? `（${workerName}）` : <> · 执行成员 ID：<code>{work.worker_member_id}</code></>}
      </> : null}
    </p> : null}
  </>;
}

function DiscussionReference({ label, id, target, scopeId, self, onNavigate }: {
  label: string; id: string; target?: Message; scopeId: string; self?: boolean; onNavigate: (id: string) => void;
}) {
  return <div>
    <dt>{target ? <a href={`#${messageAnchor(scopeId, id)}`} onClick={event => {
      if (!event.defaultPrevented && event.button === 0 && !event.altKey && !event.ctrlKey && !event.metaKey && !event.shiftKey) onNavigate(id);
    }}>{label}</a> : label}</dt>
    <dd><code>{id}</code>{self ? "（本条消息）" : !target ? "（未在当前窗口载入）" : null}
      {target ? <p className="discussion-reference-preview">{discussionPreview(target.content)}</p> : null}
    </dd>
  </div>;
}
