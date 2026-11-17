import { useEffect, useState } from "react";
import { ApiError } from "./api";
import { getDispatchExecution } from "./dispatch-execution-api";
import type { DispatchExecution, ExecutionTurn, ExecutionTurnState } from "./dispatch-execution-api";
import { useActiveRefresh } from "./useActiveRefresh";

type Identity = { groupId: string; taskId: string; requestId: string };
const stateText: Record<ExecutionTurnState, string> = {
  starting: "正在启动（已认领轮次）", running: "运行中", cancel_requested: "已请求取消，尚未确认停止", unknown: "执行结果未知，需对账",
  succeeded: "执行成功", failed: "执行失败", aborted: "执行已中止", truncated: "执行输出被截断", not_started: "执行未启动",
};
const assignmentText = { pending: "等待投递", enqueued: "已进入执行成员投递队列", held: "执行成员投递暂停" };
function TurnEvidence({ label, turn }: { label: string; turn: ExecutionTurn | null }) {
  const receipt = turn?.receipt;
  return <section className="tasks-delivery" aria-label={label}>
    <h4>{label}</h4>
    {turn ? <>
      <p role="status">{stateText[turn.state]}</p>
      <dl><dt>轮次标识</dt><dd className="tasks-meta">{turn.id}</dd><dt>成员标识</dt><dd className="tasks-meta">{turn.member_id}</dd>
        <dt>原生会话标识</dt><dd className="tasks-meta">{turn.native_session_id || "尚未观测"}</dd>
        <dt>创建 / 更新时间</dt><dd className="tasks-meta">{turn.created_at} / {turn.updated_at}</dd>
        <dt>CLI 接收时间</dt><dd className="tasks-meta">{turn.accepted_at ?? "尚未观测"}</dd>
        <dt>取消请求时间</dt><dd className="tasks-meta">{turn.cancel_requested_at ?? "尚未观测"}</dd></dl>
      {turn.cancel_requested ? <p className="tasks-note">已记录取消请求，不表示执行已停止或资源已释放。</p> : null}
      {receipt ? <>
        <p>回执结果：{stateText[receipt.outcome]}</p>
        <p>原生终态证据：{receipt.terminal ? stateText[receipt.terminal] : "未观测"}（不覆盖回执结果）</p>
        <p>启动观测：{receipt.started ? "已观测" : "本次未观测"}；CLI 接收观测：{receipt.accepted ? "已观测" : "本次未观测"}</p>
        <p>输出完整性：{receipt.output_complete ? "已确认完整" : "未确认完整"}</p>
        <p>{receipt.direct_child_reaped ? "已回收直接子进程（不代表后代进程已停止）" : "未确认直接子进程已回收"}</p>
        <p>投递失败观测：{receipt.delivery_failed ? "是" : "否"}；日志截断：{receipt.logs_truncated ? "是" : "否"}；退出码：{receipt.exit_code}</p>
        <p className="tasks-meta">回执原生会话：{receipt.native_session_id || "未观测"}</p>
      </> : <p className="tasks-note">尚未观测到持久执行回执；不能据此判断成功或未执行。</p>}
    </> : <p className="tasks-note">尚未观测到轮次；不代表没有执行，也不是“执行未启动”的回执。</p>}
  </section>;
}

// Identity changes and hiding unmount the sample, while dispatch mutation ownership stays in the parent.
export function TaskExecution({ active, ...identity }: Identity & { active: boolean }) {
  return active ? <ExecutionSample key={JSON.stringify([identity.groupId, identity.taskId, identity.requestId])} {...identity} /> : null;
}
function ExecutionSample({ groupId, taskId, requestId }: Identity) {
  const [round, setRound] = useState(1);
  const [sample, setSample] = useState<{ round: number; view: DispatchExecution | null; error: string }>({ round: 0, view: null, error: "" });
  const current = sample.round === round, view = current ? sample.view : null, pending = !current;
  useActiveRefresh(true, pending, () => setRound(value => value + 1));
  useEffect(() => {
    let ignore = false;
    const controller = new AbortController();
    void getDispatchExecution(groupId, taskId, requestId, controller.signal).then(view => {
      if (!ignore) setSample({ round, view, error: "" });
    }, error => {
      if (!ignore) setSample({ round, view: null, error: error instanceof ApiError ? error.message : "执行回执未确认，请重新读取。" });
    });
    return () => { ignore = true; controller.abort(); };
  }, [groupId, taskId, requestId, round]);
  return <section className="tasks-lifecycle" aria-label="执行回执" aria-busy={pending}>
    <h3>执行回执</h3>
    <p className="tasks-note">当前面板自动读取执行回执，仅展示服务端观测。执行不等于任务提交或用户验收。</p>
    <p className="tasks-meta">请求标识：{requestId}</p>
    <button type="button" onClick={() => setRound(value => value + 1)}>刷新执行回执</button>
    {pending ? <p role="status">正在读取执行回执…</p> : null}
    {current && sample.error ? <p role="alert">{sample.error}</p> : null}
    {view ? <>
      <p>协调决定：{view.decision ? view.decision.kind === "assign" ? "分配" : "拒绝分派" : "尚未观测"}</p>
      {view.decision ? <p className="tasks-meta">决定标识：{view.decision.id}</p> : null}
      {view.assignment ? <p className="tasks-meta">分配标识：{view.assignment.id}（{assignmentText[view.assignment_dispatch_status!]}，不代表正在运行）</p> : null}
      {view.binding ? <dl><dt>绑定的任务尝试</dt><dd className="tasks-meta">{view.binding.attempt_id}</dd></dl> : <p>尚未观测到分配与任务尝试的绑定。</p>}
      <TurnEvidence label="协调员执行" turn={view.coordinator_turn} />
      <TurnEvidence label="执行成员执行" turn={view.worker_turn} />
    </> : null}
  </section>;
}
