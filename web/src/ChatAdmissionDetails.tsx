import { useEffect, useRef, useState } from "react";
import { ApiError } from "./api";
import { getChatAdmission } from "./chat-api";
import type { ChatAdmission } from "./chat-api";
import { getDispatchRequest } from "./dispatch-request-api";
import type { DispatchView } from "./dispatch-request-api";
import { followsDispatchState, sameDispatchRequest } from "./dispatch-state";
import { TaskExecution } from "./TaskExecution";
import { useActiveRefresh } from "./useActiveRefresh";

const errorText = (error: unknown) => error instanceof ApiError ? error.message : "读取未确认，请重试。";
const statusText = { pending: "已登记，等待协调", enqueued: "已进入协调员投递队列", held: "协调请求暂停",
  assigned: "已记录分配，执行情况见回执", declined: "协调员已拒绝本次分派" };

// This read-only projection never shares or replaces a manual dispatch mutation's identity.
export function ChatAdmissionDetails({ groupId, taskId, active }: { groupId: string; taskId: string; active: boolean }) {
  return active ? <AdmissionSample key={JSON.stringify([groupId, taskId])} groupId={groupId} taskId={taskId} /> : null;
}
function AdmissionSample({ groupId, taskId }: { groupId: string; taskId: string }) {
  const [version, setVersion] = useState(0);
  const [read, setRead] = useState<{ pending: boolean; receipt: ChatAdmission | null; error: string }>({ pending: true, receipt: null, error: "" });
  useEffect(() => {
    let ignore = false;
    const controller = new AbortController();
    setRead({ pending: true, receipt: null, error: "" });
    void getChatAdmission(groupId, taskId, controller.signal).then(receipt => {
      if (!ignore) setRead({ pending: false, receipt, error: "" });
    }, error => { if (!ignore) setRead({ pending: false, receipt: null, error: `聊天自动准入依据未确认。${errorText(error)}` }); });
    return () => { ignore = true; controller.abort(); };
  }, [groupId, taskId, version]);
  if (read.pending) return <p role="status">正在读取聊天自动准入依据…</p>;
  if (read.error) return <div><p role="alert">{read.error}</p><button type="button" onClick={() => setVersion(value => value + 1)}>重试读取聊天准入依据</button></div>;
  const receipt = read.receipt;
  if (!receipt) return null;
  return <section className="tasks-lifecycle" aria-label="聊天自动准入依据">
    <h3>来自普通群聊的工作</h3>
    <p className="tasks-note">以下来自服务端不可变准入回执，不是模型正文推断。已登记分派不表示执行成功、交付或用户接纳。</p>
    <dl><dt>授权方式</dt><dd>{receipt.authority === "explicit_request" ? "本地用户明确请求" : "已启用的用户书面预授权"}</dd>
      <dt>来源消息</dt><dd className="tasks-meta">{receipt.message_id}</dd>
      <dt>来源发送者</dt><dd>{receipt.source_sender_id === "user" ? "本地用户（user）" : `群成员 ${receipt.source_sender_id}`}</dd>
      <dt>授权来源消息</dt><dd className="tasks-meta">{receipt.authorization_message_id}</dd>
      <dt>范围</dt><dd>当前群 / {receipt.topic_id ? `话题 ${receipt.topic_id}` : "主话题"} · 仅文本提交</dd>
      <dt>聊天策略</dt><dd>聊天策略版本 {receipt.policy_revision}</dd>
      <dt>授权标识</dt><dd className="tasks-meta">{receipt.mandate_id}</dd>
      <dt>评估成员</dt><dd className="tasks-meta">{receipt.assessed_by}</dd>
      <dt>准入回执时间</dt><dd><time dateTime={receipt.created_at}>{receipt.created_at}</time></dd></dl>
    <details><summary>准入技术依据</summary><dl>
      <dt>准入回执</dt><dd className="tasks-meta">{receipt.id}</dd><dt>候选</dt><dd className="tasks-meta">{receipt.candidate_id}</dd>
      <dt>候选代次</dt><dd>{receipt.candidate_generation}</dd><dt>评估轮次</dt><dd className="tasks-meta">{receipt.turn_id}</dd>
      <dt>群内行动说明消息</dt><dd className="tasks-meta">{receipt.notice_message_id}</dd>
      <dt>任务</dt><dd className="tasks-meta">{receipt.task_id}</dd>
    </dl></details>
    <AutomaticDispatch receipt={receipt} />
  </section>;
}
function AutomaticDispatch({ receipt }: { receipt: ChatAdmission }) {
  const [version, setVersion] = useState(0);
  const [read, setRead] = useState<{ pending: boolean; view: DispatchView | null; error: string }>({ pending: true, view: null, error: "" });
  const observed = useRef<DispatchView | null>(null);
  useActiveRefresh(true, read.pending, () => setVersion(value => value + 1));
  useEffect(() => {
    let ignore = false;
    const controller = new AbortController();
    setRead(previous => ({ ...previous, pending: true, error: "" }));
    void getDispatchRequest(receipt.group_id, receipt.task_id, receipt.dispatch_request_id, controller.signal).then(view => {
      if (ignore) return;
      const old = observed.current;
      if (view.request.topic_id !== receipt.topic_id || view.request.mandate_id !== receipt.mandate_id
        || (old && (!sameDispatchRequest(old.request, view.request) || !followsDispatchState(old.state, view.state)))) {
        setRead({ pending: false, view: null, error: "自动分派读取与原聊天准入依据不匹配或已过期，当前事实未确认。" }); return;
      }
      observed.current = view;
      setRead({ pending: false, view, error: "" });
    }, error => { if (!ignore) setRead({ pending: false, view: null, error: `自动分派当前状态未确认。${errorText(error)}` }); });
    return () => { ignore = true; controller.abort(); };
  }, [receipt, version]);
  const view = read.view;
  return <div className="tasks-lifecycle" aria-busy={read.pending}>
    <p className="tasks-meta">自动登记的分派请求：{receipt.dispatch_request_id}</p>
    {read.pending ? <p role="status">正在读取自动分派当前事实…</p> : null}
    {read.error ? <p role="alert">{read.error}</p> : null}
    {view ? <>
      <p role="status">{statusText[view.state.status]}</p>
      <p>任务版本 {view.request.task_revision} / 授权代次 {view.request.authority_generation}</p>
      <p>协调员绑定版本 {view.request.binding_revision} / 分派策略版本 {view.request.policy_revision}</p>
      <p className="tasks-meta">协调员：{view.request.coordinator_member_id} · 状态代次 {view.state.generation}</p>
      {view.state.held_reason ? <p className="tasks-meta">暂停原因：{view.state.held_reason}</p> : null}
    </> : null}
    <button type="button" onClick={() => setVersion(value => value + 1)}>读取自动分派当前状态</button>
    <TaskExecution groupId={receipt.group_id} taskId={receipt.task_id} requestId={receipt.dispatch_request_id} active={Boolean(view) && !read.error} />
  </div>;
}
