import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import { getAgentRun, listAgentRuns } from "./agent-runs-api";
import type {
  AgentRunDetail, AgentRunDispatchDecision, AgentRunEntry, AgentRunIdentity, AgentRunMessage, AgentRunPage,
  AgentRunState, AgentRunSummary, AgentRunWorkspaceOperation,
} from "./agent-runs-api";
import { ApiError } from "./api";
import { readHistory } from "./history-api";
import { useActiveRefresh } from "./useActiveRefresh";
import "./agent-run-log.css";

const runStatus: Record<AgentRunState, string> = {
  starting: "正在启动", running: "运行中", cancel_requested: "已请求取消，尚未确认停止",
  succeeded: "运行成功", failed: "运行失败", unknown: "运行结果未知", not_started: "运行未启动",
  aborted: "运行已中止", truncated: "运行输出被截断",
};
const terminalStatus = { succeeded: "报告成功", failed: "报告失败", aborted: "报告中止", truncated: "报告输出被截断" };
const operationStatus: Record<AgentRunWorkspaceOperation["state"], string> = {
  pending: "已登记，尚无操作回执", done: "已确认写入并同步", unchanged: "已确认未改动",
  unknown: "操作结果未知，待核对", applied_sync_error: "已应用，持久同步未确认",
};
const entryTitle: Record<AgentRunEntry["kind"], string> = {
  trigger_message: "触发消息", published_reply: "群内发言", public_explanation: "公开说明", submission: "正式提交",
  dispatch_decision: "分派决定", workspace_operation: "工作区动作",
};
const runKey = ({ group_id, run_id }: AgentRunIdentity) => JSON.stringify([group_id, run_id]);
const memberName = (run: AgentRunSummary) => run.member_name || "未命名 Agent";
const groupName = (run: AgentRunSummary) => run.group_name || "未命名群组";
const errorText = (error: unknown) => error instanceof ApiError ? error.message : "请求未完成，请重试。";
const dateFormat = new Intl.DateTimeFormat("zh-CN", { dateStyle: "short", timeStyle: "medium" });

function Time({ value, id }: { value: string; id?: string }) {
  return <time id={id} dateTime={value} title={value}>{dateFormat.format(new Date(value))}</time>;
}
function Facts({ items }: { items: [string, ReactNode][] }) {
  return <dl className="run-log-facts">{items.map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}</dl>;
}

type ReadWindow = { before?: string; through?: string };
type ReadState<T> = { value: T | null; error: string; request: { window: ReadWindow; pending: boolean } | null };
function useLogRead<T>(active: boolean, fetchPage: (window: ReadWindow, signal: AbortSignal) => Promise<T>,
  merge?: (previous: T | null, next: T, window: ReadWindow) => T, eager = true) {
  const [state, setState] = useState<ReadState<T>>(() => ({ value: null, error: "", request: eager ? { window: {}, pending: true } : null }));
  const { request } = state;
  useEffect(() => {
    if (!active || !request?.pending) return;
    const controller = new AbortController();
    const finish = (result: (previous: ReadState<T>) => Pick<ReadState<T>, "value" | "error">) => {
      if (controller.signal.aborted) return;
      setState(previous => previous.request === request
        ? { ...result(previous), request: { ...request, pending: false } } : previous);
    };
    void fetchPage(request.window, controller.signal).then(next => finish(previous => ({
      value: merge ? merge(previous.value, next, request.window) : next, error: "",
    })), error => finish(previous => ({ value: previous.value, error: errorText(error) })));
    // Keep the requested window while hidden; abort transports and resume only unfinished reads.
    return () => controller.abort();
  }, [active, fetchPage, merge, request]);
  function load(window: ReadWindow = {}) {
    if (!active) return;
    setState(previous => previous.request?.pending ? previous : { ...previous, error: "", request: { window, pending: true } });
  }
  return { value: state.value, error: state.error, pending: request?.pending ?? false, load, retry: () => load(request?.window) };
}
function ReadFeedback({ subject, pending, error, retry }: { subject: string; pending: boolean; error: string; retry: () => void }) {
  return pending ? <p role="status">正在读取{subject}…</p> : error ? <div className="run-log-error">
    <p role="alert">{error}</p><button type="button" onClick={retry}>重试读取{subject}</button>
  </div> : null;
}

