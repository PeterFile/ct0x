import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import type { ComponentPropsWithoutRef, ReactNode, RefObject } from "react";
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
    // Keep the retry control mounted until the new read settles.
    setState(previous => previous.request?.pending ? previous : { ...previous, request: { window, pending: true } });
  }
  return { value: state.value, error: state.error, pending: request?.pending ?? false, load, retry: () => load(request?.window) };
}

type FocusTarget = RefObject<HTMLElement | null>;
function ReadButton({ active, show, focusTarget, ...props }: ComponentPropsWithoutRef<"button"> & {
  active: boolean; show: boolean; focusTarget: FocusTarget;
}) {
  const ref = useRef<HTMLButtonElement>(null);
  const [retained, setRetained] = useState(show);
  useLayoutEffect(() => {
    if (show === retained) return;
    const button = ref.current;
    // Check actual focus while the outgoing control still exists, before paint.
    if (!show && active && button && button.ownerDocument.activeElement === button) focusTarget.current?.focus();
    setRetained(show);
  }, [active, focusTarget, retained, show]);
  return show || retained ? <button {...props} ref={ref} type="button" /> : null;
}

function ReadFeedback({ active, subject, pending, error, retry, focusTarget, initial }: {
  active: boolean; subject: string; pending: boolean; error: string; retry: () => void; focusTarget: FocusTarget;
  initial?: { label: string; load: () => void };
}) {
  return <>
    {pending ? <p role="status">正在读取{subject}…</p> : null}
    {!pending && error ? <p role="alert">{error}</p> : null}
    <ReadButton active={active} show={Boolean(error || initial)} focusTarget={focusTarget} aria-disabled={pending}
      aria-label={error ? undefined : initial?.label} onClick={error ? retry : initial?.load}>
      {error ? `重试读取${subject}` : `读取${subject}`}
    </ReadButton>
  </>;
}

function unique<T>(items: T[], key: (item: T) => string) {
  return [...new Map(items.map(item => [key(item), item] as const)).values()];
}
function mergeRuns(previous: AgentRunPage<AgentRunSummary> | null, next: AgentRunPage<AgentRunSummary>, { before }: ReadWindow): AgentRunPage<AgentRunSummary> {
  return before && previous ? { ...next, items: unique([...previous.items, ...next.items], runKey) } : next;
}
type TimelineRead = AgentRunDetail & { pollHead: boolean };
function runStatusChanged(head: AgentRunDetail, older: AgentRunDetail) {
  return head.run.state !== older.run.state
    || (head.receipt?.observed_terminal ?? "") !== (older.receipt?.observed_terminal ?? "");
}
function mergeTimeline(previous: TimelineRead | null, next: TimelineRead, { before, through }: ReadWindow): TimelineRead {
  // Older observations can request a recheck, but only a newest read supplies run metadata.
  const head = before && previous ? previous : next;
  let pollHead = head.pollHead;
  let { items, next_cursor } = next.timeline;
  if (before && previous) {
    pollHead ||= runStatusChanged(previous, next);
    items = [...previous.timeline.items, ...items];
  } else if (!through && previous && next_cursor) {
    // Only retain a cached tail when the fresh window ends at an exact shared event.
    // Otherwise start pagination at the fresh cursor rather than leaving an unverified gap.
    const boundary = items.at(-1)?.id;
    const frontier = previous.timeline.items.findIndex(item => item.id === boundary);
    if (frontier >= 0 && frontier < previous.timeline.items.length - 1) {
      items = [...items, ...previous.timeline.items.slice(frontier + 1)];
      next_cursor = previous.timeline.next_cursor;
    }
  }
  // Keep the first occurrence of each exact event ID in wire order.
  const seen = new Set<string>();
  items = items.filter(({ id }) => {
    if (seen.has(id)) return false;
    seen.add(id);
    return true;
  });
  return { ...head, pollHead, timeline: { items, next_cursor } };
}
async function readTimeline(run: AgentRunIdentity, { before, through }: ReadWindow, signal: AbortSignal): Promise<TimelineRead> {
  const head = await getAgentRun(run, before, signal);
  // A newest-only read consumes any recheck hint and bases further polling on its own evidence.
  let pollHead = hasPendingEvidence(head);
  if (!through) return { ...head, pollHead };
  // Explicit refresh revisits the loaded range for older action receipts. The first
  // page supplies run metadata; later pages may observe completion before that page does.
  let timeline = head.timeline;
  const items = [...timeline.items];
  const cursors = new Set<string>();
  while (timeline.next_cursor && !timeline.items.some(item => item.id === through)) {
    signal.throwIfAborted();
    const cursor = timeline.next_cursor;
    if (cursors.has(cursor)) throw new ApiError("运行时间线无法继续分页，请重试。", 200, "invalid_response");
    cursors.add(cursor);
    const older = await getAgentRun(run, cursor, signal);
    pollHead ||= runStatusChanged(head, older);
    timeline = older.timeline;
    items.push(...timeline.items);
  }
  return { ...head, pollHead, timeline: { ...timeline, items } };
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
      <ReadFeedback active={active} subject="运行轮次" focusTarget={listHeading} {...read} />
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
      <ReadButton active={active} show={Boolean(cursor)} focusTarget={listHeading} aria-disabled={read.pending}
        onClick={() => { if (cursor) read.load({ before: cursor }); }}>加载更早轮次</ReadButton>
    </section>
    <section className="run-log-detail-pane" id={detailId} aria-labelledby={`${detailId}-heading`}>
      <header className="run-log-bar"><h2 id={`${detailId}-heading`} ref={detailHeading} tabIndex={-1}>本轮证据</h2>
        {selected ? <button type="button" onClick={() => (selectedButton.current ?? listHeading.current)?.focus()}>返回轮次列表</button> : null}
      </header>
      {selected ? <RunEvidence key={selectedKey} active={active} selected={selected} heading={detailHeading} />
        : <p className="run-log-note">选择一轮查看运行证据。</p>}
    </section>
  </div>;
}

