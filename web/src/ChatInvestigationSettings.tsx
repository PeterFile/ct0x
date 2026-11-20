import { useId } from "react";
import { chatInvestigationLimits as limits } from "./chat-api";

type VersionRow = Readonly<{ row: number; artifact_id: string; version_id: string }>;
export type InvestigationDraft = Readonly<{
  enabled: boolean; objective: string; allowTopicMessages: boolean; artifactVersions: readonly VersionRow[]; maxDelegations: number; clueTerms: string;
}>;

export function ChatInvestigationSettings({ draft, disabled, supported, onChange }: {
  draft: InvestigationDraft; disabled: boolean; supported: boolean; onChange: (draft: InvestigationDraft) => void;
}) {
  const id = useId(), rows = draft.artifactVersions;
  const change = (patch: Partial<InvestigationDraft>) => onChange({ ...draft, ...patch });
  const changeId = (row: number, field: "artifact_id" | "version_id", value: string) =>
    change({ artifactVersions: rows.map(ref => ref.row === row ? { ...ref, [field]: value } : ref) });
  return <fieldset disabled={disabled}>
    <legend>只读调查</legend>
    <p className="tasks-note">只读调查（read_only_investigation）仅限本群主话题与下列获准资料，只交付文本结论，不授予文件、进程或网络权限。与其他动作共用本版任务预算；自然聊天工作关闭时不准入，委派上限也不是授权。</p>
    <label className="chat-policy-toggle"><input type="checkbox" checked={draft.enabled} disabled={!supported && !draft.enabled}
      onChange={event => change({ enabled: event.target.checked })} />配置只读调查</label>
    {!supported && draft.enabled ? <p className="tasks-note">配置能力未确认或不可用。保留调查草稿；如要移除调查权限，请明确取消勾选后保存。</p> : null}
    {draft.enabled ? <fieldset disabled={!supported}>
      <label htmlFor={`${id}-objective`}>调查目标</label>
      <textarea id={`${id}-objective`} aria-describedby={`${id}-objective-limit`} rows={2} value={draft.objective}
        onChange={event => change({ objective: event.target.value })} />
      <p className="tasks-note" id={`${id}-objective-limit`}>目标不得为空，最多 {limits.objectiveBytes} UTF-8 字节。</p>
      <label htmlFor={`${id}-clues`}>调查线索词组（可选）</label>
      <textarea id={`${id}-clues`} aria-describedby={`${id}-clue-limit`} rows={2} value={draft.clueTerms}
        onChange={event => change({ clueTerms: event.target.value })} />
      <p className="tasks-note" id={`${id}-clue-limit`}>每行一个词组，最多 {limits.clueTerms} 个，每个最多 {limits.clueTermBytes} UTF-8 字节。全部词组须按原样出现在同一条消息原文中；「只讨论」不准入。留空则沿用非线索调查方式。</p>
      <label className="chat-policy-toggle"><input type="checkbox" checked={draft.allowTopicMessages}
        onChange={event => change({ allowTopicMessages: event.target.checked })} />允许使用本群主话题已保存消息</label>
      <label htmlFor={`${id}-delegations`}>每项调查最多委派次数</label>
      <select id={`${id}-delegations`} value={draft.maxDelegations} onChange={event => change({ maxDelegations: Number(event.target.value) })}>
        {Array.from({ length: limits.maxDelegations + 1 }, (_, count) => <option key={count} value={count}>{count}</option>)}
      </select>
      <p className="tasks-note" id={`${id}-materials`}>至少选择本话题消息或一个本群确切成果版本。最多 {limits.artifactVersions} 对且不得重复，每个 ID 最多 {limits.idBytes} UTF-8 字节；请填写确切 ID，不使用路径或“最新版本”。</p>
      {rows.map(ref => <div key={ref.row}>
        <label htmlFor={`${id}-artifact-${ref.row}`}>成果 ID（第 {ref.row} 项）</label>
        <input id={`${id}-artifact-${ref.row}`} autoCapitalize="none" spellCheck={false} value={ref.artifact_id}
          onChange={event => changeId(ref.row, "artifact_id", event.target.value)} />
        <label htmlFor={`${id}-version-${ref.row}`}>版本 ID（第 {ref.row} 项）</label>
        <input id={`${id}-version-${ref.row}`} autoCapitalize="none" spellCheck={false} value={ref.version_id}
          onChange={event => changeId(ref.row, "version_id", event.target.value)} />
        <button type="button" onClick={() => change({ artifactVersions: rows.filter(row => row.row !== ref.row) })}>移除第 {ref.row} 项</button>
      </div>)}
      <button type="button" aria-describedby={`${id}-materials`} disabled={rows.length >= limits.artifactVersions}
        onClick={() => change({ artifactVersions: [...rows, { row: (rows.at(-1)?.row ?? 0) + 1, artifact_id: "", version_id: "" }] })}>
        添加确切成果版本</button>
    </fieldset> : null}
  </fieldset>;
}