function unique<T>(items: T[], key: (item: T) => string) {
  return [...new Map(items.map(item => [key(item), item] as const)).values()];
}
function mergeRuns(previous: AgentRunPage<AgentRunSummary> | null, next: AgentRunPage<AgentRunSummary>, { before }: ReadWindow): AgentRunPage<AgentRunSummary> {
  return before && previous ? { ...next, items: unique([...previous.items, ...next.items], runKey) } : next;
}
function mergeTimeline(previous: AgentRunDetail | null, next: AgentRunDetail, { before }: ReadWindow): AgentRunDetail {
  // Append older pages in wire order. On overlap keep the first exact event ID.
  const rows = before && previous ? [...previous.timeline.items, ...next.timeline.items] : next.timeline.items;
  const seen = new Set<string>();
  const items = rows.filter(({ id }) => {
    if (seen.has(id)) return false;
    seen.add(id);
    return true;
  });
  return { ...next, timeline: { ...next.timeline, items } };
}
async function readTimeline(run: AgentRunIdentity, { before, through }: ReadWindow, signal: AbortSignal): Promise<AgentRunDetail> {
  let detail = await getAgentRun(run, before, signal);
  if (!through) return detail;
  // Refresh the loaded range from the head, including new entries and receipts on older pages.
  const items = [...detail.timeline.items];
  const cursors = new Set<string>();
  while (detail.timeline.next_cursor && !detail.timeline.items.some(item => item.id === through)) {
    signal.throwIfAborted();
    const cursor = detail.timeline.next_cursor;
    if (cursors.has(cursor)) throw new ApiError("运行时间线无法继续分页，请重试。", 200, "invalid_response");
    cursors.add(cursor);
    detail = await getAgentRun(run, cursor, signal);
    items.push(...detail.timeline.items);
  }
  return { ...detail, timeline: { ...detail.timeline, items } };
}
function hasPendingEvidence({ run, timeline }: AgentRunDetail) {
  return run.state === "starting" || run.state === "running" || run.state === "cancel_requested"
    || timeline.items.some(item => item.kind === "workspace_operation" && item.workspace_operation.state === "pending");
}

export function AgentRunLogPage({ active }: { active: boolean }) {
  const headingId = useId();
  const [memberId, setMemberId] = useState("");
  const [agents, setAgents] = useState(() => new Map<string, AgentRunSummary>());
  const rememberAgents = useCallback((runs: AgentRunSummary[]) => {
    if (!runs.length) return;
    setAgents(previous => {
      const next = new Map(previous);
      for (const run of runs) next.set(run.member_id, run);
      return next;
    });
  }, []);
  return <section className="agent-run-log" aria-labelledby={headingId} hidden={!active}>
    <header className="run-log-page-header">
      <h1 id={headingId}>Agent 运行日志</h1>
      <p className="run-log-note">按 Agent 浏览可见轮次，核对群内原话、记录的动作与回执。</p>
    </header>
    <label className="run-log-filter">Agent 范围
      <select value={memberId} onChange={event => setMemberId(event.currentTarget.value)}>
        <option value="">全部 Agent</option>
        {[...agents.values()].map(run => <option key={run.member_id} value={run.member_id}>
          {memberName(run)} · {groupName(run)} · {run.member_id}
        </option>)}
      </select>
    </label>
    <RunScope key={memberId} memberId={memberId} active={active} rememberAgents={rememberAgents} />
  </section>;
}

