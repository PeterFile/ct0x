import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, getMessage, isMessageSourceIdentityConflict } from "./api";
import type { Message } from "./api";
import { messageSourceLabel } from "./message-source";
import { getChatAdmission } from "./chat-api";
import type { ChatAdmission, ChatRepairLink } from "./chat-api";
import { getDispatchRequest } from "./dispatch-request-api";
import type { DispatchView } from "./dispatch-request-api";
import { followsDispatchState, sameDispatchRequest } from "./dispatch-state";
import { TaskExecution } from "./TaskExecution";
import { useActiveRefresh } from "./useActiveRefresh";

const errorText = (error: unknown) => error instanceof ApiError ? error.message : "读取未确认，请重试。";
const statusText = { pending: "已登记，等待协调", enqueued: "已进入协调员投递队列", held: "协调请求暂停",
  assigned: "已记录分配，执行情况见回执", declined: "协调员已拒绝本次分派" };

type Observation = { view: DispatchView | null; sourceConflict: boolean; readEpoch: number; cancelRead?: () => void };
type Observations = Map<string, Observation>;

// The visited-task owner survives hiding; its cache is local to this mounted scope, never global.
// This read-only projection never shares or replaces a manual dispatch mutation's identity.
export function ChatAdmissionDetails({ groupId, taskId, active }: { groupId: string; taskId: string; active: boolean }) {
  const observations = useRef<Observations>(new Map());
  return active ? <AdmissionSample key={JSON.stringify([groupId, taskId])} groupId={groupId} taskId={taskId} observations={observations.current} /> : null;
}
function AdmissionSample({ groupId, taskId, observations }: { groupId: string; taskId: string; observations: Observations }) {
  const [version, setVersion] = useState(0), [sourceConflict, setSourceConflict] = useState(false);
  const [read, setRead] = useState<{ pending: boolean; receipt: ChatAdmission | null; observation: Observation | null; error: string }>(
    { pending: true, receipt: null, observation: null, error: "" });
  const onSourceCheck = useCallback((conflict: boolean) => {
    if (read.observation) {
      read.observation.sourceConflict = conflict;
      if (conflict) {
        read.observation.readEpoch++;
        read.observation.cancelRead?.();
      }
    }
    setSourceConflict(conflict);
  }, [read.observation]);
  useEffect(() => {
    let ignore = false;
    const controller = new AbortController();
    setRead({ pending: true, receipt: null, observation: null, error: "" });
    void getChatAdmission(groupId, taskId, controller.signal).then(receipt => {
      if (ignore) return;
      let observation: Observation | null = null;
      if (receipt) {
        const key = JSON.stringify([groupId, taskId, receipt.dispatch_request_id]);
        observation = observations.get(key) ?? { view: null, sourceConflict: false, readEpoch: 0 };
        observations.set(key, observation);
      }
      setSourceConflict(observation?.sourceConflict ?? false);
      setRead({ pending: false, receipt, observation, error: "" });
    }, error => { if (!ignore) setRead({ pending: false, receipt: null, observation: null, error: `聊天自动准入依据未确认。${errorText(error)}` }); });
    return () => { ignore = true; controller.abort(); };
  }, [groupId, taskId, version, observations]);
  if (read.pending) return <p role="status">正在读取聊天自动准入依据…</p>;
  if (read.error) return <div><p role="alert">{read.error}</p><button type="button" onClick={() => setVersion(value => value + 1)}>重试读取聊天准入依据</button></div>;
  const receipt = read.receipt, observation = read.observation;
  if (!receipt || !observation) return null;
  return <section className="tasks-lifecycle" aria-label="聊天自动准入依据">
    <h3>{receipt.repair_link ? "来自正式拒收的修复工作" : "来自普通群聊的工作"}</h3>
    <p className="tasks-note">以下来自服务端不可变准入回执，不是模型正文推断。已登记分派不表示执行成功、交付或用户接纳。</p>
    <p className="tasks-note">来源核验不证明自然语言意图零误判，也不等同人类签批；最终用户审查与接纳仍独立。</p>
    {!receipt.authority_evidence_verified ? <p role="status">历史来源未核验或当前评估证据待确认。保留原授权声明，不补成真人批准；既有任务、交付与用户审查不因此受阻。</p> : null}
    <dl><dt>服务端意图判定</dt><dd>{receipt.authority_evidence_verified
      ? receipt.authority === "explicit_request" ? "受信意图判定为明确请求"
        : receipt.repair_link ? "受信意图判定为原约定许可的修复请求" : "受信意图判定为策略授权的准备工作"
      : receipt.authority === "explicit_request" ? "历史声明：明确请求（未核验）" : "历史声明：书面预授权（未核验）"}</dd>
      <dt>来源 / 职责核验</dt><dd>{sourceConflict ? "来源证据矛盾，当前未确认" : receipt.authority_evidence_verified ? "来源与评估职责已核验" : "未确认"}</dd>
      <dt>来源类型</dt><dd>{messageSourceLabel(receipt.source_kind)}{receipt.source_kind ? `（${receipt.source_kind}）` : ""}</dd>
      <dt>来源命令 / 回执 ID</dt><dd className="tasks-meta">{receipt.source_id || "未提供"}</dd>
      <dt>评估 purpose</dt><dd className="tasks-meta">{receipt.purpose || "未提供（历史未核验）"}</dd>
      <dt>来源消息</dt><dd className="tasks-meta">{receipt.message_id}</dd>
      <dt>来源发送者 ID</dt><dd className="tasks-meta">{receipt.source_sender_id}</dd>
      <dt>授权来源消息</dt><dd className="tasks-meta">{receipt.repair_link && !receipt.authorization_message_id
        ? "实际用户审查（无聊天消息 ID）" : receipt.authorization_message_id}</dd>
      <dt>范围</dt><dd>当前群 / {receipt.topic_id ? `话题 ${receipt.topic_id}` : "主话题"} · {receipt.action === "workspace_change" ? "目录改动（workspace_change）" : "仅文本提交"}</dd>
      <dt>聊天策略</dt><dd>聊天策略版本 {receipt.policy_revision}</dd>
      <dt>授权标识</dt><dd className="tasks-meta">{receipt.mandate_id}</dd>
      <dt>评估成员</dt><dd className="tasks-meta">{receipt.assessed_by}</dd>
      <dt>准入回执时间</dt><dd><time dateTime={receipt.created_at}>{receipt.created_at}</time></dd></dl>
    <details><summary>准入技术依据</summary><dl>
      <dt>准入回执</dt><dd className="tasks-meta">{receipt.id}</dd><dt>候选</dt><dd className="tasks-meta">{receipt.candidate_id}</dd>
      <dt>候选代次</dt><dd>{receipt.candidate_generation}</dd><dt>评估轮次</dt><dd className="tasks-meta">{receipt.turn_id}</dd>
      <dt>原 authority</dt><dd>{receipt.authority}</dd><dt>原 disposition</dt><dd>{receipt.disposition}</dd>
      <dt>原核验标记</dt><dd>{receipt.authority_evidence_verified === undefined ? "未提供" : String(receipt.authority_evidence_verified)}</dd>
      <dt>群内行动说明消息</dt><dd className="tasks-meta">{receipt.notice_message_id}</dd>
      <dt>任务</dt><dd className="tasks-meta">{receipt.task_id}</dd>
    </dl></details>
    {receipt.workspace_scope ? <section aria-label="目录工作准入范围">
      <h4>目录工作准入范围</h4>
      <p className="tasks-note">以下仅为不可变权限与来源事实，不证明当前可执行、文件已改动、Artifact 已交付或用户接纳。执行时仍须核对物理目录、当前适配器能力与资源锁。</p>
      <dl><dt>准入版本</dt><dd>准入时任务版本 {receipt.workspace_scope.task_revision} / 策略版本 {receipt.workspace_scope.policy_revision}</dd>
        <dt>执行节点</dt><dd>本机（{receipt.workspace_scope.node_id}）</dd>
        <dt>物理资源标识</dt><dd className="tasks-meta">{receipt.workspace_scope.resource_key}</dd></dl>
      <ul aria-label="准入绑定的目录授权">{receipt.workspace_scope.grants.map(grant => <li key={grant.grant_id}>
        <dl><dt>目录</dt><dd className="tasks-copy">{grant.root}</dd>
          <dt>授权时解析目录</dt><dd className="tasks-copy">{grant.resolved_root}</dd>
          <dt>授权成员 ID</dt><dd className="tasks-meta">{grant.member_id}</dd>
          <dt>读写授权 ID</dt><dd className="tasks-meta">{grant.grant_id}</dd>
          <dt>准入绑定的授权合同</dt><dd>v{grant.grant_schema_version}（不是当前执行能力版本）</dd></dl>
      </li>)}</ul>
    </section> : null}
    {receipt.repair_link ? <RepairSource link={receipt.repair_link} /> : null}
    <SourceMessage receipt={receipt} onCheck={onSourceCheck} />
    {sourceConflict ? <p role="alert">来源消息与聊天准入依据存在矛盾，自动分派与执行事实未确认；既有任务与用户审查仍独立。</p>
      : <AutomaticDispatch receipt={receipt} observation={observation} />}
  </section>;
}
function RepairSource({ link }: { link: ChatRepairLink }) {
  const authorization = { review_request: "正式拒收中的明确修复请求", original_agreement: "原有用户约定", chat_request: "当前用户聊天请求" };
  const source = { local_user_review: "实际用户审查", local_user_chat: "实际用户聊天", local_user_policy: "实际用户策略" };
  return <section aria-label="修复来源">
    <h4>修复来源</h4>
    <p className="tasks-note">以下为不可变修复关联，不证明执行成功或用户接纳。平台通知或成员正文不是用户许可。修复任务的接纳不自动完成原任务；原任务、交付、审查和成果历史保留。</p>
    <dl><dt>修复依据</dt><dd>{authorization[link.authorization]}（{link.authorization}）</dd>
      <dt>修复关联 ID</dt><dd className="tasks-meta">{link.id}</dd>
      <dt>原任务</dt><dd className="tasks-meta">{link.parent_task_id}</dd>
      <dt>原交付</dt><dd className="tasks-meta">{link.parent_submission_id}</dd>
      <dt>原拒收</dt><dd className="tasks-meta">{link.parent_review_id}</dd>
      <dt>原任务版本</dt><dd>原拒收任务版本 {link.reviewed_task_revision}</dd>
      <dt>原授权标识</dt><dd className="tasks-meta">{link.parent_mandate_id}</dd>
      <dt>实际许可来源与版本</dt><dd className="tasks-meta">{source[link.authorization_source_kind]}（{link.authorization_source_kind}） · {link.authorization_source_id}
        <br />许可来源策略版本 {link.authorization_policy_revision} / 本次准入策略版本 {link.policy_revision}</dd>
      <dt>实际许可消息</dt><dd className="tasks-meta">{link.authorization_message_id ?? "实际用户审查（无聊天消息 ID）"}</dd>
      <dt>修复关联时间</dt><dd><time dateTime={link.created_at}>{link.created_at}</time></dd></dl>
    <h5>原冻结版本</h5>
    {link.artifact_versions.length ? <ul className="tasks-artifact-refs" aria-label="原冻结版本">{link.artifact_versions.map(ref => {
      const [groupId, artifactId, versionId] = [link.group_id, ref.artifact_id, ref.version_id].map(id => encodeURIComponent(id));
      return <li key={JSON.stringify([ref.artifact_id, ref.version_id])} className="tasks-meta">
        <span>{ref.artifact_id} / {ref.version_id}</span> · <a href={`/api/v1/groups/${groupId}/artifacts/${artifactId}/versions/${versionId}/content`} download>下载原冻结成果文件</a>
      </li>;
    })}</ul> : <p className="tasks-note">原交付无冻结成果版本，仅文本修复来源。</p>}
  </section>;
}
function SourceMessage({ receipt, onCheck }: { receipt: ChatAdmission; onCheck: (conflict: boolean) => void }) {
  const [expanded, setExpanded] = useState(false), [version, setVersion] = useState(0);
  const [read, setRead] = useState<{ pending: boolean; value: Message | null; error: string }>({ pending: false, value: null, error: "" });
  useEffect(() => {
    if (!expanded) return;
    let ignore = false;
    const controller = new AbortController();
    setRead({ pending: true, value: null, error: "" });
    void getMessage(receipt.group_id, receipt.message_id, controller.signal).then(message => {
      if (ignore) return;
      const expectedSource = receipt.source_kind !== undefined && receipt.source_kind !== "unknown";
      const knownSource = message.source_kind !== undefined && message.source_kind !== "unknown";
      if (message.sender_id !== receipt.source_sender_id || message.topic_id !== receipt.topic_id || (expectedSource && knownSource
        && (message.source_kind !== receipt.source_kind || message.source_id !== receipt.source_id))) {
        onCheck(true);
        setRead({ pending: false, value: null, error: "来源原文与准入所绑定的发送者、话题或来源不一致，原文查阅未确认。" }); return;
      }
      // Missing legacy metadata is not a contradiction, nor can it clear a previously known conflict.
      if (!expectedSource || knownSource) onCheck(false);
      setRead({ pending: false, value: message, error: "" });
    }, error => {
      if (ignore) return;
      if (isMessageSourceIdentityConflict(error)) onCheck(true);
      setRead({ pending: false, value: null, error: `来源原文查阅未确认。${errorText(error)}` });
    });
    return () => { ignore = true; controller.abort(); };
  }, [expanded, receipt, version, onCheck]);
  return <div>
    <button type="button" aria-expanded={expanded} onClick={() => setExpanded(value => !value)}>{expanded ? "收起来源消息" : "检查来源消息"}</button>
    {expanded ? <section aria-label="来源消息原文">
      {read.pending ? <p role="status">正在读取绑定的来源消息…</p> : null}
      {read.error ? <><p role="status">{read.error}</p><button type="button" onClick={() => setVersion(value => value + 1)}>重新读取来源消息</button></> : null}
      {read.value ? <><p className="tasks-copy">{read.value.content}</p><time dateTime={read.value.created_at}>{read.value.created_at}</time>
        <p className="tasks-note">消息来源事实：{messageSourceLabel(read.value.source_kind)}；不以正文替代授权证据。</p></> : null}
    </section> : null}
  </div>;
}
function AutomaticDispatch({ receipt, observation }: { receipt: ChatAdmission; observation: Observation }) {
  const [version, setVersion] = useState(0);
  const [read, setRead] = useState<{ pending: boolean; view: DispatchView | null; error: string }>({ pending: true, view: null, error: "" });
  useActiveRefresh(true, read.pending, () => setVersion(value => value + 1));
  useEffect(() => {
    let ignore = false;
    const controller = new AbortController(), epoch = ++observation.readEpoch;
    const cancelRead = () => {
      ignore = true;
      if (observation.readEpoch === epoch) observation.readEpoch++;
      controller.abort();
    };
    observation.cancelRead = cancelRead;
    const canCommit = () => !ignore && !observation.sourceConflict && observation.readEpoch === epoch;
    setRead(previous => ({ ...previous, pending: true, error: "" }));
    void getDispatchRequest(receipt.group_id, receipt.task_id, receipt.dispatch_request_id, controller.signal).then(view => {
      if (!canCommit()) return;
      const old = observation.view;
      if (view.request.topic_id !== receipt.topic_id || view.request.mandate_id !== receipt.mandate_id
        || (receipt.workspace_scope && view.request.task_revision !== receipt.workspace_scope.task_revision)
        || (old && (!sameDispatchRequest(old.request, view.request) || !followsDispatchState(old.state, view.state)))) {
        setRead({ pending: false, view: null, error: "自动分派读取与原聊天准入依据不匹配或已过期，当前事实未确认。" }); return;
      }
      if (!canCommit()) return;
      observation.view = view;
      setRead({ pending: false, view, error: "" });
    }, error => { if (canCommit()) setRead({ pending: false, view: null, error: `自动分派当前状态未确认。${errorText(error)}` }); });
    return () => {
      cancelRead();
      if (observation.cancelRead === cancelRead) delete observation.cancelRead;
    };
  }, [receipt, version, observation]);
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