function RunEvidence({ active, selected, heading }: { active: boolean; selected: AgentRunSummary; heading: FocusTarget }) {
  const timelineHeading = useRef<HTMLHeadingElement>(null);
  const { group_id, run_id, member_id } = selected;
  const fetchTimeline = useCallback((window: ReadWindow, signal: AbortSignal) => readTimeline({ group_id, run_id, member_id }, window, signal),
    [group_id, run_id, member_id]);
  const read = useLogRead(active, fetchTimeline, mergeTimeline);
  const detail = read.value;
  const refresh = () => read.load({ through: detail?.timeline.items.at(-1)?.id });
  // Poll at most one newest-page request per tick. Old action receipts can be revalidated explicitly.
  useActiveRefresh(active && !read.error && Boolean(detail?.pollHead), read.pending, () => read.load());
  const cursor = detail?.timeline.next_cursor;
  const displayEntries = detail ? [...detail.timeline.items].reverse() : [];
  return <>
    <div className="run-log-bar">
      <p><strong>{memberName(detail?.run ?? selected)}</strong> · {groupName(detail?.run ?? selected)}</p>
      <button type="button" aria-disabled={read.pending} onClick={refresh}>刷新本轮证据</button>
    </div>
    <ReadFeedback active={active} subject="本轮证据" focusTarget={heading} {...read} />
    {detail ? <>
      <p className="run-log-state" role="status">{runStatus[detail.run.state]}</p>
      <details><summary>查看轮次标识</summary><Facts items={[
        ["运行 ID", <code>{run_id}</code>], ["群组 ID", <code>{group_id}</code>], ["成员 ID", <code>{member_id}</code>],
        ["创建时间", <Time value={detail.run.created_at} />], ["更新时间", <Time value={detail.run.updated_at} />],
      ]} /></details>
      <RunReceipt detail={detail} />
      <h3 ref={timelineHeading} tabIndex={-1}>事件时间线</h3>
      <ReadButton active={active} show={Boolean(cursor)} focusTarget={timelineHeading} aria-disabled={read.pending}
        onClick={() => { if (cursor) read.load({ before: cursor }); }}>加载更早证据</ReadButton>
      {displayEntries.length ? <ol className="run-log-timeline" aria-label="事件时间线" role="list">
        {displayEntries.map(entry => <li key={entry.id}>
          <EvidenceEntry entry={entry} groupId={group_id} active={active} focusTarget={timelineHeading} />
        </li>)}
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

function EvidenceEntry({ entry, groupId, active, focusTarget }: {
  entry: AgentRunEntry; groupId: string; active: boolean; focusTarget: FocusTarget;
}) {
  const article = useRef<HTMLElement>(null);
  useLayoutEffect(() => {
    const row = article.current;
    return () => {
      // Check focus before a window reset removes this event, not when the read starts.
      if (row?.contains(row.ownerDocument.activeElement) && !row.closest("[hidden], [inert]")) focusTarget.current?.focus();
    };
  }, [focusTarget]);
  return <article ref={article} aria-label={`${entryTitle[entry.kind]} ${entry.id}`}>
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
  const fullText = useRef<HTMLParagraphElement>(null);
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
    <p className="run-log-copy" ref={fullText} tabIndex={read.value === null ? undefined : -1}>{read.value ?? preview}</p>
    <ReadFeedback active={active} subject="完整消息" focusTarget={fullText} {...read}
      initial={truncated && read.value === null ? { label: `读取完整消息 ${message_id}`, load: () => read.load() } : undefined} />
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