function RunScope({ active, memberId, rememberAgents }: { active: boolean; memberId: string; rememberAgents: (runs: AgentRunSummary[]) => void }) {
  const listId = useId(), detailId = useId();
  const listHeading = useRef<HTMLHeadingElement>(null), detailHeading = useRef<HTMLHeadingElement>(null);
  const selectedButton = useRef<HTMLButtonElement>(null);
  const [selected, setSelected] = useState<AgentRunSummary | null>(null);
  const selectedKey = selected ? runKey(selected) : "";
  const fetchRuns = useCallback(({ before }: ReadWindow, signal: AbortSignal) => listAgentRuns(memberId, before, signal), [memberId]);
  const read = useLogRead(active, fetchRuns, mergeRuns);
  useEffect(() => { if (read.value) rememberAgents(read.value.items); }, [read.value, rememberAgents]);
  useLayoutEffect(() => { if (selected) detailHeading.current?.focus(); }, [selected]);

  const byMember = new Map<string, AgentRunSummary[]>();
  for (const run of read.value?.items ?? []) {
    const runs = byMember.get(run.member_id);
    if (runs) runs.push(run);
    else byMember.set(run.member_id, [run]);
  }
  const cursor = read.value?.next_cursor;
  return <div className="run-log-layout">
    <section className="run-log-list-pane" aria-labelledby={listId}>
      <header className="run-log-bar"><h2 id={listId} ref={listHeading} tabIndex={-1}>运行轮次</h2>
        <button type="button" aria-disabled={read.pending} onClick={() => read.load()}>刷新运行列表</button>
      </header>
      <ReadFeedback subject="运行轮次" {...read} />
      {!read.pending && !read.error && !byMember.size ? <p className="run-log-note">
        {memberId ? "此 Agent 暂无可查看的运行记录。" : "暂无可查看的运行记录。"}
      </p> : null}
      {[...byMember].map(([id, runs]) => <div className="run-log-member" key={id}>
        <h3>{memberName(runs[0])}</h3>
        <p className="run-log-note">成员 ID：<code>{id}</code></p>
        <ul className="run-log-runs" aria-label={`${memberName(runs[0])} ${id} 的轮次`} role="list">
          {runs.map(run => {
            const key = runKey(run), pressed = selectedKey === key, timeId = `${listId}-time-${encodeURIComponent(key)}`;
            return <li key={key}><button type="button" ref={pressed ? selectedButton : undefined} aria-pressed={pressed}
              aria-controls={detailId} aria-describedby={timeId} aria-label={`${groupName(run)} · 运行 ${run.run_id}`} onClick={() => {
                if (pressed) detailHeading.current?.focus();
                else setSelected(run);
              }}>
              <strong>{groupName(run)}</strong><Time id={timeId} value={run.created_at} />
            </button></li>;
          })}
        </ul>
      </div>)}
      {cursor ? <button type="button" aria-disabled={read.pending} onClick={() => read.load({ before: cursor })}>加载更早轮次</button> : null}
    </section>
    <section className="run-log-detail-pane" id={detailId} aria-labelledby={`${detailId}-heading`}>
      <header className="run-log-bar"><h2 id={`${detailId}-heading`} ref={detailHeading} tabIndex={-1}>本轮证据</h2>
        {selected ? <button type="button" onClick={() => (selectedButton.current ?? listHeading.current)?.focus()}>返回轮次列表</button> : null}
      </header>
      {selected ? <RunEvidence key={selectedKey} active={active} selected={selected} />
        : <p className="run-log-note">选择一轮查看运行证据。</p>}
    </section>
  </div>;
}

function RunEvidence({ active, selected }: { active: boolean; selected: AgentRunSummary }) {
  const { group_id, run_id, member_id } = selected;
  const fetchTimeline = useCallback((window: ReadWindow, signal: AbortSignal) => readTimeline({ group_id, run_id, member_id }, window, signal),
    [group_id, run_id, member_id]);
  const read = useLogRead(active, fetchTimeline, mergeTimeline);
  const detail = read.value;
  const refresh = () => read.load({ through: detail?.timeline.items.at(-1)?.id });
  useActiveRefresh(active && !read.error && Boolean(detail && hasPendingEvidence(detail)), read.pending, refresh);
  const cursor = detail?.timeline.next_cursor;
  const displayEntries = detail ? [...detail.timeline.items].reverse() : [];
  return <>
    <div className="run-log-bar">
      <p><strong>{memberName(detail?.run ?? selected)}</strong> · {groupName(detail?.run ?? selected)}</p>
      <button type="button" aria-disabled={read.pending} onClick={refresh}>刷新本轮证据</button>
    </div>
    <ReadFeedback subject="本轮证据" {...read} />
    {detail ? <>
      <p className="run-log-state" role="status">{runStatus[detail.run.state]}</p>
      <details><summary>查看轮次标识</summary><Facts items={[
        ["运行 ID", <code>{run_id}</code>], ["群组 ID", <code>{group_id}</code>], ["成员 ID", <code>{member_id}</code>],
        ["创建时间", <Time value={detail.run.created_at} />], ["更新时间", <Time value={detail.run.updated_at} />],
      ]} /></details>
      <RunReceipt detail={detail} />
      <h3>事件时间线</h3>
      {cursor ? <button type="button" aria-disabled={read.pending} onClick={() => read.load({ before: cursor })}>加载更早证据</button> : null}
      {displayEntries.length ? <ol className="run-log-timeline" aria-label="事件时间线" role="list">
        {displayEntries.map(entry => <li key={entry.id}><EvidenceEntry entry={entry} groupId={group_id} active={active} /></li>)}
      </ol> : <p className="run-log-note">本轮暂无可查看的时间线记录。</p>}
    </> : null}
  </>;
}

