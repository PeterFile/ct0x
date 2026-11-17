import type { AdapterProtocol, AdapterSupport, Provider } from "./api";

const protocolText: Record<AdapterProtocol, string> = {
  one_shot: "一次性 CLI（one_shot）",
  pi_extension: "Pi 扩展（pi_extension）",
  codex_app_server: "Codex App Server（codex_app_server，实验性）",
};
const supportText: Record<AdapterSupport, string> = {
  supported: "支持（实现合同）", unsupported: "不支持", unknown: "未知",
};
const capabilityFields = [
  ["platform_tools", "平台工具"], ["native_resume", "原生会话恢复"], ["permission_interaction", "权限交互"],
  ["cancel", "原生取消"], ["terminal_evidence", "终态证据"], ["steer", "运行中追加"],
] as const;

export function MemberAdapterDetails({ provider }: { provider?: Provider }) {
  const adapter = provider?.adapter;
  const capabilities = adapter?.capabilities;
  return <>
    <h4>当前接入能力</h4>
    <dl>
      <dt>CLI 版本</dt><dd>{provider?.version || "版本未知"}</dd>
      <dt>CLI 路径</dt><dd>{provider?.path || "路径未知"}</dd>
      <dt>安装状态</dt><dd>{provider?.installation === "installed" ? "已发现可执行文件" : "未知"}</dd>
      <dt>账户状态</dt><dd>{provider?.account === "authenticated" ? "已登录（运行未验证）"
        : provider?.account === "unauthenticated" ? "未登录" : "未知"}</dd>
      <dt>连通性</dt><dd>{provider?.connectivity === "unchecked" ? "未检查" : "未知"}</dd>
      <dt>当前接入方式</dt><dd>{adapter ? protocolText[adapter.protocol] : "未提供"}</dd>
      <dt>进程启用状态</dt><dd>{adapter ? adapter.enabled ? "已启用（不代表运行就绪）" : "未启用" : "未知"}</dd>
      {capabilityFields.map(([key, label]) => <div key={key}>
        <dt>{label}</dt><dd>{supportText[capabilities?.[key] ?? "unknown"]}</dd>
      </div>)}
      <dt>能力证据来源</dt><dd>{capabilities ? "内置实现合同（builtin_contract）" : "未知，未提供实现合同证据"}</dd>
    </dl>
    {!adapter ? <p>接入信息未提供/能力未知</p> : null}
    {capabilities ? <p>实现合同只证明此版本的接入实现，并非握手或实际运行证明；安装、账户、连通性分别展示。</p> : null}
    <p>以上为当前目录与成员配置，不证明历史 Turn 的接入协议、实际模型或运行结果。</p>
  </>;
}