function RunReceipt({ detail }: { detail: AgentRunDetail }) {
  const { receipt, accepted_at: acceptedAt, cancel_requested_at: cancelAt } = detail;
  const flag = (value: boolean) => value ? "是" : "否";
  const items: [string, ReactNode][] = [];
  if (acceptedAt) items.push(["CLI 接收输入时间", <Time value={acceptedAt} />]);
  if (cancelAt) items.push(["取消请求时间", <Time value={cancelAt} />]);
  if (receipt) items.push(
    ["回执结论", runStatus[receipt.outcome]],
    ["CLI 终止观测", receipt.observed_terminal === "" ? "未记录" : terminalStatus[receipt.observed_terminal]],
    ["记录到启动", flag(receipt.started)], ["CLI 接收输入", flag(receipt.accepted)],
    ["执行投递失败", flag(receipt.delivery_failed)], ["直接子进程已回收", flag(receipt.direct_child_reaped)],
    ["输出完整", flag(receipt.output_complete)],
  );
  return <details><summary>{receipt ? "查看运行回执" : "运行回执（未提供）"}</summary>
    {items.length ? <Facts items={items} /> : <p className="run-log-note">没有可查看的运行回执。</p>}
    {receipt ? <p className="run-log-note">这些标记只描述本次调用。CLI 终止观测不等于运行结论，也不证明任何工作区动作成功。</p> : null}
    {receipt || acceptedAt ? <p className="run-log-note">CLI 接收输入不表示消息已读或已处理。</p> : null}
  </details>;
}

function EvidenceEntry({ entry, groupId, active }: { entry: AgentRunEntry; groupId: string; active: boolean }) {
  return <article aria-label={`${entryTitle[entry.kind]} ${entry.id}`}>
    <header className="run-log-bar"><h4>{entryTitle[entry.kind]}</h4><Time value={entry.created_at} /></header>
    {entry.kind === "trigger_message" || entry.kind === "published_reply" ? <>
      <MessageEvidence message={entry.message} groupId={groupId} active={active} />
      {entry.kind === "published_reply" ? <p className="run-log-note">群内原话不作为动作成功的回执。</p> : null}
      <details><summary>查看消息标识</summary><Facts items={[
        ["事件 ID", <code>{entry.id}</code>], ["群消息 ID", <code>{entry.message.message_id}</code>],
        ["发送者 ID", <code>{entry.message.sender_id}</code>],
      ]} /></details>
    </> : entry.kind === "public_explanation" ? <>
      <p className="run-log-copy">{entry.public_explanation.text}</p>
      <p className="run-log-note">这是成员主动公开的陈述，不是运行或动作回执。</p>
    </> : entry.kind === "submission" ? <>
      <p className="run-log-state">已记录正式提交，非验收</p>
      {entry.submission.truncated ? <p className="run-log-note">提交摘要摘录</p> : null}
      <p className="run-log-copy">{entry.submission.preview}</p>
      <details><summary>查看提交与版本</summary><Facts items={[
        ["事件 ID", <code>{entry.id}</code>], ["提交 ID", <code>{entry.submission.submission_id}</code>],
        ["任务 ID", <code>{entry.submission.task_id}</code>], ["任务版本", entry.submission.task_revision],
        ["尝试 ID", <code>{entry.submission.attempt_id}</code>],
      ]} /></details>
    </> : entry.kind === "dispatch_decision" ? <DispatchDecisionEvidence eventId={entry.id} decision={entry.dispatch_decision} />
      : entry.kind === "workspace_operation" ? <OperationEvidence eventId={entry.id} operation={entry.workspace_operation} /> : null}
  </article>;
}

function MessageEvidence({ message, groupId, active }: { message: AgentRunMessage; groupId: string; active: boolean }) {
  const { message_id, sender_id, preview, truncated } = message;
  const fetchFull = useCallback(async (_window: ReadWindow, signal: AbortSignal) => {
    // The run projection binds a group and message ID, but does not project a topic selector.
    const full = await readHistory(groupId, message_id, undefined, signal);
    if (full.sender_id !== sender_id || !full.content.startsWith(preview) || full.content.length <= preview.length) {
      throw new ApiError("历史原文与运行记录不一致，无法显示完整消息。", 200, "invalid_response");
    }
    return full.content;
  }, [groupId, message_id, preview, sender_id]);
  const read = useLogRead(active, fetchFull, undefined, false);
  return <>
    {truncated ? <p className="run-log-note">{read.value === null ? "群消息摘录" : "完整群消息"}</p> : null}
    <p className="run-log-copy">{read.value ?? preview}</p>
    <ReadFeedback subject="完整消息" {...read} />
    {truncated && read.value === null && !read.error ? <button type="button" aria-label={`读取完整消息 ${message_id}`}
      aria-disabled={read.pending} onClick={() => read.load()}>读取完整消息</button> : null}
  </>;
}

function DispatchDecisionEvidence({ eventId, decision }: { eventId: string; decision: AgentRunDispatchDecision }) {
  const items: [string, ReactNode][] = [
    ["事件 ID", <code>{eventId}</code>], ["决定 ID", <code>{decision.decision_id}</code>],
    ["分派请求 ID", <code>{decision.request_id}</code>], ["任务 ID", <code>{decision.task_id}</code>],
  ];
  if (decision.kind === "assign") {
    items.push(["执行成员 ID", <code>{decision.worker_member_id}</code>], ["分派 ID", <code>{decision.assignment_id}</code>]);
  }
  return <>
    <p className="run-log-state">{decision.kind === "assign" ? "已记录分派，不代表已执行" : "已记录拒绝分派"}</p>
    <details><summary>查看分派决定</summary>
      <p className="run-log-note">这条决定不代表本轮运行结果。</p>
      {decision.reason_preview ? <><p className="run-log-note">{decision.truncated ? "理由摘录" : "理由"}</p>
        <p className="run-log-copy">{decision.reason_preview}</p></>
        : decision.truncated ? <p className="run-log-note">理由已截断，无可显示的摘录。</p> : null}
      <Facts items={items} />
    </details>
  </>;
}

function OperationEvidence({ eventId, operation: op }: { eventId: string; operation: AgentRunWorkspaceOperation }) {
  const result = op.result;
  const items: [string, ReactNode][] = [
    ["事件 ID", <code>{eventId}</code>], ["操作 ID", <code>{op.operation_id}</code>],
    ["任务 ID", <code>{op.task_id}</code>], ["尝试 ID", <code>{op.attempt_id}</code>],
  ];
  if (result) {
    items.push(["回执记录时间", <Time value={result.finished_at} />]);
    if (result.observed_hash) items.push(["观测哈希", <code>{result.observed_hash}</code>], ["观测大小（字节）", result.observed_size]);
    if (result.error_code) items.push(["结果代码", <code>{result.error_code}</code>]);
  }
  return <>
    <p className="run-log-state">{op.kind === "create" ? "新建文件" : "替换文件"} · {operationStatus[op.state]}</p>
    <details><summary>查看动作结果</summary>
      {!result ? <p className="run-log-note">只有操作登记，尚未记录执行结果。</p> : null}
      <Facts items={items} />
    </details>
  </>;
}
